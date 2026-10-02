package agentexecution

import (
	"context"
	"errors"
	"strings"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"google.golang.org/protobuf/proto"
)

func TestInputBundleFactoryBuildsReferenceOnlyAgentInput(t *testing.T) {
	factory := testInputBundleFactory(t)
	input := validAgentInput()

	bundle, binding, err := factory.Build(
		context.Background(), input, "conversation-1", "message-1", "chat_predict",
	)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if err := bundle.Validate(); err != nil {
		t.Fatalf("bundle.Validate() error = %v", err)
	}
	if err := binding.Validate(bundle); err != nil {
		t.Fatalf("binding.Validate() error = %v", err)
	}
	if binding.RequestEntryID != requestEntryID ||
		binding.ClientStreamID != "conversation-1" ||
		binding.ClientMessageID != "message-1" ||
		binding.ClientExecutionGeneration != input.GetExecutionGeneration() ||
		binding.SIOEvent != "chat_predict" {
		t.Fatalf("unexpected binding: %#v", binding)
	}
	if len(bundle.Entries) != 1 {
		t.Fatalf("entry count = %d, want 1", len(bundle.Entries))
	}
	entry := bundle.Entries[0]
	if entry.SemanticRole != executiondomain.AgentExecutionRequestRole ||
		entry.MediaType != executiondomain.AgentExecutionInputMediaType {
		t.Fatalf("unexpected agent entry: %#v", entry)
	}

	decoded := &runtimev1.AgentExecutionInputV1{}
	if err := proto.Unmarshal(entry.Content, decoded); err != nil {
		t.Fatalf("proto.Unmarshal() error = %v", err)
	}
	if !proto.Equal(decoded, input) {
		t.Fatal("stored immutable input differs from the resolved request")
	}
}

func TestInputBundleFactoryIsDeterministicForResolvedContent(t *testing.T) {
	firstFactory := inputBundleFactoryWithIDs(t, "bundle-a", "content-a")
	secondFactory := inputBundleFactoryWithIDs(t, "bundle-b", "content-b")
	input := validAgentInput()

	first, _, err := firstFactory.Build(context.Background(), input, "room", "message", "chat_predict")
	if err != nil {
		t.Fatalf("first Build() error = %v", err)
	}
	second, _, err := secondFactory.Build(context.Background(), input, "room", "message", "chat_predict")
	if err != nil {
		t.Fatalf("second Build() error = %v", err)
	}
	if first.Entries[0].ContentDigest != second.Entries[0].ContentDigest ||
		first.Entries[0].Version != second.Entries[0].Version {
		t.Fatal("same resolved request produced different immutable content identity")
	}
	if string(first.Entries[0].Content) != string(second.Entries[0].Content) {
		t.Fatal("same resolved request produced different bytes")
	}
}

func TestInputBundleFactoryRejectsInvalidOrOversizedInput(t *testing.T) {
	tests := []struct {
		name  string
		input *runtimev1.AgentExecutionInputV1
	}{
		{name: "nil", input: nil},
		{name: "wrong revision", input: func() *runtimev1.AgentExecutionInputV1 {
			value := validAgentInput()
			value.SchemaRevision = "v2"
			return value
		}()},
		{name: "wrong semantic shape", input: func() *runtimev1.AgentExecutionInputV1 {
			value := validAgentInput()
			value.Tools = []byte(`{}`)
			return value
		}()},
		{name: "missing client execution generation", input: func() *runtimev1.AgentExecutionInputV1 {
			value := validAgentInput()
			value.ExecutionGeneration = nil
			return value
		}()},
		{name: "oversized", input: func() *runtimev1.AgentExecutionInputV1 {
			value := validAgentInput()
			value.UserInput = []byte(`"` + strings.Repeat("x", executiondomain.MaxAgentExecutionInputBytes) + `"`)
			return value
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			factory := testInputBundleFactory(t)
			_, _, err := factory.Build(
				context.Background(), test.input, "conversation", "message", "chat_predict",
			)
			if !errors.Is(err, ErrInvalidAuthoritativeAgentInput) {
				t.Fatalf("Build() error = %v, want %v", err, ErrInvalidAuthoritativeAgentInput)
			}
		})
	}
}

