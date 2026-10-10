package social_test

// DEFECT: a request for a project that does not exist answered 500.
//
// `RequireProjectAccess` asked only "may this user enter?". Its central
// administrator branch matches on a role row, not on the project, so it
// admitted a super_admin to EVERY project id. The request then reached
// the old tenant-table feedback list, which built the schema name `p_<id>` and ran the query, and
// PostgreSQL answered SQLSTATE 3F000 (invalid_schema_name). The handler maps
// every query error to `500 {"error":"failed to list feedback"}`.
//
// Live reproduction before the fix: GET /api/v2/social/feedbacks/default/1
// answered 200 {"items":[],"total":0} and /999999 answered 500, although the
// server understood the request perfectly. The condition is reachable in
// normal use: DeleteProject drops the tenant schema and clears
// auth_core__project_user_role, so after a real delete an ordinary member
// correctly reads 403 while a platform administrator with a stale tab read
// 500. A 5xx for a known client condition also denies the SPA any way to tell
// "project gone" from "server broken".
//
// The middleware now answers the project-existence question in the same query
// and returns 404. The membership refusal stays FIRST, so the status is not an
// id-enumeration oracle for a non-member.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	infradb "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

func TestFeedbacksAnswer404ForAProjectThatDoesNotExist(t *testing.T) {
	pool := newSocialFeedbackPool(t)
	adminID := seedPlatformAdministrator(t, pool)

	routes := handler.NewHandler(pool, handler.WithPermissionResolver(feedbackGrantAll{})).Routes()
	do := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request = request.WithContext(auth.ContextWithUser(request.Context(),
			auth.User{ID: fmt.Sprintf("%d", adminID)}))
		recorder := httptest.NewRecorder()
		routes.ServeHTTP(recorder, request)
		return recorder
	}

	// The control: project 1 exists and the administrator reads it.
	if recorder := do("/feedbacks/default/1"); recorder.Code != http.StatusOK {
		t.Fatalf("existing project status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	// The defect: the same caller, a project id that has no row.
	recorder := do("/feedbacks/default/999999")
	if recorder.Code == http.StatusInternalServerError {
		t.Fatalf("a project that does not exist answered 500: %s", recorder.Body.String())
	}
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", recorder.Code, recorder.Body.String())
	}
}

// A caller who is not a member reads 403 for an absent project, not 404. The
// opposite order would let a stranger enumerate the project ids that exist.
func TestFeedbacksAnswer403ForANonMemberOfAnAbsentProject(t *testing.T) {
	pool := newSocialFeedbackPool(t)
	seedPlatformAdministrator(t, pool)

	var strangerID int
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO public.auth_core__user (email, name) VALUES ('stranger@autotest.local', 'Stranger') RETURNING id`,
	).Scan(&strangerID); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/feedbacks/default/999999", nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(),
		auth.User{ID: fmt.Sprintf("%d", strangerID)}))
	recorder := httptest.NewRecorder()
	handler.NewHandler(pool, handler.WithPermissionResolver(feedbackGrantAll{})).Routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", recorder.Code, recorder.Body.String())
	}
}

// seedPlatformAdministrator creates one user holding the administration-mode
// super_admin role — the principal that reaches every project.
func seedPlatformAdministrator(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	ctx := context.Background()

	var userID int
	if err := pool.QueryRow(ctx,
		`INSERT INTO public.auth_core__user (email, name) VALUES ('platform-admin@autotest.local', 'Admin') RETURNING id`,
	).Scan(&userID); err != nil {
		t.Fatalf("seed administrator: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, role.id FROM public.auth_core__role role
WHERE role.name = 'super_admin' AND role.mode = 'administration'`, userID); err != nil {
		t.Fatalf("assign super_admin: %v", err)
	}
	var assigned int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM public.auth_core__user_role WHERE user_id = $1`, userID).Scan(&assigned); err != nil {
		t.Fatal(err)
	}
	if assigned != 1 {
		t.Fatalf("fixture holds %d central role rows, want 1", assigned)
	}
	return userID
}

func newSocialFeedbackPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
	databaseName := fmt.Sprintf("elitea_social_fb_it_%d_%d", os.Getpid(), time.Now().UnixNano())
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

	if err := infradb.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}
	// The shared feedback table comes from the shared history, which this
	// helper does not run; apply the one file the feedback routes need.
	feedbackSQL, err := platformmigrations.Files.ReadFile("shared/0158_social_feedbacks_project.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(feedbackSQL)); err != nil {
		t.Fatalf("apply shared feedback migration: %v", err)
	}
	// The project-existence check reads the tombstone column of shared/0160,
	// which this helper does not run either (#1211).
	if _, err := pool.Exec(ctx, `ALTER TABLE centry.project ADD COLUMN IF NOT EXISTS deleting_at TIMESTAMPTZ`); err != nil {
		t.Fatalf("add the project tombstone column: %v", err)
	}
	return pool
}
