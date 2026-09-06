package repos

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	toolkitexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
)

var ErrPendingToolkitExecuteReadDispatchNotFound = errors.New("pending direct toolkit execution dispatch not found")

func (r *ToolkitExecuteReadJobsRepository) LoadPendingToolkitExecuteRead(
	ctx context.Context,
	outboxID string,
) (toolkitexecutionapp.ToolkitExecuteReadDispatch, error) {
	if outboxID == "" {
		return toolkitexecutionapp.ToolkitExecuteReadDispatch{}, toolkitexecutionapp.ErrInvalidToolkitExecuteReadDispatch
	}
	queries, err := runtimeSQLCQueries(r.store)
	if err != nil {
		return toolkitexecutionapp.ToolkitExecuteReadDispatch{}, err
	}
	row, err := queries.GetPendingToolkitExecuteReadDispatch(
		ctx,
		sqlcgen.GetPendingToolkitExecuteReadDispatchParams{OutboxID: outboxID, StreamName: r.policy.StreamName},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return toolkitexecutionapp.ToolkitExecuteReadDispatch{}, ErrPendingToolkitExecuteReadDispatchNotFound
	}
	if err != nil {
		return toolkitexecutionapp.ToolkitExecuteReadDispatch{}, fmt.Errorf("load pending direct toolkit outbox: %w", err)
	}
	if row.Generation <= 0 || row.DispatchOrdinal <= 0 || row.ManifestSize <= 0 ||
		row.Priority <= 0 || !row.Deadline.Valid || row.Deadline.Time.IsZero() {
		return toolkitexecutionapp.ToolkitExecuteReadDispatch{}, errors.New("pending direct toolkit outbox contains invalid numeric fields")
	}
	digest, err := storedDigest(row.ManifestDigest)
	if err != nil {
		return toolkitexecutionapp.ToolkitExecuteReadDispatch{}, fmt.Errorf("pending direct toolkit input digest: %w", err)
	}
	dispatch := toolkitexecutionapp.ToolkitExecuteReadDispatch{
		OutboxID: row.OutboxID, CommandID: row.CommandID, ExecutionID: row.ExecutionID,
		Generation: uint64(row.Generation), DispatchOrdinal: uint64(row.DispatchOrdinal),
		TenantID:            row.TenantID,
		ResourceProjectID:   strconv.FormatInt(int64(row.ResourceProjectID), 10),
		ProjectionProjectID: strconv.FormatInt(int64(row.ProjectionProjectID), 10),
		PrincipalRef:        row.PrincipalRef,
		InputBundleID:       row.InputBundleID, InputBundleVersion: row.ImmutableVersion,
		InputBundleMediaType: row.MediaType, InputBundleByteLength: uint64(row.ManifestSize),
		InputBundleDigest: digest, CapabilityID: row.CapabilityID,
		CapabilityVersion: row.CapabilityVersion, ResourceClass: row.ResourceClass,
		IsolationClass: row.IsolationClass, Priority: uint32(row.Priority),
		Deadline: row.Deadline.Time.UTC(), LimitsRevision: row.LimitsRevision,
		Traceparent: row.Traceparent, Tracestate: row.Tracestate,
		RequestEntryID: row.RequestEntryID,
	}
	if err := dispatch.Validate(); err != nil {
		return toolkitexecutionapp.ToolkitExecuteReadDispatch{}, fmt.Errorf("invalid stored direct toolkit dispatch: %w", err)
	}
	return dispatch, nil
}

func (r *ToolkitExecuteReadJobsRepository) LoadPreparedToolkitExecuteRead(
	ctx context.Context,
	outboxID string,
) (*executionapp.StoredPreparedEnvelope, error) {
	if outboxID == "" {
		return nil, toolkitexecutionapp.ErrInvalidToolkitExecuteReadDispatch
	}
	queries, err := runtimeSQLCQueries(r.store)
	if err != nil {
		return nil, err
	}
	row, err := queries.GetPreparedToolkitExecuteReadEnvelope(
		ctx,
		sqlcgen.GetPreparedToolkitExecuteReadEnvelopeParams{OutboxID: outboxID, StreamName: r.policy.StreamName},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPendingToolkitExecuteReadDispatchNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load prepared direct toolkit envelope: %w", err)
	}
	if row.Retired || row.AuthorityGranted ||
		(row.State != string(executiondomain.JobPending) && row.State != string(executiondomain.JobDispatched)) {
		return nil, executionapp.ErrDispatchRetired
	}
	if row.DeadlineExpired {
		return nil, executionapp.ErrDispatchDeadlineExpired
	}
	return storedPreparedEnvelope(
		row.PreparedSignedEnvelopeBytes, row.PreparedSignedEnvelopeDigest,
		row.PreparedSignatureProfile, row.PreparedKeyID, row.Published,
	)
}

