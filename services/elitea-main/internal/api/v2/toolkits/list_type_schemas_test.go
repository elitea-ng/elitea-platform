package toolkits_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

// The web client reads properties.selected_tools.args_schemas[tool] and renders
// a form from it (apps/elitea-web/src/features/toolkits/ui/test-tools/
// useGetSelectedToolSchema.ts:76, ui/form/ToolBase/ToolBase.render.tsx:274).
// Until the SDK snapshot started carrying argument schemas, every entry here was
// the placeholder {"type":"object"} — an object schema with no properties, from
// which the create-index form rendered zero inputs and left its Index button
// permanently disabled. Every assertion below is on the real index_data schema
// at SDK revision b5113a1, so none of them can pass against that placeholder.
func TestToolkitTypeCatalogueServesTheSDKArgumentSchemas(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, toolkits.WithArgumentSchemas(pinnedSnapshot(t)))

	indexData := argumentSchema(t, body, "artifact", "index_data")
	if indexData["type"] != "object" {
		t.Errorf("index_data type=%#v, want %q", indexData["type"], "object")
	}
	required, ok := indexData["required"].([]any)
	if !ok || len(required) != 1 || required[0] != "index_name" {
		t.Errorf("index_data required=%#v, want [index_name]", indexData["required"])
	}
	properties, ok := indexData["properties"].(map[string]any)
	if !ok {
		t.Fatalf("index_data carries no properties object: %#v", indexData)
	}
	for _, argument := range []string{
		"index_name", "clean_index", "folder", "include_extensions",
		"skip_extensions", "progress_step", "chunking_config",
	} {
		if _, ok := properties[argument].(map[string]any); !ok {
			t.Errorf("index_data.properties is missing %q; have %v", argument, keysOf(properties))
		}
	}
	indexName, _ := properties["index_name"].(map[string]any)
	if indexName["type"] != "string" || indexName["maxLength"] != float64(32) {
		t.Errorf("index_name=%#v, want a string with maxLength 32", indexName)
	}

	// The SETTINGS properties are a different resource and must survive the
	// swap: they are the fields the create-toolkit form renders, and the
	// snapshot's own annotation "properties" do not contain them (artifact's
	// annotations name only embedding_model and pgvector_configuration — no
	// bucket at all), so they cannot be sourced from it.
	artifact := body["artifact"].(map[string]any)["properties"].(map[string]any)
	for _, setting := range []string{"bucket", "embedding_model", "pgvector_configuration"} {
		if _, ok := artifact[setting]; !ok {
			t.Errorf("artifact settings lost %q; have %v", setting, keysOf(artifact))
		}
	}
}

// Remote MCP and OpenAPI discover their tools at runtime.
func TestToolkitTypeCatalogueServesAnEmptyArgumentSchemaMapForRuntimeDiscoveredTools(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, toolkits.WithArgumentSchemas(pinnedSnapshot(t)))

	for _, toolkitType := range []string{"openapi", "mcp"} {
		selectedTools := selectedToolsSchema(t, body, toolkitType)
		argsSchemas, ok := selectedTools["args_schemas"].(map[string]any)
		if !ok {
			t.Fatalf("%s selected_tools has no args_schemas object: %#v", toolkitType, selectedTools)
		}
		if len(argsSchemas) != 0 {
			t.Errorf("%s args_schemas=%v, want empty", toolkitType, keysOf(argsSchemas))
		}
	}
}

func TestToolkitTypeCatalogueServesRemoteMCPConnectionSettings(t *testing.T) {
	t.Parallel()
	body := getToolkitTypeCatalogue(t, pinnedCatalogueOptions(t)...)
	schema, ok := body["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("catalogue has no remote MCP type; have %v", keysOf(body))
	}
	if !reflect.DeepEqual(schema["required"], []any{"url"}) {
		t.Fatalf("remote MCP required=%#v, want only url", schema["required"])
	}
	properties := schema["properties"].(map[string]any)
	for _, field := range []string{"url", "headers", "client_id", "client_secret", "scopes", "timeout", "selected_tools", "enable_caching", "cache_ttl", "ssl_verify"} {
		if _, ok := properties[field].(map[string]any); !ok {
			t.Errorf("remote MCP settings omit %q", field)
		}
	}
	secret := properties["client_secret"].(map[string]any)
	if secret["writeOnly"] != true || secret["format"] != "password" {
		t.Errorf("client_secret must use the password field: %#v", secret)
	}
	if properties["ssl_verify"].(map[string]any)["default"] != true {
		t.Error("TLS verification must default to true")
	}
	if selectedToolsSchema(t, body, "mcp")["type"] != "array" {
		t.Error("selected_tools must retain its array contract")
	}
}

