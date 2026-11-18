package marketplace

import (
	"encoding/json"
	"testing"
)

func nested(name string, tools []ToolRef, below ...NestedAgent) NestedAgent {
	return NestedAgent{ToolName: name, Node: DefinitionNode{Name: name, Tools: tools, NestedAgents: below}}
}

func versionOf(t *testing.T, root DefinitionNode) StoredVersion {
	t.Helper()
	raw, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return StoredVersion{Definition: raw, Requirements: Requirements{Edges: EdgesOf(root)}}
}

func TestEdgesOfListsEveryNestedAgentThatHoldsAToolkit(t *testing.T) {
	root := DefinitionNode{NestedAgents: []NestedAgent{
		nested("git", []ToolRef{{Key: "toolkit:github:g", SelectedTools: []string{"read", "commit"}}},
			nested("docs", []ToolRef{{Key: "toolkit:confluence:c"}})),
		nested("idle", nil),
	}}
	edges := EdgesOf(root)
	if len(edges) != 2 || edges[0].Path != "git" || edges[1].Path != "git/docs" {
		t.Fatalf("edges %+v", edges)
	}
	if got := edges[0].Holds["toolkit:github:g"]; len(got) != 2 || got[0] != "commit" {
		t.Fatalf("selected tools are sorted: %+v", got)
	}
	if got := edges[1].Holds["toolkit:confluence:c"]; len(got) != 1 || got[0] != allTools {
		t.Fatalf("no selection is every tool: %+v", got)
	}
}

func TestAnEdgeThatHoldsMoreIsAWideningAndOneThatHoldsLessIsNot(t *testing.T) {
	read := []ToolRef{{Key: "k", SelectedTools: []string{"read"}}}
	prev := versionOf(t, DefinitionNode{NestedAgents: []NestedAgent{nested("git", read)}})

	for name, tc := range map[string]struct {
		next []NestedAgent
		want int
	}{
		"same":             {[]NestedAgent{nested("git", read)}, 0},
		"every tool":       {[]NestedAgent{nested("git", []ToolRef{{Key: "k", SelectedTools: []string{}}})}, 1}, // every tool
		"a new tool":       {[]NestedAgent{nested("git", []ToolRef{{Key: "k", SelectedTools: []string{"read", "push"}}})}, 1},
		"a new toolkit":    {[]NestedAgent{nested("git", append([]ToolRef{{Key: "j"}}, read...))}, 1},
		"a new edge":       {[]NestedAgent{nested("git", read), nested("db", []ToolRef{{Key: "sql"}})}, 1},
		"the edge is gone": {nil, 0},
	} {
		next := versionOf(t, DefinitionNode{NestedAgents: tc.next})
		got := WideningOf(prev.Requirements, next.Requirements)
		if len(got) != tc.want {
			t.Fatalf("%s: %+v", name, got)
		}
		for _, w := range got {
			if w.Kind != WidenDelegation {
				t.Fatalf("%s: kind %s", name, w.Kind)
			}
		}
	}
}

func TestAWidenedEdgeBlocksTheAutoPolicy(t *testing.T) {
	read := []ToolRef{{Key: "k", SelectedTools: []string{"read"}}}
	prev := versionOf(t, DefinitionNode{NestedAgents: []NestedAgent{nested("git", read)}})
	next := versionOf(t, DefinitionNode{NestedAgents: []NestedAgent{nested("git", []ToolRef{{Key: "k", SelectedTools: []string{"read", "push"}}})}})
	prev.Requirements.Tier, next.Requirements.Tier = "low", "low"
	if allowed, blocked := AutoUpgradeAllowed("low", prev.Requirements, next.Requirements); allowed || len(blocked) == 0 {
		t.Fatalf("auto must refuse a widening edge: %v %+v", allowed, blocked)
	}
	same := next
	if allowed, _ := AutoUpgradeAllowed("low", next.Requirements, same.Requirements); !allowed {
		t.Fatal("an unchanged edge does not block auto")
	}
}

func TestTheDiffListsTheWidenedEdges(t *testing.T) {
	read := []ToolRef{{Key: "k", SelectedTools: []string{"read"}}}
	prev := versionOf(t, DefinitionNode{NestedAgents: []NestedAgent{nested("git/hub", read)}})
	next := versionOf(t, DefinitionNode{NestedAgents: []NestedAgent{nested("git/hub", []ToolRef{{Key: "k", SelectedTools: []string{"read", "push"}}})}})
	changes, err := Changes(prev, next)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, change := range changes {
		if change.Path == "/delegation/edges/git~1hub" && change.Op == "replace" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the widened edge is in the diff: %+v", changes)
	}
}
