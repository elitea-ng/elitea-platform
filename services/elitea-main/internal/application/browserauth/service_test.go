package browserauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/identity"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
	sessionstate "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/session"
)

var _ IdentityProvisioner = (*identity.ProvisionService)(nil)

var (
	errTransactionMissing  = fmt.Errorf("%w: test transaction missing or consumed", browserflow.ErrTransactionRejected)
	errTransactionMismatch = fmt.Errorf("%w: test transaction binding mismatch", browserflow.ErrTransactionRejected)
	errSessionMissing      = sessionstate.ErrNotFound
)

func TestBrowserAuthenticationLifecycleRotatesAndRevalidates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 20, 9, 0, 0, 0, time.UTC)
	clock := &testClock{now: now}
	sessions := newMemorySessionStore()
	transactions := newMemoryTransactionStore()
	provisioner := &provisionerStub{result: identity.ProvisionResult{UserID: 42}}
	validator := &principalValidatorStub{validate: func(_ context.Context, principal auth.User) (auth.User, error) {
		principal.Email = "user@example.test"
		principal.Roles = []string{"viewer"}
		return principal, nil
	}}
	service := mustService(t, sessions, transactions, provisioner, validator, clock)

	correlation := browserflow.ProtocolCorrelation{Nonce: "nonce-1"}
	providerState := browserflow.ProviderState{PKCEVerifier: strings.Repeat("v", browserflow.MinPKCEVerifierBytes)}
	begin, err := service.Begin(context.Background(), BeginRequest{
		Provider:      "oidc",
		ReturnTarget:  "/projects/7?tab=artifacts",
		Correlation:   correlation,
		ProviderState: providerState,
	})
	if err != nil {
		t.Fatal(err)
	}
	if begin.SessionID == "" || begin.TransactionID == "" || begin.ExpiresAt != now.Add(5*time.Minute) {
		t.Fatalf("begin result = %+v", begin)
	}
	initial, err := sessions.Read(context.Background(), begin.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Done || initial.UserID != nil || initial.Provider == nil || *initial.Provider != "oidc" {
		t.Fatalf("initial state = %+v", initial)
	}
	transaction := transactions.lookup(t, begin.TransactionID)
	if transaction.Provider != "oidc" || transaction.OriginatingSessionID != begin.SessionID ||
		transaction.ReturnTarget != "/projects/7?tab=artifacts" || !transaction.Correlation.Equal(correlation) ||
		transaction.ProviderState != providerState {
		t.Fatalf("transaction = %+v", transaction)
	}

	expiresAt := now.Add(time.Hour)
	assertion := browserflow.VerifiedAssertion{
		Provider:            "oidc",
		ProviderReference:   "subject-42",
		Email:               "USER@Example.Test",
		GivenName:           "Ada",
		FamilyName:          "Lovelace",
		Name:                "Ada Lovelace",
		ProviderAttributes:  json.RawMessage(`{"subject":"subject-42","groups":["users"]}`),
		Expiration:          &expiresAt,
		ProtocolCorrelation: correlation,
	}
	verifier := &assertionVerifierStub{assertion: assertion}
	complete, err := service.Complete(context.Background(), CompleteRequest{
		SessionID:     begin.SessionID,
		TransactionID: begin.TransactionID,
		Provider:      "oidc",
	}, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if complete.SessionID == begin.SessionID || complete.ReturnTarget != "/projects/7?tab=artifacts" {
		t.Fatalf("complete result = %+v", complete)
	}
	if _, err := sessions.Read(context.Background(), begin.SessionID); !errors.Is(err, errSessionMissing) {
		t.Fatalf("fixation candidate remains readable: %v", err)
	}
	completed, err := sessions.Read(context.Background(), complete.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !completed.Done || completed.UserID == nil || *completed.UserID != 42 ||
		completed.Provider == nil || *completed.Provider != "oidc" ||
		string(completed.ProviderAttributes) != string(assertion.ProviderAttributes) {
		t.Fatalf("completed state = %+v", completed)
	}
	if completed.Expiration == nil || !completed.Expiration.Equal(expiresAt) {
		t.Fatalf("completed expiration = %v, want %v", completed.Expiration, expiresAt)
	}
	verification := verifier.lastVerification(t)
	if verification.Provider != "oidc" || verification.OriginatingSessionID != begin.SessionID ||
		!verification.Correlation.Equal(correlation) ||
		verification.ProviderState != providerState {
		t.Fatalf("verification context = %+v", verification)
	}

	request := provisioner.lastRequest(t)
	if request.Assertion.Provider != assertion.Provider ||
		request.Assertion.ProviderReference != assertion.ProviderReference ||
		request.Assertion.Email != assertion.Email ||
		request.Assertion.GivenName != assertion.GivenName ||
		request.Assertion.FamilyName != assertion.FamilyName ||
		request.Assertion.Name != assertion.Name {
		t.Fatalf("provision request = %+v", request)
	}

	assertion.ProviderAttributes[2] = 'X'
	authorized, err := service.Authorize(context.Background(), complete.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if authorized.Principal.UserID != "42" || authorized.Principal.ID != "42" ||
		authorized.Principal.AuthType != "session" || authorized.Principal.Email != "user@example.test" ||
		authorized.Provider != "oidc" || string(authorized.ProviderAttributes) != string(completed.ProviderAttributes) {
		t.Fatalf("authorization = %+v", authorized)
	}
	validated := validator.lastPrincipal(t)
	if validated.ID != "42" || validated.UserID != "42" || validated.AuthType != "session" || validated.TokenID != "" {
		t.Fatalf("principal validation input = %+v", validated)
	}

	logout, err := service.Logout(context.Background(), complete.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if logout.Provider != "oidc" || string(logout.ProviderAttributes) != string(completed.ProviderAttributes) ||
		logout.Expiration == nil || !logout.Expiration.Equal(expiresAt) {
		t.Fatalf("logout context = %+v", logout)
	}
	if _, err := service.Logout(context.Background(), complete.SessionID); err != nil {
		t.Fatalf("idempotent logout: %v", err)
	}
	if _, err := service.Authorize(context.Background(), complete.SessionID); err == nil {
		t.Fatal("logged-out session authorized")
	}
}

func TestCompleteRejectsBindingMismatchWithoutProvisioning(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*CompleteRequest)
	}{
		{name: "provider", mutate: func(request *CompleteRequest) { request.Provider = "saml" }},
		{name: "originating session", mutate: func(request *CompleteRequest) { request.SessionID = "session-other" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service, _, transactions, provisioner, _, clock := newTestService(t)
			begin := beginFlow(t, service, "oidc", browserflow.ProtocolCorrelation{Nonce: "nonce-1"})
			request := CompleteRequest{
				SessionID:     begin.SessionID,
				TransactionID: begin.TransactionID,
				Provider:      "oidc",
			}
			test.mutate(&request)
			verifier := &assertionVerifierStub{
				assertion: validAssertion(clock.Now(), request.Provider, browserflow.ProtocolCorrelation{Nonce: "nonce-1"}),
			}

			if _, err := service.Complete(context.Background(), request, verifier); !errors.Is(err, ErrTransactionRejected) {
				t.Fatalf("error = %v, want %v", err, ErrTransactionRejected)
			}
			if provisioner.callCount() != 0 {
				t.Fatalf("provision calls = %d, want 0", provisioner.callCount())
			}
			if transactions.consumeCount() != 1 {
				t.Fatalf("consume calls = %d, want 1", transactions.consumeCount())
			}
			if verifier.callCount() != 0 {
				t.Fatalf("verifier calls = %d, want 0", verifier.callCount())
			}
		})
	}
}

func TestCompleteRejectsNonCanonicalTransactionIDBeforeStoreAccess(t *testing.T) {
	t.Parallel()

	service, _, transactions, provisioner, _, clock := newTestService(t)
	verifier := &assertionVerifierStub{
		assertion: validAssertion(clock.Now(), "oidc", browserflow.ProtocolCorrelation{Nonce: "nonce-1"}),
	}
	_, err := service.Complete(context.Background(), CompleteRequest{
		SessionID:     "session-1",
		TransactionID: "transaction-1",
		Provider:      "oidc",
	}, verifier)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want %v", err, ErrInvalidRequest)
	}
	if transactions.consumeCount() != 0 || verifier.callCount() != 0 || provisioner.callCount() != 0 {
		t.Fatalf(
			"consume calls = %d, verifier calls = %d, provision calls = %d",
			transactions.consumeCount(),
			verifier.callCount(),
			provisioner.callCount(),
		)
	}
}

func TestCompleteConsumesTransactionBeforeCorrelationOrProvisionFailure(t *testing.T) {
	t.Parallel()

	t.Run("correlation mismatch", func(t *testing.T) {
		t.Parallel()
		service, _, _, provisioner, _, clock := newTestService(t)
		begin := beginFlow(t, service, "oidc", browserflow.ProtocolCorrelation{Nonce: "expected"})
		request := CompleteRequest{
			SessionID:     begin.SessionID,
			TransactionID: begin.TransactionID,
			Provider:      "oidc",
		}
		mismatched := &assertionVerifierStub{
			assertion: validAssertion(clock.Now(), "oidc", browserflow.ProtocolCorrelation{Nonce: "other"}),
		}
		if _, err := service.Complete(context.Background(), request, mismatched); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("first error = %v, want %v", err, ErrUnauthenticated)
		}
		matching := &assertionVerifierStub{
			assertion: validAssertion(clock.Now(), "oidc", browserflow.ProtocolCorrelation{Nonce: "expected"}),
		}
		if _, err := service.Complete(context.Background(), request, matching); !errors.Is(err, ErrTransactionRejected) {
			t.Fatalf("replay error = %v, want %v", err, ErrTransactionRejected)
		}
		if provisioner.callCount() != 0 {
			t.Fatalf("provision calls = %d, want 0", provisioner.callCount())
		}
	})

	t.Run("provision failure", func(t *testing.T) {
		t.Parallel()
		service, sessions, _, provisioner, _, clock := newTestService(t)
		provisioner.err = errors.New("repository unavailable")
		begin := beginFlow(t, service, "saml", browserflow.ProtocolCorrelation{RequestID: "request-1"})
		request := CompleteRequest{
			SessionID:     begin.SessionID,
			TransactionID: begin.TransactionID,
			Provider:      "saml",
		}
		verifier := &assertionVerifierStub{
			assertion: validAssertion(clock.Now(), "saml", browserflow.ProtocolCorrelation{RequestID: "request-1"}),
		}
		if _, err := service.Complete(context.Background(), request, verifier); err == nil {
			t.Fatal("provision failure completed authentication")
		}
		if _, err := service.Complete(context.Background(), request, verifier); !errors.Is(err, ErrTransactionRejected) {
			t.Fatalf("replay error = %v, want %v", err, ErrTransactionRejected)
		}
		if provisioner.callCount() != 1 || sessions.rotateCount() != 0 {
			t.Fatalf("provision calls = %d, rotate calls = %d", provisioner.callCount(), sessions.rotateCount())
		}
	})
}

