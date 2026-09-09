package runtimecomposition

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

type currentActorVisibleSchemaSourceStub struct {
	schema    configurationapp.CurrentToolkitSchema
	found     bool
	err       error
	calls     int
	projectID int32
	userID    int32
	typeName  string
}

func (s *currentActorVisibleSchemaSourceStub) FindCurrentActorVisibleToolkitSchema(
	_ context.Context,
	projectID int32,
	userID int32,
	typeName string,
) (configurationapp.CurrentToolkitSchema, bool, error) {
	s.calls++
	s.projectID = projectID
	s.userID = userID
	s.typeName = typeName
	return s.schema, s.found, s.err
}

func TestPinnedCurrentToolkitSchemaSnapshotMatchesAdmittedSDKProjection(t *testing.T) {
	snapshot, err := LoadPinnedCurrentToolkitSchemaSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SDKRevision() != "b5113a129329b85d23c2d5c2bf55f18e307414ec" || snapshot.EntryCount() != 53 {
		t.Fatalf("snapshot revision=%q entries=%d", snapshot.SDKRevision(), snapshot.EntryCount())
	}

	catalog, err := NewCurrentCompositeToolkitSchemaCatalog(
		snapshot,
		UnavailableCurrentActorVisibleToolkitSchemas{},
	)
	if err != nil {
		t.Fatal(err)
	}
	schema, found, err := catalog.FindEffectiveToolkitSchema(context.Background(), 7, 11, "github")
	if err != nil || !found {
		t.Fatalf("github schema found=%t err=%v", found, err)
	}
	credential, ok := schema.Properties["github_configuration"].(map[string]any)
	if !ok {
		t.Fatalf("github credential annotation=%#v", schema.Properties["github_configuration"])
	}
	types, ok := credential["configuration_types"].([]any)
	if !ok || len(types) != 1 || types[0] != "github" {
		t.Fatalf("github configuration types=%#v", credential["configuration_types"])
	}
	// Callers cannot mutate the immutable built-in snapshot through a result.
	types[0] = "caller-corruption"
	again, found, err := catalog.FindEffectiveToolkitSchema(context.Background(), 7, 11, "github")
	if err != nil || !found {
		t.Fatalf("second github lookup found=%t err=%v", found, err)
	}
	againTypes := again.Properties["github_configuration"].(map[string]any)["configuration_types"].([]any)
	if againTypes[0] != "github" {
		t.Fatalf("snapshot was mutated: %#v", againTypes)
	}

	aha, found, err := catalog.FindEffectiveToolkitSchema(context.Background(), 7, 11, "aha")
	if err != nil || !found {
		t.Fatalf("aha schema found=%t err=%v", found, err)
	}
	ahaCredential, ok := aha.Properties["aha_configuration"].(map[string]any)
	if !ok {
		t.Fatalf("aha credential annotation=%#v", aha.Properties["aha_configuration"])
	}
	ahaTypes, ok := ahaCredential["configuration_types"].([]any)
	if !ok || len(ahaTypes) != 1 || ahaTypes[0] != "aha" {
		t.Fatalf("aha configuration types=%#v", ahaCredential["configuration_types"])
	}
}

