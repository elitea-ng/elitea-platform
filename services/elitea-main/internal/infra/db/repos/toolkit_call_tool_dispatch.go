package repos

import (
	"context"
	"errors"
	"fmt"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
)

func (r *ToolkitCallToolJobsRepository) LoadPreparedToolkitCallTool(
	ctx context.Context,
	outboxID string,
) (*executionapp.StoredPreparedEnvelope, error) {
	if outboxID == "" {
		return nil, toolkitcalltoolapp.ErrInvalidToolRunDispatch
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite,
	})
	if err != nil {
		return nil, fmt.Errorf("begin tool-run envelope read: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	row, err := sqlcgen.New(tx).LockToolkitCallToolEnvelope(ctx, sqlcgen.LockToolkitCallToolEnvelopeParams{
		OutboxID:   outboxID,
		StreamName: r.policy.StreamName,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPendingToolkitCallToolDispatchNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load prepared tool-run envelope: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tool-run envelope read: %w", err)
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

// StorePreparedToolkitCallTool selects one exact signed envelope under the
// job/outbox lock, so a retry inside the same request appends the byte sequence
// that is already durable rather than signing a second, possibly different one.
func (r *ToolkitCallToolJobsRepository) StorePreparedToolkitCallTool(
	ctx context.Context,
	outboxID string,
	candidate executionapp.PreparedCommandEnvelope,
) (executionapp.StoredPreparedEnvelope, error) {
	if outboxID == "" {
		return executionapp.StoredPreparedEnvelope{}, toolkitcalltoolapp.ErrInvalidToolRunDispatch
	}
	if err := candidate.Validate(); err != nil {
		return executionapp.StoredPreparedEnvelope{}, err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite,
	})
	if err != nil {
		return executionapp.StoredPreparedEnvelope{}, fmt.Errorf("begin tool-run envelope selection: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	queries := sqlcgen.New(tx)
	row, err := queries.LockToolkitCallToolEnvelope(ctx, sqlcgen.LockToolkitCallToolEnvelopeParams{
		OutboxID:   outboxID,
		StreamName: r.policy.StreamName,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return executionapp.StoredPreparedEnvelope{}, ErrPendingToolkitCallToolDispatchNotFound
	}
	if err != nil {
		return executionapp.StoredPreparedEnvelope{}, fmt.Errorf("lock tool-run envelope: %w", err)
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
		// Selection is a one-shot. Only a PENDING job may select a new
		// envelope; a DISPATCHED job already holds its durable winner and a
		// second signature must never replace it.
		if row.Published || row.State != string(executiondomain.JobPending) {
			return executionapp.StoredPreparedEnvelope{}, ErrPendingToolkitCallToolDispatchNotFound
		}
		rows, err := queries.StorePreparedToolkitCallToolEnvelope(
			ctx,
			sqlcgen.StorePreparedToolkitCallToolEnvelopeParams{
				EnvelopeBytes:    append([]byte(nil), candidate.Bytes...),
				EnvelopeDigest:   append([]byte(nil), candidate.Digest[:]...),
				SignatureProfile: candidate.SignatureProfile,
				KeyID:            candidate.KeyID,
				OutboxID:         outboxID,
			},
		)
		if err != nil {
			return executionapp.StoredPreparedEnvelope{}, fmt.Errorf("store prepared tool-run envelope: %w", err)
		}
		if rows != 1 {
			return executionapp.StoredPreparedEnvelope{}, executionapp.ErrDispatchDeadlineExpired
		}
		selected = executionapp.StoredPreparedEnvelope{Envelope: candidate.Clone()}
	}
	if err := tx.Commit(ctx); err != nil {
		return executionapp.StoredPreparedEnvelope{}, fmt.Errorf("commit tool-run envelope selection: %w", err)
	}
	committed = true
	return selected, nil
}

func (r *ToolkitCallToolJobsRepository) MarkToolkitCallToolPublished(
	ctx context.Context,
	outboxID string,
	encodedDigest runtimedomain.Digest,
) error {
	if outboxID == "" || encodedDigest.IsZero() {
		return toolkitcalltoolapp.ErrInvalidToolRunDispatch
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite,
	})
	if err != nil {
		return fmt.Errorf("begin tool-run publication transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	queries := sqlcgen.New(tx)
	row, err := queries.LockToolkitCallToolPublication(ctx, sqlcgen.LockToolkitCallToolPublicationParams{
		OutboxID:   outboxID,
		StreamName: r.policy.StreamName,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPendingToolkitCallToolDispatchNotFound
	}
	if err != nil {
		return fmt.Errorf("lock tool-run publication: %w", err)
	}
	if row.Retired {
		return executionapp.ErrDispatchRetired
	}
	persisted, err := storedDigest(row.PreparedSignedEnvelopeDigest)
	if err != nil {
		return fmt.Errorf("invalid prepared tool-run digest: %w", err)
	}
	if persisted != encodedDigest {
		return ErrOutboxPublishConflict
	}
	if row.Published {
		published, err := storedDigest(row.PublishedEnvelopeDigest)
		if err != nil {
			return fmt.Errorf("invalid published tool-run digest: %w", err)
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
		rows, err := queries.MarkToolkitCallToolPublished(ctx, sqlcgen.MarkToolkitCallToolPublishedParams{
			OutboxID:       outboxID,
			EnvelopeDigest: append([]byte(nil), encodedDigest[:]...),
		})
		if err != nil {
			return fmt.Errorf("mark tool run published: %w", err)
		}
		if rows != 1 {
			return executionapp.ErrDispatchDeadlineExpired
		}
		rows, err = queries.MarkToolkitCallToolDispatched(ctx, sqlcgen.MarkToolkitCallToolDispatchedParams{
			ExecutionID: row.ExecutionID,
			Generation:  row.Generation,
		})
		if err != nil {
			return fmt.Errorf("mark tool run dispatched: %w", err)
		}
		if rows != 1 {
			return ErrOutboxPublishConflict
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tool-run publication: %w", err)
	}
	committed = true
	return nil
}

// ReclaimExpiredToolkitCallToolRuns retires the outbox rows a crash between
// admission and the Redis append would otherwise leave holding admission
// capacity forever. It touches ONLY work past its deadline that no worker ever
// claimed, so a live run is never affected.
func (r *ToolkitCallToolJobsRepository) ReclaimExpiredToolkitCallToolRuns(
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
		return 0, fmt.Errorf("begin tool-run retirement transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	queries := sqlcgen.New(tx)
	executor := pgxExecutor{queryer: tx}
	expired, err := queries.LockExpiredNoAuthorityToolkitCallToolExecutions(
		ctx,
		sqlcgen.LockExpiredNoAuthorityToolkitCallToolExecutionsParams{
			StreamName: r.policy.StreamName,
			BatchLimit: int32(limit),
		},
	)
	if err != nil {
		return 0, fmt.Errorf("lock expired no-authority tool runs: %w", err)
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
		return 0, fmt.Errorf("commit tool-run retirement transaction: %w", err)
	}
	committed = true
	return retired, nil
}

// ReadToolkitCallToolSettlement is the bounded wait's only read. It is raw SQL
// rather than sqlc because `elitea_runtime.output_inbox` is not in the sqlc
// schema projection — every other reader of that table is raw SQL for the same
// reason.
//
// found=false means the run has not settled YET. It never means the run failed:
// a failure settles as a RUNTIME_FAILURE row, which this returns like any other.
func (r *ToolkitCallToolJobsRepository) ReadToolkitCallToolSettlement(
	ctx context.Context,
	executionID string,
	generation uint64,
) (toolkitcalltoolapp.Settlement, bool, error) {
	if executionID == "" || generation == 0 || generation > uint64(1<<62) {
		return toolkitcalltoolapp.Settlement{}, false, toolkitcalltoolapp.ErrInvalidToolRun
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
  AND j.capability_id = 'toolkit.call_tool.v1'
  AND o.payload_type IN ('TOOLKIT_CALL_TOOL_RESULT', 'RUNTIME_FAILURE')
ORDER BY o.sequence DESC
LIMIT 1`, executionID, int64(generation)).Scan(&payloadType, &settlementOutcome, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return toolkitcalltoolapp.Settlement{}, false, nil
	}
	if err != nil {
		return toolkitcalltoolapp.Settlement{}, false, fmt.Errorf("read tool-run settlement: %w", err)
	}
	if len(payload) == 0 {
		return toolkitcalltoolapp.Settlement{}, false,
			errors.New("stored tool-run settlement carries no payload")
	}
	return toolkitcalltoolapp.Settlement{
		Outcome:     executionapp.SettlementOutcome(settlementOutcome),
		PayloadType: payloadType,
		Payload:     payload,
	}, true, nil
}

var _ toolkitcalltoolapp.PendingDispatchStore = (*ToolkitCallToolJobsRepository)(nil)
var _ toolkitcalltoolapp.SettlementReader = (*ToolkitCallToolJobsRepository)(nil)