func TestConcurrentCompleteHasOneProvisionAndOneRotation(t *testing.T) {
	t.Parallel()

	service, sessions, transactions, provisioner, _, clock := newTestService(t)
	correlation := browserflow.ProtocolCorrelation{Nonce: "nonce-1"}
	begin := beginFlow(t, service, "oidc", correlation)
	request := CompleteRequest{
		SessionID:     begin.SessionID,
		TransactionID: begin.TransactionID,
		Provider:      "oidc",
	}
	verifier := &assertionVerifierStub{assertion: validAssertion(clock.Now(), "oidc", correlation)}

	const attempts = 24
	start := make(chan struct{})
	results := make(chan error, attempts)
	var wait sync.WaitGroup
	for range attempts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := service.Complete(context.Background(), request, verifier)
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)

	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 || provisioner.callCount() != 1 || sessions.rotateCount() != 1 {
		t.Fatalf("successes = %d, provision calls = %d, rotate calls = %d", successes, provisioner.callCount(), sessions.rotateCount())
	}
	if transactions.consumeCount() != attempts {
		t.Fatalf("consume calls = %d, want %d", transactions.consumeCount(), attempts)
	}
}

func TestCompleteValidatesBeforeIdentityEffects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*browserflow.VerifiedAssertion, time.Time)
		want   error
	}{
		{
			name: "expired assertion",
			mutate: func(assertion *browserflow.VerifiedAssertion, now time.Time) {
				assertion.Expiration = &now
			},
			want: ErrAuthenticationExpired,
		},
		{
			name: "duplicate provider attribute",
			mutate: func(assertion *browserflow.VerifiedAssertion, _ time.Time) {
				assertion.ProviderAttributes = json.RawMessage(`{"subject":"one","subject":"two"}`)
			},
			want: ErrUnauthenticated,
		},
		{
			name: "non object provider attribute",
			mutate: func(assertion *browserflow.VerifiedAssertion, _ time.Time) {
				assertion.ProviderAttributes = json.RawMessage(`[]`)
			},
			want: ErrUnauthenticated,
		},
		{
			name: "malformed identity",
			mutate: func(assertion *browserflow.VerifiedAssertion, _ time.Time) {
				assertion.ProviderReference = ""
			},
			want: ErrUnauthenticated,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service, _, transactions, provisioner, _, clock := newTestService(t)
			correlation := browserflow.ProtocolCorrelation{Nonce: "nonce-1"}
			begin := beginFlow(t, service, "oidc", correlation)
			assertion := validAssertion(clock.Now(), "oidc", correlation)
			test.mutate(&assertion, clock.Now())

			verifier := &assertionVerifierStub{assertion: assertion}
			_, err := service.Complete(context.Background(), CompleteRequest{
				SessionID:     begin.SessionID,
				TransactionID: begin.TransactionID,
				Provider:      "oidc",
			}, verifier)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if transactions.consumeCount() != 1 || verifier.callCount() != 1 || provisioner.callCount() != 0 {
				t.Fatalf("consume calls = %d, provision calls = %d", transactions.consumeCount(), provisioner.callCount())
			}
		})
	}
}

