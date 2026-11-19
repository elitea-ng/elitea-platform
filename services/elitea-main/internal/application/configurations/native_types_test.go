package configurations

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/sdkfixture"
)

// stubNativeType is the worked example of a post-pin credential type: a
// delegated-OAuth-shaped definition with a secret and a scope list, as the
// Microsoft types of epic #2254 will be. It is a test fixture, not served.
func stubNativeType() NativeConfigurationType {
	return NativeConfigurationType{
		Type:            "stub_example",
		Section:         "credentials",
		Title:           "StubExampleConfiguration",
		Label:           "Stub example",
		Categories:      []string{"other"},
		ExtraCategories: []string{"stub"},
		Properties: map[string]any{
			"api_key": map[string]any{
				"title": "API Key", "type": "string", "format": "password",
				"secret": true, "writeOnly": true, "description": "The key.",
			},
			"scopes": map[string]any{
				"title": "Scopes", "type": "array", "items": map[string]any{"type": "string"},
				"default": []any{"User.Read"},
			},
		},
		Required:        []string{"api_key"},
		AuthSubsections: []NativeAuthSubsection{{Name: "API key", Fields: []string{"api_key"}}},
		CheckConnection: true,
	}
}

const stubFixture = `{
 "fixture_version": "elitea.sdk-fixture.v1",
 "kind": "configuration",
 "type": "stub_example",
 "upstream": {"repo": "EliteaAI/elitea-sdk", "branch": "main",
  "commit": "0c1ce249846fb92d3bbd15e24b94ae5a6c747a28",
  "paths": ["elitea_sdk/configurations/stub_example.py"],
  "committed_at": "2026-10-19", "captured_at": "2026-11-18",
  "captured_with": "pydantic 2.12.5 model_json_schema()"},
 "model_schema": {"title": "StubExampleConfiguration", "type": "object",
  "required": ["api_key"],
  "properties": {
   "api_key": {"title": "Api Key", "type": "string", "format": "password", "secret": true, "writeOnly": true},
   "scopes": {"title": "Scopes", "type": "array", "items": {"type": "string"}, "default": ["User.Read"]}}}
}`

func loadWithNative(t *testing.T, natives ...NativeConfigurationType) (*CurrentAvailableCatalog, error) {
	t.Helper()
	catalog, err := LoadCurrentAvailableCatalog([]byte(pinnedCurrentAvailableSnapshot))
	if err != nil {
		t.Fatalf("the pinned snapshot did not load: %v", err)
	}
	return catalog, catalog.addNativeConfigurationTypes(natives)
}

// The worked example end to end: define natively, register beside the pinned
// entries, serve it, and prove it equal to the captured upstream fixture.
func TestNativeConfigurationTypeIsServedBesideThePinnedOnes(t *testing.T) {
	pinned, _ := loadWithNative(t)
	catalog, err := loadWithNative(t, stubNativeType())
	if err != nil {
		t.Fatalf("the native type was refused: %v", err)
	}
	entry, found := catalog.EntryByType("stub_example")
	if !found || entry.Section != "credentials" || !entry.HasTestConnection || entry.CheckConnectionFunc == nil {
		t.Fatalf("the native entry is wrong: found=%v %+v", found, entry)
	}
	if entry.UsesSDKValidation() {
		t.Error("a native type is validated by Main, not by the SDK validation boundary")
	}
	schema, ok := catalog.DataSchemaByType("stub_example")
	if !ok || schema["title"] != "StubExampleConfiguration" {
		t.Fatalf("the data schema is not served: %v", schema)
	}
	metadata, _ := schema["metadata"].(map[string]any)
	if metadata["label"] != "Stub example" || metadata["type"] != "stub_example" || metadata["section"] != "credentials" {
		t.Errorf("metadata = %v", metadata)
	}
	if len(catalog.PinnedEntries("credentials")) != len(pinned.PinnedEntries("credentials"))+1 {
		t.Error("the native type is listed in the credentials section exactly once")
	}
	// The pinned entries are untouched, byte for byte.
	for _, before := range pinned.PinnedEntries() {
		after, found := catalog.EntryByType(before.Type)
		if !found || string(after.ConfigSchema) != string(before.ConfigSchema) {
			t.Fatalf("the pinned entry %q changed", before.Type)
		}
	}

	fixture, err := sdkfixture.Parse([]byte(stubFixture))
	if err != nil {
		t.Fatal(err)
	}
	properties, _ := schema["properties"].(map[string]any)
	required := []string{}
	for _, name := range schema["required"].([]any) {
		required = append(required, name.(string))
	}
	if diffs := fixture.DiffConfiguration(properties, required); len(diffs) != 0 {
		t.Errorf("the native type drifted from its upstream fixture:\n%s", strings.Join(diffs, "\n"))
	}
}

