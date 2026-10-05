package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"

	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	runtime "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
)

// ReadRegisteredCodeIntent consumes the exact once-frozen final record under
// current writer/actor rights. It cannot establish another job or runtime.
func (r *CodeIntentRepository) ReadRegisteredCodeIntent(ctx context.Context, tx sqlExecutor, claim storage.ContentClaim, dispatch, request, purpose string) (storage.RegisteredCodeIntent, error) {
	access, err := r.lockAccess(ctx, tx, claim, "RUNNING")
	if err != nil {
		return storage.RegisteredCodeIntent{}, err
	}
	return r.readRegisteredCodeIntent(ctx, tx, claim, dispatch, request, purpose, access)
}
func (r *CodeIntentRepository) readRegisteredCodeIntent(ctx context.Context, tx sqlExecutor, claim storage.ContentClaim, dispatch, request, purpose string, access codeAccess) (storage.RegisteredCodeIntent, error) {
	if !originalCodePurpose(purpose) || !code.NonzeroDigest(dispatch) || !code.NonzeroDigest(request) {
		return storage.RegisteredCodeIntent{}, code.ErrRejected
	}
	var err error
	var visitID, visitDigest, bindingDigest string
	var wire, selector []byte
	err = tx.QueryRow(ctx, `SELECT i.visit_id,v.visit_digest,i.binding_json,i.binding_digest,i.compiled_selector_json
 FROM elitea_runtime.original_code_intents i JOIN elitea_runtime.original_code_visits v USING(execution_id,generation,visit_id)
 WHERE i.execution_id=$1 AND i.generation=$2 AND i.dispatch_activation=$3 FOR UPDATE OF i,v`, claim.ExecutionID, int64(claim.Generation), dispatch).Scan(&visitID, &visitDigest, &wire, &bindingDigest, &selector)
	if err != nil || code.Digest(wire) != bindingDigest {
		return storage.RegisteredCodeIntent{}, code.ErrRejected
	}
	var binding code.Binding
	if code.Decode(wire, &binding, 8192) != nil || binding.Validate() != nil || binding.ExecutionID != claim.ExecutionID || binding.OriginalGeneration != claim.Generation || binding.DispatchActivation != dispatch || binding.RequestDigest != request {
		return storage.RegisteredCodeIntent{}, code.ErrRejected
	}
	canonical, err := code.Canonical(binding)
	if err != nil || !bytes.Equal(canonical, wire) {
		return storage.RegisteredCodeIntent{}, code.ErrRejected
	}
	ref := code.OriginalVisitRef{VisitID: visitID, Revision: 1, DigestSHA256: visitDigest}
	original, declaration, err := r.readVisit(ctx, tx, claim, access, ref, purpose)
	if err != nil {
		return storage.RegisteredCodeIntent{}, err
	}
	if binding.ActivationID != original.Activation || binding.NodeID != original.Node || binding.GraphThread != original.Thread || binding.Step != original.Step || binding.Attempt != original.Attempt || binding.NodeDigest != original.NodeDigest || binding.Language != original.PreWorkspace.Language || binding.SourceSHA256 != original.PreWorkspace.SourceSHA256 || binding.InputSHA256 != original.PreWorkspace.InputSHA256 || matchOriginalCodeDispatch(declaration, original.Activation, original.Attempt, binding.DispatchActivation) != nil {
		return storage.RegisteredCodeIntent{}, code.ErrRejected
	}
	fingerprint, compiledExecute, err := r.resolveRegisteredCodeSelector(ctx, access, original, binding, selector)
	if err != nil {
		return storage.RegisteredCodeIntent{}, err
	}
	compiled := compiledExecute != nil
	issued, expires, err := grantEnd(access)
	if err != nil {
		return storage.RegisteredCodeIntent{}, err
	}
	fence := sha256.Sum256(claim.FenceToken)
	return storage.RegisteredCodeIntent{Original: originalVisitView(access, claim, ref, original, declaration), Access: storage.CodeIntentAccess{ClaimID: claim.ClaimID, ClaimAttempt: access.attempt, LeaseEpoch: access.epoch, FenceSHA256: hex.EncodeToString(fence[:]), WorkloadIdentity: access.peer, IssuedAtUnixMillis: issued, ExpiresAtUnixMillis: expires}, Binding: binding, BindingSHA256: bindingDigest, PreparedFingerprint: fingerprint, Compiled: compiled, CompiledExecute: compiledExecute}, nil
}

func originalCodePurpose(purpose string) bool {
	return purpose == "code_debug" || purpose == "code_workspace" || purpose == "platform_broker" || purpose == "code_recovery"
}

// The callback is restricted to short database admission/metadata operations.
// Toolkit, object, binary and Supervisor requests run after this transaction ends.
func (r *CodeIntentRepository) WithRegisteredCodeIntent(ctx context.Context, claim storage.ContentClaim, dispatch, request, purpose string, apply func(context.Context, storage.CodeTransaction, storage.RegisteredCodeIntent) error) error {
	if r == nil || r.shared == nil || apply == nil || !originalCodePurpose(purpose) {
		return code.ErrRejected
	}
	return r.shared.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		intent, err := r.ReadRegisteredCodeIntent(ctx, tx, claim, dispatch, request, purpose)
		if err != nil {
			return err
		}
		if err = apply(ctx, codeIntentTransaction{tx}, intent); err != nil {
			return err
		}
		current, err := r.lockAccess(ctx, tx, claim, "RUNNING")
		if err != nil || current.attempt != intent.Access.ClaimAttempt || current.epoch != intent.Access.LeaseEpoch {
			return storage.ErrContentUnauthorized
		}
		return nil
	})
}

