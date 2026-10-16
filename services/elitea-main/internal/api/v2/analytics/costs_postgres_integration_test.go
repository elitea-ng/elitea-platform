package analytics_test

// Issue 253 acceptance for the LLM cost breakdown.
//
// A 200 proves nothing here — this repo has shipped routes that answered 200
// and aggregated nothing (#128) — so every case below asserts a FIGURE, and
// each figure is chosen to be one a handler could not produce by accident:
//
//   - the headline total is checked against a fixture written as nano-USD
//     BUDGET DELTAS, converted by the same in-SQL expression the write-back
//     consumer uses, so a read path that lost or rounded the denomination
//     fails here rather than in production;
//   - a user-scope accumulator row is planted alongside the project ones, and
//     the total must NOT grow — the narrower scope is a subset of the same
//     spend, so summing every row is the double count this endpoint exists not
//     to make;
//   - a row in a neighbouring period and a row belonging to another project are
//     both planted, and both must be absent from the total;
//   - the dimensions nothing produces are asserted ABSENT, not empty.

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/analytics"
	infradb "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
)

const (
	costProjectID = 31
	costOtherID   = 32
)

// fixedNow pins the reporting window. Without it a run that straddles a month
// boundary writes into one period and reads a window that ends in the next.
var costNow = time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

func augustStart() time.Time { return time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC) }
func augustEnd() time.Time   { return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) }
func julyStart() time.Time   { return time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC) }
func julyEnd() time.Time     { return time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC) }

/* ── harness ───────────────────────────────────────────────────────────── */

func costsRouter(pool *pgxpool.Pool) chi.Router {
	costs := handler.NewCostsHandler(pool).WithClock(func() time.Time { return costNow })
	router := chi.NewRouter()
	router.Get("/analytics_costs/prompt_lib/{projectID}", costs.Costs)
	return router
}

func costsDo(t *testing.T, router chi.Router, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder
}

func decodeCosts(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	decoder := json.NewDecoder(recorder.Body)
	// UseNumber so an exact decimal that survived a float64 round trip fails
	// rather than "nearly" passing.
	decoder.UseNumber()
	payload := map[string]any{}
	if err := decoder.Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return payload
}

func costKPIs(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	kpis, ok := payload["kpis"].(map[string]any)
	if !ok {
		t.Fatalf("kpis missing from %v", payload)
	}
	return kpis
}

func wantCostNumber(t *testing.T, payload map[string]any, field, want string) {
	t.Helper()
	number, ok := payload[field].(json.Number)
	if !ok {
		t.Fatalf("%s = %#v, want a JSON number", field, payload[field])
	}
	if number.String() != want {
		t.Fatalf("%s = %s, want %s", field, number, want)
	}
}

// plantDeltas writes an accumulator row from a list of nano-USD BUDGET DELTAS —
// the unit the gateway publishes onto GATEWAY_BUDGET_DELTAS — summed and
// converted by `sum::numeric / 1000000000`, which is the single conversion point
// elitea-scheduler's budgetwriteback Store performs in SQL (store.go's
// upsertSQL) and never in float64.
//
// The consumer itself is not imported: it lives in the elitea-scheduler module
// and elitea-main depends on nothing there. What the criterion needs is that
// this read reports the same money that path persists, and that is exactly what
// travels through this fixture — the delta list, the SQL conversion, the
// NUMERIC column, the response.
func plantDeltas(
	t *testing.T, pool *pgxpool.Pool,
	scope, scopeID string, projectID int, start, end time.Time, nanoUSD ...int64,
) {
	t.Helper()
	var sum int64
	for _, delta := range nanoUSD {
		sum += delta
	}
	_, err := pool.Exec(context.Background(), `
INSERT INTO gateway.llm_budget_accumulators
    (project_id, scope, scope_id, period_start, period_end, accumulated_cost)
VALUES ($1, $2, $3, $4, $5, $6::numeric / 1000000000)`,
		projectID, scope, scopeID, start, end, sum)
	if err != nil {
		t.Fatalf("plant deltas: %v", err)
	}
}

