package agentexecution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
)

type currentAgentSettingsResolverStub struct {
	requests []configurationapp.CurrentToolkitSettingsRequest
	result   map[string]any
	err      error
}

func (stub *currentAgentSettingsResolverStub) Resolve(
	_ context.Context,
	request configurationapp.CurrentToolkitSettingsRequest,
) (map[string]any, error) {
	stub.requests = append(stub.requests, request)
	return stub.result, stub.err
}

type currentAgentNameResolverStub struct {
	requests []CurrentAgentToolkitNameRequest
	result   string
	err      error
}

type currentAgentModelCatalogStub struct {
	queries  []configurationapp.CurrentModelCatalogQuery
	response configurationapp.CurrentModelCatalogResponse
	err      error
}

func (stub *currentAgentModelCatalogStub) Get(
	_ context.Context,
	query configurationapp.CurrentModelCatalogQuery,
) (configurationapp.CurrentModelCatalogResponse, error) {
	stub.queries = append(stub.queries, query)
	return stub.response, stub.err
}

func currentAgentModelCatalogForTest(compatible bool) *currentAgentModelCatalogStub {
	reasoning := false
	return &currentAgentModelCatalogStub{response: configurationapp.CurrentModelCatalogResponse{
		Items: []configurationapp.CurrentModelCatalogItem{{
			Name: "model", ProjectID: 7, OpenAICompatible: &compatible,
			SupportsReasoning: &reasoning,
		}},
	}}
}

func (stub *currentAgentNameResolverStub) ResolveCurrentAgentToolkitName(
	_ context.Context,
	request CurrentAgentToolkitNameRequest,
) (string, error) {
	stub.requests = append(stub.requests, request)
	return stub.result, stub.err
}

