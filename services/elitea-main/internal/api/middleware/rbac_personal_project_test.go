package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

const (
	settingsEdit = "models.project_settings.edit"
	contextEdit  = "models.project_context.edit"
)

// grantingUser42 resolves user 42 with the given permissions in project 7 only.
func grantingUser42(permissions ...string) permissionResolverFunc {
	return func(_ context.Context, _ auth.User, _ string, projectID string) (auth.PermissionResolution, error) {
		if projectID != "7" {
			return auth.PermissionResolution{}, auth.ErrPermissionDenied
		}
		return auth.PermissionResolution{UserID: 42, Permissions: permissions}, nil
	}
}

// personalFor answers true for (project 7, user 42) only, and counts its calls.
type personalFor struct {
	calls int
	err   error
}

func (p *personalFor) check(_ context.Context, projectID string, userID int64) (bool, error) {
	p.calls++
	if p.err != nil {
		return false, p.err
	}
	return projectID == "7" && userID == 42, nil
}

func serveSettingsWrite(
	t *testing.T, resolver auth.PermissionResolver, check middleware.PersonalProjectCheck, projectID string,
) int {
	t.Helper()
	router := chi.NewRouter()
	router.With(middleware.RequireResolvedPermissionOrPersonalProject(
		resolver, auth.PermissionModeDefault, check, contextEdit, settingsEdit,
	)).Put("/{projectID}", func(w http.ResponseWriter, r *http.Request) {
		user, _ := auth.UserFromContext(r.Context())
		if owner, ok := user.OwningUserID(); !ok || owner != 42 {
			t.Fatalf("resolved owner = (%d, %v), want (42, true)", owner, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPut, "/"+projectID, nil)
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "42"}))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec.Code
}

func TestPersonalProjectGate_AdminPermissionAdmitsWithoutAnOwnershipLookup(t *testing.T) {
	check := &personalFor{}
	if code := serveSettingsWrite(t, grantingUser42(settingsEdit), check.check, "7"); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}
	if check.calls != 0 {
		t.Fatalf("ownership check ran %d times for a caller the strict permission admits", check.calls)
	}
}

// The team-project case of #6789: an editor holds the context permission but
// not the settings one, and the project is not their personal project.
func TestPersonalProjectGate_EditorIsRefusedOutsideTheirPersonalProject(t *testing.T) {
	notPersonal := func(context.Context, string, int64) (bool, error) { return false, nil }
	if code := serveSettingsWrite(t, grantingUser42(contextEdit), notPersonal, "7"); code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", code)
	}
}

// The personal-project case of #6789: the owner is an `editor` there, and must
// keep the settings of their own project.
func TestPersonalProjectGate_OwnerKeepsTheirPersonalProjectSettings(t *testing.T) {
	check := &personalFor{}
	if code := serveSettingsWrite(t, grantingUser42(contextEdit), check.check, "7"); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}
	if check.calls != 1 {
		t.Fatalf("ownership check ran %d times, want 1", check.calls)
	}
}

// The exception never widens the route past the fallback permission: a viewer
// in their own personal project still holds nothing that lets them write.
func TestPersonalProjectGate_ExceptionStillNeedsTheFallbackPermission(t *testing.T) {
	check := &personalFor{}
	if code := serveSettingsWrite(t, grantingUser42("models.project_context.view"), check.check, "7"); code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", code)
	}
	if check.calls != 0 {
		t.Fatalf("ownership check ran %d times for a caller without the fallback permission", check.calls)
	}
}

func TestPersonalProjectGate_FailsClosed(t *testing.T) {
	// A nil check leaves the strict gate.
	if code := serveSettingsWrite(t, grantingUser42(contextEdit), nil, "7"); code != http.StatusForbidden {
		t.Fatalf("nil check: status = %d, want 403", code)
	}
	// A nil resolver refuses everybody.
	if code := serveSettingsWrite(t, nil, (&personalFor{}).check, "7"); code != http.StatusForbidden {
		t.Fatalf("nil resolver: status = %d, want 403", code)
	}
	// Another project resolves nothing.
	if code := serveSettingsWrite(t, grantingUser42(settingsEdit), (&personalFor{}).check, "8"); code != http.StatusForbidden {
		t.Fatalf("other project: status = %d, want 403", code)
	}
	// A failed lookup is a 500, not a refusal and not an admission.
	failing := &personalFor{err: errors.New("pool exhausted")}
	if code := serveSettingsWrite(t, grantingUser42(contextEdit), failing.check, "7"); code != http.StatusInternalServerError {
		t.Fatalf("failing check: status = %d, want 500", code)
	}
	// No principal is a 401.
	router := chi.NewRouter()
	router.With(middleware.RequireResolvedPermissionOrPersonalProject(
		grantingUser42(settingsEdit), auth.PermissionModeDefault, nil, contextEdit, settingsEdit,
	)).Put("/{projectID}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/7", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no principal: status = %d, want 401", rec.Code)
	}
}

type flagKey struct{}

func TestResolvedPermissionFlag_MarksOnlyAHolder(t *testing.T) {
	mark := func(ctx context.Context) context.Context { return context.WithValue(ctx, flagKey{}, true) }
	serve := func(resolver auth.PermissionResolver, projectID string) (int, bool) {
		marked := false
		router := chi.NewRouter()
		router.With(middleware.ResolvedPermissionFlag(resolver, auth.PermissionModeDefault, mark, settingsEdit)).
			Get("/{projectID}", func(w http.ResponseWriter, r *http.Request) {
				marked, _ = r.Context().Value(flagKey{}).(bool)
				w.WriteHeader(http.StatusNoContent)
			})
		req := httptest.NewRequest(http.MethodGet, "/"+projectID, nil)
		req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "42"}))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code, marked
	}

	if code, marked := serve(grantingUser42(settingsEdit), "7"); code != http.StatusNoContent || !marked {
		t.Fatalf("holder: status = %d, marked = %v; want 204 and marked", code, marked)
	}
	if code, marked := serve(grantingUser42(contextEdit), "7"); code != http.StatusNoContent || marked {
		t.Fatalf("non-holder: status = %d, marked = %v; want 204 and unmarked", code, marked)
	}
	if code, marked := serve(nil, "7"); code != http.StatusNoContent || marked {
		t.Fatalf("nil resolver: status = %d, marked = %v; want 204 and unmarked", code, marked)
	}
}

// A resolver error must not degrade to the narrow scope. The ACL editor saves a
// member's whole map from the list it was given, so a 200 with a silently
// narrowed list would erase other members' exceptions on the next save. The
// request fails instead, and the handler never runs.
func TestResolvedPermissionFlag_FailsOnAResolverError(t *testing.T) {
	mark := func(ctx context.Context) context.Context { return context.WithValue(ctx, flagKey{}, true) }
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"transient fault": {err: errors.New("connection reset"), want: http.StatusInternalServerError},
		"denied":          {err: auth.ErrPermissionDenied, want: http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			resolver := permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{}, tc.err
			})
			reached := false
			router := chi.NewRouter()
			router.With(middleware.ResolvedPermissionFlag(resolver, auth.PermissionModeDefault, mark, settingsEdit)).
				Get("/{projectID}", func(w http.ResponseWriter, _ *http.Request) {
					reached = true
					w.WriteHeader(http.StatusOK)
				})
			req := httptest.NewRequest(http.MethodGet, "/7", nil)
			req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "42"}))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tc.want || reached {
				t.Fatalf("status = %d, handler reached = %v; want %d and not reached", rec.Code, reached, tc.want)
			}
		})
	}
}
