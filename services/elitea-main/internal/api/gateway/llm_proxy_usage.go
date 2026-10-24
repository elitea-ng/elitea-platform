package gateway

// llm_proxy_usage.go — the platform-wide usage report,
// `GET /api/v2/admin/gateway/usage`.
//
// ## What this is, and which LiteLLM screen it replaces
//
// LiteLLM's admin UI had a Usage page: spend over time, a per-model table,
// per-key and per-team breakdowns, token and request counts. When ADR-0015
// replaced LiteLLM with `services/elitea-llm-gateway`, the whole of it went with
// it, and migration 0084 recorded the loss in its own header — "the accumulator
// holds money per (scope, scope_id, period) and nothing else … so the port of
// that page has a meter and nothing else". `gateway.llm_usage_events` is the
// per-request ledger 0084 added to fix that, one row per BILLED request with
// provider, model, tokens, `api_requests` and `cost_usd`.
//
// Two surfaces read it. `internal/api/v2/budgets` answers the PROJECT-scoped
// question ("what has this project spent, and against what limit") for the
// product's own Settings → Usage. This file answers the PLATFORM one, which has
// no other home: what is this deployment spending in total, on which providers
// and models, and which projects and members account for it. That question is
// the reason an operator opens an admin panel, and until now the only answer
// available anywhere was a per-project bar the operator had to visit one project
// at a time to read.
//
// ## It is a REPORT, never a second source of truth
//
// The same rule the model catalogue states: no budget decision reads this, and
// nothing here writes. Summing this ledger and `gateway.llm_budget_accumulators`
// together would double-count, and nothing does. What the two are allowed to
// disagree about is bounded — the ledger is written in the same transaction as
// the accumulator UPSERT — but they answer different questions and this endpoint
// never presents one as the other.
//
// ## Retention bounds what any window could ever show
//
// The scheduler prunes the ledger on its write-back loop
// (`budgetwriteback.RetentionWindow`, 400 days), so the response carries
// `retention_days`. Every window offered here is far inside it — see
// usageRetentionDays for why the field is published regardless.
//
// ## Every failure is reported, never swallowed
//
// Each of the four sections is its own statement and its own error field, for
// the reason `ListModels` separates its unpriced report: a section that failed
// renders exactly like a section with nothing in it, and "no spend" is the
// reassuring reading an operator would take from an empty usage table. The
// totals are the one part whose failure refuses the whole response, because
// every other number on the screen is a share of them.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// usageReadTimeout bounds the four aggregate scans. They are wider than the
// catalogue's point reads — a 30-day window over a busy deployment scans every
// event in it — so the budget is larger, and it is still a budget: an admin
// screen that hangs is one an operator abandons during the incident they opened
// it for.
const usageReadTimeout = 20 * time.Second

// maxUsageRows caps each breakdown. These are "who accounts for the spend"
// tables, read top-down; the long tail is not what an operator came for, and an
// unbounded GROUP BY over projects or members is unbounded by TRAFFIC. Each
// section reports its own `truncated` so a capped list is never a silently short
// one.
const maxUsageRows = 25

// UsageTotals is the window's whole.
type UsageTotals struct {
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	// Models is the number of DISTINCT (provider, model) pairs called in the
	// window, which is not derivable from the capped breakdown below.
	Models int64 `json:"models"`
	// Projects, likewise, so "25 projects shown" can be read against the true
	// count rather than mistaken for it.
	Projects int64 `json:"projects"`
}

// UsageDay is one UTC day of the series.
//
// The bucket is UTC because billing periods are computed in UTC (migration
// 0084's note on `occurred_at`), and a chart bucketed in the viewer's zone would
// put spend in a different day from the period that billed it.
type UsageDay struct {
	Day         string  `json:"day"`
	Requests    int64   `json:"requests"`
	TotalTokens int64   `json:"total_tokens"`
	CostUSD     float64 `json:"cost_usd"`
}

// UsageSlice is one row of a breakdown — by model, by project or by member.
type UsageSlice struct {
	// Key identifies the row for React; it is a rendering handle, not an id.
	Key string `json:"key"`
	// Label is what an operator reads. For a project or a member it is the
	// resolved NAME where one could be resolved — see usageProjectsSQL.
	Label string `json:"label"`
	// Detail is the secondary line: the provider for a model, the numeric id
	// for a project or member. It is always populated for projects and members,
	// because two of them can carry the same display name and an operator
	// acting on the row needs the id.
	Detail           string  `json:"detail"`
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	// Operation, Units and Unit are set on a model slice only. A metered tool
	// operation (#1238) is a "model" row whose provider is the vendor and
	// whose model is the operation; it has no tokens, so the table shows its
	// units instead.
	Operation string `json:"operation,omitempty"`
	Units     *int64 `json:"units,omitempty"`
	Unit      string `json:"unit,omitempty"`
}

