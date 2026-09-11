package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	configurationdomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/configurations"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

const (
	replayEventConfigurationValidation = "configuration.validation.completed"
	// EliteaUI already listens for execution.failed. Keep the durable replay
	// event compatible so a failed or cancelled indexing run cannot leave the
	// existing UI waiting indefinitely for a terminal event.
	replayEventRuntimeFailure = "execution.failed"
)

type ConfigurationValidationResultsRepository struct {
	projects projectStore
}

// RuntimeFailureResultsRepository owns the capability-neutral runtime failure
// envelope and the index-only current Activity side effect. Keeping it separate
// from configuration validation prevents configuration failures from acquiring
// index projection dependencies or queries.
type RuntimeFailureResultsRepository struct {
	projects projectStore
	activity currentIndexActivityProjector
}

func NewConfigurationValidationResultsRepository(pool *pgxpool.Pool) (*ConfigurationValidationResultsRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return &ConfigurationValidationResultsRepository{
		projects: projects,
	}, nil
}

func newConfigurationValidationResultsRepository(projects projectStore) (*ConfigurationValidationResultsRepository, error) {
	if projects == nil {
		return nil, errors.New("tenant projection database is required")
	}
	return &ConfigurationValidationResultsRepository{
		projects: projects,
	}, nil
}

func NewRuntimeFailureResultsRepository(pool *pgxpool.Pool) (*RuntimeFailureResultsRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return &RuntimeFailureResultsRepository{
		projects: projects,
		activity: &postgresCurrentIndexActivityProjector{},
	}, nil
}

func newRuntimeFailureResultsRepository(projects projectStore) (*RuntimeFailureResultsRepository, error) {
	if projects == nil {
		return nil, errors.New("tenant projection database is required")
	}
	return &RuntimeFailureResultsRepository{
		projects: projects,
		activity: noopCurrentIndexActivityProjector{},
	}, nil
}

