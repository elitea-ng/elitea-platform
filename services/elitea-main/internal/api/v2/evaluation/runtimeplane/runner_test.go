package runtimeplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runevidence"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

type fakeStore struct {
	mu        sync.Mutex
	turns     []Turn // one per read; the last repeats
	reads     int
	stepCount int
	prepared  []string
	discarded []string
}

func (f *fakeStore) PrepareConversation(_ context.Context, _, _, applicationID, versionID int64, _ string) (string, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepared = append(f.prepared, "app/version")
	_ = applicationID
	_ = versionID
	return "conv-uuid", 5, nil
}

func (f *fakeStore) DiscardConversation(_ context.Context, _ int64, conversationUUID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.discarded = append(f.discarded, conversationUUID)
}

func (f *fakeStore) ReadTurn(context.Context, int64, string) (Turn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := f.reads
	if index >= len(f.turns) {
		index = len(f.turns) - 1
	}
	f.reads++
	return f.turns[index], nil
}

func (f *fakeStore) StepCount(context.Context, int64, string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stepCount, nil
}

type fakeStarter struct {
	request agentexecutionapp.CurrentApplicationStartRequest
	err     error
}

func (f *fakeStarter) StartCurrentApplication(
	_ context.Context, request agentexecutionapp.CurrentApplicationStartRequest,
) (agentexecutionapp.CurrentApplicationStartOutcome, error) {
	f.request = request
	if f.err != nil {
		return agentexecutionapp.CurrentApplicationStartOutcome{}, f.err
	}
	return agentexecutionapp.CurrentApplicationStartOutcome{ExecutionID: "exec-1", ResponseMessageID: "resp-1"}, nil
}

type fakeCanceller struct {
	mu       sync.Mutex
	requests []agentexecutionapp.CurrentAgentCancelRequest
}

func (f *fakeCanceller) Cancel(
	_ context.Context, request agentexecutionapp.CurrentAgentCancelRequest,
) (agentexecutionapp.CurrentAgentCancelOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	return agentexecutionapp.CurrentAgentCancelOutcome{}, nil
}

type fakeEvidence struct {
	records []runevidence.Record
	reads   []string
}

func (f *fakeEvidence) Steps(context.Context, int64, string) ([]runevidence.Record, error) {
	return f.records, nil
}

func (f *fakeEvidence) RecordContentRead(_ context.Context, _, _ int64, _, stepID string, _ runevidence.Side, _ string) error {
	f.reads = append(f.reads, stepID)
	return nil
}

type fakeContent map[string]string

func (f fakeContent) Get(_ context.Context, key string) ([]byte, error) {
	if body, ok := f[key]; ok {
		return []byte(body), nil
	}
	return nil, errors.New("missing")
}

