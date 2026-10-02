package agentexecution

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

func TestCurrentContinuationDatabaseIDBoundsCurrentIntegerSchema(t *testing.T) {
	for _, test := range []struct {
		name  string
		value int64
		want  int32
		ok    bool
	}{
		{name: "minimum", value: 1, want: 1, ok: true},
		{name: "maximum", value: math.MaxInt32, want: math.MaxInt32, ok: true},
		{name: "zero", value: 0},
		{name: "negative", value: -1},
		{name: "overflow", value: math.MaxInt32 + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := currentContinuationDatabaseID(test.value)
			if got != test.want || ok != test.ok {
				t.Fatalf("current continuation database ID = (%d, %t), want (%d, %t)", got, ok, test.want, test.ok)
			}
		})
	}
}

func TestCurrentApplicationOutputContinuationReusesExactSessionAndVisiblePartial(t *testing.T) {
	resolver := &currentApplicationResolverStub{
		continuationTarget: CurrentContinuationTarget{
			ContinuationKind:    CurrentContinuationOutputLimit,
			Kind:                CurrentRegenerationApplication,
			TargetParticipantID: 21,
			QuestionID:          "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
			UserInput:           "Explain durable workers",
			ThreadID:            "thread-current-output-1",
			ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
			TruncatedContent:    "A durable worker stores progress before it acknowledges work.",
			OutputLimitSequence: 2,
		},
		target: CurrentApplicationTarget{
			ApplicationID: 31, ApplicationVersionID: 41,
			Variables: json.RawMessage(`[]`),
			VersionDetails: json.RawMessage(`{
  "id":41,"application_id":31,"agent_type":"agent","instructions":"Be precise",
  "llm_settings":{"model_name":"test","model_project_id":7,"openai_compatible":false},
  "meta":{},"tools":[]
}`),
			ChatHistory: json.RawMessage(`[]`),
		},
	}
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-output-continue", CommandID: "command-output-continue", Created: true,
	}}
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{}, admissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	request := CurrentContinuationRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID:  "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a",
		Kind:              CurrentContinuationOutputLimit,
	}

	outcome, err := service.ContinueCurrentAgent(context.Background(), request)
	if err != nil || len(admissions.requests) != 1 {
		t.Fatalf("ContinueCurrentAgent() error=%v admissions=%d", err, len(admissions.requests))
	}
	admission := admissions.requests[0]
	turn := admission.CurrentContinueTurn
	if outcome.ResponseMessageID != request.ResponseMessageID ||
		admission.CapabilityID != executiondomain.AgentApplicationCapability ||
		admission.SIOEvent != "chat_continue_predict" || turn == nil ||
		turn.ContinuationKind != CurrentContinuationOutputLimit ||
		turn.OutputLimitSequence != 2 || turn.ThreadID != "thread-current-output-1" ||
		turn.ExecutionGeneration != resolver.continuationTarget.ExecutionGeneration ||
		turn.InterruptID != "" || turn.Action != "" || len(turn.HITLDecisions) != 0 {
		t.Fatalf("outcome=%+v admission=%+v turn=%+v", outcome, admission, turn)
	}
	input := admission.Input
	if !input.ShouldContinue || input.HitlResume || input.HitlAction != nil || input.HitlValue != nil ||
		string(input.HitlDecisions) != `[]` || input.GetThreadId() != "thread-current-output-1" ||
		input.GetExecutionGeneration() != resolver.continuationTarget.ExecutionGeneration ||
		string(input.TruncatedContent) != `"A durable worker stores progress before it acknowledges work."` {
		t.Fatalf("input=%+v truncated_content=%s", input, input.TruncatedContent)
	}
	if admission.IdempotencyKey != "continue-output/30e0913e-10d4-43db-b8d0-c7b79480935a/2" {
		t.Fatalf("idempotency_key=%q", admission.IdempotencyKey)
	}
	if err := validateAuthoritativeInput(input); err != nil {
		t.Fatalf("output continuation must pass authoritative validation: %v", err)
	}
}

