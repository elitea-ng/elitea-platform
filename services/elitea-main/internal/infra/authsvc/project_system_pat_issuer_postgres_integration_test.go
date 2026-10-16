package authsvc

import (
	"context"
	"errors"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	dbschema "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/schema"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

func TestPostgresIndexRuntimeCurrentIdentityPATSelection(t *testing.T) {
	pool := newProjectSystemPATTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user (id, email, suspended)
VALUES
    (7, 'actor-active@example.test', false),
    (8, 'actor-expired@example.test', false),
    (9, 'actor-no-pat@example.test', false),
    (10, 'actor-suspended@example.test', true),
    (74, 'system_user_42@centry.user', false),
    (75, 'system_user_43@centry.user', false),
    (76, 'system_user_44@centry.user', false),
    (77, 'system_user_45@centry.user', true),
    (78, 'system_user_46@centry.user', false),
    (79, 'system_user_47@centry.user', false),
    (80, 'system_user_48@centry.user', false);

INSERT INTO centry.project (
    id, name, owner_id, secrets_json, plugins, keycloak_groups, create_success, suspended
) VALUES
    (42, 'active', 7, '{}'::json, ARRAY[]::text[], '{}'::json, true, false),
    (43, 'expired-token', 7, '{}'::json, ARRAY[]::text[], '{}'::json, true, false),
    (44, 'no-token', 7, '{}'::json, ARRAY[]::text[], '{}'::json, true, false),
    (45, 'suspended-system-user', 7, '{}'::json, ARRAY[]::text[], '{}'::json, true, false),
    (46, 'suspended-project', 7, '{}'::json, ARRAY[]::text[], '{}'::json, true, true),
    (47, 'active-cross-project', 7, '{}'::json, ARRAY[]::text[], '{}'::json, true, false),
    (48, 'missing-system-role', 7, '{}'::json, ARRAY[]::text[], '{}'::json, true, false);

INSERT INTO public.auth_core__project_role (id, project_id, name)
VALUES
    (142, 42, 'system'),
    (143, 43, 'system'),
    (144, 44, 'system'),
    (145, 45, 'system'),
    (146, 46, 'system'),
    (147, 47, 'system'),
    (148, 48, 'system');

INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
VALUES
    (42, 74, 142),
    (43, 75, 143),
    (44, 76, 144),
    (45, 77, 145),
    (46, 78, 146),
    (47, 79, 147);

INSERT INTO public.auth_core__token (uuid, expires, user_id, name)
VALUES
    ('00000000-0000-0000-0000-000000000007', NULL, 7, 'actor'),
    ('00000000-0000-0000-0000-000000000008', (clock_timestamp() AT TIME ZONE 'UTC') - INTERVAL '1 minute', 8, 'actor'),
    ('00000000-0000-0000-0000-000000000010', NULL, 10, 'actor'),
    ('00000000-0000-0000-0000-000000000042', NULL, 74, 'api'),
    ('00000000-0000-0000-0000-000000000043', (clock_timestamp() AT TIME ZONE 'UTC') - INTERVAL '1 minute', 75, 'api'),
    ('00000000-0000-0000-0000-000000000045', NULL, 77, 'api'),
    ('00000000-0000-0000-0000-000000000046', NULL, 78, 'api'),
    ('00000000-0000-0000-0000-000000000047', NULL, 74, 'api'),
    ('00000000-0000-0000-0000-000000000048', NULL, 80, 'api');`); err != nil {
		t.Fatal(err)
	}

	const signingKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	actorIssuer := NewLocalIssuerBytes(pool, []byte(signingKey))
	if token, err := actorIssuer.IssueToken(ctx, 7); err != nil || token == "" {
		t.Fatalf("active actor PAT token empty=%t, error=%v", token == "", err)
	}
	for _, userID := range []int64{8, 9, 10} {
		if _, err := actorIssuer.IssueToken(ctx, userID); !errors.Is(err, ErrTokenRejected) {
			t.Fatalf("actor %d error = %v, want ErrTokenRejected", userID, err)
		}
	}

	systemIssuer := NewProjectSystemIssuerBytes(pool, []byte(signingKey))
	systemToken, err := systemIssuer.IssueProjectToken(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if systemToken.ProjectID() != 42 || systemToken.UserID() != 74 || systemToken.Token() == "" {
		t.Fatalf(
			"project-system identity = project %d, user %d, token empty=%t",
			systemToken.ProjectID(),
			systemToken.UserID(),
			systemToken.Token() == "",
		)
	}
	principal, err := NewLocalValidator(pool, signingKey).ValidateToken(ctx, systemToken.Token())
	if err != nil {
		t.Fatal(err)
	}
	if principal.UserID != "74" {
		t.Fatalf("validated system principal user = %q, want 74", principal.UserID)
	}

	for _, projectID := range []int64{43, 44, 45, 46, 47, 48} {
		if _, err := systemIssuer.IssueProjectToken(ctx, projectID); !errors.Is(err, ErrTokenRejected) {
			t.Fatalf("project %d error = %v, want ErrTokenRejected", projectID, err)
		}
	}

	// A process restart re-queries the durable current schema; it does not rely
	// on process-local identity state.
	restartedIssuer := NewProjectSystemIssuerBytes(pool, []byte(signingKey))
	afterRestart, err := restartedIssuer.IssueProjectToken(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if afterRestart.ProjectID() != systemToken.ProjectID() ||
		afterRestart.UserID() != systemToken.UserID() ||
		afterRestart.Token() != systemToken.Token() {
		t.Fatalf("project-system identity changed across issuer restart")
	}
}

func newProjectSystemPATTestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminURL := testpg.DSN(os.Getenv("ELITEA_AUTH_TEST_DATABASE_URL"))
	if adminURL == "" {
		t.Skip("set ELITEA_AUTH_TEST_DATABASE_URL to an isolated PostgreSQL admin database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	adminConfig, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)

	databaseName := fmt.Sprintf("elitea_index_identity_test_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, _ = adminPool.Exec(cleanupCtx,
			`SELECT pg_terminate_backend(pid)
			 FROM pg_stat_activity
			 WHERE datname = $1 AND pid <> pg_backend_pid()`,
			databaseName,
		)
		_, _ = adminPool.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+identifier)
	})

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, dbschema.AuthCoreBaselineSQLCProjection); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, dbschema.CentryProjectsBaselineSQLCProjection); err != nil {
		t.Fatal(err)
	}
	// GetActivePATPrincipalByUUID LEFT JOINs elitea_identity.token_project_binding
	// on every credential validation (ADR-0018). A database without that table
	// answers every token with 42P01, so the harness applies the REAL migration
	// rather than a hand-copied projection.
	bindingMigration, err := platformmigrations.Files.ReadFile("shared/0071_token_project_binding.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(bindingMigration)); err != nil {
		t.Fatal(err)
	}
	return pool
}
