package evaluation

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAgentRunner is the dispatcher a runtime_plane run drives, scripted per
// case. It records each request so a test can read what the orchestrator asked
// for, and it calls Admitted the way the real runner does.
type fakeAgentRunner struct {
	mu       sync.Mutex
	requests []AgentRunRequest
	results  map[string]AgentRunResult
	errs     map[string]error
}

func (f *fakeAgentRunner) RunCase(ctx context.Context, request AgentRunRequest) (AgentRunResult, error) {
	f.mu.Lock()
	f.requests = append(f.requests, request)
	result, scripted := f.results[request.CaseID]
	err := f.errs[request.CaseID]
	f.mu.Unlock()
	if !scripted {
		result = AgentRunResult{Outcome: AgentOutcomeCompleted, Output: "agent answer"}
	}
	if result.ExecutionID == "" {
		result.ExecutionID = "exec-" + request.CaseID
	}
	if request.Admitted != nil {
		request.Admitted(ctx, result.ExecutionID, "conv-"+request.CaseID)
	}
	return result, err
}

// recordingRepo adds the CaseRunRecorder the repository implements.
type recordingRepo struct {
	*fakeRunRepo
	mu       sync.Mutex
	recorded [][3]string
}

func (r *recordingRepo) RecordCaseRun(_ context.Context, _, runID, caseID, executionID, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorded = append(r.recorded, [3]string{runID, caseID, executionID})
	return nil
}

func runtimeRun(limits *RunLimits) (*recordingRepo, RunRef) {
	repo, run := twoCaseRun()
	app := 4
	run.ApplicationID = &app
	by := 7
	run.CreatedBy = &by
	run.ExecutionMode = ExecutionModeRuntimePlane
	run.Snapshot.Limits = limits
	repo.seedRun(run)
	return &recordingRepo{fakeRunRepo: repo}, RunRef{ProjectID: "1", RunID: "1"}
}

func executeRun(repo RunRepository, judge Judge, completer Completer, runner AgentRunner, ref RunRef) {
	orchestrator := NewOrchestrator(repo, judge, completer, quietLogger())
	if runner != nil {
		orchestrator.WithAgentRunner(runner)
	}
	orchestrator.Execute(context.Background(), ref)
}

func trajectoryOf(t *testing.T, result RunResult) map[string]any {
	t.Helper()
	trajectory, ok := result.Evidence["trajectory"].(map[string]any)
	if !ok {
		t.Fatalf("case %s has no trajectory in its evidence: %v", result.DatasetCaseID, result.Evidence)
	}
	return trajectory
}