func TestCurrentOutputContinuationSupportsZeroVisibleContentAndRejectsMixedResume(t *testing.T) {
	target := CurrentContinuationTarget{
		ContinuationKind: CurrentContinuationOutputLimit,
		Kind:             CurrentRegenerationAdhoc, TargetParticipantID: 21,
		QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e", UserInput: "Think first",
		ThreadID: "thread-output-empty", ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
		TruncatedContent: "", OutputLimitSequence: 1,
	}
	if err := target.Validate(); err != nil {
		t.Fatalf("zero-visible output continuation must be valid: %v", err)
	}
	request := CurrentContinuationRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID:  "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a",
		Kind:              CurrentContinuationOutputLimit,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("minimal output continuation request must be valid: %v", err)
	}
	request.ThreadID = "browser-selected-thread"
	if err := request.Validate(); err != ErrInvalidCurrentAgentStart {
		t.Fatalf("mixed output continuation error=%v", err)
	}
	target.HITLInterrupts = []CurrentHITLInterrupt{{InterruptID: "interrupt-1", AvailableActions: []string{"approve"}}}
	if err := target.Validate(); err != ErrUnsupportedCurrentAgentStart {
		t.Fatalf("mixed output target error=%v", err)
	}
}

func TestCurrentApplicationContinuationReusesCheckpointAndResponse(t *testing.T) {
	resolver := &currentApplicationResolverStub{
		continuationTarget: CurrentContinuationTarget{
			Kind: CurrentRegenerationApplication, TargetParticipantID: 21,
			QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
			UserInput:  "delete the stale branch", ThreadID: "thread-current-1",
			ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
			InterruptID:         "interrupt-sensitive-1",
			AvailableActions:    []string{"approve", "reject", "edit", "block_with_comment"},
			HITLInterrupts: []CurrentHITLInterrupt{{
				InterruptID: "interrupt-sensitive-1", AvailableActions: []string{"approve", "reject", "edit", "block_with_comment"},
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
	admittedAt := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-continue", CommandID: "command-continue", Created: true,
		AdmittedAt: admittedAt, Deadline: admittedAt.Add(time.Minute),
	}}
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}
	request := CurrentContinuationRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID:  "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a",
		ThreadID:          "thread-current-1", Action: "edit", Value: "delete only merged branches",
	}
	outcome, err := service.ContinueCurrentAgent(context.Background(), request)
	if err != nil {
		t.Fatalf("ContinueCurrentAgent() error = %v", err)
	}
	if outcome.ExecutionID != "execution-continue" || outcome.ResponseMessageID != request.ResponseMessageID ||
		len(admissions.requests) != 1 {
		t.Fatalf("outcome=%+v admissions=%d", outcome, len(admissions.requests))
	}
	admission := admissions.requests[0]
	turn := admission.CurrentContinueTurn
	if admission.CapabilityID != executiondomain.AgentApplicationCapability ||
		admission.SIOEvent != "chat_continue_predict" || admission.CurrentTurn != nil ||
		admission.CurrentAdhocTurn != nil || admission.CurrentRegenerateTurn != nil || turn == nil ||
		turn.ResponseMessageID != request.ResponseMessageID ||
		turn.ExecutionGeneration != resolver.continuationTarget.ExecutionGeneration ||
		turn.ThreadID != resolver.continuationTarget.ThreadID ||
		turn.InterruptID != resolver.continuationTarget.InterruptID ||
		turn.ApplicationID != 31 || turn.ApplicationVersionID != 41 || turn.Action != "edit" {
		t.Fatalf("admission=%+v turn=%+v", admission, turn)
	}
	input := admission.Input
	if !input.HitlResume || !input.ShouldContinue || input.GetHitlAction() != "edit" ||
		input.GetHitlValue() != request.Value || input.GetThreadId() != resolver.continuationTarget.ThreadID ||
		input.GetExecutionGeneration() != resolver.continuationTarget.ExecutionGeneration {
		t.Fatalf("input=%+v", input)
	}
	var decisions []map[string]string
	if err := json.Unmarshal(input.HitlDecisions, &decisions); err != nil || len(decisions) != 1 ||
		decisions[0]["interrupt_id"] != resolver.continuationTarget.InterruptID ||
		decisions[0]["action"] != "edit" || decisions[0]["value"] != request.Value {
		t.Fatalf("decisions=%s error=%v", input.HitlDecisions, err)
	}
}

