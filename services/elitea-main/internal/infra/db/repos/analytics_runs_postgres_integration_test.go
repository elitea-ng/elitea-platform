package repos

// The run-level analytics reads and the trigger-origin filter (legacy issues
// 6667, 6677, 6678, 6802, 6816, 6817 and 6881), against the migrated corpus.
//
// Every case runs on the ledgered template, so shared 0100 (execution_id),
// 0119 (tool_call_records) and 0140 (trigger_origin and the ledger row the
// evaluation boundary reads) are exercised as DDL. It SKIPS without
// ELITEA_TEST_DATABASE_URL, so each case asserts a figure a skip could not
// produce.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/analytics"
)

// seedOriginExecution writes one execution_jobs row with a trigger origin and
// an admission time.
func seedOriginExecution(t *testing.T, pool *pgxpool.Pool, projectID int64, executionID, origin string, admittedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles
    (input_bundle_id, immutable_version, resource_project_id, media_type,
     manifest_digest, manifest_size, manifest_bytes, created_by)
VALUES ($1, 'admission:'||$1, $2, 'application/x-protobuf',
        decode(repeat('61', 32), 'hex'), 1, decode('00', 'hex'), 'actor-1')
ON CONFLICT DO NOTHING`, "bundle-"+executionID, projectID)
	require.NoError(t, err, "seed input bundle")
	_, err = pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state, admitted_at, trigger_origin
) VALUES (
    $1, 1, 'cmd-'||$1, $2::bigint::text, $2::integer, $2::integer, '8', '8',
    'agent.execute.application.v1', 'v1', $3,
    decode(repeat('61', 32), 'hex'), 'scope-'||$1, 'key-'||$1, 'SUCCEEDED', 'RUNNING',
    $4, $5
)`, executionID, projectID, "bundle-"+executionID, admittedAt, origin)
	require.NoError(t, err, "seed execution job")
}

// plantRunCall writes one request-log row.
func plantRunCall(t *testing.T, pool *pgxpool.Pool, projectID int64, userID int, executionID string,
	at time.Time, model string, status int, prompt, completion int64,
) {
	t.Helper()
	var execution any
	if executionID != "" {
		execution = executionID
	}
	errorCode := ""
	if status >= 400 {
		errorCode = "upstream_error"
	}
	_, err := pool.Exec(context.Background(), `
INSERT INTO gateway.llm_request_logs
    (occurred_at, project_id, user_id, route, method, status, duration_ms,
     provider, model, prompt_tokens, completion_tokens, execution_id, error_code)
VALUES ($1, $2, $3, '/llm/v1/chat/completions', 'POST', $4::smallint, 200,
        'openai', $5, $6, $7, $8, $9)`,
		at, projectID, userID, status, model, prompt, completion, execution, errorCode)
	require.NoError(t, err, "plant request log row")
}

// recordRunToolCall records one explicit tool call made by executionID.
func recordRunToolCall(t *testing.T, pool *pgxpool.Pool, executionID string, isError bool) {
	t.Helper()
	started := time.Now().UTC().Add(-time.Minute)
	recordToolCallForTest(t, pool, ToolCallRecord{
		ProjectID: 1, Source: ToolCallSourceExplicitRun, SourceRef: "ref-" + executionID,
		ToolkitID: 7, ToolkitType: "github", ToolName: "search",
		StartedAt: started, FinishedAt: started.Add(100 * time.Millisecond),
		IsError: isError, ActorUserID: 7, ExecutionID: executionID,
	})
}

func numberString(t *testing.T, value *json.Number) string {
	t.Helper()
	require.NotNil(t, value, "a money field is absent")
	return value.String()
}

/* ── one execution ─────────────────────────────────────────────────────── */