func (r *ConfigurationValidationResultsRepository) ProjectConfigurationValidation(ctx context.Context, projection outputapp.ValidationProjection) (outputapp.ProjectionOutcome, error) {
	if err := projection.Frame.Validate(); err != nil {
		return outputapp.ProjectionOutcome{}, err
	}
	if len(projection.BrowserData) == 0 || len(projection.BrowserData) > outputapp.MaxConfigurationValidationResultBytes || !json.Valid(projection.BrowserData) {
		return outputapp.ProjectionOutcome{}, outputapp.ErrInvalidValidationOutput
	}
	record, projectID, err := validationOutputRecord(projection.Frame)
	if err != nil {
		return outputapp.ProjectionOutcome{}, err
	}
	issuesJSON, err := validationIssuesJSON(projection.Frame.Result.Issues)
	if err != nil {
		return outputapp.ProjectionOutcome{}, fmt.Errorf("encode durable validation issues: %w", err)
	}

	var outcome outputapp.ProjectionOutcome
	cancellationWon := false
	err = r.projects.WithinProjectTx(ctx, projectID, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		existing, err := loadExistingOutput(ctx, tx, record)
		if err == nil {
			if sameDurableOutput(existing, record) {
				cursor, err := replayCursor(ctx, tx, record.EventID)
				if err != nil {
					return fmt.Errorf("load replayed validation cursor: %w", err)
				}
				outcome = outputapp.ProjectionOutcome{Inserted: false, Cursor: cursor, CommittedSequence: record.Sequence}
				return nil
			}
			if sameCanonicalCancellation(existing, record) {
				if err := persistCurrentIndexMetaTerminalIntent(ctx, tx, existing); err != nil {
					return err
				}
				if _, err := replayCursor(ctx, tx, record.EventID); err != nil {
					return fmt.Errorf("load materialized cancellation cursor: %w", err)
				}
				cancellationWon = true
				return nil
			}
			if !sameDurableOutput(existing, record) {
				return outputapp.ErrValidationOutputConflict
			}
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		insertResult, err := insertOutputInbox(ctx, tx, record)
		if err != nil {
			return err
		}
		if !insertResult.Inserted {
			existing, loadErr := loadExistingOutput(ctx, tx, record)
			if loadErr == nil {
				if sameDurableOutput(existing, record) {
					cursor, cursorErr := replayCursor(ctx, tx, record.EventID)
					if cursorErr != nil {
						return cursorErr
					}
					outcome = outputapp.ProjectionOutcome{Inserted: false, Cursor: cursor, CommittedSequence: record.Sequence}
					return nil
				}
				if sameCanonicalCancellation(existing, record) {
					if err := persistCurrentIndexMetaTerminalIntent(ctx, tx, existing); err != nil {
						return err
					}
					if _, cursorErr := replayCursor(ctx, tx, record.EventID); cursorErr != nil {
						return cursorErr
					}
					cancellationWon = true
					return nil
				}
				return outputapp.ErrValidationOutputConflict
			}
			if errors.Is(loadErr, pgx.ErrNoRows) {
				if insertResult.CancellationRejected {
					if err := materializeCanonicalCancellation(ctx, tx, record, false); err != nil {
						return err
					}
					cancellationWon = true
					return nil
				}
				if insertResult.DeadlineRejected {
					return outputapp.ErrOutputDeadlineExceeded
				}
				return runtimedomain.ErrStaleFence
			}
			return loadErr
		}

		binding := projection.Frame.Result.Binding
		command := binding.Command
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.configuration_validation_results (
    logical_output_id, execution_id, generation, configuration_revision_id,
    configuration_type, catalog_revision, catalog_digest, schema_id,
    schema_revision, schema_digest, input_bundle_id, input_bundle_digest,
    settings_entry_id, settings_entry_version, settings_content_digest,
    valid, issues_json
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
    $15, $16, $17
)`,
			record.LogicalOutputID,
			record.ExecutionID,
			int64(record.Generation),
			command.ConfigurationRevisionID,
			command.ConfigurationType,
			command.CatalogRevision,
			command.CatalogDigest[:],
			command.SchemaID,
			command.SchemaRevision,
			command.SchemaDigest[:],
			binding.InputBundleID,
			binding.InputBundleDigest[:],
			command.SettingsEntryID,
			binding.SettingsEntryVersion,
			binding.SettingsContentDigest[:],
			projection.Frame.Result.Valid,
			issuesJSON,
		); err != nil {
			return fmt.Errorf("insert configuration validation result: %w", err)
		}

		var projectedRevision string
		err = tx.QueryRow(ctx, `
INSERT INTO configuration_validation_projection (
    revision_id, execution_id, execution_generation, logical_output_id,
    valid, issues_json, projected_at
)
SELECT revision_id, $2, $3, $4, $5, $6, clock_timestamp()
FROM configuration_revisions
WHERE revision_id = $1
RETURNING revision_id`,
			command.ConfigurationRevisionID,
			record.ExecutionID,
			int64(record.Generation),
			record.LogicalOutputID,
			projection.Frame.Result.Valid,
			issuesJSON,
		).Scan(&projectedRevision)
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("configuration revision is not present in the authorized projection project")
		}
		if err != nil {
			return fmt.Errorf("insert tenant validation projection: %w", err)
		}
		if projectedRevision != command.ConfigurationRevisionID {
			return errors.New("tenant validation projection returned a mismatched revision")
		}

		cursor, err := appendReplayEvent(ctx, tx, record, replayEventConfigurationValidation, projection.BrowserData)
		if err != nil {
			return err
		}
		if err := markOutputProjected(ctx, tx, record.EventID); err != nil {
			return err
		}
		outcome = outputapp.ProjectionOutcome{Inserted: true, Cursor: cursor, CommittedSequence: record.Sequence}
		return nil
	})
	if err != nil {
		return outputapp.ProjectionOutcome{}, err
	}
	if cancellationWon {
		// The transaction above has committed a canonical terminal cancellation
		// (or proved its exact replay) before the transport is authorized to tell
		// the worker to replace its local frame.
		return outputapp.ProjectionOutcome{}, outputapp.ErrOutputCancelled
	}
	return outcome, nil
}

func (r *RuntimeFailureResultsRepository) ProjectRuntimeFailure(ctx context.Context, projection outputapp.RuntimeFailureProjection) (outputapp.ProjectionOutcome, error) {
	if err := projection.Frame.Validate(); err != nil {
		return outputapp.ProjectionOutcome{}, err
	}
	switch projection.CapabilityID {
	case executiondomain.ConfigurationValidationCapability,
		executiondomain.IndexIngestCapability,
		executiondomain.AgentApplicationCapability,
		executiondomain.AgentAdhocCapability,
		executiondomain.ToolkitExecuteReadCapability,
		executiondomain.ToolkitCallToolCapability,
		executiondomain.ToolkitAvailableToolsCapability:
	default:
		return outputapp.ProjectionOutcome{}, outputapp.ErrInvalidValidationOutput
	}
	if len(projection.BrowserData) == 0 || len(projection.BrowserData) > outputapp.MaxConfigurationValidationResultBytes || !json.Valid(projection.BrowserData) {
		return outputapp.ProjectionOutcome{}, outputapp.ErrInvalidValidationOutput
	}
	record, projectID, err := failureOutputRecord(projection.Frame)
	if err != nil {
		return outputapp.ProjectionOutcome{}, err
	}

	var outcome outputapp.ProjectionOutcome
	cancellationWon := false
	err = r.projects.WithinProjectTx(ctx, projectID, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		existing, err := loadExistingOutput(ctx, tx, record)
		if err == nil {
			if sameDurableOutput(existing, record) {
				if err := persistRuntimeIndexMetaTerminalIntent(
					ctx, tx, projection.CapabilityID, record,
				); err != nil {
					return err
				}
				if err := persistRuntimeIndexTerminalNotification(
					ctx,
					tx,
					projection.CapabilityID,
					record,
					projection.Frame.Failure.SafeMessage,
				); err != nil {
					return err
				}
				cursor, err := replayCursor(ctx, tx, record.EventID)
				if err != nil {
					return fmt.Errorf("load replayed failure cursor: %w", err)
				}
				outcome = outputapp.ProjectionOutcome{Inserted: false, Cursor: cursor, CommittedSequence: record.Sequence}
				return nil
			}
			if sameCanonicalCancellation(existing, record) {
				if err := persistRuntimeIndexMetaTerminalIntent(
					ctx, tx, projection.CapabilityID, existing,
				); err != nil {
					return err
				}
				if _, err := replayCursor(ctx, tx, record.EventID); err != nil {
					return fmt.Errorf("load materialized failure cancellation cursor: %w", err)
				}
				cancellationWon = true
				return nil
			}
			return outputapp.ErrValidationOutputConflict
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		insertResult, err := insertOutputInbox(ctx, tx, record)
		if err != nil {
			return err
		}
		if !insertResult.Inserted {
			existing, loadErr := loadExistingOutput(ctx, tx, record)
			if loadErr == nil {
				if sameDurableOutput(existing, record) {
					if err := persistRuntimeIndexMetaTerminalIntent(
						ctx, tx, projection.CapabilityID, record,
					); err != nil {
						return err
					}
					if err := persistRuntimeIndexTerminalNotification(
						ctx,
						tx,
						projection.CapabilityID,
						record,
						projection.Frame.Failure.SafeMessage,
					); err != nil {
						return err
					}
					cursor, cursorErr := replayCursor(ctx, tx, record.EventID)
					if cursorErr != nil {
						return cursorErr
					}
					outcome = outputapp.ProjectionOutcome{Inserted: false, Cursor: cursor, CommittedSequence: record.Sequence}
					return nil
				}
				if sameCanonicalCancellation(existing, record) {
					if err := persistRuntimeIndexMetaTerminalIntent(
						ctx, tx, projection.CapabilityID, existing,
					); err != nil {
						return err
					}
					if _, cursorErr := replayCursor(ctx, tx, record.EventID); cursorErr != nil {
						return cursorErr
					}
					cancellationWon = true
					return nil
				}
				return outputapp.ErrValidationOutputConflict
			}
			if errors.Is(loadErr, pgx.ErrNoRows) {
				if insertResult.CancellationRejected {
					if err := materializeCanonicalCancellation(
						ctx, tx, record, projection.CapabilityID == executiondomain.IndexIngestCapability,
					); err != nil {
						return err
					}
					if err := r.projectCurrentIndexActivityTerminal(
						ctx, tx, projectID, projection.CapabilityID, currentIndexActivityCancellation(record),
					); err != nil {
						return err
					}
					cancellationWon = true
					return nil
				}
				if insertResult.DeadlineRejected {
					return outputapp.ErrOutputDeadlineExceeded
				}
				return runtimedomain.ErrStaleFence
			}
			return loadErr
		}
		cursor, err := appendReplayEvent(ctx, tx, record, replayEventRuntimeFailure, projection.BrowserData)
		if err != nil {
			return err
		}
		if err := r.projectCurrentIndexActivityTerminal(ctx, tx, projectID, projection.CapabilityID, currentIndexActivityTerminal{
			ExecutionID: record.ExecutionID,
			Generation:  record.Generation,
			OccurredAt:  record.OccurredAt,
			Message:     projection.Frame.Failure.SafeMessage,
			IsError:     true,
		}); err != nil {
			return err
		}
		if err := persistCurrentAgentRuntimeTerminal(
			ctx,
			tx,
			projectID,
			projection.CapabilityID,
			record,
			projection.Frame.Failure.Code,
			projection.Frame.Failure.SafeMessage,
		); err != nil {
			return err
		}
		if err := markOutputProjected(ctx, tx, record.EventID); err != nil {
			return err
		}
		if err := persistRuntimeIndexMetaTerminalIntent(
			ctx, tx, projection.CapabilityID, record,
		); err != nil {
			return err
		}
		if err := persistRuntimeIndexTerminalNotification(
			ctx,
			tx,
			projection.CapabilityID,
			record,
			projection.Frame.Failure.SafeMessage,
		); err != nil {
			return err
		}
		outcome = outputapp.ProjectionOutcome{Inserted: true, Cursor: cursor, CommittedSequence: record.Sequence}
		return nil
	})
	if err != nil {
		return outputapp.ProjectionOutcome{}, err
	}
	if cancellationWon {
		return outputapp.ProjectionOutcome{}, outputapp.ErrOutputCancelled
	}
	return outcome, nil
}

func persistCurrentAgentRuntimeTerminal(
	ctx context.Context,
	tx sqlExecutor,
	projectID int64,
	capabilityID string,
	record outputRecord,
	failureCode string,
	safeMessage string,
) error {
	if capabilityID != executiondomain.AgentApplicationCapability &&
		capabilityID != executiondomain.AgentAdhocCapability {
		return nil
	}
	if safeMessage == "" || len(safeMessage) > 256 {
		return outputapp.ErrInvalidValidationOutput
	}
	schema, err := currentProjectSchema(projectID)
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s AS message_group
SET is_streaming = FALSE,
    meta = CASE
        WHEN $4::boolean THEN message_group.meta - 'is_error' - 'error'
        ELSE message_group.meta || jsonb_build_object(
            'is_error', TRUE,
            'error', $3::text
        )
    END,
    updated_at = clock_timestamp()
FROM elitea_runtime.agent_execution_jobs AS agent,
     %s AS conversation
WHERE agent.execution_id = $1
  AND agent.generation = $2
  AND agent.capability_id IN (
      'agent.execute.application.v1',
      'agent.execute.adhoc.v1'
  )
  AND message_group.uuid::text = agent.client_message_id
  AND conversation.id = message_group.conversation_id
  AND conversation.uuid::text = agent.client_stream_id
  AND message_group.task_id = agent.execution_id
  AND message_group.meta->>'execution_generation' =
      agent.client_execution_generation`,
		schema+".chat_message_group",
		schema+".chat_conversations",
	), record.ExecutionID, int64(record.Generation), safeMessage, failureCode == "CANCELLED")
	if err != nil {
		return fmt.Errorf("finalize current agent terminal state: %w", err)
	}
	if result.RowsAffected() == 0 && failureCode == "CANCELLED" {
		// The synchronous Stop projection intentionally removes an empty response
		// group (and its question) instead of leaving a blank cancelled turn in
		// the current chat schema. The later canonical runtime cancellation still
		// has to settle the durable execution and replay stream.
		return nil
	}
	if result.RowsAffected() != 1 {
		return errors.New("current agent terminal response message group is unavailable")
	}
	return nil
}

