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

type recordingInternalApplicationExecutor struct {
	projectID int64
	actorID   int64
	operation internalApplicationOperation
	arguments map[string]any
	result    internalApplicationExecution
	err       error
	calls     int
}

func (executor *recordingInternalApplicationExecutor) Execute(
	_ context.Context,
	projectID int64,
	actorID int64,
	operation internalApplicationOperation,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	executor.calls++
	executor.projectID = projectID
	executor.actorID = actorID
	executor.operation = operation
	executor.arguments = arguments
	return executor.result, executor.err
}

type recordingInternalPermissionResolver struct {
	mode       string
	projectID  string
	resolution auth.PermissionResolution
	err        error
	calls      int
}

func (resolver *recordingInternalPermissionResolver) ResolvePermissions(
	_ context.Context,
	_ auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	resolver.calls++
	resolver.mode = mode
	resolver.projectID = projectID
	return resolver.resolution, resolver.err
}

func internalApplicationRouter(
	t *testing.T,
	tool Tool,
	executor internalApplicationExecutor,
	permissions auth.PermissionResolver,
) chi.Router {
	t.Helper()
	handler := NewHandler(nil, nil, nil, permissions)
	handler.source = staticSource(tool)
	handler.internalApplications = executor
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

func internalApplicationTool(operation internalApplicationOperation, permission string) Tool {
	return Tool{
		Name:                         "internal_test",
		Description:                  "test",
		InputSchema:                  objectSchema(map[string]any{}),
		internalApplicationOperation: operation,
		permission:                   permission,
	}
}

func TestInternalApplicationsCategoryPublishesExactlyTheCurrentEightOperations(t *testing.T) {
	tools, err := (postgresToolSource{}).tools(
		context.Background(), `"p_7"`, scope{kind: scopeCategory, category: internalApplicationsCategory},
	)
	if err != nil {
		t.Fatalf("list internal applications tools: %v", err)
	}
	if len(tools) != 8 {
		t.Fatalf("tool count = %d, want 8", len(tools))
	}
	wantNames := []string{
		"get_elitea_core_applications",
		"post_elitea_core_applications",
		"get_elitea_core_application",
		"post_elitea_core_versions",
		"get_elitea_core_version",
		"put_elitea_core_version",
		"post_elitea_core_version_instruction_patch",
		"patch_elitea_core_application_relation",
	}
	for index, want := range wantNames {
		if tools[index].Name != want {
			t.Fatalf("tool %d = %q, want %q", index, tools[index].Name, want)
		}
	}
	listProperties := tools[0].InputSchema["properties"].(map[string]any)
	copyProperties := tools[3].InputSchema["properties"].(map[string]any)
	copySource, ok := copyProperties["copy_skills_from_version_id"].(map[string]any)
	if !ok || copySource["type"] != "integer" {
		t.Fatal("create-version schema omits the optional skill source")
	}
	for _, toolIndex := range []int{1, 5} {
		properties := tools[toolIndex].InputSchema["properties"].(map[string]any)
		if _, exists := properties["copy_skills_from_version_id"]; exists {
			t.Fatal("skill copy was advertised outside create-version")
		}
	}
	for _, unsupported := range []string{"tags", "folder_id", "author_id", "statuses", "my_liked", "ids"} {
		if _, advertised := listProperties[unsupported]; advertised {
			t.Fatalf("list schema advertises unsupported filter %q", unsupported)
		}
	}
	wire, err := json.Marshal(tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	for _, forbidden := range []string{"internalApplicationOperation", "permission", "delete", "publish"} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("wire contains private or unselected operation %q: %s", forbidden, wire)
		}
	}
}

