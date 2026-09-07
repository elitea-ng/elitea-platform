package repos

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/predict"
)

// Agent Evaluation runs, results and the read-only scorecard, against a REAL
// migrated tenant schema and through the REAL routes.
//
// The three things that can only be proved here, and not with a fake
// repository, are the three that the state machine depends on:
//
//   - the CLAIM is a conditional UPDATE, so two workers cannot both take a run;
//   - the FINISH refuses to move a row that is already terminal, which is the
//     whole mechanism behind a cancellation surviving its own worker;
//   - the RE-QUEUE sweep finds an orphaned `running` row across tenant schemas,
//     which is what makes a run survive a restart.
//
// A fake can be written to agree with any of them. Only the table decides.
type evalRunFixture struct {
	router  http.Handler
	repo    *EvalRunsRepo
	pool    *pgxpool.Pool
	dataset string
	caseIDs []string
	version int
}

func newEvalRunFixture(t *testing.T) evalRunFixture {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	runsRepo := NewEvalRunsRepo(pool)
	datasetRepo := NewEvalDatasetsRepo(pool)
	dimensionRepo := NewEvalDimensionsRepo(pool)

	datasetHandler := evaluation.NewDatasetHandler(datasetRepo)
	dimensionHandler := evaluation.NewHandler(dimensionRepo)
	runHandler := evaluation.NewRunHandler(runsRepo, dimensionRepo, &noopEnqueuer{})

	r := chi.NewRouter()
	r.Post("/eval_datasets/prompt_lib/{projectID}", datasetHandler.Create)
	r.Post("/eval_dataset_cases/prompt_lib/{projectID}/{datasetID}", datasetHandler.AddCase)
	r.Delete("/eval_dataset/prompt_lib/{projectID}/{datasetID}", datasetHandler.Delete)
	r.Post("/eval_dimensions/prompt_lib/{projectID}", dimensionHandler.Create)
	r.Get("/eval_runs/prompt_lib/{projectID}", runHandler.List)
	r.Post("/eval_runs/prompt_lib/{projectID}", runHandler.Start)
	r.Get("/eval_run/prompt_lib/{projectID}/{runID}", runHandler.Get)
	r.Post("/eval_run_cancel/prompt_lib/{projectID}/{runID}", runHandler.Cancel)
	r.Get("/eval_results/prompt_lib/{projectID}/{runID}", runHandler.Results)

	fixture := evalRunFixture{router: r, repo: runsRepo, pool: pool}

	created := callEvalRoute(t, r, http.MethodPost, "/eval_datasets/prompt_lib/1",
		`{"name":"Support","description":"","application_id":null,"is_shared":false}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("seed dataset: %d %s", created.Code, created.Body.String())
	}
	var dataset evaluation.Dataset
	if err := json.Unmarshal(created.Body.Bytes(), &dataset); err != nil {
		t.Fatalf("decode dataset: %v", err)
	}
	fixture.dataset = dataset.ID

	for _, input := range []string{"what is Go?", "what is Rust?"} {
		body := fmt.Sprintf(`{"input":%q,"variables":{}}`, input)
		response := callEvalRoute(t, r, http.MethodPost,
			"/eval_dataset_cases/prompt_lib/1/"+dataset.ID, body)
		if response.Code != http.StatusCreated {
			t.Fatalf("seed case: %d %s", response.Code, response.Body.String())
		}
		var seeded evaluation.DatasetCase
		if err := json.Unmarshal(response.Body.Bytes(), &seeded); err != nil {
			t.Fatalf("decode case: %v", err)
		}
		fixture.caseIDs = append(fixture.caseIDs, seeded.ID)
	}

	// A real application version, so the orchestrator's AgentVersion read has
	// something to find.
	//
	// The `instructions` column is ADDED HERE and not in
	// postgresIntegrationSeedSQL, deliberately. That seed is the shared
	// template for this whole package and its text is part of the template
	// FINGERPRINT, so widening it rebuilds the template for every other test in
	// the package. It is also declared as "the minimum legacy project schema" —
	// the columns the tenant history alters — and `instructions` is not one of
	// them. Adding it on the copied database keeps the change inside the tests
	// that need it. `applications` is not seeded at all: tenant/0132's FK guard
	// only adds the agent references where that table exists, which is exactly
	// what this template proves.
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`ALTER TABLE p_1.application_versions ADD COLUMN IF NOT EXISTS instructions TEXT`); err != nil {
		t.Fatalf("widen application_versions: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.application_versions (instructions, llm_settings)
		VALUES ('be helpful', '{"model_name":"qwen"}'::jsonb)
		RETURNING id`).Scan(&fixture.version); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	return fixture
}

