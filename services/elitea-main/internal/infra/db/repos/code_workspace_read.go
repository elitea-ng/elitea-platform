package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"

	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
)

// WithCodeWorkspaceRead consumes sealed Supervisor authority. It authenticates
// the actual original Worker claim without fabricating a Worker certificate.
// Callbacks perform only short metadata reads; all binary/object IO is outside.
func (r *CodeIntentRepository) WithCodeWorkspaceRead(ctx context.Context, authority storage.CodeWorkspaceReadAuthority, prepared []byte, apply func(context.Context, storage.CodeTransaction, storage.OriginalCodeVisit, storage.CodePreparedRequest) error) error {
	view, ok := authority.View()
	if r == nil || r.shared == nil || r.workspace == nil || !ok || apply == nil {
		return code.ErrRejected
	}
	parsed, err := storage.ParseCodePreparedRequest(prepared)
	if err != nil || parsed.Workspace == nil || parsed.PreparedSHA256 != view.PreparedSHA256 || parsed.Fingerprint != view.PreparedFingerprint {
		return code.ErrRejected
	}
	return r.shared.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		var token []byte
		err := tx.QueryRow(ctx, `SELECT c.fence_token FROM elitea_runtime.execution_claims c JOIN elitea_runtime.execution_jobs j USING(execution_id,generation)
   WHERE c.claim_id=$1 AND c.execution_id=$2 AND c.generation=$3 AND c.workload_identity=$4 AND c.claim_attempt=$5 AND c.lease_epoch=$6
   AND c.released_at IS NULL AND c.lease_expires_at>clock_timestamp() FOR UPDATE OF j,c`, view.ClaimID, view.ExecutionID, int64(view.Generation), view.WorkerIdentity, int64(view.ClaimAttempt), int64(view.LeaseEpoch)).Scan(&token)
		digest := sha256.Sum256(token)
		if err != nil || len(token) != 32 || hex.EncodeToString(digest[:]) != view.FenceSHA256 {
			return storage.ErrContentUnauthorized
		}
		claim := storage.ContentClaim{ExecutionID: view.ExecutionID, Generation: view.Generation, ClaimID: view.ClaimID, FenceToken: bytes.Clone(token)}
		access, err := r.lockAccessIdentity(ctx, tx, claim, "RUNNING", view.WorkerIdentity)
		if err != nil || access.tenant != view.TenantID || access.project != view.ProjectID || access.attempt != view.ClaimAttempt || access.epoch != view.LeaseEpoch || access.now.UnixMilli() >= view.ExpiresAtUnixMillis {
			return storage.ErrContentUnauthorized
		}
		var original storage.OriginalCodeVisit
		switch view.Mode {
		case storage.CodeWorkspaceExecuteMode:
			intent, err := r.readRegisteredCodeIntent(ctx, tx, claim, view.JobActivation, view.RequestDigest, "code_workspace", access)
			if err != nil || intent.Binding != view.IntentBinding || intent.PreparedFingerprint != view.PreparedFingerprint {
				return code.ErrRejected
			}
			original = intent.Original
		case storage.CodeWorkspaceCompileMode:
			if parsed.Language != "rust" || parsed.Broker != nil && parsed.PolicyRevision != "cargo-broker-execute-v1" {
				return code.ErrRejected
			}
			record, declaration, err := r.readVisit(ctx, tx, claim, access, view.OriginalVisit, "code_workspace")
			if err != nil || declaration.PlatformClient != (parsed.Broker != nil) {
				return code.ErrRejected
			}
			original = originalVisitView(access, claim, view.OriginalVisit, record, declaration)
		default:
			return code.ErrRejected
		}
		if _, err := storage.MatchFinalCodePrepared(original.Declaration, storage.CodePreparedMetadata{Language: original.Language, SourceSHA256: original.SourceSHA256, InputSHA256: original.InputSHA256, ImageDigest: original.ImageDigest, PolicyRevision: original.PolicyRevision, TimeoutSeconds: original.TimeoutSeconds}, prepared); err != nil {
			return err
		}
		if err = r.workspace.VerifyOriginalCodeWorkspace(ctx, codeIntentTransaction{tx}, claim, original, parsed); err != nil {
			return err
		}
		if err = apply(ctx, codeIntentTransaction{tx}, original, parsed); err != nil {
			return err
		}
		current, err := r.lockAccessIdentity(ctx, tx, claim, "RUNNING", view.WorkerIdentity)
		if err != nil || current.attempt != access.attempt || current.epoch != access.epoch || current.now.UnixMilli() >= view.ExpiresAtUnixMillis {
			return storage.ErrContentUnauthorized
		}
		return nil
	})
}

var _ storage.CodeWorkspaceReadConsumer = (*CodeIntentRepository)(nil)
