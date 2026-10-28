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
// the commit meets the admission gate instead.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
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
//   - toolkit_type: none. A toolkit run is one bounded synchronous call and
//     its job row does not name its type; an agent run's tool calls are
//     refused by the worker through the run's frozen guardrails. See the
//     runtime-safety page for what that leaves open.
func suspensionCancelSQL(target safety.Target) (string, []any, bool) {
	switch target.Scope {
	case safety.ScopePlatform:
		return `UPDATE elitea_runtime.execution_jobs AS j SET desired_state = 'CANCELLED'
WHERE ` + cancellableJobPredicate, nil, true
	case safety.ScopeProject:
		return `UPDATE elitea_runtime.execution_jobs AS j SET desired_state = 'CANCELLED'
WHERE (j.resource_project_id = $1 OR j.projection_project_id = $1)
  AND ` + cancellableJobPredicate, []any{int32(target.ProjectID)}, true
	case safety.ScopeAgent:
		return `UPDATE elitea_runtime.execution_jobs AS j SET desired_state = 'CANCELLED'
FROM elitea_runtime.agent_execution_jobs AS a
WHERE a.execution_id = j.execution_id AND a.generation = j.generation
  AND a.application_id = $2
  AND j.resource_project_id = $1
  AND ` + cancellableJobPredicate, []any{int32(target.ProjectID), target.ApplicationID}, true
	}
	return "", nil, false
}

// CreateSuspension inserts the row and cancels the covered runs in one
// transaction.
func (r *ExecutionSuspensionRepository) CreateSuspension(
	ctx context.Context, s safety.NewSuspension,
) (safety.Suspension, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return safety.Suspension{}, fmt.Errorf("begin suspension: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var projectID *int32
	if s.Target.Scope == safety.ScopeProject || s.Target.Scope == safety.ScopeAgent {
		id := int32(s.Target.ProjectID)
		projectID = &id
	}
	var key *string
	if k := s.Target.Key(); k != "" {
		key = &k
	}
	row := tx.QueryRow(ctx, `
INSERT INTO elitea_runtime.execution_suspensions
    (scope, project_id, scope_key, reason, actor_id, source, resume_requires)
VALUES ($1, $2, $3, $4, $5, $6, $7)
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
	var projectID *int32
	if filter.ProjectID > 0 {
		id := int32(filter.ProjectID)
		projectID = &id
	}
	rows, err := r.pool.Query(ctx, `
SELECT `+suspensionColumns+` FROM elitea_runtime.execution_suspensions
WHERE ($1::boolean IS FALSE OR resumed_at IS NULL)
  AND ($2::integer IS NULL OR project_id = $2)
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
