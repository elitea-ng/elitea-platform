package analytics

// The EVALUATION spend term of the cost estimate (legacy issues 6677 and
// 6678).
//
// The evaluation orchestrator signs every model call it makes with an
// attribution id (internal/api/v2/evaluation/attribution.go):
//
//	eval:<run>:case:<case>   the agent turn
//	eval:<run>:judge:<case>  a judge call
//
// These ids name no execution_jobs row, so the agent attribution above counts
// them as UNATTRIBUTED and estimateByAgent never sees them. That is correct
// for the agent breakdown, which is about runtime executions, and it is why
// this block exists: without it the spend of every evaluation run was visible
// only inside the model and user totals, with no way to say which part of it
// was evaluation.
//
// The block gives the window's evaluation total, its agent and judge roles
// side by side, and one row per evaluated agent (via the tenant eval_runs
// row the run id names).
//
// AVAILABILITY IS A PROPERTY OF THE WINDOW. Nothing signed an evaluation call
// before the release that ran shared migration 0140, so a window that ENDS
// before that migration was applied cannot speak for evaluation spend. It is
// reported unavailable, with no figures, rather than as zero.
//
// A window that STARTS before that time and ends after it holds only the
// attributed part of its evaluation spend. It is reported with partial=true
// and attributed_since, so a client does not show a part as the whole. The
// boundary is the migration's apply time, which is also an approximation: a
// pod of the previous release can still run an evaluation between the
// migration and the rollout. That gap is one more reason a window close to
// attributed_since is partial.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// evaluationAttributionMigration is the ledger NAME of the migration that
// shipped with the evaluation attribution. Matched by name so a renumbering
// at merge time cannot move the boundary.
const evaluationAttributionMigration = "execution_trigger_origin"

// estimateEvaluationAgentRows caps ByAgent, for the reason estimateAgentRows
// caps the agent view.
const estimateEvaluationAgentRows = 100

// evaluationCallPredicate selects the evaluation calls. The prefix holds no
// LIKE wildcard, so no ESCAPE is needed.
const evaluationCallPredicate = `l.execution_id LIKE 'eval:%'`

// estimateEvaluation is the `estimate.evaluation` block.
type estimateEvaluation struct {
	// Available is false for a window that ends before evaluation calls were
	// attributed. Every other field is then absent.
	Available bool `json:"evaluation_dimension_available"`
	// AttributedSince is when this database began attributing evaluation
	// calls. Partial is true when the window starts before it: the figures
	// then cover only the attributed part of the window.
	AttributedSince *time.Time `json:"attributed_since,omitempty"`
	Partial         bool       `json:"partial,omitempty"`
	// Runs is how many evaluation runs made a call in the window.
	Runs   int64           `json:"runs,omitempty"`
	Totals *estimateTotals `json:"totals,omitempty"`
	// Agent and Judge are the two roles. They add up to Totals, and the
	// response never adds one into the other.
	Agent            *estimateTotals           `json:"agent,omitempty"`
	Judge            *estimateTotals           `json:"judge,omitempty"`
	ByAgent          []estimateEvaluationAgent `json:"by_agent,omitempty"`
	ByAgentTruncated bool                      `json:"by_agent_truncated,omitempty"`
}

// estimateEvaluationAgent is one evaluated agent: every run against it in the
// window. ApplicationID is empty for a run that names no agent.
type estimateEvaluationAgent struct {
	ApplicationID string `json:"application_id"`
	Name          string `json:"name"`
	Runs          int64  `json:"runs"`
	estimateTotals
	Priced bool `json:"priced"`
}

// buildEstimateEvaluation fills estimate.Evaluation. It runs inside the
// caller's snapshot transaction, so every probe is asked before a statement
// names what it probes.
func buildEstimateEvaluation(ctx context.Context, tx pgx.Tx, estimate *costEstimate,
	pricesPresent, priced bool, projectID int64, from, to time.Time,
) error {
	hasExecutionColumn, err := requestLogExecutionIDColumn(ctx, tx)
	if err != nil || !hasExecutionColumn {
		return err
	}
	since, err := evaluationAttributedSince(ctx, tx)
	if err != nil {
		return err
	}
	block := &estimateEvaluation{}
	estimate.Evaluation = block
	if since.IsZero() || !to.After(since) {
		return nil
	}
	block.Available = true
	attributedSince := since.UTC()
	block.AttributedSince = &attributedSince
	block.Partial = from.Before(since)

	totals, runs, err := evaluationTotals(ctx, tx, pricesPresent, priced, projectID, from, to, `'eval:%'`)
	if err != nil {
		return err
	}
	block.Totals, block.Runs = &totals, runs
	agent, _, err := evaluationTotals(ctx, tx, pricesPresent, priced, projectID, from, to, `'eval:%:case:%'`)
	if err != nil {
		return err
	}
	judge, _, err := evaluationTotals(ctx, tx, pricesPresent, priced, projectID, from, to, `'eval:%:judge:%'`)
	if err != nil {
		return err
	}
	block.Agent, block.Judge = &agent, &judge
	return evaluationByAgent(ctx, tx, block, pricesPresent, priced, projectID, from, to)
}

// evaluationAttributedSince is when this database began attributing
// evaluation calls: the apply time of evaluationAttributionMigration. Zero
// when the ledger has no such row.
func evaluationAttributedSince(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	present, err := relationsPresent(ctx, tx, "elitea_runtime.schema_migrations")
	if err != nil || !present {
		return time.Time{}, err
	}
	var appliedAt *time.Time
	if err := tx.QueryRow(ctx, `
SELECT min(applied_at)
FROM elitea_runtime.schema_migrations
WHERE target_kind = 'shared' AND name = $1`, evaluationAttributionMigration).Scan(&appliedAt); err != nil {
		return time.Time{}, fmt.Errorf("analytics: evaluation attribution probe: %w", err)
	}
	if appliedAt == nil {
		return time.Time{}, nil
	}
	return *appliedAt, nil
}

