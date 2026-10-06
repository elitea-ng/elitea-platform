package agentexecution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	"google.golang.org/protobuf/proto"
)

func staticInventoryRootFixture(t *testing.T, kind CurrentStaticInventoryRootKind) (*CurrentPipelineStaticTools, *runtimev1.AgentExecutionInputV1) {
	t.Helper()
	_, input := staticInputFixture(t)
	input.Application = bytes.ReplaceAll(input.Application, []byte(`"agent_type":"pipeline"`), []byte(`"agent_type":"`+string(kind)+`"`))
	inventory := CurrentStaticToolInventory{Revision: 1, Pauses: []CurrentStaticToolPause{{ToolCallID: "original-call", ChildThreadID: "original-child", OriginalBatchEventID: "original-batch", OriginalOrdinal: 16, Proof: staticProofFixture()}}}
	encoded, err := proto.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	tools, err := DecodeCurrentStaticToolInput(encoded, digest[:], inventory, input.GetConversationId(), input.GetExecutionGeneration(), input.GetThreadId())
	if err != nil {
		t.Fatal(err)
	}
	return tools, input
}

func TestStaticInventoryFrozenRootKinds(t *testing.T) {
	for _, kind := range []CurrentStaticInventoryRootKind{CurrentStaticInventoryAgentRoot, CurrentStaticInventoryPipelineRoot} {
		t.Run(string(kind), func(t *testing.T) {
			tools, input := staticInventoryRootFixture(t, kind)
			if tools.RootKind != kind {
				t.Fatalf("kind=%q", tools.RootKind)
			}
			target := CurrentContinuationTarget{Kind: CurrentRegenerationApplication, ThreadID: input.GetThreadId(), ExecutionGeneration: input.GetExecutionGeneration(), PipelineStaticTools: tools}
			if target.validateStaticTools() != nil {
				t.Fatal("original authority rejected")
			}
			clone := tools.clone()
			clone.RootKind = "forged"
			target.PipelineStaticTools = clone
			if target.validateStaticTools() == nil {
				t.Fatal("forged root authority accepted")
			}
			if tools.RootKind != kind {
				t.Fatal("clone mutated original authority")
			}
		})
	}
}

func TestStaticInventoryOriginalInputFences(t *testing.T) {
	tools, input := staticInventoryRootFixture(t, CurrentStaticInventoryPipelineRoot)
	for _, tc := range []struct {
		name   string
		change func(*runtimev1.AgentExecutionInputV1)
	}{
		{"unknown kind", func(i *runtimev1.AgentExecutionInputV1) {
			i.Application = bytes.ReplaceAll(i.Application, []byte(`"agent_type":"pipeline"`), []byte(`"agent_type":"openai"`))
		}},
		{"contradictory kind", func(i *runtimev1.AgentExecutionInputV1) {
			i.Application = bytes.Replace(i.Application, []byte(`{"id":31`), []byte(`{"agent_type":"agent","id":31`), 1)
		}},
		{"missing version kind", func(i *runtimev1.AgentExecutionInputV1) {
			i.Application = bytes.Replace(i.Application, []byte(`"agent_type":"pipeline",`), nil, 1)
		}},
		{"wrong version", func(i *runtimev1.AgentExecutionInputV1) {
			i.Application = bytes.Replace(i.Application, []byte(`"version_id":41`), []byte(`"version_id":42`), 1)
		}},
		{"wrong application", func(i *runtimev1.AgentExecutionInputV1) {
			i.Application = bytes.Replace(i.Application, []byte(`"application_id":31`), []byte(`"application_id":32`), 1)
		}},
		{"wrong conversation", func(i *runtimev1.AgentExecutionInputV1) { v := "different-conversation"; i.ConversationId = &v }},
		{"wrong generation", func(i *runtimev1.AgentExecutionInputV1) { v := "different-generation"; i.ExecutionGeneration = &v }},
		{"wrong thread", func(i *runtimev1.AgentExecutionInputV1) { v := "different-thread"; i.ThreadId = &v }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := proto.Clone(input).(*runtimev1.AgentExecutionInputV1)
			tc.change(changed)
			encoded, err := proto.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(encoded)
			if _, err := DecodeCurrentStaticToolInput(encoded, digest[:], tools.Inventory, input.GetConversationId(), input.GetExecutionGeneration(), input.GetThreadId()); err == nil {
				t.Fatal("unbound input accepted")
			}
		})
	}
	encoded, _ := proto.Marshal(input)
	badDigest := bytes.Clone(tools.InputDigest)
	badDigest[0] ^= 1
	if _, err := DecodeCurrentStaticToolInput(encoded, badDigest, tools.Inventory, input.GetConversationId(), input.GetExecutionGeneration(), input.GetThreadId()); err == nil {
		t.Fatal("forged digest accepted")
	}
}