func (r *RuntimeFailureResultsRepository) projectCurrentIndexActivityTerminal(
	ctx context.Context,
	tx sqlExecutor,
	projectID int64,
	capabilityID string,
	terminal currentIndexActivityTerminal,
) error {
	if capabilityID != executiondomain.IndexIngestCapability {
		return nil
	}
	return r.activity.projectTerminal(ctx, tx, projectID, terminal)
}

func persistRuntimeIndexMetaTerminalIntent(
	ctx context.Context,
	tx sqlExecutor,
	capabilityID string,
	record outputRecord,
) error {
	if capabilityID != executiondomain.IndexIngestCapability {
		return nil
	}
	return persistCurrentIndexMetaTerminalIntent(ctx, tx, record)
}

func persistRuntimeIndexTerminalNotification(
	ctx context.Context,
	tx sqlExecutor,
	capabilityID string,
	record outputRecord,
	safeMessage string,
) error {
	if capabilityID != executiondomain.IndexIngestCapability {
		return nil
	}
	return persistCurrentIndexTerminalNotification(
		ctx,
		tx,
		record,
		outputapp.IndexIngestSummary{
			Status:        outputapp.IndexIngestStatusError,
			Message:       safeMessage,
			TerminalState: outputapp.IndexIngestTerminalFailed,
		},
	)
}