// evaluationTotals sums the window's calls whose execution id matches
// pattern, a FIXED literal chosen by the caller and never request data.
func evaluationTotals(ctx context.Context, tx pgx.Tx, pricesPresent, priced bool,
	projectID int64, from, to time.Time, pattern string,
) (estimateTotals, int64, error) {
	query := `
WITH calls AS (
  SELECT l.execution_id, l.prompt_tokens, l.completion_tokens, ` + rateColumns(pricesPresent) +
		sourceClause(pricesPresent) + requestLogWindow + `
    AND l.execution_id LIKE ` + pattern + `
)
SELECT count(*)::bigint,
       count(DISTINCT split_part(execution_id, ':', 2))::bigint,
       coalesce(sum(prompt_tokens), 0)::bigint,
       coalesce(sum(completion_tokens), 0)::bigint,
       ` + costExpr("prompt_tokens", "in_rate") + `,
       ` + costExpr("completion_tokens", "out_rate") + `,
       ` + totalCostExpr() + `
FROM calls`
	var (
		totals                     estimateTotals
		runs                       int64
		inCost, outCost, totalCost string
	)
	if err := tx.QueryRow(ctx, query, projectID, from, to).Scan(&totals.Calls, &runs,
		&totals.PromptTokens, &totals.CompletionTokens, &inCost, &outCost, &totalCost); err != nil {
		return estimateTotals{}, 0, fmt.Errorf("analytics: estimate evaluation totals: %w", err)
	}
	totals.TotalTokens = totals.PromptTokens + totals.CompletionTokens
	totals.InputCost, totals.OutputCost, totals.TotalCost = scanCosts(priced, inCost, outCost, totalCost)
	return totals, runs, nil
}

// evaluationByAgent resolves each run id to the agent its tenant eval_runs
// row names. A run whose row is gone (deleted with its dataset) or names no
// agent folds into the row with an empty application id, so the rows still
// add up to the total.
func evaluationByAgent(ctx context.Context, tx pgx.Tx, block *estimateEvaluation,
	pricesPresent, priced bool, projectID int64, from, to time.Time,
) error {
	schema := pgx.Identifier{"p_" + strconv.FormatInt(projectID, 10)}.Sanitize()
	runsPresent, err := relationsPresent(ctx, tx, schema+".eval_runs")
	if err != nil {
		return err
	}
	if !runsPresent {
		block.ByAgent = []estimateEvaluationAgent{}
		return nil
	}
	named, err := relationsPresent(ctx, tx, schema+".applications")
	if err != nil {
		return err
	}
	nameSelect, nameJoin, nameGroup := `''`, ``, ``
	if named {
		nameSelect = `coalesce(app.name, '')`
		nameJoin = `
LEFT JOIN ` + schema + `.applications AS app ON app.id = run.application_id`
		nameGroup = `, coalesce(app.name, '')`
	}

	query := `
WITH calls AS (
  SELECT l.execution_id, l.prompt_tokens, l.completion_tokens, ` + rateColumns(pricesPresent) +
		sourceClause(pricesPresent) + requestLogWindow + `
    AND ` + evaluationCallPredicate + `
), tagged AS (
  SELECT calls.*,
         CASE WHEN split_part(execution_id, ':', 2) ~ '^[1-9][0-9]{0,8}$'
              THEN split_part(execution_id, ':', 2)::integer END AS run_id
  FROM calls
)
SELECT coalesce(run.application_id::text, ''),
       ` + nameSelect + `,
       count(DISTINCT tagged.run_id)::bigint,
       count(*)::bigint,
       coalesce(sum(tagged.prompt_tokens), 0)::bigint,
       coalesce(sum(tagged.completion_tokens), 0)::bigint,
       bool_or(` + pricedPredicate + `),
       ` + costExpr("prompt_tokens", "in_rate") + `,
       ` + costExpr("completion_tokens", "out_rate") + `,
       ` + totalCostExpr() + `
FROM tagged
LEFT JOIN ` + schema + `.eval_runs AS run ON run.id = tagged.run_id` + nameJoin + `
GROUP BY coalesce(run.application_id::text, '')` + nameGroup + `
ORDER BY count(*) DESC, 1 ASC
LIMIT $4`
	rows, err := tx.Query(ctx, query, projectID, from, to, estimateEvaluationAgentRows+1)
	if err != nil {
		return fmt.Errorf("analytics: estimate evaluation by agent: %w", err)
	}
	defer rows.Close()

	out := make([]estimateEvaluationAgent, 0)
	for rows.Next() {
		var (
			row                        estimateEvaluationAgent
			rowPriced                  bool
			inCost, outCost, totalCost string
		)
		if err := rows.Scan(&row.ApplicationID, &row.Name, &row.Runs, &row.Calls,
			&row.PromptTokens, &row.CompletionTokens, &rowPriced,
			&inCost, &outCost, &totalCost); err != nil {
			return fmt.Errorf("analytics: estimate evaluation by agent scan: %w", err)
		}
		row.TotalTokens = row.PromptTokens + row.CompletionTokens
		row.Priced = rowPriced
		row.InputCost, row.OutputCost, row.TotalCost = scanCosts(priced && rowPriced, inCost, outCost, totalCost)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(out) > estimateEvaluationAgentRows {
		block.ByAgent, block.ByAgentTruncated = out[:estimateEvaluationAgentRows], true
		return nil
	}
	block.ByAgent = out
	return nil
}
