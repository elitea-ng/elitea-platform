// Package browserauth coordinates provider-neutral browser authentication.
package browserauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/identity"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
	sessionstate "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/session"
)

const minTransactionLifetime = time.Second

var (
	ErrInvalidConfiguration = errors.New("invalid browser authentication configuration")
	ErrInvalidRequest       = errors.New("invalid browser authentication request")
	ErrTransactionRejected  = browserflow.ErrTransactionRejected
	ErrUnauthenticated      = errors.New("browser session is not authenticated")
	// ErrAssertionVerifierUnavailable is returned only by trusted protocol
	// adapters when assertion verification could not be attempted because an
	// identity-provider dependency was unavailable. Invalid codes, tokens, and
	// claims must return a different error and remain authentication failures.
	ErrAssertionVerifierUnavailable = errors.New("browser assertion verifier unavailable")
	ErrAuthenticationExpired        = errors.New("browser authentication expired")
	ErrDependencyUnavailable        = errors.New("browser authentication dependency unavailable")
	ErrAuthenticationFinalization   = errors.New("browser authentication session finalization failed")
)

// SessionStore is the server-side browser-session contract. It retains the
// existing Create, Read, Delete, and RotateAndReplace primitives and adds the
// atomic logout consumption required before production composition.
type SessionStore interface {
	Create(ctx context.Context, state sessionstate.State) (string, error)
	// Read returns session.ErrNotFound when id is absent or has expired. Other
	// errors represent malformed state or a storage dependency failure.
	Read(ctx context.Context, id string) (sessionstate.State, error)
	RotateAndReplace(ctx context.Context, id string, replacement sessionstate.State) (string, error)
	// ConsumeForLogout atomically reads and deletes id. A missing ID returns a
	// zero State and nil so logout remains idempotent. This atomic operation
	// fences a concurrent RotateAndReplace from authenticating a consumed ID.
	ConsumeForLogout(ctx context.Context, id string) (sessionstate.State, error)
	Delete(ctx context.Context, id string) error
}

// TransactionStore persists short-lived login transactions. Consume must
// atomically reject a missing, expired, already-consumed, provider-mismatched,
// or originating-session-mismatched transaction with
// browserflow.ErrTransactionRejected before returning it. Create returns a
// canonical browserflow transaction ID generated from 256 CSPRNG bits.
type TransactionStore interface {
	Create(ctx context.Context, transaction browserflow.Transaction) (string, error)
	Consume(
		ctx context.Context,
		id string,
		provider string,
		originatingSessionID string,
	) (browserflow.Transaction, error)
}

type IdentityProvisioner interface {
	Provision(ctx context.Context, request identity.ProvisionRequest) (identity.ProvisionResult, error)
}

// ActivePrincipalValidator reloads mutable identity state. It returns
// auth.ErrPrincipalInactive only for a missing or suspended identity; storage
// failures remain distinct. The production authsvc.PrincipalValidator
// satisfies this interface.
type ActivePrincipalValidator interface {
	ValidatePrincipal(ctx context.Context, principal auth.User) (auth.User, error)
}

// AssertionVerifier is implemented by a trusted Form, OIDC, or SAML adapter.
// Complete calls it only after atomically consuming the bound transaction. The
// verifier receives bounded server-only material and must return verified
// claims without copying secrets into the assertion. The verifier instance
// owns the already bounded provider callback input (authorization code, SAML
// response, or form submission); callback bytes are not stored in Transaction.
type AssertionVerifier interface {
	Verify(
		ctx context.Context,
		verification browserflow.VerificationContext,
	) (browserflow.VerifiedAssertion, error)
}

type BeginRequest struct {
	Provider      string
	ReturnTarget  string
	Correlation   browserflow.ProtocolCorrelation
	ProviderState browserflow.ProviderState
}

type BeginResult struct {
	SessionID     string
	TransactionID string
	ExpiresAt     time.Time
}