func TestNativeConfigurationTypeIsCheckedAgainstItsFixture(t *testing.T) {
	fixture, err := sdkfixture.Parse([]byte(stubFixture))
	if err != nil {
		t.Fatal(err)
	}
	drifted := stubNativeType()
	drifted.Properties["api_key"] = map[string]any{"type": "string"} // lost its secret flag
	drifted.Properties["extra"] = map[string]any{"type": "string"}
	delete(drifted.Properties, "scopes")
	diffs := strings.Join(fixture.DiffConfiguration(drifted.Properties, []string{}), "\n")
	for _, want := range []string{
		`field "scopes" is in upstream and missing here`,
		`field "extra" is here and not in upstream`,
		`field "api_key": secret`,
		`required field "api_key" is in upstream and missing here`,
	} {
		if !strings.Contains(diffs, want) {
			t.Errorf("the drift %q went unreported:\n%s", want, diffs)
		}
	}
}

func TestNativeConfigurationTypeCannotDefineATypeTwice(t *testing.T) {
	duplicate := stubNativeType()
	duplicate.Type = "github" // defined by the pinned snapshot
	if _, err := loadWithNative(t, duplicate); err == nil {
		t.Error("a native type shadowing a pinned one was accepted")
	}
	if _, err := loadWithNative(t, stubNativeType(), stubNativeType()); err == nil {
		t.Error("a native type defined twice was accepted")
	}
	incomplete := stubNativeType()
	incomplete.Properties = nil
	if _, err := loadWithNative(t, incomplete); err == nil {
		t.Error("a native type without fields was accepted")
	}
	invalid := stubNativeType()
	invalid.Type = "Not Valid"
	if _, err := loadWithNative(t, invalid); err == nil {
		t.Error("a native type with an invalid id was accepted")
	}
}

// Every type in the registry must have a fixture beside it, and no type may be
// defined both natively and in the pinned snapshot. Empty today; the first
// native type (Dataverse, #2257) makes it bite.
func TestEveryNativeConfigurationTypeHasAFixtureAndOneDefinition(t *testing.T) {
	pinned, err := LoadCurrentAvailableCatalog([]byte(pinnedCurrentAvailableSnapshot))
	if err != nil {
		t.Fatal(err)
	}
	for _, native := range NativeConfigurationTypes() {
		if _, shadows := pinned.EntryByType(native.Type); shadows {
			t.Errorf("native type %q is also defined by the pinned snapshot", native.Type)
		}
		fixture, err := sdkfixture.Load(filepath.Join("testdata", "sdk-fixtures", native.Type+".json"))
		if err != nil {
			t.Errorf("native type %q has no usable upstream fixture: %v", native.Type, err)
			continue
		}
		if fixture.Kind != sdkfixture.KindConfiguration || fixture.Type != native.Type {
			t.Errorf("fixture of %q is a %s fixture of %q", native.Type, fixture.Kind, fixture.Type)
			continue
		}
		required := append([]string(nil), native.Required...)
		if diffs := fixture.DiffConfiguration(native.Properties, required); len(diffs) != 0 {
			t.Errorf("native type %q drifted from its upstream fixture:\n%s", native.Type, strings.Join(diffs, "\n"))
		}
	}
}

func TestNativeFixtureDirectoryHoldsOnlyFixturesOfNativeTypes(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "sdk-fixtures"))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	native := map[string]bool{}
	for _, definition := range NativeConfigurationTypes() {
		native[definition.Type] = true
	}
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".json")
		if !native[name] {
			t.Errorf("fixture %s names no native credential type", entry.Name())
		}
	}
}

// withNativeRegistry swaps the production (empty) registry for a test's, the
// way the first real native type will be present at boot.
func withNativeRegistry(t *testing.T, natives ...NativeConfigurationType) {
	t.Helper()
	previous := nativeConfigurationTypes
	nativeConfigurationTypes = natives
	t.Cleanup(func() { nativeConfigurationTypes = previous })
}

