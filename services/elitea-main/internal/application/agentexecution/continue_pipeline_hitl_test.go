package agentexecution

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

// The card a PIPELINE attached to an agent as a tool raises (#973).
//
// It is not a `ToolConfirmationRequest`: the pause happens inside the child
// pipeline's own graph, so the card offers the `hitl` node's configured routes
// (approve / reject / edit) and names no `tool_call_id`. Main does not need to
// know any of that — it stores the card's own `available_actions` and replays
// the answer — and these pin that the continuation path really carries that
// shape, because the worker's resume binds the decision to the pause by
// `interrupt_id` alone.
func pipelineHITLResolver(actions []string) *currentApplicationResolverStub {
	return &currentApplicationResolverStub{
		continuationTarget: CurrentContinuationTarget{
			Kind: CurrentRegenerationApplication, TargetParticipantID: 21,
			QuestionID:          "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
			UserInput:           "review the release note",
			ThreadID:            "thread-current-1",
			ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
			InterruptID:         "pipeline-hitl-1",
			AvailableActions:    actions,
			HITLInterrupts: []CurrentHITLInterrupt{{
				InterruptID: "pipeline-hitl-1", AvailableActions: actions,
			}},
		},
		target: CurrentApplicationTarget{
			ApplicationID: 31, ApplicationVersionID: 41,
			Variables: json.RawMessage(`[]`),
			VersionDetails: json.RawMessage(`{
  "id":41,"application_id":31,"agent_type":"agent","instructions":"Coordinate work",
  "llm_settings":{"model_name":"test","model_project_id":7,"openai_compatible":false},
  "meta":{},"tools":[]
}`),
			ChatHistory: json.RawMessage(`[]`),
		},
	}
}

func TestCurrentContinuationCarriesAPipelineNodeDecisionWithoutAToolCall(t *testing.T) {
	for _, test := range []struct {
		name   string
		action string
		value  string
	}{
		{name: "approve", action: "approve"},
		{name: "reject", action: "reject"},
		{name: "edit", action: "edit", value: "ship on Monday instead"},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolver := pipelineHITLResolver([]string{"approve", "reject", "edit"})
			admittedAt := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
			admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
				ExecutionID: "execution-pipeline-hitl", CommandID: "command-pipeline-hitl",
				Created: true, AdmittedAt: admittedAt, Deadline: admittedAt.Add(time.Minute),
			}}
			service, err := NewCurrentApplicationStartService(
				resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
				&currentApplicationVersionFreezerStub{}, admissions)
			if err != nil {
				t.Fatal(err)
			}

			outcome, err := service.ContinueCurrentAgent(context.Background(), CurrentContinuationRequest{
				ProjectID: 7, ActorUserID: 11,
				ConversationUUID:  "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
				ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a",
				ThreadID:          "thread-current-1",
				Action:            test.action, Value: test.value,
			})
			if err != nil || len(admissions.requests) != 1 {
				t.Fatalf("ContinueCurrentAgent() error=%v admissions=%d", err, len(admissions.requests))
			}
			if outcome.ExecutionID != "execution-pipeline-hitl" {
				t.Fatalf("outcome=%+v", outcome)
			}

			admission := admissions.requests[0]
			if admission.CapabilityID != executiondomain.AgentApplicationCapability ||
				admission.SIOEvent != "chat_continue_predict" {
				t.Fatalf("admission=%+v", admission)
			}
			input := admission.Input
			if !input.HitlResume || !input.ShouldContinue ||
				input.GetHitlAction() != test.action || input.GetHitlValue() != test.value ||
				input.GetThreadId() != resolver.continuationTarget.ThreadID {
				t.Fatalf("input=%+v", input)
			}
			// No `checkpoint_id` is sent, and none is needed: the child's
			// checkpoint identity never leaves the worker, which binds the
			// decision to the pause it persisted on its own event.
			if input.CheckpointId != nil {
				t.Fatalf("a pipeline-node decision must not select a checkpoint: %v", input.CheckpointId)
			}

			var decisions []map[string]any
			if err := json.Unmarshal(input.HitlDecisions, &decisions); err != nil || len(decisions) != 1 {
				t.Fatalf("decisions=%s error=%v", input.HitlDecisions, err)
			}
			decision := decisions[0]
			if decision["interrupt_id"] != "pipeline-hitl-1" || decision["action"] != test.action {
				t.Fatalf("decision=%v", decision)
			}
			if _, named := decision["tool_call_id"]; named {
				t.Fatalf("a pipeline `hitl` node names no tool call: %v", decision)
			}
			if test.value == "" {
				if _, carried := decision["value"]; carried {
					t.Fatalf("an empty value must not be carried: %v", decision)
				}
			} else if decision["value"] != test.value {
				t.Fatalf("decision=%v", decision)
			}
		})
	}
}

// A pipeline `hitl` node publishes only the routes it configured. An answer
// naming a route the card did not offer is refused before admission, so the
// worker never has to decide what an unrouted action means.
func TestCurrentContinuationRefusesARouteThePipelineCardDidNotOffer(t *testing.T) {
	resolver := pipelineHITLResolver([]string{"approve", "reject"})
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
		ThreadID:          "thread-current-1",
		Action:            "edit", Value: "a route this node has no edge for",
	})
	if err != ErrUnsupportedCurrentAgentStart || len(admissions.requests) != 0 {
		t.Fatalf("error=%v admissions=%d", err, len(admissions.requests))
	}
}
