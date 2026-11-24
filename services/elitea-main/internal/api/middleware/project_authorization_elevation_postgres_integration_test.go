package middleware_test

// The project access check keeps the role NAME super_admin (ADR-0059 decision
// 7, "Checks by role name", and decision 5, "Break-glass"; story R0.13).
//
// RequireProjectAccess admits a member of the project, or a holder of the
// standing administration role named super_admin (projectaccess.Membership).
// That name test is kept on purpose: an elevation gives permissions, not the
// role's name, so break-glass repairs the platform and does not open project
// content. Nor does a custom role, whatever it holds. Do not "fix" the check
// to a permission.
//
// The cases: a user with no standing role, and a holder of a custom role
// named differently that holds every permission of super_admin, both get 403
// on a project they are not a member of; the standing super_admin gets
// through (the control). A user with no role and an ACTIVE super_admin-level
// elevation bound to the session of the request gets 403 too (R2.1a, #2515).
// The elevation is written by the ledger itself (adminaccess.StartElevation),
// so the case keeps holding once the resolver adds the elevated set (R2.1b,
// #2595).
//
// It runs on the real schema: 001_initial.sql and the shared corpus.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	migrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const elevationTestProject = 4242

func TestProjectAccessKeepsTheStandingSuperAdminName(t *testing.T) {
	pool := newProjectAccessDatabase(t)
	for _, statement := range []string{
		fmt.Sprintf(`INSERT INTO centry.project (id, name, owner_id) VALUES (%d, 'r013 project', 1)`, elevationTestProject),
		`INSERT INTO public.auth_core__user (email, name) VALUES
			('r013-standing@autotest.local', 'Standing'), ('r013-none@autotest.local', 'None'),
			('r013-copy@autotest.local', 'Copy')`,
		// A custom role named differently that holds every permission of
		// super_admin, in every mode the database defines super_admin.
		`INSERT INTO public.auth_core__role (name, mode)
			SELECT 'r013_full_copy', mode FROM public.auth_core__role WHERE name = 'super_admin'`,
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT copy.id, grant_row.permission
			FROM public.auth_core__role super
			JOIN public.auth_core__role_permission grant_row ON grant_row.role_id = super.id
			JOIN public.auth_core__role copy ON copy.name = 'r013_full_copy' AND copy.mode = super.mode
			WHERE super.name = 'super_admin'`,
		`INSERT INTO public.auth_core__user_role (user_id, role_id)
			SELECT account.id, role.id FROM public.auth_core__user account
			JOIN public.auth_core__role role ON role.mode = 'administration'
			WHERE (account.email = 'r013-standing@autotest.local' AND role.name = 'super_admin')
			   OR (account.email = 'r013-copy@autotest.local' AND role.name = 'r013_full_copy')`,
	} {
		if _, err := pool.Exec(t.Context(), statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	var copied, held int
	if err := pool.QueryRow(t.Context(), `
SELECT (SELECT COUNT(*) FROM public.auth_core__role_permission p JOIN public.auth_core__role r ON r.id = p.role_id
        WHERE r.name = 'r013_full_copy' AND r.mode = 'administration'),
       (SELECT COUNT(*) FROM public.auth_core__role_permission p JOIN public.auth_core__role r ON r.id = p.role_id
        WHERE r.name = 'super_admin' AND r.mode = 'administration')`).Scan(&copied, &held); err != nil {
		t.Fatalf("count the copied set: %v", err)
	}
	if held == 0 || copied != held {
		t.Fatalf("premise: the custom role holds %d of the %d permissions of super_admin", copied, held)
	}

	expectProjectAccess(t, pool, "r013-standing@autotest.local", 0, http.StatusOK)
	expectProjectAccess(t, pool, "r013-none@autotest.local", 0, http.StatusForbidden)
	expectProjectAccess(t, pool, "r013-copy@autotest.local", 0, http.StatusForbidden)

	// An active super_admin-level elevation of the user with no role, bound to
	// the session the request comes from: permissions, not the role name.
	const elevatedSession = 913001
	var noneID, approverID int64
	if err := pool.QueryRow(t.Context(), `
SELECT (SELECT id FROM public.auth_core__user WHERE email = 'r013-none@autotest.local'),
       (SELECT id FROM public.auth_core__user WHERE email = 'r013-standing@autotest.local')`).Scan(&noneID, &approverID); err != nil {
		t.Fatalf("read the elevated user: %v", err)
	}
	appender, err := securityaudit.NewPostgresAppender("elitea-main/r013-test")
	if err != nil {
		t.Fatalf("appender: %v", err)
	}
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		_, err := adminaccess.StartElevation(t.Context(), tx, appender, adminaccess.ElevationStart{
			UserID:            noneID,
			BrowserSessionRef: elevatedSession,
			Level:             adminaccess.ElevationLevelSuperAdmin,
			Path:              adminaccess.ElevationPathApproved,
			ReasonCode:        adminaccess.ElevationReasonIncident,
			Reason:            "R0.13: an elevation does not open project content",
			ApproverUserID:    approverID,
			At:                time.Now().Add(-time.Minute),
			Duration:          time.Hour,
		})
		return err
	}); err != nil {
		t.Fatalf("start the elevation: %v", err)
	}
	var active bool
	if err := pool.QueryRow(t.Context(), `
SELECT EXISTS (SELECT 1 FROM elitea_auth.admin_elevations
               WHERE user_id = $1 AND browser_session_ref = $2 AND level = 'super_admin'
                 AND started_at IS NOT NULL AND ended_at IS NULL AND expires_at > now())`,
		noneID, elevatedSession).Scan(&active); err != nil {
		t.Fatalf("read the elevation: %v", err)
	}
	if !active {
		t.Fatal("premise: the super_admin-level elevation is not active")
	}
	expectProjectAccess(t, pool, "r013-none@autotest.local", elevatedSession, http.StatusForbidden)
}

// expectProjectAccess sends the request as a browser session of the user;
// sessionRef is that session's reference (0: none).
func expectProjectAccess(t *testing.T, pool *pgxpool.Pool, email string, sessionRef int64, want int) {
	t.Helper()
	var id int
	if err := pool.QueryRow(t.Context(),
		`SELECT id FROM public.auth_core__user WHERE email = $1`, email).Scan(&id); err != nil {
		t.Fatalf("read %s: %v", email, err)
	}
	router := chi.NewRouter()
	router.With(apimw.RequireProjectAccess(pool)).Get("/p/{projectID}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	principal := auth.User{ID: fmt.Sprint(id), UserID: fmt.Sprint(id), CredentialKind: auth.CredentialKindSession,
		BrowserSessionRef: sessionRef}
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/p/%d", elevationTestProject), nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(), principal))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != want {
		t.Fatalf("%s on project %d: %d %s, want %d", email, elevationTestProject,
			recorder.Code, recorder.Body.String(), want)
	}
}

// newProjectAccessDatabase is an isolated database with 001_initial.sql and
// the shared corpus applied.
func newProjectAccessDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	databaseName := fmt.Sprintf("elitea_project_access_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s: the DROP queues behind the CREATE DATABASE calls of every
		// package `go test ./...` runs at the same time (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		dropPool, err := pgxpool.New(dropCtx, databaseURL)
		if err != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)")
	})
	bootstrap, err := os.ReadFile("../../infra/db/migrations/001_initial.sql")
	if err != nil {
		t.Fatalf("read the bootstrap schema: %v", err)
	}
	if _, err := pool.Exec(ctx, string(bootstrap)); err != nil {
		t.Fatalf("apply the bootstrap schema: %v", err)
	}
	if err := migrate.New(pool, migrations.Files).ApplyShared(ctx); err != nil {
		t.Fatalf("apply the shared corpus: %v", err)
	}
	return pool
}
