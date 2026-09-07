package repos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

const (
	payloadTypeConfigurationValidation = "CONFIGURATION_VALIDATION"
	payloadTypeRuntimeFailure          = "RUNTIME_FAILURE"
	payloadTypeIndexIngestResult       = "INDEX_INGEST_RESULT"
	payloadTypeAgentExecutionResult    = "AGENT_EXECUTION_RESULT"
	payloadTypeToolkitCallToolResult   = "TOOLKIT_CALL_TOOL_RESULT"
)

type OutputInboxRepository struct {
	store sqlExecutor
}

func NewOutputInboxRepository(pool *pgxpool.Pool) (*OutputInboxRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return &OutputInboxRepository{store: store}, nil
}

func newOutputInboxRepository(store sqlExecutor) *OutputInboxRepository {
	return &OutputInboxRepository{store: store}
}

func (r *OutputInboxRepository) ExpectedValidation(ctx context.Context, executionID string, generation uint64) (outputapp.ExpectedValidation, error) {
	if executionID == "" || generation == 0 {
		return outputapp.ExpectedValidation{}, outputapp.ErrInvalidValidationOutput
	}
	var expected outputapp.ExpectedValidation
	var catalogDigest, schemaDigest, bundleDigest, settingsDigest []byte
	err := r.store.QueryRow(ctx, `
SELECT j.tenant_id,
       j.resource_project_id::text,
       j.projection_project_id::text,
       j.command_id,
       j.execution_id,
       j.generation,
       j.configuration_revision_id,
       j.configuration_type,
       j.catalog_revision,
       j.catalog_digest,
       j.schema_id,
       j.schema_revision,
       j.schema_digest,
       j.settings_entry_id,
       b.input_bundle_id,
       b.manifest_digest,
       e.entry_version,
       e.content_digest
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.input_bundles AS b
  ON b.input_bundle_id = j.input_bundle_id
JOIN elitea_runtime.input_bundle_entries AS e
  ON e.input_bundle_id = b.input_bundle_id
 AND e.entry_id = j.settings_entry_id
WHERE j.execution_id = $1
  AND j.generation = $2
  AND j.capability_id = 'configuration.validate.v1'`, executionID, int64(generation)).Scan(
		&expected.TenantID,
		&expected.ResourceProjectID,
		&expected.ProjectionProjectID,
		&expected.CommandID,
		&expected.ExecutionID,
		&expected.Generation,
		&expected.Binding.Command.ConfigurationRevisionID,
		&expected.Binding.Command.ConfigurationType,
		&expected.Binding.Command.CatalogRevision,
		&catalogDigest,
		&expected.Binding.Command.SchemaID,
		&expected.Binding.Command.SchemaRevision,
		&schemaDigest,
		&expected.Binding.Command.SettingsEntryID,
		&expected.Binding.InputBundleID,
		&bundleDigest,
		&expected.Binding.SettingsEntryVersion,
		&settingsDigest,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return outputapp.ExpectedValidation{}, outputapp.ErrInvalidValidationOutput
	}
	if err != nil {
		return outputapp.ExpectedValidation{}, fmt.Errorf("load expected configuration validation: %w", err)
	}
	if expected.Binding.Command.CatalogDigest, err = storedDigest(catalogDigest); err != nil {
		return outputapp.ExpectedValidation{}, fmt.Errorf("stored catalog digest: %w", err)
	}
	if expected.Binding.Command.SchemaDigest, err = storedDigest(schemaDigest); err != nil {
		return outputapp.ExpectedValidation{}, fmt.Errorf("stored schema digest: %w", err)
	}
	if expected.Binding.InputBundleDigest, err = storedDigest(bundleDigest); err != nil {
		return outputapp.ExpectedValidation{}, fmt.Errorf("stored input bundle digest: %w", err)
	}
	if expected.Binding.SettingsContentDigest, err = storedDigest(settingsDigest); err != nil {
		return outputapp.ExpectedValidation{}, fmt.Errorf("stored settings digest: %w", err)
	}
	if err := expected.Validate(); err != nil {
		return outputapp.ExpectedValidation{}, fmt.Errorf("invalid stored configuration validation binding: %w", err)
	}
	return expected, nil
}

