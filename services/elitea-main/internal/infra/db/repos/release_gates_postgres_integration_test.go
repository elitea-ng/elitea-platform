package repos

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/releasegate"
)

// Shared 0203 (ADR-0040 decision 4, issue #1497): the release gate over a
// real migrated database. The gate starts its runs through the real run
// starter into the tenant's eval_runs, settles once the runs are terminal,
// lists a regressed case, records an override once, and the scheduled
// re-evaluation finds a due agent. CI-only: skipped without the
// integration database.

type releaseGateFixture struct {
	pool      *pgxpool.Pool
	store     *ReleaseGates
	runs      *EvalRunsRepo
	gate      *releasegate.Gate
	draft     int64
	released  int64
	releaseID int64
	datasetA  int64
	datasetB  int64
	dimension string
	cases     []string
}

func newReleaseGateFixture(t *testing.T) releaseGateFixture {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	fixture := releaseGateFixture{pool: pool, runs: NewEvalRunsRepo(pool)}

	if _, err := pool.Exec(ctx, `
		ALTER TABLE p_1.application_versions
			ADD COLUMN IF NOT EXISTS application_id INTEGER,
			ADD COLUMN IF NOT EXISTS instructions TEXT`); err != nil {
		t.Fatalf("widen application_versions: %v", err)
	}
	for _, target := range []struct {
		id           *int64
		instructions string
	}{{&fixture.draft, "draft instructions"}, {&fixture.released, "released instructions"}} {
		if err := pool.QueryRow(ctx, `
			INSERT INTO p_1.application_versions (application_id, instructions, llm_settings)
			VALUES (3, $1, '{"model_name":"qwen"}'::jsonb) RETURNING id`, target.instructions).Scan(target.id); err != nil {
			t.Fatalf("seed version: %v", err)
		}
	}
	for _, target := range []*int64{&fixture.datasetA, &fixture.datasetB} {
		if err := pool.QueryRow(ctx, `
			INSERT INTO p_1.eval_datasets (name, application_id) VALUES ('Support', 3) RETURNING id`).Scan(target); err != nil {
			t.Fatalf("seed dataset: %v", err)
		}
	}
	for index, input := range []string{"What is Go?", "And Rust?"} {
		var id int
		if err := pool.QueryRow(ctx, `
			INSERT INTO p_1.eval_dataset_cases (dataset_id, input, order_index) VALUES ($1, $2, $3) RETURNING id`,
			fixture.datasetA, input, index).Scan(&id); err != nil {
			t.Fatalf("seed case: %v", err)
		}
		fixture.cases = append(fixture.cases, strconv.Itoa(id))
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.eval_dataset_cases (dataset_id, input) VALUES ($1, 'Another')`, fixture.datasetB); err != nil {
		t.Fatalf("seed case: %v", err)
	}
	var dimension int
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.eval_dimensions (name, description, allowed_engines, scale_type, scale_min, scale_max,
		                                 polarity, default_target, default_target_operator)
		VALUES ('Helpfulness', 'Does it help?', ARRAY['ai'], 'ordinal', 1, 5, 'higher_better', 4, '>=')
		RETURNING id`).Scan(&dimension); err != nil {
		t.Fatalf("seed dimension: %v", err)
	}
	fixture.dimension = strconv.Itoa(dimension)

	if err := pool.QueryRow(ctx, `
		INSERT INTO elitea_lifecycle.application_releases (
			tenant_project_id, application_id, application_version_id, name, definition_digest, definition)
		VALUES (1, 3, $1, 'v1', sha256('v1'::bytea),
		        '{"instructions":"released instructions","llm_settings":{"model_name":"qwen"}}'::jsonb)
		RETURNING id`, fixture.released).Scan(&fixture.releaseID); err != nil {
		t.Fatalf("seed release: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO elitea_lifecycle.application_release_heads (tenant_project_id, application_id, release_id, cause)
		VALUES (1, 3, $1, 'publish')`, fixture.releaseID); err != nil {
		t.Fatalf("seed release head: %v", err)
	}

	store, err := NewReleaseGates(pool)
	if err != nil {
		t.Fatal(err)
	}
	fixture.store = store
	starter := evaluation.NewRunStarter(fixture.runs, NewEvalDimensionsRepo(pool), noopEnqueuer{})
	fixture.gate, err = releasegate.New(store, fixture.runs, starter, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

// finish claims a run, stores one result per case and finishes it.
func (f releaseGateFixture) finish(t *testing.T, runID int64, scores map[string]float64) {
	t.Helper()
	ctx := t.Context()
	id := strconv.FormatInt(runID, 10)
	if _, err := f.runs.ClaimRun(ctx, "1", id); err != nil {
		t.Fatalf("claim run %s: %v", id, err)
	}
	for caseID, native := range scores {
		normalized, _ := evaluation.NormalizeScore(native, evaluation.ScaleOrdinal, 1, 5, evaluation.PolarityHigherBetter)
		targetMet := native >= 4
		if err := f.runs.SaveResult(ctx, "1", evaluation.RunResult{
			RunID: id, DatasetCaseID: caseID, DimensionID: f.dimension, Status: evaluation.ResultStatusOK,
			NativeScore: &native, NormalizedScore: &normalized, TargetMet: &targetMet,
		}); err != nil {
			t.Fatalf("save result: %v", err)
		}
	}
	if err := f.runs.FinishRun(ctx, "1", id, evaluation.RunStatusFinished, nil, ""); err != nil {
		t.Fatalf("finish run: %v", err)
	}
}

func TestReleaseGateStorePostgres(t *testing.T) {
	fixture := newReleaseGateFixture(t)
	ctx := context.Background()
	gate, store := fixture.gate, fixture.store
	actor := int64(11)

	// Suite config: one regression suite per agent.
	threshold := 50.0
	regression, err := gate.PutSuite(ctx, 1, releasegate.SuiteConfig{
		ApplicationID: 3, DatasetID: fixture.datasetA, Regression: true, UpdatedBy: &actor,
		Criteria: []releasegate.Criterion{{DimensionID: fixture.dimension, Kind: releasegate.CriterionThreshold, MinScore: &threshold}},
	})
	if err != nil {
		t.Fatalf("put regression suite: %v", err)
	}
	if !regression.Regression || regression.UpdatedAt.IsZero() {
		t.Fatalf("stored suite = %+v", regression)
	}
	second := releasegate.SuiteConfig{
		ApplicationID: 3, DatasetID: fixture.datasetB, Regression: true,
		Criteria: []releasegate.Criterion{{DimensionID: fixture.dimension, Kind: releasegate.CriterionThreshold, MinScore: &threshold}},
	}
	if _, err := store.PutSuite(ctx, 1, second); !errors.Is(err, releasegate.ErrRegressionTaken) {
		t.Fatalf("a second regression suite: %v", err)
	}
	if _, err := gate.PutSuite(ctx, 1, releasegate.SuiteConfig{ApplicationID: 4, DatasetID: fixture.datasetA,
		Criteria: second.Criteria}); !errors.Is(err, releasegate.ErrDatasetNotFound) {
		t.Fatalf("a dataset of another agent: %v", err)
	}

	// The change gate: the regression suite runs against the candidate and
	// against the current release, each with its agent frozen.
	releaseID := fixture.releaseID
	gateID, err := gate.StartGate(ctx, 1, 3, fixture.draft, &releaseID)
	if err != nil {
		t.Fatalf("start gate: %v", err)
	}
	result, err := gate.Result(ctx, 1, gateID)
	if err != nil || result.State != releasegate.StatePending || len(result.Runs) != 2 {
		t.Fatalf("started gate = %+v, %v", result, err)
	}
	runs := map[string]releasegate.GateRun{}
	for _, run := range result.Runs {
		runs[run.Role] = run
	}
	baselineRun, err := fixture.runs.GetRun(ctx, "1", strconv.FormatInt(runs[releasegate.RoleBaseline].EvalRunID, 10))
	if err != nil {
		t.Fatal(err)
	}
	if baselineRun.Snapshot.Agent == nil || baselineRun.Snapshot.Agent.Instructions != "released instructions" ||
		*baselineRun.ApplicationVersionID != int(fixture.released) {
		t.Fatalf("baseline run = %+v", baselineRun)
	}

	// Case 2 passed on the release and fails on the candidate.
	fixture.finish(t, runs[releasegate.RoleBaseline].EvalRunID, map[string]float64{fixture.cases[0]: 5, fixture.cases[1]: 5})
	if result, _ := gate.Result(ctx, 1, gateID); result.State != releasegate.StatePending {
		t.Fatalf("one run still queued: state = %s", result.State)
	}
	fixture.finish(t, runs[releasegate.RoleCandidate].EvalRunID, map[string]float64{fixture.cases[0]: 5, fixture.cases[1]: 2})

	result, err = gate.Result(ctx, 1, gateID)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != releasegate.StateFailed || result.EvaluatedAt == nil {
		t.Fatalf("settled gate = %+v", result)
	}
	var listed []releasegate.RegressedCase
	for _, failure := range result.Failures {
		if failure.Code == releasegate.FailureRegression {
			listed = failure.Cases
		}
	}
	if len(listed) != 1 || listed[0].CaseID != fixture.cases[1] || listed[0].Name != "And Rust?" {
		t.Fatalf("regression listing = %+v (failures %+v)", listed, result.Failures)
	}

	// The outcome is written once.
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE elitea_lifecycle.release_gates SET state = 'passed' WHERE id = $1`, gateID); err == nil {
		t.Fatal("a settled gate's outcome was changed")
	}

	// The override is recorded once, with its reason, and the gate stays
	// failed.
	if err := gate.Override(ctx, 1, gateID, actor, "case 2 asks about a retired product"); err != nil {
		t.Fatalf("override: %v", err)
	}
	if err := store.OverrideGate(ctx, 1, gateID, actor, "again"); !errors.Is(err, releasegate.ErrAlreadyOverridden) {
		t.Fatalf("second override: %v", err)
	}
	result, _ = gate.Result(ctx, 1, gateID)
	if result.State != releasegate.StateFailed || result.Override == nil ||
		result.Override.Reason != "case 2 asks about a retired product" || result.Override.ActorID != actor {
		t.Fatalf("overridden gate = %+v", result)
	}
	listedGates, err := store.ListGates(ctx, 1, 3, 10)
	if err != nil || len(listedGates) != 1 || len(listedGates[0].Runs) != 2 {
		t.Fatalf("list gates = %+v, %v", listedGates, err)
	}
	if _, err := store.GetGate(ctx, 2, gateID); !errors.Is(err, releasegate.ErrGateNotFound) {
		t.Fatalf("a gate of another project: %v", err)
	}

	// The scheduled re-evaluation: the suite is due, a scheduled gate runs
	// the release's definition, and the agent is not due again while it is
	// pending.
	days := 7
	regression.ReevaluateEveryDays = &days
	if _, err := gate.PutSuite(ctx, 1, regression); err != nil {
		t.Fatal(err)
	}
	due, err := store.DueScheduled(ctx, time.Now(), 10)
	if err != nil || len(due) != 1 || due[0].ApplicationID != 3 || due[0].ReleaseID != fixture.releaseID ||
		due[0].ActorID == nil || *due[0].ActorID != actor {
		t.Fatalf("due = %+v, %v", due, err)
	}
	pass, err := gate.Pass(ctx)
	if err != nil || pass.Started != 1 {
		t.Fatalf("pass = %+v, %v", pass, err)
	}
	if due, _ := store.DueScheduled(ctx, time.Now(), 10); len(due) != 0 {
		t.Fatalf("due while a scheduled gate is pending: %+v", due)
	}
	scheduled, err := store.ScheduledGatesSince(ctx, 1, 3, fixture.releaseID, time.Now().Add(-time.Hour), 10)
	if err != nil || len(scheduled) != 1 {
		t.Fatalf("scheduled gates = %v, %v", scheduled, err)
	}
	scheduledGate, _ := gate.Result(ctx, 1, scheduled[0])
	if scheduledGate.Kind != releasegate.KindScheduled || len(scheduledGate.Runs) != 1 ||
		scheduledGate.CandidateVersionID != fixture.released {
		t.Fatalf("scheduled gate = %+v", scheduledGate)
	}
	fixture.finish(t, scheduledGate.Runs[0].EvalRunID, map[string]float64{fixture.cases[0]: 1, fixture.cases[1]: 1})
	regressed, err := gate.RegressedSince(ctx, 1, 3, time.Now().Add(-time.Hour))
	if err != nil || regressed == nil || regressed.ID != scheduled[0] {
		t.Fatalf("RegressedSince = %+v, %v", regressed, err)
	}
	if references, _ := store.ReferenceRuns(ctx, 1, 3, fixture.releaseID); len(references) != 0 {
		t.Fatalf("a failed scheduled gate is not a reference: %+v", references)
	}

	// A deleted dataset is answered as missing.
	if _, err := fixture.pool.Exec(ctx, `DELETE FROM p_1.eval_datasets WHERE id = $1`, fixture.datasetA); err != nil {
		t.Fatal(err)
	}
	suites, err := store.ListSuites(ctx, 1, 3)
	if err != nil || len(suites) != 1 || !suites[0].DatasetMissing {
		t.Fatalf("suites after the dataset was deleted = %+v, %v", suites, err)
	}
}
