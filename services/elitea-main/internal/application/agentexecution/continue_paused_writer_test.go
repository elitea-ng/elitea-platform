package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
)

// FU-265: a continuation's command names the paused writer the resolver
// read, in a meta key of its own, and the admitted turn carries it for the
// admission transaction to re-read under the response lock.
func TestContinuationCommandNamesThePausedWriter(t *testing.T) {
	resolver := &currentApplicationResolverStub{
		continuationTarget: CurrentContinuationTarget{
			Kind: CurrentRegenerationApplication, TargetParticipantID: 21,
			QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
			UserInput:  "delete the stale branch", ThreadID: "thread-current-1",
			ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
			InterruptID:         "interrupt-sensitive-1",
			AvailableActions:    []string{"approve", "reject"},
			HITLInterrupts: []CurrentHITLInterrupt{{
				InterruptID: "interrupt-sensitive-1", AvailableActions: []string{"approve", "reject"},
			}},
			PausedExecutionID: "0123456789abcdef0123456789abcdef", PausedGeneration: 3,
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
	admittedAt := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-continue", CommandID: "command-continue", Created: true,
		AdmittedAt: admittedAt, Deadline: admittedAt.Add(time.Minute),
	}}
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}
	// Off (an older Worker may still run): no marker.
	request0 := CurrentContinuationRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID:  "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a",
		ThreadID:          "thread-current-1", Action: "approve",
	}
	if _, err := service.ContinueCurrentAgent(context.Background(), request0); err != nil {
		t.Fatalf("naming off: %v", err)
	}
	if meta := admissions.requests[0].Input.GetMeta(); len(meta) != 0 && string(meta) != "{}" && json.Valid(meta) {
		var keys map[string]json.RawMessage
		_ = json.Unmarshal(meta, &keys)
		if _, named := keys[PausedWriterMetaKey]; named {
			t.Fatalf("naming off sent a marker: %s", meta)
		}
	}
	if turn := admissions.requests[0].CurrentContinueTurn; turn.PausedExecutionID != "" {
		t.Fatalf("naming off: turn %+v", turn)
	}
	admissions.requests = nil
	service = service.WithContinuationPausedWriter(true)
	request := CurrentContinuationRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID:  "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a",
		ThreadID:          "thread-current-1", Action: "approve",
	}
	if _, err := service.ContinueCurrentAgent(context.Background(), request); err != nil {
		t.Fatalf("ContinueCurrentAgent() error = %v", err)
	}
	admission := admissions.requests[0]
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(admission.Input.GetMeta(), &meta); err != nil {
		t.Fatal(err)
	}
	if got := string(meta[PausedWriterMetaKey]); got != `{"execution_id":"0123456789abcdef0123456789abcdef","generation":3}` {
		t.Fatalf("marker = %s", got)
	}
	turn := admission.CurrentContinueTurn
	if turn.PausedExecutionID != "0123456789abcdef0123456789abcdef" || turn.PausedGeneration != 3 {
		t.Fatalf("turn = %+v", turn)
	}

	// A target without a paused writer is never admitted without one.
	resolver.continuationTarget.PausedGeneration = 0
	admissions.requests = nil
	if _, err := service.ContinueCurrentAgent(context.Background(), request); !errors.Is(err, ErrUnsupportedCurrentAgentStart) {
		t.Fatalf("missing paused writer: %v", err)
	}
	if len(admissions.requests) != 0 {
		t.Fatal("a continuation without its paused writer was admitted")
	}
}

func TestOutputLimitContinuationNamesNoPausedWriterAndAWakeDropsIt(t *testing.T) {
	input := &runtimev1.AgentExecutionInputV1{}
	turn := &CurrentContinueTurn{ContinuationKind: CurrentContinuationOutputLimit}
	service := (&CurrentApplicationStartService{}).WithContinuationPausedWriter(true)
	if err := service.namePausedWriter(input, turn, CurrentContinuationTarget{}); err != nil || len(input.Meta) != 0 {
		t.Fatalf("output limit: %v %s", err, input.Meta)
	}
	if !slices.Contains(fanoutWakeResumeMarkers, PausedWriterMetaKey) {
		t.Fatal("a wake derived from a paused continuation's frozen input keeps its paused writer")
	}
}
