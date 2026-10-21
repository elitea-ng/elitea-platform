package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// CallPlatformTool is the endpoint's own dispatch, in process (issue 1231):
// the category's listing, the operation's own permission against the caller
// on the request, the project clamp, and the executor's answer.
func TestCallPlatformToolRunsTheCategoryOperationAsTheCaller(t *testing.T) {
	const permission = "models.notifications.notifications.view"
	executor := &recordingInternalNotificationExecutor{result: internalApplicationExecution{status: http.StatusOK, body: []byte(`{"rows":[]}`)}}
	permissions := &recordingInternalPermissionResolver{resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}}}
	handler := NewHandler(nil, nil, nil, permissions)
	var asked scope
	handler.source = &funcSource{fn: func(_ string, s scope) ([]Tool, error) {
		asked = s
		return []Tool{internalNotificationTool(internalListNotifications, permission)}, nil
	}}
	handler.internalNotifications = executor
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "41", UserID: "41", AuthType: "token"}))

	outcome, err := handler.CallPlatformTool(request, 7, "mcp_elitea_internal_notifications", "internal_notification_test", json.RawMessage(`{"limit":3}`))
	if err != nil || outcome.IsError || outcome.Text != `{"rows":[]}` {
		t.Fatalf("outcome = %+v, %v", outcome, err)
	}
	if asked.kind != scopeCategory || asked.category != "notifications" {
		t.Fatalf("listed scope = %+v", asked)
	}
	if executor.calls != 1 || executor.projectID != 7 || executor.actorID != 41 ||
		executor.arguments["project_id"] != json.Number("7") || executor.arguments["limit"] != json.Number("3") {
		t.Fatalf("executor = %+v", executor)
	}
	if permissions.projectID != "7" {
		t.Fatalf("permissions resolved for project %q", permissions.projectID)
	}

	// Another project in the arguments is refused, as on the endpoint.
	outcome, err = handler.CallPlatformTool(request, 7, "mcp_elitea_internal_notifications", "internal_notification_test", json.RawMessage(`{"project_id":8}`))
	if err != nil || !outcome.IsError || executor.calls != 1 {
		t.Fatalf("cross-project outcome = %+v, %v (calls %d)", outcome, err, executor.calls)
	}
	// A caller without the operation's permission runs nothing.
	permissions.resolution.Permissions = nil
	outcome, err = handler.CallPlatformTool(request, 7, "mcp_elitea_internal_notifications", "internal_notification_test", nil)
	if err != nil || !outcome.IsError || !strings.Contains(outcome.Text, permission) || executor.calls != 1 {
		t.Fatalf("unpermitted outcome = %+v, %v", outcome, err)
	}
	// An unknown tool names the category's tools; a type that is no builder
	// is not a platform tool.
	outcome, err = handler.CallPlatformTool(request, 7, "mcp_elitea_internal_notifications", "nope", nil)
	if err != nil || !outcome.Unknown || len(outcome.Available) != 1 || outcome.Available[0] != "internal_notification_test" {
		t.Fatalf("unknown outcome = %+v, %v", outcome, err)
	}
	if _, err := handler.CallPlatformTool(request, 7, "github", "x", nil); err == nil {
		t.Fatal("a saved toolkit type was dispatched as a platform tool")
	}
}
