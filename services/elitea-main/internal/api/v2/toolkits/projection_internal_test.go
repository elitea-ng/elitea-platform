package toolkits

// The merge point's contract, pinned.
//
// These are internal tests because `mergeProjectedTypes` is the seam two
// branches compose through, and a change to its rules must break a test in the
// same package rather than only an end-to-end assertion.

import (
	"context"
	"errors"
	"testing"
)

// stubProjection drives the merge from a table.
type stubProjection struct {
	name      string
	projected []ProjectedToolkitType
	err       error
	calls     int
}

func (s *stubProjection) Name() string { return s.name }

func (s *stubProjection) ProjectToolkitTypes(context.Context) ([]ProjectedToolkitType, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.projected, nil
}

func schemaOf(t *testing.T, catalogue map[string]map[string]any, key string) map[string]any {
	t.Helper()
	schema, found := catalogue[key]
	if !found {
		t.Fatalf("catalogue has no %q; keys are %v", key, keysOf(catalogue))
	}
	return schema
}

func keysOf(catalogue map[string]map[string]any) []string {
	keys := make([]string, 0, len(catalogue))
	for key := range catalogue {
		keys = append(keys, key)
	}
	sortStrings(keys)
	return keys
}

func TestMergeProjectedTypesWithoutSourcesReturnsTheBaseUnchanged(t *testing.T) {
	base := map[string]map[string]any{"github": {"type": "object"}}
	merged := mergeProjectedTypes(base)
	if len(merged) != 1 {
		t.Fatalf("want the base alone, got %v", keysOf(merged))
	}
	if _, found := merged["github"]; !found {
		t.Fatal("the base entry did not survive")
	}
}

func TestMergeProjectedTypesAddsATypeOnlyTheProjectionDeclares(t *testing.T) {
	base := map[string]map[string]any{"github": {"type": "object"}}
	merged := mergeProjectedTypes(base, []ProjectedToolkitType{
		{Type: "mcp_context7", Schema: map[string]any{"title": "mcp_context7"}},
	})
	if _, found := merged["github"]; !found {
		t.Fatal("a projection removed a base entry")
	}
	if got := schemaOf(t, merged, "mcp_context7")["title"]; got != "mcp_context7" {
		t.Fatalf("projected title = %v", got)
	}
}

func TestMergeProjectedTypesNeverOverwritesABaseKeyThatCarriesAValue(t *testing.T) {
	base := map[string]map[string]any{"mcp": {
		"title":    "mcp",
		"metadata": map[string]any{"label": "The SDK's own label"},
	}}
	merged := mergeProjectedTypes(base, []ProjectedToolkitType{
		{Type: "mcp", Schema: map[string]any{
			"title":    "projected",
			"metadata": map[string]any{"label": "Remote MCP"},
			"required": []any{"url"},
		}},
	})
	schema := schemaOf(t, merged, "mcp")
	if schema["title"] != "mcp" {
		t.Fatalf("the projection overwrote a base key: title = %v", schema["title"])
	}
	metadata, _ := schema["metadata"].(map[string]any)
	if metadata["label"] != "The SDK's own label" {
		t.Fatalf("the projection overwrote a non-empty base metadata block: %v", metadata)
	}
	required, _ := schema["required"].([]any)
	if len(required) != 1 || required[0] != "url" {
		t.Fatalf("the projection did not fill the absent key: required = %v", schema["required"])
	}
}

// The case the whole projection exists for: the pinned SDK snapshot's `mcp`
// entry is `{"args_schemas":{},"properties":{},"type":"mcp"}`. An empty
// properties block must be treated as absent, or the Remote MCP settings never
// reach the create form and the page stays blank.
func TestMergeProjectedTypesFillsAnEmptyBaseNode(t *testing.T) {
	base := map[string]map[string]any{"mcp": {
		"type":       "mcp",
		"properties": map[string]any{},
	}}
	merged := mergeProjectedTypes(base, []ProjectedToolkitType{remoteMCPType()})
	properties, _ := schemaOf(t, merged, "mcp")["properties"].(map[string]any)
	if _, found := properties["url"]; !found {
		t.Fatalf("an empty base properties block was treated as declared: %v", properties)
	}
}

func TestMergeProjectedTypesFillsPropertiesOneByOne(t *testing.T) {
	base := map[string]map[string]any{"mcp": {
		"properties": map[string]any{
			"url": map[string]any{"type": "string", "title": "The base URL field"},
		},
	}}
	merged := mergeProjectedTypes(base, []ProjectedToolkitType{remoteMCPType()})
	properties, _ := schemaOf(t, merged, "mcp")["properties"].(map[string]any)
	url, _ := properties["url"].(map[string]any)
	if url["title"] != "The base URL field" {
		t.Fatalf("a declared property was replaced: %v", url)
	}
	if _, found := properties["timeout"]; !found {
		t.Fatalf("an absent property was not filled: %v", keysOfAny(properties))
	}
}

