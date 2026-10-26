package workspaceexecutor

import (
	"context"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacechanges"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
)

type recordedChanges struct {
	err   error
	calls []int64
}

func (r *recordedChanges) Record(_ context.Context, _ workspacehandoff.Handoff, executionID string, seq int64, _ workspaceruntime.TurnChanges) error {
	if executionID != "exec" {
		return errors.New("wrong execution")
	}
	r.calls = append(r.calls, seq)
	return r.err
}

// TestRecordChangesOnlyForThePublishedTip: the record is kept only for the
// thread's cloud tip measured from its snapshot (a ref conflict otherwise),
// with the publish's seq; without a recorder it is unavailable, and the
// recorder's not-found (the handoff is gone) is a stale epoch.
func TestRecordChangesOnlyForThePublishedTip(t *testing.T) {
	snapshot := "5555555555555555555555555555555555555555"
	tip := cloudSHA
	handoffs := &fakeHandoffs{running: map[string]workspacehandoff.Handoff{
		"exec": {ProjectID: 7, ConversationUUID: conversation, State: workspacestore.StateCloudRunning, Epoch: 2,
			RunningExecutionID: "exec", CloudSHA: &tip, HandoffSHA: &snapshot, LastSeq: 9},
	}}
	ref := workspaceruntime.ClaimRef{ExecutionID: "exec", ThreadUUID: conversation, Epoch: 2}
	changes := workspaceruntime.TurnChanges{Commit: cloudSHA, Parent: parentSHA, Base: snapshot}

	adapter := newAdapter(t, handoffs, fakeObjects{}, idleThread())
	if err := adapter.RecordChanges(context.Background(), ref, changes); !errors.Is(err, workspaceruntime.ErrUnavailable) {
		t.Fatalf("no recorder = %v", err)
	}
	recorder := &recordedChanges{}
	adapter.WithChanges(recorder)
	if err := adapter.RecordChanges(context.Background(), ref, changes); err != nil || len(recorder.calls) != 1 || recorder.calls[0] != 9 {
		t.Fatalf("record = %v %v", err, recorder.calls)
	}
	notTip := changes
	notTip.Commit = parentSHA
	if err := adapter.RecordChanges(context.Background(), ref, notTip); !isRefusal(err, workspaceruntime.RefusalRefConflict) {
		t.Fatalf("not the tip = %v", err)
	}
	otherBase := changes
	otherBase.Base = parentSHA
	if err := adapter.RecordChanges(context.Background(), ref, otherBase); !isRefusal(err, workspaceruntime.RefusalRefConflict) {
		t.Fatalf("not the snapshot = %v", err)
	}
	stale := ref
	stale.Epoch = 1
	if err := adapter.RecordChanges(context.Background(), stale, changes); !isRefusal(err, workspaceruntime.RefusalStaleEpoch) {
		t.Fatalf("stale = %v", err)
	}
	recorder.err = workspacechanges.ErrNotFound
	if err := adapter.RecordChanges(context.Background(), ref, changes); !isRefusal(err, workspaceruntime.RefusalStaleEpoch) {
		t.Fatalf("gone = %v", err)
	}
	if len(recorder.calls) != 2 {
		t.Fatalf("calls = %v", recorder.calls)
	}
}
