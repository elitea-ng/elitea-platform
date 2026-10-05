package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

// holdsThrough runs check inside a chi route carrying {projectID}, the way a
// handler calls it, and reports its answer.
func holdsThrough(t *testing.T, check func(*http.Request) (bool, error), path string, withUser bool) (bool, error) {
	t.Helper()
	var held bool
	var err error
	router := chi.NewRouter()
	handler := func(_ http.ResponseWriter, r *http.Request) { held, err = check(r) }
	router.Get("/p/{projectID}", handler)
	router.Get("/none", handler)
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if withUser {
		request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "user-1"}))
	}
	router.ServeHTTP(httptest.NewRecorder(), request)
	return held, err
}

func TestHoldsResolvedPermission(t *testing.T) {
	t.Parallel()

	var sawProject, sawMode string
	resolver := permissionResolverFunc(func(_ context.Context, _ auth.User, mode, projectID string) (auth.PermissionResolution, error) {
		sawProject, sawMode = projectID, mode
		switch projectID {
		case "1":
			return auth.PermissionResolution{UserID: 1, Permissions: []string{"x.update", "x.create"}}, nil
		case "2":
			return auth.PermissionResolution{UserID: 1, Permissions: []string{"x.update"}}, nil
		case "3":
			return auth.PermissionResolution{}, auth.ErrPermissionDenied
		default:
			return auth.PermissionResolution{}, errors.New("database down")
		}
	})
	check := middleware.HoldsResolvedPermission(resolver, "default", "x.create")

	if held, err := holdsThrough(t, check, "/p/1", true); !held || err != nil {
		t.Errorf("holder: got %v %v, want true", held, err)
	}
	if sawProject != "1" || sawMode != "default" {
		t.Errorf("resolved project %q mode %q, want the route's project in default mode", sawProject, sawMode)
	}
	if held, err := holdsThrough(t, check, "/p/2", true); held || err != nil {
		t.Errorf("update-only role: got %v %v, want false", held, err)
	}
	if held, err := holdsThrough(t, check, "/p/3", true); held || err != nil {
		t.Errorf("permission denied: got %v %v, want false and no error", held, err)
	}
	if _, err := holdsThrough(t, check, "/p/4", true); err == nil {
		t.Error("a failed resolution must be an error, not a no")
	}
	if held, _ := holdsThrough(t, check, "/p/1", false); held {
		t.Error("no principal: want false")
	}
	if held, _ := holdsThrough(t, check, "/none", true); held {
		t.Error("no project in the route: want false")
	}
	if held, _ := holdsThrough(t, middleware.HoldsResolvedPermission(nil, "default", "x.create"), "/p/1", true); held {
		t.Error("nil resolver: want false")
	}
}
