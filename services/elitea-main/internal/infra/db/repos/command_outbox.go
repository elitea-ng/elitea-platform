package repos

import (
	"context"
	"errors"
	"fmt"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrPendingDispatchNotFound = errors.New("pending validation dispatch not found")
	ErrOutboxPublishConflict   = errors.New("outbox published with a different envelope digest")
)

const (
	retirementCodeDeadlineExceeded  = "DEADLINE_EXCEEDED"
	retirementCodeCancelled         = "CANCELLED"
	deadlineRetirementEventType     = "execution.failed"
	cancellationRetirementEventType = replayEventRuntimeFailure
)

var (
	deadlineRetirementEventBytes     = []byte(`{"code":"DEADLINE_EXCEEDED","safe_message":"The execution deadline was exceeded before worker authority was granted.","retryable":true}`)
	cancellationRetirementEventBytes = []byte(`{"code":"CANCELLED","safe_message":"Execution was cancelled.","retryable":false}`)
)

type noAuthorityRetirementCandidate struct {
	OutboxID            string
	ExecutionID         string
	Generation          int64
	ProjectionProjectID int64
	DesiredState        string
}

type CommandOutboxRepository struct {
	store          sharedStore
	expectedStream string
	activity       currentIndexActivityProjector
}

func NewCommandOutboxRepository(pool *pgxpool.Pool, expectedStream string) (*CommandOutboxRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	repository, err := newCommandOutboxRepository(store, expectedStream)
	if err != nil {
		return nil, err
	}
	repository.activity = &postgresCurrentIndexActivityProjector{}
	return repository, nil
}

func newCommandOutboxRepository(store sharedStore, expectedStream string) (*CommandOutboxRepository, error) {
	if store == nil || expectedStream == "" {
		return nil, errors.New("command outbox database and expected stream are required")
	}
	return &CommandOutboxRepository{
		store: store, expectedStream: expectedStream,
		activity: noopCurrentIndexActivityProjector{},
	}, nil
}

func insertCommandOutbox(ctx context.Context, tx sqlExecutor, policy ExecutionDispatchPolicy, record executiondomain.Admission, timing admissionTiming) error {
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.command_outbox (
    outbox_id, execution_id, generation, stream_name, dispatch_ordinal,
    resource_class, isolation_class, priority, deadline, limits_revision,
    traceparent, tracestate, created_at
) VALUES ($1, $2, $3, $4, 1, $5, $6, $7, $8, $9, '', '', $10)`,
		record.Outbox.ID,
		record.Outbox.ExecutionID,
		int64(record.Outbox.Generation),
		policy.StreamName,
		policy.ResourceClass,
		policy.IsolationClass,
		int32(policy.Priority),
		timing.Deadline,
		policy.LimitsRevision,
		timing.AdmittedAt,
	); err != nil {
		return fmt.Errorf("insert command outbox: %w", err)
	}
	return nil
}

// RetireNoAuthorityValidation terminalizes at most limit cancelled or expired
// phase-one jobs using the database clock. Candidate job/outbox rows are locked
// together, and any historical claim excludes retirement because worker
// authority may already have existed. Replay lifecycle events are written in
// the same transaction; worker output/result/settlement tables are untouched.
func (r *CommandOutboxRepository) RetireNoAuthorityValidation(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > executionapp.MaxOutboxPublisherBatchSize {
		return 0, executionapp.ErrInvalidPendingOutboxLimit
	}

	retiredCount := 0
	err := r.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		cancelled, err := selectCancelledNoAuthorityValidation(ctx, tx, r.expectedStream, limit)
		if err != nil {
			return err
		}
		for _, candidate := range cancelled {
			retired, err := retireLockedNoAuthorityValidation(ctx, tx, candidate)
			if err != nil {
				return err
			}
			if retired {
				retiredCount++
			}
		}

		remaining := limit - retiredCount
		if remaining == 0 {
			return nil
		}
		expired, err := selectExpiredNoAuthorityValidation(ctx, tx, r.expectedStream, remaining)
		if err != nil {
			return err
		}
		for _, candidate := range expired {
			retired, err := retireLockedNoAuthorityValidation(ctx, tx, candidate)
			if err != nil {
				return err
			}
			if retired {
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

func selectCancelledNoAuthorityValidation(ctx context.Context, tx sqlExecutor, expectedStream string, limit int) ([]noAuthorityRetirementCandidate, error) {
	rows, err := tx.Query(ctx, `
SELECT o.outbox_id, j.execution_id, j.generation,
       j.projection_project_id, j.desired_state
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
WHERE j.desired_state = 'CANCELLED'
  AND j.state IN ('PENDING', 'DISPATCHED')
  AND j.capability_id = 'configuration.validate.v1'
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
		return nil, fmt.Errorf("select cancelled no-authority validation outbox: %w", err)
	}
	return scanNoAuthorityRetirementCandidates(rows, limit, "cancelled")
}

