package eliteacore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/publicproject"
)

// newHandler creates a Handler with a nil pool. Safe only for static handlers
// (those that never dereference h.pool).
func newHandler() *eliteacore.Handler {
	return eliteacore.NewHandler(nil)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type permissionResolverStub struct{}

func (permissionResolverStub) ResolvePermissions(
	context.Context,
	auth.User,
	string,
	string,
) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{
		UserID:      1,
		Permissions: []string{"configurations.view"},
	}, nil
}

// newRequest builds an httptest.Request with optional chi URL params and body.
func newRequest(method, target string, params map[string]string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	if len(params) > 0 {
		rctx := chi.NewRouteContext()
		for k, v := range params {
			rctx.URLParams.Add(k, v)
		}
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	}
	return req
}

// decodeObj decodes the response body into a map[string]any.
func decodeObj(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("failed to decode JSON object response: %v", err)
	}
	return out
}

// decodeArr decodes the response body into a []any.
func decodeArr(t *testing.T, w *httptest.ResponseRecorder) []any {
	t.Helper()
	var out []any
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("failed to decode JSON array response: %v", err)
	}
	return out
}

func assertStatus(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Errorf("status: want %d, got %d", want, w.Code)
	}
}

func assertContentTypeJSON(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: want application/json, got %q", ct)
	}
}

// ---- PlatformSettings -------------------------------------------------------

func TestPlatformSettings(t *testing.T) {
	h := newHandler()
	w := httptest.NewRecorder()
	h.PlatformSettings(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assertStatus(t, w, http.StatusOK)
	assertContentTypeJSON(t, w)

	body := decodeObj(t, w)

	// All expected keys must be present.
	for _, key := range []string{
		"chat_enabled", "applications_enabled", "skills_enabled",
		"toolkits_enabled", "datasources_enabled", "pipelines_enabled",
		"publishing_enabled", "moderation_enabled", "mcp_enabled",
		"support_chat_enabled",
	} {
		if _, ok := body[key]; !ok {
			t.Errorf("missing key %q", key)
		}
	}

	// Features that should be enabled.
	for _, key := range []string{"chat_enabled", "applications_enabled", "skills_enabled", "toolkits_enabled", "datasources_enabled"} {
		if v, _ := body[key].(bool); !v {
			t.Errorf("%s should be true", key)
		}
	}

	// Features that should be disabled.
	for _, key := range []string{"moderation_enabled", "support_chat_enabled"} {
		if v, _ := body[key].(bool); v {
			t.Errorf("%s should be false", key)
		}
	}
}

// TestPlatformSettingsPublishesPublicProjectID covers the key the SPA reads
// instead of its build-time VITE_PUBLIC_PROJECT_ID copy. The default and the
// configured value are BOTH asserted: a handler that published a constant 1
// would pass the default case on its own, and a deployment whose public project
// is not id 1 is exactly the case this key exists for.
func TestPlatformSettingsPublishesPublicProjectID(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		for _, name := range append([]string{publicproject.Canonical}, publicproject.Deprecated...) {
			t.Setenv(name, "")
		}
		h := newHandler()
		w := httptest.NewRecorder()
		h.PlatformSettings(w, httptest.NewRequest(http.MethodGet, "/", nil))
		assertStatus(t, w, http.StatusOK)
		if got := decodeObj(t, w)["public_project_id"]; got != float64(publicproject.Default) {
			t.Errorf("public_project_id = %v, want %d", got, publicproject.Default)
		}
	})

	t.Run("configured", func(t *testing.T) {
		for _, name := range append([]string{publicproject.Canonical}, publicproject.Deprecated...) {
			t.Setenv(name, "")
		}
		t.Setenv(publicproject.Canonical, "42")
		h := newHandler()
		w := httptest.NewRecorder()
		h.PlatformSettings(w, httptest.NewRequest(http.MethodGet, "/", nil))
		assertStatus(t, w, http.StatusOK)
		if got := decodeObj(t, w)["public_project_id"]; got != float64(42) {
			t.Errorf("public_project_id = %v, want 42", got)
		}
	})
}

