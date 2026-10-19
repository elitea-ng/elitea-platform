// Package workspaceexecutor adapts the handoff service (workspacehandoff) and
// the workspace store (workspacestore) to the cloud executor's seam
// (workspaceruntime): the Store the worker's gRPC calls reach, the
// TurnResolver the runtime-context redemption asks at claim, the
// PolicyEvaluator, and the TurnLifecycle that ends a cloud turn.
//
// Every Store call resolves the claim's execution to the thread whose
// running cloud turn it is (CLOUD_RUNNING, running_execution_id) and refuses
// it otherwise; the thread is never the request's.
package workspaceexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
)

// Handoffs is the part of *workspacehandoff.Service the executor uses.
type Handoffs interface {
	ResolveRunningTurn(ctx context.Context, executionID string) (workspacehandoff.Handoff, bool, error)
	StartCloudTurn(ctx context.Context, thread workspacehandoff.Thread, epoch int64, executionID, responseMessageID string) (workspacehandoff.Handoff, workspacehandoff.Policy, error)
	FinishCloudTurn(ctx context.Context, thread workspacehandoff.Thread, epoch int64, executionID string) (workspacehandoff.Handoff, error)
	CloudTurnEnv(ctx context.Context, thread workspacehandoff.Thread, epoch int64, executionID string) (map[string]string, error)
	EffectiveCloudPolicy(ctx context.Context, projectID int64) (workspacehandoff.Policy, error)
	RecordExecutorEvent(ctx context.Context, thread workspacehandoff.Thread, epoch int64, executionID, kind string, attrs map[string]string) error
}

// Objects is the part of *workspacestore.Service the executor uses.
type Objects interface {
	Plan(ctx context.Context, thread workspacestore.Thread, caller workspacestore.Caller, sinceSeq int64) (workspacestore.FetchPlan, error)
	OpenObject(ctx context.Context, thread workspacestore.Thread, caller workspacestore.Caller, sha string) (io.ReadCloser, int64, error)
	UploadSegment(ctx context.Context, thread workspacestore.Thread, caller workspacestore.Caller, epoch int64, sha string, size int64, body io.Reader) (bool, error)
	Publish(ctx context.Context, request workspacestore.PublishRequest) (workspacestore.PublishResult, error)
}

// Execution is a claimed agent execution's chat binding.
type Execution struct {
	ResponseMessageID string
	ConversationUUID  string
	AdmittedAt        time.Time
}

// Threads reads handoff rows and executions (the handoffs repository).
type Threads interface {
	Read(ctx context.Context, thread workspacehandoff.Thread) (workspacehandoff.Handoff, bool, error)
	Execution(ctx context.Context, executionID string, generation uint64) (Execution, bool, error)
	FinishedCloudTurns(ctx context.Context, limit int) ([]workspacehandoff.Handoff, error)
	ReclaimPending(ctx context.Context, executionID string) (bool, error)
}

// Resources are the chart's sandbox limits, parsed (zero: the worker's
// deployment default).
type Resources struct {
	TurnWallSeconds uint64
	CPUMillis       uint64
	MemoryBytes     uint64
	DiskBytes       uint64
}

// Adapter implements workspaceruntime.Store, TurnResolver, PolicyEvaluator
// and TurnLifecycle.
type Adapter struct {
	handoffs  Handoffs
	objects   Objects
	threads   Threads
	resources Resources
	logger    *slog.Logger
}

var (
	_ workspaceruntime.Store           = (*Adapter)(nil)
	_ workspaceruntime.TurnResolver    = (*Adapter)(nil)
	_ workspaceruntime.PolicyEvaluator = (*Adapter)(nil)
	_ workspaceruntime.TurnLifecycle   = (*Adapter)(nil)
)

