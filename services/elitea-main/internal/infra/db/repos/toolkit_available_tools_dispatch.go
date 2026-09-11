package repos

import (
	"context"
	"errors"
	"fmt"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	toolkitdiscoveryapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitdiscovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
)

func (r *ToolkitAvailableToolsJobsRepository) LoadPreparedToolkitAvailableTools(
	ctx context.Context,
	outboxID string,
) (*executionapp.StoredPreparedEnvelope, error) {
	if outboxID == "" {
		return nil, toolkitdiscoveryapp.ErrInvalidDiscoveryDispatch
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite,
	})
	if err != nil {
		return nil, fmt.Errorf("begin toolkit discovery envelope read: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	row, err := sqlcgen.New(tx).LockToolkitAvailableToolsEnvelope(ctx, sqlcgen.LockToolkitAvailableToolsEnvelopeParams{
		OutboxID:   outboxID,
		StreamName: r.policy.StreamName,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPendingToolkitAvailableToolsDispatchNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load prepared toolkit discovery envelope: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit toolkit discovery envelope read: %w", err)
	}
	committed = true
	if row.Retired || row.AuthorityGranted ||
		(row.State != string(executiondomain.JobPending) &&
			row.State != string(executiondomain.JobDispatched)) {
		return nil, executionapp.ErrDispatchRetired
	}
	if row.DeadlineExpired {
		return nil, executionapp.ErrDispatchDeadlineExpired
	}
	return storedPreparedEnvelope(
		row.PreparedSignedEnvelopeBytes,
		row.PreparedSignedEnvelopeDigest,
		row.PreparedSignatureProfile,
		row.PreparedKeyID,
		row.Published,
	)
}

