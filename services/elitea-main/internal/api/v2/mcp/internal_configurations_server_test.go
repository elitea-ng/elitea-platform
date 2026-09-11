package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type recordingInternalConfigurationExecutor struct {
	projectID int64
	actorID   int64
	operation internalConfigurationOperation
	arguments map[string]any
	result    internalApplicationExecution
	err       error
	calls     int
}

func (executor *recordingInternalConfigurationExecutor) Execute(
	_ context.Context,
	projectID int64,
	actorID int64,
	operation internalConfigurationOperation,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	executor.calls++
	executor.projectID = projectID
	executor.actorID = actorID
	executor.operation = operation
	executor.arguments = arguments
	return executor.result, executor.err
}

func internalConfigurationRouter(
	t *testing.T,
	tool Tool,
	executor internalConfigurationExecutor,
	permissions auth.PermissionResolver,
) chi.Router {
	t.Helper()
	handler := NewHandler(nil, nil, nil, permissions)
	handler.source = staticSource(tool)
	handler.internalConfigurations = executor
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			principal := auth.User{ID: "41", UserID: "41"}
			ctx := auth.ContextWithUser(request.Context(), principal)
			next.ServeHTTP(writer, request.WithContext(ctx))
		})
	})
	router.Post("/app/{projectID}/mcp/*", handler.Endpoint)
	return router
}

func internalConfigurationTool(operation internalConfigurationOperation, permission string) Tool {
	return Tool{
		Name:                           "internal_configuration_test",
		Description:                    "test",
		InputSchema:                    objectSchema(map[string]any{}),
		internalConfigurationOperation: operation,
		permission:                     permission,
	}
}

func TestInternalConfigurationsCategoryPublishesOnlyTruthfulMainOwnedOperations(t *testing.T) {
	tools, err := (postgresToolSource{}).tools(
		context.Background(), `"p_7"`, scope{kind: scopeCategory, category: internalConfigurationsCategory},
	)
	if err != nil {
		t.Fatalf("list internal configuration tools: %v", err)
	}
	wantNames := []string{
		"get_configurations_available",
		"get_configurations_configurations",
		"post_configurations_configurations",
		"get_configurations_configuration",
		"put_configurations_configuration",
	}
	wantPermissions := []string{
		"configurations.configurations.list",
		"configurations.configurations.list",
		"configurations.configuration.create",
		"configurations.configuration.details",
		"configurations.configuration.update",
	}
	if len(tools) != len(wantNames) {
		t.Fatalf("tool count = %d, want %d", len(tools), len(wantNames))
	}
	for index, want := range wantNames {
		if tools[index].Name != want || tools[index].permission != wantPermissions[index] {
			t.Fatalf("tool %d = %q/%q, want %q/%q",
				index, tools[index].Name, tools[index].permission, want, wantPermissions[index])
		}
	}

	listProperties := tools[1].InputSchema["properties"].(map[string]any)
	if ids, ok := listProperties["ids"].(map[string]any); !ok || ids["type"] != "string" || ids["maxLength"] != 1200 {
		t.Fatal("list schema must advertise bounded comma-separated IDs")
	}
	updateProperties := tools[4].InputSchema["properties"].(map[string]any)
	for _, forbidden := range []string{"type", "section", "project_id_override", "author_id", "source", "status_ok"} {
		if _, advertised := updateProperties[forbidden]; advertised && forbidden != "project_id" {
			t.Fatalf("update schema advertises forbidden field %q", forbidden)
		}
	}
	wire, err := json.Marshal(tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	for _, forbidden := range []string{
		"internalConfigurationOperation", "permission", "get_configurations_types",
		"get_configurations_models", "post_configurations_models", "delete",
	} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("wire contains private or unsupported operation %q: %s", forbidden, wire)
		}
	}
}

func TestInternalConfigurationCallUsesExactPermissionAndEndpointProject(t *testing.T) {
	const permission = "configurations.configurations.list"
	executor := &recordingInternalConfigurationExecutor{
		result: internalApplicationExecution{status: http.StatusOK, body: []byte(`{"items":[],"total":0}`)},
	}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalConfigurationRouter(
		t, internalConfigurationTool(internalListConfigurations, permission), executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/configurations",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_configuration_test","arguments":{}}}`))

	if executor.calls != 1 || executor.projectID != 7 || executor.actorID != 41 {
		t.Fatalf("executor = calls:%d project:%d actor:%d", executor.calls, executor.projectID, executor.actorID)
	}
	if got := scalarArgument(executor.arguments["project_id"]); got != "7" {
		t.Fatalf("injected project_id = %q, want 7", got)
	}
	if permissions.mode != auth.PermissionModeDefault || permissions.projectID != "7" {
		t.Fatalf("permission request = mode:%q project:%q", permissions.mode, permissions.projectID)
	}
	if text := textOf(t, result); text != `{"items":[],"total":0}` {
		t.Fatalf("result text = %q", text)
	}
}

func TestInternalConfigurationCallRefusesForeignProjectBeforeAuthorization(t *testing.T) {
	const permission = "configurations.configuration.update"
	executor := &recordingInternalConfigurationExecutor{}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalConfigurationRouter(
		t, internalConfigurationTool(internalUpdateConfiguration, permission), executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/configurations",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_configuration_test","arguments":{"project_id":8}}}`))

	if result["isError"] != true || executor.calls != 0 || permissions.calls != 0 {
		t.Fatalf("result=%v executor calls=%d permission calls=%d", result, executor.calls, permissions.calls)
	}
}

func TestInternalConfigurationInfrastructureFailureIsRedacted(t *testing.T) {
	const permission = "configurations.configuration.details"
	executor := &recordingInternalConfigurationExecutor{err: errors.New("vault password=do-not-leak")}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalConfigurationRouter(
		t, internalConfigurationTool(internalGetConfiguration, permission), executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/configurations",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_configuration_test","arguments":{}}}`))

	text := textOf(t, result)
	if result["isError"] != true || strings.Contains(text, "password") || strings.Contains(text, "do-not-leak") {
		t.Fatalf("unredacted failure result: %v", result)
	}
}
