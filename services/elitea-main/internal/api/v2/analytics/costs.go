package analytics

// analytics_costs — the LLM cost breakdown for a project.
//
//	GET /elitea_core/analytics_costs/prompt_lib/{projectID}
//	    ← legacy/plugins/elitea_core/api/v2/analytics_costs.py
//
// # Where the numbers come from, and where they do NOT
//
// gateway.llm_budget_accumulators, and nothing else. The LLM gateway publishes
// a billing delta per call onto the GATEWAY_BUDGET_DELTAS stream and
// elitea-scheduler's budgetwriteback consumer folds those deltas into that
// table (services/elitea-scheduler/internal/budgetwriteback). It is the
// platform's single accounting path, and issue 253 asked this endpoint to
// surface what that path writes rather than to resurrect the LiteLLM-era
// tables the pylon original read.
//
// It is also the same table /elitea_core/usage and /elitea_core/project_budget
// report from (issue 246). One source, three views: a figure here can never
// disagree with the one on the usage bar, because there is no second place for
// either of them to come from.
//
// # What the reference served that this cannot, and why it is ABSENT
//
// The pylon handler aggregated `centry.audit_events` rows — one row per LLM
// call, each carrying llm_cost, input_tokens, output_tokens, model_name, the
// caller and a trace_id it correlated back to an agent. So it could answer
// by_model, by_agent, by_user, a daily trend, a call count and an average cost
// per call. Those columns have no producer in this architecture: a
// GATEWAY_BUDGET_DELTAS delta carries {event_id, scope, scope_id, project_id,
// org_id, period_start, period_end, delta_nano_usd} and NOTHING else — no
// model, no user, no tokens, no call count (budgetwriteback/types.go), and the
// accumulator it lands in is one summed USD figure per (scope, scope_id,
// period_start).
//
// Those five views are therefore ABSENT from this response rather than present
// and zero. `by_model: []` renders as "this project called no models", which is
// a different and false claim; a missing key is one a client can detect and one
// a reviewer must look at. TestCostBreakdownOmitsTheDimensionsNothingProduces
// pins the absence, so the day a dimension-carrying producer lands, the test
// that fails is the one that says what this endpoint could not answer — the
// disclosure is machine-checked rather than a comment that goes stale.
//
// # Aggregation, and the double count that is deliberately not made
//
// `total_cost` sums PROJECT-scope rows only. The accumulator is keyed by
// (scope, scope_id, period_start), and since #321 the gateway bills BOTH the
// project scope and a user scope for the same request. A user-scope row is a
// SUBSET of that project's spend, not an addition to it, so adding every row
// for a project would count the same dollars twice. This rule was written
// before a second scope existed; it is now load-bearing rather than
// anticipatory. Every scope present is still reported, under `by_scope`, so
// the narrower rows are visible without being summed into the headline.
// TestCostBreakdownDoesNotDoubleCountNarrowerScopes is that rule.
//
// Every figure is a PostgreSQL NUMERIC aggregate over the whole window. The
// per-row `periods` listing is capped (maxPeriodRows) and says so when it is
// cut, but no total is ever computed from that array, so a capped response and
// a complete one report the same money.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ViewPermission gates the route in internal/api/router.go, transcribed from
// analytics_costs.py's `check_api` — DEFAULT mode, project-scoped. The grant
// that makes it resolvable on a Go-bootstrapped database is seeded by
// migrations/shared/0063_trace_and_cost_read_permissions.sql.
//
// This is the one /analytics_* route that carries a gate, and the difference is
// not an oversight in either direction: its neighbours answer with counts and
// zero-filled KPI stubs, while this one answers with money. The reference gates
// it too.
const ViewPermission = "models.monitoring.tracing.view"

// Date-window bounds, transcribed from legacy/plugins/elitea_core/utils/
// constants.py.
const (
	defaultDateRangeDays = 7
	maxDateRangeDays     = 366
)

