package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// EvalRunsRepo stores Agent Evaluation runs and their results in the project's
// own tenant schema (tenant/0132_eval_datasets_runs.sql).
type EvalRunsRepo struct {
	pool *pgxpool.Pool
}

func NewEvalRunsRepo(pool *pgxpool.Pool) *EvalRunsRepo {
	return &EvalRunsRepo{pool: pool}
}

const runColumns = `
	id::text,
	COALESCE(uuid::text, ''),
	dataset_id::text,
	application_id,
	application_version_id,
	trigger_type,
	created_by,
	status,
	execution_mode,
	snapshot,
	progress_done,
	progress_total,
	headline_score,
	COALESCE(error, ''),
	created_at,
	started_at,
	finished_at`

func scanRun(row pgx.Row) (evaluation.Run, error) {
	var run evaluation.Run
	var snapshot []byte
	var createdAt time.Time
	var startedAt, finishedAt *time.Time
	err := row.Scan(
		&run.ID,
		&run.UUID,
		&run.DatasetID,
		&run.ApplicationID,
		&run.ApplicationVersionID,
		&run.TriggerType,
		&run.CreatedBy,
		&run.Status,
		&run.ExecutionMode,
		&snapshot,
		&run.Progress.Done,
		&run.Progress.Total,
		&run.HeadlineScore,
		&run.Error,
		&createdAt,
		&startedAt,
		&finishedAt,
	)
	if err != nil {
		return evaluation.Run{}, err
	}
	// The snapshot's collections are never nil on the wire. A client that
	// receives `"bindings": null` where it expects an array renders nothing and
	// reports no error — the invisible-data shape #132 catalogues.
	run.Snapshot = evaluation.RunSnapshot{
		Cases:      []evaluation.SnapshotCase{},
		Dimensions: map[string]evaluation.SnapshotDimension{},
		Bindings:   []evaluation.SnapshotBinding{},
	}
	if len(snapshot) > 0 {
		if err := json.Unmarshal(snapshot, &run.Snapshot); err != nil {
			return evaluation.Run{}, fmt.Errorf("eval runs: snapshot is not readable: %w", err)
		}
		if run.Snapshot.Cases == nil {
			run.Snapshot.Cases = []evaluation.SnapshotCase{}
		}
		if run.Snapshot.Dimensions == nil {
			run.Snapshot.Dimensions = map[string]evaluation.SnapshotDimension{}
		}
		if run.Snapshot.Bindings == nil {
			run.Snapshot.Bindings = []evaluation.SnapshotBinding{}
		}
	}
	run.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	if startedAt != nil {
		run.StartedAt = startedAt.UTC().Format(time.RFC3339)
	}
	if finishedAt != nil {
		run.FinishedAt = finishedAt.UTC().Format(time.RFC3339)
	}
	return run, nil
}

func (r *EvalRunsRepo) ListRuns(
	ctx context.Context,
	projectID string,
	filter evaluation.RunListFilter,
) ([]evaluation.Run, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = evaluation.DefaultRunListLimit
	}
	var datasetID *int
	if filter.DatasetID != nil {
		parsed, parseErr := parseEvalID(*filter.DatasetID, "dataset")
		if parseErr != nil {
			return nil, parseErr
		}
		datasetID = &parsed
	}

	// Newest first. The reference's run-history panel computes a delta against
	// the nearest OLDER scored run, so the order is not cosmetic: it is the
	// order that makes "the previous run" mean the same thing to the client and
	// to a person reading the list.
	rows, err := r.pool.Query(ctx, fmt.Sprintf(`
		SELECT `+runColumns+`
		FROM %s.eval_runs
		WHERE ($1::int IS NULL OR application_id = $1::int)
		  AND ($2::int IS NULL OR dataset_id = $2::int)
		ORDER BY id DESC
		LIMIT $3`, schema), filter.ApplicationID, datasetID, limit)
	if err != nil {
		return nil, fmt.Errorf("eval runs: list: %w", err)
	}
	defer rows.Close()

	runs := []evaluation.Run{}
	for rows.Next() {
		run, scanErr := scanRun(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("eval runs: list scan: %w", scanErr)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eval runs: list: %w", err)
	}
	return runs, nil
}

