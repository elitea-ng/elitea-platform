package workspaceexecutor

import (
	"context"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
)

// forgeHandoffs adds the credential call to the fake.
type forgeHandoffs struct {
	*fakeHandoffs
	err  error
	seen []workspacehandoff.Handoff
}

func (f *forgeHandoffs) ForgeCredentialForTurn(_ context.Context, handoff workspacehandoff.Handoff) (workspacehandoff.RedeemedForgeCredential, error) {
	f.seen = append(f.seen, handoff)
	if f.err != nil {
		return workspacehandoff.RedeemedForgeCredential{}, f.err
	}
	return workspacehandoff.RedeemedForgeCredential{Type: "gitlab", Configuration: map[string]any{"access_token": "ghp_x"}}, nil
}

func TestForgeCredentialIsBoundToTheRunningTurnAndMapsItsRefusals(t *testing.T) {
	row := workspacehandoff.Handoff{ProjectID: 1, ConversationUUID: conversation, Epoch: 2, State: workspacestore.StateCloudRunning}
	inner := &fakeHandoffs{running: map[string]workspacehandoff.Handoff{"exec": row}}
	handoffs := &forgeHandoffs{fakeHandoffs: inner}
	adapter, err := New(handoffs, fakeObjects{}, &fakeThreads{}, Resources{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref := workspaceruntime.ClaimRef{ExecutionID: "exec", ThreadUUID: conversation, Epoch: 2}
	if credential, err := adapter.ForgeCredential(context.Background(), ref); err != nil || credential.Configuration["access_token"] != "ghp_x" || credential.Forge != "gitlab" ||
		len(handoffs.seen) != 1 || handoffs.seen[0].ConversationUUID != conversation {
		t.Fatalf("credential = %v %v", credential, err)
	}
	stale := ref
	stale.Epoch = 1
	if _, err := adapter.ForgeCredential(context.Background(), stale); !isRefusal(err, workspaceruntime.RefusalStaleEpoch) || len(handoffs.seen) != 1 {
		t.Fatalf("stale = %v", err)
	}
	for want, cause := range map[workspaceruntime.RefusalCode]error{
		workspaceruntime.RefusalNoForgeCredential:    workspacehandoff.ErrNoForgeCredential,
		workspaceruntime.RefusalPullRequestsDisabled: workspacehandoff.ErrPullRequestsDisabled,
	} {
		handoffs.err = cause
		if _, err := adapter.ForgeCredential(context.Background(), ref); !isRefusal(err, want) {
			t.Fatalf("%s = %v", want, err)
		}
	}
	handoffs.err = workspacehandoff.ErrForgeCredentialInvalid
	if _, err := adapter.ForgeCredential(context.Background(), ref); !isRefusal(err, workspaceruntime.RefusalNoForgeCredential) {
		t.Fatalf("invalid = %v", err)
	}
	// Handoffs without the call: unavailable.
	plain, err := New(inner, fakeObjects{}, &fakeThreads{}, Resources{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.ForgeCredential(context.Background(), ref); !errors.Is(err, workspaceruntime.ErrUnavailable) {
		t.Fatalf("plain = %v", err)
	}
}

// Every executor event the worker sends reaches RecordExecutorEvent through
// the adapter: branch.pushed as well as scan_blocked and pull_request.opened
// (an event the allowlist dropped never wrote its audit row). Anything else
// is refused before the thread is read.
func TestRecordEventPassesEveryExecutorEventKind(t *testing.T) {
	row := workspacehandoff.Handoff{ProjectID: 1, ConversationUUID: conversation, Epoch: 2, State: workspacestore.StateCloudRunning}
	handoffs := &fakeHandoffs{running: map[string]workspacehandoff.Handoff{"exec": row}}
	adapter, err := New(handoffs, fakeObjects{}, &fakeThreads{}, Resources{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref := workspaceruntime.ClaimRef{ExecutionID: "exec", ThreadUUID: conversation, Epoch: 2}
	for _, kind := range []string{workspaceruntime.EventScanBlocked, workspaceruntime.EventPullRequestOpened, workspaceruntime.EventBranchPushed} {
		if err := adapter.RecordEvent(context.Background(), ref, workspaceruntime.ExecutorEvent{Kind: kind, Attrs: map[string]string{"commits": "2"}}); err != nil {
			t.Fatalf("%s = %v", kind, err)
		}
	}
	if err := adapter.RecordEvent(context.Background(), ref, workspaceruntime.ExecutorEvent{Kind: "store.publish"}); !errors.Is(err, workspaceruntime.ErrInvalidRequest) {
		t.Fatalf("unknown kind = %v", err)
	}
	want := []string{"exec|handoff.scan_blocked", "exec|pull_request.opened", "exec|branch.pushed"}
	if len(handoffs.events) != len(want) {
		t.Fatalf("events = %v", handoffs.events)
	}
	for i := range want {
		if handoffs.events[i] != want[i] {
			t.Fatalf("events = %v", handoffs.events)
		}
	}
}
