package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/noderecovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/workloadidentity"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
)

type recoveryClaimScope struct {
	projectID      int64
	actorID        int64
	responseID     string
	desired        string
	bundleID       string
	manifestDigest []byte
}

const recoveryClaimAuthoritySQL = `
SELECT j.resource_project_id,j.actor_id::bigint,binding.client_message_id,j.desired_state,j.input_bundle_id,bundle.manifest_digest
FROM elitea_runtime.execution_jobs j
JOIN elitea_runtime.execution_claims c USING(execution_id,generation)
JOIN elitea_runtime.workload_sessions ws ON ws.workload_session_id=c.workload_session_id AND ws.workload_identity=c.workload_identity AND ws.producer_id=c.producer_id
JOIN elitea_runtime.command_outbox command USING(execution_id,generation)
JOIN elitea_runtime.agent_execution_jobs binding USING(execution_id,generation)
JOIN elitea_runtime.input_bundles bundle ON bundle.input_bundle_id=j.input_bundle_id
WHERE c.claim_id=$1 AND c.execution_id=$2 AND c.generation=$3 AND c.workload_identity=$4 AND c.fence_token=$5
 AND c.released_at IS NULL AND c.lease_expires_at>clock_timestamp() AND c.recovery_mode='NODE_RECOVERY'
 AND ws.issued_at<=clock_timestamp() AND ws.expires_at>clock_timestamp() AND ws.revoked_at IS NULL
 AND j.state='RUNNING' AND j.desired_state IN ('SUSPENDED','RUNNING')
 AND j.tenant_id=j.resource_project_id::text AND j.projection_project_id=j.resource_project_id
 AND j.capability_id IN ('agent.execute.application.v1','agent.execute.adhoc.v1') AND binding.capability_id=j.capability_id
 AND command.retired_at IS NULL AND command.authority_granted_at IS NOT NULL AND command.deadline>clock_timestamp()
 AND NOT EXISTS(SELECT 1 FROM elitea_runtime.output_inbox terminal WHERE terminal.execution_id=j.execution_id AND terminal.generation=j.generation)
FOR UPDATE OF j,c,ws,command`

func (r *NodeRecoveryRepository) lockRecoveryClaim(ctx context.Context, tx sqlExecutor, claim storage.ContentClaim) (recoveryClaimScope, error) {
	identity, err := workloadidentity.Certificate(claim.PeerCertificate)
	if err != nil {
		return recoveryClaimScope{}, storage.ErrContentUnauthorized
	}
	if !domain.ValidExecutionID(claim.ExecutionID) || claim.Generation == 0 || len(claim.FenceToken) != 32 || claim.ClaimID == "" {
		return recoveryClaimScope{}, storage.ErrContentUnauthorized
	}
	args := []any{claim.ClaimID, claim.ExecutionID, int64(claim.Generation), identity, claim.FenceToken}
	var scope recoveryClaimScope
	for i := 0; i < 2; i++ {
		err = tx.QueryRow(ctx, recoveryClaimAuthoritySQL, args...).Scan(&scope.projectID, &scope.actorID, &scope.responseID, &scope.desired, &scope.bundleID, &scope.manifestDigest)
		if errors.Is(err, pgx.ErrNoRows) {
			return recoveryClaimScope{}, storage.ErrContentUnauthorized
		}
		if err != nil {
			return recoveryClaimScope{}, err
		}
	}
	if len(scope.manifestDigest) != 32 || scope.bundleID == "" {
		return recoveryClaimScope{}, storage.ErrContentUnauthorized
	}
	if err := r.permissions(ctx, tx, app.Selector{ProjectID: scope.projectID, ActorUserID: scope.actorID, ResponseMessageID: scope.responseID}, "models.chat.messages.create"); err != nil {
		return recoveryClaimScope{}, storage.ErrContentUnauthorized
	}
	return scope, nil
}

