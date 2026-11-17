package migrate

import (
	"context"
	"fmt"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
)

// A concurrent transaction takes the locks in the opposite order to a
// two-migration history (issue 2188). PostgreSQL aborts the migrator with
// 40P01; apply must roll back, retry and finish once the other side is done.
func TestApplyRetriesDeadlockAgainstConcurrentTransaction(t *testing.T) {
	url := testpg.DSN(os.Getenv("ELITEA_TEST_DATABASE_URL"))
	if url == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the deadlock retry story")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(url)
	require.NoError(t, err)
	admin, err := pgxpool.NewWithConfig(ctx, adminConfig)
	require.NoError(t, err)
	defer admin.Close()
	name := fmt.Sprintf("elitea_deadlock_retry_%d_%d", os.Getpid(), time.Now().UnixNano())
	_, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	require.NoError(t, err)
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	}()
	childConfig := adminConfig.Copy()
	childConfig.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, childConfig)
	require.NoError(t, err)
	defer pool.Close()

	_, err = pool.Exec(ctx, `CREATE SCHEMA elitea_runtime;
CREATE TABLE lock_a (id int); CREATE TABLE lock_b (id int);`)
	require.NoError(t, err)

	files := fstest.MapFS{
		"shared/0001_lock_a.sql": {Data: []byte("LOCK TABLE lock_a IN ACCESS EXCLUSIVE MODE;")},
		"shared/0002_lock_b.sql": {Data: []byte("LOCK TABLE lock_b IN ACCESS EXCLUSIVE MODE;")},
	}

	// The "running stack": holds lock_b, then (once the migrator is queued on
	// it) asks for lock_a, which the migrator already holds.
	other, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = other.Exec(ctx, "LOCK TABLE lock_b IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)

	otherDone := make(chan error, 1)
	go func() {
		for {
			var waiting bool
			if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks l
JOIN pg_class c ON c.oid = l.relation
WHERE c.relname = 'lock_b' AND NOT l.granted)`).Scan(&waiting); err != nil {
				otherDone <- err
				return
			}
			if waiting {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		// Blocks until the migrator is aborted as the deadlock victim.
		if _, err := other.Exec(ctx, "LOCK TABLE lock_a IN ACCESS EXCLUSIVE MODE"); err != nil {
			otherDone <- err
			return
		}
		otherDone <- other.Commit(ctx)
	}()

	original := deadlockBackoff
	t.Cleanup(func() { deadlockBackoff = original })
	deadlockBackoff = func(int) time.Duration { return 10 * time.Millisecond }
	require.NoError(t, New(pool, files).ApplyShared(ctx))
	require.NoError(t, <-otherDone, "the concurrent transaction must not be the victim")
	require.NoError(t, New(pool, files).CheckHead(ctx, ScopeShared, "platform"))
}

func TestIsDeadlock(t *testing.T) {
	require.False(t, isDeadlock(nil))
	require.False(t, isDeadlock(fmt.Errorf("plain")))
	require.True(t, isDeadlock(fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "40P01"})))
	require.False(t, isDeadlock(&pgconn.PgError{Code: "23505"}))
}