// The runtime_plane mode runs each case through the AgentRunner and NOT through
// the blocking completer, and the judge scores the run's final output.
func TestRuntimePlaneRunsCasesThroughTheRunnerNotTheCompleter(t *testing.T) {
	t.Parallel()

	repo, ref := runtimeRun(nil)
	runner := &fakeAgentRunner{results: map[string]AgentRunResult{
		"11": {Outcome: AgentOutcomeCompleted, Output: "Go is a language", Steps: []AgentStep{
			{Seq: 1, Kind: StepKindModelCall, DurationMs: 40},
			{Seq: 2, Kind: StepKindToolCall, Name: "search", ToolkitID: "wiki", DurationMs: 90, Output: "result text"},
			{Seq: 3, Kind: StepKindModelCall, DurationMs: 30},
		}},
	}}
	completer := &stubCompleter{answer: "must not be used"}
	judge := &scriptedJudge{answers: []JudgeVerdict{{Score: 5}, {Score: 3}}}

	executeRun(repo, judge, completer, runner, ref)

	run := repo.run("1")
	if run.Status != RunStatusFinished {
		t.Fatalf("status = %q (error %q), want finished", run.Status, run.Error)
	}
	if len(completer.requests) != 0 {
		t.Fatalf("the blocking completer was called %d times in a runtime_plane run", len(completer.requests))
	}
	if len(runner.requests) != 2 {
		t.Fatalf("the runner ran %d cases, want 2", len(runner.requests))
	}
	first := runner.requests[0]
	if first.Input != "what is Go?" || first.ApplicationID != 4 || first.VersionID != 9 || first.ActorUserID != 7 {
		t.Errorf("the first case was dispatched as %+v", first)
	}
	if first.TimeLimit != DefaultCaseTimeLimit || first.StepLimit != 0 || !first.KeepToolOutputs {
		t.Errorf("a run with no suite got limits %v / %d / %v", first.TimeLimit, first.StepLimit, first.KeepToolOutputs)
	}
	if len(repo.recorded) != 2 || repo.recorded[0][2] != "exec-11" {
		t.Errorf("the cases' executions were recorded as %v", repo.recorded)
	}

	for _, result := range repo.storedResults() {
		if result.DatasetCaseID != "11" {
			continue
		}
		if result.Evidence["output"] != "Go is a language" {
			t.Errorf("the judged answer was %v, want the run's final output", result.Evidence["output"])
		}
		if result.Evidence["execution_id"] != "exec-11" || result.Evidence["execution_mode"] != ExecutionModeRuntimePlane {
			t.Errorf("the evidence does not name the agent run: %v", result.Evidence)
		}
		trajectory := trajectoryOf(t, result)
		if trajectory["steps"] != 3 || trajectory["tool_calls"] != 1 || trajectory["model_calls"] != 2 {
			t.Errorf("trajectory counts = %v", trajectory)
		}
	}
}

// predict_blocking stays the default: a run row that says so never touches the
// runner, even when one is composed.
func TestPredictBlockingRunIgnoresTheRunner(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	runner := &fakeAgentRunner{}
	completer := &stubCompleter{answer: "an answer"}

	executeRun(repo, &scriptedJudge{}, completer, runner, RunRef{ProjectID: "1", RunID: "1"})

	if len(runner.requests) != 0 || len(completer.requests) != 2 {
		t.Fatalf("runner calls = %d, completer calls = %d; want 0 and 2", len(runner.requests), len(completer.requests))
	}
}

func TestRuntimePlaneWithoutARunnerErrorsTheRunWithAReason(t *testing.T) {
	t.Parallel()

	repo, ref := runtimeRun(nil)
	executeRun(repo, &scriptedJudge{}, &stubCompleter{answer: "x"}, nil, ref)

	run := repo.run("1")
	if run.Status != RunStatusErrored || !strings.Contains(run.Error, "no agent runtime") {
		t.Fatalf("status = %q, error = %q; want errored naming the missing runtime", run.Status, run.Error)
	}
}

// The suite's controls reach the runner as the request's limits.
func TestRuntimePlaneForwardsTheSuiteControls(t *testing.T) {
	t.Parallel()

	steps, seconds, drop := 12, 90, false
	repo, ref := runtimeRun(&RunLimits{
		OnRunLimit: RunLimitActionStop, ExecutionMode: ExecutionModeRuntimePlane,
		StepLimitPerCase: &steps, TimeLimitPerCaseSeconds: &seconds, StoreToolOutputs: &drop,
	})
	runner := &fakeAgentRunner{}

	executeRun(repo, &scriptedJudge{}, &stubCompleter{}, runner, ref)

	request := runner.requests[0]
	if request.StepLimit != 12 || request.TimeLimit != 90*time.Second || request.KeepToolOutputs {
		t.Fatalf("request limits = %d / %v / keep=%v, want 12 / 90s / false",
			request.StepLimit, request.TimeLimit, request.KeepToolOutputs)
	}
}

