package storage

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPostgresCurrentSecretVaultCompatibility crosses a real PostgreSQL
// service, the current centry table shape and Python-Fernet-compatible reader.
// It does not exercise mTLS, claims, HTTP, the command bus or a worker process.
func TestPostgresCurrentSecretVaultCompatibility(t *testing.T) {
	databaseURL := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the PostgreSQL secret-vault compatibility test")
	}
	pool := isolatedStoragePostgresPool(t, databaseURL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
CREATE SCHEMA centry;
CREATE TABLE centry.secrets_key (id TEXT PRIMARY KEY, data BYTEA);
CREATE TABLE centry.secrets_data (id TEXT PRIMARY KEY, data BYTEA)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO centry.secrets_key (id, data) VALUES ('project-2', $1), ('admin', $2)`,
		[]byte(storagePythonProjectKey),
		[]byte(storagePythonWrappedKey),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO centry.secrets_data (id, data) VALUES ('project-2', $1), ('admin', $1)`,
		[]byte(storagePythonVaultToken),
	); err != nil {
		t.Fatal(err)
	}

	unwrapped, err := NewPostgresSecretVaultLoader(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	project, err := unwrapped.LoadProjectVault(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := project.Lookup("normal")
	if err != nil || secret.Value != "normal-canary" {
		t.Fatalf("unwrapped project lookup failed: hidden=%t err=%v", secret.Hidden, err)
	}

	wrapped, err := NewPostgresSecretVaultLoader(pool, []byte(storagePythonMasterKey))
	if err != nil {
		t.Fatal(err)
	}
	admin, err := wrapped.LoadAdminVault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	secret, err = admin.LookupRegular("normal")
	if err != nil || secret.Value != "normal-canary" {
		t.Fatalf("wrapped admin lookup failed: hidden=%t err=%v", secret.Hidden, err)
	}
}

func isolatedStoragePostgresPool(t *testing.T, databaseURL string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig.MaxConns = 2
	admin, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Fatal(err)
	}

	databaseName := fmt.Sprintf("elitea_storage_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		admin.Close()
		t.Fatal(err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		admin.Close()
		t.Fatal(err)
	}

	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := admin.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated storage database: %v", err)
		}
		admin.Close()
	})
	return pool
}