func TestCompleteRejectsExpiredTransactionReturnedByStore(t *testing.T) {
	t.Parallel()

	service, _, transactions, provisioner, _, clock := newTestService(t)
	correlation := browserflow.ProtocolCorrelation{Nonce: "nonce-1"}
	begin := beginFlow(t, service, "oidc", correlation)
	clock.Set(begin.ExpiresAt)
	_, err := service.Complete(context.Background(), CompleteRequest{
		SessionID:     begin.SessionID,
		TransactionID: begin.TransactionID,
		Provider:      "oidc",
	}, &assertionVerifierStub{assertion: validAssertion(clock.Now(), "oidc", correlation)})
	if !errors.Is(err, ErrTransactionRejected) {
		t.Fatalf("error = %v, want %v", err, ErrTransactionRejected)
	}
	if transactions.consumeCount() != 1 || provisioner.callCount() != 0 {
		t.Fatalf("consume calls = %d, provision calls = %d", transactions.consumeCount(), provisioner.callCount())
	}
}

func TestCompleteRejectsChangedOriginatingSession(t *testing.T) {
	t.Parallel()

	service, sessions, _, provisioner, _, clock := newTestService(t)
	correlation := browserflow.ProtocolCorrelation{Nonce: "nonce-1"}
	begin := beginFlow(t, service, "oidc", correlation)
	otherProvider := "saml"
	attackerUserID := int64(99)
	sessions.replace(begin.SessionID, sessionstate.State{
		SchemaVersion:      sessionstate.CurrentSchemaVersion,
		Done:               true,
		Provider:           &otherProvider,
		ProviderAttributes: json.RawMessage("{}"),
		UserID:             &attackerUserID,
	})

	_, err := service.Complete(context.Background(), CompleteRequest{
		SessionID:     begin.SessionID,
		TransactionID: begin.TransactionID,
		Provider:      "oidc",
	}, &assertionVerifierStub{assertion: validAssertion(clock.Now(), "oidc", correlation)})
	if !errors.Is(err, ErrTransactionRejected) {
		t.Fatalf("error = %v, want %v", err, ErrTransactionRejected)
	}
	if provisioner.callCount() != 0 || sessions.rotateCount() != 0 {
		t.Fatalf("provision calls = %d, rotate calls = %d", provisioner.callCount(), sessions.rotateCount())
	}
}