func (r *OutputInboxRepository) ExpectedRuntimeFailure(ctx context.Context, executionID string, generation uint64) (outputapp.ExpectedRuntimeFailure, error) {
	if executionID == "" || generation == 0 || generation > math.MaxInt64 {
		return outputapp.ExpectedRuntimeFailure{}, outputapp.ErrInvalidValidationOutput
	}
	var expected outputapp.ExpectedRuntimeFailure
	var storedGeneration int64
	err := r.store.QueryRow(ctx, `
SELECT j.tenant_id,
       j.resource_project_id::text,
       j.projection_project_id::text,
       j.capability_id,
       j.command_id,
       j.execution_id,
       j.generation,
	       CASE j.capability_id
           WHEN 'configuration.validate.v1'
               THEN 'configuration-validation:' || j.configuration_revision_id
	           WHEN 'index.ingest.v1'
	               THEN 'index-ingest:' || j.execution_id
	           WHEN 'agent.execute.application.v1'
	               THEN 'agent-execution:' || j.execution_id
	           WHEN 'agent.execute.adhoc.v1'
	               THEN 'agent-execution:' || j.execution_id
	           WHEN 'toolkit.call_tool.v1'
	               THEN 'toolkit-call-tool:' || j.execution_id
       END AS logical_output_id
FROM elitea_runtime.execution_jobs AS j
WHERE j.execution_id = $1
  AND j.generation = $2
  AND (
      (
          j.capability_id = 'configuration.validate.v1'
          AND EXISTS (
              SELECT 1
              FROM elitea_runtime.input_bundles AS b
              JOIN elitea_runtime.input_bundle_entries AS e
                ON e.input_bundle_id = b.input_bundle_id
               AND e.entry_id = j.settings_entry_id
              WHERE b.input_bundle_id = j.input_bundle_id
          )
      )
      OR
	      (
	          j.capability_id = 'index.ingest.v1'
          AND EXISTS (
              SELECT 1
              FROM elitea_runtime.index_ingest_jobs AS i
              JOIN elitea_runtime.input_bundles AS b
                ON b.input_bundle_id = i.input_bundle_id
              WHERE i.execution_id = j.execution_id
                AND i.generation = j.generation
                AND i.capability_id = j.capability_id
                AND i.input_bundle_id = j.input_bundle_id
	          )
	      )
	      OR
	      (
	          j.capability_id IN (
	              'agent.execute.application.v1',
	              'agent.execute.adhoc.v1'
	          )
	          AND EXISTS (
	              SELECT 1
	              FROM elitea_runtime.agent_execution_jobs AS a
	              JOIN elitea_runtime.input_bundles AS b
	                ON b.input_bundle_id = a.input_bundle_id
	              WHERE a.execution_id = j.execution_id
	                AND a.generation = j.generation
	                AND a.capability_id = j.capability_id
	                AND a.input_bundle_id = j.input_bundle_id
	          )
	      )
	      OR
	      (
	          -- toolkit.call_tool.v1 owns no per-capability table, so the
	          -- existence proof is the SETTINGS entry of its own bundle. The
	          -- other three arms prove the same thing through their binding row.
	          j.capability_id = 'toolkit.call_tool.v1'
	          AND EXISTS (
	              SELECT 1
	              FROM elitea_runtime.input_bundles AS b
	              JOIN elitea_runtime.input_bundle_entries AS e
	                ON e.input_bundle_id = b.input_bundle_id
	              WHERE b.input_bundle_id = j.input_bundle_id
	                AND e.semantic_role = 'toolkit.call_tool.settings'
	          )
	      )
	  )`, executionID, int64(generation)).Scan(
		&expected.TenantID,
		&expected.ResourceProjectID,
		&expected.ProjectionProjectID,
		&expected.CapabilityID,
		&expected.CommandID,
		&expected.ExecutionID,
		&storedGeneration,
		&expected.LogicalOutputID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return outputapp.ExpectedRuntimeFailure{}, outputapp.ErrInvalidValidationOutput
	}
	if err != nil {
		return outputapp.ExpectedRuntimeFailure{}, fmt.Errorf("load expected runtime failure: %w", err)
	}
	if storedGeneration <= 0 {
		return outputapp.ExpectedRuntimeFailure{}, outputapp.ErrInvalidValidationOutput
	}
	expected.Generation = uint64(storedGeneration)
	if err := expected.Validate(); err != nil {
		return outputapp.ExpectedRuntimeFailure{}, fmt.Errorf("invalid stored runtime failure binding: %w", err)
	}
	return expected, nil
}

