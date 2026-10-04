package syncretention

// The sweep against a real PostgreSQL holding the REAL tombstone tables:
// elitea-main's tenant 0142 applied to two tenant schemas (with no chat
// tables, which also proves its guards) and shared 0144 applied over a
// 001_initial-shaped centry.notifications. Runs when ELITEA_TEST_DATABASE_URL
// is set; skips otherwise.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newSyncPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the sync retention integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("elitea_sync_retention_%d_%d", os.Getpid(), time.Now().UnixNano())
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

	tenantSQL, err := os.ReadFile("../../../elitea-main/migrations/tenant/0142_chat_sync.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, schema := range []string{"p_1", "p_2"} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('search_path', $1, true)`, schema); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(tenantSQL)); err != nil {
			t.Fatalf("apply tenant 0142 to %s: %v", schema, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `CREATE SCHEMA centry;
		CREATE TABLE centry.notifications (
		    id SERIAL PRIMARY KEY,
		    uuid UUID NOT NULL DEFAULT gen_random_uuid(),
		    is_seen BOOLEAN NOT NULL DEFAULT FALSE,
		    project_id INTEGER NOT NULL,
		    user_id INTEGER NOT NULL,
		    meta JSONB,
		    event_type TEXT NOT NULL,
		    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		    updated_at TIMESTAMP NOT NULL DEFAULT NOW())`); err != nil {
		t.Fatal(err)
	}
	sharedSQL, err := os.ReadFile("../../../elitea-main/migrations/shared/0144_notification_sync.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if _, err := pool.Exec(ctx, string(sharedSQL)); err != nil {
			t.Fatalf("apply shared 0144 (pass %d): %v", i+1, err)
		}
	}
	return pool
}

func TestSweepRemovesOnlyExpiredTombstonesInEveryTenant(t *testing.T) {
	pool := newSyncPool(t)
	ctx := context.Background()
	for _, schema := range []string{"p_1", "p_2"} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.chat_sync_tombstones (kind, entity_id, deleted_at) VALUES
			('conversation', 1, now() - interval '200 days'),
			('message_group', 2, now() - interval '98 days'),
			('conversation', 3, now() - interval '96 days'),
			('access_filter', 4, now())`, schema)); err != nil {
			t.Fatal(err)
		}
	}
	// A notification delete writes its tombstone through 0144's trigger.
	if _, err := pool.Exec(ctx, `INSERT INTO centry.notifications (project_id, user_id, event_type) VALUES (1, 5, 'x'), (1, 5, 'y');
		DELETE FROM centry.notifications`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE centry.notification_tombstones SET deleted_at = now() - interval '120 days'
		WHERE id = (SELECT min(id) FROM centry.notification_tombstones)`); err != nil {
		t.Fatal(err)
	}

	sweeper, _, err := New(pool, func(context.Context) bool { return false }, Config{BatchSize: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Drained || stats.Deleted[`"p_1"."chat_sync_tombstones"`] != 2 ||
		stats.Deleted[`"p_2"."chat_sync_tombstones"`] != 2 ||
		stats.Deleted[`"centry"."notification_tombstones"`] != 1 {
		t.Fatalf("stats = %+v, want 2 + 2 + 1 deleted and drained", stats)
	}
	for _, table := range []string{"p_1.chat_sync_tombstones", "p_2.chat_sync_tombstones"} {
		var left []int
		rows, err := pool.Query(ctx, `SELECT entity_id FROM `+table+` ORDER BY entity_id`)
		if err != nil {
			t.Fatal(err)
		}
		left, err = pgx.CollectRows(rows, pgx.RowTo[int])
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(left) != "[3 4]" {
			t.Fatalf("%s kept %v, want the two inside the 97-day window", table, left)
		}
	}
	var notificationsLeft int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM centry.notification_tombstones`).Scan(&notificationsLeft); err != nil {
		t.Fatal(err)
	}
	if notificationsLeft != 1 {
		t.Fatalf("notification tombstones left = %d, want 1", notificationsLeft)
	}
}