func TestCompleteClassifiesOriginatingSessionReadFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		readErr error
		want    error
	}{
		{name: "missing", readErr: sessionstate.ErrNotFound, want: ErrTransactionRejected},
		{name: "malformed", readErr: sessionstate.ErrInvalidState, want: ErrDependencyUnavailable},
		{name: "unavailable", readErr: errors.New("redis unavailable"), want: ErrDependencyUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service, sessions, transactions, provisioner, _, clock := newTestService(t)
			correlation := browserflow.ProtocolCorrelation{Nonce: "nonce-1"}
			begin := beginFlow(t, service, "oidc", correlation)
			sessions.readErr = test.readErr
			verifier := &assertionVerifierStub{assertion: validAssertion(clock.Now(), "oidc", correlation)}

			_, err := service.Complete(context.Background(), CompleteRequest{
				SessionID:     begin.SessionID,
				TransactionID: begin.TransactionID,
				Provider:      "oidc",
			}, verifier)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if transactions.consumeCount() != 1 || verifier.callCount() != 0 || provisioner.callCount() != 0 {
				t.Fatalf(
					"consume calls = %d, verifier calls = %d, provision calls = %d",
					transactions.consumeCount(),
					verifier.callCount(),
					provisioner.callCount(),
				)
			}
		})
	}
}

func TestAuthorizeFailsClosedForExpiredInactiveOrMismatchedState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 20, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		state         sessionstate.State
		readErr       error
		validate      func(context.Context, auth.User) (auth.User, error)
		want          error
		validatorCall bool
	}{
		{
			name:  "incomplete",
			state: incompleteSession("oidc"),
			want:  ErrUnauthenticated,
		},
		{
			name:  "expired at exact boundary",
			state: authenticatedSession("oidc", 42, &now),
			want:  ErrAuthenticationExpired,
		},
		{
			name:          "suspended or inactive",
			state:         authenticatedSession("oidc", 42, nil),
			validate:      func(context.Context, auth.User) (auth.User, error) { return auth.User{}, auth.ErrPrincipalInactive },
			want:          ErrUnauthenticated,
			validatorCall: true,
		},
		{
			name:  "validator changes both owner IDs",
			state: authenticatedSession("oidc", 42, nil),
			validate: func(_ context.Context, principal auth.User) (auth.User, error) {
				principal.ID = "99"
				principal.UserID = "99"
				return principal, nil
			},
			want:          ErrUnauthenticated,
			validatorCall: true,
		},
		{
			name:  "validator changes compatibility ID only",
			state: authenticatedSession("oidc", 42, nil),
			validate: func(_ context.Context, principal auth.User) (auth.User, error) {
				principal.ID = "99"
				return principal, nil
			},
			want:          ErrUnauthenticated,
			validatorCall: true,
		},
		{
			name:  "validator changes typed user ID only",
			state: authenticatedSession("oidc", 42, nil),
			validate: func(_ context.Context, principal auth.User) (auth.User, error) {
				principal.UserID = "99"
				return principal, nil
			},
			want:          ErrUnauthenticated,
			validatorCall: true,
		},
		{
			name:  "validator changes auth type casing",
			state: authenticatedSession("oidc", 42, nil),
			validate: func(_ context.Context, principal auth.User) (auth.User, error) {
				principal.AuthType = "Session"
				return principal, nil
			},
			want:          ErrUnauthenticated,
			validatorCall: true,
		},
		{
			name:  "validator returns token ID",
			state: authenticatedSession("oidc", 42, nil),
			validate: func(_ context.Context, principal auth.User) (auth.User, error) {
				principal.TokenID = "7"
				return principal, nil
			},
			want:          ErrUnauthenticated,
			validatorCall: true,
		},
		{
			name:  "validator dependency",
			state: authenticatedSession("oidc", 42, nil),
			validate: func(context.Context, auth.User) (auth.User, error) {
				return auth.User{}, errors.New("database unavailable")
			},
			want:          ErrDependencyUnavailable,
			validatorCall: true,
		},
		{
			name:    "session not found",
			readErr: sessionstate.ErrNotFound,
			want:    ErrUnauthenticated,
		},
		{
			name:    "malformed session",
			readErr: sessionstate.ErrInvalidState,
			want:    ErrDependencyUnavailable,
		},
		{
			name:    "session unavailable",
			readErr: errors.New("session unavailable"),
			want:    ErrDependencyUnavailable,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			sessions := newMemorySessionStore()
			sessions.readErr = test.readErr
			id, err := sessions.Create(context.Background(), test.state)
			if err != nil {
				t.Fatal(err)
			}
			validator := &principalValidatorStub{validate: test.validate}
			service := mustService(
				t,
				sessions,
				newMemoryTransactionStore(),
				&provisionerStub{result: identity.ProvisionResult{UserID: 42}},
				validator,
				&testClock{now: now},
			)

			authorization, err := service.Authorize(context.Background(), id)
			if err == nil {
				t.Fatal("authorization succeeded")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if authorization.Principal.ID != "" || authorization.Provider != "" || authorization.ProviderAttributes != nil {
				t.Fatalf("failed authorization leaked result: %+v", authorization)
			}
			if got := validator.callCount() > 0; got != test.validatorCall {
				t.Fatalf("validator called = %t, want %t", got, test.validatorCall)
			}
		})
	}
}