// cost_budgets_enabled gates Settings → Usage, exactly as the reference's own
// platform_settings endpoint gated it. Two properties matter and neither is
// observable from the tab itself:
//
//   - it is ALWAYS present. An absent key is read by the client as a boolean
//     that is not false, so a deployment with no gateway would show a Usage tab
//     of structural zeroes as if they were measurements.
//   - it tracks the composed gateway rather than defaulting open. This is the
//     one flag on this endpoint whose safe direction is CLOSED: every other one
//     hides a feature that still works, this one hides a page that has no data
//     behind it at all.
func TestPlatformSettingsPublishesCostBudgets(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		enabled bool
	}{
		{name: "no gateway composed", enabled: false},
		{name: "gateway composed", enabled: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := eliteacore.NewHandler(nil, eliteacore.WithCostBudgets(testCase.enabled))
			w := httptest.NewRecorder()
			h.PlatformSettings(w, httptest.NewRequest(http.MethodGet, "/", nil))
			assertStatus(t, w, http.StatusOK)

			body := decodeObj(t, w)
			value, present := body["cost_budgets_enabled"]
			if !present {
				t.Fatal("cost_budgets_enabled is absent; the client reads that as enabled")
			}
			if got, _ := value.(bool); got != testCase.enabled {
				t.Fatalf("cost_budgets_enabled = %#v, want %v", value, testCase.enabled)
			}
		})
	}
}

