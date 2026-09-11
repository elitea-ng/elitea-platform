package repos

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ClaimsRepository struct {
	store      sharedStore
	newClaimID func() (string, error)
	newToken   func() (runtimedomain.FenceToken, error)
}

func NewClaimsRepository(pool *pgxpool.Pool) (*ClaimsRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return newClaimsRepository(store, randomClaimID, randomFenceToken)
}

func newClaimsRepository(store sharedStore, newClaimID func() (string, error), newToken func() (runtimedomain.FenceToken, error)) (*ClaimsRepository, error) {
	if store == nil || newClaimID == nil || newToken == nil {
		return nil, errors.New("claim database and generators are required")
	}
	return &ClaimsRepository{store: store, newClaimID: newClaimID, newToken: newToken}, nil
}

func (r *ClaimsRepository) ClaimValidation(ctx context.Context, request executionapp.ClaimRequest, leaseTTL executionapp.ClaimLeaseTTLMillis) (executionapp.ClaimDecision, error) {
	if request.CommandID == "" || request.OutboxID == "" || request.ExecutionID == "" || request.Generation == 0 || !claimCapabilityAllowed(request.CapabilityID) || request.SignedEnvelopeDigest.IsZero() || request.WorkloadIdentity == "" || request.WorkloadSessionID == "" || request.ProducerID == "" || !leaseTTL.Valid() {
		return executionapp.ClaimDecision{}, executionapp.ErrInvalidClaim
	}
	var decision executionapp.ClaimDecision
	err := r.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		var commandID, desiredState, jobState, invocationState, terminalErrorCode, outboxID, retirementCode string
		var published, retired, deadlineExpired, authorityGranted bool
		var projectionProjectID int64
		var preparedEnvelopeDigest, publishedEnvelopeDigest []byte
		err := tx.QueryRow(ctx, `
SELECT j.command_id, j.desired_state, j.state, j.invocation_state,
       COALESCE(j.terminal_error_code, ''),
       o.outbox_id, o.published_at IS NOT NULL,
       o.prepared_signed_envelope_digest, o.published_envelope_digest,
       o.retired_at IS NOT NULL, COALESCE(o.retirement_code, ''),
       o.deadline <= clock_timestamp(),
       o.authority_granted_at IS NOT NULL,
       j.projection_project_id
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = j.execution_id AND o.generation = j.generation
WHERE j.execution_id = $1
  AND j.generation = $2
  AND j.capability_id = $3
FOR UPDATE OF j, o`, request.ExecutionID, int64(request.Generation), request.CapabilityID).Scan(
			&commandID,
			&desiredState,
			&jobState,
			&invocationState,
			&terminalErrorCode,
			&outboxID,
			&published,
			&preparedEnvelopeDigest,
			&publishedEnvelopeDigest,
			&retired,
			&retirementCode,
			&deadlineExpired,
			&authorityGranted,
			&projectionProjectID,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return executionapp.ErrInvalidClaim
		}
		if err != nil {
			return fmt.Errorf("lock execution for claim: %w", err)
		}
		if commandID != request.CommandID || outboxID != request.OutboxID {
			return executionapp.ErrInvalidClaim
		}
		desired := runtimedomain.DesiredState(desiredState)
		if !desired.Valid() {
			return errors.New("execution contains invalid desired state")
		}
		state := executiondomain.JobState(jobState)
		if !state.Valid() {
			return errors.New("execution contains invalid state")
		}
		preparedMatches := exactClaimDigest(preparedEnvelopeDigest, request.SignedEnvelopeDigest)
		publishedMatches := !published || (len(publishedEnvelopeDigest) == len(preparedEnvelopeDigest) &&
			subtle.ConstantTimeCompare(preparedEnvelopeDigest, publishedEnvelopeDigest) == 1)
		if retired {
			retiredDecision, err := retiredClaimDecision(
				desired,
				state,
				terminalErrorCode,
				retirementCode,
				authorityGranted,
				preparedMatches,
				publishedMatches,
			)
			if err != nil {
				return err
			}
			decision = retiredDecision
			return nil
		}
		if len(preparedEnvelopeDigest) == 0 {
			decision = executionapp.ClaimDecision{
				Disposition:  executionapp.ClaimRetryLaterNoACK,
				DesiredState: desired,
			}
			return nil
		}
		if !preparedMatches || !publishedMatches {
			return executionapp.ErrInvalidClaim
		}
		if !authorityGranted &&
			(state == executiondomain.JobPending || state == executiondomain.JobDispatched) &&
			(desired == runtimedomain.DesiredCancelled || deadlineExpired) {
			retiredNow, err := retireLockedNoAuthorityValidation(ctx, tx, noAuthorityRetirementCandidate{
				OutboxID:            outboxID,
				ExecutionID:         request.ExecutionID,
				Generation:          int64(request.Generation),
				ProjectionProjectID: projectionProjectID,
				DesiredState:        desiredState,
			})
			if err != nil {
				return err
			}
			if retiredNow {
				if desired == runtimedomain.DesiredCancelled {
					decision = executionapp.ClaimDecision{
						Disposition:  executionapp.ClaimObsoleteACK,
						DesiredState: runtimedomain.DesiredCancelled,
					}
				} else {
					decision = executionapp.ClaimDecision{
						Disposition:      executionapp.ClaimRetiredACK,
						DesiredState:     desired,
						RetirementReason: executionapp.RetirementDeadlineExceeded,
					}
				}
				return nil
			}
			decision = executionapp.ClaimDecision{
				Disposition:  executionapp.ClaimRetryLaterNoACK,
				DesiredState: desired,
			}
			return nil
		}
		if !published || len(publishedEnvelopeDigest) == 0 {
			decision = executionapp.ClaimDecision{
				Disposition:  executionapp.ClaimRetryLaterNoACK,
				DesiredState: desired,
			}
			return nil
		}
		if state == executiondomain.JobQuarantined {
			decision = executionapp.ClaimDecision{
				Disposition:  executionapp.ClaimRetryLaterNoACK,
				DesiredState: desired,
			}
			return nil
		}
		if desired == runtimedomain.DesiredCancelled && state == executiondomain.JobCancelled {
			// The terminal state is already durable, so no worker authority or
			// immutable input is required to retire this Redis delivery.
			decision = executionapp.ClaimDecision{
				Disposition:  executionapp.ClaimObsoleteACK,
				DesiredState: runtimedomain.DesiredCancelled,
			}
			return nil
		}

		if err := completeToolkitInboxProjection(ctx, tx, request); err != nil {
			return err
		}

		existing, observedAt, live, err := loadActiveClaimForUpdate(ctx, tx, request.ExecutionID, request.Generation, commandID, desired)
		switch {
		case err == nil && live:
			if existing.Fence.WorkloadIdentity != request.WorkloadIdentity || existing.Fence.WorkloadSessionID != request.WorkloadSessionID || existing.Fence.ProducerID != request.ProducerID {
				decision = executionapp.ClaimDecision{
					Disposition:  executionapp.ClaimRetryLaterNoACK,
					DesiredState: desired,
				}
				return nil
			}
			decision = executionapp.ClaimDecision{Lease: existing, LeaseObservedAt: observedAt, Disposition: executionapp.ClaimActiveLeaseNoACK}
			proposal, watermark, recoveryErr := loadTerminalSettlementRecovery(ctx, tx, existing.Fence)
			if recoveryErr == nil {
				decision.Disposition = executionapp.ClaimRecoverTerminalACK
				decision.ClaimHandoffWatermark = watermark
				decision.SettlementRecovery = &executionapp.SettlementRecovery{Proposal: &proposal}
			} else if !errors.Is(recoveryErr, pgx.ErrNoRows) {
				return recoveryErr
			} else if executiondomain.NodeEventCapability(request.CapabilityID) {
				watermark, err := loadClaimHandoffWatermark(ctx, tx, existing.Fence, existing.ClaimID)
				if err != nil {
					return err
				}
				decision.ClaimHandoffWatermark = watermark
				if state == executiondomain.JobRunning {
					decision.Disposition = executionapp.ClaimRecoverRunningNoACK
				}
			}
			return nil
		case err == nil:
			if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.execution_claims
SET released_at = clock_timestamp(), release_reason = 'LEASE_EXPIRED'
WHERE claim_id = $1 AND released_at IS NULL`, existing.ClaimID); err != nil {
				return fmt.Errorf("release expired execution claim: %w", err)
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("load active execution claim: %w", err)
		}
		if desired == runtimedomain.DesiredCancelled && (state == executiondomain.JobPending || state == executiondomain.JobDispatched) {
			// A prior authority grant or any historical claim excludes the
			// no-authority cancellation finalizer. Reconciliation must inspect the
			// durable owner/output state; never infer ACK safety.
			decision = executionapp.ClaimDecision{
				Disposition:  executionapp.ClaimRetryLaterNoACK,
				DesiredState: desired,
			}
			return nil
		}
		terminalRecoveryOnly := false
		runningRecoveryOnly := false
		if desired == runtimedomain.DesiredCancelled {
			recoverable, err := hasExpiredPredecessorTerminalOutput(ctx, tx, request.ExecutionID, request.Generation)
			if err != nil {
				return err
			}
			if recoverable {
				terminalRecoveryOnly = true
			} else if executiondomain.NodeEventCapability(request.CapabilityID) && state == executiondomain.JobRunning {
				// A cancelled RUNNING index execution has no executable authority
				// after its predecessor lease expires. Give a replacement worker a
				// recovery-only fence so it can emit the canonical cancellation;
				// it never receives inputs or may begin business execution.
				expired, err := hasExpiredPredecessorClaim(ctx, tx, request.ExecutionID, request.Generation)
				if err != nil {
					return err
				}
				runningRecoveryOnly = expired
			}
		}
		if desired != runtimedomain.DesiredRunning && !terminalRecoveryOnly && !runningRecoveryOnly {
			// CLAIMED/RUNNING and other ambiguous states require the existing live
			// owner or explicit reconciliation; DRAINING remains non-terminal.
			decision = executionapp.ClaimDecision{
				Disposition:  executionapp.ClaimRetryLaterNoACK,
				DesiredState: desired,
			}
			return nil
		}

		if !authorityGranted {
			tag, err := tx.Exec(ctx, `
UPDATE elitea_runtime.command_outbox
SET authority_granted_at = clock_timestamp()
WHERE outbox_id = $1
  AND execution_id = $2
  AND generation = $3
  AND retired_at IS NULL
  AND authority_granted_at IS NULL
  AND published_at IS NOT NULL
  AND prepared_signed_envelope_digest = $4
  AND published_envelope_digest = $4
  AND deadline > clock_timestamp()`,
				outboxID,
				request.ExecutionID,
				int64(request.Generation),
				request.SignedEnvelopeDigest[:],
			)
			if err != nil {
				return fmt.Errorf("grant validation worker authority: %w", err)
			}
			if tag.RowsAffected() != 1 {
				retiredNow, err := retireLockedNoAuthorityValidation(ctx, tx, noAuthorityRetirementCandidate{
					OutboxID:            outboxID,
					ExecutionID:         request.ExecutionID,
					Generation:          int64(request.Generation),
					ProjectionProjectID: projectionProjectID,
					DesiredState:        desiredState,
				})
				if err != nil {
					return err
				}
				if retiredNow {
					decision = executionapp.ClaimDecision{
						Disposition:      executionapp.ClaimRetiredACK,
						DesiredState:     desired,
						RetirementReason: executionapp.RetirementDeadlineExceeded,
					}
					return nil
				}
				return executionapp.ErrInvalidClaim
			}
		}

		var claimAttempt int64
		if err := tx.QueryRow(ctx, `
SELECT COALESCE(MAX(claim_attempt), 0) + 1
FROM elitea_runtime.execution_claims
WHERE execution_id = $1 AND generation = $2`, request.ExecutionID, int64(request.Generation)).Scan(&claimAttempt); err != nil {
			return fmt.Errorf("allocate execution claim attempt: %w", err)
		}
		claimID, err := r.newClaimID()
		if err != nil || claimID == "" {
			return fmt.Errorf("generate execution claim ID: %w", err)
		}
		token, err := r.newToken()
		if err != nil || token.IsZero() {
			return fmt.Errorf("generate execution fence token: %w", err)
		}
		var expiresAt, claimObservedAt time.Time
		var initialOutputWatermark int64
		err = tx.QueryRow(ctx, `
WITH authority_clock AS MATERIALIZED (
    SELECT clock_timestamp() AS observed_at
), output_watermark AS MATERIALIZED (
    SELECT COALESCE((
        SELECT last_node_sequence
        FROM elitea_runtime.execution_replay_state
        WHERE execution_id = $2
          AND generation = $3
    ), 0) AS value
)
INSERT INTO elitea_runtime.execution_claims (
    claim_id, execution_id, generation, workload_session_id,
    workload_identity, producer_id, claim_attempt, lease_epoch,
    fence_token, claimed_at, lease_expires_at, initial_output_watermark
) SELECT $1, $2, $3, $4, $5, $6, $7, $7, $8,
         authority_clock.observed_at,
         authority_clock.observed_at + ($9::bigint * interval '1 millisecond'),
         output_watermark.value
FROM authority_clock, output_watermark
RETURNING lease_expires_at,
          (SELECT observed_at FROM authority_clock),
          initial_output_watermark`,
			claimID,
			request.ExecutionID,
			int64(request.Generation),
			request.WorkloadSessionID,
			request.WorkloadIdentity,
			request.ProducerID,
			claimAttempt,
			token[:],
			int64(leaseTTL),
		).Scan(&expiresAt, &claimObservedAt, &initialOutputWatermark)
		if errors.Is(err, pgx.ErrNoRows) {
			return executionapp.ErrInvalidClaim
		}
		if err != nil {
			return fmt.Errorf("insert execution claim: %w", err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET state = CASE WHEN state IN ('PENDING', 'DISPATCHED') THEN 'CLAIMED' ELSE state END
WHERE execution_id = $1 AND generation = $2`, request.ExecutionID, int64(request.Generation)); err != nil {
			return fmt.Errorf("mark execution claimed: %w", err)
		}
		lease := runtimedomain.ActiveLease{
			ClaimID: claimID,
			Fence: runtimedomain.Fence{
				CommandID:         commandID,
				ExecutionID:       request.ExecutionID,
				Generation:        request.Generation,
				WorkloadIdentity:  request.WorkloadIdentity,
				WorkloadSessionID: request.WorkloadSessionID,
				ProducerID:        request.ProducerID,
				ClaimAttempt:      uint64(claimAttempt),
				LeaseEpoch:        uint64(claimAttempt),
				Token:             token,
			},
			ExpiresAt:    expiresAt.UTC(),
			DesiredState: desired,
		}
		if initialOutputWatermark < 0 {
			return executionapp.ErrInvalidClaim
		}
		decision = executionapp.ClaimDecision{
			Lease:                 lease,
			LeaseObservedAt:       claimObservedAt.UTC(),
			Disposition:           executionapp.ClaimAccepted,
			ClaimHandoffWatermark: uint64(initialOutputWatermark),
		}
		if state == executiondomain.JobSucceeded || state == executiondomain.JobFailed || state == executiondomain.JobCancelled || state == executiondomain.JobSettling {
			receipt, settlementErr := loadSettlementForExecution(ctx, tx, request.ExecutionID, request.Generation)
			if settlementErr == nil {
				decision.Disposition = executionapp.ClaimRecoverSettlement
				decision.SettlementRecovery = &executionapp.SettlementRecovery{Receipt: &receipt}
			} else if !errors.Is(settlementErr, pgx.ErrNoRows) {
				return fmt.Errorf("load claim settlement recovery: %w", settlementErr)
			} else {
				// A terminal job without a durable settlement receipt must never be
				// re-executed. Keep the command unacknowledged for reconciliation.
				decision.Disposition = executionapp.ClaimActiveLeaseNoACK
			}
		}
		if decision.Disposition == executionapp.ClaimAccepted {
			proposal, watermark, recoveryErr := loadTerminalSettlementRecovery(ctx, tx, lease.Fence)
			if recoveryErr == nil {
				decision.Disposition = executionapp.ClaimRecoverTerminalACK
				decision.ClaimHandoffWatermark = watermark
				decision.SettlementRecovery = &executionapp.SettlementRecovery{Proposal: &proposal}
			} else if !errors.Is(recoveryErr, pgx.ErrNoRows) {
				return recoveryErr
			} else if terminalRecoveryOnly {
				// Recovery-only authority must never commit as executable authority.
				// The eligibility check above and this exact predecessor lookup run in
				// one transaction, so a miss indicates inconsistent durable state.
				return errors.New("cancelled terminal output is not recoverable from its expired predecessor")
			} else if runningRecoveryOnly {
				// The replacement may only settle the cancellation. Its transport
				// receipt deliberately excludes an input bundle, and BeginExecution
				// rejects a non-RUNNING desired state.
				decision.Disposition = executionapp.ClaimRecoverRunningNoACK
			} else if durable, durableErr := hasDurableTerminalOutput(ctx, tx, lease.Fence.ExecutionID, lease.Fence.Generation); durableErr != nil {
				return durableErr
			} else if durable {
				// A terminal result exists but its predecessor claim is not eligible
				// for this authenticated handoff. Never re-execute business logic.
				decision.Disposition = executionapp.ClaimRetryLaterNoACK
			} else if executiondomain.NodeEventCapability(request.CapabilityID) &&
				state == executiondomain.JobRunning &&
				invocationState != "PREPARING" {
				// A replacement fence over RUNNING may reconcile durable output,
				// but it must never receive business inputs or invoke the SDK.
				decision.Disposition = executionapp.ClaimRecoverAmbiguousInvocationNoACK
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, executionapp.ErrInvalidClaim) || errors.Is(err, runtimedomain.ErrStaleFence) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return executionapp.ClaimDecision{}, err
		}
		return executionapp.ClaimDecision{}, fmt.Errorf("%w: %w", executionapp.ErrClaimDependencyUnavailable, err)
	}
	return decision, nil
}