func TestCurrentApplicationToolSnapshotFreezesGenericToolkitReferences(t *testing.T) {
	settings := &currentAgentSettingsResolverStub{result: map[string]any{
		"selected_tools":  []any{"search", "read"},
		"available_tools": []any{"search", "read", "write"},
		"credential": map[string]any{
			configurationapp.CurrentFrozenConfigurationMarker: true,
			"configuration_project_id":                        json.Number("7"),
			"configuration_type":                              "sharepoint",
			"configuration_uuid":                              "configuration-1",
			"token":                                           "{{secret.SHAREPOINT_TOKEN}}",
		},
	}}
	names := &currentAgentNameResolverStub{result: "team_docs"}
	models := currentAgentModelCatalogForTest(true)
	service, err := NewCurrentApplicationToolSnapshotService(settings, names, models, &currentAgentGuardrailStub{}, &currentProjectContextStub{}, 1)
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.FreezeCurrentApplicationVersion(
		context.Background(),
		CurrentApplicationVersionFreezeRequest{
			ProjectID: 7, ActorUserID: 11,
			VersionDetails: json.RawMessage(`{
  "id":41,
  "agent_type":"agent",
  "llm_settings":{"model_name":"model"},
  "tools":[{
    "id":19,
    "type":"sharepoint",
    "name":"Team Docs",
    "description":"Current toolkit",
    "author_id":11,
    "settings":{"selected_tools":["search","read"],"sharepoint_configuration":{"elitea_title":"team"}},
    "meta":{"current":true},
    "is_pinned":false
  }]
}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	version, err := decodeCurrentApplicationVersion(result)
	if err != nil {
		t.Fatal(err)
	}
	tool := version["tools"].([]any)[0].(map[string]any)
	llmSettings := version["llm_settings"].(map[string]any)
	toolID, validToolID := positiveCurrentAgentJSONInteger(tool["id"])
	if !validToolID || toolID != 19 || tool["type"] != "sharepoint" ||
		tool["description"] != "Current toolkit" || tool["toolkit_name"] != "team_docs" ||
		!reflect.DeepEqual(tool["settings"], settings.result) {
		t.Fatalf("frozen tool=%#v", tool)
	}
	modelProjectID, validModelProjectID := positiveCurrentAgentJSONInteger(llmSettings["model_project_id"])
	if llmSettings["openai_compatible"] != true || !validModelProjectID || modelProjectID != 7 ||
		len(models.queries) != 1 || models.queries[0].Section != configurationapp.CurrentModelSectionLLM ||
		models.queries[0].ProjectID != 7 || models.queries[0].PublicProjectID != 1 ||
		!models.queries[0].IncludeShared {
		t.Fatalf("llm_settings=%#v queries=%+v", llmSettings, models.queries)
	}
	if len(settings.requests) != 1 || settings.requests[0].Mode != configurationapp.CurrentToolkitSettingsReferenceMode ||
		settings.requests[0].ProjectID != 7 || settings.requests[0].UserID != 11 ||
		len(names.requests) != 1 || names.requests[0].ToolkitType != "sharepoint" ||
		!reflect.DeepEqual(names.requests[0].Settings, settings.result) {
		t.Fatalf("settings=%+v names=%+v", settings.requests, names.requests)
	}
}

func TestCurrentApplicationToolSnapshotFreezesSavedMCPReferenceWithoutSpecialCase(t *testing.T) {
	settings := &currentAgentSettingsResolverStub{result: map[string]any{
		"url":            "https://mcp.example.invalid/events",
		"selected_tools": []any{"search_docs"},
	}}
	names := &currentAgentNameResolverStub{result: "documentation-mcp"}
	service, err := NewCurrentApplicationToolSnapshotService(
		settings,
		names,
		currentAgentModelCatalogForTest(false),
		&currentAgentGuardrailStub{}, &currentProjectContextStub{},
		1,
	)
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.FreezeCurrentApplicationVersion(
		context.Background(),
		CurrentApplicationVersionFreezeRequest{
			ProjectID: 7, ActorUserID: 11,
			VersionDetails: json.RawMessage(`{
  "llm_settings":{"model_name":"model"},
  "tools":[{
    "id":52,
    "type":"mcp",
    "name":"documentation-mcp",
    "description":"Saved external MCP server",
    "author_id":11,
    "settings":{"url":"https://mcp.example.invalid/events","selected_tools":["search_docs"]},
    "meta":{"mcp":true},
    "is_pinned":false
  }]
}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	version, err := decodeCurrentApplicationVersion(result)
	if err != nil {
		t.Fatal(err)
	}
	tool := version["tools"].([]any)[0].(map[string]any)
	toolID, validToolID := positiveCurrentAgentJSONInteger(tool["id"])
	if !validToolID || toolID != 52 || tool["type"] != "mcp" ||
		tool["toolkit_name"] != "documentation-mcp" ||
		!reflect.DeepEqual(tool["settings"], settings.result) ||
		len(settings.requests) != 1 || settings.requests[0].ToolkitType != "mcp" ||
		len(names.requests) != 1 || names.requests[0].ToolkitType != "mcp" {
		t.Fatalf("tool=%#v settings=%+v names=%+v", tool, settings.requests, names.requests)
	}
}

func TestCurrentApplicationToolSnapshotOmitsOnlySchemaUnavailableToolkit(t *testing.T) {
	settings := &currentAgentSettingsResolverStub{err: configurationapp.ErrCurrentToolkitSchemaNotFound}
	names := &currentAgentNameResolverStub{}
	service, err := NewCurrentApplicationToolSnapshotService(
		settings,
		names,
		currentAgentModelCatalogForTest(false),
		&currentAgentGuardrailStub{}, &currentProjectContextStub{},
		1,
	)
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.FreezeCurrentApplicationVersion(
		context.Background(),
		CurrentApplicationVersionFreezeRequest{
			ProjectID: 7, ActorUserID: 11,
			VersionDetails: json.RawMessage(`{
  "llm_settings":{"model_name":"model"},
  "tools":[{
    "id":11,
    "type":"wikis_Wikis",
    "name":"configurations",
    "settings":{"selected_tools":[]}
  }]
}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	version, err := decodeCurrentApplicationVersion(result)
	if err != nil {
		t.Fatal(err)
	}
	tools, ok := version["tools"].([]any)
	if !ok || len(tools) != 0 || len(settings.requests) != 1 || len(names.requests) != 0 {
		t.Fatalf("tools=%#v settings=%+v names=%+v", version["tools"], settings.requests, names.requests)
	}
}

func TestCurrentApplicationToolSnapshotDoesNotHideToolkitDependencyFailure(t *testing.T) {
	settings := &currentAgentSettingsResolverStub{err: configurationapp.ErrCurrentToolkitSettingsDependency}
	service, err := NewCurrentApplicationToolSnapshotService(
		settings,
		&currentAgentNameResolverStub{},
		currentAgentModelCatalogForTest(false),
		&currentAgentGuardrailStub{}, &currentProjectContextStub{},
		1,
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.FreezeCurrentApplicationVersion(
		context.Background(),
		CurrentApplicationVersionFreezeRequest{
			ProjectID: 7, ActorUserID: 11,
			VersionDetails: json.RawMessage(`{
  "llm_settings":{"model_name":"model"},
  "tools":[{"id":19,"type":"github","settings":{}}]
}`),
		},
	)
	if !errors.Is(err, ErrUnsupportedCurrentAgentStart) {
		t.Fatalf("error=%v", err)
	}
}

func TestCurrentApplicationToolSnapshotPreservesSameProjectLeafApplicationReference(t *testing.T) {
	settings := &currentAgentSettingsResolverStub{}
	names := &currentAgentNameResolverStub{}
	service, err := NewCurrentApplicationToolSnapshotService(
		settings,
		names,
		currentAgentModelCatalogForTest(false),
		&currentAgentGuardrailStub{}, &currentProjectContextStub{},
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.FreezeCurrentApplicationVersion(
		context.Background(),
		CurrentApplicationVersionFreezeRequest{
			ProjectID: 7, ActorUserID: 11,
			VersionDetails: json.RawMessage(`{
  "llm_settings":{"model_name":"model"},
  "tools":[{
    "type":"application",
    "name":"release-notes",
    "description":"Read-only child agent",
    "author_id":11,
    "participant_id":29,
    "project_id":7,
    "settings":{
      "variables":[],
      "application_id":3,
      "selected_tools":[],
      "application_version_id":4
    },
    "id":null,
    "toolkit_name":"release-notes",
    "agent_type":"openai",
    "created_at":"2026-08-04T10:00:00Z"
  }]
}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	version, err := decodeCurrentApplicationVersion(result)
	if err != nil {
		t.Fatal(err)
	}
	tool := version["tools"].([]any)[0].(map[string]any)
	toolSettings := tool["settings"].(map[string]any)
	toolProjectID, validProjectID := positiveCurrentAgentJSONInteger(tool["project_id"])
	applicationID, validApplicationID := positiveCurrentAgentJSONInteger(toolSettings["application_id"])
	versionID, validVersionID := positiveCurrentAgentJSONInteger(toolSettings["application_version_id"])
	// `agent`, not the authored `openai`: normalizeCurrentAgentRuntimeProfile
	// rewrites the stored direct-agent name to the one the runtime matches on,
	// for nested references as well as the top level — the runtime applies the
	// same rule to a nested agent
	// (services/elitea-worker-rust/src/agents/application_tools.rs:1043), so
	// leaving the child alone would refuse every agent that calls another agent.
	if tool["type"] != "application" || tool["id"] != nil ||
		tool["name"] != "release-notes" || tool["toolkit_name"] != "release-notes" ||
		tool["agent_type"] != "agent" || !validProjectID || toolProjectID != 7 ||
		!validApplicationID || applicationID != 3 || !validVersionID || versionID != 4 ||
		len(settings.requests) != 0 || len(names.requests) != 0 {
		t.Fatalf("tool=%#v settings=%+v names=%+v", tool, settings.requests, names.requests)
	}
}

func TestCurrentApplicationToolSnapshotPreservesSameProjectPipelineReference(t *testing.T) {
	service, err := NewCurrentApplicationToolSnapshotService(
		&currentAgentSettingsResolverStub{},
		&currentAgentNameResolverStub{},
		currentAgentModelCatalogForTest(false),
		&currentAgentGuardrailStub{}, &currentProjectContextStub{},
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.FreezeCurrentApplicationVersion(
		context.Background(),
		CurrentApplicationVersionFreezeRequest{
			ProjectID: 7, ActorUserID: 11,
			VersionDetails: json.RawMessage(`{
  "llm_settings":{"model_name":"model"},
  "tools":[{
    "type":"application",
    "name":"release-pipeline",
    "description":"Current pipeline nesting primitive",
    "author_id":11,
    "participant_id":29,
    "project_id":7,
    "settings":{
      "variables":[],
      "application_id":3,
      "selected_tools":[],
      "application_version_id":4
    },
    "id":null,
    "toolkit_name":"release-pipeline",
    "agent_type":"pipeline",
    "created_at":"2026-08-04T10:00:00Z"
  }]
}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	version, err := decodeCurrentApplicationVersion(result)
	if err != nil {
		t.Fatal(err)
	}
	tool := version["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "application" || tool["agent_type"] != "pipeline" ||
		tool["name"] != "release-pipeline" || tool["toolkit_name"] != "release-pipeline" {
		t.Fatalf("pipeline tool=%#v", tool)
	}
}

func TestCurrentApplicationToolSnapshotPreservesStoredApplicationReference(t *testing.T) {
	settings := &currentAgentSettingsResolverStub{}
	names := &currentAgentNameResolverStub{}
	service, err := NewCurrentApplicationToolSnapshotService(
		settings,
		names,
		currentAgentModelCatalogForTest(false),
		&currentAgentGuardrailStub{}, &currentProjectContextStub{},
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.FreezeCurrentApplicationVersion(
		context.Background(),
		CurrentApplicationVersionFreezeRequest{
			ProjectID: 7, ActorUserID: 11,
			VersionDetails: json.RawMessage(`{
  "llm_settings":{"model_name":"model"},
  "tools":[{
    "id":44,
    "type":"application",
    "name":"nested-agent",
    "description":null,
    "author_id":11,
    "settings":{"application_id":3,"application_version_id":4},
    "meta":{},
    "created_at":"2026-08-07T10:00:00Z",
    "toolkit_name":"nested-agent",
    "author":null,
    "agent_type":"agent",
    "online":null,
    "icon_meta":null,
    "variables":[],
    "is_pinned":false,
    "indexes_count":null
  }]
}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	version, err := decodeCurrentApplicationVersion(result)
	if err != nil {
		t.Fatal(err)
	}
	tool := version["tools"].([]any)[0].(map[string]any)
	toolSettings := tool["settings"].(map[string]any)
	toolID, validToolID := positiveCurrentAgentJSONInteger(tool["id"])
	applicationID, validApplicationID := positiveCurrentAgentJSONInteger(toolSettings["application_id"])
	versionID, validVersionID := positiveCurrentAgentJSONInteger(toolSettings["application_version_id"])
	if !validToolID || toolID != 44 || !validApplicationID || applicationID != 3 ||
		!validVersionID || versionID != 4 || tool["agent_type"] != "agent" ||
		len(settings.requests) != 0 || len(names.requests) != 0 {
		t.Fatalf("tool=%#v settings=%+v names=%+v", tool, settings.requests, names.requests)
	}
}

func TestFreezeCurrentStoredApplicationReferenceRejectsCredentialBearingSettings(t *testing.T) {
	tool := map[string]any{
		"id": json.Number("44"), "name": "nested-agent", "description": nil,
		"author_id": json.Number("11"), "toolkit_name": "nested-agent",
		"agent_type": "agent", "created_at": "2026-08-07T10:00:00Z",
		"settings": map[string]any{
			"application_id": json.Number("3"), "application_version_id": json.Number("4"),
			"credential": "plaintext-must-not-cross-the-worker-boundary",
		},
		"meta": map[string]any{}, "variables": []any{}, "is_pinned": false,
		"author": nil, "online": nil, "icon_meta": nil, "indexes_count": nil,
	}
	if frozen, ok := freezeCurrentStoredApplicationReference(tool); ok || frozen != nil {
		t.Fatalf("frozen=%#v ok=%v", frozen, ok)
	}
}

func TestFreezeCurrentStoredApplicationReferencePreservesCompactNestedSkillRegistry(t *testing.T) {
	tool := map[string]any{
		"id": json.Number("44"), "name": "nested-agent", "description": nil,
		"author_id": json.Number("11"), "toolkit_name": "nested-agent",
		"agent_type": "agent", "created_at": "2026-08-07T10:00:00Z",
		"settings": map[string]any{
			"application_id": json.Number("3"), "application_version_id": json.Number("4"),
		},
		"meta": map[string]any{}, "variables": []any{}, "is_pinned": false,
		"author": nil, "online": nil, "icon_meta": nil, "indexes_count": nil,
		currentNestedSkillRegistryField: []any{map[string]any{
			"application_id": json.Number("3"), "application_version_id": json.Number("4"),
			"application_name": "nested-agent",
			"skills": []any{map[string]any{
				"skill_id": json.Number("7"), "name": "Deploy",
				"icon_meta": map[string]any{"icon": "deploy"},
			}},
		}},
	}
	frozen, ok := freezeCurrentStoredApplicationReference(tool)
	if !ok {
		t.Fatal("compact nested skill registry was rejected")
	}
	encoded, err := json.Marshal(frozen[currentNestedSkillRegistryField])
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `[{"application_id":3,"application_name":"nested-agent","application_version_id":4,"skills":[{"icon_meta":{"icon":"deploy"},"name":"Deploy","skill_id":7}]}]` ||
		strings.Contains(string(encoded), "instructions") {
		t.Fatalf("nested skill registry=%s", encoded)
	}

	registry := tool[currentNestedSkillRegistryField].([]any)
	registry[0].(map[string]any)["skills"].([]any)[0].(map[string]any)["skill_id"] = nil
	if rejected, valid := freezeCurrentStoredApplicationReference(tool); valid || rejected != nil {
		t.Fatalf("invalid registry was accepted: %#v", rejected)
	}
}

func TestCurrentApplicationToolSnapshotRejectsUnsupportedApplicationReferences(t *testing.T) {
	base := map[string]any{
		"type": "application", "name": "child", "description": "",
		"author_id": json.Number("11"), "participant_id": json.Number("29"),
		"project_id": json.Number("7"), "id": nil, "toolkit_name": "child",
		"agent_type": "openai", "created_at": "2026-08-04T10:00:00Z",
		"settings": map[string]any{
			"variables": []any{}, "application_id": json.Number("3"),
			"selected_tools": []any{}, "application_version_id": json.Number("4"),
		},
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "cross project", mutate: func(tool map[string]any) { tool["project_id"] = json.Number("8") }},
		{name: "wrong actor", mutate: func(tool map[string]any) { tool["author_id"] = json.Number("12") }},
		{name: "selected child tools", mutate: func(tool map[string]any) {
			tool["settings"].(map[string]any)["selected_tools"] = []any{"tool"}
		}},
		{name: "unexpected settings", mutate: func(tool map[string]any) {
			tool["settings"].(map[string]any)["credential"] = "plaintext"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			var tool map[string]any
			decoder := json.NewDecoder(bytes.NewReader(encoded))
			decoder.UseNumber()
			if err := decoder.Decode(&tool); err != nil {
				t.Fatal(err)
			}
			test.mutate(tool)
			version, err := json.Marshal(map[string]any{
				"llm_settings": map[string]any{"model_name": "model"},
				"tools":        []any{tool},
			})
			if err != nil {
				t.Fatal(err)
			}
			service, err := NewCurrentApplicationToolSnapshotService(
				&currentAgentSettingsResolverStub{}, &currentAgentNameResolverStub{},
				currentAgentModelCatalogForTest(false), &currentAgentGuardrailStub{}, &currentProjectContextStub{}, 1,
			)
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.FreezeCurrentApplicationVersion(
				context.Background(),
				CurrentApplicationVersionFreezeRequest{
					ProjectID: 7, ActorUserID: 11, VersionDetails: version,
				},
			)
			if !errors.Is(err, ErrUnsupportedCurrentAgentStart) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCurrentApplicationToolSnapshotValidatesConstruction(t *testing.T) {
	models := currentAgentModelCatalogForTest(false)
	rails := &currentAgentGuardrailStub{}
	if service, err := NewCurrentApplicationToolSnapshotService(nil, &currentAgentNameResolverStub{}, models, rails, &currentProjectContextStub{}, 1); err == nil || service != nil {
		t.Fatalf("service=%#v error=%v", service, err)
	}
	if service, err := NewCurrentApplicationToolSnapshotService(&currentAgentSettingsResolverStub{}, nil, models, rails, &currentProjectContextStub{}, 1); err == nil || service != nil {
		t.Fatalf("service=%#v error=%v", service, err)
	}
	if service, err := NewCurrentApplicationToolSnapshotService(&currentAgentSettingsResolverStub{}, &currentAgentNameResolverStub{}, nil, rails, &currentProjectContextStub{}, 1); err == nil || service != nil {
		t.Fatalf("service=%#v error=%v", service, err)
	}
	// The guardrail resolver is required, not optional. A service built without
	// one would enforce nothing and be indistinguishable from one whose operator
	// had configured nothing — see CurrentAgentGuardrailResolver.
	if service, err := NewCurrentApplicationToolSnapshotService(&currentAgentSettingsResolverStub{}, &currentAgentNameResolverStub{}, models, nil, &currentProjectContextStub{}, 1); err == nil || service != nil {
		t.Fatalf("service=%#v error=%v", service, err)
	}
	if service, err := NewCurrentApplicationToolSnapshotService(&currentAgentSettingsResolverStub{}, &currentAgentNameResolverStub{}, models, rails, &currentProjectContextStub{}, 0); err == nil || service != nil {
		t.Fatalf("service=%#v error=%v", service, err)
	}
}

func TestCurrentApplicationToolSnapshotPreservesProviderAutoMaxTokens(t *testing.T) {
	tests := []struct {
		name              string
		compatible        bool
		supportsReasoning bool
		wantMaxTokens     int64
	}{
		{name: "OpenAI-compatible model", compatible: true, wantMaxTokens: -1},
		{name: "native Anthropic model", supportsReasoning: true, wantMaxTokens: 32_000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			maxOutputTokens := 32_000
			models := &currentAgentModelCatalogStub{response: configurationapp.CurrentModelCatalogResponse{
				Items: []configurationapp.CurrentModelCatalogItem{{
					Name: "model", ProjectID: 7, OpenAICompatible: &test.compatible,
					SupportsReasoning: &test.supportsReasoning, MaxOutputTokens: &maxOutputTokens,
				}},
			}}
			service, err := NewCurrentApplicationToolSnapshotService(
				&currentAgentSettingsResolverStub{}, &currentAgentNameResolverStub{}, models,
				&currentAgentGuardrailStub{}, &currentProjectContextStub{}, 1,
			)
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.FreezeCurrentApplicationVersion(
				context.Background(), CurrentApplicationVersionFreezeRequest{
					ProjectID: 7, ActorUserID: 11,
					VersionDetails: json.RawMessage(`{"llm_settings":{"model_name":"model","max_tokens":-1},"tools":[]}`),
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			version, err := decodeCurrentApplicationVersion(result)
			if err != nil {
				t.Fatal(err)
			}
			settings := version["llm_settings"].(map[string]any)
			maxTokens, valid := currentAgentJSONInteger(settings["max_tokens"])
			if !valid || maxTokens != test.wantMaxTokens {
				t.Fatalf("max_tokens=%v, want %d", settings["max_tokens"], test.wantMaxTokens)
			}
		})
	}
}

// TestCurrentApplicationToolSnapshotAlwaysCarriesATemperature pins the key the
// SDK worker reads with a subscript.
//
// `elitea_sdk` 0.9.8's `runtime/clients/client.py` builds its model config with
// `"temperature": data['llm_settings']['temperature']` -- not `.get`. A frozen
// version that carries no such key therefore ends the turn in a bare
// `builtins.KeyError`, surfaced to the browser as an empty `is_error` row. The
// native Rust runtime reads the same document and does not need the key, so a
// stack running that worker cannot see this at all.
//
// The absent-key shape is not exotic: the agent editor's picker writes at most
// ONE of `temperature`/`reasoning_effort`, so it is what a reasoning-model
// version stores, and any API caller that sends `llm_settings` without one
// stores it too. Before this, the freeze normalized the family only when the
// model fell back to the catalogue default, or when a temperature was already
// present AND conflicted with a reasoning effort -- never for the shape that
// actually breaks.
func TestCurrentApplicationToolSnapshotAlwaysCarriesATemperature(t *testing.T) {
	tests := []struct {
		name              string
		supportsReasoning bool
		settings          string
		wantTemperature   any
	}{
		{
			name:            "a plain model with no temperature is given the platform default",
			settings:        `{"model_name":"model"}`,
			wantTemperature: json.Number("0.7"),
		},
		{
			// The reasoning family's answer is an explicit null, not 0.7: the
			// two are mutually exclusive on the wire, and `None` is what the
			// SDK's subscript must find.
			name:              "a reasoning model with no temperature is given an explicit null",
			supportsReasoning: true,
			settings:          `{"model_name":"model","reasoning_effort":"medium"}`,
			wantTemperature:   nil,
		},
		{
			// Unchanged: a version that already carries one keeps its own
			// value. This normalization fills a hole; it does not re-decide.
			name:            "a stored temperature is left exactly as authored",
			settings:        `{"model_name":"model","temperature":0.15}`,
			wantTemperature: json.Number("0.15"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compatible := true
			models := &currentAgentModelCatalogStub{response: configurationapp.CurrentModelCatalogResponse{
				Items: []configurationapp.CurrentModelCatalogItem{{
					Name: "model", ProjectID: 7, OpenAICompatible: &compatible,
					SupportsReasoning: &test.supportsReasoning,
				}},
			}}
			service, err := NewCurrentApplicationToolSnapshotService(
				&currentAgentSettingsResolverStub{}, &currentAgentNameResolverStub{}, models,
				&currentAgentGuardrailStub{}, &currentProjectContextStub{}, 1,
			)
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.FreezeCurrentApplicationVersion(
				context.Background(), CurrentApplicationVersionFreezeRequest{
					ProjectID: 7, ActorUserID: 11,
					VersionDetails: json.RawMessage(
						`{"llm_settings":` + test.settings + `,"tools":[]}`,
					),
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			version, err := decodeCurrentApplicationVersion(result)
			if err != nil {
				t.Fatal(err)
			}
			settings := version["llm_settings"].(map[string]any)
			temperature, present := settings["temperature"]
			if !present {
				t.Fatalf("llm_settings carries no temperature key at all: %v", settings)
			}
			if temperature != test.wantTemperature {
				t.Fatalf("temperature=%#v, want %#v", temperature, test.wantTemperature)
			}
		})
	}
}

// currentAgentGuardrailStub is the freeze's guardrail dependency in tests.
//
// The zero value carries an empty policy, so every pre-existing case in this
// file continues to assert what it asserted before guardrails existed: nothing
// is blocked, and the frozen output is unchanged. Cases that DO exercise a
// policy set `policy`, and `err` covers the read-failure path.
type currentAgentGuardrailStub struct {
	policy guardrails.Policy
	err    error
}

func (stub *currentAgentGuardrailStub) ResolveCurrentAgentGuardrails(
	context.Context,
) (guardrails.Policy, error) {
	if stub.err != nil {
		return guardrails.Policy{}, stub.err
	}
	return stub.policy, nil
}
