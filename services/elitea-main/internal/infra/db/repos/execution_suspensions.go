package repos

// The kill switch's durable half (ADR-0041 decision 1, shared 0194): the
// suspension rows and the cancel of the runs a new suspension covers.
//
// The cancel is the SAME primitive every other stop in this repository uses
// (CancelRuntimeJob, agent_cancel.sql, index_cancel.go): it sets the durable
// desired state of every non-terminal generation to CANCELLED, the worker
// observes it on its next lease renewal or desired-state poll, and
// settlement writes the terminal state. Like the admin Tasks cancel, it does
// NOT run the per-kind projection cleanup (deleting a half-written chat
// message group): an administrator stopping runs must not delete a user's
// conversation as a side effect.
//
// The insert and the cancel commit in one transaction, so a suspension that
// exists has cancelled what it covered at that instant. A run admitted after
// the commit meets the admission gate instead, and the admission fence
// (safety/fence.go, fenceSuspendedAdmission below) closes the window
// between the two: no admission commits unseen by both.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/guardrails"
)

// ExecutionSuspensionRepository implements safety.Store.
type ExecutionSuspensionRepository struct {
	pool *pgxpool.Pool
}

// NewExecutionSuspensionRepository builds the repository.
func NewExecutionSuspensionRepository(pool *pgxpool.Pool) (*ExecutionSuspensionRepository, error) {
	if pool == nil {
		return nil, errors.New("execution suspension database is required")
	}
	return &ExecutionSuspensionRepository{pool: pool}, nil
}

var _ safety.Store = (*ExecutionSuspensionRepository)(nil)

const suspensionColumns = `id, scope, project_id, scope_key, reason, actor_id, source, resume_requires,
       created_at, cancelled_runs, resumed_at, resumed_by, resume_reason`

// cancellableStates are the non-terminal states, the same list as the admin
// Tasks cancel and agent_cancel.sql.
const cancellableJobPredicate = `j.desired_state IN ('RUNNING', 'SUSPENDED')
  AND j.state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING')`

// suspensionCancelSQL is the cancel of the runs a new suspension covers, per
// scope. Each answers the number of job generations it flipped.
//
//   - platform: every runtime job (agent, index, toolkit, validation).
//   - project: every job whose resource or projection project is the one.
//   - agent: every agent job admitted for that agent of that project
//     (agent_execution_jobs.application_id, written at admission since 0194).
//   - toolkit_type: every job whose recorded toolkit types
//     (execution_jobs.toolkit_types, written at admission since 0201)
//     contain the canonical key: an agent run whose frozen toolkit set (its
//     sub-agents' included) has the type, and a direct toolkit run of the
//     type. A run admitted before 0201 has no record; backfillToolkitTypes
//     reads an agent run's from its stored input first, in the same
//     transaction. A non-agent job with no record names no type and is not
//     cancelled.
func suspensionCancelSQL(target safety.Target) (string, []any, bool) {
	switch target.Scope {
	case safety.ScopePlatform:
		return `UPDATE elitea_runtime.execution_jobs AS j SET desired_state = 'CANCELLED'
WHERE ` + cancellableJobPredicate, nil, true
	case safety.ScopeProject:
		return `UPDATE elitea_runtime.execution_jobs AS j SET desired_state = 'CANCELLED'
WHERE (j.resource_project_id = $1::bigint OR j.projection_project_id = $1::bigint)
  AND ` + cancellableJobPredicate, []any{target.ProjectID}, true
	case safety.ScopeAgent:
		return `UPDATE elitea_runtime.execution_jobs AS j SET desired_state = 'CANCELLED'
FROM elitea_runtime.agent_execution_jobs AS a
WHERE a.execution_id = j.execution_id AND a.generation = j.generation
  AND a.application_id = $2::bigint
  AND j.resource_project_id = $1::bigint
  AND ` + cancellableJobPredicate, []any{target.ProjectID, target.ApplicationID}, true
	case safety.ScopeToolkitType:
		key := guardrails.CanonicalKey(target.ToolkitType)
		if key == "" {
			return "", nil, false
		}
		return `UPDATE elitea_runtime.execution_jobs AS j SET desired_state = 'CANCELLED'
WHERE j.toolkit_types @> ARRAY[$1::text]
  AND ` + cancellableJobPredicate, []any{key}, true
	}
	return "", nil, false
}

