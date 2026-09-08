package secrets

// The backfill runs on ONE replica at a time (O4).
//
// BackfillProjectSecretsHeaderValues runs on every elitea-main replica before
// its listeners bind, and the chart's default is two with an autoscaler that
// may add eight more. EnsureProjectSecretsHeaderValue is a read-modify-write of
// the WHOLE vault — decrypt, check one key, re-encrypt everything back — so
// concurrent passes do not merely duplicate work. Each generates a DIFFERENT
// random value, the last write wins, and a client that read the first is
// refused. A legitimate secret written through the secrets API between one
// replica's read and its write is clobbered outright.
//
// These tests need a real PostgreSQL because the serialisation IS a PostgreSQL
// advisory lock. A fake would assert that the code calls a lock function, which
// is the claim least worth checking.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedProjectsWithVaults creates the project rows the backfill joins against
// and a vault for each, with NO header value — the state a backfill exists to
// repair.
func seedProjectsWithVaults(t *testing.T, pool *pgxpool.Pool, projectIDs ...string) {
	t.Helper()
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS centry.project (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create centry.project: %v", err)
	}
	handler := NewHandler(pool)
	for _, projectID := range projectIDs {
		if _, err := pool.Exec(ctx,
			`INSERT INTO centry.project (id) VALUES ($1::integer) ON CONFLICT DO NOTHING`,
			projectID); err != nil {
			t.Fatalf("seed project %s: %v", projectID, err)
		}
		if err := handler.writeVaultCtx(ctx, projectID, vaultData{
			Secrets:       map[string]string{"unrelated": "keep-me-" + projectID},
			HiddenSecrets: map[string]string{},
		}); err != nil {
			t.Fatalf("seed project %s vault: %v", projectID, err)
		}
	}
}

