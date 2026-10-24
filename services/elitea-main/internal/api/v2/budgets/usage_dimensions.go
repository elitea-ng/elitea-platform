package budgets

// The dimensional half of the usage report (issue #320).
//
// LiteLLM kept a per-tag daily ledger and legacy read it: usage.py assembled a
// daily breakdown and a per-model breakdown carrying spend, total_tokens and
// api_requests, and the Settings → Usage screen drew both plus the token
// columns and an Excel export. The Go port had a meter and nothing else,
// because the write-back path persisted one accumulated USD figure per scope
// and period and there was no ledger behind it.
//
// gateway.llm_usage_events is that ledger. These reads aggregate it; they never
// price anything and never estimate a token. Every figure here is a sum of
// values the provider reported and the gateway billed.
//
// # Why these numbers cannot double-count the accumulator
//
// They are the same money, aggregated two ways from one delta. The gateway
// publishes ONE delta per (request, scope); the write-back consumer folds its
// nano-USD into the accumulator and, inside the same dedup gate, appends one
// ledger row under the same event id. A request that bills both the project and
// the member scope publishes two deltas and only the project one carries
// dimensions, so the ledger holds one row per request, not two.
//
// So SUM(cost_usd) over a period equals that period's project accumulator, and
// nothing anywhere adds the two together. The accumulator remains the only
// table a budget decision reads.

import (
	"context"
	"encoding/json"
	"fmt"
)

// usageDimensions is the dimensional block attached to a usage payload.
//
// Every field is a pointer or a slice that marshals to null/absent when the
// ledger has nothing to report, and the whole block is omitted rather than
// zero-filled — see Available.
type usageDimensions struct {
	// Available reports whether the ledger holds ANY row for this scope in this
	// period. False means "no data", which is not the same claim as "no calls":
	// a deployment upgraded mid-period has accumulator spend from before the
	// ledger existed. When it is false the three views below are absent.
	Available bool `json:"usage_events_available"`

	PromptTokens     *int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens *int64 `json:"completion_tokens,omitempty"`
	TotalTokens      *int64 `json:"total_tokens,omitempty"`
	APIRequests      *int64 `json:"api_requests,omitempty"`

	// Daily is the per-day series, one entry per day that had at least one
	// call. Days with no calls are NOT filled with zeros: the series covers a
	// period whose remaining days have not happened yet, and a zero for a future
	// day is a claim about calls nobody could have made.
	Daily []usageDailyRow `json:"daily,omitempty"`
	// Models is the per-model table, ordered by spend.
	Models []usageModelRow `json:"models,omitempty"`
}

// usageDailyRow is one day of the series.
type usageDailyRow struct {
	Date         string       `json:"date"`
	Spend        *json.Number `json:"spend"`
	TotalTokens  int64        `json:"total_tokens"`
	APIRequests  int64        `json:"api_requests"`
	PromptTokens int64        `json:"prompt_tokens"`
	Completion   int64        `json:"completion_tokens"`
}

// usageModelRow is one model's total for the period.
//
// A metered tool operation (#1238) is a row here too: provider is the vendor
// (`tavily`), model is the operation (`web_search`), operation is "tool", and
// Units/Unit say what was consumed, since a tool row has no tokens.
type usageModelRow struct {
	Provider     string       `json:"provider"`
	Model        string       `json:"model"`
	Operation    string       `json:"operation"`
	Spend        *json.Number `json:"spend"`
	TotalTokens  int64        `json:"total_tokens"`
	APIRequests  int64        `json:"api_requests"`
	PromptTokens int64        `json:"prompt_tokens"`
	Completion   int64        `json:"completion_tokens"`
	Units        *int64       `json:"units,omitempty"`
	Unit         string       `json:"unit,omitempty"`
}

// usageScopeFilter is the ledger predicate for a scope. The user scope adds
// `user_id = $4`, so a member's own Usage tab reports THEIR calls rather than
// their project's.
//
// It is a fixed fragment chosen from two constants, never assembled from
// request data.
const (
	usageFilterProject = ``
	usageFilterUser    = ` AND user_id = $4`
)

// The period bound is the accumulator's OWN key, not a time range over
// occurred_at.
//
// period_start is the exact value the gateway billed this request into, and it
// is what keys the accumulator row the meter reads. Selecting the ledger by the
// same value is what makes the chart and the meter describe one set of calls.
// A range over occurred_at would not: a request billed at 23:59 on the last of
// the month has its money in that month's accumulator, and this endpoint would
// have to decide separately whether its ledger row belongs to the period. It
// does not have to decide — the writer already did.
//
// occurred_at then does one job: it names the DAY inside the period, for the
// per-day series. It is the gateway's billing instant, not a write-time
// default, so the day it names is the day the call was made.
//
// The scope filter is appended to THIS fragment, never to a finished query:
// the aggregates below end in GROUP BY / ORDER BY, and a filter concatenated
// onto the end of one of those is a syntax error. It is an error only for the
// USER scope, because the project scope's filter is the empty string — so the
// project-scope tests would pass and the member view alone would 500.
const usagePeriodBound = ` WHERE project_id = $1 AND period_start = $2 AND period_end > $3`

// usageQuery assembles one aggregate: its SELECT ... FROM head, the period
// bound, the caller's scope filter, and its grouping tail.
func usageQuery(head, filter, tail string) string {
	return head + usagePeriodBound + filter + tail
}