func (r *ClaimsRepository) BeginExecution(ctx context.Context, fence runtimedomain.Fence) (executionapp.BeginExecutionDisposition, error) {
	if err := fence.Validate(); err != nil {
		return "", err
	}
	var disposition executionapp.BeginExecutionDisposition
	err := r.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		var state, desired, invocationState string
		var live bool
		err := tx.QueryRow(ctx, `
WITH authority_clock AS MATERIALIZED (
    SELECT clock_timestamp() AS observed_at
)
SELECT j.state, j.desired_state, j.invocation_state,
       c.lease_expires_at > authority_clock.observed_at
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.execution_claims AS c
  ON c.execution_id = j.execution_id AND c.generation = j.generation
JOIN authority_clock ON TRUE
WHERE j.execution_id = $1
  AND j.generation = $2
  AND j.command_id = $3
  AND c.workload_identity = $4
  AND c.workload_session_id = $5
  AND c.producer_id = $6
  AND c.claim_attempt = $7
  AND c.lease_epoch = $8
  AND c.fence_token = $9
  AND c.released_at IS NULL
FOR UPDATE OF j, c`,
			fence.ExecutionID,
			int64(fence.Generation),
			fence.CommandID,
			fence.WorkloadIdentity,
			fence.WorkloadSessionID,
			fence.ProducerID,
			int64(fence.ClaimAttempt),
			int64(fence.LeaseEpoch),
			fence.Token[:],
		).Scan(&state, &desired, &invocationState, &live)
		if errors.Is(err, pgx.ErrNoRows) {
			return runtimedomain.ErrStaleFence
		}
		if err != nil {
			return fmt.Errorf("lock execution start authority: %w", err)
		}
		if !live {
			return runtimedomain.ErrLeaseExpired
		}
		if runtimedomain.DesiredState(desired) != runtimedomain.DesiredRunning {
			return runtimedomain.ErrStaleFence
		}
		switch executiondomain.JobState(state) {
		case executiondomain.JobRunning:
			switch invocationState {
			case "PREPARING":
				// No SDK submission was durably authorized. A replacement claim
				// may safely repeat input preparation and reach the final
				// invocation fence.
				disposition = executionapp.BeginExecutionStartedNow
				return nil
			case "MAY_HAVE_STARTED":
				disposition = executionapp.BeginExecutionAlreadyStarted
				return nil
			default:
				return runtimedomain.ErrStaleFence
			}
		case executiondomain.JobClaimed:
			if invocationState != "NOT_STARTED" {
				return runtimedomain.ErrStaleFence
			}
		default:
			return runtimedomain.ErrStaleFence
		}

		tag, err := tx.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET state = 'RUNNING',
    invocation_state = 'PREPARING'
WHERE execution_id = $1
  AND generation = $2
  AND command_id = $3
  AND state = 'CLAIMED'
  AND desired_state = 'RUNNING'`,
			fence.ExecutionID,
			int64(fence.Generation),
			fence.CommandID,
		)
		if err != nil {
			return fmt.Errorf("mark execution running: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return runtimedomain.ErrStaleFence
		}
		disposition = executionapp.BeginExecutionStartedNow
		return nil
	})
	if err != nil {
		if errors.Is(err, runtimedomain.ErrStaleFence) || errors.Is(err, runtimedomain.ErrLeaseExpired) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return "", fmt.Errorf("%w: %w", executionapp.ErrClaimDependencyUnavailable, err)
	}
	return disposition, nil
}

func (r *ClaimsRepository) AuthorizeInvocation(ctx context.Context, fence runtimedomain.Fence) (executionapp.AuthorizeInvocationDisposition, error) {
	if err := fence.Validate(); err != nil {
		return "", err
	}
	var disposition executionapp.AuthorizeInvocationDisposition
	err := r.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		var state, desired, invocationState string
		var live bool
		err := tx.QueryRow(ctx, `
WITH authority_clock AS MATERIALIZED (
    SELECT clock_timestamp() AS observed_at
)
SELECT j.state, j.desired_state, j.invocation_state,
       c.lease_expires_at > authority_clock.observed_at
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.execution_claims AS c
  ON c.execution_id = j.execution_id AND c.generation = j.generation
JOIN authority_clock ON TRUE
WHERE j.execution_id = $1
  AND j.generation = $2
  AND j.command_id = $3
  AND c.workload_identity = $4
  AND c.workload_session_id = $5
  AND c.producer_id = $6
  AND c.claim_attempt = $7
  AND c.lease_epoch = $8
  AND c.fence_token = $9
  AND c.released_at IS NULL
FOR UPDATE OF j, c`,
			fence.ExecutionID,
			int64(fence.Generation),
			fence.CommandID,
			fence.WorkloadIdentity,
			fence.WorkloadSessionID,
			fence.ProducerID,
			int64(fence.ClaimAttempt),
			int64(fence.LeaseEpoch),
			fence.Token[:],
		).Scan(&state, &desired, &invocationState, &live)
		if errors.Is(err, pgx.ErrNoRows) {
			return runtimedomain.ErrStaleFence
		}
		if err != nil {
			return fmt.Errorf("lock invocation authority: %w", err)
		}
		if !live {
			return runtimedomain.ErrLeaseExpired
		}
		if executiondomain.JobState(state) != executiondomain.JobRunning ||
			runtimedomain.DesiredState(desired) != runtimedomain.DesiredRunning {
			return runtimedomain.ErrStaleFence
		}
		switch invocationState {
		case "MAY_HAVE_STARTED":
			disposition = executionapp.AuthorizeInvocationAlready
			return nil
		case "PREPARING":
		default:
			return runtimedomain.ErrStaleFence
		}

		tag, err := tx.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET invocation_state = 'MAY_HAVE_STARTED'
WHERE execution_id = $1
  AND generation = $2
  AND command_id = $3
  AND state = 'RUNNING'
  AND desired_state = 'RUNNING'
  AND invocation_state = 'PREPARING'`,
			fence.ExecutionID,
			int64(fence.Generation),
			fence.CommandID,
		)
		if err != nil {
			return fmt.Errorf("mark invocation authorized: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return runtimedomain.ErrStaleFence
		}
		disposition = executionapp.AuthorizeInvocationNow
		return nil
	})
	if err != nil {
		if errors.Is(err, runtimedomain.ErrStaleFence) || errors.Is(err, runtimedomain.ErrLeaseExpired) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return "", fmt.Errorf("%w: %w", executionapp.ErrClaimDependencyUnavailable, err)
	}
	return disposition, nil
}