type CompleteRequest struct {
	SessionID     string
	TransactionID string
	Provider      string
}

type CompleteResult struct {
	SessionID    string
	ReturnTarget string
}

type Authorization struct {
	Principal          auth.User
	Provider           string
	ProviderAttributes json.RawMessage
	Expiration         *time.Time
}

// LogoutContext is a bounded snapshot for optional OIDC or SAML federated
// logout after the local session has been invalidated. Expired authentication
// state remains eligible; logout never requires an active principal.
type LogoutContext struct {
	Provider           string
	ProviderAttributes json.RawMessage
	Expiration         *time.Time
}

type Service struct {
	sessions           SessionStore
	transactions       TransactionStore
	provisioner        IdentityProvisioner
	principalValidator ActivePrincipalValidator
	transactionTTL     time.Duration
	now                func() time.Time
}

func NewService(
	sessions SessionStore,
	transactions TransactionStore,
	provisioner IdentityProvisioner,
	principalValidator ActivePrincipalValidator,
	transactionTTL time.Duration,
) (*Service, error) {
	return newService(
		sessions,
		transactions,
		provisioner,
		principalValidator,
		transactionTTL,
		time.Now,
	)
}

func newService(
	sessions SessionStore,
	transactions TransactionStore,
	provisioner IdentityProvisioner,
	principalValidator ActivePrincipalValidator,
	transactionTTL time.Duration,
	now func() time.Time,
) (*Service, error) {
	if sessions == nil || transactions == nil || provisioner == nil || principalValidator == nil || now == nil ||
		transactionTTL < minTransactionLifetime || transactionTTL > browserflow.MaxTransactionLifetime {
		return nil, ErrInvalidConfiguration
	}
	return &Service{
		sessions:           sessions,
		transactions:       transactions,
		provisioner:        provisioner,
		principalValidator: principalValidator,
		transactionTTL:     transactionTTL,
		now:                now,
	}, nil
}

// Begin creates a fresh unauthenticated session instead of accepting a
// browser-selected session ID. Complete can therefore rotate only a
// server-generated fixation candidate bound into this transaction.
func (s *Service) Begin(ctx context.Context, request BeginRequest) (BeginResult, error) {
	if err := ctx.Err(); err != nil {
		return BeginResult{}, err
	}
	if browserflow.ValidateProvider(request.Provider) != nil ||
		browserflow.ValidateReturnTarget(request.ReturnTarget) != nil ||
		request.Correlation.Validate() != nil || request.ProviderState.Validate() != nil {
		return BeginResult{}, ErrInvalidRequest
	}

	now := s.now().UTC()
	expiresAt := now.Add(s.transactionTTL)
	provider := request.Provider
	state := sessionstate.State{
		SchemaVersion:      sessionstate.CurrentSchemaVersion,
		Provider:           &provider,
		ProviderAttributes: json.RawMessage("{}"),
	}
	if err := state.Validate(); err != nil {
		return BeginResult{}, fmt.Errorf("%w: initial session state", ErrInvalidConfiguration)
	}

	sessionID, err := s.sessions.Create(ctx, state)
	if err != nil {
		return BeginResult{}, sanitizedError(
			ctx,
			ErrDependencyUnavailable,
			"create unauthenticated browser session",
			err,
		)
	}
	if browserflow.ValidateOpaqueID(sessionID) != nil {
		_ = s.sessions.Delete(ctx, sessionID)
		return BeginResult{}, fmt.Errorf("%w: session store returned an invalid ID", ErrInvalidConfiguration)
	}

	transaction := browserflow.Transaction{
		SchemaVersion:        browserflow.CurrentTransactionSchemaVersion,
		Provider:             request.Provider,
		OriginatingSessionID: sessionID,
		ReturnTarget:         request.ReturnTarget,
		CreatedAt:            now,
		ExpiresAt:            expiresAt,
		Correlation:          request.Correlation,
		ProviderState:        request.ProviderState,
	}
	if err := transaction.Validate(); err != nil {
		_ = s.sessions.Delete(ctx, sessionID)
		return BeginResult{}, fmt.Errorf("%w: transaction", ErrInvalidConfiguration)
	}

	transactionID, err := s.transactions.Create(ctx, transaction)
	if err != nil {
		// A failed Begin never exposes sessionID. Best-effort deletion uses the
		// caller's remaining budget; an undeleted unauthenticated record remains
		// harmless and bounded by the session store TTL.
		_ = s.sessions.Delete(ctx, sessionID)
		return BeginResult{}, sanitizedError(
			ctx,
			ErrDependencyUnavailable,
			"create browser authentication transaction",
			err,
		)
	}
	if browserflow.ValidateTransactionID(transactionID) != nil {
		_ = s.sessions.Delete(ctx, sessionID)
		return BeginResult{}, fmt.Errorf("%w: transaction store returned an invalid ID", ErrInvalidConfiguration)
	}

	return BeginResult{
		SessionID:     sessionID,
		TransactionID: transactionID,
		ExpiresAt:     expiresAt,
	}, nil
}

