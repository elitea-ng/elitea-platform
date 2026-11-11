package storage

import (
	"encoding/json"
	"testing"
)

const savedCodeChildRoot = "entry_point: run\nnodes:\n" +
	"  - id: run\n    type: agent\n    tool: etl\n    transition: each\n" +
	"  - id: each\n    type: map\n    worker: worker\n    max_items: 4\n    transition: gather\n" +
	"  - id: worker\n    type: agent\n    tool: etl\n" +
	"  - id: gather\n    type: parallel\n    branches: [{id: a, node: branch}]\n    transition: END\n" +
	"  - id: branch\n    type: agent\n    tool: other\n"

func savedCodeChildVersion(t *testing.T, project any) []byte {
	t.Helper()
	settings := func(application, version int) map[string]any {
		value := map[string]any{"application_id": application, "application_version_id": version}
		if project != nil {
			value["application_project_id"] = project
		}
		return value
	}
	raw, err := json.Marshal(map[string]any{"instructions": savedCodeChildRoot, "tools": []map[string]any{
		{"type": "application", "toolkit_name": "etl", "settings": settings(23, 29)},
		{"type": "application", "toolkit_name": "other", "settings": settings(31, 37)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The root declares the Agent node and its selected application; anything
// else is refused (W3-2b, B1/B2).
func TestSavedCodeChildEdgeSelectsOnlyTheDeclaredApplication(t *testing.T) {
	doc := ParseSavedCodeDocument(savedCodeChildRoot)
	version := savedCodeChildVersion(t, nil)
	admitted := []struct {
		kind, node, agent string
		ordinal, count    uint32
		want              [2]uint64
	}{
		{"", "", "run", 0, 0, [2]uint64{23, 29}},
		{"map_item", "each", "worker", 3, 4, [2]uint64{23, 29}},
		{"parallel_branch", "gather", "branch", 0, 1, [2]uint64{31, 37}},
	}
	for _, tc := range admitted {
		got, err := SavedCodeChildEdge(doc, version, 7, tc.kind, tc.node, tc.agent, tc.ordinal, tc.count)
		if err != nil || got != tc.want {
			t.Fatalf("%s %s: got %v %v, want %v", tc.kind, tc.agent, got, err, tc.want)
		}
	}
	refused := []struct {
		name, kind, node, agent string
		ordinal, count          uint32
	}{
		{"a fan-out owned Agent node at the root", "", "", "worker", 0, 0},
		{"a root node with a family", "", "each", "run", 0, 0},
		{"a Map item at the limit", "map_item", "each", "worker", 4, 4},
		{"a frozen count above max_items", "map_item", "each", "worker", 0, 5},
		{"a root Agent node as a Map worker", "map_item", "each", "run", 0, 4},
		{"a forged family", "parallel_branch", "each", "worker", 0, 4},
		{"an unknown family", "agent_graph", "", "run", 0, 0},
		{"an undeclared node", "", "", "missing", 0, 0},
		{"a non-Agent node", "", "", "each", 0, 0},
	}
	for _, tc := range refused {
		if _, err := SavedCodeChildEdge(doc, version, 7, tc.kind, tc.node, tc.agent, tc.ordinal, tc.count); err == nil {
			t.Fatalf("%s was admitted", tc.name)
		}
	}
	if _, err := SavedCodeChildEdge(doc, savedCodeChildVersion(t, 8), 7, "", "", "run", 0, 0); err == nil {
		t.Fatal("an application of a foreign project was admitted")
	}
	other, _ := json.Marshal(map[string]any{"instructions": "entry_point: run\nnodes: []\n"})
	if _, err := SavedCodeChildEdge(doc, other, 7, "", "", "run", 0, 0); err == nil {
		t.Fatal("a version whose instructions are not the parsed source was admitted")
	}
}

func TestSavedCodeChildThreadIsTheAgentNodesSubgraphThread(t *testing.T) {
	if !SavedCodeChildThread("root", "run", "root/run") || !SavedCodeChildThread("m1:x", "worker", "m1:x/worker") {
		t.Fatal("the subgraph thread was refused")
	}
	for _, child := range []string{"root", "root/other", "root/run/deeper", "other/run", ""} {
		if SavedCodeChildThread("root", "run", child) {
			t.Fatalf("%q was admitted", child)
		}
	}
	long := make([]byte, maxSavedCodeChildThread)
	for i := range long {
		long[i] = 'r'
	}
	if SavedCodeChildThread(string(long[:maxSavedCodeChildThread-3]), "run", string(long[:maxSavedCodeChildThread-3])+"/run") {
		t.Fatal("a thread above the bound was admitted")
	}
	if !SavedCodeChildThread(string(long[:maxSavedCodeChildThread-4]), "run", string(long[:maxSavedCodeChildThread-4])+"/run") {
		t.Fatal("a thread at the bound was refused")
	}
}