// New builds the adapter; every dependency is required.
func New(handoffs Handoffs, objects Objects, threads Threads, resources Resources, logger *slog.Logger) (*Adapter, error) {
	if handoffs == nil || objects == nil || threads == nil {
		return nil, errors.New("the workspace executor adapter needs the handoff service, the store and the thread reader")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Adapter{handoffs: handoffs, objects: objects, threads: threads, resources: resources, logger: logger}, nil
}

func threadOf(handoff workspacehandoff.Handoff) workspacehandoff.Thread {
	return workspacehandoff.Thread{ProjectID: handoff.ProjectID, ConversationUUID: handoff.ConversationUUID}
}

// ---- TurnResolver ----

// HandedOff: the conversation has a handoff row in any state but LOCAL.
func (a *Adapter) HandedOff(ctx context.Context, claim workspaceruntime.TurnClaim) (bool, error) {
	if claim.ConversationUUID == "" || !workspacestore.ValidUUID(claim.ConversationUUID) || claim.ProjectID <= 0 {
		return false, nil
	}
	handoff, ok, err := a.threads.Read(ctx, workspacehandoff.Thread{ProjectID: claim.ProjectID, ConversationUUID: claim.ConversationUUID})
	if err != nil {
		return false, err
	}
	return ok && handoff.State != workspacestore.StateLocal, nil
}

// ResolveCloudTurn starts the claimed execution's cloud turn (see
// workspaceruntime.TurnResolver).
func (a *Adapter) ResolveCloudTurn(ctx context.Context, claim workspaceruntime.TurnClaim) (workspaceruntime.Turn, bool, error) {
	if claim.ConversationUUID == "" || !workspacestore.ValidUUID(claim.ConversationUUID) || claim.ProjectID <= 0 {
		return workspaceruntime.Turn{}, false, nil
	}
	thread := workspacehandoff.Thread{ProjectID: claim.ProjectID, ConversationUUID: claim.ConversationUUID}
	handoff, ok, err := a.threads.Read(ctx, thread)
	if err != nil {
		return workspaceruntime.Turn{}, false, err
	}
	if !ok {
		return workspaceruntime.Turn{}, false, nil
	}
	var response string
	switch handoff.State {
	case workspacestore.StateCloudRunning:
		if handoff.RunningExecutionID != claim.ExecutionID {
			// Another cloud turn runs in this thread.
			return workspaceruntime.Turn{}, false, workspaceruntime.ErrNotCloudTurn
		}
		// A repeated redemption of the running turn.
		response = handoff.RunningResponseID
	case workspacestore.StateCloudIdle:
		execution, found, err := a.threads.Execution(ctx, claim.ExecutionID, claim.Generation)
		if err != nil {
			return workspaceruntime.Turn{}, false, err
		}
		if !found || execution.ConversationUUID != thread.ConversationUUID || !workspacestore.ValidUUID(execution.ResponseMessageID) {
			return workspaceruntime.Turn{}, false, workspaceruntime.ErrNotCloudTurn
		}
		// The fence admits a turn only while the thread is CLOUD_IDLE, and a
		// previous turn ends before the next is admitted. A state change after
		// this turn's admission is a take back, release or a new send in
		// between: the turn was asked of a thread that is not this one any more.
		// admitted_at is stored to the millisecond and state_changed_at to
		// the microsecond: compare both at the millisecond, so a turn admitted
		// in the same millisecond as the previous turn's end is not refused.
		if handoff.StateChangedAt.Truncate(time.Millisecond).After(execution.AdmittedAt.Truncate(time.Millisecond)) {
			return workspaceruntime.Turn{}, false, workspaceruntime.ErrNotCloudTurn
		}
		response = execution.ResponseMessageID
	case workspacestore.StateLocal:
		// Released: the thread is the desktop's again. Not a cloud turn; the
		// Local work fence refuses its web turns at admission.
		return workspaceruntime.Turn{}, false, workspaceruntime.ErrNotCloudTurn
	default:
		// HANDING_OFF, RECLAIMING.
		return workspaceruntime.Turn{}, false, workspaceruntime.ErrNotCloudTurn
	}
	started, _, err := a.handoffs.StartCloudTurn(ctx, thread, handoff.Epoch, claim.ExecutionID, response)
	switch {
	case errors.Is(err, workspacehandoff.ErrDisabled):
		return workspaceruntime.Turn{}, false, workspaceruntime.ErrCloudExecutorDisabled
	case errors.Is(err, workspacehandoff.ErrStaleEpoch), errors.Is(err, workspacehandoff.ErrInProgress),
		errors.Is(err, workspacehandoff.ErrNotFound), errors.Is(err, workspacehandoff.ErrInvalid):
		return workspaceruntime.Turn{}, false, workspaceruntime.ErrNotCloudTurn
	case err != nil:
		return workspaceruntime.Turn{}, false, err
	}
	if started.Epoch <= 0 {
		return workspaceruntime.Turn{}, false, workspaceruntime.ErrNotCloudTurn
	}
	var stats workspacehandoff.Stats
	_ = json.Unmarshal(started.Stats, &stats)
	unpacked := uint64(0)
	if stats.UnpackedBytes > 0 {
		unpacked = uint64(stats.UnpackedBytes)
	}
	return workspaceruntime.Turn{
		ThreadUUID:    started.ConversationUUID,
		Epoch:         uint64(started.Epoch),
		CloudSHA:      started.CloudSHA,
		HasUpstream:   started.HasUpstream,
		UnpackedBytes: unpacked,
		EnvSecret:     started.EnvSecret(),
		Rewind:        started.RewindTo != nil,
		RewindTo:      started.RewindTo,
	}, true, nil
}

// ---- PolicyEvaluator ----

// EffectiveCloudPolicy is the policy recomputed at this claim.
func (a *Adapter) EffectiveCloudPolicy(ctx context.Context, projectID int64, _ string) (workspaceruntime.Policy, error) {
	policy, err := a.handoffs.EffectiveCloudPolicy(ctx, projectID)
	if err != nil {
		return workspaceruntime.Policy{}, err
	}
	return workspaceruntime.Policy{
		Enabled:          policy.Enabled,
		Shell:            policy.Shell,
		Network:          policy.Network,
		NetworkAllowlist: append([]string(nil), policy.NetworkAllowlist...),
		MaxSegmentBytes:  nonNegative(policy.Limits.MaxSegmentBytes),
		MaxFileBytes:     nonNegative(policy.Limits.MaxFileBytes),
		MaxFiles:         nonNegative(policy.Limits.MaxFiles),
		MaxUnpackedBytes: nonNegative(policy.Limits.MaxUnpackedBytes),
		TurnWallSeconds:  a.resources.TurnWallSeconds,
		CPUMillis:        a.resources.CPUMillis,
		MemoryBytes:      a.resources.MemoryBytes,
		DiskBytes:        a.resources.DiskBytes,
	}, nil
}

func nonNegative(value int64) uint64 {
	if value < 0 {
		return 0
	}
	return uint64(value)
}

// ---- Store ----

// running resolves the claim to its thread and checks what the worker
// believes (thread and epoch).
func (a *Adapter) running(ctx context.Context, claim workspaceruntime.ClaimRef) (workspacehandoff.Handoff, workspacestore.Caller, error) {
	handoff, ok, err := a.handoffs.ResolveRunningTurn(ctx, claim.ExecutionID)
	if err != nil {
		return workspacehandoff.Handoff{}, workspacestore.Caller{}, storeError(err)
	}
	if !ok {
		return workspacehandoff.Handoff{}, workspacestore.Caller{}, workspaceruntime.Refuse(workspaceruntime.RefusalNotWorkspaceTurn)
	}
	if handoff.ConversationUUID != claim.ThreadUUID || handoff.Epoch <= 0 || uint64(handoff.Epoch) != claim.Epoch {
		return workspacehandoff.Handoff{}, workspacestore.Caller{}, workspaceruntime.Refuse(workspaceruntime.RefusalStaleEpoch)
	}
	return handoff, workspacestore.Caller{Writer: workspacestore.WriterCloudExecutor, ExecutionID: claim.ExecutionID}, nil
}

// FetchPlan answers the plan of the claim's thread.
func (a *Adapter) FetchPlan(ctx context.Context, claim workspaceruntime.ClaimRef, sinceSeq uint64) (workspaceruntime.FetchPlan, error) {
	if sinceSeq > 1<<62 {
		return workspaceruntime.FetchPlan{}, workspaceruntime.ErrInvalidRequest
	}
	handoff, caller, err := a.running(ctx, claim)
	if err != nil {
		return workspaceruntime.FetchPlan{}, err
	}
	plan, err := a.objects.Plan(ctx, threadOf(handoff), caller, int64(sinceSeq))
	if err != nil {
		return workspaceruntime.FetchPlan{}, storeError(err)
	}
	out := workspaceruntime.FetchPlan{HeadSeq: nonNegative(plan.HeadSeq), Refs: refList(plan.Refs)}
	for _, segment := range plan.Segments {
		out.Segments = append(out.Segments, workspaceruntime.Segment{
			Seq: nonNegative(segment.Seq), Kind: segment.Kind, SHA256: segment.SHA256, Bytes: nonNegative(segment.Bytes),
			Prerequisites: append([]string{}, segment.Prerequisites...), Refs: refList(segment.Refs),
		})
	}
	return out, nil
}

// OpenSegment opens one stored segment of the claim's thread.
func (a *Adapter) OpenSegment(ctx context.Context, claim workspaceruntime.ClaimRef, sha256 string) (io.ReadCloser, error) {
	handoff, caller, err := a.running(ctx, claim)
	if err != nil {
		return nil, err
	}
	body, _, err := a.objects.OpenObject(ctx, threadOf(handoff), caller, sha256)
	if errors.Is(err, workspacestore.ErrNotFound) {
		return nil, workspaceruntime.Refuse(workspaceruntime.RefusalSegmentMissing)
	}
	if err != nil {
		return nil, storeError(err)
	}
	return body, nil
}

// PutSegment streams one segment into the thread's bucket.
func (a *Adapter) PutSegment(ctx context.Context, claim workspaceruntime.ClaimRef, sha256 string, size uint64, body io.Reader) (workspaceruntime.UploadResult, error) {
	if size == 0 || size > workspaceruntime.MaxSegmentBytes {
		return workspaceruntime.UploadResult{}, workspaceruntime.ErrInvalidRequest
	}
	handoff, caller, err := a.running(ctx, claim)
	if err != nil {
		return workspaceruntime.UploadResult{}, err
	}
	exists, err := a.objects.UploadSegment(ctx, threadOf(handoff), caller, handoff.Epoch, sha256, int64(size), body)
	if err != nil {
		return workspaceruntime.UploadResult{}, storeError(err)
	}
	return workspaceruntime.UploadResult{Exists: exists}, nil
}

// Publish moves refs/elitea/cloud (the publish stamps the answer's
// workspace_commit in its transaction).
func (a *Adapter) Publish(ctx context.Context, claim workspaceruntime.ClaimRef, request workspaceruntime.PublishRequest) (workspaceruntime.PublishResult, error) {
	if err := workspaceruntime.ValidateExecutorPublish(request); err != nil {
		return workspaceruntime.PublishResult{}, err
	}
	handoff, caller, err := a.running(ctx, claim)
	if err != nil {
		return workspaceruntime.PublishResult{}, err
	}
	refs := make([]workspacestore.RefUpdate, 0, len(request.Refs))
	for _, update := range request.Refs {
		var old *string
		if update.Old != nil {
			value := *update.Old
			old = &value
		}
		refs = append(refs, workspacestore.RefUpdate{Name: update.Name, Old: old, New: update.New})
	}
	result, err := a.objects.Publish(ctx, workspacestore.PublishRequest{
		Thread: threadOf(handoff), Caller: caller, Epoch: handoff.Epoch, Kind: request.Kind,
		SHA256: request.SegmentSHA256, Bytes: int64(request.Bytes), Prerequisites: []string{},
		Refs: refs, ObjectFormat: request.ObjectFormat,
	})
	if err != nil {
		return workspaceruntime.PublishResult{}, storeError(err)
	}
	return workspaceruntime.PublishResult{Seq: nonNegative(result.Seq), Refs: refList(result.Refs)}, nil
}

// EnvSecret redeems the thread's harvested env for the running turn.
func (a *Adapter) EnvSecret(ctx context.Context, claim workspaceruntime.ClaimRef) (map[string]string, error) {
	handoff, _, err := a.running(ctx, claim)
	if err != nil {
		return nil, err
	}
	env, err := a.handoffs.CloudTurnEnv(ctx, threadOf(handoff), handoff.Epoch, claim.ExecutionID)
	if err != nil {
		return nil, storeError(err)
	}
	if env == nil {
		env = map[string]string{}
	}
	return env, nil
}

// RecordEvent writes the running turn's executor audit event.
func (a *Adapter) RecordEvent(ctx context.Context, claim workspaceruntime.ClaimRef, event workspaceruntime.ExecutorEvent) error {
	if event.Kind != workspaceruntime.EventScanBlocked && event.Kind != workspaceruntime.EventPullRequestOpened {
		return workspaceruntime.ErrInvalidRequest
	}
	handoff, _, err := a.running(ctx, claim)
	if err != nil {
		return err
	}
	return storeError(a.handoffs.RecordExecutorEvent(ctx, threadOf(handoff), handoff.Epoch, claim.ExecutionID, event.Kind, event.Attrs))
}

// ---- TurnLifecycle ----

// FinishExecution ends executionID's cloud turn, if it is one.
func (a *Adapter) FinishExecution(ctx context.Context, executionID string) error {
	if executionID == "" || len(executionID) > 64 {
		return nil
	}
	handoff, ok, err := a.handoffs.ResolveRunningTurn(ctx, executionID)
	if err != nil || !ok {
		return err
	}
	return a.finish(ctx, handoff)
}

func (a *Adapter) finish(ctx context.Context, handoff workspacehandoff.Handoff) error {
	finished, err := a.handoffs.FinishCloudTurn(ctx, threadOf(handoff), handoff.Epoch, handoff.RunningExecutionID)
	if errors.Is(err, workspacehandoff.ErrStaleEpoch) || errors.Is(err, workspacehandoff.ErrNotFound) {
		// Another hook, the reconciler or a forced take back ended it first.
		return nil
	}
	if err != nil {
		return fmt.Errorf("finish cloud turn: %w", err)
	}
	a.logger.InfoContext(ctx, "cloud turn ended", "project_id", handoff.ProjectID,
		"conversation_uuid", handoff.ConversationUUID, "execution_id", handoff.RunningExecutionID,
		"state", string(finished.State), "epoch", finished.Epoch)
	return nil
}

// WrapUpRequested: executionID runs a cloud turn whose thread is being taken
// back. Read on every lease poll, so it is one indexed lookup.
func (a *Adapter) WrapUpRequested(ctx context.Context, executionID string) (bool, error) {
	if executionID == "" || len(executionID) > 64 {
		return false, nil
	}
	return a.threads.ReclaimPending(ctx, executionID)
}

// reconcileBatch bounds one reconciliation pass.
const reconcileBatch = 100

// ReconcileFinished ends every running cloud turn whose execution is terminal.
func (a *Adapter) ReconcileFinished(ctx context.Context) (int, error) {
	due, err := a.threads.FinishedCloudTurns(ctx, reconcileBatch)
	if err != nil {
		return 0, err
	}
	var failures []error
	for _, handoff := range due {
		if err := a.finish(ctx, handoff); err != nil {
			failures = append(failures, err)
		}
	}
	return len(due), errors.Join(failures...)
}

// ---- errors ----

// storeError maps the server services' errors onto the seam's.
func storeError(err error) error {
	var conflict *workspacestore.RefConflictError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &conflict):
		current := []workspaceruntime.Ref{}
		for name, value := range conflict.Current {
			if value != nil {
				current = append(current, workspaceruntime.Ref{Name: name, SHA: *value})
			}
		}
		sort.Slice(current, func(i, j int) bool { return current[i].Name < current[j].Name })
		return &workspaceruntime.Refusal{Code: workspaceruntime.RefusalRefConflict, Current: current}
	case errors.Is(err, workspacestore.ErrThreadBusy):
		return workspaceruntime.ErrThreadBusy
	case errors.Is(err, workspacestore.ErrStaleEpoch), errors.Is(err, workspacehandoff.ErrStaleEpoch):
		return workspaceruntime.Refuse(workspaceruntime.RefusalStaleEpoch)
	case errors.Is(err, workspacestore.ErrNotFound), errors.Is(err, workspacehandoff.ErrNotFound):
		return workspaceruntime.Refuse(workspaceruntime.RefusalNotWorkspaceTurn)
	case errors.Is(err, workspacestore.ErrRefNotAllowed):
		return workspaceruntime.Refuse(workspaceruntime.RefusalRefNotAllowed)
	case errors.Is(err, workspacestore.ErrSegmentMissing):
		return workspaceruntime.Refuse(workspaceruntime.RefusalSegmentMissing)
	case errors.Is(err, workspacestore.ErrChecksumMismatch):
		return workspaceruntime.Refuse(workspaceruntime.RefusalChecksumMismatch)
	case errors.Is(err, workspacestore.ErrUnsupportedObjectFormat):
		return workspaceruntime.Refuse(workspaceruntime.RefusalUnsupportedObjectFormat)
	case errors.Is(err, workspacestore.ErrSegmentTooLarge):
		return workspaceruntime.Refuse(workspaceruntime.RefusalSegmentTooLarge)
	case errors.Is(err, workspacestore.ErrQuotaExceeded):
		return workspaceruntime.Refuse(workspaceruntime.RefusalQuotaExceeded)
	case errors.Is(err, workspacestore.ErrInvalid), errors.Is(err, workspacehandoff.ErrInvalid):
		return workspaceruntime.ErrInvalidRequest
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		// The database or the object store: the worker may retry.
		return errors.Join(workspaceruntime.ErrUnavailable, err)
	}
}

func refList(refs map[string]string) []workspaceruntime.Ref {
	out := make([]workspaceruntime.Ref, 0, len(refs))
	for name, sha := range refs {
		out = append(out, workspaceruntime.Ref{Name: name, SHA: sha})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
