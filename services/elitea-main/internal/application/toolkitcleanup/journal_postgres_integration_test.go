package toolkitcleanup_test

// The toolkit cleanup journal against a real PostgreSQL (migration
// shared/0163): enqueue inside a transaction, claim with a lease, resume, and
// the row's closing, backoff, reopening (with its generation), superseding,
// and a close that cannot be written. Gated on
// ELITEA_TEST_DATABASE_URL, which ci-go.yml sets; the template is the ledgered
// migration corpus over the bootstrap seed, as the other postgres suites use.

import (
	"context"
	"errors"
	"fmt"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitcleanup"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/platformops"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	databaseURLEnv  = "ELITEA_TEST_DATABASE_URL"
	bootstrapSchema = "../../infra/db/migrations/001_initial.sql"
)

var journalTemplate string

func TestMain(m *testing.M) {
	databaseURL := testpg.DSN(os.Getenv(databaseURLEnv))
	if databaseURL == "" {
		os.Exit(m.Run())
	}
	bootstrap, err := os.ReadFile(bootstrapSchema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read bootstrap schema: %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := dbtest.BuildContext(context.Background())
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open admin pool: %v\n", err)
		cancel()
		os.Exit(1)
	}
	templateName, err := dbtest.EnsureTemplate(ctx, adminPool, dbtest.Spec{
		Files: platformmigrations.Files,
		Seed:  string(bootstrap),
	})
	adminPool.Close()
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build toolkitcleanup template: %v\n", err)
		os.Exit(1)
	}
	journalTemplate = templateName
	os.Exit(m.Run())
}

func newJournalPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(databaseURLEnv))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", databaseURLEnv)
	}
	if journalTemplate == "" {
		t.Fatalf("TestMain did not build the toolkitcleanup template")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()

	databaseName := fmt.Sprintf("elitea_toolkitcleanup_%d_%d", os.Getpid(), time.Now().UnixNano())
	if err := dbtest.CreateFromTemplate(ctx, adminPool, journalTemplate, databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", databaseURLEnv, err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, databaseName))
	})
	return pool
}

type fakeRemover struct {
	calls   [][2]int64
	deleted bool
	err     error
}

func (f *fakeRemover) DeleteToolkitData(_ context.Context, projectID, toolkitID int64) (platformops.ToolkitDeletion, error) {
	f.calls = append(f.calls, [2]int64{projectID, toolkitID})
	return platformops.ToolkitDeletion{Deleted: f.deleted}, f.err
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// journalRow is what the tests read back of one row.
type journalRow struct {
	outcome, note, lastError *string
	attempts                 int
	generation               int64
	nextAttemptAt            time.Time
	claimedUntil, completed  *time.Time
}

func readRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID, toolkitID int64) journalRow {
	t.Helper()
	var r journalRow
	if err := pool.QueryRow(ctx, `
SELECT outcome, note, last_error, attempts, generation, next_attempt_at, claimed_until, completed_at
FROM centry.toolkit_deletions WHERE project_id = $1 AND toolkit_id = $2`, projectID, toolkitID,
	).Scan(&r.outcome, &r.note, &r.lastError, &r.attempts, &r.generation, &r.nextAttemptAt, &r.claimedUntil, &r.completed); err != nil {
		t.Fatalf("read journal row: %v", err)
	}
	return r
}