// loadClaimHandoffWatermark returns the immutable sequence floor captured when
// this exact claim was created. It must not be recomputed after the claim starts:
// later progress belongs above this handoff boundary.
func loadClaimHandoffWatermark(ctx context.Context, tx sqlExecutor, fence runtimedomain.Fence, claimID string) (uint64, error) {
	if err := fence.Validate(); err != nil || claimID == "" {
		return 0, executionapp.ErrInvalidClaim
	}
	var watermark int64
	if err := tx.QueryRow(ctx, `
SELECT initial_output_watermark
FROM elitea_runtime.execution_claims
WHERE claim_id = $1
  AND execution_id = $2
  AND generation = $3
  AND workload_identity = $4
  AND workload_session_id = $5
  AND producer_id = $6
  AND claim_attempt = $7
  AND lease_epoch = $8
  AND fence_token = $9`,
		claimID,
		fence.ExecutionID,
		int64(fence.Generation),
		fence.WorkloadIdentity,
		fence.WorkloadSessionID,
		fence.ProducerID,
		int64(fence.ClaimAttempt),
		int64(fence.LeaseEpoch),
		fence.Token[:],
	).Scan(&watermark); err != nil {
		return 0, fmt.Errorf("load claim handoff watermark: %w", err)
	}
	if watermark < 0 {
		return 0, executionapp.ErrInvalidClaim
	}
	return uint64(watermark), nil
}

