package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
)

// NodeRecoveryResultSource reads a committed result owned by the effect adapter.
// It may neither dispatch an effect nor infer success from a missing record.
type NodeRecoveryResultSource interface {
	ReadNodeRecoveryHTTPResult(context.Context, sqlExecutor, string, uint64, domain.Receipt, json.RawMessage) ([]byte, error)
}

func (r *NodeRecoveryRepository) ReadNodeRecoveryResult(ctx context.Context, claim storage.ContentClaim, contentID, version string) ([]byte, error) {
	if r == nil || r.shared == nil || r.permissions == nil || !domain.ValidID(contentID) || !domain.ValidID(version) {
		return nil, storage.ErrContentRejected
	}
	owner, ok := r.effects.(NodeRecoveryResultSource)
	if !ok {
		return nil, storage.ErrContentRejected
	}
	var result []byte
	err := r.shared.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		scope, err := r.lockRecoveryClaim(ctx, tx, claim)
		if err != nil {
			return err
		}
		current, err := lockLatestNodeRecoveryVisit(ctx, tx, claim.ExecutionID, claim.Generation)
		if err != nil {
			return err
		}
		var raw, digest, actionBytes []byte
		var actor, status string
		err = tx.QueryRow(ctx, `SELECT visit.receipt_json,visit.receipt_digest,action.action_json,action.actor_id,visit.status
FROM elitea_runtime.node_recovery_visits visit JOIN elitea_runtime.node_recovery_control_outbox action
 ON action.execution_id=visit.execution_id AND action.generation=visit.generation AND action.activation_id=visit.activation_id AND action.expected_revision=visit.journal_revision
WHERE visit.execution_id=$1 AND visit.generation=$2 AND visit.activation_id=$3 AND visit.journal_revision=$4 AND visit.status IN ('AUTHORIZED','RESUMED')
FOR UPDATE OF visit,action`, claim.ExecutionID, int64(claim.Generation), current.activation, int64(current.revision)).Scan(&raw, &digest, &actionBytes, &actor, &status)
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
		hash := sha256.Sum256(raw)
		var action recoveryAction
		if receipt.ActivationID != current.activation || receipt.JournalRevision != current.revision || json.Unmarshal(actionBytes, &action) != nil || action.ActivationID != receipt.ActivationID || action.ExpectedRevision != receipt.JournalRevision || action.LastAttempt != receipt.Attempt || action.ReceiptSHA256 != hex.EncodeToString(hash[:]) || !bytes.Equal(digest, hash[:]) || actor != strconv.FormatInt(scope.actorID, 10) || ((status == "AUTHORIZED") != (scope.desired == "SUSPENDED")) {
			return storage.ErrContentRejected
		}
		proof, err := domain.DecodeOwnerProof(action.OwnerProof, claim.ExecutionID, claim.Generation, receipt, action.Action)
		ref := proof.ResultRef
		if proof.Kind == "verified_no_effect" {
			ref = proof.OwnerReceiptRef
		}
		if err != nil || ref == nil || ref.ContentID != contentID || ref.ImmutableVersion != version || (proof.Kind == "verified_no_effect" && ref.RequiredGrantAudience != domain.CodeResultAudience) {
			return storage.ErrContentRejected
		}
		data, err := owner.ReadNodeRecoveryHTTPResult(ctx, tx, claim.ExecutionID, claim.Generation, receipt, action.OwnerProof)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return storage.ErrContentRejected
		}
		resultDigest := sha256.Sum256(data)
		if uint64(len(data)) != ref.ByteLength || hex.EncodeToString(resultDigest[:]) != ref.DigestSHA256 {
			return storage.ErrContentRejected
		}
		result = bytes.Clone(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
