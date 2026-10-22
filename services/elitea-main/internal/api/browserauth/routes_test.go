package browserauth

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestFormRoutesOwnExactChildSurfaceWithoutDuplicates(t *testing.T) {
	routes, _ := newTestFormRoutes(t)
	observed := make(map[string]map[string]int)
	if err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if observed[route] == nil {
			observed[route] = make(map[string]int)
		}
		observed[route][method]++
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	expected := map[string][]string{
		LoginPath:         {http.MethodGet, http.MethodHead, http.MethodOptions},
		LogoutPath:        {http.MethodGet, http.MethodHead, http.MethodOptions},
		FormLoginPath:     {http.MethodGet, http.MethodHead, http.MethodOptions},
		FormAuthorizePath: {http.MethodOptions, http.MethodPost},
		FormLogoutPath:    {http.MethodGet, http.MethodHead, http.MethodOptions},
	}
	if len(observed) != len(expected) {
		t.Fatalf("routes = %v, want %v", observed, expected)
	}
	for route, wantMethods := range expected {
		methods := observed[route]
		if len(methods) != len(wantMethods) {
			t.Fatalf("methods for %s = %v, want %v", route, methods, wantMethods)
		}
		gotMethods := make([]string, 0, len(methods))
		for method, count := range methods {
			if count != 1 {
				t.Fatalf("route %s %s registered %d times", route, method, count)
			}
			gotMethods = append(gotMethods, method)
		}
		sort.Strings(gotMethods)
		sort.Strings(wantMethods)
		if strings.Join(gotMethods, ",") != strings.Join(wantMethods, ",") {
			t.Fatalf("methods for %s = %v, want %v", route, gotMethods, wantMethods)
		}
	}
	if _, exposed := observed["/oidc/login"]; exposed {
		t.Fatal("Form-selected routes exposed the unselected OIDC provider")
	}
	// FormPaths is what the composition root registers under BasePath; a path
	// the router serves but FormPaths omits would be unreachable, and the
	// reverse would mount a 404.
	paths := FormPaths()
	if len(paths) != len(expected) {
		t.Fatalf("FormPaths() = %v, want the %d routed paths", paths, len(expected))
	}
	for _, path := range paths {
		if _, routed := expected[path]; !routed {
			t.Fatalf("FormPaths() lists %q, which the router does not serve", path)
		}
	}
}

// The forward-auth check and its X-Auth-* identity projection are gone
// (#1359). Every method on /auth/check is now chi's 404, and it reaches no
// Form dependency.
func TestFormRoutesNoLongerServeTheForwardAuthCheck(t *testing.T) {
	routes, dependencies := newTestFormRoutes(t)
	router := mountFormRoutes(routes)
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPost} {
		request := httptest.NewRequest(method, BasePath+"/check?target=rpc", nil)
		request.Header.Set("Authorization", "Bearer valid")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s /auth/check status=%d, want 404", method, recorder.Code)
		}
		for name := range recorder.Header() {
			if strings.HasPrefix(name, "X-Auth-") {
				t.Fatalf("%s /auth/check emitted %s", method, name)
			}
		}
	}
	if dependencies.flow.beginCalls != 0 || dependencies.admitter.calls != 0 {
		t.Fatal("/auth/check reached Form dependencies")
	}
}