func claimCapabilityAllowed(capabilityID string) bool {
	return executiondomain.SupportedCapability(capabilityID)
}

func exactClaimDigest(stored []byte, requested runtimedomain.Digest) bool {
	return len(stored) == len(requested) && subtle.ConstantTimeCompare(stored, requested[:]) == 1
}

func retiredClaimDecision(
	desired runtimedomain.DesiredState,
	state executiondomain.JobState,
	terminalErrorCode string,
	retirementCode string,
	authorityGranted bool,
	preparedMatches bool,
	publishedMatches bool,
) (executionapp.ClaimDecision, error) {
	if authorityGranted || !preparedMatches || !publishedMatches {
		return executionapp.ClaimDecision{}, executionapp.ErrInvalidClaim
	}
	switch retirementCode {
	case retirementCodeDeadlineExceeded:
		if state != executiondomain.JobFailed || terminalErrorCode != retirementCodeDeadlineExceeded {
			return executionapp.ClaimDecision{}, executionapp.ErrInvalidClaim
		}
		return executionapp.ClaimDecision{
			Disposition:      executionapp.ClaimRetiredACK,
			DesiredState:     desired,
			RetirementReason: executionapp.RetirementDeadlineExceeded,
		}, nil
	case retirementCodeCancelled:
		if desired != runtimedomain.DesiredCancelled || state != executiondomain.JobCancelled || terminalErrorCode != "" {
			return executionapp.ClaimDecision{}, executionapp.ErrInvalidClaim
		}
		return executionapp.ClaimDecision{
			Disposition:  executionapp.ClaimObsoleteACK,
			DesiredState: runtimedomain.DesiredCancelled,
		}, nil
	default:
		return executionapp.ClaimDecision{}, executionapp.ErrInvalidClaim
	}
}