// legacyToolkitTypesSQL reads the stored input of every in-flight agent run
// that has no toolkit_types record (admitted before shared 0201). The input
// is the request entry the worker reads (agent_execution_jobs
// .request_entry_id). FOR UPDATE OF j holds the rows until the backfill and
// the cancel in the same transaction.
const legacyToolkitTypesSQL = `SELECT j.execution_id, j.generation, j.capability_id, e.content_bytes
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.agent_execution_jobs AS a
  ON a.execution_id = j.execution_id AND a.generation = j.generation
JOIN elitea_runtime.input_bundle_entries AS e
  ON e.input_bundle_id = a.input_bundle_id AND e.entry_id = a.request_entry_id
WHERE j.toolkit_types IS NULL
  AND j.capability_id IN ('agent.execute.application.v1', 'agent.execute.adhoc.v1')
  AND ` + cancellableJobPredicate + `
FOR UPDATE OF j`

// backfillToolkitTypes records the toolkit types of the in-flight agent runs
// admitted before shared 0201, from their stored input, so the
// toolkit_type cancel that follows sees them. Only the run's OWN frozen
// toolkit set is read (agentexecution.FrozenToolkitTypes): a sub-agent's
// toolkits of such a run are not found, and the worker's call-time refusal
// does not cover them either. An input that cannot be read is recorded as
// no toolkit and logged; it is never guessed.
func backfillToolkitTypes(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, legacyToolkitTypesSQL)
	if err != nil {
		return fmt.Errorf("read the toolkit sets of runs admitted before 0201: %w", err)
	}
	type legacyRun struct {
		executionID string
		generation  int64
		types       []string
	}
	var runs []legacyRun
	for rows.Next() {
		var (
			run        legacyRun
			capability string
			content    []byte
		)
		if err := rows.Scan(&run.executionID, &run.generation, &capability, &content); err != nil {
			rows.Close()
			return fmt.Errorf("scan a run admitted before 0201: %w", err)
		}
		input := &runtimev1.AgentExecutionInputV1{}
		if err := proto.Unmarshal(content, input); err != nil {
			slog.WarnContext(ctx, "the stored input of an in-flight run cannot be read for its toolkit types",
				"execution_id", run.executionID)
			run.types = []string{}
		} else if run.types, err = agentexecutionapp.FrozenToolkitTypes(capability, input); err != nil {
			slog.WarnContext(ctx, "the frozen toolkit set of an in-flight run cannot be read",
				"execution_id", run.executionID)
			run.types = []string{}
		}
		runs = append(runs, run)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the toolkit sets of runs admitted before 0201: %w", err)
	}
	for _, run := range runs {
		if _, err := tx.Exec(ctx, `UPDATE elitea_runtime.execution_jobs SET toolkit_types = $3::text[]
WHERE execution_id = $1 AND generation = $2 AND toolkit_types IS NULL`,
			run.executionID, run.generation, run.types); err != nil {
			return fmt.Errorf("record the toolkit types of a run admitted before 0201: %w", err)
		}
	}
	return nil
}

// admissionFenceLockSQL takes the admission fence (safety/fence.go) shared:
// every admission transaction, as its first statement.
const admissionFenceLockSQL = `SELECT pg_advisory_xact_lock_shared(hashtextextended($1::text, 0))`