func TestCurrentApplicationContinuationCarriesBlockWithCommentToOneExactDecision(t *testing.T) {
	resolver := &currentApplicationResolverStub{
		continuationTarget: CurrentContinuationTarget{
			Kind: CurrentRegenerationApplication, TargetParticipantID: 21,
			QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
			UserInput:  "delete the stale artifact", ThreadID: "thread-current-1",
			ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
			InterruptID:         "interrupt-sensitive-1",
			AvailableActions:    []string{"approve", "reject", "block_with_comment"},
			HITLInterrupts: []CurrentHITLInterrupt{{
				InterruptID: "interrupt-sensitive-1", AvailableActions: []string{"approve", "reject", "block_with_comment"},
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
		ExecutionID: "execution-comment", CommandID: "command-comment", Created: true,
	}}
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}
	comment := "append the requested data before retrying the sensitive action"
	_, err = service.ContinueCurrentAgent(context.Background(), CurrentContinuationRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID:  "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a",
		ThreadID:          "thread-current-1",
		Action:            "block_with_comment", Value: comment,
	})
	if err != nil || len(admissions.requests) != 1 {
		t.Fatalf("ContinueCurrentAgent() error=%v admissions=%d", err, len(admissions.requests))
	}
	admission := admissions.requests[0]
	if admission.CurrentContinueTurn == nil ||
		admission.CurrentContinueTurn.InterruptID != resolver.continuationTarget.InterruptID ||
		admission.CurrentContinueTurn.Action != "block_with_comment" ||
		admission.Input.GetHitlAction() != "block_with_comment" ||
		admission.Input.GetHitlValue() != comment {
		t.Fatalf("admission=%+v input=%+v", admission.CurrentContinueTurn, admission.Input)
	}
	var decisions []map[string]string
	if err := json.Unmarshal(admission.Input.HitlDecisions, &decisions); err != nil || len(decisions) != 1 ||
		decisions[0]["interrupt_id"] != resolver.continuationTarget.InterruptID ||
		decisions[0]["action"] != "block_with_comment" || decisions[0]["value"] != comment {
		t.Fatalf("decisions=%s error=%v", admission.Input.HitlDecisions, err)
	}
}

func TestCurrentApplicationContinuationCarriesClarifyingAnswerToOneExactDecision(t *testing.T) {
	resolver := &currentApplicationResolverStub{
		continuationTarget: CurrentContinuationTarget{
			Kind: CurrentRegenerationApplication, TargetParticipantID: 21,
			QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
			UserInput:  "prepare the release", ThreadID: "thread-current-1",
			ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
			InterruptID:         "interrupt-clarifying-1",
			AvailableActions:    []string{"answer"},
			HITLInterrupts: []CurrentHITLInterrupt{{
				InterruptID: "interrupt-clarifying-1", AvailableActions: []string{"answer"},
			}},
		},
		target: CurrentApplicationTarget{
			ApplicationID: 31, ApplicationVersionID: 41,
			Variables: json.RawMessage(`[]`),
			VersionDetails: json.RawMessage(`{
  "id":41,"application_id":31,"agent_type":"agent","instructions":"Clarify first",
  "llm_settings":{"model_name":"test","model_project_id":7,"openai_compatible":false},
  "meta":{},"tools":[]
}`),
			ChatHistory: json.RawMessage(`[]`),
		},
	}
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-answer", CommandID: "command-answer", Created: true,
	}}
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{}, admissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	answer := `{"q1":"Production","q2":["API","UI"]}`
	_, err = service.ContinueCurrentAgent(context.Background(), CurrentContinuationRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID:  "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a",
		ThreadID:          "thread-current-1",
		Action:            "answer", Value: answer,
	})
	if err != nil || len(admissions.requests) != 1 {
		t.Fatalf("ContinueCurrentAgent() error=%v admissions=%d", err, len(admissions.requests))
	}
	admission := admissions.requests[0]
	if admission.CurrentContinueTurn == nil ||
		admission.CurrentContinueTurn.InterruptID != resolver.continuationTarget.InterruptID ||
		admission.CurrentContinueTurn.Action != "answer" ||
		admission.Input.GetHitlAction() != "answer" ||
		admission.Input.GetHitlValue() != answer {
		t.Fatalf("admission=%+v input=%+v", admission.CurrentContinueTurn, admission.Input)
	}
	var decisions []map[string]string
	if err := json.Unmarshal(admission.Input.HitlDecisions, &decisions); err != nil || len(decisions) != 1 ||
		decisions[0]["interrupt_id"] != resolver.continuationTarget.InterruptID ||
		decisions[0]["action"] != "answer" || decisions[0]["value"] != answer {
		t.Fatalf("decisions=%s error=%v", admission.Input.HitlDecisions, err)
	}
}

