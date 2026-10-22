package runtimecomposition

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const settingsDefinitionsTestSDKRevision = "b5113a129329b85d23c2d5c2bf55f18e307414ec"

// The pinned pair is the contract this catalogue exists to join, so the first
// test reads it rather than a fixture: github carries a credential reference,
// and no longer the retired vector-storage (pgvector) one.
func TestPinnedSettingsDefinitionsResolveTheCredentialSectionOnly(t *testing.T) {
	t.Parallel()

	catalog := pinnedSettingsDefinitionCatalog(t)
	definitions, properties, found, err := catalog.ToolkitSettingsDefinitions("github")
	if err != nil || !found {
		t.Fatalf("github definitions: found=%v err=%v", found, err)
	}

	if _, present := definitions["pgvector"]; present {
		t.Errorf("the retired pgvector definition is still emitted; have %v", sortedKeys(definitions))
	}
	if _, present := properties["pgvector_configuration"]; present {
		t.Error("the retired pgvector_configuration property is still emitted")
	}
	for _, expected := range []struct{ key, section string }{
		{"github", "credentials"},
	} {
		definition, ok := definitions[expected.key].(map[string]any)
		if !ok {
			t.Fatalf("no %q definition; have %v", expected.key, sortedKeys(definitions))
		}
		metadata, ok := definition["metadata"].(map[string]any)
		if !ok {
			t.Fatalf("%q definition carries no metadata: %#v", expected.key, definition)
		}
		if metadata["section"] != expected.section {
			t.Errorf("%q section=%#v, want %q", expected.key, metadata["section"], expected.section)
		}
	}

	configuration, ok := properties["github_configuration"].(map[string]any)
	if !ok {
		t.Fatalf("no github_configuration property; have %v", sortedKeys(properties))
	}
	if configuration["$ref"] != "#/$defs/github" {
		t.Errorf("github_configuration $ref=%#v, want %q", configuration["$ref"], "#/$defs/github")
	}
}

// configuration_model names a model family, not a saved configuration, and has
// no catalogue entry. Treating it as a reference would emit a dangling "$ref".
func TestPinnedSettingsDefinitionsIgnoreTheModelAnnotation(t *testing.T) {
	t.Parallel()

	catalog := pinnedSettingsDefinitionCatalog(t)
	definitions, properties, _, err := catalog.ToolkitSettingsDefinitions("github")
	if err != nil {
		t.Fatalf("github definitions: %v", err)
	}
	if _, present := properties["embedding_model"]; present {
		t.Error("embedding_model was treated as a configuration reference")
	}
	if _, present := definitions["embedding"]; present {
		t.Error("an embedding definition was emitted from configuration_model")
	}
}

// Every configuration type the pinned snapshot names must exist in the pinned
// configuration catalogue. This is the pairing check: re-pinning one snapshot
// without the other is what would break it.
func TestPinnedSettingsDefinitionsResolveEveryReferencedConfigurationType(t *testing.T) {
	t.Parallel()

	catalog := pinnedSettingsDefinitionCatalog(t)
	types := pinnedToolkitTypes(t)
	if len(types) == 0 {
		t.Fatal("pinned snapshot declares no toolkit types")
	}

	resolved := 0
	for _, toolkitType := range types {
		definitions, properties, found, err := catalog.ToolkitSettingsDefinitions(toolkitType)
		if err != nil {
			t.Errorf("%s definitions: %v", toolkitType, err)
			continue
		}
		if !found {
			t.Errorf("%s is in the snapshot but reports found=false", toolkitType)
			continue
		}
		for property, raw := range properties {
			schema, ok := raw.(map[string]any)
			if !ok {
				t.Errorf("%s.%s is not an object", toolkitType, property)
				continue
			}
			reference, ok := schema["$ref"].(string)
			if !ok {
				continue
			}
			key := strings.TrimPrefix(reference, "#/$defs/")
			if _, present := definitions[key]; !present {
				t.Errorf("%s.%s points at missing definition %q", toolkitType, property, key)
				continue
			}
			resolved++
		}
	}
	if resolved == 0 {
		t.Fatal("no configuration reference resolved across the whole pinned snapshot")
	}
}

func TestSettingsDefinitionsReportUnknownTypesAsNotFound(t *testing.T) {
	t.Parallel()

	catalog := pinnedSettingsDefinitionCatalog(t)
	definitions, properties, found, err := catalog.ToolkitSettingsDefinitions("not_a_toolkit")
	if err != nil || found || definitions != nil || properties != nil {
		t.Errorf("unknown type: definitions=%v properties=%v found=%v err=%v",
			definitions, properties, found, err)
	}
}