// The default is the closed one: a handler nobody told about a gateway must not
// claim this platform tracks cost.
func TestPlatformSettingsDefaultsCostBudgetsOff(t *testing.T) {
	w := httptest.NewRecorder()
	newHandler().PlatformSettings(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if got, _ := decodeObj(t, w)["cost_budgets_enabled"].(bool); got {
		t.Fatal("cost_budgets_enabled defaulted to true with no gateway configured")
	}
}

// ---- ProjectContext / UpdateProjectContext ----------------------------------

func TestProjectContext(t *testing.T) {
	h := newHandler()
	w := httptest.NewRecorder()
	h.ProjectContext(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assertStatus(t, w, http.StatusOK)
	assertContentTypeJSON(t, w)

	body := decodeObj(t, w)
	if body["id"] != nil || body["content"] != "" || body["enabled"] != true ||
		body["activation_description"] != nil || body["updated_at"] != nil {
		t.Fatalf("default project context = %#v", body)
	}
}

func TestUpdateProjectContext(t *testing.T) {
	h := newHandler()
	w := httptest.NewRecorder()
	h.UpdateProjectContext(w, httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{}`)))

	assertStatus(t, w, http.StatusOK)
	body := decodeObj(t, w)
	if body["id"] != nil || body["content"] != "" || body["enabled"] != true ||
		body["activation_description"] != nil || body["updated_at"] != nil {
		t.Fatalf("default updated project context = %#v", body)
	}
}

func TestDeleteProjectContextWithoutStoreIsNotFound(t *testing.T) {
	h := newHandler()
	w := httptest.NewRecorder()
	h.DeleteProjectContext(w, httptest.NewRequest(http.MethodDelete, "/", nil))

	assertStatus(t, w, http.StatusNotFound)
	if body := decodeObj(t, w); body["error"] != "Project context not found" {
		t.Fatalf("delete response = %#v", body)
	}
}

// ---- Notifications ----------------------------------------------------------

func TestNotifications(t *testing.T) {
	h := newHandler()
	w := httptest.NewRecorder()
	h.Notifications(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assertStatus(t, w, http.StatusOK)
	assertContentTypeJSON(t, w)

	body := decodeObj(t, w)
	if _, ok := body["notifications"]; !ok {
		t.Error("response must contain 'notifications' key")
	}
	if total, _ := body["total"].(float64); total != 0 {
		t.Errorf("total should be 0, got %v", body["total"])
	}
}

// ---- PublicApplications / TrendingAuthors -----------------------------------

func TestPublicApplications(t *testing.T) {
	h := newHandler()
	w := httptest.NewRecorder()
	h.PublicApplications(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assertStatus(t, w, http.StatusOK)
	body := decodeObj(t, w)
	if _, ok := body["rows"]; !ok {
		t.Error("response must contain 'rows' key")
	}
	if total, _ := body["total"].(float64); total != 0 {
		t.Errorf("total should be 0, got %v", body["total"])
	}
}

func TestTrendingAuthors(t *testing.T) {
	h := newHandler()
	w := httptest.NewRecorder()
	h.TrendingAuthors(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assertStatus(t, w, http.StatusOK)
	assertContentTypeJSON(t, w)

	// Handler returns a plain JSON array (not an object with items/total wrapper).
	items := decodeArr(t, w)
	if len(items) != 0 {
		t.Errorf("expected empty array, got %d items", len(items))
	}
}

// `TestModerationStatus` used to sit here, and it PASSED: it asserted that the
// handler answers `{"status":"approved"}` to a request with no project, no
// entity and no principal. That is the whole trouble with pinning a stub — the
// test agreed with the code and both were wrong about the product. The
// replacement is
// internal/api/v2/moderation/requests_postgres_integration_test.go, which reads
// back what it wrote through a real database.

// ---- Permissions ------------------------------------------------------------

func TestPermissions_NoAuth_ReturnsEmptyArray(t *testing.T) {
	h := newHandler()
	w := httptest.NewRecorder()
	h.Permissions(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assertStatus(t, w, http.StatusOK)
	assertContentTypeJSON(t, w)

	items := decodeArr(t, w)
	if len(items) != 0 {
		t.Errorf("expected empty array without auth, got %d items", len(items))
	}
}

func TestPermissions_WithAuth_ReturnsPermissionList(t *testing.T) {
	h := eliteacore.NewHandler(
		nil,
		eliteacore.WithPermissionResolver(permissionResolverStub{}),
	)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{
		ID:    "user-1",
		Email: "test@test.com",
	}))
	w := httptest.NewRecorder()
	h.Permissions(w, req)

	assertStatus(t, w, http.StatusOK)
	assertContentTypeJSON(t, w)

	items := decodeArr(t, w)
	if len(items) == 0 {
		t.Fatal("expected non-empty permissions with auth context")
	}

	for i, item := range items {
		perm, ok := item.(map[string]any)
		if !ok {
			t.Errorf("permission[%d] is not an object", i)
			continue
		}
		name, hasName := perm["name"].(string)
		if !hasName || name == "" {
			t.Errorf("permission[%d] missing or empty 'name'", i)
		}
		enabled, hasEnabled := perm["enabled"].(bool)
		if !hasEnabled {
			t.Errorf("permission[%d] missing 'enabled'", i)
		} else if !enabled {
			t.Errorf("permission[%d] (%q) should be enabled", i, name)
		}
	}
}

// ---- DefaultIcons -----------------------------------------------------------

// DEFECT: DefaultIcons returned five invented entries — /icons/robot.svg,
// /icons/brain.svg, /icons/chat.svg, /icons/code.svg and /icons/data.svg.
// No such file exists in the repository. The only /icons route the router
// mounts is the two-segment /icons/{projectID}/{filename} object-store route
// (internal/api/router.go:625). Every one of those URLs therefore answered 404.
//
// The project icon picker renders each entry as <img src={icon.url}> and takes
// the url branch, so its initial-letter fallback never runs: the "Default"
// section showed five broken images. The legacy handler
// (legacy/plugins/elitea_core/api/v2/default_icons.py) enumerated a real
// directory; the port replaced that with a hardcoded list.
//
// The catalogue now reports only files that exist. An empty directory means an
// empty array, never a fabricated URL.

func TestDefaultIconsReportsNoIconWhenTheDirectoryIsAbsent(t *testing.T) {
	t.Setenv("DEFAULT_ICON_DATA_DIR", filepath.Join(t.TempDir(), "absent"))

	h := newHandler()
	w := httptest.NewRecorder()
	h.DefaultIcons(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assertStatus(t, w, http.StatusOK)
	assertContentTypeJSON(t, w)

	// Handler returns a plain JSON array of icon objects (no wrapper object).
	if items := decodeArr(t, w); len(items) != 0 {
		t.Fatalf("catalogue = %v for a directory that does not exist, want an empty array; "+
			"every entry here renders as a broken image", items)
	}
}

func TestDefaultIconsEnumeratesTheIconDirectory(t *testing.T) {
	directory := t.TempDir()
	for _, filename := range []string{"robot.svg", "brain.svg"} {
		if err := os.WriteFile(filepath.Join(directory, filename), []byte("<svg/>"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A sub-directory and a dot file are not icons.
	if err := os.Mkdir(filepath.Join(directory, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEFAULT_ICON_DATA_DIR", directory)

	h := newHandler()
	w := httptest.NewRecorder()
	h.DefaultIcons(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assertStatus(t, w, http.StatusOK)
	items := decodeArr(t, w)
	if len(items) != 2 {
		t.Fatalf("catalogue = %v, want the two files on disk", items)
	}

	urls := map[string]string{}
	for i, raw := range items {
		icon, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("icon[%d] is not an object", i)
		}
		name, _ := icon["name"].(string)
		iconURL, _ := icon["url"].(string)
		if name == "" {
			t.Errorf("icon[%d] missing or empty 'name'", i)
		}
		if !strings.HasPrefix(iconURL, eliteacore.DefaultIconURLPrefix) {
			t.Errorf("icon[%d] url = %q, want the %q prefix that a static route serves",
				i, iconURL, eliteacore.DefaultIconURLPrefix)
		}
		urls[name] = iconURL
	}
	if got := urls["robot"]; got != eliteacore.DefaultIconURLPrefix+"robot.svg" {
		t.Errorf("robot url = %q", got)
	}
}

// ---- UploadIcon -------------------------------------------------------------

func TestUploadIcon(t *testing.T) {
	h := newHandler()
	w := httptest.NewRecorder()
	h.UploadIcon(w, httptest.NewRequest(http.MethodPost, "/", nil))

	assertStatus(t, w, http.StatusOK)
	body := decodeObj(t, w)
	if ok, _ := body["ok"].(bool); !ok {
		t.Error("ok should be true")
	}
	if _, hasURL := body["url"]; !hasURL {
		t.Error("response must contain 'url' key")
	}
}

// ---- Export / Import --------------------------------------------------------

// TestExportImportPostRefusesWithNoPool corrects a test that agreed with a
// defect (#505).
//
// It read:
//
//	// With nil pool and empty body, the handler returns 201 with result/errors shape.
//	assertStatus(t, w, http.StatusCreated)
//
// The comment describes the behaviour correctly and the assertion approves of
// it. 201 Created is the answer "every entity in your file was imported". A
// handler with no pool imported nothing and can import nothing, so the answer
// was untrue, and the import wizard reads any 2xx as a completed import: it
// marks each selected entity green and closes. The test would have failed the
// repair, which is worse than having no test at all.
//
// The condition is not reachable in the production router, which always builds
// the handler with a pool. It is reachable in this package, and the assertion
// is what the repair has to be measured against: a handler that cannot write
// must not report a write.
func TestExportImportPostRefusesWithNoPool(t *testing.T) {
	h := newHandler()
	w := httptest.NewRecorder()
	h.ExportImportPost(w, httptest.NewRequest(http.MethodPost, "/", nil))

	assertStatus(t, w, http.StatusInternalServerError)
	body := decodeObj(t, w)
	if _, hasResult := body["result"]; hasResult {
		t.Error("a refusal must not carry a 'result' key: nothing was imported")
	}
	message, _ := body["error"].(string)
	if message == "" {
		t.Errorf("response must name the reason, got %v", body)
	}
}

// TestExportImportGetRefusesWithNoPool is the same correction on the export.
//
// The old assertion was `assertStatus(t, w, http.StatusOK)` on a body of
// {"ok": true}. That body has no `applications` key at all, and the export
// button saves whatever it is given as the agent's backup file. An empty
// backup that reports success is the failure mode this whole issue is about.
func TestExportImportGetRefusesWithNoPool(t *testing.T) {
	h := newHandler()
	w := httptest.NewRecorder()
	h.ExportImportGet(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assertStatus(t, w, http.StatusInternalServerError)
	body := decodeObj(t, w)
	if ok, _ := body["ok"].(bool); ok {
		t.Error("a failed export must not answer ok")
	}
	if _, hasApplications := body["applications"]; hasApplications {
		t.Error("a failed export must not carry an 'applications' key")
	}
}

// TestForkRefusesWithNoPool is the third of the same shape. Fork now has a
// route (#505), so the answer it gives when it cannot write is a real answer.
func TestForkRefusesWithNoPool(t *testing.T) {
	h := newHandler()
	w := httptest.NewRecorder()
	h.Fork(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"applications":[{"name":"a"}]}`)))

	assertStatus(t, w, http.StatusInternalServerError)
	body := decodeObj(t, w)
	if _, hasResult := body["result"]; hasResult {
		t.Error("a refusal must not carry a 'result' key: nothing was forked")
	}
}

