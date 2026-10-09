package run_test

// import_graph / export_graph (descriptor revision legacy-v2): the host
// reads the import document from the toolkit's bucket, never from the
// caller, and an export lands in the bucket as the graph browser's object.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/inventory/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/artifacts"
)

func importHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, map[string]run.Tool{
		run.ImportTool: answer(map[string]any{"success": true, "result": "imported"}),
	})
}

func TestImportReadsTheBucketDocumentAndNeverTheCallers(t *testing.T) {
	h := importHarness(t)
	h.uploads.objects["legacy-graphs/old/graph.json"] = []byte(`{"nodes": [{"id": "x"}]}`)
	_, err := h.invoke("inventory", "inventory", run.ImportTool, read(map[string]any{
		"llm_settings":                 llmSettings(),
		"toolkit_configuration_bucket": "legacy-graphs",
		"artifact_name":                "old/graph.json",
		"replace_ingestion_state":      true,
		run.GraphDocumentParam:         `{"nodes": [{"id": "forged"}]}`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	params, _ := h.lastArgs["params"].(map[string]any)
	if params[run.GraphDocumentParam] != `{"nodes": [{"id": "x"}]}` {
		t.Fatalf("the engine received %v, not the bucket's document", params[run.GraphDocumentParam])
	}
	if params["replace_ingestion_state"] != true {
		t.Fatalf("replace_ingestion_state did not reach the engine: %v", params)
	}
}

func TestImportDefaultsToTheBucketsGraphJSON(t *testing.T) {
	h := importHarness(t)
	if _, err := h.invoke("inventory", "inventory", run.ImportTool, read(map[string]any{"llm_settings": llmSettings()})); err != nil {
		t.Fatal(err)
	}
	params, _ := h.lastArgs["params"].(map[string]any)
	if params[run.GraphDocumentParam] != seededGraph {
		t.Fatalf("graphs/graph.json was not the default: %v", params[run.GraphDocumentParam])
	}
}

func TestImportRefusalsNameTheirCause(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		params   map[string]any
		fail     error
		category string
		needle   string
	}{
		{"no transport", map[string]any{}, nil, "invalid_input", "no bucket transport"},
		{"absent object", map[string]any{"llm_settings": llmSettings(), "artifact_name": "nope.json"}, nil,
			"resource_not_found", "holds no nope.json"},
		{"escaping key", map[string]any{"llm_settings": llmSettings(), "artifact_name": "../other/graph.json"}, nil,
			"invalid_input", "not a key inside"},
		{"absolute key", map[string]any{"llm_settings": llmSettings(), "artifact_name": "/graph.json"}, nil,
			"invalid_input", "not a key inside"},
		{"too large", map[string]any{"llm_settings": llmSettings()},
			fmt.Errorf("%w: graphs/graph.json", artifacts.ErrTooLarge), "invalid_input", "import-graph command"},
		{"transport fault", map[string]any{"llm_settings": llmSettings()},
			fmt.Errorf("failed to download artifact: HTTP 500 — secret detail"), "runtime_error", "could not be read"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := importHarness(t)
			if testCase.fail != nil {
				h.uploads.fail["download:graph.json"] = testCase.fail
			}
			body, err := h.invoke("inventory", "inventory", run.ImportTool, read(testCase.params))
			if err == nil {
				t.Fatal("the import was not refused")
			}
			if category(body) != testCase.category {
				t.Fatalf("category %s, want %s: %v", category(body), testCase.category, body)
			}
			text := fmt.Sprint(body["result"])
			if !strings.Contains(text, testCase.needle) || strings.Contains(text, "secret detail") {
				t.Fatalf("%q does not name %q, or leaks the transport's text", text, testCase.needle)
			}
			if h.lastArgs != nil {
				t.Fatal("the engine was called for a refused import")
			}
		})
	}
}

func TestTheFixtureExportLandsGraphJSONAsTheKnowledgeGraph(t *testing.T) {
	h := fixtureHarness(t)
	body, err := h.invoke("inventory", "inventory", run.ExportTool, read(map[string]any{
		"llm_settings": llmSettings(), "bucket": "graphs"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.uploads.uploads) != 1 || h.uploads.uploads[0].Name != "graph.json" || h.uploads.uploads[0].Bucket != "graphs" {
		t.Fatalf("uploads %+v", h.uploads.uploads)
	}
	var graph map[string]any
	if err := json.Unmarshal([]byte(h.uploads.uploads[0].Data), &graph); err != nil {
		t.Fatal(err)
	}
	if nodes, _ := graph["nodes"].([]any); len(nodes) != len(run.FixtureEntities) {
		t.Fatalf("exported %d nodes", len(nodes))
	}
	for _, object := range objects(t, body) {
		if object["name"] == "graph.json" && object["object_type"] != "knowledge_graph" {
			t.Fatalf("graph.json composed as %v", object["object_type"])
		}
	}
}

func TestTheFixtureImportSaysItStoredNothing(t *testing.T) {
	h := fixtureHarness(t)
	body, err := h.invoke("inventory", "inventory", run.ImportTool, read(map[string]any{"llm_settings": llmSettings()}))
	if err != nil {
		t.Fatal(err)
	}
	if text := message(t, body); !strings.Contains(text, "2 entities and 1 relations") || !strings.Contains(text, "nothing was imported") {
		t.Fatalf("%q", text)
	}
}