func validationOutputRecord(frame outputapp.ConfigurationValidationFrame) (outputRecord, int64, error) {
	resourceProjectID, err := parseProjectID(frame.ResourceProjectID)
	if err != nil {
		return outputRecord{}, 0, outputapp.ErrInvalidValidationOutput
	}
	projectionProjectID, err := parseProjectID(frame.ProjectionProjectID)
	if err != nil {
		return outputRecord{}, 0, outputapp.ErrInvalidValidationOutput
	}
	return frameOutputRecord(
		frame.EventID, frame.LogicalOutputID, frame.StreamID, frame.TenantID,
		resourceProjectID, projectionProjectID, frame.Sequence,
		frame.ClaimHandoffWatermark, frame.OccurredAt, frame.Fence,
		payloadTypeConfigurationValidation, frame.PayloadDigest, frame.EncodedResult,
		frame.Settlement, frame.EncodedSettlement,
	), projectionProjectID, nil
}

func failureOutputRecord(frame outputapp.RuntimeFailureFrame) (outputRecord, int64, error) {
	resourceProjectID, err := parseProjectID(frame.ResourceProjectID)
	if err != nil {
		return outputRecord{}, 0, outputapp.ErrInvalidValidationOutput
	}
	projectionProjectID, err := parseProjectID(frame.ProjectionProjectID)
	if err != nil {
		return outputRecord{}, 0, outputapp.ErrInvalidValidationOutput
	}
	return frameOutputRecord(
		frame.EventID, frame.LogicalOutputID, frame.StreamID, frame.TenantID,
		resourceProjectID, projectionProjectID, frame.Sequence,
		frame.ClaimHandoffWatermark, frame.OccurredAt, frame.Fence,
		payloadTypeRuntimeFailure, frame.PayloadDigest, frame.EncodedFailure,
		frame.Settlement, frame.EncodedSettlement,
	), projectionProjectID, nil
}

