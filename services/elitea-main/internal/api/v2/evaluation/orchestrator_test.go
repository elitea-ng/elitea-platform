package evaluation

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/predict"
)

// fakeRunRepo is an in-memory RunRepository.
//
// It is a fake and not a mock: it enforces the SAME state transitions the
// PostgreSQL one does — the claim only moves `created`, the finish only moves
// `running`, results upsert on (run, case, dimension) — because those
// transitions are the mechanism under test. A mock that recorded calls would
// let a cancellation test pass while the real repository overwrote the
// cancelled status a millisecond later.
type fakeRunRepo struct {
	mu       sync.Mutex
	runs     map[string]*Run
	results  map[string]RunResult
	cases    []DatasetCase
	version  AgentVersion
	casesErr error
	beats    []int
}

func newFakeRunRepo() *fakeRunRepo {
	return &fakeRunRepo{
		runs:    map[string]*Run{},
		results: map[string]RunResult{},
		version: AgentVersion{ID: 9, Instructions: "be helpful", ModelName: "qwen"},
	}
}

func (f *fakeRunRepo) seedRun(run Run) {
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := run
	f.runs[run.ID] = &copied
}

func (f *fakeRunRepo) run(id string) Run {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.runs[id]
}

func (f *fakeRunRepo) storedResults() []RunResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RunResult, 0, len(f.results))
	for _, result := range f.results {
		out = append(out, result)
	}
	return out
}

func (f *fakeRunRepo) ListRuns(context.Context, string, RunListFilter) ([]Run, error) {
	return nil, nil
}

func (f *fakeRunRepo) GetRun(_ context.Context, _, runID string) (Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.runs[runID]
	if !ok {
		return Run{}, errors.New("not found")
	}
	return *run, nil
}

func (f *fakeRunRepo) CreateRun(_ context.Context, _ string, run Run) (Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run.ID = strconv.Itoa(len(f.runs) + 1)
	copied := run
	f.runs[run.ID] = &copied
	return run, nil
}

func (f *fakeRunRepo) CancelRun(_ context.Context, _, runID string) (Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.runs[runID]
	if !ok {
		return Run{}, errors.New("not found")
	}
	if IsRunTerminal(run.Status) {
		return Run{}, errors.New("already terminal")
	}
	run.Status = RunStatusCancelled
	return *run, nil
}

func (f *fakeRunRepo) ListResults(_ context.Context, _, runID string, _ ResultPage) ([]RunResult, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []RunResult{}
	for _, result := range f.results {
		if result.RunID == runID {
			out = append(out, result)
		}
	}
	return out, len(out), nil
}

// ClaimRun reproduces the conditional UPDATE: only a `created` row moves.
func (f *fakeRunRepo) ClaimRun(_ context.Context, _, runID string) (Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.runs[runID]
	if !ok || run.Status != RunStatusCreated {
		return Run{}, errors.New("run is not in the created state")
	}
	run.Status = RunStatusRunning
	return *run, nil
}

func (f *fakeRunRepo) Heartbeat(_ context.Context, _, runID string, done int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.runs[runID]
	if !ok || run.Status != RunStatusRunning {
		return nil
	}
	run.Progress.Done = done
	f.beats = append(f.beats, done)
	return nil
}

func (f *fakeRunRepo) SaveResult(_ context.Context, _ string, result RunResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[result.RunID+"/"+result.DatasetCaseID+"/"+result.DimensionID] = result
	return nil
}

// FinishRun reproduces the `status = 'running'` predicate, which is what stops
// a worker from overwriting a cancellation.
func (f *fakeRunRepo) FinishRun(_ context.Context, _, runID, status string, headline *float64, failure string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.runs[runID]
	if !ok || run.Status != RunStatusRunning {
		return nil
	}
	run.Status = status
	run.HeadlineScore = headline
	run.Error = failure
	return nil
}

func (f *fakeRunRepo) RequeueStaleRuns(context.Context, int) ([]RunRef, error) { return nil, nil }
func (f *fakeRunRepo) PendingRuns(context.Context, int) ([]RunRef, error)      { return nil, nil }

func (f *fakeRunRepo) AgentVersion(context.Context, string, int) (AgentVersion, error) {
	return f.version, nil
}

func (f *fakeRunRepo) DatasetCases(context.Context, string, string) ([]DatasetCase, error) {
	if f.casesErr != nil {
		return nil, f.casesErr
	}
	return f.cases, nil
}

// scriptedJudge answers per call, so a test can make ONE case fail.
type scriptedJudge struct {
	mu      sync.Mutex
	answers []JudgeVerdict
	errs    []error
	calls   int
}