func (r *NodeRecoveryRepository) PollNodeRecovery(ctx context.Context, claim storage.ContentClaim) (storage.NodeRecoveryControl, error) {
	if r == nil || r.shared == nil || r.permissions == nil {
		return storage.NodeRecoveryControl{}, storage.ErrContentUnavailable
	}
	var control storage.NodeRecoveryControl
	err := r.shared.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		scope, err := r.lockRecoveryClaim(ctx, tx, claim)
		if err != nil {
			return err
		}
		var raw, digest []byte
		var activation string
		var revision uint64
		err = tx.QueryRow(ctx, `SELECT receipt_json,receipt_digest,activation_id,journal_revision FROM elitea_runtime.node_recovery_visits
WHERE execution_id=$1 AND generation=$2 AND status IN ('SUSPENDED','AUTHORIZED','RESUMED','STOPPED') ORDER BY created_at DESC,journal_revision DESC LIMIT 1 FOR UPDATE`, claim.ExecutionID, int64(claim.Generation)).Scan(&raw, &digest, &activation, &revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return storage.ErrContentRejected
		}
		if err != nil {
			return err
		}
		receipt, err := domain.DecodeReceipt(raw)
		if err != nil {
			return storage.ErrContentRejected
		}
		want := sha256.Sum256(raw)
		if receipt.ActivationID != activation || receipt.JournalRevision != revision || !bytes.Equal(digest, want[:]) {
			return storage.ErrContentRejected
		}
		var action []byte
		err = tx.QueryRow(ctx, `SELECT action_json FROM elitea_runtime.node_recovery_control_outbox
WHERE execution_id=$1 AND generation=$2 AND activation_id=$3 AND expected_revision=$4`, claim.ExecutionID, int64(claim.Generation), activation, int64(revision)).Scan(&action)
		if errors.Is(err, pgx.ErrNoRows) {
			action = []byte("null")
		} else if err != nil {
			return err
		}
		control = storage.NodeRecoveryControl{Schema: "elitea.pipeline.node-recovery-control.v1", ExecutionID: claim.ExecutionID, Generation: claim.Generation, DesiredState: scope.desired, Receipt: bytes.Clone(raw), Action: bytes.Clone(action)}
		return nil
	})
	if err != nil {
		return storage.NodeRecoveryControl{}, err
	}
	return control, nil
}

