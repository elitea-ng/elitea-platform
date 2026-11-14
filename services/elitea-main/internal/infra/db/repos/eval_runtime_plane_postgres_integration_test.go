package repos

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
)

// The runtime_plane execution mode against a REAL migrated tenant schema
// (tenant/0246): the suite's mode and controls, the run that freezes them, and
// the attribution of a case's agent run to its evaluation run.

func decodeSuite(t *testing.T, body []byte) evaluation.Suite {
	t.Helper()
	var suite evaluation.Suite
	require.NoError(t, json.Unmarshal(body, &suite))
	return suite
}

func TestEvalSuiteRuntimePlaneControlsRoundTripAndAreRefusedOnPredictBlocking(t *testing.T) {
	fixture := newEvalRunFixture(t)
	router := humanScoreRouter(fixture)

	created := callEvalRoute(t, router, http.MethodPost, "/eval_suites/prompt_lib/1",
		`{"name":"Agent runs","execution_mode":"runtime_plane","step_limit_per_case":12,`+
			`"time_limit_per_case_seconds":90,"store_tool_outputs":false}`)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	suite := decodeSuite(t, created.Body.Bytes())
	require.Equal(t, evaluation.ExecutionModeRuntimePlane, suite.ExecutionMode)
	require.NotNil(t, suite.StepLimitPerCase)
	require.Equal(t, 12, *suite.StepLimitPerCase)
	require.Equal(t, 90, *suite.TimeLimitPerCaseSeconds)
	require.False(t, suite.KeepsToolOutputs())

	// A suite that says nothing keeps predict_blocking and keeps tool outputs.
	plain := callEvalRoute(t, router, http.MethodPost, "/eval_suites/prompt_lib/1", `{"name":"Plain"}`)
	require.Equal(t, http.StatusCreated, plain.Code, plain.Body.String())
	plainSuite := decodeSuite(t, plain.Body.Bytes())
	require.Equal(t, evaluation.ExecutionModePredictBlocking, plainSuite.ExecutionMode)
	require.Nil(t, plainSuite.StepLimitPerCase)
	require.True(t, plainSuite.KeepsToolOutputs())

	// The controls mean nothing for predict_blocking: the handler refuses them,
	// and so does the table when something bypasses the handler.
	refused := callEvalRoute(t, router, http.MethodPost, "/eval_suites/prompt_lib/1",
		`{"name":"Wrong","step_limit_per_case":3}`)
	require.Equal(t, http.StatusBadRequest, refused.Code, refused.Body.String())
	_, err := fixture.pool.Exec(context.Background(),
		`INSERT INTO p_1.eval_suites (name, step_limit_per_case) VALUES ('bypass', 3)`)
	require.Error(t, err, "the table stored a step limit on a predict_blocking suite")

	// Switching a suite back to predict_blocking while keeping the controls is
	// refused; clearing them is accepted.
	update := callEvalRoute(t, router, http.MethodPut, "/eval_suite/prompt_lib/1/"+suite.ID,
		`{"name":"Agent runs","execution_mode":"predict_blocking","step_limit_per_case":12}`)
	require.Equal(t, http.StatusBadRequest, update.Code, update.Body.String())
	update = callEvalRoute(t, router, http.MethodPut, "/eval_suite/prompt_lib/1/"+suite.ID,
		`{"name":"Agent runs","execution_mode":"predict_blocking"}`)
	require.Equal(t, http.StatusOK, update.Code, update.Body.String())
	require.Nil(t, decodeSuite(t, update.Body.Bytes()).StepLimitPerCase)
}