func selectExpiredNoAuthorityValidation(ctx context.Context, tx sqlExecutor, expectedStream string, limit int) ([]noAuthorityRetirementCandidate, error) {
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
  AND j.capability_id = 'configuration.validate.v1'
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
		return nil, fmt.Errorf("select expired no-authority validation outbox: %w", err)
	}
	return scanNoAuthorityRetirementCandidates(rows, limit, "expired")
}

func scanNoAuthorityRetirementCandidates(rows sqlRows, limit int, kind string) ([]noAuthorityRetirementCandidate, error) {
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
			return nil, fmt.Errorf("scan %s no-authority validation outbox: %w", kind, err)
		}
		candidates = append(candidates, candidate)
		if len(candidates) > limit {
			return nil, executionapp.ErrPendingOutboxBatchLimitExceeded
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s no-authority validation outbox: %w", kind, err)
	}
	return candidates, nil
}

func retireLockedNoAuthorityValidation(ctx context.Context, tx sqlExecutor, candidate noAuthorityRetirementCandidate) (bool, error) {
	retirementCode := retirementCodeDeadlineExceeded
	jobState := string(executiondomain.JobFailed)
	terminalErrorCode := any(retirementCodeDeadlineExceeded)
	eventType := deadlineRetirementEventType
	eventBytes := deadlineRetirementEventBytes
	if candidate.DesiredState == string(runtimedomain.DesiredCancelled) {
		retirementCode = retirementCodeCancelled
		jobState = string(executiondomain.JobCancelled)
		terminalErrorCode = nil
		eventType = cancellationRetirementEventType
		eventBytes = cancellationRetirementEventBytes
	}

	var projectionProjectID int64
	err := tx.QueryRow(ctx, `
WITH retired AS (
    UPDATE elitea_runtime.command_outbox AS o
    SET retired_at = clock_timestamp(), retirement_code = $4
    FROM elitea_runtime.execution_jobs AS j
    WHERE o.outbox_id = $1
      AND o.execution_id = $2
      AND o.generation = $3
      AND o.retired_at IS NULL
      AND o.authority_granted_at IS NULL
      AND j.execution_id = o.execution_id
      AND j.generation = o.generation
      AND j.state IN ('PENDING', 'DISPATCHED')
      AND (
          ($4 = 'CANCELLED' AND j.desired_state = 'CANCELLED')
          OR
          ($4 = 'DEADLINE_EXCEEDED' AND j.desired_state <> 'CANCELLED'
              AND o.deadline <= clock_timestamp())
      )
      AND NOT EXISTS (
          SELECT 1
          FROM elitea_runtime.execution_claims AS c
          WHERE c.execution_id = o.execution_id AND c.generation = o.generation
      )
    RETURNING o.execution_id, o.generation, o.retired_at
)
UPDATE elitea_runtime.execution_jobs AS j
SET state = $5, settled_at = retired.retired_at, terminal_error_code = $6
FROM retired
WHERE j.execution_id = retired.execution_id
  AND j.generation = retired.generation
RETURNING j.projection_project_id`,
		candidate.OutboxID,
		candidate.ExecutionID,
		candidate.Generation,
		retirementCode,
		jobState,
		terminalErrorCode,
	).Scan(&projectionProjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("retire no-authority validation execution: %w", err)
	}
	if projectionProjectID != candidate.ProjectionProjectID {
		return false, errors.New("retired validation projection identity changed")
	}

	eventID := "retirement:" + candidate.OutboxID
	eventDigest := runtimedomain.SHA256(eventBytes)
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.execution_replay_events (
    event_id, execution_id, generation, projection_project_id,
    event_type, event_bytes, event_digest
) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		eventID,
		candidate.ExecutionID,
		candidate.Generation,
		projectionProjectID,
		eventType,
		append([]byte(nil), eventBytes...),
		eventDigest[:],
	); err != nil {
		return false, fmt.Errorf("insert no-authority retirement replay event: %w", err)
	}
	return true, nil
}

