package budgets_test

// #320 acceptance for the dimensional half of the usage report.
//
// The ledger rows are planted exactly as the writers write them — the same
// table, the same columns, the same nano-USD → USD division the scheduler's
// write-back consumer and the gateway's outage path both perform — so the read
// path is exercised against the real write shape.
//
// The assertions that matter are the two the issue names:
//
//   - dimensions ABSENT when the ledger has no rows, so a deployment upgraded
//     mid-period does not render its accumulator spend as "no calls were made";
//   - the ledger sum agreeing with the accumulator, so the chart and the meter
//     on the same screen cannot disagree about what was billed.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// plantUsageEvent writes one ledger row the way both writers do, billed into
// the period under test.
func plantUsageEvent(
	t *testing.T, pool *pgxpool.Pool,
	eventID string, projectID int, userID *int,
	provider, model string, prompt, completion int64, costUSD string, occurredAt time.Time,
) {
	t.Helper()
	plantUsageEventForPeriod(t, pool, eventID, projectID, userID, provider, model,
		prompt, completion, costUSD, occurredAt, periodStart(), periodEnd())
}

// plantUsageEventForPeriod writes a row billed into an arbitrary period, so a
// test can put a row outside the one under report.
func plantUsageEventForPeriod(
	t *testing.T, pool *pgxpool.Pool,
	eventID string, projectID int, userID *int,
	provider, model string, prompt, completion int64, costUSD string,
	occurredAt, pStart, pEnd time.Time,
) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO gateway.llm_usage_events
    (event_id, project_id, user_id, provider, model,
     prompt_tokens, completion_tokens, cost_usd, period_start, period_end, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9, $10, $11)`,
		eventID, projectID, userID, provider, model, prompt, completion, costUSD,
		pStart, pEnd, occurredAt)
	if err != nil {
		t.Fatalf("plant usage event: %v", err)
	}
}

func usageDay(day int) time.Time {
	return time.Date(2026, 8, day, 9, 0, 0, 0, time.UTC)
}

// TestUsageWithoutLedgerRowsOmitsDimensions is the "absent, not empty" case.
// An empty daily array would render as "this project made no calls", which is a
// different and wrong claim about a period whose accumulator holds spend.
func TestUsageWithoutLedgerRowsOmitsDimensions(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)
	plantAccumulator(t, pool, "project", fmt.Sprint(budgetProjectID), budgetProjectID,
		periodStart(), periodEnd(), "9.00")

	recorder := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/usage/prompt_lib/%d/usage", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	body := decodeMap(t, recorder)

	wantBool(t, body, "usage_events_available", false)
	for _, field := range []string{"daily", "models", "prompt_tokens", "completion_tokens", "total_tokens", "api_requests"} {
		wantAbsent(t, body, field)
	}
	// The meter still reports the accumulator's spend: the missing dimensions
	// are a reporting gap, not a claim that nothing was billed.
	wantNumber(t, body, "spend", "9.00000000")
	wantBool(t, body, "spend_available", true)
}

// TestUsageServesPerDayAndPerModelDimensions is the capability #320 is about.
func TestUsageServesPerDayAndPerModelDimensions(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)
	admin := budgetAdminUser
	member := budgetMemberUser

	// Two days, two models, two members — and a total that must equal the
	// accumulator planted below.
	plantUsageEvent(t, pool, "11111111-1111-1111-1111-111111111111", budgetProjectID, &admin,
		"openai", "gpt-4o", 100, 200, "1.00", usageDay(3))
	plantUsageEvent(t, pool, "22222222-2222-2222-2222-222222222222", budgetProjectID, &admin,
		"openai", "gpt-4o", 10, 20, "0.50", usageDay(3))
	plantUsageEvent(t, pool, "33333333-3333-3333-3333-333333333333", budgetProjectID, &member,
		"anthropic", "claude-3-5-sonnet", 5, 5, "0.25", usageDay(4))
	plantAccumulator(t, pool, "project", fmt.Sprint(budgetProjectID), budgetProjectID,
		periodStart(), periodEnd(), "1.75")

	recorder := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/usage/prompt_lib/%d/usage", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	body := decodeMap(t, recorder)

	wantBool(t, body, "usage_events_available", true)
	wantNumber(t, body, "prompt_tokens", "115")
	wantNumber(t, body, "completion_tokens", "225")
	wantNumber(t, body, "total_tokens", "340")
	wantNumber(t, body, "api_requests", "3")

	// The per-day series has one entry per day that had calls, not one per
	// calendar day: a zero for a day nobody could have used yet is a claim
	// about the future.
	daily := rowsOf(t, body, "daily")
	if len(daily) != 2 {
		t.Fatalf("daily series has %d entries, want 2 (the two days with calls)", len(daily))
	}
	wantString(t, daily[0], "date", "2026-08-03")
	wantNumber(t, daily[0], "spend", "1.50000000")
	wantNumber(t, daily[0], "api_requests", "2")
	wantNumber(t, daily[0], "total_tokens", "330")
	wantString(t, daily[1], "date", "2026-08-04")
	wantNumber(t, daily[1], "spend", "0.25000000")

	// The per-model table is ordered by spend.
	models := rowsOf(t, body, "models")
	if len(models) != 2 {
		t.Fatalf("model table has %d rows, want 2", len(models))
	}
	wantString(t, models[0], "model", "gpt-4o")
	wantString(t, models[0], "provider", "openai")
	wantNumber(t, models[0], "spend", "1.50000000")
	wantNumber(t, models[0], "total_tokens", "330")
	wantString(t, models[1], "model", "claude-3-5-sonnet")

	// The ledger and the accumulator are the same money. If these two ever
	// disagree, the chart and the meter on one screen report different numbers
	// for one period — which is worse than the missing chart #320 is about.
	wantNumber(t, body, "spend", "1.75000000")
	total := sumSpend(t, daily)
	if total != "1.75000000" {
		t.Fatalf("the daily series sums to %s but the accumulator holds 1.75000000", total)
	}
}

// TestUsageUserScopeReportsOnlyTheCallersEvents proves the member filter
// reaches the ledger. Without it a member's own Usage tab would report the
// whole project's per-model table.
func TestUsageUserScopeReportsOnlyTheCallersEvents(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)
	admin := budgetAdminUser
	member := budgetMemberUser

	plantUsageEvent(t, pool, "11111111-1111-1111-1111-111111111111", budgetProjectID, &admin,
		"openai", "gpt-4o", 100, 200, "1.00", usageDay(3))
	plantUsageEvent(t, pool, "33333333-3333-3333-3333-333333333333", budgetProjectID, &member,
		"anthropic", "claude-3-5-sonnet", 5, 5, "0.25", usageDay(4))

	recorder := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/usage/prompt_lib/%d/usage?scope=user", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	body := decodeMap(t, recorder)

	wantNumber(t, body, "api_requests", "1")
	wantNumber(t, body, "total_tokens", "300")
	models := rowsOf(t, body, "models")
	if len(models) != 1 {
		t.Fatalf("member scope reported %d models, want only the caller's 1", len(models))
	}
	wantString(t, models[0], "model", "gpt-4o")
}

// TestUsageRedactionReachesInsideTheDimensionRows is the control the top-level
// redaction alone would not give: stripping `spend` from the payload while
// leaving a per-day series of the same money makes the control decorative.
func TestUsageRedactionReachesInsideTheDimensionRows(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)
	member := budgetMemberUser
	plantUsageEvent(t, pool, "44444444-4444-4444-4444-444444444444", budgetProjectID, &member,
		"openai", "gpt-4o", 10, 20, "0.75", usageDay(5))

	// budgetMemberUser is an editor of budgetProjectID, not an admin, and this
	// is not their personal project — so they may not see amounts.
	recorder := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/usage/prompt_lib/%d/usage", budgetProjectID), budgetMemberUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	body := decodeMap(t, recorder)

	wantBool(t, body, "can_see_amounts", false)
	wantAbsent(t, body, "spend")

	// The token counts survive: a member is allowed to know how much they used,
	// on the same principle that leaves them the percentage and the period.
	wantNumber(t, body, "total_tokens", "30")

	for _, field := range []string{"daily", "models"} {
		for _, row := range rowsOf(t, body, field) {
			if _, present := row["spend"]; present {
				t.Fatalf("%s row still carries spend for a caller who may not see amounts: %v", field, row)
			}
			// The row is still useful without it.
			if _, present := row["total_tokens"]; !present {
				t.Fatalf("%s row lost its token counts to redaction: %v", field, row)
			}
		}
	}
}

// TestUsageIgnoresEventsOfOtherPeriods pins the period key. The ledger is
// selected by the accumulator's own period_start, so the chart covers exactly
// the calls the meter's spend covers.
func TestUsageIgnoresEventsOfOtherPeriods(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)
	admin := budgetAdminUser

	plantUsageEvent(t, pool, "11111111-1111-1111-1111-111111111111", budgetProjectID, &admin,
		"openai", "gpt-4o", 10, 20, "1.00", usageDay(3))
	// Billed into the NEXT period.
	plantUsageEventForPeriod(t, pool, "55555555-5555-5555-5555-555555555555", budgetProjectID, &admin,
		"openai", "gpt-4o", 999, 999, "9.00", periodEnd().AddDate(0, 0, 1),
		periodEnd(), periodEnd().AddDate(0, 1, 0))
	// Billed into the PREVIOUS period.
	plantUsageEventForPeriod(t, pool, "66666666-6666-6666-6666-666666666666", budgetProjectID, &admin,
		"openai", "gpt-4o", 777, 777, "7.00", periodStart().AddDate(0, 0, -2),
		periodStart().AddDate(0, -1, 0), periodStart())

	recorder := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/usage/prompt_lib/%d/usage", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	body := decodeMap(t, recorder)

	wantNumber(t, body, "api_requests", "1")
	wantNumber(t, body, "total_tokens", "30")
}

// TestUsageKeepsALateWrittenEventInItsBilledPeriod is the month-boundary case
// the period key exists for.
//
// The write-back consumer runs behind the stream, and an outage-deferred group
// is redelivered until the accumulator row stops being outage-owned. So a call
// billed at 23:59 on the last of the month can reach the ledger days later. Its
// money is in THIS period's accumulator. A range over occurred_at would put the
// row in the next period, and the chart and the meter would then describe
// different sets of calls.
func TestUsageKeepsALateWrittenEventInItsBilledPeriod(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)
	admin := budgetAdminUser

	// Billed into THIS period, on its last day, and stored with an occurred_at
	// that the gateway supplied.
	lastMinute := periodEnd().Add(-time.Minute)
	plantUsageEvent(t, pool, "11111111-1111-1111-1111-111111111111", budgetProjectID, &admin,
		"openai", "gpt-4o", 10, 20, "1.00", lastMinute)

	recorder := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/usage/prompt_lib/%d/usage", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	body := decodeMap(t, recorder)

	wantNumber(t, body, "api_requests", "1")
	daily := rowsOf(t, body, "daily")
	if len(daily) != 1 {
		t.Fatalf("daily series has %d entries, want 1", len(daily))
	}
	// The last day of the reporting period, not the first day of the next one.
	wantString(t, daily[0], "date", periodEnd().AddDate(0, 0, -1).Format("2006-01-02"))
}

// TestUsageEventsAreScopedToTheProject stops one project's ledger leaking into
// another's report.
func TestUsageEventsAreScopedToTheProject(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)
	admin := budgetAdminUser

	plantUsageEvent(t, pool, "11111111-1111-1111-1111-111111111111", budgetProjectID, &admin,
		"openai", "gpt-4o", 10, 20, "1.00", usageDay(3))
	plantUsageEvent(t, pool, "77777777-7777-7777-7777-777777777777", budgetOtherID, &admin,
		"openai", "gpt-4o", 500, 500, "5.00", usageDay(3))

	recorder := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/usage/prompt_lib/%d/usage", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	wantNumber(t, decodeMap(t, recorder), "total_tokens", "30")
}

// TestUsageLedgerTotalTokensIsGenerated proves the generated column: a writer
// cannot store a total that disagrees with its own parts.
func TestUsageLedgerTotalTokensIsGenerated(t *testing.T) {
	pool, _ := newBudgetsEnvironment(t)
	plantUsageEvent(t, pool, "88888888-8888-8888-8888-888888888888", budgetProjectID, nil,
		"openai", "gpt-4o", 7, 13, "0.01", usageDay(6))

	var total int64
	err := pool.QueryRow(context.Background(),
		`SELECT total_tokens FROM gateway.llm_usage_events WHERE event_id = $1`,
		"88888888-8888-8888-8888-888888888888").Scan(&total)
	if err != nil {
		t.Fatal(err)
	}
	if total != 20 {
		t.Fatalf("total_tokens = %d, want 20 (7 + 13) computed by Postgres", total)
	}
}

/* ── helpers ───────────────────────────────────────────────────────────── */

func rowsOf(t *testing.T, payload map[string]any, field string) []map[string]any {
	t.Helper()
	raw, present := payload[field]
	if !present {
		t.Fatalf("%s is absent from %v", field, payload)
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("%s = %#v, want a list", field, raw)
	}
	rows := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		row, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("%s entry = %#v, want an object", field, entry)
		}
		rows = append(rows, row)
	}
	return rows
}

// sumSpend adds the exact decimals without going through float64, which is the
// whole reason these values are json.Number.
func sumSpend(t *testing.T, rows []map[string]any) string {
	t.Helper()
	var cents int64
	for _, row := range rows {
		number, ok := row["spend"].(json.Number)
		if !ok {
			t.Fatalf("spend = %#v, want a JSON number", row["spend"])
		}
		// The column is NUMERIC(20,8); scale by 1e8 and stay in integers.
		var whole, frac int64
		if _, err := fmt.Sscanf(number.String(), "%d.%8d", &whole, &frac); err != nil {
			t.Fatalf("parse spend %q: %v", number, err)
		}
		cents += whole*100_000_000 + frac
	}
	return fmt.Sprintf("%d.%08d", cents/100_000_000, cents%100_000_000)
}

// TestUsageModelsIncludeToolRowsWithOperation is the #1238 half of the model
// table: a metered tool settlement is a ledger row whose provider is the
// vendor and whose model is the operation, listed beside the LLM rows with
// its operation and units, and summed into the same totals.
func TestUsageModelsIncludeToolRowsWithOperation(t *testing.T) {
	pool, router := newBudgetsEnvironment(t)
	admin := budgetAdminUser
	plantUsageEvent(t, pool, "55555555-5555-5555-5555-555555555555", budgetProjectID, &admin,
		"openai", "gpt-4o", 100, 200, "1.00", usageDay(6))
	if _, err := pool.Exec(context.Background(), `