func TestEvalRunStartFreezesTheExecutionMode(t *testing.T) {
	fixture := newEvalRunFixture(t)
	router := humanScoreRouter(fixture)
	dimensionID := fixture.seedAIDimension(t)

	created := callEvalRoute(t, router, http.MethodPost, "/eval_suites/prompt_lib/1",
		`{"name":"Agent runs","execution_mode":"runtime_plane","step_limit_per_case":8,"time_limit_per_case_seconds":60}`)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	suite := decodeSuite(t, created.Body.Bytes())

	start := func(extra string) evaluation.Run {
		body := fmt.Sprintf(`{"dataset_id":%q,"application_id":900,"application_version_id":%d,"dimension_ids":[%q]%s}`,
			fixture.dataset, fixture.version, dimensionID, extra)
		response := callEvalRoute(t, router, http.MethodPost, "/eval_runs/prompt_lib/1", body)
		require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
		var run evaluation.Run
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &run))
		return fixture.readRun(t, run.ID)
	}

	bySuite := start(fmt.Sprintf(`,"suite_id":%q`, suite.ID))
	require.Equal(t, evaluation.ExecutionModeRuntimePlane, bySuite.ExecutionMode)
	require.NotNil(t, bySuite.Snapshot.Limits)
	require.Equal(t, 8, *bySuite.Snapshot.Limits.StepLimitPerCase)
	require.Equal(t, 60, *bySuite.Snapshot.Limits.TimeLimitPerCaseSeconds)

	// The start may override the suite, and the run then carries no control
	// that it cannot enforce.
	overridden := start(fmt.Sprintf(`,"suite_id":%q,"execution_mode":"predict_blocking"`, suite.ID))
	require.Equal(t, evaluation.ExecutionModePredictBlocking, overridden.ExecutionMode)
	require.Nil(t, overridden.Snapshot.Limits.StepLimitPerCase)

	// Nothing set: nothing changes silently.
	require.Equal(t, evaluation.ExecutionModePredictBlocking, start("").ExecutionMode)
	require.Equal(t, evaluation.ExecutionModeRuntimePlane, start(`,"execution_mode":"runtime_plane"`).ExecutionMode)

	// A runtime_plane run executes the agent, so it needs one.
	noApp := fmt.Sprintf(`{"dataset_id":%q,"application_version_id":%d,"dimension_ids":[%q],"execution_mode":"runtime_plane"}`,
		fixture.dataset, fixture.version, dimensionID)
	require.Equal(t, http.StatusBadRequest,
		callEvalRoute(t, router, http.MethodPost, "/eval_runs/prompt_lib/1", noApp).Code)
}

// The worker signs a case's model calls with the runtime execution id, not with
// `eval:<run>:case:<case>`. The recorded execution is what attributes them, so
// the run limits and the consumption card count an agent run's spend.
func TestEvaluationUsageAttributesTheCaseRunsExecution(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	runs := &EvalRunsRepo{pool: pool}
	ctx := context.Background()
	now := time.Now().UTC()

	plantModelPrice(t, pool, "openai", "gpt-4o", "1.00000000", "1.00000000")
	seedEvalRun(t, pool, 1, 4001, now.Add(-time.Minute))
	seedEvalRun(t, pool, 2, 4001, now.Add(-time.Minute))

	require.NoError(t, runs.RecordCaseRun(ctx, "1", "1", "21", "exec-a", "00000000-0000-4000-8000-000000000001"))
	require.NoError(t, runs.RecordCaseRun(ctx, "1", "1", "21", "exec-a", ""), "recording twice is idempotent")
	require.NoError(t, runs.RecordCaseRun(ctx, "1", "1", "22", "exec-b", ""))

	plantRunCall(t, pool, 1, 7, "exec-a", now, "gpt-4o", 200, 100, 50)
	plantRunCall(t, pool, 1, 7, "exec-a", now, "gpt-4o", 200, 200, 50)
	plantRunCall(t, pool, 1, 7, "exec-b", now, "gpt-4o", 200, 1000, 0)
	plantRunCall(t, pool, 1, 7, "eval:1:judge:21", now, "gpt-4o", 200, 30, 5)
	plantRunCall(t, pool, 1, 7, "exec-other", now, "gpt-4o", 200, 9999, 9999)

	whole, err := repo.EvaluationUsage(ctx, "1", "1", "")
	require.NoError(t, err)
	require.EqualValues(t, 150+250+1000+35, whole.Tokens, "both executions and the judge, and not another run's execution")

	one, err := repo.EvaluationUsage(ctx, "1", "1", "21")
	require.NoError(t, err)
	require.EqualValues(t, 150+250+35, one.Tokens, "case 21 is its execution and its judge, not case 22's")

	other, err := repo.EvaluationUsage(ctx, "1", "2", "")
	require.NoError(t, err)
	require.EqualValues(t, 0, other.Tokens, "run 2 recorded no execution")

	analytics, err := repo.GetEvaluationRunAnalytics(ctx, "1", "1")
	require.NoError(t, err)
	require.True(t, analytics.Available)
	require.EqualValues(t, 3, analytics.Agent.LLMCalls, "the agent role is the runtime executions")
	require.EqualValues(t, 1, analytics.Judge.LLMCalls)
	require.EqualValues(t, 4, analytics.Totals.LLMCalls)
	require.Len(t, analytics.ByCase, 2)
	require.Equal(t, "21", analytics.ByCase[0].CaseID)
	require.EqualValues(t, 2, analytics.ByCase[0].Agent.LLMCalls)
	require.EqualValues(t, 1, analytics.ByCase[0].Judge.LLMCalls)
	require.Equal(t, "22", analytics.ByCase[1].CaseID)
	require.EqualValues(t, 1, analytics.ByCase[1].Agent.LLMCalls)
}
