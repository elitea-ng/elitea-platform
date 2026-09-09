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
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// agentExecutionCapabilities is internal/infra/db/repos/analytics.go's
// agentCapabilities, duplicated rather than imported: this package must not
// import internal/infra/db/repos (a repos-package integration test builds this
// package's CostsHandler over the SAME migrated template
// GetAgentAnalytics' own tests use, and repos -> analytics -> repos would be an
// import cycle for that test binary). The two lists must never drift —
// agent_execution_jobs' own CHECK constraint (shared 0055) is the same pair,
// which is what keeps THAT copy from drifting either.
const agentExecutionCapabilities = `('agent.execute.application.v1', 'agent.execute.adhoc.v1')`

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

	// AgentDimensionAvailable is true when this deployment can correlate a
	// request to an agent at all — the same capability GetAgentAnalytics
	// (internal/infra/db/repos/analytics.go) reports as
	// AgentBreakdown.Available. False for a database that has not run shared
	// migration 0100, or for a window with no execution-tagged request.
	//
	// This is a MONEY view of the SAME correlation that endpoint already
	// serves as counts (issue 875): both read gateway.llm_request_logs
	// .execution_id and resolve it through elitea_runtime.execution_jobs into
	// the tenant chat projection. Nothing here is a second source of truth
	// about which requests are agent requests — see estimateByAgent.
	AgentDimensionAvailable bool `json:"agent_dimension_available"`
	// AttributedAgentCalls and UnattributedAgentCalls split the window's
	// PRICEABLE requests the same way GetAgentAnalytics splits ALL of them:
	// most /llm traffic is not made from a runtime execution, so ByAgent is
	// never expected to sum to Totals.Calls.
	AttributedAgentCalls   int64              `json:"attributed_agent_calls"`
	UnattributedAgentCalls int64              `json:"unattributed_agent_calls"`
	ByAgent                []estimateAgentRow `json:"by_agent,omitempty"`
	ByAgentTruncated       bool               `json:"by_agent_truncated"`

	// ToolDimensionAvailable is true when this deployment records tool calls
	// at all (elitea_runtime.tool_call_records, shared migration 0119) and can
	// resolve the request log's execution id (0100). It says nothing about
	// whether any ROW in the window correlates — see AttributedToolCalls.
	ToolDimensionAvailable bool `json:"tool_dimension_available"`
	// AttributedToolCalls and UnattributedToolCalls split the tool_call_records
	// rows in the window by whether they carry an execution id. A call an
	// agent made inside a turn carries one only from #875 onward
	// (internal/infra/db/repos/agent_trace.go); an explicit run
	// (toolkit.call_tool.v1) always did, but makes no LLM call itself, so it
	// contributes rows here and never money. A large UnattributedToolCalls is
	// the signal that ByTool's money covers a fraction of the Tools tab's own
	// call counts.
	AttributedToolCalls   int64             `json:"attributed_tool_calls"`
	UnattributedToolCalls int64             `json:"unattributed_tool_calls"`
	ByTool                []estimateToolRow `json:"by_tool,omitempty"`
	ByToolTruncated       bool              `json:"by_tool_truncated"`
}

// estimateAgentRows and estimateToolRows cap ByAgent and ByTool, in the same
// order as estimateModelRows and for the same reason (agentRowsLimit /
// toolRowsLimit, internal/infra/db/repos/analytics.go): the rows are one per
// AGENT or one per TOOL, not one per call, so a group-by can collapse a
// project's whole history into a much smaller table. The cut is reported
// (ByAgentTruncated / ByToolTruncated).
const (
	estimateAgentRows = 100
	estimateToolRows  = 100
)