func TestStaticPipelineInventoryContinuationRetainsOriginalAuthority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*CurrentContinuationRequest, *currentApplicationResolverStub, *currentApplicationVersionFreezerStub)
	}{
		{"original", func(*CurrentContinuationRequest, *currentApplicationResolverStub, *currentApplicationVersionFreezerStub) {
		}},
		{"wrong thread", func(r *CurrentContinuationRequest, _ *currentApplicationResolverStub, _ *currentApplicationVersionFreezerStub) {
			r.ThreadID = "new-thread"
		}},
		{"forged child", func(r *CurrentContinuationRequest, _ *currentApplicationResolverStub, _ *currentApplicationVersionFreezerStub) {
			r.StaticDecisions[0].ChildThreadID = "new-child"
		}},
		{"forged call", func(r *CurrentContinuationRequest, _ *currentApplicationResolverStub, _ *currentApplicationVersionFreezerStub) {
			r.StaticDecisions[0].ToolCallID = "new-call"
		}},
		{"wrong version", func(_ *CurrentContinuationRequest, r *currentApplicationResolverStub, _ *currentApplicationVersionFreezerStub) {
			r.target.ApplicationVersionID = 42
		}},
		{"authority refused", func(_ *CurrentContinuationRequest, r *currentApplicationResolverStub, _ *currentApplicationVersionFreezerStub) {
			r.err = ErrUnsupportedCurrentAgentStart
		}},
		{"freezer changed kind", func(_ *CurrentContinuationRequest, r *currentApplicationResolverStub, f *currentApplicationVersionFreezerStub) {
			f.result = bytes.Replace(r.target.VersionDetails, []byte(`"agent_type":"pipeline"`), []byte(`"agent_type":"agent"`), 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools, input := staticInventoryRootFixture(t, CurrentStaticInventoryPipelineRoot)
			resolver := pipelineHITLResolver(nil)
			resolver.continuationTarget = CurrentContinuationTarget{ContinuationKind: CurrentContinuationStatic, Kind: CurrentRegenerationApplication, TargetParticipantID: 21, QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e", UserInput: "original task", ThreadID: input.GetThreadId(), ExecutionGeneration: input.GetExecutionGeneration(), PipelineStaticTools: tools}
			var app struct {
				VersionDetails json.RawMessage `json:"version_details"`
			}
			if json.Unmarshal(input.Application, &app) != nil {
				t.Fatal("bad fixture")
			}
			resolver.target.VersionDetails = app.VersionDetails
			leaf := tools.Inventory.Pauses[0]
			request := CurrentContinuationRequest{ProjectID: 7, ActorUserID: 11, ConversationUUID: input.GetConversationId(), ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a", ThreadID: input.GetThreadId(), Kind: CurrentContinuationStatic, StaticDecisions: []CurrentStaticLeafDecision{{PauseID: leaf.Proof.PauseID, ChildThreadID: leaf.ChildThreadID, ToolCallID: leaf.ToolCallID, Action: "continue", Value: "next"}}}
			freezer := &currentApplicationVersionFreezerStub{}
			admissions := &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{ExecutionID: "static-resume", CommandID: "static-command", Created: true}}
			service, err := NewCurrentApplicationStartService(resolver, resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{}, freezer, admissions)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(&request, resolver, freezer)
			outcome, err := service.ContinueCurrentAgent(context.Background(), request)
			if tc.name != "original" {
				if err == nil || len(admissions.requests) != 0 {
					t.Fatal("unbound continuation admitted")
				}
				if !errors.Is(err, ErrUnsupportedCurrentAgentStart) {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if err != nil || len(admissions.requests) != 1 {
				t.Fatalf("err=%v admissions=%d", err, len(admissions.requests))
			}
			admitted := admissions.requests[0]
			turn := admitted.CurrentContinueTurn
			if outcome.ResponseMessageID != request.ResponseMessageID || admitted.Input.GetThreadId() != input.GetThreadId() || admitted.Input.GetExecutionGeneration() != input.GetExecutionGeneration() || turn.ApplicationID != tools.ApplicationID || turn.ApplicationVersionID != tools.ApplicationVersionID || turn.PipelineStaticTools.RootKind != CurrentStaticInventoryPipelineRoot {
				t.Fatal("original root identity lost")
			}
			if admitted.Input.HitlResume || !admitted.Input.ShouldContinue || admitted.Input.CheckpointId != nil || !bytes.Equal(admitted.Input.UserInput, input.UserInput) || turn.PipelineStaticTools.Inventory.Pauses[0].OriginalBatchEventID != leaf.OriginalBatchEventID || turn.PipelineStaticTools.Inventory.Pauses[0].OriginalOrdinal != 16 {
				t.Fatal("original leaf ownership changed")
			}
			if !bytes.Equal(input.Meta, []byte(`{}`)) || !bytes.Equal(tools.InputDigest, turn.PipelineStaticTools.InputDigest) {
				t.Fatal("original input authority mutated")
			}
		})
	}
}