func (s *Service) Complete(
	ctx context.Context,
	request CompleteRequest,
	verifier AssertionVerifier,
) (CompleteResult, error) {
	if err := ctx.Err(); err != nil {
		return CompleteResult{}, err
	}
	if browserflow.ValidateOpaqueID(request.SessionID) != nil ||
		browserflow.ValidateTransactionID(request.TransactionID) != nil ||
		browserflow.ValidateProvider(request.Provider) != nil || verifier == nil {
		return CompleteResult{}, ErrInvalidRequest
	}

	now := s.now().UTC()
	transaction, err := s.transactions.Consume(
		ctx,
		request.TransactionID,
		request.Provider,
		request.SessionID,
	)
	if err != nil {
		if errors.Is(err, browserflow.ErrTransactionRejected) {
			return CompleteResult{}, ErrTransactionRejected
		}
		return CompleteResult{}, sanitizedError(
			ctx,
			ErrDependencyUnavailable,
			"consume browser authentication transaction",
			err,
		)
	}
	if transaction.Validate() != nil || !transaction.ActiveAt(now) ||
		transaction.Provider != request.Provider ||
		transaction.OriginatingSessionID != request.SessionID {
		return CompleteResult{}, ErrTransactionRejected
	}

	current, err := s.sessions.Read(ctx, request.SessionID)
	if err != nil {
		if errors.Is(err, sessionstate.ErrNotFound) {
			return CompleteResult{}, ErrTransactionRejected
		}
		return CompleteResult{}, sanitizedError(
			ctx,
			ErrDependencyUnavailable,
			"read originating browser session",
			err,
		)
	}
	if !isOriginatingSession(current, request.Provider) {
		return CompleteResult{}, ErrTransactionRejected
	}

	assertion, err := verifier.Verify(ctx, browserflow.VerificationContext{
		Provider:             transaction.Provider,
		OriginatingSessionID: transaction.OriginatingSessionID,
		Correlation:          transaction.Correlation,
		ProviderState:        transaction.ProviderState,
	})
	if err != nil {
		if errors.Is(err, ErrAssertionVerifierUnavailable) {
			return CompleteResult{}, sanitizedError(
				ctx,
				ErrDependencyUnavailable,
				"verify provider assertion",
				err,
			)
		}
		// The return target survives a wrong password (ADR-0025 WP2, §3.9):
		// the Form page sends the browser back to /auth/login with it, so a
		// typo on the way to a deep link — or to a native sign-in's consent
		// page — does not strand the user on the default landing page. Only
		// ReturnTarget is set; no session is.
		return CompleteResult{ReturnTarget: transaction.ReturnTarget},
			sanitizedError(ctx, ErrUnauthenticated, "verify provider assertion", err)
	}
	assertion = cloneAssertion(assertion)
	if assertion.Validate() != nil || assertion.Provider != transaction.Provider ||
		!transaction.Correlation.Equal(assertion.ProtocolCorrelation) {
		return CompleteResult{}, ErrUnauthenticated
	}
	if assertion.Expiration != nil && !now.Before(*assertion.Expiration) {
		return CompleteResult{}, ErrAuthenticationExpired
	}
	if err := authenticatedState(assertion, 1).Validate(); err != nil {
		return CompleteResult{}, ErrUnauthenticated
	}

	provisioned, err := s.provisioner.Provision(ctx, identity.ProvisionRequest{
		Assertion: identity.VerifiedAssertion{
			Provider:          assertion.Provider,
			ProviderReference: assertion.ProviderReference,
			Email:             assertion.Email,
			GivenName:         assertion.GivenName,
			FamilyName:        assertion.FamilyName,
			Name:              assertion.Name,
		},
	})
	if err != nil {
		if errors.Is(err, identity.ErrIdentitySuspended) ||
			errors.Is(err, identity.ErrIdentityConflict) {
			return CompleteResult{}, ErrUnauthenticated
		}
		// A configuration error, not an outage: the provider asserted no
		// usable address. Refuse with the generic failure (never a 503) and
		// say why in the server log.
		if errors.Is(err, identity.ErrMissingEmail) {
			slog.ErrorContext(ctx, "browser sign-in refused: the identity has no usable email address",
				"provider", assertion.Provider, "provider_reference", assertion.ProviderReference,
				"error", err)
			return CompleteResult{}, ErrUnauthenticated
		}
		return CompleteResult{}, sanitizedError(
			ctx,
			ErrDependencyUnavailable,
			"provision authenticated identity",
			err,
		)
	}
	if provisioned.UserID <= 0 || provisioned.Suspended {
		return CompleteResult{}, ErrUnauthenticated
	}

	replacement := authenticatedState(assertion, provisioned.UserID)
	newSessionID, err := s.sessions.RotateAndReplace(ctx, request.SessionID, replacement)
	if err != nil {
		// Identity provisioning may already have committed in PostgreSQL. The
		// one-time transaction remains consumed and the old session remains
		// unauthenticated; the caller must begin a new, idempotent login flow.
		return CompleteResult{}, sanitizedError(
			ctx,
			ErrAuthenticationFinalization,
			"rotate authenticated browser session",
			err,
		)
	}
	if browserflow.ValidateOpaqueID(newSessionID) != nil || newSessionID == request.SessionID {
		_ = s.sessions.Delete(ctx, newSessionID)
		return CompleteResult{}, fmt.Errorf("%w: session rotation returned an invalid ID", ErrAuthenticationFinalization)
	}

	return CompleteResult{
		SessionID:    newSessionID,
		ReturnTarget: transaction.ReturnTarget,
	}, nil
}