// estimateAgentRow is one agent (application) in the window, money derived the
// same way estimateModelRow's is.
type estimateAgentRow struct {
	ApplicationID    string       `json:"application_id"`
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

// estimateToolRow is one (toolkit, tool) in the window.
//
// AttributedRuns is NOT the Tools tab's call count. It counts distinct
// EXECUTIONS that called this tool and that also correlate to at least one
// priceable request — not tool_call_records rows — because the money below is
// an execution's total LLM spend attributed to every tool that execution used,
// and an execution that called the same tool three times must not triple that
// attribution. A tool called twice inside one execution and never elsewhere
// therefore reads AttributedRuns: 1 here and RunCount: 2 on the Tools tab, and
// both are correct measurements of different things.
type estimateToolRow struct {
	ToolkitID        string       `json:"toolkit_id"`
	ToolkitName      string       `json:"toolkit_name"`
	ToolName         string       `json:"tool_name"`
	AttributedRuns   int64        `json:"attributed_runs"`
	PromptTokens     int64        `json:"prompt_tokens"`
	CompletionTokens int64        `json:"completion_tokens"`
	TotalTokens      int64        `json:"total_tokens"`
	Priced           bool         `json:"priced"`
	InputCost        *json.Number `json:"input_cost,omitempty"`
	OutputCost       *json.Number `json:"output_cost,omitempty"`
	TotalCost        *json.Number `json:"total_cost,omitempty"`
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
	if err := estimateByAgent(ctx, tx, estimate, pricesPresent, priced, projectID, from, to); err != nil {
		return nil, err
	}
	if err := estimateByTool(ctx, tx, estimate, pricesPresent, priced, projectID, from, to); err != nil {
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

/* ── agent and tool cost, issue 875 ───────────────────────────────────── */

// requestLogExecutionIDColumn probes for the column shared migration 0100
// adds to gateway.llm_request_logs — the same probe
// internal/infra/db/repos/analytics.go's agentExecutionColumn runs, duplicated
// here rather than shared because this package reads with raw pgx.Tx and that
// one reads through its own repository seam.
//
// The probe runs BEFORE any query that references the column, never as a
// caught error after: this file's queries share one REPEATABLE READ
// transaction with by_model, by_user and daily, and PostgreSQL aborts the
// WHOLE transaction on a failed statement, which would take those down too.
func requestLogExecutionIDColumn(ctx context.Context, tx pgx.Tx) (bool, error) {
	const query = `
SELECT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = 'gateway'
      AND table_name = 'llm_request_logs'
      AND column_name = 'execution_id'
)`
	var present bool
	if err := tx.QueryRow(ctx, query).Scan(&present); err != nil {
		return false, err
	}
	return present, nil
}

// estimateAgentAttribution splits the window's request-log rows into those
// that carry a usable execution id and those that do not — the same split
// GetAgentAnalytics' agentAttribution reports as AttributedCalls /
// UnattributedCalls, so the two numbers can be compared across the Overview
// and Agents tabs without reading two different predicates.
func estimateAgentAttribution(
	ctx context.Context, tx pgx.Tx, projectID int64, from, to time.Time,
) (attributed, unattributed int64, err error) {
	query := `
SELECT count(*) FILTER (WHERE l.execution_id IS NOT NULL AND EXISTS (
           SELECT 1 FROM elitea_runtime.execution_jobs AS j
           WHERE j.execution_id = l.execution_id
             AND j.capability_id IN ` + agentExecutionCapabilities + `
             AND (j.resource_project_id = l.project_id
                  OR j.projection_project_id = l.project_id)))::bigint,
       count(*)::bigint
FROM gateway.llm_request_logs AS l
WHERE l.project_id = $1
  AND l.occurred_at >= $2
  AND l.occurred_at < $3`

	var total int64
	if err := tx.QueryRow(ctx, query, projectID, from, to).Scan(&attributed, &total); err != nil {
		return 0, 0, fmt.Errorf("analytics: estimate agent attribution: %w", err)
	}
	return attributed, total - attributed, nil
}

// estimateByAgent is the ByAgent view: the SAME execution-to-agent correlation
// GetAgentAnalytics reads (internal/infra/db/repos/analytics.go's agentUsage —
// gateway.llm_request_logs.execution_id, resolved through
// elitea_runtime.execution_jobs into the tenant chat projection), priced
// through gateway.gateway_models the way ByModel and ByUser already are in
// this file.
//
// It is a SEPARATE query from agentUsage, not a shared one: that one answers
// on its own snapshot and reports token/duration/error counts and never money
// (analytics.go's own header: "Money is deliberately not read here"). This one
// runs inside the SAME REPEATABLE READ transaction Costs() opened for
// by_model, by_user and daily, so a reader comparing this table against them
// never sees two snapshots of a table the gateway commits into continuously.
func estimateByAgent(ctx context.Context, tx pgx.Tx, estimate *costEstimate,
	pricesPresent, priced bool, projectID int64, from, to time.Time,
) error {
	hasExecutionColumn, err := requestLogExecutionIDColumn(ctx, tx)
	if err != nil {
		return err
	}
	if !hasExecutionColumn {
		// A NAMED absence rather than an empty table — see AgentDimensionAvailable.
		return nil
	}
	// estimateAgentAttribution's query references elitea_runtime.execution_jobs
	// directly, with no probe of its own — a Go-bootstrapped database can carry
	// 0100's column (part of GatewayMigrationSQL's history) without ever having
	// run the elitea_runtime baseline, and PostgreSQL would answer 42P01/3F000
	// rather than zero rows. Checked here, not caught after: this read shares
	// the ONE REPEATABLE READ transaction Costs() opened for by_model, by_user
	// and daily, and a failed statement poisons all of them.
	jobsPresent, err := relationsPresent(ctx, tx, "elitea_runtime.execution_jobs")
	if err != nil {
		return err
	}
	if !jobsPresent {
		return nil
	}

	attributed, unattributed, err := estimateAgentAttribution(ctx, tx, projectID, from, to)
	if err != nil {
		return err
	}
	estimate.AttributedAgentCalls = attributed
	estimate.UnattributedAgentCalls = unattributed
	if attributed == 0 {
		// NOT AVAILABLE, and not "zero agent spend" — the pre-0100 window, or a
		// runtime that is not tagging its calls. See AgentBreakdown.Available.
		return nil
	}
	estimate.AgentDimensionAvailable = true

	schema := pgx.Identifier{"p_" + strconv.FormatInt(projectID, 10)}.Sanitize()
	present, err := relationsPresent(ctx, tx, schema+".chat_message_group", schema+".chat_participants")
	if err != nil {
		return err
	}
	if !present {
		// The chat projection is required for the execution-to-agent join and
		// is absent on a Go-bootstrapped database with no pylon history — the
		// same split agentUsage documents. The attributed count above is still
		// a true measurement of the window.
		estimate.ByAgent = []estimateAgentRow{}
		return nil
	}
	named, err := relationsPresent(ctx, tx, schema+".applications")
	if err != nil {
		return err
	}
	// A fixed fragment chosen from two constants, never assembled from request
	// data — mirrors agentUsage's own nameSelect/nameJoin/nameGroup split, and
	// for the same reason a bare literal cannot appear in GROUP BY.
	nameSelect, nameJoin, nameGroup := `''`, ``, ``
	if named {
		nameSelect = `coalesce(app.name, '')`
		nameJoin = `
LEFT JOIN ` + schema + `.applications AS app
       ON app.id = agent.application_id::integer`
		nameGroup = `, coalesce(app.name, '')`
	}

	query := `
WITH calls AS (
  SELECT l.execution_id, l.prompt_tokens, l.completion_tokens, ` + rateColumns(pricesPresent) +
		sourceClause(pricesPresent) + requestLogWindow + `
    AND l.execution_id IS NOT NULL
    AND EXISTS (
        SELECT 1 FROM elitea_runtime.execution_jobs AS j
        WHERE j.execution_id = l.execution_id
          AND j.capability_id IN ` + agentExecutionCapabilities + `
          AND (j.resource_project_id = l.project_id OR j.projection_project_id = l.project_id)
    )
), agent AS (
    SELECT DISTINCT ON (g.task_id)
           g.task_id AS execution_id,
           (author.entity_meta ->> 'id') AS application_id
    FROM ` + schema + `.chat_message_group AS g
    JOIN ` + schema + `.chat_participants AS author
      ON author.id = g.author_participant_id
     AND author.entity_name = 'application'
    WHERE g.task_id IN (SELECT DISTINCT execution_id FROM calls)
      AND author.entity_meta ->> 'id' ~ '^[1-9][0-9]*$'
    ORDER BY g.task_id, g.id
)
SELECT agent.application_id,
       ` + nameSelect + `,
       count(*)::bigint,
       coalesce(sum(calls.prompt_tokens), 0)::bigint,
       coalesce(sum(calls.completion_tokens), 0)::bigint,
       bool_or(` + pricedPredicate + `),
       ` + costExpr("prompt_tokens", "in_rate") + `,
       ` + costExpr("completion_tokens", "out_rate") + `,
       ` + totalCostExpr() + `
FROM calls
JOIN agent ON agent.execution_id = calls.execution_id` + nameJoin + `
GROUP BY agent.application_id` + nameGroup + `
ORDER BY count(*) DESC, agent.application_id ASC
LIMIT $4`

	rows, err := tx.Query(ctx, query, projectID, from, to, estimateAgentRows+1)
	if err != nil {
		return fmt.Errorf("analytics: estimate by agent: %w", err)
	}
	defer rows.Close()

	out := make([]estimateAgentRow, 0, estimateAgentRows)
	for rows.Next() {
		var row estimateAgentRow
		var rowPriced bool
		var inCost, outCost, totalCost string
		if err := rows.Scan(&row.ApplicationID, &row.Name, &row.Calls,
			&row.PromptTokens, &row.CompletionTokens, &rowPriced,
			&inCost, &outCost, &totalCost); err != nil {
			return err
		}
		row.TotalTokens = row.PromptTokens + row.CompletionTokens
		row.Priced = rowPriced
		row.InputCost, row.OutputCost, row.TotalCost = scanCosts(priced && rowPriced, inCost, outCost, totalCost)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(out) > estimateAgentRows {
		estimate.ByAgent, estimate.ByAgentTruncated = out[:estimateAgentRows], true
		return nil
	}
	estimate.ByAgent = out
	return nil
}

// estimateToolAttribution splits the window's tool_call_records rows into
// those that carry an execution id and those that do not, the same way
// estimateAgentAttribution splits the request log. See
// ToolCallRecord.ExecutionID (internal/infra/db/repos/tool_call_records.go)
// for which producer sets it and since when.
func estimateToolAttribution(
	ctx context.Context, tx pgx.Tx, projectID int64, from, to time.Time,
) (attributed, unattributed int64, err error) {
	const query = `
SELECT count(*) FILTER (WHERE r.execution_id IS NOT NULL)::bigint,
       count(*)::bigint
FROM elitea_runtime.tool_call_records AS r
WHERE r.project_id = $1
  AND r.started_at >= $2
  AND r.started_at < $3`

	var total int64
	if err := tx.QueryRow(ctx, query, projectID, from, to).Scan(&attributed, &total); err != nil {
		return 0, 0, fmt.Errorf("analytics: estimate tool attribution: %w", err)
	}
	return attributed, total - attributed, nil
}

// estimateByTool is the ByTool view.
//
// There is no producer that ties an LLM request directly to a tool: a
// completion decides whether to call a tool, but the token cost belongs to the
// COMPLETION, not to any one tool it may have invoked. So this attributes an
// EXECUTION's total LLM cost to every tool that execution called (#875) —
// correlated through elitea_runtime.tool_call_records.execution_id, which
// agent_trace.go's recordAgentToolCalls now stamps with the same value
// gateway.llm_request_logs.execution_id carries.
//
// AN EXECUTION THAT CALLS TWO TOOLS COUNTS ITS COST TWICE, ONCE PER TOOL. That
// is a deliberate fan-out, not a bug: ByTool answers "what did using this tool
// cost", not "how was the project's spend partitioned", and those are
// different questions with different arithmetic — the second one is
// Totals.TotalCost, and ByTool is never expected to sum to it, the same way
// ByAgent and ByModel already are not (each request has exactly one model but
// can touch several tools). A tool called more than once inside one execution
// is folded into ONE attribution (see estimateToolRow.AttributedRuns) so a
// repeated call cannot multiply the same execution's cost.
func estimateByTool(ctx context.Context, tx pgx.Tx, estimate *costEstimate,
	pricesPresent, priced bool, projectID int64, from, to time.Time,
) error {
	hasExecutionColumn, err := requestLogExecutionIDColumn(ctx, tx)
	if err != nil {
		return err
	}
	recordsPresent, err := relationsPresent(ctx, tx, "elitea_runtime.tool_call_records")
	if err != nil {
		return err
	}
	if !hasExecutionColumn || !recordsPresent {
		return nil
	}
	estimate.ToolDimensionAvailable = true

	attributed, unattributed, err := estimateToolAttribution(ctx, tx, projectID, from, to)
	if err != nil {
		return err
	}
	estimate.AttributedToolCalls = attributed
	estimate.UnattributedToolCalls = unattributed
	if attributed == 0 {
		estimate.ByTool = []estimateToolRow{}
		return nil
	}

	query := `
WITH calls AS (
  SELECT l.execution_id, l.prompt_tokens, l.completion_tokens, ` + rateColumns(pricesPresent) +
		sourceClause(pricesPresent) + requestLogWindow + `
    AND l.execution_id IS NOT NULL
), executions AS (
  SELECT execution_id,
         coalesce(sum(prompt_tokens), 0)::bigint AS prompt_tokens,
         coalesce(sum(completion_tokens), 0)::bigint AS completion_tokens,
         bool_or(` + pricedPredicate + `) AS priced,
         ` + costNumeric("prompt_tokens", "in_rate") + ` AS input_cost,
         ` + costNumeric("completion_tokens", "out_rate") + ` AS output_cost
  FROM calls
  GROUP BY execution_id
), tool_calls AS (
  SELECT DISTINCT r.execution_id, r.toolkit_id, r.toolkit_name, r.tool_name
  FROM elitea_runtime.tool_call_records AS r
  WHERE r.project_id = $1
    AND r.started_at >= $2
    AND r.started_at < $3
    AND r.execution_id IS NOT NULL
)
SELECT coalesce(tc.toolkit_id::text, ''),
       coalesce(tc.toolkit_name, ''),
       tc.tool_name,
       count(*)::bigint,
       coalesce(sum(e.prompt_tokens), 0)::bigint,
       coalesce(sum(e.completion_tokens), 0)::bigint,
       bool_or(e.priced),
       round(coalesce(sum(e.input_cost), 0), ` + strconv.Itoa(estimateCostScale) + `)::text,
       round(coalesce(sum(e.output_cost), 0), ` + strconv.Itoa(estimateCostScale) + `)::text,
       round(coalesce(sum(e.input_cost) + sum(e.output_cost), 0), ` + strconv.Itoa(estimateCostScale) + `)::text
FROM tool_calls AS tc
JOIN executions AS e ON e.execution_id = tc.execution_id
GROUP BY tc.toolkit_id, tc.toolkit_name, tc.tool_name
ORDER BY count(*) DESC, tc.tool_name ASC
LIMIT $4`

	rows, err := tx.Query(ctx, query, projectID, from, to, estimateToolRows+1)
	if err != nil {
		return fmt.Errorf("analytics: estimate by tool: %w", err)
	}
	defer rows.Close()

	out := make([]estimateToolRow, 0, estimateToolRows)
	for rows.Next() {
		var row estimateToolRow
		var rowPriced bool
		var inCost, outCost, totalCost string
		if err := rows.Scan(&row.ToolkitID, &row.ToolkitName, &row.ToolName, &row.AttributedRuns,
			&row.PromptTokens, &row.CompletionTokens, &rowPriced,
			&inCost, &outCost, &totalCost); err != nil {
			return err
		}
		row.TotalTokens = row.PromptTokens + row.CompletionTokens
		row.Priced = rowPriced
		row.InputCost, row.OutputCost, row.TotalCost = scanCosts(priced && rowPriced, inCost, outCost, totalCost)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(out) > estimateToolRows {
		estimate.ByTool, estimate.ByToolTruncated = out[:estimateToolRows], true
		return nil
	}
	estimate.ByTool = out
	return nil
}
