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

type recordingInternalProjectContextExecutor struct {
	projectID int64
	actorID   int64
	operation internalProjectContextOperation
	arguments map[string]any
	result    internalApplicationExecution
	err       error
	calls     int
}

func (executor *recordingInternalProjectContextExecutor) Execute(
	_ context.Context,
	projectID int64,
	actorID int64,
	operation internalProjectContextOperation,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	executor.calls++
	executor.projectID = projectID
	executor.actorID = actorID
	executor.operation = operation
	executor.arguments = arguments
	return executor.result, executor.err
}

func internalProjectContextRouter(
	t *testing.T,
	tool Tool,
	executor internalProjectContextExecutor,
	permissions auth.PermissionResolver,
) chi.Router {
	t.Helper()
	handler := NewHandler(nil, nil, nil, permissions)
	handler.source = staticSource(tool)
	handler.internalProjectContext = executor
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

func internalProjectContextTool(operation internalProjectContextOperation, permission string) Tool {
	return Tool{
		Name:                            "internal_project_context_test",
		Description:                     "test",
		InputSchema:                     objectSchema(map[string]any{}),
		internalProjectContextOperation: operation,
		permission:                      permission,
	}
}

func TestInternalProjectContextCategoryPublishesExactlyCurrentBuilderOperations(t *testing.T) {
	tools, err := (postgresToolSource{}).tools(
		context.Background(), `"p_7"`, scope{kind: scopeCategory, category: internalProjectContextCategory},
	)
	if err != nil {
		t.Fatalf("list internal project-context tools: %v", err)
	}
	wantNames := []string{
		"get_prompt_lib_project-context",
		"put_prompt_lib_project-context",
		"delete_prompt_lib_project-context",
		"post_prompt_lib_generate_project_context_draft",
	}
	wantPermissions := []string{
		"models.project_context.view",
		"models.project_context.edit",
		"models.project_context.edit",
		"models.project_context.edit",
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

	updateProperties := tools[1].InputSchema["properties"].(map[string]any)
	for _, field := range []string{"project_id", "content", "enabled", "activation_description"} {
		if _, advertised := updateProperties[field]; !advertised {
			t.Fatalf("update schema does not advertise %q", field)
		}
	}
	activation := updateProperties["activation_description"].(map[string]any)
	variants := activation["anyOf"].([]any)
	if len(variants) != 2 || variants[0].(map[string]any)["maxLength"] != 300 ||
		variants[1].(map[string]any)["type"] != "null" {
		t.Fatalf("activation_description schema = %#v, want bounded string or null", activation)
	}
	wire, err := json.Marshal(tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	for _, forbidden := range []string{
		"internalProjectContextOperation", "permission",
	} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("wire contains private or unsupported operation %q: %s", forbidden, wire)
		}
	}
}

func TestInternalProjectContextCallUsesExactPermissionActorAndEndpointProject(t *testing.T) {
	const permission = "models.project_context.view"
	executor := &recordingInternalProjectContextExecutor{
		result: internalApplicationExecution{status: http.StatusOK, body: []byte(`{"enabled":true}`)},
	}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalProjectContextRouter(
		t, internalProjectContextTool(internalGetProjectContext, permission), executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/project_context",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_project_context_test","arguments":{}}}`))

	if executor.calls != 1 || executor.projectID != 7 || executor.actorID != 41 ||
		executor.operation != internalGetProjectContext {
		t.Fatalf("executor = calls:%d project:%d actor:%d operation:%q",
			executor.calls, executor.projectID, executor.actorID, executor.operation)
	}
	if got := scalarArgument(executor.arguments["project_id"]); got != "7" {
		t.Fatalf("injected project_id = %q, want 7", got)
	}
	if permissions.mode != auth.PermissionModeDefault || permissions.projectID != "7" {
		t.Fatalf("permission request = mode:%q project:%q", permissions.mode, permissions.projectID)
	}
	if text := textOf(t, result); text != `{"enabled":true}` {
		t.Fatalf("result text = %q", text)
	}
}

func TestInternalProjectContextCallRefusesForeignProjectBeforeAuthorization(t *testing.T) {
	const permission = "models.project_context.edit"
	executor := &recordingInternalProjectContextExecutor{}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalProjectContextRouter(
		t, internalProjectContextTool(internalUpdateProjectContext, permission), executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/project_context",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_project_context_test","arguments":{"project_id":8}}}`))

	if result["isError"] != true || executor.calls != 0 || permissions.calls != 0 {
		t.Fatalf("result=%v executor calls=%d permission calls=%d", result, executor.calls, permissions.calls)
	}
}

func TestInternalProjectContextInfrastructureFailureIsRedacted(t *testing.T) {
	const permission = "models.project_context.edit"
	executor := &recordingInternalProjectContextExecutor{err: errors.New("database password=do-not-leak")}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalProjectContextRouter(
		t, internalProjectContextTool(internalDeleteProjectContext, permission), executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/project_context",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_project_context_test","arguments":{}}}`))

	text := textOf(t, result)
	if result["isError"] != true || strings.Contains(text, "password") || strings.Contains(text, "do-not-leak") {
		t.Fatalf("unredacted failure result: %v", result)
	}
}
