package repos

import (
	"context"
	"errors"
	"fmt"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CurrentIndexManualStopCleanupRepository owns only PostgreSQL cleanup intent
// state. The immutable project/toolkit/index/generation binding remains in the
// admitted execution and is loaded separately immediately before PgVector I/O.
type CurrentIndexManualStopCleanupRepository struct {
	store sqlExecutor
}

func NewCurrentIndexManualStopCleanupRepository(
	pool *pgxpool.Pool,
) (*CurrentIndexManualStopCleanupRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return &CurrentIndexManualStopCleanupRepository{store: store}, nil
}

func newCurrentIndexManualStopCleanupRepository(
	store sqlExecutor,
) (*CurrentIndexManualStopCleanupRepository, error) {
	if store == nil {
		return nil, errors.New("current index manual Stop cleanup database is required")
	}
	return &CurrentIndexManualStopCleanupRepository{store: store}, nil
}

// ClaimPendingManualStopCleanups selects only executions that have durably
// settled CANCELLED and whose cancelled index_meta projection is already
// resolved. This prevents cleanup from racing a still-running SDK writer.
func (r *CurrentIndexManualStopCleanupRepository) ClaimPendingManualStopCleanups(
	ctx context.Context,
	claimToken string,
	limit int,
	lease time.Duration,
) ([]indexingapp.CurrentManualStopCleanupClaim, error) {
	if r == nil || r.store == nil || ctx == nil ||
		!validTerminalClaimToken(claimToken) ||
		limit <= 0 || limit > executionapp.MaxOutboxPublisherBatchSize ||
		lease <= 0 || lease > 10*time.Minute {
		return nil, executionapp.ErrInvalidPendingOutboxLimit
	}
	rows, err := r.store.Query(ctx, `
WITH candidates AS (
    SELECT i.execution_id,
           i.generation
    FROM elitea_runtime.index_ingest_jobs AS i
    JOIN elitea_runtime.execution_jobs AS j
      ON j.execution_id = i.execution_id
     AND j.generation = i.generation
     AND j.capability_id = i.capability_id
    WHERE i.capability_id = 'index.ingest.v1'
      AND i.index_manual_cleanup_status = 'PENDING'
      AND i.index_manual_cleanup_next_attempt_at <= clock_timestamp()
      AND (
          i.index_manual_cleanup_claim_expires_at IS NULL
          OR i.index_manual_cleanup_claim_expires_at <= clock_timestamp()
      )
      AND j.desired_state = 'CANCELLED'
      AND j.state = 'CANCELLED'
      AND j.settled_at IS NOT NULL
      AND i.index_meta_terminal_state = 'cancelled'
      AND i.index_meta_terminal_status IN ('APPLIED', 'SUPERSEDED')
    ORDER BY i.index_manual_cleanup_next_attempt_at,
             i.execution_id,
             i.generation
    LIMIT $1
    FOR UPDATE OF i SKIP LOCKED
),
claimed AS (
    UPDATE elitea_runtime.index_ingest_jobs AS i
    SET index_manual_cleanup_claim_token = $2,
        index_manual_cleanup_claim_expires_at =
            clock_timestamp() + ($3::bigint * interval '1 microsecond'),
        index_manual_cleanup_attempt_count =
            LEAST(
                COALESCE(i.index_manual_cleanup_attempt_count, 0)::bigint + 1,
                2147483647
            )::integer,
        index_manual_cleanup_last_error_code = NULL
    FROM candidates
    WHERE i.execution_id = candidates.execution_id
      AND i.generation = candidates.generation
    RETURNING i.execution_id,
              i.generation
)
SELECT execution_id,
       generation
FROM claimed
ORDER BY execution_id,
         generation`,
		int32(limit),
		claimToken,
		lease.Microseconds(),
	)
	if err != nil {
		return nil, fmt.Errorf("claim current index manual Stop cleanups: %w", err)
	}
	defer rows.Close()

	claims := make([]indexingapp.CurrentManualStopCleanupClaim, 0, limit)
	for rows.Next() {
		var claim indexingapp.CurrentManualStopCleanupClaim
		var generation int64
		if err := rows.Scan(&claim.ExecutionID, &generation); err != nil {
			return nil, fmt.Errorf("scan current index manual Stop cleanup: %w", err)
		}
		if generation <= 0 {
			return nil, indexingapp.ErrCurrentIndexMetaInitializationInvalid
		}
		claim.Generation = uint64(generation)
		claim.ClaimToken = claimToken
		if err := claim.Validate(); err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate current index manual Stop cleanups: %w", err)
	}
	return claims, nil
}

// SupersedeManualStopCleanupIfNewerInitialized fences a cleanup before any
// external write when PostgreSQL already owns a newer logical index generation
// or the prerequisite terminal projection was itself superseded.
func (r *CurrentIndexManualStopCleanupRepository) SupersedeManualStopCleanupIfNewerInitialized(
	ctx context.Context,
	claim indexingapp.CurrentManualStopCleanupClaim,
) (bool, error) {
	if r == nil || r.store == nil || ctx == nil || claim.Validate() != nil {
		return false, indexingapp.ErrCurrentIndexMetaInitializationInvalid
	}
	var owned, superseded bool
	err := r.store.QueryRow(ctx, `
WITH stale AS MATERIALIZED (
    SELECT i.execution_id,
           i.generation,
           i.toolkit_id,
           i.index_name,
           i.index_generation,
           i.index_meta_terminal_status,
           j.resource_project_id,
           j.admitted_at
    FROM elitea_runtime.index_ingest_jobs AS i
    JOIN elitea_runtime.execution_jobs AS j
      ON j.execution_id = i.execution_id
     AND j.generation = i.generation
     AND j.capability_id = i.capability_id
    WHERE i.execution_id = $1
      AND i.generation = $2
      AND i.capability_id = 'index.ingest.v1'
      AND i.index_manual_cleanup_status = 'PENDING'
      AND i.index_manual_cleanup_claim_token = $3
),
resolved AS (
    UPDATE elitea_runtime.index_ingest_jobs AS i
    SET index_manual_cleanup_status = 'SUPERSEDED',
        index_manual_cleanup_claim_token = NULL,
        index_manual_cleanup_claim_expires_at = NULL,
        index_manual_cleanup_next_attempt_at = NULL,
        index_manual_cleanup_last_error_code = NULL,
        index_manual_cleanup_resolved_at =
            COALESCE(i.index_manual_cleanup_resolved_at, clock_timestamp())
    FROM stale
    WHERE i.execution_id = stale.execution_id
      AND i.generation = stale.generation
      AND (
          stale.index_meta_terminal_status = 'SUPERSEDED'
          OR EXISTS (
              SELECT 1
              FROM elitea_runtime.execution_jobs AS newer_job
              JOIN elitea_runtime.index_ingest_jobs AS newer_index
                ON newer_index.execution_id = newer_job.execution_id
               AND newer_index.generation = newer_job.generation
               AND newer_index.capability_id = newer_job.capability_id
              WHERE newer_job.resource_project_id = stale.resource_project_id
                AND newer_job.capability_id = 'index.ingest.v1'
                AND newer_index.toolkit_id = stale.toolkit_id
                AND newer_index.index_name = stale.index_name
                AND newer_index.index_meta_initialized_at IS NOT NULL
                AND (
                    newer_index.index_generation > stale.index_generation
                    OR (
                        newer_index.index_generation = stale.index_generation
                        AND (newer_job.admitted_at, newer_job.execution_id)
                            > (stale.admitted_at, stale.execution_id)
                    )
                )
          )
      )
    RETURNING 1
)
SELECT EXISTS (SELECT 1 FROM stale),
       EXISTS (SELECT 1 FROM resolved)`,
		claim.ExecutionID,
		int64(claim.Generation),
		claim.ClaimToken,
	).Scan(&owned, &superseded)
	if err != nil {
		return false, fmt.Errorf("supersede current index manual Stop cleanup: %w", err)
	}
	if !owned {
		return false, indexingapp.ErrCurrentIndexMetaConflict
	}
	return superseded, nil
}

func (r *CurrentIndexManualStopCleanupRepository) ResolveManualStopCleanup(
	ctx context.Context,
	claim indexingapp.CurrentManualStopCleanupClaim,
	resolution indexingapp.CurrentManualStopCleanupResolution,
) error {
	if r == nil || r.store == nil || ctx == nil || claim.Validate() != nil ||
		(resolution != indexingapp.CurrentManualStopCleanupApplied &&
			resolution != indexingapp.CurrentManualStopCleanupSuperseded) {
		return indexingapp.ErrCurrentIndexMetaInitializationInvalid
	}
	var status string
	err := r.store.QueryRow(ctx, `
UPDATE elitea_runtime.index_ingest_jobs
SET index_manual_cleanup_status = $4,
    index_manual_cleanup_claim_token = NULL,
    index_manual_cleanup_claim_expires_at = NULL,
    index_manual_cleanup_next_attempt_at = NULL,
    index_manual_cleanup_last_error_code = NULL,
    index_manual_cleanup_resolved_at =
        COALESCE(index_manual_cleanup_resolved_at, clock_timestamp())
WHERE execution_id = $1
  AND generation = $2
  AND capability_id = 'index.ingest.v1'
  AND index_manual_cleanup_status = 'PENDING'
  AND index_manual_cleanup_claim_token = $3
RETURNING index_manual_cleanup_status`,
		claim.ExecutionID,
		int64(claim.Generation),
		claim.ClaimToken,
		string(resolution),
	).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return indexingapp.ErrCurrentIndexMetaConflict
	}
	if err != nil {
		return fmt.Errorf("resolve current index manual Stop cleanup: %w", err)
	}
	if status != string(resolution) {
		return indexingapp.ErrCurrentIndexMetaConflict
	}
	return nil
}

