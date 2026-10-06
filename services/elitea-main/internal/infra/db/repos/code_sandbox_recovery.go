package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	recovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
)

type codeRecoveryRecord struct {
	visit               originalCodeVisitRecord
	binding             code.Binding
	bindingSHA          string
	preparedFingerprint string
}
type codeRecoveryFence struct {
	claim, peer          string
	attempt, epoch       uint64
	fence                []byte
	now, lease, deadline time.Time
}

const codeRecoveryFenceSQL = `SELECT c.claim_id,c.workload_identity,c.claim_attempt,c.lease_epoch,c.fence_token,clock_timestamp(),c.lease_expires_at,command.deadline
 FROM elitea_runtime.execution_claims c
 JOIN elitea_runtime.execution_jobs j USING(execution_id,generation)
 JOIN elitea_runtime.workload_sessions ws ON ws.workload_session_id=c.workload_session_id AND ws.workload_identity=c.workload_identity AND ws.producer_id=c.producer_id
 JOIN elitea_runtime.command_outbox command USING(execution_id,generation)
 JOIN centry.project project ON project.id=j.resource_project_id
 WHERE c.execution_id=$1 AND c.generation=$2 AND c.released_at IS NULL AND c.lease_expires_at>clock_timestamp()
 AND c.recovery_mode='NODE_RECOVERY' AND j.state='RUNNING' AND j.desired_state IN ('SUSPENDED','RUNNING') AND project.suspended=FALSE
 AND j.tenant_id=$3 AND j.resource_project_id=$4 AND j.projection_project_id=$5 AND j.actor_id=$6 AND j.input_bundle_id IS NOT NULL
 AND ws.issued_at<=clock_timestamp() AND ws.expires_at>clock_timestamp() AND ws.revoked_at IS NULL
 AND command.retired_at IS NULL AND command.authority_granted_at IS NOT NULL AND command.deadline>clock_timestamp()
 AND NOT EXISTS(SELECT 1 FROM elitea_runtime.output_inbox terminal WHERE terminal.execution_id=j.execution_id AND terminal.generation=j.generation)
 ORDER BY c.lease_epoch DESC LIMIT 1 FOR UPDATE OF j,c,ws,command`