// The per-execution sum includes the run's own calls and its child
// attributions, and nothing else: not another execution, not an id that only
// shares a prefix without the `:` separator, not another project's calls.
func TestExecutionAnalyticsSumsTheRunAndItsChildAttributions(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	plantModelPrice(t, pool, "openai", "gpt-4o", "2.00000000", "10.00000000")
	seedOriginExecution(t, pool, 1, "exec_run", "manual", now.Add(-time.Minute))

	plantRunCall(t, pool, 1, 7, "exec_run", now, "gpt-4o", 200, 100, 10)
	plantRunCall(t, pool, 1, 7, "exec_run", now, "gpt-4o", 500, 50, 0)
	plantRunCall(t, pool, 1, 7, "exec_run:child-1", now, "gpt-4o-mini", 200, 20, 5)
	// Not part of the run: another execution, a look-alike id with no
	// separator (the `_` in the id must not act as a LIKE wildcard), and the
	// same id in another project's log.
	plantRunCall(t, pool, 1, 7, "exec_runner", now, "gpt-4o", 200, 1000, 1000)
	plantRunCall(t, pool, 1, 7, "execXrun:child", now, "gpt-4o", 200, 1000, 1000)
	plantRunCall(t, pool, 1, 7, "", now, "gpt-4o", 200, 1000, 1000)
	recordRunToolCall(t, pool, "exec_run", false)
	recordRunToolCall(t, pool, "exec_run:child-1", true)
	recordRunToolCall(t, pool, "exec_runner", false)

	result, err := repo.GetExecutionAnalytics(ctx, "1", "exec_run")
	require.NoError(t, err)
	require.True(t, result.Available)
	require.Equal(t, "manual", result.TriggerOrigin)
	require.Equal(t, "agent.execute.application.v1", result.CapabilityID)
	require.NotNil(t, result.Totals)

	totals := *result.Totals
	require.EqualValues(t, 3, totals.LLMCalls, "the run's two calls and its child's one")
	require.EqualValues(t, 170, totals.PromptTokens)
	require.EqualValues(t, 15, totals.CompletionTokens)
	require.EqualValues(t, 185, totals.TotalTokens)
	require.EqualValues(t, 1, totals.Errors)
	require.EqualValues(t, 1, result.ChildAttributions)
	// gpt-4o-mini has no catalogue price, so one call is unpriced and the
	// money covers the two gpt-4o calls: (150*2 + 10*10) / 1e6.
	require.EqualValues(t, 1, totals.UnpricedCalls)
	require.Equal(t, "0.000300000000", numberString(t, totals.InputCost))
	require.Equal(t, "0.000100000000", numberString(t, totals.OutputCost))
	require.Equal(t, "0.000400000000", numberString(t, totals.TotalCost))

	require.Len(t, result.ByModel, 2)
	require.Equal(t, "gpt-4o", result.ByModel[0].Model)
	require.EqualValues(t, 2, result.ByModel[0].LLMCalls)
	require.Nil(t, result.ByModel[1].TotalCost, "an unpriced model publishes no money")

	require.Len(t, result.ByUser, 1)
	require.Equal(t, "7", result.ByUser[0].UserID)
	require.EqualValues(t, 3, result.ByUser[0].LLMCalls)

	require.Len(t, result.ByErrorCode, 1)
	require.Equal(t, "upstream_error", result.ByErrorCode[0].ErrorCode)

	require.True(t, result.ToolsAvailable)
	require.Len(t, result.Tools, 1)
	require.EqualValues(t, 2, result.Tools[0].RunCount, "the run's tool call and its child's")
	require.EqualValues(t, 1, result.Tools[0].ErrorCount)

	_, err = repo.GetExecutionAnalytics(ctx, "2", "exec_run")
	require.True(t, errors.Is(err, analytics.ErrNotFound), "another project must not resolve the id: %v", err)
	_, err = repo.GetExecutionAnalytics(ctx, "1", "missing")
	require.True(t, errors.Is(err, analytics.ErrNotFound), "%v", err)
	_, err = repo.GetExecutionAnalytics(ctx, "1", "bad id")
	require.True(t, errors.Is(err, analytics.ErrBadID), "%v", err)
}

// A run admitted before shared 0100 has no attributed call. It reports
// UNAVAILABLE with no figures, never zero totals.
func TestExecutionAnalyticsBeforeAttributionIsUnavailable(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)

	seedOriginExecution(t, pool, 1, "exec-ancient", "manual", time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))
	result, err := repo.GetExecutionAnalytics(context.Background(), "1", "exec-ancient")
	require.NoError(t, err)
	require.False(t, result.Available)
	require.Equal(t, analytics.UnavailableBeforeAttribution, result.UnavailableReason)
	require.Nil(t, result.Totals, "an unavailable run publishes no totals")

	body, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(body), `"totals"`)
	require.NotContains(t, string(body), `"llm_calls"`)
}