func frameOutputRecord(eventID, logicalOutputID, streamID, tenantID string, resourceProjectID, projectionProjectID int64, sequence, watermark uint64, occurredAt time.Time, fence runtimedomain.Fence, payloadType string, payloadDigest runtimedomain.Digest, payload []byte, settlement executionapp.SettlementProposal, encodedSettlement []byte) outputRecord {
	return outputRecord{
		EventID:               eventID,
		LogicalOutputID:       logicalOutputID,
		ExecutionID:           fence.ExecutionID,
		Generation:            fence.Generation,
		TenantID:              tenantID,
		ResourceProjectID:     resourceProjectID,
		ProjectionProjectID:   projectionProjectID,
		CommandID:             fence.CommandID,
		WorkloadIdentity:      fence.WorkloadIdentity,
		WorkloadSessionID:     fence.WorkloadSessionID,
		ProducerID:            fence.ProducerID,
		ClaimAttempt:          fence.ClaimAttempt,
		LeaseEpoch:            fence.LeaseEpoch,
		FenceToken:            fence.Token,
		StreamID:              streamID,
		Sequence:              sequence,
		ClaimHandoffWatermark: watermark,
		PayloadType:           payloadType,
		PayloadDigest:         payloadDigest,
		PayloadBytes:          append([]byte(nil), payload...),
		SettlementProposalID:  settlement.ProposalID,
		SettlementOutcome:     settlement.Outcome,
		SettlementBytes:       append([]byte(nil), encodedSettlement...),
		SettlementDigest:      settlement.ProposalDigest,
		SettlementKey:         settlement.IdempotencyKey,
		OccurredAt:            occurredAt.UTC(),
	}
}