// The snapshot carries the per-tool ARGUMENT schemas as well as the settings
// annotations. They are a different resource: the annotations drive settings
// expansion and toolkit naming inside this process, while the argument schemas
// are what the toolkit type catalogue serves to the web client so it can render
// a form for a tool. Until the sync script stopped projecting them away, the
// create-index form had nothing to render and its Index button was permanently
// disabled.
func TestPinnedCurrentToolkitSchemaSnapshotCarriesRealToolArgumentSchemas(t *testing.T) {
	snapshot, err := LoadPinnedCurrentToolkitSchemaSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	argsSchemas, found, err := snapshot.ToolkitArgumentSchemas("artifact")
	if err != nil || !found {
		t.Fatalf("artifact argument schemas found=%t err=%v", found, err)
	}
	indexData, ok := argsSchemas["index_data"]
	if !ok {
		t.Fatalf("artifact exposes no index_data argument schema; tools=%v", sortedKeys(argsSchemas))
	}
	if indexData["type"] != "object" {
		t.Errorf("index_data type=%#v, want %q", indexData["type"], "object")
	}
	required, ok := indexData["required"].([]any)
	if !ok || len(required) != 1 || required[0] != "index_name" {
		t.Errorf("index_data required=%#v, want [index_name]", indexData["required"])
	}
	properties, ok := indexData["properties"].(map[string]any)
	if !ok {
		t.Fatalf("index_data has no properties object: %#v", indexData)
	}
	// A placeholder {"type":"object"} has none of these. Each is a control the
	// create-index form renders.
	for _, argument := range []string{
		"index_name", "clean_index", "folder", "include_extensions",
		"skip_extensions", "progress_step", "chunking_config",
	} {
		if _, ok := properties[argument].(map[string]any); !ok {
			t.Errorf("index_data.properties is missing %q: have %v", argument, sortedKeys(properties))
		}
	}
	indexName, _ := properties["index_name"].(map[string]any)
	if indexName["type"] != "string" || indexName["maxLength"] != json.Number("32") {
		t.Errorf("index_name=%#v, want a string with maxLength 32", indexName)
	}

	// Detached copies: a caller that edits the served schema must not corrupt
	// the process-wide snapshot for every later request.
	properties["index_name"] = "caller-corruption"
	again, _, err := snapshot.ToolkitArgumentSchemas("artifact")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again["index_data"]["properties"].(map[string]any)["index_name"].(map[string]any); !ok {
		t.Error("the snapshot was mutated through a returned argument schema")
	}
}

// mcp, mcp_config and openapi discover their tools at runtime from a remote
// server or an OpenAPI specification, so the SDK declares no argument models for
// them. That is a legitimate empty result, not a missing type and not an error;
// treating it as either would break every screen that lists toolkit types.
func TestPinnedCurrentToolkitSchemaSnapshotAllowsRuntimeDiscoveredToolkitsToHaveNoArgumentSchemas(t *testing.T) {
	snapshot, err := LoadPinnedCurrentToolkitSchemaSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, toolkitType := range []string{"mcp", "mcp_config", "openapi"} {
		argsSchemas, found, err := snapshot.ToolkitArgumentSchemas(toolkitType)
		if err != nil || !found {
			t.Fatalf("%s argument schemas found=%t err=%v", toolkitType, found, err)
		}
		if argsSchemas == nil || len(argsSchemas) != 0 {
			t.Errorf("%s argument schemas=%#v, want an empty non-nil map", toolkitType, argsSchemas)
		}
	}
	if _, found, err := snapshot.ToolkitArgumentSchemas("not_a_built_in_toolkit"); found || err != nil {
		t.Errorf("unknown type found=%t err=%v, want (false, nil)", found, err)
	}
	if _, _, err := snapshot.ToolkitArgumentSchemas("bad\nname"); !errors.Is(err, ErrCurrentToolkitSchemaSnapshotInvalid) {
		t.Errorf("invalid type name error=%v", err)
	}
}