// maxPeriodRows caps the `periods` array.
//
// The date clamp above bounds ONE axis of this response. The other — the
// accumulator's (scope, scope_id) — is not the clamp's to bound: the table is
// keyed by (scope, scope_id, period_start), so a project with ten thousand
// members has ten thousand rows per period, not one.
//
// That day has arrived. Issue #321 made the gateway publish user-scope deltas,
// so a project now has one row per member per period on top of its own. A
// year's window on a fifty-member project passes this cap, and
// `periods_truncated` is therefore a normal answer rather than a rare one. The
// cap is doing the job it was written for; the sentence that used to say "only
// project-scope rows exist today" is what changed.
//
// The cap CANNOT silently change a number. The totals are computed by SQL
// aggregate over every matching row (see scopeTotals) and never by summing the
// array below, so a truncated response reports the same money as a complete
// one. What truncation costs is per-row DETAIL, and the response says so with
// `periods_truncated: true` rather than presenting a short list as the whole of
// it — a silent cap reads as "this is everything".
const maxPeriodRows = 500

// budgetScopeProject is the accumulator scope the gateway bills against —
// llmproxy/budget_gate.go's `budgetScopeProject`. Its scope_id is the numeric
// project id as text.
const budgetScopeProject = "project"

// currency is the only currency the money path has. The gateway compares
// hard_limit_usd against a nano-USD counter and converts nowhere, so a figure
// labelled anything else would be a lie about the same number.
const currency = "USD"

// CostsHandler serves the cost breakdown straight from the pool. It is separate
// from Handler above because that one is built over an injected Repository of
// zero-filled stubs; this endpoint reads a real table and would have nothing to
// gain from being routed through it.
type CostsHandler struct {
	pool *pgxpool.Pool
	// now is the clock the default window ends at. Tests pin it; production
	// leaves it nil.
	now func() time.Time
}

// NewCostsHandler builds a CostsHandler over the shared pool.
func NewCostsHandler(pool *pgxpool.Pool) *CostsHandler { return &CostsHandler{pool: pool} }

// WithClock pins the clock the default date window is measured back from.
func (h *CostsHandler) WithClock(now func() time.Time) *CostsHandler {
	h.now = now
	return h
}

func (h *CostsHandler) clock() time.Time {
	if h.now == nil {
		return time.Now().UTC()
	}
	return h.now()
}

// costPeriod is one accumulator row: the durable tier's unit of accounting.
type costPeriod struct {
	Scope       string      `json:"scope"`
	ScopeID     string      `json:"scope_id"`
	PeriodStart time.Time   `json:"period_start"`
	PeriodEnd   time.Time   `json:"period_end"`
	TotalCost   json.Number `json:"total_cost"`
	LastUpdated time.Time   `json:"last_updated"`
	// PendingReconciliation marks a row the gateway's recovery goroutine still
	// owns (outage_mode AND NOT reconciled): the write-back consumer is barred
	// from it until reconciliation clears the flag, so the figure is the last
	// durable one and not necessarily the current one. Reporting the number
	// without the flag would present a knowingly-stale total as settled.
	PendingReconciliation bool `json:"pending_reconciliation"`
}

// scopeTotal is the per-scope roll-up, summed by PostgreSQL over every row in
// the window — not over the (possibly truncated) `periods` array.
type scopeTotal struct {
	Scope     string      `json:"scope"`
	TotalCost json.Number `json:"total_cost"`
	// Rows is how many accumulator rows the sum covers, which is how a client
	// can tell that a scope contributed more rows than the array shows.
	Rows int64 `json:"rows"`
}

