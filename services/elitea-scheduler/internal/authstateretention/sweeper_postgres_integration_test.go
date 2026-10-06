package authstateretention

// The sweep against a real PostgreSQL holding the REAL tables: the statements
// read elitea-main's shared migrations 0117 and 0145, so a column rename there
// fails here instead of diverging. Runs when ELITEA_TEST_DATABASE_URL is set
// (CI sets it); skips otherwise.

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var schemaFiles = []string{
	"../../../elitea-main/migrations/shared/0117_browser_sessions.sql",
	"../../../elitea-main/migrations/shared/0145_form_auth_state.sql",
}

func newAuthStatePool(t *testing.T, files []string) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the auth state retention integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("elitea_authstate_retention_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatal(err)
	}
	config := admin.Config().Copy()
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), time.Minute)
		defer dropCancel()
		_, _ = admin.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)")
		admin.Close()
	})
	for _, file := range files {
		raw, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if _, err := pool.Exec(ctx, string(raw)); err != nil {
			t.Fatalf("apply %s: %v", file, err)
		}
	}
	return pool
}

func opaqueID(n int) string {
	raw := make([]byte, 32)
	raw[0], raw[1] = byte(n), byte(n>>8)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func attemptKey(n int) []byte {
	raw := make([]byte, 32)
	raw[0], raw[1] = byte(n), byte(n>>8)
	return raw
}

func TestSweepRemovesOnlyWhatIsPastRetention(t *testing.T) {
	pool := newAuthStatePool(t, schemaFiles)
	ctx := context.Background()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	// For each table: one row past its grace (removed), one expired but
	// inside its grace (kept), one live (kept).
	pastForm := now.Add(-expiryGrace - time.Minute)
	insideForm := now.Add(-expiryGrace + time.Minute)
	live := now.Add(time.Hour)
	for index, expiresAt := range []time.Time{pastForm, insideForm, live} {
		exec(`INSERT INTO elitea_auth.form_sessions (id, record, expires_at) VALUES ($1, '{}', $2)`,
			opaqueID(index), expiresAt)
		exec(`INSERT INTO elitea_auth.form_login_transactions
		          (id, provider, originating_session_id, record, expires_at)
		      VALUES ($1, 'form', 's', '{}', $2)`, opaqueID(index), expiresAt)
		exec(`INSERT INTO elitea_auth.browser_attempt_windows (key, attempts, window_ends_at)
		      VALUES ($1, 1, $2)`, attemptKey(index), expiresAt)
	}
	pastBrowser := now.Add(-BrowserSessionGrace - time.Minute)
	insideBrowser := now.Add(-BrowserSessionGrace + time.Minute)
	for index, expiresAt := range []time.Time{pastBrowser, insideBrowser, live} {
		exec(`INSERT INTO elitea_auth.browser_sessions
		          (id, user_id, provider, expires_at, idle_timeout_seconds)
		      VALUES ($1, 1, 'oidc', $2, 0)`, opaqueID(index), expiresAt)
	}
	// A revoked session past its deadline goes too: revocation does not keep
	// a row beyond the grace.
	exec(`INSERT INTO elitea_auth.browser_sessions
	          (id, user_id, provider, expires_at, idle_timeout_seconds, revoked_at)
	      VALUES ($1, 1, 'saml', $2, 0, $3)`, opaqueID(9), pastBrowser, pastBrowser.Add(-time.Hour))

	sweeper, err := New(pool, func(context.Context) bool { return false }, Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sweeper.now = func() time.Time { return now }
	stats, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{
		"elitea_auth.form_sessions":           1,
		"elitea_auth.form_login_transactions": 1,
		"elitea_auth.browser_attempt_windows": 1,
		"elitea_auth.browser_sessions":        2,
	}
	for table, count := range want {
		if stats.Deleted[table] != count {
			t.Fatalf("deleted from %s = %d, want %d (stats %+v)", table, stats.Deleted[table], count, stats)
		}
	}
	if !stats.Drained || len(stats.Absent) != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	for table, remaining := range map[string]int{
		"form_sessions": 2, "form_login_transactions": 2, "browser_attempt_windows": 2, "browser_sessions": 2,
	} {
		var count int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM elitea_auth."+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != remaining {
			t.Fatalf("%s keeps %d rows, want %d", table, count, remaining)
		}
	}

	// A second pass finds nothing.
	again, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for table, count := range again.Deleted {
		if count != 0 {
			t.Fatalf("second pass deleted %d from %s", count, table)
		}
	}
}

// A backlog larger than one batch is drained in batches within a pass, and a
// backlog larger than the pass ceiling is reported as not drained.
func TestSweepDrainsInBatchesAndReportsTheCeiling(t *testing.T) {
	pool := newAuthStatePool(t, schemaFiles)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `
		INSERT INTO elitea_auth.browser_attempt_windows (key, attempts, window_ends_at)
		SELECT sha256(int4send(n)), 1, $1 FROM generate_series(1, 25) AS n`,
		now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	capped, err := New(pool, func(context.Context) bool { return false },
		Config{BatchSize: 10, MaxBatchesPerPass: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := capped.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted["elitea_auth.browser_attempt_windows"] != 20 || stats.Drained {
		t.Fatalf("capped pass = %+v, want 20 deleted and not drained", stats)
	}
	stats, err = capped.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted["elitea_auth.browser_attempt_windows"] != 5 || !stats.Drained {
		t.Fatalf("second pass = %+v, want the last 5 and drained", stats)
	}
}

// The scheduler can start before elitea-main applies 0145. The tables it
// does not find are skipped and named, and the pass still sweeps the rest.
func TestSweepSkipsTablesTheDatabaseDoesNotHaveYet(t *testing.T) {
	pool := newAuthStatePool(t, schemaFiles[:1])
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO elitea_auth.browser_sessions (id, user_id, provider, expires_at, idle_timeout_seconds)
		VALUES ($1, 1, 'oidc', now() - interval '2 days', 0)`, opaqueID(1)); err != nil {
		t.Fatal(err)
	}
	sweeper, err := New(pool, func(context.Context) bool { return false }, Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Absent) != 3 || stats.Deleted["elitea_auth.browser_sessions"] != 1 {
		t.Fatalf("stats = %+v, want three absent Form tables and one swept session", stats)
	}
}
