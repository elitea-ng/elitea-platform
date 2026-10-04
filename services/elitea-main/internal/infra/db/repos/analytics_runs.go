package repos

// The run-level analytics reads, and the trigger-origin filter the window
// reads use.
//
// # One execution, one evaluation run (legacy issues 6667, 6816, 6817)
//
// gateway.llm_request_logs.execution_id (shared 0100) names the work a call
// was made for. A runtime execution signs its own id; the evaluation
// orchestrator signs `eval:<run>:case:<case>` and `eval:<run>:judge:<case>`
// (internal/api/v2/evaluation/attribution.go). So one run's spend is a
// selection by id, with no window: a run is a fixed set of calls, and a date
// range that cut one in half would report half a run.
//
// "Including child executions" is a PREFIX. A nested agent in the Rust and
// Python workers runs inside its parent's claim and signs the parent's id, so
// its calls already carry that id. Any attribution a caller derives from an
// execution is named `<execution_id>:<suffix>`, and the read adds those calls
// in and reports how many such ids contributed (ChildAttributions). There is
// no second hop: a child of a child carries the same root prefix.
//
// # Unavailable is not zero
//
// A run that started before its calls were attributed, or whose calls the
// gateway has since pruned, has no figures. The read says so
// (Available=false, UnavailableReason) and omits every figure, for the rule
// the file header of analytics.go states: zero is a measurement.
//
// # Trigger origin (legacy issues 6802 and 6881)
//
// An unattended run executes as the person who configured it, so its calls
// carry that person's user id. elitea_runtime.execution_jobs.trigger_origin
// (shared 0140) says how the run started, and humanCallFilter drops the calls
// of `schedule`, `webhook` and `index` runs from every figure that claims a
// PERSON was active. Calls, tokens and money still count them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/analytics"
)

// The ledger NAMES of the migrations whose apply time bounds what a run read
// can speak for. Matched by name, not by number: numbers are assigned when a
// branch is merged, and a renumbering must not move the boundary.
const (
	requestLogExecutionIDMigration = "gateway_request_log_execution_id" // shared 0100
	triggerOriginMigration         = "execution_trigger_origin"         // shared 0140
)

// automatedOrigins are the trigger origins no person started directly.
// executiondomain.TriggerOrigin.Automated is the same list.
const automatedOrigins = `('schedule', 'webhook', 'index')`

// automatedCall is true for a request-log row (alias l) made from an execution
// an unattended trigger started.
//
// The project guard is the one agentAttribution applies: the id resolves only
// inside the project the LOG row names, so a forged id cannot borrow another
// project's origin. EXISTS rather than a join: execution_jobs is keyed
// (execution_id, generation), and a join on the id would multiply rows.
const automatedCall = `(l.execution_id IS NOT NULL AND EXISTS (
    SELECT 1 FROM elitea_runtime.execution_jobs AS origin_job
    WHERE origin_job.execution_id = l.execution_id
      AND origin_job.trigger_origin IN ` + automatedOrigins + `
      AND (origin_job.resource_project_id = l.project_id
           OR origin_job.projection_project_id = l.project_id)))`

// humanCallFilter is the predicate fragment that keeps only the calls a
// person made. It is empty when the database cannot tell (shared 0140 has not
// run), which keeps the behaviour from before the column existed.
func humanCallFilter(originsReadable bool) string {
	if !originsReadable {
		return ""
	}
	return "\n  AND NOT " + automatedCall
}

// triggerOriginReadable probes for the column shared 0140 adds. It is asked
// BEFORE any statement names the column: a missing column fails at parse
// time and would abort the caller's snapshot transaction.
func triggerOriginReadable(ctx context.Context, q analyticsQuerier) (bool, error) {
	return columnPresent(ctx, q, "elitea_runtime", "execution_jobs", "trigger_origin")
}

func columnPresent(ctx context.Context, q analyticsQuerier, schema, table, column string) (bool, error) {
	var present bool
	if err := q.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = $1 AND table_name = $2 AND column_name = $3
)`, schema, table, column).Scan(&present); err != nil {
		return false, fmt.Errorf("analytics: column probe %s.%s.%s: %w", schema, table, column, err)
	}
	return present, nil
}

// migrationAppliedAt is when the named shared migration ran on this database.
// Zero when the ledger has no such row.
func migrationAppliedAt(ctx context.Context, q analyticsQuerier, name string) (time.Time, error) {
	var appliedAt *time.Time
	if err := q.QueryRow(ctx, `