// Costs serves analytics_costs.py's prompt_lib GET.
func (h *CostsHandler) Costs(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || projectID < 1 {
		writeJSON(w, http.StatusBadRequest,
			map[string]any{"error": "project id must be a positive integer"})
		return
	}
	from, to := dateWindow(r.URL.Query(), h.clock)
	ctx := r.Context()

	// ONE snapshot for both reads.
	//
	// The totals and the row listing are two statements over a table the
	// write-back consumer commits into continuously — that tier is designed to
	// be seconds-fresh. Under READ COMMITTED each statement takes its own
	// snapshot even inside a transaction, so without this the two could see
	// different databases: `by_scope[...].rows` could say 2 while `periods`
	// holds 1 row and `periods_truncated` is false, which is precisely the
	// completeness signal those two fields exist to give. REPEATABLE READ
	// pins both statements to one snapshot, so the listing is always a subset
	// of exactly the rows the totals were summed from.
	//
	// Read-only and rolled back rather than committed: nothing here writes, and
	// a read-only REPEATABLE READ transaction cannot raise a serialization
	// failure (only SERIALIZABLE does), so this adds a snapshot guarantee and
	// no retry path.
	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError,
			map[string]any{"error": "failed to query analytics costs"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The money first, and from an aggregate: every figure this endpoint
	// reports is summed over the whole window by PostgreSQL, so the row listing
	// below cannot influence a total by being short.
	totals, err := scopeTotals(ctx, tx, projectID, from, to)
	if err != nil {
		// Not an empty breakdown: "no spend" and "the query failed" must not
		// render as the same screen.
		writeJSON(w, http.StatusInternalServerError,
			map[string]any{"error": "failed to query analytics costs"})
		return
	}

	periods, truncated, err := periods(ctx, tx, projectID, from, to)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError,
			map[string]any{"error": "failed to query analytics costs"})
		return
	}

	// The dimensional half, read from the request log rather than from the
	// accumulator (estimate.go). It is nil when this database carries no
	// request log, and the key is then absent — the answer a deployment
	// without the gateway's own tables must give.
	estimate, err := buildEstimate(ctx, tx, projectID, from, to)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError,
			map[string]any{"error": "failed to query analytics costs"})
		return
	}

	body := map[string]any{
		"kpis":     kpis(totals, from, to),
		"periods":  periods,
		"by_scope": totals,
		// Stated rather than implied: with this false, `periods` is every row in
		// the window; with it true, it is the first maxPeriodRows of them and
		// the totals above still cover the rest.
		"periods_truncated": truncated,
		"date_from":         from,
		"date_to":           to,
	}
	if estimate != nil {
		body["estimate"] = estimate
	}
	writeJSON(w, http.StatusOK, body)
}

// dateWindow reproduces the reference's _parse_dates: an unparseable or missing
// bound falls back, a window given by neither bound is the last
// defaultDateRangeDays, and a span wider than maxDateRangeDays is clamped from
// the far end.
//
// SHARED with the /analytics_* handlers next door (handler.go's parseParams),
// not because the code is worth reusing but because the RULES are: the
// Overview tab paints volume from one endpoint and cost from this one, side by
// side, and two windows that default or clamp differently put two numbers about
// two different weeks on one screen with nothing saying so.
func dateWindow(query url.Values, clock func() time.Time) (from, to time.Time) {
	parsedFrom, hasFrom := parseDate(query.Get("date_from"))
	parsedTo, hasTo := parseDate(query.Get("date_to"))

	switch {
	case !hasFrom && !hasTo:
		to = clock()
		from = to.AddDate(0, 0, -defaultDateRangeDays)
	case !hasFrom:
		to = parsedTo
		from = to.AddDate(0, 0, -defaultDateRangeDays)
	case !hasTo:
		from = parsedFrom
		to = clock()
	default:
		from, to = parsedFrom, parsedTo
	}
	if to.Sub(from) > maxDateRangeDays*24*time.Hour {
		from = to.AddDate(0, 0, -maxDateRangeDays)
	}
	return from.UTC(), to.UTC()
}

