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

type recordingInternalNotificationExecutor struct {
	projectID int64
	actorID   int64
	operation internalNotificationOperation
	arguments map[string]any
	result    internalApplicationExecution
	err       error
	calls     int
}

func (executor *recordingInternalNotificationExecutor) Execute(
	_ context.Context,
	projectID int64,
	actorID int64,
	operation internalNotificationOperation,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	executor.calls++
	executor.projectID = projectID
	executor.actorID = actorID
	executor.operation = operation
	executor.arguments = arguments
	return executor.result, executor.err
}

func internalNotificationRouter(
	t *testing.T,
	tool Tool,
	executor internalNotificationExecutor,
	permissions auth.PermissionResolver,
) chi.Router {
	t.Helper()
	handler := NewHandler(nil, nil, nil, permissions)
	handler.source = staticSource(tool)
	handler.internalNotifications = executor
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

func internalNotificationTool(operation internalNotificationOperation, permission string) Tool {
	return Tool{
		Name:                          "internal_notification_test",
		Description:                   "test",
		InputSchema:                   objectSchema(map[string]any{}),
		internalNotificationOperation: operation,
		permission:                    permission,
	}
}

func TestInternalNotificationsCategoryPublishesOnlyCurrentMCPOptIns(t *testing.T) {
	tools, err := (postgresToolSource{}).tools(
		context.Background(), `"p_7"`, scope{kind: scopeCategory, category: internalNotificationsCategory},
	)
	if err != nil {
		t.Fatalf("list internal notification tools: %v", err)
	}
	wantNames := []string{
		"get_notifications_notifications",
		"get_notifications_notification",
		"put_notifications_notification",
	}
	wantPermissions := []string{
		"models.notifications.notifications.list",
		"models.notifications.notification.details",
		"models.notifications.notification.update",
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

	wire, err := json.Marshal(tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	for _, forbidden := range []string{
		"internalNotificationOperation", "permission", "delete_notifications",
		"bulk", "Bulk", "ids",
	} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("wire contains private or unsupported operation %q: %s", forbidden, wire)
		}
	}
}

func TestInternalNotificationCallUsesExactPermissionActorAndEndpointProject(t *testing.T) {
	const permission = "models.notifications.notifications.list"
	executor := &recordingInternalNotificationExecutor{
		result: internalApplicationExecution{status: http.StatusOK, body: []byte(`{"rows":[],"total":0}`)},
	}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalNotificationRouter(
		t, internalNotificationTool(internalListNotifications, permission), executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/notifications",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_notification_test","arguments":{}}}`))

	if executor.calls != 1 || executor.projectID != 7 || executor.actorID != 41 ||
		executor.operation != internalListNotifications {
		t.Fatalf("executor = calls:%d project:%d actor:%d operation:%q",
			executor.calls, executor.projectID, executor.actorID, executor.operation)
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

func TestInternalNotificationCallRefusesForeignProjectBeforeAuthorization(t *testing.T) {
	const permission = "models.notifications.notification.update"
	executor := &recordingInternalNotificationExecutor{}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalNotificationRouter(
		t, internalNotificationTool(internalMarkNotification, permission), executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/notifications",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_notification_test","arguments":{"project_id":8,"notification_id":1}}}`))

	if result["isError"] != true || executor.calls != 0 || permissions.calls != 0 {
		t.Fatalf("result=%v executor calls=%d permission calls=%d", result, executor.calls, permissions.calls)
	}
}

func TestInternalNotificationUnavailableAndInfrastructureFailuresAreSafe(t *testing.T) {
	const permission = "models.notifications.notification.details"
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	for _, test := range []struct {
		name     string
		executor internalNotificationExecutor
		forbid   []string
	}{
		{name: "unavailable", executor: nil},
		{name: "redacted", executor: &recordingInternalNotificationExecutor{
			err: errors.New("database password=do-not-leak"),
		}, forbid: []string{"password", "do-not-leak"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			router := internalNotificationRouter(
				t, internalNotificationTool(internalGetNotification, permission), test.executor, permissions,
			)
			result := resultOf(t, post(t, router, "/app/7/mcp/notifications",
				`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_notification_test","arguments":{"notification_id":1}}}`))
			text := textOf(t, result)
			if result["isError"] != true {
				t.Fatalf("failure was not an MCP tool error: %v", result)
			}
			for _, forbidden := range test.forbid {
				if strings.Contains(text, forbidden) {
					t.Fatalf("unredacted failure result: %v", result)
				}
			}
		})
	}
}