// An argument schema is carried as a generic decoded JSON tree precisely so
// that keywords this code knows nothing about survive. $defs/$ref is the case
// that makes it load-bearing: narrowing the representation to the keywords we
// recognise would drop $defs and leave every $ref pointing at nothing, which
// fails at the client as an unrenderable form rather than as an error here.
func TestCurrentToolkitSchemaSnapshotCarriesNestedSchemaReferencesVerbatim(t *testing.T) {
	const document = `{
		"schema_version":"elitea.current-toolkit-schema-snapshot.v1",
		"sdk_revision":"sdk-revision",
		"entries":[{
			"type":"referencing",
			"properties":{},
			"args_schemas":{"index_data":{
				"$defs":{"Chunk":{"properties":{"max_tokens":{"type":"integer","default":512}},"type":"object"}},
				"properties":{"chunk":{"$ref":"#/$defs/Chunk"},"chunks":{"items":{"$ref":"#/$defs/Chunk"},"type":"array"}},
				"required":["chunk"],
				"type":"object"
			}},
			"naming":{"field":null,"max_length":0}
		}]
	}`
	snapshot := loadCurrentToolkitSchemaSnapshotForTest(t, document)
	argsSchemas, found, err := snapshot.ToolkitArgumentSchemas("referencing")
	if err != nil || !found {
		t.Fatalf("found=%t err=%v", found, err)
	}

	// Compare after a JSON round trip, because serving the schema over HTTP is
	// exactly a JSON round trip: anything the Go representation cannot express
	// shows up here as a difference.
	var want any
	if err := json.Unmarshal([]byte(`{
		"$defs":{"Chunk":{"properties":{"max_tokens":{"type":"integer","default":512}},"type":"object"}},
		"properties":{"chunk":{"$ref":"#/$defs/Chunk"},"chunks":{"items":{"$ref":"#/$defs/Chunk"},"type":"array"}},
		"required":["chunk"],
		"type":"object"
	}`), &want); err != nil {
		t.Fatal(err)
	}
	served, err := json.Marshal(argsSchemas["index_data"])
	if err != nil {
		t.Fatal(err)
	}
	var got any
	if err := json.Unmarshal(served, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-tripped argument schema:\n got %s\nwant the input verbatim", served)
	}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestCurrentToolkitSchemaCatalogUsesBuiltInBeforeActorVisibleOverlay(t *testing.T) {
	snapshot := loadCurrentToolkitSchemaSnapshotForTest(t, `{
		"schema_version":"elitea.current-toolkit-schema-snapshot.v1",
		"sdk_revision":"sdk-revision",
		"entries":[{
			"type":"same_type",
			"properties":{"credential":{"configuration_types":["built_in"]}},
			"args_schemas":{},
			"naming":{"field":null,"max_length":0}
		}]
	}`)
	dynamic := &currentActorVisibleSchemaSourceStub{
		found: true,
		schema: configurationapp.CurrentToolkitSchema{Properties: map[string]any{
			"credential": map[string]any{"configuration_types": []any{"dynamic"}},
		}},
	}
	catalog, err := NewCurrentCompositeToolkitSchemaCatalog(snapshot, dynamic)
	if err != nil {
		t.Fatal(err)
	}

	schema, found, err := catalog.FindEffectiveToolkitSchema(context.Background(), 7, 11, "same_type")
	if err != nil || !found || dynamic.calls != 0 {
		t.Fatalf("found=%t err=%v dynamic calls=%d", found, err, dynamic.calls)
	}
	types := schema.Properties["credential"].(map[string]any)["configuration_types"].([]any)
	if len(types) != 1 || types[0] != "built_in" {
		t.Fatalf("schema precedence=%#v", schema)
	}
}

func TestCurrentToolkitSchemaCatalogDelegatesMissingTypeWithActorScope(t *testing.T) {
	snapshot := loadMinimalCurrentToolkitSchemaSnapshot(t)
	dynamicProperties := map[string]any{
		"toolkit_configuration_token": map[string]any{"secret": true},
	}
	dynamic := &currentActorVisibleSchemaSourceStub{
		found:  true,
		schema: configurationapp.CurrentToolkitSchema{Properties: dynamicProperties},
	}
	catalog, err := NewCurrentCompositeToolkitSchemaCatalog(snapshot, dynamic)
	if err != nil {
		t.Fatal(err)
	}

	schema, found, err := catalog.FindEffectiveToolkitSchema(context.Background(), 7, 11, "provider_dynamic")
	if err != nil || !found {
		t.Fatalf("dynamic schema found=%t err=%v", found, err)
	}
	if dynamic.calls != 1 || dynamic.projectID != 7 || dynamic.userID != 11 || dynamic.typeName != "provider_dynamic" {
		t.Fatalf("dynamic scope calls=%d project=%d user=%d type=%q", dynamic.calls, dynamic.projectID, dynamic.userID, dynamic.typeName)
	}
	field := schema.Properties["toolkit_configuration_token"].(map[string]any)
	field["secret"] = false
	if dynamicProperties["toolkit_configuration_token"].(map[string]any)["secret"] != true {
		t.Fatal("dynamic source map was aliased to the caller")
	}
}

func TestCurrentToolkitSchemaCatalogFailsClosedWithoutDynamicOwnership(t *testing.T) {
	catalog, err := NewCurrentCompositeToolkitSchemaCatalog(
		loadMinimalCurrentToolkitSchemaSnapshot(t),
		UnavailableCurrentActorVisibleToolkitSchemas{},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, found, err := catalog.FindEffectiveToolkitSchema(context.Background(), 7, 11, "provider_dynamic")
	if found || !errors.Is(err, ErrCurrentDynamicToolkitSchemasUnavailable) {
		t.Fatalf("found=%t err=%v", found, err)
	}
}

func TestCurrentToolkitSchemaCatalogRejectsInvalidRequestsAndDynamicSchemas(t *testing.T) {
	snapshot := loadMinimalCurrentToolkitSchemaSnapshot(t)
	dynamic := &currentActorVisibleSchemaSourceStub{
		found: true,
		schema: configurationapp.CurrentToolkitSchema{Properties: map[string]any{
			"invalid": make(chan int),
		}},
	}
	catalog, err := NewCurrentCompositeToolkitSchemaCatalog(snapshot, dynamic)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.FindEffectiveToolkitSchema(context.Background(), 7, 11, "dynamic"); !errors.Is(err, ErrCurrentDynamicToolkitSchemaInvalid) {
		t.Fatalf("invalid dynamic schema error=%v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	before := dynamic.calls
	if _, _, err := catalog.FindEffectiveToolkitSchema(canceled, 7, 11, "dynamic"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lookup error=%v", err)
	}
	if dynamic.calls != before {
		t.Fatal("canceled lookup reached dynamic source")
	}
	for _, request := range []struct {
		ctx       context.Context
		projectID int32
		userID    int32
		typeName  string
	}{
		{ctx: nil, projectID: 7, userID: 11, typeName: "dynamic"},
		{ctx: context.Background(), projectID: 0, userID: 11, typeName: "dynamic"},
		{ctx: context.Background(), projectID: 7, userID: 0, typeName: "dynamic"},
		{ctx: context.Background(), projectID: 7, userID: 11, typeName: "bad\nname"},
	} {
		if _, _, err := catalog.FindEffectiveToolkitSchema(request.ctx, request.projectID, request.userID, request.typeName); !errors.Is(err, ErrCurrentToolkitSchemaLookupInvalid) {
			t.Fatalf("request=%#v error=%v", request, err)
		}
	}
}

func TestCurrentToolkitSchemaSnapshotRejectsDriftedOrUnboundedDocuments(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "empty", data: ``},
		{name: "wrong version", data: `{"schema_version":"v2","sdk_revision":"r","entries":[]}`},
		{name: "unknown field", data: `{"schema_version":"elitea.current-toolkit-schema-snapshot.v1","sdk_revision":"r","entries":[{"type":"a","properties":{},"args_schemas":{},"naming":{"field":null,"max_length":0}}],"extra":true}`},
		{name: "trailing value", data: `{"schema_version":"elitea.current-toolkit-schema-snapshot.v1","sdk_revision":"r","entries":[{"type":"a","properties":{},"args_schemas":{},"naming":{"field":null,"max_length":0}}]} {}`},
		{name: "duplicate type", data: `{"schema_version":"elitea.current-toolkit-schema-snapshot.v1","sdk_revision":"r","entries":[{"type":"a","properties":{},"args_schemas":{},"naming":{"field":null,"max_length":0}},{"type":"a","properties":{},"args_schemas":{},"naming":{"field":null,"max_length":0}}]}`},
		{name: "unsorted", data: `{"schema_version":"elitea.current-toolkit-schema-snapshot.v1","sdk_revision":"r","entries":[{"type":"b","properties":{},"args_schemas":{},"naming":{"field":null,"max_length":0}},{"type":"a","properties":{},"args_schemas":{},"naming":{"field":null,"max_length":0}}]}`},
		{name: "missing naming", data: `{"schema_version":"elitea.current-toolkit-schema-snapshot.v1","sdk_revision":"r","entries":[{"type":"a","properties":{},"args_schemas":{}}]}`},
		{name: "missing max length", data: `{"schema_version":"elitea.current-toolkit-schema-snapshot.v1","sdk_revision":"r","entries":[{"type":"a","properties":{},"args_schemas":{},"naming":{"field":null}}]}`},
		{name: "name annotation mismatch", data: `{"schema_version":"elitea.current-toolkit-schema-snapshot.v1","sdk_revision":"r","entries":[{"type":"a","properties":{"url":{"toolkit_name":false}},"args_schemas":{},"naming":{"field":"url","max_length":0}}]}`},
		{name: "negative max length", data: `{"schema_version":"elitea.current-toolkit-schema-snapshot.v1","sdk_revision":"r","entries":[{"type":"a","properties":{},"args_schemas":{},"naming":{"field":null,"max_length":-1}}]}`},
		// args_schemas is mandatory, not optional-with-a-nil-default: a snapshot
		// regenerated by a sync script that still projects the field away would
		// otherwise load and quietly serve no argument schemas at all, which is
		// exactly the defect this field exists to end.
		{name: "missing args schemas", data: `{"schema_version":"elitea.current-toolkit-schema-snapshot.v1","sdk_revision":"r","entries":[{"type":"a","properties":{},"naming":{"field":null,"max_length":0}}]}`},
		{name: "null args schemas", data: `{"schema_version":"elitea.current-toolkit-schema-snapshot.v1","sdk_revision":"r","entries":[{"type":"a","properties":{},"args_schemas":null,"naming":{"field":null,"max_length":0}}]}`},
		{name: "non-object args schema", data: `{"schema_version":"elitea.current-toolkit-schema-snapshot.v1","sdk_revision":"r","entries":[{"type":"a","properties":{},"args_schemas":{"tool":"not-a-schema"},"naming":{"field":null,"max_length":0}}]}`},
		{name: "invalid tool name", data: `{"schema_version":"elitea.current-toolkit-schema-snapshot.v1","sdk_revision":"r","entries":[{"type":"a","properties":{},"args_schemas":{"bad\nname":{}},"naming":{"field":null,"max_length":0}}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := LoadCurrentToolkitSchemaSnapshot([]byte(test.data)); !errors.Is(err, ErrCurrentToolkitSchemaSnapshotInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	if _, err := LoadCurrentToolkitSchemaSnapshot([]byte(strings.Repeat("x", maxCurrentToolkitSchemaSnapshotBytes+1))); !errors.Is(err, ErrCurrentToolkitSchemaSnapshotInvalid) {
		t.Fatalf("oversized snapshot error=%v", err)
	}
}

func TestCurrentBuiltInToolkitNameDeriverMatchesCurrentSanitizationAndFallback(t *testing.T) {
	snapshot := loadCurrentToolkitSchemaSnapshotForTest(t, `{
		"schema_version":"elitea.current-toolkit-schema-snapshot.v1",
		"sdk_revision":"sdk-revision",
		"entries":[{
			"type":"named",
			"properties":{"url":{"toolkit_name":true,"max_toolkit_length":5}},
			"args_schemas":{},
			"naming":{"field":"url","max_length":5}
		}]
	}`)
	deriver, err := NewCurrentBuiltInToolkitNameDeriver(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	stored := "stored fallback.v2"

	name, err := deriver.DeriveCurrentToolkitName(context.Background(), CurrentToolkitNameInput{
		ProjectID: 7, UserID: 11, ToolkitType: "named", StoredName: &stored,
		Settings: map[string]any{"url": "https://my.host/x"},
	})
	if err != nil || name != "https" {
		t.Fatalf("derived name=%q err=%v", name, err)
	}
	name, err = deriver.DeriveCurrentToolkitName(context.Background(), CurrentToolkitNameInput{
		ProjectID: 7, UserID: 11, ToolkitType: "named", StoredName: &stored, Settings: map[string]any{},
	})
	if err != nil || name != "store" {
		t.Fatalf("truncated fallback name=%q err=%v", name, err)
	}
	name, err = deriver.DeriveCurrentToolkitName(context.Background(), CurrentToolkitNameInput{
		ProjectID: 7, UserID: 11, ToolkitType: "dynamic_only", StoredName: &stored, Settings: map[string]any{},
	})
	if err != nil || name != "storedfallback_v2" {
		t.Fatalf("dynamic-only fallback name=%q err=%v", name, err)
	}
	name, err = deriver.DeriveCurrentToolkitName(context.Background(), CurrentToolkitNameInput{
		ProjectID: 7, UserID: 11, ToolkitType: "named", StoredName: &stored,
		Settings: map[string]any{"url": true},
	})
	if err != nil || name != "True" {
		t.Fatalf("Python boolean rendering name=%q err=%v", name, err)
	}
	name, err = deriver.DeriveCurrentToolkitName(context.Background(), CurrentToolkitNameInput{
		ProjectID: 7, UserID: 11, ToolkitType: "named", StoredName: &stored,
		Settings: map[string]any{"url": ""},
	})
	if err != nil || name != "" {
		t.Fatalf("empty explicit name=%q err=%v", name, err)
	}
}

func TestCurrentBuiltInToolkitNameDeriverFailsClosedForInvalidRows(t *testing.T) {
	deriver, err := NewCurrentBuiltInToolkitNameDeriver(loadMinimalCurrentToolkitSchemaSnapshot(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deriver.DeriveCurrentToolkitName(context.Background(), CurrentToolkitNameInput{
		ProjectID: 7, UserID: 11, ToolkitType: "built_in", Settings: map[string]any{
			"name": map[string]any{"corrupt": true},
		},
	}); !errors.Is(err, ErrCurrentToolkitNameInputInvalid) {
		t.Fatalf("compound name error=%v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := deriver.DeriveCurrentToolkitName(canceled, CurrentToolkitNameInput{
		ProjectID: 7, UserID: 11, ToolkitType: "built_in", Settings: map[string]any{},
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled name derivation error=%v", err)
	}
	if _, err := NewCurrentBuiltInToolkitNameDeriver(nil); err == nil {
		t.Fatal("nil snapshot was accepted")
	}
	if _, err := NewCurrentCompositeToolkitSchemaCatalog(nil, UnavailableCurrentActorVisibleToolkitSchemas{}); err == nil {
		t.Fatal("nil built-in catalog was accepted")
	}
}

func loadMinimalCurrentToolkitSchemaSnapshot(t *testing.T) *CurrentToolkitSchemaSnapshot {
	t.Helper()
	return loadCurrentToolkitSchemaSnapshotForTest(t, `{
		"schema_version":"elitea.current-toolkit-schema-snapshot.v1",
		"sdk_revision":"sdk-revision",
		"entries":[{
			"type":"built_in",
			"properties":{"name":{"toolkit_name":true}},
			"args_schemas":{},
			"naming":{"field":"name","max_length":0}
		}]
	}`)
}

func loadCurrentToolkitSchemaSnapshotForTest(t *testing.T, data string) *CurrentToolkitSchemaSnapshot {
	t.Helper()
	snapshot, err := LoadCurrentToolkitSchemaSnapshot([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestCurrentToolkitSchemaSnapshotRetainsExactJSONNumbers(t *testing.T) {
	snapshot := loadCurrentToolkitSchemaSnapshotForTest(t, `{
		"schema_version":"elitea.current-toolkit-schema-snapshot.v1",
		"sdk_revision":"sdk-revision",
		"entries":[{
			"type":"numeric",
			"properties":{"field":{"custom_limit":9007199254740993}},
			"args_schemas":{},
			"naming":{"field":null,"max_length":0}
		}]
	}`)
	catalog, err := NewCurrentCompositeToolkitSchemaCatalog(snapshot, UnavailableCurrentActorVisibleToolkitSchemas{})
	if err != nil {
		t.Fatal(err)
	}
	schema, found, err := catalog.FindEffectiveToolkitSchema(context.Background(), 7, 11, "numeric")
	if err != nil || !found {
		t.Fatalf("numeric schema found=%t err=%v", found, err)
	}
	limit := schema.Properties["field"].(map[string]any)["custom_limit"]
	if limit != json.Number("9007199254740993") {
		t.Fatalf("numeric schema value=%#v", limit)
	}
}
