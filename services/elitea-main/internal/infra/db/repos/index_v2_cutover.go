package repos

import (
	"context"
	"errors"
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/cutover"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CurrentIndexV2CutoverRepository reads the authoritative persisted version-1
// state. Terminal jobs and their post-authority outboxes are retained for
// audit. A retained outbox is safe only after an exact committed terminal
// settlement; an unreleased claim always blocks the coordinated cutover.
type CurrentIndexV2CutoverRepository struct {
	query sqlExecutor
}

func NewCurrentIndexV2CutoverRepository(pool *pgxpool.Pool) (*CurrentIndexV2CutoverRepository, error) {
	if pool == nil {
		return nil, errors.New("index v2 cutover PostgreSQL pool is required")
	}
	return newCurrentIndexV2CutoverRepository(pgxExecutor{queryer: pool})
}

func newCurrentIndexV2CutoverRepository(query sqlExecutor) (*CurrentIndexV2CutoverRepository, error) {
	if query == nil {
		return nil, errors.New("index v2 cutover PostgreSQL query executor is required")
	}
	return &CurrentIndexV2CutoverRepository{query: query}, nil
}

func (r *CurrentIndexV2CutoverRepository) ReadIndexV1CutoverState(
	ctx context.Context,
) (cutover.IndexV1PersistedState, error) {
	if ctx == nil {
		return cutover.IndexV1PersistedState{}, errors.New("index v2 cutover context is required")
	}
	if err := ctx.Err(); err != nil {
		return cutover.IndexV1PersistedState{}, err
	}
	var state cutover.IndexV1PersistedState
	err := r.query.QueryRow(ctx, `
WITH index_effects AS (
    SELECT
        count(*) FILTER (
            WHERE ingest.index_meta_initialization_status IN ('PENDING', 'RUNNING')
        ) AS pending_initializations,
        count(*) FILTER (
            WHERE ingest.index_meta_terminal_status = 'PENDING'
        ) AS pending_terminal_projections,
        count(*) FILTER (
            WHERE ingest.index_manual_cleanup_status = 'PENDING'
        ) AS pending_manual_cleanups,
        count(*) FILTER (
            WHERE ingest.index_meta_task_restamp_status = 'PENDING'
        ) AS pending_task_restamps
    FROM elitea_runtime.index_ingest_jobs AS ingest
    JOIN elitea_runtime.execution_jobs AS job
      ON job.execution_id = ingest.execution_id
     AND job.generation = ingest.generation
     AND job.capability_id = ingest.capability_id
    WHERE job.capability_id = 'index.ingest.v1'
      AND job.capability_version = '1'
)
SELECT
    (
        SELECT count(*)
        FROM elitea_runtime.execution_jobs AS job
        WHERE job.capability_id = 'index.ingest.v1'
          AND job.capability_version = '1'
          AND job.state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING')
    ) AS live_jobs,
    (
        SELECT count(*)
        FROM elitea_runtime.command_outbox AS outbox
        JOIN elitea_runtime.execution_jobs AS job
          ON job.execution_id = outbox.execution_id
         AND job.generation = outbox.generation
        WHERE job.capability_id = 'index.ingest.v1'
          AND job.capability_version = '1'
          AND outbox.retired_at IS NULL
          AND NOT (
              outbox.authority_granted_at IS NOT NULL
              AND job.state IN ('SUCCEEDED', 'FAILED', 'CANCELLED')
              AND job.settled_at IS NOT NULL
              AND EXISTS (
                  SELECT 1
                  FROM elitea_runtime.execution_settlements AS settlement
                  WHERE settlement.execution_id = job.execution_id
                    AND settlement.generation = job.generation
                    AND settlement.disposition = job.state
                    AND settlement.committed_at IS NOT NULL
              )
          )
    ) AS outstanding_outbox,
    (
        SELECT count(*)
        FROM elitea_runtime.execution_claims AS claim
        JOIN elitea_runtime.execution_jobs AS job
          ON job.execution_id = claim.execution_id
         AND job.generation = claim.generation
        WHERE job.capability_id = 'index.ingest.v1'
          AND job.capability_version = '1'
          AND claim.released_at IS NULL
    ) AS active_claims,
    index_effects.pending_initializations,
    index_effects.pending_terminal_projections,
    index_effects.pending_manual_cleanups,
    index_effects.pending_task_restamps
FROM index_effects`,
	).Scan(
		&state.LiveJobs,
		&state.OutstandingOutbox,
		&state.ActiveClaims,
		&state.PendingInitializations,
		&state.PendingTerminalProjections,
		&state.PendingManualCleanups,
		&state.PendingTaskRestamps,
	)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return cutover.IndexV1PersistedState{}, contextErr
		}
		return cutover.IndexV1PersistedState{}, fmt.Errorf("query persisted index v1 cutover state: %w", err)
	}
	return state, nil
}

var _ cutover.IndexV1PersistedStateReader = (*CurrentIndexV2CutoverRepository)(nil)
