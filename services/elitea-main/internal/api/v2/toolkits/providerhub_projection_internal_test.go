package toolkits

// The provider-hub projection, against pylon's own rules.
//
// Every expected value below was read out of
// `legacy/plugins/elitea_core/methods/provider_hub_schemas.py`, not invented:
// the `toolkit_configuration_` prefix, the type mapping table, the
// `json_schema_extra` merge order, the `configuration` flip and its three
// reference-picker exceptions, and `prettify_title`'s abbreviation set. A port
// that got one of these wrong would serve a form the provider worker cannot
// read back.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhub"
)

type stubManifests struct {
	manifests []providerhub.AdmittedManifest
	err       error
}

func (s stubManifests) AdmittedManifests(context.Context) ([]providerhub.AdmittedManifest, error) {
	return s.manifests, s.err
}

func manifestOf(t *testing.T, status string, descriptor map[string]any) providerhub.AdmittedManifest {
	t.Helper()
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatalf("encode the test descriptor: %v", err)
	}
	name, _ := descriptor["name"].(string)
	return providerhub.AdmittedManifest{
		ProviderID: name,
		RevisionID: "1:" + name + ":deadbeefdeadbeef",
		Digest:     "0123456789abcdef",
		Status:     status,
		Origin:     "https://" + name + ".example",
		Manifest:   encoded,
	}
}

func oneToolkitDescriptor(providerName, toolkitName string, extra map[string]any) map[string]any {
	toolkit := map[string]any{
		"name":        toolkitName,
		"description": "A test toolkit",
		"toolkit_config": map[string]any{
			"type":       "Test Configuration",
			"parameters": map[string]any{},
		},
		"provided_tools": []any{},
	}
	for key, value := range extra {
		toolkit[key] = value
	}
	return map[string]any{
		"name":                 providerName,
		"service_location_url": "https://" + providerName + ".example",
		"configuration":        map[string]any{},
		"provided_toolkits":    []any{toolkit},
	}
}

func TestAdmissionAllowsProjectionTable(t *testing.T) {
	cases := []struct {
		name    string
		status  string
		posture facade.AdmissionPosture
		allowed bool
	}{
		{"active under record", "active", facade.AdmissionRecord, true},
		{"active under enforce", "active", facade.AdmissionEnforce, true},
		{"inactive under record", "inactive", facade.AdmissionRecord, true},
		{"inactive under enforce", "inactive", facade.AdmissionEnforce, false},
		{"revoked under record", "revoked", facade.AdmissionRecord, false},
		{"revoked under enforce", "revoked", facade.AdmissionEnforce, false},
		{"an unknown future status under record", "quarantined", facade.AdmissionRecord, true},
		{"an unknown future status under enforce", "quarantined", facade.AdmissionEnforce, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := admissionAllowsProjection(testCase.status, testCase.posture); got != testCase.allowed {
				t.Fatalf("status %q under %q = %v, want %v",
					testCase.status, testCase.posture, got, testCase.allowed)
			}
		})
	}
}