func TestInternalApplicationCallUsesItsExactPermissionAndDoesNotNeedAgentRuntime(t *testing.T) {
	const permission = "models.applications.version.details"
	executor := &recordingInternalApplicationExecutor{
		result: internalApplicationExecution{status: http.StatusOK, body: []byte(`{"ok":true}`)},
	}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalApplicationRouter(
		t, internalApplicationTool(internalGetVersion, permission), executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/applications",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_test","arguments":{}}}`))

	if executor.calls != 1 || executor.projectID != 7 || executor.actorID != 41 {
		t.Fatalf("executor = calls:%d project:%d actor:%d", executor.calls, executor.projectID, executor.actorID)
	}
	if got := scalarArgument(executor.arguments["project_id"]); got != "7" {
		t.Fatalf("injected project_id = %q, want 7", got)
	}
	if permissions.mode != auth.PermissionModeDefault || permissions.projectID != "7" {
		t.Fatalf("permission request = mode:%q project:%q", permissions.mode, permissions.projectID)
	}
	if text := textOf(t, result); text != `{"ok":true}` {
		t.Fatalf("result text = %q", text)
	}
}

func TestInternalApplicationCallRefusesForeignProjectBeforeExecution(t *testing.T) {
	const permission = "models.applications.applications.list"
	executor := &recordingInternalApplicationExecutor{}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalApplicationRouter(
		t, internalApplicationTool(internalListApplications, permission), executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/applications",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_test","arguments":{"project_id":8}}}`))

	if result["isError"] != true || executor.calls != 0 || permissions.calls != 0 {
		t.Fatalf("result=%v executor calls=%d permission calls=%d", result, executor.calls, permissions.calls)
	}
}

func TestInternalApplicationCallFailsClosedOnMissingPermission(t *testing.T) {
	executor := &recordingInternalApplicationExecutor{}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{"some.other.permission"}},
	}
	router := internalApplicationRouter(
		t, internalApplicationTool(internalCreateApplication, "models.applications.applications.create"),
		executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/applications",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_test","arguments":{}}}`))

	if result["isError"] != true || executor.calls != 0 {
		t.Fatalf("result=%v executor calls=%d", result, executor.calls)
	}
}

func TestInternalApplicationInfrastructureFailureIsRedacted(t *testing.T) {
	const permission = "models.applications.application.details"
	executor := &recordingInternalApplicationExecutor{err: errors.New("postgres password=do-not-leak")}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalApplicationRouter(
		t, internalApplicationTool(internalGetApplication, permission), executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/applications",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_test","arguments":{}}}`))

	text := textOf(t, result)
	if result["isError"] != true || strings.Contains(text, "password") || strings.Contains(text, "do-not-leak") {
		t.Fatalf("unredacted failure result: %v", result)
	}
}

func TestInternalApplicationHTTPServerFailureIsRedacted(t *testing.T) {
	const permission = "models.applications.application.details"
	executor := &recordingInternalApplicationExecutor{result: internalApplicationExecution{
		status: http.StatusInternalServerError,
		body:   []byte(`{"error":"connection failed for password=do-not-leak"}`),
	}}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalApplicationRouter(
		t, internalApplicationTool(internalGetApplication, permission), executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/applications",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_test","arguments":{}}}`))

	text := textOf(t, result)
	if result["isError"] != true || strings.Contains(text, "password") || strings.Contains(text, "do-not-leak") {
		t.Fatalf("unredacted HTTP failure result: %v", result)
	}
}

func TestInternalApplicationBusinessFailureRemainsAToolResult(t *testing.T) {
	const permission = "models.applications.version.update"
	executor := &recordingInternalApplicationExecutor{
		result: internalApplicationExecution{
			status: http.StatusConflict,
			body:   []byte(`{"error":"Instructions changed after they were read."}`),
		},
	}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalApplicationRouter(
		t, internalApplicationTool(internalPatchInstructions, permission), executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/applications",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_test","arguments":{}}}`))

	if result["isError"] != true || !strings.Contains(textOf(t, result), "Instructions changed") {
		t.Fatalf("result = %v", result)
	}
}