// A settings field that names a configuration type the catalogue does not carry
// must fail the request, not serve a "$ref" with nothing behind it. The web
// client drops such a property without reporting it, which is how #330 stayed
// invisible.
func TestSettingsDefinitionsFailClosedOnAnUnknownConfigurationType(t *testing.T) {
	t.Parallel()

	catalog := settingsDefinitionCatalog(t,
		toolkitSnapshotFixture(map[string]any{
			"ghost_configuration": map[string]any{"configuration_types": []any{"ghost"}},
		}),
		configurationCatalogFixture(map[string]string{"github": "credentials"}),
	)

	_, _, _, err := catalog.ToolkitSettingsDefinitions("probe")
	if !errors.Is(err, ErrCurrentToolkitConfigurationTypeUnknown) {
		t.Fatalf("err=%v, want ErrCurrentToolkitConfigurationTypeUnknown", err)
	}
}

// Several accepted types produce the "anyOf" union Pydantic emits for
// Optional/Union fields, with one branch per type. The pinned revision has no
// such field, so only a fixture can hold this shape to the contract.
func TestSettingsDefinitionsEmitAnyOfForSeveralAcceptedTypes(t *testing.T) {
	t.Parallel()

	catalog := settingsDefinitionCatalog(t,
		toolkitSnapshotFixture(map[string]any{
			"vault": map[string]any{"configuration_types": []any{"github", "jira"}},
		}),
		configurationCatalogFixture(map[string]string{
			"github": "credentials",
			"jira":   "credentials",
		}),
	)

	definitions, properties, found, err := catalog.ToolkitSettingsDefinitions("probe")
	if err != nil || !found {
		t.Fatalf("probe definitions: found=%v err=%v", found, err)
	}
	if len(definitions) != 2 {
		t.Errorf("definitions=%v, want one per accepted type", sortedKeys(definitions))
	}

	vault, ok := properties["vault"].(map[string]any)
	if !ok {
		t.Fatalf("no vault property; have %v", sortedKeys(properties))
	}
	if _, present := vault["$ref"]; present {
		t.Error("a multi-type property served a single $ref")
	}
	branches, ok := vault["anyOf"].([]any)
	if !ok || len(branches) != 2 {
		t.Fatalf("vault anyOf=%#v, want two branches", vault["anyOf"])
	}
	for index, raw := range branches {
		branch, ok := raw.(map[string]any)
		if !ok {
			t.Errorf("branch %d is not an object: %#v", index, raw)
			continue
		}
		reference, ok := branch["$ref"].(string)
		if !ok {
			t.Errorf("branch %d carries no $ref: %#v", index, branch)
			continue
		}
		if _, present := definitions[strings.TrimPrefix(reference, "#/$defs/")]; !present {
			t.Errorf("branch %d points at missing definition %q", index, reference)
		}
	}
}

// A built-in type with no configuration field is not an error. It serves empty
// maps, and the caller omits the "$defs" block entirely.
func TestSettingsDefinitionsReturnEmptyMapsForATypeWithNoConfiguration(t *testing.T) {
	t.Parallel()

	catalog := settingsDefinitionCatalog(t,
		toolkitSnapshotFixture(map[string]any{
			"bucket": map[string]any{"toolkit_name": true},
		}),
		configurationCatalogFixture(map[string]string{"github": "credentials"}),
	)

	definitions, properties, found, err := catalog.ToolkitSettingsDefinitions("probe")
	if err != nil || !found {
		t.Fatalf("probe definitions: found=%v err=%v", found, err)
	}
	if len(definitions) != 0 || len(properties) != 0 {
		t.Errorf("definitions=%v properties=%v, want both empty",
			sortedKeys(definitions), sortedKeys(properties))
	}
}

// The composition root holds this as an interface value, so a nil catalogue
// arrives as a typed non-nil interface. It must report an error, not panic the
// endpoint.
func TestSettingsDefinitionsOnANilCatalogReturnAnError(t *testing.T) {
	t.Parallel()

	var catalog *CurrentToolkitSettingsDefinitionCatalog
	if _, _, _, err := catalog.ToolkitSettingsDefinitions("github"); err == nil {
		t.Fatal("a nil catalog reported no error")
	}
}

