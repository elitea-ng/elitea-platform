package repos

// Issue 875: cost attributed to an agent and to a tool, over the SAME
// execution-to-agent correlation GetAgentAnalytics reads (analytics_agents.go
// above), but priced — the way GetAgentAnalytics deliberately never is (see
// "Money is deliberately not read here" atop analytics.go).
//
// This drives the real HTTP handler (internal/api/v2/analytics.CostsHandler),
// not the unexported query functions it calls, and it runs on the migrated
// template so shared migrations 0100 (execution_id) and 0119
// (tool_call_records) are exercised as DDL. It reuses this package's own
// agent-analytics fixtures (seedAgentExecution, seedPylonApplicationsTable,
// seedUnattributedRequests) rather than duplicating them, and this package's
// own tool-analytics fixture (recordToolCallForTest) for the tool half.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/analytics"
)

func costAttributionRouter(pool *pgxpool.Pool, now time.Time) chi.Router {
	costs := handler.NewCostsHandler(pool).WithClock(func() time.Time { return now })
	router := chi.NewRouter()
	router.Get("/analytics_costs/prompt_lib/{projectID}", costs.Costs)
	return router
}

func costAttributionEstimate(t *testing.T, router chi.Router, now time.Time) map[string]any {
	t.Helper()
	target := fmt.Sprintf("/analytics_costs/prompt_lib/%d?date_from=%s&date_to=%s",
		agentAnalyticsProject,
		now.Add(-time.Hour).Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	decoder := json.NewDecoder(recorder.Body)
	// UseNumber so an exact decimal that survived a float64 round trip fails
	// rather than "nearly" passing — the same discipline
	// internal/api/v2/analytics's own cost tests apply.
	decoder.UseNumber()
	var body map[string]any
	require.NoError(t, decoder.Decode(&body))

	estimate, ok := body["estimate"].(map[string]any)
	require.True(t, ok, "estimate missing from %v", body)
	return estimate
}

func costAttributionRows(t *testing.T, estimate map[string]any, key string) []map[string]any {
	t.Helper()
	raw, _ := estimate[key].([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		row, ok := item.(map[string]any)
		require.True(t, ok, "%s row = %#v, want an object", key, item)
		rows = append(rows, row)
	}
	return rows
}

func wantCostAttributionNumber(t *testing.T, row map[string]any, field, want string) {
	t.Helper()
	number, ok := row[field].(json.Number)
	require.True(t, ok, "%s = %#v, want a JSON number", field, row[field])
	require.Equal(t, want, number.String(), "field %s", field)
}

func plantModelPrice(t *testing.T, pool *pgxpool.Pool, provider, model, inRate, outRate string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO gateway.gateway_models
    (provider, model_name, input_cost_per_1m_tokens, output_cost_per_1m_tokens)
VALUES ($1, $2, $3::numeric, $4::numeric)`, provider, model, inRate, outRate)
	require.NoError(t, err, "plant model price")
}

// TestCostEstimateAttributesSpendToTheAgent is the assertion issue 875 exists
// for: the SAME execution seedAgentExecution wires to an agent for
// GetAgentAnalytics also prices out under /analytics_costs, in the SAME window,
// through the SAME correlation.
func TestCostEstimateAttributesSpendToTheAgent(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	now := time.Now().UTC()
	router := costAttributionRouter(pool, now)

	// seedAgentExecution plants 10 prompt + 20 completion tokens per request.
	plantModelPrice(t, pool, "openai", "gpt-4o", "2.50000000", "10.00000000")
	seedAgentExecution(t, pool, "exec-cost-agent-1", 501, "Support Bot", 2, 0)

	estimate := costAttributionEstimate(t, router, now)
	if got := estimate["agent_dimension_available"]; got != true {
		t.Fatalf("agent_dimension_available = %v, want true", got)
	}
	wantCostAttributionNumber(t, estimate, "attributed_agent_calls", "2")
	wantCostAttributionNumber(t, estimate, "unattributed_agent_calls", "0")

	byAgent := costAttributionRows(t, estimate, "by_agent")
	require.Len(t, byAgent, 1)
	row := byAgent[0]
	if row["application_id"] != "501" {
		t.Fatalf("application_id = %v, want 501", row["application_id"])
	}
	if row["name"] != "Support Bot" {
		t.Fatalf("name = %v, want the resolved agent name", row["name"])
	}
	wantCostAttributionNumber(t, row, "calls", "2")
	wantCostAttributionNumber(t, row, "prompt_tokens", "20")
	wantCostAttributionNumber(t, row, "completion_tokens", "40")
	// 20 prompt tokens at 2.50/1M is 0.00005; 40 completion tokens at 10.00/1M
	// is 0.0004; the sum is 0.00045 — computed by PostgreSQL, never by Go.
	wantCostAttributionNumber(t, row, "input_cost", "0.000050000000")
	wantCostAttributionNumber(t, row, "output_cost", "0.000400000000")
	wantCostAttributionNumber(t, row, "total_cost", "0.000450000000")
}

// TestCostEstimateAgentDimensionIsUnavailableWithoutAttributableTraffic is the
// no-backfill contract estimateByAgent shares with GetAgentAnalytics: a window
// with traffic but no execution ids must not read as "zero agent spend".
func TestCostEstimateAgentDimensionIsUnavailableWithoutAttributableTraffic(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	now := time.Now().UTC()
	router := costAttributionRouter(pool, now)

	seedUnattributedRequests(t, pool, 3)

	estimate := costAttributionEstimate(t, router, now)
	if got := estimate["agent_dimension_available"]; got != false {
		t.Fatalf("agent_dimension_available = %v, want false", got)
	}
	if _, present := estimate["by_agent"]; present {
		t.Fatalf("by_agent is present with no attributable traffic: %v", estimate["by_agent"])
	}
	wantCostAttributionNumber(t, estimate, "unattributed_agent_calls", "3")
}

// TestCostEstimateAttributesExecutionSpendToEveryToolItUsed is the fan-out
// this package's estimateByTool documents: an execution that calls two tools
// contributes its FULL LLM cost to each of them, because the cost belongs to
// the completion, not to any one tool it decided to call. This is also the
// regression test for agent_trace.go's recordAgentToolCalls now stamping
// execution_id on an agent-turn tool call — without it, neither row here would
// correlate to any request-log spend at all.
func TestCostEstimateAttributesExecutionSpendToEveryToolItUsed(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	now := time.Now().UTC()
	router := costAttributionRouter(pool, now)

	// One execution, one LLM call: 10 prompt + 20 completion tokens at
	// 1.00/1M each is 0.00001 + 0.00002 = 0.00003.
	plantModelPrice(t, pool, "openai", "gpt-4o", "1.00000000", "1.00000000")
	seedAgentExecution(t, pool, "exec-cost-tool-1", 502, "Ops Bot", 1, 0)

	started := now.Add(-30 * time.Minute)
	recordToolCallForTest(t, pool, ToolCallRecord{
		ProjectID: agentAnalyticsProject, Source: ToolCallSourceAgentTurn, SourceRef: "502:run-a",
		ToolkitName: "GitHub", ToolName: "list_issues",
		StartedAt: started, FinishedAt: started.Add(time.Second),
		ExecutionID: "exec-cost-tool-1",
	})
	recordToolCallForTest(t, pool, ToolCallRecord{
		ProjectID: agentAnalyticsProject, Source: ToolCallSourceAgentTurn, SourceRef: "502:run-b",
		ToolkitName: "Jira", ToolName: "create_issue",
		StartedAt: started.Add(2 * time.Second), FinishedAt: started.Add(3 * time.Second),
		ExecutionID: "exec-cost-tool-1",
	})

	estimate := costAttributionEstimate(t, router, now)
	if got := estimate["tool_dimension_available"]; got != true {
		t.Fatalf("tool_dimension_available = %v, want true", got)
	}
	wantCostAttributionNumber(t, estimate, "attributed_tool_calls", "2")
	wantCostAttributionNumber(t, estimate, "unattributed_tool_calls", "0")

	byTool := costAttributionRows(t, estimate, "by_tool")
	require.Len(t, byTool, 2)
	rows := map[string]map[string]any{}
	for _, row := range byTool {
		rows[fmt.Sprint(row["tool_name"])] = row
	}
	listIssues, ok := rows["list_issues"]
	require.True(t, ok, "by_tool = %v, want list_issues", byTool)
	createIssue, ok := rows["create_issue"]
	require.True(t, ok, "by_tool = %v, want create_issue", byTool)

	for name, row := range map[string]map[string]any{"list_issues": listIssues, "create_issue": createIssue} {
		wantCostAttributionNumber(t, row, "attributed_runs", "1")
		wantCostAttributionNumber(t, row, "total_cost", "0.000030000000")
		if row["toolkit_name"] == "" {
			t.Errorf("%s: toolkit_name is empty, want the producer's own name", name)
		}
	}
}

// TestCostEstimateExcludesToolCallsWithoutAnExecutionID covers the row
// agent_trace.go could not have stamped before #875, and the explicit run,
// which carries an execution id but makes no LLM call of its own: both count
// toward unattributed_tool_calls and neither invents a cost.
func TestCostEstimateExcludesToolCallsWithoutAnExecutionID(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	now := time.Now().UTC()
	router := costAttributionRouter(pool, now)

	started := now.Add(-10 * time.Minute)
	recordToolCallForTest(t, pool, ToolCallRecord{
		ProjectID: agentAnalyticsProject, Source: ToolCallSourceAgentTurn, SourceRef: "999:run-a",
		ToolkitName: "Confluence", ToolName: "search_pages",
		StartedAt: started, FinishedAt: started.Add(time.Second),
		// No ExecutionID: a row projected before #875, or a producer that still
		// cannot supply one.
	})
	recordToolCallForTest(t, pool, ToolCallRecord{
		ProjectID: agentAnalyticsProject, Source: ToolCallSourceExplicitRun, SourceRef: "exec-explicit-1",
		ToolkitID: 9, ToolkitType: "github", ToolName: "list_issues",
		StartedAt: started, FinishedAt: started.Add(200 * time.Millisecond),
		ActorUserID: 1, ExecutionID: "exec-explicit-1",
	})

	estimate := costAttributionEstimate(t, router, now)
	if got := estimate["tool_dimension_available"]; got != true {
		t.Fatalf("tool_dimension_available = %v, want true", got)
	}
	wantCostAttributionNumber(t, estimate, "attributed_tool_calls", "1")
	wantCostAttributionNumber(t, estimate, "unattributed_tool_calls", "1")

	byTool := costAttributionRows(t, estimate, "by_tool")
	// The explicit run's execution id resolves to no request-log row — it made
	// no LLM call — so it costs nothing and adds no row either.
	require.Len(t, byTool, 0, "by_tool = %v, want no correlatable spend", byTool)
}