// A run with no call in the log is either a run that made none (zero is the
// measurement) or a run whose calls were pruned (unavailable). The log's
// horizon tells them apart.
func TestExecutionAnalyticsTellsANoCallRunFromAPrunedOne(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	// The template ran shared 0100 moments ago, so the pruned run is placed
	// between that boundary and the oldest retained log row.
	boundary, err := migrationAppliedAt(ctx, pool, requestLogExecutionIDMigration)
	require.NoError(t, err)
	require.False(t, boundary.IsZero(), "the ledger has no row for shared 0100")
	oldest := now
	plantRunCall(t, pool, 1, 7, "", oldest, "gpt-4o", 200, 1, 1)

	seedOriginExecution(t, pool, 1, "exec-pruned", "manual", boundary.Add(oldest.Sub(boundary)/2))
	pruned, err := repo.GetExecutionAnalytics(ctx, "1", "exec-pruned")
	require.NoError(t, err)
	require.False(t, pruned.Available)
	require.Equal(t, analytics.UnavailableLogPruned, pruned.UnavailableReason)

	seedOriginExecution(t, pool, 1, "exec-silent", "manual", oldest.Add(time.Millisecond))
	silent, err := repo.GetExecutionAnalytics(ctx, "1", "exec-silent")
	require.NoError(t, err)
	require.True(t, silent.Available)
	require.NotNil(t, silent.Totals)
	require.EqualValues(t, 0, silent.Totals.LLMCalls)
}

/* ── automated activity (legacy issues 6802 and 6881) ──────────────────── */

// A scheduled run executes as its author. The author is NOT an active user,
// an adopter or a row on the Users tab because of it; the project totals
// still count every call, and the run appears in the automated bucket.
func TestScheduledRunIsExcludedFromActiveUsersAndAdopters(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	// User 7 chats. User 8 only owns a schedule, a webhook and an index.
	seedOriginExecution(t, pool, 1, "exec-chat", "manual", now.Add(-time.Minute))
	seedOriginExecution(t, pool, 1, "exec-cron", "schedule", now.Add(-time.Minute))
	seedOriginExecution(t, pool, 1, "exec-hook", "webhook", now.Add(-time.Minute))
	seedOriginExecution(t, pool, 1, "exec-index", "index", now.Add(-time.Minute))
	seedOriginExecution(t, pool, 1, "exec-mcp", "api", now.Add(-time.Minute))
	plantRunCall(t, pool, 1, 7, "exec-chat", now, "gpt-4o", 200, 10, 10)
	plantRunCall(t, pool, 1, 8, "exec-cron", now, "gpt-4o", 200, 100, 100)
	plantRunCall(t, pool, 1, 8, "exec-cron", now, "gpt-4o", 200, 100, 100)
	plantRunCall(t, pool, 1, 8, "exec-hook", now, "gpt-4o", 200, 100, 100)
	plantRunCall(t, pool, 1, 8, "exec-index", now, "gpt-4o", 200, 100, 100)
	// An API run is a person acting through a client, so user 9 counts.
	plantRunCall(t, pool, 1, 9, "exec-mcp", now, "gpt-4o", 200, 1, 1)

	params := analytics.QueryParams{ProjectID: "1", From: now.Add(-time.Hour), To: now.Add(time.Hour)}
	summary, err := repo.GetUsageSummary(ctx, params)
	require.NoError(t, err)

	require.EqualValues(t, 6, summary.TotalRuns, "the call total counts unattended calls")
	require.EqualValues(t, 20+800+2, summary.TotalTokens, "the token total counts unattended calls")
	require.EqualValues(t, 2, summary.ActiveUsers, "only users 7 and 9 acted")
	require.Len(t, summary.DailyActivity, 1)
	require.EqualValues(t, 2, summary.DailyActivity[0].ActiveUsers)
	require.EqualValues(t, 6, summary.DailyActivity[0].LLMCalls)
	for _, user := range summary.TopUsers {
		require.NotEqual(t, "8", user.UserID, "the schedule's author is not an adopter")
	}
	require.Len(t, summary.TopUsers, 2)

	byOrigin := map[string]analytics.AutomatedActivity{}
	for _, row := range summary.Automated {
		byOrigin[row.TriggerOrigin] = row
	}
	require.Len(t, byOrigin, 3)
	require.EqualValues(t, 2, byOrigin["schedule"].LLMCalls)
	require.EqualValues(t, 1, byOrigin["schedule"].Executions)
	require.EqualValues(t, 1, byOrigin["schedule"].Users)
	require.EqualValues(t, 400, byOrigin["schedule"].TotalTokens)
	require.EqualValues(t, 1, byOrigin["webhook"].LLMCalls)
	require.EqualValues(t, 1, byOrigin["index"].LLMCalls)

	users, truncated, err := repo.GetUserActivity(ctx, params)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Len(t, users, 2)
	for _, user := range users {
		require.NotEqual(t, "8", user.UserID, "the Users tab lists people, not their cron jobs")
	}
}

