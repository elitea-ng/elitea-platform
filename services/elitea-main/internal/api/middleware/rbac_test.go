package middleware_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

type permissionResolverFunc func(
	context.Context,
	auth.User,
	string,
	string,
) (auth.PermissionResolution, error)

func (f permissionResolverFunc) ResolvePermissions(
	ctx context.Context,
	principal auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	return f(ctx, principal, mode, projectID)
}

func TestRequirePermissions_NoUser(t *testing.T) {
	handler := middleware.RequirePermissions("admin")(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler should not be called")
		}),
	)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestRequirePermissions_InsufficientPerms(t *testing.T) {
	handler := middleware.RequirePermissions("admin.users.delete")(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler should not be called")
		}),
	)

	req := httptest.NewRequest("GET", "/", nil)
	ctx := auth.ContextWithUser(req.Context(), auth.User{
		ID:          "user-1",
		Permissions: []string{"apps.view"},
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
}

func TestRequirePermissions_ExactMatch(t *testing.T) {
	handler := middleware.RequirePermissions("apps.edit")(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest("GET", "/", nil)
	ctx := auth.ContextWithUser(req.Context(), auth.User{
		ID:          "user-1",
		Permissions: []string{"apps.edit"},
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestRequirePermissions_ParentPermissionDoesNotMatch(t *testing.T) {
	// Legacy has_access uses exact set intersection. A stored child permission
	// does not implicitly grant a parent permission.
	handler := middleware.RequirePermissions("a.b")(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler should not be called")
		}),
	)

	req := httptest.NewRequest("GET", "/", nil)
	ctx := auth.ContextWithUser(req.Context(), auth.User{
		ID:          "user-1",
		Permissions: []string{"a.b.c.view"},
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 (exact permission matching), got %d", rec.Code)
	}
}

func TestRequirePermissions_NoFalsePrefix(t *testing.T) {
	// Requiring a similar prefix still fails exact matching.
	handler := middleware.RequirePermissions("apps.editor")(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler should not be called")
		}),
	)

	req := httptest.NewRequest("GET", "/", nil)
	ctx := auth.ContextWithUser(req.Context(), auth.User{
		ID:          "user-1",
		Permissions: []string{"apps.edit"},
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 (no prefix matching), got %d", rec.Code)
	}
}

func TestRequirePermissions_MultipleRequired_OneMatches(t *testing.T) {
	// set intersection: ANY match is sufficient
	handler := middleware.RequirePermissions("admin.delete", "apps.view")(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest("GET", "/", nil)
	ctx := auth.ContextWithUser(req.Context(), auth.User{
		ID:          "user-1",
		Permissions: []string{"apps.view"},
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 (intersection has apps.view), got %d", rec.Code)
	}
}

func TestRequireResolvedPermissionsUsesDatabaseResolutionAndPropagatesOwner(t *testing.T) {
	resolver := permissionResolverFunc(func(
		_ context.Context,
		principal auth.User,
		mode string,
		projectID string,
	) (auth.PermissionResolution, error) {
		if principal.TokenID != "900" || mode != auth.PermissionModeDefault || projectID != "7" {
			t.Fatalf("unexpected resolution input: principal=%+v mode=%q project=%q", principal, mode, projectID)
		}
		return auth.PermissionResolution{
			UserID:      42,
			Permissions: []string{"configurations.configuration.create"},
		}, nil
	})

	router := chi.NewRouter()
	router.With(middleware.RequireResolvedPermissions(
		resolver,
		auth.PermissionModeDefault,
		"configurations.configuration.create",
	)).Post("/{projectID}", func(w http.ResponseWriter, r *http.Request) {
		user, ok := auth.UserFromContext(r.Context())
		if !ok {
			t.Fatal("resolved user missing from context")
		}
		ownerID, ok := user.OwningUserID()
		if !ok || ownerID != 42 {
			t.Fatalf("resolved owner = (%d, %v), want (42, true)", ownerID, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/7", nil)
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{
		ID:       "900",
		TokenID:  "900",
		AuthType: "token",
	}))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestRequireResolvedPermissionsFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name       string
		resolver   auth.PermissionResolver
		withUser   bool
		wantStatus int
	}{
		{
			name: "missing authenticated user",
			resolver: permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{}, nil
			}),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing resolver",
			withUser:   true,
			wantStatus: http.StatusForbidden,
		},
		{
			// DEFECT: the middleware collapsed a resolver error into the same
			// 403 branch as a refusal (`if err != nil || !hasIntersection(...)`).
			// A saturated pool or a query timeout then reached the user as
			// "insufficient permissions" on every screen. It reached the
			// operator as 403 answers with no 5xx and no error rate alert.
			// A database failure is an infrastructure failure. It is 500.
			name:     "resolver failure is an infrastructure failure",
			withUser: true,
			resolver: permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{}, errors.New("database unavailable")
			}),
			wantStatus: http.StatusInternalServerError,
		},
		{
			// A REFUSAL still answers 403. The resolver reports it with the
			// auth.ErrPermissionDenied sentinel.
			name:     "resolver refusal stays 403",
			withUser: true,
			resolver: permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{}, auth.ErrPermissionDenied
			}),
			wantStatus: http.StatusForbidden,
		},
		{
			// A wrapped sentinel is still a refusal.
			name:     "wrapped resolver refusal stays 403",
			withUser: true,
			resolver: permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{}, fmt.Errorf("resolve: %w", auth.ErrPermissionDenied)
			}),
			wantStatus: http.StatusForbidden,
		},
		{
			name:     "exact permission missing",
			withUser: true,
			resolver: permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{UserID: 1, Permissions: []string{"configurations.configuration.details"}}, nil
			}),
			wantStatus: http.StatusForbidden,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Mount on a chi route that carries {projectID}. Without the URL
			// param the extractor refuses the request BEFORE the resolver
			// runs. Every case then answers 403, and the table proves
			// nothing about the resolver.
			router := chi.NewRouter()
			router.With(middleware.RequireResolvedPermissions(
				test.resolver,
				auth.PermissionModeDefault,
				"configurations.configuration.create",
			)).Post("/{projectID}", func(http.ResponseWriter, *http.Request) {
				t.Fatal("protected handler should not run")
			})
			req := httptest.NewRequest(http.MethodPost, "/7", nil)
			if test.withUser {
				req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "1"}))
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, test.wantStatus)
			}
		})
	}
}