func lockCodeRecoveryFence(ctx context.Context, tx sqlExecutor, v originalCodeVisitRecord) (codeRecoveryFence, error) {
	var f codeRecoveryFence
	err := tx.QueryRow(ctx, codeRecoveryFenceSQL, v.ExecutionID, int64(v.Generation), v.Tenant, v.Project, v.Projection, v.Actor).Scan(&f.claim, &f.peer, &f.attempt, &f.epoch, &f.fence, &f.now, &f.lease, &f.deadline)
	if err != nil || !recovery.ValidExecutionID(f.claim) || !code.Identity(f.peer) || f.attempt == 0 || f.epoch == 0 || len(f.fence) != 32 {
		return f, code.ErrRejected
	}
	return f, nil
}
func (r *CodeIntentRepository) recoveryRecord(ctx context.Context, tx sqlExecutor, execution string, generation uint64, journal recovery.Receipt, action string) (codeRecoveryRecord, code.Visit, error) {
	if r == nil || r.signer == nil || r.owner == nil || !recovery.ValidExecutionID(execution) || generation == 0 || journal.Validate() != nil || action != "reconcile" && action != "resume_result" {
		return codeRecoveryRecord{}, code.Visit{}, code.ErrRejected
	}
	effect := journal.ReplaySafety.EffectID
	if journal.ReplaySafety.Kind == "completed_external_effect" {
		effect = journal.ReplaySafety.ReceiptID
	}
	if !code.NonzeroDigest(effect) {
		return codeRecoveryRecord{}, code.Visit{}, code.ErrRejected
	}
	var raw, bindingRaw, selector []byte
	var bindingSHA string
	err := tx.QueryRow(ctx, `SELECT v.record_json,i.binding_json,i.binding_digest,i.compiled_selector_json FROM elitea_runtime.original_code_intents i
 JOIN elitea_runtime.original_code_visits v USING(execution_id,generation,visit_id)
 WHERE i.execution_id=$1 AND i.generation=$2 AND i.dispatch_activation=$3 FOR SHARE OF i,v`, execution, int64(generation), effect).Scan(&raw, &bindingRaw, &bindingSHA, &selector)
	if err != nil {
		return codeRecoveryRecord{}, code.Visit{}, code.ErrRejected
	}
	var record codeRecoveryRecord
	if code.Decode(raw, &record.visit, 16384) != nil || code.Decode(bindingRaw, &record.binding, 8192) != nil || record.binding.Validate() != nil || code.Digest(bindingRaw) != bindingSHA {
		return record, code.Visit{}, code.ErrRejected
	}
	ref, canonical, err := codeVisitRef(record.visit)
	_ = ref
	if err != nil || !bytes.Equal(raw, canonical) || record.visit.ExecutionID != execution || record.visit.Generation != generation || record.visit.Activation != journal.ActivationID || record.visit.Node != journal.NodeID || record.visit.Thread != journal.GraphThread || record.visit.Step != journal.Step || record.visit.Attempt != journal.Attempt || record.binding.ExecutionID != execution || record.binding.OriginalGeneration != generation || record.binding.NodeDigest != record.visit.NodeDigest || record.binding.DispatchActivation != effect || record.binding.ActivationID != record.visit.Activation || record.binding.NodeID != record.visit.Node || record.binding.GraphThread != record.visit.Thread || record.binding.Step != record.visit.Step || record.binding.Attempt != record.visit.Attempt {
		return record, code.Visit{}, code.ErrRejected
	}
	canonical, err = code.Canonical(record.binding)
	if err != nil || !bytes.Equal(canonical, bindingRaw) {
		return record, code.Visit{}, code.ErrRejected
	}
	record.bindingSHA = bindingSHA
	fingerprint, _, err := r.resolveRegisteredCodeSelector(ctx, codeAccess{tenant: record.visit.Tenant, project: record.visit.Project}, record.visit, record.binding, selector)
	if err != nil {
		return record, code.Visit{}, err
	}
	record.preparedFingerprint = fingerprint
	latest, err := lockLatestNodeRecoveryVisit(ctx, tx, execution, generation)
	if err != nil || latest.activation != journal.ActivationID || latest.revision != journal.JournalRevision {
		return record, code.Visit{}, code.ErrRejected
	}
	receiptRaw, err := recovery.CanonicalReceipt(mustRecoveryJSON(journal))
	if err != nil {
		return record, code.Visit{}, code.ErrRejected
	}
	visit := code.Visit{ActivationID: journal.ActivationID, NodeID: journal.NodeID, GraphThread: journal.GraphThread, Step: journal.Step, Attempt: journal.Attempt, ExpectedRevision: journal.JournalRevision, ReceiptSHA256: code.Digest(receiptRaw)}
	// Bind the exact frozen input digest too; a child intent never rebounds to a root intent.
	var inputDigest []byte
	err = tx.QueryRow(ctx, `SELECT e.content_digest FROM elitea_runtime.agent_execution_jobs a JOIN elitea_runtime.input_bundle_entries e ON e.input_bundle_id=a.input_bundle_id AND e.entry_id=a.request_entry_id WHERE a.execution_id=$1 AND a.generation=$2 AND e.semantic_role='agent.execution_request'`, execution, int64(generation)).Scan(&inputDigest)
	if err != nil || !bytes.Equal(inputDigest, codeIntentDigestBytes(record.visit.InputSHA256)) {
		return record, visit, code.ErrRejected
	}
	return record, visit, nil
}
func mustRecoveryJSON(r recovery.Receipt) []byte { raw, _ := json.Marshal(r); return raw }
func codeOwnerProof(execution string, generation uint64, journal recovery.Receipt, record codeRecoveryRecord, visit code.Visit, wire []byte) (recovery.OwnerProof, error) {
	receipt, err := code.DecodeReceipt(wire, record.binding, visit)
	if err != nil {
		return recovery.OwnerProof{}, err
	}
	digest := code.Digest(wire)
	ref := &recovery.ResultReference{ContentID: record.binding.DispatchActivation, ImmutableVersion: digest, DigestSHA256: digest, ByteLength: uint64(len(wire)), MediaType: "application/json", RequiredGrantAudience: code.ResultAudience}
	proof := recovery.OwnerProof{Schema: "elitea.pipeline.node-recovery-owner-proof.v1", Kind: receipt.Kind, ExecutionID: execution, Generation: generation, ActivationID: journal.ActivationID, Attempt: journal.Attempt, ExpectedRevision: journal.JournalRevision, EffectID: record.binding.DispatchActivation, OwnerReceiptSHA256: digest}
	if receipt.Kind == "committed_result" {
		proof.ResultRef = ref
	} else {
		proof.OwnerReceiptRef = ref
	}
	return proof, nil
}
func (r *CodeIntentRepository) VerifyNodeRecoveryEffect(ctx context.Context, tx sqlExecutor, execution string, generation uint64, journal recovery.Receipt, action string) (json.RawMessage, error) {
	record, visit, err := r.recoveryRecord(ctx, tx, execution, generation, journal, action)
	if err != nil {
		return nil, err
	}
	f, err := lockCodeRecoveryFence(ctx, tx, record.visit)
	if err != nil {
		return nil, err
	}
	var wire []byte
	var savedDigest string
	err = tx.QueryRow(ctx, `SELECT receipt_wire,receipt_sha256 FROM elitea_runtime.original_code_owner_receipts WHERE execution_id=$1 AND generation=$2 AND dispatch_activation=$3 AND node_receipt_sha256=$4`, execution, int64(generation), record.binding.DispatchActivation, visit.ReceiptSHA256).Scan(&wire, &savedDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		operation := "read"
		if action == "reconcile" && journal.ReplaySafety.Kind == "unknown_external_effect" {
			operation = "seal_no_effect"
		}
		issued, expires, err := grantEnd(codeAccess{now: f.now, lease: f.lease, deadline: f.deadline})
		if err != nil {
			return nil, err
		}
		fence := sha256.Sum256(f.fence)
		claims := code.GrantClaims{Schema: "elitea.sandbox.node-code-recovery-grant.v1", TenantID: record.visit.Tenant, ProjectID: record.visit.Project, ExecutionID: execution, OriginalGeneration: generation, ClaimID: f.claim, ClaimAttempt: f.attempt, LeaseEpoch: f.epoch, FenceSHA256: hex.EncodeToString(fence[:]), ActivationID: visit.ActivationID, NodeID: visit.NodeID, GraphThread: visit.GraphThread, Step: visit.Step, Attempt: visit.Attempt, ExpectedRevision: visit.ExpectedRevision, ReceiptSHA256: visit.ReceiptSHA256, DispatchActivation: record.binding.DispatchActivation, JobKey: record.binding.JobKey, RequestDigest: record.binding.RequestDigest, BindingSHA256: record.bindingSHA, SupervisorAudience: record.binding.SupervisorAudience, RequesterWorkloadIdentity: r.signer.RequesterIdentity(), Operation: operation, IssuedAtMillis: issued, ExpiresAtMillis: expires}
		grant, err := r.signer.SignRecovery(claims)
		if err != nil {
			return nil, err
		}
		wire, err = r.owner.Read(ctx, claims, grant)
		if err != nil {
			return nil, err
		}
		after, err := lockCodeRecoveryFence(ctx, tx, record.visit)
		if err != nil || after.claim != f.claim || after.attempt != f.attempt || after.epoch != f.epoch || !bytes.Equal(after.fence, f.fence) {
			return nil, code.ErrRejected
		}
		admitted, err := codeOwnerProof(execution, generation, journal, record, visit, wire)
		if err != nil {
			return nil, err
		}
		if admitted.Kind == "verified_no_effect" {
			if err = r.requireNoObservedCodeBrokerEffects(ctx, tx, record); err != nil {
				return nil, err
			}
		}
		tag, err := tx.Exec(ctx, `INSERT INTO elitea_runtime.original_code_owner_receipts(execution_id,generation,dispatch_activation,node_receipt_sha256,receipt_wire,receipt_sha256,admitted_claim_id) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, execution, int64(generation), record.binding.DispatchActivation, visit.ReceiptSHA256, wire, code.Digest(wire), f.claim)
		if err != nil || tag.RowsAffected() != 1 {
			return nil, code.ErrRejected
		}
	} else if err != nil || code.Digest(wire) != savedDigest {
		return nil, code.ErrRejected
	}
	proof, err := codeOwnerProof(execution, generation, journal, record, visit, wire)
	if err != nil || action == "resume_result" && proof.Kind != "committed_result" {
		return nil, code.ErrRejected
	}
	if proof.Kind == "verified_no_effect" {
		if err = r.requireNoObservedCodeBrokerEffects(ctx, tx, record); err != nil {
			return nil, err
		}
	}
	return json.Marshal(proof)
}
func (r *CodeIntentRepository) ReadNodeRecoveryHTTPResult(ctx context.Context, tx sqlExecutor, execution string, generation uint64, journal recovery.Receipt, authorized json.RawMessage) ([]byte, error) {
	proof, err := recovery.DecodeOwnerProof(authorized, execution, generation, journal, "reconcile")
	if err != nil {
		return nil, code.ErrRejected
	}
	ref := proof.ResultRef
	if proof.Kind == "verified_no_effect" {
		ref = proof.OwnerReceiptRef
	}
	if ref == nil || ref.RequiredGrantAudience != code.ResultAudience {
		return nil, code.ErrRejected
	}
	record, visit, err := r.recoveryRecord(ctx, tx, execution, generation, journal, "reconcile")
	if err != nil {
		return nil, err
	}
	if _, err = lockCodeRecoveryFence(ctx, tx, record.visit); err != nil {
		return nil, err
	}
	var wire []byte
	var digest string
	err = tx.QueryRow(ctx, `SELECT receipt_wire,receipt_sha256 FROM elitea_runtime.original_code_owner_receipts WHERE execution_id=$1 AND generation=$2 AND dispatch_activation=$3 AND node_receipt_sha256=$4`, execution, int64(generation), record.binding.DispatchActivation, visit.ReceiptSHA256).Scan(&wire, &digest)
	if err != nil || digest != code.Digest(wire) {
		return nil, code.ErrRejected
	}
	current, err := codeOwnerProof(execution, generation, journal, record, visit, wire)
	if err != nil {
		return nil, err
	}
	if current.Kind == "verified_no_effect" {
		if err = r.requireNoObservedCodeBrokerEffects(ctx, tx, record); err != nil {
			return nil, err
		}
	}
	expected, _ := json.Marshal(current)
	if !bytes.Equal(expected, authorized) {
		return nil, code.ErrRejected
	}
	return bytes.Clone(wire), nil
}

// RecoveryEffectOwners chooses the durable registered owner once. Owner failure never falls through.
type RecoveryEffectOwners struct {
	Code *CodeIntentRepository
	HTTP NodeRecoveryEffectProofProvider
}

func (o *RecoveryEffectOwners) isCode(ctx context.Context, tx sqlExecutor, execution string, generation uint64, journal recovery.Receipt) (bool, error) {
	effect := journal.ReplaySafety.EffectID
	if journal.ReplaySafety.Kind == "completed_external_effect" {
		effect = journal.ReplaySafety.ReceiptID
	}
	var found bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM elitea_runtime.original_code_intents WHERE execution_id=$1 AND generation=$2 AND dispatch_activation=$3)`, execution, int64(generation), effect).Scan(&found)
	return found, err
}
func (o *RecoveryEffectOwners) VerifyNodeRecoveryEffect(ctx context.Context, tx sqlExecutor, execution string, generation uint64, journal recovery.Receipt, action string) (json.RawMessage, error) {
	if o == nil {
		return nil, code.ErrRejected
	}
	found, err := o.isCode(ctx, tx, execution, generation, journal)
	if err != nil {
		return nil, err
	}
	if found {
		if o.Code == nil {
			return nil, code.ErrRejected
		}
		return o.Code.VerifyNodeRecoveryEffect(ctx, tx, execution, generation, journal, action)
	}
	if o.HTTP == nil {
		return nil, code.ErrRejected
	}
	return o.HTTP.VerifyNodeRecoveryEffect(ctx, tx, execution, generation, journal, action)
}
func (o *RecoveryEffectOwners) ReadNodeRecoveryHTTPResult(ctx context.Context, tx sqlExecutor, execution string, generation uint64, journal recovery.Receipt, authorized json.RawMessage) ([]byte, error) {
	if o == nil {
		return nil, code.ErrRejected
	}
	var audience struct {
		ResultRef *recovery.ResultReference `json:"result_ref"`
		OwnerRef  *recovery.ResultReference `json:"owner_receipt_ref"`
	}
	if json.Unmarshal(authorized, &audience) != nil {
		return nil, code.ErrRejected
	}
	ref := audience.ResultRef
	if ref == nil {
		ref = audience.OwnerRef
	}
	if ref == nil {
		return nil, code.ErrRejected
	}
	if ref.RequiredGrantAudience == code.ResultAudience {
		if o.Code == nil {
			return nil, code.ErrRejected
		}
		return o.Code.ReadNodeRecoveryHTTPResult(ctx, tx, execution, generation, journal, authorized)
	}
	if ref.RequiredGrantAudience != recovery.HTTPResultAudience || o.HTTP == nil {
		return nil, code.ErrRejected
	}
	source, ok := o.HTTP.(NodeRecoveryResultSource)
	if !ok {
		return nil, code.ErrRejected
	}
	return source.ReadNodeRecoveryHTTPResult(ctx, tx, execution, generation, journal, authorized)
}

var _ NodeRecoveryEffectProofProvider = (*CodeIntentRepository)(nil)
var _ NodeRecoveryResultSource = (*RecoveryEffectOwners)(nil)

func (r *CodeIntentRepository) ValidateNodeRecoveryFailureRoute(ctx context.Context, tx sqlExecutor, claim storage.ContentClaim, journal recovery.Receipt, route *storage.NodeRecoveryFailureRoute) error {
	record, _, err := r.recoveryRecord(ctx, tx, claim.ExecutionID, claim.Generation, journal, "reconcile")
	if err != nil {
		return err
	}
	access, err := r.lockAccess(ctx, tx, claim, "SUSPENDED")
	if err != nil {
		return err
	}
	instructions, err := r.originalInstructions(ctx, tx, claim, access, record.visit, "code_recovery")
	if err != nil {
		return err
	}
	handler, err := storage.MatchCodeFailureRoute(instructions, record.visit.YAML, record.visit.Node, route)
	if err != nil {
		return err
	}
	if !bytes.Equal(bytes.TrimSpace(record.visit.Scope), []byte("null")) {
		v := record.visit
		v.Node = handler
		_, err = r.originalInstructions(ctx, tx, claim, access, v, "code_recovery")
		if err != nil {
			return err
		}
	}
	return nil
}
func (o *RecoveryEffectOwners) ValidateNodeRecoveryFailureRoute(ctx context.Context, tx sqlExecutor, claim storage.ContentClaim, journal recovery.Receipt, route *storage.NodeRecoveryFailureRoute) error {
	if o == nil || o.Code == nil {
		return code.ErrRejected
	}
	found, err := o.isCode(ctx, tx, claim.ExecutionID, claim.Generation, journal)
	if err != nil || !found {
		return code.ErrRejected
	}
	return o.Code.ValidateNodeRecoveryFailureRoute(ctx, tx, claim, journal, route)
}

// A Supervisor no-dispatch tombstone remains mandatory. Broker facts add an
// independent refusal: any observed call or an unknown/missing registration
// prevents no-effect reconciliation for a platform-enabled Code declaration.
func (r *CodeIntentRepository) requireNoObservedCodeBrokerEffects(ctx context.Context, tx sqlExecutor, record codeRecoveryRecord) error {
	if !record.visit.PlatformClient {
		return nil
	}
	reader, ok := r.broker.(storage.OriginalCodeBrokerEffectFactsReader)
	if !ok || reader == nil {
		return code.ErrRejected
	}
	facts, err := reader.ReadOriginalCodeBrokerEffects(ctx, codeIntentTransaction{tx}, record.binding.ExecutionID, record.binding.OriginalGeneration, record.binding.DispatchActivation, record.binding.PreparedSHA256, record.preparedFingerprint)
	if err != nil || !facts.Registered || facts.HasObservedCalls || facts.HasDispatchedEffects || facts.HasUncertainEffects || facts.HasPendingToolkitChildren {
		return code.ErrRejected
	}
	return nil
}
