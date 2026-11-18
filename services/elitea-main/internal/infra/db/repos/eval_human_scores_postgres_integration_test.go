package repos

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
)

// Human scores, suites and run limits against a REAL migrated tenant schema
// (tenant/0244): the append-only store, the transaction that turns a pending
// result into a score and recomputes the headline, and the suite round trip.

func humanScoreRouter(f evalRunFixture) http.Handler {
	humanHandler := evaluation.NewHumanScoreHandler(f.repo, f.repo)
	suiteRepo := NewEvalSuitesRepo(f.pool)
	suiteHandler := evaluation.NewSuiteHandler(suiteRepo)
	dimensionRepo := NewEvalDimensionsRepo(f.pool)
	runHandler := evaluation.NewRunHandler(f.repo, dimensionRepo, &noopEnqueuer{}).WithSuites(suiteRepo)

	r := chi.NewRouter()
	r.Get("/eval_human_scores/prompt_lib/{projectID}/{runID}", humanHandler.List)
	r.Post("/eval_human_scores/prompt_lib/{projectID}/{runID}", humanHandler.Create)
	r.Get("/eval_suites/prompt_lib/{projectID}", suiteHandler.List)
	r.Post("/eval_suites/prompt_lib/{projectID}", suiteHandler.Create)
	r.Put("/eval_suite/prompt_lib/{projectID}/{suiteID}", suiteHandler.Update)
	r.Delete("/eval_suite/prompt_lib/{projectID}/{suiteID}", suiteHandler.Delete)
	r.Post("/eval_runs/prompt_lib/{projectID}", runHandler.Start)
	r.Get("/eval_run/prompt_lib/{projectID}/{runID}", runHandler.Get)
	return r
}