func TestExportConverter(t *testing.T) {
	h := newHandler()
	w := httptest.NewRecorder()
	h.ExportConverter(w, httptest.NewRequest(http.MethodPost, "/", nil))

	assertStatus(t, w, http.StatusOK)
	body := decodeObj(t, w)
	if ok, _ := body["ok"].(bool); !ok {
		t.Error("ok should be true")
	}
}

// ---- UpdateNotification -----------------------------------------------------

func TestUpdateNotification(t *testing.T) {
	h := newHandler()
	w := httptest.NewRecorder()
	h.UpdateNotification(w, httptest.NewRequest(http.MethodPut, "/", nil))

	assertStatus(t, w, http.StatusOK)
	body := decodeObj(t, w)
	if ok, _ := body["ok"].(bool); !ok {
		t.Error("ok should be true")
	}
}

// ---- Project Icons ----------------------------------------------------------
//
// The three tests that used to live here asserted the stubs: an empty `items`
// page, a fabricated `{name,url}` for an upload that was discarded, and a 204
// from a delete that deleted nothing. All three passed BECAUSE the handlers
// did nothing, so they would have gone on passing forever. They are replaced
// by project_info_honest_refusal_test.go, which asserts the refusal when no
// store is composed and the round trip (upload → download → list → delete)
// when one is.

