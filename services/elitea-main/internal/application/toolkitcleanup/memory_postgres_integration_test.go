package toolkitcleanup_test

// Memory cleanup against a real PostgreSQL (worker unit M9). Two kinds of
// journal row, one table, one reconciler:
//
//   - a memory TOOLKIT's delete journals the toolkit id in the delete's
//     transaction, and the reconciler's replica asks the vector store to delete
//     every point stamped with it (every namespace, every owner);
//   - a USER's removal (deleted outright: project 0, or removed from one
//     project) journals the user id, and the vector store deletes the user's
//     personal points, retried until it can.

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitcleanup"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/platformops"
)

type memoryCall struct {
	kind      string // "toolkit" or "user"
	project   int64
	toolkitOr int64 // the toolkit id, or the user id
}

type fakeMemory struct {
	calls []memoryCall
	err   error
}

func (f *fakeMemory) DeleteMemoryToolkit(_ context.Context, projectID, toolkitID int64) error {
	f.calls = append(f.calls, memoryCall{"toolkit", projectID, toolkitID})
	return f.err
}

func (f *fakeMemory) DeleteMemoryUser(_ context.Context, projectID, userID int64) error {
	f.calls = append(f.calls, memoryCall{"user", projectID, userID})
	return f.err
}

func enqueueMemory(ctx context.Context, t *testing.T, pool *pgxpool.Pool, journal *toolkitcleanup.Journal) string {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	app, err := journal.EnqueueMemory(ctx, tx, 7, 70)
	if err != nil {
		t.Fatalf("enqueue memory: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return app
}

func enqueueUsers(ctx context.Context, t *testing.T, pool *pgxpool.Pool, journal *toolkitcleanup.Journal, projectID int64, userIDs ...int64) string {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	app, err := journal.EnqueueMemoryUsers(ctx, tx, projectID, userIDs...)
	if err != nil {
		t.Fatalf("enqueue memory users: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return app
}

func countRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM centry.toolkit_deletions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestAMemoryToolkitDeleteJournalsItsIdAndTheReconcilerDeletesEverythingOfIt(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	memory := &fakeMemory{}
	journal := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithMemory(memory, true))

	if app := enqueueMemory(ctx, t, pool, journal); app != toolkitcleanup.AppMemory {
		t.Fatalf("enqueued app = %q", app)
	}
	if completed, err := claimAndResume(ctx, t, journal); err != nil || !completed {
		t.Fatalf("resume: completed=%v err=%v", completed, err)
	}
	if want := []memoryCall{{"toolkit", 7, 70}}; !slices.Equal(memory.calls, want) {
		t.Fatalf("the vector store was asked %v, want %v", memory.calls, want)
	}
	row := readRow(ctx, t, pool, 7, 70)
	if deref(row.outcome) != toolkitcleanup.OutcomeDeleted || row.completed == nil {
		t.Fatalf("closed row = %+v outcome=%q", row, deref(row.outcome))
	}
}

func TestUsersMemoryCleanupIsOneStatementForManyUsersAndDrainsToEveryProject(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	memory := &fakeMemory{}
	journal := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithMemory(memory, true))

	// Deleted users: project 0 (every project). Non-positive ids are ignored.
	if app := enqueueUsers(ctx, t, pool, journal, 0, 10, 11, 12, 0, -3); app != toolkitcleanup.AppMemoryUser {
		t.Fatalf("enqueued app = %q", app)
	}
	if rows := countRows(ctx, t, pool); rows != 3 {
		t.Fatalf("%d journal rows, want 3", rows)
	}
	// A user removed from one project: that project only.
	enqueueUsers(ctx, t, pool, journal, 7, 10)
	if rows := countRows(ctx, t, pool); rows != 4 {
		t.Fatalf("%d journal rows, want 4", rows)
	}
	for range 4 {
		if completed, err := claimAndResume(ctx, t, journal); err != nil || !completed {
			t.Fatalf("resume: completed=%v err=%v", completed, err)
		}
	}
	var got []memoryCall
	got = append(got, memory.calls...)
	slices.SortFunc(got, func(a, b memoryCall) int {
		if a.project != b.project {
			return int(a.project - b.project)
		}
		return int(a.toolkitOr - b.toolkitOr)
	})
	want := []memoryCall{{"user", 0, 10}, {"user", 0, 11}, {"user", 0, 12}, {"user", 7, 10}}
	if !slices.Equal(got, want) {
		t.Fatalf("the vector store was asked %v, want %v", got, want)
	}
	if row := readRow(ctx, t, pool, 0, 11); deref(row.outcome) != toolkitcleanup.OutcomeDeleted || row.completed == nil {
		t.Fatalf("closed row = %+v", row)
	}
	// The same user again reopens the row (generation bumped), it does not add one.
	enqueueUsers(ctx, t, pool, journal, 0, 11)
	if rows := countRows(ctx, t, pool); rows != 4 {
		t.Fatalf("%d journal rows after a repeat, want 4", rows)
	}
	if row := readRow(ctx, t, pool, 0, 11); row.generation != 2 || row.completed != nil {
		t.Fatalf("reopened row = %+v", row)
	}
	// Nobody to clean: no row, no app.
	if app := enqueueUsers(ctx, t, pool, journal, 0); app != "" {
		t.Fatalf("an empty list enqueued %q", app)
	}
	if app := enqueueUsers(ctx, t, pool, journal, 0, 0, -1); app != "" {
		t.Fatalf("non-positive ids enqueued %q", app)
	}
}

func TestAUsersCleanupSurvivesTheVectorServiceBeingDownAndRunsLater(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// The vector service is unavailable.
	memory := &fakeMemory{err: errors.Join(platformops.ErrRetryLater)}
	journal := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithMemory(memory, true))
	enqueueUsers(ctx, t, pool, journal, 0, 10)
	if completed, err := claimAndResume(ctx, t, journal); completed || !errors.Is(err, platformops.ErrRetryLater) {
		t.Fatalf("down: completed=%v err=%v", completed, err)
	}
	row := readRow(ctx, t, pool, 0, 10)
	if row.completed != nil || row.attempts != 1 || row.lastError == nil {
		t.Fatalf("the row must stay open and back off: %+v", row)
	}
	// Still down on the next pass.
	reopenForRetry(ctx, t, pool)
	if completed, err := claimAndResume(ctx, t, journal); completed || err == nil {
		t.Fatalf("still down: completed=%v err=%v", completed, err)
	}
	// Back up: the retry closes the row, and the call was made each time.
	reopenForRetry(ctx, t, pool)
	memory.err = nil
	if completed, err := claimAndResume(ctx, t, journal); err != nil || !completed {
		t.Fatalf("recovered: completed=%v err=%v", completed, err)
	}
	if row := readRow(ctx, t, pool, 0, 10); deref(row.outcome) != toolkitcleanup.OutcomeDeleted || row.completed == nil {
		t.Fatalf("closed row = %+v", row)
	}
	if len(memory.calls) != 3 {
		t.Fatalf("calls = %v", memory.calls)
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
	if _, err := journal.EnqueueMemory(ctx, tx, 7, 70); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.EnqueueMemoryUsers(ctx, tx, 0, 10); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if rows := countRows(ctx, t, pool); rows != 0 {
		t.Fatalf("%d journal rows after a rollback", rows)
	}
	// A deployment that keeps memories in the Python worker's database has
	// nothing in the vector store to delete.
	python := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithMemory(&fakeMemory{}, false))
	if app := enqueueMemory(ctx, t, pool, python); app != "" {
		t.Fatalf("a non-recording replica enqueued %q", app)
	}
	if app := enqueueUsers(ctx, t, pool, python, 0, 10); app != "" {
		t.Fatalf("a non-recording replica enqueued %q", app)
	}
	if rows := countRows(ctx, t, pool); rows != 0 {
		t.Fatalf("%d journal rows", rows)
	}
	// A project id that is not valid for the row is refused, not journaled.
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := journal.EnqueueMemory(ctx, tx, 0, 70); err == nil {
		t.Fatal("a memory toolkit row for project 0 was accepted")
	}
}

func TestAMemoryRowWaitsForAReplicaWithTheClientAndRetriesATransientFailure(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// The deciding replica records it without a client.
	unwired := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithMemory(nil, true))
	enqueueMemory(ctx, t, pool, unwired)
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
	enqueueMemory(ctx, t, pool, wired)
	memory.err = platformops.ErrNotAllowed
	if completed, err := claimAndResume(ctx, t, wired); err != nil || !completed {
		t.Fatalf("refused: completed=%v err=%v", completed, err)
	}
	if row := readRow(ctx, t, pool, 7, 70); deref(row.outcome) != toolkitcleanup.OutcomeFailedPermanent {
		t.Fatalf("outcome = %q", deref(row.outcome))
	}
}

