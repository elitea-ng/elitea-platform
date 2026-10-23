package repos

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

// CurrentIndexMetaTerminalBindingsRepository loads only immutable admission
// evidence. It never resolves a mutable toolkit/configuration record.
type CurrentIndexMetaTerminalBindingsRepository struct {
	store sqlExecutor
}

func NewCurrentIndexMetaTerminalBindingsRepository(
	pool *pgxpool.Pool,
) (*CurrentIndexMetaTerminalBindingsRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return &CurrentIndexMetaTerminalBindingsRepository{store: store}, nil
}

func newCurrentIndexMetaTerminalBindingsRepository(
	store sqlExecutor,
) (*CurrentIndexMetaTerminalBindingsRepository, error) {
	if store == nil {
		return nil, errors.New("current index metadata terminal binding database is required")
	}
	return &CurrentIndexMetaTerminalBindingsRepository{store: store}, nil
}

func (r *CurrentIndexMetaTerminalBindingsRepository) LoadCurrentIndexMetaTerminalBinding(
	ctx context.Context,
	executionID string,
	generation uint64,
) (indexingapp.CurrentIndexMetaTerminalBinding, error) {
	if r == nil || r.store == nil || ctx == nil || executionID == "" ||
		generation == 0 || generation > math.MaxInt64 {
		return indexingapp.CurrentIndexMetaTerminalBinding{}, indexingapp.ErrCurrentIndexMetaInitializationInvalid
	}
	var binding indexingapp.CurrentIndexMetaTerminalBinding
	var actorID string
	var storedGeneration int64
	var storedIndexGeneration int64
	err := r.store.QueryRow(ctx, `
SELECT j.resource_project_id,
       j.actor_id,
       i.toolkit_id,
       i.index_name,
       i.index_meta_id,
       j.execution_id,
       j.generation,
       i.index_generation,
       e.content_bytes
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.index_ingest_jobs AS i
  ON i.execution_id = j.execution_id
 AND i.generation = j.generation
 AND i.capability_id = j.capability_id
JOIN elitea_runtime.input_bundle_entries AS e
  ON e.input_bundle_id = i.input_bundle_id
 AND e.entry_id = i.toolkit_configuration_entry_id
WHERE j.execution_id = $1
  AND j.generation = $2
  AND j.capability_id = 'index.ingest.v1'
  AND i.index_meta_initialized_at IS NOT NULL`,
		executionID,
		int64(generation),
	).Scan(
		&binding.ResourceProjectID,
		&actorID,
		&binding.ToolkitID,
		&binding.IndexName,
		&binding.MetaID,
		&binding.ExecutionID,
		&storedGeneration,
		&storedIndexGeneration,
		&binding.ToolkitConfiguration,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return indexingapp.CurrentIndexMetaTerminalBinding{}, indexingapp.ErrCurrentIndexMetaConflict
	}
	if err != nil {
		return indexingapp.CurrentIndexMetaTerminalBinding{}, fmt.Errorf("load current index metadata terminal binding: %w", err)
	}
	parsedActorID, err := strconv.ParseInt(actorID, 10, 32)
	if err != nil || parsedActorID <= 0 || storedGeneration <= 0 || storedIndexGeneration <= 0 {
		return indexingapp.CurrentIndexMetaTerminalBinding{}, indexingapp.ErrCurrentIndexMetaInitializationInvalid
	}
	binding.ActorUserID = int32(parsedActorID)
	binding.Generation = uint64(storedGeneration)
	binding.IndexGeneration = uint64(storedIndexGeneration)
	if err := binding.Validate(); err != nil ||
		binding.ExecutionID != executionID ||
		binding.Generation != generation {
		return indexingapp.CurrentIndexMetaTerminalBinding{}, indexingapp.ErrCurrentIndexMetaInitializationInvalid
	}
	return binding, nil
}

