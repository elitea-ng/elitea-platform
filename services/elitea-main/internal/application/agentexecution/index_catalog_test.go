package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/indexcatalog"
)

type indexCatalogStub struct {
	catalog []map[string]any
	err     error
	// seen records each call: the project, then the toolkit ids asked.
	seen [][]int32
}

func (s *indexCatalogStub) ToolkitIndexCatalogs(_ context.Context, projectID int32, toolkitIDs []int32) (map[int32][]map[string]any, error) {
	s.seen = append(s.seen, append([]int32{projectID}, toolkitIDs...))
	if s.err != nil {
		return nil, s.err
	}
	result := make(map[int32][]map[string]any, len(toolkitIDs))
	for _, toolkitID := range toolkitIDs {
		result[toolkitID] = s.catalog
	}
	return result, nil
}

func freezeOneGithubToolkit(t *testing.T, service *CurrentApplicationToolSnapshotService) map[string]any {
	t.Helper()
	result, err := service.FreezeCurrentApplicationVersion(
		context.Background(),
		CurrentApplicationVersionFreezeRequest{
			ProjectID: 7, ActorUserID: 11,
			VersionDetails: json.RawMessage(`{
  "id":41,
  "agent_type":"agent",
  "llm_settings":{"model_name":"model"},
  "tools":[{
    "id":19,
    "type":"github",
    "name":"Repo",
    "description":"Current toolkit",
    "author_id":11,
    "settings":{"selected_tools":["get_me","search_index"],"repository":"a/b"},
    "meta":{"current":true},
    "is_pinned":false
  }]
}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	version, err := decodeCurrentApplicationVersion(result)
	if err != nil {
		t.Fatal(err)
	}
	return version["tools"].([]any)[0].(map[string]any)
}

func newGithubFreezer(t *testing.T) *CurrentApplicationToolSnapshotService {
	t.Helper()
	settings := &currentAgentSettingsResolverStub{result: map[string]any{
		"selected_tools": []any{"get_me", "search_index"},
		"repository":     "a/b",
	}}
	names := &currentAgentNameResolverStub{result: "repo"}
	service, err := NewCurrentApplicationToolSnapshotService(
		settings, names, currentAgentModelCatalogForTest(true),
		&currentAgentGuardrailStub{}, &currentProjectContextStub{}, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// The Python indexing path writes no `indexes` key: its absence is what tells
// the Rust worker the SDK serves the index tools (ADR-0030 I1).
func TestCurrentApplicationToolSnapshotWritesNoIndexCatalogWithoutAResolver(t *testing.T) {
	tool := freezeOneGithubToolkit(t, newGithubFreezer(t))
	if _, present := tool[indexcatalog.Key]; present {
		t.Fatalf("frozen tool carries %q on the Python path: %#v", indexcatalog.Key, tool)
	}
}

func TestCurrentApplicationToolSnapshotFreezesTheIndexCatalogOfAnIndexingFamily(t *testing.T) {
	catalog := &indexCatalogStub{catalog: []map[string]any{
		indexcatalog.Entry("docs", "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b", "text-embedding-3-small", 1536),
	}}
	tool := freezeOneGithubToolkit(t, newGithubFreezer(t).WithIndexCatalog(catalog))
	frozen, ok := tool[indexcatalog.Key].([]any)
	if !ok || len(frozen) != 1 {
		t.Fatalf("frozen tool has no index catalog: %#v", tool)
	}
	entry := frozen[0].(map[string]any)
	if entry["name"] != "docs" || entry["index_id"] != "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b" ||
		entry["embedding_model"] != "text-embedding-3-small" {
		t.Fatalf("entry = %#v", entry)
	}
	if dimension, _ := positiveCurrentAgentJSONInteger(entry["embedding_dimension"]); dimension != 1536 {
		t.Fatalf("dimension = %#v", entry["embedding_dimension"])
	}
	if !reflect.DeepEqual(catalog.seen, [][]int32{{7, 19}}) {
		t.Fatalf("catalog asked for %v, want the request's project and the tool's id", catalog.seen)
	}
}

// Review round 2, finding 4: one registry read for every indexing toolkit of
// the freeze, not one per toolkit; a non-indexing toolkit between them is not
// asked for and gets no catalog.
func TestCurrentApplicationToolSnapshotFreezesEveryIndexCatalogInOneRead(t *testing.T) {
	catalog := &indexCatalogStub{catalog: []map[string]any{
		indexcatalog.Entry("docs", "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b", "text-embedding-3-small", 1536),
	}}
	settings := &currentAgentSettingsResolverStub{result: map[string]any{
		"selected_tools": []any{"search_index"},
		"repository":     "a/b",
	}}
	names := &currentAgentNameResolverStub{result: "repo"}
	service, err := NewCurrentApplicationToolSnapshotService(
		settings, names, currentAgentModelCatalogForTest(true),
		&currentAgentGuardrailStub{}, &currentProjectContextStub{}, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.WithIndexCatalog(catalog).FreezeCurrentApplicationVersion(
		context.Background(),
		CurrentApplicationVersionFreezeRequest{
			ProjectID: 7, ActorUserID: 11,
			VersionDetails: json.RawMessage(`{
  "id":41,
  "agent_type":"agent",
  "llm_settings":{"model_name":"model"},
  "tools":[
    {"id":19,"type":"github","name":"Repo","author_id":11,"settings":{"selected_tools":["search_index"],"repository":"a/b"},"meta":{"current":true}},
    {"id":23,"type":"aha","name":"Team Docs","author_id":11,"settings":{"selected_tools":["search"]},"meta":{"current":true}},
    {"id":29,"type":"gitlab","name":"Release","author_id":11,"settings":{"selected_tools":["search_index"],"repository":"a/b"},"meta":{"current":true}}
  ]
}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	version, err := decodeCurrentApplicationVersion(result)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(catalog.seen, [][]int32{{7, 19, 29}}) {
		t.Fatalf("catalog asked %v, want one read naming both indexing toolkits", catalog.seen)
	}
	tools := version["tools"].([]any)
	for i, wantCatalog := range []bool{true, false, true} {
		tool := tools[i].(map[string]any)
		frozen, present := tool[indexcatalog.Key]
		if present != wantCatalog {
			t.Fatalf("tool %d: catalog present=%v want %v: %#v", i, present, wantCatalog, tool)
		}
		if wantCatalog && len(frozen.([]any)) != 1 {
			t.Fatalf("tool %d: catalog = %#v", i, frozen)
		}
	}
}

// An empty catalog is still a catalog ([]): the toolkit has no indexes yet,
// and list_indexes answers "No indexed collections" on the worker.
func TestCurrentApplicationToolSnapshotFreezesAnEmptyIndexCatalogAsAnArray(t *testing.T) {
	catalog := &indexCatalogStub{catalog: []map[string]any{}}
	tool := freezeOneGithubToolkit(t, newGithubFreezer(t).WithIndexCatalog(catalog))
	frozen, ok := tool[indexcatalog.Key].([]any)
	if !ok || len(frozen) != 0 {
		t.Fatalf("an empty catalog must be frozen as []: %#v", tool[indexcatalog.Key])
	}
}

// A toolkit outside the I1 families is frozen as before, and the registry is
// not even asked.
func TestCurrentApplicationToolSnapshotLeavesNonIndexingFamiliesAlone(t *testing.T) {
	catalog := &indexCatalogStub{catalog: []map[string]any{indexcatalog.Entry("x", "a", "m", 3)}}
	settings := &currentAgentSettingsResolverStub{result: map[string]any{"selected_tools": []any{"search"}}}
	names := &currentAgentNameResolverStub{result: "team_docs"}
	service, err := NewCurrentApplicationToolSnapshotService(
		settings, names, currentAgentModelCatalogForTest(true),
		&currentAgentGuardrailStub{}, &currentProjectContextStub{}, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	service = service.WithIndexCatalog(catalog)
	result, err := service.FreezeCurrentApplicationVersion(
		context.Background(),
		CurrentApplicationVersionFreezeRequest{
			ProjectID: 7, ActorUserID: 11,
			VersionDetails: json.RawMessage(`{
  "id":41,
  "agent_type":"agent",
  "llm_settings":{"model_name":"model"},
  "tools":[{"id":19,"type":"aha","name":"Team Docs","author_id":11,
    "settings":{"selected_tools":["search"]},"meta":{"current":true}}]
}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	version, err := decodeCurrentApplicationVersion(result)
	if err != nil {
		t.Fatal(err)
	}
	tool := version["tools"].([]any)[0].(map[string]any)
	if _, present := tool[indexcatalog.Key]; present || len(catalog.seen) != 0 {
		t.Fatalf("an aha toolkit (not an indexing family) must carry no catalog and ask nothing: %#v %v", tool, catalog.seen)
	}
}

// Review round 2, finding 3: a registry failure does not fail the start. The
// indexing toolkit is frozen with an EMPTY catalog ([]), which keeps the
// worker and Main in agreement — the key is present, so the Rust worker
// serves the index tools (over no indexes) instead of leaving them to an SDK
// worker that serves nothing here — and the failure is logged.
func TestCurrentApplicationToolSnapshotFreezesAnEmptyCatalogWhenTheRegistryFails(t *testing.T) {
	catalog := &indexCatalogStub{err: errors.New("registry down")}
	service := newGithubFreezer(t).WithIndexCatalog(catalog)
	result, err := service.FreezeCurrentApplicationVersion(
		context.Background(),
		CurrentApplicationVersionFreezeRequest{
			ProjectID: 7, ActorUserID: 11,
			VersionDetails: json.RawMessage(`{
  "id":41,
  "agent_type":"agent",
  "llm_settings":{"model_name":"model"},
  "tools":[{"id":19,"type":"github","name":"Repo","author_id":11,
    "settings":{"selected_tools":["get_me"],"repository":"a/b"},"meta":{"current":true}}]
}`),
		},
	)
	if err != nil {
		t.Fatalf("a registry failure must not fail the freeze: %v", err)
	}
	version, err := decodeCurrentApplicationVersion(result)
	if err != nil {
		t.Fatal(err)
	}
	tool := version["tools"].([]any)[0].(map[string]any)
	frozen, ok := tool[indexcatalog.Key].([]any)
	if !ok || len(frozen) != 0 {
		t.Fatalf("the toolkit must carry an empty catalog, got %#v", tool[indexcatalog.Key])
	}
	if len(catalog.seen) != 1 {
		t.Fatalf("the registry is asked once: %v", catalog.seen)
	}
}
