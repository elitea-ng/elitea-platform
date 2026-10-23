package api

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
)

// secretsRoutes is the URL surface the secrets domain must expose: the legacy
// /api/v2/<plugin>/<resource-module>/<mode>/<params>, where the plugin is
// `secrets` and the resource modules are legacy/plugins/secrets/api/v2/
// {secrets,secret,hide}.py.
//
// This list previously held the UNPREFIXED shape, which #137 moved the server
// to after reading the doubled segment as a Go double-mount bug. It is not a
// bug — the pinned baseline client (apps/elitea-ui/src/api/secrets.js:3,
// apiSlicePath = '/secrets'), elitea-sdk, admin_ui and qa/elitea-api-testing
// all call the prefixed form, and moving the server broke all four (#151).
var secretsRoutes = []struct {
	method string
	route  string
}{
	{http.MethodGet, "/api/v2/secrets/secrets/{mode}/{projectID}"},
	{http.MethodPost, "/api/v2/secrets/secrets/{mode}/{projectID}"},
	{http.MethodGet, "/api/v2/secrets/secret/{mode}/{projectID}/{name}"},
	// administration-mode create (unit A14). The legacy ProjectAPI has no POST
	// on this path; its AdminAPI does, and it is how admin_ui creates a global
	// secret.
	{http.MethodPost, "/api/v2/secrets/secret/{mode}/{projectID}/{name}"},
	{http.MethodPut, "/api/v2/secrets/secret/{mode}/{projectID}/{name}"},
	{http.MethodDelete, "/api/v2/secrets/secret/{mode}/{projectID}/{name}"},
	{http.MethodPost, "/api/v2/secrets/hide/{mode}/{projectID}/{name}"},
	// The mode-less show route the legacy runtime also serves and elitea-sdk is the sole
	// caller of (elitea_sdk/runtime/clients/client.py:108 builds
	// {api_v2}/secrets/secret/{project_id} and appends /{name}).
	{http.MethodGet, "/api/v2/secrets/secret/{projectID}/{name}"},
}

// v2RootSecretsRequests is the shape #137 introduced. It must be GONE: while
// it was served, every consumer outside apps/elitea-web 404'd. Asserted by
// REQUEST rather than by walking the route table, because chi.Walk reports a
// Mount("/", …) as a wildcard rather than as the concrete patterns behind it,
// so a table assertion would pass against a handler mounted at the v2 root.
var v2RootSecretsRequests = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/v2/secrets/default/1"},
	{http.MethodPost, "/api/v2/secrets/default/1"},
	{http.MethodGet, "/api/v2/secret/default/1/token"},
	{http.MethodPut, "/api/v2/secret/default/1/token"},
	{http.MethodDelete, "/api/v2/secret/default/1/token"},
	{http.MethodPost, "/api/v2/hide/default/1/token"},
}

func walkRoutes(t *testing.T, router chi.Router) map[string]struct{} {
	t.Helper()
	got := make(map[string]struct{})
	if err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		got[method+" "+route] = struct{}{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

// serveResponse runs one request through the router and returns the recorder.
// A panic from a handler entered with a nil pool is left as the recorder's
// zero-value 200→500 default, the same answer apimw.Recover produces, so
// callers can treat a 500 as "the route matched and the handler ran".
func serveResponse(t *testing.T, router chi.Router, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				recorder.Code = http.StatusInternalServerError
			}
		}()
		router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(method, path, nil)))
	}()
	return recorder
}

func serveStatus(t *testing.T, router chi.Router, method, path string) int {
	t.Helper()
	return serveResponse(t, router, method, path).Code
}

func newSecretsTestRouter(t *testing.T) chi.Router {
	t.Helper()
	return NewRouter(RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
	})
}

// TestRouterServesSecretsUnderThePluginPrefix pins #151: the six routes sit
// under the `secrets` plugin mount, reproducing the legacy URL shape.
func TestRouterServesSecretsUnderThePluginPrefix(t *testing.T) {
	router := newSecretsTestRouter(t)

	got := walkRoutes(t, router)

	// Control: the walk really does report /api/v2 patterns in this shape, so
	// a "missing" verdict below cannot come from a mismatched control string.
	if _, ok := got["GET /api/v2/elitea_core/skills/{mode}/{projectID}"]; !ok {
		t.Fatalf("the prototype group is not walked in the expected shape; this test proves nothing")
	}

	for _, want := range secretsRoutes {
		key := want.method + " " + want.route
		if _, ok := got[key]; !ok {
			var registered []string
			for route := range got {
				if strings.Contains(route, "secret") || strings.Contains(route, "/hide/") {
					registered = append(registered, route)
				}
			}
			sort.Strings(registered)
			t.Errorf("route %q is not registered; secrets-ish routes present: %v", key, registered)
		}
	}
}

