package run_test

// The fixture runner must answer the argument names the DESCRIPTOR declares.
//
// WHY THIS TEST EXISTS. Every entity tool in the fixture read `entity_id`,
// `entity` and `id`, and the descriptor declares `entity_name` for four of
// them — `get_entity`, `get_entity_content`, `impact_analysis` and
// `get_related_entities`. A caller that followed the descriptor, which is the
// document the facade admits a tool by and the document a browser reads to
// learn what to send, got `resource_not_found` for an entity the graph holds.
// Nothing failed: the call was accepted, the answer was well formed, and the
// screen said the entity did not exist — on the row the user had just clicked.
// `remove_source_entities` had the same shape; it declares `toolkit_id` and
// read `source_toolkit`/`source`.
//
// The expectations are read OUT OF THE DESCRIPTOR rather than written down
// here, so a revision that renames an argument fails in this test rather than
// in a browser journey — or, as happened, in neither.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// entityTools maps a tool to the argument the descriptor is expected to
// declare for it. The expectation is asserted against the descriptor first, so
// this table cannot quietly disagree with the document.
var entityTools = map[string]string{
	"get_entity":           "entity_name",
	"get_entity_content":   "entity_name",
	"impact_analysis":      "entity_name",
	"get_related_entities": "entity_name",
	"get_entity_neighbors": "entity_id",
}

func TestTheFixtureAnswersTheDescriptorsArgumentNames(t *testing.T) {
	declared := declaredArguments(t)
	entity := run_FirstFixtureEntityID()

	for tool, argument := range entityTools {
		t.Run(tool, func(t *testing.T) {
			if !declared[tool][argument] {
				t.Fatalf("the descriptor no longer declares %q for %s; it declares %s",
					argument, tool, keysOf(declared[tool]))
			}

			h := fixtureHarness(t)
			// The entity is named ONLY by the descriptor's own argument. Sending
			// several spellings would pass against a runner that read any one of
			// them, which is the failure this test exists to catch.
			body, err := h.invoke("inventory", "inventory", tool,
				read(map[string]any{argument: entity, "output_format": "json"}))
			if err != nil {
				t.Fatalf("%s refused an entity the graph holds: %v", tool, err)
			}
			if text := message(t, body); strings.Contains(strings.ToLower(text), "no entity") {
				t.Fatalf("%s answered a not-found for %q: %s", tool, entity, text)
			}
		})
	}
}

func TestRemoveSourceEntitiesReadsTheDeclaredToolkitID(t *testing.T) {
	declared := declaredArguments(t)
	if !declared["remove_source_entities"]["toolkit_id"] {
		t.Fatalf("the descriptor no longer declares toolkit_id for remove_source_entities; it declares %s",
			keysOf(declared["remove_source_entities"]))
	}

	h := fixtureHarness(t)
	// `code` is the source two of the fixture's six entities cite, so a runner
	// that reads the argument reports a non-zero removal and one that ignores
	// it reports zero. The two answers are distinguishable, which is what makes
	// the assertion worth making.
	body, err := h.invoke("inventory", "inventory", "remove_source_entities",
		read(map[string]any{"toolkit_id": "code", "output_format": "json"}))
	if err != nil {
		t.Fatalf("remove_source_entities: %v", err)
	}
	removed, _ := document(t, body)["removed_entities"].(float64)
	if removed == 0 {
		t.Fatalf("toolkit_id was not read: the tool removed 0 entities")
	}
}

// run_FirstFixtureEntityID is the id of the first canned entity, which every
// retrieval tool must be able to resolve.
func run_FirstFixtureEntityID() string { return "code:checkout-service" }

// declaredArguments reads the descriptor's own tool table: tool name → the set
// of argument names it declares.
func declaredArguments(t *testing.T) map[string]map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "descriptor.json"))
	if err != nil {
		t.Fatalf("read descriptor: %v", err)
	}
	var descriptor struct {
		Toolkits []struct {
			Tools []struct {
				Name string          `json:"name"`
				Args json.RawMessage `json:"args_schema"`
			} `json:"provided_tools"`
		} `json:"provided_toolkits"`
	}
	if err := json.Unmarshal(raw, &descriptor); err != nil {
		t.Fatalf("parse descriptor: %v", err)
	}
	declared := map[string]map[string]bool{}
	for _, toolkit := range descriptor.Toolkits {
		for _, tool := range toolkit.Tools {
			names, ok := declared[tool.Name]
			if !ok {
				names = map[string]bool{}
				declared[tool.Name] = names
			}
			var args map[string]json.RawMessage
			if err := json.Unmarshal(tool.Args, &args); err != nil {
				continue
			}
			for name := range args {
				names[name] = true
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("the descriptor declared no tool arguments, so this test gates nothing")
	}
	return declared
}

func keysOf(set map[string]bool) string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