// TestCreateProjectIconRejectsARequestWithNoFile keeps the one input case that
// is the caller's mistake rather than the deployment's absence. The previous
// handler answered 200 here, with a URL built from a generated id, for a
// request that carried no file at all.
func TestCreateProjectIconRejectsARequestWithNoFile(t *testing.T) {
	w := httptest.NewRecorder()
	newHandler().CreateProjectIcon(
		w,
		newRequest(http.MethodPost, "/", map[string]string{"projectID": "7"}, nil),
	)

	assertStatus(t, w, http.StatusBadRequest)
	if code, _ := decodeObj(t, w)["code"].(string); code != "missing_file" {
		t.Errorf("code = %q, want missing_file", code)
	}
}

// ---- MCP Proxies ------------------------------------------------------------

func TestMCPOAuthProxy(t *testing.T) {
	h := newHandler()
	w := httptest.NewRecorder()
	h.MCPOAuthProxy(w, httptest.NewRequest(http.MethodPost, "/", nil))

	assertStatus(t, w, http.StatusBadRequest)
	body := decodeObj(t, w)
	if body["error"] != "invalid_request" {
		t.Fatalf("unexpected error: %#v", body)
	}
}

func TestMCPDCRProxy(t *testing.T) {
	h := newHandler()
	w := httptest.NewRecorder()
	h.MCPDCRProxy(w, httptest.NewRequest(http.MethodPost, "/", nil))

	assertStatus(t, w, http.StatusBadRequest)
	body := decodeObj(t, w)
	if body["error"] != "invalid_request" {
		t.Fatalf("unexpected error: %#v", body)
	}
}

