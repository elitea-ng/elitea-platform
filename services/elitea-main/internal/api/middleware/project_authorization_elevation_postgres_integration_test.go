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
// Two halves:
//
//   - now: a user with no standing role, and a holder of a custom role named
//     differently that holds every permission of super_admin, both get 403
//     on a project they are not a member of; the standing super_admin gets
//     through (the control);
//   - with R2.1 (#2515): a user with an active super_admin-level elevation
//     and no role gets 403. Until R2.1 creates elitea_auth.admin_elevations
//     there is nothing to insert, so a tripwire stands in its place: it
//     passes while the table is absent and FAILS the run that creates it,
//     which must replace it with the elevation case. It never skips (a
//     skip is no coverage, scripts/go/declared-skips.txt).
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

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
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

	expectProjectAccess(t, pool, "r013-standing@autotest.local", http.StatusOK)
	expectProjectAccess(t, pool, "r013-none@autotest.local", http.StatusForbidden)
	expectProjectAccess(t, pool, "r013-copy@autotest.local", http.StatusForbidden)

	// The R2.1 tripwire (see the header).
	var elevationsExist bool
	if err := pool.QueryRow(t.Context(),
		`SELECT to_regclass('elitea_auth.admin_elevations') IS NOT NULL`).Scan(&elevationsExist); err != nil {
		t.Fatalf("look for the elevations table: %v", err)
	}
	if elevationsExist {
		t.Fatal("elitea_auth.admin_elevations exists: R2.1 (#2515) replaces this tripwire with an active " +
			"super_admin-level elevation of r013-none@autotest.local, and expects 403 for it")
	}
}

func expectProjectAccess(t *testing.T, pool *pgxpool.Pool, email string, want int) {
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
	principal := auth.User{ID: fmt.Sprint(id), UserID: fmt.Sprint(id), CredentialKind: auth.CredentialKindSession}
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