func markOutage(t *testing.T, pool *pgxpool.Pool, projectID int, start time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
UPDATE gateway.llm_budget_accumulators
SET outage_mode = true, reconciled = false
WHERE project_id = $1 AND period_start = $2`, projectID, start)
	if err != nil {
		t.Fatalf("mark outage: %v", err)
	}
}

func newCostsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_costs_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	// The REAL gateway migration, not a copy: a column added there and not here
	// would otherwise let this pass against a schema production does not have.
	gatewaySQL, err := infradb.GatewayMigrationSQL()
	if err != nil {
		t.Fatalf("load gateway migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, gatewaySQL); err != nil {
		t.Fatalf("apply gateway migrations: %v", err)
	}
	return pool
}

func newCostsEnvironment(t *testing.T) (*pgxpool.Pool, chi.Router) {
	t.Helper()
	pool := newCostsPool(t)
	return pool, costsRouter(pool)
}

/* ── reconciliation with the write-back path ───────────────────────────── */

func TestCostBreakdownReportsWhatTheWriteBackPathPersisted(t *testing.T) {
	pool, router := newCostsEnvironment(t)

	// Three billing deltas in nano-USD: 0.25 + 1.75 + 0.000000010.
	plantDeltas(t, pool, "project", "31", costProjectID, augustStart(), augustEnd(),
		250_000_000, 1_750_000_000, 10)

	body := decodeCosts(t, costsDo(t, router,
		fmt.Sprintf("/analytics_costs/prompt_lib/%d", costProjectID)))

	kpis := costKPIs(t, body)
	// 2.00000001 exactly. A read path that took the NUMERIC through a float64
	// on its way to JSON does not produce this literal.
	wantCostNumber(t, kpis, "total_cost", "2.00000001")
	if kpis["currency"] != "USD" {
		t.Fatalf("currency = %v, want USD", kpis["currency"])
	}
	if spend, _ := kpis["spend_available"].(bool); !spend {
		t.Fatal("spend_available = false with a persisted accumulator row")
	}

	periods, ok := body["periods"].([]any)
	if !ok || len(periods) != 1 {
		t.Fatalf("periods = %#v, want exactly one row", body["periods"])
	}
	period := periods[0].(map[string]any)
	wantCostNumber(t, period, "total_cost", "2.00000001")
	if period["scope"] != "project" {
		t.Fatalf("scope = %v, want project", period["scope"])
	}
	if pending, _ := period["pending_reconciliation"].(bool); pending {
		t.Fatal("pending_reconciliation = true on a settled row")
	}
}

/* ── the double count this endpoint does not make ──────────────────────── */

func TestCostBreakdownDoesNotDoubleCountNarrowerScopes(t *testing.T) {
	pool, router := newCostsEnvironment(t)

	plantDeltas(t, pool, "project", "31", costProjectID, augustStart(), augustEnd(),
		4_000_000_000)
	// A member's share of that same 4.00 — a SUBSET of the project's spend, not
	// an addition to it. Nothing publishes user-scope deltas today; the day
	// something does, this row must still not inflate the project total.
	plantDeltas(t, pool, "user", "31:77", costProjectID, augustStart(), augustEnd(),
		1_000_000_000)

	body := decodeCosts(t, costsDo(t, router,
		fmt.Sprintf("/analytics_costs/prompt_lib/%d", costProjectID)))

	// 4.00, not 5.00.
	wantCostNumber(t, costKPIs(t, body), "total_cost", "4.00000000")
	if periods := costKPIs(t, body)["periods"]; fmt.Sprint(periods) != "1" {
		t.Fatalf("kpis.periods = %v, want 1 (project-scope rows only)", periods)
	}

	// Both scopes are still REPORTED — the narrower row is visible, it is just
	// not summed into the headline.
	byScope, ok := body["by_scope"].([]any)
	if !ok || len(byScope) != 2 {
		t.Fatalf("by_scope = %#v, want both scopes", body["by_scope"])
	}
	totals := map[string]string{}
	for _, entry := range byScope {
		row := entry.(map[string]any)
		totals[fmt.Sprint(row["scope"])] = fmt.Sprint(row["total_cost"])
	}
	if totals["project"] != "4.00000000" || totals["user"] != "1.00000000" {
		t.Fatalf("by_scope = %v, want project 4 and user 1 reported separately", totals)
	}
}

/* ── filtering ─────────────────────────────────────────────────────────── */

func TestCostBreakdownFiltersByWindowAndProject(t *testing.T) {
	pool, router := newCostsEnvironment(t)

	plantDeltas(t, pool, "project", "31", costProjectID, augustStart(), augustEnd(),
		3_000_000_000)
	// Last month: outside the default seven-day window ending 2026-08-12.
	plantDeltas(t, pool, "project", "31", costProjectID, julyStart(), julyEnd(),
		9_000_000_000)
	// Another project, same period.
	plantDeltas(t, pool, "project", "32", costOtherID, augustStart(), augustEnd(),
		7_000_000_000)

	body := decodeCosts(t, costsDo(t, router,
		fmt.Sprintf("/analytics_costs/prompt_lib/%d", costProjectID)))
	wantCostNumber(t, costKPIs(t, body), "total_cost", "3.00000000")

	// Widen the window and July joins it — proof the exclusion above was the
	// date filter doing its job and not a row the reader never saw.
	body = decodeCosts(t, costsDo(t, router, fmt.Sprintf(
		"/analytics_costs/prompt_lib/%d?date_from=2026-07-01T00:00:00Z&date_to=2026-08-31T00:00:00Z",
		costProjectID)))
	wantCostNumber(t, costKPIs(t, body), "total_cost", "12.00000000")
	if periods := costKPIs(t, body)["periods"]; fmt.Sprint(periods) != "2" {
		t.Fatalf("kpis.periods = %v, want 2", periods)
	}

	// The other project's 7.00 never appears in either answer.
	body = decodeCosts(t, costsDo(t, router,
		fmt.Sprintf("/analytics_costs/prompt_lib/%d", costOtherID)))
	wantCostNumber(t, costKPIs(t, body), "total_cost", "7.00000000")
}

/* ── no data is not zero spend ─────────────────────────────────────────── */

func TestCostBreakdownDistinguishesNoDataFromNoSpend(t *testing.T) {
	_, router := newCostsEnvironment(t)

	body := decodeCosts(t, costsDo(t, router,
		fmt.Sprintf("/analytics_costs/prompt_lib/%d", costProjectID)))

	kpis := costKPIs(t, body)
	wantCostNumber(t, kpis, "total_cost", "0.00000000")
	if spend, _ := kpis["spend_available"].(bool); spend {
		t.Fatal("spend_available = true with nothing persisted")
	}
	if periods, ok := body["periods"].([]any); !ok || len(periods) != 0 {
		t.Fatalf("periods = %#v, want an empty array rather than null", body["periods"])
	}
}

/* ── a knowingly-stale figure says so ──────────────────────────────────── */

func TestCostBreakdownFlagsRowsTheRecoveryPathStillOwns(t *testing.T) {
	pool, router := newCostsEnvironment(t)

	plantDeltas(t, pool, "project", "31", costProjectID, augustStart(), augustEnd(),
		5_000_000_000)
	// The write-back consumer is BARRED from this row until the gateway's
	// recovery goroutine clears the flag (store.go's ON CONFLICT guard), so the
	// figure is the last durable one and not necessarily the current one.
	markOutage(t, pool, costProjectID, augustStart())

	body := decodeCosts(t, costsDo(t, router,
		fmt.Sprintf("/analytics_costs/prompt_lib/%d", costProjectID)))

	periods := body["periods"].([]any)
	if len(periods) != 1 {
		t.Fatalf("periods = %#v, want one row", periods)
	}
	if pending, _ := periods[0].(map[string]any)["pending_reconciliation"].(bool); !pending {
		t.Fatal("pending_reconciliation = false on an un-reconciled outage row: " +
			"a stale total would be presented as settled")
	}
}

/* ── the row cap cannot move a number ──────────────────────────────────── */

// The `periods` array is capped so the (scope, scope_id) axis — which the date
// clamp does not bound — cannot turn one GET into a hundred-thousand-object
// response the day user-scope deltas start being published.
//
// The cap must cost DETAIL and nothing else: every figure is a SQL aggregate
// over the whole window, so a truncated response reports the same money as a
// complete one, and it says it was truncated rather than presenting a short
// list as the whole of it.
func TestCostBreakdownCapsTheRowListingWithoutChangingAnyTotal(t *testing.T) {
	pool, router := newCostsEnvironment(t)

	// 600 member-scope rows — over the 500 cap — plus the project row the
	// headline is made of. Each member row is 0.01; the project row is 12.00.
	plantDeltas(t, pool, "project", "31", costProjectID, augustStart(), augustEnd(),
		12_000_000_000)
	for member := range 600 {
		plantDeltas(t, pool, "user", fmt.Sprintf("31:%d", member), costProjectID,
			augustStart(), augustEnd(), 10_000_000)
	}

	body := decodeCosts(t, costsDo(t, router,
		fmt.Sprintf("/analytics_costs/prompt_lib/%d", costProjectID)))

	periods, ok := body["periods"].([]any)
	if !ok || len(periods) != 500 {
		t.Fatalf("periods holds %d rows, want the 500-row cap", len(periods))
	}
	if truncated, _ := body["periods_truncated"].(bool); !truncated {
		t.Fatal("periods_truncated = false on a capped listing: a silent cap reads " +
			"as 'this is everything'")
	}

	// THE assertion: the headline is unmoved by the truncation, because it is
	// not computed from the array.
	wantCostNumber(t, costKPIs(t, body), "total_cost", "12.00000000")

	// And the per-scope roll-up still covers every dropped row: 600 × 0.01.
	totals := map[string]string{}
	rowCounts := map[string]string{}
	for _, entry := range body["by_scope"].([]any) {
		row := entry.(map[string]any)
		totals[fmt.Sprint(row["scope"])] = fmt.Sprint(row["total_cost"])
		rowCounts[fmt.Sprint(row["scope"])] = fmt.Sprint(row["rows"])
	}
	if totals["user"] != "6.00000000" || rowCounts["user"] != "600" {
		t.Fatalf("by_scope[user] = %s over %s rows, want 6.00000000 over 600 — the "+
			"aggregate must cover rows the listing dropped", totals["user"], rowCounts["user"])
	}
	if totals["project"] != "12.00000000" {
		t.Fatalf("by_scope[project] = %s, want 12.00000000", totals["project"])
	}

	// The project row survives the cut: the listing orders project scope first
	// precisely so a truncation never drops the rows behind the headline.
	if scope := periods[0].(map[string]any)["scope"]; scope != "project" {
		t.Fatalf("first row scope = %v, want project", scope)
	}
}

// The flag is not stuck on: an ordinary response says it is complete.
func TestCostBreakdownReportsAnUncappedListingAsComplete(t *testing.T) {
	pool, router := newCostsEnvironment(t)
	plantDeltas(t, pool, "project", "31", costProjectID, augustStart(), augustEnd(),
		1_000_000_000)

	body := decodeCosts(t, costsDo(t, router,
		fmt.Sprintf("/analytics_costs/prompt_lib/%d", costProjectID)))
	if truncated, _ := body["periods_truncated"].(bool); truncated {
		t.Fatal("periods_truncated = true on a one-row listing")
	}
}

/* ── the access path both queries need ─────────────────────────────────── */

// Both of this endpoint's queries filter on project_id, which is not the
// leading column of any index this table shipped with — the primary key, the
// (scope, scope_id, period_start) unique constraint and the partial outage
// index all lead with something else. Without 004 they are sequential scans of
// a table that only grows, repeated on every dashboard load.
//
// The schema here is the REAL gateway migration set, so this fails if 004 is
// dropped, renamed away or has its columns reordered.
func TestCostBreakdownHasAnIndexForItsProjectFilter(t *testing.T) {
	pool, _ := newCostsEnvironment(t)

	var columns string
	err := pool.QueryRow(context.Background(), `
SELECT pg_get_indexdef(index.indexrelid)
FROM pg_index AS index
JOIN pg_class AS table_class ON table_class.oid = index.indrelid
JOIN pg_namespace AS schema ON schema.oid = table_class.relnamespace
WHERE schema.nspname = 'gateway'
  AND table_class.relname = 'llm_budget_accumulators'
  AND pg_get_indexdef(index.indexrelid) LIKE '%(project_id%'`).Scan(&columns)
	if err != nil {
		t.Fatalf("no index leads with project_id on gateway.llm_budget_accumulators — "+
			"both cost queries fall back to a sequential scan (%v)", err)
	}
	// period_start second is what turns the window filter into a range scan
	// rather than a recheck over every period the project has ever had.
	if !strings.Contains(columns, "period_start") {
		t.Fatalf("index is %q, want project_id followed by period_start", columns)
	}
}

/* ── the disclosure, machine-checked ───────────────────────────────────── */

// TestCostBreakdownOmitsTheDimensionsNothingProduces is the version of this
// package's central claim that cannot go stale.
//
// A budget delta carries no model, no user, no agent, no tokens and no call
// count, so this endpoint cannot answer by_model, by_agent, by_user, a daily
// trend or an average cost per call. They are ABSENT rather than empty, because
// `by_model: []` renders as "this project called no models" — a different and
// false claim. The day a dimension-carrying producer lands, this test is what
// fails, and it names what to reinstate.
func TestCostBreakdownOmitsTheDimensionsNothingProduces(t *testing.T) {
	pool, router := newCostsEnvironment(t)
	plantDeltas(t, pool, "project", "31", costProjectID, augustStart(), augustEnd(),
		1_000_000_000)

	body := decodeCosts(t, costsDo(t, router,
		fmt.Sprintf("/analytics_costs/prompt_lib/%d", costProjectID)))

	for _, absent := range []string{"by_model", "by_agent", "by_user", "daily"} {
		if _, present := body[absent]; present {
			t.Fatalf("%s is present: the billing path records no such dimension, "+
				"so any value here was invented", absent)
		}
	}
	for _, absent := range []string{
		"total_input_tokens", "total_output_tokens", "total_tokens",
		"avg_cost_per_call", "total_calls",
	} {
		if _, present := costKPIs(t, body)[absent]; present {
			t.Fatalf("kpis.%s is present: a budget delta carries no token or call count", absent)
		}
	}
}
