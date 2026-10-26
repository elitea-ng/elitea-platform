package repos

// The Files changed records (shared/0177) against a REAL database: a record
// is saved under the handoff it names and read back, a replay replaces it,
// only the newest commit keeps cumulative diffs, a record for a handoff that
// is no longer the thread's is refused, and the erasure removes them all.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacechanges"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
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

type allowAllPolicy struct{}

func (allowAllPolicy) EffectivePolicy(context.Context, int64) (workspacehandoff.Policy, error) {
	return workspacehandoff.Policy{}, nil
}

// TestPostgresWorkspaceChangesViewSurvivesAMissingTipRecord: five cloud
// turns, the fifth (the tip) without a record: turns 1 to 4 still show, by
// their numbers, with the newest record's cumulative set, and the view is
// not complete. A missing record in the middle (turn 3) still lets the walk
// reach turns 1 and 2.
func TestPostgresWorkspaceChangesViewSurvivesAMissingTipRecord(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner int64 = 5502
	thread := seedLocalWorkThread(t, pool, owner, "local_work").thread
	seedHandoffRow(t, pool, thread, owner, workspacestore.StateCloudIdle, 2, false, "")
	base := sha1Of("base")
	commits := []string{sha1Of("t1"), sha1Of("t2"), sha1Of("t3"), sha1Of("t4"), sha1Of("t5")}
	var handoffID string
	if err := pool.QueryRow(ctx, `UPDATE elitea_runtime.workspace_handoffs SET handoff_sha = $3, cloud_sha = $4
WHERE project_id = $1 AND conversation_uuid = $2::uuid RETURNING handoff_id::text`,
		thread.ProjectID, thread.ConversationUUID, base, commits[4]).Scan(&handoffID); err != nil {
		t.Fatal(err)
	}
	repo, err := NewWorkspaceChangesRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	handoffs, err := NewWorkspaceHandoffsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, err := workspacechanges.NewService(repo, handoffs, allowAllPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	save := func(index int) {
		t.Helper()
		parent := base
		if index > 0 {
			parent = commits[index-1]
		}
		path := fmt.Sprintf("turn%d.go", index+1)
		if err := repo.Save(ctx, workspacechanges.Record{
			Thread: thread, HandoffID: handoffID, Commit: commits[index], Parent: parent, Base: base,
			Seq: int64(index + 1), Depth: int64(index + 1), ExecutionID: "exec",
			Turn:       workspacechanges.Manifest{Files: []workspacechanges.File{{Path: path, Status: "added"}}},
			Cumulative: workspacechanges.Manifest{Files: []workspacechanges.File{{Path: path, Status: "added"}}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	for index := range 4 {
		save(index)
	}
	view, err := service.View(ctx, thread)
	if err != nil {
		t.Fatal(err)
	}
	if view.Complete || len(view.Turns) != 4 || *view.HeadSHA != commits[4] || *view.CumulativeCommit != commits[3] {
		t.Fatalf("view = %+v", view)
	}
	for index, turn := range view.Turns {
		if turn.Commit != commits[index] || turn.Number == nil || *turn.Number != int64(index+1) {
			t.Fatalf("turn %d = %+v", index, turn)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM elitea_runtime.workspace_turn_changes WHERE commit_sha = $1`, commits[2]); err != nil {
		t.Fatal(err)
	}
	view, err = service.View(ctx, thread)
	if err != nil || view.Complete || len(view.Turns) != 3 || view.Turns[0].Commit != commits[0] ||
		view.Turns[1].Commit != commits[1] || view.Turns[2].Commit != commits[3] {
		t.Fatalf("with a gap = %+v %v", view, err)
	}
}
