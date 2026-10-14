package toolkitcleanup_test

// A memory toolkit's delete against a real PostgreSQL (shared/0166): the
// namespace its setting named is journaled in the delete's transaction, because
// the setting leaves with the row, and the reconciler's replica asks the vector
// store to delete that namespace (every owner's points of it).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcleanup"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
)

type fakeMemory struct {
	calls []struct {
		project   int64
		namespace string
	}
	err error
}

func (f *fakeMemory) DeleteMemoryNamespace(_ context.Context, projectID int64, namespace string) error {
	f.calls = append(f.calls, struct {
		project   int64
		namespace string
	}{projectID, namespace})
	return f.err
}

func enqueueMemory(ctx context.Context, t *testing.T, pool *pgxpool.Pool, journal *toolkitcleanup.Journal, namespace string) string {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	app, err := journal.EnqueueMemory(ctx, tx, 7, 70, namespace)
	if err != nil {
		t.Fatalf("enqueue memory: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestAMemoryToolkitDeleteJournalsItsNamespaceAndTheReconcilerDeletesIt(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	memory := &fakeMemory{}
	journal := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithMemory(memory, true))

	if app := enqueueMemory(ctx, t, pool, journal, "team-notes"); app != toolkitcleanup.AppMemory {
		t.Fatalf("enqueued app = %q", app)
	}
	var namespace string
	if err := pool.QueryRow(ctx, `SELECT namespace FROM centry.toolkit_deletions WHERE project_id = 7 AND toolkit_id = 70`).Scan(&namespace); err != nil || namespace != "team-notes" {
		t.Fatalf("the journal row's namespace = %q (%v)", namespace, err)
	}
	if completed, err := claimAndResume(ctx, t, journal); err != nil || !completed {
		t.Fatalf("resume: completed=%v err=%v", completed, err)
	}
	if len(memory.calls) != 1 || memory.calls[0].project != 7 || memory.calls[0].namespace != "team-notes" {
		t.Fatalf("the vector store was asked %v", memory.calls)
	}
	row := readRow(ctx, t, pool, 7, 70)
	if deref(row.outcome) != toolkitcleanup.OutcomeDeleted || row.completed == nil {
		t.Fatalf("closed row = %+v outcome=%q", row, deref(row.outcome))
	}
}

func TestAMemoryRowRollsBackWithTheDeleteAndANonRecordingReplicaWritesNone(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	journal := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithMemory(&fakeMemory{}, true))
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.EnqueueMemory(ctx, tx, 7, 70, "n"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// A deployment that keeps memories in the Python worker's database has
	// nothing in the vector store to delete.
	python := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithMemory(&fakeMemory{}, false))
	if app := enqueueMemory(ctx, t, pool, python, "n"); app != "" {
		t.Fatalf("a non-recording replica enqueued %q", app)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM centry.toolkit_deletions`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("%d journal rows (%v)", rows, err)
	}
}

func TestAMemoryRowWaitsForAReplicaWithTheClientAndRetriesATransientFailure(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// The deciding replica records it without a client.
	unwired := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithMemory(nil, true))
	enqueueMemory(ctx, t, pool, unwired, "n")
	completed, err := claimAndResume(ctx, t, unwired)
	if completed || err == nil {
		t.Fatalf("an unwired replica closed the memory row: %v %v", completed, err)
	}
	if row := readRow(ctx, t, pool, 7, 70); row.completed != nil {
		t.Fatalf("the row was closed: %+v", row)
	}
	reopenForRetry(ctx, t, pool)
	// A replica with the client, whose store is briefly down.
	memory := &fakeMemory{err: errors.Join(platformops.ErrRetryLater)}
	wired := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithMemory(memory, true))
	if completed, err := claimAndResume(ctx, t, wired); completed || !errors.Is(err, platformops.ErrRetryLater) {
		t.Fatalf("transient: completed=%v err=%v", completed, err)
	}
	reopenForRetry(ctx, t, pool)
	memory.err = nil
	if completed, err := claimAndResume(ctx, t, wired); err != nil || !completed {
		t.Fatalf("recovered: completed=%v err=%v", completed, err)
	}
	// A refused certificate is closed as a permanent failure, the way an
	// engine's is; the vector store logs what to fix.
	enqueueMemory(ctx, t, pool, wired, "n")
	memory.err = platformops.ErrNotAllowed
	if completed, err := claimAndResume(ctx, t, wired); err != nil || !completed {
		t.Fatalf("refused: completed=%v err=%v", completed, err)
	}
	if row := readRow(ctx, t, pool, 7, 70); deref(row.outcome) != toolkitcleanup.OutcomeFailedPermanent {
		t.Fatalf("outcome = %q", deref(row.outcome))
	}
}