func enqueue(ctx context.Context, t *testing.T, pool *pgxpool.Pool, journal *toolkitcleanup.Journal, projectID, toolkitID int64) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	app, err := journal.Enqueue(ctx, tx, projectID, toolkitID, "inventory")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if app != platformops.AppInventory {
		t.Fatalf("enqueued app = %q", app)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func claimAndResume(ctx context.Context, t *testing.T, journal *toolkitcleanup.Journal) (bool, error) {
	t.Helper()
	return journal.Resume(ctx, claim(ctx, t, journal))
}

func claim(ctx context.Context, t *testing.T, journal *toolkitcleanup.Journal) toolkitcleanup.Claim {
	t.Helper()
	c, ok, err := journal.ClaimNext(ctx, 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !ok {
		t.Fatal("nothing claimable")
	}
	return c
}

func reopenForRetry(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE centry.toolkit_deletions SET next_attempt_at = clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// An enqueued row rolls back with the transaction that wrote it: the journal
// is part of the delete's transaction, not beside it.
func TestEnqueueIsPartOfTheCallersTransaction(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	journal := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithRemover(platformops.AppInventory, &fakeRemover{}))

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Enqueue(ctx, tx, 7, 70, "inventory"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM centry.toolkit_deletions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("%d journal rows after a rollback", rows)
	}
}

// The whole path: enqueue, claim (leased), resume, closed with the engine's
// outcome; nothing left to claim.
func TestAClaimedRowIsResumedAndClosedWithTheEnginesOutcome(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	remover := &fakeRemover{deleted: true}
	journal := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithRemover(platformops.AppInventory, remover))
	enqueue(ctx, t, pool, journal, 7, 70)

	claimed, ok, err := journal.ClaimNext(ctx, 0)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if claimed.Generation != 1 {
		t.Fatalf("a fresh row's claim is at generation %d, want 1", claimed.Generation)
	}
	if leased := readRow(ctx, t, pool, 7, 70); leased.claimedUntil == nil || !leased.claimedUntil.After(time.Now()) {
		t.Fatalf("a claimed row carries no lease: %+v", leased)
	}
	if _, again, _ := journal.ClaimNext(ctx, 0); again {
		t.Fatal("a leased row was claimed twice")
	}
	completed, err := journal.Resume(ctx, claimed)
	if err != nil || !completed {
		t.Fatalf("resume: completed=%v err=%v", completed, err)
	}
	row := readRow(ctx, t, pool, 7, 70)
	if deref(row.outcome) != toolkitcleanup.OutcomeDeleted || row.completed == nil || row.claimedUntil != nil || row.attempts != 1 {
		t.Fatalf("closed row = %+v outcome=%q", row, deref(row.outcome))
	}
	if len(remover.calls) != 1 || remover.calls[0] != [2]int64{7, 70} {
		t.Fatalf("engine called with %v", remover.calls)
	}
	if _, more, _ := journal.ClaimNext(ctx, 0); more {
		t.Fatal("a closed row was claimable")
	}
	// Resuming a closed row again is a no-op that reports completion.
	if completed, err := journal.Resume(ctx, claimed); err != nil || !completed {
		t.Fatalf("resume of a closed row: %v %v", completed, err)
	}
}

// A transient refusal leaves the row open with a backoff and the failure's
// class (not the raw error); the row is not claimable before the backoff.
func TestATransientFailureBacksOffAndIsRetried(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	remover := &fakeRemover{err: fmt.Errorf("%w: inventory delete_toolkit: ingest running at 10.0.7.3 (FailedPrecondition)", platformops.ErrRetryLater)}
	journal := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithRemover(platformops.AppInventory, remover))
	enqueue(ctx, t, pool, journal, 7, 70)

	completed, err := claimAndResume(ctx, t, journal)
	if completed || !errors.Is(err, platformops.ErrRetryLater) {
		t.Fatalf("resume: completed=%v err=%v", completed, err)
	}
	row := readRow(ctx, t, pool, 7, 70)
	if row.completed != nil || row.claimedUntil != nil || row.attempts != 1 {
		t.Fatalf("open row = %+v", row)
	}
	if !row.nextAttemptAt.After(time.Now().Add(30 * time.Second)) {
		t.Fatalf("no backoff: next attempt at %v", row.nextAttemptAt)
	}
	if last := deref(row.lastError); last == "" || containsAddress(last) {
		t.Fatalf("last_error = %q, want the class without the address", last)
	}
	if _, ok, _ := journal.ClaimNext(ctx, 0); ok {
		t.Fatal("a backed-off row was claimed before its time")
	}
	// Past the backoff the row is worked again, and a healthy engine closes it.
	if _, err := pool.Exec(ctx, `UPDATE centry.toolkit_deletions SET next_attempt_at = clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
	remover.err, remover.deleted = nil, false
	if completed, err := claimAndResume(ctx, t, journal); err != nil || !completed {
		t.Fatalf("second run: completed=%v err=%v", completed, err)
	}
	row = readRow(ctx, t, pool, 7, 70)
	if deref(row.outcome) != toolkitcleanup.OutcomeNothingToDelete || row.attempts != 2 || deref(row.lastError) != "" {
		t.Fatalf("closed row = %+v outcome=%q", row, deref(row.outcome))
	}
}

func containsAddress(s string) bool {
	for i := 0; i+8 <= len(s); i++ {
		if s[i:i+8] == "10.0.7.3" {
			return true
		}
	}
	return false
}

// A replica without the client for the row's application ENQUEUES the row all
// the same (the obligation does not depend on the replica's wiring) and, when
// it claims it, leaves it pending: open, backed off with the reason as its
// class, and finished by a replica that has the client.
func TestAnUnwiredReplicaEnqueuesAndLeavesTheRowForOneThatIsWired(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	unwired := toolkitcleanup.New(pool, quiet())
	enqueue(ctx, t, pool, unwired, 7, 70)

	completed, err := claimAndResume(ctx, t, unwired)
	if completed || err == nil {
		t.Fatalf("an unwired replica closed the row: completed=%v err=%v", completed, err)
	}
	row := readRow(ctx, t, pool, 7, 70)
	if row.completed != nil || row.outcome != nil || row.claimedUntil != nil || row.attempts != 1 {
		t.Fatalf("row after the unwired run = %+v", row)
	}
	if !contains(deref(row.lastError), "no client for the application") || !row.nextAttemptAt.After(time.Now().Add(30*time.Second)) {
		t.Fatalf("row after the unwired run: last_error=%q next_attempt_at=%v, want the class and a backoff", deref(row.lastError), row.nextAttemptAt)
	}
	wired := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithRemover(platformops.AppInventory, &fakeRemover{deleted: true}))
	if _, err := pool.Exec(ctx, `UPDATE centry.toolkit_deletions SET next_attempt_at = clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
	if completed, err := claimAndResume(ctx, t, wired); err != nil || !completed {
		t.Fatalf("the wired replica did not finish it: completed=%v err=%v", completed, err)
	}
	if row := readRow(ctx, t, pool, 7, 70); deref(row.outcome) != toolkitcleanup.OutcomeDeleted {
		t.Fatalf("outcome = %q", deref(row.outcome))
	}
}

// Permanent and unsupported answers close the row at once, with the CLASS of
// the reason as the note (never the engine's message).
func TestTerminalAnswersCloseTheRowWithTheReasonsClass(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	refused := fmt.Errorf("%w: inventory delete_toolkit: CN=elitea-main is not in the allowlist (PermissionDenied)", platformops.ErrNotAllowed)
	journal := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithRemover(platformops.AppInventory, &fakeRemover{err: refused}))
	enqueue(ctx, t, pool, journal, 7, 70)
	if completed, err := claimAndResume(ctx, t, journal); err != nil || !completed {
		t.Fatalf("resume: completed=%v err=%v", completed, err)
	}
	row := readRow(ctx, t, pool, 7, 70)
	if deref(row.outcome) != toolkitcleanup.OutcomeFailedPermanent || row.completed == nil {
		t.Fatalf("row = %+v outcome=%q", row, deref(row.outcome))
	}
	if note := deref(row.note); note != toolkitcleanup.NoteRefused || contains(note, "allowlist") || contains(note, "CN=") {
		t.Fatalf("note = %q, want %q and none of the engine's message", note, toolkitcleanup.NoteRefused)
	}

	unsupported := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithRemover(platformops.AppInventory,
		&fakeRemover{err: fmt.Errorf("%w: inventory delete_toolkit", platformops.ErrNotSupported)}))
	enqueue(ctx, t, pool, unsupported, 7, 71)
	if completed, err := claimAndResume(ctx, t, unsupported); err != nil || !completed {
		t.Fatalf("resume: completed=%v err=%v", completed, err)
	}
	if row := readRow(ctx, t, pool, 7, 71); deref(row.outcome) != toolkitcleanup.OutcomeUnsupported || deref(row.note) != toolkitcleanup.NoteUnsupported {
		t.Fatalf("row = %+v outcome=%q note=%q", row, deref(row.outcome), deref(row.note))
	}
}