type outputRecord struct {
	EventID               string
	LogicalOutputID       string
	ExecutionID           string
	Generation            uint64
	TenantID              string
	ResourceProjectID     int64
	ProjectionProjectID   int64
	CommandID             string
	WorkloadIdentity      string
	WorkloadSessionID     string
	ProducerID            string
	ClaimAttempt          uint64
	LeaseEpoch            uint64
	FenceToken            runtimedomain.FenceToken
	StreamID              string
	Sequence              uint64
	ClaimHandoffWatermark uint64
	PayloadType           string
	PayloadDigest         runtimedomain.Digest
	PayloadBytes          []byte
	SettlementProposalID  string
	SettlementOutcome     executionapp.SettlementOutcome
	SettlementBytes       []byte
	SettlementDigest      runtimedomain.Digest
	SettlementKey         string
	OccurredAt            time.Time
}

func (r outputRecord) validate() error {
	if r.EventID == "" || r.LogicalOutputID == "" || r.ExecutionID == "" || r.Generation == 0 || r.TenantID == "" || r.ResourceProjectID <= 0 || r.ProjectionProjectID <= 0 || r.CommandID == "" || r.WorkloadIdentity == "" || r.WorkloadSessionID == "" || r.ProducerID == "" || r.ClaimAttempt == 0 || r.LeaseEpoch == 0 || r.FenceToken.IsZero() || r.StreamID == "" || r.Sequence == 0 || r.PayloadDigest.IsZero() || len(r.PayloadBytes) == 0 || r.SettlementProposalID == "" || r.SettlementOutcome == "" || len(r.SettlementBytes) == 0 || r.SettlementDigest.IsZero() || r.SettlementKey == "" || r.OccurredAt.IsZero() {
		return outputapp.ErrInvalidValidationOutput
	}
	if r.PayloadType != payloadTypeConfigurationValidation && r.PayloadType != payloadTypeRuntimeFailure && r.PayloadType != payloadTypeIndexIngestResult && r.PayloadType != payloadTypeAgentExecutionResult && r.PayloadType != payloadTypeToolkitCallToolResult {
		return outputapp.ErrInvalidValidationOutput
	}
	// Keep the durable payload type and terminal disposition coupled even when
	// this lower-level repository is called without the transport/application
	// validators. In particular, cancellation authority must never turn an
	// ordinary validation result into an admissible terminal output.
	switch r.PayloadType {
	case payloadTypeConfigurationValidation:
		if r.SettlementOutcome != executionapp.SettlementSucceeded {
			return outputapp.ErrInvalidValidationOutput
		}
	case payloadTypeIndexIngestResult:
		if r.SettlementOutcome != executionapp.SettlementSucceeded &&
			r.SettlementOutcome != executionapp.SettlementFailed {
			return outputapp.ErrInvalidValidationOutput
		}
	case payloadTypeAgentExecutionResult:
		if r.SettlementOutcome != executionapp.SettlementSucceeded {
			return outputapp.ErrInvalidValidationOutput
		}
	case payloadTypeToolkitCallToolResult:
		// Both, because a tool result settles FAILED for the two refusals made
		// before any provider work and SUCCEEDED for everything else,
		// including a tool that raised.
		if r.SettlementOutcome != executionapp.SettlementSucceeded &&
			r.SettlementOutcome != executionapp.SettlementFailed {
			return outputapp.ErrInvalidValidationOutput
		}
	case payloadTypeRuntimeFailure:
		if r.SettlementOutcome != executionapp.SettlementFailed && r.SettlementOutcome != executionapp.SettlementCancelled {
			return outputapp.ErrInvalidValidationOutput
		}
	}
	if runtimedomain.SHA256(r.PayloadBytes) != r.PayloadDigest || runtimedomain.SHA256(r.SettlementBytes) != r.SettlementDigest {
		return outputapp.ErrInvalidValidationOutput
	}
	return nil
}

