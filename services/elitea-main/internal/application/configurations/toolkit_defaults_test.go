package configurations_test

// ApplyToolkitSettingsDefaults, the write-path half of #978.
//
// The behaviour that matters is not "a default is copied" but WHICH keys are
// left alone: a filled key that should have stayed absent is a silent change
// to a saved toolkit, and this function runs on every toolkit write.

import (
	"testing"

	configurations "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

func qtestShapedSchema() map[string]any {
	return map[string]any{
		"properties": map[string]any{
			// The field the issue is about: optional, integer, defaulted.
			"no_of_tests_shown_in_dql_search": map[string]any{
				"anyOf":   []any{map[string]any{"type": "integer"}, map[string]any{"type": "null"}},
				"default": 10,
			},
			// A credential reference with a null default — never written.
			"pgvector_configuration": map[string]any{
				"configuration_types": []any{"pgvector"},
				"default":             nil,
			},
			// Every SDK type carries this, defaulted to the empty list.
			"selected_tools": map[string]any{
				"default": []any{},
				"type":    "array",
			},
			// A required field with no default — never written.
			"qtest_project_id": map[string]any{"type": "integer"},
		},
	}
}

func TestApplyToolkitSettingsDefaultsFillsTheOmittedDefaultedField(t *testing.T) {
	t.Parallel()

	settings := map[string]any{"qtest_project_id": 1}
	added := configurations.ApplyToolkitSettingsDefaults(qtestShapedSchema(), settings)

	if added != 1 {
		t.Fatalf("added = %d, want 1 (only the defaulted integer); settings=%v", added, settings)
	}
	if settings["no_of_tests_shown_in_dql_search"] != 10 {
		t.Fatalf("no_of_tests_shown_in_dql_search = %v, want 10", settings["no_of_tests_shown_in_dql_search"])
	}
	if _, present := settings["pgvector_configuration"]; present {
		t.Fatal("a null default was written — the save-time resolver would be handed a null reference")
	}
	if _, present := settings["selected_tools"]; present {
		t.Fatal("an empty-list default was written — absent and [] are not the same to every reader")
	}
	if _, present := settings["qtest_project_id"]; !present {
		t.Fatal("the caller's own value was dropped")
	}
}

// A value the caller sent — including an explicit null — is never replaced.
func TestApplyToolkitSettingsDefaultsNeverOverwritesACallerValue(t *testing.T) {
	t.Parallel()

	settings := map[string]any{"no_of_tests_shown_in_dql_search": 25}
	if added := configurations.ApplyToolkitSettingsDefaults(qtestShapedSchema(), settings); added != 0 {
		t.Fatalf("added = %d, want 0", added)
	}
	if settings["no_of_tests_shown_in_dql_search"] != 25 {
		t.Fatalf("the caller's 25 was replaced with %v", settings["no_of_tests_shown_in_dql_search"])
	}

	cleared := map[string]any{"no_of_tests_shown_in_dql_search": nil}
	if added := configurations.ApplyToolkitSettingsDefaults(qtestShapedSchema(), cleared); added != 0 {
		t.Fatalf("added = %d for an explicit null, want 0", added)
	}
	if value, present := cleared["no_of_tests_shown_in_dql_search"]; !present || value != nil {
		t.Fatalf("an explicitly cleared field became %v", value)
	}
}

// The stored settings must not alias the process-wide catalogue snapshot.
func TestApplyToolkitSettingsDefaultsClonesContainerDefaults(t *testing.T) {
	t.Parallel()

	shared := map[string]any{"mode": "fast", "tags": []any{"a"}}
	schema := map[string]any{"properties": map[string]any{
		"options": map[string]any{"default": shared},
	}}

	settings := map[string]any{}
	if added := configurations.ApplyToolkitSettingsDefaults(schema, settings); added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}
	stored, ok := settings["options"].(map[string]any)
	if !ok {
		t.Fatalf("options = %v, want an object", settings["options"])
	}
	stored["mode"] = "slow"
	if shared["mode"] != "fast" {
		t.Fatal("the stored settings alias the catalogue snapshot — one save mutated it for every later read")
	}
}

// A schema the catalogue cannot describe must leave the save untouched rather
// than fail it.
func TestApplyToolkitSettingsDefaultsIgnoresMalformedInput(t *testing.T) {
	t.Parallel()

	cases := map[string]map[string]any{
		"nil schema":            nil,
		"no properties":         {"type": "object"},
		"properties not object": {"properties": []any{"no_of_tests_shown_in_dql_search"}},
		"property not object":   {"properties": map[string]any{"a": "b"}},
	}
	for name, schema := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			settings := map[string]any{"kept": true}
			if added := configurations.ApplyToolkitSettingsDefaults(schema, settings); added != 0 {
				t.Fatalf("added = %d, want 0", added)
			}
			if len(settings) != 1 || settings["kept"] != true {
				t.Fatalf("settings were changed: %v", settings)
			}
		})
	}
	if added := configurations.ApplyToolkitSettingsDefaults(qtestShapedSchema(), nil); added != 0 {
		t.Fatalf("a nil settings map reported %d additions", added)
	}
}