// A case that reaches its time limit stores a timeout result: an error row
// flagged timed-out, never a score, and the run carries on with the next case.
func TestRuntimePlaneTimeoutStoresATimeoutResult(t *testing.T) {
	t.Parallel()

	repo, ref := runtimeRun(nil)
	runner := &fakeAgentRunner{results: map[string]AgentRunResult{
		"11": {Outcome: AgentOutcomeTimedOut, Steps: []AgentStep{{Seq: 1, Kind: StepKindModelCall}}},
	}}
	judge := &scriptedJudge{answers: []JudgeVerdict{{Score: 4}}}

	executeRun(repo, judge, &stubCompleter{}, runner, ref)

	if run := repo.run("1"); run.Status != RunStatusFinished {
		t.Fatalf("status = %q, want finished: one slow case must not error the run", run.Status)
	}
	var timedOut, scored int
	for _, result := range repo.storedResults() {
		switch result.DatasetCaseID {
		case "11":
			timedOut++
			if result.Status != ResultStatusError || result.Evidence["agent_timed_out"] != true {
				t.Errorf("the slow case stored status %q with evidence %v", result.Status, result.Evidence)
			}
			if result.NativeScore != nil {
				t.Error("a timed-out case was scored")
			}
		case "12":
			scored++
			if result.Status != ResultStatusOK {
				t.Errorf("the next case stored status %q", result.Status)
			}
		}
	}
	if timedOut != 1 || scored != 1 || judge.calls != 1 {
		t.Fatalf("timed out %d, scored %d, judge calls %d", timedOut, scored, judge.calls)
	}
}

func TestRuntimePlaneStepLimitStoresAnErrorNamingTheLimit(t *testing.T) {
	t.Parallel()

	steps := 2
	repo, ref := runtimeRun(&RunLimits{
		OnRunLimit: RunLimitActionStop, ExecutionMode: ExecutionModeRuntimePlane, StepLimitPerCase: &steps,
	})
	runner := &fakeAgentRunner{results: map[string]AgentRunResult{
		"11": {Outcome: AgentOutcomeStepLimit},
	}}

	executeRun(repo, &scriptedJudge{}, &stubCompleter{}, runner, ref)

	for _, result := range repo.storedResults() {
		if result.DatasetCaseID != "11" {
			continue
		}
		message, _ := result.Evidence["error"].(string)
		if result.Status != ResultStatusError || result.Evidence["agent_step_limit"] != true ||
			!strings.Contains(message, "step limit") {
			t.Fatalf("status %q, evidence %v", result.Status, result.Evidence)
		}
	}
}

// A paused run, a failed run and a runner error are all case errors with the
// reason on the row, and none of them is scored 0.
func TestRuntimePlaneFailuresAreCaseErrors(t *testing.T) {
	t.Parallel()

	repo, ref := runtimeRun(nil)
	runner := &fakeAgentRunner{
		results: map[string]AgentRunResult{
			"11": {Outcome: AgentOutcomePaused, Failure: "the agent paused for human approval"},
		},
		errs: map[string]error{"12": errors.New("the agent could not be started")},
	}

	executeRun(repo, &scriptedJudge{}, &stubCompleter{}, runner, ref)

	if run := repo.run("1"); run.Status != RunStatusFinished {
		t.Fatalf("status = %q, want finished", run.Status)
	}
	reasons := map[string]string{}
	for _, result := range repo.storedResults() {
		if result.Status != ResultStatusError {
			t.Errorf("case %s stored status %q, want error", result.DatasetCaseID, result.Status)
		}
		reasons[result.DatasetCaseID], _ = result.Evidence["error"].(string)
	}
	if !strings.Contains(reasons["11"], "paused for human approval") || !strings.Contains(reasons["12"], "could not be started") {
		t.Fatalf("reasons = %v", reasons)
	}
}