SELECT min(applied_at)
FROM elitea_runtime.schema_migrations
WHERE target_kind = 'shared' AND name = $1`, name).Scan(&appliedAt); err != nil {
		if missingRelation(err) {
			return time.Time{}, nil
		}
		return time.Time{}, fmt.Errorf("analytics: migration ledger probe: %w", err)
	}
	if appliedAt == nil {
		return time.Time{}, nil
	}
	return *appliedAt, nil
}

/* ── figures ─────────────────────────────────────────────────────────── */

// figuresColumns is the aggregate list figuresScan reads. It runs over the
// `calls` CTE figuresSource builds, so the rate columns exist either way.
const figuresColumns = `count(*)::bigint,
       coalesce(sum(prompt_tokens), 0)::bigint,
       coalesce(sum(completion_tokens), 0)::bigint,
       count(*) FILTER (WHERE ` + errorPredicate + `)::bigint,
       coalesce(avg(duration_ms), 0)::double precision,
       count(*) FILTER (WHERE in_rate IS NOT NULL OR out_rate IS NOT NULL)::bigint,
       round(coalesce(sum(prompt_tokens::numeric * in_rate) / 1000000, 0), ` + agentCostScale + `)::text,
       round(coalesce(sum(completion_tokens::numeric * out_rate) / 1000000, 0), ` + agentCostScale + `)::text,
       round(coalesce(sum(prompt_tokens::numeric * in_rate) / 1000000, 0)
             + coalesce(sum(completion_tokens::numeric * out_rate) / 1000000, 0), ` + agentCostScale + `)::text`

// figuresSource is a `calls` CTE over the request-log rows of project $1 that
// match scope, priced at the catalogue rate when the catalogue exists.
//
// The rate is joined PER ROW, before any aggregate, because one run can call
// several models — the rule estimate.go and agentUsage follow.
func figuresSource(pricesPresent bool, scope string) string {
	if pricesPresent {
		return `
WITH calls AS (
    SELECT l.*, m.input_cost_per_1m_tokens AS in_rate, m.output_cost_per_1m_tokens AS out_rate
    FROM gateway.llm_request_logs AS l
    LEFT JOIN gateway.gateway_models AS m
      ON m.provider = l.provider AND m.model_name = l.model
    WHERE l.project_id = $1 AND ` + scope + `
)`
	}
	return `
WITH calls AS (
    SELECT l.*, NULL::numeric AS in_rate, NULL::numeric AS out_rate
    FROM gateway.llm_request_logs AS l
    WHERE l.project_id = $1 AND ` + scope + `
)`
}

// figuresTargets returns the scan targets for figuresColumns and a function
// that folds them into the figures once the row is scanned.
func figuresTargets(figures *analytics.UsageFigures) ([]any, func()) {
	var pricedCalls int64
	var inCost, outCost, totalCost string
	targets := []any{
		&figures.LLMCalls, &figures.PromptTokens, &figures.CompletionTokens,
		&figures.Errors, &figures.AvgDurationMS, &pricedCalls,
		&inCost, &outCost, &totalCost,
	}
	return targets, func() {
		figures.TotalTokens = figures.PromptTokens + figures.CompletionTokens
		figures.AvgDurationMS = math.Round(figures.AvgDurationMS*10) / 10
		figures.UnpricedCalls = figures.LLMCalls - pricedCalls
		if pricedCalls > 0 {
			in, out, total := json.Number(inCost), json.Number(outCost), json.Number(totalCost)
			figures.InputCost, figures.OutputCost, figures.TotalCost = &in, &out, &total
		}
	}
}

// runRowsLimit caps a run's per-model and per-user lists. A run is one
// person's work, so the cap is a backstop.
const runRowsLimit = 100

func scopeTotals(ctx context.Context, q analyticsQuerier, prices bool, scope string, args ...any) (analytics.UsageFigures, error) {
	var figures analytics.UsageFigures
	targets, fold := figuresTargets(&figures)
	if err := q.QueryRow(ctx, figuresSource(prices, scope)+`
SELECT `+figuresColumns+` FROM calls`, args...).Scan(targets...); err != nil {
		return analytics.UsageFigures{}, fmt.Errorf("analytics: run totals: %w", err)
	}
	fold()
	return figures, nil
}

func scopeByModel(ctx context.Context, q analyticsQuerier, prices bool, scope string, args ...any) ([]analytics.ModelFigures, error) {
	rows, err := q.Query(ctx, figuresSource(prices, scope)+`