func TestCancellationIsPreservedAndStopsLaterEffects(t *testing.T) {
	t.Parallel()

	t.Run("pre-canceled begin", func(t *testing.T) {
		t.Parallel()
		service, sessions, _, _, _, _ := newTestService(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := service.Begin(ctx, BeginRequest{Provider: "oidc", ReturnTarget: "/"})
		if !errors.Is(err, context.Canceled) || sessions.createCount() != 0 {
			t.Fatalf("error = %v, create calls = %d", err, sessions.createCount())
		}
	})

	t.Run("transaction create canceled", func(t *testing.T) {
		t.Parallel()
		service, sessions, transactions, _, _, _ := newTestService(t)
		transactions.createErr = fmt.Errorf("secret dependency detail: %w", context.Canceled)
		_, err := service.Begin(context.Background(), BeginRequest{Provider: "oidc", ReturnTarget: "/"})
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "secret") || sessions.deleteCount() != 1 {
			t.Fatalf("error = %v, cleanup deletes = %d", err, sessions.deleteCount())
		}
	})

	t.Run("provision cancellation", func(t *testing.T) {
		t.Parallel()
		service, sessions, _, provisioner, _, clock := newTestService(t)
		correlation := browserflow.ProtocolCorrelation{Nonce: "nonce-1"}
		begin := beginFlow(t, service, "oidc", correlation)
		ctx, cancel := context.WithCancel(context.Background())
		provisioner.provision = func(ctx context.Context, _ identity.ProvisionRequest) (identity.ProvisionResult, error) {
			cancel()
			return identity.ProvisionResult{}, ctx.Err()
		}
		_, err := service.Complete(ctx, CompleteRequest{
			SessionID:     begin.SessionID,
			TransactionID: begin.TransactionID,
			Provider:      "oidc",
		}, &assertionVerifierStub{assertion: validAssertion(clock.Now(), "oidc", correlation)})
		if !errors.Is(err, context.Canceled) || sessions.rotateCount() != 0 {
			t.Fatalf("error = %v, rotate calls = %d", err, sessions.rotateCount())
		}
	})

	t.Run("principal validation deadline", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, time.July, 20, 9, 0, 0, 0, time.UTC)
		sessions := newMemorySessionStore()
		id, err := sessions.Create(context.Background(), authenticatedSession("oidc", 42, nil))
		if err != nil {
			t.Fatal(err)
		}
		validator := &principalValidatorStub{validate: func(context.Context, auth.User) (auth.User, error) {
			return auth.User{}, context.DeadlineExceeded
		}}
		service := mustService(t, sessions, newMemoryTransactionStore(), &provisionerStub{}, validator, &testClock{now: now})
		if _, err := service.Authorize(context.Background(), id); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want %v", err, context.DeadlineExceeded)
		}
	})

	t.Run("logout cancellation", func(t *testing.T) {
		t.Parallel()
		service, sessions, _, _, _, _ := newTestService(t)
		sessions.logoutErr = context.Canceled
		if _, err := service.Logout(context.Background(), "session-1"); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want %v", err, context.Canceled)
		}
	})
}

func TestCompleteReportsPostProvisionSessionFinalizationFailure(t *testing.T) {
	t.Parallel()

	service, sessions, _, provisioner, _, clock := newTestService(t)
	sessions.rotateErr = errors.New("redis password=must-not-leak")
	correlation := browserflow.ProtocolCorrelation{Nonce: "nonce-1"}
	begin := beginFlow(t, service, "oidc", correlation)
	request := CompleteRequest{
		SessionID:     begin.SessionID,
		TransactionID: begin.TransactionID,
		Provider:      "oidc",
	}
	verifier := &assertionVerifierStub{assertion: validAssertion(clock.Now(), "oidc", correlation)}

	_, err := service.Complete(context.Background(), request, verifier)
	if !errors.Is(err, ErrAuthenticationFinalization) {
		t.Fatalf("error = %v, want %v", err, ErrAuthenticationFinalization)
	}
	if strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "must-not-leak") {
		t.Fatalf("finalization error leaked dependency details: %v", err)
	}
	if provisioner.callCount() != 1 || sessions.rotateCount() != 1 {
		t.Fatalf("provision calls = %d, rotate calls = %d", provisioner.callCount(), sessions.rotateCount())
	}
	state, readErr := sessions.Read(context.Background(), begin.SessionID)
	if readErr != nil || state.Done || state.UserID != nil {
		t.Fatalf("old session after failed finalization = %+v, %v", state, readErr)
	}
	if _, replayErr := service.Complete(context.Background(), request, verifier); !errors.Is(replayErr, ErrTransactionRejected) {
		t.Fatalf("replay error = %v, want %v", replayErr, ErrTransactionRejected)
	}
}