func (r *ClaimsRepository) CurrentLease(ctx context.Context, executionID string, generation uint64) (runtimedomain.ActiveLease, time.Time, error) {
	if executionID == "" || generation == 0 {
		return runtimedomain.ActiveLease{}, time.Time{}, runtimedomain.ErrInvalidFence
	}
	lease, observedAt, live, err := scanLeaseAuthority(r.store.QueryRow(ctx, `
WITH authority_clock AS MATERIALIZED (
    SELECT clock_timestamp() AS observed_at
)
SELECT c.claim_id, j.command_id, c.execution_id, c.generation,
       c.workload_identity, c.workload_session_id, c.producer_id,
       c.claim_attempt, c.lease_epoch,
       c.fence_token, c.lease_expires_at, j.desired_state,
       c.lease_expires_at > authority_clock.observed_at,
       authority_clock.observed_at
FROM elitea_runtime.execution_claims AS c
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = c.execution_id AND j.generation = c.generation
JOIN authority_clock ON TRUE
WHERE c.execution_id = $1 AND c.generation = $2 AND c.released_at IS NULL`, executionID, int64(generation)))
	if errors.Is(err, pgx.ErrNoRows) {
		return runtimedomain.ActiveLease{}, time.Time{}, runtimedomain.ErrStaleFence
	}
	if err != nil {
		return runtimedomain.ActiveLease{}, time.Time{}, fmt.Errorf("load current execution claim: %w", err)
	}
	if !live {
		return runtimedomain.ActiveLease{}, time.Time{}, runtimedomain.ErrLeaseExpired
	}
	return lease, observedAt, nil
}