type outputInsertResult struct {
	Inserted             bool
	CancellationRejected bool
	DeadlineRejected     bool
}

func insertOutputInbox(ctx context.Context, tx sqlExecutor, record outputRecord) (outputInsertResult, error) {
	if err := record.validate(); err != nil {
		return outputInsertResult{}, err
	}
	// Keep the authority lock in its own READ COMMITTED statement. A query that
	// waits for this lock retains its original statement snapshot; the following
	// statement must freshly observe progress and competing terminal output.
	locked, err := lockOutputInboxAuthority(ctx, tx, record)
	if err != nil {
		return outputInsertResult{}, err
	}
	if !locked {
		return outputInsertResult{}, nil
	}
	canonicalDeadline := isCanonicalDeadlineOutput(record)
	var insertedClaimID, authorityClaimID, desiredState string
	var conflictExists, deadlineExpired, sequenceRejected bool
	err = tx.QueryRow(ctx, `
WITH authority AS MATERIALIZED (
    SELECT c.claim_id, j.desired_state,
           o.deadline <= clock_timestamp() AS deadline_expired
    FROM elitea_runtime.execution_claims AS c
    JOIN elitea_runtime.execution_jobs AS j
      ON j.execution_id = c.execution_id AND j.generation = c.generation
    JOIN elitea_runtime.command_outbox AS o
      ON o.execution_id = j.execution_id AND o.generation = j.generation
    WHERE c.execution_id = $3
      AND c.generation = $4
      AND j.tenant_id = $23
      AND j.resource_project_id = $24
      AND j.projection_project_id = $25
      AND j.command_id = $26
      AND c.workload_identity = $6
      AND c.workload_session_id = $7
      AND c.producer_id = $8
      AND c.claim_attempt = $9
      AND c.lease_epoch = $10
      AND c.fence_token = $5
      AND c.initial_output_watermark = $13
      AND c.released_at IS NULL
      AND c.lease_expires_at > clock_timestamp()
      AND o.retired_at IS NULL
      AND o.authority_granted_at IS NOT NULL
), previous_sequence AS MATERIALIZED (
    SELECT TRUE AS present
    FROM elitea_runtime.execution_replay_state
    WHERE execution_id = $3
      AND generation = $4
      AND last_node_sequence = $12::bigint - 1
      AND last_node_event_id = $26 || ':' || (($12::bigint - 1)::text)
    LIMIT 1
), conflicting_output AS MATERIALIZED (
    SELECT TRUE AS present
    FROM elitea_runtime.output_inbox
    WHERE event_id = $1
       OR (execution_id = $3 AND generation = $4 AND logical_output_id = $2)
       OR (execution_id = $3 AND generation = $4 AND producer_id = $8 AND sequence = $12)
    LIMIT 1
), sequence_conflict AS MATERIALIZED (
    SELECT TRUE AS present
    FROM elitea_runtime.execution_replay_events
    WHERE event_id = $1
    UNION ALL
    SELECT TRUE
    FROM elitea_runtime.execution_replay_state
    WHERE last_node_event_id = $1
    UNION ALL
    SELECT TRUE
    WHERE $12::bigint > 1
      AND NOT EXISTS (SELECT 1 FROM previous_sequence)
    LIMIT 1
), inserted AS (
    INSERT INTO elitea_runtime.output_inbox (
	    event_id, logical_output_id, execution_id, generation, claim_id,
	    fence_token, workload_identity, workload_session_id, producer_id,
	    claim_attempt, lease_epoch,
	        stream_id, sequence, claim_handoff_watermark, payload_type,
	        payload_digest, payload_bytes, settlement_proposal_id,
	        settlement_outcome, settlement_proposal_bytes,
	        settlement_proposal_digest, settlement_idempotency_key, occurred_at
	    )
	    SELECT $1, $2, $3, $4, authority.claim_id, $5, $6, $7, $8, $9, $10,
	           $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22
	    FROM authority
	    WHERE (
	          (
	           authority.desired_state = 'RUNNING'
	           AND (NOT authority.deadline_expired OR $27)
	          )
	       OR (
	           authority.desired_state = 'CANCELLED'
	           AND $14 = 'RUNTIME_FAILURE'
	           AND $18 = 'CANCELLED'
	          )
	      )
	      AND NOT EXISTS (SELECT 1 FROM conflicting_output)
	      AND NOT EXISTS (SELECT 1 FROM sequence_conflict)
	    ON CONFLICT DO NOTHING
	    RETURNING claim_id
)
SELECT COALESCE((SELECT claim_id FROM inserted LIMIT 1), ''),
       COALESCE((SELECT claim_id FROM authority LIMIT 1), ''),
       COALESCE((SELECT desired_state FROM authority LIMIT 1), ''),
       COALESCE((SELECT present FROM conflicting_output LIMIT 1), FALSE),
       COALESCE((SELECT deadline_expired FROM authority LIMIT 1), FALSE),
       COALESCE((SELECT present FROM sequence_conflict LIMIT 1), FALSE)`,
		record.EventID,
		record.LogicalOutputID,
		record.ExecutionID,
		int64(record.Generation),
		record.FenceToken[:],
		record.WorkloadIdentity,
		record.WorkloadSessionID,
		record.ProducerID,
		int64(record.ClaimAttempt),
		int64(record.LeaseEpoch),
		record.StreamID,
		int64(record.Sequence),
		int64(record.ClaimHandoffWatermark),
		record.PayloadType,
		record.PayloadDigest[:],
		record.PayloadBytes,
		record.SettlementProposalID,
		string(record.SettlementOutcome),
		record.SettlementBytes,
		record.SettlementDigest[:],
		record.SettlementKey,
		record.OccurredAt.UTC(),
		record.TenantID,
		record.ResourceProjectID,
		record.ProjectionProjectID,
		record.CommandID,
		canonicalDeadline,
	).Scan(&insertedClaimID, &authorityClaimID, &desiredState, &conflictExists, &deadlineExpired, &sequenceRejected)
	if err != nil {
		return outputInsertResult{}, fmt.Errorf("insert output inbox: %w", err)
	}
	if insertedClaimID != "" {
		if authorityClaimID == "" || insertedClaimID != authorityClaimID {
			return outputInsertResult{}, errors.New("insert output inbox returned mismatched authority")
		}
		return outputInsertResult{Inserted: true}, nil
	}
	if sequenceRejected {
		return outputInsertResult{}, outputapp.ErrValidationOutputConflict
	}
	cancellationRejected := authorityClaimID != "" && desiredState == string(runtimedomain.DesiredCancelled) && !conflictExists && (record.PayloadType != payloadTypeRuntimeFailure || record.SettlementOutcome != executionapp.SettlementCancelled)
	deadlineRejected := authorityClaimID != "" && desiredState == string(runtimedomain.DesiredRunning) && deadlineExpired && !conflictExists && !canonicalDeadline
	return outputInsertResult{CancellationRejected: cancellationRejected, DeadlineRejected: deadlineRejected}, nil
}

