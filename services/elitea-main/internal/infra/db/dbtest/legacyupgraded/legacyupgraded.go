// Package legacyupgraded gives a test its own database built in the shape the
// legacy runtime left behind (legacyshape.BuiltSQL) and then upgraded by the
// production path: migrate.Bootstrap, the shared history and project 1's
// tenant history. Tests outside package migrate use it to prove Main's code
// works on such a project; migrate's own tests build the same shape inline,
// because this package imports migrate.
package legacyupgraded

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest/legacyshape"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	bootstrapschema "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// DatabaseURLEnv names the admin DSN the database is created through.
const DatabaseURLEnv = "ELITEA_TEST_DATABASE_URL"

var databaseSeq atomic.Uint64

// NewPool creates the database, upgrades it and returns a pool on it. The
// database is dropped when the test ends. The test is skipped when
// DatabaseURLEnv is not set.
func NewPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(DatabaseURLEnv))
	if databaseURL == "" {
		t.Skipf("set %s to run the legacy-built upgrade tests", DatabaseURLEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	name := fmt.Sprintf("elitea_legacyupgraded_%d_%d_%d", os.Getpid(), time.Now().UnixNano(), databaseSeq.Add(1))
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", DatabaseURLEnv, err)
	}
	config.ConnConfig.Database = name
	config.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})

	if _, err := pool.Exec(ctx, bootstrapschema.Initial); err != nil {
		t.Fatalf("apply the legacy-era schema: %v", err)
	}
	if _, err := pool.Exec(ctx, legacyshape.BuiltSQL); err != nil {
		t.Fatalf("build the legacy-built shape: %v", err)
	}
	if _, err := migrate.Bootstrap(ctx, pool, bootstrapschema.Initial); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("ApplyShared: %v", err)
	}
	if err := runner.ApplyTenant(ctx, 1); err != nil {
		t.Fatalf("ApplyTenant(1): %v", err)
	}
	return pool
}