func TestCurrentApplicationContinuationCarriesOneAtomicDecisionPerPendingInterrupt(t *testing.T) {
	interrupts := []CurrentHITLInterrupt{
		{InterruptID: "interrupt-delete-1", AvailableActions: []string{"approve", "reject"}},
		{InterruptID: "interrupt-delete-2", AvailableActions: []string{"approve", "reject", "block_with_comment"}},
	}
	resolver := &currentApplicationResolverStub{
		continuationTarget: CurrentContinuationTarget{
			Kind: CurrentRegenerationApplication, TargetParticipantID: 21,
			QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
			UserInput:  "handle both deletes", ThreadID: "thread-current-parallel-1",
			ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
			InterruptID:         interrupts[0].InterruptID, AvailableActions: interrupts[0].AvailableActions,
			HITLInterrupts: interrupts,
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
		ExecutionID: "execution-parallel-hitl", CommandID: "command-parallel-hitl", Created: true,
	}}
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}
	want := []CurrentHITLDecision{
		{InterruptID: interrupts[0].InterruptID, Action: "approve"},
		{InterruptID: interrupts[1].InterruptID, ToolCallID: "tool-delete-2", Action: "block_with_comment", Value: "archive first"},
	}
	// Card reconstruction order is not part of the durable request identity.
	provided := []CurrentHITLDecision{want[1], want[0]}
	_, err = service.ContinueCurrentAgent(context.Background(), CurrentContinuationRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID:  "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a",
		ThreadID:          "thread-current-parallel-1", HITLDecisions: provided,
	})
	if err != nil || len(admissions.requests) != 1 {
		t.Fatalf("ContinueCurrentAgent() error=%v admissions=%d", err, len(admissions.requests))
	}
	admission := admissions.requests[0]
	if admission.Input.HitlAction != nil || admission.Input.HitlValue != nil ||
		admission.CurrentContinueTurn == nil || admission.CurrentContinueTurn.InterruptID != "" ||
		admission.CurrentContinueTurn.Action != "" {
		t.Fatalf("input=%+v turn=%+v", admission.Input, admission.CurrentContinueTurn)
	}
	var got []CurrentHITLDecision
	if err := json.Unmarshal(admission.Input.HitlDecisions, &got); err != nil || !reflect.DeepEqual(got, want) ||
		!jsonEqual(admission.CurrentContinueTurn.HITLDecisions, admission.Input.HitlDecisions) {
		t.Fatalf("decisions=%s turn=%s error=%v", admission.Input.HitlDecisions, admission.CurrentContinueTurn.HITLDecisions, err)
	}
}