func lockOutputInboxAuthority(ctx context.Context, tx sqlExecutor, record outputRecord) (bool, error) {
	var claimID string
	err := tx.QueryRow(ctx, `
SELECT c.claim_id
FROM elitea_runtime.execution_claims AS c
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = c.execution_id AND j.generation = c.generation
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
WHERE c.execution_id = $1
  AND c.generation = $2
  AND j.tenant_id = $3
  AND j.resource_project_id = $4
  AND j.projection_project_id = $5
  AND j.command_id = $6
  AND c.workload_identity = $7
  AND c.workload_session_id = $8
  AND c.producer_id = $9
  AND c.claim_attempt = $10
  AND c.lease_epoch = $11
  AND c.fence_token = $12
  AND c.initial_output_watermark = $13
  AND c.released_at IS NULL
  AND c.lease_expires_at > clock_timestamp()
  AND o.retired_at IS NULL
  AND o.authority_granted_at IS NOT NULL
FOR UPDATE OF j, o, c`,
		record.ExecutionID,
		int64(record.Generation),
		record.TenantID,
		record.ResourceProjectID,
		record.ProjectionProjectID,
		record.CommandID,
		record.WorkloadIdentity,
		record.WorkloadSessionID,
		record.ProducerID,
		int64(record.ClaimAttempt),
		int64(record.LeaseEpoch),
		record.FenceToken[:],
		int64(record.ClaimHandoffWatermark),
	).Scan(&claimID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock output inbox authority: %w", err)
	}
	if claimID == "" {
		return false, errors.New("lock output inbox authority returned an empty claim")
	}
	return true, nil
}

