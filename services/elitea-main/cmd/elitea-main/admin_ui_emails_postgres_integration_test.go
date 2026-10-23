package main

import (
	"context"
	"fmt"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	dbschema "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/schema"
)

// adminUIEmails is the only part of the admin-console identity path that talks
// to PostgreSQL, and its correctness is not observable from a unit test: the
// destination is a **string, because public.auth_core__user.email is NULLABLE
// (internal/db/schema/auth_core_baseline.sql). A decode that cannot handle the
// column silently takes the error branch, the console footer falls back to the
// generic word "Admin", and nothing else on the page changes — the same
// silent-fallback shape the forwarded-identity fix exists to remove.
//
// So these cases run the real query against a real database. Requires
// PostgreSQL to create an isolated database in; skipped otherwise, like every
// other *_postgres_integration_test.go in this service.

func TestAdminUIEmailsReadsTheOperatorsAddress(t *testing.T) {
	pool := newAdminUIEmailsPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	seedAdminUIUsers(t, ctx, pool, `
INSERT INTO public.auth_core__user (id, email, name, suspended) VALUES
    (1, 'operator@example.com', 'Operator', false),
    (2, NULL,                   'No Address', false),
    (3, 'suspended@example.com','Suspended', true)`)

	emails := adminUIEmails{pool: pool}

	t.Run("an active user's address", func(t *testing.T) {
		email, err := emails.UserEmail(ctx, 1)
		if err != nil {
			t.Fatalf("UserEmail(1) error = %v, want nil", err)
		}
		if email != "operator@example.com" {
			t.Errorf("UserEmail(1) = %q, want operator@example.com", email)
		}
	})

	// The column is nullable. A NULL is an answer about the user, not a
	// failure, so it must not surface as an error the caller logs as an outage.
	t.Run("a NULL address is empty and not an error", func(t *testing.T) {
		email, err := emails.UserEmail(ctx, 2)
		if err != nil {
			t.Fatalf("UserEmail(2) error = %v, want nil for a NULL email", err)
		}
		if email != "" {
			t.Errorf("UserEmail(2) = %q, want empty", email)
		}
	})

	// Belt to the resolver's braces: the resolver already refuses a suspended
	// user before this runs, so this row must not be readable here either.
	t.Run("a suspended user yields nothing", func(t *testing.T) {
		email, err := emails.UserEmail(ctx, 3)
		if err != nil {
			t.Fatalf("UserEmail(3) error = %v, want nil", err)
		}
		if email != "" {
			t.Errorf("UserEmail(3) = %q, want empty for a suspended user", email)
		}
	})

	t.Run("an absent user yields nothing", func(t *testing.T) {
		email, err := emails.UserEmail(ctx, 4242)
		if err != nil {
			t.Fatalf("UserEmail(4242) error = %v, want nil", err)
		}
		if email != "" {
			t.Errorf("UserEmail(4242) = %q, want empty", email)
		}
	})
}

// A nil pool is a composition failure and must be reported as one. Returning
// "" with no error would render the fallback footer name and leave no trace.
func TestAdminUIEmailsWithoutAPoolReportsTheCompositionFailure(t *testing.T) {
	t.Parallel()

	email, err := adminUIEmails{}.UserEmail(context.Background(), 1)
	if err == nil {
		t.Fatal("UserEmail error = nil, want a composition failure")
	}
	if email != "" {
		t.Errorf("UserEmail = %q, want empty", email)
	}
}

func seedAdminUIUsers(t *testing.T, ctx context.Context, pool *pgxpool.Pool, statement string) {
	t.Helper()
	if _, err := pool.Exec(ctx, statement); err != nil {
		t.Fatalf("seed admin ui users: %v", err)
	}
}

// newAdminUIEmailsPool opens an isolated database carrying the auth_core
// baseline. It mirrors newAdminUsersPool in internal/api/v2/admin.
func newAdminUIEmailsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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

	databaseName := fmt.Sprintf("elitea_admin_ui_emails_it_%d_%d", os.Getpid(), time.Now().UnixNano())
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
		// 120 s: this DROP queues behind the CREATE DATABASE calls of every
		// package `go test ./...` runs at the same time (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	if _, err := pool.Exec(ctx, dbschema.AuthCoreBaselineSQLCProjection); err != nil {
		t.Fatalf("apply auth_core baseline projection: %v", err)
	}
	return pool
}