func TestProviderHubProjectionAppliesTheAdmissionDecision(t *testing.T) {
	manifests := []providerhub.AdmittedManifest{
		manifestOf(t, "active", oneToolkitDescriptor("alpha", "Alpha", nil)),
		manifestOf(t, "inactive", oneToolkitDescriptor("beta", "Beta", nil)),
		manifestOf(t, "revoked", oneToolkitDescriptor("gamma", "Gamma", nil)),
	}

	recorded := &providerHubProjection{
		manifests: stubManifests{manifests: manifests}, posture: facade.AdmissionRecord,
	}
	projected, err := recorded.ProjectToolkitTypes(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, found := projectedByType(projected, "alpha_Alpha"); !found {
		t.Error("an active provider's toolkit must be offered")
	}
	if _, found := projectedByType(projected, "beta_Beta"); !found {
		t.Error("under `record` an inactive provider's toolkit is offered, flagged")
	}
	if _, found := projectedByType(projected, "gamma_Gamma"); found {
		t.Error("a revoked provider must never be offered, in either posture")
	}

	enforced := &providerHubProjection{
		manifests: stubManifests{manifests: manifests}, posture: facade.AdmissionEnforce,
	}
	projected, err = enforced.ProjectToolkitTypes(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projected) != 1 {
		t.Fatalf("under `enforce` only the active provider survives: %v", projected)
	}
}

func TestProviderHubProjectionFlagsAnInactiveRevision(t *testing.T) {
	projection := &providerHubProjection{
		manifests: stubManifests{manifests: []providerhub.AdmittedManifest{
			manifestOf(t, "inactive", oneToolkitDescriptor("beta", "Beta", nil)),
		}},
		posture: facade.AdmissionRecord,
	}
	projected, err := projection.ProjectToolkitTypes(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entry, found := projectedByType(projected, "beta_Beta")
	if !found {
		t.Fatal("the toolkit was not projected")
	}
	metadata, _ := entry.Schema["metadata"].(map[string]any)
	if metadata["admission_status"] != "inactive" {
		t.Fatalf("a type offered on a decision that is not in force must say so: %v", metadata)
	}
	if metadata["provider_origin"] != "https://beta.example" {
		t.Fatalf("provenance is reported, never dialled: %v", metadata["provider_origin"])
	}
}

func TestProviderHubProjectionPrefersTypeOverride(t *testing.T) {
	descriptor := oneToolkitDescriptor("deepwiki", "Wikis", map[string]any{
		"toolkit_metadata": map[string]any{"type_override": "wikis_query"},
	})
	projection := &providerHubProjection{
		manifests: stubManifests{manifests: []providerhub.AdmittedManifest{
			manifestOf(t, "active", descriptor),
		}},
		posture: facade.AdmissionRecord,
	}
	projected, _ := projection.ProjectToolkitTypes(context.Background())
	if _, found := projectedByType(projected, "wikis_query"); !found {
		t.Fatalf("type_override must name the type: got %v", projected)
	}
	if _, found := projectedByType(projected, "deepwiki_Wikis"); found {
		t.Fatal("the composed name must not be served beside the override")
	}
}

func TestProviderHubProjectionComposesTheNameWithoutAnOverride(t *testing.T) {
	projection := &providerHubProjection{
		manifests: stubManifests{manifests: []providerhub.AdmittedManifest{
			manifestOf(t, "active", oneToolkitDescriptor("deepwiki", "Wikis", nil)),
		}},
		posture: facade.AdmissionRecord,
	}
	projected, _ := projection.ProjectToolkitTypes(context.Background())
	if _, found := projectedByType(projected, "deepwiki_Wikis"); !found {
		t.Fatalf("want `<provider>_<toolkit>`: got %v", projected)
	}
}

func TestProviderHubProjectionKeepsTheFirstDeclarationOfAType(t *testing.T) {
	descriptor := map[string]any{
		"name":                 "dup",
		"service_location_url": "https://dup.example",
		"configuration":        map[string]any{},
		"provided_toolkits": []any{
			map[string]any{"name": "One", "description": "first",
				"toolkit_metadata": map[string]any{"type_override": "shared"},
				"toolkit_config":   map[string]any{"parameters": map[string]any{}}},
			map[string]any{"name": "Two", "description": "second",
				"toolkit_metadata": map[string]any{"type_override": "shared"},
				"toolkit_config":   map[string]any{"parameters": map[string]any{}}},
		},
	}
	projection := &providerHubProjection{
		manifests: stubManifests{manifests: []providerhub.AdmittedManifest{
			manifestOf(t, "active", descriptor),
		}},
		posture: facade.AdmissionRecord,
	}
	projected, _ := projection.ProjectToolkitTypes(context.Background())
	if len(projected) != 1 {
		t.Fatalf("want one type, got %v", projected)
	}
	if got := projected[0].Schema["description"]; got != "first" {
		t.Fatalf("the first declaration must win: description = %v", got)
	}
}

func TestProviderHubProjectionReportsAnUnreadableManifest(t *testing.T) {
	projection := &providerHubProjection{
		manifests: stubManifests{manifests: []providerhub.AdmittedManifest{
			{ProviderID: "broken", Status: "active", Manifest: []byte("{not json")},
		}},
		posture: facade.AdmissionRecord,
	}
	if _, err := projection.ProjectToolkitTypes(context.Background()); err == nil {
		t.Fatal("a manifest that will not parse must be reported, not read as an empty provider")
	}
}

// One provider's broken manifest must not take the others out of the catalogue,
// which is pylon's own per-provider try/except.
func TestProviderHubProjectionKeepsTheProvidersThatDoParse(t *testing.T) {
	projection := &providerHubProjection{
		manifests: stubManifests{manifests: []providerhub.AdmittedManifest{
			{ProviderID: "broken", Status: "active", Manifest: []byte("{not json")},
			manifestOf(t, "active", oneToolkitDescriptor("alpha", "Alpha", nil)),
		}},
		posture: facade.AdmissionRecord,
	}
	projected, err := projection.ProjectToolkitTypes(context.Background())
	if err != nil {
		t.Fatalf("one bad manifest must not fail the read: %v", err)
	}
	if _, found := projectedByType(projected, "alpha_Alpha"); !found {
		t.Fatalf("the readable provider was dropped: %v", projected)
	}
}

func TestProviderHubProjectionReportsAReadFailure(t *testing.T) {
	projection := &providerHubProjection{
		manifests: stubManifests{err: errors.New("connection refused")},
		posture:   facade.AdmissionRecord,
	}
	if _, err := projection.ProjectToolkitTypes(context.Background()); err == nil {
		t.Fatal("an unreadable admission plane must be reported")
	}
}

func TestProviderHubProjectionWithNoSourceContributesNothing(t *testing.T) {
	var projection *providerHubProjection
	projected, err := projection.ProjectToolkitTypes(context.Background())
	if err != nil || projected != nil {
		t.Fatalf("want a silent empty answer, got %v / %v", projected, err)
	}
	if got := newProviderHubProjection(nil).Name(); got == "" {
		t.Fatal("a source that fails must be identifiable in the log")
	}
}

func TestProviderParameterSchemaTable(t *testing.T) {
	yes := true
	cases := []struct {
		name      string
		parameter providerhub.ParameterDescriptor
		wantType  any
		wantKeys  map[string]any
	}{
		{
			name:      "Text becomes a multi-line string",
			parameter: providerhub.ParameterDescriptor{Type: "Text"},
			wantType:  "string",
			wantKeys:  map[string]any{"lines": 5},
		},
		{
			name:      "String",
			parameter: providerhub.ParameterDescriptor{Type: "String"},
			wantType:  "string",
		},
		{
			name:      "URL",
			parameter: providerhub.ParameterDescriptor{Type: "URL"},
			wantType:  "string",
		},
		{
			name:      "UUID",
			parameter: providerhub.ParameterDescriptor{Type: "UUID"},
			wantType:  "string",
		},
		{
			name:      "Secret is write-only and masked",
			parameter: providerhub.ParameterDescriptor{Type: "Secret"},
			wantType:  "string",
			wantKeys:  map[string]any{"format": "password", "secret": true, "writeOnly": true},
		},
		{
			name:      "Integer",
			parameter: providerhub.ParameterDescriptor{Type: "Integer"},
			wantType:  "integer",
		},
		{
			name:      "Float maps to integer, as pylon does",
			parameter: providerhub.ParameterDescriptor{Type: "Float"},
			wantType:  "integer",
		},
		{
			name:      "Bool",
			parameter: providerhub.ParameterDescriptor{Type: "Bool"},
			wantType:  "boolean",
		},
		{
			name:      "JSON with a list default is an array",
			parameter: providerhub.ParameterDescriptor{Type: "JSON", Default: []any{}},
			wantType:  "array",
		},
		{
			name:      "JSON with any other default is an object",
			parameter: providerhub.ParameterDescriptor{Type: "JSON", Default: map[string]any{}},
			wantType:  "object",
		},
		{
			name:      "an unknown type is an object",
			parameter: providerhub.ParameterDescriptor{Type: "Klingon"},
			wantType:  "object",
		},
		{
			name:      "an absent type is an object",
			parameter: providerhub.ParameterDescriptor{},
			wantType:  "object",
		},
		{
			name:      "the schema's own underscore spelling is read",
			parameter: providerhub.ParameterDescriptor{UnderscoreType: "Bool"},
			wantType:  "boolean",
		},
		{
			name: "json_schema_extra flips the property to the configuration kind",
			parameter: providerhub.ParameterDescriptor{
				Type:            "String",
				JSONSchemaExtra: map[string]any{"configuration_model": "llm"},
			},
			wantType: "configuration",
			wantKeys: map[string]any{"configuration_model": "llm"},
		},
		{
			name: "a reference picker keeps its array semantics",
			parameter: providerhub.ParameterDescriptor{
				Type:            "JSON",
				Default:         []any{},
				JSONSchemaExtra: map[string]any{"toolkit_types": []any{"github"}},
			},
			wantType: "array",
		},
		{
			name:      "required is read in either spelling",
			parameter: providerhub.ParameterDescriptor{Type: "String", UnderscoreReq: &yes},
			wantType:  "string",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			property := providerParameterSchema("some_field", testCase.parameter)
			if property["type"] != testCase.wantType {
				t.Fatalf("type = %v, want %v", property["type"], testCase.wantType)
			}
			for key, want := range testCase.wantKeys {
				if got := property[key]; got != want {
					t.Errorf("%s = %v, want %v", key, got, want)
				}
			}
			if property["title"] != "Some Field" {
				t.Errorf("title = %v, want the prettified key", property["title"])
			}
		})
	}
}

func TestProviderParameterSchemaDoesNotLetTheExtraOverwriteAComputedKey(t *testing.T) {
	property := providerParameterSchema("token", providerhub.ParameterDescriptor{
		Type:            "Secret",
		Description:     "the real description",
		JSONSchemaExtra: map[string]any{"description": "the extra's description", "hidden": true},
	})
	if property["description"] != "the real description" {
		t.Fatalf("the extra overwrote a computed key: %v", property["description"])
	}
	if property["hidden"] != true {
		t.Fatalf("the extra's own key was dropped: %v", property)
	}
}

func TestProviderToolkitSettingsHonoursFieldsOrderAndPrefixesEveryKey(t *testing.T) {
	yes := true
	config := providerhub.ToolkitConfigDescriptor{
		FieldsOrder: []string{"bucket", "llm_model", "absent_from_parameters"},
		Parameters: map[string]providerhub.ParameterDescriptor{
			"bucket":    {Type: "String", Required: &yes},
			"llm_model": {Type: "String"},
			"extra":     {Type: "String"},
		},
	}
	properties, required := providerToolkitSettings(config)

	for _, key := range []string{"bucket", "llm_model", "extra"} {
		if _, found := properties[providerParameterPrefix+key]; !found {
			t.Errorf("property %q is missing its `toolkit_configuration_` prefix: %v",
				key, keysOfAny(properties))
		}
	}
	if len(required) != 1 || required[0] != providerParameterPrefix+"bucket" {
		t.Fatalf("required must name the PREFIXED key: %v", required)
	}
	if len(properties) != 3 {
		t.Fatalf("a fields_order entry naming no parameter must add nothing: %v", keysOfAny(properties))
	}
}

func TestConvertArgsSchemaTable(t *testing.T) {
	args := map[string]providerhub.ArgDescriptor{
		"toolkit_id": {Type: "Integer", Required: true, Description: "the source", GreaterEq: 1.0},
		"branch":     {Type: "String"},
		"ratio":      {Type: "Float", LessThan: 1.0},
		"flag":       {Type: "Boolean"},
		"modes":      {Type: "List", Enum: []any{"fast", "slow"}},
		"blob":       {Type: "Yaml"},
	}
	schema := convertArgsSchema(args, "run_ingestion", "Run it")

	if schema["title"] != "run_ingestion" || schema["description"] != "Run it" {
		t.Fatalf("the tool's own name and description carry into its argument schema: %v", schema)
	}
	properties, _ := schema["properties"].(map[string]any)
	wantTypes := map[string]string{
		"toolkit_id": "integer", "branch": "string", "ratio": "number",
		"flag": "boolean", "modes": "array", "blob": "object",
	}
	for name, want := range wantTypes {
		property, _ := properties[name].(map[string]any)
		if property["type"] != want {
			t.Errorf("%s type = %v, want %v", name, property["type"], want)
		}
	}
	toolkitID, _ := properties["toolkit_id"].(map[string]any)
	if toolkitID["minimum"] != 1.0 {
		t.Errorf("ge maps to minimum: %v", toolkitID)
	}
	ratio, _ := properties["ratio"].(map[string]any)
	if ratio["exclusiveMaximum"] != 1.0 {
		t.Errorf("lt maps to exclusiveMaximum: %v", ratio)
	}
	branch, _ := properties["branch"].(map[string]any)
	if branch["description"] != "branch" {
		t.Errorf("an argument with no description falls back to its own name: %v", branch)
	}
	if _, bounded := branch["minimum"]; bounded {
		t.Error("a numeric bound must not be written onto a string")
	}
	required, _ := schema["required"].([]any)
	if len(required) != 1 || required[0] != "toolkit_id" {
		t.Fatalf("required = %v", required)
	}
}

func TestConvertArgsSchemaKeepsADeclaredFalsyDefault(t *testing.T) {
	var argument providerhub.ArgDescriptor
	if err := json.Unmarshal([]byte(`{"type":"Integer","default":0}`), &argument); err != nil {
		t.Fatalf("decode: %v", err)
	}
	schema := convertArgsSchema(map[string]providerhub.ArgDescriptor{"count": argument}, "", "")
	properties, _ := schema["properties"].(map[string]any)
	count, _ := properties["count"].(map[string]any)
	value, declared := count["default"]
	if !declared {
		t.Fatalf("a declared default of 0 must survive: %v", count)
	}
	if value != 0.0 {
		t.Fatalf("default = %v", value)
	}
}

func TestConvertArgsSchemaOmitsADefaultTheDocumentNeverDeclared(t *testing.T) {
	var argument providerhub.ArgDescriptor
	if err := json.Unmarshal([]byte(`{"type":"String"}`), &argument); err != nil {
		t.Fatalf("decode: %v", err)
	}
	schema := convertArgsSchema(map[string]providerhub.ArgDescriptor{"name": argument}, "", "")
	properties, _ := schema["properties"].(map[string]any)
	name, _ := properties["name"].(map[string]any)
	if _, declared := name["default"]; declared {
		t.Fatalf("an undeclared default must not be invented: %v", name)
	}
}

func TestPrettifyTitleTable(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"bucket", "Bucket"},
		{"llm_model", "LLM Model"},
		{"api_key", "API Key"},
		{"APIKey", "API Key"},
		{"serviceLocationUrl", "Service Location URL"},
		{"toolkit-id", "Toolkit ID"},
		{"source_configs", "Source Configs"},
		{"HTTPTimeout", "HTTP Timeout"},
	}
	for _, testCase := range cases {
		t.Run(testCase.in, func(t *testing.T) {
			if got := prettifyTitle(testCase.in); got != testCase.want {
				t.Fatalf("prettifyTitle(%q) = %q, want %q", testCase.in, got, testCase.want)
			}
		})
	}
}

