package analytics

// The COSTS and TOKENS dimensions, over the gateway request log.
//
// # Why this is a second source inside one endpoint
//
// `analytics_costs` reports the platform's ACCOUNTING figure, and that figure
// has exactly one shape: gateway.llm_budget_accumulators holds a summed USD
// NUMERIC per (scope, scope_id, period_start). It carries no model, no user, no
// day and no token, so it can never answer "which model spent this" — see the
// header of costs.go, and the test that pins the absence.
//
// gateway.llm_request_logs (shared migration 0099) carries the other half: one
// row PER CALL, with the project, the user, the provider, the model, the clock
// and the token counts. It is what the Overview tab's TOKENS tile and per-model
// table already read. It records no money.
//
// So the two dimensions the Costs and Tokens tabs need are: tokens, RECORDED by
// the request log; and cost, DERIVED from those tokens and the price catalogue
// in gateway.gateway_models. The derived figure is an ESTIMATE and this file
// never pretends otherwise — it lives under its own `estimate` key, so a reader
// cannot confuse it with `kpis.total_cost`, which is the accounted figure.
//
// # What is absent, and why it is absent rather than zero
//
//   - CACHE tokens. 0099 has prompt_tokens and completion_tokens and no third
//     pair. `cache_dimension_available` is a constant false, in the shape of
//     `tool_dimension_available` next door: the caller learns the deployment
//     cannot answer, instead of reading a fabricated 0.
//   - COST, when nothing in the window has a catalogue price. An operator who
//     has not populated gateway.gateway_models gets `cost_dimension_available:
//     false` and no money keys at all. A price of zero for every call would
//     render as "this project spent nothing", which is a different and false
//     claim.
//   - A per-row cost, for a model the catalogue does not price. That row keeps
//     its token counts, sets `priced: false` and omits its money keys, and
//     `unpriced_calls` says how many calls are in that state. Summing them as
//     zero would make an under-priced deployment look cheap.
//
// # Money stays exact
//
// Every cost is computed by PostgreSQL in NUMERIC and returned as a string
// (json.Number), the same treatment `total_cost` gets above. No float64 touches
// a price. The catalogue rate is per 1,000,000 tokens, which is the
// denomination the gateway's own calculator uses (internal/cost); dividing by a
// literal 1000000 in NUMERIC keeps the 1000x denomination error out.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Row caps. Both probe for one extra row, so a cut is reported rather than
// silent — the rule the neighbouring repository states at length.
const (
	estimateModelRows = 100
	estimateUserRows  = 200
)

// requestLogWindow is the window predicate, half-open at the top so a call at
// exactly `to` belongs to the next window and never to both.
//
// It is the same predicate internal/infra/db/repos/analytics.go applies, and it
// has to be: the Overview tab and the Costs tab paint the same window side by
// side, and two predicates that disagree at a boundary put two counts of one
// day on one screen.
const requestLogWindow = ` WHERE l.project_id = $1 AND l.occurred_at >= $2 AND l.occurred_at < $3`

// pricedSource is the FROM clause when the price catalogue is present.
//
// A LEFT JOIN, never an inner one: a call to a model the catalogue does not
// carry must still count its tokens. The rates come back NULL for such a row,
// and every cost aggregate below skips NULL terms, so an unpriced call adds
// nothing to the money and everything to the tokens.
const pricedSource = `
  FROM gateway.llm_request_logs AS l
  LEFT JOIN gateway.gateway_models AS m
    ON m.provider = l.provider AND m.model_name = l.model`

// unpricedSource is the same read with no catalogue to join.
//
// The rate columns become typed NULLs so every statement below keeps one shape.
// Naming them in the projection rather than branching each aggregate is what
// stops the two variants from drifting apart.
const unpricedSource = `
  FROM gateway.llm_request_logs AS l`

// costNumeric sums one side of the bill, in NUMERIC.
//
// sum() skips NULL terms, so a row whose rate is NULL contributes nothing. The
// coalesce covers the all-NULL case, where sum() itself answers NULL. The rate
// is per 1,000,000 tokens, the denomination the gateway calculator uses.
func costNumeric(tokens, rate string) string {
	return fmt.Sprintf("round(coalesce(sum(%s::numeric * %s) / 1000000, 0), %d)",
		tokens, rate, estimateCostScale)
}