// A user removed from a project and re-added before the drain keeps their
// personal memories: the drain re-checks the membership right before the
// delete, and a member again closes the row as superseded without calling the
// vector store. A user still gone is deleted; a deleted user (project 0) whose
// id exists again is not.
func TestAUserReAddedBeforeTheDrainKeepsTheirPersonalMemories(t *testing.T) {
	pool := newJournalPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// The migrated database has the auth tables (with their foreign keys);
	// they are made here only if it does not. User 20 is the member, user 22
	// a deleted one; role 1 exists in projects 7 and 8.
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS public.auth_core__user (id serial PRIMARY KEY, email text UNIQUE)`,
		`CREATE TABLE IF NOT EXISTS public.auth_core__project_role (id serial PRIMARY KEY, project_id integer NOT NULL, name text NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS public.auth_core__project_user_role (
		    id serial PRIMARY KEY, project_id integer NOT NULL,
		    user_id integer NOT NULL REFERENCES public.auth_core__user (id) ON DELETE CASCADE,
		    role_id integer NOT NULL REFERENCES public.auth_core__project_role (id) ON DELETE CASCADE)`,
		`INSERT INTO public.auth_core__user (id, email) VALUES (20, 'member@example.com') ON CONFLICT DO NOTHING`,
		`INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES (9001, 7, 'm9-test') ON CONFLICT DO NOTHING`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	memory := &fakeMemory{}
	journal := toolkitcleanup.New(pool, quiet(), toolkitcleanup.WithMemory(memory, true))

	// User 20 is removed from project 7 (the membership row is gone when the
	// removal commits), then re-added before the reconciler runs.
	enqueueUsers(ctx, t, pool, journal, 7, 20)
	exec(`INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES (7, 20, 9001)`)
	if completed, err := claimAndResume(ctx, t, journal); err != nil || !completed {
		t.Fatalf("re-added: completed=%v err=%v", completed, err)
	}
	if len(memory.calls) != 0 {
		t.Fatalf("a re-added user's memories were deleted: %v", memory.calls)
	}
	if row := readRow(ctx, t, pool, 7, 20); deref(row.outcome) != toolkitcleanup.OutcomeSuperseded || row.completed == nil {
		t.Fatalf("the cancelled row = %+v outcome=%q", row, deref(row.outcome))
	}

	// Removed again, and still gone at the drain: deleted. A membership in
	// ANOTHER project does not keep this project's memories.
	exec(`DELETE FROM public.auth_core__project_user_role WHERE project_id = 7 AND user_id = 20`)
	exec(`INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES (8, 20, 9001)`)
	enqueueUsers(ctx, t, pool, journal, 7, 20)
	if completed, err := claimAndResume(ctx, t, journal); err != nil || !completed {
		t.Fatalf("still gone: completed=%v err=%v", completed, err)
	}
	if want := []memoryCall{{"user", 7, 20}}; !slices.Equal(memory.calls, want) {
		t.Fatalf("the vector store was asked %v, want %v", memory.calls, want)
	}
	if row := readRow(ctx, t, pool, 7, 20); deref(row.outcome) != toolkitcleanup.OutcomeDeleted {
		t.Fatalf("outcome = %q", deref(row.outcome))
	}

	// A deleted user (project 0): a user row with the id existing again
	// cancels it; with none, the delete runs.
	exec(`INSERT INTO public.auth_core__user (id, email) VALUES (21, 'back@example.com')`)
	enqueueUsers(ctx, t, pool, journal, 0, 21, 22)
	for range 2 {
		if completed, err := claimAndResume(ctx, t, journal); err != nil || !completed {
			t.Fatalf("project 0: completed=%v err=%v", completed, err)
		}
	}
	if want := []memoryCall{{"user", 7, 20}, {"user", 0, 22}}; !slices.Equal(memory.calls, want) {
		t.Fatalf("the vector store was asked %v, want %v", memory.calls, want)
	}
	if row := readRow(ctx, t, pool, 0, 21); deref(row.outcome) != toolkitcleanup.OutcomeSuperseded {
		t.Fatalf("a user that exists again: outcome = %q", deref(row.outcome))
	}
}
