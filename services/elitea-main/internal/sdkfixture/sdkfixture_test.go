package sdkfixture

import (
	"strings"
	"testing"
)

const toolkitDocument = `{
 "fixture_version": "elitea.sdk-fixture.v1", "kind": "toolkit", "type": "demo",
 "upstream": {"repo": "EliteaAI/elitea-sdk", "branch": "main",
  "commit": "0c1ce249846fb92d3bbd15e24b94ae5a6c747a28",
  "paths": ["elitea_sdk/tools/demo/__init__.py"],
  "committed_at": "2026-10-19", "captured_at": "2026-11-18", "captured_with": "pydantic 2.12.5"},
 "model_schema": {"title": "demo", "type": "object", "required": ["demo_configuration"],
  "properties": {
   "demo_configuration": {"$ref": "#/$defs/DemoConfiguration", "configuration_types": ["demo"]},
   "selected_tools": {"default": [], "type": "array", "title": "Selected Tools",
     "items": {"enum": ["a", "b"], "type": "string"}}}},
 "tools": {"a": {"title": "A", "type": "object", "properties": {}},
           "b": {"title": "B", "type": "object", "properties": {"x": {"type": "integer"}}}}
}`

func TestParseRequiresProvenanceAndAKnownKind(t *testing.T) {
	if _, err := Parse([]byte(toolkitDocument)); err != nil {
		t.Fatalf("a complete fixture was refused: %v", err)
	}
	for name, mutate := range map[string]func(string) string{
		"no commit": func(s string) string {
			return strings.Replace(s, "0c1ce249846fb92d3bbd15e24b94ae5a6c747a28", "main", 1)
		},
		"bad date":        func(s string) string { return strings.Replace(s, "2026-10-19", "last week", 1) },
		"unknown kind":    func(s string) string { return strings.Replace(s, `"toolkit"`, `"widget"`, 1) },
		"unknown field":   func(s string) string { return strings.Replace(s, `"type": "demo",`, `"type": "demo", "extra": 1,`, 1) },
		"no tools":        func(s string) string { return strings.Replace(s, `"tools": {`, `"tools_": {`, 1) },
		"another version": func(s string) string { return strings.Replace(s, "sdk-fixture.v1", "sdk-fixture.v0", 1) },
	} {
		if _, err := Parse([]byte(mutate(toolkitDocument))); err == nil {
			t.Errorf("%s: the fixture was accepted", name)
		}
	}
}

func TestDiffToolkitReportsWhatAClientWouldNotice(t *testing.T) {
	fixture, err := Parse([]byte(toolkitDocument))
	if err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{
		"title":    "demo",
		"required": []string{"demo_configuration"},
		"properties": map[string]any{
			"demo_configuration": map[string]any{"$ref": "#/$defs/DemoConfiguration", "configuration_types": []any{"demo"}},
			"selected_tools": map[string]any{
				"default": []any{}, "type": "array", "title": "other title", "args_schemas": map[string]any{"a": 1},
				"items": map[string]any{"enum": []any{"a", "b"}, "type": "string"},
			},
		},
	}
	args := map[string]map[string]any{
		"a": {"title": "A", "type": "object", "properties": map[string]any{}},
		"b": {"title": "B", "type": "object", "properties": map[string]any{"x": map[string]any{"type": "integer"}}},
	}
	if diffs := fixture.DiffToolkit(settings, args); len(diffs) != 0 {
		t.Fatalf("an equal definition drifted: %v", diffs)
	}
	delete(args, "b")
	args["c"] = map[string]any{"title": "C"}
	args["a"]["properties"] = map[string]any{"y": 1}
	settings["required"] = []string{}
	diffs := strings.Join(fixture.DiffToolkit(settings, args), "\n")
	for _, want := range []string{
		`tool "b" is in upstream and missing here`,
		`tool "c" is here and not in upstream`,
		`tool "a": argument schema`,
		`required settings field "demo_configuration"`,
	} {
		if !strings.Contains(diffs, want) {
			t.Errorf("%q went unreported:\n%s", want, diffs)
		}
	}
}