func canonicalCancellationOutput(source outputRecord) (outputRecord, []byte, error) {
	if err := source.validate(); err != nil {
		return outputRecord{}, nil, outputapp.ErrInvalidValidationOutput
	}
	switch source.PayloadType {
	case payloadTypeConfigurationValidation, payloadTypeIndexIngestResult,
		payloadTypeAgentExecutionResult, payloadTypeToolkitExecuteReadResult:
		if source.SettlementOutcome != executionapp.SettlementSucceeded {
			return outputRecord{}, nil, outputapp.ErrInvalidValidationOutput
		}
	case payloadTypeRuntimeFailure:
		if source.SettlementOutcome != executionapp.SettlementFailed {
			return outputRecord{}, nil, outputapp.ErrInvalidValidationOutput
		}
	default:
		return outputRecord{}, nil, outputapp.ErrInvalidValidationOutput
	}
	failure := &runtimev1.RuntimeErrorV1{
		Code:        runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_CANCELLED,
		SafeMessage: "Execution was cancelled.",
		Retryable:   false,
	}
	payloadBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(failure)
	if err != nil {
		return outputRecord{}, nil, fmt.Errorf("encode canonical cancellation payload: %w", err)
	}
	payloadDigest := runtimedomain.SHA256(payloadBytes)
	wireProposal := &runtimev1.SettlementProposalV1{
		ProposalId:              source.SettlementProposalID,
		RequestedOutcome:        runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_CANCELLED,
		TerminalLogicalOutputId: source.LogicalOutputID,
		TerminalEventId:         source.EventID,
		TerminalSequence:        source.Sequence,
		TerminalPayloadDigest: &runtimev1.DigestV1{
			Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
			Value:     append([]byte(nil), payloadDigest[:]...),
		},
		PrepareIdempotencyKey: source.SettlementKey,
	}
	settlementBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(wireProposal)
	if err != nil {
		return outputRecord{}, nil, fmt.Errorf("encode canonical cancellation settlement: %w", err)
	}
	cancelled := source
	cancelled.PayloadType = payloadTypeRuntimeFailure
	cancelled.PayloadDigest = payloadDigest
	cancelled.PayloadBytes = payloadBytes
	cancelled.SettlementOutcome = executionapp.SettlementCancelled
	cancelled.SettlementBytes = settlementBytes
	cancelled.SettlementDigest = runtimedomain.SHA256(settlementBytes)
	if err := cancelled.validate(); err != nil {
		return outputRecord{}, nil, err
	}
	browserData, err := json.Marshal(struct {
		Code        string `json:"code"`
		SafeMessage string `json:"safe_message"`
		Retryable   bool   `json:"retryable"`
	}{Code: "CANCELLED", SafeMessage: failure.GetSafeMessage(), Retryable: failure.GetRetryable()})
	if err != nil {
		return outputRecord{}, nil, fmt.Errorf("encode canonical cancellation browser event: %w", err)
	}
	return cancelled, browserData, nil
}

func sameCanonicalCancellation(existing, source outputRecord) bool {
	cancelled, _, err := canonicalCancellationOutput(source)
	return err == nil && sameDurableOutput(existing, cancelled)
}

func materializeCanonicalCancellation(
	ctx context.Context,
	tx sqlExecutor,
	source outputRecord,
	persistIndexMeta bool,
) error {
	cancelled, browserData, err := canonicalCancellationOutput(source)
	if err != nil {
		return err
	}
	insertResult, err := insertOutputInbox(ctx, tx, cancelled)
	if err != nil {
		return err
	}
	if !insertResult.Inserted {
		existing, loadErr := loadExistingOutput(ctx, tx, cancelled)
		if loadErr != nil {
			if errors.Is(loadErr, pgx.ErrNoRows) {
				return runtimedomain.ErrStaleFence
			}
			return loadErr
		}
		if !sameDurableOutput(existing, cancelled) {
			return outputapp.ErrValidationOutputConflict
		}
		if _, err := replayCursor(ctx, tx, cancelled.EventID); err != nil {
			return fmt.Errorf("load canonical cancellation cursor: %w", err)
		}
		if persistIndexMeta {
			return persistCurrentIndexMetaTerminalIntent(ctx, tx, existing)
		}
		return nil
	}
	if _, err := appendReplayEvent(ctx, tx, cancelled, replayEventRuntimeFailure, browserData); err != nil {
		return err
	}
	if err := markOutputProjected(ctx, tx, cancelled.EventID); err != nil {
		return err
	}
	if persistIndexMeta {
		return persistCurrentIndexMetaTerminalIntent(ctx, tx, cancelled)
	}
	return nil
}

