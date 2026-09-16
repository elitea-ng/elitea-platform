package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	configapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	configapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type typedConfigurationFixture struct {
	types  []configapp.CurrentConfigurationTypesQuery
	models []configapp.CurrentModelCatalogQuery
	writes []configapp.CurrentModelDefaultSelection
	err    error
}

func (f *typedConfigurationFixture) List(_ context.Context, query configapp.CurrentConfigurationTypesQuery) (configapp.CurrentConfigurationTypesResult, error) {
	f.types = append(f.types, query)
	return configapp.CurrentConfigurationTypesResult{Rows: []string{"openapi", "sharepoint"}, Total: 2}, f.err
}

func (f *typedConfigurationFixture) Get(_ context.Context, query configapp.CurrentModelCatalogQuery) (configapp.CurrentModelCatalogResponse, error) {
	f.models = append(f.models, query)
	name, project := "selected-model", int32(7)
	return configapp.CurrentModelCatalogResponse{
		Items:            []configapp.CurrentModelCatalogItem{},
		DefaultModelName: &name, DefaultModelProjectID: &project,
	}, f.err
}

func (f *typedConfigurationFixture) SetCurrentModelDefault(_ context.Context, selection configapp.CurrentModelDefaultSelection) error {
	f.writes = append(f.writes, selection)
	return f.err
}

func typedConfigurationExecutor(f *typedConfigurationFixture) internalConfigurationExecutor {
	return newHandlerInternalConfigurationExecutor(configapi.NewHandler(nil),
		configapi.NewCurrentConfigurationToolHandler(f, f, f, 1))
}

func TestInternalTypedConfigurationCatalogueRequiresComposition(t *testing.T) {
	f := &typedConfigurationFixture{}
	typed := configapi.NewCurrentConfigurationToolHandler(f, f, f, 1)
	h := NewHandler(nil, nil, nil, nil, WithInternalConfigurationHandler(configapi.NewHandler(nil), typed))
	tools, err := (postgresToolSource{handler: h}).tools(context.Background(), `"p_7"`, scope{kind: scopeCategory, category: internalConfigurationsCategory})
	if err != nil || len(tools) != 8 {
		t.Fatalf("composed catalogue: %d tools, %v", len(tools), err)
	}
	want := []string{"get_configurations_types", "get_configurations_models", "post_configurations_models"}
	for index, name := range want {
		tool := tools[index+5]
		permission := configapi.CurrentConfigurationListPermission
		if index == 2 {
			permission = configapi.CurrentModelDefaultPermission
		}
		if tool.Name != name || tool.permission != permission {
			t.Fatalf("unexpected typed tool: %s/%s", tool.Name, tool.permission)
		}
	}
	for _, input := range []struct {
		types    configapi.CurrentConfigurationTypesReader
		models   configapi.CurrentModelCatalogReader
		defaults configapi.CurrentModelDefaultWriter
		public   int32
	}{{nil, f, f, 1}, {f, nil, f, 1}, {f, f, nil, 1}, {f, f, f, 0}} {
		if configapi.NewCurrentConfigurationToolHandler(input.types, input.models, input.defaults, input.public) != nil {
			t.Fatal("incomplete typed services were accepted")
		}
	}
	h = NewHandler(nil, nil, nil, nil, WithInternalConfigurationHandler(configapi.NewHandler(nil), nil))
	tools, err = (postgresToolSource{handler: h}).tools(context.Background(), `"p_7"`, scope{kind: scopeCategory, category: internalConfigurationsCategory})
	if err != nil || len(tools) != 5 {
		t.Fatalf("unavailable catalogue: %d tools, %v", len(tools), err)
	}
	h = NewHandler(nil, nil, nil, nil, WithInternalConfigurationHandler(nil, typed))
	tools, err = (postgresToolSource{handler: h}).tools(context.Background(), `"p_7"`, scope{kind: scopeCategory, category: internalConfigurationsCategory})
	if err != nil || len(tools) != 5 {
		t.Fatalf("unavailable executor catalogue: %d tools, %v", len(tools), err)
	}
}