// ListPendingValidationIDs returns both new unpublished work and published,
// unclaimed, nonterminal work whose bounded command-bus visibility observation has
// expired. PostgreSQL publication is therefore a visibility lease rather than
// a permanent delivery acknowledgement. The query deliberately does not lock
// rows: scaled-out publishers may discover the same ID, while the command-bus adapter
// atomically deduplicates by stable outbox identity and exact prepared bytes.
func (r *CommandOutboxRepository) ListPendingValidationIDs(ctx context.Context, limit int, visibilityTimeout time.Duration) ([]string, error) {
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
        WHERE o.stream_name = $1
          AND o.published_at IS NULL
          AND o.retired_at IS NULL
          AND o.authority_granted_at IS NULL
          AND o.deadline > statement_timestamp()
          AND j.state = 'PENDING'
          AND j.capability_id = 'configuration.validate.v1'
          AND j.generation = 1
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
        WHERE o.stream_name = $1
          AND o.published_at IS NOT NULL
          AND o.retired_at IS NULL
          AND o.authority_granted_at IS NULL
          AND o.deadline > statement_timestamp()
          AND COALESCE(o.last_visibility_at, o.published_at)
              <= statement_timestamp() - ($3::bigint * interval '1 millisecond')
          AND j.state IN ('PENDING', 'DISPATCHED')
          AND j.capability_id = 'configuration.validate.v1'
          AND j.generation = 1
        ORDER BY COALESCE(o.last_visibility_at, o.published_at) ASC, o.outbox_id ASC
        LIMIT $2
    )
) AS candidate
ORDER BY candidate.visibility_order ASC, candidate.outbox_id ASC
LIMIT $2`, r.expectedStream, int32(limit), visibilityMillis)
	if err != nil {
		return nil, fmt.Errorf("list pending command outbox: %w", err)
	}
	defer rows.Close()

	var outboxIDs []string
	for rows.Next() {
		var outboxID string
		if err := rows.Scan(&outboxID); err != nil {
			return nil, fmt.Errorf("scan pending command outbox: %w", err)
		}
		if outboxID == "" {
			return nil, errors.New("pending command outbox contains empty identity")
		}
		outboxIDs = append(outboxIDs, outboxID)
		if len(outboxIDs) > limit {
			return nil, executionapp.ErrPendingOutboxBatchLimitExceeded
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending command outbox: %w", err)
	}
	return outboxIDs, nil
}

func (r *CommandOutboxRepository) LoadPendingValidation(ctx context.Context, outboxID string) (executionapp.ValidationDispatch, error) {
	if outboxID == "" {
		return executionapp.ValidationDispatch{}, executionapp.ErrInvalidDispatch
	}
	var dispatch executionapp.ValidationDispatch
	var generation, ordinal int64
	var priority int32
	var manifestSize int64
	var bundleDigest, catalogDigest, schemaDigest []byte
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
       j.configuration_revision_id,
       j.configuration_type,
       j.catalog_revision,
       j.catalog_digest,
       j.schema_id,
       j.schema_revision,
       j.schema_digest,
       j.settings_entry_id
FROM elitea_runtime.command_outbox AS o
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = o.execution_id AND j.generation = o.generation
JOIN elitea_runtime.input_bundles AS b
  ON b.input_bundle_id = j.input_bundle_id
WHERE o.outbox_id = $1
  AND o.stream_name = $2
  AND o.published_at IS NULL
  AND o.retired_at IS NULL
  AND o.authority_granted_at IS NULL
  AND o.deadline > clock_timestamp()
  AND j.state = 'PENDING'
  AND j.capability_id = 'configuration.validate.v1'`, outboxID, r.expectedStream).Scan(
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
		&dispatch.Command.ConfigurationRevisionID,
		&dispatch.Command.ConfigurationType,
		&dispatch.Command.CatalogRevision,
		&catalogDigest,
		&dispatch.Command.SchemaID,
		&dispatch.Command.SchemaRevision,
		&schemaDigest,
		&dispatch.Command.SettingsEntryID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return executionapp.ValidationDispatch{}, ErrPendingDispatchNotFound
	}
	if err != nil {
		return executionapp.ValidationDispatch{}, fmt.Errorf("load pending command outbox: %w", err)
	}
	if generation <= 0 || ordinal <= 0 || priority <= 0 || manifestSize <= 0 {
		return executionapp.ValidationDispatch{}, errors.New("pending outbox contains invalid numeric fields")
	}
	dispatch.Generation = uint64(generation)
	dispatch.DispatchOrdinal = uint64(ordinal)
	dispatch.Priority = uint32(priority)
	dispatch.InputBundleByteLength = uint64(manifestSize)
	if dispatch.InputBundleDigest, err = storedDigest(bundleDigest); err != nil {
		return executionapp.ValidationDispatch{}, fmt.Errorf("pending outbox input digest: %w", err)
	}
	if dispatch.Command.CatalogDigest, err = storedDigest(catalogDigest); err != nil {
		return executionapp.ValidationDispatch{}, fmt.Errorf("pending outbox catalog digest: %w", err)
	}
	if dispatch.Command.SchemaDigest, err = storedDigest(schemaDigest); err != nil {
		return executionapp.ValidationDispatch{}, fmt.Errorf("pending outbox schema digest: %w", err)
	}
	if err := dispatch.Validate(); err != nil {
		return executionapp.ValidationDispatch{}, fmt.Errorf("invalid stored validation dispatch: %w", err)
	}
	return dispatch, nil
}

