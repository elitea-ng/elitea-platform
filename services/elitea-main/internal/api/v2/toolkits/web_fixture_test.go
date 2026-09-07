package toolkits_test

// The per-type SETTINGS fixture the web unit suite renders its form against,
// and the comparison that keeps it honest.
//
// The web suite already had a catalogue fixture — servedToolkitTypeCatalogue.json
// — but it carries only each type's `metadata`, so it can drive the type chooser
// and nothing else. The form renderer is generic, and a generic renderer tested
// against two hand-written schemas is tested against two hand-written schemas:
// it says nothing about whether the fifty-six served types render.
//
// This test writes and checks a second fixture, servedToolkitTypeSettings.json,
// holding a TRIMMED but real JSON Schema per served type. Trimmed, because the
// full catalogue is dominated by prose descriptions that no assertion reads and
// that would put a 78 KB payload in a unit-test bundle. Real, because the shape
// the web test renders has to be the shape the server sends — a fixture invented
// beside the client is a second source of truth that keeps passing while the
// server serves something else, which is exactly the failure
// TestTheWebChooserFixtureMatchesTheServedCatalogue exists to stop.
//
// Regenerate with:
//
//	ELITEA_WRITE_TOOLKIT_WEB_FIXTURES=1 go test ./internal/api/v2/toolkits/ \
//	  -run TestTheWebPerTypeSettingsFixtureMatchesTheServedCatalogue
//
// Never edit the file by hand to make this pass.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

const webFixtureWriteEnv = "ELITEA_WRITE_TOOLKIT_WEB_FIXTURES"

// maxFixtureEnumMembers bounds each enum and each tool list. Six is enough for
// the web assertions (they check that the chip picker renders and that
// index_data is present or absent) and keeps the fixture inside a size a unit
// bundle can carry.
const maxFixtureEnumMembers = 6

// fixtureSchemaKeys are the property-schema keys the web form actually reads.
// Everything else — description, examples, tooltip, minimum, default — is
// dropped: no assertion depends on it and the prose is most of the bytes.
var fixtureSchemaKeys = []string{
	"type", "title", "format", "secret", "writeOnly", "enum", "$ref",
	"configuration_types", "configuration_model", "toolkit_name", "ui_component",
}

func TestTheWebPerTypeSettingsFixtureMatchesTheServedCatalogue(t *testing.T) {
	t.Parallel()

	catalogue := getToolkitTypeCatalogue(t, catalogueOptions(t, "python")...)
	projected := make(map[string]any, len(catalogue))
	for _, toolkitType := range sortedKeys(catalogue) {
		typeSchema, ok := catalogue[toolkitType].(map[string]any)
		if !ok {
			t.Fatalf("served entry %q is not an object", toolkitType)
		}
		projected[toolkitType] = projectTypeSchemaForWeb(toolkitType, typeSchema)
	}

	want, err := json.MarshalIndent(projected, "", "  ")
	if err != nil {
		t.Fatalf("encode the projection: %v", err)
	}
	want = append(want, '\n')

	path := filepath.Join(
		repositoryRootFromTest(t), "apps", "elitea-web", "src", "entities", "toolkit",
		"model", "__fixtures__", "servedToolkitTypeSettings.json",
	)
	if os.Getenv(webFixtureWriteEnv) == "1" {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatalf("write the web settings fixture: %v", err)
		}
		t.Logf("wrote %s (%d bytes, %d types)", path, len(want), len(projected))
		return
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the web settings fixture: %v\nRegenerate it with %s=1.", err, webFixtureWriteEnv)
	}
	if string(got) != string(want) {
		// Report the DIFFERENCE in type coverage first: that is the failure
		// that means a served type has no web fixture, which is the one this
		// gate exists for. A changed schema body reports as the fallback.
		var fixture map[string]json.RawMessage
		if err := json.Unmarshal(got, &fixture); err != nil {
			t.Fatalf("the committed web settings fixture does not parse: %v", err)
		}
		var missing, extra []string
		for toolkitType := range projected {
			if _, present := fixture[toolkitType]; !present {
				missing = append(missing, toolkitType)
			}
		}
		for toolkitType := range fixture {
			if _, served := projected[toolkitType]; !served {
				extra = append(extra, toolkitType)
			}
		}
		sort.Strings(missing)
		sort.Strings(extra)
		if len(missing) > 0 || len(extra) > 0 {
			t.Fatalf("the web per-type settings fixture is out of step with the served"+
				" catalogue.\n  served but not in the fixture (no web test covers them): %v"+
				"\n  in the fixture but not served: %v\nRegenerate with %s=1.",
				missing, extra, webFixtureWriteEnv)
		}
		t.Fatalf("the web per-type settings fixture holds %d types, the same %d the"+
			" catalogue serves, but at least one schema differs. Regenerate with %s=1"+
			" — do not edit the file to match.", len(fixture), len(projected), webFixtureWriteEnv)
	}
}