// usageTotalsHead sums the whole period. Money stays NUMERIC and comes back as
// text, like every other money figure in this package.
const usageTotalsHead = `SELECT
	COALESCE(SUM(prompt_tokens), 0),
	COALESCE(SUM(completion_tokens), 0),
	COALESCE(SUM(total_tokens), 0),
	COALESCE(SUM(api_requests), 0),
	COUNT(*)
FROM gateway.llm_usage_events`

// usageDailyHead buckets on occurred_at in UTC — the zone billing periods are
// computed in, so a day boundary and a period boundary cannot disagree.
const usageDailyHead = `SELECT
	to_char((occurred_at AT TIME ZONE 'UTC')::date, 'YYYY-MM-DD') AS day,
	SUM(cost_usd)::text,
	COALESCE(SUM(prompt_tokens), 0),
	COALESCE(SUM(completion_tokens), 0),
	COALESCE(SUM(total_tokens), 0),
	COALESCE(SUM(api_requests), 0)
FROM gateway.llm_usage_events`

const usageDailyTail = `
GROUP BY day
ORDER BY day`

// usageModelsHead is the per-model table. It orders by spend so the models that
// matter are first, then by name so the order is stable when two models cost
// the same — an unstable order makes an export diff noise. Grouping on the
// operation and unit too keeps a tool row from merging into a model that
// happens to share its provider and name.
const usageModelsHead = `SELECT
	provider,
	model,
	operation,
	SUM(cost_usd)::text,
	COALESCE(SUM(prompt_tokens), 0),
	COALESCE(SUM(completion_tokens), 0),
	COALESCE(SUM(total_tokens), 0),
	COALESCE(SUM(api_requests), 0),
	SUM(units),
	COALESCE(unit, '')
FROM gateway.llm_usage_events`

const usageModelsTail = `
GROUP BY provider, model, operation, unit
ORDER BY SUM(cost_usd) DESC, provider, model, operation`

// readUsageDimensions assembles the dimensional block for one scope.
//
// It returns Available=false and no views when the ledger holds no row for the
// scope in the period. It does NOT return an empty daily series in that case:
// the caller marshals the block with omitempty, so the client sees the fields
// missing and can tell "the ledger has nothing for you" from "you made no
// calls", which is the distinction the package doc is about.
func (h *Handler) readUsageDimensions(
	ctx context.Context, projectID int64, userID *int64, period reportingPeriod,
) (usageDimensions, error) {
	// $2 is the accumulator's period_start. $3 repeats period.start against
	// period_end, so a row whose period ENDED at or before this period's start
	// cannot match — a cheap guard against a writer that ever reuses a
	// period_start for a different window.
	filter := usageFilterProject
	args := []any{projectID, period.start, period.start}
	if userID != nil {
		filter = usageFilterUser
		args = append(args, *userID)
	}

	var dims usageDimensions
	var prompt, completion, total, requests, rowCount int64
	if err := h.pool.QueryRow(ctx, usageQuery(usageTotalsHead, filter, ""), args...).Scan(
		&prompt, &completion, &total, &requests, &rowCount,
	); err != nil {
		return usageDimensions{}, fmt.Errorf("budgets: read usage totals: %w", err)
	}
	if rowCount == 0 {
		return dims, nil
	}
	dims.Available = true
	dims.PromptTokens = &prompt
	dims.CompletionTokens = &completion
	dims.TotalTokens = &total
	dims.APIRequests = &requests

	daily, err := h.readUsageDaily(ctx, usageQuery(usageDailyHead, filter, usageDailyTail), args)
	if err != nil {
		return usageDimensions{}, err
	}
	dims.Daily = daily

	models, err := h.readUsageModels(ctx, usageQuery(usageModelsHead, filter, usageModelsTail), args)
	if err != nil {
		return usageDimensions{}, err
	}
	dims.Models = models
	return dims, nil
}

func (h *Handler) readUsageDaily(ctx context.Context, query string, args []any) ([]usageDailyRow, error) {
	rows, err := h.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("budgets: read usage daily series: %w", err)
	}
	defer rows.Close()

	series := make([]usageDailyRow, 0)
	for rows.Next() {
		var (
			row   usageDailyRow
			spend *string
		)
		if err := rows.Scan(&row.Date, &spend, &row.PromptTokens, &row.Completion,
			&row.TotalTokens, &row.APIRequests); err != nil {
			return nil, fmt.Errorf("budgets: scan usage daily row: %w", err)
		}
		row.Spend = numeric(spend)
		series = append(series, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("budgets: read usage daily series: %w", err)
	}
	return series, nil
}

func (h *Handler) readUsageModels(ctx context.Context, query string, args []any) ([]usageModelRow, error) {
	rows, err := h.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("budgets: read usage model table: %w", err)
	}
	defer rows.Close()

	table := make([]usageModelRow, 0)
	for rows.Next() {
		var (
			row   usageModelRow
			spend *string
		)
		if err := rows.Scan(&row.Provider, &row.Model, &row.Operation, &spend, &row.PromptTokens,
			&row.Completion, &row.TotalTokens, &row.APIRequests, &row.Units, &row.Unit); err != nil {
			return nil, fmt.Errorf("budgets: scan usage model row: %w", err)
		}
		row.Spend = numeric(spend)
		table = append(table, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("budgets: read usage model table: %w", err)
	}
	return table, nil
}
