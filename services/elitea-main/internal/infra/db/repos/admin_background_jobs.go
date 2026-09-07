package repos

// The admin Tasks page's read and its cancel.
//
// # What legacy's Tasks page was, and why this is not a copy of it
//
// legacy/plugins/admin's Tasks page (module.py:392-421, api/v2/tasks.py) lists
// an in-process `arbiter.TaskNode`'s `global_task_state` — one-off maintenance
// operations that plugins register at import time, started and stopped through
// the Arbiter. There is no such registry here and AGENTS.md says there will not
// be one, so `/api/v2/admin/tasks/administration` answers 501 and keeps
// answering it (internal/api/v2/admin/handler.go, arbiterTaskNodeUnavailable).
//
// What the page MEANT to an operator — "what is running on this platform right
// now, who started it, and can I stop it" — is a real question this platform
// can answer, from its own tables. That is what this file serves. It keeps the
// legacy row keys an operator's tooling reads (`task_id`, `status`,
// `started_at`, `user`) and adds the two the legacy shape had no need for,
// because pylon had exactly one kind of task and one project: `kind` and
// `project_id`.
//
// # The three sources
//
//	elitea_runtime.execution_jobs        every runtime-plane job. `kind` comes
//	                                     from capability_id, so index ingest,
//	                                     agent execution and toolkit tool runs
//	                                     are three kinds of one table.
//	elitea_runtime.scheduled_occurrences the scheduler's due-work rows.
//	<tenant>.eval_runs                   agent-evaluation runs, which live in
//	                                     EVERY project's own schema.
//
// # Why the query is built rather than written once
//
// `eval_runs` is a TENANT table. There is no one relation holding every
// project's runs, so the union has one leg per project schema that has the
// table. The schema list comes from the catalogue and every name is matched
// against `^p_[0-9]+$` before it is quoted, so nothing caller-supplied reaches
// the SQL text.
//
// # Why the scan is capped
//
// Each leg carries its own ORDER BY / LIMIT, so the union is bounded no matter
// how large `execution_jobs` has grown. The page therefore reads the most
// recent `backgroundJobScanCap` rows PER SOURCE and paginates within them. An
// admin console that polls every ten seconds must not be able to table-scan the
// execution history, and an operator looking for a running job is looking at
// the recent end of it. `Truncated` says out loud when the window may be
// hiding older rows, rather than presenting a capped list as the whole truth.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// backgroundJobScanCap is the per-source row window. See this file's header.
const backgroundJobScanCap = 500

// Job kinds, as the wire spells them.
const (
	BackgroundJobKindIndex     = "index"
	BackgroundJobKindAgent     = "agent"
	BackgroundJobKindToolkit   = "toolkit"
	BackgroundJobKindExecution = "execution"
	BackgroundJobKindSchedule  = "schedule"
	BackgroundJobKindEval      = "eval"
)

// BackgroundJobRow is one row of the admin Tasks table.
type BackgroundJobRow struct {
	TaskID     string
	Kind       string
	Name       string
	Status     string
	StartedAt  *time.Time
	FinishedAt *time.Time
	ProjectID  *int64
	// Principal is the actor the job runs as. It is the raw stored identifier
	// (a user id for a runtime job, empty for a scheduled occurrence that has
	// no interactive principal at all) — the handler resolves it to a name.
	Principal string
	// Cancellable reports whether THIS row can still be stopped. It is computed
	// from the row's own state, so the page never offers a control the cancel
	// route would refuse.
	Cancellable bool
}

// BackgroundJobFilter narrows the listing. An empty field means "every value".
type BackgroundJobFilter struct {
	Kind      string
	Status    string
	ProjectID *int64
	Limit     int
	Offset    int
}

// BackgroundJobPage is one page of the listing.
type BackgroundJobPage struct {
	Rows  []BackgroundJobRow
	Total int
	// Truncated reports that the per-source window was full, so older rows
	// exist that this page cannot reach. See this file's header.
	Truncated bool
}

// AdminBackgroundJobsRepository reads the three job sources and flips the
// durable cancel flag.
type AdminBackgroundJobsRepository struct {
	pool *pgxpool.Pool
}

func NewAdminBackgroundJobsRepository(pool *pgxpool.Pool) (*AdminBackgroundJobsRepository, error) {
	if pool == nil {
		return nil, errors.New("admin background jobs database is required")
	}
	return &AdminBackgroundJobsRepository{pool: pool}, nil
}