func TestAtomicLogoutFencesCompletionAfterOriginatingSessionRead(t *testing.T) {
	t.Parallel()

	service, sessions, _, provisioner, _, clock := newTestService(t)
	correlation := browserflow.ProtocolCorrelation{Nonce: "nonce-1"}
	begin := beginFlow(t, service, "oidc", correlation)
	verificationStarted := make(chan struct{})
	releaseVerification := make(chan struct{})
	verifier := &assertionVerifierStub{verify: func(
		_ context.Context,
		_ browserflow.VerificationContext,
	) (browserflow.VerifiedAssertion, error) {
		close(verificationStarted)
		<-releaseVerification
		return validAssertion(clock.Now(), "oidc", correlation), nil
	}}
	completion := make(chan error, 1)
	go func() {
		_, err := service.Complete(context.Background(), CompleteRequest{
			SessionID:     begin.SessionID,
			TransactionID: begin.TransactionID,
			Provider:      "oidc",
		}, verifier)
		completion <- err
	}()

	<-verificationStarted
	logout, err := service.Logout(context.Background(), begin.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if logout.Provider != "" {
		t.Fatalf("unauthenticated flow exposed federated logout context: %+v", logout)
	}
	close(releaseVerification)
	if err := <-completion; !errors.Is(err, ErrAuthenticationFinalization) {
		t.Fatalf("completion error = %v, want %v", err, ErrAuthenticationFinalization)
	}
	if provisioner.callCount() != 1 || sessions.recordCount() != 0 {
		t.Fatalf("provision calls = %d, remaining sessions = %d", provisioner.callCount(), sessions.recordCount())
	}
}

func TestLogoutReturnsExpiredProviderContextWithoutPrincipalValidation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 20, 9, 0, 0, 0, time.UTC)
	expiredAt := now.Add(-time.Minute)
	sessions := newMemorySessionStore()
	state := authenticatedSession("saml", 42, &expiredAt)
	state.ProviderAttributes = json.RawMessage(`{"sessionindex":"saml-session-1"}`)
	id, err := sessions.Create(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	validator := &principalValidatorStub{validate: func(context.Context, auth.User) (auth.User, error) {
		return auth.User{}, errors.New("suspended")
	}}
	service := mustService(
		t,
		sessions,
		newMemoryTransactionStore(),
		&provisionerStub{},
		validator,
		&testClock{now: now},
	)

	logout, err := service.Logout(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if logout.Provider != "saml" || string(logout.ProviderAttributes) != string(state.ProviderAttributes) ||
		logout.Expiration == nil || !logout.Expiration.Equal(expiredAt) {
		t.Fatalf("logout context = %+v", logout)
	}
	if validator.callCount() != 0 {
		t.Fatalf("logout called active-principal validator %d times", validator.callCount())
	}
}

func TestDependencyErrorsAreSanitized(t *testing.T) {
	t.Parallel()

	const protectedDetail = "password=super-secret"
	t.Run("begin store", func(t *testing.T) {
		t.Parallel()
		service, sessions, _, _, _, _ := newTestService(t)
		sessions.createErr = errors.New(protectedDetail)
		_, err := service.Begin(context.Background(), BeginRequest{Provider: "oidc", ReturnTarget: "/"})
		if !errors.Is(err, ErrDependencyUnavailable) || strings.Contains(err.Error(), protectedDetail) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("provider verifier", func(t *testing.T) {
		t.Parallel()
		service, _, _, _, _, _ := newTestService(t)
		correlation := browserflow.ProtocolCorrelation{Nonce: "nonce-1"}
		begin := beginFlow(t, service, "oidc", correlation)
		_, err := service.Complete(context.Background(), CompleteRequest{
			SessionID:     begin.SessionID,
			TransactionID: begin.TransactionID,
			Provider:      "oidc",
		}, &assertionVerifierStub{err: errors.New(protectedDetail)})
		if !errors.Is(err, ErrUnauthenticated) || strings.Contains(err.Error(), protectedDetail) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("provider verifier dependency", func(t *testing.T) {
		t.Parallel()
		service, _, _, _, _, _ := newTestService(t)
		correlation := browserflow.ProtocolCorrelation{Nonce: "nonce-1"}
		begin := beginFlow(t, service, "oidc", correlation)
		_, err := service.Complete(context.Background(), CompleteRequest{
			SessionID:     begin.SessionID,
			TransactionID: begin.TransactionID,
			Provider:      "oidc",
		}, &assertionVerifierStub{err: fmt.Errorf("%w: %s", ErrAssertionVerifierUnavailable, protectedDetail)})
		if !errors.Is(err, ErrDependencyUnavailable) || errors.Is(err, ErrUnauthenticated) ||
			strings.Contains(err.Error(), protectedDetail) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("principal validator", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, time.July, 20, 9, 0, 0, 0, time.UTC)
		sessions := newMemorySessionStore()
		id, err := sessions.Create(context.Background(), authenticatedSession("oidc", 42, nil))
		if err != nil {
			t.Fatal(err)
		}
		validator := &principalValidatorStub{validate: func(context.Context, auth.User) (auth.User, error) {
			return auth.User{}, errors.New(protectedDetail)
		}}
		service := mustService(t, sessions, newMemoryTransactionStore(), &provisionerStub{}, validator, &testClock{now: now})
		_, err = service.Authorize(context.Background(), id)
		if !errors.Is(err, ErrDependencyUnavailable) || strings.Contains(err.Error(), protectedDetail) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestNewServiceRejectsMissingDependenciesAndUnboundedTTL(t *testing.T) {
	t.Parallel()

	sessions := newMemorySessionStore()
	transactions := newMemoryTransactionStore()
	provisioner := &provisionerStub{}
	validator := &principalValidatorStub{}
	tests := []struct {
		name         string
		sessions     SessionStore
		transactions TransactionStore
		provisioner  IdentityProvisioner
		validator    ActivePrincipalValidator
		ttl          time.Duration
	}{
		{name: "sessions", transactions: transactions, provisioner: provisioner, validator: validator, ttl: time.Minute},
		{name: "transactions", sessions: sessions, provisioner: provisioner, validator: validator, ttl: time.Minute},
		{name: "provisioner", sessions: sessions, transactions: transactions, validator: validator, ttl: time.Minute},
		{name: "validator", sessions: sessions, transactions: transactions, provisioner: provisioner, ttl: time.Minute},
		{name: "short TTL", sessions: sessions, transactions: transactions, provisioner: provisioner, validator: validator, ttl: time.Millisecond},
		{name: "long TTL", sessions: sessions, transactions: transactions, provisioner: provisioner, validator: validator, ttl: browserflow.MaxTransactionLifetime + time.Nanosecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewService(test.sessions, test.transactions, test.provisioner, test.validator, test.ttl)
			if !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("error = %v, want %v", err, ErrInvalidConfiguration)
			}
		})
	}
}

func newTestService(t *testing.T) (
	*Service,
	*memorySessionStore,
	*memoryTransactionStore,
	*provisionerStub,
	*principalValidatorStub,
	*testClock,
) {
	t.Helper()
	now := time.Date(2026, time.July, 20, 9, 0, 0, 0, time.UTC)
	clock := &testClock{now: now}
	sessions := newMemorySessionStore()
	transactions := newMemoryTransactionStore()
	provisioner := &provisionerStub{result: identity.ProvisionResult{UserID: 42}}
	validator := &principalValidatorStub{}
	service := mustService(t, sessions, transactions, provisioner, validator, clock)
	return service, sessions, transactions, provisioner, validator, clock
}

func mustService(
	t *testing.T,
	sessions SessionStore,
	transactions TransactionStore,
	provisioner IdentityProvisioner,
	validator ActivePrincipalValidator,
	clock *testClock,
) *Service {
	t.Helper()
	service, err := newService(sessions, transactions, provisioner, validator, 5*time.Minute, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func beginFlow(
	t *testing.T,
	service *Service,
	provider string,
	correlation browserflow.ProtocolCorrelation,
) BeginResult {
	t.Helper()
	result, err := service.Begin(context.Background(), BeginRequest{
		Provider:     provider,
		ReturnTarget: "/projects/7",
		Correlation:  correlation,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func validAssertion(
	now time.Time,
	provider string,
	correlation browserflow.ProtocolCorrelation,
) browserflow.VerifiedAssertion {
	expiresAt := now.Add(time.Hour)
	return browserflow.VerifiedAssertion{
		Provider:            provider,
		ProviderReference:   "subject-42",
		Email:               "user@example.test",
		GivenName:           "Ada",
		FamilyName:          "Lovelace",
		Name:                "Ada Lovelace",
		ProviderAttributes:  json.RawMessage(`{"subject":"subject-42"}`),
		Expiration:          &expiresAt,
		ProtocolCorrelation: correlation,
	}
}

func incompleteSession(provider string) sessionstate.State {
	return sessionstate.State{
		SchemaVersion:      sessionstate.CurrentSchemaVersion,
		Provider:           &provider,
		ProviderAttributes: json.RawMessage("{}"),
	}
}

func authenticatedSession(provider string, userID int64, expiration *time.Time) sessionstate.State {
	return sessionstate.State{
		SchemaVersion:      sessionstate.CurrentSchemaVersion,
		Done:               true,
		Provider:           &provider,
		ProviderAttributes: json.RawMessage("{}"),
		Expiration:         cloneTime(expiration),
		UserID:             &userID,
	}
}

type testClock struct {
	mu  sync.RWMutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *testClock) Set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

type memorySessionStore struct {
	mu          sync.Mutex
	records     map[string]sessionstate.State
	next        int
	createCalls int
	readCalls   int
	rotateCalls int
	logoutCalls int
	deleteCalls int
	createErr   error
	readErr     error
	rotateErr   error
	logoutErr   error
	deleteErr   error
}

func newMemorySessionStore() *memorySessionStore {
	return &memorySessionStore{records: make(map[string]sessionstate.State)}
}

func (s *memorySessionStore) Create(_ context.Context, state sessionstate.State) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	if s.createErr != nil {
		return "", s.createErr
	}
	s.next++
	id := fmt.Sprintf("session-%d", s.next)
	s.records[id] = cloneSessionState(state)
	return id, nil
}

func (s *memorySessionStore) Read(_ context.Context, id string) (sessionstate.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readCalls++
	if s.readErr != nil {
		return sessionstate.State{}, s.readErr
	}
	state, ok := s.records[id]
	if !ok {
		return sessionstate.State{}, errSessionMissing
	}
	return cloneSessionState(state), nil
}

func (s *memorySessionStore) RotateAndReplace(
	_ context.Context,
	id string,
	replacement sessionstate.State,
) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rotateCalls++
	if s.rotateErr != nil {
		return "", s.rotateErr
	}
	if _, ok := s.records[id]; !ok {
		return "", errSessionMissing
	}
	s.next++
	newID := fmt.Sprintf("session-%d", s.next)
	s.records[newID] = cloneSessionState(replacement)
	delete(s.records, id)
	return newID, nil
}

func (s *memorySessionStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteCalls++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.records, id)
	return nil
}

func (s *memorySessionStore) ConsumeForLogout(
	_ context.Context,
	id string,
) (sessionstate.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logoutCalls++
	if s.logoutErr != nil {
		return sessionstate.State{}, s.logoutErr
	}
	state, ok := s.records[id]
	if !ok {
		return sessionstate.State{}, nil
	}
	delete(s.records, id)
	return cloneSessionState(state), nil
}

func (s *memorySessionStore) replace(id string, state sessionstate.State) {
	s.mu.Lock()
	s.records[id] = cloneSessionState(state)
	s.mu.Unlock()
}

func (s *memorySessionStore) createCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createCalls
}

func (s *memorySessionStore) rotateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rotateCalls
}

func (s *memorySessionStore) deleteCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteCalls
}

func (s *memorySessionStore) recordCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

type memoryTransactionStore struct {
	mu           sync.Mutex
	records      map[string]browserflow.Transaction
	next         int
	createCalls  int
	consumeCalls int
	createErr    error
	consumeErr   error
}

func newMemoryTransactionStore() *memoryTransactionStore {
	return &memoryTransactionStore{records: make(map[string]browserflow.Transaction)}
}

func (s *memoryTransactionStore) Create(
	_ context.Context,
	transaction browserflow.Transaction,
) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	if s.createErr != nil {
		return "", s.createErr
	}
	s.next++
	id := testTransactionID(byte(s.next))
	s.records[id] = transaction
	return id, nil
}

func (s *memoryTransactionStore) Consume(
	_ context.Context,
	id string,
	provider string,
	originatingSessionID string,
) (browserflow.Transaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consumeCalls++
	if s.consumeErr != nil {
		return browserflow.Transaction{}, s.consumeErr
	}
	transaction, ok := s.records[id]
	if !ok {
		return browserflow.Transaction{}, errTransactionMissing
	}
	if transaction.Provider != provider || transaction.OriginatingSessionID != originatingSessionID {
		return browserflow.Transaction{}, errTransactionMismatch
	}
	delete(s.records, id)
	return transaction, nil
}

func (s *memoryTransactionStore) lookup(t *testing.T, id string) browserflow.Transaction {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	transaction, ok := s.records[id]
	if !ok {
		t.Fatalf("transaction %q not found", id)
	}
	return transaction
}

func (s *memoryTransactionStore) consumeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.consumeCalls
}

type provisionerStub struct {
	mu        sync.Mutex
	requests  []identity.ProvisionRequest
	result    identity.ProvisionResult
	err       error
	provision func(context.Context, identity.ProvisionRequest) (identity.ProvisionResult, error)
}

type assertionVerifierStub struct {
	mu            sync.Mutex
	verifications []browserflow.VerificationContext
	assertion     browserflow.VerifiedAssertion
	err           error
	verify        func(context.Context, browserflow.VerificationContext) (browserflow.VerifiedAssertion, error)
}

func (s *assertionVerifierStub) Verify(
	ctx context.Context,
	verification browserflow.VerificationContext,
) (browserflow.VerifiedAssertion, error) {
	s.mu.Lock()
	s.verifications = append(s.verifications, verification)
	assertion := cloneAssertion(s.assertion)
	err := s.err
	verify := s.verify
	s.mu.Unlock()
	if verify != nil {
		return verify(ctx, verification)
	}
	return assertion, err
}

func (s *assertionVerifierStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.verifications)
}

func (s *assertionVerifierStub) lastVerification(t *testing.T) browserflow.VerificationContext {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.verifications) == 0 {
		t.Fatal("assertion verifier was not called")
	}
	return s.verifications[len(s.verifications)-1]
}

func (s *provisionerStub) Provision(
	ctx context.Context,
	request identity.ProvisionRequest,
) (identity.ProvisionResult, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	result := s.result
	err := s.err
	provision := s.provision
	s.mu.Unlock()
	if provision != nil {
		return provision(ctx, request)
	}
	return result, err
}

func (s *provisionerStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *provisionerStub) lastRequest(t *testing.T) identity.ProvisionRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		t.Fatal("provisioner was not called")
	}
	return s.requests[len(s.requests)-1]
}