// The two values of the ledger's operation column (shared migration 0174).
const (
	usageOperationLLM  = "llm"
	usageOperationTool = "tool"
)

// usageTotalsSQL — the window's whole, in one scan.
const usageTotalsSQL = `
	SELECT COALESCE(SUM(api_requests), 0),
	       COALESCE(SUM(prompt_tokens), 0),
	       COALESCE(SUM(completion_tokens), 0),
	       COALESCE(SUM(total_tokens), 0),
	       COALESCE(SUM(cost_usd), 0),
	       COUNT(DISTINCT (provider, model)),
	       COUNT(DISTINCT project_id)
	  FROM gateway.llm_usage_events
	 WHERE occurred_at >= now() - $1::interval`

// usageDailySQL — the series.
//
// `date_trunc('day', occurred_at AT TIME ZONE 'UTC')` rather than a bare
// `date_trunc`, which would bucket in the SERVER's timezone: a deployment whose
// Postgres runs on local time would silently draw a chart whose days do not
// line up with the billing periods.
const usageDailySQL = `
	SELECT to_char(date_trunc('day', occurred_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD'),
	       COALESCE(SUM(api_requests), 0),
	       COALESCE(SUM(total_tokens), 0),
	       COALESCE(SUM(cost_usd), 0)
	  FROM gateway.llm_usage_events
	 WHERE occurred_at >= now() - $1::interval
	 GROUP BY 1
	 ORDER BY 1`

// usageModelsSQL — spend by (provider, model), most expensive first.
//
// Ordered by COST rather than by request count: the question this table answers
// is "what is this deployment paying for", and a cheap model called a million
// times is not the answer to it. The catalogue tab beside it orders by name,
// because there the question is "is this model priced".
const usageModelsSQL = `
	SELECT provider, model,
	       COALESCE(SUM(api_requests), 0),
	       COALESCE(SUM(prompt_tokens), 0),
	       COALESCE(SUM(completion_tokens), 0),
	       COALESCE(SUM(total_tokens), 0),
	       COALESCE(SUM(cost_usd), 0),
	       operation,
	       COALESCE(SUM(units), 0),
	       COALESCE(unit, '')
	  FROM gateway.llm_usage_events
	 WHERE occurred_at >= now() - $1::interval
	 GROUP BY provider, model, operation, unit
	 ORDER BY 7 DESC, 3 DESC
	 LIMIT $2`

// usageProjectsSQL — spend by project, with the project's name.
//
// A LEFT JOIN, not an inner one. The ledger holds a project id whether or not
// `centry.project` still has the row: a deleted project's spend is spend the
// deployment made, and dropping it would make the breakdown fail to sum to the
// totals beside it with no explanation. An unresolvable id renders as the id.
const usageProjectsSQL = `
	SELECT e.project_id, COALESCE(p.name, ''),
	       COALESCE(SUM(e.api_requests), 0),
	       COALESCE(SUM(e.prompt_tokens), 0),
	       COALESCE(SUM(e.completion_tokens), 0),
	       COALESCE(SUM(e.total_tokens), 0),
	       COALESCE(SUM(e.cost_usd), 0)
	  FROM gateway.llm_usage_events e
	  LEFT JOIN centry.project p ON p.id = e.project_id
	 WHERE e.occurred_at >= now() - $1::interval
	 GROUP BY e.project_id, p.name
	 ORDER BY 7 DESC
	 LIMIT $2`