func contains(s, needle string) bool {
	for i := 0; i+len(needle) <= len(s); i++ {
		if s[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// A second delete of the same toolkit id reopens its (closed) row.
func TestEnqueueReopensAClosedRow(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	journal := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithRemover(platformops.AppInventory, &fakeRemover{deleted: true}))
	enqueue(ctx, t, pool, journal, 7, 70)
	if completed, err := claimAndResume(ctx, t, journal); err != nil || !completed {
		t.Fatalf("resume: completed=%v err=%v", completed, err)
	}
	enqueue(ctx, t, pool, journal, 7, 70)
	row := readRow(ctx, t, pool, 7, 70)
	if row.completed != nil || row.outcome != nil || row.attempts != 0 || row.claimedUntil != nil || row.generation != 2 {
		t.Fatalf("the row was not reopened at the next generation: %+v", row)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM centry.toolkit_deletions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("%d rows for one toolkit", rows)
	}
}

// A toolkit whose id exists again in its project (a restored database) is not
// deleted from the engine: the row closes as superseded without a call.
func TestAToolkitThatExistsAgainSupersedesTheRow(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	remover := &fakeRemover{deleted: true}
	journal := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithRemover(platformops.AppInventory, remover))
	enqueue(ctx, t, pool, journal, 7, 70)
	for _, statement := range []string{
		`INSERT INTO centry.project (id, name, owner_id, create_success) VALUES (7, 'restored', 1, true)`,
		`CREATE SCHEMA p_7`,
		`CREATE TABLE p_7.elitea_tools (id INTEGER PRIMARY KEY, type TEXT NOT NULL)`,
		`INSERT INTO p_7.elitea_tools (id, type) VALUES (70, 'inventory')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if completed, err := claimAndResume(ctx, t, journal); err != nil || !completed {
		t.Fatalf("resume: completed=%v err=%v", completed, err)
	}
	if len(remover.calls) != 0 {
		t.Fatalf("the engine was called for a live toolkit: %v", remover.calls)
	}
	if row := readRow(ctx, t, pool, 7, 70); deref(row.outcome) != toolkitcleanup.OutcomeSuperseded {
		t.Fatalf("outcome = %q", deref(row.outcome))
	}
}

// A run the process stops releases the lease without counting an attempt.
func TestAnInterruptedRunReleasesTheLeaseWithoutAnAttempt(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stopping, stop := context.WithCancel(ctx)
	remover := &stoppingRemover{stop: stop}
	journal := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithRemover(platformops.AppInventory, remover))
	enqueue(ctx, t, pool, journal, 7, 70)
	claimed, ok, err := journal.ClaimNext(ctx, 0)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	completed, err := journal.Resume(stopping, claimed)
	if completed || err != nil {
		t.Fatalf("an interrupted run reported completed=%v err=%v", completed, err)
	}
	row := readRow(ctx, t, pool, 7, 70)
	if row.attempts != 0 || row.claimedUntil != nil || deref(row.lastError) != "interrupted" {
		t.Fatalf("row after the interruption = %+v last_error=%q", row, deref(row.lastError))
	}
}

// stoppingRemover cancels the run's parent context from inside the call, as a
// shutdown arriving mid-call would, and answers as a cancelled call does.
type stoppingRemover struct{ stop context.CancelFunc }

func (s *stoppingRemover) DeleteToolkitData(_ context.Context, _, _ int64) (platformops.ToolkitDeletion, error) {
	s.stop()
	return platformops.ToolkitDeletion{}, context.Canceled
}

// A row reopened under a claim (a second delete of the same toolkit id while a
// run still holds the old claim) is the stale run's no longer: its resume is
// ErrClaimLost and touches nothing, and the new obligation is claimed and
// closed on its own.
func TestAReopenedRowIsNotTheStaleClaimsToResume(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	remover := &fakeRemover{deleted: true}
	journal := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithRemover(platformops.AppInventory, remover))
	enqueue(ctx, t, pool, journal, 7, 70)
	stale := claim(ctx, t, journal)

	// The toolkit id came back and was deleted again while the claim is held:
	// the reopen clears the lease and bumps the generation.
	enqueue(ctx, t, pool, journal, 7, 70)
	reopened := readRow(ctx, t, pool, 7, 70)
	if reopened.generation != stale.Generation+1 || reopened.claimedUntil != nil {
		t.Fatalf("reopened row = %+v, want generation %d and no lease", reopened, stale.Generation+1)
	}
	completed, err := journal.Resume(ctx, stale)
	if completed || !errors.Is(err, toolkitcleanup.ErrClaimLost) {
		t.Fatalf("stale resume: completed=%v err=%v, want ErrClaimLost", completed, err)
	}
	if len(remover.calls) != 0 {
		t.Fatalf("the stale run called the engine: %v", remover.calls)
	}
	if row := readRow(ctx, t, pool, 7, 70); row.attempts != 0 || row.completed != nil || row.lastError != nil {
		t.Fatalf("the stale run touched the reopened row: %+v", row)
	}

	fresh := claim(ctx, t, journal)
	if fresh.ID != stale.ID || fresh.Generation != stale.Generation+1 {
		t.Fatalf("fresh claim = %+v, want the same row at the next generation (stale %+v)", fresh, stale)
	}
	if completed, err := journal.Resume(ctx, fresh); err != nil || !completed {
		t.Fatalf("fresh resume: completed=%v err=%v", completed, err)
	}
	if row := readRow(ctx, t, pool, 7, 70); deref(row.outcome) != toolkitcleanup.OutcomeDeleted || row.attempts != 1 {
		t.Fatalf("row after the fresh run = %+v outcome=%q", row, deref(row.outcome))
	}
}

// The reopen can also land DURING the stale run's engine call. Its close then
// matches no row at its generation: the run reports ErrClaimLost, the new
// obligation stays open and unbacked-off, and the next claim works it.
func TestAReopenDuringTheRunKeepsTheStaleRunFromClosingTheNewObligation(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	journal := toolkitcleanup.New(pool, quiet())
	remover := &reopeningRemover{reopen: func() { enqueue(ctx, t, pool, journal, 7, 70) }}
	journal = toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithRemover(platformops.AppInventory, remover))
	enqueue(ctx, t, pool, journal, 7, 70)

	completed, err := claimAndResume(ctx, t, journal)
	if completed || !errors.Is(err, toolkitcleanup.ErrClaimLost) {
		t.Fatalf("resume across a reopen: completed=%v err=%v, want ErrClaimLost", completed, err)
	}
	row := readRow(ctx, t, pool, 7, 70)
	// No backoff either: the reopen set next_attempt_at to now (a few seconds
	// of clock difference between the database and this process allowed).
	if row.completed != nil || row.outcome != nil || row.attempts != 0 || row.generation != 2 || row.nextAttemptAt.After(time.Now().Add(5*time.Second)) {
		t.Fatalf("the new obligation was touched by the stale run: %+v", row)
	}
	remover.reopen = nil
	if completed, err := claimAndResume(ctx, t, journal); err != nil || !completed {
		t.Fatalf("the next run did not close the new obligation: completed=%v err=%v", completed, err)
	}
	if row := readRow(ctx, t, pool, 7, 70); deref(row.outcome) != toolkitcleanup.OutcomeDeleted || row.generation != 2 || row.attempts != 1 {
		t.Fatalf("row after the next run = %+v outcome=%q", row, deref(row.outcome))
	}
}

// reopeningRemover re-enqueues the row from inside the engine call, as a second
// delete of the same toolkit id landing mid-call would, then answers deleted.
type reopeningRemover struct{ reopen func() }

func (r *reopeningRemover) DeleteToolkitData(context.Context, int64, int64) (platformops.ToolkitDeletion, error) {
	if r.reopen != nil {
		r.reopen()
	}
	return platformops.ToolkitDeletion{Deleted: true}, nil
}

// A close that cannot be written is a FAILED run, not a silent success: the
// resume reports the error (the reconciler counts it), the row is backed off
// with the failure's class, and a later run repeats the idempotent call and
// closes it.
func TestACloseThatCannotBeWrittenIsAFailedRun(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	remover := &fakeRemover{deleted: true}
	journal := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithRemover(platformops.AppInventory, remover))
	enqueue(ctx, t, pool, journal, 7, 70)
	for _, statement := range []string{
		`CREATE FUNCTION centry.refuse_close_for_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'the test refuses the close'; END $$`,
		`CREATE TRIGGER refuse_close_for_test BEFORE UPDATE OF completed_at ON centry.toolkit_deletions
		 FOR EACH ROW WHEN (NEW.completed_at IS NOT NULL) EXECUTE FUNCTION centry.refuse_close_for_test()`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	completed, err := claimAndResume(ctx, t, journal)
	if completed || err == nil || !contains(err.Error(), "could not be closed") {
		t.Fatalf("resume with a refused close: completed=%v err=%v, want the close failure", completed, err)
	}
	row := readRow(ctx, t, pool, 7, 70)
	if row.completed != nil || row.outcome != nil || row.claimedUntil != nil || row.attempts != 1 {
		t.Fatalf("row after the refused close = %+v", row)
	}
	if !contains(deref(row.lastError), "could not be closed") || !row.nextAttemptAt.After(time.Now().Add(30*time.Second)) {
		t.Fatalf("row after the refused close: last_error=%q next_attempt_at=%v, want the class and a backoff", deref(row.lastError), row.nextAttemptAt)
	}
	if len(remover.calls) != 1 {
		t.Fatalf("engine calls = %d, want one", len(remover.calls))
	}

	if _, err := pool.Exec(ctx, `DROP TRIGGER refuse_close_for_test ON centry.toolkit_deletions`); err != nil {
		t.Fatal(err)
	}
	reopenForRetry(ctx, t, pool)
	if completed, err := claimAndResume(ctx, t, journal); err != nil || !completed {
		t.Fatalf("the retry did not close the row: completed=%v err=%v", completed, err)
	}
	if row := readRow(ctx, t, pool, 7, 70); deref(row.outcome) != toolkitcleanup.OutcomeDeleted || row.attempts != 2 || len(remover.calls) != 2 {
		t.Fatalf("row after the retry = %+v outcome=%q calls=%d", row, deref(row.outcome), len(remover.calls))
	}
}
