package repos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
)

// AuthorizeAgentModelCheckpoint grants one restored model attempt per claim.
// The digest is an authority receipt. Checkpoint contents stay in the worker store.
func (r *ClaimsRepository) AuthorizeAgentModelCheckpoint(ctx context.Context, fence runtimedomain.Fence, digest runtimedomain.Digest) (executionapp.AuthorizeInvocationDisposition, error) {
	if err := fence.Validate(); err != nil {
		return "", err
	}
	if digest.IsZero() {
		return "", executionapp.ErrInvalidClaim
	}
	var disposition executionapp.AuthorizeInvocationDisposition
	err := r.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		var state, desired, invocationState, capability, mode string
		var storedDigest []byte
		var live bool
		err := tx.QueryRow(ctx, `
WITH authority_clock AS MATERIALIZED (
    SELECT clock_timestamp() AS observed_at
)
SELECT j.state, j.desired_state, j.invocation_state, j.capability_id, c.recovery_mode, c.model_checkpoint_digest,
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
		).Scan(&state, &desired, &invocationState, &capability, &mode, &storedDigest, &live)
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
		if invocationState != "MAY_HAVE_STARTED" || mode != "AGENT_MODEL_CHECKPOINT" ||
			(capability != executiondomain.AgentApplicationCapability && capability != executiondomain.AgentAdhocCapability) {
			return runtimedomain.ErrStaleFence
		}
		if storedDigest != nil {
			if !bytes.Equal(storedDigest, digest[:]) {
				return executionapp.ErrInvalidClaim
			}
			disposition = executionapp.AuthorizeInvocationAlready
			return nil
		}
		terminal, err := hasDurableTerminalOutput(ctx, tx, fence.ExecutionID, fence.Generation)
		if err != nil {
			return err
		}
		if terminal {
			return runtimedomain.ErrStaleFence
		}
		tag, err := tx.Exec(ctx, `UPDATE elitea_runtime.execution_claims
SET model_checkpoint_digest = $1
WHERE execution_id = $2 AND generation = $3 AND claim_attempt = $4
  AND released_at IS NULL AND recovery_mode = 'AGENT_MODEL_CHECKPOINT'
  AND model_checkpoint_digest IS NULL`, digest[:], fence.ExecutionID, int64(fence.Generation), int64(fence.ClaimAttempt))
		if err != nil {
			return fmt.Errorf("record model checkpoint authority: %w", err)
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
