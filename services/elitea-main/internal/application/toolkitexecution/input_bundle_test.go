package toolkitexecution

import (
	"context"
	"errors"
	"strings"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"google.golang.org/protobuf/proto"
)

func TestInputBundleFactoryBuildsReferenceOnlyToolkitReadInput(t *testing.T) {
	factory := toolkitReadInputFactory(t, "bundle-1", "content-1")
	frozen := validFrozenCurrentReadTool()

	bundle, binding, err := factory.Build(context.Background(), frozen)
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Validate(); err != nil {
		t.Fatalf("bundle.Validate() error = %v", err)
	}
	if err := binding.Validate(bundle); err != nil {
		t.Fatalf("binding.Validate() error = %v", err)
	}
	if len(bundle.Entries) != 1 || binding.RequestEntryID != toolkitReadRequestEntryID {
		t.Fatalf("binding = %#v entries = %d", binding, len(bundle.Entries))
	}
	entry := bundle.Entries[0]
	if entry.SemanticRole != executiondomain.ToolkitExecuteReadRequestRole ||
		entry.MediaType != executiondomain.ToolkitExecuteReadInputMediaType {
		t.Fatalf("entry = %#v", entry)
	}
	var decoded runtimev1.ToolkitExecuteReadInputV1
	if err := proto.Unmarshal(entry.Content, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.GetSchemaRevision() != toolkitReadSchemaRevision ||
		decoded.GetToolkitType() != frozen.ToolkitType || decoded.GetToolkitName() != frozen.ToolkitName ||
		decoded.GetToolName() != frozen.ToolName || string(decoded.GetToolkit()) != string(frozen.ToolkitJSON) ||
		string(decoded.GetArguments()) != string(frozen.ArgumentsJSON) {
		t.Fatalf("decoded = %#v", &decoded)
	}
}

func TestInputBundleFactoryIsDeterministicForFrozenContent(t *testing.T) {
	first, _, err := toolkitReadInputFactory(t, "bundle-a", "content-a").Build(
		context.Background(), validFrozenCurrentReadTool(),
	)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := toolkitReadInputFactory(t, "bundle-b", "content-b").Build(
		context.Background(), validFrozenCurrentReadTool(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.Entries[0].Version != second.Entries[0].Version ||
		first.Entries[0].ContentDigest != second.Entries[0].ContentDigest ||
		string(first.Entries[0].Content) != string(second.Entries[0].Content) {
		t.Fatal("same frozen invocation produced different immutable content")
	}
}

func TestInputBundleFactoryRejectsMismatchedMalformedAndOversizedInput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*FrozenCurrentReadTool)
	}{
		{name: "type mismatch", mutate: func(value *FrozenCurrentReadTool) { value.ToolkitType = "jira" }},
		{name: "invalid toolkit", mutate: func(value *FrozenCurrentReadTool) { value.ToolkitJSON = []byte(`[]`) }},
		{name: "invalid arguments", mutate: func(value *FrozenCurrentReadTool) { value.ArgumentsJSON = []byte(`[]`) }},
		{name: "invalid guardrails", mutate: func(value *FrozenCurrentReadTool) { value.GuardrailsJSON = []byte(`null`) }},
		{name: "oversized arguments", mutate: func(value *FrozenCurrentReadTool) {
			value.ArgumentsJSON = []byte(`{"value":"` + strings.Repeat("x", MaxCurrentReadToolArgumentsBytes) + `"}`)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := validFrozenCurrentReadTool()
			test.mutate(&value)
			_, _, err := toolkitReadInputFactory(t, "bundle", "content").Build(context.Background(), value)
			if !errors.Is(err, ErrInvalidAuthoritativeToolkitReadInput) {
				t.Fatalf("Build() error = %v", err)
			}
		})
	}
}

func TestInputBundleFactoryPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := toolkitReadInputFactory(t, "bundle", "content").Build(ctx, validFrozenCurrentReadTool())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Build() error = %v", err)
	}
}

func validFrozenCurrentReadTool() FrozenCurrentReadTool {
	return FrozenCurrentReadTool{
		ToolkitType:    "github",
		ToolkitName:    "Source Control",
		ToolName:       "get_issue",
		ToolkitJSON:    []byte(`{"id":19,"type":"github","toolkit_name":"Source Control","settings":{"selected_tools":["get_issue"],"token":{"configuration_uuid":"sealed"}}}`),
		ArgumentsJSON:  []byte(`{"issue":9007199254740993}`),
		GuardrailsJSON: []byte(`{"blocked_tools":{},"sensitive_tools":{}}`),
	}
}

func toolkitReadInputFactory(t *testing.T, ids ...string) *InputBundleFactory {
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
		t.Fatal(err)
	}
	return factory
}