SELECT provider, model, `+figuresColumns+`
FROM calls
WHERE model <> ''
GROUP BY provider, model
ORDER BY count(*) DESC, model ASC, provider ASC
LIMIT `+strconv.Itoa(runRowsLimit), args...)
	if err != nil {
		return nil, fmt.Errorf("analytics: run models: %w", err)
	}
	defer rows.Close()
	out := make([]analytics.ModelFigures, 0)
	for rows.Next() {
		var row analytics.ModelFigures
		targets, fold := figuresTargets(&row.UsageFigures)
		if err := rows.Scan(append([]any{&row.Provider, &row.Model}, targets...)...); err != nil {
			return nil, fmt.Errorf("analytics: run models scan: %w", err)
		}
		fold()
		out = append(out, row)
	}
	return out, rows.Err()
}

func scopeByUser(ctx context.Context, q analyticsQuerier, prices bool, scope string, args ...any) ([]analytics.UserFigures, error) {
	rows, err := q.Query(ctx, figuresSource(prices, scope)+`
SELECT user_id, `+figuresColumns+`
FROM calls
WHERE user_id IS NOT NULL
GROUP BY user_id
ORDER BY count(*) DESC, user_id ASC
LIMIT `+strconv.Itoa(runRowsLimit), args...)
	if err != nil {
		return nil, fmt.Errorf("analytics: run users: %w", err)
	}
	out := make([]analytics.UserFigures, 0)
	ids := make([]int64, 0)
	for rows.Next() {
		var (
			row    analytics.UserFigures
			userID int64
		)
		targets, fold := figuresTargets(&row.UsageFigures)
		if err := rows.Scan(append([]any{&userID}, targets...)...); err != nil {
			rows.Close()
			return nil, fmt.Errorf("analytics: run users scan: %w", err)
		}
		fold()
		row.UserID = strconv.FormatInt(userID, 10)
		out = append(out, row)
		ids = append(ids, userID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	identities, err := userIdentities(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if identity, ok := identities[out[i].UserID]; ok {
			out[i].Email, out[i].Name = identity.email, identity.name
		}
	}
	return out, nil
}

func scopeByErrorCode(ctx context.Context, q analyticsQuerier, scope string, args ...any) ([]analytics.ErrorCodeCount, error) {
	rows, err := q.Query(ctx, figuresSource(false, scope)+`
SELECT coalesce(nullif(error_code, ''), 'http_' || status::text), count(*)::bigint
FROM calls
WHERE `+errorPredicate+`
GROUP BY 1
ORDER BY count(*) DESC, 1 ASC
LIMIT `+strconv.Itoa(errorCodeRowsLimit), args...)
	if err != nil {
		return nil, fmt.Errorf("analytics: run errors: %w", err)
	}
	defer rows.Close()
	out := make([]analytics.ErrorCodeCount, 0)
	for rows.Next() {
		var row analytics.ErrorCodeCount
		if err := rows.Scan(&row.ErrorCode, &row.Requests); err != nil {
			return nil, fmt.Errorf("analytics: run errors scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// likePrefix escapes a prefix for `LIKE … ESCAPE '\'`. An execution id may
// hold `_`, which LIKE reads as "any character".
func likePrefix(prefix string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(prefix) + "%"
}

/* ── one execution ───────────────────────────────────────────────────── */

// executionScope selects the execution's own calls and its child
// attributions. $2 is the id, $3 its escaped `<id>:` prefix.
const executionScope = `(l.execution_id = $2 OR l.execution_id LIKE $3 ESCAPE '\')` + inferenceRouteOnLog