// estimateCostScale is how many decimal places a published cost carries.
//
// Twelve, which is three digits FINER than the platform's own money unit: the
// gateway counts in int64 nano-USD, so 1e-9 USD is the smallest amount any
// billed figure can hold. A division by 1,000,000 in NUMERIC otherwise keeps
// growing the scale — an unrounded total came back as
// "0.001000000000000000000000" — and a response full of trailing zeros invites
// a client to reach for a float to tidy it.
const estimateCostScale = 12

// costExpr is costNumeric as the text the response publishes.
func costExpr(tokens, rate string) string { return costNumeric(tokens, rate) + "::text" }

// totalCostExpr adds the two sides in NUMERIC and only then makes it text, so
// the addition happens in PostgreSQL and never in Go.
func totalCostExpr() string {
	return "(" + costNumeric("prompt_tokens", "in_rate") +
		" + " + costNumeric("completion_tokens", "out_rate") + ")::text"
}

// pricedPredicate is true for a row the catalogue prices on either side.
const pricedPredicate = `(in_rate IS NOT NULL OR out_rate IS NOT NULL)`

// estimateTotals is the window roll-up. Tokens are recorded; money is derived.
type estimateTotals struct {
	Calls            int64 `json:"calls"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	// The three money fields are pointers so they disappear from the JSON when
	// no price produced them. See the header.
	InputCost  *json.Number `json:"input_cost,omitempty"`
	OutputCost *json.Number `json:"output_cost,omitempty"`
	TotalCost  *json.Number `json:"total_cost,omitempty"`
}

// estimateModelRow is one (provider, model) in the window.
type estimateModelRow struct {
	Provider         string       `json:"provider"`
	Model            string       `json:"model"`
	Calls            int64        `json:"calls"`
	PromptTokens     int64        `json:"prompt_tokens"`
	CompletionTokens int64        `json:"completion_tokens"`
	TotalTokens      int64        `json:"total_tokens"`
	Priced           bool         `json:"priced"`
	InputCost        *json.Number `json:"input_cost,omitempty"`
	OutputCost       *json.Number `json:"output_cost,omitempty"`
	TotalCost        *json.Number `json:"total_cost,omitempty"`
}

// estimateUserRow is one caller in the window.
type estimateUserRow struct {
	UserID int64 `json:"user_id"`
	// Email and Name are empty when the identity corpus is not on this
	// database. The row still carries its figures: a display name is a nicety,
	// and losing it must not lose the usage.
	Email            string       `json:"email"`
	Name             string       `json:"name"`
	Calls            int64        `json:"calls"`
	PromptTokens     int64        `json:"prompt_tokens"`
	CompletionTokens int64        `json:"completion_tokens"`
	TotalTokens      int64        `json:"total_tokens"`
	Priced           bool         `json:"priced"`
	InputCost        *json.Number `json:"input_cost,omitempty"`
	OutputCost       *json.Number `json:"output_cost,omitempty"`
	TotalCost        *json.Number `json:"total_cost,omitempty"`
}

// estimateDailyRow is one UTC day in the window. Days with no call are absent,
// not zero-filled — the chart draws what happened.
type estimateDailyRow struct {
	Date             string       `json:"date"`
	Calls            int64        `json:"calls"`
	PromptTokens     int64        `json:"prompt_tokens"`
	CompletionTokens int64        `json:"completion_tokens"`
	TotalTokens      int64        `json:"total_tokens"`
	InputCost        *json.Number `json:"input_cost,omitempty"`
	OutputCost       *json.Number `json:"output_cost,omitempty"`
	TotalCost        *json.Number `json:"total_cost,omitempty"`
}

// costEstimate is the whole block, published under `estimate`.
type costEstimate struct {
	// TokenDimensionAvailable is true when the request log answered. It is
	// always true when this struct exists at all — the block is omitted when
	// the table is not on the database — and it is published anyway so a client
	// reads a flag rather than inferring from a missing key.
	TokenDimensionAvailable bool `json:"token_dimension_available"`
	CostDimensionAvailable  bool `json:"cost_dimension_available"`
	// CacheDimensionAvailable is a constant false. 0099 records no cache token
	// counts, and there is no second source to take them from.
	CacheDimensionAvailable bool   `json:"cache_dimension_available"`
	Currency                string `json:"currency"`
	// PricedCalls and UnpricedCalls partition the window. Their sum is
	// Totals.Calls, and a large UnpricedCalls is the signal that the money
	// below covers a fraction of the traffic.
	PricedCalls      int64              `json:"priced_calls"`
	UnpricedCalls    int64              `json:"unpriced_calls"`
	Totals           estimateTotals     `json:"totals"`
	ByModel          []estimateModelRow `json:"by_model"`
	ByUser           []estimateUserRow  `json:"by_user"`
	Daily            []estimateDailyRow `json:"daily"`
	ByModelTruncated bool               `json:"by_model_truncated"`
	ByUserTruncated  bool               `json:"by_user_truncated"`
}

// relationsPresent reports whether every named relation exists.
//
// The check runs BEFORE the statement, and it takes the name as a STRING. A
// caught 42P01 does not contain the failure: every read here shares one
// transaction, and in PostgreSQL a failed statement aborts it, so everything
// after would answer 25P02. An in-query `WHERE to_regclass(...) IS NOT NULL` is
// useless for the same reason a catch is — PostgreSQL resolves a FROM-clause
// name at PARSE time.
//
// A probe that FAILS returns the error rather than false: a transient fault
// must not be recorded as a permanent absence.
func relationsPresent(ctx context.Context, tx pgx.Tx, names ...string) (bool, error) {
	const query = `SELECT bool_and(to_regclass(name) IS NOT NULL) FROM unnest($1::text[]) AS name`
	var present bool
	if err := tx.QueryRow(ctx, query, names).Scan(&present); err != nil {
		return false, err
	}
	return present, nil
}

// buildEstimate reads the request log and, when a catalogue prices it, the
// money that log implies.
//
// It returns nil when the request log is not on this database. The caller then
// omits the whole `estimate` key, which is the answer a deployment without the
// gateway's own tables must give.
func buildEstimate(ctx context.Context, tx pgx.Tx, projectID int64, from, to time.Time) (*costEstimate, error) {
	logPresent, err := relationsPresent(ctx, tx, "gateway.llm_request_logs")
	if err != nil {
		return nil, err
	}
	if !logPresent {
		return nil, nil
	}
	pricesPresent, err := relationsPresent(ctx, tx, "gateway.gateway_models")
	if err != nil {
		return nil, err
	}

	estimate := &costEstimate{
		TokenDimensionAvailable: true,
		CacheDimensionAvailable: false,
		Currency:                currency,
	}
	if err := estimateWindowTotals(ctx, tx, estimate, pricesPresent, projectID, from, to); err != nil {
		return nil, err
	}
	// A catalogue that prices nothing in this window is the same answer as no
	// catalogue at all: there is no money to report. Deciding it from the
	// COUNT rather than from the table's existence is what makes an empty
	// catalogue honest instead of free.
	estimate.CostDimensionAvailable = estimate.PricedCalls > 0
	priced := estimate.CostDimensionAvailable

	if err := estimateByModel(ctx, tx, estimate, pricesPresent, priced, projectID, from, to); err != nil {
		return nil, err
	}
	if err := estimateByUser(ctx, tx, estimate, pricesPresent, priced, projectID, from, to); err != nil {
		return nil, err
	}
	if err := estimateDaily(ctx, tx, estimate, pricesPresent, priced, projectID, from, to); err != nil {
		return nil, err
	}
	if !priced {
		// The totals were read before the flag was known, so drop the money
		// they carry. It is zero by construction here, and a published zero is
		// the fabricated figure this package refuses to print.
		estimate.Totals.InputCost, estimate.Totals.OutputCost, estimate.Totals.TotalCost = nil, nil, nil
	}
	return estimate, nil
}

// rateColumns names the two rate expressions for the chosen source.
func rateColumns(pricesPresent bool) string {
	if pricesPresent {
		return `m.input_cost_per_1m_tokens AS in_rate, m.output_cost_per_1m_tokens AS out_rate`
	}
	return `NULL::numeric AS in_rate, NULL::numeric AS out_rate`
}

func sourceClause(pricesPresent bool) string {
	if pricesPresent {
		return pricedSource
	}
	return unpricedSource
}

// scanCosts folds the two raw NUMERIC strings into the three published money
// fields, or into nothing when the window has no price.
//
// The total is summed as text by PostgreSQL rather than added here, so no Go
// arithmetic touches the money.
func scanCosts(priced bool, in, out, total string) (*json.Number, *json.Number, *json.Number) {
	if !priced {
		return nil, nil, nil
	}
	inCost, outCost, totalCost := json.Number(in), json.Number(out), json.Number(total)
	return &inCost, &outCost, &totalCost
}

func estimateWindowTotals(ctx context.Context, tx pgx.Tx, estimate *costEstimate,
	pricesPresent bool, projectID int64, from, to time.Time,
) error {
	query := `
WITH calls AS (
  SELECT l.prompt_tokens, l.completion_tokens, ` + rateColumns(pricesPresent) +
		sourceClause(pricesPresent) + requestLogWindow + `
)
SELECT count(*)::bigint,
       coalesce(sum(prompt_tokens), 0)::bigint,
       coalesce(sum(completion_tokens), 0)::bigint,
       count(*) FILTER (WHERE ` + pricedPredicate + `)::bigint,
       ` + costExpr("prompt_tokens", "in_rate") + `,
       ` + costExpr("completion_tokens", "out_rate") + `,
       ` + totalCostExpr() + `
FROM calls`

	var inCost, outCost, totalCost string
	if err := tx.QueryRow(ctx, query, projectID, from, to).Scan(
		&estimate.Totals.Calls,
		&estimate.Totals.PromptTokens,
		&estimate.Totals.CompletionTokens,
		&estimate.PricedCalls,
		&inCost, &outCost, &totalCost,
	); err != nil {
		return err
	}
	estimate.Totals.TotalTokens = estimate.Totals.PromptTokens + estimate.Totals.CompletionTokens
	estimate.UnpricedCalls = estimate.Totals.Calls - estimate.PricedCalls
	estimate.Totals.InputCost, estimate.Totals.OutputCost, estimate.Totals.TotalCost =
		scanCosts(true, inCost, outCost, totalCost)
	return nil
}

func estimateByModel(ctx context.Context, tx pgx.Tx, estimate *costEstimate,
	pricesPresent, priced bool, projectID int64, from, to time.Time,
) error {
	query := `
WITH calls AS (
  SELECT l.provider, l.model, l.prompt_tokens, l.completion_tokens, ` + rateColumns(pricesPresent) +
		sourceClause(pricesPresent) + requestLogWindow + ` AND l.model <> ''
)
SELECT provider, model,
       count(*)::bigint,
       coalesce(sum(prompt_tokens), 0)::bigint,
       coalesce(sum(completion_tokens), 0)::bigint,
       bool_or(` + pricedPredicate + `),
       ` + costExpr("prompt_tokens", "in_rate") + `,
       ` + costExpr("completion_tokens", "out_rate") + `,
       ` + totalCostExpr() + `
FROM calls
GROUP BY provider, model
ORDER BY count(*) DESC, model ASC
LIMIT $4`

	rows, err := tx.Query(ctx, query, projectID, from, to, estimateModelRows+1)
	if err != nil {
		return err
	}
	defer rows.Close()

	out := make([]estimateModelRow, 0, estimateModelRows)
	for rows.Next() {
		var row estimateModelRow
		var inCost, outCost, totalCost string
		if err := rows.Scan(&row.Provider, &row.Model, &row.Calls,
			&row.PromptTokens, &row.CompletionTokens, &row.Priced,
			&inCost, &outCost, &totalCost); err != nil {
			return err
		}
		row.TotalTokens = row.PromptTokens + row.CompletionTokens
		row.InputCost, row.OutputCost, row.TotalCost = scanCosts(priced && row.Priced, inCost, outCost, totalCost)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(out) > estimateModelRows {
		estimate.ByModel, estimate.ByModelTruncated = out[:estimateModelRows], true
		return nil
	}
	estimate.ByModel = out
	return nil
}

func estimateByUser(ctx context.Context, tx pgx.Tx, estimate *costEstimate,
	pricesPresent, priced bool, projectID int64, from, to time.Time,
) error {
	query := `
WITH calls AS (
  SELECT l.user_id, l.prompt_tokens, l.completion_tokens, ` + rateColumns(pricesPresent) +
		sourceClause(pricesPresent) + requestLogWindow + ` AND l.user_id IS NOT NULL
)
SELECT user_id,
       count(*)::bigint,
       coalesce(sum(prompt_tokens), 0)::bigint,
       coalesce(sum(completion_tokens), 0)::bigint,
       bool_or(` + pricedPredicate + `),
       ` + costExpr("prompt_tokens", "in_rate") + `,
       ` + costExpr("completion_tokens", "out_rate") + `,
       ` + totalCostExpr() + `
FROM calls
GROUP BY user_id
ORDER BY count(*) DESC, user_id ASC
LIMIT $4`

	rows, err := tx.Query(ctx, query, projectID, from, to, estimateUserRows+1)
	if err != nil {
		return err
	}
	defer rows.Close()

	out := make([]estimateUserRow, 0, estimateUserRows)
	ids := make([]int64, 0, estimateUserRows)
	for rows.Next() {
		var row estimateUserRow
		var inCost, outCost, totalCost string
		if err := rows.Scan(&row.UserID, &row.Calls,
			&row.PromptTokens, &row.CompletionTokens, &row.Priced,
			&inCost, &outCost, &totalCost); err != nil {
			return err
		}
		row.TotalTokens = row.PromptTokens + row.CompletionTokens
		row.InputCost, row.OutputCost, row.TotalCost = scanCosts(priced && row.Priced, inCost, outCost, totalCost)
		out = append(out, row)
		ids = append(ids, row.UserID)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(out) > estimateUserRows {
		out, estimate.ByUserTruncated = out[:estimateUserRows], true
		ids = ids[:estimateUserRows]
	}
	if err := attachIdentities(ctx, tx, out, ids); err != nil {
		return err
	}
	estimate.ByUser = out
	return nil
}

// attachIdentities fills in the display email and name.
//
// It is a SEPARATE statement behind its own guard because public.auth_core__user
// belongs to another corpus and a Go-bootstrapped database has none. Its
// absence must cost the rows their display names and never their figures.
func attachIdentities(ctx context.Context, tx pgx.Tx, rows []estimateUserRow, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	present, err := relationsPresent(ctx, tx, "public.auth_core__user")
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	const query = `SELECT u.id, coalesce(u.email, ''), coalesce(u.name, '')
FROM public.auth_core__user AS u WHERE u.id = ANY($1)`
	identities, err := tx.Query(ctx, query, ids)
	if err != nil {
		return err
	}
	defer identities.Close()

	byID := make(map[int64][2]string, len(ids))
	for identities.Next() {
		var id int64
		var email, name string
		if err := identities.Scan(&id, &email, &name); err != nil {
			return err
		}
		byID[id] = [2]string{email, name}
	}
	if err := identities.Err(); err != nil {
		return err
	}
	for i := range rows {
		if identity, ok := byID[rows[i].UserID]; ok {
			rows[i].Email, rows[i].Name = identity[0], identity[1]
		}
	}
	return nil
}

func estimateDaily(ctx context.Context, tx pgx.Tx, estimate *costEstimate,
	pricesPresent, priced bool, projectID int64, from, to time.Time,
) error {
	query := `
WITH calls AS (
  SELECT l.occurred_at, l.prompt_tokens, l.completion_tokens, ` + rateColumns(pricesPresent) +
		sourceClause(pricesPresent) + requestLogWindow + `
)
SELECT to_char(date_trunc('day', occurred_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD'),
       count(*)::bigint,
       coalesce(sum(prompt_tokens), 0)::bigint,
       coalesce(sum(completion_tokens), 0)::bigint,
       ` + costExpr("prompt_tokens", "in_rate") + `,
       ` + costExpr("completion_tokens", "out_rate") + `,
       ` + totalCostExpr() + `
FROM calls
GROUP BY 1
ORDER BY 1`

	rows, err := tx.Query(ctx, query, projectID, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()

	out := make([]estimateDailyRow, 0)
	for rows.Next() {
		var row estimateDailyRow
		var inCost, outCost, totalCost string
		if err := rows.Scan(&row.Date, &row.Calls,
			&row.PromptTokens, &row.CompletionTokens,
			&inCost, &outCost, &totalCost); err != nil {
			return err
		}
		row.TotalTokens = row.PromptTokens + row.CompletionTokens
		row.InputCost, row.OutputCost, row.TotalCost = scanCosts(priced, inCost, outCost, totalCost)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	estimate.Daily = out
	return nil
}