func isCanonicalDeadlineOutput(record outputRecord) bool {
	if record.PayloadType != payloadTypeRuntimeFailure || record.SettlementOutcome != executionapp.SettlementFailed {
		return false
	}
	want := &runtimev1.RuntimeErrorV1{
		Code:        runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED,
		SafeMessage: outputapp.DeadlineExceededSafeMessage,
		Retryable:   true,
	}
	wantBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(want)
	if err != nil || !bytes.Equal(record.PayloadBytes, wantBytes) {
		return false
	}
	return record.PayloadDigest == runtimedomain.SHA256(wantBytes)
}

func loadExistingOutput(ctx context.Context, tx sqlExecutor, wanted outputRecord) (outputRecord, error) {
	byEvent, eventErr := loadOutputByIdentity(ctx, tx, `event_id = $1`, wanted.EventID)
	byLogical, logicalErr := loadOutputByIdentity(ctx, tx, `execution_id = $1 AND generation = $2 AND logical_output_id = $3`, wanted.ExecutionID, int64(wanted.Generation), wanted.LogicalOutputID)
	byProducerSequence, producerSequenceErr := loadOutputByIdentity(
		ctx,
		tx,
		`execution_id = $1 AND generation = $2 AND producer_id = $3 AND sequence = $4`,
		wanted.ExecutionID,
		int64(wanted.Generation),
		wanted.ProducerID,
		int64(wanted.Sequence),
	)

	var existing *outputRecord
	for _, candidate := range []struct {
		record outputRecord
		err    error
	}{
		{record: byEvent, err: eventErr},
		{record: byLogical, err: logicalErr},
		{record: byProducerSequence, err: producerSequenceErr},
	} {
		if errors.Is(candidate.err, pgx.ErrNoRows) {
			continue
		}
		if candidate.err != nil {
			return outputRecord{}, candidate.err
		}
		if existing != nil && (existing.EventID != candidate.record.EventID || existing.ExecutionID != candidate.record.ExecutionID || existing.Generation != candidate.record.Generation) {
			return outputRecord{}, outputapp.ErrValidationOutputConflict
		}
		copy := candidate.record
		existing = &copy
	}
	if existing == nil {
		return outputRecord{}, pgx.ErrNoRows
	}
	return *existing, nil
}