func TestMCPOAuthProxyAllowsCustomHTTPSHost(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://identity.custom.example/oauth/token" {
			t.Fatalf("unexpected request URL %q", req.URL)
		}
		if err := req.ParseForm(); err != nil {
			t.Fatalf("parse OAuth form: %v", err)
		}
		if got := req.Form.Get("client_id"); got != "client&id" {
			t.Fatalf("client_id = %q, want %q", got, "client&id")
		}
		if got := req.Form.Get("code"); got != "code=value" {
			t.Fatalf("code = %q, want %q", got, "code=value")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"token"}`)),
			Request:    req,
		}, nil
	})}

	body := `{
		"token_endpoint":"https://identity.custom.example/oauth/token",
		"client_id":"client&id",
		"code":"code=value",
		"redirect_uri":"https://elitea.example/mcp-auth-callback"
	}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client)).MCPOAuthProxy(w, req)

	assertStatus(t, w, http.StatusOK)
	response := decodeObj(t, w)
	if response["access_token"] != "token" {
		t.Fatalf("unexpected OAuth response: %#v", response)
	}
}

func TestMCPDCRProxyAllowsCustomHTTPSHost(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://mcp.custom.example/register" {
			t.Fatalf("unexpected request URL %q", req.URL)
		}
		var requestBody map[string]any
		if err := json.NewDecoder(req.Body).Decode(&requestBody); err != nil {
			t.Fatalf("decode DCR body: %v", err)
		}
		if requestBody["client_name"] != "Elitea" {
			t.Fatalf("unexpected DCR body: %#v", requestBody)
		}
		if got := requestBody["token_endpoint_auth_method"]; got != "none" {
			t.Fatalf("token_endpoint_auth_method = %#v, want none", got)
		}
		if got := requestBody["application_type"]; got != "web" {
			t.Fatalf("application_type = %#v, want web", got)
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"client_id":"registered"}`)),
			Request:    req,
		}, nil
	})}

	body := `{
		"registration_endpoint":"https://mcp.custom.example/register",
		"client_name":"Elitea",
		"redirect_uris":["https://elitea.example/mcp-auth-callback"]
	}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client)).MCPDCRProxy(w, req)

	assertStatus(t, w, http.StatusOK)
	response := decodeObj(t, w)
	if response["client_id"] != "registered" {
		t.Fatalf("unexpected DCR response: %#v", response)
	}
}

func TestMCPProxiesRejectUnsafeEndpointURLs(t *testing.T) {
	tests := []struct {
		name    string
		handler func(*eliteacore.Handler, http.ResponseWriter, *http.Request)
		field   string
	}{
		{
			name:    "OAuth",
			handler: (*eliteacore.Handler).MCPOAuthProxy,
			field:   "token_endpoint",
		},
		{
			name:    "DCR",
			handler: (*eliteacore.Handler).MCPDCRProxy,
			field:   "registration_endpoint",
		},
	}
	unsafeURLs := []string{
		"file:///etc/passwd",
		"http://mcp.internal.example/token",
		"https:///missing-host",
		"https://user:password@mcp.example/token",
		"https://mcp.example/trusted/%2e%2e/token",
	}

	for _, test := range tests {
		for _, unsafeURL := range unsafeURLs {
			t.Run(test.name+"/"+unsafeURL, func(t *testing.T) {
				transportCalled := false
				client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					transportCalled = true
					return nil, nil
				})}
				requestBody, err := json.Marshal(map[string]string{test.field: unsafeURL})
				if err != nil {
					t.Fatalf("marshal request: %v", err)
				}
				req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(requestBody))
				w := httptest.NewRecorder()

				test.handler(eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client)), w, req)

				assertStatus(t, w, http.StatusBadRequest)
				if transportCalled {
					t.Fatal("unsafe endpoint reached the HTTP transport")
				}
			})
		}
	}
}

