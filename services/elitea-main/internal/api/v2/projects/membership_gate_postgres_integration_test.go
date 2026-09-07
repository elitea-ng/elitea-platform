package projects_test

// The project-list route's authorization gate, against a real PostgreSQL and
// the real legacyrbac resolver (#830).
//
// # What this file pins
//
// The route answers with the caller's OWN projects: ListCurrentUserProjects
// joins public.auth_core__project_user_role on the caller. Its gate, however,
// resolved `projects.projects.project.view` against the project standing in the
// path — the public project 1. A fresh account is never enrolled there: the
// Go sign-in path provisions `project_user_<id>` and nothing else. So the whole
// list answered 403 `insufficient permissions`, and the switcher read
// "No projects" for every account but the ones seeded into project 1.
//
// The cases below are run through the composed production route, so what they
// observe is the gate and not the handler. The fixtures reproduce the
// deployment shape that made the defect visible: 0081_project_permissions.sql's
// CENTRAL default-mode grants, no per-project grant rows, one public project
// with its own members, and personal projects beside it.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/projects"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/schema"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

func TestCurrentProjectListGateFollowsTheCallersOwnMemberships(t *testing.T) {
	pool := newProjectListGatePool(t)
	seedProjectListGateFixtures(t, pool)
	route := newProjectListGateRoute(t, pool)

	for _, test := range []struct {
		name       string
		userID     string
		wantStatus int
		wantIDs    []int32
	}{
		{
			// The reported case. User 9 is a member of its personal project
			// only, exactly as first sign-in leaves it.
			name:       "personal project only",
			userID:     "9",
			wantStatus: http.StatusOK,
			wantIDs:    []int32{5},
		},
		{
			name:       "two memberships list both projects",
			userID:     "7",
			wantStatus: http.StatusOK,
			wantIDs:    []int32{3, 4},
		},
		{
			// The gate is not weaker than it was. A user with no membership
			// resolves to an empty permission set, which is a 403.
			name:       "no membership at all is refused",
			userID:     "8",
			wantStatus: http.StatusForbidden,
		},
		{
			// The path that already worked must keep working: a member of the
			// public project still lists it.
			name:       "public project member is unaffected",
			userID:     "5",
			wantStatus: http.StatusOK,
			wantIDs:    []int32{1},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			route.ServeHTTP(recorder, projectListGateRequest(test.userID))

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)",
					recorder.Code, test.wantStatus, strings.TrimSpace(recorder.Body.String()))
			}
			if test.wantStatus != http.StatusOK {
				if body := strings.TrimSpace(recorder.Body.String()); body != `{"error":"insufficient permissions"}` {
					t.Fatalf("refusal body = %s", body)
				}
				return
			}

			var projects []struct {
				ID int32 `json:"id"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &projects); err != nil {
				t.Fatalf("decode %s: %v", recorder.Body.String(), err)
			}
			if len(projects) != len(test.wantIDs) {
				t.Fatalf("projects = %s, want ids %v", recorder.Body.String(), test.wantIDs)
			}
			for index, want := range test.wantIDs {
				if projects[index].ID != want {
					t.Fatalf("projects = %s, want ids %v", recorder.Body.String(), test.wantIDs)
				}
			}
		})
	}
}

// An unauthenticated caller is still refused before the resolver is asked. The
// gate change moves the authorization question, not the authentication one.
func TestCurrentProjectListGateStillRefusesAnUnauthenticatedCaller(t *testing.T) {
	pool := newProjectListGatePool(t)
	seedProjectListGateFixtures(t, pool)
	route := newProjectListGateRoute(t, pool)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, handler.CurrentProjectListPath, nil)
	request.RemoteAddr = projectListGatePeer
	route.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %s)", recorder.Code, strings.TrimSpace(recorder.Body.String()))
	}
}

// A suspended project grants nothing, which is the rule requireActiveProject
// enforces on the single-project path. Without it, membership of a suspended
// project would be enough to pass the gate.
func TestCurrentProjectListGateIgnoresASuspendedMembership(t *testing.T) {
	pool := newProjectListGatePool(t)
	seedProjectListGateFixtures(t, pool)
	if _, err := pool.Exec(context.Background(),
		`UPDATE centry.project SET suspended = true WHERE id = 5`); err != nil {
		t.Fatal(err)
	}
	route := newProjectListGateRoute(t, pool)

	recorder := httptest.NewRecorder()
	route.ServeHTTP(recorder, projectListGateRequest("9"))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", recorder.Code, strings.TrimSpace(recorder.Body.String()))
	}
}

/* ── harness ───────────────────────────────────────────────────────────── */

const projectListGatePeer = "10.0.0.8:43120"

func projectListGateRequest(userID string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, handler.CurrentProjectListPath, nil)
	request.RemoteAddr = projectListGatePeer
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", userID)
	return request
}

func newProjectListGateRoute(t *testing.T, pool *pgxpool.Pool) *handler.CurrentProjectListRoute {
	t.Helper()
	route, err := handler.NewCurrentProjectListRoute(
		sqlcgen.New(pool),
		apimw.AuthConfig{
			PrincipalValidator: principalValidatorFunc(
				func(_ context.Context, user auth.User) (auth.User, error) { return user, nil },
			),
			ForwardedIdentityVerifier: forwardedPeerVerifierFunc(func(*http.Request) error { return nil }),
		},
		legacyrbac.NewPostgresResolver(pool),
	)
	if err != nil {
		t.Fatalf("compose current project-list route: %v", err)
	}
	return route
}

// seedProjectListGateFixtures reproduces a Go-provisioned install after
// 0081_project_permissions.sql: default-mode roles carrying the central grants,
// NO per-project grant rows, a public project whose members were seeded at
// install time, and personal or team projects belonging to everyone else.
func seedProjectListGateFixtures(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success) VALUES
			(1, 'promptlib_public', 5, '{}', true),
			(3, 'team_alpha', 7, '{}', true),
			(4, 'team_beta', 7, '{}', true),
			(5, 'project_user_9', 9, '{}', true)`,
		`INSERT INTO public.auth_core__user (id, email, name) VALUES
			(5, 'owner@example.com', 'Owner'),
			(7, 'two@example.com', 'Two'),
			(8, 'nobody@example.com', 'Nobody'),
			(9, 'alice@example.com', 'Alice')`,
		`INSERT INTO public.auth_core__role (id, name, mode) VALUES
			(1, 'admin', 'default'), (2, 'editor', 'default'), (3, 'viewer', 'default')`,
		// 0081_project_permissions.sql's widest grant, the one the gate reads.
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT id, 'projects.projects.project.view' FROM public.auth_core__role WHERE mode = 'default'`,
		`INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES
			(10, 1, 'admin'),
			(11, 3, 'editor'),
			(12, 4, 'editor'),
			(13, 5, 'admin')`,
		`INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES
			(1, 5, 10),
			(3, 7, 11),
			(4, 7, 12),
			(5, 9, 13)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

func newProjectListGatePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_project_list_gate_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	for _, projection := range []string{
		schema.CentryProjectsBaselineSQLCProjection,
		schema.AuthCoreBaselineSQLCProjection,
	} {
		if _, err := pool.Exec(ctx, projection); err != nil {
			t.Fatalf("apply schema projection: %v", err)
		}
	}
	return pool
}