func (r *ToolkitExecuteReadJobsRepository) StorePreparedToolkitExecuteRead(
	ctx context.Context,
	outboxID string,
	candidate executionapp.PreparedCommandEnvelope,
) (executionapp.StoredPreparedEnvelope, error) {
	if outboxID == "" {
		return executionapp.StoredPreparedEnvelope{}, toolkitexecutionapp.ErrInvalidToolkitExecuteReadDispatch
	}
	if err := candidate.Validate(); err != nil {
		return executionapp.StoredPreparedEnvelope{}, err
	}
	var selected executionapp.StoredPreparedEnvelope
	err := r.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		queries, err := runtimeSQLCQueries(tx)
		if err != nil {
			return err
		}
		row, err := queries.LockToolkitExecuteReadEnvelope(
			ctx,
			sqlcgen.LockToolkitExecuteReadEnvelopeParams{OutboxID: outboxID, StreamName: r.policy.StreamName},
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPendingToolkitExecuteReadDispatchNotFound
		}
		if err != nil {
			return fmt.Errorf("lock direct toolkit envelope: %w", err)
		}
		if row.Retired || row.AuthorityGranted ||
			(row.State != string(executiondomain.JobPending) && row.State != string(executiondomain.JobDispatched)) {
			return executionapp.ErrDispatchRetired
		}
		if row.DeadlineExpired {
			return executionapp.ErrDispatchDeadlineExpired
		}
		stored, err := storedPreparedEnvelope(
			row.PreparedSignedEnvelopeBytes, row.PreparedSignedEnvelopeDigest,
			row.PreparedSignatureProfile, row.PreparedKeyID, row.Published,
		)
		if err != nil {
			return err
		}
		if stored != nil {
			selected = *stored
			return nil
		}
		if row.Published || row.State != string(executiondomain.JobPending) {
			return ErrPendingToolkitExecuteReadDispatchNotFound
		}
		rows, err := queries.StorePreparedToolkitExecuteReadEnvelope(
			ctx,
			sqlcgen.StorePreparedToolkitExecuteReadEnvelopeParams{
				EnvelopeBytes:    append([]byte(nil), candidate.Bytes...),
				EnvelopeDigest:   append([]byte(nil), candidate.Digest[:]...),
				SignatureProfile: candidate.SignatureProfile, KeyID: candidate.KeyID,
				OutboxID: outboxID,
			},
		)
		if err != nil {
			return fmt.Errorf("store prepared direct toolkit envelope: %w", err)
		}
		if rows != 1 {
			return executionapp.ErrDispatchDeadlineExpired
		}
		selected = executionapp.StoredPreparedEnvelope{Envelope: candidate.Clone()}
		return nil
	})
	return selected, err
}

func (r *ToolkitExecuteReadJobsRepository) MarkToolkitExecuteReadPublished(
	ctx context.Context,
	outboxID string,
	encodedDigest runtimedomain.Digest,
) error {
	if outboxID == "" || encodedDigest.IsZero() {
		return toolkitexecutionapp.ErrInvalidToolkitExecuteReadDispatch
	}
	return r.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		queries, err := runtimeSQLCQueries(tx)
		if err != nil {
			return err
		}
		row, err := queries.LockToolkitExecuteReadPublication(
			ctx,
			sqlcgen.LockToolkitExecuteReadPublicationParams{OutboxID: outboxID, StreamName: r.policy.StreamName},
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPendingToolkitExecuteReadDispatchNotFound
		}
		if err != nil {
			return fmt.Errorf("lock direct toolkit publication: %w", err)
		}
		if row.Retired {
			return executionapp.ErrDispatchRetired
		}
		persisted, err := storedDigest(row.PreparedSignedEnvelopeDigest)
		if err != nil {
			return fmt.Errorf("invalid prepared direct toolkit digest: %w", err)
		}
		if persisted != encodedDigest {
			return ErrOutboxPublishConflict
		}
		if row.Published {
			published, err := storedDigest(row.PublishedEnvelopeDigest)
			if err != nil || published != persisted {
				return ErrOutboxPublishConflict
			}
			if !row.AuthorityGranted &&
				(row.State == string(executiondomain.JobPending) || row.State == string(executiondomain.JobDispatched)) {
				rows, err := queries.RefreshToolkitExecuteReadPublication(
					ctx,
					sqlcgen.RefreshToolkitExecuteReadPublicationParams{
						OutboxID: outboxID, EnvelopeDigest: append([]byte(nil), encodedDigest[:]...),
					},
				)
				if err != nil {
					return fmt.Errorf("refresh direct toolkit visibility: %w", err)
				}
				if rows != 1 {
					return executionapp.ErrDispatchDeadlineExpired
				}
			}
			return nil
		}
		if row.AuthorityGranted {
			return ErrOutboxPublishConflict
		}
		if row.DeadlineExpired {
			return executionapp.ErrDispatchDeadlineExpired
		}
		if row.State != string(executiondomain.JobPending) {
			return ErrOutboxPublishConflict
		}
		rows, err := queries.MarkToolkitExecuteReadPublished(
			ctx,
			sqlcgen.MarkToolkitExecuteReadPublishedParams{
				OutboxID: outboxID, EnvelopeDigest: append([]byte(nil), encodedDigest[:]...),
			},
		)
		if err != nil {
			return fmt.Errorf("mark direct toolkit published: %w", err)
		}
		if rows != 1 {
			return executionapp.ErrDispatchDeadlineExpired
		}
		rows, err = queries.MarkToolkitExecuteReadDispatched(
			ctx,
			sqlcgen.MarkToolkitExecuteReadDispatchedParams{ExecutionID: row.ExecutionID, Generation: row.Generation},
		)
		if err != nil {
			return fmt.Errorf("mark direct toolkit dispatched: %w", err)
		}
		if rows != 1 {
			return ErrOutboxPublishConflict
		}
		return nil
	})
}