func parseDate(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// windowPredicate is the row set both queries below read, written once so the
// aggregate and the listing can never describe different rows.
//
// Overlap, not containment: an accumulator period is a whole billing month, so
// a seven-day default window is inside one rather than around it, and asking
// for rows contained in the window would answer "no spend" for every default
// request. The bounds are half-open on both sides (period_start < to AND
// period_end > from) so two adjacent periods cannot both match an instant.
const windowPredicate = `
WHERE project_id = $1
  AND period_start < $2
  AND period_end > $3`

// querier is the read seam both queries take, so they run on whatever the
// caller pins them to — today the one REPEATABLE READ transaction Costs opens,
// which is what makes the listing a subset of the rows the totals cover.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// scopeTotals rolls the window up per scope, in PostgreSQL.
//
// The sum is an exact NUMERIC aggregate over EVERY matching row, which is what
// lets the row listing be capped without any figure changing: truncation costs
// per-row detail and nothing else. Its result is bounded by the number of
// distinct scopes — a four-value vocabulary — not by the number of rows.
func scopeTotals(
	ctx context.Context, db querier, projectID int64, from, to time.Time,
) ([]scopeTotal, error) {
	const statement = `
SELECT scope, sum(accumulated_cost)::text AS total_cost, count(*) AS rows
FROM gateway.llm_budget_accumulators` + windowPredicate + `
GROUP BY scope
ORDER BY scope ASC`

	rows, err := db.Query(ctx, statement, projectID, to, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	totals := []scopeTotal{}
	for rows.Next() {
		var total scopeTotal
		var cost string
		if err := rows.Scan(&total.Scope, &cost, &total.Rows); err != nil {
			return nil, err
		}
		// The exact NUMERIC PostgreSQL produced, carried as a JSON number
		// without a float64 round trip: 0.10 must not come back as
		// 0.10000000000000001 on a money path.
		total.TotalCost = json.Number(cost)
		totals = append(totals, total)
	}
	return totals, rows.Err()
}

// periods lists the individual accumulator rows behind those totals, capped at
// maxPeriodRows.
//
// It asks for one row more than the cap and reports the overflow rather than
// hiding it, and it orders PROJECT-scope rows first so the rows backing the
// headline figure are the ones that survive a truncation. Within each group the
// order is chronological.
func periods(
	ctx context.Context, db querier, projectID int64, from, to time.Time,
) (listed []costPeriod, truncated bool, err error) {
	const statement = `
SELECT scope, scope_id, period_start, period_end,
       accumulated_cost::text, last_updated,
       (outage_mode AND NOT reconciled) AS pending_reconciliation
FROM gateway.llm_budget_accumulators` + windowPredicate + `
ORDER BY (scope = 'project') DESC, period_start ASC, scope ASC, scope_id ASC
LIMIT $4`

	rows, err := db.Query(ctx, statement, projectID, to, from, maxPeriodRows+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	periods := []costPeriod{}
	for rows.Next() {
		var period costPeriod
		var cost string
		if err := rows.Scan(
			&period.Scope, &period.ScopeID, &period.PeriodStart, &period.PeriodEnd,
			&cost, &period.LastUpdated, &period.PendingReconciliation,
		); err != nil {
			return nil, false, err
		}
		period.TotalCost = json.Number(cost)
		periods = append(periods, period)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(periods) > maxPeriodRows {
		return periods[:maxPeriodRows], true, nil
	}
	return periods, false, nil
}

// kpis is the headline: the project-scope total over the window, and enough
// metadata for a client to tell "nothing was spent" from "nothing was
// persisted".
//
// It reads the SQL aggregate, so it is unaffected by any capping of the row
// listing, and it takes the PROJECT scope alone — a narrower scope is a subset
// of the same spend, so adding it would count the same dollars twice.
func kpis(totals []scopeTotal, from, to time.Time) map[string]any {
	total := json.Number(zeroUSD)
	var counted int64
	for _, scope := range totals {
		if scope.Scope != budgetScopeProject {
			continue
		}
		total = scope.TotalCost
		counted = scope.Rows
	}
	return map[string]any{
		"total_cost": total,
		"currency":   currency,
		"periods":    counted,
		// False when the write-back path has persisted nothing for this project
		// in this window. The reference had no equivalent because an audit-event
		// query cannot tell the two apart; issue 246's surface reports the same
		// distinction under the same name.
		"spend_available": counted > 0,
		"window_days":     int(to.Sub(from).Hours() / 24),
	}
}

// zeroUSD is what a project with no persisted row reports. It carries the
// accumulator column's own scale so "no spend" and a real zero are the same
// literal, and neither is a bare `0` a client has to special-case.
const zeroUSD = "0.00000000"
