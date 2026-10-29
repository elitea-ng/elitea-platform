package releasegate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// fakeStore keeps gates in memory with the store's rules: a settled gate's
// outcome is written once and an override once.
type fakeStore struct {
	suites   []SuiteConfig
	versions map[int64]int64
	releases map[int64]Release
	head     *Release
	gates    map[int64]*GateResult
	nextID   int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{versions: map[int64]int64{}, releases: map[int64]Release{}, gates: map[int64]*GateResult{}}
}

func (s *fakeStore) ListSuites(context.Context, int64, int64) ([]SuiteConfig, error) {
	return append([]SuiteConfig(nil), s.suites...), nil
}
func (s *fakeStore) PutSuite(_ context.Context, _ int64, suite SuiteConfig) (SuiteConfig, error) {
	s.suites = append(s.suites, suite)
	return suite, nil
}
func (s *fakeStore) DeleteSuite(context.Context, int64, int64, int64) (bool, error) {
	return false, nil
}
func (s *fakeStore) DatasetApplication(context.Context, int64, int64) (*int64, error) {
	return nil, nil
}
func (s *fakeStore) VersionApplication(_ context.Context, _ int64, versionID int64) (int64, error) {
	application, found := s.versions[versionID]
	if !found {
		return 0, ErrVersionNotFound
	}
	return application, nil
}
func (s *fakeStore) Release(_ context.Context, _, _, releaseID int64) (Release, error) {
	release, found := s.releases[releaseID]
	if !found {
		return Release{}, ErrReleaseNotFound
	}
	return release, nil
}
func (s *fakeStore) CurrentRelease(context.Context, int64, int64) (*Release, error) {
	return s.head, nil
}
func (s *fakeStore) CreateGate(_ context.Context, gate GateResult) (int64, error) {
	s.nextID++
	gate.ID = s.nextID
	if gate.State != StatePending {
		now := time.Now()
		gate.EvaluatedAt = &now
	}
	s.gates[gate.ID] = &gate
	return gate.ID, nil
}
func (s *fakeStore) GetGate(_ context.Context, _, gateID int64) (GateResult, error) {
	gate, found := s.gates[gateID]
	if !found {
		return GateResult{}, ErrGateNotFound
	}
	return *gate, nil
}
func (s *fakeStore) ListGates(context.Context, int64, int64, int) ([]GateResult, error) {
	return nil, nil
}
func (s *fakeStore) SettleGate(_ context.Context, _, gateID int64, state string, outcome []SuiteOutcome, failures []GateFailure) error {
	gate := s.gates[gateID]
	if gate.State != StatePending {
		return nil
	}
	now := time.Now()
	gate.State, gate.Outcome, gate.Failures, gate.EvaluatedAt = state, outcome, failures, &now
	return nil
}
func (s *fakeStore) OverrideGate(_ context.Context, _, gateID, actorID int64, reason string) error {
	gate := s.gates[gateID]
	if gate.State != StateFailed {
		return ErrGateNotFailed
	}
	if gate.Override != nil {
		return ErrAlreadyOverridden
	}
	gate.Override = &Override{ActorID: actorID, Reason: reason, At: time.Now()}
	return nil
}
func (s *fakeStore) PendingGates(context.Context, int) ([]GateKey, error) {
	var keys []GateKey
	for id, gate := range s.gates {
		if gate.State == StatePending {
			keys = append(keys, GateKey{TenantProjectID: 1, GateID: id})
		}
	}
	return keys, nil
}
func (s *fakeStore) DueScheduled(context.Context, time.Time, int) ([]ScheduledDue, error) {
	return nil, nil
}
func (s *fakeStore) ReferenceRuns(context.Context, int64, int64, int64) ([]GateRun, error) {
	return nil, nil
}
func (s *fakeStore) ScheduledGatesSince(context.Context, int64, int64, int64, time.Time, int) ([]int64, error) {
	var ids []int64
	for id := s.nextID; id > 0; id-- {
		if gate, found := s.gates[id]; found && gate.Kind == KindScheduled {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// fakeRuns stores runs started by fakeStarter and answers them back.
type fakeRuns struct {
	runs    map[string]*evaluation.Run
	results map[string][]evaluation.RunResult
	cases   map[string][]evaluation.DatasetCase
}

func newFakeRuns() *fakeRuns {
	return &fakeRuns{
		runs: map[string]*evaluation.Run{}, results: map[string][]evaluation.RunResult{},
		cases: map[string][]evaluation.DatasetCase{},
	}
}

func (r *fakeRuns) GetRun(_ context.Context, _, runID string) (evaluation.Run, error) {
	run, found := r.runs[runID]
	if !found {
		return evaluation.Run{}, apierr.NotFound("run not found")
	}
	return *run, nil
}
func (r *fakeRuns) ListResults(_ context.Context, _, runID string, _ evaluation.ResultPage) ([]evaluation.RunResult, int, error) {
	return r.results[runID], len(r.results[runID]), nil
}
func (r *fakeRuns) DatasetCases(_ context.Context, _, datasetID string) ([]evaluation.DatasetCase, error) {
	return r.cases[datasetID], nil
}
func (r *fakeRuns) AgentVersion(_ context.Context, _ string, versionID int) (evaluation.AgentVersion, error) {
	return evaluation.AgentVersion{ID: versionID, Instructions: "draft instructions", ModelName: "m"}, nil
}

func (r *fakeRuns) finish(runID string, status string, results ...evaluation.RunResult) {
	r.runs[runID].Status = status
	r.results[runID] = results
}

type fakeStarter struct {
	runs    *fakeRuns
	started []evaluation.RunStartInput
	agents  []*evaluation.SnapshotAgent
	refuse  error
}

func (s *fakeStarter) Start(_ context.Context, _ string, input evaluation.RunStartInput, _ *int, agent *evaluation.SnapshotAgent) (evaluation.Run, error) {
	if s.refuse != nil {
		return evaluation.Run{}, s.refuse
	}
	s.started = append(s.started, input)
	s.agents = append(s.agents, agent)
	id := strconv.Itoa(len(s.runs.runs) + 100)
	run := evaluation.Run{ID: id, DatasetID: input.DatasetID, ApplicationVersionID: input.ApplicationVersionID, Status: evaluation.RunStatusCreated}
	s.runs.runs[id] = &run
	return run, nil
}

func newTestGate(t *testing.T) (*Gate, *fakeStore, *fakeRuns, *fakeStarter) {
	t.Helper()
	store, runs := newFakeStore(), newFakeRuns()
	starter := &fakeStarter{runs: runs}
	gate, err := New(store, runs, starter, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	store.versions[30] = 3
	store.releases[5] = Release{ID: 5, ApplicationVersionID: 20, Instructions: "released instructions", ModelName: "m"}
	runs.cases["4"] = []evaluation.DatasetCase{{ID: "7", Input: "What is Go?"}, {ID: "8", Input: "And Rust?"}}
	return gate, store, runs, starter
}

func TestStartGateWithNoSuitePasses(t *testing.T) {
	gate, store, _, starter := newTestGate(t)
	release := int64(5)

	id, err := gate.StartGate(context.Background(), 1, 3, 30, &release)
	if err != nil {
		t.Fatal(err)
	}
	result, err := gate.Result(context.Background(), 1, id)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != StatePassed || len(result.Suites) != 0 || result.EvaluatedAt == nil {
		t.Fatalf("result = %+v", result)
	}
	if len(starter.started) != 0 || len(store.gates) != 1 {
		t.Fatalf("started %d runs, stored %d gates", len(starter.started), len(store.gates))
	}
}

func TestStartGateRunsRegressionAgainstBothAndSettlesOnRead(t *testing.T) {
	gate, store, runs, starter := newTestGate(t)
	store.suites = []SuiteConfig{{ApplicationID: 3, DatasetID: 4, Regression: true, Criteria: []Criterion{threshold("1", 50)}}}
	release := int64(5)

	id, err := gate.StartGate(context.Background(), 1, 3, 30, &release)
	if err != nil {
		t.Fatal(err)
	}
	if len(starter.started) != 2 {
		t.Fatalf("started %d runs, want the candidate and the baseline", len(starter.started))
	}
	if *starter.started[0].ApplicationVersionID != 30 || *starter.started[1].ApplicationVersionID != 20 {
		t.Fatalf("versions = %d, %d", *starter.started[0].ApplicationVersionID, *starter.started[1].ApplicationVersionID)
	}
	if starter.agents[1].Instructions != "released instructions" || starter.agents[0].Instructions != "draft instructions" {
		t.Fatalf("frozen agents = %+v, %+v", starter.agents[0], starter.agents[1])
	}

	result, err := gate.Result(context.Background(), 1, id)
	if err != nil || result.State != StatePending {
		t.Fatalf("before the runs finish: %s %v", result.State, err)
	}

	candidate, baseline := result.Runs[0], result.Runs[1]
	runs.finish(strconv.FormatInt(baseline.EvalRunID, 10), evaluation.RunStatusFinished,
		ok("7", "1", 100, met(true)), ok("8", "1", 100, met(true)))
	runs.finish(strconv.FormatInt(candidate.EvalRunID, 10), evaluation.RunStatusFinished,
		ok("7", "1", 100, met(true)), ok("8", "1", 0, met(false)))

	result, err = gate.Result(context.Background(), 1, id)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != StateFailed {
		t.Fatalf("state = %s", result.State)
	}
	var regression *GateFailure
	for index := range result.Failures {
		if result.Failures[index].Code == FailureRegression {
			regression = &result.Failures[index]
		}
	}
	if regression == nil || len(regression.Cases) != 1 || regression.Cases[0].CaseID != "8" || regression.Cases[0].Name != "And Rust?" {
		t.Fatalf("failures = %+v", result.Failures)
	}
	if !result.Blocks() {
		t.Fatal("a failed gate must block")
	}

	// The override: a reason is required, it is recorded once, and the
	// gate stays failed but no longer blocks.
	if err := gate.Override(context.Background(), 1, id, 9, "  "); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("empty reason: %v", err)
	}
	if err := gate.Override(context.Background(), 1, id, 9, "accepted: case 8 is obsolete"); err != nil {
		t.Fatal(err)
	}
	if err := gate.Override(context.Background(), 1, id, 9, "again"); !errors.Is(err, ErrAlreadyOverridden) {
		t.Fatalf("second override: %v", err)
	}
	result, _ = gate.Result(context.Background(), 1, id)
	if result.State != StateFailed || result.Override == nil || result.Override.ActorID != 9 || result.Blocks() {
		t.Fatalf("after override: %+v", result)
	}
}

func TestStartGateErroredRunFailsTheGate(t *testing.T) {
	gate, store, runs, _ := newTestGate(t)
	store.suites = []SuiteConfig{{ApplicationID: 3, DatasetID: 4, Criteria: []Criterion{threshold("1", 50)}}}

	id, err := gate.StartGate(context.Background(), 1, 3, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := gate.Result(context.Background(), 1, id)
	if len(result.Runs) != 1 {
		t.Fatalf("runs = %+v, want the candidate only (no release)", result.Runs)
	}
	runs.runs[strconv.FormatInt(result.Runs[0].EvalRunID, 10)].Status = evaluation.RunStatusErrored

	result, err = gate.Result(context.Background(), 1, id)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != StateFailed || result.Failures[0].Code != FailureRunErrored {
		t.Fatalf("result = %s %+v", result.State, result.Failures)
	}
	if err := gate.Override(context.Background(), 1, id, 0, "reason"); !errors.Is(err, ErrInvalidActor) {
		t.Fatalf("override with no actor: %v", err)
	}
}

func TestStartGateWithAnUnrunnableSuiteFailsAtOnce(t *testing.T) {
	gate, store, _, starter := newTestGate(t)
	store.suites = []SuiteConfig{
		{ApplicationID: 3, DatasetID: 4, Criteria: []Criterion{threshold("1", 50)}},
		{ApplicationID: 3, DatasetID: 6, Criteria: []Criterion{threshold("1", 50)}, DatasetMissing: true},
	}

	id, err := gate.StartGate(context.Background(), 1, 3, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(starter.started) != 0 {
		t.Fatalf("started %d runs; the preflight must refuse before any run", len(starter.started))
	}
	result, _ := gate.Result(context.Background(), 1, id)
	if result.State != StateFailed || len(result.Failures) != 1 ||
		result.Failures[0].Code != FailureSuiteNotRunnable || result.Failures[0].DatasetID != 6 {
		t.Fatalf("result = %s %+v", result.State, result.Failures)
	}

	// A refusal of the run start (a dimension the ai engine cannot score)
	// fails the gate the same way.
	store.suites = store.suites[:1]
	starter.refuse = apierr.NotImplemented("dimension x cannot be scored")
	id, err = gate.StartGate(context.Background(), 1, 3, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, _ = gate.Result(context.Background(), 1, id)
	if result.State != StateFailed || result.Failures[0].Code != FailureSuiteNotRunnable {
		t.Fatalf("refused start: %s %+v", result.State, result.Failures)
	}
}

func TestStartGateRefusesAVersionOfAnotherAgent(t *testing.T) {
	gate, store, _, _ := newTestGate(t)
	store.versions[31] = 99
	if _, err := gate.StartGate(context.Background(), 1, 3, 31, nil); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("err = %v", err)
	}
	release := int64(77)
	if _, err := gate.StartGate(context.Background(), 1, 3, 30, &release); !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestRegressedSinceAnswersAFailedScheduledGate(t *testing.T) {
	gate, store, runs, _ := newTestGate(t)
	store.suites = []SuiteConfig{{ApplicationID: 3, DatasetID: 4, Criteria: []Criterion{threshold("1", 50)}}}
	head := store.releases[5]
	store.head = &head

	none, err := gate.RegressedSince(context.Background(), 1, 3, time.Time{})
	if err != nil || none != nil {
		t.Fatalf("no scheduled gate: %v %v", none, err)
	}

	id, err := gate.StartScheduled(context.Background(), 1, 3, nil)
	if err != nil || id == 0 {
		t.Fatalf("StartScheduled = %d, %v", id, err)
	}
	stored := store.gates[id]
	if stored.Kind != KindScheduled || stored.CandidateVersionID != 20 {
		t.Fatalf("scheduled gate = %+v", stored)
	}
	runs.finish(strconv.FormatInt(stored.Runs[0].EvalRunID, 10), evaluation.RunStatusFinished, ok("7", "1", 10, nil))

	// The routine's pass settles the gate without a reader.
	pass, err := gate.Pass(context.Background())
	if err != nil || pass.Settled != 1 {
		t.Fatalf("pass = %+v, %v", pass, err)
	}
	regressed, err := gate.RegressedSince(context.Background(), 1, 3, time.Time{})
	if err != nil || regressed == nil || regressed.ID != id || regressed.Failures[0].Code != FailureThresholdNotMet {
		t.Fatalf("RegressedSince = %+v, %v", regressed, err)
	}
}

func TestScheduledGateWithARefusedUserRunsNothing(t *testing.T) {
	gate, store, _, starter := newTestGate(t)
	store.suites = []SuiteConfig{{ApplicationID: 3, DatasetID: 4, Criteria: []Criterion{threshold("1", 50)}}}
	head := store.releases[5]
	store.head = &head
	gate.WithActorCheck(func(context.Context, int64, int64) (bool, error) { return false, nil })
	actor := int64(11)

	id, err := gate.StartScheduled(context.Background(), 1, 3, &actor)
	if err != nil {
		t.Fatal(err)
	}
	if len(starter.started) != 0 {
		t.Fatalf("started %d runs for a refused user", len(starter.started))
	}
	stored := store.gates[id]
	if stored.State != StateFailed || stored.Failures[0].Code != FailureScheduleActorRefused {
		t.Fatalf("gate = %+v", stored)
	}
	// No evaluation ran, so it is not a regression.
	if regressed, _ := gate.RegressedSince(context.Background(), 1, 3, time.Time{}); regressed != nil {
		t.Fatalf("RegressedSince = %+v", regressed)
	}
}