// TestRouterDoesNotServeSecretsAtTheV2Root is the other half: #137's shape
// must be absent, even if the correct routes were registered alongside it —
// serving both would leave two conventions live, which is what #151 set out
// to end.
func TestRouterDoesNotServeSecretsAtTheV2Root(t *testing.T) {
	router := newSecretsTestRouter(t)

	// Control: the prefixed shape IS served, so a 404 below means "this path
	// is not routed" rather than "the router serves no secrets at all".
	if code := serveStatus(t, router, http.MethodGet, "/api/v2/secrets/secrets/default/1"); code == http.StatusNotFound {
		t.Fatalf("the prefixed list route 404s, so the assertions below prove nothing")
	}

	for _, unwanted := range v2RootSecretsRequests {
		if code := serveStatus(t, router, unwanted.method, unwanted.path); code != http.StatusNotFound {
			t.Errorf("%s %s answers %d, not 404; #137's v2-root shape is back", unwanted.method, unwanted.path, code)
		}
	}
}

// TestSecretsRoutesAnswerRequests proves the registration is reachable through
// the real router, not merely present in the route table. A nil pool makes the
// handlers panic once entered, which apimw.Recover turns into a 500 — a 500 is
// proof the route matched and the mode gate let the request through.
func TestSecretsRoutesAnswerRequests(t *testing.T) {
	router := newSecretsTestRouter(t)

	requests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v2/secrets/secrets/default/1"},
		{http.MethodPost, "/api/v2/secrets/secrets/default/1"},
		{http.MethodGet, "/api/v2/secrets/secret/default/1/token"},
		{http.MethodPut, "/api/v2/secrets/secret/default/1/token"},
		{http.MethodDelete, "/api/v2/secrets/secret/default/1/token"},
		{http.MethodPost, "/api/v2/secrets/hide/default/1/token"},
		// elitea-sdk's mode-less show route.
		{http.MethodGet, "/api/v2/secrets/secret/1/token"},
	}

	// Control: an unregistered path in the same namespace still 404s.
	if code := serveStatus(t, router, http.MethodGet, "/api/v2/secrets/not_a_route/default/1"); code != http.StatusNotFound {
		t.Fatalf("an unregistered path answers %d, so the assertions below cannot detect a missing route", code)
	}

	for _, request := range requests {
		t.Run(request.method+" "+request.path, func(t *testing.T) {
			code := serveStatus(t, router, request.method, request.path)
			if code == http.StatusNotFound || code == http.StatusMethodNotAllowed {
				t.Errorf("status = %d: the path the client calls is not routed", code)
			}
		})
	}
}

// TestSecretsModeDispatch pins the mode half of #151. The legacy
// APIBase.proxy_method looks `mode` up in mode_handlers and abort(404)s on a
// miss, so an invented mode must not be silently accepted — `prompt_lib`, the
// third convention the new client had introduced, is exactly such a miss.
//
// `administration` selects the legacy AdminAPI over the GLOBAL vault
// (VaultClient() with no project). Unit A14 implements it as a genuinely
// separate handler over the `admin` row (internal/api/v2/secrets/admin.go); it
// used to answer 501, and the reason it did — routing it into the project
// handler would have read and WRITTEN project 0's vault — is why the
// implementation is separate rather than a flag.
func TestSecretsModeDispatch(t *testing.T) {
	router := newSecretsTestRouter(t)

	cases := []struct {
		name   string
		method string
		path   string
		want   int
		body   string
	}{
		{
			name: "invented mode is rejected", method: http.MethodGet,
			path: "/api/v2/secrets/secrets/prompt_lib/1",
			want: http.StatusNotFound, body: "unknown mode",
		},
		{
			// This router is built with no permission resolver, and the
			// administration routes fail CLOSED on that — 403, not 501 and not
			// a 500 from the nil pool the handler would otherwise reach. That
			// is the property worth pinning: the gate runs before the handler.
			name: "administration is routed and gated", method: http.MethodGet,
			path: "/api/v2/secrets/secrets/administration/0",
			want: http.StatusForbidden, body: "insufficient permissions",
		},
		{
			name: "administration create is routed and gated", method: http.MethodPost,
			path: "/api/v2/secrets/secret/administration/0/probe",
			want: http.StatusForbidden, body: "insufficient permissions",
		},
		{
			// The one administration route deliberately not built: the legacy
			// bulk vault replacement, which no client in the workspace calls.
			name: "bulk vault replacement is not implemented", method: http.MethodPost,
			path: "/api/v2/secrets/secrets/administration/0",
			want: http.StatusNotImplemented, body: "bulk replacement of the global vault is not implemented",
		},
		{
			// Project mode has no POST on the single-secret path.
			name: "project mode rejects the create verb", method: http.MethodPost,
			path: "/api/v2/secrets/secret/default/1/probe",
			want: http.StatusMethodNotAllowed, body: "method not allowed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := serveResponse(t, router, tc.method, tc.path)
			if recorder.Code != tc.want {
				t.Errorf("status = %d, want %d (body %q)", recorder.Code, tc.want, recorder.Body.String())
			}
			// The status alone does not discriminate a mode rejection from
			// chi's own "no such route" 404, so the body is asserted too.
			if !strings.Contains(recorder.Body.String(), tc.body) {
				t.Errorf("body = %q, want it to contain %q", recorder.Body.String(), tc.body)
			}
		})
	}
}
