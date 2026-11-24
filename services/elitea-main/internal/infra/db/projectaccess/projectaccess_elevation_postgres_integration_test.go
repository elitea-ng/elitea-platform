package projectaccess_test

// An elevation gives permissions, not a role name (ADR-0059 decision 5,
// "Permissions, not a name"; story R2.1b, issue 2595). A person under an
// ACTIVE super_admin-level elevation, who is not a member of a project,
// resolves admin.auth.users.super_admin through the real resolver (the
// premise: the union is in place), and still does not pass the project access
// check: Membership tests the STANDING role row named super_admin, which an
// elevation never writes. The standing super_admin passes (the control).
//
// It runs on the real schema: 001_initial.sql and the shared corpus.

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/projectaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const elevationProject = 4343

func TestASuperAdminLevelElevationDoesNotPassTheProjectAccessCheck(t *testing.T) {
	pool := newMembershipDatabase(t)
	ctx := t.Context()
	if _, err := pool.Exec(ctx,
		fmt.Sprintf(`INSERT INTO centry.project (id, name, owner_id) VALUES (%d, 'r21b project', 1)`, elevationProject)); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	holder := seedMembershipPerson(t, pool, adminaccess.RoleAdmin)
	super := seedMembershipPerson(t, pool, adminaccess.RoleSuperAdmin)
	var session int64
	if err := pool.QueryRow(ctx, `
INSERT INTO elitea_auth.browser_sessions (id, user_id, provider, expires_at, idle_timeout_seconds)
VALUES ($1, $2, 'oidc', now() + interval '1 day', 0) RETURNING ref`,
		strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", ""), holder).Scan(&session); err != nil {
		t.Fatalf("open a browser session of the holder: %v", err)
	}
	appender, err := securityaudit.NewPostgresAppender("elitea-main/r21b-projectaccess-test")
	if err != nil {
		t.Fatalf("appender: %v", err)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, err := adminaccess.StartElevation(ctx, tx, appender, adminaccess.ElevationStart{
			UserID:            holder,
			BrowserSessionRef: session,
			Level:             adminaccess.ElevationLevelSuperAdmin,
			Path:              adminaccess.ElevationPathApproved,
			ReasonCode:        adminaccess.ElevationReasonIncident,
			Reason:            "R2.1b: an elevation does not open project content",
			ApproverUserID:    super,
			At:                time.Now().Add(-time.Minute),
			Duration:          time.Hour,
		})
		return err
	}); err != nil {
		t.Fatalf("start the elevation: %v", err)
	}

	id := strconv.FormatInt(holder, 10)
	resolution, err := legacyrbac.NewPostgresResolver(pool).ResolvePermissions(ctx,
		auth.User{ID: id, UserID: id, CredentialKind: auth.CredentialKindSession, BrowserSessionRef: session},
		auth.PermissionModeAdministration, "")
	if err != nil || !slices.Contains(resolution.ElevatedPermissions, adminaccess.SuperAdminPermission) {
		t.Fatalf("premise: the elevated session resolves %+v (%v), want %s from the elevation",
			resolution, err, adminaccess.SuperAdminPermission)
	}

	if member(t, pool, holder) {
		t.Fatal("a super_admin-level elevation passed the project access check of a project its holder is not a member of")
	}
	if !member(t, pool, super) {
		t.Fatal("the control: the standing super_admin does not pass the project access check")
	}
}

func member(t *testing.T, pool *pgxpool.Pool, userID int64) bool {
	t.Helper()
	var passes bool
	if err := pool.QueryRow(t.Context(), `SELECT `+projectaccess.Membership(1, 2),
		elevationProject, userID).Scan(&passes); err != nil {
		t.Fatalf("evaluate the project access check: %v", err)
	}
	return passes
}

func seedMembershipPerson(t *testing.T, pool *pgxpool.Pool, role string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO public.auth_core__user (email, name) VALUES ($1, 'R2.1b') RETURNING id`,
		"r21b-"+uuid.NewString()+"@autotest.local").Scan(&id); err != nil {
		t.Fatalf("seed a person: %v", err)
	}
	tag, err := pool.Exec(t.Context(), `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, id FROM public.auth_core__role WHERE name = $2 AND mode = 'administration'`, id, role)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("give %s: %v", role, err)
	}
	return id
}

// newMembershipDatabase is an isolated database with 001_initial.sql and the
// shared corpus applied.
func newMembershipDatabase(t *testing.T) *pgxpool.Pool {
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
	databaseName := fmt.Sprintf("elitea_projectaccess_elevation_%d_%d", os.Getpid(), time.Now().UnixNano())
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
	bootstrap, err := os.ReadFile("../migrations/001_initial.sql")
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