// A member whose only calls are a schedule's is not an active member, so
// the adoption numerator does not count them.
func TestScheduledRunDoesNotCountTowardAdoption(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	// The membership table is pylon-owned and the ledgered template does not
	// create it, so the test declares the columns projectAdoption reads.
	_, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS public.auth_core__project_user_role (
    project_id integer NOT NULL,
    user_id    integer NOT NULL,
    role_id    integer NOT NULL
);
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
VALUES (1, 7, 1), (1, 8, 1)`)
	require.NoError(t, err, "seed project membership")

	seedOriginExecution(t, pool, 1, "exec-cron-adoption", "schedule", now.Add(-time.Minute))
	plantRunCall(t, pool, 1, 7, "", now, "gpt-4o", 200, 1, 1)
	plantRunCall(t, pool, 1, 8, "exec-cron-adoption", now, "gpt-4o", 200, 1, 1)

	summary, err := repo.GetUsageSummary(ctx, analytics.QueryParams{ProjectID: "1", From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	require.NoError(t, err)
	require.NotNil(t, summary.ActiveMembers)
	require.NotNil(t, summary.TotalProjectUsers)
	require.EqualValues(t, 2, *summary.TotalProjectUsers)
	require.EqualValues(t, 1, *summary.ActiveMembers, "the schedule's author did not use AI")
}

/* ── one evaluation run ────────────────────────────────────────────────── */

func seedEvalRun(t *testing.T, pool *pgxpool.Pool, runID, applicationID int, createdAt time.Time) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO p_1.eval_datasets (id, name) VALUES (900, 'dataset') ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err, "seed dataset")
	_, err = pool.Exec(ctx, `
INSERT INTO p_1.eval_runs (id, dataset_id, application_id, application_version_id, created_by, status, created_at)
VALUES ($1, 900, $2, $2 + 1, 7, 'finished', $3)`, runID, applicationID, createdAt)
	require.NoError(t, err, "seed evaluation run")
}

// Legacy issues 6677 and 6817: one run's spend, with the agent turn and the
// judge kept as two roles, per case, and nothing from run 11 leaking into
// run 1 through the shared prefix digit.
func TestEvaluationRunAnalyticsSplitsAgentAndJudgePerCase(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	plantModelPrice(t, pool, "openai", "gpt-4o", "1.00000000", "1.00000000")
	seedEvalRun(t, pool, 1, 4001, now.Add(-time.Minute))
	seedEvalRun(t, pool, 11, 4001, now.Add(-time.Minute))

	plantRunCall(t, pool, 1, 7, "eval:1:case:21", now, "gpt-4o", 200, 100, 50)
	plantRunCall(t, pool, 1, 7, "eval:1:judge:21", now, "gpt-4o", 200, 30, 5)
	plantRunCall(t, pool, 1, 7, "eval:1:judge:21", now, "gpt-4o", 500, 30, 0)
	plantRunCall(t, pool, 1, 7, "eval:1:case:22", now, "gpt-4o", 200, 200, 100)
	plantRunCall(t, pool, 1, 7, "eval:11:case:21", now, "gpt-4o", 200, 9999, 9999)

	result, err := repo.GetEvaluationRunAnalytics(ctx, "1", "1")
	require.NoError(t, err)
	require.True(t, result.Available)
	require.NotNil(t, result.ApplicationID)
	require.Equal(t, 4001, *result.ApplicationID)

	require.EqualValues(t, 4, result.Totals.LLMCalls, "run 11's call must not leak into run 1")
	require.EqualValues(t, 360, result.Totals.PromptTokens)
	require.EqualValues(t, 2, result.Agent.LLMCalls)
	require.EqualValues(t, 300, result.Agent.PromptTokens)
	require.EqualValues(t, 2, result.Judge.LLMCalls)
	require.EqualValues(t, 60, result.Judge.PromptTokens)
	require.EqualValues(t, 1, result.Judge.Errors)
	require.Equal(t, "0.000450000000", numberString(t, result.Agent.TotalCost))
	require.Equal(t, "0.000065000000", numberString(t, result.Judge.TotalCost))
	require.Equal(t, "0.000515000000", numberString(t, result.Totals.TotalCost))

	require.Len(t, result.ByCase, 2)
	require.Equal(t, "21", result.ByCase[0].CaseID)
	require.EqualValues(t, 1, result.ByCase[0].Agent.LLMCalls)
	require.EqualValues(t, 2, result.ByCase[0].Judge.LLMCalls)
	require.Equal(t, "22", result.ByCase[1].CaseID)
	require.EqualValues(t, 1, result.ByCase[1].Agent.LLMCalls)
	require.EqualValues(t, 0, result.ByCase[1].Judge.LLMCalls)

	_, err = repo.GetEvaluationRunAnalytics(ctx, "1", "404")
	require.True(t, errors.Is(err, analytics.ErrNotFound), "%v", err)
	_, err = repo.GetEvaluationRunAnalytics(ctx, "1", "x")
	require.True(t, errors.Is(err, analytics.ErrBadID), "%v", err)
}

// A run created before the release that signs evaluation calls is
// unavailable, never zero.
func TestEvaluationRunAnalyticsBeforeAttributionIsUnavailable(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)

	seedEvalRun(t, pool, 5, 4001, time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))
	result, err := repo.GetEvaluationRunAnalytics(context.Background(), "1", "5")
	require.NoError(t, err)
	require.False(t, result.Available)
	require.Equal(t, analytics.UnavailableBeforeAttribution, result.UnavailableReason)
	require.Nil(t, result.Totals)
	require.Nil(t, result.Agent)
	require.Nil(t, result.Judge)
}

// Legacy issue 6678: the cost estimate attributes evaluation calls, which the
// execution_jobs join of the agent split cannot see, as an evaluation term
// with the two roles and a per-agent row.
func TestCostEstimateReportsEvaluationSpend(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	now := time.Now().UTC()
	router := costAttributionRouter(pool, now)

	plantModelPrice(t, pool, "openai", "gpt-4o", "1.00000000", "1.00000000")
	seedPylonApplicationsTable(t, pool)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO p_1.applications (id, name, owner_id) VALUES (4001, 'Graded Agent', 1)`)
	require.NoError(t, err)
	seedEvalRun(t, pool, 1, 4001, now.Add(-time.Minute))
	seedEvalRun(t, pool, 2, 4001, now.Add(-time.Minute))

	plantRunCall(t, pool, 1, 7, "eval:1:case:21", now, "gpt-4o", 200, 100, 50)
	plantRunCall(t, pool, 1, 7, "eval:1:judge:21", now, "gpt-4o", 200, 30, 5)
	plantRunCall(t, pool, 1, 7, "eval:2:case:21", now, "gpt-4o", 200, 10, 10)
	seedUnattributedRequests(t, pool, 2)

	estimate := costAttributionEstimate(t, router, now)
	// The evaluation calls name no execution, so the agent split counts them
	// as unattributed — which is why the evaluation block exists.
	wantCostAttributionNumber(t, estimate, "unattributed_agent_calls", "5")

	evaluation, ok := estimate["evaluation"].(map[string]any)
	require.True(t, ok, "estimate.evaluation missing: %v", estimate)
	require.Equal(t, true, evaluation["evaluation_dimension_available"])
	wantCostAttributionNumber(t, evaluation, "runs", "2")

	totals := evaluation["totals"].(map[string]any)
	wantCostAttributionNumber(t, totals, "calls", "3")
	wantCostAttributionNumber(t, totals, "total_cost", "0.000205000000")
	agent := evaluation["agent"].(map[string]any)
	wantCostAttributionNumber(t, agent, "calls", "2")
	wantCostAttributionNumber(t, agent, "total_cost", "0.000170000000")
	judge := evaluation["judge"].(map[string]any)
	wantCostAttributionNumber(t, judge, "calls", "1")
	wantCostAttributionNumber(t, judge, "total_cost", "0.000035000000")

	byAgent := costAttributionRows(t, evaluation, "by_agent")
	require.Len(t, byAgent, 1)
	require.Equal(t, "4001", byAgent[0]["application_id"])
	require.Equal(t, "Graded Agent", byAgent[0]["name"])
	wantCostAttributionNumber(t, byAgent[0], "runs", "2")
	wantCostAttributionNumber(t, byAgent[0], "calls", "3")
}