func (r *ToolkitAvailableToolsJobsRepository) StorePreparedToolkitAvailableTools(
	ctx context.Context,
	outboxID string,
	candidate executionapp.PreparedCommandEnvelope,
) (executionapp.StoredPreparedEnvelope, error) {
	if outboxID == "" {
		return executionapp.StoredPreparedEnvelope{}, toolkitdiscoveryapp.ErrInvalidDiscoveryDispatch
	}
	if err := candidate.Validate(); err != nil {
		return executionapp.StoredPreparedEnvelope{}, err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite,
	})
	if err != nil {
		return executionapp.StoredPreparedEnvelope{}, fmt.Errorf("begin toolkit discovery envelope selection: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	queries := sqlcgen.New(tx)
	row, err := queries.LockToolkitAvailableToolsEnvelope(ctx, sqlcgen.LockToolkitAvailableToolsEnvelopeParams{
		OutboxID:   outboxID,
		StreamName: r.policy.StreamName,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return executionapp.StoredPreparedEnvelope{}, ErrPendingToolkitAvailableToolsDispatchNotFound
	}
	if err != nil {
		return executionapp.StoredPreparedEnvelope{}, fmt.Errorf("lock toolkit discovery envelope: %w", err)
	}
	if row.Retired || row.AuthorityGranted ||
		(row.State != string(executiondomain.JobPending) &&
			row.State != string(executiondomain.JobDispatched)) {
		return executionapp.StoredPreparedEnvelope{}, executionapp.ErrDispatchRetired
	}
	if row.DeadlineExpired {
		return executionapp.StoredPreparedEnvelope{}, executionapp.ErrDispatchDeadlineExpired
	}
	stored, err := storedPreparedEnvelope(
		row.PreparedSignedEnvelopeBytes,
		row.PreparedSignedEnvelopeDigest,
		row.PreparedSignatureProfile,
		row.PreparedKeyID,
		row.Published,
	)
	if err != nil {
		return executionapp.StoredPreparedEnvelope{}, err
	}
	var selected executionapp.StoredPreparedEnvelope
	if stored != nil {
		selected = *stored
	} else {
		if row.Published || row.State != string(executiondomain.JobPending) {
			return executionapp.StoredPreparedEnvelope{}, ErrPendingToolkitAvailableToolsDispatchNotFound
		}
		rows, err := queries.StorePreparedToolkitAvailableToolsEnvelope(
			ctx,
			sqlcgen.StorePreparedToolkitAvailableToolsEnvelopeParams{
				EnvelopeBytes:    append([]byte(nil), candidate.Bytes...),
				EnvelopeDigest:   append([]byte(nil), candidate.Digest[:]...),
				SignatureProfile: candidate.SignatureProfile,
				KeyID:            candidate.KeyID,
				OutboxID:         outboxID,
			},
		)
		if err != nil {
			return executionapp.StoredPreparedEnvelope{}, fmt.Errorf("store prepared toolkit discovery envelope: %w", err)
		}
		if rows != 1 {
			return executionapp.StoredPreparedEnvelope{}, executionapp.ErrDispatchDeadlineExpired
		}
		selected = executionapp.StoredPreparedEnvelope{Envelope: candidate.Clone()}
	}
	if err := tx.Commit(ctx); err != nil {
		return executionapp.StoredPreparedEnvelope{}, fmt.Errorf("commit toolkit discovery envelope selection: %w", err)
	}
	committed = true
	return selected, nil
}

func (r *ToolkitAvailableToolsJobsRepository) MarkToolkitAvailableToolsPublished(
	ctx context.Context,
	outboxID string,
	encodedDigest runtimedomain.Digest,
) error {
	if outboxID == "" || encodedDigest.IsZero() {
		return toolkitdiscoveryapp.ErrInvalidDiscoveryDispatch
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite,
	})
	if err != nil {
		return fmt.Errorf("begin toolkit discovery publication transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	queries := sqlcgen.New(tx)
	row, err := queries.LockToolkitAvailableToolsPublication(ctx, sqlcgen.LockToolkitAvailableToolsPublicationParams{
		OutboxID:   outboxID,
		StreamName: r.policy.StreamName,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPendingToolkitAvailableToolsDispatchNotFound
	}
	if err != nil {
		return fmt.Errorf("lock toolkit discovery publication: %w", err)
	}
	if row.Retired {
		return executionapp.ErrDispatchRetired
	}
	persisted, err := storedDigest(row.PreparedSignedEnvelopeDigest)
	if err != nil {
		return fmt.Errorf("invalid prepared toolkit discovery digest: %w", err)
	}
	if persisted != encodedDigest {
		return ErrOutboxPublishConflict
	}
	if row.Published {
		published, err := storedDigest(row.PublishedEnvelopeDigest)
		if err != nil {
			return fmt.Errorf("invalid published toolkit discovery digest: %w", err)
		}
		if published != persisted {
			return ErrOutboxPublishConflict
		}
	} else {
		if row.AuthorityGranted {
			return ErrOutboxPublishConflict
		}
		if row.DeadlineExpired {
			return executionapp.ErrDispatchDeadlineExpired
		}
		if row.State != string(executiondomain.JobPending) {
			return ErrOutboxPublishConflict
		}
		rows, err := queries.MarkToolkitAvailableToolsPublished(ctx, sqlcgen.MarkToolkitAvailableToolsPublishedParams{
			OutboxID:       outboxID,
			EnvelopeDigest: append([]byte(nil), encodedDigest[:]...),
		})
		if err != nil {
			return fmt.Errorf("mark toolkit discovery published: %w", err)
		}
		if rows != 1 {
			return executionapp.ErrDispatchDeadlineExpired
		}
		rows, err = queries.MarkToolkitAvailableToolsDispatched(ctx, sqlcgen.MarkToolkitAvailableToolsDispatchedParams{
			ExecutionID: row.ExecutionID,
			Generation:  row.Generation,
		})
		if err != nil {
			return fmt.Errorf("mark toolkit discovery dispatched: %w", err)
		}
		if rows != 1 {
			return ErrOutboxPublishConflict
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit toolkit discovery publication: %w", err)
	}
	committed = true
	return nil
}

func (r *ToolkitAvailableToolsJobsRepository) ReclaimExpiredToolkitAvailableToolsRuns(
	ctx context.Context,
	limit int,
) (int, error) {
	if limit <= 0 || limit > executionapp.MaxOutboxPublisherBatchSize {
		return 0, executionapp.ErrInvalidPendingOutboxLimit
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite,
	})
	if err != nil {
		return 0, fmt.Errorf("begin toolkit discovery retirement transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	queries := sqlcgen.New(tx)
	executor := pgxExecutor{queryer: tx}
	expired, err := queries.LockExpiredNoAuthorityToolkitAvailableToolsExecutions(
		ctx,
		sqlcgen.LockExpiredNoAuthorityToolkitAvailableToolsExecutionsParams{
			StreamName: r.policy.StreamName,
			BatchLimit: int32(limit),
		},
	)
	if err != nil {
		return 0, fmt.Errorf("lock expired no-authority toolkit discoverys: %w", err)
	}
	retired := 0
	for _, row := range expired {
		changed, err := retireLockedNoAuthorityValidation(ctx, executor, noAuthorityRetirementCandidate{
			OutboxID:            row.OutboxID,
			ExecutionID:         row.ExecutionID,
			Generation:          row.Generation,
			ProjectionProjectID: int64(row.ProjectionProjectID),
			DesiredState:        row.DesiredState,
		})
		if err != nil {
			return 0, err
		}
		if changed {
			retired++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit toolkit discovery retirement transaction: %w", err)
	}
	committed = true
	return retired, nil
}

func (r *ToolkitAvailableToolsJobsRepository) ReadToolkitAvailableToolsSettlement(
	ctx context.Context,
	executionID string,
	generation uint64,
) (toolkitdiscoveryapp.Settlement, bool, error) {
	if executionID == "" || generation == 0 || generation > uint64(1<<62) {
		return toolkitdiscoveryapp.Settlement{}, false, toolkitdiscoveryapp.ErrInvalidDiscovery
	}
	var payloadType, settlementOutcome string
	var payload []byte
	err := r.pool.QueryRow(ctx, `
SELECT o.payload_type, o.settlement_outcome, o.payload_bytes
FROM elitea_runtime.output_inbox AS o
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = o.execution_id AND j.generation = o.generation
WHERE o.execution_id = $1
  AND o.generation = $2
  AND j.capability_id = 'toolkit.available_tools.v1'
  AND o.payload_type IN ('TOOLKIT_AVAILABLE_TOOLS_RESULT', 'RUNTIME_FAILURE')
ORDER BY o.sequence DESC
LIMIT 1`, executionID, int64(generation)).Scan(&payloadType, &settlementOutcome, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return toolkitdiscoveryapp.Settlement{}, false, nil
	}
	if err != nil {
		return toolkitdiscoveryapp.Settlement{}, false, fmt.Errorf("read toolkit discovery settlement: %w", err)
	}
	if len(payload) == 0 {
		return toolkitdiscoveryapp.Settlement{}, false,
			errors.New("stored toolkit discovery settlement carries no payload")
	}
	return toolkitdiscoveryapp.Settlement{
		Outcome:     executionapp.SettlementOutcome(settlementOutcome),
		PayloadType: payloadType,
		Payload:     payload,
	}, true, nil
}

var _ toolkitdiscoveryapp.PendingDispatchStore = (*ToolkitAvailableToolsJobsRepository)(nil)
var _ toolkitdiscoveryapp.SettlementReader = (*ToolkitAvailableToolsJobsRepository)(nil)
