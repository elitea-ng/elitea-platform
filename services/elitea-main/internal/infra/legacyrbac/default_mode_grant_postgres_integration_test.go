package legacyrbac_test

// The blast radius of the default-mode grant added by
// migrations/shared/0062_budgets_quota_statistics.sql (#246), pinned.
//
// # Why this file exists
//
// That migration grants `models.project_context.view` to the default-mode
// admin/editor/viewer roles, because without it the project-scoped budget and
// usage reads are 403-for-everyone on a Go-bootstrapped database: 001_initial.sql
// seeds default-mode ROLES but not one default-mode role_permission row, so
// projectPermissions()' central fallback has nothing to fall back to.
//
// It is also the FIRST default-mode grant in this codebase, which makes it the
// first time a project member on a fresh database resolves to a NON-EMPTY
// permission set. That matters beyond the permission itself, because one
// production authorization decision keys off the SIZE of that set rather than
// its contents:
//
//	internal/runtimecomposition/public_authorizer.go, AuthorizeExecutionEvents:
//	    case executiondomain.ConfigurationValidationCapability:
//	        if len(resolution.Permissions) == 0 { return ...Forbidden }
//
// That branch is a deliberate placeholder — its own comment says it stands in
// for "project member" until admission persists the originating permission. On
// a fresh database it currently refuses EVERY caller, because every caller
// resolves to the empty set; with the grant it admits any project member, which
// is what it says it means to do. The behaviour change is real either way, so
// it is pinned here rather than left to be discovered.
//
// Breaking that coupling is issue #276: the check should ask the membership
// question directly, as the admission path for the same capability already
// does, so a future default-mode grant cannot move it again. When that lands,
// this file keeps its value — it pins the GRANT's blast radius, not that
// branch's implementation.
//
// The three cases below are the whole blast radius, each verified against a
// real PostgreSQL rather than argued from the query text.

import (
	"context"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	dbschema "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/schema"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

const defaultModeGrant = `
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, 'models.project_context.view'
FROM public.auth_core__role AS role
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING`

// A member of the project GAINS the permission — and, with it, a non-empty
// permission set where they previously had an empty one.
func TestDefaultModeGrantMakesAProjectMemberResolveNonEmpty(t *testing.T) {
	pool := newGrantPool(t)
	seedFreshDatabaseShape(t, pool)
	resolver := legacyrbac.NewPostgresResolver(pool)
	member := auth.User{ID: "1", UserID: "1"}

	before, err := resolver.ResolvePermissions(context.Background(), member, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Permissions) != 0 {
		t.Fatalf("pre-grant permissions = %v, want empty — this test's premise is that a fresh "+
			"database resolves every project member to nothing", before.Permissions)
	}

	applyGrant(t, pool)

	after, err := resolver.ResolvePermissions(context.Background(), member, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Permissions) != 1 || after.Permissions[0] != "models.project_context.view" {
		t.Fatalf("post-grant permissions = %v, want exactly [models.project_context.view]", after.Permissions)
	}
	// Spelled out because it is the coupling, not a restatement: any consumer
	// that reads len(Permissions) as "is a project member" flips here. Today
	// that is public_authorizer.go's ConfigurationValidationCapability branch.
	if len(after.Permissions) == 0 {
		t.Fatal("unreachable")
	}
}

// A user with NO role in the project gains nothing. The central fallback is
// joined through the caller's assigned project roles, so a non-member has no
// row to fall back from — this is what keeps the grant from being a leak
// outside the project.
func TestDefaultModeGrantGivesANonMemberNothing(t *testing.T) {
	pool := newGrantPool(t)
	seedFreshDatabaseShape(t, pool)
	applyGrant(t, pool)

	resolver := legacyrbac.NewPostgresResolver(pool)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO public.auth_core__user (id, email, name) VALUES (2, 'out@example.com', 'Out')`); err != nil {
		t.Fatal(err)
	}

	resolution, err := resolver.ResolvePermissions(
		context.Background(), auth.User{ID: "2", UserID: "2"}, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(resolution.Permissions) != 0 {
		t.Fatalf("non-member permissions = %v, want empty", resolution.Permissions)
	}
}

// A project that carries its OWN per-project grants — the shape every
// legacy-backed database and every legacy dump has — suppresses the central
// fallback entirely, so the grant is inert there. This is why the migration
// cannot change what an existing deployment's members can do.
func TestDefaultModeGrantIsInertForAProjectWithItsOwnGrants(t *testing.T) {
	pool := newGrantPool(t)
	seedFreshDatabaseShape(t, pool)
	applyGrant(t, pool)

	if _, err := pool.Exec(context.Background(), `
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
VALUES (1, 10, 'models.something.else')`); err != nil {
		t.Fatal(err)
	}

	resolver := legacyrbac.NewPostgresResolver(pool)
	resolution, err := resolver.ResolvePermissions(
		context.Background(), auth.User{ID: "1", UserID: "1"}, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(resolution.Permissions) != 1 || resolution.Permissions[0] != "models.something.else" {
		t.Fatalf("permissions = %v, want only the project's own grant — the central fallback must "+
			"not apply to a project that has per-project rows", resolution.Permissions)
	}
}

/* ── harness ───────────────────────────────────────────────────────────── */

// seedFreshDatabaseShape reproduces exactly what 001_initial.sql leaves behind:
// default-mode ROLES and no default-mode role_permission row of any kind, with
// one project and one member holding a project role.
func seedFreshDatabaseShape(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success)
			VALUES (1, 'team', 1, '{}', true)`,
		`INSERT INTO public.auth_core__user (id, email, name) VALUES (1, 'ada@example.com', 'Ada')`,
		`INSERT INTO public.auth_core__role (id, name, mode) VALUES
			(1, 'admin', 'default'), (2, 'editor', 'default'), (3, 'viewer', 'default')`,
		`INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES (10, 1, 'editor')`,
		`INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES (1, 1, 10)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

func applyGrant(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), defaultModeGrant); err != nil {
		t.Fatalf("apply default-mode grant: %v", err)
	}
}

func newGrantPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
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

	databaseName := fmt.Sprintf("elitea_rbac_grant_it_%d_%d", os.Getpid(), time.Now().UnixNano())
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
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	for _, projection := range []string{
		dbschema.CentryProjectsBaselineSQLCProjection,
		dbschema.AuthCoreBaselineSQLCProjection,
	} {
		if _, err := pool.Exec(ctx, projection); err != nil {
			t.Fatalf("apply schema projection: %v", err)
		}
	}
	return pool
}
