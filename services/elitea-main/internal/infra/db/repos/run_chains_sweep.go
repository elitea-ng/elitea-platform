package repos

// The chain sweep (ADR-0045 decision 4): Main ends a chain that nothing
// else will end. A cancelled local turn commits nothing, so its
// chain_outcome never reaches Main; a continuation can fail to start for a
// reason other than a limit.

import (
	"context"
	"fmt"
	"time"
)

// SweptChain is one chain the sweep ended.
type SweptChain struct {
	RootExecutionID string
	EndState        string
}

// sweepRunChainsSQL ends at most $2 open chains:
//
//   - the deadline passed: limit_reached (deadline);
//   - the latest local turn expired uncommitted more than $1 ago:
//     cancelled;
//   - the latest local turn committed, or the latest cloud run settled, more
//     than $1 ago and no continuation started: failed.
//
// The UPDATE re-checks end_state and last_execution_id on the current row
// version (READ COMMITTED). A continuation that commits first moves
// last_execution_id, so the sweep skips the chain; a sweep that commits
// first sets end_state, so the continuation's chain check refuses it.
const sweepRunChainsSQL = `
WITH candidates AS (
    SELECT c.root_execution_id, c.last_execution_id,
           CASE
               WHEN c.deadline <= clock_timestamp() THEN 'limit_reached'
               WHEN t.execution_id IS NOT NULL AND t.committed_at IS NULL THEN 'cancelled'
               ELSE 'failed'
           END AS end_state
    FROM elitea_runtime.run_chains AS c
    LEFT JOIN elitea_runtime.local_turn_executions AS t
           ON c.origin = 'desktop' AND t.execution_id = c.last_execution_id
    WHERE c.end_state IS NULL
      AND (c.deadline <= clock_timestamp()
           OR (t.execution_id IS NOT NULL
               AND COALESCE(t.committed_at, t.expires_at) <= clock_timestamp() - $1::interval)
           OR (c.origin = 'cloud' AND EXISTS (
                   SELECT 1 FROM elitea_runtime.execution_jobs AS j
                   WHERE j.execution_id = c.last_execution_id
                     AND j.settled_at IS NOT NULL
                     AND j.settled_at <= clock_timestamp() - $1::interval)))
    ORDER BY c.updated_at
    LIMIT $2
)
UPDATE elitea_runtime.run_chains AS c
SET end_state = k.end_state,
    end_limit = CASE WHEN k.end_state = 'limit_reached' THEN 'deadline' END,
    ended_at = clock_timestamp(),
    updated_at = clock_timestamp()
FROM candidates AS k
WHERE c.root_execution_id = k.root_execution_id
  AND c.end_state IS NULL
  AND c.last_execution_id = k.last_execution_id
RETURNING c.root_execution_id, c.end_state`

// SweepChains ends at most limit chains that nothing else will end. quiet
// is how long a chain waits for a continuation after its latest run ended.
// It is idempotent: an ended chain is never selected again.
func (r *RunChainsRepository) SweepChains(ctx context.Context, quiet time.Duration, limit int) ([]SweptChain, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, sweepRunChainsSQL,
		fmt.Sprintf("%d milliseconds", quiet.Milliseconds()), limit)
	if err != nil {
		if undefinedColumnOrTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("unattended chain: sweep: %w", err)
	}
	defer rows.Close()
	var swept []SweptChain
	for rows.Next() {
		var chain SweptChain
		if err := rows.Scan(&chain.RootExecutionID, &chain.EndState); err != nil {
			return nil, fmt.Errorf("unattended chain: sweep: %w", err)
		}
		swept = append(swept, chain)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("unattended chain: sweep: %w", err)
	}
	return swept, nil
}
