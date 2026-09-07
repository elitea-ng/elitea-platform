package browsersession

// The store against the REAL table, the one shared migration 0117 creates.
//
// WHY NOT A HAND-BUILT SCHEMA. The properties under test are properties of the
// MIGRATION: the check constraints, the column types, the primary key. A test
// that wrote its own CREATE TABLE would assert its own CREATE TABLE, which is
// the shape `absence reads as correctness` warns about. This file therefore
// applies the embedded corpus and works against what it produced.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	migrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
)

const (
	browserSessionDatabaseURLEnv = "ELITEA_TEST_DATABASE_URL"
	// bootstrapSchemaPath is the pre-ledger dump, relative to this package.
	bootstrapSchemaPath = "../../infra/db/migrations/001_initial.sql"
)

// newBrowserSessionPool builds an isolated database with the shared corpus
// applied, exactly as the corpus suite does.
func newBrowserSessionPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	databaseURL := os.Getenv(browserSessionDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL integration test", browserSessionDatabaseURLEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", browserSessionDatabaseURLEnv, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open the admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		t.Fatalf("ping the admin pool: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_bs_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatalf("create the isolated database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		t.Fatalf("open the test pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop the isolated database: %v", err)
		}
		adminPool.Close()
	})

	// The bootstrap dump FIRST, then the ledgered shared history.
	//
	// An empty database cannot run the corpus on its own: shared/0030 writes
	// into `centry`, and nothing in the ledgered history creates that schema.
	// The corpus suite in services/elitea-main/migrations applies the same dump
	// for the same reason. Only the SHARED history is applied here; this table
	// needs no tenant schema.
	bootstrap, err := os.ReadFile(bootstrapSchemaPath)
	if err != nil {
		t.Fatalf("read the bootstrap schema: %v", err)
	}
	if _, err := pool.Exec(ctx, string(bootstrap)); err != nil {
		t.Fatalf("apply the bootstrap schema: %v", err)
	}
	if err := migrate.New(pool, migrations.Files).ApplyShared(ctx); err != nil {
		t.Fatalf("apply the shared migrations: %v", err)
	}
	return pool
}

func newIntegrationManager(t *testing.T, policy Policy, now func() time.Time) *Manager {
	t.Helper()
	store, err := NewPostgresStore(newBrowserSessionPool(t))
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	manager, err := NewManager(store, policy, WithClock(now))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return manager
}

// TestPostgresSessionLifecycle drives the same contract the in-memory suite
// pins, against the migrated table. It is the half that proves the SQL: a
// column name this file gets wrong fails here and nowhere else.
func TestPostgresSessionLifecycle(t *testing.T) {
	dial := &clock{now: time.Date(2026, time.September, 7, 9, 0, 0, 0, time.UTC)}
	manager := newIntegrationManager(t, Policy{
		IdleTimeout: time.Hour, AbsoluteLifetime: 4 * time.Hour,
	}, dial.Now)
	ctx := context.Background()

	value, err := manager.Create(ctx, NewSession{
		UserID: 42, Email: "owner@example.test", Provider: ProviderSAML,
		ProviderSessionIndex: "provider-session-index",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	session, err := manager.Validate(ctx, value)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if session.UserID != 42 || session.Provider != ProviderSAML ||
		session.ProviderSessionIndex != "provider-session-index" {
		t.Fatalf("session = %+v, want the values Create was given", session)
	}
	if session.IdleTimeout != time.Hour {
		t.Fatalf("idle timeout = %s, want the policy the row was stamped with", session.IdleTimeout)
	}

	// The touch throttle, through real SQL.
	dial.advance(2 * TouchInterval)
	touched, err := manager.Validate(ctx, value)
	if err != nil {
		t.Fatalf("Validate after the throttle window: %v", err)
	}
	if !touched.LastSeenAt.After(session.LastSeenAt) {
		t.Fatalf("last_seen_at did not move: %s then %s", session.LastSeenAt, touched.LastSeenAt)
	}

	// Idle expiry.
	dial.advance(90 * time.Minute)
	if _, err := manager.Validate(ctx, value); !errors.Is(err, ErrIdle) {
		t.Fatalf("Validate after the idle window = %v, want ErrIdle", err)
	}

	// Revocation survives a fresh read, which a signed cookie could not do.
	fresh, err := manager.Create(ctx, NewSession{UserID: 42, Provider: ProviderOIDC})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := manager.Revoke(ctx, fresh); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := manager.Validate(ctx, fresh); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Validate after Revoke = %v, want ErrRevoked", err)
	}
}

// TestPostgresRefusesAnUnknownProvider proves the migration's check constraint
// is real and that Create's own guard agrees with it. Both halves matter: a
// guard that admitted a value the constraint refuses would fail at the INSERT,
// in a login, with a pgx error.
func TestPostgresRefusesAnUnknownProvider(t *testing.T) {
	pool := newBrowserSessionPool(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
		INSERT INTO elitea_auth.browser_sessions
			(id, user_id, provider, expires_at, idle_timeout_seconds)
		VALUES ($1, 1, 'invented', now() + interval '1 hour', 3600)`,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err == nil {
		t.Fatal("the table accepted a provider no login plane writes")
	}

	// The positive half: the three the planes DO write are accepted.
	for _, provider := range []string{ProviderOIDC, ProviderSAML, ProviderForm} {
		id, idErr := NewID()
		if idErr != nil {
			t.Fatal(idErr)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO elitea_auth.browser_sessions
				(id, user_id, provider, expires_at, idle_timeout_seconds)
			VALUES ($1, 1, $2, now() + interval '1 hour', 3600)`, id, provider); err != nil {
			t.Fatalf("the table refused provider %q: %v", provider, err)
		}
	}
}

// TestPostgresSweepRemovesOnlyDeadRows. The sweep bounds the table; it must not
// remove a session somebody is using.
func TestPostgresSweepRemovesOnlyDeadRows(t *testing.T) {
	pool := newBrowserSessionPool(t)
	store, err := NewPostgresStore(pool)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	ctx := context.Background()
	now := time.Now().UTC()

	live := Session{
		ID: mustID(t), UserID: 1, Provider: ProviderOIDC,
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour), IdleTimeout: time.Hour,
	}
	dead := Session{
		ID: mustID(t), UserID: 1, Provider: ProviderOIDC,
		CreatedAt: now.Add(-48 * time.Hour), LastSeenAt: now.Add(-48 * time.Hour),
		ExpiresAt: now.Add(-24 * time.Hour), IdleTimeout: time.Hour,
	}
	for _, session := range []Session{live, dead} {
		if err := store.Insert(ctx, session); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	removed, err := store.DeleteExpired(ctx, now, time.Hour)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if removed != 1 {
		t.Fatalf("swept %d rows, want 1", removed)
	}
	if _, err := store.Get(ctx, live.ID); err != nil {
		t.Fatalf("the sweep removed a live session: %v", err)
	}
	if _, err := store.Get(ctx, dead.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get on a swept session = %v, want ErrNotFound", err)
	}
}

func mustID(t *testing.T) string {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