func TestFormRoutesMethodContractsAndUnsupportedMethods(t *testing.T) {
	routes, dependencies := newTestFormRoutes(t)
	router := mountFormRoutes(routes)
	contracts := []struct {
		path    string
		allow   string
		allowed map[string]struct{}
	}{
		{path: LoginPath, allow: "GET, HEAD, OPTIONS", allowed: methodSet(http.MethodGet, http.MethodHead, http.MethodOptions)},
		{path: LogoutPath, allow: "GET, HEAD, OPTIONS", allowed: methodSet(http.MethodGet, http.MethodHead, http.MethodOptions)},
		{path: FormLoginPath, allow: "GET, HEAD, OPTIONS", allowed: methodSet(http.MethodGet, http.MethodHead, http.MethodOptions)},
		{path: FormAuthorizePath, allow: "POST, OPTIONS", allowed: methodSet(http.MethodPost, http.MethodOptions)},
		{path: FormLogoutPath, allow: "GET, HEAD, OPTIONS", allowed: methodSet(http.MethodGet, http.MethodHead, http.MethodOptions)},
	}
	methods := []string{
		http.MethodGet,
		http.MethodHead,
		http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
		http.MethodOptions,
		http.MethodTrace,
	}

	for _, contract := range contracts {
		t.Run(contract.path+" options", func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(
				recorder,
				httptest.NewRequest(http.MethodOptions, BasePath+contract.path, nil),
			)
			if recorder.Code != http.StatusOK || recorder.Header().Get("Allow") != contract.allow {
				t.Fatalf("status=%d Allow=%q", recorder.Code, recorder.Header().Get("Allow"))
			}
			requireSecurityHeaders(t, recorder.Header())
		})

		for _, method := range methods {
			if _, allowed := contract.allowed[method]; allowed {
				continue
			}
			t.Run(contract.path+" "+method, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				router.ServeHTTP(
					recorder,
					httptest.NewRequest(method, BasePath+contract.path, nil),
				)
				if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Allow") != "" {
					t.Fatalf("status=%d Allow=%q", recorder.Code, recorder.Header().Get("Allow"))
				}
				requireSecurityHeaders(t, recorder.Header())
			})
		}
	}

	if dependencies.flow.beginCalls != 0 || dependencies.flow.completeCalls != 0 ||
		dependencies.flow.logoutCalls != 0 || dependencies.admitter.calls != 0 ||
		len(*dependencies.events) != 0 {
		t.Fatalf(
			"unsupported methods reached dependencies: begin=%d complete=%d logout=%d admit=%d events=%v",
			dependencies.flow.beginCalls,
			dependencies.flow.completeCalls,
			dependencies.flow.logoutCalls,
			dependencies.admitter.calls,
			*dependencies.events,
		)
	}
}

func TestFormRoutesDoNotExposeUnselectedOrUnknownChildren(t *testing.T) {
	routes, dependencies := newTestFormRoutes(t)
	router := mountFormRoutes(routes)
	for _, path := range []string{
		BasePath + "/oidc/login",
		BasePath + "/oidc/login_callback",
		BasePath + "/info",
		BasePath + "/unknown",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d", path, recorder.Code)
		}
	}
	if dependencies.flow.beginCalls != 0 || dependencies.flow.completeCalls != 0 ||
		dependencies.flow.logoutCalls != 0 || dependencies.admitter.calls != 0 ||
		len(*dependencies.events) != 0 {
		t.Fatal("unknown route reached Form dependencies")
	}

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodOptions} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(method, BasePath+"/info?target=json&scope=galloper", nil))
		if recorder.Code != http.StatusNotFound || recorder.Header().Get("Allow") != "" {
			t.Fatalf("%s /info status=%d Allow=%q", method, recorder.Code, recorder.Header().Get("Allow"))
		}
		for _, header := range []string{"X-Auth-Session-Id", "X-Auth-Session-Endpoint", "X-Auth-Session-Name"} {
			if value := recorder.Header().Get(header); value != "" {
				t.Fatalf("%s /info emitted %s=%q", method, header, value)
			}
		}
	}
}

func newTestFormRoutes(t *testing.T) (chi.Router, *testDependencies) {
	t.Helper()
	form, dependencies := newTestHandler(t)
	return form.Routes(), dependencies
}

func mountFormRoutes(routes chi.Router) http.Handler {
	router := chi.NewRouter()
	router.Mount(BasePath, routes)
	return router
}

func methodSet(methods ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(methods))
	for _, method := range methods {
		result[method] = struct{}{}
	}
	return result
}