func TestCurrentApplicationContinuationRejectsIncompleteOrAmbiguousParallelDecisionSet(t *testing.T) {
	interrupts := []CurrentHITLInterrupt{
		{InterruptID: "interrupt-1", AvailableActions: []string{"approve", "reject"}},
		{InterruptID: "interrupt-2", AvailableActions: []string{"approve", "reject"}},
	}
	resolver := &currentApplicationResolverStub{continuationTarget: CurrentContinuationTarget{
		Kind: CurrentRegenerationAdhoc, TargetParticipantID: 21,
		QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e", UserInput: "parallel",
		ThreadID: "thread-current-parallel-1", ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
		InterruptID: interrupts[0].InterruptID, AvailableActions: interrupts[0].AvailableActions,
		HITLInterrupts: interrupts,
	}}
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{}, &currentApplicationAdmissionStub{})
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		decisions []CurrentHITLDecision
		wantErr   error
	}{
		"incomplete": {decisions: []CurrentHITLDecision{{InterruptID: "interrupt-1", Action: "approve"}}, wantErr: ErrUnsupportedCurrentAgentStart},
		"duplicate": {decisions: []CurrentHITLDecision{
			{InterruptID: "interrupt-1", Action: "approve"},
			{InterruptID: "interrupt-1", Action: "reject"},
		}, wantErr: ErrInvalidCurrentAgentStart},
		"unknown": {decisions: []CurrentHITLDecision{
			{InterruptID: "interrupt-1", Action: "approve"},
			{InterruptID: "interrupt-other", Action: "reject"},
		}, wantErr: ErrUnsupportedCurrentAgentStart},
	} {
		t.Run(name, func(t *testing.T) {
			_, gotErr := service.ContinueCurrentAgent(context.Background(), CurrentContinuationRequest{
				ProjectID: 7, ActorUserID: 11,
				ConversationUUID:  "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
				ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a",
				HITLDecisions:     test.decisions,
			})
			if gotErr != test.wantErr {
				t.Fatalf("error=%v", gotErr)
			}
		})
	}
}

func TestCurrentApplicationAuthorizationContinuationCarriesOnlyRuntimeCredentials(t *testing.T) {
	resolver := &currentApplicationResolverStub{
		continuationTarget: CurrentContinuationTarget{
			ContinuationKind: CurrentContinuationAuthorization,
			Kind:             CurrentRegenerationApplication, TargetParticipantID: 21,
			QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
			UserInput:  "list SharePoint sites", ThreadID: "thread-current-1",
			ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
			InterruptID:         "tool-run-sharepoint-1",
			ToolCallID:          "call-sharepoint-search-1",
			AvailableActions:    []string{"authorize", "skip"},
			AuthorizationRequests: []CurrentAuthorizationRequest{{
				InterruptID: "tool-run-sharepoint-1", ToolCallID: "call-sharepoint-search-1",
				AvailableActions: []string{"authorize", "skip"},
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
		ExecutionID: "execution-authorization", CommandID: "command-authorization", Created: true,
	}}
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}
	tokens := json.RawMessage(`{"https://sharepoint.example.test":{"access_token":"runtime-secret"}}`)
	ignored := json.RawMessage(`[]`)
	declined := json.RawMessage(`[{"server_url":"https://other.example.test"}]`)
	request := CurrentContinuationRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID:   "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		ResponseMessageID:  "30e0913e-10d4-43db-b8d0-c7b79480935a",
		ThreadID:           "thread-current-1",
		Kind:               CurrentContinuationAuthorization,
		AuthorizationID:    "tool-run-sharepoint-1",
		Action:             "authorize",
		MCPTokens:          tokens,
		IgnoredMCPServers:  ignored,
		DeclinedMCPServers: declined,
	}

	_, err = service.ContinueCurrentAgent(context.Background(), request)
	if err != nil || len(admissions.requests) != 1 {
		t.Fatalf("ContinueCurrentAgent() error=%v admissions=%d", err, len(admissions.requests))
	}
	admission := admissions.requests[0]
	turn := admission.CurrentContinueTurn
	if turn == nil || turn.ContinuationKind != CurrentContinuationAuthorization ||
		turn.InterruptID != request.AuthorizationID || turn.Action != "authorize" ||
		turn.ThreadID != request.ThreadID || admission.SIOEvent != "chat_continue_predict" {
		t.Fatalf("admission=%+v turn=%+v", admission, turn)
	}
	input := admission.Input
	if !input.ShouldContinue || !input.HitlResume || input.GetHitlAction() != "authorize" ||
		input.HitlValue == nil || input.GetHitlValue() != "" ||
		!jsonEqual(input.McpTokens, tokens) ||
		!jsonEqual(input.IgnoredMcpServers, ignored) ||
		!jsonEqual(input.UserDeclinedMcpServers, declined) {
		t.Fatalf("input=%+v", input)
	}
	var decisions []CurrentHITLDecision
	if json.Unmarshal(input.HitlDecisions, &decisions) != nil || len(decisions) != 1 ||
		decisions[0].InterruptID != request.AuthorizationID ||
		decisions[0].ToolCallID != "call-sharepoint-search-1" ||
		decisions[0].GuardrailType != "mcp_auth" || decisions[0].Action != "authorize" {
		t.Fatalf("normalized authorization decisions=%s", input.HitlDecisions)
	}
	if err := validateAuthoritativeInput(input); err != nil {
		t.Fatalf("authorization continuation must pass authoritative admission validation: %v", err)
	}
}