// usageMembersSQL — spend by member, with the member's name or email.
//
// `WHERE e.user_id IS NOT NULL` is the point of the filter and not a tidying
// step. NULL means the call carried no resolvable member — a service account, a
// token-authenticated integration — which 0084 stores as NULL specifically so it
// stays distinguishable from member 0. Folding those rows into a member bucket
// would attribute a service account's spend to a person; they are visible in the
// project breakdown, where they belong.
const usageMembersSQL = `
	SELECT e.user_id, COALESCE(NULLIF(u.name, ''), u.email, ''),
	       COALESCE(SUM(e.api_requests), 0),
	       COALESCE(SUM(e.prompt_tokens), 0),
	       COALESCE(SUM(e.completion_tokens), 0),
	       COALESCE(SUM(e.total_tokens), 0),
	       COALESCE(SUM(e.cost_usd), 0)
	  FROM gateway.llm_usage_events e
	  LEFT JOIN public.auth_core__user u ON u.id = e.user_id
	 WHERE e.occurred_at >= now() - $1::interval
	   AND e.user_id IS NOT NULL
	 GROUP BY e.user_id, u.name, u.email
	 ORDER BY 7 DESC
	 LIMIT $2`

// Usage serves GET /gateway/usage.
func (h *LLMProxyHandler) Usage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), usageReadTimeout)
	defer cancel()

	window, interval := resolveWindow(r.URL.Query().Get("window"))
	pgInterval := strconv.FormatInt(int64(interval/time.Second), 10) + " seconds"

	if h == nil || h.db == nil {
		writeJSON(w, http.StatusOK, emptyUsageBody(window,
			"this deployment has no database pool, so usage cannot be read."))
		return
	}

	totals, err := h.queryUsageTotals(ctx, pgInterval)
	if err != nil {
		// The one failure that refuses the whole report. Every breakdown below
		// is a share of these numbers, and rendering shares against an unknown
		// whole invites an operator to read a partial table as the total.
		writeJSON(w, http.StatusOK, emptyUsageBody(window, err.Error()))
		return
	}

	body := map[string]any{
		"window": window,
		"totals": totals,
		// How far back the ledger goes. See usageRetentionDays.
		"retention_days": usageRetentionDays,
	}

	daily, dailyErr := h.queryUsageDaily(ctx, pgInterval)
	body["daily"] = daily
	attachUsageError(body, "daily_error", dailyErr)

	for _, section := range []struct {
		key      string
		errorKey string
		query    string
		scan     usageScanner
	}{
		{"models", "models_error", usageModelsSQL, scanUsageModel},
		{"projects", "projects_error", usageProjectsSQL, scanUsageProject},
		{"members", "members_error", usageMembersSQL, scanUsageMember},
	} {
		slices, sliceErr := h.queryUsageSlices(ctx, section.query, pgInterval, section.scan)
		body[section.key] = slices
		body[section.key+"_truncated"] = len(slices) >= maxUsageRows
		attachUsageError(body, section.errorKey, sliceErr)
	}

	writeJSON(w, http.StatusOK, body)
}

// usageRetentionDays restates `budgetwriteback.RetentionWindow` — 400 days.
//
// RESTATED, not imported, and there is no guard that can hold the two together:
// the constant lives in `services/elitea-scheduler/internal/budgetwriteback`,
// and an `internal/` package of another module is unimportable by construction.
// So this is a copy, and the honest thing is to say so here rather than to
// imply a link that does not exist. It is a compiled constant on both sides —
// the scheduler's window is deliberately not an environment variable, so no
// deployment can vary it — which makes drift a code change that edits one and
// not the other.
//
// NO WINDOW THIS ENDPOINT OFFERS CAN REACH PAST IT: the longest is 30 days
// against a 400-day retention, so the field cannot currently be the difference
// between a total and a truncated one. It is published anyway, for the case it
// exists to cover — a longer window added later, where an operator reading a
// truncated year as a cheap one is a mistake with no other warning — and so
// "how far back does this go" has an answer on the screen instead of in a
// migration header.
const usageRetentionDays = 400

// emptyUsageBody is the shape a failed or unbacked read answers with.
//
// Every collection is present and empty rather than absent, so the client
// renders an explained empty state instead of branching on undefined; the
// `error` beside them is what stops that empty state reading as "no spend".
func emptyUsageBody(window, reason string) map[string]any {
	return map[string]any{
		"window":         window,
		"totals":         UsageTotals{},
		"daily":          []UsageDay{},
		"models":         []UsageSlice{},
		"projects":       []UsageSlice{},
		"members":        []UsageSlice{},
		"retention_days": usageRetentionDays,
		"error":          reason,
	}
}

func attachUsageError(body map[string]any, key string, err error) {
	if err != nil {
		body[key] = err.Error()
	}
}

