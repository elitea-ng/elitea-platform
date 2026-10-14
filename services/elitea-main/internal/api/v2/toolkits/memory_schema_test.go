package toolkits_test

// The memory type's served schema and availability under the Rust indexing
// runtime (worker unit M9): over the REAL pinned snapshots.

import (
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

func memorySchemaRequired(t *testing.T, body map[string]any) (map[string]any, []string) {
	t.Helper()
	schema := typeSchemaOf(t, body, "memory")
	properties, _ := schema["properties"].(map[string]any)
	var required []string
	if list, ok := schema["required"].([]any); ok {
		for _, name := range list {
			required = append(required, name.(string))
		}
	}
	return properties, required
}

func TestMemorySchemaKeepsPgvectorRequiredUnlessItLivesOnTheVectorStore(t *testing.T) {
	t.Parallel()

	// The SDK schema, as pinned: both fields required (Python indexing).
	properties, required := memorySchemaRequired(t, getToolkitTypeCatalogue(t, catalogueOptions(t, "")...))
	if _, present := properties["pgvector_configuration"]; !present {
		t.Fatal("the SDK schema has pgvector_configuration")
	}
	if strings.Join(required, ",") != "namespace,pgvector_configuration" {
		t.Fatalf("SDK required = %v", required)
	}

	// Under the Rust indexing runtime the field is neither shown nor required.
	options := append(catalogueOptions(t, ""), toolkits.WithMemoryOnVectorStore(true))
	properties, required = memorySchemaRequired(t, getToolkitTypeCatalogue(t, options...))
	if _, present := properties["pgvector_configuration"]; present {
		t.Fatal("pgvector_configuration is still served under Rust indexing")
	}
	if strings.Join(required, ",") != "namespace" {
		t.Fatalf("Rust-indexing required = %v, want only namespace", required)
	}
	for _, name := range []string{"namespace", "selected_tools"} {
		if _, present := properties[name]; !present {
			t.Fatalf("%s vanished from the memory schema", name)
		}
	}

	scope, _ := properties["memory_scope"].(map[string]any)
	if scope == nil || scope["default"] != "personal" {
		t.Fatalf("memory_scope = %v, want a setting defaulting to personal", scope)
	}
	// Two values: a toolkit is personal OR project (the former `both` is gone).
	if values, _ := scope["enum"].([]any); len(values) != 2 || values[0] != "personal" || values[1] != "project" {
		t.Fatalf("memory_scope enum = %v, want [personal project]", values)
	}
	if !strings.Contains(scope["description"].(string), "Personal") || !strings.Contains(scope["description"].(string), "Project") {
		t.Fatalf("the scope setting is not described in plain words: %v", scope["description"])
	}

	// Serving it that way must not edit the shared pinned schema.
	properties, required = memorySchemaRequired(t, getToolkitTypeCatalogue(t, catalogueOptions(t, "")...))
	if _, present := properties["memory_scope"]; present {
		t.Fatal("the SDK schema (Python indexing) has no memory_scope")
	}
	if _, present := properties["pgvector_configuration"]; !present || len(required) != 2 {
		t.Fatalf("the shared schema was mutated: %v %v", properties, required)
	}
	// Other types keep theirs.
	options = append(catalogueOptions(t, ""), toolkits.WithMemoryOnVectorStore(true))
	body := getToolkitTypeCatalogue(t, options...)
	if _, present := typeSchemaOf(t, body, "artifact")["properties"].(map[string]any)["pgvector_configuration"]; !present {
		t.Fatal("artifact lost pgvector_configuration")
	}
}

func TestMemoryIsOfferedOnTheRustWorkerOnlyWhenItIsActuallyServed(t *testing.T) {
	t.Parallel()

	rustIndexing := runtimecomposition.Config{
		Enabled: true, IndexingRuntime: runtimecomposition.IndexingRuntimeRust,
		IndexIngestDispatchEnabled: true,
	}
	withModel := rustIndexing
	withModel.MemoryEmbeddingModel = "text-embedding-3-small"

	cases := map[string]struct {
		config    runtimecomposition.Config
		hidden    bool
		reasonHas string
	}{
		"rust indexing, model configured": {config: withModel},
		"rust indexing, no model": {
			config: rustIndexing, hidden: true,
			reasonHas: "Memory needs an embedding model configured by the operator",
		},
		"python indexing": {
			config: runtimecomposition.Config{}, hidden: true,
			reasonHas: "only when it indexes with the Rust runtime",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			capability := pinnedWorkerCapability(t, "rust").ForIndexingRuntime(tc.config)
			body := getToolkitTypeCatalogue(t,
				toolkits.WithArgumentSchemas(pinnedSnapshot(t)),
				toolkits.WithSettingsDefinitions(pinnedSettingsDefinitions(t)),
				toolkits.WithCatalogue(pinnedCatalogue(t)),
				toolkits.WithWorkerCapability(capability),
			)
			metadata := metadataOf(t, body, "memory")
			hidden, _ := metadata["hidden"].(bool)
			if hidden != tc.hidden {
				t.Fatalf("hidden = %v, want %v (%v)", hidden, tc.hidden, metadata)
			}
			reason, _ := metadata["unavailable_reason"].(string)
			if !strings.Contains(reason, tc.reasonHas) {
				t.Fatalf("reason = %q, want it to contain %q", reason, tc.reasonHas)
			}
		})
	}
}
