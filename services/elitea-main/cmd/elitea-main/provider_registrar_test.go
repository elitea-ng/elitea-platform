package main

import (
	"context"
	"fmt"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/facade"
)

// The composition root's rule: no database, no public project, or a
// database the admission migration has not reached — the provider keeps
// serving and nothing is registered. Nothing here panics on a nil pool.
func TestProviderRegistrarSkipsWhatItCannotRegisterWith(t *testing.T) {
	cfg := facade.Config{Target: "elitea-deepwiki:9443"}
	if startProviderRegistrar(context.Background(), nil, nil, 1, "deepwiki", cfg, "X", nil) != nil {
		t.Fatal("started with no database")
	}
	pool := registrarTestPool(t)
	if startProviderRegistrar(context.Background(), nil, pool, 0, "deepwiki", cfg, "X", nil) != nil {
		t.Fatal("started with no public project")
	}
	if startProviderRegistrar(context.Background(), nil, pool, 1, "deepwiki", cfg, "X", nil) != nil {
		t.Fatal("started against a database without the admission plane")
	}
	sql, err := os.ReadFile(filepath.Join("..", "..", "migrations", "shared", "0107_provider_admitted_revisions.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(sql)); err != nil {
		t.Fatal(err)
	}
	// The plane is present, so the registrar is built — and refused here
	// only because no provider client was handed in, which proves the plane
	// check was passed rather than short-circuited.
	if startProviderRegistrar(context.Background(), nil, pool, 1, "deepwiki", cfg, "X", nil) != nil {
		t.Fatal("started without the facade's provider client")
	}
}

// The gate the facade carries follows the registrar: no registration, no
// gate. THE E2E STACK IS THIS CASE — it composes no public project, so the
// facade there must be handed a nil hook and forward exactly as it did
// before admission existed. A gate that was built anyway and simply always
// answered "allow" would be indistinguishable at runtime and would put a
// database round trip on an invoke path that has no plane to read.
func TestNoRegistrationMeansNoAdmissionGate(t *testing.T) {
	if hook := providerAdmissionGate(nil, nil, 0, facade.AdmissionEnforce, nil); hook != nil {
		t.Fatal("a facade with no registrar was given an admission hook")
	}
}

func registrarTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the registrar composition test against PostgreSQL", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)
	name := fmt.Sprintf("elitea_registrar_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = adminPool.Exec(cleanupCtx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = adminPool.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+quoted)
	})
	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
