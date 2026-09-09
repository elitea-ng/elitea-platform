package analytics_test

// The COSTS and TOKENS dimensions, over the gateway request log.
//
// Every case here asserts a FIGURE, and each figure is one a handler cannot
// produce by accident:
//
//   - the money is checked against a rate written per 1,000,000 tokens, so a
//     read that drops or repeats the 1000x denomination fails here;
//   - a call to a model the catalogue does not price keeps its tokens, loses
//     its money keys, and moves `unpriced_calls`;
//   - a window with no priced call publishes NO money at all, rather than a
//     zero that reads as "this project spent nothing";
//   - cache tokens are absent with a flag, because 0099 records none.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// estimateNow pins the window, for the reason costNow does.
var estimateNow = costNow

/* ── fixtures ──────────────────────────────────────────────────────────── */

// plantCall writes one row of the gateway request log, in the shape the log
// carries after a real call.
func plantCall(
	t *testing.T, pool *pgxpool.Pool,
	projectID, userID int, at time.Time, provider, model string,
	promptTokens, completionTokens int64,
) {
	t.Helper()
	var user any
	if userID != 0 {
		user = userID
	}
	_, err := pool.Exec(context.Background(), `
INSERT INTO gateway.llm_request_logs
    (project_id, user_id, occurred_at, route, method, status,
     duration_ms, provider, model, streaming, prompt_tokens, completion_tokens)
VALUES ($1, $2, $3, '/llm/v1/chat/completions', 'POST', 200, 900, $4, $5, true, $6, $7)`,
		projectID, user, at, provider, model, promptTokens, completionTokens)
	if err != nil {
		t.Fatalf("plant call: %v", err)
	}
}