func TestNewSettingsDefinitionCatalogRequiresBothSnapshots(t *testing.T) {
	t.Parallel()

	snapshot, err := LoadPinnedCurrentToolkitSchemaSnapshot()
	if err != nil {
		t.Fatalf("load pinned toolkit schema snapshot: %v", err)
	}
	if _, err := NewCurrentToolkitSettingsDefinitionCatalog(snapshot, nil); err == nil {
		t.Error("a missing configuration catalog was accepted")
	}
	if _, err := NewCurrentToolkitSettingsDefinitionCatalog(nil, nil); err == nil {
		t.Error("a missing toolkit snapshot was accepted")
	}
}

func pinnedSettingsDefinitionCatalog(t *testing.T) *CurrentToolkitSettingsDefinitionCatalog {
	t.Helper()
	snapshot, err := LoadPinnedCurrentToolkitSchemaSnapshot()
	if err != nil {
		t.Fatalf("load pinned toolkit schema snapshot: %v", err)
	}
	configurations, err := LoadPinnedCurrentSDKConfigurationCatalog()
	if err != nil {
		t.Fatalf("load pinned SDK configuration catalog: %v", err)
	}
	catalog, err := NewCurrentToolkitSettingsDefinitionCatalog(snapshot, configurations)
	if err != nil {
		t.Fatalf("compose settings definition catalog: %v", err)
	}
	return catalog
}

func settingsDefinitionCatalog(
	t *testing.T,
	snapshotJSON []byte,
	catalogJSON []byte,
) *CurrentToolkitSettingsDefinitionCatalog {
	t.Helper()
	snapshot, err := LoadCurrentToolkitSchemaSnapshot(snapshotJSON)
	if err != nil {
		t.Fatalf("load toolkit schema snapshot fixture: %v (%s)", err, snapshotJSON)
	}
	configurations, err := LoadCurrentSDKConfigurationCatalog(catalogJSON)
	if err != nil {
		t.Fatalf("load SDK configuration catalog fixture: %v (%s)", err, catalogJSON)
	}
	catalog, err := NewCurrentToolkitSettingsDefinitionCatalog(snapshot, configurations)
	if err != nil {
		t.Fatalf("compose settings definition catalog: %v", err)
	}
	return catalog
}

// toolkitSnapshotFixture builds a one-entry snapshot named "probe" whose
// annotation projection is the supplied properties map.
func toolkitSnapshotFixture(properties map[string]any) []byte {
	document := map[string]any{
		"schema_version": "elitea.current-toolkit-schema-snapshot.v1",
		"sdk_revision":   settingsDefinitionsTestSDKRevision,
		"entries": []any{
			map[string]any{
				"type":         "probe",
				"properties":   properties,
				"args_schemas": map[string]any{},
				"naming":       map[string]any{"field": nil, "max_length": 0},
			},
		},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return encoded
}

// configurationCatalogFixture builds a catalogue of the supplied
// configuration-type to section pairs. Entries must be sorted by type. The
// loader checks digest shape only, so distinct well-formed digests suffice.
func configurationCatalogFixture(sections map[string]string) []byte {
	types := sortedKeys(sections)
	entries := make([]any, 0, len(types))
	for index, configurationType := range types {
		entries = append(entries, map[string]any{
			"configuration_type":         configurationType,
			"section":                    sections[configurationType],
			"schema_id":                  "elitea.configuration." + configurationType,
			"schema_revision":            settingsDefinitionsTestSDKRevision,
			"schema_digest":              fmt.Sprintf("sha256:%064x", index+1),
			"validation_supported":       true,
			"connection_check_supported": false,
		})
	}
	document := map[string]any{
		"schema_version":   "elitea.worker-sdk-configuration-catalog.v1",
		"sdk_revision":     settingsDefinitionsTestSDKRevision,
		"catalog_revision": settingsDefinitionsTestSDKRevision,
		"catalog_digest":   fmt.Sprintf("sha256:%064x", 0),
		"complete":         true,
		"entry_count":      len(entries),
		"entries":          entries,
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return encoded
}

func pinnedToolkitTypes(t *testing.T) []string {
	t.Helper()
	var document struct {
		Entries []struct {
			Type string `json:"type"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(pinnedCurrentToolkitSchemaSnapshotJSON, &document); err != nil {
		t.Fatalf("decode pinned toolkit snapshot: %v", err)
	}
	types := make([]string, 0, len(document.Entries))
	for _, entry := range document.Entries {
		types = append(types, entry.Type)
	}
	return types
}