type principalValidatorStub struct {
	mu         sync.Mutex
	principals []auth.User
	validate   func(context.Context, auth.User) (auth.User, error)
}

func (s *principalValidatorStub) ValidatePrincipal(
	ctx context.Context,
	principal auth.User,
) (auth.User, error) {
	s.mu.Lock()
	s.principals = append(s.principals, principal)
	validate := s.validate
	s.mu.Unlock()
	if validate != nil {
		return validate(ctx, principal)
	}
	return principal, nil
}

func (s *principalValidatorStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.principals)
}

func (s *principalValidatorStub) lastPrincipal(t *testing.T) auth.User {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.principals) == 0 {
		t.Fatal("principal validator was not called")
	}
	return s.principals[len(s.principals)-1]
}

func cloneSessionState(state sessionstate.State) sessionstate.State {
	if state.Provider != nil {
		provider := *state.Provider
		state.Provider = &provider
	}
	if state.UserID != nil {
		userID := *state.UserID
		state.UserID = &userID
	}
	state.Expiration = cloneTime(state.Expiration)
	state.ProviderAttributes = append(json.RawMessage(nil), state.ProviderAttributes...)
	return state
}

func testTransactionID(value byte) string {
	random := make([]byte, browserflow.TransactionIDRandomBytes)
	for index := range random {
		random[index] = value
	}
	return base64.RawURLEncoding.EncodeToString(random)
}

// ADR-0025 §3.9: a refused credential (a wrong Form password) reports the
// transaction's return target with the error, and nothing else, so the Form
// page can send the browser back to sign-in WITH it.
func TestCompleteKeepsTheReturnTargetWhenTheCredentialIsRefused(t *testing.T) {
	t.Parallel()
	service, _, _, provisioner, _, _ := newTestService(t)
	correlation := browserflow.ProtocolCorrelation{Nonce: "nonce-1"}
	begin := beginFlow(t, service, "oidc", correlation)
	result, err := service.Complete(context.Background(), CompleteRequest{
		SessionID:     begin.SessionID,
		TransactionID: begin.TransactionID,
		Provider:      "oidc",
	}, &assertionVerifierStub{err: errors.New("invalid login or password")})
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("error = %v, want %v", err, ErrUnauthenticated)
	}
	if result.ReturnTarget != "/projects/7" || result.SessionID != "" {
		t.Fatalf("result = %+v, want only the return target", result)
	}
	if provisioner.callCount() != 0 {
		t.Fatal("a refused credential reached provisioning")
	}
}
