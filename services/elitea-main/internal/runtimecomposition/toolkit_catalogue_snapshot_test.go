package runtimecomposition_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

func TestPinnedToolkitCatalogueSnapshotLoads(t *testing.T) {
	t.Parallel()

	catalogue, err := runtimecomposition.LoadPinnedCurrentToolkitCatalogueSnapshot()
	if err != nil {
		t.Fatalf("load pinned toolkit catalogue snapshot: %v", err)
	}
	if catalogue.EntryCount() != 52 {
		t.Errorf("entry count=%d, want 52", catalogue.EntryCount())
	}
	if catalogue.SDKRevision() == "" {
		t.Error("catalogue names no SDK revision")
	}
}

// The two pinned files are two projections of ONE registry read. A revision or
// a type set that differs between them means one of them was regenerated alone,
// and the served catalogue would then join a settings schema of one SDK to the
// argument schemas of another.
func TestToolkitCatalogueAgreesWithTheArgumentSchemaSnapshot(t *testing.T) {
	t.Parallel()

	catalogue, err := runtimecomposition.LoadPinnedCurrentToolkitCatalogueSnapshot()
	if err != nil {
		t.Fatalf("load pinned toolkit catalogue snapshot: %v", err)
	}
	snapshot, err := runtimecomposition.LoadPinnedCurrentToolkitSchemaSnapshot()
	if err != nil {
		t.Fatalf("load pinned toolkit schema snapshot: %v", err)
	}
	if catalogue.SDKRevision() != snapshot.SDKRevision() {
		t.Errorf("catalogue revision=%q, argument schema revision=%q",
			catalogue.SDKRevision(), snapshot.SDKRevision())
	}
	catalogueTypes := strings.Join(catalogue.ToolkitTypes(), ",")
	snapshotTypes := strings.Join(snapshot.ToolkitTypes(), ",")
	if catalogueTypes != snapshotTypes {
		t.Errorf("the two pinned projections hold different types:\n catalogue=%s\n snapshot =%s",
			catalogueTypes, snapshotTypes)
	}
}

// Every entry must carry a settings schema and a metadata block. A type with
// neither would be served as an empty create form.
func TestEveryCatalogueEntryCarriesSettingsAndMetadata(t *testing.T) {
	t.Parallel()

	catalogue, err := runtimecomposition.LoadPinnedCurrentToolkitCatalogueSnapshot()
	if err != nil {
		t.Fatalf("load pinned toolkit catalogue snapshot: %v", err)
	}
	for _, toolkitType := range catalogue.ToolkitTypes() {
		settings, metadata, found, err := catalogue.ToolkitCatalogueEntry(toolkitType)
		if err != nil || !found {
			t.Fatalf("%s: found=%v err=%v", toolkitType, found, err)
		}
		properties, ok := settings["properties"].(map[string]any)
		if !ok || len(properties) == 0 {
			t.Errorf("%s carries no settings properties", toolkitType)
		}
		if settings["type"] != "object" {
			t.Errorf("%s settings type=%#v", toolkitType, settings["type"])
		}
		// The reference deployment injects these two fields before it serves
		// the registry. Both are present for every type, mcp_config included.
		for _, key := range []string{"check_connection_supported", "has_function_validators"} {
			if _, present := metadata[key]; !present {
				t.Errorf("%s metadata carries no %s", toolkitType, key)
			}
		}
		// The argument schemas live in the other file. Carrying them here too
		// would double a 596 KB payload.
		if selected, ok := properties["selected_tools"].(map[string]any); ok {
			if _, present := selected["args_schemas"]; present {
				t.Errorf("%s duplicates its argument schemas into the catalogue", toolkitType)
			}
		}
	}
}

// The import key is the join with the worker capability answer, and it is not
// the type.
func TestCatalogueImportKeysNameTheSDKRegistry(t *testing.T) {
	t.Parallel()

	catalogue, err := runtimecomposition.LoadPinnedCurrentToolkitCatalogueSnapshot()
	if err != nil {
		t.Fatalf("load pinned toolkit catalogue snapshot: %v", err)
	}
	if key, found := catalogue.ToolkitImportKey("kubernetes"); !found || key != "k8s" {
		t.Errorf("kubernetes import key=%q found=%v, want k8s", key, found)
	}
	if key, found := catalogue.ToolkitImportKey("github"); !found || key != "github" {
		t.Errorf("github import key=%q found=%v", key, found)
	}
	// A runtime toolkit is imported with the SDK itself, so no import of it can
	// fail and it declares no key.
	if key, found := catalogue.ToolkitImportKey("artifact"); !found || key != "" {
		t.Errorf("artifact import key=%q found=%v, want an empty key", key, found)
	}
	if _, found := catalogue.ToolkitImportKey("custom"); found {
		t.Error("custom is not an SDK toolkit and the catalogue claims it")
	}
}