func (s *scriptedJudge) Score(_ context.Context, _ JudgeRequest) (JudgeVerdict, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.calls
	s.calls++
	if index < len(s.errs) && s.errs[index] != nil {
		return JudgeVerdict{Raw: "not json"}, s.errs[index]
	}
	if index < len(s.answers) {
		return s.answers[index], nil
	}
	return JudgeVerdict{Score: 3}, nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func twoCaseRun() (*fakeRunRepo, Run) {
	repo := newFakeRunRepo()
	repo.cases = []DatasetCase{
		{ID: "11", DatasetID: "5", Input: "what is {{topic}}?", Variables: map[string]any{"topic": "Go"}},
		{ID: "12", DatasetID: "5", Input: "and Rust?", Variables: map[string]any{}},
	}
	version := 9
	run := Run{
		ID: "1", DatasetID: "5", ApplicationVersionID: &version,
		Status: RunStatusCreated, ExecutionMode: ExecutionModePredictBlocking,
		Progress: RunProgress{Total: 2},
		Snapshot: RunSnapshot{
			Cases: []SnapshotCase{{ID: "11"}, {ID: "12", OrderIndex: 1}},
			Dimensions: map[string]SnapshotDimension{
				"3": {Name: "Helpfulness", ScaleType: ScaleOrdinal, ScaleMin: 1, ScaleMax: 5, Polarity: PolarityHigherBetter},
			},
			Bindings: []SnapshotBinding{{DimensionID: "3", Engine: EngineAI, Weight: 1}},
		},
	}
	repo.seedRun(run)
	return repo, run
}

// THE HAPPY PATH. Every case is scored, the run finishes, and the headline is
// the average of the NORMALISED scores — read back through the repository, not
// asserted on a return value.
func TestOrchestratorScoresEveryCaseAndFinishes(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	judge := &scriptedJudge{answers: []JudgeVerdict{{Score: 5}, {Score: 3}}}
	completer := &stubCompleter{answer: "an answer"}

	NewOrchestrator(repo, judge, completer, quietLogger()).
		Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	run := repo.run("1")
	if run.Status != RunStatusFinished {
		t.Fatalf("status = %q (error %q), want finished", run.Status, run.Error)
	}
	if run.Progress.Done != 2 {
		t.Errorf("progress.done = %d, want 2", run.Progress.Done)
	}
	// 5 of 1..5 is 100; 3 of 1..5 is 50; the average is 75.
	if run.HeadlineScore == nil || *run.HeadlineScore != 75 {
		t.Fatalf("headline = %v, want 75", run.HeadlineScore)
	}

	results := repo.storedResults()
	if len(results) != 2 {
		t.Fatalf("stored %d results, want 2", len(results))
	}
	for _, result := range results {
		if result.Status != ResultStatusOK {
			t.Errorf("case %s: status = %q, want ok", result.DatasetCaseID, result.Status)
		}
		if result.NativeScore == nil || result.NormalizedScore == nil {
			t.Errorf("case %s: an `ok` result carries no score", result.DatasetCaseID)
		}
		// The evidence is what a person reads when a score surprises them.
		if result.Evidence["output"] != "an answer" {
			t.Errorf("case %s: evidence has no agent output: %v", result.DatasetCaseID, result.Evidence)
		}
	}

	// The case template was substituted before the agent saw it. An
	// unsubstituted `{{topic}}` asks the agent a question containing literal
	// braces, and the score would measure the confusion rather than the agent.
	if len(completer.requests) != 2 {
		t.Fatalf("the agent was called %d times, want once per case", len(completer.requests))
	}
	asked := completer.requests[0].Messages[len(completer.requests[0].Messages)-1].Content
	if asked != "what is Go?" {
		t.Errorf("the agent was asked %q, want the substituted template", asked)
	}
	if completer.requests[0].Messages[0].Content != "be helpful" {
		t.Error("the agent turn did not carry the version's instructions")
	}
}

// DELIVERABLE 3's SECOND REQUIREMENT. A judge failure marks THAT CASE `error`
// and the run still finishes — with a headline over the cases it could score,
// never a zero for the one it could not.
func TestOrchestratorMarksAJudgeFailureAsACaseErrorAndStillFinishes(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	judge := &scriptedJudge{
		errs:    []error{ErrJudgeUnparseable, nil},
		answers: []JudgeVerdict{{}, {Score: 5}},
	}

	NewOrchestrator(repo, judge, &stubCompleter{answer: "an answer"}, quietLogger()).
		Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	run := repo.run("1")
	if run.Status != RunStatusFinished {
		t.Fatalf("status = %q, want finished — one bad case must not error the run", run.Status)
	}
	// 5 of 1..5 is 100, and it is the ONLY scored case. An implementation that
	// counted the failed case as 0 would answer 50.
	if run.HeadlineScore == nil || *run.HeadlineScore != 100 {
		t.Fatalf("headline = %v, want 100 — an errored case is not a zero", run.HeadlineScore)
	}

	var errored, ok int
	for _, result := range repo.storedResults() {
		switch result.Status {
		case ResultStatusError:
			errored++
			if result.NativeScore != nil {
				t.Error("an errored result carries a score")
			}
			if result.Verdict["raw"] != "not json" {
				t.Errorf("the judge's raw answer was not kept: %v", result.Verdict)
			}
		case ResultStatusOK:
			ok++
		}
	}
	if errored != 1 || ok != 1 {
		t.Fatalf("stored %d errored and %d ok results, want 1 and 1", errored, ok)
	}
}

// An agent turn that fails is a CASE error too, and its evidence says which
// half failed. Scoring it 0 would report an unreachable gateway as a bad agent.
func TestOrchestratorReportsAFailedAgentTurnAsACaseError(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	NewOrchestrator(repo, &scriptedJudge{}, &stubCompleter{err: errors.New("gateway said 502")}, quietLogger()).
		Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	run := repo.run("1")
	if run.Status != RunStatusFinished {
		t.Fatalf("status = %q, want finished", run.Status)
	}
	if run.HeadlineScore != nil {
		t.Errorf("headline = %v, want none — nothing was scored", *run.HeadlineScore)
	}
	for _, result := range repo.storedResults() {
		if result.Status != ResultStatusError {
			t.Errorf("case %s: status = %q, want error", result.DatasetCaseID, result.Status)
		}
		if result.Evidence["error"] == nil {
			t.Errorf("case %s: the evidence does not say what failed", result.DatasetCaseID)
		}
	}
}

// CANCELLATION. The row is moved to `cancelled` while the worker is mid-run,
// and the worker must NOT overwrite it with `finished`.
func TestOrchestratorCancellationSurvivesTheWorkersOwnFinish(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	orchestrator := NewOrchestrator(repo, &scriptedJudge{}, &stubCompleter{answer: "a"}, quietLogger())

	// A completer that cancels the run through the ROUTE's path — the
	// repository — the moment the first agent turn happens. That is exactly the
	// race the production predicate has to win.
	cancelling := &cancellingCompleter{repo: repo, orchestrator: orchestrator}
	orchestrator = NewOrchestrator(repo, &scriptedJudge{}, cancelling, quietLogger())
	cancelling.orchestrator = orchestrator

	orchestrator.Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	run := repo.run("1")
	if run.Status != RunStatusCancelled {
		t.Fatalf("status = %q, want cancelled — the worker overwrote the user's stop", run.Status)
	}
}

type cancellingCompleter struct {
	repo         *fakeRunRepo
	orchestrator *Orchestrator
	calls        int
}

func (c *cancellingCompleter) Complete(_ context.Context, _ predict.CompletionRequest) (string, error) {
	c.calls++
	if c.calls == 1 {
		_, _ = c.repo.CancelRun(context.Background(), "1", "1")
		c.orchestrator.Cancel(RunRef{ProjectID: "1", RunID: "1"})
	}
	return "an answer", nil
}

// A run whose dataset cannot be read errors the RUN, because there is nothing
// left to do. That is the other side of the case/run distinction.
func TestOrchestratorErrorsTheRunWhenTheDatasetCannotBeRead(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	repo.casesErr = errors.New("relation p_1.eval_dataset_cases does not exist")

	NewOrchestrator(repo, &scriptedJudge{}, &stubCompleter{}, quietLogger()).
		Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	run := repo.run("1")
	if run.Status != RunStatusErrored {
		t.Fatalf("status = %q, want errored", run.Status)
	}
	if run.Error == "" {
		t.Error("the run errored with no reason on the row")
	}
	if run.HeadlineScore != nil {
		t.Error("a run that scored nothing carries a headline")
	}
}

// A CLAIM only moves a `created` row, so a run that is already running (another
// replica has it) is left alone. Without this both would score every case and
// the upsert would be the only thing between them and a doubled bill.
func TestOrchestratorDoesNotClaimARunSomebodyElseHas(t *testing.T) {
	t.Parallel()

	repo, run := twoCaseRun()
	run.Status = RunStatusRunning
	repo.seedRun(run)

	NewOrchestrator(repo, &scriptedJudge{}, &stubCompleter{answer: "a"}, quietLogger()).
		Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	if got := repo.run("1"); got.Status != RunStatusRunning {
		t.Fatalf("status = %q, want the run left untouched", got.Status)
	}
	if len(repo.storedResults()) != 0 {
		t.Error("a run that was not claimed was scored anyway")
	}
}

// RESTART RE-QUEUE. The sweep asks the repository for orphans and enqueues
// what it gets, so a run whose worker died is picked up by the next process.
func TestOrchestratorSweepRequeuesOrphansAndPendingRuns(t *testing.T) {
	t.Parallel()

	repo := &sweepRepo{
		fakeRunRepo: newFakeRunRepo(),
		stale:       []RunRef{{ProjectID: "1", RunID: "7"}},
		pending:     []RunRef{{ProjectID: "1", RunID: "7"}, {ProjectID: "2", RunID: "8"}},
	}
	orchestrator := NewOrchestrator(repo, &scriptedJudge{}, &stubCompleter{}, quietLogger())
	orchestrator.sweep(context.Background())

	if repo.ttl != int(StaleRunTTL.Seconds()) {
		t.Errorf("the sweep asked for a TTL of %ds, want %ds", repo.ttl, int(StaleRunTTL.Seconds()))
	}
	queued := drain(orchestrator)
	if len(queued) != 2 {
		t.Fatalf("queued %d runs, want the two pending ones", len(queued))
	}
	if queued[0].RunID != "7" || queued[1].RunID != "8" {
		t.Errorf("queued %+v, want runs 7 and 8", queued)
	}
}

type sweepRepo struct {
	*fakeRunRepo
	stale   []RunRef
	pending []RunRef
	ttl     int
}

func (s *sweepRepo) RequeueStaleRuns(_ context.Context, olderThanSeconds int) ([]RunRef, error) {
	s.ttl = olderThanSeconds
	return s.stale, nil
}

func (s *sweepRepo) PendingRuns(context.Context, int) ([]RunRef, error) { return s.pending, nil }

func drain(o *Orchestrator) []RunRef {
	out := []RunRef{}
	for {
		select {
		case ref := <-o.queue:
			out = append(out, ref)
		case <-time.After(10 * time.Millisecond):
			return out
		}
	}
}

// A binding for a dimension the snapshot does not carry is stored as
// `skipped` with a reason, not dropped. A missing cell in a scorecard reads as
// a bug in the UI; a skipped one with a reason reads as what it is.
func TestOrchestratorStoresASkippedResultForAnUnknownBinding(t *testing.T) {
	t.Parallel()

	repo, run := twoCaseRun()
	run.Snapshot.Bindings = append(run.Snapshot.Bindings,
		SnapshotBinding{DimensionID: "99", Engine: EngineAI, Weight: 1})
	repo.seedRun(run)

	NewOrchestrator(repo, &scriptedJudge{}, &stubCompleter{answer: "a"}, quietLogger()).
		Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	skipped := 0
	for _, result := range repo.storedResults() {
		if result.DimensionID == "99" {
			skipped++
			if result.Status != ResultStatusSkipped {
				t.Errorf("status = %q, want skipped", result.Status)
			}
			if result.Verdict["error"] == nil {
				t.Error("a skipped result carries no reason")
			}
		}
	}
	if skipped != 2 {
		t.Fatalf("stored %d skipped results, want one per case", skipped)
	}
}

// A nil judge does not panic and does not score zero: every case is `error`
// naming the missing plane, and the run finishes so an operator can read it.
func TestOrchestratorWithoutAJudgeNamesTheMissingPlaneOnEveryCase(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	NewOrchestrator(repo, nil, &stubCompleter{answer: "a"}, quietLogger()).
		Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	if got := repo.run("1"); got.Status != RunStatusFinished || got.HeadlineScore != nil {
		t.Fatalf("run = %q headline %v, want finished with no headline", got.Status, got.HeadlineScore)
	}
	for _, result := range repo.storedResults() {
		if result.Status != ResultStatusError {
			t.Errorf("status = %q, want error", result.Status)
		}
		if result.NormalizedScore != nil {
			t.Error("a case with no judge was given a score")
		}
	}
}

// The target verdict is computed on the NATIVE scale from the binding's stored
// operator, so a scorecard can say "met" without re-deriving the threshold.
func TestOrchestratorRecordsTheTargetVerdict(t *testing.T) {
	t.Parallel()

	repo, run := twoCaseRun()
	target := 4.0
	run.Snapshot.Bindings[0].Target = &target
	run.Snapshot.Bindings[0].TargetOperator = ">="
	repo.seedRun(run)

	judge := &scriptedJudge{answers: []JudgeVerdict{{Score: 5}, {Score: 2}}}
	NewOrchestrator(repo, judge, &stubCompleter{answer: "a"}, quietLogger()).
		Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	met := map[string]bool{}
	for _, result := range repo.storedResults() {
		if result.TargetMet == nil {
			t.Fatalf("case %s: no target verdict was recorded", result.DatasetCaseID)
		}
		met[result.DatasetCaseID] = *result.TargetMet
	}
	if !met["11"] || met["12"] {
		t.Errorf("target verdicts = %v, want case 11 met and case 12 not", met)
	}
}