func TestInternalTypedConfigurationTypesPreserveSectionSemantics(t *testing.T) {
	for _, test := range []struct {
		name    string
		args    map[string]any
		section string
	}{
		{"default", map[string]any{}, "credentials"},
		{"all", map[string]any{"section": ""}, ""},
		{"selected", map[string]any{"section": "llm", "project_id": 999, "author_id": 999}, "llm"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &typedConfigurationFixture{}
			result, err := typedConfigurationExecutor(f).Execute(context.Background(), 7, 41, internalListStoredConfigurationTypes, test.args)
			if err != nil || result.status != 200 || string(result.body) != `{"rows":["openapi","sharepoint"],"total":2}` {
				t.Fatalf("result: %s, %v", result.body, err)
			}
			if len(f.types) != 1 || f.types[0] != (configapp.CurrentConfigurationTypesQuery{ProjectID: 7, Section: test.section}) {
				t.Fatalf("query: %#v", f.types)
			}
		})
	}
}

func TestInternalTypedConfigurationModelsUsePublicScopeAndDefaults(t *testing.T) {
	for _, shared := range []bool{false, true} {
		f := &typedConfigurationFixture{}
		result, err := typedConfigurationExecutor(f).Execute(context.Background(), 7, 41, internalListConfigurationModels, map[string]any{"section": "EMBEDDING", "include_shared": shared, "public_project_id": 999})
		if err != nil || result.status != 200 {
			t.Fatalf("result: %s, %v", result.body, err)
		}
		if len(f.models) != 1 || f.models[0] != (configapp.CurrentModelCatalogQuery{ProjectID: 7, PublicProjectID: 1, Section: configapp.CurrentModelSectionEmbedding, IncludeShared: shared}) {
			t.Fatalf("query: %#v", f.models)
		}
		var value map[string]any
		if json.Unmarshal(result.body, &value) != nil || value["default_model_name"] != "selected-model" || value["default_model_project_id"] != float64(7) {
			t.Fatalf("defaults: %s", result.body)
		}
	}
	f := &typedConfigurationFixture{}
	result, err := typedConfigurationExecutor(f).Execute(context.Background(), 7, 41, internalListConfigurationModels, map[string]any{"section": "not-a-section"})
	if err != nil || result.status != 200 || len(f.models) != 0 || !strings.Contains(string(result.body), `"items":[]`) {
		t.Fatalf("unknown section: %s, %v", result.body, err)
	}
	result, err = typedConfigurationExecutor(f).Execute(context.Background(), 7, 41, internalListConfigurationModels, map[string]any{})
	if err != nil || result.status != 200 || len(f.models) != 1 || f.models[0].Section != configapp.CurrentModelSectionLLM || f.models[0].IncludeShared {
		t.Fatalf("defaults: %#v, %v", f.models, err)
	}
}

func TestInternalTypedConfigurationDefaultWritesOnlyEndpointProject(t *testing.T) {
	for _, test := range []struct {
		args    map[string]any
		section string
	}{
		{map[string]any{}, "llm"}, {map[string]any{"section": "embedding"}, "embedding"},
		{map[string]any{"section": nil}, "None"}, {map[string]any{"section": ""}, ""},
	} {
		f := &typedConfigurationFixture{}
		test.args["name"] = "selected-model"
		test.args["target_project_id"] = json.Number("1")
		test.args["project_id"] = 999
		test.args["author_id"] = 999
		result, err := typedConfigurationExecutor(f).Execute(context.Background(), 7, 41, internalSetDefaultConfigurationModel, test.args)
		if err != nil || result.status != 200 || string(result.body) != `{"result":"success"}` {
			t.Fatalf("result: %s, %v", result.body, err)
		}
		want := configapp.CurrentModelDefaultSelection{ProjectID: 7, TargetProjectID: 1, Name: "selected-model", Section: test.section}
		if len(f.writes) != 1 || f.writes[0] != want {
			t.Fatalf("write: %#v", f.writes)
		}
	}
}