func (r *ClaimsRepository) RenewLease(ctx context.Context, fence runtimedomain.Fence, leaseTTL executionapp.ClaimLeaseTTLMillis) (runtimedomain.ActiveLease, time.Time, error) {
	if err := fence.Validate(); err != nil {
		return runtimedomain.ActiveLease{}, time.Time{}, err
	}
	if !leaseTTL.Valid() {
		return runtimedomain.ActiveLease{}, time.Time{}, runtimedomain.ErrInvalidFence
	}
	lease, observedAt, _, err := scanLeaseAuthority(r.store.QueryRow(ctx, `
WITH authority_clock AS MATERIALIZED (
    SELECT clock_timestamp() AS observed_at
)
UPDATE elitea_runtime.execution_claims AS c
SET lease_expires_at = authority_clock.observed_at + ($10::bigint * interval '1 millisecond')
FROM elitea_runtime.execution_jobs AS j, authority_clock
WHERE c.execution_id = $1
  AND c.generation = $2
  AND j.execution_id = c.execution_id AND j.generation = c.generation
  AND j.command_id = $3
  AND c.workload_identity = $4
  AND c.workload_session_id = $5
  AND c.producer_id = $6
  AND c.claim_attempt = $7
  AND c.lease_epoch = $8
  AND c.fence_token = $9
  AND c.released_at IS NULL
  AND c.lease_expires_at > authority_clock.observed_at
RETURNING c.claim_id, j.command_id, c.execution_id, c.generation,
          c.workload_identity, c.workload_session_id, c.producer_id,
          c.claim_attempt, c.lease_epoch,
          c.fence_token, c.lease_expires_at, j.desired_state, TRUE,
          authority_clock.observed_at`,
		fence.ExecutionID,
		int64(fence.Generation),
		fence.CommandID,
		fence.WorkloadIdentity,
		fence.WorkloadSessionID,
		fence.ProducerID,
		int64(fence.ClaimAttempt),
		int64(fence.LeaseEpoch),
		fence.Token[:],
		int64(leaseTTL),
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return runtimedomain.ActiveLease{}, time.Time{}, runtimedomain.ErrStaleFence
	}
	if err != nil {
		return runtimedomain.ActiveLease{}, time.Time{}, fmt.Errorf("renew execution claim: %w", err)
	}
	return lease, observedAt, nil
}

func (r *ClaimsRepository) ReleaseClaim(ctx context.Context, fence runtimedomain.Fence) error {
	if err := fence.Validate(); err != nil {
		return err
	}
	commandTag, err := r.store.Exec(ctx, `
UPDATE elitea_runtime.execution_claims AS c
SET released_at = clock_timestamp(), release_reason = 'WORKER_RELEASED'
FROM elitea_runtime.execution_jobs AS j
WHERE c.execution_id = $1
  AND c.generation = $2
  AND j.execution_id = c.execution_id AND j.generation = c.generation
  AND j.command_id = $3
  AND c.workload_identity = $4
  AND c.workload_session_id = $5
  AND c.producer_id = $6
  AND c.claim_attempt = $7
  AND c.lease_epoch = $8
  AND c.fence_token = $9
  AND c.released_at IS NULL
  AND c.lease_expires_at > clock_timestamp()`,
		fence.ExecutionID,
		int64(fence.Generation),
		fence.CommandID,
		fence.WorkloadIdentity,
		fence.WorkloadSessionID,
		fence.ProducerID,
		int64(fence.ClaimAttempt),
		int64(fence.LeaseEpoch),
		fence.Token[:],
	)
	if err != nil {
		return fmt.Errorf("release execution claim: %w", err)
	}
	if commandTag.RowsAffected() != 1 {
		return runtimedomain.ErrStaleFence
	}
	return nil
}

func (r *ClaimsRepository) AbortClaim(ctx context.Context, fence runtimedomain.Fence, disposition executionapp.ClaimAbortDisposition) error {
	if err := fence.Validate(); err != nil {
		return err
	}
	var nextState string
	switch disposition {
	case executionapp.ClaimAbortInputResolutionRetry:
		nextState = string(executiondomain.JobDispatched)
	case executionapp.ClaimAbortInputResolutionExhausted, executionapp.ClaimAbortInputManifestInvalid:
		nextState = string(executiondomain.JobQuarantined)
	default:
		return executionapp.ErrInvalidClaim
	}

	var executionID string
	err := r.store.QueryRow(ctx, `
WITH released AS (
    UPDATE elitea_runtime.execution_claims AS c
    SET released_at = clock_timestamp(), release_reason = $10
    FROM elitea_runtime.execution_jobs AS j
    WHERE c.execution_id = $1
      AND c.generation = $2
      AND j.execution_id = c.execution_id AND j.generation = c.generation
      AND j.command_id = $3
      AND j.state = 'CLAIMED'
      AND c.workload_identity = $4
      AND c.workload_session_id = $5
      AND c.producer_id = $6
      AND c.claim_attempt = $7
      AND c.lease_epoch = $8
      AND c.fence_token = $9
      AND c.released_at IS NULL
      AND c.lease_expires_at > clock_timestamp()
    RETURNING c.execution_id, c.generation
)
UPDATE elitea_runtime.execution_jobs AS j
SET state = $11
FROM released
WHERE j.execution_id = released.execution_id
  AND j.generation = released.generation
RETURNING j.execution_id`,
		fence.ExecutionID,
		int64(fence.Generation),
		fence.CommandID,
		fence.WorkloadIdentity,
		fence.WorkloadSessionID,
		fence.ProducerID,
		int64(fence.ClaimAttempt),
		int64(fence.LeaseEpoch),
		fence.Token[:],
		string(disposition),
		nextState,
	).Scan(&executionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtimedomain.ErrStaleFence
	}
	if err != nil {
		return fmt.Errorf("abort execution claim: %w", err)
	}
	if executionID != fence.ExecutionID {
		return runtimedomain.ErrStaleFence
	}
	return nil
}

func (r *ClaimsRepository) DesiredState(ctx context.Context, executionID string, generation uint64) (runtimedomain.DesiredState, error) {
	if executionID == "" || generation == 0 {
		return "", runtimedomain.ErrInvalidFence
	}
	var state string
	if err := r.store.QueryRow(ctx, `
SELECT desired_state
FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 AND generation = $2`, executionID, int64(generation)).Scan(&state); errors.Is(err, pgx.ErrNoRows) {
		return "", runtimedomain.ErrStaleFence
	} else if err != nil {
		return "", fmt.Errorf("load execution desired state: %w", err)
	}
	desired := runtimedomain.DesiredState(state)
	if !desired.Valid() {
		return "", errors.New("execution contains invalid desired state")
	}
	return desired, nil
}

func loadActiveClaimForUpdate(ctx context.Context, tx sqlExecutor, executionID string, generation uint64, commandID string, desired runtimedomain.DesiredState) (runtimedomain.ActiveLease, time.Time, bool, error) {
	return scanLeaseAuthority(tx.QueryRow(ctx, `
WITH authority_clock AS MATERIALIZED (
    SELECT clock_timestamp() AS observed_at
)
SELECT c.claim_id, $3, c.execution_id, c.generation,
       c.workload_identity, c.workload_session_id, c.producer_id,
       c.claim_attempt, c.lease_epoch,
       c.fence_token, c.lease_expires_at, $4,
       c.lease_expires_at > authority_clock.observed_at,
       authority_clock.observed_at
FROM elitea_runtime.execution_claims AS c
JOIN authority_clock ON TRUE
WHERE c.execution_id = $1 AND c.generation = $2 AND c.released_at IS NULL
FOR UPDATE OF c`, executionID, int64(generation), commandID, string(desired)))
}

func scanLeaseAuthority(row sqlRow) (runtimedomain.ActiveLease, time.Time, bool, error) {
	var observedAt time.Time
	lease, live, err := scanLease(&leaseAuthorityRow{row: row, observedAt: &observedAt})
	if err != nil {
		return runtimedomain.ActiveLease{}, time.Time{}, false, err
	}
	if observedAt.IsZero() {
		return runtimedomain.ActiveLease{}, time.Time{}, false, runtimedomain.ErrInvalidFence
	}
	return lease, observedAt.UTC(), live, nil
}

type leaseAuthorityRow struct {
	row        sqlRow
	observedAt *time.Time
}

func (r *leaseAuthorityRow) Scan(dest ...any) error {
	dest = append(dest, r.observedAt)
	return r.row.Scan(dest...)
}

func scanLease(row sqlRow) (runtimedomain.ActiveLease, bool, error) {
	var lease runtimedomain.ActiveLease
	var generation, claimAttempt, leaseEpoch int64
	var token []byte
	var desired string
	var live bool
	err := row.Scan(
		&lease.ClaimID,
		&lease.Fence.CommandID,
		&lease.Fence.ExecutionID,
		&generation,
		&lease.Fence.WorkloadIdentity,
		&lease.Fence.WorkloadSessionID,
		&lease.Fence.ProducerID,
		&claimAttempt,
		&leaseEpoch,
		&token,
		&lease.ExpiresAt,
		&desired,
		&live,
	)
	if err != nil {
		return runtimedomain.ActiveLease{}, false, err
	}
	if generation <= 0 || claimAttempt <= 0 || leaseEpoch <= 0 || len(token) != len(lease.Fence.Token) {
		return runtimedomain.ActiveLease{}, false, runtimedomain.ErrInvalidFence
	}
	lease.Fence.Generation = uint64(generation)
	lease.Fence.ClaimAttempt = uint64(claimAttempt)
	lease.Fence.LeaseEpoch = uint64(leaseEpoch)
	copy(lease.Fence.Token[:], token)
	lease.ExpiresAt = lease.ExpiresAt.UTC()
	lease.DesiredState = runtimedomain.DesiredState(desired)
	if err := lease.Fence.Validate(); err != nil || lease.ClaimID == "" || !lease.DesiredState.Valid() {
		return runtimedomain.ActiveLease{}, false, runtimedomain.ErrInvalidFence
	}
	return lease, live, nil
}

func loadTerminalSettlementRecovery(ctx context.Context, tx sqlExecutor, fence runtimedomain.Fence) (executionapp.SettlementProposal, uint64, error) {
	var proposal executionapp.SettlementProposal
	var outcome string
	var sequence, watermark int64
	var payloadDigest, proposalBytes, proposalDigest []byte
	err := tx.QueryRow(ctx, `
SELECT settlement_proposal_id,
       settlement_outcome,
       logical_output_id,
       event_id,
       sequence,
       payload_digest,
       settlement_proposal_bytes,
       settlement_proposal_digest,
       settlement_idempotency_key,
       claim_handoff_watermark
FROM elitea_runtime.output_inbox
JOIN elitea_runtime.execution_claims AS source_claim
  ON source_claim.claim_id = output_inbox.claim_id
WHERE output_inbox.execution_id = $1
  AND output_inbox.generation = $2
  AND output_inbox.projected_at IS NOT NULL
  AND (
      (
          output_inbox.claim_attempt = $5
          AND output_inbox.lease_epoch = $6
          AND output_inbox.fence_token = $7
          AND output_inbox.workload_session_id = $8
          AND output_inbox.workload_identity = $3
          AND output_inbox.producer_id = $4
      )
      OR
      (
          output_inbox.claim_attempt < $5
          AND source_claim.released_at IS NOT NULL
          AND source_claim.release_reason = 'LEASE_EXPIRED'
      )
  )
ORDER BY output_inbox.sequence DESC
LIMIT 1`,
		fence.ExecutionID,
		int64(fence.Generation),
		fence.WorkloadIdentity,
		fence.ProducerID,
		int64(fence.ClaimAttempt),
		int64(fence.LeaseEpoch),
		fence.Token[:],
		fence.WorkloadSessionID,
	).Scan(
		&proposal.ProposalID,
		&outcome,
		&proposal.TerminalLogicalOutputID,
		&proposal.TerminalEventID,
		&sequence,
		&payloadDigest,
		&proposalBytes,
		&proposalDigest,
		&proposal.IdempotencyKey,
		&watermark,
	)
	if err != nil {
		return executionapp.SettlementProposal{}, 0, err
	}
	if sequence <= 0 || watermark < 0 {
		return executionapp.SettlementProposal{}, 0, executionapp.ErrInvalidClaim
	}
	proposal.Fence = fence
	proposal.Outcome = executionapp.SettlementOutcome(outcome)
	proposal.TerminalSequence = uint64(sequence)
	if proposal.TerminalPayloadDigest, err = storedDigest(payloadDigest); err != nil {
		return executionapp.SettlementProposal{}, 0, executionapp.ErrInvalidClaim
	}
	if proposal.ProposalDigest, err = storedDigest(proposalDigest); err != nil {
		return executionapp.SettlementProposal{}, 0, executionapp.ErrInvalidClaim
	}
	if err := proposal.Validate(); err != nil {
		return executionapp.SettlementProposal{}, 0, executionapp.ErrInvalidClaim
	}
	canonical, err := settlementProposalBytes(proposal)
	if err != nil || !bytes.Equal(canonical, proposalBytes) {
		return executionapp.SettlementProposal{}, 0, executionapp.ErrInvalidClaim
	}
	return proposal, uint64(watermark), nil
}

func hasDurableTerminalOutput(ctx context.Context, tx sqlExecutor, executionID string, generation uint64) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM elitea_runtime.output_inbox
    WHERE execution_id = $1 AND generation = $2 AND projected_at IS NOT NULL
)`, executionID, int64(generation)).Scan(&exists); err != nil {
		return false, fmt.Errorf("check durable terminal output: %w", err)
	}
	return exists, nil
}

func hasExpiredPredecessorTerminalOutput(ctx context.Context, tx sqlExecutor, executionID string, generation uint64) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM elitea_runtime.output_inbox AS output
    JOIN elitea_runtime.execution_claims AS source_claim
      ON source_claim.claim_id = output.claim_id
    WHERE output.execution_id = $1
      AND output.generation = $2
      AND output.projected_at IS NOT NULL
      AND source_claim.released_at IS NOT NULL
      AND source_claim.release_reason = 'LEASE_EXPIRED'
)`, executionID, int64(generation)).Scan(&exists); err != nil {
		return false, fmt.Errorf("check expired predecessor terminal output: %w", err)
	}
	return exists, nil
}

func hasExpiredPredecessorClaim(ctx context.Context, tx sqlExecutor, executionID string, generation uint64) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM elitea_runtime.execution_claims AS source_claim
    WHERE source_claim.execution_id = $1
      AND source_claim.generation = $2
      AND source_claim.released_at IS NOT NULL
      AND source_claim.release_reason = 'LEASE_EXPIRED'
)`, executionID, int64(generation)).Scan(&exists); err != nil {
		return false, fmt.Errorf("check expired predecessor claim: %w", err)
	}
	return exists, nil
}

func randomClaimID() (string, error) {
	var value [16]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func randomFenceToken() (runtimedomain.FenceToken, error) {
	var token runtimedomain.FenceToken
	_, err := io.ReadFull(rand.Reader, token[:])
	return token, err
}

var _ executionapp.ClaimRepository = (*ClaimsRepository)(nil)