func TestBuildTrajectoryCountsToolsErrorsAndRetries(t *testing.T) {
	t.Parallel()

	steps := []AgentStep{
		{Seq: 1, Kind: StepKindModelCall},
		{Seq: 2, Kind: StepKindToolCall, Name: "fetch", ToolkitID: "web", InputsDigest: "a", Error: "503"},
		{Seq: 3, Kind: StepKindToolCall, Name: "fetch", ToolkitID: "web", InputsDigest: "a", Output: "ok body"},
		{Seq: 4, Kind: StepKindToolCall, Name: "fetch", ToolkitID: "web", InputsDigest: "b"},
		{Seq: 5, Kind: StepKindRetrieval, Name: "search_index", ToolkitID: "kb", InputsDigest: "c"},
		{Seq: 6, Kind: "approval"},
		{Seq: 7, Kind: "output"},
	}

	got := BuildTrajectory(steps, true)

	// Approvals and the final output are evidence, not work the limit counts.
	if got["steps"] != 5 || got["model_calls"] != 1 || got["tool_calls"] != 4 {
		t.Errorf("steps/model/tool = %v / %v / %v", got["steps"], got["model_calls"], got["tool_calls"])
	}
	if got["tool_errors"] != 1 {
		t.Errorf("tool_errors = %v, want 1", got["tool_errors"])
	}
	// Step 3 repeats the failed call with the same arguments; step 4 has
	// different arguments and is a new call, not a retry.
	if got["retries"] != 1 {
		t.Errorf("retries = %v, want 1", got["retries"])
	}
	tools := got["tools"].([]map[string]any)
	if len(tools) != 2 || tools[0]["name"] != "fetch" || tools[0]["calls"] != 3 || tools[0]["errors"] != 1 {
		t.Errorf("tools = %v", tools)
	}
	items := got["items"].([]map[string]any)
	if items[2]["output"] != "ok body" {
		t.Errorf("a kept tool output is missing: %v", items[2])
	}
}

func TestBuildTrajectoryDropsToolOutputsWhenNotStored(t *testing.T) {
	t.Parallel()

	got := BuildTrajectory([]AgentStep{{Seq: 1, Kind: StepKindToolCall, Name: "t", Output: "secret body"}}, false)

	items := got["items"].([]map[string]any)
	if _, kept := items[0]["output"]; kept {
		t.Fatalf("the tool output was kept although the suite drops them: %v", items[0])
	}
	if got["tool_calls"] != 1 {
		t.Errorf("the counts must survive the drop: %v", got)
	}
}

func TestBuildTrajectoryBoundsTheItemList(t *testing.T) {
	t.Parallel()

	steps := make([]AgentStep, maxTrajectoryItems+10)
	for i := range steps {
		steps[i] = AgentStep{Seq: i + 1, Kind: StepKindModelCall}
	}

	got := BuildTrajectory(steps, true)

	if got["steps"] != len(steps) || len(got["items"].([]map[string]any)) != maxTrajectoryItems || got["truncated"] != true {
		t.Fatalf("steps %v, items %d, truncated %v", got["steps"], len(got["items"].([]map[string]any)), got["truncated"])
	}
}

func TestSuiteValidationRefusesRuntimeControlsOnPredictBlocking(t *testing.T) {
	t.Parallel()

	steps := 5
	write := SuiteWrite{Name: "s", RunLimits: RunLimits{StepLimitPerCase: &steps}}
	write.Normalize()
	if err := write.Validate(); err == nil || !strings.Contains(err.Error(), "runtime_plane") {
		t.Fatalf("a step limit on a predict_blocking suite was accepted: %v", err)
	}

	write.ExecutionMode = ExecutionModeRuntimePlane
	write.Normalize()
	if err := write.Validate(); err != nil {
		t.Fatalf("a step limit on a runtime_plane suite was refused: %v", err)
	}

	zero := 0
	write.StepLimitPerCase = &zero
	if err := write.Validate(); err == nil {
		t.Fatal("a step limit of 0 was accepted")
	}

	write.StepLimitPerCase = nil
	write.ExecutionMode = "chaos"
	if err := write.Validate(); err == nil {
		t.Fatal("an unknown execution mode was accepted")
	}
}

