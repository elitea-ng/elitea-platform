package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	runtime "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
)

type nodeRecoveryClaimVisit struct {
	receipt []byte
	status  string
}

func loadLatestNodeRecoveryClaimVisit(ctx context.Context, tx sqlExecutor, executionID string, generation uint64) (nodeRecoveryClaimVisit, error) {
	var raw, digest []byte
	var status string
	err := tx.QueryRow(ctx, `SELECT receipt_json,receipt_digest,status FROM elitea_runtime.node_recovery_visits
WHERE execution_id=$1 AND generation=$2
ORDER BY created_at DESC,journal_revision DESC LIMIT 1 FOR UPDATE`, executionID, int64(generation)).Scan(&raw, &digest, &status)
	if err != nil {
		return nodeRecoveryClaimVisit{}, err
	}
	wanted := sha256.Sum256(raw)
	if _, err := domain.DecodeReceipt(raw); err != nil || !bytes.Equal(digest, wanted[:]) {
		return nodeRecoveryClaimVisit{}, errors.New("stored node recovery receipt is invalid")
	}
	return nodeRecoveryClaimVisit{receipt: bytes.Clone(raw), status: status}, nil
}

func (v nodeRecoveryClaimVisit) matches(desired runtime.DesiredState) bool {
	return (desired == runtime.DesiredSuspended && (v.status == "SUSPENDED" || v.status == "AUTHORIZED")) ||
		(desired == runtime.DesiredRunning && v.status == "STOPPED")
}

func loadNodeRecoveryClaimReceipt(ctx context.Context, tx sqlExecutor, executionID string, generation uint64, desired runtime.DesiredState) ([]byte, error) {
	visit, err := loadLatestNodeRecoveryClaimVisit(ctx, tx, executionID, generation)
	if err != nil {
		return nil, err
	}
	if !visit.matches(desired) {
		return nil, executionapp.ErrInvalidClaim
	}
	return visit.receipt, nil
}

func selectNodeRecoveryClaim(ctx context.Context, tx sqlExecutor, request executionapp.ClaimRequest, lease runtime.ActiveLease, invocationState string, decision *executionapp.ClaimDecision) error {
	if request.CapabilityID != executiondomain.AgentApplicationCapability && request.CapabilityID != executiondomain.AgentAdhocCapability {
		return executionapp.ErrInvalidClaim
	}
	visit, err := loadLatestNodeRecoveryClaimVisit(ctx, tx, request.ExecutionID, request.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		if nodeCheckpointInspectionEligible(lease, invocationState, decision.Disposition) {
			return selectNodeCheckpointInspection(ctx, tx, lease, decision)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if visit.status == "RESUMED" && nodeCheckpointInspectionEligible(lease, invocationState, decision.Disposition) {
		return selectNodeCheckpointInspection(ctx, tx, lease, decision)
	}
	if !visit.matches(lease.DesiredState) {
		return executionapp.ErrInvalidClaim
	}
	if err := setNodeRecoveryClaimMode(ctx, tx, lease, "NODE_RECOVERY"); err != nil {
		return err
	}
	decision.Disposition = executionapp.ClaimRecoverNodeVisit
	decision.NodeRecoveryReceipt = string(visit.receipt)
	return nil
}

func nodeCheckpointInspectionEligible(lease runtime.ActiveLease, invocationState string, disposition executionapp.ClaimDisposition) bool {
	return lease.DesiredState == runtime.DesiredRunning && invocationState == "MAY_HAVE_STARTED" &&
		(disposition == executionapp.ClaimRecoverAmbiguousInvocationNoACK || disposition == executionapp.ClaimRecoverRunningNoACK)
}

func selectNodeCheckpointInspection(ctx context.Context, tx sqlExecutor, lease runtime.ActiveLease, decision *executionapp.ClaimDecision) error {
	if err := setNodeRecoveryClaimMode(ctx, tx, lease, "AGENT_MODEL_CHECKPOINT"); err != nil {
		return err
	}
	decision.Disposition = executionapp.ClaimRecoverAgentModelCheckpoint
	decision.NodeRecoveryReceipt = ""
	return nil
}

// The caller holds the original job lock and selects the exact current visit.
// Preserve any existing one-use model checkpoint digest during mode changes.
func setNodeRecoveryClaimMode(ctx context.Context, tx sqlExecutor, lease runtime.ActiveLease, mode string) error {
	if mode != "NODE_RECOVERY" && mode != "AGENT_MODEL_CHECKPOINT" {
		return executionapp.ErrInvalidClaim
	}
	f := lease.Fence
	tag, err := tx.Exec(ctx, `UPDATE elitea_runtime.execution_claims c SET recovery_mode=$1
WHERE c.claim_id=$2 AND c.execution_id=$3 AND c.generation=$4
 AND c.workload_identity=$6 AND c.workload_session_id=$7 AND c.producer_id=$8
 AND c.claim_attempt=$9 AND c.lease_epoch=$10 AND c.fence_token=$11
 AND c.released_at IS NULL AND c.lease_expires_at>clock_timestamp()
 AND (c.recovery_mode IN ('NONE','NODE_RECOVERY')
      OR ($1='AGENT_MODEL_CHECKPOINT' AND c.recovery_mode='AGENT_MODEL_CHECKPOINT')
      OR ($1='NODE_RECOVERY' AND $12='SUSPENDED' AND c.recovery_mode='AGENT_MODEL_CHECKPOINT'))
 AND EXISTS(SELECT 1 FROM elitea_runtime.execution_jobs j
   JOIN elitea_runtime.command_outbox command USING(execution_id,generation)
   JOIN elitea_runtime.workload_sessions ws ON ws.workload_session_id=c.workload_session_id
    AND ws.workload_identity=c.workload_identity AND ws.producer_id=c.producer_id
   WHERE j.execution_id=c.execution_id AND j.generation=c.generation AND j.command_id=$5
    AND j.state='RUNNING' AND j.desired_state=$12
    AND j.capability_id IN ('agent.execute.application.v1','agent.execute.adhoc.v1')
    AND ($1='NODE_RECOVERY' OR j.invocation_state='MAY_HAVE_STARTED')
    AND command.retired_at IS NULL
    AND command.authority_granted_at IS NOT NULL AND command.deadline>clock_timestamp()
    AND ws.issued_at<=clock_timestamp() AND ws.expires_at>clock_timestamp() AND ws.revoked_at IS NULL
    AND NOT EXISTS(SELECT 1 FROM elitea_runtime.output_inbox terminal
      WHERE terminal.execution_id=j.execution_id AND terminal.generation=j.generation))`, mode, lease.ClaimID, f.ExecutionID, int64(f.Generation), f.CommandID, f.WorkloadIdentity, f.WorkloadSessionID, f.ProducerID, int64(f.ClaimAttempt), int64(f.LeaseEpoch), f.Token[:], string(lease.DesiredState))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return runtime.ErrStaleFence
	}
	return nil
}