// The current product UI renders OpenAPI from this exact settings contract.
// A spec_url field falls through to a plain text box and removes the inline
// schema editor, operation preview, and selected-operation state.
func TestToolkitTypeCatalogueServesTheCurrentOpenAPISettingsContract(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, pinnedCatalogueOptions(t)...)
	typeSchema, ok := body["openapi"].(map[string]any)
	if !ok {
		t.Fatalf("catalogue has no openapi type; have %v", keysOf(body))
	}
	properties, ok := typeSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("openapi has no properties object: %#v", typeSchema)
	}

	if _, present := properties["spec_url"]; present {
		t.Error("openapi still serves the retired spec_url setting")
	}
	spec, ok := properties["spec"].(map[string]any)
	if !ok {
		t.Fatalf("openapi has no spec property; have %v", keysOf(properties))
	}
	if spec["type"] != "string" || spec["ui_component"] != "openapi_spec" {
		t.Errorf("openapi spec=%#v, want the inline OpenAPI editor contract", spec)
	}
	baseURL, ok := properties["base_url"].(map[string]any)
	if !ok || baseURL["default"] != nil {
		t.Errorf("openapi base_url=%#v, want an optional null-default override", properties["base_url"])
	}
	selectedTools := selectedToolsSchema(t, body, "openapi")
	if selectedTools["type"] != "array" {
		t.Errorf("openapi selected_tools type=%#v, want array", selectedTools["type"])
	}
	items, ok := selectedTools["items"].(map[string]any)
	if !ok || items["type"] != "string" {
		t.Errorf("openapi selected_tools items=%#v, want string items", selectedTools["items"])
	}

	required, ok := typeSchema["required"].([]any)
	if !ok || !reflect.DeepEqual(required, []any{"openapi_configuration", "spec"}) {
		t.Errorf("openapi required=%#v, want [openapi_configuration spec]", typeSchema["required"])
	}
}

// The description must not promise a capability nothing implements.
//
// It used to say the specification could be "a URL", which no component in this
// system has ever honoured: the native worker refuses an http(s) string outright
// (parse_source -> UnsupportedSource), the SDK worker only runs json.loads then
// yaml.safe_load over it, elitea-main answers DiscoverTools from the stored row,
// and the create form's editor parses the same two ways and shows no operations
// for a URL. A user who believed it saved a toolkit that failed at its first
// tool call instead of at save time. The handler's own note records why the
// fetch was not implemented here instead.
//
// Asserted on the property the client is SERVED, not on the package map, so a
// later projection step that rewrites descriptions cannot reintroduce the claim
// behind this test's back.
func TestToolkitTypeCatalogueDoesNotPromiseSpecificationsAreFetchedFromAURL(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, pinnedCatalogueOptions(t)...)
	typeSchema, ok := body["openapi"].(map[string]any)
	if !ok {
		t.Fatalf("catalogue has no openapi type; have %v", keysOf(body))
	}
	properties, ok := typeSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("openapi has no properties object: %#v", typeSchema)
	}
	spec, ok := properties["spec"].(map[string]any)
	if !ok {
		t.Fatalf("openapi has no spec property; have %v", keysOf(properties))
	}
	description, ok := spec["description"].(string)
	if !ok || description == "" {
		t.Fatalf("openapi spec has no description: %#v", spec)
	}
	// Pinned verbatim rather than matched on "url". The honest text names a URL
	// on purpose — to tell the user holding one that it will not work — so a
	// keyword search would fail the correct string and pass a reworded promise
	// that avoided the word. What must not come back is the CLAIM, and the only
	// stable way to say that here is to state the sentence the handler serves.
	const want = "OpenAPI specification as raw JSON or YAML text. A URL is not fetched."
	if description != want {
		t.Errorf("openapi spec description = %q, want %q", description, want)
	}
	// The retired promise, in the exact form it was served in, so a revert is
	// named rather than merely different.
	if strings.Contains(description, "as a URL or raw JSON") {
		t.Errorf("openapi spec description offers a URL again: %q", description)
	}
}