type noopEnqueuer struct{}

func (noopEnqueuer) Enqueue(evaluation.RunRef) {}
func (noopEnqueuer) Cancel(evaluation.RunRef)  {}

func (f evalRunFixture) seedAIDimension(t *testing.T) string {
	t.Helper()
	response := callEvalRoute(t, f.router, http.MethodPost, "/eval_dimensions/prompt_lib/1",
		`{"name":"Helpfulness","description":"Does it help?","tier":"project","application_id":null,`+
			`"allowed_engines":["ai"],"scale_type":"ordinal","scale_min":1,"scale_max":5,`+
			`"polarity":"higher_better","default_weight":1,"default_target":4,"default_target_operator":">=",`+
			`"code":"","return_contract":""}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("seed dimension: %d %s", response.Code, response.Body.String())
	}
	var dimension evaluation.Dimension
	if err := json.Unmarshal(response.Body.Bytes(), &dimension); err != nil {
		t.Fatalf("decode dimension: %v", err)
	}
	return dimension.ID
}

func (f evalRunFixture) startRun(t *testing.T, dimensionID string) evaluation.Run {
	t.Helper()
	body := fmt.Sprintf(
		`{"dataset_id":%q,"application_id":900,"application_version_id":%d,"dimension_ids":[%q]}`,
		f.dataset, f.version, dimensionID)
	response := callEvalRoute(t, f.router, http.MethodPost, "/eval_runs/prompt_lib/1", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("start run: %d %s", response.Code, response.Body.String())
	}
	var run evaluation.Run
	if err := json.Unmarshal(response.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	return run
}

func (f evalRunFixture) readRun(t *testing.T, runID string) evaluation.Run {
	t.Helper()
	response := callEvalRoute(t, f.router, http.MethodGet, "/eval_run/prompt_lib/1/"+runID, "")
	if response.Code != http.StatusOK {
		t.Fatalf("get run: %d %s", response.Code, response.Body.String())
	}
	var run evaluation.Run
	if err := json.Unmarshal(response.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	return run
}

// The snapshot has to SURVIVE the jsonb round trip. A run whose snapshot came
// back empty would score nothing and report a finished run with no headline —
// a 200 at every step and no evaluation at all.
func TestEvalRunSnapshotSurvivesTheDatabase(t *testing.T) {
	fixture := newEvalRunFixture(t)
	dimensionID := fixture.seedAIDimension(t)

	created := fixture.startRun(t, dimensionID)
	stored := fixture.readRun(t, created.ID)

	if stored.Status != evaluation.RunStatusCreated {
		t.Errorf("status = %q, want created", stored.Status)
	}
	if stored.ExecutionMode != evaluation.ExecutionModePredictBlocking {
		t.Errorf("execution_mode = %q — the read must say what actually ran", stored.ExecutionMode)
	}
	if stored.Progress.Total != 2 {
		t.Errorf("progress.total = %d, want the dataset's two cases", stored.Progress.Total)
	}
	if len(stored.Snapshot.Cases) != 2 {
		t.Errorf("the stored snapshot has %d cases, want 2", len(stored.Snapshot.Cases))
	}
	if len(stored.Snapshot.Bindings) != 1 {
		t.Fatalf("the stored snapshot has %d bindings, want 1", len(stored.Snapshot.Bindings))
	}
	frozen, ok := stored.Snapshot.Dimensions[dimensionID]
	if !ok {
		t.Fatalf("the stored snapshot has no dimension %q: %+v", dimensionID, stored.Snapshot.Dimensions)
	}
	if frozen.Description != "Does it help?" || frozen.ScaleMax != 5 {
		t.Errorf("the frozen dimension lost its rubric or scale: %+v", frozen)
	}
	binding := stored.Snapshot.Bindings[0]
	if binding.Target == nil || *binding.Target != 4 || binding.TargetOperator != ">=" {
		t.Errorf("the binding lost the dimension's target: %+v", binding)
	}
}

// THE CLAIM IS EXCLUSIVE. Two workers ask for the same run; exactly one gets
// it. Without the `status = 'created'` predicate both would score every case
// and the bill would double.
func TestEvalRunClaimIsExclusive(t *testing.T) {
	fixture := newEvalRunFixture(t)
	run := fixture.startRun(t, fixture.seedAIDimension(t))
	ctx := context.Background()

	if _, err := fixture.repo.ClaimRun(ctx, "1", run.ID); err != nil {
		t.Fatalf("the first claim failed: %v", err)
	}
	if _, err := fixture.repo.ClaimRun(ctx, "1", run.ID); err == nil {
		t.Fatal("a second worker claimed a run that was already running")
	}

	stored := fixture.readRun(t, run.ID)
	if stored.Status != evaluation.RunStatusRunning {
		t.Errorf("status = %q, want running", stored.Status)
	}
	if stored.StartedAt == "" {
		t.Error("the claim did not stamp started_at")
	}
}

// A CANCELLATION SURVIVES ITS OWN WORKER. The route moves the row to
// `cancelled`; the worker then reaches the end of its walk and calls FinishRun,
// which must NOT overwrite it. `status = 'running'` in the finish predicate is
// the whole mechanism, and only the table can prove it.
func TestEvalRunCancellationIsNotOverwrittenByTheWorkersFinish(t *testing.T) {
	fixture := newEvalRunFixture(t)
	run := fixture.startRun(t, fixture.seedAIDimension(t))
	ctx := context.Background()

	if _, err := fixture.repo.ClaimRun(ctx, "1", run.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	cancelResponse := callEvalRoute(t, fixture.router, http.MethodPost,
		"/eval_run_cancel/prompt_lib/1/"+run.ID, "")
	if cancelResponse.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", cancelResponse.Code, cancelResponse.Body.String())
	}

	headline := 99.0
	if err := fixture.repo.FinishRun(ctx, "1", run.ID, evaluation.RunStatusFinished, &headline, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}

	stored := fixture.readRun(t, run.ID)
	if stored.Status != evaluation.RunStatusCancelled {
		t.Fatalf("status = %q — the worker overwrote the user's stop", stored.Status)
	}
	if stored.HeadlineScore != nil {
		t.Errorf("a cancelled run was given a headline of %v", *stored.HeadlineScore)
	}
}

// A run that is already terminal answers 409, not 404. The run exists; telling
// the caller it does not sends them looking for a missing row.
func TestEvalRunCancelOfATerminalRunIsAConflict(t *testing.T) {
	fixture := newEvalRunFixture(t)
	run := fixture.startRun(t, fixture.seedAIDimension(t))

	if code := callEvalRoute(t, fixture.router, http.MethodPost,
		"/eval_run_cancel/prompt_lib/1/"+run.ID, "").Code; code != http.StatusOK {
		t.Fatalf("first cancel: %d", code)
	}
	second := callEvalRoute(t, fixture.router, http.MethodPost,
		"/eval_run_cancel/prompt_lib/1/"+run.ID, "")
	if second.Code != http.StatusConflict {
		t.Fatalf("second cancel: %d %s, want 409", second.Code, second.Body.String())
	}
}

// THE RESTART RE-QUEUE. A `running` row with a stale heartbeat is moved back to
// `created` and reported, so the next process picks it up. Without it the run
// stays `running` for ever: the goroutine died with the process, and
// `started_at` cannot tell a dead run from a slow one.
func TestEvalRunSweepRequeuesAnOrphanedRun(t *testing.T) {
	fixture := newEvalRunFixture(t)
	run := fixture.startRun(t, fixture.seedAIDimension(t))
	ctx := context.Background()

	if _, err := fixture.repo.ClaimRun(ctx, "1", run.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Age the heartbeat rather than sleeping: the TTL is ten minutes.
	if _, err := fixture.pool.Exec(ctx,
		`UPDATE p_1.eval_runs SET heartbeat_at = now() - interval '1 hour' WHERE id = $1`,
		run.ID); err != nil {
		t.Fatalf("age the heartbeat: %v", err)
	}

	requeued, err := fixture.repo.RequeueStaleRuns(ctx, int(evaluation.StaleRunTTL.Seconds()))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	found := false
	for _, ref := range requeued {
		if ref.ProjectID == "1" && ref.RunID == run.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the orphan was not re-queued: %+v", requeued)
	}
	if stored := fixture.readRun(t, run.ID); stored.Status != evaluation.RunStatusCreated {
		t.Errorf("status = %q, want created so a worker can claim it again", stored.Status)
	}

	// A FRESH heartbeat must NOT be re-queued: a slow run is not a dead one,
	// and re-queueing it would put two workers on the same dataset.
	if _, err := fixture.repo.ClaimRun(ctx, "1", run.ID); err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	again, err := fixture.repo.RequeueStaleRuns(ctx, int(evaluation.StaleRunTTL.Seconds()))
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	for _, ref := range again {
		if ref.RunID == run.ID {
			t.Fatal("a run with a fresh heartbeat was re-queued")
		}
	}

	// And PendingRuns finds what the sweep leaves behind.
	if _, err := fixture.pool.Exec(ctx,
		`UPDATE p_1.eval_runs SET status = 'created' WHERE id = $1`, run.ID); err != nil {
		t.Fatalf("reset: %v", err)
	}
	pending, err := fixture.repo.PendingRuns(ctx, 10)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) == 0 || pending[0].RunID != run.ID || pending[0].ProjectID != "1" {
		t.Fatalf("PendingRuns = %+v, want the created run in project 1", pending)
	}
}

// A RESULT UPSERTS on (run, case, dimension). A re-queued run re-scores cases
// it already scored, and a second row would be counted twice by every average.
func TestEvalRunResultUpsertsRatherThanDuplicating(t *testing.T) {
	fixture := newEvalRunFixture(t)
	dimensionID := fixture.seedAIDimension(t)
	run := fixture.startRun(t, dimensionID)
	ctx := context.Background()

	first, second := 20.0, 80.0
	for _, score := range []float64{first, second} {
		native, normalized := score, score
		if err := fixture.repo.SaveResult(ctx, "1", evaluation.RunResult{
			RunID: run.ID, DatasetCaseID: fixture.caseIDs[0], DimensionID: dimensionID,
			Status: evaluation.ResultStatusOK, NativeScore: &native, NormalizedScore: &normalized,
			Verdict: map[string]any{"score": score}, Evidence: map[string]any{"output": "an answer"},
		}); err != nil {
			t.Fatalf("save result: %v", err)
		}
	}

	results, total, err := fixture.repo.ListResults(ctx, "1", run.ID, evaluation.ResultPage{Limit: 100})
	if err != nil {
		t.Fatalf("list results: %v", err)
	}
	if total != 1 || len(results) != 1 {
		t.Fatalf("stored %d results (total %d), want exactly 1 — the re-score duplicated", len(results), total)
	}
	if results[0].NormalizedScore == nil || *results[0].NormalizedScore != second {
		t.Errorf("normalized_score = %v, want the re-scored %v", results[0].NormalizedScore, second)
	}
}

// The table refuses an `ok` result with no score. Without the CHECK, a judge
// path that forgot to set the status would store `ok` with a NULL score, the
// scorecard would average it as absent, and the run would report a headline
// over fewer cases than it claims to cover.
func TestEvalResultTableRefusesAnOKRowWithNoScore(t *testing.T) {
	fixture := newEvalRunFixture(t)
	dimensionID := fixture.seedAIDimension(t)
	run := fixture.startRun(t, dimensionID)

	err := fixture.repo.SaveResult(context.Background(), "1", evaluation.RunResult{
		RunID: run.ID, DatasetCaseID: fixture.caseIDs[0], DimensionID: dimensionID,
		Status: evaluation.ResultStatusOK,
	})
	if err == nil {
		t.Fatal("an `ok` result with no score was stored")
	}
	if !strings.Contains(err.Error(), "eval_results_ok_has_scores_check") {
		t.Errorf("the refusal did not come from the stored constraint: %v", err)
	}
}

// The scorecard reads results back THROUGH THE ROUTE, with the run, the total
// and the named gaps. A stored row a client cannot see is not a stored row as
// far as anyone using the product is concerned.
func TestEvalScorecardReadsResultsThroughTheRoute(t *testing.T) {
	fixture := newEvalRunFixture(t)
	dimensionID := fixture.seedAIDimension(t)
	run := fixture.startRun(t, dimensionID)
	ctx := context.Background()

	if _, err := fixture.repo.ClaimRun(ctx, "1", run.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	for index, caseID := range fixture.caseIDs {
		native := float64(index + 4)
		normalized, ok := evaluation.NormalizeScore(native, "ordinal", 1, 5, "higher_better")
		if !ok {
			t.Fatalf("case %s: the score could not be normalised", caseID)
		}
		met, _ := evaluation.EvaluateTargetMet(native, ">=", 4)
		if err := fixture.repo.SaveResult(ctx, "1", evaluation.RunResult{
			RunID: run.ID, DatasetCaseID: caseID, DimensionID: dimensionID,
			Status: evaluation.ResultStatusOK, NativeScore: &native, NormalizedScore: &normalized,
			TargetMet: &met,
			Verdict:   map[string]any{"score": native, "reason": "graded"},
			Evidence:  map[string]any{"input": "q", "output": "a"},
		}); err != nil {
			t.Fatalf("save result: %v", err)
		}
	}
	headline := 87.5
	if err := fixture.repo.FinishRun(ctx, "1", run.ID, evaluation.RunStatusFinished, &headline, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}

	response := callEvalRoute(t, fixture.router, http.MethodGet, "/eval_results/prompt_lib/1/"+run.ID, "")
	if response.Code != http.StatusOK {
		t.Fatalf("scorecard: %d %s", response.Code, response.Body.String())
	}
	var scorecard struct {
		Run           evaluation.Run         `json:"run"`
		Results       []evaluation.RunResult `json:"results"`
		HeadlineScore *float64               `json:"headline_score"`
		Total         int                    `json:"total"`
		Unavailable   []struct {
			Key    string `json:"key"`
			Reason string `json:"reason"`
		} `json:"unavailable"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &scorecard); err != nil {
		t.Fatalf("decode scorecard: %v", err)
	}
	if scorecard.Total != 2 || len(scorecard.Results) != 2 {
		t.Fatalf("the scorecard shows %d of %d results, want 2 and 2", len(scorecard.Results), scorecard.Total)
	}
	if scorecard.HeadlineScore == nil || *scorecard.HeadlineScore != headline {
		t.Errorf("headline = %v, want %v", scorecard.HeadlineScore, headline)
	}
	if scorecard.Run.Status != evaluation.RunStatusFinished {
		t.Errorf("run status = %q, want finished", scorecard.Run.Status)
	}
	// The verdict and the evidence are what a person reads when a score
	// surprises them; a scorecard that lost them is a table of numbers with no
	// explanation.
	for _, result := range scorecard.Results {
		if result.Verdict["reason"] != "graded" {
			t.Errorf("case %s: the verdict did not survive: %v", result.DatasetCaseID, result.Verdict)
		}
		if result.Evidence["output"] != "a" {
			t.Errorf("case %s: the evidence did not survive: %v", result.DatasetCaseID, result.Evidence)
		}
		if result.TargetMet == nil {
			t.Errorf("case %s: the target verdict was lost", result.DatasetCaseID)
		}
	}
	if len(scorecard.Unavailable) == 0 {
		t.Error("the scorecard does not name what this release cannot serve")
	}
}