func TestCurrentApplicationAuthorizationContinuationCarriesExactCompleteDecisionSet(t *testing.T) {
	resolver := &currentApplicationResolverStub{
		continuationTarget: CurrentContinuationTarget{
			ContinuationKind: CurrentContinuationAuthorization,
			Kind:             CurrentRegenerationApplication, TargetParticipantID: 21,
			QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
			UserInput:  "resolve records", ThreadID: "thread-current-1",
			ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
			AuthorizationRequests: []CurrentAuthorizationRequest{
				{InterruptID: "auth-2", ToolCallID: "call-2", AvailableActions: []string{"authorize", "skip"}},
				{InterruptID: "auth-1", ToolCallID: "call-1", AvailableActions: []string{"authorize", "skip"}},
			},
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
		ExecutionID: "execution-parallel-authorization", CommandID: "command-parallel-authorization", Created: true,
	}}
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{}, admissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	request := CurrentContinuationRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID:  "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a",
		Kind:              CurrentContinuationAuthorization,
		HITLDecisions: []CurrentHITLDecision{
			{InterruptID: "auth-2", GuardrailType: "mcp_auth", Action: "skip"},
			{InterruptID: "auth-1", GuardrailType: "mcp_auth", Action: "authorize"},
		},
		MCPTokens:         json.RawMessage(`{"https://records.example.test":{"access_token":"runtime-secret"}}`),
		IgnoredMCPServers: json.RawMessage(`[]`), DeclinedMCPServers: json.RawMessage(`[]`),
	}

	_, err = service.ContinueCurrentAgent(context.Background(), request)
	if err != nil || len(admissions.requests) != 1 {
		t.Fatalf("ContinueCurrentAgent() error=%v admissions=%d", err, len(admissions.requests))
	}
	admission := admissions.requests[0]
	turn := admission.CurrentContinueTurn
	if turn == nil || turn.InterruptID != "" || turn.Action != "" ||
		turn.ContinuationKind != CurrentContinuationAuthorization {
		t.Fatalf("turn=%+v", turn)
	}
	if admission.Input.HitlAction != nil || admission.Input.HitlValue != nil {
		t.Fatalf("plural authorization must not mint a scalar alias: %+v", admission.Input)
	}
	var decisions []CurrentHITLDecision
	if json.Unmarshal(admission.Input.HitlDecisions, &decisions) != nil || len(decisions) != 2 ||
		decisions[0].InterruptID != "auth-1" || decisions[0].ToolCallID != "call-1" ||
		decisions[1].InterruptID != "auth-2" || decisions[1].ToolCallID != "call-2" {
		t.Fatalf("normalized authorization decisions=%s", admission.Input.HitlDecisions)
	}
	if err := turn.Validate(); err != nil {
		t.Fatalf("plural authorization turn must remain authoritative: %v", err)
	}
}