func TestSuiteDefaultsToPredictBlockingAndKeepsToolOutputs(t *testing.T) {
	t.Parallel()

	write := SuiteWrite{Name: "s"}
	write.Normalize()

	if write.ExecutionMode != ExecutionModePredictBlocking || !write.KeepsToolOutputs() {
		t.Fatalf("defaults = %q / keep=%v", write.ExecutionMode, write.KeepsToolOutputs())
	}
}

func TestResolveExecutionMode(t *testing.T) {
	t.Parallel()

	suiteMode := func(mode string) RunSnapshot { return RunSnapshot{Limits: &RunLimits{ExecutionMode: mode}} }
	cases := []struct {
		name     string
		input    RunStartInput
		snapshot RunSnapshot
		agent    *SnapshotAgent
		want     string
		refused  bool
		note     bool
	}{
		{name: "nothing set keeps predict_blocking", want: ExecutionModePredictBlocking},
		{name: "suite mode", snapshot: suiteMode(ExecutionModeRuntimePlane), want: ExecutionModeRuntimePlane},
		{name: "the start overrides the suite", input: RunStartInput{ExecutionMode: ExecutionModePredictBlocking},
			snapshot: suiteMode(ExecutionModeRuntimePlane), want: ExecutionModePredictBlocking},
		{name: "a frozen agent refuses an explicit runtime_plane", input: RunStartInput{ExecutionMode: ExecutionModeRuntimePlane},
			agent: &SnapshotAgent{}, refused: true},
		{name: "a frozen agent falls back from the suite's runtime_plane", snapshot: suiteMode(ExecutionModeRuntimePlane),
			agent: &SnapshotAgent{}, want: ExecutionModePredictBlocking, note: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, note, err := resolveExecutionMode(tc.input, tc.snapshot, tc.agent)
			if (err != nil) != tc.refused || got != tc.want || (note != "") != tc.note {
				t.Fatalf("mode %q note %q err %v, want %q note=%v refused=%v", got, note, err, tc.want, tc.note, tc.refused)
			}
		})
	}
}

func TestFreezeExecutionModeDropsRuntimeLimitsOfAnOverriddenSuite(t *testing.T) {
	t.Parallel()

	steps := 5
	snapshot := RunSnapshot{Limits: &RunLimits{ExecutionMode: ExecutionModeRuntimePlane, StepLimitPerCase: &steps}}

	freezeExecutionMode(&snapshot, ExecutionModePredictBlocking)

	if snapshot.Limits.StepLimitPerCase != nil || snapshot.Limits.ExecutionMode != ExecutionModePredictBlocking {
		t.Fatalf("limits = %+v", snapshot.Limits)
	}

	bare := RunSnapshot{}
	freezeExecutionMode(&bare, ExecutionModePredictBlocking)
	if bare.Limits != nil {
		t.Fatal("a plain predict_blocking run without a suite gained a limits block")
	}
	freezeExecutionMode(&bare, ExecutionModeRuntimePlane)
	if bare.Limits == nil || !bare.Limits.IsRuntimePlane() || !bare.Limits.KeepsToolOutputs() {
		t.Fatalf("a runtime_plane run without a suite has limits %+v", bare.Limits)
	}
}

// failingRepo refuses the results whose evidence names the marker, as a jsonb
// column refuses a NUL byte.
type failingRepo struct {
	*recordingRepo
	badOutput string
	failAll   bool
}

func (f *failingRepo) SaveResult(ctx context.Context, project string, result RunResult) error {
	if f.failAll || (f.badOutput != "" && result.Evidence["output"] == f.badOutput) {
		return errors.New("invalid byte sequence")
	}
	return f.recordingRepo.SaveResult(ctx, project, result)
}

func twoBindings(repo *recordingRepo) {
	run := repo.run("1")
	run.Snapshot.Dimensions["4"] = run.Snapshot.Dimensions["3"]
	run.Snapshot.Bindings = append(run.Snapshot.Bindings, SnapshotBinding{DimensionID: "4", Engine: EngineAI, Weight: 1, OrderIndex: 1})
	repo.seedRun(run)
}