func (r *CurrentIndexManualStopCleanupRepository) ReleaseManualStopCleanup(
	ctx context.Context,
	claim indexingapp.CurrentManualStopCleanupClaim,
	errorCode string,
) error {
	if r == nil || r.store == nil || ctx == nil || claim.Validate() != nil ||
		!validTerminalErrorCode(errorCode) {
		return indexingapp.ErrCurrentIndexMetaInitializationInvalid
	}
	tag, err := r.store.Exec(ctx, `
UPDATE elitea_runtime.index_ingest_jobs
SET index_manual_cleanup_claim_token = NULL,
    index_manual_cleanup_claim_expires_at = NULL,
    index_manual_cleanup_next_attempt_at =
        clock_timestamp()
        + (
            LEAST(
                30,
                (1::bigint << LEAST(index_manual_cleanup_attempt_count, 5))
            ) * interval '1 second'
        ),
    index_manual_cleanup_last_error_code = $4
WHERE execution_id = $1
  AND generation = $2
  AND capability_id = 'index.ingest.v1'
  AND index_manual_cleanup_status = 'PENDING'
  AND index_manual_cleanup_claim_token = $3`,
		claim.ExecutionID,
		int64(claim.Generation),
		claim.ClaimToken,
		errorCode,
	)
	if err != nil {
		return fmt.Errorf("release current index manual Stop cleanup: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return indexingapp.ErrCurrentIndexMetaConflict
	}
	return nil
}
