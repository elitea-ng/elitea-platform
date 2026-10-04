package nativeauthretention

// The sweep against a real PostgreSQL holding the REAL native tables: the
// statements below read elitea-main's shared migration 0141 and its auth_core
// and elitea_identity projections, so a column rename there fails here instead
// of diverging. Runs when ELITEA_TEST_DATABASE_URL is set; skips otherwise.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var schemaFiles = []string{
	"../../../elitea-main/internal/db/schema/auth_core_baseline.sql",
	"../../../elitea-main/internal/db/schema/elitea_identity_baseline.sql",
	"../../../elitea-main/migrations/shared/0141_native_auth.sql",
}

func newNativePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the native auth retention integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("elitea_native_retention_%d_%d", os.Getpid(), time.Now().UnixNano())
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
	for _, file := range schemaFiles {
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

const hexA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func hexOf(n int) string { return fmt.Sprintf("%064x", n) }

func TestSweepRemovesOnlyWhatIsPastRetention(t *testing.T) {
	pool := newNativePool(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	var userID, liveAnchor, idleAnchor int64
	if err := pool.QueryRow(ctx, `INSERT INTO public.auth_core__user (email) VALUES ('u@example.test') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRow(ctx, `INSERT INTO public.auth_core__token (user_id, name) VALUES ($1, 'native:x') RETURNING id`, userID).Scan(&liveAnchor)
	_ = pool.QueryRow(ctx, `INSERT INTO public.auth_core__token (user_id, name) VALUES ($1, 'native:x') RETURNING id`, userID).Scan(&idleAnchor)
	exec(`INSERT INTO elitea_identity.token_project_binding (token_id, project_id) VALUES ($1, 7)`, idleAnchor)

	day := 24 * time.Hour
	const idle = 30 * 24 * 3600
	// live: refreshed an hour ago. idle: refreshed 31 days ago. old: revoked
	// 91 days ago. recent: revoked yesterday.
	exec(`INSERT INTO elitea_auth.native_sessions (id, user_id, token_id, client_id, device_name, platform,
	        last_refreshed_at, idle_timeout_seconds) VALUES
	      ('00000000-0000-0000-0000-000000000001', $1, $2, 'c.d.e', 'live', 'ios', $4, $6),
	      ('00000000-0000-0000-0000-000000000002', $1, $3, 'c.d.e', 'idle', 'ios', $5, $6)`,
		userID, liveAnchor, idleAnchor, now.Add(-time.Hour), now.Add(-31*day), idle)
	exec(`INSERT INTO elitea_auth.native_sessions (id, user_id, client_id, device_name, platform,
	        idle_timeout_seconds, revoked_at, revoke_reason) VALUES
	      ('00000000-0000-0000-0000-000000000003', $1, 'c.d.e', 'old', 'ios', $2, $3, 'user'),
	      ('00000000-0000-0000-0000-000000000004', $1, 'c.d.e', 'recent', 'ios', $2, $4, 'user')`,
		userID, idle, now.Add(-91*day), now.Add(-day))
	// Access tokens: one expired two hours ago (swept), one 30 min ago (kept).
	exec(`INSERT INTO elitea_auth.native_access_tokens (token_hash, session_id, expires_at) VALUES
	      ($1, '00000000-0000-0000-0000-000000000001', $3),
	      ($2, '00000000-0000-0000-0000-000000000001', $4)`, hexOf(1), hexOf(2), now.Add(-2*time.Hour), now.Add(-30*time.Minute))
	// Refresh tokens: consumed 31 days ago (swept), consumed a minute ago
	// with a sealed successor (kept), and the idle family's sealed one.
	exec(`INSERT INTO elitea_auth.native_refresh_tokens (token_hash, session_id, generation, consumed_at, successor_sealed) VALUES
	      ($1, '00000000-0000-0000-0000-000000000001', 1, $4, NULL),
	      ($2, '00000000-0000-0000-0000-000000000001', 2, $5, '\x01'),
	      ($3, '00000000-0000-0000-0000-000000000002', 1, $6, '\x01')`,
		hexOf(3), hexOf(4), hexOf(5), now.Add(-31*day), now.Add(-time.Minute), now.Add(-31*day+time.Second))
	// Authorizations: expired two hours ago (swept), expires in five minutes.
	exec(`INSERT INTO elitea_auth.native_authorizations (handle_hash, binder_hash, client_id, redirect_uri,
	        code_challenge, state, device_name, platform, expires_at) VALUES
	      ($1, $3, 'c.d.e', 'c.d.e:/cb', $4, 's', 'd', 'ios', $5),
	      ($2, $3, 'c.d.e', 'c.d.e:/cb', $4, 's', 'd', 'ios', $6)`,
		hexOf(6), hexOf(7), hexA, "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
		now.Add(-2*time.Hour), now.Add(5*time.Minute))

	sweeper, err := New(pool, Config{BatchSize: 1, MaxBatchesPerPass: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sweeper.now = func() time.Time { return now }
	stats, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := Stats{"access_tokens": 1, "authorizations": 1, "consumed_refresh_tokens": 2, "idle_families": 1, "revoked_families": 1}
	for name, count := range want {
		if stats[name] != count {
			t.Fatalf("stats = %v, want %v", stats, want)
		}
	}

	count := func(sql string, args ...any) int {
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	var reason *string
	var anchor *int64
	_ = pool.QueryRow(ctx, `SELECT revoke_reason, token_id FROM elitea_auth.native_sessions
	                        WHERE id = '00000000-0000-0000-0000-000000000002'`).Scan(&reason, &anchor)
	if reason == nil || *reason != "expired" || anchor != nil {
		t.Fatalf("idle family = %v %v, want revoked expired with no anchor", reason, anchor)
	}
	if count(`SELECT count(*) FROM public.auth_core__token WHERE id = $1`, idleAnchor) != 0 ||
		count(`SELECT count(*) FROM elitea_identity.token_project_binding WHERE token_id = $1`, idleAnchor) != 0 {
		t.Fatal("the idle family's anchor and binding must be deleted")
	}
	if count(`SELECT count(*) FROM public.auth_core__token WHERE id = $1`, liveAnchor) != 1 {
		t.Fatal("a live family's anchor was deleted")
	}
	if count(`SELECT count(*) FROM elitea_auth.native_sessions WHERE revoked_at IS NULL`) != 1 ||
		count(`SELECT count(*) FROM elitea_auth.native_sessions`) != 3 {
		t.Fatal("families: want the live one live, the old revoked one deleted, the recent one kept")
	}
	if count(`SELECT count(*) FROM elitea_auth.native_refresh_tokens WHERE token_hash = $1 AND successor_sealed IS NOT NULL`, hexOf(4)) != 1 {
		t.Fatal("a refresh token consumed a minute ago (inside the re-delivery horizon) must be kept with its sealed successor")
	}
	if count(`SELECT count(*) FROM elitea_auth.native_access_tokens`) != 1 ||
		count(`SELECT count(*) FROM elitea_auth.native_authorizations`) != 1 {
		t.Fatal("unexpired access tokens or authorizations were deleted")
	}
}