var _ storage.RegisteredCodeIntentConsumer = (*CodeIntentRepository)(nil)

// WithRegisteredCodePlatformIntent accepts only the step route's prepared
// fingerprint selector. The whole request digest is read from the immutable
// registered intent, then the same original profile/index/visit checks run.
func (r *CodeIntentRepository) WithRegisteredCodePlatformIntent(ctx context.Context, claim storage.ContentClaim, dispatch, fingerprint string, apply func(context.Context, storage.CodeTransaction, storage.RegisteredCodeIntent) error) error {
	if r == nil || r.shared == nil || apply == nil || !code.NonzeroDigest(dispatch) || !code.NonzeroDigest(fingerprint) {
		return code.ErrRejected
	}
	return r.shared.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		current, err := r.lockAccess(ctx, tx, claim, "RUNNING")
		if err != nil {
			return err
		}
		var raw []byte
		if tx.QueryRow(ctx, `SELECT binding_json FROM elitea_runtime.original_code_intents WHERE execution_id=$1 AND generation=$2 AND dispatch_activation=$3 FOR UPDATE`, claim.ExecutionID, int64(claim.Generation), dispatch).Scan(&raw) != nil {
			return code.ErrRejected
		}
		var binding code.Binding
		if code.Decode(raw, &binding, 8192) != nil || binding.Validate() != nil {
			return code.ErrRejected
		}
		intent, err := r.readRegisteredCodeIntent(ctx, tx, claim, dispatch, binding.RequestDigest, "platform_broker", current)
		if err != nil || intent.PreparedFingerprint != fingerprint || !intent.Original.Declaration.PlatformClient {
			return code.ErrRejected
		}
		if err = apply(ctx, codeIntentTransaction{tx}, intent); err != nil {
			return err
		}
		latest, err := r.lockAccess(ctx, tx, claim, "RUNNING")
		if err != nil || latest.attempt != intent.Access.ClaimAttempt || latest.epoch != intent.Access.LeaseEpoch {
			return storage.ErrContentUnauthorized
		}
		return nil
	})
}

var _ storage.CodePlatformIntentConsumer = (*CodeIntentRepository)(nil)

// The immutable selector is validated again by the original profile and index.
// Both ordinary platform reads and recovery broker facts use this same relation.
func (r *CodeIntentRepository) resolveRegisteredCodeSelector(ctx context.Context, access codeAccess, original originalCodeVisitRecord, binding code.Binding, selector []byte) (string, *storage.OriginalCompiledCodeExecute, error) {
	var selected []*string
	if code.Decode(selector, &selected, 131072) != nil || (len(selected) != 2 && len(selected) != 3) || (selected[0] == nil) != (selected[1] == nil) {
		return "", nil, code.ErrRejected
	}
	canonical, err := code.Canonical(selected)
	if err != nil || !bytes.Equal(canonical, selector) {
		return "", nil, code.ErrRejected
	}
	fingerprint := binding.RequestDigest
	compiled := selected[0] != nil
	var compiledExecute *storage.OriginalCompiledCodeExecute
	if !compiled && len(selected) != 2 {
		return "", nil, code.ErrRejected
	}
	if compiled {
		raw, err := code.DecodeBase64(*selected[0], runtime.SnapshotDescriptorLimit)
		if err != nil {
			return "", nil, code.ErrRejected
		}
		preparedBinding, err := runtime.ParseRustSnapshotBinding(raw)
		if err != nil || !code.NonzeroDigest(*selected[1]) || preparedBinding.TenantID != original.Tenant || int64(preparedBinding.ProjectID) != original.Project {
			return "", nil, code.ErrRejected
		}
		// Older two-field compiled selectors are accepted only with a no-bundle
		// configured profile; they may not invent a dependency measurement.
		bundle := ""
		if len(selected) == 3 {
			if selected[2] == nil {
				return "", nil, code.ErrRejected
			}
			bundle = *selected[2]
		}
		if preparedBinding.SourceSHA256 != binding.SourceSHA256 || preparedBinding.ExecutionImageDigest != original.PreWorkspace.ImageDigest || preparedBinding.PolicyRevision != original.PreWorkspace.PolicyRevision {
			return "", nil, code.ErrRejected
		}
		relation, exact, err := r.resolveOriginalCompiledCodeExecute(ctx, access, original, binding.DispatchActivation, preparedBinding, *selected[1], bundle)
		if err != nil || exact != binding.RequestDigest {
			return "", nil, code.ErrRejected
		}
		fingerprint = preparedBinding.BasePreparedRequestSHA256
		compiledExecute = relation
	}
	return fingerprint, compiledExecute, nil
}
