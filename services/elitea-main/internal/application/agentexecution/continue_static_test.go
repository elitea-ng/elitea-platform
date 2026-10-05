package agentexecution

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	"google.golang.org/protobuf/proto"
)

func staticProofFixture() CurrentPipelineStaticProof {
	return CurrentPipelineStaticProof{Revision: 1, PauseID: "pipeline-static:sha256:" + strings.Repeat("a", 64), CheckpointID: "checkpoint-1", Kind: "before", NodeName: "tick", DefinitionDigest: "sha256:" + strings.Repeat("b", 64), NodeDigest: "sha256:" + strings.Repeat("c", 64), PendingNodes: []string{"tick"}, Step: 7, DescendantPath: []CurrentStaticDescendant{}}
}
func staticInputFixture(t *testing.T) (*CurrentPipelineStaticPause, *runtimev1.AgentExecutionInputV1) {
	t.Helper()
	conversation := "8bc66e50-46c4-4e2c-94ec-daec6c596ac0"
	generation := "9fba0a08-5049-42bb-9019-c2f3df686010"
	thread := "thread-current-1"
	input := &runtimev1.AgentExecutionInputV1{SchemaRevision: "elitea.runtime.agent-execution-input.v1", ConversationId: &conversation, ExecutionGeneration: &generation, ThreadId: &thread,
		Application: []byte(`{"id":31,"version_id":41,"variables":[],"version_details":{"id":41,"application_id":31,"agent_type":"pipeline","instructions":"interrupt_before: [tick]\nstate:\n  count: {type: int, value: 0}\nentry_point: tick\nnodes:\n  - id: tick\n    type: state_modifier\n    template: '{{ count + 1 }}'\n    input: [count]\n    output: [count]\n    transition: END","llm_settings":{"model_name":"test","model_project_id":7,"openai_compatible":false},"meta":{},"tools":[]}}`),
		UserInput:   []byte(`"original task"`), InternalTools: []byte(`[]`), Meta: []byte(`{}`), ChatHistory: []byte(`[]`), HitlDecisions: []byte(`[]`)}
	encoded, err := proto.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	pause, err := DecodeCurrentStaticInput(encoded, digest[:], staticProofFixture(), conversation, generation, thread)
	if err != nil {
		t.Fatal(err)
	}
	return pause, input
}
func TestStaticProofRejectsStaleKindFrontierAndDescendantIdentity(t *testing.T) {
	proof := staticProofFixture()
	raw, _ := json.Marshal(proof)
	if _, err := ParseCurrentPipelineStaticProof(raw, "thread-current-1"); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*CurrentPipelineStaticProof){
		func(p *CurrentPipelineStaticProof) { p.Revision = 2 }, func(p *CurrentPipelineStaticProof) { p.Kind = "dynamic" },
		func(p *CurrentPipelineStaticProof) { p.PendingNodes = []string{"successor"} }, func(p *CurrentPipelineStaticProof) { p.NodeDigest = "sha256:old" },
		func(p *CurrentPipelineStaticProof) {
			p.DescendantPath = []CurrentStaticDescendant{{NodeName: "delegate", ThreadID: "other/delegate", CheckpointID: "child"}}
		},
		func(p *CurrentPipelineStaticProof) { p.PendingNodes = []string{"tick", "tick"} },
	} {
		changed := proof
		change(&changed)
		raw, _ := json.Marshal(changed)
		if _, err := ParseCurrentPipelineStaticProof(raw, "thread-current-1"); err == nil {
			t.Fatal("invalid static identity accepted")
		}
	}
	for _, raw := range []string{string(raw) + ` {}`, strings.TrimSuffix(string(raw), "}") + `,"state":{"secret":"hidden"}}`} {
		if _, err := ParseCurrentPipelineStaticProof([]byte(raw), "thread-current-1"); err == nil {
			t.Fatal("extra checkpoint data accepted")
		}
	}
}
func TestStaticInputRequiresTheOriginalDigestVersionAndExecution(t *testing.T) {
	_, input := staticInputFixture(t)
	encoded, _ := proto.Marshal(input)
	digest := sha256.Sum256(encoded)
	for _, change := range []func(){func() { encoded[len(encoded)-1] ^= 1 }, func() { digest[0] ^= 1 }} {
		oldBytes := append([]byte{}, encoded...)
		oldDigest := digest
		change()
		if _, err := DecodeCurrentStaticInput(encoded, digest[:], staticProofFixture(), input.GetConversationId(), input.GetExecutionGeneration(), input.GetThreadId()); err == nil {
			t.Fatal("changed immutable input accepted")
		}
		encoded = oldBytes
		digest = oldDigest
	}
	if _, err := DecodeCurrentStaticInput(encoded, digest[:], staticProofFixture(), input.GetConversationId(), "different-generation", input.GetThreadId()); err == nil {
		t.Fatal("different generation accepted")
	}
	input.Application = []byte(`{"id":31,"version_id":42,"version_details":{"id":41,"application_id":31,"agent_type":"pipeline"}}`)
	encoded, _ = proto.Marshal(input)
	digest = sha256.Sum256(encoded)
	if _, err := DecodeCurrentStaticInput(encoded, digest[:], staticProofFixture(), input.GetConversationId(), input.GetExecutionGeneration(), input.GetThreadId()); err == nil {
		t.Fatal("different frozen version accepted")
	}
}
func TestStaticRequestKeepsTextAndHITLActionsSeparate(t *testing.T) {
	request := CurrentContinuationRequest{ProjectID: 7, ActorUserID: 11, ConversationUUID: "8bc66e50-46c4-4e2c-94ec-daec6c596ac0", ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a", Kind: CurrentContinuationStatic, StaticPauseID: staticProofFixture().PauseID, StaticInputText: "continue"}
	if request.Validate() != nil {
		t.Fatal("valid static request rejected")
	}
	for _, change := range []func(*CurrentContinuationRequest){func(r *CurrentContinuationRequest) { r.Action = "approve" }, func(r *CurrentContinuationRequest) { r.StaticInputText = " " }, func(r *CurrentContinuationRequest) { r.StaticInputText = strings.Repeat("x", 8193) }, func(r *CurrentContinuationRequest) {
		r.HITLDecisions = []CurrentHITLDecision{{InterruptID: "dynamic", Action: "answer", Value: "go"}}
	}} {
		changed := request
		change(&changed)
		if changed.Validate() == nil {
			t.Fatal("mixed static request accepted")
		}
	}
}
func TestStaticContinuationReusesFrozenInputWithoutAHITLAction(t *testing.T) {
	pause, input := staticInputFixture(t)
	resolver := pipelineHITLResolver([]string{"approve"})
	resolver.continuationTarget = CurrentContinuationTarget{ContinuationKind: CurrentContinuationStatic, Kind: CurrentRegenerationApplication, TargetParticipantID: 21, QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e", UserInput: "original task", ThreadID: input.GetThreadId(), ExecutionGeneration: input.GetExecutionGeneration(), PipelineStaticPause: pause}
	var app struct {
		VersionDetails json.RawMessage `json:"version_details"`
	}
	_ = json.Unmarshal(input.Application, &app)
	resolver.target.VersionDetails = app.VersionDetails
	admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{ExecutionID: "static-resume", CommandID: "static-command", Created: true}}
	service, err := NewCurrentApplicationStartService(resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{}, &currentApplicationVersionFreezerStub{}, admissions)
	if err != nil {
		t.Fatal(err)
	}
	request := CurrentContinuationRequest{ProjectID: 7, ActorUserID: 11, ConversationUUID: input.GetConversationId(), ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a", Kind: CurrentContinuationStatic, StaticPauseID: pause.Proof.PauseID, StaticInputText: "go again"}
	outcome, err := service.ContinueCurrentAgent(context.Background(), request)
	if err != nil || len(admissions.requests) != 1 {
		t.Fatalf("err=%v admissions=%d", err, len(admissions.requests))
	}
	admitted := admissions.requests[0]
	got := admitted.Input
	if !got.ShouldContinue || got.HitlResume || got.HitlAction != nil || got.CheckpointId != nil || string(got.UserInput) != `"go again"` || outcome.ResponseMessageID != request.ResponseMessageID {
		t.Fatalf("static semantics lost: %+v", got)
	}
	if admitted.CurrentContinueTurn.Validate() != nil || admitted.CurrentContinueTurn.PipelineStaticPause.Proof.PauseID != pause.Proof.PauseID {
		t.Fatal("frozen turn proof lost")
	}
	if string(input.UserInput) != `"original task"` {
		t.Fatal("original command was mutated")
	}
	clone := admitted.CurrentContinueTurn.Clone()
	clone.PipelineStaticPause.Proof.PendingNodes[0] = "changed"
	if admitted.CurrentContinueTurn.PipelineStaticPause.Proof.PendingNodes[0] != "tick" {
		t.Fatal("clone changed source proof")
	}
}
func TestStaticContinuationRejectsNewVersionAndDifferentOccurrenceBeforeAdmission(t *testing.T) {
	pause, input := staticInputFixture(t)
	resolver := pipelineHITLResolver([]string{"approve"})
	resolver.continuationTarget = CurrentContinuationTarget{ContinuationKind: CurrentContinuationStatic, Kind: CurrentRegenerationApplication, TargetParticipantID: 21, QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e", UserInput: "original task", ThreadID: input.GetThreadId(), ExecutionGeneration: input.GetExecutionGeneration(), PipelineStaticPause: pause}
	admissions := &currentApplicationAdmissionStub{}
	service, _ := NewCurrentApplicationStartService(resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{}, &currentApplicationVersionFreezerStub{}, admissions)
	request := CurrentContinuationRequest{ProjectID: 7, ActorUserID: 11, ConversationUUID: input.GetConversationId(), ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a", Kind: CurrentContinuationStatic, StaticPauseID: pause.Proof.PauseID, StaticInputText: "continue"}
	resolver.target.ApplicationVersionID = 42
	if _, err := service.ContinueCurrentAgent(context.Background(), request); err == nil || len(admissions.requests) != 0 {
		t.Fatal("changed version admitted")
	}
	resolver.target.ApplicationVersionID = 41
	request.StaticPauseID = "pipeline-static:sha256:" + strings.Repeat("d", 64)
	if _, err := service.ContinueCurrentAgent(context.Background(), request); err == nil || len(admissions.requests) != 0 {
		t.Fatal("different occurrence admitted")
	}
}

func TestStaticLeafSelectionRequiresOriginalChildAndCallAndRetainsUntouchedInventory(t *testing.T) {
	first := CurrentStaticToolPause{ToolCallID: "call-1", ChildThreadID: "child-1", OriginalBatchEventID: "batch-1", OriginalOrdinal: 1, Proof: staticProofFixture()}
	second := first
	second.ToolCallID = "call-2"
	second.ChildThreadID = "child-2"
	second.OriginalOrdinal = 2
	second.Proof.PauseID = "pipeline-static:sha256:" + strings.Repeat("d", 64)
	inventory := CurrentStaticToolInventory{Revision: 1, Pauses: []CurrentStaticToolPause{first, second}}
	raw, _ := json.Marshal(inventory)
	parsed, err := ParseCurrentStaticToolInventory(raw)
	if err != nil {
		t.Fatal(err)
	}
	decision := CurrentStaticLeafDecision{PauseID: first.Proof.PauseID, ChildThreadID: first.ChildThreadID, ToolCallID: first.ToolCallID, Action: "continue", Value: "next"}
	if !staticLeafDecisionsMatch([]CurrentStaticLeafDecision{decision}, *parsed) || len(parsed.Pauses) != 2 {
		t.Fatal("partial leaf choice changed pending inventory")
	}
	for _, change := range []func(*CurrentStaticLeafDecision){func(d *CurrentStaticLeafDecision) { d.ToolCallID = "call-2" }, func(d *CurrentStaticLeafDecision) { d.ChildThreadID = "child-2" }, func(d *CurrentStaticLeafDecision) { d.Action = "approve" }} {
		wrong := decision
		change(&wrong)
		if staticLeafDecisionsMatch([]CurrentStaticLeafDecision{wrong}, inventory) {
			t.Fatal("wrong static leaf accepted")
		}
	}
	if staticLeafDecisionsMatch([]CurrentStaticLeafDecision{decision, decision}, inventory) {
		t.Fatal("duplicate static choice accepted")
	}
}