func (f evalRunFixture) seedHumanDimension(t *testing.T) string {
	t.Helper()
	response := callEvalRoute(t, f.router, http.MethodPost, "/eval_dimensions/prompt_lib/1",
		`{"name":"Tone","description":"Is it warm?","tier":"project","application_id":null,`+
			`"allowed_engines":["human"],"scale_type":"ordinal","scale_min":1,"scale_max":5,`+
			`"polarity":"higher_better","default_weight":1,"default_target":4,"default_target_operator":">=",`+
			`"code":"","return_contract":""}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("seed human dimension: %d %s", response.Code, response.Body.String())
	}
	var dimension evaluation.Dimension
	if err := json.Unmarshal(response.Body.Bytes(), &dimension); err != nil {
		t.Fatal(err)
	}
	return dimension.ID
}

// startHumanRun starts a run over the human dimension and finishes it with a
// pending_human result per case, which is what the orchestrator writes.
func (f evalRunFixture) startHumanRun(t *testing.T, router http.Handler, dimensionID string) evaluation.Run {
	t.Helper()
	body := fmt.Sprintf(`{"dataset_id":%q,"application_id":900,"application_version_id":%d,"dimension_ids":[%q]}`,
		f.dataset, f.version, dimensionID)
	response := callEvalRoute(t, router, http.MethodPost, "/eval_runs/prompt_lib/1", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("start human run: %d %s", response.Code, response.Body.String())
	}
	var run evaluation.Run
	if err := json.Unmarshal(response.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if len(run.Snapshot.Bindings) != 1 || run.Snapshot.Bindings[0].Engine != evaluation.EngineHuman {
		t.Fatalf("bindings = %+v, want one human binding", run.Snapshot.Bindings)
	}
	ctx := context.Background()
	for _, caseID := range f.caseIDs {
		if err := f.repo.SaveResult(ctx, "1", evaluation.RunResult{
			RunID: run.ID, DatasetCaseID: caseID, DimensionID: dimensionID,
			Status:   evaluation.ResultStatusPendingHuman,
			Evidence: map[string]any{"output": "an answer"},
		}); err != nil {
			t.Fatalf("save pending result: %v", err)
		}
	}
	if _, err := f.repo.ClaimRun(ctx, "1", run.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := f.repo.FinishRun(ctx, "1", run.ID, evaluation.RunStatusFinished, nil, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	return run
}

func scoreBody(caseID, dimensionID string, score float64, note string) string {
	return fmt.Sprintf(`{"dataset_case_id":%q,"dimension_id":%q,"score":%v,"note":%q}`, caseID, dimensionID, score, note)
}

func TestEvalHumanScoreTurnsAPendingResultIntoAScoreAndRecomputesTheHeadline(t *testing.T) {
	fixture := newEvalRunFixture(t)
	router := humanScoreRouter(fixture)
	dimensionID := fixture.seedHumanDimension(t)
	run := fixture.startHumanRun(t, router, dimensionID)
	path := "/eval_human_scores/prompt_lib/1/" + run.ID

	if got := fixture.readRun(t, run.ID); got.HeadlineScore != nil {
		t.Fatalf("headline = %v before any human score, want none", *got.HeadlineScore)
	}

	// 4 on 1..5 is 75.
	response := callEvalRoute(t, router, http.MethodPost, path, scoreBody(fixture.caseIDs[0], dimensionID, 4, "warm"))
	if response.Code != http.StatusCreated {
		t.Fatalf("score: %d %s", response.Code, response.Body.String())
	}
	var recorded evaluation.HumanScoreRecorded
	if err := json.Unmarshal(response.Body.Bytes(), &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.Result.Status != evaluation.ResultStatusOK || recorded.Result.NormalizedScore == nil || *recorded.Result.NormalizedScore != 75 {
		t.Fatalf("result = %+v, want ok at 75", recorded.Result)
	}
	if recorded.Result.TargetMet == nil || !*recorded.Result.TargetMet {
		t.Errorf("target_met = %v, want true (4 >= 4)", recorded.Result.TargetMet)
	}
	if recorded.HeadlineScore == nil || *recorded.HeadlineScore != 75 {
		t.Fatalf("headline = %v, want 75 after the first score", recorded.HeadlineScore)
	}
	if got := fixture.readRun(t, run.ID); got.HeadlineScore == nil || *got.HeadlineScore != 75 {
		t.Fatalf("stored headline = %v, want 75", got.HeadlineScore)
	}

	// The second case is still pending, so it does not count; scoring it moves
	// the headline to the average, and a re-score replaces the first score.
	callEvalRoute(t, router, http.MethodPost, path, scoreBody(fixture.caseIDs[1], dimensionID, 2, ""))
	callEvalRoute(t, router, http.MethodPost, path, scoreBody(fixture.caseIDs[0], dimensionID, 5, "on reflection, great"))
	// case 0: 5 -> 100; case 1: 2 -> 25; average 62.5.
	if got := fixture.readRun(t, run.ID); got.HeadlineScore == nil || *got.HeadlineScore != 62.5 {
		t.Fatalf("headline = %v, want 62.5", got.HeadlineScore)
	}

	// Append-only: three rows, oldest first, the reviewer and note kept.
	list := callEvalRoute(t, router, http.MethodGet, path, "")
	var body struct {
		Rows  []evaluation.HumanScore `json:"rows"`
		Total int                     `json:"total"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 3 {
		t.Fatalf("stored %d scores, want 3 (a re-score appends)", body.Total)
	}
	if body.Rows[0].Note != "warm" || body.Rows[1].Note != "on reflection, great" {
		t.Errorf("history = %+v", body.Rows)
	}

	// The table refuses to change a score.
	if _, err := fixture.pool.Exec(context.Background(), `UPDATE p_1.eval_human_scores SET native_score = 1`); err == nil {
		t.Error("an UPDATE of a human score was accepted")
	}
}

func TestEvalHumanScoreRefusesAResultTheJudgeScoredAndAMissingOne(t *testing.T) {
	fixture := newEvalRunFixture(t)
	router := humanScoreRouter(fixture)
	dimensionID := fixture.seedHumanDimension(t)
	run := fixture.startHumanRun(t, router, dimensionID)
	path := "/eval_human_scores/prompt_lib/1/" + run.ID

	// Replace case 0's pending result with a judge-scored one.
	native, normalized := 5.0, 100.0
	if err := fixture.repo.SaveResult(context.Background(), "1", evaluation.RunResult{
		RunID: run.ID, DatasetCaseID: fixture.caseIDs[0], DimensionID: dimensionID,
		Status: evaluation.ResultStatusOK, NativeScore: &native, NormalizedScore: &normalized,
	}); err != nil {
		t.Fatal(err)
	}
	if response := callEvalRoute(t, router, http.MethodPost, path,
		scoreBody(fixture.caseIDs[0], dimensionID, 1, "")); response.Code != http.StatusConflict {
		t.Errorf("scoring a judge-scored result: code = %d, want 409: %s", response.Code, response.Body.String())
	}
	if _, err := fixture.pool.Exec(context.Background(),
		`DELETE FROM p_1.eval_results WHERE run_id = $1 AND dataset_case_id = $2`, run.ID, fixture.caseIDs[1]); err != nil {
		t.Fatal(err)
	}
	if response := callEvalRoute(t, router, http.MethodPost, path,
		scoreBody(fixture.caseIDs[1], dimensionID, 3, "")); response.Code != http.StatusConflict {
		t.Errorf("scoring a case with no result: code = %d, want 409: %s", response.Code, response.Body.String())
	}
}

func TestEvalSuiteRoundTripAndStartFreezesItsLimits(t *testing.T) {
	fixture := newEvalRunFixture(t)
	router := humanScoreRouter(fixture)
	dimensionID := fixture.seedAIDimension(t)

	created := callEvalRoute(t, router, http.MethodPost, "/eval_suites/prompt_lib/1",
		`{"name":"Cheap","description":"d","token_limit_per_case":2000,"cost_limit_per_run":1.5,"on_run_limit":"report"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create suite: %d %s", created.Code, created.Body.String())
	}
	var suite evaluation.Suite
	if err := json.Unmarshal(created.Body.Bytes(), &suite); err != nil {
		t.Fatal(err)
	}
	if suite.TokenLimitPerCase == nil || *suite.TokenLimitPerCase != 2000 ||
		suite.CostLimitPerRun == nil || *suite.CostLimitPerRun != 1.5 ||
		suite.TokenLimitPerRun != nil || suite.OnRunLimit != "report" {
		t.Fatalf("suite = %+v", suite)
	}

	// A second suite with the same name (any case) is a conflict.
	if response := callEvalRoute(t, router, http.MethodPost, "/eval_suites/prompt_lib/1",
		`{"name":"cheap"}`); response.Code != http.StatusConflict {
		t.Errorf("duplicate name: code = %d, want 409", response.Code)
	}

	body := fmt.Sprintf(`{"dataset_id":%q,"application_id":900,"application_version_id":%d,"dimension_ids":[%q],"suite_id":%q}`,
		fixture.dataset, fixture.version, dimensionID, suite.ID)
	started := callEvalRoute(t, router, http.MethodPost, "/eval_runs/prompt_lib/1", body)
	if started.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", started.Code, started.Body.String())
	}
	var run evaluation.Run
	if err := json.Unmarshal(started.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}

	// Editing the suite afterwards must not change the run that started.
	if response := callEvalRoute(t, router, http.MethodPut, "/eval_suite/prompt_lib/1/"+suite.ID,
		`{"name":"Cheap","token_limit_per_run":5,"on_run_limit":"stop"}`); response.Code != http.StatusOK {
		t.Fatalf("update suite: %d %s", response.Code, response.Body.String())
	}
	stored := fixture.readRun(t, run.ID)
	limits := stored.Snapshot.Limits
	if limits == nil || limits.TokenLimitPerCase == nil || *limits.TokenLimitPerCase != 2000 ||
		limits.TokenLimitPerRun != nil || limits.OnRunLimit != "report" {
		t.Fatalf("the stored snapshot limits = %+v, want the suite as it was at start", limits)
	}

	// The limit marker is read back on the run.
	if err := fixture.repo.MarkLimitReached(context.Background(), "1", run.ID, evaluation.LimitReachedCost); err != nil {
		t.Fatal(err)
	}
	if got := fixture.readRun(t, run.ID); got.LimitReached != evaluation.LimitReachedCost {
		t.Errorf("limit_reached = %q, want cost", got.LimitReached)
	}

	// The table refuses what the handler would: a non-positive limit.
	if _, err := fixture.pool.Exec(context.Background(),
		`INSERT INTO p_1.eval_suites (name, token_limit_per_run) VALUES ('bad', 0)`); err == nil {
		t.Error("a zero limit was stored")
	}

	if response := callEvalRoute(t, router, http.MethodDelete, "/eval_suite/prompt_lib/1/"+suite.ID, ""); response.Code != http.StatusNoContent {
		t.Errorf("delete: %d", response.Code)
	}
	// The run keeps its frozen limits after its suite is gone.
	if got := fixture.readRun(t, run.ID); got.Snapshot.Limits == nil {
		t.Error("the run lost its limits when the suite was deleted")
	}
}

// The orchestrator enforces a run's limits from the usage the gateway
// attributed to it. This reads the figures through the real request log: a
// whole run, one case (the agent turn and the judge, by exact id so case 2 does
// not take case 21's calls), and nothing from another run.
func TestEvaluationUsageReadsRunAndCaseSpendFromTheRequestLog(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	plantModelPrice(t, pool, "openai", "gpt-4o", "1.00000000", "1.00000000")
	seedEvalRun(t, pool, 1, 4001, now.Add(-time.Minute))
	seedEvalRun(t, pool, 11, 4001, now.Add(-time.Minute))
	plantRunCall(t, pool, 1, 7, "eval:1:case:2", now, "gpt-4o", 200, 100, 50)
	plantRunCall(t, pool, 1, 7, "eval:1:judge:2", now, "gpt-4o", 200, 30, 5)
	plantRunCall(t, pool, 1, 7, "eval:1:case:21", now, "gpt-4o", 200, 1000, 500)
	plantRunCall(t, pool, 1, 7, "eval:11:case:2", now, "gpt-4o", 200, 9999, 9999)

	run, err := repo.EvaluationUsage(ctx, "1", "1", "")
	require.NoError(t, err)
	require.EqualValues(t, 100+50+30+5+1000+500, run.Tokens, "the whole run, and not run 11")
	require.Greater(t, run.CostUSD, 0.0)

	one, err := repo.EvaluationUsage(ctx, "1", "1", "2")
	require.NoError(t, err)
	require.EqualValues(t, 100+50+30+5, one.Tokens, "case 2 is the agent turn and the judge, not case 21")

	none, err := repo.EvaluationUsage(ctx, "1", "1", "3")
	require.NoError(t, err)
	require.EqualValues(t, 0, none.Tokens)
}

// A resumed run re-saves its results. Saving `pending_human` over a score a
// reviewer recorded is refused by the upsert itself.
func TestEvalSaveResultNeverMovesAHumanScoreBackToPending(t *testing.T) {
	fixture := newEvalRunFixture(t)
	router := humanScoreRouter(fixture)
	dimensionID := fixture.seedHumanDimension(t)
	run := fixture.startHumanRun(t, router, dimensionID)
	ctx := context.Background()

	if response := callEvalRoute(t, router, http.MethodPost, "/eval_human_scores/prompt_lib/1/"+run.ID,
		scoreBody(fixture.caseIDs[0], dimensionID, 4, "")); response.Code != http.StatusCreated {
		t.Fatalf("score: %d %s", response.Code, response.Body.String())
	}
	if err := fixture.repo.SaveResult(ctx, "1", evaluation.RunResult{
		RunID: run.ID, DatasetCaseID: fixture.caseIDs[0], DimensionID: dimensionID,
		Status: evaluation.ResultStatusPendingHuman,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	results, _, err := fixture.repo.ListResults(ctx, "1", run.ID, evaluation.ResultPage{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if result.DatasetCaseID == fixture.caseIDs[0] && (result.Status != evaluation.ResultStatusOK || result.NativeScore == nil) {
			t.Fatalf("the score was moved back: %+v", result)
		}
	}
}

// FinishRun computes the headline from the stored results, not from the number
// the caller read earlier: a score that landed in between is counted.
func TestEvalFinishRunRecomputesTheHeadlineFromStoredResults(t *testing.T) {
	fixture := newEvalRunFixture(t)
	router := humanScoreRouter(fixture)
	dimensionID := fixture.seedHumanDimension(t)
	body := fmt.Sprintf(`{"dataset_id":%q,"application_id":900,"application_version_id":%d,"dimension_ids":[%q]}`,
		fixture.dataset, fixture.version, dimensionID)
	started := callEvalRoute(t, router, http.MethodPost, "/eval_runs/prompt_lib/1", body)
	var run evaluation.Run
	if err := json.Unmarshal(started.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, caseID := range fixture.caseIDs {
		if err := fixture.repo.SaveResult(ctx, "1", evaluation.RunResult{
			RunID: run.ID, DatasetCaseID: caseID, DimensionID: dimensionID, Status: evaluation.ResultStatusPendingHuman,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.repo.ClaimRun(ctx, "1", run.ID); err != nil {
		t.Fatal(err)
	}
	// A reviewer scores while the run is still running; the run's own headline
	// read (nil) is stale by the time it finishes.
	if response := callEvalRoute(t, router, http.MethodPost, "/eval_human_scores/prompt_lib/1/"+run.ID,
		scoreBody(fixture.caseIDs[0], dimensionID, 5, "")); response.Code != http.StatusCreated {
		t.Fatalf("score: %d %s", response.Code, response.Body.String())
	}
	stale := 10.0
	if err := fixture.repo.FinishRun(ctx, "1", run.ID, evaluation.RunStatusFinished, &stale, ""); err != nil {
		t.Fatal(err)
	}
	if got := fixture.readRun(t, run.ID); got.HeadlineScore == nil || *got.HeadlineScore != 100 {
		t.Fatalf("headline = %v, want 100 from the stored score and not the stale 10", got.HeadlineScore)
	}
}