// A case's trajectory is stored once, and the other rows of the case point at
// it instead of repeating it per dimension.
func TestRuntimePlaneStoresTheTrajectoryOncePerCase(t *testing.T) {
	t.Parallel()

	repo, ref := runtimeRun(nil)
	twoBindings(repo)
	runner := &fakeAgentRunner{results: map[string]AgentRunResult{
		"11": {Outcome: AgentOutcomeCompleted, Output: "a", Steps: []AgentStep{{Seq: 1, Kind: StepKindModelCall}}},
	}}

	executeRun(repo, &scriptedJudge{}, &stubCompleter{}, runner, ref)

	var holders, refs int
	for _, result := range repo.storedResults() {
		if result.DatasetCaseID != "11" {
			continue
		}
		if _, ok := result.Evidence["trajectory"]; ok {
			holders++
		}
		if result.Evidence["trajectory_ref"] != nil {
			refs++
		}
	}
	if holders != 1 || refs != 1 {
		t.Fatalf("case 11 has %d trajectories and %d references, want 1 and 1", holders, refs)
	}
	// Case 12 recorded no steps: no trajectory at all, not an empty one.
	for _, result := range repo.storedResults() {
		if result.DatasetCaseID == "12" && (result.Evidence["trajectory"] != nil || result.Evidence["trajectory_ref"] != nil) {
			t.Fatalf("a case with no step evidence carries a trajectory: %v", result.Evidence)
		}
	}
}

// A result the store refuses is replaced by a visible error row for the same
// cell; when even that is refused the run is errored with a count. Never only a
// log line.
func TestRuntimePlaneStoreFailureIsVisible(t *testing.T) {
	t.Parallel()

	repo, ref := runtimeRun(nil)
	failing := &failingRepo{recordingRepo: repo, failAll: true}

	executeRun(failing, &scriptedJudge{}, &stubCompleter{}, &fakeAgentRunner{}, ref)

	if run := repo.run("1"); run.Status != RunStatusErrored || !strings.Contains(run.Error, "could not be stored") {
		t.Fatalf("status %q error %q, want errored naming the lost results", run.Status, run.Error)
	}

	// One bad cell: the fallback error row is stored and the run finishes.
	repo2, ref2 := runtimeRun(nil)
	partial := &failingRepo{recordingRepo: repo2, badOutput: "bad"}
	runner := &fakeAgentRunner{results: map[string]AgentRunResult{"11": {Outcome: AgentOutcomeCompleted, Output: "bad"}}}
	executeRun(partial, &scriptedJudge{}, &stubCompleter{}, runner, ref2)
	var fallback bool
	for _, result := range repo2.storedResults() {
		if result.DatasetCaseID == "11" && result.Status == ResultStatusError && result.Evidence["store_failed"] == true {
			fallback = true
		}
	}
	if !fallback {
		t.Fatalf("the refused result left no error row: %v", repo2.storedResults())
	}
}

func TestCleanTextMakesAnAnswerStorable(t *testing.T) {
	t.Parallel()

	if got := cleanText("a\x00b\xffc"); got != "abc"+"" && got != "ab�c" {
		t.Fatalf("cleanText = %q", got)
	}
}

func TestRuntimePlaneWithoutAnAuthorErrorsTheRunOnce(t *testing.T) {
	t.Parallel()

	repo, ref := runtimeRun(nil)
	run := repo.run("1")
	run.CreatedBy = nil
	repo.seedRun(run)
	runner := &fakeAgentRunner{}

	executeRun(repo, &scriptedJudge{}, &stubCompleter{}, runner, ref)

	if got := repo.run("1"); got.Status != RunStatusErrored || !strings.Contains(got.Error, "author") {
		t.Fatalf("status %q error %q", got.Status, got.Error)
	}
	if len(runner.requests) != 0 {
		t.Fatalf("the runner was called %d times for a run with no author", len(runner.requests))
	}
}