// Four catalogue types are not SDK toolkits at all — the SDK's database toolkit
// is `sql`, and datasource/application/custom are elitea_core-native. Their
// hand-written tool-name lists are the only source there is, and dropping them
// would take the Indexes tab away from datasource toolkits, which is decided by
// exactly these keys (apps/elitea-web/src/features/toolkits/lib/helpers/
// indexesTabVisibility.ts:49).
func TestToolkitTypeCatalogueKeepsToolNamesForTypesTheSDKDoesNotDefine(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, toolkits.WithArgumentSchemas(pinnedSnapshot(t)))

	for toolkitType, tools := range map[string][]string{
		"datasource":  {"index_data", "search_data"},
		"database":    {"query", "list_tables", "describe_table"},
		"application": {"ask_agent"},
	} {
		selectedTools := selectedToolsSchema(t, body, toolkitType)
		argsSchemas, ok := selectedTools["args_schemas"].(map[string]any)
		if !ok {
			t.Fatalf("%s selected_tools has no args_schemas object: %#v", toolkitType, selectedTools)
		}
		for _, tool := range tools {
			if _, ok := argsSchemas[tool]; !ok {
				t.Errorf("%s lost tool %q; have %v", toolkitType, tool, keysOf(argsSchemas))
			}
		}
	}
}

// A $ref is only meaningful next to the $defs it points at. This is the test
// that fails if the argument schema is ever carried in a narrowed Go type that
// knows only the keywords this codebase happens to use: the schema arrives at
// the browser structurally intact or not at all.
func TestToolkitTypeCatalogueServesNestedSchemaReferencesVerbatim(t *testing.T) {
	t.Parallel()

	const schema = `{
		"$defs":{"Chunk":{"properties":{"max_tokens":{"type":"integer","default":512}},"type":"object"}},
		"properties":{"chunk":{"$ref":"#/$defs/Chunk"},"chunks":{"items":{"$ref":"#/$defs/Chunk"},"type":"array"}},
		"required":["chunk"],
		"type":"object"
	}`
	var indexData map[string]any
	if err := json.Unmarshal([]byte(schema), &indexData); err != nil {
		t.Fatal(err)
	}
	source := stubArgumentSchemas{"artifact": {"index_data": indexData}}

	body := getToolkitTypeCatalogue(t, toolkits.WithArgumentSchemas(source))

	var want any
	if err := json.Unmarshal([]byte(schema), &want); err != nil {
		t.Fatal(err)
	}
	if got := any(argumentSchema(t, body, "artifact", "index_data")); !reflect.DeepEqual(got, want) {
		t.Errorf("served argument schema:\n got %#v\nwant the source schema verbatim", got)
	}
}

// A source that cannot produce its schemas is a broken binary. Answering 200
// with the settings-only catalogue would put the empty-form defect back, silently.
func TestToolkitTypeCatalogueFailsLoudlyWhenTheSchemaSourceErrors(t *testing.T) {
	t.Parallel()

	handler := toolkits.NewHandlerWithRepo(
		&mockRepo{},
		toolkits.WithArgumentSchemas(failingArgumentSchemas{}),
	)
	response := httptest.NewRecorder()
	handler.ListTypeSchemas(response, httptest.NewRequest(http.MethodGet, "/toolkits/prompt_lib/1", nil))

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500: %s", response.Code, response.Body.String())
	}
}

