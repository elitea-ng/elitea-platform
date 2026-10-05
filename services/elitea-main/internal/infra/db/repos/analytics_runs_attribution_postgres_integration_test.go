package repos

// The attribution rules of the run-level reads, against the migrated corpus:
// who an execution id can move out of the active-user figures, which rows a
// run and its roles count, and when a figure is unavailable rather than zero.
// It SKIPS without ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/analytics"
)

/* ── the execution id cannot hide a person ─────────────────────────────── */

// A project member who sends the id of somebody else's scheduled run, or of
// a run that settled long ago, is still an active user. Only the run's actor,
// inside the run's lifetime, makes an unattended call.
func TestForgedExecutionIDDoesNotHideAnActiveUser(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	settledLongAgo := now.Add(-2 * time.Hour)

	// User 8 owns a live schedule run and a schedule run that settled two
	// hours ago.
	seedOriginExecutionAs(t, pool, 1, "exec-cron-live", "schedule", "8", now.Add(-time.Minute), nil)
	seedOriginExecutionAs(t, pool, 1, "exec-cron-old", "schedule", "8", now.Add(-3*time.Hour), &settledLongAgo)

	// The run's own call.
	plantRunCall(t, pool, 1, 8, "exec-cron-live", now, "gpt-4o", 200, 10, 10)
	// User 7 names user 8's run: still user 7's own call.
	plantRunCall(t, pool, 1, 7, "exec-cron-live", now, "gpt-4o", 200, 1, 1)
	// User 8 names their own run that settled two hours ago: a person's call.
	plantRunCall(t, pool, 1, 8, "exec-cron-old", now, "gpt-4o", 200, 1, 1)

	params := analytics.QueryParams{ProjectID: "1", From: now.Add(-time.Hour), To: now.Add(time.Hour)}
	summary, err := repo.GetUsageSummary(ctx, params)
	require.NoError(t, err)
	require.EqualValues(t, 3, summary.TotalRuns)
	require.EqualValues(t, 2, summary.ActiveUsers, "users 7 and 8 both made a call of their own")

	require.Len(t, summary.Automated, 1)
	require.Equal(t, "schedule", summary.Automated[0].TriggerOrigin)
	require.EqualValues(t, 1, summary.Automated[0].LLMCalls, "only the run's own call is unattended")
	require.EqualValues(t, 1, summary.Automated[0].Executions)
	require.EqualValues(t, 1, summary.Automated[0].Users)

	users, _, err := repo.GetUserActivity(ctx, params)
	require.NoError(t, err)
	byUser := map[string]analytics.UserActivity{}
	for _, user := range users {
		byUser[user.UserID] = user
	}
	require.Contains(t, byUser, "7", "the forger stays on the Users tab")
	require.Contains(t, byUser, "8")
	require.EqualValues(t, 1, byUser["7"].RunCount)
	require.EqualValues(t, 1, byUser["8"].RunCount, "user 8's call outside the run's lifetime is theirs")
}

/* ── the automated bucket is a share of the KPI totals ─────────────────── */