// executionJobsLeg is the runtime plane.
//
// `status` is the job's OBSERVED state except while a cancel is in flight: a
// row whose desired_state is CANCELLED and whose state is still live has been
// asked to stop and has not stopped, and reporting it as RUNNING is how an
// operator presses cancel a second time. CANCELLING is not a state the table
// holds; it is the pair of columns, named.
const executionJobsLeg = `
SELECT job.execution_id AS task_id,
       CASE job.capability_id
           WHEN 'index.ingest.v1'              THEN 'index'
           WHEN 'agent.execute.application.v1' THEN 'agent'
           WHEN 'agent.execute.adhoc.v1'       THEN 'agent'
           WHEN 'toolkit.call_tool.v1'         THEN 'toolkit'
           ELSE 'execution'
       END AS kind,
       job.capability_id AS name,
       CASE
           WHEN job.desired_state = 'CANCELLED'
            AND job.state NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED')
           THEN 'CANCELLING'
           ELSE job.state
       END AS status,
       job.admitted_at AS started_at,
       job.settled_at AS finished_at,
       job.resource_project_id::bigint AS project_id,
       job.actor_id AS principal,
       (job.state NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED')
        AND job.desired_state <> 'CANCELLED') AS cancellable
FROM elitea_runtime.execution_jobs AS job
ORDER BY job.admitted_at DESC
LIMIT %d`

// scheduledOccurrencesLeg is the scheduler's due work.
//
// `cancellable` is FALSE for every row, and that is the whole decision: an
// occurrence is one firing of a recurring schedule, so stopping it stops
// nothing durable — the next occurrence is already computed from the same cron
// row. What an operator wants is to DISABLE the schedule, which is the switch
// on Admin › Schedules & Tasks. Offering a cancel here would be a control that
// appears to work and changes nothing by the next minute.
const scheduledOccurrencesLeg = `
SELECT occurrence.invocation_id AS task_id,
       'schedule' AS kind,
       occurrence.job_id AS name,
       occurrence.state AS status,
       occurrence.admitted_at AS started_at,
       occurrence.completed_at AS finished_at,
       NULL::bigint AS project_id,
       '' AS principal,
       FALSE AS cancellable
FROM elitea_runtime.scheduled_occurrences AS occurrence
ORDER BY occurrence.created_at DESC
LIMIT %d`

// evalRunsLeg is one project's evaluation runs. %s is the QUOTED schema name
// and %d the project id, both derived from the catalogue — see the header.
const evalRunsLeg = `
SELECT run.id::text AS task_id,
       'eval' AS kind,
       COALESCE(run.application_id::text, '') AS name,
       run.status AS status,
       COALESCE(run.started_at, run.created_at) AS started_at,
       run.finished_at AS finished_at,
       %d::bigint AS project_id,
       COALESCE(run.created_by::text, '') AS principal,
       (run.status NOT IN ('finished', 'errored', 'cancelled')) AS cancellable
FROM %s.eval_runs AS run
ORDER BY run.created_at DESC
LIMIT %d`

// evalRunSchemas lists the tenant schemas that carry an eval_runs table, with
// the project id each one belongs to.
//
// It reads the CATALOGUE rather than the project table: a project row whose
// schema was never created would produce a leg over a relation that does not
// exist, and one failed leg fails the whole listing. The `^p_[0-9]+$` predicate
// is applied in SQL and again in Go before the name is quoted.
func (r *AdminBackgroundJobsRepository) evalRunSchemas(ctx context.Context) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `
SELECT table_schema
FROM information_schema.tables
WHERE table_name = 'eval_runs'
  AND table_schema ~ '^p_[0-9]+$'
ORDER BY (substring(table_schema FROM 3))::bigint`)
	if err != nil {
		return nil, fmt.Errorf("list evaluation tenant schemas: %w", err)
	}
	defer rows.Close()

	projectIDs := []int64{}
	for rows.Next() {
		var schema string
		if err := rows.Scan(&schema); err != nil {
			return nil, fmt.Errorf("scan evaluation tenant schema: %w", err)
		}
		projectID, err := strconv.ParseInt(strings.TrimPrefix(schema, "p_"), 10, 64)
		if err != nil || projectID <= 0 {
			continue
		}
		projectIDs = append(projectIDs, projectID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list evaluation tenant schemas: %w", err)
	}
	return projectIDs, nil
}