func (r *CurrentIndexMetaTerminalBindingsRepository) ClaimPendingTerminalEffects(
	ctx context.Context,
	claimToken string,
	limit int,
	lease time.Duration,
) ([]indexingapp.CurrentIndexMetaTerminalClaim, error) {
	if r == nil || r.store == nil || ctx == nil || !validTerminalClaimToken(claimToken) ||
		limit <= 0 || limit > executionapp.MaxOutboxPublisherBatchSize ||
		lease <= 0 || lease > 10*time.Minute {
		return nil, executionapp.ErrInvalidPendingOutboxLimit
	}
	rows, err := r.store.Query(ctx, `
WITH candidates AS (
    SELECT i.execution_id,
           i.generation,
           i.index_meta_terminal_state,
           i.index_meta_terminal_occurred_at
    FROM elitea_runtime.index_ingest_jobs AS i
    WHERE i.capability_id = 'index.ingest.v1'
      AND i.index_meta_initialized_at IS NOT NULL
      AND i.index_meta_terminal_status = 'PENDING'
      AND i.index_meta_terminal_next_attempt_at <= clock_timestamp()
      AND (
          i.index_meta_terminal_claim_expires_at IS NULL
          OR i.index_meta_terminal_claim_expires_at <= clock_timestamp()
      )
    ORDER BY i.index_meta_terminal_next_attempt_at,
             i.execution_id,
             i.generation
    LIMIT $1
    FOR UPDATE OF i SKIP LOCKED
),
claimed AS (
    UPDATE elitea_runtime.index_ingest_jobs AS i
    SET index_meta_terminal_claim_token = $2,
        index_meta_terminal_claim_expires_at =
            clock_timestamp() + ($3::bigint * interval '1 microsecond'),
        index_meta_terminal_attempt_count =
            LEAST(
                COALESCE(i.index_meta_terminal_attempt_count, 0)::bigint + 1,
                2147483647
            )::integer,
        index_meta_terminal_last_error_code = NULL
    FROM candidates
    WHERE i.execution_id = candidates.execution_id
      AND i.generation = candidates.generation
    RETURNING i.execution_id,
              i.generation,
              i.index_meta_terminal_state,
              i.index_meta_terminal_occurred_at
)
SELECT claimed.execution_id,
       claimed.generation,
       claimed.index_meta_terminal_state,
       claimed.index_meta_terminal_occurred_at
FROM claimed
ORDER BY claimed.index_meta_terminal_occurred_at,
         claimed.execution_id,
         claimed.generation`,
		int32(limit),
		claimToken,
		lease.Microseconds(),
	)
	if err != nil {
		return nil, fmt.Errorf("claim pending current index metadata terminal effects: %w", err)
	}
	defer rows.Close()

	claims := make([]indexingapp.CurrentIndexMetaTerminalClaim, 0, limit)
	for rows.Next() {
		var claim indexingapp.CurrentIndexMetaTerminalClaim
		var generation int64
		var state string
		if err := rows.Scan(
			&claim.ExecutionID,
			&generation,
			&state,
			&claim.OccurredAt,
		); err != nil {
			return nil, fmt.Errorf("scan pending current index metadata terminal effect: %w", err)
		}
		if generation <= 0 {
			return nil, indexingapp.ErrCurrentIndexMetaInitializationInvalid
		}
		claim.Generation = uint64(generation)
		claim.State = indexingapp.CurrentIndexMetaTerminalState(state)
		claim.OccurredAt = claim.OccurredAt.UTC()
		claim.ClaimToken = claimToken
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending current index metadata terminal effects: %w", err)
	}
	rows.Close()
	for index := range claims {
		if claims[index].State == indexingapp.CurrentIndexMetaFailed {
			safeError, err := r.loadCurrentIndexMetaTerminalSafeError(ctx, claims[index])
			if err != nil {
				return nil, err
			}
			claims[index].SafeError = safeError
		}
		if err := claims[index].Validate(); err != nil {
			return nil, err
		}
	}
	return claims, nil
}