func (r *ToolkitExecuteReadJobsRepository) ListPendingToolkitExecuteReadIDs(
	ctx context.Context,
	limit int,
	visibilityTimeout time.Duration,
) ([]string, error) {
	if limit <= 0 || limit > executionapp.MaxOutboxPublisherBatchSize ||
		visibilityTimeout < executionapp.MinOutboxVisibilityTimeout ||
		visibilityTimeout > executionapp.MaxOutboxVisibilityTimeout {
		return nil, executionapp.ErrInvalidPendingOutboxLimit
	}
	queries, err := runtimeSQLCQueries(r.store)
	if err != nil {
		return nil, err
	}
	ids, err := queries.ListPendingToolkitExecuteReadIDs(
		ctx,
		sqlcgen.ListPendingToolkitExecuteReadIDsParams{
			StreamName: r.policy.StreamName, BatchLimit: int32(limit),
			VisibilityMillis: visibilityTimeout.Milliseconds(),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("list pending direct toolkit outbox: %w", err)
	}
	if len(ids) > limit {
		return nil, executionapp.ErrPendingOutboxBatchLimitExceeded
	}
	for _, id := range ids {
		if id == "" {
			return nil, errors.New("pending direct toolkit outbox contains empty identity")
		}
	}
	return ids, nil
}

func (r *ToolkitExecuteReadJobsRepository) RetireNoAuthorityToolkitExecuteRead(
	ctx context.Context,
	limit int,
) (int, error) {
	if limit <= 0 || limit > executionapp.MaxOutboxPublisherBatchSize {
		return 0, executionapp.ErrInvalidPendingOutboxLimit
	}
	retired := 0
	err := r.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		queries, err := runtimeSQLCQueries(tx)
		if err != nil {
			return err
		}
		cancelled, err := queries.LockCancelledNoAuthorityToolkitExecuteReads(
			ctx,
			sqlcgen.LockCancelledNoAuthorityToolkitExecuteReadsParams{
				StreamName: r.policy.StreamName, BatchLimit: int32(limit),
			},
		)
		if err != nil {
			return fmt.Errorf("lock cancelled direct toolkit executions: %w", err)
		}
		for _, row := range cancelled {
			changed, err := retireLockedNoAuthorityValidation(ctx, tx, noAuthorityRetirementCandidate{
				OutboxID: row.OutboxID, ExecutionID: row.ExecutionID,
				Generation: row.Generation, ProjectionProjectID: int64(row.ProjectionProjectID),
				DesiredState: row.DesiredState,
			})
			if err != nil {
				return err
			}
			if changed {
				retired++
			}
		}
		remaining := limit - retired
		if remaining == 0 {
			return nil
		}
		expired, err := queries.LockExpiredNoAuthorityToolkitExecuteReads(
			ctx,
			sqlcgen.LockExpiredNoAuthorityToolkitExecuteReadsParams{
				StreamName: r.policy.StreamName, BatchLimit: int32(remaining),
			},
		)
		if err != nil {
			return fmt.Errorf("lock expired direct toolkit executions: %w", err)
		}
		for _, row := range expired {
			changed, err := retireLockedNoAuthorityValidation(ctx, tx, noAuthorityRetirementCandidate{
				OutboxID: row.OutboxID, ExecutionID: row.ExecutionID,
				Generation: row.Generation, ProjectionProjectID: int64(row.ProjectionProjectID),
				DesiredState: row.DesiredState,
			})
			if err != nil {
				return err
			}
			if changed {
				retired++
			}
		}
		return nil
	})
	return retired, err
}

var _ toolkitexecutionapp.PendingDispatchStore = (*ToolkitExecuteReadJobsRepository)(nil)
var _ toolkitexecutionapp.PendingOutbox = (*ToolkitExecuteReadJobsRepository)(nil)