// Returned trees are detached. The served catalogue is written to per request.
func TestCatalogueEntriesAreDetachedCopies(t *testing.T) {
	t.Parallel()

	catalogue, err := runtimecomposition.LoadPinnedCurrentToolkitCatalogueSnapshot()
	if err != nil {
		t.Fatalf("load pinned toolkit catalogue snapshot: %v", err)
	}
	settings, metadata, _, err := catalogue.ToolkitCatalogueEntry("github")
	if err != nil {
		t.Fatalf("read github: %v", err)
	}
	settings["properties"] = map[string]any{}
	metadata["label"] = "tampered"

	settingsAgain, metadataAgain, _, err := catalogue.ToolkitCatalogueEntry("github")
	if err != nil {
		t.Fatalf("re-read github: %v", err)
	}
	if properties, _ := settingsAgain["properties"].(map[string]any); len(properties) == 0 {
		t.Error("a caller emptied the shared settings tree")
	}
	if metadataAgain["label"] != "GitHub" {
		t.Errorf("label=%#v after a caller wrote to a previous copy", metadataAgain["label"])
	}
}

func TestLoadCurrentToolkitCatalogueSnapshotRefusesBadDocuments(t *testing.T) {
	t.Parallel()

	valid := `{"schema_version":"elitea.current-toolkit-catalogue-snapshot.v1",` +
		`"sdk_revision":"abc","entries":[{"type":"a","import_key":null,` +
		`"metadata":{"label":"A"},"settings":{"type":"object"}}]}`
	if _, err := runtimecomposition.LoadCurrentToolkitCatalogueSnapshot([]byte(valid)); err != nil {
		t.Fatalf("a valid document was refused: %v", err)
	}

	for name, document := range map[string]string{
		"empty":            "",
		"not json":         "{",
		"trailing content": valid + `{"more":true}`,
		"unknown field": `{"schema_version":"elitea.current-toolkit-catalogue-snapshot.v1",` +
			`"sdk_revision":"abc","entries":[],"extra":1}`,
		"wrong version": `{"schema_version":"elitea.other.v1","sdk_revision":"abc",` +
			`"entries":[{"type":"a","import_key":null,"metadata":{},"settings":{}}]}`,
		"no entries": `{"schema_version":"elitea.current-toolkit-catalogue-snapshot.v1",` +
			`"sdk_revision":"abc","entries":[]}`,
		"unsorted entries": `{"schema_version":"elitea.current-toolkit-catalogue-snapshot.v1",` +
			`"sdk_revision":"abc","entries":[` +
			`{"type":"b","import_key":null,"metadata":{},"settings":{}},` +
			`{"type":"a","import_key":null,"metadata":{},"settings":{}}]}`,
		"missing metadata": `{"schema_version":"elitea.current-toolkit-catalogue-snapshot.v1",` +
			`"sdk_revision":"abc","entries":[{"type":"a","import_key":null,"settings":{}}]}`,
		"missing settings": `{"schema_version":"elitea.current-toolkit-catalogue-snapshot.v1",` +
			`"sdk_revision":"abc","entries":[{"type":"a","import_key":null,"metadata":{}}]}`,
		"invalid import key": `{"schema_version":"elitea.current-toolkit-catalogue-snapshot.v1",` +
			`"sdk_revision":"abc","entries":[{"type":"a","import_key":"a\nb",` +
			`"metadata":{},"settings":{}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runtimecomposition.LoadCurrentToolkitCatalogueSnapshot([]byte(document))
			if !errors.Is(err, runtimecomposition.ErrCurrentToolkitCatalogueSnapshotInvalid) {
				t.Errorf("err=%v, want ErrCurrentToolkitCatalogueSnapshotInvalid", err)
			}
		})
	}
}

func TestNilCatalogueSnapshotAnswersSafely(t *testing.T) {
	t.Parallel()

	var catalogue *runtimecomposition.CurrentToolkitCatalogueSnapshot
	if catalogue.EntryCount() != 0 || catalogue.SDKRevision() != "" ||
		catalogue.ToolkitTypes() != nil {
		t.Error("a nil catalogue answered with content")
	}
	if _, found := catalogue.ToolkitImportKey("github"); found {
		t.Error("a nil catalogue claims an import key")
	}
	if _, _, _, err := catalogue.ToolkitCatalogueEntry("github"); err == nil {
		t.Error("a nil catalogue returned an entry without an error")
	}
	loaded, err := runtimecomposition.LoadPinnedCurrentToolkitCatalogueSnapshot()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, _, _, err := loaded.ToolkitCatalogueEntry("bad\nname"); err == nil {
		t.Error("an invalid type name returned an entry without an error")
	}
	if _, _, found, err := loaded.ToolkitCatalogueEntry("absent_type"); found || err != nil {
		t.Errorf("an absent type returned found=%v err=%v", found, err)
	}
}