func TestCurrentAuthorizationContinuationRejectsDifferentInvocation(t *testing.T) {
	resolver := &currentApplicationResolverStub{continuationTarget: CurrentContinuationTarget{
		ContinuationKind: CurrentContinuationAuthorization,
		Kind:             CurrentRegenerationApplication, TargetParticipantID: 21,
		QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e", UserInput: "authorize toolkit",
		ThreadID: "thread-current-1", ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
		InterruptID: "tool-run-current", AvailableActions: []string{"authorize", "skip"},
		AuthorizationRequests: []CurrentAuthorizationRequest{{
			InterruptID: "tool-run-current", AvailableActions: []string{"authorize", "skip"},
		}},
	}}
	admissions := &currentApplicationAdmissionStub{}
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.ContinueCurrentAgent(context.Background(), CurrentContinuationRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID:   "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		ResponseMessageID:  "30e0913e-10d4-43db-b8d0-c7b79480935a",
		Kind:               CurrentContinuationAuthorization,
		AuthorizationID:    "tool-run-stale",
		Action:             "skip",
		MCPTokens:          json.RawMessage(`{}`),
		IgnoredMCPServers:  json.RawMessage(`[]`),
		DeclinedMCPServers: json.RawMessage(`[]`),
	})
	if err != ErrUnsupportedCurrentAgentStart || len(admissions.requests) != 0 {
		t.Fatalf("error=%v admissions=%d", err, len(admissions.requests))
	}
}

func jsonEqual(left, right json.RawMessage) bool {
	var leftValue, rightValue any
	return json.Unmarshal(left, &leftValue) == nil && json.Unmarshal(right, &rightValue) == nil &&
		reflect.DeepEqual(leftValue, rightValue)
}

func TestCurrentContinuationRejectsUnavailableActionBeforeAdmission(t *testing.T) {
	resolver := &currentApplicationResolverStub{continuationTarget: CurrentContinuationTarget{
		Kind: CurrentRegenerationApplication, TargetParticipantID: 21,
		QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e", UserInput: "sensitive action",
		ThreadID: "thread-current-1", ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
		InterruptID: "interrupt-sensitive-1", AvailableActions: []string{"approve", "reject"},
		HITLInterrupts: []CurrentHITLInterrupt{{
			InterruptID: "interrupt-sensitive-1", AvailableActions: []string{"approve", "reject"},
		}},
	}}
	admissions := &currentApplicationAdmissionStub{}
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.ContinueCurrentAgent(context.Background(), CurrentContinuationRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID:  "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a",
		Action:            "edit", Value: "replacement",
	})
	if err != ErrUnsupportedCurrentAgentStart || len(admissions.requests) != 0 {
		t.Fatalf("error=%v admissions=%d", err, len(admissions.requests))
	}
}

func TestCurrentOutputContinuationAcceptsLargePartialAndBoundsBytes(t *testing.T) {
	target := CurrentContinuationTarget{
		ContinuationKind: CurrentContinuationOutputLimit,
		Kind:             CurrentRegenerationAdhoc, TargetParticipantID: 21,
		QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e", UserInput: "Continue the report",
		ThreadID: "thread-output-large", ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
		OutputLimitSequence: 1,
	}
	for _, size := range []int{65537, 4 * 1024 * 1024} {
		target.TruncatedContent = strings.Repeat("x", size)
		if err := target.Validate(); err != nil {
			t.Fatalf("valid partial with %d bytes: %v", size, err)
		}
	}
	for _, partial := range []string{strings.Repeat("x", 4*1024*1024+1), "invalid\x00partial", strings.Repeat("é", 2*1024*1024) + "x"} {
		target.TruncatedContent = partial
		if err := target.Validate(); err != ErrUnsupportedCurrentAgentStart {
			t.Fatalf("invalid partial with %d bytes: %v", len(partial), err)
		}
	}
}