// The dataset delete REFUSES while a run references it. tenant/0132 gives the
// run's `dataset_id` an ON DELETE CASCADE — it has to, or an agent with an
// evaluated dataset becomes undeletable — so this refusal is the only thing
// between a casual tidy-up and the loss of every score.
func TestEvalDatasetDeleteRefusesWhileARunReferencesIt(t *testing.T) {
	fixture := newEvalRunFixture(t)
	run := fixture.startRun(t, fixture.seedAIDimension(t))

	response := callEvalRoute(t, fixture.router, http.MethodDelete,
		"/eval_dataset/prompt_lib/1/"+fixture.dataset, "")
	if response.Code != http.StatusConflict {
		t.Fatalf("delete: %d %s, want 409", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "run") {
		t.Errorf("the refusal does not say what is holding the dataset: %s", response.Body.String())
	}
	if stored := fixture.readRun(t, run.ID); stored.ID != run.ID {
		t.Error("the run disappeared with a refused delete")
	}
}

// The agent version read is what the orchestrator builds its turn from. A
// version in ANOTHER project must not resolve: the schema comes from the
// project id, so this is a check that nothing here reads a fixed schema.
func TestEvalRunAgentVersionIsPerProject(t *testing.T) {
	fixture := newEvalRunFixture(t)
	ctx := context.Background()

	version, err := fixture.repo.AgentVersion(ctx, "1", fixture.version)
	if err != nil {
		t.Fatalf("agent version: %v", err)
	}
	if version.Instructions != "be helpful" || version.ModelName != "qwen" {
		t.Fatalf("the agent turn would be built from %+v", version)
	}

	// p_2 does not exist in this template, so the read must FAIL rather than
	// answer p_1's row. Failing closed is the requirement.
	if _, err := fixture.repo.AgentVersion(ctx, "2", fixture.version); err == nil {
		t.Fatal("project 2 was served project 1's agent version")
	}
}

// A run started against a dataset id from another project is refused, because
// the schema is derived from the path segment the membership middleware already
// bound to the caller. This is the cross-project refusal stated as the route
// sees it.
func TestEvalRunStartIsPerProject(t *testing.T) {
	fixture := newEvalRunFixture(t)
	dimensionID := fixture.seedAIDimension(t)

	body := fmt.Sprintf(
		`{"dataset_id":%q,"application_id":900,"application_version_id":%d,"dimension_ids":[%q]}`,
		fixture.dataset, fixture.version, dimensionID)
	response := callEvalRoute(t, fixture.router, http.MethodPost, "/eval_runs/prompt_lib/2", body)
	if response.Code == http.StatusCreated {
		t.Fatalf("project 2 started a run over project 1's dataset: %s", response.Body.String())
	}

	// And project 1's run listing is not visible through project 2's segment.
	fixture.startRun(t, dimensionID)
	other := callEvalRoute(t, fixture.router, http.MethodGet, "/eval_runs/prompt_lib/2", "")
	if other.Code == http.StatusOK && strings.Contains(other.Body.String(), `"dataset_id"`) {
		t.Fatalf("project 2 was served project 1's runs: %s", other.Body.String())
	}
}

// The run listing is newest first, because the reference's history panel
// computes a delta against the nearest OLDER run. A listing in insertion order
// would make "the previous run" mean the next one.
func TestEvalRunListIsNewestFirst(t *testing.T) {
	fixture := newEvalRunFixture(t)
	dimensionID := fixture.seedAIDimension(t)

	first := fixture.startRun(t, dimensionID)
	second := fixture.startRun(t, dimensionID)

	response := callEvalRoute(t, fixture.router, http.MethodGet, "/eval_runs/prompt_lib/1", "")
	if response.Code != http.StatusOK {
		t.Fatalf("list: %d", response.Code)
	}
	var page struct {
		Rows  []evaluation.Run `json:"rows"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Rows) != 2 {
		t.Fatalf("listed %d runs, want 2", len(page.Rows))
	}
	if page.Rows[0].ID != second.ID || page.Rows[1].ID != first.ID {
		t.Errorf("run order = %s, %s; want the newest first (%s, %s)",
			page.Rows[0].ID, page.Rows[1].ID, second.ID, first.ID)
	}
}

// scriptedLLM answers the agent turn and the judge turn from one stub, the way
// the production composition does: both take the SAME PredictCompleter, so a
// test that used two clients would not be exercising the wiring the service
// has.
//
// It distinguishes the two by the system prompt: the judge's says "evaluation
// judge", the agent's is the version's own instructions.
type scriptedLLM struct {
	agentAnswer string
	judgeAnswer string
	judgeErr    error
	agentCalls  int
	judgeCalls  int
}

func (s *scriptedLLM) Complete(_ context.Context, req predict.CompletionRequest) (string, error) {
	if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "evaluation judge") {
		s.judgeCalls++
		if s.judgeErr != nil {
			return "", s.judgeErr
		}
		return s.judgeAnswer, nil
	}
	s.agentCalls++
	return s.agentAnswer, nil
}

// THE WHOLE SLICE, END TO END, OVER REAL SQL: one dataset, one AI dimension,
// one run, one score per case, read back through the scorecard route.
//
// This is the journey #617 names as "the concrete evidence the feature works",
// with the browser replaced by the routes it calls. Everything except the model
// is real: the tables, the migrations, the handlers, the orchestrator's state
// machine and the score normalisation.
func TestEvalRunEndToEndScoresADatasetAndServesTheScorecard(t *testing.T) {
	fixture := newEvalRunFixture(t)
	dimensionID := fixture.seedAIDimension(t)
	run := fixture.startRun(t, dimensionID)

	llm := &scriptedLLM{agentAnswer: "Go is a programming language.", judgeAnswer: `{"score": 5, "reason": "correct"}`}
	orchestrator := evaluation.NewOrchestrator(
		fixture.repo, evaluation.NewAIJudge(llm), llm, quietTestLogger())
	orchestrator.Execute(context.Background(), evaluation.RunRef{ProjectID: "1", RunID: run.ID})

	// One agent call per case, one judge call per (case, dimension).
	if llm.agentCalls != 2 || llm.judgeCalls != 2 {
		t.Fatalf("model calls = %d agent / %d judge, want 2 and 2", llm.agentCalls, llm.judgeCalls)
	}

	finished := fixture.readRun(t, run.ID)
	if finished.Status != evaluation.RunStatusFinished {
		t.Fatalf("status = %q (error %q), want finished", finished.Status, finished.Error)
	}
	if finished.Progress.Done != 2 || finished.Progress.Total != 2 {
		t.Errorf("progress = %d/%d, want 2/2", finished.Progress.Done, finished.Progress.Total)
	}
	// 5 on a 1..5 higher_better scale is 100, for both cases.
	if finished.HeadlineScore == nil || *finished.HeadlineScore != 100 {
		t.Fatalf("headline = %v, want 100", finished.HeadlineScore)
	}

	response := callEvalRoute(t, fixture.router, http.MethodGet, "/eval_results/prompt_lib/1/"+run.ID, "")
	if response.Code != http.StatusOK {
		t.Fatalf("scorecard: %d %s", response.Code, response.Body.String())
	}
	var scorecard struct {
		Results []evaluation.RunResult `json:"results"`
		Total   int                    `json:"total"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &scorecard); err != nil {
		t.Fatalf("decode scorecard: %v", err)
	}
	if scorecard.Total != 2 {
		t.Fatalf("the scorecard shows %d results, want one per case", scorecard.Total)
	}
	for _, result := range scorecard.Results {
		if result.Status != evaluation.ResultStatusOK {
			t.Errorf("case %s: status = %q, want ok", result.DatasetCaseID, result.Status)
		}
		if result.NormalizedScore == nil || *result.NormalizedScore != 100 {
			t.Errorf("case %s: normalized = %v, want 100", result.DatasetCaseID, result.NormalizedScore)
		}
		if result.TargetMet == nil || !*result.TargetMet {
			t.Errorf("case %s: the >= 4 target was not recorded as met", result.DatasetCaseID)
		}
		if result.Evidence["output"] != "Go is a programming language." {
			t.Errorf("case %s: the agent's answer is not in the evidence: %v", result.DatasetCaseID, result.Evidence)
		}
	}
}

// A judge that fails marks the CASE `error` and the run still FINISHES, over
// real SQL — including tenant/0132's CHECK, which refuses an `ok` row with no
// score and would turn a mis-statused result into a write failure.
func TestEvalRunEndToEndKeepsAJudgeFailureToItsOwnCase(t *testing.T) {
	fixture := newEvalRunFixture(t)
	run := fixture.startRun(t, fixture.seedAIDimension(t))

	llm := &scriptedLLM{agentAnswer: "an answer", judgeAnswer: "I would say about four out of five."}
	orchestrator := evaluation.NewOrchestrator(
		fixture.repo, evaluation.NewAIJudge(llm), llm, quietTestLogger())
	orchestrator.Execute(context.Background(), evaluation.RunRef{ProjectID: "1", RunID: run.ID})

	finished := fixture.readRun(t, run.ID)
	if finished.Status != evaluation.RunStatusFinished {
		t.Fatalf("status = %q, want finished — an unparseable judge is not a broken run", finished.Status)
	}
	if finished.HeadlineScore != nil {
		t.Errorf("headline = %v, want none — nothing was scored", *finished.HeadlineScore)
	}

	results, _, err := fixture.repo.ListResults(context.Background(), "1", run.ID,
		evaluation.ResultPage{Limit: 100})
	if err != nil {
		t.Fatalf("list results: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("stored %d results, want one per case", len(results))
	}
	for _, result := range results {
		if result.Status != evaluation.ResultStatusError {
			t.Errorf("case %s: status = %q, want error", result.DatasetCaseID, result.Status)
		}
		// NOT a zero. Zero is a real and very bad score.
		if result.NativeScore != nil || result.NormalizedScore != nil {
			t.Errorf("case %s: an unscorable case was given a score: %v", result.DatasetCaseID, result.NativeScore)
		}
		// The RAW model text is kept, because a judge failure is a prompt
		// problem and a prompt problem cannot be diagnosed from the word
		// "error".
		if result.Verdict["raw"] == nil {
			t.Errorf("case %s: the judge's own answer was discarded: %v", result.DatasetCaseID, result.Verdict)
		}
	}
}

func quietTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