// Authorize reads the server-side state, applies authentication expiration,
// and revalidates the mutable user on every call. No dependency failure is
// converted into a public or anonymous principal.
func (s *Service) Authorize(ctx context.Context, sessionID string) (Authorization, error) {
	if err := ctx.Err(); err != nil {
		return Authorization{}, err
	}
	if browserflow.ValidateOpaqueID(sessionID) != nil {
		return Authorization{}, ErrInvalidRequest
	}
	state, err := s.sessions.Read(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sessionstate.ErrNotFound) {
			return Authorization{}, ErrUnauthenticated
		}
		return Authorization{}, sanitizedError(
			ctx,
			ErrDependencyUnavailable,
			"read authenticated browser session",
			err,
		)
	}
	if !isAuthenticatedSession(state) {
		return Authorization{}, ErrUnauthenticated
	}

	now := s.now().UTC()
	if state.Expiration != nil && !now.Before(*state.Expiration) {
		return Authorization{}, ErrAuthenticationExpired
	}

	userID := strconv.FormatInt(*state.UserID, 10)
	principal, err := s.principalValidator.ValidatePrincipal(ctx, auth.User{
		ID:       userID,
		UserID:   userID,
		AuthType: "session",
	})
	if err != nil {
		if errors.Is(err, auth.ErrPrincipalInactive) {
			return Authorization{}, ErrUnauthenticated
		}
		return Authorization{}, sanitizedError(
			ctx,
			ErrDependencyUnavailable,
			"validate active browser principal",
			err,
		)
	}
	if principal.ID != userID || principal.UserID != userID || principal.TokenID != "" ||
		principal.AuthType != "session" {
		return Authorization{}, ErrUnauthenticated
	}

	providerAttributes := append(json.RawMessage(nil), state.ProviderAttributes...)
	return Authorization{
		Principal:          principal,
		Provider:           *state.Provider,
		ProviderAttributes: providerAttributes,
		Expiration:         cloneTime(state.Expiration),
	}, nil
}