func (r *NodeRecoveryRepository) AcknowledgeNodeRecovery(ctx context.Context, claim storage.ContentClaim, ack storage.NodeRecoveryAck) (storage.NodeRecoveryAckOutcome, error) {
	if r == nil || r.shared == nil || r.permissions == nil || !ack.Validate() {
		return storage.NodeRecoveryAckOutcome{}, storage.ErrContentRejected
	}
	var outcome storage.NodeRecoveryAckOutcome
	err := r.shared.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		scope, err := r.lockRecoveryClaim(ctx, tx, claim)
		if err != nil {
			return err
		}
		current, err := lockLatestNodeRecoveryVisit(ctx, tx, claim.ExecutionID, claim.Generation)
		if err != nil {
			return err
		}
		if current.activation != ack.ActivationID || current.revision != ack.ExpectedRevision {
			return storage.ErrContentRejected
		}
		var raw, digest, actionBytes, savedAck []byte
		var status, actor, consumedClaim string
		var applied uint64
		err = tx.QueryRow(ctx, `SELECT visit.receipt_json,visit.receipt_digest,visit.status,action.action_json,action.actor_id,
COALESCE(action.consumed_claim_id,''),COALESCE(action.applied_revision,0),action.ack_json
FROM elitea_runtime.node_recovery_visits visit JOIN elitea_runtime.node_recovery_control_outbox action
 ON action.execution_id=visit.execution_id AND action.generation=visit.generation AND action.activation_id=visit.activation_id AND action.expected_revision=visit.journal_revision
WHERE visit.execution_id=$1 AND visit.generation=$2 AND visit.activation_id=$3 AND visit.journal_revision=$4 AND action.request_id=$5
FOR UPDATE OF visit,action`, claim.ExecutionID, int64(claim.Generation), ack.ActivationID, int64(ack.ExpectedRevision), ack.RequestID).Scan(&raw, &digest, &status, &actionBytes, &actor, &consumedClaim, &applied, &savedAck)
		if errors.Is(err, pgx.ErrNoRows) {
			return storage.ErrContentRejected
		}
		if err != nil {
			return err
		}
		receipt, err := domain.DecodeReceipt(raw)
		if err != nil {
			return storage.ErrContentRejected
		}
		want := sha256.Sum256(raw)
		var action recoveryAction
		if json.Unmarshal(actionBytes, &action) != nil || action.RequestID != ack.RequestID || action.ActivationID != ack.ActivationID || action.ExpectedRevision != ack.ExpectedRevision || action.LastAttempt != receipt.Attempt || action.ReceiptSHA256 != ack.ReceiptSHA256 || action.Action != receipt.AllowedActions[0] || receipt.ActivationID != ack.ActivationID || receipt.JournalRevision != ack.ExpectedRevision || !bytes.Equal(digest, want[:]) || ack.ReceiptSHA256 != hex.EncodeToString(want[:]) || actor != strconv.FormatInt(scope.actorID, 10) {
			return storage.ErrContentRejected
		}
		if ack.FailureRouteContinuation != nil {
			owner, ok := r.effects.(NodeRecoveryFailureRouteAuthorizer)
			if !ok || owner.ValidateNodeRecoveryFailureRoute(ctx, tx, claim, receipt, ack.FailureRouteContinuation) != nil {
				return storage.ErrContentRejected
			}
		}
		// The authenticated Worker supplies this only after its exact journal CAS.
		// Main separately checks the stored operator action and effect-owner proof.
		branch, err := recoveryAckBranch(claim, ack, receipt, action)
		if err != nil {
			return err
		}
		ackBytes, err := canonicalRecoveryAck(ack)
		if err != nil {
			return storage.ErrContentRejected
		}
		replay := consumedClaim != ""
		if replay {
			if status != branch.status || applied != ack.AppliedRevision || scope.desired != branch.desired || !bytes.Equal(savedAck, ackBytes) {
				return storage.ErrContentRejected
			}
		} else {
			if status != "AUTHORIZED" || scope.desired != "SUSPENDED" {
				return storage.ErrContentRejected
			}
			if action.Action != "retry" || receipt.ReplaySafety.Kind != "no_external_effect" {
				if r.effects == nil {
					return storage.ErrContentRejected
				}
				proof, err := r.effects.VerifyNodeRecoveryEffect(ctx, tx, claim.ExecutionID, claim.Generation, receipt, action.Action)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					return storage.ErrContentRejected
				}
				if !bytes.Equal(proof, action.OwnerProof) {
					return storage.ErrContentRejected
				}
			}
			tag, err := tx.Exec(ctx, `UPDATE elitea_runtime.node_recovery_control_outbox SET consumed_at=clock_timestamp(),consumed_claim_id=$6,applied_revision=$7,ack_json=$8
WHERE execution_id=$1 AND generation=$2 AND activation_id=$3 AND expected_revision=$4 AND request_id=$5 AND consumed_at IS NULL`, claim.ExecutionID, int64(claim.Generation), ack.ActivationID, int64(ack.ExpectedRevision), ack.RequestID, claim.ClaimID, int64(ack.AppliedRevision), ackBytes)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return storage.ErrContentRejected
			}
			tag, err = tx.Exec(ctx, `UPDATE elitea_runtime.node_recovery_visits SET status=$5,updated_at=clock_timestamp()
WHERE execution_id=$1 AND generation=$2 AND activation_id=$3 AND journal_revision=$4 AND status='AUTHORIZED'`, claim.ExecutionID, int64(claim.Generation), ack.ActivationID, int64(ack.ExpectedRevision), branch.status)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return storage.ErrContentRejected
			}
			if branch.status == "RECONCILED" {
				if err := persistReconciledVisit(ctx, tx, scope, claim, ack, branch.continuation); err != nil {
					return err
				}
			} else {
				tag, err = tx.Exec(ctx, `UPDATE elitea_runtime.execution_jobs SET desired_state='RUNNING'
WHERE execution_id=$1 AND generation=$2 AND desired_state='SUSPENDED' AND state='RUNNING'`, claim.ExecutionID, int64(claim.Generation))
				if err != nil {
					return err
				}
				if tag.RowsAffected() != 1 {
					return storage.ErrContentRejected
				}
			}
			_, err = tx.Exec(ctx, `INSERT INTO elitea_runtime.node_recovery_audit(execution_id,generation,request_id,transition,actor_id,activation_id,journal_revision,claim_id)
VALUES($1,$2,$3,'APPLIED',$4,$5,$6,$7)`, claim.ExecutionID, int64(claim.Generation), ack.RequestID, actor, ack.ActivationID, int64(ack.AppliedRevision), claim.ClaimID)
			if err != nil {
				return err
			}
		}
		outcome = storage.NodeRecoveryAckOutcome{Schema: "elitea.pipeline.node-recovery-ack.v1", ExecutionID: claim.ExecutionID, Generation: claim.Generation, RequestID: ack.RequestID, AppliedRevision: ack.AppliedRevision, Replay: replay}
		switch branch.status {
		case "RESUMED":
			outcome.RecoveryResumeAuthorized = true
			outcome.Resumption = &storage.NodeRecoveryResumption{Schema: "elitea.pipeline.node-recovery-resumption.v1", ExecutionID: claim.ExecutionID, Generation: claim.Generation, RequestID: ack.RequestID, ActivationID: ack.ActivationID, JournalRevision: ack.AppliedRevision, InputBundleID: scope.bundleID, InputManifestSHA256: hex.EncodeToString(scope.manifestDigest), ReceiptSHA256: ack.ReceiptSHA256, ClaimID: claim.ClaimID, FailureRouteContinuation: ack.FailureRouteContinuation}

		case "STOPPED":
			outcome.TerminalSettlementAuthorized = true
			outcome.TerminalAuthorization = &storage.NodeRecoveryTerminalSettlement{Schema: "elitea.pipeline.node-recovery-terminal-settlement.v1", ExecutionID: claim.ExecutionID, Generation: claim.Generation, RequestID: ack.RequestID, ActivationID: ack.ActivationID, JournalRevision: ack.AppliedRevision, ReceiptSHA256: ack.ReceiptSHA256, ClaimID: claim.ClaimID, StopReason: *ack.TerminalStopReason}

		}
		return nil
	})
	if err != nil {
		return storage.NodeRecoveryAckOutcome{}, err
	}
	return outcome, nil
}

