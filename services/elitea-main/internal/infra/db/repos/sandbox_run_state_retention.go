package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SandboxRunState prunes the per-run sandbox rows the Rust worker keeps in the
// AgentState database: the dispatch journal (elitea_runtime.sandbox_dispatches,
// agentstate 0007) and the failed Code node's output
// (elitea_runtime.sandbox_run_failure_details, agentstate 0016). Neither is
// read once its run is settled, and nothing else removed them (#2241).
//
// It is the run_steps retention sweep's sandbox side
// (runevidence.SandboxStateStore): the sweep passes each project's
// step_record cutoff, the run retention window.
type SandboxRunState struct{ pool *pgxpool.Pool }

// NewSandboxRunState returns a store over the AgentState pool, or nil when
// pool is nil.
func NewSandboxRunState(pool *pgxpool.Pool) *SandboxRunState {
	if pool == nil {
		return nil
	}
	return &SandboxRunState{pool: pool}
}

// PurgeSettledRuns deletes, in one transaction, the dispatch and failure-detail
// rows of up to limit runs of the project whose latest sandbox activity (a
// dispatch written or a failure detail recorded, any generation) is before
// cutoff. A run with any newer row keeps all its rows, so a resumed run is
// never cut in half. It returns how many runs it cleared.
//
// The AgentState database holds no run status of its own; the window is the
// settlement guarantee: a run idle in the sandbox for the whole retention
// window is not executing.
func (s *SandboxRunState) PurgeSettledRuns(ctx context.Context, projectID int64, cutoff time.Time, limit int) (int, error) {
	if limit < 1 {
		return 0, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("sandbox run state: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, `
SELECT tenant_id, execution_id FROM (
    SELECT tenant_id, execution_id, created_at AS at
      FROM elitea_runtime.sandbox_dispatches WHERE project_id = $1
    UNION ALL
    SELECT tenant_id, execution_id, recorded_at AS at
      FROM elitea_runtime.sandbox_run_failure_details WHERE project_id = $1
) activity
 GROUP BY tenant_id, execution_id
HAVING max(at) < $2
 ORDER BY max(at)
 LIMIT $3`, projectID, cutoff.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("sandbox run state: select settled runs: %w", err)
	}
	var tenants, executions []string
	for rows.Next() {
		var tenant, execution string
		if err := rows.Scan(&tenant, &execution); err != nil {
			rows.Close()
			return 0, fmt.Errorf("sandbox run state: scan: %w", err)
		}
		tenants = append(tenants, tenant)
		executions = append(executions, execution)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("sandbox run state: select settled runs: %w", err)
	}
	if len(executions) == 0 {
		return 0, nil
	}
	// A row written after the select (a resumed run) is newer than cutoff and
	// stays. Table and column names are constants, never input.
	for _, target := range [][2]string{{"sandbox_dispatches", "created_at"}, {"sandbox_run_failure_details", "recorded_at"}} {
		if _, err := tx.Exec(ctx, `
DELETE FROM elitea_runtime.`+target[0]+` t
 USING unnest($2::text[], $3::text[]) AS r(tenant_id, execution_id)
 WHERE t.project_id = $1 AND t.tenant_id = r.tenant_id AND t.execution_id = r.execution_id
   AND t.`+target[1]+` < $4`,
			projectID, tenants, executions, cutoff.UTC()); err != nil {
			return 0, fmt.Errorf("sandbox run state: delete %s: %w", target[0], err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("sandbox run state: commit: %w", err)
	}
	return len(executions), nil
}