func (r *EvalRunsRepo) GetRun(ctx context.Context, projectID, runID string) (evaluation.Run, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return evaluation.Run{}, err
	}
	id, err := parseEvalID(runID, "run")
	if err != nil {
		return evaluation.Run{}, err
	}

	run, err := scanRun(r.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT `+runColumns+` FROM %s.eval_runs WHERE id = $1`, schema), id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return evaluation.Run{}, apierr.NotFound("run not found")
		}
		return evaluation.Run{}, fmt.Errorf("eval runs: get: %w", err)
	}
	return run, nil
}

func (r *EvalRunsRepo) CreateRun(
	ctx context.Context,
	projectID string,
	run evaluation.Run,
) (evaluation.Run, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return evaluation.Run{}, err
	}
	datasetID, err := parseEvalID(run.DatasetID, "dataset")
	if err != nil {
		return evaluation.Run{}, err
	}
	snapshot, err := json.Marshal(run.Snapshot)
	if err != nil {
		return evaluation.Run{}, fmt.Errorf("eval runs: snapshot could not be stored: %w", err)
	}

	created, err := scanRun(r.pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %s.eval_runs (
			dataset_id, application_id, application_version_id, trigger_type,
			created_by, status, execution_mode, snapshot, progress_done, progress_total
		) VALUES ($1, $2::int, $3::int, $4, $5::int, $6, $7, $8::jsonb, 0, $9)
		RETURNING `+runColumns, schema),
		datasetID, run.ApplicationID, run.ApplicationVersionID, run.TriggerType,
		run.CreatedBy, run.Status, run.ExecutionMode, string(snapshot), run.Progress.Total))
	if err != nil {
		return evaluation.Run{}, wrapEvalWrite("run create", err)
	}
	return created, nil
}

// CancelRun moves a NON-TERMINAL run to `cancelled`.
//
// The predicate is what makes this safe against the orchestrator: a run that
// already finished must not be re-labelled cancelled, and a run that is
// running must be stoppable. `status NOT IN (terminal)` is both, in one
// statement, so there is no read-then-write window for the worker to finish in.
//
// A run that is already terminal answers 409 and not 404: the run exists, and
// telling the caller it does not would send them looking for a missing row.
func (r *EvalRunsRepo) CancelRun(ctx context.Context, projectID, runID string) (evaluation.Run, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return evaluation.Run{}, err
	}
	id, err := parseEvalID(runID, "run")
	if err != nil {
		return evaluation.Run{}, err
	}

	cancelled, err := scanRun(r.pool.QueryRow(ctx, fmt.Sprintf(`
		UPDATE %s.eval_runs
		SET status = $1, finished_at = now()
		WHERE id = $2 AND status IN ($3, $4)
		RETURNING `+runColumns, schema),
		evaluation.RunStatusCancelled, id,
		evaluation.RunStatusCreated, evaluation.RunStatusRunning))
	if err == nil {
		return cancelled, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return evaluation.Run{}, fmt.Errorf("eval runs: cancel: %w", err)
	}

	existing, getErr := r.GetRun(ctx, projectID, runID)
	if getErr != nil {
		return evaluation.Run{}, getErr
	}
	return evaluation.Run{}, apierr.Conflict("this run is already " + existing.Status + " and cannot be cancelled")
}

const resultColumns = `
	id::text,
	run_id::text,
	dataset_case_id::text,
	dimension_id::text,
	status,
	native_score,
	normalized_score,
	target_met,
	verdict,
	evidence,
	created_at`

func scanResult(row pgx.Row) (evaluation.RunResult, error) {
	var result evaluation.RunResult
	var verdict, evidence []byte
	var createdAt time.Time
	err := row.Scan(
		&result.ID,
		&result.RunID,
		&result.DatasetCaseID,
		&result.DimensionID,
		&result.Status,
		&result.NativeScore,
		&result.NormalizedScore,
		&result.TargetMet,
		&verdict,
		&evidence,
		&createdAt,
	)
	if err != nil {
		return evaluation.RunResult{}, err
	}
	result.Verdict = map[string]any{}
	result.Evidence = map[string]any{}
	if len(verdict) > 0 {
		_ = json.Unmarshal(verdict, &result.Verdict)
	}
	if len(evidence) > 0 {
		_ = json.Unmarshal(evidence, &result.Evidence)
	}
	result.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	return result, nil
}

