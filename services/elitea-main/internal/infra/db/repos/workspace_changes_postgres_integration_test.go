package repos

// The Files changed records (shared/0177) against a REAL database: a record
// is saved under the handoff it names and read back, a replay replaces it,
// only the newest commit keeps cumulative diffs, a record for a handoff that
// is no longer the thread's is refused, and the erasure removes them all.

import (
	"context"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacechanges"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
)

func TestPostgresWorkspaceChangesRecordsReadsAndErasure(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner int64 = 5501
	thread := seedLocalWorkThread(t, pool, owner, "local_work").thread
	seedHandoffRow(t, pool, thread, owner, workspacestore.StateCloudIdle, 2, false, "")
	var handoffID string
	if err := pool.QueryRow(ctx, `SELECT handoff_id::text FROM elitea_runtime.workspace_handoffs
WHERE project_id = $1 AND conversation_uuid = $2::uuid`, thread.ProjectID, thread.ConversationUUID).Scan(&handoffID); err != nil {
		t.Fatal(err)
	}
	repo, err := NewWorkspaceChangesRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	base, one, two := sha1Of("base"), sha1Of("one"), sha1Of("two")
	added := int64(3)
	file := func(path string) workspacechanges.File {
		return workspacechanges.File{Path: path, Status: "added", Additions: &added, Deletions: new(int64), HasDiff: true}
	}
	save := func(commit, parent string, seq int64, handoff string) error {
		return repo.Save(ctx, workspacechanges.Record{
			Thread: thread, HandoffID: handoff, Commit: commit, Parent: parent, Base: base, Seq: seq, ExecutionID: "exec",
			ResponseMessageID: "answer", Turn: workspacechanges.Manifest{Files: []workspacechanges.File{file("a.go")}},
			Cumulative: workspacechanges.Manifest{Files: []workspacechanges.File{file("a.go")}, Hidden: 1},
			Diffs:      []workspacechanges.Diff{{Scope: "turn", Path: "a.go", Text: "+a\n"}, {Scope: "cumulative", Path: "a.go", Text: "+a\n"}},
		})
	}
	if err := save(one, base, 1, handoffID); err != nil {
		t.Fatal(err)
	}
	// A replay of the same commit replaces it.
	if err := save(one, base, 1, handoffID); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if err := save(two, one, 2, handoffID); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.Turns(ctx, thread, handoffID, 10)
	if err != nil || len(stored) != 2 || stored[0].Commit != two || stored[1].Parent != base ||
		stored[0].Cumulative.Hidden != 1 || *stored[0].Turn.Files[0].Additions != 3 || stored[0].ResponseMessageID != "answer" {
		t.Fatalf("turns = %+v, %v", stored, err)
	}
	if text, ok, err := repo.Diff(ctx, thread, one, "turn", "a.go"); err != nil || !ok || text != "+a\n" {
		t.Fatalf("turn diff of the older commit = %q %v %v", text, ok, err)
	}
	if _, ok, err := repo.Diff(ctx, thread, one, "cumulative", "a.go"); err != nil || ok {
		t.Fatalf("the older commit's cumulative diff must be dropped: %v %v", ok, err)
	}
	if _, ok, err := repo.Diff(ctx, thread, two, "cumulative", "a.go"); err != nil || !ok {
		t.Fatalf("the tip's cumulative diff = %v %v", ok, err)
	}
	if err := save(sha1Of("three"), two, 3, "6b1f3f0e-6a52-4d0b-9a55-7f3c2f2f7a10"); !errors.Is(err, workspacechanges.ErrNotFound) {
		t.Fatalf("another handoff's record = %v, want ErrNotFound", err)
	}

	// The erasure takes the records with the cloud copy.
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.workspace_handoffs SET state = 'LOCAL',
delete_after = clock_timestamp() - interval '1 second' WHERE project_id = $1 AND conversation_uuid = $2::uuid`,
		thread.ProjectID, thread.ConversationUUID); err != nil {
		t.Fatal(err)
	}
	handoffs, err := NewWorkspaceHandoffsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := handoffs.MarkErased(ctx, thread, 2); err != nil {
		t.Fatal(err)
	}
	var records, diffs int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM elitea_runtime.workspace_turn_changes),
       (SELECT count(*) FROM elitea_runtime.workspace_change_diffs)`).Scan(&records, &diffs); err != nil || records != 0 || diffs != 0 {
		t.Fatalf("after the erasure: %d records, %d diffs, %v", records, diffs, err)
	}
	if err := save(sha1Of("four"), two, 4, handoffID); !errors.Is(err, workspacechanges.ErrNotFound) {
		t.Fatalf("a record after the erasure = %v, want ErrNotFound", err)
	}
}