// GetExecutionAnalytics is one runtime execution's totals.
func (r *AnalyticsRepo) GetExecutionAnalytics(ctx context.Context, projectIDRaw, executionID string) (analytics.ExecutionAnalytics, error) {
	id, err := projectID(analytics.QueryParams{ProjectID: projectIDRaw})
	if err != nil {
		return analytics.ExecutionAnalytics{}, err
	}
	if !validAttributionID(executionID) {
		return analytics.ExecutionAnalytics{}, analytics.BadIDError("execution", executionID)
	}

	ctx, cancel := context.WithTimeout(ctx, analyticsReadTimeout)
	defer cancel()
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return analytics.ExecutionAnalytics{}, fmt.Errorf("analytics: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	present, err := checkRelations(ctx, tx, "gateway.llm_request_logs", "elitea_runtime.execution_jobs")
	if err != nil {
		return analytics.ExecutionAnalytics{}, err
	}
	if !present {
		return analytics.ExecutionAnalytics{}, analytics.NoSourceError("execution analytics",
			"gateway.llm_request_logs or elitea_runtime.execution_jobs is absent on this database")
	}
	tagged, err := columnPresent(ctx, tx, "gateway", "llm_request_logs", "execution_id")
	if err != nil {
		return analytics.ExecutionAnalytics{}, err
	}
	if !tagged {
		return analytics.ExecutionAnalytics{}, analytics.NoSourceError("execution analytics",
			"gateway.llm_request_logs has no execution_id column — shared migration 0100 has not run on this database")
	}
	origins, err := triggerOriginReadable(ctx, tx)
	if err != nil {
		return analytics.ExecutionAnalytics{}, err
	}

	result, err := executionHeader(ctx, tx, id, executionID, origins)
	if err != nil {
		return analytics.ExecutionAnalytics{}, err
	}

	boundary, err := migrationAppliedAt(ctx, tx, requestLogExecutionIDMigration)
	if err != nil {
		return analytics.ExecutionAnalytics{}, err
	}
	if !boundary.IsZero() && result.AdmittedAt.Before(boundary) {
		result.UnavailableReason = analytics.UnavailableBeforeAttribution
		return result, nil
	}

	prices, err := checkRelations(ctx, tx, "gateway.gateway_models")
	if err != nil {
		return analytics.ExecutionAnalytics{}, err
	}
	args := []any{id, executionID, likePrefix(executionID + ":")}
	totals, err := scopeTotals(ctx, tx, prices, executionScope, args...)
	if err != nil {
		return analytics.ExecutionAnalytics{}, err
	}
	if totals.LLMCalls == 0 {
		pruned, err := logPrunedBefore(ctx, tx, id, result.AdmittedAt)
		if err != nil {
			return analytics.ExecutionAnalytics{}, err
		}
		if pruned {
			result.UnavailableReason = analytics.UnavailableLogPruned
			return result, nil
		}
	}
	result.Available = true
	result.Totals = &totals

	if err := tx.QueryRow(ctx, `
SELECT count(DISTINCT l.execution_id) FILTER (WHERE l.execution_id <> $2)::bigint
FROM gateway.llm_request_logs AS l
WHERE l.project_id = $1 AND `+executionScope, args...).Scan(&result.ChildAttributions); err != nil {
		return analytics.ExecutionAnalytics{}, fmt.Errorf("analytics: child attributions: %w", err)
	}
	if result.ByModel, err = scopeByModel(ctx, tx, prices, executionScope, args...); err != nil {
		return analytics.ExecutionAnalytics{}, err
	}
	if result.ByUser, err = scopeByUser(ctx, tx, prices, executionScope, args...); err != nil {
		return analytics.ExecutionAnalytics{}, err
	}
	if result.ByErrorCode, err = scopeByErrorCode(ctx, tx, executionScope, args...); err != nil {
		return analytics.ExecutionAnalytics{}, err
	}
	if err := executionTools(ctx, tx, id, executionID, &result); err != nil {
		return analytics.ExecutionAnalytics{}, err
	}
	return result, nil
}

// executionHeader reads the execution's row, guarded by the project the way
// agentAttribution guards it: an id from another project is not found.
//
// The LATEST generation gives the state; the EARLIEST admission gives the
// start, because a retried run started when it was first admitted.
func executionHeader(ctx context.Context, q analyticsQuerier, id int64, executionID string, origins bool) (analytics.ExecutionAnalytics, error) {
	origin := `'manual'`
	if origins {
		origin = `j.trigger_origin`
	}
	result := analytics.ExecutionAnalytics{ExecutionID: executionID}
	err := q.QueryRow(ctx, `
SELECT j.capability_id, `+origin+`, j.actor_id, j.state, j.settled_at,
       (SELECT min(first.admitted_at)
          FROM elitea_runtime.execution_jobs AS first
         WHERE first.execution_id = j.execution_id)
FROM elitea_runtime.execution_jobs AS j
WHERE j.execution_id = $2
  AND (j.resource_project_id = $1 OR j.projection_project_id = $1)
ORDER BY j.generation DESC
LIMIT 1`, id, executionID).Scan(&result.CapabilityID, &result.TriggerOrigin, &result.ActorUserID,
		&result.State, &result.SettledAt, &result.AdmittedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return analytics.ExecutionAnalytics{}, fmt.Errorf("%w: execution %q", analytics.ErrNotFound, executionID)
		}
		return analytics.ExecutionAnalytics{}, fmt.Errorf("analytics: execution lookup: %w", err)
	}
	return result, nil
}