func TestInternalTypedConfigurationFailuresAreBoundedAndRedacted(t *testing.T) {
	for _, operation := range []internalConfigurationOperation{internalListStoredConfigurationTypes, internalListConfigurationModels, internalSetDefaultConfigurationModel} {
		f := &typedConfigurationFixture{err: errors.New("credential-secret-canary")}
		args := map[string]any{"name": "model", "target_project_id": json.Number("1")}
		result, err := typedConfigurationExecutor(f).Execute(context.Background(), 7, 41, operation, args)
		if err != nil || result.status < 400 || strings.Contains(string(result.body), "canary") {
			t.Fatalf("failure: %s, %v", result.body, err)
		}
		f = &typedConfigurationFixture{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = typedConfigurationExecutor(f).Execute(ctx, 7, 41, operation, args)
		if !errors.Is(err, context.Canceled) || len(f.types)+len(f.models)+len(f.writes) != 0 {
			t.Fatalf("cancellation: %v", err)
		}
		result, err = newHandlerInternalConfigurationExecutor(configapi.NewHandler(nil), nil).Execute(context.Background(), 7, 41, operation, args)
		if err != nil || result.status != 503 {
			t.Fatalf("unavailable: %s, %v", result.body, err)
		}
	}
	for _, test := range []struct {
		op   internalConfigurationOperation
		args map[string]any
	}{
		{internalListStoredConfigurationTypes, map[string]any{"section": nil}},
		{internalListStoredConfigurationTypes, map[string]any{"section": strings.Repeat("x", 129)}},
		{internalListConfigurationModels, map[string]any{"include_shared": "true"}},
		{internalSetDefaultConfigurationModel, map[string]any{"name": "model", "target_project_id": 0}},
		{internalSetDefaultConfigurationModel, map[string]any{"name": strings.Repeat("x", 1025), "target_project_id": 1}},
	} {
		f := &typedConfigurationFixture{}
		result, err := typedConfigurationExecutor(f).Execute(context.Background(), 7, 41, test.op, test.args)
		if err != nil || result.status != 400 || len(f.types)+len(f.models)+len(f.writes) != 0 {
			t.Fatalf("invalid input: %s, %v", result.body, err)
		}
	}
}

func TestInternalTypedConfigurationMCPEnforcesEachPermissionAndProject(t *testing.T) {
	for _, tool := range internalConfigurationTools(true)[5:] {
		for _, scenario := range []string{"allowed", "denied", "foreign-project"} {
			t.Run(tool.Name+"/"+scenario, func(t *testing.T) {
				f := &typedConfigurationFixture{}
				permissions := &recordingInternalPermissionResolver{resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{tool.permission}}}
				if scenario == "denied" {
					permissions.resolution.Permissions = nil
				}
				router := internalConfigurationRouter(t, tool, typedConfigurationExecutor(f), permissions)
				args := map[string]any{"project_id": 7, "name": "model", "target_project_id": 1}
				if scenario == "foreign-project" {
					args["project_id"] = 8
				}
				body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool.Name, "arguments": args}})
				if err != nil {
					t.Fatal(err)
				}
				response := post(t, router, "/app/7/mcp/configurations", string(body))
				calls := len(f.types) + len(f.models) + len(f.writes)
				if scenario == "allowed" {
					if calls != 1 || response.Code != http.StatusOK {
						t.Fatalf("allowed: %d calls, %s", calls, response.Body.String())
					}
				} else if calls != 0 {
					t.Fatalf("denied request reached service: %s", scenario)
				}
			})
		}
	}
}

func FuzzInternalTypedConfigurationSection(f *testing.F) {
	for _, value := range []string{"", "credentials", "llm", strings.Repeat("x", 129), "🦀"} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, section string) {
		fixture := &typedConfigurationFixture{}
		result, err := typedConfigurationExecutor(fixture).Execute(context.Background(), 7, 41, internalListStoredConfigurationTypes, map[string]any{"section": section})
		if err != nil {
			t.Fatal(err)
		}
		if result.status == 200 && (len(fixture.types) != 1 || fixture.types[0].ProjectID != 7 || fixture.types[0].Section != section) {
			t.Fatal("section or project changed")
		}
		if result.status >= 400 && len(fixture.types) != 0 {
			t.Fatal("invalid input reached reader")
		}
	})
}