// ListBackgroundJobs answers one page of the union.
func (r *AdminBackgroundJobsRepository) ListBackgroundJobs(
	ctx context.Context, filter BackgroundJobFilter,
) (BackgroundJobPage, error) {
	// Each leg is PARENTHESISED. A bare `... ORDER BY x LIMIT n UNION ALL ...`
	// is a syntax error in PostgreSQL: an unparenthesised ORDER BY/LIMIT binds
	// to the whole union, not to the branch it is written under.
	legs := []string{
		"(" + fmt.Sprintf(executionJobsLeg, backgroundJobScanCap) + ")",
		"(" + fmt.Sprintf(scheduledOccurrencesLeg, backgroundJobScanCap) + ")",
	}
	evalProjects, err := r.evalRunSchemas(ctx)
	if err != nil {
		return BackgroundJobPage{}, err
	}
	for _, projectID := range evalProjects {
		legs = append(legs, "("+fmt.Sprintf(
			evalRunsLeg, projectID, quotedTenantSchema(projectID), backgroundJobScanCap,
		)+")")
	}

	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	// The filters are BIND PARAMETERS, and each one is "no filter" when empty.
	// Writing them as `($1 = '' OR kind = $1)` rather than as appended SQL is
	// what keeps this one prepared statement instead of a per-request string.
	query := fmt.Sprintf(`
WITH jobs AS (
%s
)
SELECT task_id, kind, name, status, started_at, finished_at, project_id,
       principal, cancellable, count(*) OVER () AS total,
       (SELECT count(*) FROM jobs) AS scanned
FROM jobs
WHERE ($1 = '' OR kind = $1)
  AND ($2 = '' OR status = $2)
  AND ($3::bigint IS NULL OR project_id = $3::bigint)
ORDER BY started_at DESC NULLS LAST, task_id
LIMIT $4 OFFSET $5`, strings.Join(legs, "\nUNION ALL\n"))

	rows, err := r.pool.Query(ctx, query,
		filter.Kind, filter.Status, filter.ProjectID, limit, offset)
	if err != nil {
		return BackgroundJobPage{}, fmt.Errorf("list background jobs: %w", err)
	}
	defer rows.Close()

	page := BackgroundJobPage{Rows: []BackgroundJobRow{}}
	scanned := 0
	for rows.Next() {
		var row BackgroundJobRow
		if err := rows.Scan(
			&row.TaskID, &row.Kind, &row.Name, &row.Status,
			&row.StartedAt, &row.FinishedAt, &row.ProjectID,
			&row.Principal, &row.Cancellable, &page.Total, &scanned,
		); err != nil {
			return BackgroundJobPage{}, fmt.Errorf("scan background job: %w", err)
		}
		page.Rows = append(page.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return BackgroundJobPage{}, fmt.Errorf("list background jobs: %w", err)
	}
	// `scanned` counts the WINDOW, before the filters. A window that reached
	// the cap for any source may be hiding older rows. Reported as one boolean
	// rather than per source, because the operator's question is "is this all
	// of it", not "which table was busy".
	page.Truncated = scanned >= backgroundJobScanCap
	return page, nil
}

// quotedTenantSchema renders `p_<id>` as a quoted identifier. The id is an
// int64 read from the catalogue, so it cannot carry a quote.
func quotedTenantSchema(projectID int64) string {
	return fmt.Sprintf(`"p_%d"`, projectID)
}

// ErrBackgroundJobNotCancellable is what a cancel answers for a job that has
// already settled, or for a kind that has no cancel at all.
var ErrBackgroundJobNotCancellable = errors.New("background job cannot be cancelled")

// CancelRuntimeJob asks a runtime-plane execution to stop.
//
// It sets the DURABLE DESIRED STATE, which is the same primitive every
// per-kind cancel in this repository uses (index_cancel.go, agent_cancel.go):
// the worker observes it, stops, and settlement writes the terminal state.
// This route does NOT run the per-kind projection cleanup those paths also
// perform — the chat cancel deletes a half-written message group, the index
// cancel restamps index metadata — and that omission is deliberate. Those
// cleanups belong to the USER who owns the artefact; an administrator stopping
// a runaway job must not delete somebody's conversation as a side effect.
//
// Every non-terminal GENERATION of the execution is flipped. A generation is a
// retry of the same execution, so an admin stop that left the next generation
// running would read as a control that did nothing.
func (r *AdminBackgroundJobsRepository) CancelRuntimeJob(
	ctx context.Context, executionID string,
) error {
	tag, err := r.pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET desired_state = 'CANCELLED'
WHERE execution_id = $1
  AND desired_state = 'RUNNING'
  AND state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING')`, executionID)
	if err != nil {
		return fmt.Errorf("cancel runtime job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrBackgroundJobNotCancellable
	}
	return nil
}