func TestMergeProjectedTypesKeepsTheFirstSourcesAnswer(t *testing.T) {
	merged := mergeProjectedTypes(nil,
		[]ProjectedToolkitType{{Type: "mcp", Schema: map[string]any{"owner": "first"}}},
		[]ProjectedToolkitType{{Type: "mcp", Schema: map[string]any{"owner": "second"}}},
	)
	if got := schemaOf(t, merged, "mcp")["owner"]; got != "first" {
		t.Fatalf("a later source won the collision: owner = %v", got)
	}
}

func TestMergeProjectedTypesDoesNotMutateWhatItIsGiven(t *testing.T) {
	baseSchema := map[string]any{"properties": map[string]any{}}
	base := map[string]map[string]any{"mcp": baseSchema}
	projectedSchema := map[string]any{"properties": map[string]any{"url": map[string]any{"type": "string"}}}

	mergeProjectedTypes(base, []ProjectedToolkitType{{Type: "mcp", Schema: projectedSchema}})

	baseProperties, _ := baseSchema["properties"].(map[string]any)
	if len(baseProperties) != 0 {
		t.Fatalf("the base schema was written to: %v", baseSchema)
	}
	projectedProperties, _ := projectedSchema["properties"].(map[string]any)
	if len(projectedProperties) != 1 {
		t.Fatalf("the projected schema was written to: %v", projectedSchema)
	}
}

func TestMergeProjectedTypesSkipsAnEntryWithNoTypeOrNoSchema(t *testing.T) {
	merged := mergeProjectedTypes(nil, []ProjectedToolkitType{
		{Type: "", Schema: map[string]any{"a": 1}},
		{Type: "named", Schema: nil},
		{Type: "kept", Schema: map[string]any{"a": 1}},
	})
	if got := keysOf(merged); len(got) != 1 || got[0] != "kept" {
		t.Fatalf("want only the well-formed entry, got %v", got)
	}
}

func TestEmptySchemaValueTable(t *testing.T) {
	cases := []struct {
		name  string
		value any
		empty bool
	}{
		{"nil", nil, true},
		{"empty string", "", true},
		{"empty map", map[string]any{}, true},
		{"empty slice", []any{}, true},
		{"string", "mcp", false},
		{"map", map[string]any{"a": 1}, false},
		{"slice", []any{"url"}, false},
		{"false", false, false},
		{"zero", 0, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := emptySchemaValue(testCase.value); got != testCase.empty {
				t.Fatalf("emptySchemaValue(%#v) = %v, want %v", testCase.value, got, testCase.empty)
			}
		})
	}
}

func TestProjectedToolkitTypesSkipsAFailingSourceAndKeepsTheRest(t *testing.T) {
	failing := &stubProjection{name: "broken", err: errors.New("table is gone")}
	working := &stubProjection{name: "working", projected: []ProjectedToolkitType{
		{Type: "mcp", Schema: map[string]any{"title": "mcp"}},
	}}
	handler := NewHandlerWithRepo(nil, WithToolkitTypeProjections(failing, working))

	contributions := handler.projectedToolkitTypes(context.Background())

	if failing.calls != 1 || working.calls != 1 {
		t.Fatalf("both sources must be read: failing=%d working=%d", failing.calls, working.calls)
	}
	if len(contributions) != 1 {
		t.Fatalf("want one surviving contribution, got %d", len(contributions))
	}
	merged := mergeProjectedTypes(map[string]map[string]any{"github": {}}, contributions...)
	if _, found := merged["github"]; !found {
		t.Fatal("a failing source emptied the base catalogue")
	}
	if _, found := merged["mcp"]; !found {
		t.Fatal("a failing source suppressed a working one")
	}
}

func TestProjectedToolkitTypesDropsAnEmptyContribution(t *testing.T) {
	empty := &stubProjection{name: "empty"}
	handler := NewHandlerWithRepo(nil, WithToolkitTypeProjections(empty))
	if got := handler.projectedToolkitTypes(context.Background()); len(got) != 0 {
		t.Fatalf("an empty contribution must not be carried: %v", got)
	}
}

func TestWithToolkitTypeProjectionsReplacesTheDefaults(t *testing.T) {
	handler := NewHandlerWithRepo(nil)
	if len(handler.projections) == 0 {
		t.Fatal("the constructor must install the default sources")
	}
	replaced := NewHandlerWithRepo(nil, WithToolkitTypeProjections())
	if len(replaced.projections) != 0 {
		t.Fatalf("the Option must replace, not append: %d sources", len(replaced.projections))
	}
	nilSafe := NewHandlerWithRepo(nil, WithToolkitTypeProjections(nil, &stubProjection{name: "kept"}))
	if len(nilSafe.projections) != 1 {
		t.Fatalf("a nil source must be dropped: %d sources", len(nilSafe.projections))
	}
}

func TestProjectedToolkitTypesOnAHandlerWithNoSources(t *testing.T) {
	var handler *Handler
	if got := handler.projectedToolkitTypes(context.Background()); got != nil {
		t.Fatalf("a nil handler must contribute nothing, got %v", got)
	}
}

func keysOfAny(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sortStrings(keys)
	return keys
}