// logPrunedBefore reports whether the request log no longer reaches back to
// `since` for this project: the oldest row it holds is younger.
//
// It is asked only for a run with NO calls in the log, to tell "this run made
// no model call" from "the gateway pruned its calls". The one case it reads
// wrongly is a run that made no call before the project's first ever logged
// call, and that run's figures are zero either way.
func logPrunedBefore(ctx context.Context, q analyticsQuerier, id int64, since time.Time) (bool, error) {
	var oldest *time.Time
	if err := q.QueryRow(ctx,
		`SELECT min(occurred_at) FROM gateway.llm_request_logs WHERE project_id = $1`, id,
	).Scan(&oldest); err != nil {
		return false, fmt.Errorf("analytics: log horizon: %w", err)
	}
	return oldest != nil && oldest.After(since), nil
}

// executionTools is the run's tool calls from elitea_runtime.tool_call_records
// (shared 0119), which carries the execution id for an explicit tool run and,
// from issue 875 on, for an agent turn's tool calls.
func executionTools(ctx context.Context, q analyticsQuerier, id int64, executionID string, result *analytics.ExecutionAnalytics) error {
	present, err := checkRelations(ctx, q, "elitea_runtime.tool_call_records")
	if err != nil || !present {
		return err
	}
	result.ToolsAvailable = true
	rows, err := q.Query(ctx, `
SELECT coalesce(r.toolkit_id::text, ''),
       coalesce(r.toolkit_name, ''),
       r.tool_name,
       count(*)::bigint,
       count(*) FILTER (WHERE r.is_error)::bigint,
       coalesce(avg(EXTRACT(EPOCH FROM (r.finished_at - r.started_at)) * 1000)
                FILTER (WHERE r.finished_at IS NOT NULL), 0)::double precision
FROM elitea_runtime.tool_call_records AS r
WHERE r.project_id = $1
  AND (r.execution_id = $2 OR r.execution_id LIKE $3 ESCAPE '\')
GROUP BY r.toolkit_id, r.toolkit_name, r.tool_name
ORDER BY count(*) DESC, r.tool_name ASC
LIMIT $4`, id, executionID, likePrefix(executionID+":"), toolRowsLimit)
	if err != nil {
		return fmt.Errorf("analytics: execution tools: %w", err)
	}
	defer rows.Close()
	result.Tools = make([]analytics.ToolAnalytics, 0)
	for rows.Next() {
		var tool analytics.ToolAnalytics
		if err := rows.Scan(&tool.ToolkitID, &tool.ToolkitName, &tool.ToolName,
			&tool.RunCount, &tool.ErrorCount, &tool.AvgDuration); err != nil {
			return fmt.Errorf("analytics: execution tools scan: %w", err)
		}
		tool.AvgDuration = math.Round(tool.AvgDuration*10) / 10
		if tool.RunCount > 0 {
			tool.ErrorRate = math.Round(float64(tool.ErrorCount)/float64(tool.RunCount)*1000) / 10
		}
		result.Tools = append(result.Tools, tool)
	}
	return rows.Err()
}

// validAttributionID is the edge's execution id rule
// (internal/llmproxy/identity.go executionIDFromHeader): nothing outside it
// can be in the column, so it is refused as a bad request.
func validAttributionID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

/* ── one evaluation run ─────────────────────────────────────────────── */

// evaluationScope selects every call of one evaluation run. $2 is the
// escaped `eval:<run>:` prefix. The role segment follows it: `case` for the
// agent turn and `judge` for a judge call.
const evaluationScope = `l.execution_id LIKE $2 ESCAPE '\'` + inferenceRouteOnLog

