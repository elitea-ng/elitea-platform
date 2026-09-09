package pipelineruns

import (
	"context"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// fakeTracker is an in-memory Tracker for the hook logic's own unit tests —
// no database, no settlement machinery, just the claim/emit contract.
type fakeTracker struct {
	rows map[string]trackedRow
	// claimed records every execution id ClaimForEvent was asked to claim,
	// in order, including a second ask for an already-claimed row — so a
	// test can assert exactly-once semantics without a real UPDATE...
	// RETURNING race.
	claimed []string
	err     error
}

type trackedRow struct {
	run          Run
	startedAt    time.Time
	errorSummary string
	claimedAt    *time.Time
}

func newFakeTracker() *fakeTracker { return &fakeTracker{rows: make(map[string]trackedRow)} }

func (f *fakeTracker) seed(run Run, startedAt time.Time, errorSummary string) {
	f.rows[run.ExecutionID] = trackedRow{run: run, startedAt: startedAt, errorSummary: errorSummary}
}

func (f *fakeTracker) RecordRunStart(_ context.Context, run Run) error {
	if f.err != nil {
		return f.err
	}
	if _, exists := f.rows[run.ExecutionID]; !exists {
		f.rows[run.ExecutionID] = trackedRow{run: run, startedAt: time.Now()}
	}
	return nil
}

func (f *fakeTracker) RecordExecutionError(_ context.Context, executionID, safeMessage string) error {
	if f.err != nil {
		return f.err
	}
	row, ok := f.rows[executionID]
	if !ok {
		return nil // not a tracked pipeline run — no-op, per the interface contract
	}
	row.errorSummary = safeMessage
	f.rows[executionID] = row
	return nil
}

func (f *fakeTracker) ClaimForEvent(_ context.Context, executionID string) (Run, time.Time, string, bool, error) {
	f.claimed = append(f.claimed, executionID)
	if f.err != nil {
		return Run{}, time.Time{}, "", false, f.err
	}
	row, ok := f.rows[executionID]
	if !ok || row.claimedAt != nil {
		return Run{}, time.Time{}, "", false, nil
	}
	now := time.Now()
	row.claimedAt = &now
	f.rows[executionID] = row
	return row.run, row.startedAt, row.errorSummary, true, nil
}

// fakeEmitter records every Emit call.
type fakeEmitter struct {
	calls []emittedEvent
}

type emittedEvent struct {
	projectID string
	eventType string
	payload   any
}

func (f *fakeEmitter) Emit(_ context.Context, projectID, eventType string, payload any) {
	f.calls = append(f.calls, emittedEvent{projectID: projectID, eventType: eventType, payload: payload})
}

func succeededProposal(executionID string) executionapp.SettlementProposal {
	return executionapp.SettlementProposal{
		Fence:                   runtimedomain.Fence{ExecutionID: executionID, Generation: 1},
		ProposalID:              "proposal-1",
		Outcome:                 executionapp.SettlementSucceeded,
		TerminalLogicalOutputID: "agent-execution:" + executionID,
		TerminalEventID:         "event-1",
		TerminalSequence:        1,
	}
}

func failedProposal(executionID string) executionapp.SettlementProposal {
	p := succeededProposal(executionID)
	p.Outcome = executionapp.SettlementFailed
	return p
}

/* ── NewSettlementHook ────────────────────────────────────────────────── */

func TestSettlementHookEmitsSucceededForATrackedRun(t *testing.T) {
	tracker := newFakeTracker()
	started := time.Now().Add(-5 * time.Second)
	run := Run{ExecutionID: "exec-1", ProjectID: "proj-1", ApplicationID: 42, VersionID: 7, ConversationUUID: "conv-1", Origin: "Webhook"}
	tracker.seed(run, started, "")
	emitter := &fakeEmitter{}

	hook := NewSettlementHook(tracker, emitter)
	hook(context.Background(), succeededProposal("exec-1"), executionapp.SettlementReceipt{ID: "r1", Outcome: executionapp.SettlementSucceeded})

	if len(emitter.calls) != 1 {
		t.Fatalf("Emit calls = %d, want 1", len(emitter.calls))
	}
	call := emitter.calls[0]
	if call.projectID != "proj-1" {
		t.Errorf("projectID = %q, want proj-1", call.projectID)
	}
	if call.eventType != "pipeline.run.succeeded" {
		t.Errorf("eventType = %q, want pipeline.run.succeeded", call.eventType)
	}
	payload, ok := call.payload.(RunOutcomePayload)
	if !ok {
		t.Fatalf("payload type = %T, want RunOutcomePayload", call.payload)
	}
	if payload.ExecutionID != "exec-1" || payload.ApplicationID != 42 || payload.VersionID != 7 || payload.ConversationUUID != "conv-1" {
		t.Errorf("payload identity = %+v", payload)
	}
	if payload.Status != "succeeded" {
		t.Errorf("status = %q, want succeeded", payload.Status)
	}
	if payload.ErrorSummary != "" {
		t.Errorf("a SUCCEEDED payload carries error_summary = %q, want empty", payload.ErrorSummary)
	}
	if payload.DurationMs < 4000 {
		t.Errorf("duration_ms = %d, want roughly >= 5000 (started 5s ago)", payload.DurationMs)
	}
}

func TestSettlementHookEmitsFailedWithTheErrorSummary(t *testing.T) {
	tracker := newFakeTracker()
	run := Run{ExecutionID: "exec-2", ProjectID: "proj-2", ApplicationID: 1, VersionID: 1, ConversationUUID: "conv-2", Origin: "Schedule"}
	tracker.seed(run, time.Now(), "the pipeline's tool call timed out")
	emitter := &fakeEmitter{}

	hook := NewSettlementHook(tracker, emitter)
	hook(context.Background(), failedProposal("exec-2"), executionapp.SettlementReceipt{ID: "r2", Outcome: executionapp.SettlementFailed})

	if len(emitter.calls) != 1 {
		t.Fatalf("Emit calls = %d, want 1", len(emitter.calls))
	}
	call := emitter.calls[0]
	if call.eventType != "pipeline.run.failed" {
		t.Errorf("eventType = %q, want pipeline.run.failed", call.eventType)
	}
	payload := call.payload.(RunOutcomePayload)
	if payload.Status != "failed" {
		t.Errorf("status = %q, want failed", payload.Status)
	}
	if payload.ErrorSummary != "the pipeline's tool call timed out" {
		t.Errorf("error_summary = %q", payload.ErrorSummary)
	}
}

func TestSettlementHookEmitsFailedForACancelledRun(t *testing.T) {
	tracker := newFakeTracker()
	run := Run{ExecutionID: "exec-3", ProjectID: "proj-3", ApplicationID: 1, VersionID: 1, ConversationUUID: "conv-3", Origin: "Webhook"}
	tracker.seed(run, time.Now(), "")
	emitter := &fakeEmitter{}

	proposal := succeededProposal("exec-3")
	proposal.Outcome = executionapp.SettlementCancelled
	hook := NewSettlementHook(tracker, emitter)
	hook(context.Background(), proposal, executionapp.SettlementReceipt{ID: "r3", Outcome: executionapp.SettlementCancelled})

	if len(emitter.calls) != 1 {
		t.Fatalf("Emit calls = %d, want 1", len(emitter.calls))
	}
	call := emitter.calls[0]
	if call.eventType != "pipeline.run.failed" {
		t.Errorf("a cancelled run's eventType = %q, want pipeline.run.failed (the catalogue has no third event)", call.eventType)
	}
	if call.payload.(RunOutcomePayload).Status != "cancelled" {
		t.Errorf("status = %q, want cancelled", call.payload.(RunOutcomePayload).Status)
	}
}

func TestSettlementHookIsSilentForAnUntrackedExecution(t *testing.T) {
	tracker := newFakeTracker() // no row seeded — an ordinary chat turn, say
	emitter := &fakeEmitter{}

	hook := NewSettlementHook(tracker, emitter)
	hook(context.Background(), succeededProposal("not-a-pipeline-run"), executionapp.SettlementReceipt{ID: "r", Outcome: executionapp.SettlementSucceeded})

	if len(emitter.calls) != 0 {
		t.Fatalf("Emit called %d times for an execution the tracker never recorded", len(emitter.calls))
	}
}

func TestSettlementHookEmitsAtMostOnceAcrossReplays(t *testing.T) {
	tracker := newFakeTracker()
	run := Run{ExecutionID: "exec-4", ProjectID: "proj-4", ApplicationID: 1, VersionID: 1, ConversationUUID: "conv-4", Origin: "Webhook"}
	tracker.seed(run, time.Now(), "")
	emitter := &fakeEmitter{}

	hook := NewSettlementHook(tracker, emitter)
	proposal := succeededProposal("exec-4")
	receipt := executionapp.SettlementReceipt{ID: "r4", Outcome: executionapp.SettlementSucceeded}

	// Two calls, the same shape a settlement replay produces per
	// execution.AfterSettleHook's own doc comment.
	hook(context.Background(), proposal, receipt)
	hook(context.Background(), proposal, receipt)

	if len(emitter.calls) != 1 {
		t.Fatalf("Emit called %d times across a replay, want exactly 1", len(emitter.calls))
	}
	if len(tracker.claimed) != 2 {
		t.Fatalf("ClaimForEvent called %d times, want 2 (the hook must still ASK on the replay)", len(tracker.claimed))
	}
}

func TestSettlementHookDoesNothingWithoutATrackerOrEmitter(t *testing.T) {
	hook := NewSettlementHook(nil, &fakeEmitter{})
	hook(context.Background(), succeededProposal("exec-5"), executionapp.SettlementReceipt{})

	emitter := &fakeEmitter{}
	hook = NewSettlementHook(newFakeTracker(), nil)
	hook(context.Background(), succeededProposal("exec-6"), executionapp.SettlementReceipt{})
	if len(emitter.calls) != 0 {
		t.Fatal("unreachable emitter recorded a call")
	}
}

/* ── NewFailureObserver ───────────────────────────────────────────────── */

func TestFailureObserverRecordsTheMessageForATrackedRun(t *testing.T) {
	tracker := newFakeTracker()
	run := Run{ExecutionID: "exec-7", ProjectID: "proj-7"}
	tracker.seed(run, time.Now(), "")

	observer := NewFailureObserver(tracker)
	observer(context.Background(), "exec-7", "the tool call failed")

	row := tracker.rows["exec-7"]
	if row.errorSummary != "the tool call failed" {
		t.Errorf("error_summary = %q", row.errorSummary)
	}
}

func TestFailureObserverIsANoOpForAnUntrackedExecution(t *testing.T) {
	tracker := newFakeTracker()
	observer := NewFailureObserver(tracker)
	// Must not panic and must not create a row: this is an ordinary chat
	// turn's failure, which pipeline_runs has no business tracking.
	observer(context.Background(), "not-a-pipeline-run", "boom")
	if _, exists := tracker.rows["not-a-pipeline-run"]; exists {
		t.Fatal("the observer created a row for an execution the tracker never started tracking")
	}
}

func TestFailureObserverWithANilTrackerDoesNotPanic(t *testing.T) {
	observer := NewFailureObserver(nil)
	observer(context.Background(), "exec-8", "boom") // must not panic
}