func (r *CommandOutboxRepository) LoadPreparedValidation(ctx context.Context, outboxID string) (*executionapp.StoredPreparedEnvelope, error) {
	if outboxID == "" {
		return nil, executionapp.ErrInvalidDispatch
	}
	var envelopeBytes, envelopeDigest []byte
	var signatureProfile int32
	var keyID string
	var published, retired, deadlineExpired bool
	err := r.store.QueryRow(ctx, `
SELECT prepared_signed_envelope_bytes,
       prepared_signed_envelope_digest,
       COALESCE(prepared_signature_profile, 0),
       COALESCE(prepared_key_id, ''),
       published_at IS NOT NULL,
       retired_at IS NOT NULL,
       deadline <= clock_timestamp() AND authority_granted_at IS NULL
FROM elitea_runtime.command_outbox
WHERE outbox_id = $1 AND stream_name = $2`, outboxID, r.expectedStream).Scan(
		&envelopeBytes,
		&envelopeDigest,
		&signatureProfile,
		&keyID,
		&published,
		&retired,
		&deadlineExpired,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPendingDispatchNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load prepared command envelope: %w", err)
	}
	if retired {
		return nil, executionapp.ErrDispatchRetired
	}
	if deadlineExpired {
		return nil, executionapp.ErrDispatchDeadlineExpired
	}
	return storedPreparedEnvelope(envelopeBytes, envelopeDigest, signatureProfile, keyID, published)
}