func loadOutputByIdentity(ctx context.Context, tx sqlExecutor, predicate string, args ...any) (outputRecord, error) {
	var record outputRecord
	var generation, claimAttempt, leaseEpoch, sequence, watermark int64
	var token, digest, settlementDigest []byte
	var settlementOutcome string
	query := `
SELECT event_id, logical_output_id, execution_id, generation,
       workload_identity, workload_session_id, producer_id,
       claim_attempt, lease_epoch,
	       fence_token, stream_id, sequence, claim_handoff_watermark,
	       payload_type, payload_digest, payload_bytes, settlement_proposal_id,
	       settlement_outcome, settlement_proposal_bytes,
	       settlement_proposal_digest, settlement_idempotency_key, occurred_at
FROM elitea_runtime.output_inbox
WHERE ` + predicate
	err := tx.QueryRow(ctx, query, args...).Scan(
		&record.EventID,
		&record.LogicalOutputID,
		&record.ExecutionID,
		&generation,
		&record.WorkloadIdentity,
		&record.WorkloadSessionID,
		&record.ProducerID,
		&claimAttempt,
		&leaseEpoch,
		&token,
		&record.StreamID,
		&sequence,
		&watermark,
		&record.PayloadType,
		&digest,
		&record.PayloadBytes,
		&record.SettlementProposalID,
		&settlementOutcome,
		&record.SettlementBytes,
		&settlementDigest,
		&record.SettlementKey,
		&record.OccurredAt,
	)
	if err != nil {
		return outputRecord{}, err
	}
	if generation <= 0 || claimAttempt <= 0 || leaseEpoch <= 0 || sequence <= 0 || watermark < 0 || len(token) != len(record.FenceToken) {
		return outputRecord{}, outputapp.ErrValidationOutputConflict
	}
	record.Generation = uint64(generation)
	record.ClaimAttempt = uint64(claimAttempt)
	record.LeaseEpoch = uint64(leaseEpoch)
	record.Sequence = uint64(sequence)
	record.ClaimHandoffWatermark = uint64(watermark)
	copy(record.FenceToken[:], token)
	var digestErr error
	record.PayloadDigest, digestErr = storedDigest(digest)
	if digestErr != nil {
		return outputRecord{}, outputapp.ErrValidationOutputConflict
	}
	record.SettlementDigest, digestErr = storedDigest(settlementDigest)
	if digestErr != nil {
		return outputRecord{}, outputapp.ErrValidationOutputConflict
	}
	record.SettlementOutcome = executionapp.SettlementOutcome(settlementOutcome)
	return record, nil
}

func sameDurableOutput(existing, wanted outputRecord) bool {
	return existing.EventID == wanted.EventID &&
		existing.LogicalOutputID == wanted.LogicalOutputID &&
		existing.ExecutionID == wanted.ExecutionID &&
		existing.Generation == wanted.Generation &&
		existing.WorkloadIdentity == wanted.WorkloadIdentity &&
		existing.WorkloadSessionID == wanted.WorkloadSessionID &&
		existing.ProducerID == wanted.ProducerID &&
		existing.ClaimAttempt == wanted.ClaimAttempt &&
		existing.LeaseEpoch == wanted.LeaseEpoch &&
		existing.FenceToken == wanted.FenceToken &&
		existing.StreamID == wanted.StreamID &&
		existing.Sequence == wanted.Sequence &&
		existing.ClaimHandoffWatermark == wanted.ClaimHandoffWatermark &&
		existing.PayloadType == wanted.PayloadType &&
		existing.PayloadDigest == wanted.PayloadDigest &&
		bytes.Equal(existing.PayloadBytes, wanted.PayloadBytes) &&
		existing.SettlementProposalID == wanted.SettlementProposalID &&
		existing.SettlementOutcome == wanted.SettlementOutcome &&
		existing.SettlementDigest == wanted.SettlementDigest &&
		existing.SettlementKey == wanted.SettlementKey &&
		bytes.Equal(existing.SettlementBytes, wanted.SettlementBytes) &&
		existing.OccurredAt.Equal(wanted.OccurredAt)
}

func replayCursor(ctx context.Context, tx sqlExecutor, eventID string) (uint64, error) {
	var cursor int64
	if err := tx.QueryRow(ctx, `
SELECT cursor
FROM elitea_runtime.execution_replay_events
WHERE event_id = $1`, eventID).Scan(&cursor); err != nil {
		return 0, err
	}
	if cursor <= 0 {
		return 0, errors.New("invalid replay cursor")
	}
	return uint64(cursor), nil
}

var _ outputapp.ValidationBindingRepository = (*OutputInboxRepository)(nil)
var _ outputapp.RuntimeFailureBindingRepository = (*OutputInboxRepository)(nil)
