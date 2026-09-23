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

type recordingInternalToolkitExecutor struct {
	projectID int64
	actorID   int64
	operation internalToolkitOperation
	arguments map[string]any
	result    internalApplicationExecution
	err       error
	calls     int
}

func (executor *recordingInternalToolkitExecutor) Execute(
	_ context.Context,
	projectID int64,
	actorID int64,
	operation internalToolkitOperation,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	executor.calls++
	executor.projectID = projectID
	executor.actorID = actorID
	executor.operation = operation
	executor.arguments = arguments
	return executor.result, executor.err
}

func internalToolkitRouter(
	t *testing.T,
	tool Tool,
	executor internalToolkitExecutor,
	permissions auth.PermissionResolver,
) chi.Router {
	t.Helper()
	handler := NewHandler(nil, nil, nil, permissions)
	handler.source = staticSource(tool)
	handler.internalToolkits = executor
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

func internalToolkitTool(operation internalToolkitOperation, permission string) Tool {
	return Tool{
		Name:                     "internal_toolkit_test",
		Description:              "test",
		InputSchema:              objectSchema(map[string]any{}),
		internalToolkitOperation: operation,
		permission:               permission,
	}
}

func TestInternalToolkitsCategoryPublishesOnlyTruthfulMainOwnedOperations(t *testing.T) {
	tools, err := (postgresToolSource{}).tools(
		context.Background(), `"p_7"`, scope{kind: scopeCategory, category: internalToolkitsCategory},
	)
	if err != nil {
		t.Fatalf("list internal toolkit tools: %v", err)
	}
	wantNames := []string{
		"get_elitea_core_toolkits",
		"get_elitea_core_tools",
		"post_elitea_core_tools",
		"put_elitea_core_tool",
		"patch_elitea_core_tool",
	}
	wantPermissions := []string{
		"models.applications.toolkits.details",
		"models.applications.tools.list",
		"models.applications.tools.create",
		"models.applications.tool.update",
		"models.applications.tool.patch",
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
	for _, unsupported := range []string{
		"query", "sort_by", "sort_order", "toolkit_type", "mcp", "application", "author_id", "ids",
	} {
		if _, advertised := listProperties[unsupported]; advertised {
			t.Fatalf("list schema advertises unsupported filter %q", unsupported)
		}
	}
	wire, err := json.Marshal(tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	for _, forbidden := range []string{
		"internalToolkitOperation", "permission", "get_elitea_core_toolkit_available_tools", "delete",
	} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("wire contains private or unsupported operation %q: %s", forbidden, wire)
		}
	}
}

func TestInternalToolkitCallUsesExactPermissionAndEndpointProject(t *testing.T) {
	const permission = "models.applications.tools.list"
	executor := &recordingInternalToolkitExecutor{
		result: internalApplicationExecution{status: http.StatusOK, body: []byte(`{"rows":[],"total":0}`)},
	}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalToolkitRouter(
		t, internalToolkitTool(internalListToolkits, permission), executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/toolkits",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_toolkit_test","arguments":{}}}`))

	if executor.calls != 1 || executor.projectID != 7 || executor.actorID != 41 {
		t.Fatalf("executor = calls:%d project:%d actor:%d", executor.calls, executor.projectID, executor.actorID)
	}
	if got := scalarArgument(executor.arguments["project_id"]); got != "7" {
		t.Fatalf("injected project_id = %q, want 7", got)
	}
	if permissions.mode != auth.PermissionModeDefault || permissions.projectID != "7" {
		t.Fatalf("permission request = mode:%q project:%q", permissions.mode, permissions.projectID)
	}
	if text := textOf(t, result); text != `{"rows":[],"total":0}` {
		t.Fatalf("result text = %q", text)
	}
}

func TestInternalToolkitCallRefusesForeignProjectBeforeAuthorization(t *testing.T) {
	const permission = "models.applications.tools.list"
	executor := &recordingInternalToolkitExecutor{}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalToolkitRouter(
		t, internalToolkitTool(internalListToolkits, permission), executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/toolkits",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_toolkit_test","arguments":{"project_id":8}}}`))

	if result["isError"] != true || executor.calls != 0 || permissions.calls != 0 {
		t.Fatalf("result=%v executor calls=%d permission calls=%d", result, executor.calls, permissions.calls)
	}
}

func TestInternalToolkitInfrastructureFailureIsRedacted(t *testing.T) {
	const permission = "models.applications.tool.update"
	executor := &recordingInternalToolkitExecutor{err: errors.New("postgres password=do-not-leak")}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalToolkitRouter(
		t, internalToolkitTool(internalUpdateToolkit, permission), executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/toolkits",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_toolkit_test","arguments":{}}}`))

	text := textOf(t, result)
	if result["isError"] != true || strings.Contains(text, "password") || strings.Contains(text, "do-not-leak") {
		t.Fatalf("unredacted failure result: %v", result)
	}
}
