package repos

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/analytics"
)

// caseEvaluationScope selects the calls of ONE case of a run: the agent turn
// ($2) and the judge calls ($5). It is an exact match, not a prefix: the prefix
// `eval:1:case:3` would also match case 31.
const caseEvaluationScope = `l.execution_id IN ($2, $5)` + lifetimeOnLog + inferenceRouteOnLog

// EvaluationUsage is the spend attributed to an evaluation run (caseID empty)
// or to one case of it, read from the gateway request log the way the run
// analytics read it (GetEvaluationRunAnalytics). The orchestrator calls it
// between calls to enforce a run's limits.
//
// The window is the run's own lifetime, open-ended while it runs. Tokens are
// prompt plus completion; cost is the catalogue estimate, so a call the
// catalogue does not price adds tokens and no cost.
func (r *AnalyticsRepo) EvaluationUsage(ctx context.Context, projectIDRaw, runID, caseID string) (evaluation.Usage, error) {
	id, err := projectID(analytics.QueryParams{ProjectID: projectIDRaw})
	if err != nil {
		return evaluation.Usage{}, err
	}
	run, err := strconv.ParseInt(runID, 10, 32)
	if err != nil || run < 1 {
		return evaluation.Usage{}, fmt.Errorf("evaluation usage: bad run id %q", runID)
	}

	ctx, cancel := context.WithTimeout(ctx, analyticsReadTimeout)
	defer cancel()
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return evaluation.Usage{}, fmt.Errorf("evaluation usage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	schema := pgx.Identifier{"p_" + strconv.FormatInt(id, 10)}.Sanitize()
	present, err := checkRelations(ctx, tx, "gateway.llm_request_logs", schema+".eval_runs")
	if err != nil {
		return evaluation.Usage{}, err
	}
	if !present {
		return evaluation.Usage{}, fmt.Errorf("evaluation usage: gateway.llm_request_logs or the project's eval_runs is absent")
	}
	var createdAt time.Time
	var finishedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT created_at, finished_at FROM `+schema+`.eval_runs WHERE id = $1`, run).
		Scan(&createdAt, &finishedAt); err != nil {
		return evaluation.Usage{}, fmt.Errorf("evaluation usage: run lookup: %w", err)
	}
	prices, err := checkRelations(ctx, tx, "gateway.gateway_models")
	if err != nil {
		return evaluation.Usage{}, err
	}
	sources := figuresSources{prices: prices}

	var figures analytics.UsageFigures
	if caseID == "" {
		prefix := "eval:" + strconv.FormatInt(run, 10) + ":"
		figures, err = scopeTotals(ctx, tx, sources, evaluationScope, id, likePrefix(prefix), createdAt, finishedAt)
	} else {
		base := "eval:" + strconv.FormatInt(run, 10)
		figures, err = scopeTotals(ctx, tx, sources, caseEvaluationScope, id,
			base+":"+evaluation.AttributionRoleAgent+":"+caseID, createdAt, finishedAt,
			base+":"+evaluation.AttributionRoleJudge+":"+caseID)
	}
	if err != nil {
		return evaluation.Usage{}, err
	}
	usage := evaluation.Usage{Tokens: figures.PromptTokens + figures.CompletionTokens}
	if figures.TotalCost != nil {
		if cost, parseErr := figures.TotalCost.Float64(); parseErr == nil {
			usage.CostUSD = cost
		}
	}
	return usage, nil
}