// GetEvaluationRunAnalytics is one evaluation run's spend.
func (r *AnalyticsRepo) GetEvaluationRunAnalytics(ctx context.Context, projectIDRaw, runID string) (analytics.EvaluationRunAnalytics, error) {
	id, err := projectID(analytics.QueryParams{ProjectID: projectIDRaw})
	if err != nil {
		return analytics.EvaluationRunAnalytics{}, err
	}
	run, err := strconv.ParseInt(runID, 10, 32)
	if err != nil || run < 1 {
		return analytics.EvaluationRunAnalytics{}, analytics.BadIDError("evaluation run", runID)
	}

	ctx, cancel := context.WithTimeout(ctx, analyticsReadTimeout)
	defer cancel()
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return analytics.EvaluationRunAnalytics{}, fmt.Errorf("analytics: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	schema := pgx.Identifier{"p_" + strconv.FormatInt(id, 10)}.Sanitize()
	present, err := checkRelations(ctx, tx, "gateway.llm_request_logs", schema+".eval_runs")
	if err != nil {
		return analytics.EvaluationRunAnalytics{}, err
	}
	if !present {
		return analytics.EvaluationRunAnalytics{}, analytics.NoSourceError("evaluation run analytics",
			"gateway.llm_request_logs or the project's eval_runs table is absent on this database")
	}
	tagged, err := columnPresent(ctx, tx, "gateway", "llm_request_logs", "execution_id")
	if err != nil {
		return analytics.EvaluationRunAnalytics{}, err
	}
	if !tagged {
		return analytics.EvaluationRunAnalytics{}, analytics.NoSourceError("evaluation run analytics",
			"gateway.llm_request_logs has no execution_id column — shared migration 0100 has not run on this database")
	}

	result := analytics.EvaluationRunAnalytics{RunID: strconv.FormatInt(run, 10)}
	if err := tx.QueryRow(ctx, `
SELECT application_id, application_version_id, status, created_by, created_at, finished_at
FROM `+schema+`.eval_runs
WHERE id = $1`, run).Scan(&result.ApplicationID, &result.ApplicationVersionID, &result.Status,
		&result.CreatedBy, &result.CreatedAt, &result.FinishedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return analytics.EvaluationRunAnalytics{}, fmt.Errorf("%w: evaluation run %s", analytics.ErrNotFound, runID)
		}
		return analytics.EvaluationRunAnalytics{}, fmt.Errorf("analytics: evaluation run lookup: %w", err)
	}

	// The orchestrator began signing its calls in the release that ran shared
	// 0140. A run created before that has no attributed call at all.
	boundary, err := migrationAppliedAt(ctx, tx, triggerOriginMigration)
	if err != nil {
		return analytics.EvaluationRunAnalytics{}, err
	}
	if boundary.IsZero() || result.CreatedAt.Before(boundary) {
		result.UnavailableReason = analytics.UnavailableBeforeAttribution
		return result, nil
	}

	prices, err := checkRelations(ctx, tx, "gateway.gateway_models")
	if err != nil {
		return analytics.EvaluationRunAnalytics{}, err
	}
	prefix := "eval:" + result.RunID + ":"
	args := []any{id, likePrefix(prefix)}
	totals, err := scopeTotals(ctx, tx, prices, evaluationScope, args...)
	if err != nil {
		return analytics.EvaluationRunAnalytics{}, err
	}
	if totals.LLMCalls == 0 {
		pruned, err := logPrunedBefore(ctx, tx, id, result.CreatedAt)
		if err != nil {
			return analytics.EvaluationRunAnalytics{}, err
		}
		if pruned {
			result.UnavailableReason = analytics.UnavailableLogPruned
			return result, nil
		}
	}
	result.Available = true
	result.Totals = &totals

	agent, err := scopeTotals(ctx, tx, prices, `l.execution_id LIKE $2 ESCAPE '\'`,
		id, likePrefix(prefix+"case:"))
	if err != nil {
		return analytics.EvaluationRunAnalytics{}, err
	}
	judge, err := scopeTotals(ctx, tx, prices, `l.execution_id LIKE $2 ESCAPE '\'`,
		id, likePrefix(prefix+"judge:"))
	if err != nil {
		return analytics.EvaluationRunAnalytics{}, err
	}
	result.Agent, result.Judge = &agent, &judge

	if result.ByCase, err = evaluationByCase(ctx, tx, prices, args...); err != nil {
		return analytics.EvaluationRunAnalytics{}, err
	}
	if result.ByModel, err = scopeByModel(ctx, tx, prices, evaluationScope, args...); err != nil {
		return analytics.EvaluationRunAnalytics{}, err
	}
	if result.ByErrorCode, err = scopeByErrorCode(ctx, tx, evaluationScope, args...); err != nil {
		return analytics.EvaluationRunAnalytics{}, err
	}
	return result, nil
}