func TestInputBundleFactoryAcceptsPluralHITLResumeWithoutScalarAction(t *testing.T) {
	input := validAgentInput()
	input.HitlResume = true
	input.HitlDecisions = []byte(`[
		{"interrupt_id":"interrupt-name","action":"approve"},
		{"interrupt_id":"interrupt-surname","action":"reject"}
	]`)

	factory := testInputBundleFactory(t)
	if _, _, err := factory.Build(
		context.Background(), input, "conversation", "message", "chat_continue_predict",
	); err != nil {
		t.Fatalf("Build() plural HITL resume error = %v", err)
	}
}

func TestInputBundleFactoryRejectsHITLResumeWithoutAnyDecision(t *testing.T) {
	input := validAgentInput()
	input.HitlResume = true

	factory := testInputBundleFactory(t)
	_, _, err := factory.Build(
		context.Background(), input, "conversation", "message", "chat_continue_predict",
	)
	if !errors.Is(err, ErrInvalidAuthoritativeAgentInput) {
		t.Fatalf("Build() error = %v, want %v", err, ErrInvalidAuthoritativeAgentInput)
	}
}

func TestInputBundleFactoryPreservesCancellation(t *testing.T) {
	factory := testInputBundleFactory(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := factory.Build(ctx, validAgentInput(), "conversation", "message", "chat_predict")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Build() error = %v, want %v", err, context.Canceled)
	}
}

func validAgentInput() *runtimev1.AgentExecutionInputV1 {
	steps := int32(12)
	return &runtimev1.AgentExecutionInputV1{
		SchemaRevision:              agentInputSchemaRevision,
		Llm:                         []byte(`{"kwargs":{"model":"model-1"}}`),
		ChatHistory:                 []byte(`[]`),
		UserInput:                   []byte(`"hello"`),
		Debug:                       true,
		Tools:                       []byte(`[]`),
		Application:                 []byte(`{"id":7,"version_id":9}`),
		InternalTools:               []byte(`[]`),
		StepsLimit:                  &steps,
		McpTokens:                   []byte(`{}`),
		IgnoredMcpServers:           []byte(`[]`),
		UserDeclinedMcpServers:      []byte(`[]`),
		HitlDecisions:               []byte(`[]`),
		ExecutionGeneration:         proto.String("client-generation-1"),
		Meta:                        []byte(`{}`),
		Persona:                     "generic",
		ContextSettings:             []byte(`{}`),
		InvokedSkills:               []byte(`[]`),
		AppliedSkills:               []byte(`[]`),
		AttachedSkills:              []byte(`[]`),
		InputAttachments:            []byte(`[]`),
		ParallelReconcile:           []byte(`null`),
		ParallelTerminalErrors:      []byte(`[]`),
		ExceptionHandlingEnabled:    proto.Bool(false),
		DebugMode:                   proto.Bool(true),
		AutoApproveSensitiveActions: false,
	}
}

func testInputBundleFactory(t *testing.T) *InputBundleFactory {
	t.Helper()
	return inputBundleFactoryWithIDs(t, "bundle-1", "content-1")
}

func inputBundleFactoryWithIDs(t *testing.T, ids ...string) *InputBundleFactory {
	t.Helper()
	index := 0
	factory, err := NewInputBundleFactory(InputProfile{
		Classification:        "tenant-confidential",
		RequiredGrantAudience: "elitea.runtime.input.read.v1",
	}, func() (string, error) {
		value := ids[index]
		index++
		return value, nil
	})
	if err != nil {
		t.Fatalf("NewInputBundleFactory() error = %v", err)
	}
	return factory
}

// A million-token history must reach the worker's token-budget decision.
func TestInputBundleFactoryPreservesFullWindowHistory(t *testing.T) {
	input := validAgentInput()
	input.ChatHistory = []byte(`[{"role":"user","content":"` + strings.Repeat("x", 4*1024*1024) + `"}]`)
	bundle, _, err := testInputBundleFactory(t).Build(context.Background(), input, "room", "message", "chat_predict")
	if err != nil {
		t.Fatalf("Build full history: %v", err)
	}
	decoded := &runtimev1.AgentExecutionInputV1{}
	if err := proto.Unmarshal(bundle.Entries[0].Content, decoded); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(input, decoded) {
		t.Fatal("full history changed during admission")
	}
}