var _ storage.NodeRecoveryControlStore = (*NodeRecoveryRepository)(nil)

// A no-effect reconciliation records evidence only; it never grants an automatic retry.
type nodeRecoveryAckBranch struct {
	status, desired string
	continuation    []byte
}

func recoveryAckBranch(claim storage.ContentClaim, ack storage.NodeRecoveryAck, receipt domain.Receipt, action recoveryAction) (nodeRecoveryAckBranch, error) {
	var proof domain.OwnerProof
	if action.Action != "retry" || receipt.ReplaySafety.Kind != "no_external_effect" {
		var err error
		proof, err = domain.DecodeOwnerProof(action.OwnerProof, claim.ExecutionID, claim.Generation, receipt, action.Action)
		if err != nil {
			return nodeRecoveryAckBranch{}, storage.ErrContentRejected
		}
	} else if len(action.OwnerProof) > 0 && !bytes.Equal(bytes.TrimSpace(action.OwnerProof), []byte("null")) {
		return nodeRecoveryAckBranch{}, storage.ErrContentRejected
	}
	if ack.FailureRouteContinuation != nil {
		if action.Action != "reconcile" || proof.Kind != "verified_no_effect" || proof.OwnerReceiptRef == nil || proof.OwnerReceiptRef.RequiredGrantAudience != domain.CodeResultAudience || ack.FailureRouteContinuation.Failed.Attempt != receipt.Attempt || ack.FailureRouteContinuation.Failed.FailureClass != receipt.FailureClass {
			return nodeRecoveryAckBranch{}, storage.ErrContentRejected
		}
		return nodeRecoveryAckBranch{status: "RESUMED", desired: "RUNNING"}, nil
	}
	if ack.TerminalStopReason != nil {
		// A stopped-journal proof cannot be used as graph restore authority.
		return nodeRecoveryAckBranch{status: "STOPPED", desired: "RUNNING"}, nil
	}
	if len(ack.ContinuationReceipt) > 0 && !bytes.Equal(bytes.TrimSpace(ack.ContinuationReceipt), []byte("null")) {
		if action.Action != "reconcile" || proof.Kind != "verified_no_effect" {
			return nodeRecoveryAckBranch{}, storage.ErrContentRejected
		}
		next, err := domain.DecodeReceipt(ack.ContinuationReceipt)
		if err != nil || next.ActivationID != receipt.ActivationID || next.JournalRevision != ack.AppliedRevision || next.NodeID != receipt.NodeID || next.GraphThread != receipt.GraphThread || next.Step != receipt.Step || next.Attempt != receipt.Attempt || next.FailureClass != receipt.FailureClass || next.ReplaySafety.Kind != "idempotent_effect_not_committed" || next.ReplaySafety.EffectID != proof.EffectID || next.AllowedActions[0] != "retry" {
			return nodeRecoveryAckBranch{}, storage.ErrContentRejected
		}
		canonical, err := domain.CanonicalReceipt(ack.ContinuationReceipt)
		if err != nil {
			return nodeRecoveryAckBranch{}, storage.ErrContentRejected
		}
		return nodeRecoveryAckBranch{status: "RECONCILED", desired: "SUSPENDED", continuation: canonical}, nil
	}
	if action.Action != "retry" && proof.Kind != "committed_result" {
		return nodeRecoveryAckBranch{}, storage.ErrContentRejected
	}
	return nodeRecoveryAckBranch{status: "RESUMED", desired: "RUNNING"}, nil
}

