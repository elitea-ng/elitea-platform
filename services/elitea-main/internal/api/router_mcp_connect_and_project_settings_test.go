package api

// Two role-level claims from the 2026-10 regression, asserted through the real
// router with the real gates in front of each route.
//
//   - #6885: a viewer can authorize an MCP server. The OAuth and DCR proxies
//     relay one exchange and bind the result to the caller, so the agent RUN
//     permission is enough. The tool sync writes toolkit rows and stays at
//     `models.applications.tool.patch`, which a viewer does not hold.
//
//   - #6789: an editor cannot change a team project's settings (name,
//     description, icon), and can still edit the project context.
//
// The permission sets below are the default-mode grants the shared migrations
// give each role for these strings. They are spelled out, not read from the
// corpus, so that each case states which string decides it.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// viewerPermissions is the part of the viewer's default-mode set these routes
// read. A viewer runs agents and reads the project context. It does not edit
// toolkits and does not edit the project.
var viewerPermissions = []string{
	"models.applications.predict.post",
	"models.project_context.view",
}

// editorPermissions is the part of the editor's default-mode set these routes
// read. 0068 gives the editor `models.project_context.edit`; 0136 gives
// `models.project_settings.edit` to the admin only.
var editorPermissions = []string{
	"models.applications.predict.post",
	"models.applications.tool.patch",
	"models.project_context.view",
	"models.project_context.edit",
}

func serveAsRole(t *testing.T, granted []string, method, path string) int {
	t.Helper()
	router := newEliteaCoreProjectScopeRouter(
		&memberOfProject{project: "7"},
		fakePermissionResolver{granted: granted, forProject: "7"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(`{}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, testAuthHeader(request))
	return recorder.Code
}

func TestViewerCanReachTheMCPAuthorizationProxies(t *testing.T) {
	for _, path := range []string{
		"/api/v2/elitea_core/mcp_oauth_proxy/7",
		"/api/v2/elitea_core/mcp_dcr_proxy/7",
	} {
		t.Run(path, func(t *testing.T) {
			code := serveAsRole(t, viewerPermissions, http.MethodPost, path)
			if code == http.StatusForbidden || code == http.StatusNotFound || code == http.StatusMethodNotAllowed {
				t.Fatalf("viewer POST %s = %d; the viewer must pass the gate (#6885)", path, code)
			}
		})
	}
}

func TestViewerCannotSyncMCPTools(t *testing.T) {
	code := serveAsRole(t, viewerPermissions, http.MethodPost,
		"/api/v2/elitea_core/mcp_sync_tools/prompt_lib/7")
	if code != http.StatusForbidden {
		t.Fatalf("viewer POST mcp_sync_tools = %d, want 403: the sync writes toolkit rows", code)
	}
	if code := serveAsRole(t, editorPermissions, http.MethodPost,
		"/api/v2/elitea_core/mcp_sync_tools/prompt_lib/7"); code == http.StatusForbidden {
		t.Fatalf("editor POST mcp_sync_tools = 403, want the gate passed")
	}
}

func TestEditorCannotChangeTeamProjectSettings(t *testing.T) {
	for _, route := range []struct{ method, path string }{
		{http.MethodPut, "/api/v2/elitea_core/project_info/prompt_lib/7/project-info"},
		{http.MethodPost, "/api/v2/elitea_core/project_icon/prompt_lib/7"},
		{http.MethodDelete, "/api/v2/elitea_core/project_icon/prompt_lib/7/icon"},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			if code := serveAsRole(t, editorPermissions, route.method, route.path); code != http.StatusForbidden {
				t.Fatalf("editor %s %s = %d, want 403 (#6789)", route.method, route.path, code)
			}
			admin := append([]string{"models.project_settings.edit"}, editorPermissions...)
			if code := serveAsRole(t, admin, route.method, route.path); code == http.StatusForbidden {
				t.Fatalf("admin %s %s = 403, want the gate passed", route.method, route.path)
			}
		})
	}
}

func TestEditorCanStillEditTheProjectContext(t *testing.T) {
	code := serveAsRole(t, editorPermissions, http.MethodPut,
		"/api/v2/elitea_core/project_context/prompt_lib/7/project-context")
	if code == http.StatusForbidden || code == http.StatusNotFound || code == http.StatusMethodNotAllowed {
		t.Fatalf("editor PUT project_context = %d; the context stays the editor's", code)
	}
	// The read stays open to a viewer.
	if code := serveAsRole(t, viewerPermissions, http.MethodGet,
		"/api/v2/elitea_core/project_info/prompt_lib/7/project-info"); code == http.StatusForbidden {
		t.Fatalf("viewer GET project_info = 403, want the read admitted")
	}
}
