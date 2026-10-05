package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	v2auth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/auth"
)

// Exactly one browser-auth plane may own /auth.
//
// router.go mounts the OIDC session lifecycle on that prefix when a
// SessionHandler is configured, and mountReviewedProductionRoutes mounts the
// production Form browser routes on the same string. Composing both panicked
// chi during NewRouter — "attempting to Mount() a handler on an existing path,
// '/auth'" — so the process died at startup rather than serving a
// degraded surface.
//
// That combination is reachable in production, not hypothetical:
// ELITEA_RUNTIME_ENABLED requires production authentication
// (cmd/elitea-main/main.go:686-688), so every OIDC deployment that enables the
// runtime composes both. main.go's comment asserting the two "can coexist"
// described an intent the router never implemented.
func TestEdgeAuthPrefixHasOneOwner(t *testing.T) {
	// A route the Form browser plane owns and the OIDC plane does not, so the
	// assertions below can tell WHICH plane holds the prefix. Deliberately not
	// /login or /logout: those exist on both planes with different meanings,
	// and a status code alone could not distinguish them.
	newAuthRoutes := func(t *testing.T) *ProductionAuthRoutes {
		t.Helper()
		browser := chi.NewRouter()
		browser.Post("/form/authorize", func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusNoContent)
		})
		main := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusNoContent)
		})
		routes, err := NewProductionAuthRoutes(browser, main)
		if err != nil {
			t.Fatal(err)
		}
		return routes
	}

	// The pool is never dialled: these assertions exercise route registration,
	// and none of the asserted paths reach a session-handler method body.
	sessionHandler := v2auth.NewSessionHandler(nil, "test-session-secret")

	t.Run("production auth alone keeps the prefix", func(t *testing.T) {
		router := NewRouter(RouterConfig{ProductionAuth: newAuthRoutes(t)})

		assertStatus(t, router, http.MethodPost, "/auth/form/authorize", http.StatusNoContent)
		assertStatus(t, router, http.MethodGet, "/internal/auth/main", http.StatusNoContent)
	})

	t.Run("OIDC session handler takes the prefix", func(t *testing.T) {
		// Before the fix this call panicked; reaching any assertion at all is
		// itself the primary regression signal.
		router := NewRouter(RouterConfig{
			ProductionAuth: newAuthRoutes(t),
			Auth:           AuthDeps{SessionHandler: sessionHandler},
		})

		// The Form browser plane yielded the prefix — proving the two are not
		// both mounted, which is what chi refused to allow.
		assertStatus(t, router, http.MethodPost, "/auth/form/authorize", http.StatusNotFound)
		// ...and the OIDC plane really does hold it, rather than the prefix
		// being unowned. /auth/logout exists only on the OIDC plane's
		// registration in router.go.
		assertNotStatus(t, router, http.MethodGet, "/auth/logout", http.StatusNotFound)
		// The internal endpoint is NOT collateral damage: it is a distinct path,
		// the auth edge depends on it, and it is what the runtime's
		// ForwardedIdentityVerifier is paired with.
		assertStatus(t, router, http.MethodGet, "/internal/auth/main", http.StatusNoContent)
	})

	t.Run("top-level SessionHandler is honoured too", func(t *testing.T) {
		// router.go folds Auth.SessionHandler into cfg.SessionHandler before the
		// OIDC mount, so a caller setting the top-level field directly reaches
		// the same mount. Guarding on the wrong one would panic here only.
		router := NewRouter(RouterConfig{
			ProductionAuth: newAuthRoutes(t),
			SessionHandler: sessionHandler,
		})

		assertStatus(t, router, http.MethodPost, "/auth/form/authorize", http.StatusNotFound)
		assertStatus(t, router, http.MethodGet, "/internal/auth/main", http.StatusNoContent)
	})
}

func assertStatus(t *testing.T, router http.Handler, method string, path string, want int) {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	if recorder.Code != want {
		t.Fatalf("%s %s status = %d, want %d", method, path, recorder.Code, want)
	}
}

func assertNotStatus(t *testing.T, router http.Handler, method string, path string, unwanted int) {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	if recorder.Code == unwanted {
		t.Fatalf("%s %s status = %d, want anything else", method, path, recorder.Code)
	}
}

// Form sign-in is OFF by default (ELITEA_FORM_LOGIN_ENABLED). With it off,
// production authentication still composes — the gateway edge, PAT
// validation and the runtime's forwarded-identity check all depend on it — but
// no Form browser route is mounted, so nothing accepts a password.
func TestEdgeOnlyProductionAuthMountsNoFormSignIn(t *testing.T) {
	main := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	routes, err := NewEdgeOnlyProductionAuthRoutes(main)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewEdgeOnlyProductionAuthRoutes(nil); err == nil {
		t.Fatal("an edge-only composition without the edge handler was accepted")
	}

	t.Run("no single sign-on: every Form path is absent", func(t *testing.T) {
		router := NewRouter(RouterConfig{ProductionAuth: routes})
		for _, path := range []string{"/auth/login", "/auth/form/login", "/auth/logout", "/auth/form/logout"} {
			assertStatus(t, router, http.MethodGet, path, http.StatusNotFound)
		}
		assertStatus(t, router, http.MethodPost, "/auth/form/authorize", http.StatusNotFound)
		assertStatus(t, router, http.MethodGet, "/internal/auth/main", http.StatusNoContent)
	})

	t.Run("single sign-on still owns the prefix", func(t *testing.T) {
		router := NewRouter(RouterConfig{
			ProductionAuth: routes,
			Auth:           AuthDeps{SessionHandler: v2auth.NewSessionHandler(nil, "test-session-secret")},
		})
		assertStatus(t, router, http.MethodPost, "/auth/form/authorize", http.StatusNotFound)
		assertNotStatus(t, router, http.MethodGet, "/auth/logout", http.StatusNotFound)
		assertStatus(t, router, http.MethodGet, "/internal/auth/main", http.StatusNoContent)
	})
}