// suspensionFenceLockSQL takes it exclusive: CreateSuspension, before it
// inserts and cancels.
const suspensionFenceLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`

// coveringSuspensionsSQL is the in-transaction re-read: the active
// suspensions that may cover a subject (a superset; safety.CheckSnapshot
// applies the exact rule). The active rows are the partial index of 0194.
const coveringSuspensionsSQL = `SELECT ` + suspensionColumns + `
FROM elitea_runtime.execution_suspensions
WHERE resumed_at IS NULL
  AND (scope = 'platform'
       OR (scope = 'project' AND project_id = $1::bigint)
       OR (scope = 'agent' AND project_id = $1::bigint AND scope_key = $2::text)
       OR (scope = 'toolkit_type' AND scope_key = $3::text))`

// fenceSuspendedAdmission is the admission half of the fence. It does nothing
// for an admission no gate marked (safety.AdmissionSubject); otherwise it
// takes the shared lock and refuses a subject an active suspension covers.
// It must be the first statement of the admission transaction, so the
// transaction holds no other lock while it waits for a suspension's commit.
func fenceSuspendedAdmission(ctx context.Context, tx sqlExecutor) error {
	subject, ok := safety.AdmissionSubject(ctx)
	if !ok {
		return nil
	}
	if _, err := tx.Exec(ctx, admissionFenceLockSQL, safety.AdmissionFenceLockName); err != nil {
		return fmt.Errorf("%w: admission fence: %v", safety.ErrStateUnavailable, err)
	}
	toolkitKey := ""
	if subject.ToolkitType != "" {
		toolkitKey = guardrails.CanonicalKey(subject.ToolkitType)
	}
	rows, err := tx.Query(ctx, coveringSuspensionsSQL,
		subject.ProjectID, strconv.FormatInt(subject.ApplicationID, 10), toolkitKey)
	if err != nil {
		return fmt.Errorf("%w: read the covering suspensions: %v", safety.ErrStateUnavailable, err)
	}
	defer rows.Close()
	var active []safety.Suspension
	for rows.Next() {
		found, err := scanSuspension(rows)
		if err != nil {
			return fmt.Errorf("%w: scan suspension: %v", safety.ErrStateUnavailable, err)
		}
		active = append(active, found)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: read the covering suspensions: %v", safety.ErrStateUnavailable, err)
	}
	return safety.CheckSnapshot(active, subject)
}

// CreateSuspension inserts the row and cancels the covered runs in one
// transaction, behind the exclusive admission fence (safety/fence.go): a
// run being admitted right now either commits before this transaction's
// cancel reads the jobs, or re-reads the suspension after it commits.
func (r *ExecutionSuspensionRepository) CreateSuspension(
	ctx context.Context, s safety.NewSuspension,
) (safety.Suspension, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return safety.Suspension{}, fmt.Errorf("begin suspension: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, suspensionFenceLockSQL, safety.AdmissionFenceLockName); err != nil {
		return safety.Suspension{}, fmt.Errorf("take the admission fence: %w", err)
	}

	// int64 end to end: an id past the integer column is refused by the
	// column's own cast, never narrowed into another project's id.
	var projectID *int64
	if s.Target.Scope == safety.ScopeProject || s.Target.Scope == safety.ScopeAgent {
		id := s.Target.ProjectID
		projectID = &id
	}
	var key *string
	if k := s.Target.Key(); k != "" {
		key = &k
	}
	row := tx.QueryRow(ctx, `
INSERT INTO elitea_runtime.execution_suspensions
    (scope, project_id, scope_key, reason, actor_id, source, resume_requires)
VALUES ($1, $2::bigint, $3, $4, $5, $6, $7)
ON CONFLICT DO NOTHING
RETURNING `+suspensionColumns,
		string(s.Target.Scope), projectID, key, s.Reason, s.ActorID, string(s.Source), string(s.ResumeRequires))
	created, err := scanSuspension(row)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, findErr := r.activeFor(ctx, tx, s.Target)
		if findErr != nil {
			return safety.Suspension{}, findErr
		}
		return safety.Suspension{}, &safety.AlreadySuspendedError{Existing: existing}
	}
	if err != nil {
		return safety.Suspension{}, fmt.Errorf("insert suspension: %w", err)
	}

	if s.Target.Scope == safety.ScopeToolkitType {
		if err := backfillToolkitTypes(ctx, tx); err != nil {
			return safety.Suspension{}, err
		}
	}
	if statement, args, ok := suspensionCancelSQL(s.Target); ok {
		tag, err := tx.Exec(ctx, statement, args...)
		if err != nil {
			return safety.Suspension{}, fmt.Errorf("cancel the runs the suspension covers: %w", err)
		}
		created.CancelledRuns = int(tag.RowsAffected())
		if _, err := tx.Exec(ctx, `UPDATE elitea_runtime.execution_suspensions SET cancelled_runs = $2 WHERE id = $1`,
			created.ID, created.CancelledRuns); err != nil {
			return safety.Suspension{}, fmt.Errorf("record the cancelled runs: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			existing, findErr := r.activeFor(ctx, r.pool, s.Target)
			if findErr == nil {
				return safety.Suspension{}, &safety.AlreadySuspendedError{Existing: existing}
			}
		}
		return safety.Suspension{}, fmt.Errorf("commit suspension: %w", err)
	}
	return created, nil
}

type suspensionQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (r *ExecutionSuspensionRepository) activeFor(
	ctx context.Context, q suspensionQuerier, target safety.Target,
) (safety.Suspension, error) {
	projectID := int64(-1)
	if target.Scope == safety.ScopeProject || target.Scope == safety.ScopeAgent {
		projectID = target.ProjectID
	}
	found, err := scanSuspension(q.QueryRow(ctx, `
SELECT `+suspensionColumns+` FROM elitea_runtime.execution_suspensions
WHERE scope = $1 AND COALESCE(project_id, -1) = $2 AND COALESCE(scope_key, '') = $3
  AND resumed_at IS NULL`, string(target.Scope), projectID, target.Key()))
	if errors.Is(err, pgx.ErrNoRows) {
		return safety.Suspension{}, safety.ErrNotFound
	}
	if err != nil {
		return safety.Suspension{}, fmt.Errorf("read the active suspension: %w", err)
	}
	return found, nil
}

// ResumeSuspension stamps an active row.
func (r *ExecutionSuspensionRepository) ResumeSuspension(
	ctx context.Context, record safety.ResumeRecord,
) (safety.Suspension, error) {
	var reason *string
	if record.Reason != "" {
		reason = &record.Reason
	}
	resumed, err := scanSuspension(r.pool.QueryRow(ctx, `
