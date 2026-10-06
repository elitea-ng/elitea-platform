package repos

import (
	"context"
	"errors"
	"fmt"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
)

var ErrPendingIndexIngestDispatchNotFound = errors.New("pending index ingest dispatch not found")

// RetireNoAuthorityIndexIngest terminalizes at most limit cancelled or expired
// index commands that never acquired worker authority. Cancellation consumes
// the bound before deadline retirement, matching the existing runtime policy.
func (r *CommandOutboxRepository) RetireNoAuthorityIndexIngest(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > executionapp.MaxOutboxPublisherBatchSize {
		return 0, executionapp.ErrInvalidPendingOutboxLimit
	}

	retiredCount := 0
	err := r.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		cancelled, err := selectCancelledNoAuthorityIndexIngest(ctx, tx, r.expectedStream, limit)
		if err != nil {
			return err
		}
		for _, candidate := range cancelled {
			retired, err := retireLockedNoAuthorityValidation(ctx, tx, candidate)
			if err != nil {
				return err
			}
			if retired {
				if err := persistCurrentIndexMetaRetirementIntent(ctx, tx, candidate); err != nil {
					return err
				}
				if err := persistCurrentIndexRetirementNotification(
					ctx,
					tx,
					candidate,
				); err != nil {
					return err
				}
				if err := r.activity.projectTerminal(
					ctx,
					tx,
					candidate.ProjectionProjectID,
					currentIndexActivityRetirement(candidate),
				); err != nil {
					return err
				}
				retiredCount++
			}
		}

		remaining := limit - retiredCount
		if remaining == 0 {
			return nil
		}
		expired, err := selectExpiredNoAuthorityIndexIngest(ctx, tx, r.expectedStream, remaining)
		if err != nil {
			return err
		}
		for _, candidate := range expired {
			retired, err := retireLockedNoAuthorityValidation(ctx, tx, candidate)
			if err != nil {
				return err
			}
			if retired {
				if err := persistCurrentIndexMetaRetirementIntent(ctx, tx, candidate); err != nil {
					return err
				}
				if err := persistCurrentIndexRetirementNotification(
					ctx,
					tx,
					candidate,
				); err != nil {
					return err
				}
				if err := r.activity.projectTerminal(
					ctx,
					tx,
					candidate.ProjectionProjectID,
					currentIndexActivityRetirement(candidate),
				); err != nil {
					return err
				}
				retiredCount++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return retiredCount, nil
}

func currentIndexActivityRetirement(
	candidate noAuthorityRetirementCandidate,
) currentIndexActivityTerminal {
	message := "The execution deadline was exceeded before worker authority was granted."
	if candidate.DesiredState == string(runtimedomain.DesiredCancelled) {
		message = "Execution was cancelled."
	}
	return currentIndexActivityTerminal{
		ExecutionID: candidate.ExecutionID,
		Generation:  uint64(candidate.Generation),
		OccurredAt:  time.Now().UTC(),
		Message:     message,
		IsError:     true,
	}
}

// persistCurrentIndexMetaRetirementIntent runs after the durable no-authority
// retirement and its replay event, in the same transaction. It queues only a
// local PostgreSQL marker; the standalone reconciler owns all external work.
func persistCurrentIndexMetaRetirementIntent(
	ctx context.Context,
	tx sqlExecutor,
	candidate noAuthorityRetirementCandidate,
) error {
	retirementCode := retirementCodeDeadlineExceeded
	state := "failed"
	if candidate.DesiredState == string(runtimedomain.DesiredCancelled) {
		retirementCode = retirementCodeCancelled
		state = "cancelled"
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.index_ingest_jobs AS i
SET index_meta_terminal_state = $4,
    index_meta_terminal_occurred_at = o.retired_at,
    index_meta_terminal_status = 'PENDING',
    index_meta_terminal_attempt_count = 0,
    index_meta_terminal_next_attempt_at = clock_timestamp()
FROM elitea_runtime.command_outbox AS o
WHERE o.outbox_id = $1
  AND o.execution_id = $2
  AND o.generation = $3
  AND o.retired_at IS NOT NULL
  AND o.authority_granted_at IS NULL
  AND o.retirement_code = $5
  AND i.execution_id = o.execution_id
  AND i.generation = o.generation
  AND i.capability_id = 'index.ingest.v1'
  AND i.index_meta_initialized_at IS NOT NULL
  AND i.index_meta_terminal_status IS NULL`,
		candidate.OutboxID,
		candidate.ExecutionID,
		candidate.Generation,
		state,
		retirementCode,
	); err != nil {
		return fmt.Errorf("persist current index metadata retirement intent: %w", err)
	}
	return nil
}

func persistCurrentIndexRetirementNotification(
	ctx context.Context,
	tx sqlExecutor,
	candidate noAuthorityRetirementCandidate,
) error {
	if candidate.DesiredState == string(runtimedomain.DesiredCancelled) {
		return nil
	}
	if candidate.Generation <= 0 {
		return errors.New("index retirement generation is invalid")
	}
	return persistCurrentIndexTerminalNotification(
		ctx,
		tx,
		outputRecord{
			ExecutionID:     candidate.ExecutionID,
			Generation:      uint64(candidate.Generation),
			LogicalOutputID: "index-ingest:" + candidate.ExecutionID,
		},
		outputapp.IndexIngestSummary{
			Status:        outputapp.IndexIngestStatusError,
			Message:       "The execution deadline was exceeded before worker authority was granted.",
			TerminalState: outputapp.IndexIngestTerminalFailed,
		},
	)
}

func selectCancelledNoAuthorityIndexIngest(ctx context.Context, tx sqlExecutor, expectedStream string, limit int) ([]noAuthorityRetirementCandidate, error) {
	rows, err := tx.Query(ctx, `
SELECT o.outbox_id, j.execution_id, j.generation,
       j.projection_project_id, j.desired_state
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
WHERE j.desired_state = 'CANCELLED'
  AND j.state IN ('PENDING', 'DISPATCHED')
  AND j.capability_id = 'index.ingest.v1'
  AND j.generation = 1
  AND o.stream_name = $1
  AND o.retired_at IS NULL
  AND o.authority_granted_at IS NULL
  AND NOT EXISTS (
      SELECT 1
      FROM elitea_runtime.execution_claims AS c
      WHERE c.execution_id = j.execution_id AND c.generation = j.generation
  )
ORDER BY j.admitted_at ASC, j.execution_id ASC, j.generation ASC
LIMIT $2
FOR UPDATE OF j, o SKIP LOCKED`, expectedStream, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("select cancelled no-authority index ingest outbox: %w", err)
	}
	return scanNoAuthorityIndexIngestCandidates(rows, limit, "cancelled")
}

func selectExpiredNoAuthorityIndexIngest(ctx context.Context, tx sqlExecutor, expectedStream string, limit int) ([]noAuthorityRetirementCandidate, error) {
	rows, err := tx.Query(ctx, `
SELECT o.outbox_id, j.execution_id, j.generation,
       j.projection_project_id, j.desired_state
FROM elitea_runtime.command_outbox AS o
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = o.execution_id AND j.generation = o.generation
WHERE o.deadline <= clock_timestamp()
  AND o.retired_at IS NULL
  AND o.authority_granted_at IS NULL
  AND o.stream_name = $1
  AND j.desired_state <> 'CANCELLED'
  AND j.state IN ('PENDING', 'DISPATCHED')
  AND j.capability_id = 'index.ingest.v1'
  AND j.generation = 1
  AND NOT EXISTS (
      SELECT 1
      FROM elitea_runtime.execution_claims AS c
      WHERE c.execution_id = o.execution_id AND c.generation = o.generation
  )
ORDER BY o.deadline ASC, o.outbox_id ASC
LIMIT $2
FOR UPDATE OF j, o SKIP LOCKED`, expectedStream, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("select expired no-authority index ingest outbox: %w", err)
	}
	return scanNoAuthorityIndexIngestCandidates(rows, limit, "expired")
}

func scanNoAuthorityIndexIngestCandidates(rows sqlRows, limit int, kind string) ([]noAuthorityRetirementCandidate, error) {
	defer rows.Close()
	candidates := make([]noAuthorityRetirementCandidate, 0, limit)
	for rows.Next() {
		var candidate noAuthorityRetirementCandidate
		if err := rows.Scan(
			&candidate.OutboxID,
			&candidate.ExecutionID,
			&candidate.Generation,
			&candidate.ProjectionProjectID,
			&candidate.DesiredState,
		); err != nil {
			return nil, fmt.Errorf("scan %s no-authority index ingest outbox: %w", kind, err)
		}
		candidates = append(candidates, candidate)
		if len(candidates) > limit {
			return nil, executionapp.ErrPendingOutboxBatchLimitExceeded
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s no-authority index ingest outbox: %w", kind, err)
	}
	return candidates, nil
}

// ListPendingIndexIngestIDs returns only index.ingest.v1 work on this
// repository's dedicated stream. Published rows become visible again only
// after their bounded visibility observation expires.
func (r *CommandOutboxRepository) ListPendingIndexIngestIDs(ctx context.Context, limit int, visibilityTimeout time.Duration) ([]string, error) {
	if limit <= 0 || limit > executionapp.MaxOutboxPublisherBatchSize || visibilityTimeout < executionapp.MinOutboxVisibilityTimeout || visibilityTimeout > executionapp.MaxOutboxVisibilityTimeout {
		return nil, executionapp.ErrInvalidPendingOutboxLimit
	}
	visibilityMillis := visibilityTimeout.Milliseconds()
	rows, err := r.store.Query(ctx, `
SELECT candidate.outbox_id
FROM (
    (
        SELECT o.outbox_id, o.created_at AS visibility_order
        FROM elitea_runtime.command_outbox AS o
        JOIN elitea_runtime.execution_jobs AS j
          ON j.execution_id = o.execution_id AND j.generation = o.generation
        JOIN elitea_runtime.index_ingest_jobs AS i
          ON i.execution_id = j.execution_id
         AND i.generation = j.generation
         AND i.capability_id = j.capability_id
        WHERE o.stream_name = $1
          AND o.published_at IS NULL
          AND o.retired_at IS NULL
          AND o.authority_granted_at IS NULL
          AND o.deadline > statement_timestamp()
          AND j.state = 'PENDING'
          AND j.desired_state = 'RUNNING'
          AND j.capability_id = 'index.ingest.v1'
          AND j.generation = 1
          AND i.index_meta_initialized_at IS NOT NULL
        ORDER BY o.created_at ASC, o.outbox_id ASC
        LIMIT $2
    )
    UNION ALL
    (
        SELECT o.outbox_id,
               COALESCE(o.last_visibility_at, o.published_at) AS visibility_order
        FROM elitea_runtime.command_outbox AS o
        JOIN elitea_runtime.execution_jobs AS j
          ON j.execution_id = o.execution_id AND j.generation = o.generation
        JOIN elitea_runtime.index_ingest_jobs AS i
          ON i.execution_id = j.execution_id
         AND i.generation = j.generation
         AND i.capability_id = j.capability_id
        WHERE o.stream_name = $1
          AND o.published_at IS NOT NULL
          AND o.retired_at IS NULL
          AND o.authority_granted_at IS NULL
          AND o.deadline > statement_timestamp()
          AND COALESCE(o.last_visibility_at, o.published_at)
              <= statement_timestamp() - ($3::bigint * interval '1 millisecond')
          AND j.state IN ('PENDING', 'DISPATCHED')
          AND j.desired_state = 'RUNNING'
          AND j.capability_id = 'index.ingest.v1'
          AND j.generation = 1
          AND i.index_meta_initialized_at IS NOT NULL
        ORDER BY COALESCE(o.last_visibility_at, o.published_at) ASC, o.outbox_id ASC
        LIMIT $2
    )
) AS candidate
ORDER BY candidate.visibility_order ASC, candidate.outbox_id ASC
LIMIT $2`, r.expectedStream, int32(limit), visibilityMillis)
	if err != nil {
		return nil, fmt.Errorf("list pending index ingest outbox: %w", err)
	}
	defer rows.Close()

	var outboxIDs []string
	for rows.Next() {
		var outboxID string
		if err := rows.Scan(&outboxID); err != nil {
			return nil, fmt.Errorf("scan pending index ingest outbox: %w", err)
		}
		if outboxID == "" {
			return nil, errors.New("pending index ingest outbox contains empty identity")
		}
		outboxIDs = append(outboxIDs, outboxID)
		if len(outboxIDs) > limit {
			return nil, executionapp.ErrPendingOutboxBatchLimitExceeded
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending index ingest outbox: %w", err)
	}
	return outboxIDs, nil
}

// LoadPendingIndexIngest joins only capability metadata, the immutable
// input-bundle manifest identity, and the optional embedding-binding entry
// identity. It deliberately never selects manifest_bytes or entry content.
func (r *CommandOutboxRepository) LoadPendingIndexIngest(ctx context.Context, outboxID string) (indexingapp.IndexIngestDispatch, error) {
	if outboxID == "" {
		return indexingapp.IndexIngestDispatch{}, indexingapp.ErrInvalidIndexIngestDispatch
	}
	var dispatch indexingapp.IndexIngestDispatch
	var generation, ordinal, manifestSize int64
	var priority int32
	var bundleDigest, embeddingBindingDigest []byte
	var embeddingBindingCount int64
	err := r.store.QueryRow(ctx, `
SELECT o.outbox_id,
       j.command_id,
       j.execution_id,
       j.generation,
       o.dispatch_ordinal,
       j.tenant_id,
       j.resource_project_id::text,
       j.projection_project_id::text,
       j.principal_ref,
       b.input_bundle_id,
       b.immutable_version,
       b.media_type,
       b.manifest_size,
       b.manifest_digest,
       j.capability_version,
       o.resource_class,
       o.isolation_class,
       o.priority,
       o.deadline,
       o.limits_revision,
       o.traceparent,
       o.tracestate,
       i.toolkit_configuration_entry_id,
       i.tool_parameters_entry_id,
       COALESCE(i.llm_model_entry_id, ''),
       COALESCE(i.llm_configuration_entry_id, ''),
       COALESCE(i.mcp_tokens_entry_id, ''),
       COALESCE(embedding_binding.entry_id, ''),
       COALESCE(embedding_binding.content_digest, ''::bytea),
       COALESCE(embedding_binding.binding_count, 0),
       COALESCE(i.client_stream_id, ''),
       COALESCE(i.client_message_id, ''),
       COALESCE(i.sio_event, ''),
       i.initiator
FROM elitea_runtime.command_outbox AS o
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = o.execution_id AND j.generation = o.generation
JOIN elitea_runtime.index_ingest_jobs AS i
  ON i.execution_id = j.execution_id
 AND i.generation = j.generation
 AND i.capability_id = j.capability_id
 AND i.input_bundle_id = j.input_bundle_id
JOIN elitea_runtime.input_bundles AS b
  ON b.input_bundle_id = i.input_bundle_id
LEFT JOIN LATERAL (
    SELECT entry.entry_id,
           entry.content_digest,
           count(*) OVER () AS binding_count
    FROM elitea_runtime.input_bundle_entries AS entry
    WHERE entry.input_bundle_id = i.input_bundle_id
      AND entry.semantic_role = 'index.embedding_binding'
    ORDER BY entry.entry_id
    LIMIT 1
) AS embedding_binding ON true
WHERE o.outbox_id = $1
  AND o.stream_name = $2
  AND o.published_at IS NULL
  AND o.retired_at IS NULL
  AND o.authority_granted_at IS NULL
  AND o.deadline > clock_timestamp()
  AND j.state = 'PENDING'
  AND j.desired_state = 'RUNNING'
  AND j.capability_id = 'index.ingest.v1'
  AND j.generation = 1
  AND i.index_meta_initialized_at IS NOT NULL`, outboxID, r.expectedStream).Scan(
		&dispatch.OutboxID,
		&dispatch.CommandID,
		&dispatch.ExecutionID,
		&generation,
		&ordinal,
		&dispatch.TenantID,
		&dispatch.ResourceProjectID,
		&dispatch.ProjectionProjectID,
		&dispatch.PrincipalRef,
		&dispatch.InputBundleID,
		&dispatch.InputBundleVersion,
		&dispatch.InputBundleMediaType,
		&manifestSize,
		&bundleDigest,
		&dispatch.CapabilityVersion,
		&dispatch.ResourceClass,
		&dispatch.IsolationClass,
		&priority,
		&dispatch.Deadline,
		&dispatch.LimitsRevision,
		&dispatch.Traceparent,
		&dispatch.Tracestate,
		&dispatch.ToolkitConfigurationEntryID,
		&dispatch.ToolParametersEntryID,
		&dispatch.LLMModelEntryID,
		&dispatch.LLMConfigurationEntryID,
		&dispatch.MCPTokensEntryID,
		&dispatch.EmbeddingBindingEntryID,
		&embeddingBindingDigest,
		&embeddingBindingCount,
		&dispatch.ClientStreamID,
		&dispatch.ClientMessageID,
		&dispatch.SIOEvent,
		&dispatch.Initiator,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return indexingapp.IndexIngestDispatch{}, ErrPendingIndexIngestDispatchNotFound
	}
	if err != nil {
		return indexingapp.IndexIngestDispatch{}, fmt.Errorf("load pending index ingest outbox: %w", err)
	}
	if generation <= 0 || ordinal <= 0 || priority <= 0 || manifestSize <= 0 {
		return indexingapp.IndexIngestDispatch{}, errors.New("pending index ingest outbox contains invalid numeric fields")
	}
	dispatch.Generation = uint64(generation)
	dispatch.DispatchOrdinal = uint64(ordinal)
	dispatch.Priority = uint32(priority)
	dispatch.InputBundleByteLength = uint64(manifestSize)
	if dispatch.InputBundleDigest, err = storedDigest(bundleDigest); err != nil {
		return indexingapp.IndexIngestDispatch{}, fmt.Errorf("pending index ingest input digest: %w", err)
	}
	if embeddingBindingCount > 1 {
		return indexingapp.IndexIngestDispatch{}, errors.New("pending index ingest has ambiguous embedding bindings")
	}
	if dispatch.EmbeddingBindingEntryID != "" {
		if embeddingBindingCount != 1 {
			return indexingapp.IndexIngestDispatch{}, errors.New("pending index ingest embedding binding count is invalid")
		}
		if dispatch.EmbeddingBindingDigest, err = storedDigest(embeddingBindingDigest); err != nil {
			return indexingapp.IndexIngestDispatch{}, errors.New("pending index ingest embedding binding digest is invalid")
		}
	} else if embeddingBindingCount != 0 || len(embeddingBindingDigest) != 0 {
		return indexingapp.IndexIngestDispatch{}, errors.New("pending index ingest embedding binding is incomplete")
	}
	if err := dispatch.Validate(); err != nil {
		return indexingapp.IndexIngestDispatch{}, fmt.Errorf("invalid stored index ingest dispatch: %w", err)
	}
	return dispatch, nil
}

func (r *CommandOutboxRepository) LoadPreparedIndexIngest(ctx context.Context, outboxID string) (*executionapp.StoredPreparedEnvelope, error) {
	if outboxID == "" {
		return nil, indexingapp.ErrInvalidIndexIngestDispatch
	}
	var envelopeBytes, envelopeDigest []byte
	var signatureProfile int32
	var keyID string
	var published, retired, deadlineExpired, authorityGranted bool
	var jobState string
	err := r.store.QueryRow(ctx, `
SELECT o.prepared_signed_envelope_bytes,
       o.prepared_signed_envelope_digest,
       COALESCE(o.prepared_signature_profile, 0),
       COALESCE(o.prepared_key_id, ''),
       o.published_at IS NOT NULL,
       o.retired_at IS NOT NULL,
       o.deadline <= clock_timestamp() AND o.authority_granted_at IS NULL,
       o.authority_granted_at IS NOT NULL,
       j.state
FROM elitea_runtime.command_outbox AS o
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = o.execution_id AND j.generation = o.generation
JOIN elitea_runtime.index_ingest_jobs AS i
  ON i.execution_id = j.execution_id
 AND i.generation = j.generation
 AND i.capability_id = j.capability_id
WHERE o.outbox_id = $1
  AND o.stream_name = $2
  AND j.capability_id = 'index.ingest.v1'
  AND j.generation = 1
  AND j.desired_state = 'RUNNING'
  AND i.index_meta_initialized_at IS NOT NULL`, outboxID, r.expectedStream).Scan(
		&envelopeBytes,
		&envelopeDigest,
		&signatureProfile,
		&keyID,
		&published,
		&retired,
		&deadlineExpired,
		&authorityGranted,
		&jobState,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPendingIndexIngestDispatchNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load prepared index ingest envelope: %w", err)
	}
	if retired {
		return nil, executionapp.ErrDispatchRetired
	}
	if authorityGranted || (jobState != string(executiondomain.JobPending) && jobState != string(executiondomain.JobDispatched)) {
		return nil, executionapp.ErrDispatchRetired
	}
	if deadlineExpired {
		return nil, executionapp.ErrDispatchDeadlineExpired
	}
	return storedPreparedEnvelope(envelopeBytes, envelopeDigest, signatureProfile, keyID, published)
}

// StorePreparedIndexIngest atomically selects one exact signed envelope before
// command-bus publish. Competing publishers receive the same durable winner.
func (r *CommandOutboxRepository) StorePreparedIndexIngest(ctx context.Context, outboxID string, candidate executionapp.PreparedCommandEnvelope) (executionapp.StoredPreparedEnvelope, error) {
	if outboxID == "" {
		return executionapp.StoredPreparedEnvelope{}, indexingapp.ErrInvalidIndexIngestDispatch
	}
	if err := candidate.Validate(); err != nil {
		return executionapp.StoredPreparedEnvelope{}, err
	}

	var selected executionapp.StoredPreparedEnvelope
	err := r.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		var envelopeBytes, envelopeDigest []byte
		var signatureProfile int32
		var keyID, jobState string
		var published, retired, deadlineExpired, authorityGranted bool
		err := tx.QueryRow(ctx, `
SELECT o.prepared_signed_envelope_bytes,
       o.prepared_signed_envelope_digest,
       COALESCE(o.prepared_signature_profile, 0),
       COALESCE(o.prepared_key_id, ''),
       o.published_at IS NOT NULL,
       o.retired_at IS NOT NULL,
       o.deadline <= clock_timestamp(),
       o.authority_granted_at IS NOT NULL,
       j.state
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
JOIN elitea_runtime.index_ingest_jobs AS i
  ON i.execution_id = j.execution_id
 AND i.generation = j.generation
 AND i.capability_id = j.capability_id
WHERE o.outbox_id = $1
  AND o.stream_name = $2
  AND j.capability_id = 'index.ingest.v1'
  AND j.generation = 1
  AND j.desired_state = 'RUNNING'
  AND i.index_meta_initialized_at IS NOT NULL
FOR UPDATE OF j, o`, outboxID, r.expectedStream).Scan(
			&envelopeBytes,
			&envelopeDigest,
			&signatureProfile,
			&keyID,
			&published,
			&retired,
			&deadlineExpired,
			&authorityGranted,
			&jobState,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPendingIndexIngestDispatchNotFound
		}
		if err != nil {
			return fmt.Errorf("select prepared index ingest envelope: %w", err)
		}
		if retired {
			return executionapp.ErrDispatchRetired
		}
		// A competing publisher can select, append and publish the envelope while
		// this publisher is signing. That moves the job to DISPATCHED, which is
		// not terminal: no worker holds the command yet, and the visibility
		// redelivery path appends the same durable winner again. The winner
		// therefore stays returnable in PENDING and in DISPATCHED. Worker
		// authority, or a terminal job state, does end the window: returning the
		// winner then would recreate a bus message that the worker may already
		// have acknowledged and deleted.
		if authorityGranted || (jobState != string(executiondomain.JobPending) && jobState != string(executiondomain.JobDispatched)) {
			return executionapp.ErrDispatchRetired
		}
		if deadlineExpired {
			return executionapp.ErrDispatchDeadlineExpired
		}
		stored, err := storedPreparedEnvelope(envelopeBytes, envelopeDigest, signatureProfile, keyID, published)
		if err != nil {
			return err
		}
		if stored != nil {
			selected = *stored
			return nil
		}
		if published {
			return ErrPendingIndexIngestDispatchNotFound
		}
		// Selection stays a one-shot. Only a PENDING job may select a new
		// envelope; a DISPATCHED job already holds its durable winner, so a
		// second signature must never replace it.
		if jobState != string(executiondomain.JobPending) {
			return ErrPendingIndexIngestDispatchNotFound
		}
		tag, err := tx.Exec(ctx, `
UPDATE elitea_runtime.command_outbox
SET prepared_signed_envelope_bytes = $2,
    prepared_signed_envelope_digest = $3,
    prepared_signature_profile = $4,
    prepared_key_id = $5,
    prepared_at = clock_timestamp()
WHERE outbox_id = $1
  AND retired_at IS NULL
  AND authority_granted_at IS NULL
  AND published_at IS NULL
  AND prepared_signed_envelope_bytes IS NULL
  AND deadline > clock_timestamp()`,
			outboxID,
			append([]byte(nil), candidate.Bytes...),
			candidate.Digest[:],
			candidate.SignatureProfile,
			candidate.KeyID,
		)
		if err != nil {
			return fmt.Errorf("store prepared index ingest envelope: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return executionapp.ErrDispatchDeadlineExpired
		}
		selected = executionapp.StoredPreparedEnvelope{Envelope: candidate.Clone()}
		return nil
	})
	if err != nil {
		return executionapp.StoredPreparedEnvelope{}, err
	}
	return selected, nil
}

func (r *CommandOutboxRepository) MarkIndexIngestPublished(ctx context.Context, outboxID string, encodedDigest runtimedomain.Digest) error {
	if outboxID == "" || encodedDigest.IsZero() {
		return indexingapp.ErrInvalidIndexIngestDispatch
	}
	return r.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		var executionID, jobState string
		var generation int64
		var prepared, stored []byte
		var published, retired, deadlineExpired, authorityGranted bool
		err := tx.QueryRow(ctx, `
SELECT j.execution_id, j.generation, j.state,
       o.prepared_signed_envelope_digest,
       o.published_envelope_digest,
       o.published_at IS NOT NULL,
       o.retired_at IS NOT NULL,
       o.deadline <= clock_timestamp(),
       o.authority_granted_at IS NOT NULL
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
JOIN elitea_runtime.index_ingest_jobs AS i
  ON i.execution_id = j.execution_id
 AND i.generation = j.generation
 AND i.capability_id = j.capability_id
WHERE o.outbox_id = $1
  AND o.stream_name = $2
  AND j.capability_id = 'index.ingest.v1'
  AND j.generation = 1
  AND j.desired_state = 'RUNNING'
  AND i.index_meta_initialized_at IS NOT NULL
FOR UPDATE OF j, o`, outboxID, r.expectedStream).Scan(
			&executionID,
			&generation,
			&jobState,
			&prepared,
			&stored,
			&published,
			&retired,
			&deadlineExpired,
			&authorityGranted,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPendingIndexIngestDispatchNotFound
		}
		if err != nil {
			return fmt.Errorf("lock index ingest outbox publication: %w", err)
		}
		if retired {
			return executionapp.ErrDispatchRetired
		}
		persisted, err := storedDigest(prepared)
		if err != nil {
			return fmt.Errorf("invalid prepared index ingest digest: %w", err)
		}
		if persisted != encodedDigest {
			return ErrOutboxPublishConflict
		}
		if published {
			publishedDigest, err := storedDigest(stored)
			if err != nil {
				return fmt.Errorf("invalid published index ingest digest: %w", err)
			}
			if publishedDigest != persisted {
				return ErrOutboxPublishConflict
			}
			if authorityGranted || (jobState != string(executiondomain.JobPending) && jobState != string(executiondomain.JobDispatched)) {
				return nil
			}
			tag, err := tx.Exec(ctx, `
UPDATE elitea_runtime.command_outbox
SET last_visibility_at = clock_timestamp(),
    publish_attempts = publish_attempts + 1,
    last_error_code = NULL
WHERE outbox_id = $1
  AND published_at IS NOT NULL
  AND retired_at IS NULL
  AND authority_granted_at IS NULL
  AND deadline > clock_timestamp()
  AND prepared_signed_envelope_digest = $2
  AND published_envelope_digest = $2`, outboxID, encodedDigest[:])
			if err != nil {
				return fmt.Errorf("refresh index ingest visibility observation: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return executionapp.ErrDispatchDeadlineExpired
			}
			return nil
		}
		if authorityGranted {
			return ErrOutboxPublishConflict
		}
		if deadlineExpired {
			return executionapp.ErrDispatchDeadlineExpired
		}
		if jobState != string(executiondomain.JobPending) {
			return ErrOutboxPublishConflict
		}
		tag, err := tx.Exec(ctx, `
UPDATE elitea_runtime.command_outbox
SET published_at = clock_timestamp(),
    last_visibility_at = clock_timestamp(),
    published_envelope_digest = prepared_signed_envelope_digest,
    publish_attempts = publish_attempts + 1,
    last_error_code = NULL
WHERE outbox_id = $1
  AND published_at IS NULL
  AND retired_at IS NULL
  AND authority_granted_at IS NULL
  AND deadline > clock_timestamp()
  AND prepared_signed_envelope_digest = $2`, outboxID, encodedDigest[:])
		if err != nil {
			return fmt.Errorf("mark index ingest outbox published: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return executionapp.ErrDispatchDeadlineExpired
		}
		tag, err = tx.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET state = 'DISPATCHED'
WHERE execution_id = $1
  AND generation = $2
  AND capability_id = 'index.ingest.v1'
  AND desired_state = 'RUNNING'
  AND state = 'PENDING'`, executionID, generation)
		if err != nil {
			return fmt.Errorf("mark index ingest execution dispatched: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrOutboxPublishConflict
		}
		return nil
	})
}

var _ indexingapp.PendingDispatchStore = (*CommandOutboxRepository)(nil)
var _ indexingapp.PendingIndexIngestOutbox = (*CommandOutboxRepository)(nil)
