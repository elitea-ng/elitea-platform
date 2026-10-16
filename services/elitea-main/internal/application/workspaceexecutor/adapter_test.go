package workspaceexecutor

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
)

const (
	conversation = "0b9c6f4e-9a7f-4d4e-8f0e-1d2c3b4a5f60"
	response     = "4d5a8c0e-3f4b-4a2b-9c1d-0e2f3a4b5c6d"
	cloudSHA     = "1111111111111111111111111111111111111111"
	parentSHA    = "2222222222222222222222222222222222222222"
)

type fakeHandoffs struct {
	running   map[string]workspacehandoff.Handoff
	startErr  error
	starts    []string
	rewindTo  *string
	finishes  []string
	finishErr error
	env       map[string]string
}

func (f *fakeHandoffs) ResolveRunningTurn(_ context.Context, executionID string) (workspacehandoff.Handoff, bool, error) {
	handoff, ok := f.running[executionID]
	return handoff, ok, nil
}

func (f *fakeHandoffs) StartCloudTurn(_ context.Context, thread workspacehandoff.Thread, epoch int64, executionID, responseMessageID string) (workspacehandoff.Handoff, workspacehandoff.Policy, error) {
	f.starts = append(f.starts, executionID+"|"+responseMessageID)
	if f.startErr != nil {
		return workspacehandoff.Handoff{}, workspacehandoff.Policy{}, f.startErr
	}
	cloud := cloudSHA
	return workspacehandoff.Handoff{
		ProjectID: thread.ProjectID, ConversationUUID: thread.ConversationUUID, State: workspacestore.StateCloudRunning,
		Epoch: epoch, CloudSHA: &cloud, HasUpstream: true, Stats: []byte(`{"unpacked_bytes": 77}`),
		EnvSecretName: "workspace_env_x", RewindTo: f.rewindTo, RunningExecutionID: executionID,
	}, workspacehandoff.Policy{Enabled: true}, nil
}

func (f *fakeHandoffs) FinishCloudTurn(_ context.Context, _ workspacehandoff.Thread, _ int64, executionID string) (workspacehandoff.Handoff, error) {
	f.finishes = append(f.finishes, executionID)
	if f.finishErr != nil {
		return workspacehandoff.Handoff{}, f.finishErr
	}
	delete(f.running, executionID)
	return workspacehandoff.Handoff{State: workspacestore.StateCloudIdle, Epoch: 2}, nil
}

func (f *fakeHandoffs) CloudTurnEnv(context.Context, workspacehandoff.Thread, int64, string) (map[string]string, error) {
	return f.env, nil
}

func (f *fakeHandoffs) EffectiveCloudPolicy(context.Context, int64) (workspacehandoff.Policy, error) {
	return workspacehandoff.Policy{Enabled: true, Network: true, Limits: workspacestore.Limits{MaxSegmentBytes: 10, MaxFiles: 3}}, nil
}

type fakeObjects struct{ publishErr error }

func (fakeObjects) Plan(context.Context, workspacestore.Thread, workspacestore.Caller, int64) (workspacestore.FetchPlan, error) {
	return workspacestore.FetchPlan{HeadSeq: 2, Refs: map[string]string{"refs/elitea/handoff/1": parentSHA, "refs/elitea/base": cloudSHA}}, nil
}

func (fakeObjects) OpenObject(context.Context, workspacestore.Thread, workspacestore.Caller, string) (io.ReadCloser, int64, error) {
	return nil, 0, workspacestore.ErrNotFound
}

func (fakeObjects) UploadSegment(context.Context, workspacestore.Thread, workspacestore.Caller, int64, string, int64, io.Reader) (bool, error) {
	return false, nil
}

func (f fakeObjects) Publish(context.Context, workspacestore.PublishRequest) (workspacestore.PublishResult, error) {
	return workspacestore.PublishResult{}, f.publishErr
}

type fakeThreads struct {
	row       workspacehandoff.Handoff
	present   bool
	execution Execution
	found     bool
	finished  []workspacehandoff.Handoff
	// pendingFor is the execution whose thread is being taken back.
	pendingFor string
}

func (f *fakeThreads) Read(context.Context, workspacehandoff.Thread) (workspacehandoff.Handoff, bool, error) {
	return f.row, f.present, nil
}

func (f *fakeThreads) Execution(context.Context, string, uint64) (Execution, bool, error) {
	return f.execution, f.found, nil
}

func (f *fakeThreads) FinishedCloudTurns(context.Context, int) ([]workspacehandoff.Handoff, error) {
	return f.finished, nil
}

func (f *fakeThreads) ReclaimPending(_ context.Context, executionID string) (bool, error) {
	return executionID == f.pendingFor, nil
}