UPDATE elitea_runtime.execution_suspensions
SET resumed_at = greatest(clock_timestamp(), created_at), resumed_by = $2, resume_reason = $3
WHERE id = $1 AND resumed_at IS NULL
RETURNING `+suspensionColumns, record.ID, record.ResumedBy, reason))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, getErr := r.GetSuspension(ctx, record.ID); getErr != nil {
			return safety.Suspension{}, getErr
		}
		return safety.Suspension{}, safety.ErrNotActive
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" {
			// execution_suspensions_two_person: the database's copy of the
			// rule the service already applied.
			return safety.Suspension{}, safety.ErrResumeForbidden
		}
		return safety.Suspension{}, fmt.Errorf("resume suspension: %w", err)
	}
	return resumed, nil
}

// GetSuspension reads one row.
func (r *ExecutionSuspensionRepository) GetSuspension(ctx context.Context, id int64) (safety.Suspension, error) {
	found, err := scanSuspension(r.pool.QueryRow(ctx,
		`SELECT `+suspensionColumns+` FROM elitea_runtime.execution_suspensions WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return safety.Suspension{}, safety.ErrNotFound
	}
	if err != nil {
		return safety.Suspension{}, fmt.Errorf("read suspension: %w", err)
	}
	return found, nil
}

// ListSuspensions lists rows, newest first.
func (r *ExecutionSuspensionRepository) ListSuspensions(
	ctx context.Context, filter safety.ListFilter,
) ([]safety.Suspension, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	var projectID *int64
	if filter.ProjectID > 0 {
		id := filter.ProjectID
		projectID = &id
	}
	rows, err := r.pool.Query(ctx, `
SELECT `+suspensionColumns+` FROM elitea_runtime.execution_suspensions
WHERE ($1::boolean IS FALSE OR resumed_at IS NULL)
  AND ($2::bigint IS NULL OR project_id = $2::bigint)
ORDER BY created_at DESC, id DESC
LIMIT $3`, filter.ActiveOnly, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("list suspensions: %w", err)
	}
	return collectSuspensions(rows)
}

// LoadActiveSuspensions is the admission read: the active rows only, from the
// partial unique index.
func (r *ExecutionSuspensionRepository) LoadActiveSuspensions(ctx context.Context) ([]safety.Suspension, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+suspensionColumns+`
FROM elitea_runtime.execution_suspensions WHERE resumed_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("load active suspensions: %w", err)
	}
	return collectSuspensions(rows)
}

func collectSuspensions(rows pgx.Rows) ([]safety.Suspension, error) {
	defer rows.Close()
	out := []safety.Suspension{}
	for rows.Next() {
		s, err := scanSuspension(rows)
		if err != nil {
			return nil, fmt.Errorf("scan suspension: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read suspensions: %w", err)
	}
	return out, nil
}

func scanSuspension(row pgx.Row) (safety.Suspension, error) {
	var (
		s            safety.Suspension
		scope        string
		projectID    *int32
		key          *string
		source       string
		requires     string
		createdAt    time.Time
		cancelled    int32
		resumedAt    *time.Time
		resumedBy    *int64
		resumeReason *string
	)
	if err := row.Scan(&s.ID, &scope, &projectID, &key, &s.Reason, &s.ActorID, &source, &requires,
		&createdAt, &cancelled, &resumedAt, &resumedBy, &resumeReason); err != nil {
		return safety.Suspension{}, err
	}
	s.Target.Scope = safety.Scope(scope)
	if projectID != nil {
		s.Target.ProjectID = int64(*projectID)
	}
	if key != nil {
		switch s.Target.Scope {
		case safety.ScopeAgent:
			s.Target.ApplicationID, _ = strconv.ParseInt(*key, 10, 64)
		case safety.ScopeToolkitType:
			s.Target.ToolkitType = *key
		}
	}
	s.Source = safety.Source(source)
	s.ResumeRequires = safety.ResumeRequirement(requires)
	s.CreatedAt = createdAt
	s.CancelledRuns = int(cancelled)
	s.ResumedAt, s.ResumedBy = resumedAt, resumedBy
	if resumeReason != nil {
		s.ResumeReason = *resumeReason
	}
	return s, nil
}