// ListResults reads one page of a run's scores and the TOTAL.
//
// The total is a separate count and not `len(rows)`. The reference's scorecard
// uses `total`, `offset` and `results.length` together to decide whether the
// page it has covers every case; a total that was really the page size would
// make a truncated scorecard claim to be complete.
func (r *EvalRunsRepo) ListResults(
	ctx context.Context,
	projectID, runID string,
	page evaluation.ResultPage,
) ([]evaluation.RunResult, int, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return nil, 0, err
	}
	id, err := parseEvalID(runID, "run")
	if err != nil {
		return nil, 0, err
	}
	limit := page.Limit
	if limit <= 0 {
		limit = evaluation.DefaultResultPageLimit
	}

	var total int
	if err := r.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT count(*)::int FROM %s.eval_results WHERE run_id = $1`, schema), id).
		Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("eval runs: result count: %w", err)
	}

	// ORDER BY (dataset_case_id, id) is the reference's own ordering, and its
	// truncation-aware pager depends on it: a page has to be a contiguous run
	// of whole cases, or "which cases does this page cover" is unanswerable.
	rows, err := r.pool.Query(ctx, fmt.Sprintf(`
		SELECT `+resultColumns+`
		FROM %s.eval_results
		WHERE run_id = $1
		ORDER BY dataset_case_id ASC, id ASC
		LIMIT $2 OFFSET $3`, schema), id, limit, page.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("eval runs: results: %w", err)
	}
	defer rows.Close()

	results := []evaluation.RunResult{}
	for rows.Next() {
		result, scanErr := scanResult(rows)
		if scanErr != nil {
			return nil, 0, fmt.Errorf("eval runs: result scan: %w", scanErr)
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("eval runs: results: %w", err)
	}
	return results, total, nil
}

// ClaimRun moves `created` → `running` and answers the claimed row.
//
// It is the whole of the concurrency control. The enqueue path and the recovery
// sweep may both deliver the same run, and two replicas may both be sweeping;
// a conditional UPDATE is what makes exactly one of them win. A read-then-write
// claim would let both see `created`.
//
// pgx.ErrNoRows is turned into a plain error and not a 404: this is not a route,
// and "somebody else has it" is an ordinary outcome the caller logs and drops.
func (r *EvalRunsRepo) ClaimRun(ctx context.Context, projectID, runID string) (evaluation.Run, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return evaluation.Run{}, err
	}
	id, err := parseEvalID(runID, "run")
	if err != nil {
		return evaluation.Run{}, err
	}

	claimed, err := scanRun(r.pool.QueryRow(ctx, fmt.Sprintf(`
		UPDATE %s.eval_runs
		SET status = $1,
		    started_at = COALESCE(started_at, now()),
		    heartbeat_at = now(),
		    error = NULL
		WHERE id = $2 AND status = $3
		RETURNING `+runColumns, schema),
		evaluation.RunStatusRunning, id, evaluation.RunStatusCreated))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return evaluation.Run{}, errors.New("run is not in the created state")
		}
		return evaluation.Run{}, fmt.Errorf("eval runs: claim: %w", err)
	}
	return claimed, nil
}

// Heartbeat records progress and refreshes the liveness stamp.
//
// `status = 'running'` is in the predicate so a heartbeat cannot resurrect a
// run somebody cancelled while it was working — without it, the worker's next
// case would stamp `heartbeat_at` on a `cancelled` row and the progress bar
// would move after the user pressed stop.
func (r *EvalRunsRepo) Heartbeat(ctx context.Context, projectID, runID string, done int) error {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	id, err := parseEvalID(runID, "run")
	if err != nil {
		return err
	}

	// LEAST(done, progress_total) keeps tenant/0132's progress CHECK
	// satisfiable if a dataset grew between the snapshot and the walk. The
	// alternative is a constraint violation that would abort a run for a
	// cosmetic reason.
	_, err = r.pool.Exec(ctx, fmt.Sprintf(`
		UPDATE %s.eval_runs
		SET progress_done = LEAST($1, progress_total), heartbeat_at = now()
		WHERE id = $2 AND status = $3`, schema),
		done, id, evaluation.RunStatusRunning)
	if err != nil {
		return fmt.Errorf("eval runs: heartbeat: %w", err)
	}
	return nil
}

// SaveResult upserts one score.
//
// UPSERT and not INSERT, because the orchestrator is restartable: a re-queued
// run re-scores cases it already scored, and a second row for the same (run,
// case, dimension) would be counted twice by every average. tenant/0132's
// UNIQUE constraint is what makes ON CONFLICT possible at all.
func (r *EvalRunsRepo) SaveResult(
	ctx context.Context,
	projectID string,
	result evaluation.RunResult,
) error {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	runID, err := parseEvalID(result.RunID, "run")
	if err != nil {
		return err
	}
	caseID, err := parseEvalID(result.DatasetCaseID, "case")
	if err != nil {
		return err
	}
	dimensionID, err := parseEvalID(result.DimensionID, "dimension")
	if err != nil {
		return err
	}
	verdict, err := json.Marshal(orEmptyObject(result.Verdict))
	if err != nil {
		return fmt.Errorf("eval runs: verdict could not be stored: %w", err)
	}
	evidence, err := json.Marshal(orEmptyObject(result.Evidence))
	if err != nil {
		return fmt.Errorf("eval runs: evidence could not be stored: %w", err)
	}

	_, err = r.pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.eval_results (
			run_id, dataset_case_id, dimension_id, status,
			native_score, normalized_score, target_met, verdict, evidence
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb)
		ON CONFLICT (run_id, dataset_case_id, dimension_id) DO UPDATE
		SET status = EXCLUDED.status,
		    native_score = EXCLUDED.native_score,
		    normalized_score = EXCLUDED.normalized_score,
		    target_met = EXCLUDED.target_met,
		    verdict = EXCLUDED.verdict,
		    evidence = EXCLUDED.evidence,
		    created_at = now()`, schema),
		runID, caseID, dimensionID, result.Status,
		result.NativeScore, result.NormalizedScore, result.TargetMet,
		string(verdict), string(evidence))
	if err != nil {
		return wrapEvalWrite("result save", err)
	}
	return nil
}