// projectTypeSchemaForWeb trims one served type schema to what the web form
// reads.
func projectTypeSchemaForWeb(toolkitType string, typeSchema map[string]any) map[string]any {
	projection := map[string]any{"type": "object"}
	title, _ := typeSchema["title"].(string)
	if title == "" {
		title = toolkitType
	}
	projection["title"] = title
	if required, ok := typeSchema["required"].([]any); ok && len(required) > 0 {
		projection["required"] = required
	}
	if metadata, ok := typeSchema[metadataFixtureKey].(map[string]any); ok {
		projection[metadataFixtureKey] = metadata
	}
	if nameRequired, ok := typeSchema["name_required"].(bool); ok {
		projection["name_required"] = nameRequired
	}
	properties, _ := typeSchema["properties"].(map[string]any)
	trimmed := make(map[string]any, len(properties))
	for name, raw := range properties {
		property, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name == "selected_tools" {
			trimmed[name] = projectSelectedToolsForWeb(property)
			continue
		}
		trimmed[name] = projectPropertyForWeb(property)
	}
	projection["properties"] = trimmed
	return projection
}

const metadataFixtureKey = "metadata"

func projectPropertyForWeb(property map[string]any) map[string]any {
	trimmed := make(map[string]any, len(fixtureSchemaKeys))
	for _, key := range fixtureSchemaKeys {
		value, present := property[key]
		if !present {
			continue
		}
		if key == "enum" {
			trimmed[key] = capList(value)
			continue
		}
		trimmed[key] = value
	}
	if branches, ok := property["anyOf"].([]any); ok {
		projected := make([]any, 0, len(branches))
		for _, raw := range branches {
			branch, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			projected = append(projected, projectPropertyForWeb(branch))
		}
		trimmed["anyOf"] = projected
	}
	if items, ok := property["items"].(map[string]any); ok {
		trimmed["items"] = projectPropertyForWeb(items)
	}
	return trimmed
}

// projectSelectedToolsForWeb keeps the two things the web reads off this node:
// items.enum, which draws the tool chips, and args_schemas, whose keys decide
// whether the Indexes tab is offered.
//
// index_data is kept whatever the cap says. It is the discriminator for the
// whole index surface, and a cap that dropped it would make an indexing type
// look like a non-indexing one in every web test at once.
func projectSelectedToolsForWeb(property map[string]any) map[string]any {
	trimmed := map[string]any{"type": "array"}
	if title, ok := property["title"]; ok {
		trimmed["title"] = title
	}
	if items, ok := property["items"].(map[string]any); ok {
		if enum, present := items["enum"]; present {
			trimmed["items"] = map[string]any{
				"type": "string", "enum": capListKeeping(enum, "index_data"),
			}
		}
	}
	if args, ok := property["args_schemas"].(map[string]any); ok {
		names := make([]string, 0, len(args))
		for name := range args {
			names = append(names, name)
		}
		sort.Strings(names)
		kept := make(map[string]any, maxFixtureEnumMembers+1)
		for _, name := range capStringsKeeping(names, "index_data") {
			kept[name] = map[string]any{"type": "object"}
		}
		trimmed["args_schemas"] = kept
	}
	return trimmed
}

func capList(value any) any {
	list, ok := value.([]any)
	if !ok || len(list) <= maxFixtureEnumMembers {
		return value
	}
	return list[:maxFixtureEnumMembers]
}

func capListKeeping(value any, keep string) any {
	list, ok := value.([]any)
	if !ok {
		return value
	}
	names := make([]string, 0, len(list))
	for _, item := range list {
		name, ok := item.(string)
		if !ok {
			return capList(value)
		}
		names = append(names, name)
	}
	capped := capStringsKeeping(names, keep)
	result := make([]any, 0, len(capped))
	for _, name := range capped {
		result = append(result, name)
	}
	return result
}

func capStringsKeeping(names []string, keep string) []string {
	if len(names) <= maxFixtureEnumMembers {
		return names
	}
	capped := append([]string(nil), names[:maxFixtureEnumMembers]...)
	for _, name := range names {
		if name != keep {
			continue
		}
		if !containsString(capped, keep) {
			capped[len(capped)-1] = keep
			sort.Strings(capped)
		}
		break
	}
	return capped
}

func containsString(names []string, needle string) bool {
	for _, name := range names {
		if name == needle {
			return true
		}
	}
	return false
}
