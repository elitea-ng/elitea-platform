package repos

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RunWallCapRepository cancels an agent run past the whole-run wall cap
// (ADR-0041 decision 3, run_caps.max_wall_seconds; #1496, #1498).
//
// The cancel is the same durable primitive as every other stop
// (execution_suspensions.go): desired_state CANCELLED, which the worker
// observes on the lease renewal that triggered it, and settlement writes
// the terminal state. It also records cancel_reason cap_reached (shared
// 0201), which the projection turns into the typed CAP_REACHED outcome.
//
// The clock is the run's ORIGINAL start: run_started_at, which a
// continuation copies from the execution it continues, else admitted_at.
// Both are database clock values, and so is the comparison.
type RunWallCapRepository struct {
	pool *pgxpool.Pool
}

// NewRunWallCapRepository builds the repository.
func NewRunWallCapRepository(pool *pgxpool.Pool) (*RunWallCapRepository, error) {
	if pool == nil {
		return nil, errors.New("run wall cap database is required")
	}
	return &RunWallCapRepository{pool: pool}, nil
}

// CapReasonWallCap is execution_jobs.cancel_reason for the wall cap.
const CapReasonWallCap = "cap_reached"

// enforceWallCapSQL cancels one running agent generation whose run started
// at least $3 milliseconds ago. A run another stop already cancelled keeps
// that stop: desired_state must still be RUNNING.
const enforceWallCapSQL = `
UPDATE elitea_runtime.execution_jobs AS j
SET desired_state = 'CANCELLED', cancel_reason = 'cap_reached'
WHERE j.execution_id = $1
  AND j.generation = $2
  AND j.desired_state = 'RUNNING'
  AND j.capability_id IN ('agent.execute.application.v1', 'agent.execute.adhoc.v1')
  AND j.state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING')
  AND COALESCE(j.run_started_at, j.admitted_at)
      <= clock_timestamp() - ($3::bigint * interval '1 millisecond')`

// EnforceWallCap cancels the run when it is past limit and reports whether
// this call cancelled it. A limit of 0 or less is off.
func (r *RunWallCapRepository) EnforceWallCap(
	ctx context.Context, executionID string, generation uint64, limit time.Duration,
) (bool, error) {
	if limit <= 0 {
		return false, nil
	}
	if executionID == "" || generation == 0 {
		return false, errors.New("run wall cap: execution identity is required")
	}
	tag, err := r.pool.Exec(ctx, enforceWallCapSQL, executionID, int64(generation), limit.Milliseconds())
	if err != nil {
		return false, fmt.Errorf("enforce the run wall cap: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
