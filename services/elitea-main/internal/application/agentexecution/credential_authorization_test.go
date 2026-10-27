package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/delegationstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/entratest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/obo"
)

const (
	credentialTenant = "aaaaaaaa-0000-4000-8000-000000000001"
	credentialUser   = "cccccccc-0000-4000-8000-000000000003"
	credentialRef    = "car_fixed-reference-for-test-0001"
)

// memoryRequirements is the requirement store for the application seam;
// the PostgreSQL store has its own integration test.
type memoryRequirements struct {
	mu       sync.Mutex
	recorded map[string]delegationstore.Requirement
}

func (m *memoryRequirements) RecordRequirement(_ context.Context, r delegationstore.Requirement) (delegationstore.Requirement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r.Reference = credentialRef
	m.recorded[r.Reference] = r
	return r, nil
}

func (m *memoryRequirements) ResolveRequirement(_ context.Context, reference string, tenantProjectID, actorUserID int64, delegationID string) (delegationstore.Requirement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recorded[reference]
	if !ok || r.TenantProjectID != tenantProjectID || r.ActorUserID != actorUserID {
		return delegationstore.Requirement{}, delegationstore.ErrRequirementNotFound
	}
	if r.Resolved {
		return delegationstore.Requirement{}, delegationstore.ErrRequirementResolved
	}
	r.Resolved, r.ResolvedDelegationID = true, delegationID
	m.recorded[reference] = r
	return r, nil
}

func credentialTarget() CredentialAuthorizationTarget {
	return CredentialAuthorizationTarget{
		TenantProjectID: 7, ActorUserID: 11,
		ConversationUUID:    "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		ResponseMessageID:   "30e0913e-10d4-43db-b8d0-c7b79480935a",
		ExecutionID:         "execution-governed-1",
		ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
		DelegationID:        "0192f5e2-7c3a-7b1e-8a9d-0000000000d1",
		Scopes:              []string{"https://graph.microsoft.com/Sites.Selected"},
	}
}