INSERT INTO gateway.llm_usage_events
    (event_id, project_id, user_id, provider, model, cost_usd, api_requests,
     operation, units, unit, period_start, period_end, occurred_at)
VALUES ($1, $2, $3, 'tavily', 'web_search', 0.008, 1, 'tool', 1, 'credit', $4, $5, $6)`,
		"66666666-6666-6666-6666-666666666666", budgetProjectID, admin,
		periodStart(), periodEnd(), usageDay(6)); err != nil {
		t.Fatalf("plant tool usage event: %v", err)
	}

	recorder := budgetsDo(t, router, http.MethodGet,
		fmt.Sprintf("/usage/prompt_lib/%d/usage", budgetProjectID), budgetAdminUser, nil)
	requireStatus(t, recorder, http.StatusOK)
	body := decodeMap(t, recorder)

	models := rowsOf(t, body, "models")
	if len(models) != 2 {
		t.Fatalf("model table has %d rows, want the LLM row and the tool row", len(models))
	}
	wantString(t, models[0], "operation", "llm")
	wantAbsent(t, models[0], "units")
	wantString(t, models[1], "provider", "tavily")
	wantString(t, models[1], "model", "web_search")
	wantString(t, models[1], "operation", "tool")
	wantNumber(t, models[1], "units", "1")
	wantString(t, models[1], "unit", "credit")
	wantNumber(t, models[1], "api_requests", "1")
	wantNumber(t, models[1], "spend", "0.00800000")
	wantNumber(t, body, "api_requests", "2")
}
