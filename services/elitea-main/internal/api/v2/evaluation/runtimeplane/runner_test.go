package runtimeplane

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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

func (f *fakeStore) StepsSettled(context.Context, int64, string) (bool, error) { return true, nil }

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
	records  []runevidence.Record
	reads    []string
	auditErr error
}

type allowEvidence bool

func (a allowEvidence) MayReadRunEvidence(context.Context, int64, int64) (bool, error) {
	return bool(a), nil
}

type fakeUsage struct{ run, perCase evaluation.Usage }

func (f fakeUsage) EvaluationUsage(_ context.Context, _, _, caseID string) (evaluation.Usage, error) {
	if caseID == "" {
		return f.run, nil
	}
	return f.perCase, nil
}

func (f *fakeEvidence) Steps(context.Context, int64, string) ([]runevidence.Record, error) {
	return f.records, nil
}

func (f *fakeEvidence) RecordContentRead(_ context.Context, _, _ int64, _, stepID string, _ runevidence.Side, _ string) error {
	if f.auditErr != nil {
		return f.auditErr
	}
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

func digestOf(body string) []byte {
	sum := sha256.Sum256([]byte(body))
	return sum[:]
}

func toolRecord(seq int64, body string) runevidence.Record {
	tool := record(seq, runevidence.KindToolCall, "fetch", `{"error": null}`)
	tool.Outputs = runevidence.ContentMeta{ObjectKey: "k/out", Digest: digestOf(body), Size: int64(len(body))}
	return tool
}

func copyRunner(evidence *fakeEvidence, content fakeContent, allowed bool) *Runner {
	return newRunner(&fakeStore{turns: []Turn{{Settled: true, Text: "answer"}}}, &fakeStarter{}, &fakeCanceller{}, evidence, content).
		WithAuthorizer(allowEvidence(allowed))
}

func keepRequest() evaluation.AgentRunRequest {
	request := caseRequest()
	request.KeepToolOutputs = true
	return request
}

func TestRunCaseReadsStepsErrorsAndKeptToolOutputs(t *testing.T) {
	t.Parallel()

	failing := record(3, runevidence.KindToolCall, "fetch", `{"error": "503 upstream"}`)
	evidence := &fakeEvidence{records: []runevidence.Record{
		record(1, runevidence.KindModelCall, "m", `{"tool_calls": 1}`), toolRecord(2, "page body"), failing,
	}}

	result, err := copyRunner(evidence, fakeContent{"k/out": "page body"}, true).RunCase(context.Background(), keepRequest())

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

	request := keepRequest()
	request.KeepToolOutputs = false
	dropped, _ := copyRunner(evidence, fakeContent{"k/out": "page body"}, true).RunCase(context.Background(), request)
	if dropped.Steps[1].Output != "" {
		t.Errorf("a tool output was read although the suite drops them: %q", dropped.Steps[1].Output)
	}
}

func outputOf(t *testing.T, evidence *fakeEvidence, content fakeContent, allowed bool) string {
	t.Helper()
	result, err := copyRunner(evidence, content, allowed).RunCase(context.Background(), keepRequest())
	if err != nil || len(result.Steps) != 1 {
		t.Fatalf("steps %d, err %v", len(result.Steps), err)
	}
	return result.Steps[0].Output
}

// A copy follows the evidence rules: refused when the author may not read
// evidence, when the audit write fails, and when the bytes do not match the
// stored digest.
func TestToolOutputCopyFollowsTheEvidenceRules(t *testing.T) {
	t.Parallel()

	rows := func() *fakeEvidence { return &fakeEvidence{records: []runevidence.Record{toolRecord(1, "body")}} }
	if got := outputOf(t, rows(), fakeContent{"k/out": "body"}, true); got != "body" {
		t.Fatalf("a permitted, audited, verified output = %q", got)
	}
	if got := outputOf(t, rows(), fakeContent{"k/out": "body"}, false); got != "" {
		t.Errorf("an author without runs.evidence.read had an output copied: %q", got)
	}
	failedAudit := rows()
	failedAudit.auditErr = errors.New("audit down")
	if got := outputOf(t, failedAudit, fakeContent{"k/out": "body"}, true); got != "" {
		t.Errorf("an output was copied although its audit write failed: %q", got)
	}
	if got := outputOf(t, rows(), fakeContent{"k/out": "tampered"}, true); got != "" {
		t.Errorf("an output that does not match its digest was copied: %q", got)
	}
	big := &fakeEvidence{records: []runevidence.Record{toolRecord(1, "x")}}
	big.records[0].Outputs.Size = maxVerifiedOutputBytes + 1
	if got := outputOf(t, big, fakeContent{"k/out": "x"}, true); got != "" {
		t.Errorf("an oversized output was read: %q", got)
	}
}

func TestOutputExcerptIsStorableInJSONB(t *testing.T) {
	t.Parallel()

	if got := outputExcerpt([]byte("a\x00b")); got != "ab" {
		t.Errorf("NUL not stripped: %q", got)
	}
	if got := outputExcerpt([]byte{0xff, 0xfe, 'a'}); !strings.HasPrefix(got, "base64:") {
		t.Errorf("invalid UTF-8 kept as text: %q", got)
	}
	long := strings.Repeat("é", maxOutputExcerptBytes) // two bytes each
	got := outputExcerpt([]byte(long))
	if len(got) > maxOutputExcerptBytes || !utf8.ValidString(got) {
		t.Errorf("excerpt len %d valid=%v: a cut must not split a character", len(got), utf8.ValidString(got))
	}
}

// While a case runs the runner polls the attributed usage and cancels the turn
// when a per-case or a stopping per-run limit is crossed.
func TestRunCaseCancelsAtATokenOrCostLimit(t *testing.T) {
	t.Parallel()

	tokens, cost := int64(1000), 2.0
	cases := []struct {
		name   string
		limits evaluation.RunLimits
		usage  fakeUsage
		stops  bool
	}{
		{"case tokens", evaluation.RunLimits{TokenLimitPerCase: &tokens}, fakeUsage{perCase: evaluation.Usage{Tokens: 1500}}, true},
		{"run cost, stopping", evaluation.RunLimits{CostLimitPerRun: &cost, OnRunLimit: evaluation.RunLimitActionStop}, fakeUsage{run: evaluation.Usage{CostUSD: 3}}, true},
		{"run cost, report only", evaluation.RunLimits{CostLimitPerRun: &cost, OnRunLimit: evaluation.RunLimitActionReport}, fakeUsage{run: evaluation.Usage{CostUSD: 3}}, false},
		{"under the limits", evaluation.RunLimits{TokenLimitPerCase: &tokens}, fakeUsage{perCase: evaluation.Usage{Tokens: 10}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			canceller := &fakeCanceller{}
			turns := []Turn{{}, {}, {Settled: true, Text: "done"}}
			request := caseRequest()
			request.Limits = &tc.limits
			result, err := newRunner(&fakeStore{turns: turns}, &fakeStarter{}, canceller, nil, nil).
				WithUsage(tc.usage).RunCase(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if tc.stops {
				if result.Outcome != evaluation.AgentOutcomeLimit || len(canceller.requests) != 1 || result.Failure == "" {
					t.Fatalf("outcome %q failure %q cancels %d", result.Outcome, result.Failure, len(canceller.requests))
				}
				return
			}
			if result.Outcome != evaluation.AgentOutcomeCompleted || len(canceller.requests) != 0 {
				t.Fatalf("outcome %q cancels %d, want a completed run", result.Outcome, len(canceller.requests))
			}
		})
	}
}