func (r *CurrentIndexMetaTerminalBindingsRepository) loadCurrentIndexMetaTerminalSafeError(
	ctx context.Context,
	claim indexingapp.CurrentIndexMetaTerminalClaim,
) (string, error) {
	var payloadType string
	var payloadBytes []byte
	var retirementCode string
	err := r.store.QueryRow(ctx, `
SELECT source.payload_type,
       source.payload_bytes,
       source.retirement_code
FROM (
    SELECT o.payload_type,
           o.payload_bytes,
           ''::text AS retirement_code,
           1 AS source_priority
    FROM elitea_runtime.output_inbox AS o
    WHERE o.execution_id = $1
      AND o.generation = $2
      AND o.occurred_at = $3
      AND o.projected_at IS NOT NULL
      AND o.payload_type IN ('RUNTIME_FAILURE', 'INDEX_INGEST_RESULT')
      AND o.settlement_outcome = 'FAILED'
    UNION ALL
    SELECT ''::text,
           NULL::bytea,
           o.retirement_code,
           2
    FROM elitea_runtime.command_outbox AS o
    WHERE o.execution_id = $1
      AND o.generation = $2
      AND o.retired_at = $3
      AND o.authority_granted_at IS NULL
      AND o.retirement_code = 'DEADLINE_EXCEEDED'
) AS source
ORDER BY source.source_priority
LIMIT 1`,
		claim.ExecutionID,
		int64(claim.Generation),
		claim.OccurredAt.UTC(),
	).Scan(&payloadType, &payloadBytes, &retirementCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", indexingapp.ErrCurrentIndexMetaConflict
	}
	if err != nil {
		return "", fmt.Errorf("load current index metadata terminal safe error: %w", err)
	}
	return terminalSafeError(payloadType, payloadBytes, retirementCode), nil
}

// SupersedeTerminalEffectIfNewerInitialized atomically resolves a claimed
// terminal intent only when PostgreSQL already contains a later initialized
// execution for the same project/toolkit/index identity. The database admission
// logical index generation is the primary order. The admission timestamp and
// execution ID break equal-generation ties left by the legacy migration.
func (r *CurrentIndexMetaTerminalBindingsRepository) SupersedeTerminalEffectIfNewerInitialized(
	ctx context.Context,
	claim indexingapp.CurrentIndexMetaTerminalClaim,
) (bool, error) {
	if r == nil || r.store == nil || ctx == nil {
		return false, indexingapp.ErrCurrentIndexMetaInitializationInvalid
	}
	if err := claim.Validate(); err != nil {
		return false, err
	}
	var owned, superseded bool
	err := r.store.QueryRow(ctx, `
WITH stale AS MATERIALIZED (
    SELECT i.execution_id,
           i.generation,
           i.toolkit_id,
           i.index_name,
           i.index_generation,
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
      AND i.index_meta_initialized_at IS NOT NULL
      AND i.index_meta_terminal_state = $3
      AND i.index_meta_terminal_occurred_at = $4
      AND i.index_meta_terminal_status = 'PENDING'
      AND i.index_meta_terminal_claim_token = $5
),
resolved AS (
UPDATE elitea_runtime.index_ingest_jobs AS i
SET index_meta_terminal_status = 'SUPERSEDED',
    index_meta_terminal_claim_token = NULL,
    index_meta_terminal_claim_expires_at = NULL,
    index_meta_terminal_next_attempt_at = NULL,
    index_meta_terminal_last_error_code = NULL,
    index_meta_terminalized_at = COALESCE(i.index_meta_terminalized_at, clock_timestamp())
FROM stale
WHERE i.execution_id = stale.execution_id
  AND i.generation = stale.generation
  AND EXISTS (
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
RETURNING 1
)
SELECT EXISTS (SELECT 1 FROM stale),
       EXISTS (SELECT 1 FROM resolved)`,
		claim.ExecutionID,
		int64(claim.Generation),
		string(claim.State),
		claim.OccurredAt.UTC(),
		claim.ClaimToken,
	).Scan(&owned, &superseded)
	if err != nil {
		return false, fmt.Errorf("supersede current index metadata terminal effect: %w", err)
	}
	if !owned {
		return false, indexingapp.ErrCurrentIndexMetaConflict
	}
	return superseded, nil
}

