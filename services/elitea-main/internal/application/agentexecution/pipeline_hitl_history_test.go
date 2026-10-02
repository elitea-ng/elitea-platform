package agentexecution

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
)

func TestPipelineHITLContinuationBindsNewResponseOnlyForDirectReview(t *testing.T) {
	for _, direct := range []bool{false, true} {
		name := "nested"
		if direct {
			name = "direct"
		}
		t.Run(name, func(t *testing.T) {
			target := pipelineHITLTarget()
			if !direct {
				target.PipelineHITLReview = nil
			}
			resolver := &currentApplicationResolverStub{
				continuationTarget: target,
				target: CurrentApplicationTarget{ApplicationID: 31, ApplicationVersionID: 41,
					Variables: json.RawMessage(`[]`), ChatHistory: json.RawMessage(`[]`),
					VersionDetails: json.RawMessage(`{"id":41,"application_id":31,"agent_type":"pipeline","instructions":"Review the joke","llm_settings":{"model_name":"test","model_project_id":7,"openai_compatible":false},"meta":{},"tools":[]}`)},
			}
			admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
				ExecutionID: "execution-review", CommandID: "command-review", Created: true,
			}}
			service, err := NewCurrentApplicationStartService(resolver, resolver, resolver, resolver, resolver,
				&currentAgentGuardrailStub{}, &currentApplicationVersionFreezerStub{}, admissions)
			if err != nil {
				t.Fatal(err)
			}
			request := CurrentContinuationRequest{ProjectID: 7, ActorUserID: 11,
				ConversationUUID:  "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
				ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a",
				ThreadID:          target.ThreadID, Action: "approve"}
			outcome, err := service.ContinueCurrentAgent(context.Background(), request)
			if err != nil || len(admissions.requests) != 1 {
				t.Fatalf("continuation error=%v admissions=%d", err, len(admissions.requests))
			}
			admission := admissions.requests[0]
			turn := admission.CurrentContinueTurn
			if turn == nil || turn.ResponseMessageID != request.ResponseMessageID {
				t.Fatal("continuation lost its paused response identity")
			}
			if outcome.ResponseMessageID != admission.ClientMessageID || outcome.ResponseMessageID != turn.ProjectionResponseID() {
				t.Fatal("browser and execution projection target different responses")
			}
			if (outcome.ResponseMessageID != request.ResponseMessageID) != direct {
				t.Fatal("response segmentation does not match direct review scope")
			}
			if admission.Input.GetThreadId() != target.ThreadID || admission.Input.GetExecutionGeneration() != target.ExecutionGeneration {
				t.Fatal("history segmentation changed the graph checkpoint identity")
			}
		})
	}
}

func TestPipelineHITLReviewRequiresMatchingApplicationInterrupt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*CurrentContinuationTarget)
	}{
		{"different interrupt", func(v *CurrentContinuationTarget) { v.PipelineHITLReview.InterruptID = "other" }},
		{"multiple interrupts", func(v *CurrentContinuationTarget) {
			v.HITLInterrupts = append(v.HITLInterrupts, CurrentHITLInterrupt{InterruptID: "other", AvailableActions: []string{"approve"}})
		}},
		{"ordinary chat", func(v *CurrentContinuationTarget) { v.Kind = CurrentRegenerationAdhoc }},
		{"authorization", func(v *CurrentContinuationTarget) { v.ContinuationKind = CurrentContinuationAuthorization }},
		{"output continuation", func(v *CurrentContinuationTarget) { v.ContinuationKind = CurrentContinuationOutputLimit }},
		{"missing node", func(v *CurrentContinuationTarget) { v.PipelineHITLReview.NodeName = "" }},
		{"missing content", func(v *CurrentContinuationTarget) { v.PipelineHITLReview.Message = "" }},
		{"invalid content", func(v *CurrentContinuationTarget) { v.PipelineHITLReview.Message = "review\x00" }},
		{"oversized content", func(v *CurrentContinuationTarget) { v.PipelineHITLReview.Message = strings.Repeat("x", 4*1024*1024+1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := pipelineHITLTarget()
			if err := v.Validate(); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&v)
			if v.Validate() == nil {
				t.Fatal("invalid review accepted")
			}
		})
	}
}

func TestPipelineHITLAdmissionPreservesReviewAndDecision(t *testing.T) {
	for _, action := range []string{"approve", "reject", "edit", "answer", "block_with_comment"} {
		t.Run(action, func(t *testing.T) {
			target := pipelineHITLTarget()
			decision := CurrentHITLDecision{InterruptID: "review-1", Action: action}
			if action == "edit" || action == "answer" || action == "block_with_comment" {
				decision.Value = "  Exact edit.\nSecond line.  "
			}
			decisions, err := json.Marshal([]CurrentHITLDecision{decision})
			if err != nil {
				t.Fatal(err)
			}
			turn := &CurrentContinueTurn{ProjectID: 7, ActorUserID: 11, TargetParticipantID: 21,
				ConversationUUID: "8bc66e50-46c4-4e2c-94ec-daec6c596ac0", QuestionID: target.QuestionID,
				ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a", ExecutionGeneration: target.ExecutionGeneration, ThreadID: target.ThreadID,
				Kind: CurrentRegenerationApplication, ApplicationID: 31, ApplicationVersionID: 41,
				InterruptID: decision.InterruptID, Action: action, HITLDecisions: decisions, PipelineHITLReview: target.PipelineHITLReview.clone()}
			valid := action == "approve" || action == "reject" || action == "edit"
			if err := turn.Validate(); (err == nil) != valid {
				t.Fatalf("valid=%t error=%v", valid, err)
			}
			if !valid {
				return
			}
			cloned := turn.Clone()
			turn.PipelineHITLReview.Message = "changed"
			turn.HITLDecisions[0] = 'x'
			if cloned.PipelineHITLReview.Message != target.PipelineHITLReview.Message {
				t.Fatal("review aliases original")
			}
			var restored []CurrentHITLDecision
			if err := json.Unmarshal(cloned.HITLDecisions, &restored); err != nil || len(restored) != 1 || restored[0] != decision {
				t.Fatalf("decision changed: %+v %v", restored, err)
			}
			cloned.PipelineHITLReview.InterruptID = "other"
			if cloned.Validate() == nil {
				t.Fatal("mismatched review admitted")
			}
		})
	}
}

func pipelineHITLTarget() CurrentContinuationTarget {
	return CurrentContinuationTarget{Kind: CurrentRegenerationApplication, TargetParticipantID: 21,
		QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e", UserInput: "Create a joke.", ThreadID: "thread-1", ExecutionGeneration: "9fba0a08-5049-42bb-9019-c2f3df686010",
		HITLInterrupts:     []CurrentHITLInterrupt{{InterruptID: "review-1", AvailableActions: []string{"approve", "reject", "edit"}}},
		PipelineHITLReview: &CurrentPipelineHITLReview{InterruptID: "review-1", NodeName: "review", Message: "Review:\n\n  Preserve this whitespace.  "}}
}