func bootNormalizer(t *testing.T) (CurrentConfigurationDataNormalizer, error) {
	t.Helper()
	catalog, err := LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatalf("the catalog with a native type did not load: %v", err)
	}
	return NewCurrentConfigurationDataNormalizer(catalog,
		&currentSDKExpanderStub{result: map[string]any{}}, &currentNormalizerChainValidator{})
}

// Boot with a real native type: Main constructs its normalizer chain over the
// real loader's catalog, and the default owner takes the type.
func TestNativeConfigurationTypeIsOwnedByTheDefaultNormalizerAtBoot(t *testing.T) {
	native := stubNativeType()
	native.Hidden = true
	withNativeRegistry(t, native)
	normalizer, err := bootNormalizer(t)
	if err != nil {
		t.Fatalf("Main would not boot with a native type: %v", err)
	}
	create := func(data map[string]any) (CurrentConfigurationNormalizationResult, error) {
		return normalizer.Normalize(context.Background(), CurrentConfigurationNormalizationRequest{
			Operation: CurrentConfigurationNormalizationCreate, ProjectID: 7, AuthorID: 13,
			Type: "stub_example", Data: data,
		})
	}
	result, err := create(map[string]any{"api_key": "k-1", "scopes": []any{"A"}, "junk": 1})
	if err != nil || !result.Complete {
		t.Fatalf("create = %#v, %v", result, err)
	}
	if _, kept := result.Data["junk"]; kept || result.Data["api_key"] != "k-1" {
		t.Errorf("an undeclared field was kept or a declared one lost: %v", result.Data)
	}
	for name, data := range map[string]map[string]any{
		"missing required": {"scopes": []any{}},
		"wrong type":       {"api_key": 5},
		"blank":            {"api_key": "  "},
		"array as string":  {"api_key": "k", "scopes": "A"},
	} {
		if _, err := create(data); err == nil {
			t.Errorf("%s: the create was accepted", name)
		}
	}
	update, err := normalizer.Normalize(context.Background(), CurrentConfigurationNormalizationRequest{
		Operation: CurrentConfigurationNormalizationUpdate, ProjectID: 7, AuthorID: 13,
		Type: "stub_example", Data: map[string]any{"scopes": []any{"B"}},
	})
	if err != nil || update.Data["scopes"] == nil {
		t.Errorf("update was not delegated: %#v, %v", update, err)
	}
}

// A type that declares its own normalizer must have it: the default steps aside
// and, with no other owner, boot fails closed (S1a adds the owner and the test
// of its own types).
func TestNativeConfigurationTypeWithOwnNormalizerNeedsAnOwner(t *testing.T) {
	native := stubNativeType()
	native.OwnNormalizer = true
	withNativeRegistry(t, native)
	if _, err := bootNormalizer(t); err == nil ||
		!strings.Contains(err.Error(), `"stub_example" has 0 create normalizer owners`) {
		t.Fatalf("boot err = %v, want the unowned type refused", err)
	}
}

func TestHiddenNativeConfigurationTypeIsRegisteredButMarkedHidden(t *testing.T) {
	visible, err := loadWithNative(t, stubNativeType())
	if err != nil {
		t.Fatal(err)
	}
	hidden := stubNativeType()
	hidden.Hidden = true
	catalog, err := loadWithNative(t, hidden)
	if err != nil {
		t.Fatal(err)
	}
	metadataOf := func(c *CurrentAvailableCatalog) map[string]any {
		schema, ok := c.DataSchemaByType("stub_example")
		if !ok {
			t.Fatal("type is not in the catalogue")
		}
		metadata, _ := schema["metadata"].(map[string]any)
		return metadata
	}
	if metadataOf(catalog)["hidden"] != true {
		t.Error("a hidden native type is not marked metadata.hidden")
	}
	if _, present := metadataOf(visible)["hidden"]; present {
		t.Error("a visible native type carries a hidden flag")
	}
}

func TestNativeConfigurationTypesAreServedAsValidJSON(t *testing.T) {
	catalog, err := loadWithNative(t, stubNativeType())
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := catalog.EntryByType("stub_example")
	var decoded map[string]any
	if err := json.Unmarshal(entry.ConfigSchema, &decoded); err != nil {
		t.Fatalf("the served schema is not JSON: %v", err)
	}
}
