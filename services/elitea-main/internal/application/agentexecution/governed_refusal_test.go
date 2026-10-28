package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type governedRefusalSettlerStub struct {
	requests []GovernedRefusalSettlement
	settled  bool
	err      error
}

func (stub *governedRefusalSettlerStub) SettleGovernedRefusal(
	_ context.Context,
	request GovernedRefusalSettlement,
) (bool, error) {
	stub.requests = append(stub.requests, request)
	return stub.settled, stub.err
}

func governedRefusalContinuationService(t *testing.T, admissionErr error, settler GovernedRefusalSettler) *CurrentApplicationStartService {
	t.Helper()
	resolver := &currentApplicationResolverStub{
		continuationTarget: CurrentContinuationTarget{
			Kind: CurrentRegenerationApplication, TargetParticipantID: 21,
			QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
			UserInput:  "comment on GOV-1", ThreadID: "thread-governed-1",
			ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
			InterruptID:         "interrupt-governed-1",
			AvailableActions:    []string{"approve", "reject"},
			HITLInterrupts: []CurrentHITLInterrupt{{
				InterruptID: "interrupt-governed-1", AvailableActions: []string{"approve", "reject"},
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
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{}, &currentApplicationAdmissionStub{err: admissionErr})
	if err != nil {
		t.Fatal(err)
	}
	if settler != nil {
		service = service.WithGovernedRefusalSettlement(settler)
	}
	return service
}

func governedRefusalContinuationRequest() CurrentContinuationRequest {
	return CurrentContinuationRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID:  "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a",
		ThreadID:          "thread-governed-1", Action: "approve",
	}
}

// #1485: a governed resume that admission refuses settles the paused turn
// with a reason that names no credential, and the caller still gets the
// refusal (the route answers it 403 governed_credential_refused).
func TestCurrentContinuationRefusedByGovernedAdmissionSettlesThePause(t *testing.T) {
	for _, test := range []struct {
		name       string
		settlerErr error
		settled    bool
	}{
		{name: "settled", settled: true},
		{name: "nothing left to settle"},
		{name: "settlement failed", settlerErr: errors.New("database unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			refusal := &GovernedCredentialAdmissionError{Code: "credential_grant_missing", ToolkitID: 5}
			settler := &governedRefusalSettlerStub{settled: test.settled, err: test.settlerErr}
			service := governedRefusalContinuationService(t, refusal, settler)
			request := governedRefusalContinuationRequest()
			_, err := service.ContinueCurrentAgent(context.Background(), request)
			var governed *GovernedCredentialAdmissionError
			if !errors.As(err, &governed) || governed.Code != "credential_grant_missing" {
				t.Fatalf("error=%v, want the governed refusal", err)
			}
			if len(settler.requests) != 1 {
				t.Fatalf("settlements=%d", len(settler.requests))
			}
			got := settler.requests[0]
			if got.ProjectID != 7 || got.ActorUserID != 11 ||
				got.ResponseMessageID != request.ResponseMessageID || got.Code != "credential_grant_missing" ||
				!strings.Contains(got.Reason, "revoked") || len(got.Reason) > 256 {
				t.Fatalf("settlement=%+v", got)
			}
		})
	}
}

func TestCurrentContinuationOtherAdmissionErrorsLeaveThePause(t *testing.T) {
	settler := &governedRefusalSettlerStub{}
	service := governedRefusalContinuationService(t, ErrInvalidAgentAdmission, settler)
	if _, err := service.ContinueCurrentAgent(context.Background(), governedRefusalContinuationRequest()); !errors.Is(err, ErrInvalidAgentAdmission) {
		t.Fatalf("error=%v", err)
	}
	if len(settler.requests) != 0 {
		t.Fatalf("a non-governed refusal settled the pause: %+v", settler.requests)
	}

	// Without a settler the refusal is answered as before.
	refusal := &GovernedCredentialAdmissionError{Code: "credential_reference_not_active"}
	unwired := governedRefusalContinuationService(t, refusal, nil)
	if _, err := unwired.ContinueCurrentAgent(context.Background(), governedRefusalContinuationRequest()); !errors.As(err, new(*GovernedCredentialAdmissionError)) {
		t.Fatalf("error=%v", err)
	}
}

func TestGovernedRefusalReasonNamesNoCredential(t *testing.T) {
	for _, code := range []string{
		"credential_grant_missing", "credential_reference_not_active", "credential_reference_not_found",
		"toolkit_digest_mismatch", "credential_reference_ambiguous", "",
	} {
		reason := governedRefusalReason(code)
		if reason == "" || len(reason) > 256 || strings.Contains(reason, code) && code != "" {
			t.Fatalf("code %q: reason %q", code, reason)
		}
	}
}