func newRunner(store *fakeStore, starter *fakeStarter, canceller *fakeCanceller, evidence *fakeEvidence, content Content) *Runner {
	var ev Evidence
	if evidence != nil {
		ev = evidence
	}
	runner := New(store, starter, canceller, ev, content, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.pollInitial, runner.pollMax, runner.settleWait = time.Millisecond, 2*time.Millisecond, time.Millisecond
	return runner
}

func caseRequest() evaluation.AgentRunRequest {
	return evaluation.AgentRunRequest{
		ProjectID: "3", RunID: "8", CaseID: "21", ActorUserID: 7, ApplicationID: 4, VersionID: 9,
		Input: "what is Go?", TimeLimit: 5 * time.Second,
	}
}

func record(seq int64, kind runevidence.Kind, name, summary string) runevidence.Record {
	started := time.Unix(100, 0)
	return runevidence.Record{
		RunID: "exec-1", StepID: "step-" + name, Seq: seq, Kind: kind, Name: name, ToolkitID: "kit",
		StartedAt: started, EndedAt: started.Add(120 * time.Millisecond), Summary: []byte(summary),
		Inputs: runevidence.ContentMeta{Digest: []byte{1, 2, 3}},
	}
}

// The case is admitted through the chat start use case, with the author's
// identity, as a programmatic start, and the final text is the answer.
func TestRunCaseAdmitsThroughTheChatStartAndReturnsTheFinalText(t *testing.T) {
	t.Parallel()

	store := &fakeStore{turns: []Turn{{}, {Settled: true, Text: "Go is a language"}}}
	starter := &fakeStarter{}
	var admitted string
	request := caseRequest()
	request.Admitted = func(_ context.Context, executionID, conversationUUID string) {
		admitted = executionID + "/" + conversationUUID
	}

	result, err := newRunner(store, starter, &fakeCanceller{}, nil, nil).RunCase(context.Background(), request)

	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != evaluation.AgentOutcomeCompleted || result.Output != "Go is a language" {
		t.Fatalf("result = %+v", result)
	}
	got := starter.request
	if got.ProjectID != 3 || got.ActorUserID != 7 || got.UserInput != "what is Go?" ||
		got.ConversationUUID != "conv-uuid" || got.TargetParticipantID != 5 ||
		got.TriggerOrigin != executiondomain.TriggerOriginAPI || got.QuestionID == "" {
		t.Errorf("the start request = %+v", got)
	}
	if got.Unattended != nil {
		t.Error("an evaluation case must start with no unattended grant")
	}
	if admitted != "exec-1/conv-uuid" {
		t.Errorf("Admitted was told %q", admitted)
	}
}

func TestRunCaseDiscardsTheConversationWhenTheStartIsRefused(t *testing.T) {
	t.Parallel()

	store := &fakeStore{turns: []Turn{{Settled: true}}}
	starter := &fakeStarter{err: agentexecutionapp.ErrUnsupportedCurrentAgentStart}

	_, err := newRunner(store, starter, &fakeCanceller{}, nil, nil).RunCase(context.Background(), caseRequest())

	if err == nil || !strings.Contains(err.Error(), "admission contract") {
		t.Fatalf("error = %v, want the admission refusal named", err)
	}
	if len(store.discarded) != 1 || store.discarded[0] != "conv-uuid" {
		t.Fatalf("discarded %v: a refused start must not leave an empty conversation", store.discarded)
	}
}

// At the time limit the run is cancelled through the chat stop use case, and
// the case reports a timeout.
func TestRunCaseTimeLimitCancelsTheRun(t *testing.T) {
	t.Parallel()

	store := &fakeStore{turns: []Turn{{}}}
	canceller := &fakeCanceller{}
	request := caseRequest()
	request.TimeLimit = 20 * time.Millisecond

	result, err := newRunner(store, &fakeStarter{}, canceller, nil, nil).RunCase(context.Background(), request)

	if err != nil || result.Outcome != evaluation.AgentOutcomeTimedOut {
		t.Fatalf("outcome %q, err %v, want timed_out", result.Outcome, err)
	}
	if len(canceller.requests) != 1 || canceller.requests[0].ResponseMessageID != "resp-1" || canceller.requests[0].ActorUserID != 7 {
		t.Fatalf("cancel requests = %+v", canceller.requests)
	}
}

func TestRunCaseStepLimitCancelsTheRunWhileItIsGoing(t *testing.T) {
	t.Parallel()

	store := &fakeStore{turns: []Turn{{}}, stepCount: 6}
	canceller := &fakeCanceller{}
	request := caseRequest()
	request.StepLimit = 5

	result, err := newRunner(store, &fakeStarter{}, canceller, nil, nil).RunCase(context.Background(), request)

	if err != nil || result.Outcome != evaluation.AgentOutcomeStepLimit {
		t.Fatalf("outcome %q, err %v, want step_limit", result.Outcome, err)
	}
	if len(canceller.requests) != 1 {
		t.Fatalf("the run was not cancelled: %+v", canceller.requests)
	}
}

// A run at exactly its limit is not over it.
func TestRunCaseAtTheStepLimitIsNotOverIt(t *testing.T) {
	t.Parallel()

	store := &fakeStore{turns: []Turn{{Settled: true, Text: "done"}}, stepCount: 5}
	evidence := &fakeEvidence{}
	for seq := int64(1); seq <= 5; seq++ {
		evidence.records = append(evidence.records, record(seq, runevidence.KindModelCall, "m", `{}`))
	}
	request := caseRequest()
	request.StepLimit = 5

	result, err := newRunner(store, &fakeStarter{}, &fakeCanceller{}, evidence, nil).RunCase(context.Background(), request)

	if err != nil || result.Outcome != evaluation.AgentOutcomeCompleted {
		t.Fatalf("outcome %q, err %v, want completed", result.Outcome, err)
	}
}

// A run that overshoots between two polls is judged on the steps it recorded.
func TestRunCaseThatOvershotTheStepLimitBeforeSettlingFails(t *testing.T) {
	t.Parallel()

	store := &fakeStore{turns: []Turn{{Settled: true, Text: "done"}}}
	evidence := &fakeEvidence{}
	for seq := int64(1); seq <= 4; seq++ {
		evidence.records = append(evidence.records, record(seq, runevidence.KindToolCall, "t", `{}`))
	}
	request := caseRequest()
	request.StepLimit = 3

	result, _ := newRunner(store, &fakeStarter{}, &fakeCanceller{}, evidence, nil).RunCase(context.Background(), request)

	if result.Outcome != evaluation.AgentOutcomeStepLimit {
		t.Fatalf("outcome = %q, want step_limit", result.Outcome)
	}
}

func TestRunCasePausedRunIsCancelledWithAReason(t *testing.T) {
	t.Parallel()

	store := &fakeStore{turns: []Turn{{Settled: true, Paused: "human approval"}}}
	canceller := &fakeCanceller{}

	result, err := newRunner(store, &fakeStarter{}, canceller, nil, nil).RunCase(context.Background(), caseRequest())

	if err != nil || result.Outcome != evaluation.AgentOutcomePaused {
		t.Fatalf("outcome %q, err %v", result.Outcome, err)
	}
	if !strings.Contains(result.Failure, "human approval") || len(canceller.requests) != 1 {
		t.Fatalf("failure %q, cancels %d", result.Failure, len(canceller.requests))
	}
}

func TestRunCaseFailedRunCarriesTheRuntimeMessage(t *testing.T) {
	t.Parallel()

	store := &fakeStore{turns: []Turn{{Settled: true, Failed: true, Failure: "model refused"}}}

	result, _ := newRunner(store, &fakeStarter{}, &fakeCanceller{}, nil, nil).RunCase(context.Background(), caseRequest())

	if result.Outcome != evaluation.AgentOutcomeFailed || !strings.Contains(result.Failure, "model refused") {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunCaseCallerCancellationStopsTheAgentRun(t *testing.T) {
	t.Parallel()

	store := &fakeStore{turns: []Turn{{}}}
	canceller := &fakeCanceller{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(15 * time.Millisecond)
		cancel()
	}()

	_, err := newRunner(store, &fakeStarter{}, canceller, nil, nil).RunCase(ctx, caseRequest())

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if len(canceller.requests) != 1 {
		t.Fatalf("the agent run outlived its evaluation: %d cancels", len(canceller.requests))
	}
}

func TestRunCaseReadsStepsErrorsAndKeptToolOutputs(t *testing.T) {
	t.Parallel()

	tool := record(2, runevidence.KindToolCall, "fetch", `{"error": null}`)
	tool.Outputs = runevidence.ContentMeta{ObjectKey: "k/out"}
	failing := record(3, runevidence.KindToolCall, "fetch", `{"error": "503 upstream"}`)
	evidence := &fakeEvidence{records: []runevidence.Record{
		record(1, runevidence.KindModelCall, "m", `{"tool_calls": 1}`), tool, failing,
	}}
	store := &fakeStore{turns: []Turn{{Settled: true, Text: "answer"}}}
	request := caseRequest()
	request.KeepToolOutputs = true

	result, err := newRunner(store, &fakeStarter{}, &fakeCanceller{}, evidence, fakeContent{"k/out": "page body"}).
		RunCase(context.Background(), request)

	if err != nil || len(result.Steps) != 3 {
		t.Fatalf("steps %d, err %v", len(result.Steps), err)
	}
	if result.Steps[1].Output != "page body" || result.Steps[1].DurationMs != 120 || result.Steps[1].Error != "" {
		t.Errorf("tool step = %+v", result.Steps[1])
	}
	if result.Steps[2].Error != "503 upstream" || result.Steps[2].InputsDigest == "" {
		t.Errorf("failing step = %+v", result.Steps[2])
	}
	if len(evidence.reads) != 1 {
		t.Errorf("the tool output read was audited %d times, want once", len(evidence.reads))
	}

	request.KeepToolOutputs = false
	store2 := &fakeStore{turns: []Turn{{Settled: true, Text: "answer"}}}
	dropped, _ := newRunner(store2, &fakeStarter{}, &fakeCanceller{}, evidence, fakeContent{"k/out": "page body"}).
		RunCase(context.Background(), request)
	if dropped.Steps[1].Output != "" {
		t.Errorf("a tool output was read although the suite drops them: %q", dropped.Steps[1].Output)
	}
}