func (r *CurrentIndexMetaTerminalBindingsRepository) ResolveTerminalEffect(
	ctx context.Context,
	claim indexingapp.CurrentIndexMetaTerminalClaim,
	resolution indexingapp.CurrentIndexMetaTerminalResolution,
) error {
	if r == nil || r.store == nil || ctx == nil {
		return indexingapp.ErrCurrentIndexMetaInitializationInvalid
	}
	if err := claim.Validate(); err != nil {
		return err
	}
	if resolution != indexingapp.CurrentIndexMetaTerminalApplied &&
		resolution != indexingapp.CurrentIndexMetaTerminalSuperseded {
		return indexingapp.ErrCurrentIndexMetaInitializationInvalid
	}
	var status string
	err := r.store.QueryRow(ctx, `
UPDATE elitea_runtime.index_ingest_jobs AS i
SET index_meta_terminal_status = $5,
    index_meta_terminal_claim_token = NULL,
    index_meta_terminal_claim_expires_at = NULL,
    index_meta_terminal_next_attempt_at = NULL,
    index_meta_terminal_last_error_code = NULL,
    index_meta_terminalized_at = COALESCE(i.index_meta_terminalized_at, clock_timestamp())
WHERE i.execution_id = $1
  AND i.generation = $2
  AND i.capability_id = 'index.ingest.v1'
  AND i.index_meta_initialized_at IS NOT NULL
  AND i.index_meta_terminal_state = $3
  AND i.index_meta_terminal_occurred_at = $4
  AND i.index_meta_terminal_status = 'PENDING'
  AND i.index_meta_terminal_claim_token = $6
RETURNING i.index_meta_terminal_status`,
		claim.ExecutionID,
		int64(claim.Generation),
		string(claim.State),
		claim.OccurredAt.UTC(),
		string(resolution),
		claim.ClaimToken,
	).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return indexingapp.ErrCurrentIndexMetaConflict
	}
	if err != nil {
		return fmt.Errorf("resolve current index metadata terminal effect: %w", err)
	}
	if status != string(resolution) {
		return indexingapp.ErrCurrentIndexMetaConflict
	}
	return nil
}

func (r *CurrentIndexMetaTerminalBindingsRepository) ReleaseTerminalEffect(
	ctx context.Context,
	claim indexingapp.CurrentIndexMetaTerminalClaim,
	errorCode string,
) error {
	if r == nil || r.store == nil || ctx == nil || claim.Validate() != nil ||
		!validTerminalErrorCode(errorCode) {
		return indexingapp.ErrCurrentIndexMetaInitializationInvalid
	}
	tag, err := r.store.Exec(ctx, `
UPDATE elitea_runtime.index_ingest_jobs
SET index_meta_terminal_claim_token = NULL,
    index_meta_terminal_claim_expires_at = NULL,
    index_meta_terminal_next_attempt_at =
        clock_timestamp()
        + (
            LEAST(
                30,
                (1::bigint << LEAST(index_meta_terminal_attempt_count, 5))
            ) * interval '1 second'
        ),
    index_meta_terminal_last_error_code = $4
WHERE execution_id = $1
  AND generation = $2
  AND capability_id = 'index.ingest.v1'
  AND index_meta_terminal_status = 'PENDING'
  AND index_meta_terminal_claim_token = $3`,
		claim.ExecutionID,
		int64(claim.Generation),
		claim.ClaimToken,
		errorCode,
	)
	if err != nil {
		return fmt.Errorf("release current index metadata terminal effect: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return indexingapp.ErrCurrentIndexMetaConflict
	}
	return nil
}

func terminalSafeError(payloadType string, payloadBytes []byte, retirementCode string) string {
	if retirementCode == "DEADLINE_EXCEEDED" {
		return "The execution deadline was exceeded before worker authority was granted."
	}
	switch payloadType {
	case payloadTypeRuntimeFailure:
		var failure runtimev1.RuntimeErrorV1
		if len(payloadBytes) != 0 && proto.Unmarshal(payloadBytes, &failure) == nil &&
			failure.GetSafeMessage() != "" {
			return failure.GetSafeMessage()
		}
	case payloadTypeIndexIngestResult:
		var result runtimev1.IndexIngestResultV1
		if len(payloadBytes) != 0 && proto.Unmarshal(payloadBytes, &result) == nil &&
			result.GetResultSummary().GetStatus() ==
				runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_ERROR &&
			result.GetResultSummary().GetMessage() != "" {
			return result.GetResultSummary().GetMessage()
		}
	}
	return "Indexing failed before completion."
}

func validTerminalClaimToken(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\x00\r\n")
}

func validTerminalErrorCode(value string) bool {
	return value != "" && len(value) <= 64 && !strings.ContainsAny(value, "\x00\r\n")
}

var _ indexingapp.CurrentIndexMetaTerminalBindingRepository = (*CurrentIndexMetaTerminalBindingsRepository)(nil)
