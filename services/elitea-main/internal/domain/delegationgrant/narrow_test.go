package delegationgrant

import (
	"reflect"
	"testing"
)

func keys(s GrantSet) []string {
	var out []string
	for _, toolkit := range s.Normalize().Toolkits {
		out = append(out, toolkit.Key)
	}
	return out
}

func toolkits(list ...ToolkitGrant) GrantSet { return GrantSet{Toolkits: list} }

// own(X) ∩ (E(Y) ∪ G): the same cases as the worker's narrow_with_grant tests
// (libs/rust/agent-runtime/src/delegation_tests.rs).
func TestNarrowWithGrantIsOwnIntersectCallerUnionGrant(t *testing.T) {
	labels := EdgeLabels{"toolkit:1": "Git", "toolkit:2": "Jira"}
	caller := toolkits(only("toolkit:1", "", "read"))
	grant := toolkits(only("toolkit:2", "ref-a", "search"))
	t.Run("what the grant covers runs, the rest is narrowed out", func(t *testing.T) {
		requested := toolkits(only("toolkit:1", "", "read"), only("toolkit:2", "ref-a", "search", "delete"), all("toolkit:3"))
		effective, narrowed := NarrowWithGrant(caller, grant, requested, labels)
		if got := keys(effective); !reflect.DeepEqual(got, []string{"toolkit:1", "toolkit:2"}) {
			t.Fatalf("effective toolkits = %v", got)
		}
		want := []Item{
			{Kind: ItemTool, Name: "delete", Toolkit: "Jira"},
			{Kind: ItemToolkit, Name: "toolkit:3"},
		}
		if !reflect.DeepEqual(narrowed, want) {
			t.Fatalf("narrowed = %+v, want %+v", narrowed, want)
		}
		if got := effective.Toolkits[1].Tools; got.Mode != ToolsOnly || !reflect.DeepEqual(got.Names, []string{"search"}) {
			t.Fatalf("Jira tools = %+v", got)
		}
	})
	t.Run("a callee within its caller and grant loses nothing", func(t *testing.T) {
		requested := toolkits(only("toolkit:1", "", "read"))
		effective, narrowed := NarrowWithGrant(caller, grant, requested, labels)
		if len(narrowed) != 0 || !reflect.DeepEqual(keys(effective), []string{"toolkit:1"}) {
			t.Fatalf("effective %v narrowed %+v", keys(effective), narrowed)
		}
	})
	t.Run("a credential nobody holds drops its toolkit", func(t *testing.T) {
		requested := toolkits(only("toolkit:2", "ref-other", "search"))
		effective, narrowed := NarrowWithGrant(caller, grant, requested, labels)
		want := []Item{{Kind: ItemCredential, Name: "ref-other", Toolkit: "Jira"}}
		if len(effective.Toolkits) != 0 || !reflect.DeepEqual(narrowed, want) {
			t.Fatalf("effective %+v narrowed %+v", effective, narrowed)
		}
	})
	t.Run("a toolkit that selected every tool is cut to the approved tools", func(t *testing.T) {
		requested := toolkits(all("toolkit:2"))
		effective, narrowed := NarrowWithGrant(caller, toolkits(only("toolkit:2", "", "search")), requested, labels)
		want := []Item{{Kind: ItemTool, Name: "*", Toolkit: "Jira"}}
		if !reflect.DeepEqual(narrowed, want) || effective.Toolkits[0].Tools.Mode != ToolsOnly {
			t.Fatalf("effective %+v narrowed %+v", effective, narrowed)
		}
	})
	t.Run("the caller's own tools widen the grant", func(t *testing.T) {
		requested := toolkits(only("toolkit:1", "", "read", "write"))
		_, narrowed := NarrowWithGrant(toolkits(only("toolkit:1", "", "write")), toolkits(only("toolkit:1", "", "read")), requested, labels)
		if len(narrowed) != 0 {
			t.Fatalf("narrowed = %+v", narrowed)
		}
	})
}

func TestRefuseWithoutGrantNamesTheFirstThingTheCallerLacks(t *testing.T) {
	labels := EdgeLabels{"toolkit:2": "Jira"}
	caller := toolkits(only("toolkit:1", "ref-a", "read"))
	cases := []struct {
		name      string
		requested GrantSet
		want      Item
		refused   bool
	}{
		{"within the caller", toolkits(only("toolkit:1", "ref-a", "read")), Item{}, false},
		{"a toolkit the caller lacks", toolkits(all("toolkit:2")), Item{Kind: ItemToolkit, Name: "Jira"}, true},
		{"a credential the caller lacks", toolkits(only("toolkit:1", "ref-b", "read")), Item{Kind: ItemCredential, Name: "ref-b", Toolkit: "toolkit:1"}, true},
		{"a tool the caller lacks", toolkits(only("toolkit:1", "ref-a", "read", "write")), Item{Kind: ItemTool, Name: "write", Toolkit: "toolkit:1"}, true},
		// The worker does not refuse a callee that selects every tool where the
		// caller selected some: it cuts it (NarrowToCaller).
		{"every tool where the caller selected some", toolkits(all("toolkit:1")), Item{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, refused := RefuseWithoutGrant(caller, c.requested, labels)
			if refused != c.refused || got != c.want {
				t.Fatalf("got %+v %v, want %+v %v", got, refused, c.want, c.refused)
			}
		})
	}
}

func TestNarrowToCallerCutsSelectionsToTheCallers(t *testing.T) {
	caller := toolkits(only("toolkit:1", "", "read"), all("toolkit:2", "drop"))
	got := NarrowToCaller(caller, toolkits(all("toolkit:1"), all("toolkit:2")))
	if got.Toolkits[0].Tools.Mode != ToolsOnly || !reflect.DeepEqual(got.Toolkits[0].Tools.Names, []string{"read"}) {
		t.Fatalf("toolkit 1 = %+v", got.Toolkits[0])
	}
	if got.Toolkits[1].Tools.Mode != ToolsAllExcept || !reflect.DeepEqual(got.Toolkits[1].Tools.Names, []string{"drop"}) {
		t.Fatalf("toolkit 2 = %+v", got.Toolkits[1])
	}
}