// StorePreparedValidation atomically selects one exact signed envelope before
// any command-bus publish. Signing is deliberately outside this short transaction:
// when multiple publishers race, only one candidate is stored and every caller
// receives that durable winner.
func (r *CommandOutboxRepository) StorePreparedValidation(ctx context.Context, outboxID string, candidate executionapp.PreparedCommandEnvelope) (executionapp.StoredPreparedEnvelope, error) {
	if outboxID == "" {
		return executionapp.StoredPreparedEnvelope{}, executionapp.ErrInvalidDispatch
	}
	if err := candidate.Validate(); err != nil {
		return executionapp.StoredPreparedEnvelope{}, err
	}

	var selected executionapp.StoredPreparedEnvelope
	err := r.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		var envelopeBytes, envelopeDigest []byte
		var signatureProfile int32
		var keyID, jobState, capabilityID string
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
       j.state,
       j.capability_id
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
WHERE o.outbox_id = $1 AND o.stream_name = $2
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
			&capabilityID,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPendingDispatchNotFound
		}
		if err != nil {
			return fmt.Errorf("select prepared command envelope: %w", err)
		}
		if retired {
			return executionapp.ErrDispatchRetired
		}
		if deadlineExpired && !authorityGranted {
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
		if published || authorityGranted || jobState != string(executiondomain.JobPending) || capabilityID != "configuration.validate.v1" {
			return ErrPendingDispatchNotFound
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
			return fmt.Errorf("store prepared command envelope: %w", err)
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

func (r *CommandOutboxRepository) MarkValidationPublished(ctx context.Context, outboxID string, encodedDigest runtimedomain.Digest) error {
	if outboxID == "" || encodedDigest.IsZero() {
		return executionapp.ErrInvalidDispatch
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
WHERE o.outbox_id = $1 AND o.stream_name = $2
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
			return ErrPendingDispatchNotFound
		}
		if err != nil {
			return fmt.Errorf("lock command outbox publication: %w", err)
		}
		if retired {
			return executionapp.ErrDispatchRetired
		}
		persisted, err := storedDigest(prepared)
		if err != nil {
			return fmt.Errorf("invalid prepared outbox digest: %w", err)
		}
		if persisted != encodedDigest {
			return ErrOutboxPublishConflict
		}
		if published {
			publishedDigest, err := storedDigest(stored)
			if err != nil {
				return fmt.Errorf("invalid published outbox digest: %w", err)
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
				return fmt.Errorf("refresh command visibility observation: %w", err)
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
			return fmt.Errorf("mark command outbox published: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return executionapp.ErrDispatchDeadlineExpired
		}
		tag, err = tx.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET state = 'DISPATCHED'
WHERE execution_id = $1 AND generation = $2 AND state = 'PENDING'`, executionID, generation)
		if err != nil {
			return fmt.Errorf("mark execution dispatched: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrOutboxPublishConflict
		}
		return nil
	})
}

func storedPreparedEnvelope(envelopeBytes, envelopeDigest []byte, signatureProfile int32, keyID string, published bool) (*executionapp.StoredPreparedEnvelope, error) {
	if len(envelopeBytes) == 0 && len(envelopeDigest) == 0 && signatureProfile == 0 && keyID == "" {
		if published {
			return nil, errors.New("published outbox is missing its prepared envelope")
		}
		return nil, nil
	}
	digest, err := storedDigest(envelopeDigest)
	if err != nil {
		return nil, fmt.Errorf("invalid prepared envelope digest: %w", err)
	}
	stored := &executionapp.StoredPreparedEnvelope{
		Envelope: executionapp.PreparedCommandEnvelope{
			Bytes:            append([]byte(nil), envelopeBytes...),
			Digest:           digest,
			SignatureProfile: signatureProfile,
			KeyID:            keyID,
		},
		Published: published,
	}
	if err := stored.Validate(); err != nil {
		return nil, fmt.Errorf("stored prepared command envelope: %w", err)
	}
	return stored, nil
}

var _ executionapp.PendingDispatchStore = (*CommandOutboxRepository)(nil)
var _ executionapp.PendingValidationOutbox = (*CommandOutboxRepository)(nil)