// The catalogue is assembled from a package-level map on every request, so a
// response that got mutated (or one request's schemas leaking into the next)
// would corrupt every later caller in the process.
func TestToolkitTypeCatalogueDoesNotLeakBetweenRequests(t *testing.T) {
	t.Parallel()

	first := getToolkitTypeCatalogue(t, toolkits.WithArgumentSchemas(
		stubArgumentSchemas{"artifact": {"only_tool": {"type": "object"}}},
	))
	if _, ok := argumentSchema(t, first, "artifact", "only_tool")["type"]; !ok {
		t.Fatal("the stub source did not reach the response")
	}

	second := getToolkitTypeCatalogue(t, toolkits.WithArgumentSchemas(pinnedSnapshot(t)))
	argsSchemas := selectedToolsSchema(t, second, "artifact")["args_schemas"].(map[string]any)
	if _, leaked := argsSchemas["only_tool"]; leaked {
		t.Errorf("the previous request's schemas survived into this one: %v", keysOf(argsSchemas))
	}
}

type dynamicTypeSchemaStub struct{}

func (dynamicTypeSchemaStub) ListToolkitTypeSchemas(context.Context) (map[string]map[string]any, error) {
	return map[string]map[string]any{
		"mcp_ado": {
			"type": "object",
			"properties": map[string]any{
				"api_token": map[string]any{"type": "string", "secret": true},
			},
		},
	}, nil
}

func TestToolkitTypeCatalogueIncludesDynamicPrebuiltMCP(t *testing.T) {
	body := getToolkitTypeCatalogue(t, toolkits.WithDynamicTypeSchemas(dynamicTypeSchemaStub{}))
	typeSchema := body["mcp_ado"].(map[string]any)
	properties := typeSchema["properties"].(map[string]any)
	if secret, _ := properties["api_token"].(map[string]any)["secret"].(bool); !secret {
		t.Fatal("dynamic MCP secret annotation was lost")
	}
}

func pinnedSnapshot(t *testing.T) *runtimecomposition.CurrentToolkitSchemaSnapshot {
	t.Helper()
	snapshot, err := runtimecomposition.LoadPinnedCurrentToolkitSchemaSnapshot()
	if err != nil {
		t.Fatalf("load pinned toolkit schema snapshot: %v", err)
	}
	return snapshot
}

func getToolkitTypeCatalogue(t *testing.T, opts ...toolkits.Option) map[string]any {
	t.Helper()
	handler := toolkits.NewHandlerWithRepo(&mockRepo{}, opts...)
	response := httptest.NewRecorder()
	handler.ListTypeSchemas(response, httptest.NewRequest(http.MethodGet, "/toolkits/prompt_lib/1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (%s)", err, response.Body.String())
	}
	return body
}

func selectedToolsSchema(t *testing.T, body map[string]any, toolkitType string) map[string]any {
	t.Helper()
	typeSchema, ok := body[toolkitType].(map[string]any)
	if !ok {
		t.Fatalf("catalogue has no %q type; have %v", toolkitType, keysOf(body))
	}
	properties, ok := typeSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no properties object: %#v", toolkitType, typeSchema)
	}
	selectedTools, ok := properties["selected_tools"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no selected_tools property: %v", toolkitType, keysOf(properties))
	}
	return selectedTools
}

func argumentSchema(t *testing.T, body map[string]any, toolkitType, tool string) map[string]any {
	t.Helper()
	selectedTools := selectedToolsSchema(t, body, toolkitType)
	argsSchemas, ok := selectedTools["args_schemas"].(map[string]any)
	if !ok {
		t.Fatalf("%s selected_tools has no args_schemas object: %#v", toolkitType, selectedTools)
	}
	schema, ok := argsSchemas[tool].(map[string]any)
	if !ok {
		t.Fatalf("%s has no %q argument schema; have %v", toolkitType, tool, keysOf(argsSchemas))
	}
	return schema
}

func keysOf[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type stubArgumentSchemas map[string]map[string]map[string]any

func (s stubArgumentSchemas) ToolkitArgumentSchemas(
	toolkitType string,
) (map[string]map[string]any, bool, error) {
	schemas, found := s[toolkitType]
	return schemas, found, nil
}

type failingArgumentSchemas struct{}

func (failingArgumentSchemas) ToolkitArgumentSchemas(
	string,
) (map[string]map[string]any, bool, error) {
	return nil, false, errors.New("snapshot unavailable")
}
