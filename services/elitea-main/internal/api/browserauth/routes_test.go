package browserauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/edgeauth"
)

func TestNewFormRoutesOwnsExactChildSurfaceWithoutDuplicates(t *testing.T) {
	routes, _, _ := newTestFormRoutes(t)
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
		AuthPath:          {http.MethodGet, http.MethodHead, http.MethodOptions},
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
}

func TestFormRoutesDispatchCoreAndProviderThroughOneBaseMount(t *testing.T) {
	form, dependencies := newTestHandler(t)
	core := newCoreTestHandler(
		t,
		coreCredentialFunc(func(context.Context, forwardapp.Source, forwardapp.CredentialInput) (forwardapp.CredentialResult, error) {
			return acceptedCoreToken(), nil
		}),
		panicCoreSession(t),
		nil,
	)
	routes, err := NewFormRoutes(core, form)
	if err != nil {
		t.Fatal(err)
	}
	router := mountFormRoutes(routes)

	authRequest := coreRequest(BasePath + AuthPath)
	authRequest.Header.Set("Authorization", "Bearer valid")
	authRecorder := httptest.NewRecorder()
	router.ServeHTTP(authRecorder, authRequest)
	requireCoreOK(t, authRecorder)

	loginRecorder := httptest.NewRecorder()
	router.ServeHTTP(
		loginRecorder,
		httptest.NewRequest(http.MethodGet, BasePath+LoginPath+"?target_to=%2Fafter", nil),
	)
	if loginRecorder.Code != http.StatusFound ||
		!strings.HasPrefix(loginRecorder.Header().Get("Location"), BasePath+FormLoginPath+"?") ||
		dependencies.flow.beginCalls != 1 {
		t.Fatalf(
			"status=%d location=%q begin=%d",
			loginRecorder.Code,
			loginRecorder.Header().Get("Location"),
			dependencies.flow.beginCalls,
		)
	}
}

func TestFormRoutesMethodContractsAndUnsupportedMethods(t *testing.T) {
	routes, _, dependencies := newTestFormRoutes(t)
	router := mountFormRoutes(routes)
	contracts := []struct {
		path    string
		allow   string
		allowed map[string]struct{}
	}{
		{path: AuthPath, allow: "GET, HEAD, OPTIONS", allowed: methodSet(http.MethodGet, http.MethodHead, http.MethodOptions)},
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
	routes, _, dependencies := newTestFormRoutes(t)
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

func TestNewFormRoutesRejectsIncompleteComposition(t *testing.T) {
	_, core, dependencies := newTestFormRoutes(t)
	form, _ := newTestHandler(t)
	for _, test := range []struct {
		name string
		core *CoreHandler
		form *Handler
	}{
		{name: "missing core", form: form},
		{name: "missing form", core: core},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewFormRoutes(test.core, test.form); !errors.Is(err, ErrInvalidHandlerConfiguration) {
				t.Fatalf("error = %v, want %v", err, ErrInvalidHandlerConfiguration)
			}
		})
	}
	if dependencies.flow.beginCalls != 0 || dependencies.flow.completeCalls != 0 ||
		dependencies.flow.logoutCalls != 0 {
		t.Fatal("constructor reached Form flow")
	}
}

func newTestFormRoutes(t *testing.T) (chi.Router, *CoreHandler, *testDependencies) {
	t.Helper()
	form, dependencies := newTestHandler(t)
	core := newCoreTestHandler(t, panicCoreCredential(t), panicCoreSession(t), nil)
	routes, err := NewFormRoutes(core, form)
	if err != nil {
		t.Fatal(err)
	}
	return routes, core, dependencies
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