func newAdapter(t *testing.T, handoffs *fakeHandoffs, objects Objects, threads *fakeThreads) *Adapter {
	t.Helper()
	adapter, err := New(handoffs, objects, threads, Resources{TurnWallSeconds: 60}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func idleThread() *fakeThreads {
	admitted := time.Now()
	return &fakeThreads{
		row: workspacehandoff.Handoff{ProjectID: 7, ConversationUUID: conversation, State: workspacestore.StateCloudIdle,
			Epoch: 2, StateChangedAt: admitted.Add(-time.Minute)},
		present:   true,
		execution: Execution{ResponseMessageID: response, ConversationUUID: conversation, AdmittedAt: admitted},
		found:     true,
	}
}

func claim() workspaceruntime.TurnClaim {
	return workspaceruntime.TurnClaim{ExecutionID: "exec", Generation: 1, ProjectID: 7, ConversationUUID: conversation}
}

func TestResolveCloudTurnStartsTheTurnAtClaimWithItsAnswer(t *testing.T) {
	handoffs := &fakeHandoffs{}
	adapter := newAdapter(t, handoffs, fakeObjects{}, idleThread())
	turn, ok, err := adapter.ResolveCloudTurn(context.Background(), claim())
	if err != nil || !ok {
		t.Fatalf("resolve = %v %v", ok, err)
	}
	if len(handoffs.starts) != 1 || handoffs.starts[0] != "exec|"+response {
		t.Fatalf("starts = %v", handoffs.starts)
	}
	if turn.Epoch != 2 || *turn.CloudSHA != cloudSHA || !turn.EnvSecret || !turn.HasUpstream || turn.UnpackedBytes != 77 || turn.Rewind {
		t.Fatalf("turn = %+v", turn)
	}
	// A regenerate of the newest cloud turn: StartCloudTurn answers the target.
	parent := parentSHA
	handoffs.rewindTo = &parent
	turn, _, _ = adapter.ResolveCloudTurn(context.Background(), claim())
	if !turn.Rewind || *turn.RewindTo != parentSHA {
		t.Fatalf("rewind turn = %+v", turn)
	}
}

func TestResolveCloudTurnRefusesATurnTheThreadIsNotTheCloudsFor(t *testing.T) {
	for name, mutate := range map[string]func(*fakeThreads, *fakeHandoffs){
		"another turn runs": func(th *fakeThreads, _ *fakeHandoffs) {
			th.row.State, th.row.RunningExecutionID = workspacestore.StateCloudRunning, "other"
		},
		"reclaiming": func(th *fakeThreads, _ *fakeHandoffs) { th.row.State = workspacestore.StateReclaiming },
		"released":   func(th *fakeThreads, _ *fakeHandoffs) { th.row.State = workspacestore.StateLocal },
		"changed hands": func(th *fakeThreads, _ *fakeHandoffs) {
			th.row.StateChangedAt = th.execution.AdmittedAt.Add(time.Second)
		},
		"no execution":   func(th *fakeThreads, _ *fakeHandoffs) { th.found = false },
		"other thread":   func(th *fakeThreads, _ *fakeHandoffs) { th.execution.ConversationUUID = response },
		"start is stale": func(_ *fakeThreads, h *fakeHandoffs) { h.startErr = workspacehandoff.ErrStaleEpoch },
	} {
		threads, handoffs := idleThread(), &fakeHandoffs{}
		mutate(threads, handoffs)
		_, _, err := newAdapter(t, handoffs, fakeObjects{}, threads).ResolveCloudTurn(context.Background(), claim())
		if !errors.Is(err, workspaceruntime.ErrNotCloudTurn) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	threads, handoffs := idleThread(), &fakeHandoffs{startErr: workspacehandoff.ErrDisabled}
	if _, _, err := newAdapter(t, handoffs, fakeObjects{}, threads).ResolveCloudTurn(context.Background(), claim()); !errors.Is(err, workspaceruntime.ErrCloudExecutorDisabled) {
		t.Fatalf("disabled: %v", err)
	}
	// An ordinary conversation is not a cloud turn and starts nothing.
	threads, handoffs = idleThread(), &fakeHandoffs{}
	threads.present = false
	if _, ok, err := newAdapter(t, handoffs, fakeObjects{}, threads).ResolveCloudTurn(context.Background(), claim()); ok || err != nil || len(handoffs.starts) != 0 {
		t.Fatalf("ordinary: %v %v", ok, err)
	}
}

func TestStoreMapsConflictAndBusyAndChecksTheClaimsBelief(t *testing.T) {
	handoffs := &fakeHandoffs{running: map[string]workspacehandoff.Handoff{
		"exec": {ProjectID: 7, ConversationUUID: conversation, State: workspacestore.StateCloudRunning, Epoch: 2, RunningExecutionID: "exec"},
	}}
	current := cloudSHA
	request := workspaceruntime.PublishRequest{Kind: "COMPACT", SegmentSHA256: strings.Repeat("a", 64), Bytes: 3, ObjectFormat: "sha1",
		Refs: []workspaceruntime.RefUpdate{{Name: workspaceruntime.RefCloud, New: parentSHA}}}
	ref := workspaceruntime.ClaimRef{ExecutionID: "exec", ThreadUUID: conversation, Epoch: 2}

	conflict := newAdapter(t, handoffs, fakeObjects{publishErr: &workspacestore.RefConflictError{Current: map[string]*string{workspaceruntime.RefCloud: &current}}}, idleThread())
	_, err := conflict.Publish(context.Background(), ref, request)
	refusal, ok := workspaceruntime.AsRefusal(err)
	if !ok || refusal.Code != workspaceruntime.RefusalRefConflict || len(refusal.Current) != 1 || refusal.Current[0].SHA != cloudSHA {
		t.Fatalf("conflict = %v", err)
	}
	busy := newAdapter(t, handoffs, fakeObjects{publishErr: workspacestore.ErrThreadBusy}, idleThread())
	if _, err := busy.Publish(context.Background(), ref, request); !errors.Is(err, workspaceruntime.ErrThreadBusy) {
		t.Fatalf("busy = %v", err)
	}
	if _, ok := workspaceruntime.AsRefusal(func() error { _, err := busy.Publish(context.Background(), ref, request); return err }()); ok {
		t.Fatal("thread_busy must not be a refusal")
	}
	stale := ref
	stale.Epoch = 1
	if _, err := busy.FetchPlan(context.Background(), stale, 0); !isRefusal(err, workspaceruntime.RefusalStaleEpoch) {
		t.Fatalf("stale = %v", err)
	}
	other := ref
	other.ExecutionID = "nobody"
	if _, err := busy.FetchPlan(context.Background(), other, 0); !isRefusal(err, workspaceruntime.RefusalNotWorkspaceTurn) {
		t.Fatalf("no turn = %v", err)
	}
	plan, err := busy.FetchPlan(context.Background(), ref, 0)
	if err != nil || plan.HeadSeq != 2 || len(plan.Refs) != 2 || plan.Refs[0].Name != "refs/elitea/base" {
		t.Fatalf("plan = %+v %v", plan, err)
	}
	if _, err := busy.OpenSegment(context.Background(), ref, strings.Repeat("a", 64)); !isRefusal(err, workspaceruntime.RefusalSegmentMissing) {
		t.Fatalf("missing = %v", err)
	}
	if env, err := busy.EnvSecret(context.Background(), ref); err != nil || env == nil || len(env) != 0 {
		t.Fatalf("env = %v %v", env, err)
	}
}

func TestFinishIsIdempotentAndReconcileEndsTerminalTurns(t *testing.T) {
	running := workspacehandoff.Handoff{ProjectID: 7, ConversationUUID: conversation, State: workspacestore.StateCloudRunning, Epoch: 2, RunningExecutionID: "exec"}
	handoffs := &fakeHandoffs{running: map[string]workspacehandoff.Handoff{"exec": running}}
	threads := idleThread()
	adapter := newAdapter(t, handoffs, fakeObjects{}, threads)
	for range 2 {
		if err := adapter.FinishExecution(context.Background(), "exec"); err != nil {
			t.Fatal(err)
		}
	}
	if len(handoffs.finishes) != 1 {
		t.Fatalf("finishes = %v", handoffs.finishes)
	}
	handoffs.finishErr = workspacehandoff.ErrStaleEpoch
	threads.finished = []workspacehandoff.Handoff{running}
	if n, err := adapter.ReconcileFinished(context.Background()); n != 1 || err != nil {
		t.Fatalf("reconcile = %d %v", n, err)
	}
}

func TestPolicyCarriesTheChartResources(t *testing.T) {
	adapter := newAdapter(t, &fakeHandoffs{}, fakeObjects{}, idleThread())
	policy, err := adapter.EffectiveCloudPolicy(context.Background(), 7, conversation)
	if err != nil || !policy.Enabled || policy.MaxSegmentBytes != 10 || policy.MaxFiles != 3 || policy.TurnWallSeconds != 60 {
		t.Fatalf("policy = %+v %v", policy, err)
	}
}

func isRefusal(err error, code workspaceruntime.RefusalCode) bool {
	refusal, ok := workspaceruntime.AsRefusal(err)
	return ok && refusal.Code == code
}