// The two descriptors this repository actually ships. They are the shape a
// running deployment stores, so they are what the projection must survive.
func TestProjectionOfTheShippedDescriptors(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "..",
		"elitea-subapp-host", "internal", "apps")
	cases := []struct {
		app       string
		wantTypes []string
	}{
		{app: "inventory", wantTypes: []string{"inventory", "inventory_search"}},
		{app: "deepwiki", wantTypes: []string{"wikis_query", "wiki_query"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.app, func(t *testing.T) {
			manifest, err := os.ReadFile(filepath.Join(root, testCase.app, "descriptor.json"))
			if err != nil {
				t.Skipf("the shipped descriptor is not readable from here: %v", err)
			}
			projection := &providerHubProjection{
				manifests: stubManifests{manifests: []providerhub.AdmittedManifest{{
					ProviderID: testCase.app, Status: "active",
					Origin: "https://" + testCase.app + ".example", Manifest: manifest,
				}}},
				posture: facade.AdmissionRecord,
			}
			projected, err := projection.ProjectToolkitTypes(context.Background())
			if err != nil {
				t.Fatalf("the shipped descriptor did not project: %v", err)
			}
			for _, want := range testCase.wantTypes {
				entry, found := projectedByType(projected, want)
				if !found {
					t.Fatalf("type %q is missing; got %v", want, typeNamesOf(projected))
				}
				assertWellFormedTypeSchema(t, want, entry.Schema)
			}
		})
	}
}

func typeNamesOf(projected []ProjectedToolkitType) []string {
	names := make([]string, 0, len(projected))
	for _, entry := range projected {
		names = append(names, entry.Type)
	}
	sortStrings(names)
	return names
}

// assertWellFormedTypeSchema states what every served type must carry for the
// create form to render it at all.
func assertWellFormedTypeSchema(t *testing.T, typeName string, schema map[string]any) {
	t.Helper()
	if schema["type"] != "object" {
		t.Errorf("%s: type = %v, want object", typeName, schema["type"])
	}
	if schema["title"] != typeName {
		t.Errorf("%s: title = %v, want the type name", typeName, schema["title"])
	}
	metadata, ok := schema["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no metadata block", typeName)
	}
	if label, _ := metadata["label"].(string); label == "" {
		t.Errorf("%s: an empty label is an unreachable tile", typeName)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no properties block", typeName)
	}
	selected, ok := properties["selected_tools"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no selected_tools property", typeName)
	}
	if _, ok := selected["args_schemas"].(map[string]any); !ok {
		t.Errorf("%s: selected_tools carries no args_schemas map", typeName)
	}
	if _, ok := schema["required"].([]any); !ok {
		t.Errorf("%s: required must be a list, even an empty one", typeName)
	}
}