func (h *LLMProxyHandler) queryUsageTotals(ctx context.Context, pgInterval string) (UsageTotals, error) {
	var totals UsageTotals
	err := h.db.QueryRow(ctx, usageTotalsSQL, pgInterval).Scan(
		&totals.Requests, &totals.PromptTokens, &totals.CompletionTokens,
		&totals.TotalTokens, &totals.CostUSD, &totals.Models, &totals.Projects,
	)
	if err != nil {
		return UsageTotals{}, fmt.Errorf("read usage totals: %w", err)
	}
	return totals, nil
}

func (h *LLMProxyHandler) queryUsageDaily(ctx context.Context, pgInterval string) ([]UsageDay, error) {
	days := make([]UsageDay, 0)
	rows, err := h.db.Query(ctx, usageDailySQL, pgInterval)
	if err != nil {
		return days, fmt.Errorf("read the usage series: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var day UsageDay
		if scanErr := rows.Scan(&day.Day, &day.Requests, &day.TotalTokens, &day.CostUSD); scanErr != nil {
			return days, fmt.Errorf("read the usage series: %w", scanErr)
		}
		days = append(days, day)
	}
	if rows.Err() != nil {
		return days, fmt.Errorf("read the usage series: %w", rows.Err())
	}
	return days, nil
}

// queryUsageSlices runs one breakdown. The three differ only in their statement
// and how a row becomes a label, so they share everything else.
func (h *LLMProxyHandler) queryUsageSlices(
	ctx context.Context, query, pgInterval string, scan usageScanner,
) ([]UsageSlice, error) {
	slices := make([]UsageSlice, 0)
	rows, err := h.db.Query(ctx, query, pgInterval, maxUsageRows)
	if err != nil {
		return slices, fmt.Errorf("read the usage breakdown: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		slice, scanErr := scan(rows.Scan)
		if scanErr != nil {
			return slices, fmt.Errorf("read the usage breakdown: %w", scanErr)
		}
		slices = append(slices, slice)
	}
	if rows.Err() != nil {
		return slices, fmt.Errorf("read the usage breakdown: %w", rows.Err())
	}
	return slices, nil
}

// usageScanner turns one result row into a slice.
//
// It takes `rows.Scan` rather than the `pgx.Rows` itself, so each labelling rule
// — which is where the decisions are: an unresolvable project id, a member with
// no name — is testable without a database or a fake cursor. The three scanners
// below are the only place a row becomes something an operator reads.
type usageScanner func(scan func(...any) error) (UsageSlice, error)

func scanUsageModel(scan func(...any) error) (UsageSlice, error) {
	var provider, model, operation, unit string
	var units int64
	var slice UsageSlice
	if err := scan(&provider, &model, &slice.Requests, &slice.PromptTokens,
		&slice.CompletionTokens, &slice.TotalTokens, &slice.CostUSD,
		&operation, &units, &unit); err != nil {
		return UsageSlice{}, err
	}
	slice.Operation = operation
	if operation == usageOperationTool {
		slice.Units = &units
		slice.Unit = unit
	}
	slice.Key = provider + "/" + model
	if operation != "" && operation != usageOperationLLM {
		slice.Key += "#" + operation
	}
	slice.Label = model
	slice.Detail = provider
	return slice, nil
}

func scanUsageProject(scan func(...any) error) (UsageSlice, error) {
	var projectID int64
	var name string
	var slice UsageSlice
	if err := scan(&projectID, &name, &slice.Requests, &slice.PromptTokens,
		&slice.CompletionTokens, &slice.TotalTokens, &slice.CostUSD); err != nil {
		return UsageSlice{}, err
	}
	id := strconv.FormatInt(projectID, 10)
	slice.Key = "project:" + id
	// An unresolvable id renders AS the id rather than as a blank cell or an
	// invented "(deleted)": the row's spend is real, and the id is the only
	// true thing left to say about it.
	slice.Label = name
	if slice.Label == "" {
		slice.Label = "Project " + id
	}
	slice.Detail = id
	return slice, nil
}

func scanUsageMember(scan func(...any) error) (UsageSlice, error) {
	var userID int64
	var name string
	var slice UsageSlice
	if err := scan(&userID, &name, &slice.Requests, &slice.PromptTokens,
		&slice.CompletionTokens, &slice.TotalTokens, &slice.CostUSD); err != nil {
		return UsageSlice{}, err
	}
	id := strconv.FormatInt(userID, 10)
	slice.Key = "member:" + id
	slice.Label = name
	if slice.Label == "" {
		slice.Label = "User " + id
	}
	slice.Detail = id
	return slice, nil
}
