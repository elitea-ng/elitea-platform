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
	// WHICH SIDE IS THE VICTIM is decided by timing: a backend runs the
	// deadlock check once, deadlock_timeout (1s by default) after it starts
	// waiting, and aborts ITSELF if it finds the cycle. The migrator waits
	// first, so it is the victim only if the other side is already waiting
	// when the migrator's check runs. On a loaded runner the other side's
	// poll-then-LOCK took longer than one second, the migrator's check found
	// no cycle, and the other side became the victim (the flake of PR 3001).
	// A longer deadlock_timeout on this throwaway database gives the other
	// side that long to queue. It needs a superuser; without one the test
	// keeps the default and its old timing.
	if _, err := admin.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{name}.Sanitize()+" SET deadlock_timeout = '5s'"); err != nil {
		t.Logf("keeping the default deadlock_timeout: %v", err)
	}
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
	// The 5 s above is BOTH sides' timeout, so it only widened the window for
	// the other side to queue; it did not order the two checks. The other side
	// queues 20 to 60 ms after the migrator, so the two checks fire that far
	// apart, and on a loaded runner the migrator's timer can be served after
	// the other side's: the other side found the cycle first and was the
	// victim (the flake of PR 3035). This transaction's own check comes after
	// the test's deadline, so only the migrator's check can break the cycle.
	// In a savepoint: without a superuser the SET fails, and a failed
	// statement would abort the whole transaction.
	setting, err := other.Begin(ctx)
	require.NoError(t, err)
	if _, err := setting.Exec(ctx, "SET LOCAL deadlock_timeout = '120s'"); err != nil {
		t.Logf("keeping the database's deadlock_timeout for the other side: %v", err)
		require.NoError(t, setting.Rollback(ctx))
	} else {
		require.NoError(t, setting.Commit(ctx))
	}
	_, err = other.Exec(ctx, "LOCK TABLE lock_b IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)

	otherDone := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
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
	// Runs before pool.Close (defers are last in, first out): a transaction
	// left open, aborted or not, holds its connection, and pool.Close waits
	// for it for ever. That turned one failed assertion into a 45-minute
	// package timeout. cancel unblocks the goroutine when the test failed
	// before it finished.
	defer func() {
		cancel()
		<-finished
		_ = other.Rollback(context.Background())
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