func orEmptyObject(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

// FinishRun writes the terminal status, and REFUSES to move a row that is
// already terminal.
//
// The refusal is what makes cancellation work. The cancel route writes
// `cancelled` on the row while the worker is mid-case; the worker then reaches
// the end of its walk and would otherwise stamp `finished` over it, so the
// user's stop would silently un-happen. `status = 'running'` in the predicate
// is the whole mechanism.
func (r *EvalRunsRepo) FinishRun(
	ctx context.Context,
	projectID, runID, status string,
	headline *float64,
	failure string,
) error {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	id, err := parseEvalID(runID, "run")
	if err != nil {
		return err
	}

	_, err = r.pool.Exec(ctx, fmt.Sprintf(`
		UPDATE %s.eval_runs
		SET status = $1,
		    headline_score = $2,
		    error = NULLIF($3, ''),
		    finished_at = now(),
		    heartbeat_at = NULL
		WHERE id = $4 AND status = $5`, schema),
		status, headline, failure, id, evaluation.RunStatusRunning)
	if err != nil {
		return fmt.Errorf("eval runs: finish: %w", err)
	}
	return nil
}

// RequeueStaleRuns moves orphaned `running` rows back to `created`, across
// every tenant schema.
//
// WHY IT WALKS THE SCHEMAS. A run belongs to one project and every project has
// its own schema, so there is no single table to sweep. The schema list comes
// from `information_schema.schemata` matched on `^p_[0-9]+$` — the same
// predicate internal/infra/db/migrate.go uses — so a schema whose name could
// not be a project's is never interpolated.
//
// WHY IT EXISTS AT ALL. The orchestrator is a goroutine, and a goroutine dies
// with its process. Without this sweep a run that was executing when the pod
// was replaced stays `running` for ever: its dataset is never scored, its
// progress bar never moves, and nothing anywhere reports it. `started_at`
// cannot distinguish a dead run from a slow one, which is why the heartbeat
// column exists.
func (r *EvalRunsRepo) RequeueStaleRuns(ctx context.Context, olderThanSeconds int) ([]evaluation.RunRef, error) {
	schemas, err := r.projectSchemas(ctx)
	if err != nil {
		return nil, err
	}

	requeued := []evaluation.RunRef{}
	for _, schema := range schemas {
		rows, queryErr := r.pool.Query(ctx, fmt.Sprintf(`
			UPDATE %s.eval_runs
			SET status = $1, heartbeat_at = NULL
			WHERE status = $2
			  AND (heartbeat_at IS NULL OR heartbeat_at < now() - make_interval(secs => $3))
			RETURNING id::text`, quoteSchema(schema)),
			evaluation.RunStatusCreated, evaluation.RunStatusRunning, olderThanSeconds)
		if queryErr != nil {
			// One project's failure must not stop the sweep for every other
			// project: a schema that has not run tenant/0132 yet has no
			// eval_runs table, and treating that as fatal would disable
			// recovery platform-wide during a rolling migration.
			continue
		}
		for rows.Next() {
			var runID string
			if scanErr := rows.Scan(&runID); scanErr != nil {
				continue
			}
			requeued = append(requeued, evaluation.RunRef{
				ProjectID: projectIDFromSchema(schema), RunID: runID,
			})
		}
		rows.Close()
	}
	return requeued, nil
}

// PendingRuns lists `created` runs across every tenant schema, oldest first.
func (r *EvalRunsRepo) PendingRuns(ctx context.Context, limit int) ([]evaluation.RunRef, error) {
	schemas, err := r.projectSchemas(ctx)
	if err != nil {
		return nil, err
	}

	pending := []evaluation.RunRef{}
	for _, schema := range schemas {
		if len(pending) >= limit {
			break
		}
		rows, queryErr := r.pool.Query(ctx, fmt.Sprintf(
			`SELECT id::text FROM %s.eval_runs WHERE status = $1 ORDER BY id ASC LIMIT $2`,
			quoteSchema(schema)), evaluation.RunStatusCreated, limit-len(pending))
		if queryErr != nil {
			continue
		}
		for rows.Next() {
			var runID string
			if scanErr := rows.Scan(&runID); scanErr != nil {
				continue
			}
			pending = append(pending, evaluation.RunRef{
				ProjectID: projectIDFromSchema(schema), RunID: runID,
			})
		}
		rows.Close()
	}
	return pending, nil
}

// projectSchemas lists the tenant schemas that actually carry an eval_runs
// table.
//
// The `to_regclass` join is not an optimisation. Interpolating a schema name
// that has no eval_runs table produces a 42P01 per project on every sweep,
// which is a log full of errors that mean nothing — and during a rolling
// migration that is EVERY project.
func (r *EvalRunsRepo) projectSchemas(ctx context.Context) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT schema_name
		FROM information_schema.schemata
		WHERE schema_name ~ '^p_[0-9]+$'
		  AND to_regclass(schema_name || '.eval_runs') IS NOT NULL
		ORDER BY schema_name`)
	if err != nil {
		return nil, fmt.Errorf("eval runs: project schema scan: %w", err)
	}
	defer rows.Close()

	schemas := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("eval runs: project schema scan: %w", err)
		}
		schemas = append(schemas, name)
	}
	return schemas, rows.Err()
}

// quoteSchema re-quotes a name that the `^p_[0-9]+$` predicate has already
// proved safe. It is applied anyway, because "the query that produced this
// string is safe" is a property a later edit can remove without any test
// noticing.
func quoteSchema(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func projectIDFromSchema(name string) string { return strings.TrimPrefix(name, "p_") }

// AgentVersion reads the instructions and model the agent turn is built from.
//
// A version that does not exist is a 404-shaped error and not an empty struct:
// running a case with no instructions would produce an answer from a bare model
// and score it as though it were the agent.
func (r *EvalRunsRepo) AgentVersion(
	ctx context.Context,
	projectID string,
	versionID int,
) (evaluation.AgentVersion, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return evaluation.AgentVersion{}, err
	}

	version := evaluation.AgentVersion{ID: versionID}
	err = r.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT COALESCE(instructions, ''),
		       COALESCE(llm_settings ->> 'model_name', '')
		FROM %s.application_versions
		WHERE id = $1`, schema), versionID).
		Scan(&version.Instructions, &version.ModelName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return evaluation.AgentVersion{}, apierr.NotFound("agent version not found in this project")
		}
		return evaluation.AgentVersion{}, fmt.Errorf("eval runs: agent version: %w", err)
	}
	return version, nil
}

// DatasetCases reads a dataset's cases in run order.
//
// It is on the RUN repository and not only on the dataset one because the
// orchestrator needs it and the orchestrator holds a RunRepository. The query
// is the same shape as the dataset detail's page, without the window: a run
// executes every case, so a page here would silently score part of a dataset.
func (r *EvalRunsRepo) DatasetCases(
	ctx context.Context,
	projectID, datasetID string,
) ([]evaluation.DatasetCase, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return nil, err
	}
	id, err := parseEvalID(datasetID, "dataset")
	if err != nil {
		return nil, err
	}

	rows, err := r.pool.Query(ctx, fmt.Sprintf(
		`SELECT `+caseColumns+` FROM %s.eval_dataset_cases WHERE dataset_id = $1 ORDER BY order_index ASC, id ASC`,
		schema), id)
	if err != nil {
		return nil, fmt.Errorf("eval runs: dataset cases: %w", err)
	}
	defer rows.Close()

	cases := []evaluation.DatasetCase{}
	for rows.Next() {
		datasetCase, scanErr := scanCase(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("eval runs: dataset case scan: %w", scanErr)
		}
		cases = append(cases, datasetCase)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eval runs: dataset cases: %w", err)
	}
	return cases, nil
}