func TestMCPProxyRejectsCrossOriginRedirect(t *testing.T) {
	requestCount := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requestCount++
		response := &http.Response{
			StatusCode: http.StatusFound,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}
		response.Header.Set("Location", "https://different.example/token")
		return response, nil
	})}

	body := `{
		"token_endpoint":"https://identity.custom.example/oauth/token",
		"code":"authorization-code",
		"redirect_uri":"https://elitea.example/mcp-auth-callback"
	}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client)).MCPOAuthProxy(w, req)

	assertStatus(t, w, http.StatusBadGateway)
	if requestCount != 1 {
		t.Fatalf("request count = %d, want 1", requestCount)
	}
	response := decodeObj(t, w)
	if _, exposesDetails := response["error_description"]; exposesDetails {
		t.Fatalf("proxy exposed transport details: %#v", response)
	}
}

// ---- SupportConfig ----------------------------------------------------------

func TestSupportConfig(t *testing.T) {
	h := newHandler()
	w := httptest.NewRecorder()
	h.SupportConfig(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assertStatus(t, w, http.StatusOK)
	body := decodeObj(t, w)
	if _, ok := body["enabled"]; !ok {
		t.Error("response must contain 'enabled' key")
	}
	if enabled, _ := body["enabled"].(bool); enabled {
		t.Error("enabled should be false")
	}
}

// ---- UpdateProjectInfo (no pool access when name is absent/empty) -----------

func TestUpdateProjectInfo_NoBody(t *testing.T) {
	h := newHandler()
	req := newRequest(http.MethodPut, "/", map[string]string{"projectID": "proj-1"}, nil)
	w := httptest.NewRecorder()
	h.UpdateProjectInfo(w, req)

	assertStatus(t, w, http.StatusOK)
	body := decodeObj(t, w)
	if ok, _ := body["ok"].(bool); !ok {
		t.Error("ok should be true")
	}
}

func TestUpdateProjectInfo_EmptyName(t *testing.T) {
	// name = "" means the pool.Exec branch is skipped entirely.
	payload := strings.NewReader(`{"name":""}`)
	req := newRequest(http.MethodPut, "/", map[string]string{"projectID": "proj-1"}, payload)
	w := httptest.NewRecorder()
	newHandler().UpdateProjectInfo(w, req)

	assertStatus(t, w, http.StatusOK)
	body := decodeObj(t, w)
	if ok, _ := body["ok"].(bool); !ok {
		t.Error("ok should be true")
	}
}

// ---- DB-dependent methods: compile-time existence check --------------------
//
// Handlers below require a live pgxpool.Pool. Calling them with a nil pool
// panics inside pgx before any error-handling code can run.  The test below
// is a compile-time assertion: if any method is removed or renamed the build
// fails, giving us a regression signal without needing a real database.

func TestDBHandlerMethodsExist(t *testing.T) {
	h := eliteacore.NewHandler(nil)

	// Assign every DB-dependent method to the same function-type slice.
	// The compiler rejects the assignment if a method signature changes.
	_ = []func(http.ResponseWriter, *http.Request){
		h.ProjectInfo,
		h.SearchOptions,
		h.Users,
		h.Roles,
		h.ChatConfig,
		h.Author,
		h.Publish,
		h.Unpublish,
		h.PublishValidate,
		h.VersionValidator,
		h.Recommendations,
		h.Feedbacks,
		h.Pin,
		h.Unpin,
		h.ApplicationRelation,
	}
	// If we reach this line, all method signatures are correct.
}
