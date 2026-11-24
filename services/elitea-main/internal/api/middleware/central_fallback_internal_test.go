package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// During a window, a holder of an administration permission that is also a
// member of the project passes the maintenance gate on a central-fallback
// route, and the gate then decides on the fallback alone: the member path is
// the product, which the window closes (ADR-0059 R0.4b with R0.6b).
func TestACentralFallbackRouteServesNoMemberPathDuringAWindow(t *testing.T) {
	resolver := stubResolver{permissions: []string{adminaccess.ProjectsMembersManage, "project.read"}}
	gate := &maintenanceGate{
		load: func(context.Context) (platformconfig.Maintenance, error) {
			return platformconfig.Maintenance{Enabled: true}, nil
		},
		resolver:            resolver,
		administrationRoute: func(*http.Request) bool { return true },
	}
	fallbackAsked := false
	fallback := func(r *http.Request) (*http.Request, bool, error) {
		fallbackAsked = true
		return r, false, nil
	}
	serveOn := func(withGate bool) int {
		router := chi.NewRouter()
		router.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: "1"})))
			})
		})
		if withGate {
			router.Use(gate.middleware)
		}
		router.With(RequireResolvedPermissionsOrCentral(resolver, auth.PermissionModeDefault, fallback, "project.read")).
			Get("/api/v2/x/{projectID}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v2/x/7", nil))
		return rec.Code
	}

	if got := serveOn(false); got != http.StatusOK {
		t.Fatalf("outside a window the member path answers %d, want 200", got)
	}
	if got := serveOn(true); got != http.StatusForbidden {
		t.Fatalf("during a window the member path answers %d, want 403", got)
	}
	if !fallbackAsked {
		t.Fatal("during a window the fallback was not asked")
	}
	if !IsAdministrationGate(RequireResolvedPermissionsOrCentral(resolver, auth.PermissionModeDefault, fallback)) {
		t.Fatal("the gate is not marked as an administration gate")
	}
}
