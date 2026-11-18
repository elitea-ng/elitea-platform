package repos

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/analytics"
)

// caseEvaluationScope selects the calls of ONE case of a run: the agent turn
// ($2) and the judge calls ($5). It is an exact match, not a prefix: the prefix
// `eval:1:case:3` would also match case 31.
const caseEvaluationScope = `l.execution_id IN ($2, $5)` + lifetimeOnLog + inferenceRouteOnLog

// caseEvaluationScopeWithCaseRuns adds the executions the case ran as when it
// was an agent run (tenant/0246), and the executions linked to its root run:
// the worker signs those calls with the execution id.
func caseEvaluationScopeWithCaseRuns(schema string, run int64, caseID int, links bool) string {
	return `(l.execution_id IN ($2, $5) OR l.execution_id IN (SELECT m.execution_id FROM (` +
		analytics.EvalCaseExecutionsSQL(schema, run, links) + `) AS m WHERE m.dataset_case_id = ` +
		strconv.Itoa(caseID) + `))` + lifetimeOnLog + inferenceRouteOnLog
}

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

	meta, err := r.evalUsage.get(ctx, tx, id, run)
	if err != nil {
		return evaluation.Usage{}, err
	}
	createdAt, finishedAt := meta.createdAt, (*time.Time)(nil)
	sources := figuresSources{prices: meta.prices}

	schema := pgx.Identifier{"p_" + strconv.FormatInt(id, 10)}.Sanitize()
	runScope, caseRunSources, err := evaluationScopeFor(ctx, tx, schema, run)
	if err != nil {
		return evaluation.Usage{}, err
	}
	var figures analytics.UsageFigures
	if caseID == "" {
		prefix := "eval:" + strconv.FormatInt(run, 10) + ":"
		figures, err = scopeTotals(ctx, tx, sources, runScope, id, likePrefix(prefix), createdAt, finishedAt)
	} else {
		caseScope := caseEvaluationScope
		if numeric, parseErr := strconv.Atoi(caseID); caseRunSources.caseRuns && parseErr == nil && numeric > 0 {
			caseScope = caseEvaluationScopeWithCaseRuns(schema, run, numeric, caseRunSources.links)
		}
		base := "eval:" + strconv.FormatInt(run, 10)
		figures, err = scopeTotals(ctx, tx, sources, caseScope, id,
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

// evalUsageMeta is what one run's usage reads need besides the log itself.
type evalUsageMeta struct {
	createdAt time.Time
	prices    bool
}

// evalUsageMetaCache keeps evalUsageMeta per run, so the relation checks and
// the run lookup run once per run and not once per read: a limit check reads
// usage up to once per case and per judge call. Both facts are fixed for the
// life of a run (the window stays open-ended while the run executes). The map
// is cleared when it grows past evalUsageMetaCap, which only costs a lookup.
type evalUsageMetaCache struct {
	mu      sync.Mutex
	entries map[string]evalUsageMeta
}

const evalUsageMetaCap = 1024

func (c *evalUsageMetaCache) get(ctx context.Context, q analyticsQuerier, project, run int64) (evalUsageMeta, error) {
	key := strconv.FormatInt(project, 10) + "/" + strconv.FormatInt(run, 10)
	c.mu.Lock()
	meta, ok := c.entries[key]
	c.mu.Unlock()
	if ok {
		return meta, nil
	}
	schema := pgx.Identifier{"p_" + strconv.FormatInt(project, 10)}.Sanitize()
	present, err := checkRelations(ctx, q, "gateway.llm_request_logs", schema+".eval_runs")
	if err != nil {
		return evalUsageMeta{}, err
	}
	if !present {
		return evalUsageMeta{}, fmt.Errorf("evaluation usage: gateway.llm_request_logs or the project's eval_runs is absent")
	}
	if err := q.QueryRow(ctx, `SELECT created_at FROM `+schema+`.eval_runs WHERE id = $1`, run).Scan(&meta.createdAt); err != nil {
		return evalUsageMeta{}, fmt.Errorf("evaluation usage: run lookup: %w", err)
	}
	if meta.prices, err = checkRelations(ctx, q, "gateway.gateway_models"); err != nil {
		return evalUsageMeta{}, err
	}
	c.mu.Lock()
	if c.entries == nil || len(c.entries) >= evalUsageMetaCap {
		c.entries = map[string]evalUsageMeta{}
	}
	c.entries[key] = meta
	c.mu.Unlock()
	return meta, nil
}