// exchangeAgainstMock runs one classic OBO exchange against the Entra mock
// with a scripted answer.
func exchangeAgainstMock(t *testing.T, answer entratest.Response) error {
	t.Helper()
	mock := entratest.New(t)
	mock.ScriptOBO(answer)
	exchanger, err := obo.NewClassicOBO(obo.ClassicConfig{
		Authority: mock.URL, ClientID: mock.MiddleTierClientID, AllowedTenants: []string{credentialTenant},
		Credential: obo.FederatedCredential(func(context.Context) (string, error) { return "wif", nil }),
		HTTPClient: mock.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = exchanger.Exchange(context.Background(), obo.Assertion{
		Token:    mock.AccessToken(credentialTenant, credentialUser, mock.MiddleTierClientID, "access_as_user", time.Hour),
		TenantID: credentialTenant, ObjectID: credentialUser, Audience: mock.MiddleTierClientID,
		ExpiresAt: time.Now().Add(time.Hour),
	}, obo.ExchangeRequest{Scopes: credentialTarget().Scopes, ExecutionID: "execution-governed-1", Generation: 1})
	return err
}

type unusedContinuer struct{}

func (unusedContinuer) ContinueCurrentAgent(context.Context, CurrentContinuationRequest) (CurrentApplicationStartOutcome, error) {
	return CurrentApplicationStartOutcome{}, errors.New("not called")
}

func TestInteractionRequiredAndClaimsChallengePauseTheRunForCredentialAuthorization(t *testing.T) {
	for name, answer := range map[string]entratest.Response{
		"interaction_required": entratest.InteractionRequired(),
		"claims challenge":     entratest.ClaimsChallenge(`{"access_token":{"acrs":{"essential":true,"value":"c1"}}}`),
	} {
		t.Run(name, func(t *testing.T) {
			store := &memoryRequirements{recorded: map[string]delegationstore.Requirement{}}
			service, err := NewCredentialAuthorizations(store, unusedContinuer{})
			if err != nil {
				t.Fatal(err)
			}
			exchangeErr := exchangeAgainstMock(t, answer)
			var challenge *obo.ErrClaimsChallenge
			if !errors.Is(exchangeErr, obo.ErrInteractionRequired) && !errors.As(exchangeErr, &challenge) {
				t.Fatalf("exchange error = %v, want a typed authorization error", exchangeErr)
			}
			pause, ok, err := service.PauseOnExchangeError(context.Background(), credentialTarget(), exchangeErr)
			if err != nil || !ok {
				t.Fatalf("PauseOnExchangeError ok=%v err=%v", ok, err)
			}
			if pause.TerminalState != runtimev1.AgentExecutionTerminalStateV1_AGENT_EXECUTION_TERMINAL_STATE_V1_PAUSED_CREDENTIAL_AUTHORIZATION ||
				pause.ContinuationKind != CurrentContinuationAuthorization ||
				pause.AuthorizationReference != credentialRef || pause.Request.InterruptID != credentialRef {
				t.Fatalf("pause = %+v", pause)
			}
			recorded := store.recorded[credentialRef]
			if recorded.DelegationID != credentialTarget().DelegationID || recorded.ExecutionID != "execution-governed-1" {
				t.Fatalf("recorded requirement = %+v", recorded)
			}
			if challenge != nil && (recorded.Reason != delegationstore.ReasonClaimsChallenge || recorded.Claims != challenge.Claims) {
				t.Fatalf("claims challenge not recorded: %+v", recorded)
			}
		})
	}
}

func TestOtherExchangeErrorsDoNotPause(t *testing.T) {
	store := &memoryRequirements{recorded: map[string]delegationstore.Requirement{}}
	service, err := NewCredentialAuthorizations(store, unusedContinuer{})
	if err != nil {
		t.Fatal(err)
	}
	for _, exchangeErr := range []error{
		exchangeAgainstMock(t, entratest.Response{Status: http.StatusBadRequest, Body: map[string]any{"error": "invalid_grant", "error_codes": []int{50013}}}),
		exchangeAgainstMock(t, entratest.Response{Status: http.StatusBadRequest, Body: map[string]any{"error": "invalid_grant", "error_codes": []int{82014}}}),
		obo.ErrUnavailable, obo.ErrAgentAssertionUnavailable,
	} {
		_, ok, err := service.PauseOnExchangeError(context.Background(), credentialTarget(), exchangeErr)
		if ok || err != nil {
			t.Fatalf("%v paused (ok=%v err=%v); only interaction and claims challenges pause", exchangeErr, ok, err)
		}
	}
	if len(store.recorded) != 0 {
		t.Fatalf("a deny recorded a requirement: %+v", store.recorded)
	}
}

// Resume after re-consent continues the same execution: the same paused
// response, thread and execution generation, through the existing
// authorization continuation.
func TestResumeAfterReconsentContinuesTheSameExecution(t *testing.T) {
	target := credentialTarget()
	resolver := &currentApplicationResolverStub{
		continuationTarget: CurrentContinuationTarget{
			ContinuationKind: CurrentContinuationAuthorization,
			Kind:             CurrentRegenerationApplication, TargetParticipantID: 21,
			QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
			UserInput:  "list SharePoint sites", ThreadID: "thread-governed-1",
			ExecutionGeneration: target.ExecutionGeneration,
			InterruptID:         credentialRef,
			AvailableActions:    []string{"authorize", "skip"},
			AuthorizationRequests: []CurrentAuthorizationRequest{{
				InterruptID: credentialRef, AvailableActions: []string{"authorize", "skip"},
			}},
		},
		target: CurrentApplicationTarget{
			ApplicationID: 31, ApplicationVersionID: 41,
			Variables: json.RawMessage(`[]`),
			VersionDetails: json.RawMessage(`{
  "id":41,"application_id":31,"agent_type":"agent","instructions":"Be careful",
  "llm_settings":{"model_name":"test","model_project_id":7,"openai_compatible":false},
  "meta":{},"tools":[]
}`),
			ChatHistory: json.RawMessage(`[]`),
		},
	}
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-governed-2", CommandID: "command-governed-2", Created: true,
	}}
	starts, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryRequirements{recorded: map[string]delegationstore.Requirement{}}
	service, err := NewCredentialAuthorizations(store, starts)
	if err != nil {
		t.Fatal(err)
	}
	pause, ok, err := service.PauseOnExchangeError(context.Background(), target, obo.ErrInteractionRequired)
	if err != nil || !ok {
		t.Fatalf("pause ok=%v err=%v", ok, err)
	}

	resume := CredentialAuthorizationResume{
		AuthorizationReference: pause.AuthorizationReference, TenantProjectID: 7, ActorUserID: 11,
		DelegationID: "0192f5e2-7c3a-7b1e-8a9d-0000000000d2",
	}
	if _, err := service.Resume(context.Background(), resume); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(admissions.requests) != 1 {
		t.Fatalf("admissions = %d, want 1", len(admissions.requests))
	}
	admission := admissions.requests[0]
	turn, input := admission.CurrentContinueTurn, admission.Input
	if turn == nil || turn.ContinuationKind != CurrentContinuationAuthorization || turn.InterruptID != credentialRef ||
		admission.ClientStreamID != target.ConversationUUID ||
		input.GetThreadId() != "thread-governed-1" || input.GetExecutionGeneration() != target.ExecutionGeneration ||
		!input.ShouldContinue || !input.HitlResume || input.GetHitlAction() != "authorize" {
		t.Fatalf("the resume did not continue the paused execution: admission=%+v turn=%+v", admission, turn)
	}
	if !store.recorded[credentialRef].Resolved {
		t.Fatal("the requirement is still pending")
	}

	// A second resume is refused, and another user cannot resume it.
	if _, err := service.Resume(context.Background(), resume); !errors.Is(err, delegationstore.ErrRequirementResolved) {
		t.Fatalf("second resume err = %v", err)
	}
	resume.ActorUserID = 12
	if _, err := service.Resume(context.Background(), resume); !errors.Is(err, delegationstore.ErrRequirementNotFound) {
		t.Fatalf("foreign resume err = %v", err)
	}
}