/* ── RequireCentralPermissions (unit A14) ──────────────────────────────── */

// centralRoute mounts the middleware on a route shaped like the admin panel's:
// a `{mode}` param and NO `{projectID}`.
func centralRoute(
	t *testing.T,
	resolver auth.PermissionResolver,
	required ...string,
) (chi.Router, *bool) {
	t.Helper()
	reached := false
	router := chi.NewRouter()
	router.With(middleware.RequireCentralPermissions(
		resolver, auth.PermissionModeAdministration, required...,
	)).Post("/admin/auth_users/{mode}", func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	return router, &reached
}

func centralRequest(router chi.Router, principal *auth.User) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/admin/auth_users/administration", nil)
	if principal != nil {
		req = req.WithContext(auth.ContextWithUser(req.Context(), *principal))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestRequireCentralPermissions_AllowsWhenTheResolverGrantsIt(t *testing.T) {
	var seenMode, seenProject string
	resolver := permissionResolverFunc(func(
		_ context.Context, _ auth.User, mode, projectID string,
	) (auth.PermissionResolution, error) {
		seenMode, seenProject = mode, projectID
		return auth.PermissionResolution{UserID: 7, Permissions: []string{"admin.auth.users"}}, nil
	})

	router, reached := centralRoute(t, resolver, "admin.auth.users")
	rec := centralRequest(router, &auth.User{ID: "7", UserID: "7"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !*reached {
		t.Fatal("handler was not reached")
	}
	if seenMode != auth.PermissionModeAdministration {
		t.Errorf("resolver saw mode %q, want administration", seenMode)
	}
	// The point of this middleware: it resolves with an EMPTY project id, which
	// is what the central modes expect and what RequireResolvedPermissions
	// refuses to produce.
	if seenProject != "" {
		t.Errorf("resolver saw projectID %q, want the empty string", seenProject)
	}
}

func TestRequireCentralPermissions_DeniesWithoutThePermission(t *testing.T) {
	resolver := permissionResolverFunc(func(
		_ context.Context, _ auth.User, _, _ string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 7, Permissions: []string{"projects.projects"}}, nil
	})

	router, reached := centralRoute(t, resolver, "admin.auth.users")
	rec := centralRequest(router, &auth.User{ID: "7", UserID: "7"})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if *reached {
		t.Fatal("handler ran despite a missing permission")
	}
}

func TestRequireCentralPermissions_DeniesWithoutAPrincipalOrResolver(t *testing.T) {
	granting := permissionResolverFunc(func(
		_ context.Context, _ auth.User, _, _ string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 7, Permissions: []string{"admin.auth.users"}}, nil
	})

	router, reached := centralRoute(t, granting, "admin.auth.users")
	if rec := centralRequest(router, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", rec.Code)
	}
	if *reached {
		t.Fatal("handler ran for an anonymous request")
	}

	// Fail closed: a nil resolver must not read as "no checks configured".
	nilRouter, nilReached := centralRoute(t, nil, "admin.auth.users")
	if rec := centralRequest(nilRouter, &auth.User{ID: "7", UserID: "7"}); rec.Code != http.StatusForbidden {
		t.Fatalf("nil-resolver status = %d, want 403", rec.Code)
	}
	if *nilReached {
		t.Fatal("handler ran with no resolver configured")
	}
}

// DEFECT: the central variant shares one body with the project variant, so it
// reported a database failure as 403 too. It must answer 500 and must still
// keep the protected handler closed.
func TestRequireCentralPermissions_ReportsAResolverFailureAs500(t *testing.T) {
	resolver := permissionResolverFunc(func(
		_ context.Context, _ auth.User, _, _ string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{}, errors.New("database down")
	})

	router, reached := centralRoute(t, resolver, "admin.auth.users")
	if rec := centralRequest(router, &auth.User{ID: "7", UserID: "7"}); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if *reached {
		t.Fatal("handler ran after a resolver error")
	}
}

func TestRequireCentralPermissions_ReportsARefusalAs403(t *testing.T) {
	resolver := permissionResolverFunc(func(
		_ context.Context, _ auth.User, _, _ string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{}, auth.ErrPermissionDenied
	})

	router, reached := centralRoute(t, resolver, "admin.auth.users")
	if rec := centralRequest(router, &auth.User{ID: "7", UserID: "7"}); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if *reached {
		t.Fatal("handler ran after a refusal")
	}
}

// A canceled client request must not become a 5xx. The SPA aborts requests as
// a matter of course, so a blanket "error -> 500" rule would replace a silent
// failure with an error rate storm.
func TestRequireResolvedPermissions_CanceledRequestGetsNo500(t *testing.T) {
	resolver := permissionResolverFunc(func(
		ctx context.Context, _ auth.User, _, _ string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{}, ctx.Err()
	})

	router := chi.NewRouter()
	router.With(middleware.RequireResolvedPermissions(
		resolver,
		auth.PermissionModeDefault,
		"configurations.configuration.create",
	)).Post("/{projectID}", func(http.ResponseWriter, *http.Request) {
		t.Fatal("protected handler should not run")
	})

	ctx, cancel := context.WithCancel(auth.ContextWithUser(context.Background(), auth.User{ID: "1"}))
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/7", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("status = %d, want anything but 500 for a canceled client request", rec.Code)
	}
}

// membershipResolverFunc is the resolver shape the membership gate asks: no
// project travels with the question.
type membershipResolverFunc func(context.Context, auth.User, string) (auth.PermissionResolution, error)

func (f membershipResolverFunc) ResolveMembershipPermissions(
	ctx context.Context,
	principal auth.User,
	mode string,
) (auth.PermissionResolution, error) {
	return f(ctx, principal, mode)
}

// The membership gate admits a caller who holds the permission somewhere, and
// refuses one whose union is empty. It reads no {projectID} URL parameter, so
// it works on a route that has none — which is the point: the project list's
// path project is the PUBLIC project, not the caller's (#830).
func TestRequireResolvedMembershipPermissions(t *testing.T) {
	for name, test := range map[string]struct {
		resolver   auth.MembershipPermissionResolver
		user       *auth.User
		wantStatus int
		wantRun    bool
	}{
		"holder is admitted": {
			resolver: membershipResolverFunc(func(
				_ context.Context, _ auth.User, mode string,
			) (auth.PermissionResolution, error) {
				if mode != auth.PermissionModeDefault {
					t.Fatalf("mode = %q", mode)
				}
				return auth.PermissionResolution{
					UserID:      9,
					Permissions: []string{"projects.projects.project.view"},
				}, nil
			}),
			user:       &auth.User{ID: "9", UserID: "9"},
			wantStatus: http.StatusOK,
			wantRun:    true,
		},
		"empty union is refused": {
			resolver: membershipResolverFunc(func(
				context.Context, auth.User, string,
			) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{UserID: 8, Permissions: []string{}}, nil
			}),
			user:       &auth.User{ID: "8", UserID: "8"},
			wantStatus: http.StatusForbidden,
		},
		"refusal is 403, not 500": {
			resolver: membershipResolverFunc(func(
				context.Context, auth.User, string,
			) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{}, auth.ErrPermissionDenied
			}),
			user:       &auth.User{ID: "8", UserID: "8"},
			wantStatus: http.StatusForbidden,
		},
		"database failure is 500, not 403": {
			resolver: membershipResolverFunc(func(
				context.Context, auth.User, string,
			) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{}, errors.New("connection refused")
			}),
			user:       &auth.User{ID: "8", UserID: "8"},
			wantStatus: http.StatusInternalServerError,
		},
		// Fail closed: a route composed without a resolver refuses everyone
		// rather than running ungated.
		"absent resolver refuses": {
			user:       &auth.User{ID: "9", UserID: "9"},
			wantStatus: http.StatusForbidden,
		},
		"no principal is 401": {
			resolver: membershipResolverFunc(func(
				context.Context, auth.User, string,
			) (auth.PermissionResolution, error) {
				t.Fatal("resolver asked without a principal")
				return auth.PermissionResolution{}, nil
			}),
			wantStatus: http.StatusUnauthorized,
		},
	} {
		t.Run(name, func(t *testing.T) {
			ran := false
			gate := middleware.RequireResolvedMembershipPermissions(
				test.resolver,
				auth.PermissionModeDefault,
				"projects.projects.project.view",
			)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				ran = true
				user, _ := auth.UserFromContext(r.Context())
				if user.UserID != "9" {
					t.Fatalf("resolved user id = %q, want the resolver's answer", user.UserID)
				}
			}))

			request := httptest.NewRequest(http.MethodGet, "/api/v2/projects/project/default/1", nil)
			if test.user != nil {
				request = request.WithContext(auth.ContextWithUser(request.Context(), *test.user))
			}
			recorder := httptest.NewRecorder()
			gate.ServeHTTP(recorder, request)

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if ran != test.wantRun {
				t.Fatalf("handler ran = %t, want %t", ran, test.wantRun)
			}
		})
	}
}
