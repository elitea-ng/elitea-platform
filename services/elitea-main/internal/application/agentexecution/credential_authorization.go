package agentexecution

// Governed credential authorization pause and resume (ADR-0039, issue #1117).
//
// When an Entra exchange answers ErrInteractionRequired or a claims
// challenge, the credential broker (#1118) calls PauseOnExchangeError. It
// records an authorization requirement and returns the pause the run ends
// with: continuation kind `authorization`, terminal state
// PAUSED_CREDENTIAL_AUTHORIZATION, and an authorization request whose
// interrupt id is the opaque authorization_reference the API returns. It
// never falls back to an app-only token.
//
// After the user re-consents and signs in again, Resume marks the
// requirement resolved by the new delegation and continues the same paused
// response through the existing authorization continuation
// (ContinueCurrentAgent), so the run resumes from its thread and
// generation.
//
// The service is built only with credential governance on, so with the flag
// off the new terminal state is never produced.

import (
	"context"
	"errors"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/delegationstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/obo"
)

// CredentialAuthorizationStore is the requirement half of the delegation
// store.
type CredentialAuthorizationStore interface {
	RecordRequirement(ctx context.Context, r delegationstore.Requirement) (delegationstore.Requirement, error)
	ResolveRequirement(ctx context.Context, reference string, tenantProjectID, actorUserID int64, delegationID string) (delegationstore.Requirement, error)
}

// CurrentAgentContinuer is the existing continuation entry point.
type CurrentAgentContinuer interface {
	ContinueCurrentAgent(ctx context.Context, request CurrentContinuationRequest) (CurrentApplicationStartOutcome, error)
}

var _ CurrentAgentContinuer = (*CurrentApplicationStartService)(nil)

// CredentialAuthorizationTarget names the governed use that was refused.
type CredentialAuthorizationTarget struct {
	TenantProjectID     int64
	ActorUserID         int64
	ConversationUUID    string
	ResponseMessageID   string
	ExecutionID         string
	ExecutionGeneration string
	DelegationID        string
	Scopes              []string
}

// CredentialAuthorizationPause is how the run ends.
type CredentialAuthorizationPause struct {
	TerminalState    runtimev1.AgentExecutionTerminalStateV1
	ContinuationKind CurrentContinuationKind
	// AuthorizationReference is the opaque reference the API returns for
	// the re-consent action.
	AuthorizationReference string
	Request                CurrentAuthorizationRequest
	// Reason is delegationstore.ReasonInteractionRequired or
	// ReasonClaimsChallenge.
	Reason string
}

// CredentialAuthorizations pauses and resumes governed runs.
type CredentialAuthorizations struct {
	store     CredentialAuthorizationStore
	continuer CurrentAgentContinuer
}

// NewCredentialAuthorizations builds the service.
func NewCredentialAuthorizations(store CredentialAuthorizationStore, continuer CurrentAgentContinuer) (*CredentialAuthorizations, error) {
	if store == nil || continuer == nil {
		return nil, errors.New("credential authorizations need a requirement store and a continuer")
	}
	return &CredentialAuthorizations{store: store, continuer: continuer}, nil
}

// PauseOnExchangeError maps an exchange error to a pause. ok is false when
// err is not an authorization requirement: the caller denies the use.
func (s *CredentialAuthorizations) PauseOnExchangeError(
	ctx context.Context,
	target CredentialAuthorizationTarget,
	exchangeErr error,
) (pause CredentialAuthorizationPause, ok bool, err error) {
	requirement := delegationstore.Requirement{
		TenantProjectID: target.TenantProjectID, ActorUserID: target.ActorUserID,
		ConversationUUID: target.ConversationUUID, ResponseMessageID: target.ResponseMessageID,
		ExecutionID: target.ExecutionID, ExecutionGeneration: target.ExecutionGeneration,
		DelegationID: target.DelegationID, Scopes: target.Scopes,
	}
	var challenge *obo.ErrClaimsChallenge
	switch {
	case errors.As(exchangeErr, &challenge):
		requirement.Reason, requirement.Claims = delegationstore.ReasonClaimsChallenge, challenge.Claims
	case errors.Is(exchangeErr, obo.ErrInteractionRequired):
		requirement.Reason = delegationstore.ReasonInteractionRequired
	default:
		return CredentialAuthorizationPause{}, false, nil
	}
	recorded, err := s.store.RecordRequirement(ctx, requirement)
	if err != nil {
		return CredentialAuthorizationPause{}, false, err
	}
	return CredentialAuthorizationPause{
		TerminalState:          runtimev1.AgentExecutionTerminalStateV1_AGENT_EXECUTION_TERMINAL_STATE_V1_PAUSED_CREDENTIAL_AUTHORIZATION,
		ContinuationKind:       CurrentContinuationAuthorization,
		AuthorizationReference: recorded.Reference,
		Request: CurrentAuthorizationRequest{
			InterruptID:      recorded.Reference,
			AvailableActions: []string{"authorize", "skip"},
		},
		Reason: recorded.Reason,
	}, true, nil
}

// CredentialAuthorizationResume is the user's answer to a pause.
type CredentialAuthorizationResume struct {
	AuthorizationReference string
	TenantProjectID        int64
	ActorUserID            int64
	// DelegationID is the delegation created after re-consent.
	DelegationID  string
	TriggerOrigin executiondomain.TriggerOrigin
}

// Resume resolves the requirement and continues the paused response.
func (s *CredentialAuthorizations) Resume(ctx context.Context, r CredentialAuthorizationResume) (CurrentApplicationStartOutcome, error) {
	resolved, err := s.store.ResolveRequirement(ctx, r.AuthorizationReference, r.TenantProjectID, r.ActorUserID, r.DelegationID)
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	return s.continuer.ContinueCurrentAgent(ctx, CurrentContinuationRequest{
		ProjectID:          resolved.TenantProjectID,
		ActorUserID:        resolved.ActorUserID,
		ConversationUUID:   resolved.ConversationUUID,
		ResponseMessageID:  resolved.ResponseMessageID,
		Kind:               CurrentContinuationAuthorization,
		AuthorizationID:    resolved.Reference,
		Action:             "authorize",
		MCPTokens:          []byte(`{}`),
		IgnoredMCPServers:  []byte(`[]`),
		DeclinedMCPServers: []byte(`[]`),
		TriggerOrigin:      r.TriggerOrigin,
	})
}