func validationIssuesJSON(issues []configurationdomain.ValidationIssue) ([]byte, error) {
	type durableIssue struct {
		Code        string `json:"code"`
		JSONPointer string `json:"json_pointer"`
		SafeMessage string `json:"safe_message"`
	}
	encoded := make([]durableIssue, len(issues))
	for i, issue := range issues {
		encoded[i] = durableIssue{Code: issue.Code, JSONPointer: issue.JSONPointer, SafeMessage: issue.SafeMessage}
	}
	return json.Marshal(encoded)
}

func appendReplayEvent(ctx context.Context, tx sqlExecutor, record outputRecord, eventType string, eventBytes []byte) (uint64, error) {
	digest := runtimedomain.SHA256(eventBytes)
	var cursor int64
	err := tx.QueryRow(ctx, `
INSERT INTO elitea_runtime.execution_replay_events (
    event_id, execution_id, generation, projection_project_id,
    event_type, event_bytes, event_digest
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING cursor`,
		record.EventID,
		record.ExecutionID,
		int64(record.Generation),
		record.ProjectionProjectID,
		eventType,
		eventBytes,
		digest[:],
	).Scan(&cursor)
	if err != nil {
		return 0, fmt.Errorf("append durable execution replay event: %w", err)
	}
	if cursor <= 0 {
		return 0, errors.New("replay event returned invalid cursor")
	}
	return uint64(cursor), nil
}

func markOutputProjected(ctx context.Context, tx sqlExecutor, eventID string) error {
	tag, err := tx.Exec(ctx, `
UPDATE elitea_runtime.output_inbox
SET projected_at = COALESCE(projected_at, clock_timestamp())
WHERE event_id = $1`, eventID)
	if err != nil {
		return fmt.Errorf("mark output inbox projected: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("output inbox was not marked projected")
	}
	return nil
}

// persistCurrentIndexMetaTerminalIntent records only lightweight PostgreSQL
// work for the standalone reconciler. It deliberately performs no secret
// redemption, PgVector connection, or external metadata write.
func persistCurrentIndexMetaTerminalIntent(
	ctx context.Context,
	tx sqlExecutor,
	record outputRecord,
) error {
	if record.PayloadType != payloadTypeRuntimeFailure &&
		(record.PayloadType != payloadTypeIndexIngestResult ||
			record.SettlementOutcome != executionapp.SettlementFailed) {
		return outputapp.ErrInvalidValidationOutput
	}
	var state string
	switch record.SettlementOutcome {
	case executionapp.SettlementFailed:
		state = "failed"
	case executionapp.SettlementCancelled:
		state = "cancelled"
	default:
		return outputapp.ErrInvalidValidationOutput
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.index_ingest_jobs
SET index_meta_terminal_state = $3,
    index_meta_terminal_occurred_at = $4,
    index_meta_terminal_status = 'PENDING',
    index_meta_terminal_attempt_count = 0,
    index_meta_terminal_next_attempt_at = clock_timestamp()
WHERE execution_id = $1
  AND generation = $2
  AND capability_id = 'index.ingest.v1'
  AND index_meta_initialized_at IS NOT NULL
  AND index_meta_terminal_status IS NULL`,
		record.ExecutionID,
		int64(record.Generation),
		state,
		record.OccurredAt.UTC(),
	); err != nil {
		return fmt.Errorf("persist current index metadata terminal intent: %w", err)
	}
	return nil
}

var _ outputapp.ConfigurationValidationProjector = (*ConfigurationValidationResultsRepository)(nil)
var _ outputapp.RuntimeFailureProjector = (*RuntimeFailureResultsRepository)(nil)