// plantPrice writes one catalogue row. The rates are PER 1,000,000 TOKENS,
// which is the denomination gateway.gateway_models declares and the gateway's
// own calculator uses.
func plantPrice(t *testing.T, pool *pgxpool.Pool, provider, model, inRate, outRate string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO gateway.gateway_models
    (provider, model_name, input_cost_per_1m_tokens, output_cost_per_1m_tokens)
VALUES ($1, $2, $3::numeric, $4::numeric)`, provider, model, inRate, outRate)
	if err != nil {
		t.Fatalf("plant price: %v", err)
	}
}

func estimateBlock(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	estimate, ok := payload["estimate"].(map[string]any)
	if !ok {
		t.Fatalf("estimate missing from %v", payload)
	}
	return estimate
}

func estimateRows(t *testing.T, estimate map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := estimate[key].([]any)
	if !ok {
		t.Fatalf("estimate.%s = %#v, want an array", key, estimate[key])
	}
	rows := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("estimate.%s row = %#v, want an object", key, item)
		}
		rows = append(rows, row)
	}
	return rows
}

func wantBool(t *testing.T, where map[string]any, field string, want bool) {
	t.Helper()
	got, ok := where[field].(bool)
	if !ok {
		t.Fatalf("%s = %#v, want a boolean", field, where[field])
	}
	if got != want {
		t.Fatalf("%s = %v, want %v", field, got, want)
	}
}

func costsTarget(from, to time.Time) string {
	return fmt.Sprintf("/analytics_costs/prompt_lib/%d?date_from=%s&date_to=%s",
		costProjectID, from.Format(time.RFC3339), to.Format(time.RFC3339))
}

func estimateWindow() (time.Time, time.Time) {
	return estimateNow.Add(-48 * time.Hour), estimateNow
}

/* ── the tokens dimension ──────────────────────────────────────────────── */

// Tokens are RECORDED, not derived, so they are answerable with no catalogue
// at all. This is the figure the Overview TOKENS tile reads, and the one that
// stayed 0 while CALLS was right.
func TestEstimateReportsRecordedTokensWithoutAnyPrice(t *testing.T) {
	pool, router := newCostsEnvironment(t)
	from, to := estimateWindow()
	at := estimateNow.Add(-time.Hour)

	plantCall(t, pool, costProjectID, 7, at, "vllm", "qwen3", 2000, 40)
	plantCall(t, pool, costProjectID, 7, at.Add(time.Minute), "vllm", "qwen3", 351, 2)

	estimate := estimateBlock(t, decodeCosts(t, costsDo(t, router, costsTarget(from, to))))
	wantBool(t, estimate, "token_dimension_available", true)
	wantBool(t, estimate, "cost_dimension_available", false)

	totals, _ := estimate["totals"].(map[string]any)
	wantCostNumber(t, totals, "calls", "2")
	wantCostNumber(t, totals, "prompt_tokens", "2351")
	wantCostNumber(t, totals, "completion_tokens", "42")
	wantCostNumber(t, totals, "total_tokens", "2393")

	// No catalogue priced anything, so there is no money to publish. A zero
	// here would render as "this project spent nothing".
	for _, absent := range []string{"input_cost", "output_cost", "total_cost"} {
		if v, present := totals[absent]; present {
			t.Errorf("totals.%s = %v with no priced call", absent, v)
		}
	}
	wantCostNumber(t, estimate, "unpriced_calls", "2")
	wantCostNumber(t, estimate, "priced_calls", "0")
}

// 0099 records no cache tokens and there is no second source for them. The flag
// says so; a zero would be a fabricated figure.
func TestEstimateDeclaresTheCacheDimensionUnavailable(t *testing.T) {
	pool, router := newCostsEnvironment(t)
	from, to := estimateWindow()
	plantCall(t, pool, costProjectID, 7, estimateNow.Add(-time.Hour), "vllm", "qwen3", 10, 5)

	estimate := estimateBlock(t, decodeCosts(t, costsDo(t, router, costsTarget(from, to))))
	wantBool(t, estimate, "cache_dimension_available", false)

	totals, _ := estimate["totals"].(map[string]any)
	for _, absent := range []string{"cache_read_tokens", "cache_creation_tokens"} {
		if _, present := totals[absent]; present {
			t.Errorf("totals.%s is present: the request log records no cache count", absent)
		}
	}
}

/* ── the cost dimension ────────────────────────────────────────────────── */

// The rate is per 1,000,000 tokens. 2000 prompt tokens at 3.00 per million is
// 0.006 exactly, and 40 completion tokens at 15.00 per million is 0.0006.
//
// The expected strings are what PostgreSQL NUMERIC produces, so a read that
// went through a float64 fails rather than nearly passing.
func TestEstimatePricesTokensAtTheCataloguePerMillionRate(t *testing.T) {
	pool, router := newCostsEnvironment(t)
	from, to := estimateWindow()

	plantPrice(t, pool, "vllm", "qwen3", "3.00000000", "15.00000000")
	plantCall(t, pool, costProjectID, 7, estimateNow.Add(-time.Hour), "vllm", "qwen3", 2000, 40)

	estimate := estimateBlock(t, decodeCosts(t, costsDo(t, router, costsTarget(from, to))))
	wantBool(t, estimate, "cost_dimension_available", true)
	if estimate["currency"] != "USD" {
		t.Fatalf("currency = %v, want USD", estimate["currency"])
	}

	totals, _ := estimate["totals"].(map[string]any)
	wantCostNumber(t, totals, "input_cost", "0.006000000000")
	wantCostNumber(t, totals, "output_cost", "0.000600000000")
	wantCostNumber(t, totals, "total_cost", "0.006600000000")
}

// A model the catalogue does not carry keeps its tokens and loses its money.
//
// Pricing it at zero would make an under-priced deployment look cheap, and the
// count of such calls is what tells an operator the money below covers only
// part of the traffic.
func TestEstimateSeparatesPricedFromUnpricedCalls(t *testing.T) {
	pool, router := newCostsEnvironment(t)
	from, to := estimateWindow()
	at := estimateNow.Add(-time.Hour)

	plantPrice(t, pool, "openai", "gpt-4o", "2.50000000", "10.00000000")
	plantCall(t, pool, costProjectID, 7, at, "openai", "gpt-4o", 1000, 100)
	plantCall(t, pool, costProjectID, 7, at.Add(time.Minute), "vllm", "qwen3", 4000, 400)

	estimate := estimateBlock(t, decodeCosts(t, costsDo(t, router, costsTarget(from, to))))
	wantCostNumber(t, estimate, "priced_calls", "1")
	wantCostNumber(t, estimate, "unpriced_calls", "1")

	// Both models are on the token side; only one carries money.
	byModel := estimateRows(t, estimate, "by_model")
	if len(byModel) != 2 {
		t.Fatalf("by_model = %v, want both models", byModel)
	}
	found := map[string]map[string]any{}
	for _, row := range byModel {
		found[fmt.Sprint(row["model"])] = row
	}
	priced, unpriced := found["gpt-4o"], found["qwen3"]
	if priced == nil || unpriced == nil {
		t.Fatalf("by_model = %v, want gpt-4o and qwen3", byModel)
	}
	wantBool(t, priced, "priced", true)
	wantCostNumber(t, priced, "total_cost", "0.003500000000")
	wantBool(t, unpriced, "priced", false)
	wantCostNumber(t, unpriced, "total_tokens", "4400")
	if v, present := unpriced["total_cost"]; present {
		t.Errorf("by_model[qwen3].total_cost = %v: the catalogue prices no such model", v)
	}
	// The window total is the priced model alone.
	totals, _ := estimate["totals"].(map[string]any)
	wantCostNumber(t, totals, "total_cost", "0.003500000000")
	wantCostNumber(t, totals, "total_tokens", "5500")
}

/* ── the breakdowns the tabs draw ──────────────────────────────────────── */

func TestEstimateBreaksDownByUserAndByDay(t *testing.T) {
	pool, router := newCostsEnvironment(t)
	from, to := estimateWindow()

	plantMembership(t, pool, costProjectID, map[int][]string{7: {"admin"}})
	plantPrice(t, pool, "openai", "gpt-4o", "1.00000000", "1.00000000")
	yesterday := estimateNow.Add(-30 * time.Hour)
	today := estimateNow.Add(-2 * time.Hour)
	plantCall(t, pool, costProjectID, 7, yesterday, "openai", "gpt-4o", 1000, 0)
	plantCall(t, pool, costProjectID, 7, today, "openai", "gpt-4o", 2000, 0)
	plantCall(t, pool, costProjectID, 8, today, "openai", "gpt-4o", 3000, 0)

	estimate := estimateBlock(t, decodeCosts(t, costsDo(t, router, costsTarget(from, to))))

	byUser := estimateRows(t, estimate, "by_user")
	if len(byUser) != 2 {
		t.Fatalf("by_user = %v, want two callers", byUser)
	}
	// Ordered by calls, so user 7 (two calls) leads.
	wantCostNumber(t, byUser[0], "user_id", "7")
	wantCostNumber(t, byUser[0], "total_tokens", "3000")
	if byUser[0]["email"] != "user7@example.com" {
		t.Errorf("by_user[0].email = %v, want the resolved address", byUser[0]["email"])
	}
	// A caller with no identity row keeps its figures and loses only its name.
	wantCostNumber(t, byUser[1], "user_id", "8")
	if byUser[1]["email"] != "" {
		t.Errorf("by_user[1].email = %v, want empty", byUser[1]["email"])
	}

	daily := estimateRows(t, estimate, "daily")
	if len(daily) != 2 {
		t.Fatalf("daily = %v, want the two days that had traffic", daily)
	}
	if daily[0]["date"].(string) >= daily[1]["date"].(string) {
		t.Fatalf("daily is not ordered by date: %v", daily)
	}
	wantCostNumber(t, daily[0], "total_tokens", "1000")
	wantCostNumber(t, daily[1], "total_tokens", "5000")
}

// The window bounds the estimate exactly as they bound the accumulator totals.
// Two windows that disagree put two numbers about two different weeks on one
// screen with nothing saying so.
func TestEstimateHonoursTheWindow(t *testing.T) {
	pool, router := newCostsEnvironment(t)
	from, to := estimateWindow()

	plantCall(t, pool, costProjectID, 7, estimateNow.Add(-time.Hour), "vllm", "qwen3", 10, 1)
	plantCall(t, pool, costProjectID, 7, from.Add(-time.Hour), "vllm", "qwen3", 999, 999)
	plantCall(t, pool, costOtherID, 7, estimateNow.Add(-time.Hour), "vllm", "qwen3", 777, 777)

	estimate := estimateBlock(t, decodeCosts(t, costsDo(t, router, costsTarget(from, to))))
	totals, _ := estimate["totals"].(map[string]any)
	wantCostNumber(t, totals, "calls", "1")
	wantCostNumber(t, totals, "total_tokens", "11")
}

// The accounted figure and the estimate are two sources and must stay two keys.
//
// `kpis.total_cost` is the accumulator: what the billing path recorded.
// `estimate.totals.total_cost` is tokens times a local price table. Merging
// them would make a deployment with no budget write-back report an estimate as
// its accounted spend.
func TestEstimateDoesNotReplaceTheAccountedTotal(t *testing.T) {
	pool, router := newCostsEnvironment(t)
	from, to := estimateWindow()

	plantPrice(t, pool, "vllm", "qwen3", "1.00000000", "1.00000000")
	plantCall(t, pool, costProjectID, 7, estimateNow.Add(-time.Hour), "vllm", "qwen3", 1000, 0)

	body := decodeCosts(t, costsDo(t, router, costsTarget(from, to)))
	kpis := costKPIs(t, body)

	// Nothing was billed, so the accounted figure stays zero and says so.
	wantCostNumber(t, kpis, "total_cost", "0.00000000")
	wantBool(t, kpis, "spend_available", false)

	// And the estimate is non-zero on the same response.
	totals, _ := estimateBlock(t, body)["totals"].(map[string]any)
	wantCostNumber(t, totals, "total_cost", "0.001000000000")
}

// A database with no request log has no dimension to report, and the key is
// absent rather than an empty block.
func TestEstimateIsAbsentWithoutARequestLog(t *testing.T) {
	pool, router := newCostsEnvironment(t)
	from, to := estimateWindow()

	if _, err := pool.Exec(context.Background(), "DROP TABLE gateway.llm_request_logs"); err != nil {
		t.Fatalf("drop request log: %v", err)
	}

	body := decodeCosts(t, costsDo(t, router, costsTarget(from, to)))
	if v, present := body["estimate"]; present {
		t.Fatalf("estimate = %v with no request log", v)
	}
	// The accounted half still answers.
	if _, ok := body["kpis"].(map[string]any); !ok {
		t.Fatalf("kpis missing: the accumulator read must not depend on the log")
	}
}

// json.Number, not float64: a price with eight decimal places must survive the
// response byte for byte.
func TestEstimateMoneyIsNotAFloat(t *testing.T) {
	pool, router := newCostsEnvironment(t)
	from, to := estimateWindow()

	// 0.1 has no exact float64, so 0.1 * 3 / 1e6 in float64 is
	// 3.0000000000000004e-07. In NUMERIC it is 0.0000003 exactly.
	plantPrice(t, pool, "vllm", "qwen3", "0.10000000", "0.10000000")
	plantCall(t, pool, costProjectID, 7, estimateNow.Add(-time.Hour), "vllm", "qwen3", 3, 0)

	body := decodeCosts(t, costsDo(t, router, costsTarget(from, to)))
	totals, _ := estimateBlock(t, body)["totals"].(map[string]any)
	number, ok := totals["input_cost"].(json.Number)
	if !ok {
		t.Fatalf("input_cost = %#v, want a JSON number", totals["input_cost"])
	}
	if number.String() != "0.000000300000" {
		t.Fatalf("input_cost = %s, want the exact NUMERIC", number)
	}
}

/* ── the agent and tool dimensions, issue 875 ────────────────────────────── */

// This harness's pool carries only GatewayMigrationSQL() (0067/0084/0086/0099)
// — no shared 0100 (execution_id), no elitea_runtime schema at all. That is a
// real deployment shape (a Go-bootstrapped database whose ledgered corpus has
// not reached 0100/0119 yet), and it is exactly what estimateByAgent and
// estimateByTool must degrade against without erroring: both dimensions are
// reported unavailable and their arrays are absent, the same contract
// GetAgentAnalytics and GetToolAnalytics already hold
// (internal/infra/db/repos/analytics.go).
func TestEstimateReportsAgentAndToolDimensionsUnavailableWithoutTheRuntimeSchema(t *testing.T) {
	pool, router := newCostsEnvironment(t)
	from, to := estimateWindow()
	plantCall(t, pool, costProjectID, 7, estimateNow.Add(-time.Hour), "vllm", "qwen3", 10, 5)

	estimate := estimateBlock(t, decodeCosts(t, costsDo(t, router, costsTarget(from, to))))
	wantBool(t, estimate, "agent_dimension_available", false)
	wantBool(t, estimate, "tool_dimension_available", false)
	if _, present := estimate["by_agent"]; present {
		t.Errorf("by_agent = %v, want absent without shared migration 0100", estimate["by_agent"])
	}
	if _, present := estimate["by_tool"]; present {
		t.Errorf("by_tool = %v, want absent without elitea_runtime.tool_call_records", estimate["by_tool"])
	}
}

// addExecutionIDColumn plants shared migration 0100's own column, without the
// rest of the ledgered history — enough for estimateByAgent's column probe to
// see it, the way TestGetAgentAnalytics_RefusesWhenTheColumnIsAbsent's sibling
// case does in the repos package.
func addExecutionIDColumn(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"ALTER TABLE gateway.llm_request_logs ADD COLUMN IF NOT EXISTS execution_id VARCHAR(128)"); err != nil {
		t.Fatalf("add execution_id column: %v", err)
	}
}

// The column exists (0100 ran) but elitea_runtime itself never has — a real
// shape for a Go-bootstrapped database whose ledgered corpus stops short of
// the runtime baseline. The read must not answer 500: it has nothing to
// attribute against and reports exactly that, the same "not available" the
// pre-0100 window already answers.
func TestEstimateByAgentSurvivesTheColumnWithNoExecutionJobsTable(t *testing.T) {
	pool, router := newCostsEnvironment(t)
	from, to := estimateWindow()
	addExecutionIDColumn(t, pool)
	plantCall(t, pool, costProjectID, 7, estimateNow.Add(-time.Hour), "vllm", "qwen3", 10, 5)

	estimate := estimateBlock(t, decodeCosts(t, costsDo(t, router, costsTarget(from, to))))
	wantBool(t, estimate, "agent_dimension_available", false)
	if _, present := estimate["by_agent"]; present {
		t.Errorf("by_agent = %v, want absent without elitea_runtime.execution_jobs", estimate["by_agent"])
	}
}

// A request with no execution id at all — the common case, most /llm traffic
// is not made from a runtime execution — must count as UNATTRIBUTED rather
// than silently vanish from both totals, once elitea_runtime.execution_jobs
// actually exists to attribute one against.
func TestEstimateByAgentReportsUnattributedTrafficWithAnEmptyExecutionJobsTable(t *testing.T) {
	pool, router := newCostsEnvironment(t)
	from, to := estimateWindow()
	addExecutionIDColumn(t, pool)
	if _, err := pool.Exec(context.Background(), `
CREATE SCHEMA IF NOT EXISTS elitea_runtime;
CREATE TABLE IF NOT EXISTS elitea_runtime.execution_jobs (
    execution_id VARCHAR(128) NOT NULL,
    capability_id VARCHAR(128) NOT NULL,
    resource_project_id INTEGER,
    projection_project_id INTEGER
)`); err != nil {
		t.Fatalf("plant an empty execution_jobs table: %v", err)
	}
	plantCall(t, pool, costProjectID, 7, estimateNow.Add(-time.Hour), "vllm", "qwen3", 10, 5)

	estimate := estimateBlock(t, decodeCosts(t, costsDo(t, router, costsTarget(from, to))))
	wantBool(t, estimate, "agent_dimension_available", false)
	wantCostNumber(t, estimate, "attributed_agent_calls", "0")
	wantCostNumber(t, estimate, "unattributed_agent_calls", "1")
}
