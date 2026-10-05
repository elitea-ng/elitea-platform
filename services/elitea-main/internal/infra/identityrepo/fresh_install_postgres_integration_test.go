package identityrepo

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/identity"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	bootstrapschema "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrations"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// A FRESH INSTALL, not the sqlc baseline.
//
// Every other test in this package seeds its database from
// dbschema.AuthCoreBaselineSQLCProjection, which is sqlc compiler input and
// carries pylon tables the Go install never creates (auth_core__group and
// auth_core__user_group among them). A database built only by elitea-migrate
// (Bootstrap + ApplyShared, the Helm and standalone-full path) has no such
// tables, so a provisioning step that writes one rolled back every first login
// on the Form/ForwardAuth plane while this suite stayed green.
//
// This test builds the schema exactly the way cmd/elitea-migrate does on an
// empty database and signs a brand-new person in.
func TestPostgresIdentityProvisioningFreshInstallNewUser(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool := newFreshInstallIdentityDatabase(t, ctx)

	service := newProvisionService(t, pool, identity.ProvisioningPolicy{})
	result, err := service.Provision(ctx, identity.ProvisionRequest{Assertion: identity.VerifiedAssertion{
		Provider:          "form",
		ProviderReference: "fresh-install-person@example.com",
		Email:             "fresh-install-person@example.com",
		Name:              "Fresh Install Person",
	}})
	if err != nil {
		t.Fatalf("first login on a fresh-install database: %v", err)
	}
	if result.UserID <= 0 {
		t.Fatalf("result = %+v, want a newly created user", result)
	}
	assertCount(t, ctx, pool, 1,
		`SELECT count(*) FROM public.auth_core__user WHERE id = $1 AND email = 'fresh-install-person@example.com'`,
		result.UserID)
	assertCount(t, ctx, pool, 1,
		`SELECT count(*) FROM public.auth_core__user_provider WHERE user_id = $1 AND provider_ref = 'fresh-install-person@example.com'`,
		result.UserID)

	// A second login of the same person resolves the existing row.
	repeated, err := service.Provision(ctx, identity.ProvisionRequest{Assertion: identity.VerifiedAssertion{
		Provider:          "form",
		ProviderReference: "fresh-install-person@example.com",
		Email:             "fresh-install-person@example.com",
	}})
	if err != nil {
		t.Fatalf("repeated login: %v", err)
	}
	if repeated.UserID != result.UserID {
		t.Fatalf("repeated = %+v, want existing user %d", repeated, result.UserID)
	}
}

// newFreshInstallIdentityDatabase creates a private empty database and runs
// migrate.Bootstrap + ApplyShared on it, the same two calls cmd/elitea-migrate
// makes on a first install. It proves the pylon-only group table is absent, so
// the test keeps discriminating if the schema source ever changes.
func newFreshInstallIdentityDatabase(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	adminURL := os.Getenv("ELITEA_AUTH_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("set ELITEA_AUTH_TEST_DATABASE_URL to an isolated PostgreSQL admin database")
	}
	adminConfig, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)

	databaseName := fmt.Sprintf("elitea_identity_fresh_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = adminPool.Exec(cleanupCtx,
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
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

	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		t.Fatalf("create the vector extension: %v", err)
	}
	applied, err := migrate.Bootstrap(ctx, pool, bootstrapschema.Initial)
	if err != nil {
		t.Fatalf("bootstrap the fresh-install schema: %v", err)
	}
	if !applied {
		t.Fatal("Bootstrap reported nothing to do on an empty database")
	}
	if err := migrate.New(pool, platformmigrations.Files).ApplyShared(ctx); err != nil {
		t.Fatalf("apply the shared migration history: %v", err)
	}

	var groupTable any
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.auth_core__user_group')`).Scan(&groupTable); err != nil {
		t.Fatal(err)
	}
	if groupTable != nil {
		t.Fatal("public.auth_core__user_group exists on this database; it is no longer the fresh-install shape this test claims")
	}
	return pool
}
