package run

// The two-runner parity anchor.
//
// There are TWO fixture runners for Inventory: this one (Go, for the E2E
// stack) and elitea_inventory.fixture_graph (Python, for the standalone-full
// stack's engine sidecar). Nothing forces them to agree — they are separate
// languages with no shared import — so conformance/provider/fixtures/inventory/
// exists to make agreement a file both sides read and a test both sides run.
//
// This file is the Go half. It is `package run`, not `run_test`, because the
// fixtures below are checked against the UNEXPORTED constants and handlers —
// the ones a caller three files away cannot reach and therefore cannot get
// wrong by importing something else. The Python half
// (services/elitea-inventory/tests/unit/test_fixture_parity.py) reads the SAME
// files and asserts the same shapes against elitea_inventory.fixture_graph.
//
// A change to the canned graph that touches only one language's copy fails
// here or there, not silently: this test loads spi/graph.json and asserts
// FixtureEntities/FixtureRelations/FixturePresets equal it; the Python test
// loads its packaged copy and asserts it equals the same file (see that
// package's README for why there are two copies of one JSON document).
import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fixtureConformancePath is conformance/provider/fixtures/inventory, four
// directories up from this package.
func fixtureConformancePath(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "..", "..", "..", "..", "conformance", "provider", "fixtures", "inventory"}, parts...)...)
}

func readFixtureJSON(t *testing.T, path string, target any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
}

// roundTrip re-decodes a Go value through JSON so its numbers become float64
// like every value decoded from a fixture file — the same shape a real caller
// receives, and the only way a plain reflect.DeepEqual is meaningful here.
func roundTrip(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("round-tripping: %v", err)
	}
	return decoded
}

// TestTheCannedGraphMatchesTheSharedFixtureFile is the anchor: the Go
// constants below are hand-written, the JSON file is hand-written, and NOTHING
// checks the file against the Python side except that other test. This is the
// half that keeps THIS language honest against the checked-in document both
// runners are supposed to replay.
func TestTheCannedGraphMatchesTheSharedFixtureFile(t *testing.T) {
	var golden struct {
		Entities []FixtureEntity   `json:"entities"`
		Relations []FixtureRelation `json:"relations"`
		Presets   map[string]string `json:"presets"`
	}
	readFixtureJSON(t, fixtureConformancePath("spi", "graph.json"), &golden)

	if len(golden.Entities) != len(FixtureEntities) {
		t.Fatalf("fixture file has %d entities, FixtureEntities has %d", len(golden.Entities), len(FixtureEntities))
	}
	for i, want := range golden.Entities {
		if got := FixtureEntities[i]; got != want {
			t.Fatalf("entity %d: got %+v, want %+v", i, got, want)
		}
	}

	if len(golden.Relations) != len(FixtureRelations) {
		t.Fatalf("fixture file has %d relations, FixtureRelations has %d", len(golden.Relations), len(FixtureRelations))
	}
	for i, want := range golden.Relations {
		if got := FixtureRelations[i]; got != want {
			t.Fatalf("relation %d: got %+v, want %+v", i, got, want)
		}
	}

	if len(golden.Presets) != len(FixturePresets) {
		t.Fatalf("fixture file has %d presets, FixturePresets has %d", len(golden.Presets), len(FixturePresets))
	}
	for name, want := range golden.Presets {
		if got, ok := FixturePresets[name]; !ok || got != want {
			t.Fatalf("preset %q: got %q (present=%v), want %q", name, got, ok, want)
		}
	}
}

// toolFixture is the {tool, params, expected} shape every ingestion/ and
// retrieval/ file carries.
type toolFixture struct {
	Tool     string         `json:"tool"`
	Params   map[string]any `json:"params"`
	Expected map[string]any `json:"expected"`
}

// TestEveryIngestionAndRetrievalFixtureMatchesTheGoHandler is "the parity test
// that loads every fixture file ... and asserts the mode answers each with the
// same shape" for the Go side. The Python side runs the identical files
// through elitea_inventory.fixture_graph (test_fixture_parity.py) and must
// reach the same `expected` document.
func TestEveryIngestionAndRetrievalFixtureMatchesTheGoHandler(t *testing.T) {
	files := listFixtureFiles(t, fixtureConformancePath("ingestion"))
	files = append(files, listFixtureFiles(t, fixtureConformancePath("retrieval"))...)
	if len(files) == 0 {
		t.Fatal("no ingestion/retrieval fixture files found")
	}
	sort.Strings(files)

	handlers := fixtureHandlers()
	for _, path := range files {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			var fx toolFixture
			readFixtureJSON(t, path, &fx)

			handler, ok := handlers[fx.Tool]
			if !ok {
				t.Fatalf("no fixture handler for tool %q", fx.Tool)
			}

			params := Params(fx.Params)
			result := handler(fx.Tool, fx.Tool, params)
			if success, _ := result["success"].(bool); !success {
				t.Fatalf("tool %q refused: %+v", fx.Tool, result)
			}

			// run_ingestion does not honour output_format (it always answers
			// artifacts), so it is checked against its own documented shape;
			// every other fixture asked for output_format=json and is checked
			// against the parsed `result` document directly.
			var got any
			if fx.Tool == "run_ingestion" {
				got = roundTrip(t, ingestionShapeFor(t, result))
			} else {
				raw, _ := result["result"].(string)
				var document any
				if err := json.Unmarshal([]byte(raw), &document); err != nil {
					t.Fatalf("tool %q did not answer JSON (params must set output_format=json): %v\nresult: %q", fx.Tool, err, raw)
				}
				got = document
			}

			want := roundTrip(t, fx.Expected)
			assertJSONEqual(t, got, want, fx.Tool)
		})
	}
}

// ingestionShapeFor extracts the {source_label, result, graph_metadata,
// sources_status, checkpoint} document run_ingestion.json's `expected` is
// shaped as, from the artifacts fixtureRunIngestion actually produced.
func ingestionShapeFor(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	artifacts, _ := result["artifacts"].([]any)
	shape := map[string]any{"result": result["result"]}
	for _, raw := range artifacts {
		artifact, _ := raw.(map[string]any)
		name, _ := artifact["name"].(string)
		data, _ := artifact["data"].(string)
		var decoded map[string]any
		if err := json.Unmarshal([]byte(data), &decoded); err != nil {
			t.Fatalf("artifact %q is not JSON: %v", name, err)
		}
		switch {
		case name == "graph.json":
			metadata, _ := decoded["_metadata"].(map[string]any)
			shape["graph_metadata"] = metadata
			if metadata != nil {
				shape["source_label"] = metadata["ingested_source"]
			}
		case name == "sources_status.json":
			shape["sources_status"] = decoded
		case strings.HasPrefix(name, ".ingestion-checkpoint-"):
			shape["checkpoint"] = decoded
		}
	}
	return shape
}

func assertJSONEqual(t *testing.T, got, want any, tool string) {
	t.Helper()
	gotEncoded, _ := json.Marshal(got)
	wantEncoded, _ := json.Marshal(want)
	if string(gotEncoded) != string(wantEncoded) {
		t.Fatalf("%s: shape differs\n got:  %s\nwant: %s", tool, gotEncoded, wantEncoded)
	}
}

func listFixtureFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		files = append(files, filepath.Join(dir, entry.Name()))
	}
	return files
}
