package repos

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
)

var ErrPendingAgentExecutionDispatchNotFound = errors.New("pending agent execution dispatch not found")

// LoadPendingAgentExecution loads only immutable bundle and browser
// correlation references. The admitted prompt, tools, model settings,
// credentials and history are deliberately absent from this query and the command bus.
func (r *AgentExecutionJobsRepository) LoadPendingAgentExecution(ctx context.Context, outboxID string) (agentexecutionapp.AgentExecutionDispatch, error) {
	if outboxID == "" {
		return agentexecutionapp.AgentExecutionDispatch{}, agentexecutionapp.ErrInvalidAgentExecutionDispatch
	}
	row, err := sqlcgen.New(r.pool).GetPendingAgentExecutionDispatch(
		ctx,
		sqlcgen.GetPendingAgentExecutionDispatchParams{
			OutboxID:   outboxID,
			StreamName: r.policy.StreamName,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentexecutionapp.AgentExecutionDispatch{}, ErrPendingAgentExecutionDispatchNotFound
	}
	if err != nil {
		return agentexecutionapp.AgentExecutionDispatch{}, fmt.Errorf("load pending agent execution outbox: %w", err)
	}
	if row.Generation <= 0 || row.DispatchOrdinal <= 0 || row.ManifestSize <= 0 ||
		row.Priority <= 0 || !row.Deadline.Valid || row.Deadline.Time.IsZero() {
		return agentexecutionapp.AgentExecutionDispatch{}, errors.New("pending agent execution outbox contains invalid numeric fields")
	}
	digest, err := storedDigest(row.ManifestDigest)
	if err != nil {
		return agentexecutionapp.AgentExecutionDispatch{}, fmt.Errorf("pending agent input digest: %w", err)
	}
	dispatch := agentexecutionapp.AgentExecutionDispatch{
		OutboxID:              row.OutboxID,
		CommandID:             row.CommandID,
		ExecutionID:           row.ExecutionID,
		Generation:            uint64(row.Generation),
		DispatchOrdinal:       uint64(row.DispatchOrdinal),
		TenantID:              row.TenantID,
		ResourceProjectID:     strconv.FormatInt(int64(row.ResourceProjectID), 10),
		ProjectionProjectID:   strconv.FormatInt(int64(row.ProjectionProjectID), 10),
		PrincipalRef:          row.PrincipalRef,
		InputBundleID:         row.InputBundleID,
		InputBundleVersion:    row.ImmutableVersion,
		InputBundleMediaType:  row.MediaType,
		InputBundleByteLength: uint64(row.ManifestSize),
		InputBundleDigest:     digest,
		CapabilityID:          row.CapabilityID,
		CapabilityVersion:     row.CapabilityVersion,
		ResourceClass:         row.ResourceClass,
		IsolationClass:        row.IsolationClass,
		Priority:              uint32(row.Priority),
		Deadline:              row.Deadline.Time.UTC(),
		LimitsRevision:        row.LimitsRevision,
		Traceparent:           row.Traceparent,
		Tracestate:            row.Tracestate,
		RequestEntryID:        row.RequestEntryID,
		ClientStreamID:        row.ClientStreamID,
		ClientMessageID:       row.ClientMessageID,
		SIOEvent:              row.SioEvent,
	}
	if err := dispatch.Validate(); err != nil {
		return agentexecutionapp.AgentExecutionDispatch{}, fmt.Errorf("invalid stored agent execution dispatch: %w", err)
	}
	return dispatch, nil
}

func (r *AgentExecutionJobsRepository) LoadPreparedAgentExecution(ctx context.Context, outboxID string) (*executionapp.StoredPreparedEnvelope, error) {
	if outboxID == "" {
		return nil, agentexecutionapp.ErrInvalidAgentExecutionDispatch
	}
	row, err := sqlcgen.New(r.pool).GetPreparedAgentExecutionEnvelope(
		ctx,
		sqlcgen.GetPreparedAgentExecutionEnvelopeParams{
			OutboxID:   outboxID,
			StreamName: r.policy.StreamName,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPendingAgentExecutionDispatchNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load prepared agent execution envelope: %w", err)
	}
	if row.Retired || row.AuthorityGranted ||
		(row.State != string(executiondomain.JobPending) && row.State != string(executiondomain.JobDispatched)) {
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

// StorePreparedAgentExecution selects one exact signed envelope under the
// job/outbox lock. Competing publishers and key rotation observe the same
// durable winner before any command-bus publish is attempted.
func (r *AgentExecutionJobsRepository) StorePreparedAgentExecution(ctx context.Context, outboxID string, candidate executionapp.PreparedCommandEnvelope) (executionapp.StoredPreparedEnvelope, error) {
	if outboxID == "" {
		return executionapp.StoredPreparedEnvelope{}, agentexecutionapp.ErrInvalidAgentExecutionDispatch
	}
	if err := candidate.Validate(); err != nil {
		return executionapp.StoredPreparedEnvelope{}, err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return executionapp.StoredPreparedEnvelope{}, fmt.Errorf("begin agent envelope selection: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	queries := sqlcgen.New(tx)
	row, err := queries.LockAgentExecutionEnvelope(
		ctx,
		sqlcgen.LockAgentExecutionEnvelopeParams{OutboxID: outboxID, StreamName: r.policy.StreamName},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return executionapp.StoredPreparedEnvelope{}, ErrPendingAgentExecutionDispatchNotFound
	}
	if err != nil {
		return executionapp.StoredPreparedEnvelope{}, fmt.Errorf("lock agent execution envelope: %w", err)
	}
	// A competing publisher can select, append and publish the envelope while
	// this publisher is signing. That moves the job to DISPATCHED, which is not
	// terminal: no worker holds the command yet, and the visibility redelivery
	// path appends the same durable winner again. The winner therefore stays
	// returnable in PENDING and in DISPATCHED. Worker authority, or a terminal
	// job state, does end the window: returning the winner then would recreate
	// a bus message that the worker may already have acknowledged and deleted.
	if row.Retired || row.AuthorityGranted ||
		(row.State != string(executiondomain.JobPending) && row.State != string(executiondomain.JobDispatched)) {
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
		// Selection stays a one-shot. Only a PENDING job may select a new
		// envelope; a DISPATCHED job already holds its durable winner, so a
		// second signature must never replace it.
		if row.Published || row.State != string(executiondomain.JobPending) {
			return executionapp.StoredPreparedEnvelope{}, ErrPendingAgentExecutionDispatchNotFound
		}
		rows, err := queries.StorePreparedAgentExecutionEnvelope(
			ctx,
			sqlcgen.StorePreparedAgentExecutionEnvelopeParams{
				EnvelopeBytes:    append([]byte(nil), candidate.Bytes...),
				EnvelopeDigest:   append([]byte(nil), candidate.Digest[:]...),
				SignatureProfile: candidate.SignatureProfile,
				KeyID:            candidate.KeyID,
				OutboxID:         outboxID,
			},
		)
		if err != nil {
			return executionapp.StoredPreparedEnvelope{}, fmt.Errorf("store prepared agent execution envelope: %w", err)
		}
		if rows != 1 {
			return executionapp.StoredPreparedEnvelope{}, executionapp.ErrDispatchDeadlineExpired
		}
		selected = executionapp.StoredPreparedEnvelope{Envelope: candidate.Clone()}
	}
	if err := tx.Commit(ctx); err != nil {
		return executionapp.StoredPreparedEnvelope{}, fmt.Errorf("commit agent envelope selection: %w", err)
	}
	committed = true
	return selected, nil
}

func (r *AgentExecutionJobsRepository) MarkAgentExecutionPublished(ctx context.Context, outboxID string, encodedDigest runtimedomain.Digest) error {
	if outboxID == "" || encodedDigest.IsZero() {
		return agentexecutionapp.ErrInvalidAgentExecutionDispatch
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return fmt.Errorf("begin agent publication transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	queries := sqlcgen.New(tx)
	row, err := queries.LockAgentExecutionPublication(
		ctx,
		sqlcgen.LockAgentExecutionPublicationParams{OutboxID: outboxID, StreamName: r.policy.StreamName},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPendingAgentExecutionDispatchNotFound
	}
	if err != nil {
		return fmt.Errorf("lock agent execution publication: %w", err)
	}
	if row.Retired {
		return executionapp.ErrDispatchRetired
	}
	persisted, err := storedDigest(row.PreparedSignedEnvelopeDigest)
	if err != nil {
		return fmt.Errorf("invalid prepared agent execution digest: %w", err)
	}
	if persisted != encodedDigest {
		return ErrOutboxPublishConflict
	}
	if row.Published {
		published, err := storedDigest(row.PublishedEnvelopeDigest)
		if err != nil {
			return fmt.Errorf("invalid published agent execution digest: %w", err)
		}
		if published != persisted {
			return ErrOutboxPublishConflict
		}
		if !row.AuthorityGranted &&
			(row.State == string(executiondomain.JobPending) || row.State == string(executiondomain.JobDispatched)) {
			rows, err := queries.RefreshAgentExecutionPublication(
				ctx,
				sqlcgen.RefreshAgentExecutionPublicationParams{
					OutboxID:       outboxID,
					EnvelopeDigest: append([]byte(nil), encodedDigest[:]...),
				},
			)
			if err != nil {
				return fmt.Errorf("refresh agent execution visibility: %w", err)
			}
			if rows != 1 {
				return executionapp.ErrDispatchDeadlineExpired
			}
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
		rows, err := queries.MarkAgentExecutionPublished(
			ctx,
			sqlcgen.MarkAgentExecutionPublishedParams{
				OutboxID:       outboxID,
				EnvelopeDigest: append([]byte(nil), encodedDigest[:]...),
			},
		)
		if err != nil {
			return fmt.Errorf("mark agent execution published: %w", err)
		}
		if rows != 1 {
			return executionapp.ErrDispatchDeadlineExpired
		}
		rows, err = queries.MarkAgentExecutionDispatched(
			ctx,
			sqlcgen.MarkAgentExecutionDispatchedParams{
				ExecutionID: row.ExecutionID,
				Generation:  row.Generation,
			},
		)
		if err != nil {
			return fmt.Errorf("mark agent execution dispatched: %w", err)
		}
		if rows != 1 {
			return ErrOutboxPublishConflict
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit agent execution publication: %w", err)
	}
	committed = true
	return nil
}

func (r *AgentExecutionJobsRepository) ListPendingAgentExecutionIDs(ctx context.Context, limit int, visibilityTimeout time.Duration) ([]string, error) {
	if limit <= 0 || limit > executionapp.MaxOutboxPublisherBatchSize ||
		visibilityTimeout < executionapp.MinOutboxVisibilityTimeout ||
		visibilityTimeout > executionapp.MaxOutboxVisibilityTimeout {
		return nil, executionapp.ErrInvalidPendingOutboxLimit
	}
	ids, err := sqlcgen.New(r.pool).ListPendingAgentExecutionIDs(
		ctx,
		sqlcgen.ListPendingAgentExecutionIDsParams{
			StreamName:       r.policy.StreamName,
			BatchLimit:       int32(limit),
			VisibilityMillis: visibilityTimeout.Milliseconds(),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("list pending agent execution outbox: %w", err)
	}
	if len(ids) > limit {
		return nil, executionapp.ErrPendingOutboxBatchLimitExceeded
	}
	for _, id := range ids {
		if id == "" {
			return nil, errors.New("pending agent execution outbox contains empty identity")
		}
	}
	return ids, nil
}

// RetireNoAuthorityAgentExecution handles only work that never obtained a
// claim. The runtime replay is durable here; current chat terminal projection
// remains a separate capability-owned gate before agent serve is enabled.
func (r *AgentExecutionJobsRepository) RetireNoAuthorityAgentExecution(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > executionapp.MaxOutboxPublisherBatchSize {
		return 0, executionapp.ErrInvalidPendingOutboxLimit
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return 0, fmt.Errorf("begin agent retirement transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	queries := sqlcgen.New(tx)
	executor := pgxExecutor{queryer: tx}
	cancelled, err := queries.LockCancelledNoAuthorityAgentExecutions(
		ctx,
		sqlcgen.LockCancelledNoAuthorityAgentExecutionsParams{
			StreamName: r.policy.StreamName,
			BatchLimit: int32(limit),
		},
	)
	if err != nil {
		return 0, fmt.Errorf("lock cancelled no-authority agent executions: %w", err)
	}
	retired := 0
	for _, row := range cancelled {
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
	remaining := limit - retired
	if remaining > 0 {
		expired, err := queries.LockExpiredNoAuthorityAgentExecutions(
			ctx,
			sqlcgen.LockExpiredNoAuthorityAgentExecutionsParams{
				StreamName: r.policy.StreamName,
				BatchLimit: int32(remaining),
			},
		)
		if err != nil {
			return 0, fmt.Errorf("lock expired no-authority agent executions: %w", err)
		}
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
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit agent retirement transaction: %w", err)
	}
	committed = true
	return retired, nil
}

var _ agentexecutionapp.PendingDispatchStore = (*AgentExecutionJobsRepository)(nil)
var _ agentexecutionapp.PendingOutbox = (*AgentExecutionJobsRepository)(nil)
