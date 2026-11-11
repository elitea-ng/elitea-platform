package agentexecution

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/contextview"
)

// reseedFollowupTarget is an application whose view holds one message.
func reseedFollowupTarget() CurrentApplicationTarget {
	return CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41,
		Variables: json.RawMessage(`[]`),
		VersionDetails: json.RawMessage(`{
  "id":41,"application_id":31,"agent_type":"agent","instructions":"Be careful",
  "llm_settings":{"model_name":"test","model_project_id":7,"openai_compatible":false},
  "meta":{"context":{"history":"since_joined"}},"tools":[]
}`),
		ChatHistory: json.RawMessage(`[{
  "role":"user","content":[{"type":"text","text":"posted while paused"}],"additional_kwargs":{}
}]`),
		ChatHistoryRefs:     []ChatHistoryRef{{MessageID: "00000000-0000-4000-8000-000000000071", AfterJoined: true}},
		ParticipantJoinedAt: "2026-11-11 09:00:00",
	}
}

func reseedFollowupService(t *testing.T, resolver *currentApplicationResolverStub) (*CurrentApplicationStartService, *currentApplicationAdmissionStub) {
	t.Helper()
	admittedAt := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-followup", CommandID: "command-followup", Created: true,
		AdmittedAt: admittedAt, Deadline: admittedAt.Add(time.Minute),
	}}
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{},
		&currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}
	return service, admissions
}

// Issue 1892: a HITL-resumed turn rebuilds its input from the conversation
// as it stands, so the turn carries that input's view for the admission to
// re-record, with the digest the worker checks on the continued run.
func TestContinuationCarriesTheContinuedRunsView(t *testing.T) {
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
		},
		target: reseedFollowupTarget(),
	}
	service, admissions := reseedFollowupService(t, resolver)
	_, err := service.ContinueCurrentAgent(context.Background(), CurrentContinuationRequest{
		ProjectID: 7, ActorUserID: 11,
		ConversationUUID:  "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a",
		ThreadID:          "thread-current-1", Action: "approve",
	})
	if err != nil {
		t.Fatalf("ContinueCurrentAgent() error = %v", err)
	}
	admission := admissions.requests[0]
	record := admission.CurrentContinueTurn.ContextView
	input := admission.Input
	if record == nil || record.ReaderParticipantID != 21 ||
		record.Digest != input.GetContextView().GetDigest() ||
		record.Settings.History != contextview.HistorySinceJoined ||
		len(record.Entries) != 1 || record.Entries[0].MessageID != "00000000-0000-4000-8000-000000000071" {
		t.Fatalf("continued view record=%+v input view=%v", record, input.GetContextView())
	}
	if input.GetContextView().GetBinding() != record.Settings.Binding("2026-11-11 09:00:00") {
		t.Fatalf("binding %q", input.GetContextView().GetBinding())
	}
	if clone := admission.CurrentContinueTurn.Clone(); clone.ContextView == record {
		t.Fatal("Clone must copy the view record")
	}
}

// Issue 1903: a regeneration names the question its own turn wrote, so the
// worker tags it; an addressed agent's answer, which reused the person's
// question, names none, as on its first run.
func TestRegenerationNamesItsOwnQuestion(t *testing.T) {
	for _, own := range []bool{true, false} {
		resolver := &currentApplicationResolverStub{
			regenerationTarget: CurrentRegenerationTarget{
				Kind:                CurrentRegenerationApplication,
				ConversationUUID:    "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
				TargetParticipantID: 21,
				QuestionID:          "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
				UserInput:           "the question",
				QuestionIsOwn:       own,
			},
			target: reseedFollowupTarget(),
		}
		service, admissions := reseedFollowupService(t, resolver)
		if _, err := service.RegenerateCurrentAgent(context.Background(), validCurrentRegenerationRequest()); err != nil {
			t.Fatalf("own=%v: %v", own, err)
		}
		input := admissions.requests[0].Input
		if own && input.GetCurrentMessageId() != "ee92ccbd-3312-4c72-b20b-fddf224e7c0e" {
			t.Fatalf("own question: current_message_id %q", input.GetCurrentMessageId())
		}
		if !own && input.CurrentMessageId != nil {
			t.Fatalf("reused question: current_message_id %q", input.GetCurrentMessageId())
		}
		if !input.IsRegenerate {
			t.Fatal("not a regeneration")
		}
	}
}