// Logout snapshots bounded provider state without checking authentication
// expiration or mutable user activity, then invalidates the local session.
// The returned context may be used for optional federated logout only after
// this method succeeds.
func (s *Service) Logout(ctx context.Context, sessionID string) (LogoutContext, error) {
	if err := ctx.Err(); err != nil {
		return LogoutContext{}, err
	}
	if browserflow.ValidateOpaqueID(sessionID) != nil {
		return LogoutContext{}, ErrInvalidRequest
	}
	state, err := s.sessions.ConsumeForLogout(ctx, sessionID)
	if err != nil {
		return LogoutContext{}, sanitizedError(ctx, ErrDependencyUnavailable, "consume browser logout state", err)
	}
	if !isAuthenticatedSession(state) {
		return LogoutContext{}, nil
	}
	return LogoutContext{
		Provider:           *state.Provider,
		ProviderAttributes: append(json.RawMessage(nil), state.ProviderAttributes...),
		Expiration:         cloneTime(state.Expiration),
	}, nil
}

func authenticatedState(assertion browserflow.VerifiedAssertion, userID int64) sessionstate.State {
	provider := assertion.Provider
	return sessionstate.State{
		SchemaVersion:      sessionstate.CurrentSchemaVersion,
		Done:               true,
		Provider:           &provider,
		ProviderAttributes: append(json.RawMessage(nil), assertion.ProviderAttributes...),
		Expiration:         cloneTime(assertion.Expiration),
		UserID:             &userID,
	}
}

func isOriginatingSession(state sessionstate.State, provider string) bool {
	return state.Validate() == nil && !state.Done && state.Error == "" && state.UserID == nil &&
		state.Expiration == nil && state.Provider != nil && *state.Provider == provider
}

func isAuthenticatedSession(state sessionstate.State) bool {
	return state.Validate() == nil && state.Done && state.Error == "" && state.UserID != nil &&
		state.Provider != nil
}

func cloneAssertion(assertion browserflow.VerifiedAssertion) browserflow.VerifiedAssertion {
	assertion.ProviderAttributes = append(json.RawMessage(nil), assertion.ProviderAttributes...)
	assertion.Expiration = cloneTime(assertion.Expiration)
	return assertion
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := value.UTC()
	return &cloned
}

func sanitizedError(ctx context.Context, sentinel error, operation string, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	// The returned error names only the sentinel and the operation, so nothing
	// sensitive reaches the response. A rejected credential is an expected
	// outcome; any other sentinel is a server-side failure whose cause would
	// otherwise be lost here, so log it.
	if !errors.Is(sentinel, ErrUnauthenticated) {
		slog.ErrorContext(ctx, "browser authentication failed",
			"operation", operation, "outcome", sentinel.Error(), "error", err)
	}
	return fmt.Errorf("%w: %s", sentinel, operation)
}
