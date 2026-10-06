// Package authstatetest gives the Form sign-in state stores (authsession,
// authflow, authattempt) a real, migrated PostgreSQL database for their
// integration tests. Only test files import it.
//
// WHY THE REAL CORPUS. The properties under test are properties of shared
// migration 0153 (the checks, the primary keys, the column types) and of the
// SQL that reads them. A test that wrote its own CREATE TABLE would assert its
// own CREATE TABLE.
package authstatetest

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	bootstrapschema "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrations"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// DatabaseURLEnv names the admin DSN. CI sets it (ci-go.yml), so a skip there
// would be an undeclared skip and fail the job.
const DatabaseURLEnv = "ELITEA_TEST_DATABASE_URL"

var sequence atomic.Int64

// Pool creates an isolated database, applies the bootstrap dump and the
// ledgered shared history, and drops the database when the test ends.
// maxConns bounds the returned pool; concurrency tests size it themselves.
func Pool(t testing.TB, maxConns int32) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(DatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL integration test", DatabaseURLEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", DatabaseURLEnv, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open the admin pool: %v", err)
	}
	databaseName := fmt.Sprintf("elitea_authstate_%d_%d_%d",
		os.Getpid(), time.Now().UnixNano(), sequence.Add(1))
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		adminPool.Close()
		t.Fatalf("create the isolated database: %v", err)
	}
	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	if maxConns <= 0 {
		maxConns = 4
	}
	testConfig.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		t.Fatalf("open the test pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop the isolated database: %v", err)
		}
		adminPool.Close()
	})

	// The bootstrap dump first: shared/0030 writes into `centry`, and nothing
	// in the ledgered history creates that schema.
	if _, err := migrate.Bootstrap(ctx, pool, bootstrapschema.Initial); err != nil {
		t.Fatalf("apply the bootstrap schema: %v", err)
	}
	if err := migrate.New(pool, platformmigrations.Files).ApplyShared(ctx); err != nil {
		t.Fatalf("apply the shared migrations: %v", err)
	}
	return pool
}

// Exec runs one statement for a test fixture (an expiry moved into the past,
// a corrupted record) and fails the test on error.
func Exec(t testing.TB, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	tag, err := pool.Exec(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("fixture statement: %v", err)
	}
	return tag.RowsAffected()
}

// ClosedPool returns a pool that is already closed, so every operation on it
// fails. The stores must answer that with their unavailable error.
func ClosedPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig("postgres://unused@127.0.0.1:1/unused?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	return pool
}