// evaluationByCase folds the run's calls into one row per case, with the
// agent and judge roles side by side. The case id is the fourth `:` segment
// of `eval:<run>:<role>:<case>`.
func evaluationByCase(ctx context.Context, q analyticsQuerier, prices bool, args ...any) ([]analytics.EvaluationCaseFigures, error) {
	rows, err := q.Query(ctx, figuresSource(prices, evaluationScope)+`
SELECT split_part(execution_id, ':', 4) AS case_id,
       split_part(execution_id, ':', 3) AS role,
       `+figuresColumns+`
FROM calls
WHERE split_part(execution_id, ':', 3) IN ('case', 'judge')
GROUP BY 1, 2
ORDER BY 1, 2`, args...)
	if err != nil {
		return nil, fmt.Errorf("analytics: evaluation cases: %w", err)
	}
	defer rows.Close()

	byCase := map[string]*analytics.EvaluationCaseFigures{}
	order := make([]string, 0)
	for rows.Next() {
		var (
			caseID, role string
			figures      analytics.UsageFigures
		)
		targets, fold := figuresTargets(&figures)
		if err := rows.Scan(append([]any{&caseID, &role}, targets...)...); err != nil {
			return nil, fmt.Errorf("analytics: evaluation cases scan: %w", err)
		}
		fold()
		entry, ok := byCase[caseID]
		if !ok {
			entry = &analytics.EvaluationCaseFigures{CaseID: caseID}
			byCase[caseID] = entry
			order = append(order, caseID)
		}
		if role == "judge" {
			entry.Judge = figures
		} else {
			entry.Agent = figures
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]analytics.EvaluationCaseFigures, 0, len(order))
	for _, caseID := range order {
		out = append(out, *byCase[caseID])
	}
	return out, nil
}

/* ── automated activity in a window ─────────────────────────────────── */

// automatedActivity is the window's unattended calls, one row per trigger
// origin: the separate bucket legacy issue 6802 asks for. The caller asks it
// only when trigger_origin is readable.
func automatedActivity(ctx context.Context, q analyticsQuerier, id int64, params analytics.QueryParams) ([]analytics.AutomatedActivity, error) {
	prices, err := checkRelations(ctx, q, "gateway.gateway_models")
	if err != nil {
		return nil, err
	}
	rateSelect, rateJoin := `NULL::numeric AS in_rate, NULL::numeric AS out_rate`, ``
	if prices {
		rateSelect = `m.input_cost_per_1m_tokens AS in_rate, m.output_cost_per_1m_tokens AS out_rate`
		rateJoin = `
    LEFT JOIN gateway.gateway_models AS m
      ON m.provider = l.provider AND m.model_name = l.model`
	}
	rows, err := q.Query(ctx, `
WITH origin AS (
    SELECT DISTINCT ON (j.execution_id) j.execution_id, j.trigger_origin
    FROM elitea_runtime.execution_jobs AS j
    WHERE j.trigger_origin IN `+automatedOrigins+`
      AND (j.resource_project_id = $1 OR j.projection_project_id = $1)
    ORDER BY j.execution_id, j.generation
), calls AS (
    SELECT l.*, origin.trigger_origin, `+rateSelect+`
    FROM gateway.llm_request_logs AS l
    JOIN origin ON origin.execution_id = l.execution_id`+rateJoin+`
    WHERE l.project_id = $1
      AND l.occurred_at >= $2
      AND l.occurred_at < $3`+inferenceRouteOnLog+`
)
SELECT trigger_origin,
       count(DISTINCT execution_id)::bigint,
       count(DISTINCT user_id)::bigint,
       `+figuresColumns+`
FROM calls
GROUP BY trigger_origin
ORDER BY trigger_origin`, id, params.From, params.To)
	if err != nil {
		return nil, fmt.Errorf("analytics: automated activity: %w", err)
	}
	defer rows.Close()
	out := make([]analytics.AutomatedActivity, 0)
	for rows.Next() {
		var row analytics.AutomatedActivity
		targets, fold := figuresTargets(&row.UsageFigures)
		if err := rows.Scan(append([]any{&row.TriggerOrigin, &row.Executions, &row.Users}, targets...)...); err != nil {
			return nil, fmt.Errorf("analytics: automated activity scan: %w", err)
		}
		fold()
		out = append(out, row)
	}
	return out, rows.Err()
}
