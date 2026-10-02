package toolkits_test

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
)

func TestAgentToolkitDiscoveryPreservesSchemaAndPolicy(t *testing.T) {
	h := toolkits.NewHandlerWithRepo(&mockRepo{}, toolkits.WithGuardrails(&guardrailSourceStub{policy: guardrails.NewPolicy(guardrails.PolicyInput{BlockedToolkits: []string{"artifact"}})}))
	read := func(path string, discovery bool) map[string]map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		if discovery {
			h.DiscoverTypeSchemas(w, r)
		} else {
			h.ListTypeSchemas(w, r)
		}
		if w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var result map[string]map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	full := read("/", false)
	summary := read("/", true)
	if len(full) != len(summary) {
		t.Fatal("summary loses types")
	}
	for _, item := range summary {
		if _, ok := item["properties"]; ok {
			t.Fatal("summary contains schemas")
		}
	}
	selected := read("/?type=github", true)
	if len(selected) != 1 || !reflect.DeepEqual(selected["github"], full["github"]) {
		t.Fatal("selected schema differs from REST catalog")
	}
	if len(read("/?type=artifact", true)) != 0 {
		t.Fatal("selection bypasses blocked type policy")
	}
}

func TestAgentToolkitDiscoveryUsesSavedSettingsContract(t *testing.T) {
	h := toolkits.NewHandlerWithRepo(&mockRepo{}, catalogueOptions(t, "")...)
	read := func(discovery bool) map[string]map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/?type=github", nil)
		if discovery {
			h.DiscoverTypeSchemas(w, r)
		} else {
			h.ListTypeSchemas(w, r)
		}
		if w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var result map[string]map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := read(false)
	uiProperties := before["github"]["properties"].(map[string]any)
	for _, field := range []string{"github_configuration", "pgvector_configuration", "embedding_model", "active_branch", "base_branch"} {
		if uiProperties[field] == nil {
			t.Errorf("GitHub UI schema omits %s", field)
		}
	}
	if uiProperties["embedding_model"].(map[string]any)["configuration_model"] != "embedding" {
		t.Fatal("embedding field does not select the embedding model catalogue")
	}
	schema := read(true)["github"]
	properties := schema["properties"].(map[string]any)
	selected := properties["selected_tools"].(map[string]any)
	if selected["type"] != "array" {
		t.Fatalf("selected tools must be a list: %v", selected["type"])
	}
	credential := properties["github_configuration"].(map[string]any)
	fields := credential["properties"].(map[string]any)
	if fields["elitea_title"] == nil || fields["private"] == nil || credential["$ref"] != nil {
		t.Fatalf("credential must describe the stored reference: %v", credential)
	}
	if properties["access_token"] != nil {
		t.Fatal("legacy inline token field must not override the SDK credential contract")
	}
	if !reflect.DeepEqual(before, read(false)) {
		t.Fatal("agent discovery mutates the UI schema")
	}
	if !reflect.DeepEqual(schema["metadata"], before["github"]["metadata"]) {
		t.Fatal("agent discovery changes deployment policy metadata")
	}
}