// kpis.llm_calls counts completed calls. The automated bucket counts the
// same row set, so the human share (llm_calls minus the buckets) is never
// negative. Failed attempts are reported as errors.
func TestAutomatedActivityIsAShareOfTheCompletedTotals(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	seedOriginExecution(t, pool, 1, "exec-cron-flaky", "schedule", now.Add(-time.Minute))
	seedOriginExecution(t, pool, 1, "exec-cron-failed", "schedule", now.Add(-time.Minute))
	for i := 0; i < 6; i++ {
		plantRunCall(t, pool, 1, 8, "exec-cron-flaky", now, "gpt-4o", 200, 10, 10)
	}
	plantRunCall(t, pool, 1, 8, "exec-cron-flaky", now, "gpt-4o", 429, 10, 0)
	plantRunCall(t, pool, 1, 8, "exec-cron-flaky", now, "gpt-4o", 500, 10, 0)
	plantRunCall(t, pool, 1, 8, "exec-cron-flaky", now, "gpt-4o", 500, 10, 0)
	plantRunCall(t, pool, 1, 8, "exec-cron-flaky", now, "gpt-4o", 500, 10, 0)
	// A run whose every attempt failed is still a run in the bucket.
	plantRunCall(t, pool, 1, 8, "exec-cron-failed", now, "gpt-4o", 500, 10, 0)

	summary, err := repo.GetUsageSummary(ctx, analytics.QueryParams{ProjectID: "1", From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	require.NoError(t, err)
	require.EqualValues(t, 6, summary.TotalRuns, "the KPI counts completed calls")

	require.Len(t, summary.Automated, 1)
	bucket := summary.Automated[0]
	require.EqualValues(t, 6, bucket.LLMCalls, "the bucket counts the KPI's row set")
	require.EqualValues(t, 120, bucket.TotalTokens)
	require.EqualValues(t, 5, bucket.Errors, "failed attempts are reported as errors")
	require.EqualValues(t, 2, bucket.Executions, "a run with only failed attempts is still a run")
	require.LessOrEqual(t, bucket.LLMCalls, summary.TotalRuns)
}

/* ── one evaluation run ────────────────────────────────────────────────── */

// The agent and judge roles use the run total's row set: a row on a
// non-inference route, or outside the run's lifetime, is in none of them, so
// agent + judge = totals and both agree with by_case.
func TestEvaluationRunRolesAddUpToTheTotal(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	seedEvalRun(t, pool, 3, 4001, now.Add(-time.Minute))
	plantRunCall(t, pool, 1, 7, "eval:3:case:21", now, "gpt-4o", 200, 100, 50)
	plantRunCall(t, pool, 1, 7, "eval:3:judge:21", now, "gpt-4o", 200, 30, 5)
	plantRunCallOnRoute(t, pool, 1, 7, "eval:3:judge:21", "/llm/v1/models", now, "gpt-4o", 200, 999, 999)
	plantRunCallOnRoute(t, pool, 1, 7, "eval:3:case:21", "/llm/v1/messages/count_tokens", now, "gpt-4o", 200, 999, 999)
	plantRunCall(t, pool, 1, 7, "eval:3:judge:21", now.Add(-time.Hour), "gpt-4o", 200, 999, 999)

	result, err := repo.GetEvaluationRunAnalytics(ctx, "1", "3")
	require.NoError(t, err)
	require.True(t, result.Available)
	require.EqualValues(t, 2, result.Totals.LLMCalls)
	require.EqualValues(t, 1, result.Agent.LLMCalls)
	require.EqualValues(t, 1, result.Judge.LLMCalls)
	require.EqualValues(t, result.Totals.PromptTokens, result.Agent.PromptTokens+result.Judge.PromptTokens)
	require.EqualValues(t, result.Totals.CompletionTokens, result.Agent.CompletionTokens+result.Judge.CompletionTokens)
	require.Len(t, result.ByCase, 1)
	require.Equal(t, result.Agent.LLMCalls, result.ByCase[0].Agent.LLMCalls)
	require.Equal(t, result.Judge.LLMCalls, result.ByCase[0].Judge.LLMCalls)
}

// A run created after the boundary that scored an answer but has no
// attributed call was run by a pod of the previous release. Its figures are
// unknown, not zero.
func TestEvaluationRunWithUnsignedCallsIsUnavailable(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	seedEvalRun(t, pool, 41, 4001, now.Add(-time.Minute))
	seedEvalRun(t, pool, 42, 4001, now.Add(-time.Minute))
	// dimension_id is deliberately not a foreign key (tenant 0132).
	_, err := pool.Exec(ctx, `
INSERT INTO p_1.eval_results (run_id, dataset_case_id, dimension_id, status, native_score, normalized_score)
VALUES (41, 21, 950, 'ok', 1, 100)`)
	require.NoError(t, err, "seed result")

	unsigned, err := repo.GetEvaluationRunAnalytics(ctx, "1", "41")
	require.NoError(t, err)
	require.False(t, unsigned.Available, "a run that scored an answer made calls; zero would be a lie")
	require.Equal(t, analytics.UnavailableBeforeAttribution, unsigned.UnavailableReason)
	require.Nil(t, unsigned.Totals)

	// A run that scored nothing and made no call is a measured zero.
	idle, err := repo.GetEvaluationRunAnalytics(ctx, "1", "42")
	require.NoError(t, err)
	require.True(t, idle.Available)
	require.EqualValues(t, 0, idle.Totals.LLMCalls)
}

// by_case is capped, and says so. The roles and the total still cover every
// case.
func TestEvaluationRunByCaseIsCapped(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	seedEvalRun(t, pool, 51, 4001, now.Add(-time.Minute))
	cases := evaluationCaseRowsLimit + 5
	_, err := pool.Exec(ctx, `
INSERT INTO gateway.llm_request_logs
    (occurred_at, project_id, user_id, route, method, status, duration_ms,
     provider, model, prompt_tokens, completion_tokens, execution_id, error_code)
SELECT $1, 1, 7, '/llm/v1/chat/completions', 'POST', 200, 10, 'openai', 'gpt-4o', 1, 1,
       'eval:51:case:' || lpad(n::text, 6, '0'), ''
FROM generate_series(1, $2) AS n`, now, cases)
	require.NoError(t, err, "plant one call per case")

	result, err := repo.GetEvaluationRunAnalytics(ctx, "1", "51")
	require.NoError(t, err)
	require.True(t, result.Available)
	require.EqualValues(t, cases, result.Totals.LLMCalls)
	require.Len(t, result.ByCase, evaluationCaseRowsLimit)
	require.True(t, result.ByCaseTruncated)
	require.Equal(t, "000001", result.ByCase[0].CaseID)
}

/* ── the evaluation estimate over a straddling window ─────────────────── */

// A window that starts before evaluation calls were attributed is partial:
// it reports attributed_since and partial=true rather than a complete figure.
func TestCostEstimateMarksAStraddlingEvaluationWindowPartial(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	now := time.Now().UTC()
	router := costAttributionRouter(pool, now)

	seedEvalRun(t, pool, 61, 4001, now.Add(-time.Minute))
	plantRunCall(t, pool, 1, 7, "eval:61:case:21", now, "gpt-4o", 200, 1, 1)

	// The estimate window is the last hour; the boundary is in the middle.
	since := now.Add(-30 * time.Minute).Truncate(time.Second)
	_, err := pool.Exec(context.Background(), `
UPDATE elitea_runtime.schema_migrations SET applied_at = $1
WHERE target_kind = 'shared' AND name = $2`, since, triggerOriginMigration)
	require.NoError(t, err)

	estimate := costAttributionEstimate(t, router, now)
	evaluation, ok := estimate["evaluation"].(map[string]any)
	require.True(t, ok, "estimate.evaluation missing: %v", estimate)
	require.Equal(t, true, evaluation["evaluation_dimension_available"])
	require.Equal(t, true, evaluation["partial"])
	require.Equal(t, since.Format(time.RFC3339), evaluation["attributed_since"])

	// A window wholly after the boundary is complete.
	_, err = pool.Exec(context.Background(), `
UPDATE elitea_runtime.schema_migrations SET applied_at = TIMESTAMPTZ '2010-01-01 00:00:00+00'
WHERE target_kind = 'shared' AND name = $1`, triggerOriginMigration)
	require.NoError(t, err)
	estimate = costAttributionEstimate(t, router, now)
	evaluation = estimate["evaluation"].(map[string]any)
	_, hasPartial := evaluation["partial"]
	require.False(t, hasPartial, "a complete window carries no partial flag")
}

/* ── the /llm edge's execution check ───────────────────────────────────── */

// The edge keeps an inbound execution id only for a live execution of the
// caller, in the caller's resolved project.
func TestExecutionAttributionVerifierAcceptsOnlyTheCallersLiveRun(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	verifier := NewExecutionAttributionVerifier(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	settledRecently := now.Add(-time.Minute)
	settledLongAgo := now.Add(-time.Hour)

	seedOriginExecutionAs(t, pool, 1, "exec-live", "schedule", "8", now.Add(-time.Minute), nil)
	seedOriginExecutionAs(t, pool, 1, "exec-just-settled", "schedule", "8", now.Add(-10*time.Minute), &settledRecently)
	seedOriginExecutionAs(t, pool, 1, "exec-settled", "schedule", "8", now.Add(-2*time.Hour), &settledLongAgo)

	cases := []struct {
		name, project, user, execution string
		want                           bool
	}{
		{"the actor's live run", "1", "8", "exec-live", true},
		{"the actor's run that settled moments ago", "1", "8", "exec-just-settled", true},
		{"another person names the run", "1", "7", "exec-live", false},
		{"the run from another project", "2", "8", "exec-live", false},
		{"a run that settled long ago", "1", "8", "exec-settled", false},
		{"an id nobody minted", "1", "8", "exec-missing", false},
		{"an unparseable project", "x", "8", "exec-live", false},
	}
	for _, tc := range cases {
		got, err := verifier.VerifyExecution(ctx, tc.project, tc.user, tc.execution)
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.want, got, tc.name)
	}
}