func canonicalRecoveryAck(ack storage.NodeRecoveryAck) ([]byte, error) {
	if len(ack.ContinuationReceipt) == 0 {
		ack.ContinuationReceipt = json.RawMessage("null")
	} else if !bytes.Equal(bytes.TrimSpace(ack.ContinuationReceipt), []byte("null")) {
		canonical, err := domain.CanonicalReceipt(ack.ContinuationReceipt)
		if err != nil {
			return nil, err
		}
		ack.ContinuationReceipt = canonical
	} else {
		ack.ContinuationReceipt = json.RawMessage("null")
	}
	return json.Marshal(ack)
}

func persistReconciledVisit(ctx context.Context, tx sqlExecutor, scope recoveryClaimScope, claim storage.ContentClaim, ack storage.NodeRecoveryAck, raw []byte) error {
	r, err := domain.DecodeReceipt(raw)
	if err != nil {
		return storage.ErrContentRejected
	}
	digest := sha256.Sum256(raw)
	tag, err := tx.Exec(ctx, `INSERT INTO elitea_runtime.node_recovery_visits
(execution_id,generation,activation_id,journal_revision,node_id,graph_thread,step,attempt,receipt_json,receipt_digest,source_event_id,source_claim_id,status)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'SUSPENDED')`, claim.ExecutionID, int64(claim.Generation), r.ActivationID, int64(r.JournalRevision), r.NodeID, r.GraphThread, int64(r.Step), r.Attempt, raw, digest[:], "node-reconcile:"+ack.RequestID, claim.ClaimID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return storage.ErrContentRejected
	}
	schema, err := currentProjectSchema(scope.projectID)
	if err != nil {
		return err
	}
	tag, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.chat_message_group SET meta=jsonb_set(COALESCE(meta,'{}'::jsonb),'{node_recovery_required_v1}',$2::jsonb),updated_at=clock_timestamp()
WHERE uuid=$1::uuid AND task_id=$3 AND is_streaming=TRUE`, schema), scope.responseID, raw, claim.ExecutionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return storage.ErrContentRejected
	}
	return nil
}

type NodeRecoveryFailureRouteAuthorizer interface {
	ValidateNodeRecoveryFailureRoute(context.Context, sqlExecutor, storage.ContentClaim, domain.Receipt, *storage.NodeRecoveryFailureRoute) error
}