// advisoryLocksHeld counts the backfill advisory locks the server holds now.
//
// It asks PostgreSQL, because the pass itself cannot answer: advisory locks are
// re-entrant inside one session, so a second pass on the same pooled connection
// sees a held lock as a free one.
func advisoryLocksHeld(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var held int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM pg_locks
WHERE locktype = 'advisory' AND objid = ($1::bigint & 4294967295)::oid`,
		backfillLockKey).Scan(&held); err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	return held
}

// poolBesideTheTestPool opens a SECOND pool on the same isolated database.
//
// The barrier below holds a transaction open for the length of the test. A
// connection out of the pool under test would take one of its four, and the
// two passes plus the barrier would then contend for connections instead of
// for the lock — which is the very thing this test measures.
func poolBesideTheTestPool(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	config := pool.Config().Copy()
	config.MaxConns = 2
	beside, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open a second pool on the test database: %v", err)
	}
	t.Cleanup(beside.Close)
	return beside
}

// TestOnlyOneReplicaRunsTheBackfill measures mutual exclusion, so the two
// passes must OVERLAP. The overlap is forced, not hoped for.
//
// THE EARLIER VERSION OF THIS TEST STARTED TWO GOROUTINES AND ASSERTED THE
// RESULT. Nothing made the second one reach `pg_try_advisory_lock` before the
// first one released it. On a loaded CI runner the scheduler ran them one after
// the other, both passes then ran legitimately, and the test reported "2
// replicas ran the pass and 0 skipped" against a lock that was working
// perfectly (#825, runs 34086841685 and 34088826392). Reproduced here by
// delaying one goroutine by 400 ms: the same two failure lines, and reports of
// `Written:3` and `AlreadySet:3` — a correct sequence, not a lost race.
//
// So the winner is PINNED INSIDE THE PASS while the loser tries. The barrier is
// a row lock on the first project's vault row, which the winner must update and
// therefore cannot pass. The winner holds the advisory lock for as long as the
// test wants, and the loser's attempt is guaranteed to be concurrent.
func TestOnlyOneReplicaRunsTheBackfill(t *testing.T) {
	pool := newSecretsPool(t)
	seedProjectsWithVaults(t, pool, "1", "2", "3")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Hold project 1's vault row. projectVaultProjectIDs orders by id, so the
	// winner meets this row first, and its write waits here.
	barrier := poolBesideTheTestPool(t, ctx, pool)
	held, err := barrier.Begin(ctx)
	if err != nil {
		t.Fatalf("open the barrier transaction: %v", err)
	}
	rolledBack := false
	defer func() {
		if !rolledBack {
			_ = held.Rollback(context.Background())
		}
	}()
	if _, err := held.Exec(ctx,
		`SELECT id FROM centry.secrets_data WHERE id = $1 FOR UPDATE`, dbKey("1")); err != nil {
		t.Fatalf("take the barrier row lock: %v", err)
	}

	type outcome struct {
		report SecretsHeaderBackfillReport
		err    error
	}
	winnerDone := make(chan outcome, 1)
	go func() {
		report, err := NewHandler(pool).BackfillProjectSecretsHeaderValues(ctx)
		winnerDone <- outcome{report, err}
	}()

	// Wait for the winner to hold the advisory lock. It cannot release it now:
	// releasing needs the pass to finish, and the pass is stopped on the
	// barrier row.
	deadline := time.Now().Add(20 * time.Second)
	for advisoryLocksHeld(t, ctx, barrier) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first pass never took the advisory lock")
		}
		select {
		case got := <-winnerDone:
			t.Fatalf("the first pass finished without meeting the barrier: %+v, %v", got.report, got.err)
		case <-time.After(10 * time.Millisecond):
		}
	}

	// The loser runs while the winner is inside the pass. This is the assertion
	// the whole file exists for.
	//
	// Its own short deadline is part of the assertion. A pass that is NOT
	// locked out enters the loop and waits on the barrier row, and without a
	// deadline that wait is the test hanging rather than the test failing.
	// Measured against a build whose lock result was ignored: the loser blocks
	// on the row, and this deadline names why.
	loserCtx, loserCancel := context.WithTimeout(ctx, 10*time.Second)
	defer loserCancel()
	loser, err := NewHandler(pool).BackfillProjectSecretsHeaderValues(loserCtx)
	if err != nil {
		t.Fatalf("the second replica was not locked out: it entered the pass and waited "+
			"on the barrier row the winner holds: %v", err)
	}
	if !loser.SkippedLocked {
		t.Fatalf("two replicas ran the pass at the same time; exactly one must run: %+v", loser)
	}
	if loser.Written != 0 || loser.Vaults != 0 {
		t.Fatalf("the locked-out replica did work: %+v", loser)
	}

	// Release the barrier and let the winner finish. It must write all three,
	// including the row the barrier held.
	if err := held.Rollback(context.Background()); err != nil {
		t.Fatalf("release the barrier: %v", err)
	}
	rolledBack = true

	select {
	case got := <-winnerDone:
		if got.err != nil {
			t.Fatalf("the first replica: %v", got.err)
		}
		if got.report.SkippedLocked {
			t.Fatalf("the first replica reported that it skipped: %+v", got.report)
		}
		if got.report.Written != 3 {
			t.Fatalf("the replica that ran wrote %d values, want 3: %+v", got.report.Written, got.report)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the first pass did not finish after the barrier was released")
	}
}

func TestASkippedReplicaReportsWhyRatherThanReportingNothing(t *testing.T) {
	// An all-zero report is ambiguous: "another replica is doing it" and "every
	// vault already had a value" produce identical counts. Only SkippedLocked
	// separates them, and the caller logs a different line for each.
	pool := newSecretsPool(t)
	seedProjectsWithVaults(t, pool, "1")
	ctx := context.Background()

	// Hold the lock the way a peer replica would: a session lock on its own
	// pinned connection.
	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	var acquired bool
	if err := holder.QueryRow(ctx,
		`SELECT pg_catalog.pg_try_advisory_lock($1)`, backfillLockKey).Scan(&acquired); err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("could not take the lock to simulate a peer replica")
	}

	report, err := NewHandler(pool).BackfillProjectSecretsHeaderValues(ctx)
	if err != nil {
		t.Fatalf("a locked-out replica returned an error instead of skipping: %v", err)
	}
	if !report.SkippedLocked {
		t.Fatal("a locked-out replica did not report that it skipped")
	}
	if report.Written != 0 || report.Vaults != 0 {
		t.Fatalf("a locked-out replica did work: %+v", report)
	}
}

func TestTheLockIsReleasedSoTheNextStartCanRunThePass(t *testing.T) {
	// A session lock held on a pooled connection outlives the function unless
	// it is released explicitly — the connection goes back to the pool still
	// holding it, and every later pass in this process is then locked out by
	// its own predecessor.
	pool := newSecretsPool(t)
	seedProjectsWithVaults(t, pool, "1")
	ctx := context.Background()

	first, err := NewHandler(pool).BackfillProjectSecretsHeaderValues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.SkippedLocked || first.Written != 1 {
		t.Fatalf("the first pass did not run: %+v", first)
	}

	// ASK POSTGRES, do not ask the next pass.
	//
	// A second pass cannot tell: advisory locks are RE-ENTRANT within a
	// session, and pgxpool hands the same connection back, so a leaked lock is
	// invisible inside one process while blocking every other replica
	// forever. The first version of this test asserted only that a second pass
	// ran, and it passed against a build whose unlock was replaced with
	// another lock call.
	var held int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM pg_locks
WHERE locktype = 'advisory' AND objid = ($1::bigint & 4294967295)::oid`,
		backfillLockKey).Scan(&held); err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	if held != 0 {
		t.Fatalf("the pass left %d advisory lock(s) held; every other replica is now locked out until this connection closes", held)
	}

	second, err := NewHandler(pool).BackfillProjectSecretsHeaderValues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.SkippedLocked {
		t.Fatal("the second pass was locked out by the first, which never released")
	}
	// Nothing left to write, and that is the point: it RAN and found the work
	// done, rather than being blocked from looking.
	if second.Written != 0 || second.AlreadySet != 1 {
		t.Fatalf("the second pass: %+v", second)
	}
}

func TestTheBackfillDoesNotClobberOtherSecretsInTheVault(t *testing.T) {
	// The read-modify-write covers the whole vault, so a pass that rebuilt it
	// from scratch would drop every other secret while looking correct on the
	// one key it manages.
	pool := newSecretsPool(t)
	seedProjectsWithVaults(t, pool, "7")
	ctx := context.Background()

	if _, err := NewHandler(pool).BackfillProjectSecretsHeaderValues(ctx); err != nil {
		t.Fatal(err)
	}

	vault, err := NewHandler(pool).readVaultCtx(ctx, "7")
	if err != nil {
		t.Fatal(err)
	}
	if got := vault.Secrets["unrelated"]; got != "keep-me-7" {
		t.Fatalf("the backfill lost an unrelated secret: %q", got)
	}
	if vault.Secrets[SecretsHeaderValueName] == "" {
		t.Fatal("the backfill wrote no header value")
	}
}
