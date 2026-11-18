package runtimecomposition

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/rustonly"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/sdkfixture"
)

// The contract tests of the native authoring path (D1 of epic #2254, #2255;
// docs/contributing/adding-types.md). A toolkit type the SDK gained after the
// snapshot pin lives in rustonly's new_types, owned by its Rust family. Its
// fixture under testdata/sdk-fixtures is the SDK's own schema, captured once.

type nativeToolkitEntry struct {
	Settings    map[string]any            `json:"settings"`
	Properties  map[string]any            `json:"properties"`
	ArgsSchemas map[string]map[string]any `json:"args_schemas"`
}

func rustOnlyNewTypes(t *testing.T) map[string]nativeToolkitEntry {
	t.Helper()
	var document struct {
		NewTypes map[string]nativeToolkitEntry `json:"new_types"`
	}
	if err := json.Unmarshal(rustonly.JSON, &document); err != nil {
		t.Fatal(err)
	}
	return document.NewTypes
}

func sdkFixtureFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "sdk-fixtures", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no SDK fixtures found: %v", err)
	}
	return files
}

// A native toolkit type serves exactly what the SDK declares for it: same
// tool names, same argument schemas, same settings fields.
func TestNativeToolkitTypesMatchTheirCapturedUpstreamFixtures(t *testing.T) {
	served := rustOnlyNewTypes(t)
	for _, file := range sdkFixtureFiles(t) {
		fixture, err := sdkfixture.Load(file)
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		if filepath.Base(file) != fixture.Type+".json" {
			t.Errorf("%s holds the fixture of %q", file, fixture.Type)
		}
		if fixture.Kind != sdkfixture.KindToolkit {
			continue
		}
		entry, ok := served[fixture.Type]
		if !ok {
			t.Errorf("fixture %s has no new_types entry in rust_only_toolkit_tools.json", file)
			continue
		}
		if diffs := fixture.DiffToolkit(entry.Settings, entry.ArgsSchemas); len(diffs) != 0 {
			t.Errorf("%s drifted from %s@%s:\n%s", fixture.Type, fixture.Upstream.Repo,
				fixture.Upstream.Commit[:8], strings.Join(diffs, "\n"))
		}
	}
}

// The first worked example: the tavily toolkit type is in the served catalogue
// and its credential field resolves to a credentials-section reference
// although the SDK configuration catalogue has no tavily entry.
func TestTavilyToolkitTypeIsCataloguedThroughTheNativePath(t *testing.T) {
	catalogue, err := LoadPinnedCurrentToolkitCatalogueSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := catalogue.entries["tavily"]; !ok {
		t.Fatal("the tavily toolkit type is not in the served catalogue")
	}
	snapshot, err := LoadPinnedCurrentToolkitSchemaSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	configurations, err := LoadPinnedCurrentSDKConfigurationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if _, inSDKCatalogue := configurations.Binding("tavily"); inSDKCatalogue {
		t.Fatal("tavily is in the SDK configuration catalogue: the fallback is not exercised")
	}
	definitions, err := NewCurrentToolkitSettingsDefinitionCatalog(snapshot, configurations)
	if err != nil {
		t.Fatal(err)
	}
	defs, properties, found, err := definitions.ToolkitSettingsDefinitions("tavily")
	if err != nil || !found {
		t.Fatalf("tavily settings definitions: found=%v err=%v", found, err)
	}
	reference, _ := properties["tavily_configuration"].(map[string]any)
	if reference["$ref"] != "#/$defs/tavily" {
		t.Errorf("tavily_configuration = %v", reference)
	}
	definition, _ := defs["tavily"].(map[string]any)
	metadata, _ := definition["metadata"].(map[string]any)
	if metadata["section"] != "credentials" || metadata["type"] != "tavily" {
		t.Errorf("tavily definition = %v", definition)
	}
}

// A type resolved through the native fallback must exist in the available
// catalogue; an invented one stays a hard error.
func TestNativeConfigurationSectionFallbackKnowsOnlyRealTypes(t *testing.T) {
	if section, ok := nativeConfigurationSection("tavily"); !ok || section != "credentials" {
		t.Errorf("tavily = %q, %v", section, ok)
	}
	if _, ok := nativeConfigurationSection("no_such_credential_type"); ok {
		t.Error("an unknown credential type resolved")
	}
}

// One type, one definition: a natively defined credential type must not also
// be in the SDK configuration catalogue (the available snapshot is checked by
// the loader itself).
func TestNativeConfigurationTypesAreNotInTheSDKCatalogue(t *testing.T) {
	configurations, err := LoadPinnedCurrentSDKConfigurationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, native := range configurationapp.NativeConfigurationTypes() {
		if _, shadows := configurations.Binding(native.Type); shadows {
			t.Errorf("credential type %q is defined natively and in the SDK catalogue", native.Type)
		}
	}
}

func TestSDKFixtureDirectoryHoldsOnlyJSONFixtures(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "sdk-fixtures"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			t.Errorf("%s is not a fixture", entry.Name())
		}
	}
}
