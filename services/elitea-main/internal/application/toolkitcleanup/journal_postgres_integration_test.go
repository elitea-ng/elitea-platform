package toolkitcleanup_test

// The toolkit cleanup journal against a real PostgreSQL (migration
// shared/0163): enqueue inside a transaction, claim with a lease, resume, and
// the row's closing, backoff, reopening and superseding. Gated on
// ELITEA_TEST_DATABASE_URL, which ci-go.yml sets; the template is the ledgered
// migration corpus over the bootstrap seed, as the other postgres suites use.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcleanup"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const (
	databaseURLEnv  = "ELITEA_TEST_DATABASE_URL"
	bootstrapSchema = "../../infra/db/migrations/001_initial.sql"
)

var journalTemplate string

func TestMain(m *testing.M) {
	databaseURL := os.Getenv(databaseURLEnv)
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
	databaseURL := os.Getenv(databaseURLEnv)
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
	nextAttemptAt            time.Time
	claimedUntil, completed  *time.Time
}

func readRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID, toolkitID int64) journalRow {
	t.Helper()
	var r journalRow
	if err := pool.QueryRow(ctx, `
SELECT outcome, note, last_error, attempts, next_attempt_at, claimed_until, completed_at
FROM centry.toolkit_deletions WHERE project_id = $1 AND toolkit_id = $2`, projectID, toolkitID,
	).Scan(&r.outcome, &r.note, &r.lastError, &r.attempts, &r.nextAttemptAt, &r.claimedUntil, &r.completed); err != nil {
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
	app, err := journal.Enqueue(ctx, tx, projectID, toolkitID, "inventory_inventory")
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
	id, ok, err := journal.ClaimNext(ctx, 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !ok {
		t.Fatal("nothing claimable")
	}
	return journal.Resume(ctx, id)
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
	if _, err := journal.Enqueue(ctx, tx, 7, 70, "inventory_inventory"); err != nil {
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

	id, ok, err := journal.ClaimNext(ctx, 0)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if leased := readRow(ctx, t, pool, 7, 70); leased.claimedUntil == nil || !leased.claimedUntil.After(time.Now()) {
		t.Fatalf("a claimed row carries no lease: %+v", leased)
	}
	if _, again, _ := journal.ClaimNext(ctx, 0); again {
		t.Fatal("a leased row was claimed twice")
	}
	completed, err := journal.Resume(ctx, id)
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
	if completed, err := journal.Resume(ctx, id); err != nil || !completed {
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

// A replica without the client for the row's application leaves it pending:
// open, backed off, and finished by a replica that has the client.
func TestAnUnwiredReplicaLeavesTheRowForOneThatIsWired(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	wired := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithRemover(platformops.AppInventory, &fakeRemover{deleted: true}))
	enqueue(ctx, t, pool, wired, 7, 70)

	unwired := toolkitcleanup.New(pool, quiet())
	completed, err := claimAndResume(ctx, t, unwired)
	if completed || err == nil {
		t.Fatalf("an unwired replica closed the row: completed=%v err=%v", completed, err)
	}
	row := readRow(ctx, t, pool, 7, 70)
	if row.completed != nil || row.outcome != nil || row.claimedUntil != nil {
		t.Fatalf("row after the unwired run = %+v", row)
	}
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

// Permanent and unsupported answers close the row at once, with the reason.
func TestTerminalAnswersCloseTheRowWithTheReason(t *testing.T) {
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
	if note := deref(row.note); !contains(note, "allowlist") {
		t.Fatalf("note = %q, want the reason", note)
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
	if row.completed != nil || row.outcome != nil || row.attempts != 0 {
		t.Fatalf("the row was not reopened: %+v", row)
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
		`INSERT INTO p_7.elitea_tools (id, type) VALUES (70, 'inventory_inventory')`,
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
	id, ok, err := journal.ClaimNext(ctx, 0)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	completed, err := journal.Resume(stopping, id)
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
