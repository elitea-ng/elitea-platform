package repos

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"math"
	"strconv"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	runtime "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CodePlatformCallsRepository owns exact Code call identities and effect observations.
// It does not execute an operation or read graph business state.
type CodePlatformCallsRepository struct{ pool *pgxpool.Pool }

func NewCodePlatformCallsRepository(pool *pgxpool.Pool) (*CodePlatformCallsRepository, error) {
	if pool == nil {
		return nil, domain.ErrUnavailable
	}
	return &CodePlatformCallsRepository{pool: pool}, nil
}

func lockCodeCallClaim(ctx context.Context, tx pgx.Tx, a domain.Admission) error {
	if a.Validate() != nil || a.Generation > math.MaxInt64 || a.LeaseEpoch > math.MaxInt64 || a.Job.OriginalGeneration > math.MaxInt64 {
		return domain.ErrUnauthorized
	}
	var claim string
	err := tx.QueryRow(ctx, `SELECT c.claim_id FROM elitea_runtime.execution_claims c
JOIN elitea_runtime.execution_jobs j USING(execution_id,generation)
JOIN elitea_runtime.workload_sessions s ON s.workload_session_id=c.workload_session_id AND s.workload_identity=c.workload_identity AND s.producer_id=c.producer_id
WHERE c.execution_id=$1 AND c.generation=$2 AND c.claim_id=$3 AND c.lease_epoch=$4 AND c.workload_identity=$5 AND c.fence_token=$6
AND c.released_at IS NULL AND c.lease_expires_at>clock_timestamp()
AND s.revoked_at IS NULL AND s.issued_at<=clock_timestamp() AND s.expires_at>clock_timestamp()
AND j.desired_state='RUNNING' AND j.state='RUNNING' AND j.resource_project_id=$7 AND j.actor_id=$8
AND j.capability_id IN ('agent.execute.application.v1','agent.execute.adhoc.v1')
FOR SHARE OF c,j,s`, a.Job.ExecutionID, int64(a.Generation), a.ClaimID, int64(a.LeaseEpoch), a.WorkloadIdentity, a.FenceToken, a.Job.ProjectID, strconv.FormatInt(a.Job.ActorID, 10)).Scan(&claim)
	if err != nil || claim != a.ClaimID {
		return domain.ErrUnauthorized
	}
	return nil
}
func lockCodeCallJob(ctx context.Context, tx storage.CodeTransaction, a domain.Admission) (uint64, uint64, error) {
	j := a.Job
	_, err := tx.Exec(ctx, `INSERT INTO elitea_runtime.code_platform_jobs(execution_id,original_generation,activation_sha256,prepared_sha256,policy_sha256,retained_runtime_id,project_id,actor_id,max_calls,max_total_bytes,tenant_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING`, j.ExecutionID, int64(j.OriginalGeneration), j.Activation[:], j.PreparedRequest[:], j.Policy[:], j.RuntimeID, j.ProjectID, j.ActorID, int64(j.MaxCalls), int64(j.MaxTotalBytes), j.TenantID)
	if err != nil {
		return 0, 0, domain.ErrUnavailable
	}
	var original, project, actor, maxCalls, maxBytes, last, total int64
	var prepared, policy []byte
	var runtime, tenant string
	err = tx.QueryRow(ctx, `SELECT original_generation,prepared_sha256,policy_sha256,retained_runtime_id,project_id,actor_id,max_calls,max_total_bytes,last_sequence,total_bytes,tenant_id FROM elitea_runtime.code_platform_jobs
WHERE execution_id=$1 AND activation_sha256=$2 FOR UPDATE`, j.ExecutionID, j.Activation[:]).Scan(&original, &prepared, &policy, &runtime, &project, &actor, &maxCalls, &maxBytes, &last, &total, &tenant)
	if err != nil {
		return 0, 0, domain.ErrUnavailable
	}
	if tenant != j.TenantID || uint64(original) != j.OriginalGeneration || !bytes.Equal(prepared, j.PreparedRequest[:]) || !bytes.Equal(policy, j.Policy[:]) || runtime != j.RuntimeID || project != j.ProjectID || actor != j.ActorID || uint64(maxCalls) != j.MaxCalls || uint64(maxBytes) != j.MaxTotalBytes || last < 0 || total < 0 || uint64(last) > j.MaxCalls || uint64(total) > j.MaxTotalBytes {
		return 0, 0, domain.ErrConflict
	}
	return uint64(last), uint64(total), nil
}

func codeIntentEqual(a, b domain.Intent) bool {
	return a.Sequence == b.Sequence && a.Operation == b.Operation && a.Call == b.Call && a.Resource == b.Resource && a.Arguments == b.Arguments && a.Payload == b.Payload && a.Frame == b.Frame && a.FrameBytes == b.FrameBytes
}

// Lookup reads an original sequence under a live current claim. It never remints intent.
func (r *CodePlatformCallsRepository) Lookup(ctx context.Context, a domain.Admission, sequence uint64) (domain.Record, bool, error) {
	if r == nil || ctx == nil || a.Validate() != nil || sequence < 1 || sequence > a.Job.MaxCalls {
		return domain.Record{}, false, domain.ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.Record{}, false, domain.ErrUnavailable
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err = lockCodeCallClaim(ctx, tx, a); err != nil {
		return domain.Record{}, false, err
	}
	if _, _, err = lockCodeCallJob(ctx, tx, a); err != nil {
		return domain.Record{}, false, err
	}
	record, found, err := readCodeCall(ctx, tx, a.Job, sequence)
	if err != nil {
		return domain.Record{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Record{}, false, domain.ErrUnavailable
	}
	return record, found, nil
}
func readCodeCall(ctx context.Context, query codeToolkitQuerier, job domain.Job, sequence uint64) (domain.Record, bool, error) {
	var r domain.Record
	var seq, size int64
	var call, resource, args, payload, frame, response []byte
	var ref *string
	var responseBytes *int64
	var owner *string
	err := query.QueryRow(ctx, `SELECT sequence,operation,effect_id,call_sha256,resource_sha256,arguments_sha256,payload_sha256,frame_sha256,frame_bytes,intent_ref,state,response_ref,response_sha256,response_bytes,owner_receipt FROM elitea_runtime.code_platform_calls
WHERE execution_id=$1 AND activation_sha256=$2 AND sequence=$3`, job.ExecutionID, job.Activation[:], int64(sequence)).Scan(&seq, &r.Intent.Operation, &r.EffectID, &call, &resource, &args, &payload, &frame, &size, &r.Intent.EncryptedReference, &r.State, &ref, &response, &responseBytes, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, domain.ErrUnavailable
	}
	if seq < 1 || size < 8 || len(call) != 32 || len(resource) != 32 || len(args) != 32 || len(payload) != 32 || len(frame) != 32 {
		return r, false, domain.ErrConflict
	}
	r.Intent.Sequence = uint64(seq)
	r.Intent.FrameBytes = uint64(size)
	copy(r.Intent.Call[:], call)
	copy(r.Intent.Resource[:], resource)
	copy(r.Intent.Arguments[:], args)
	copy(r.Intent.Payload[:], payload)
	copy(r.Intent.Frame[:], frame)
	if ref != nil {
		r.ResponseReference = *ref
	}
	if len(response) != 0 && len(response) != 32 {
		return r, false, domain.ErrConflict
	}
	copy(r.Response[:], response)
	if responseBytes != nil {
		if *responseBytes < 1 {
			return r, false, domain.ErrConflict
		}
		r.ResponseBytes = uint64(*responseBytes)
	}
	if owner != nil {
		r.OwnerReceipt = *owner
	}
	if r.EffectID != domain.EffectID(job, r.Intent) {
		return r, false, domain.ErrConflict
	}
	if r.State == "committed" && !r.Committed() {
		return r, false, domain.ErrConflict
	}
	return r, true, nil
}

// Begin commits immutable encrypted intent before granting a dispatch transition.
// A duplicate sequence can only read its exact original record.
func (r *CodePlatformCallsRepository) Begin(ctx context.Context, a domain.Admission, intent domain.Intent) (domain.Record, error) {
	if r == nil || ctx == nil || a.Validate() != nil || intent.Sequence < 1 || intent.Sequence > a.Job.MaxCalls || intent.FrameBytes < 8 || intent.FrameBytes > 8+domain.MaxRequestHeader+domain.MaxChunk {
		return domain.Record{}, domain.ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.Record{}, domain.ErrUnavailable
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err = lockCodeCallClaim(ctx, tx, a); err != nil {
		return domain.Record{}, err
	}
	last, total, err := lockCodeCallJob(ctx, tx, a)
	if err != nil {
		return domain.Record{}, err
	}
	existing, found, err := readCodeCall(ctx, tx, a.Job, intent.Sequence)
	if err != nil {
		return domain.Record{}, err
	}
	if found {
		if !codeIntentEqual(existing.Intent, intent) {
			return domain.Record{}, domain.ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return domain.Record{}, domain.ErrUnavailable
		}
		return existing, nil
	}
	if intent.Sequence != last+1 || intent.FrameBytes > a.Job.MaxTotalBytes-total {
		return domain.Record{}, domain.ErrConflict
	}
	var inflight bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM elitea_runtime.code_platform_calls WHERE execution_id=$1 AND activation_sha256=$2 AND state<>'committed')`, a.Job.ExecutionID, a.Job.Activation[:]).Scan(&inflight); err != nil {
		return domain.Record{}, domain.ErrUnavailable
	}
	if inflight {
		return domain.Record{}, domain.ErrUnknown
	}
	effect := domain.EffectID(a.Job, intent)
	_, err = tx.Exec(ctx, `INSERT INTO elitea_runtime.code_platform_calls(execution_id,activation_sha256,sequence,operation,effect_id,call_sha256,resource_sha256,arguments_sha256,payload_sha256,frame_sha256,frame_bytes,intent_ref,state)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'prepared')`, a.Job.ExecutionID, a.Job.Activation[:], int64(intent.Sequence), intent.Operation, effect, intent.Call[:], intent.Resource[:], intent.Arguments[:], intent.Payload[:], intent.Frame[:], int64(intent.FrameBytes), intent.EncryptedReference)
	if err != nil {
		return domain.Record{}, domain.ErrUnavailable
	}
	_, err = tx.Exec(ctx, `UPDATE elitea_runtime.code_platform_jobs SET last_sequence=$3,total_bytes=total_bytes+$4 WHERE execution_id=$1 AND activation_sha256=$2`, a.Job.ExecutionID, a.Job.Activation[:], int64(intent.Sequence), int64(intent.FrameBytes))
	if err != nil {
		return domain.Record{}, domain.ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Record{}, domain.ErrUnavailable
	}
	return domain.Record{Intent: intent, EffectID: effect, State: "prepared"}, nil
}

// Dispatch returns true only for the first persisted prepared-to-dispatching transition.
// Dispatching or uncertain replay never returns true.
func (r *CodePlatformCallsRepository) Dispatch(ctx context.Context, a domain.Admission, record domain.Record) (bool, error) {
	if r == nil || ctx == nil || a.Validate() != nil || record.EffectID != domain.EffectID(a.Job, record.Intent) {
		return false, domain.ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, domain.ErrUnavailable
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err = lockCodeCallClaim(ctx, tx, a); err != nil {
		return false, err
	}
	if _, _, err = lockCodeCallJob(ctx, tx, a); err != nil {
		return false, err
	}
	saved, found, err := readCodeCall(ctx, tx, a.Job, record.Intent.Sequence)
	if err != nil {
		return false, err
	}
	if !found || !codeIntentEqual(saved.Intent, record.Intent) || saved.EffectID != record.EffectID {
		return false, domain.ErrConflict
	}
	if saved.State != "prepared" {
		return false, domain.ErrUnknown
	}
	tag, err := tx.Exec(ctx, `UPDATE elitea_runtime.code_platform_calls SET state='dispatching',dispatch_claim_id=$4,dispatch_generation=$5,dispatch_lease_epoch=$6 WHERE execution_id=$1 AND activation_sha256=$2 AND sequence=$3 AND state='prepared'`, a.Job.ExecutionID, a.Job.Activation[:], int64(record.Intent.Sequence), a.ClaimID, int64(a.Generation), int64(a.LeaseEpoch))
	if err != nil || tag.RowsAffected() != 1 {
		return false, domain.ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return false, domain.ErrUnavailable
	}
	return true, nil
}

// Commit requires the same dispatch owner and a live current claim.
// Reconciliation under another owner uses a separate verified owner receipt path.
func (r *CodePlatformCallsRepository) Commit(ctx context.Context, a domain.Admission, record domain.Record) error {
	if r == nil || ctx == nil || !record.Committed() || a.Validate() != nil || record.EffectID != domain.EffectID(a.Job, record.Intent) {
		return domain.ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.ErrUnavailable
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err = lockCodeCallClaim(ctx, tx, a); err != nil {
		return err
	}
	_, total, err := lockCodeCallJob(ctx, tx, a)
	if err != nil {
		return err
	}
	saved, found, err := readCodeCall(ctx, tx, a.Job, record.Intent.Sequence)
	if err != nil {
		return err
	}
	if !found || !codeIntentEqual(saved.Intent, record.Intent) {
		return domain.ErrConflict
	}
	if saved.Committed() {
		if saved.Response == record.Response && saved.ResponseReference == record.ResponseReference && saved.OwnerReceipt == record.OwnerReceipt {
			return tx.Commit(ctx)
		}
		return domain.ErrConflict
	}
	if record.ResponseBytes > a.Job.MaxTotalBytes-total {
		return domain.ErrConflict
	}
	tag, err := tx.Exec(ctx, `UPDATE elitea_runtime.code_platform_calls SET state='committed',response_ref=$7,response_sha256=$8,response_bytes=$9,owner_receipt=$10,resolved_at=clock_timestamp()
WHERE execution_id=$1 AND activation_sha256=$2 AND sequence=$3 AND state='dispatching' AND dispatch_claim_id=$4 AND dispatch_generation=$5 AND dispatch_lease_epoch=$6`, a.Job.ExecutionID, a.Job.Activation[:], int64(record.Intent.Sequence), a.ClaimID, int64(a.Generation), int64(a.LeaseEpoch), record.ResponseReference, record.Response[:], int64(record.ResponseBytes), record.OwnerReceipt)
	if err != nil || tag.RowsAffected() != 1 {
		return domain.ErrUnknown
	}
	_, err = tx.Exec(ctx, `UPDATE elitea_runtime.code_platform_jobs SET total_bytes=total_bytes+$3 WHERE execution_id=$1 AND activation_sha256=$2`, a.Job.ExecutionID, a.Job.Activation[:], int64(record.ResponseBytes))
	if err != nil {
		return domain.ErrUnavailable
	}
	return tx.Commit(ctx)
}

// VerifyCommittedCodeCall is the sole signing admission. It never trusts caller state.
func (r *CodePlatformCallsRepository) VerifyCommittedCodeCall(ctx context.Context, a domain.Admission, record domain.Record) error {
	if r == nil || ctx == nil || a.Validate() != nil || !record.Committed() {
		return domain.ErrUnauthorized
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.ErrUnavailable
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err = lockCodeCallClaim(ctx, tx, a); err != nil {
		return err
	}
	if _, _, err = lockCodeCallJob(ctx, tx, a); err != nil {
		return err
	}
	saved, found, err := readCodeCall(ctx, tx, a.Job, record.Intent.Sequence)
	if err != nil {
		return err
	}
	if !found || !saved.Committed() || !codeIntentEqual(saved.Intent, record.Intent) || saved.EffectID != record.EffectID || saved.Response != record.Response || saved.ResponseReference != record.ResponseReference || saved.ResponseBytes != record.ResponseBytes || saved.OwnerReceipt != record.OwnerReceipt {
		return domain.ErrConflict
	}
	return tx.Commit(ctx)
}

// RegisterCodePlatformJob is called only from the live original-intent callback
// after an authenticated owner attests the actual retained runtime. It does not
// create a process or mint a grant. The caller rechecks its fence before commit.
func (r *CodePlatformCallsRepository) RegisterCodePlatformJob(ctx context.Context, tx storage.CodeTransaction, a domain.Admission) error {
	if r == nil || ctx == nil || tx == nil || a.Validate() != nil {
		return domain.ErrUnauthorized
	}
	_, _, err := lockCodeCallJob(ctx, tx, a)
	return err
}

// readCodePlatformOriginal uses the one original intent owner. It does not
// duplicate logical visit/attempt identities in a second registration store.
func readCodePlatformOriginal(ctx context.Context, tx storage.CodeTransaction, execution string, generation uint64, dispatch string) (storage.CodeCallOriginalIntent, error) {
	var result storage.CodeCallOriginalIntent
	var binding, broker, visitRaw, selector []byte
	var rawSHA, policy, brokerRuntime, jobRuntime, bundle, tenant string
	var activation, prepared, jobPolicy []byte
	var project, actor, maxCalls, maxBytes int64
	if ctx == nil || tx == nil || execution == "" || len(execution) > 256 || generation < 1 || generation > math.MaxInt64 || !code.NonzeroDigest(dispatch) {
		return result, domain.ErrConflict
	}
	err := tx.QueryRow(ctx, `SELECT i.visit_id,v.visit_digest,i.binding_json,i.binding_digest,b.prepared_sha256,b.prepared_fingerprint,b.policy_sha256,b.broker_json,b.retained_runtime_id,j.retained_runtime_id,j.activation_sha256,j.prepared_sha256,j.policy_sha256,b.dependency_bundle_sha256,i.compiled_selector_json,v.record_json,j.tenant_id,j.project_id,j.actor_id,j.max_calls,j.max_total_bytes
FROM elitea_runtime.original_code_intents i
JOIN elitea_runtime.original_code_visits v USING(execution_id,generation,visit_id)
JOIN elitea_runtime.original_code_broker_bindings b ON b.execution_id=i.execution_id AND b.original_generation=i.generation AND b.visit_id=i.visit_id
JOIN elitea_runtime.code_platform_jobs j ON j.execution_id=i.execution_id AND j.original_generation=i.generation AND encode(j.activation_sha256,'hex')=i.dispatch_activation
WHERE i.execution_id=$1 AND i.generation=$2 AND i.dispatch_activation=$3
FOR SHARE OF i,v,b,j`, execution, int64(generation), dispatch).Scan(&result.OriginalVisit.VisitID, &result.OriginalVisit.DigestSHA256, &binding, &result.BindingSHA256, &rawSHA, &result.PreparedFingerprint, &policy, &broker, &brokerRuntime, &jobRuntime, &activation, &prepared, &jobPolicy, &bundle, &selector, &visitRaw, &tenant, &project, &actor, &maxCalls, &maxBytes)
	if err != nil {
		return result, domain.ErrConflict
	}
	result.OriginalVisit.Revision = 1
	if result.OriginalVisit.Validate() != nil || code.Digest(binding) != result.BindingSHA256 || code.Decode(binding, &result.Binding, 8192) != nil || result.Binding.Validate() != nil || code.Decode(broker, &result.Broker, 512) != nil {
		return storage.CodeCallOriginalIntent{}, domain.ErrConflict
	}
	canonical, err := code.Canonical(result.Binding)
	if err != nil || !bytes.Equal(canonical, binding) {
		return storage.CodeCallOriginalIntent{}, domain.ErrConflict
	}
	canonical, err = code.Canonical(result.Broker)
	if err != nil || !bytes.Equal(canonical, broker) {
		return storage.CodeCallOriginalIntent{}, domain.ErrConflict
	}
	if brokerRuntime == "" || brokerRuntime != jobRuntime || result.Binding.ExecutionID != execution || result.Binding.OriginalGeneration != generation || result.Binding.DispatchActivation != dispatch || result.Binding.PreparedSHA256 != rawSHA || result.Broker.Revision != 1 || result.Broker.PolicySHA256 != policy || result.Broker.MaxCalls < 1 || result.Broker.MaxCalls > 4096 || result.Broker.MaxTotalBytes < 1 || result.Broker.MaxTotalBytes > 67108864 || !code.NonzeroDigest(rawSHA) || !code.NonzeroDigest(result.PreparedFingerprint) || !code.NonzeroDigest(policy) || len(activation) != 32 || len(prepared) != 32 || len(jobPolicy) != 32 || hex.EncodeToString(activation) != dispatch || hex.EncodeToString(prepared) != result.PreparedFingerprint || hex.EncodeToString(jobPolicy) != policy {
		return storage.CodeCallOriginalIntent{}, domain.ErrConflict
	}
	var original originalCodeVisitRecord
	if code.Digest(visitRaw) != result.OriginalVisit.DigestSHA256 || code.Decode(visitRaw, &original, 16384) != nil || !original.PlatformClient || original.ExecutionID != execution || original.Generation != generation || original.Tenant != tenant || original.Project != project || original.Actor != strconv.FormatInt(actor, 10) || original.Activation != result.Binding.ActivationID || original.Node != result.Binding.NodeID || original.Thread != result.Binding.GraphThread || original.Step != result.Binding.Step || original.Attempt != result.Binding.Attempt || original.NodeDigest != result.Binding.NodeDigest || original.PreWorkspace.SourceSHA256 != result.Binding.SourceSHA256 || original.PreWorkspace.InputSHA256 != result.Binding.InputSHA256 || maxCalls != int64(result.Broker.MaxCalls) || maxBytes != int64(result.Broker.MaxTotalBytes) {
		return storage.CodeCallOriginalIntent{}, domain.ErrConflict
	}
	ref, canonicalVisit, err := codeVisitRef(original)
	if err != nil || ref != result.OriginalVisit || !bytes.Equal(canonicalVisit, visitRaw) {
		return storage.CodeCallOriginalIntent{}, domain.ErrConflict
	}
	var saved []*string
	if code.Decode(selector, &saved, 131072) != nil || (len(saved) != 2 && len(saved) != 3) || (saved[0] == nil) != (saved[1] == nil) {
		return storage.CodeCallOriginalIntent{}, domain.ErrConflict
	}
	canonical, err = code.Canonical(saved)
	if err != nil || !bytes.Equal(canonical, selector) {
		return storage.CodeCallOriginalIntent{}, domain.ErrConflict
	}
	compiled := saved[0] != nil
	if !compiled && len(saved) != 2 {
		return storage.CodeCallOriginalIntent{}, domain.ErrConflict
	}
	if compiled {
		raw, err := code.DecodeBase64(*saved[0], runtime.SnapshotDescriptorLimit)
		if err != nil {
			return storage.CodeCallOriginalIntent{}, domain.ErrConflict
		}
		selected, err := runtime.ParseRustSnapshotBinding(raw)
		if err != nil || !code.NonzeroDigest(*saved[1]) {
			return storage.CodeCallOriginalIntent{}, domain.ErrConflict
		}
		selectedBundle := ""
		if len(saved) == 3 {
			if saved[2] == nil {
				return storage.CodeCallOriginalIntent{}, domain.ErrConflict
			}
			selectedBundle = *saved[2]
		}
		key, err := selected.Key()
		if err != nil {
			return storage.CodeCallOriginalIntent{}, domain.ErrConflict
		}
		result.CompiledExecute = &storage.OriginalCompiledCodeExecute{Binding: selected, DescriptorSHA256: *saved[1], SnapshotKeySHA256: key, DependencyBundleSHA256: selectedBundle}
	}
	registered := storage.RegisteredCodeIntent{Binding: result.Binding, PreparedFingerprint: result.PreparedFingerprint, Compiled: compiled, CompiledExecute: result.CompiledExecute, Original: storage.OriginalCodeVisit{TenantID: original.Tenant, ResourceProjectID: original.Project, SourceSHA256: original.PreWorkspace.SourceSHA256, ImageDigest: original.PreWorkspace.ImageDigest, PolicyRevision: original.PreWorkspace.PolicyRevision}}
	brokerBinding := storage.RegisteredCodeBrokerBinding{PreparedSHA256: rawSHA, PreparedFingerprint: result.PreparedFingerprint, DependencyBundleSHA256: bundle, Broker: result.Broker}
	if storage.VerifyCodeBrokerRequestIdentity(registered, brokerBinding, result.CompiledExecute) != nil {
		return storage.CodeCallOriginalIntent{}, domain.ErrConflict
	}
	result.BrokerSHA256 = code.Digest(broker)
	return result, nil
}

// ResolveOriginalCodeCallEffect refuses missing or foreign effects. It returns
// only immutable identity, never a partial call response as whole-Code output.
func (r *CodePlatformCallsRepository) ResolveOriginalCodeCallEffect(ctx context.Context, tx storage.CodeTransaction, execution string, generation uint64, effect string) (storage.CodeCallOriginalIntent, error) {
	var result storage.CodeCallOriginalIntent
	if r == nil || ctx == nil || tx == nil || execution == "" || len(execution) > 256 || generation < 1 || generation > math.MaxInt64 || !code.NonzeroDigest(effect) {
		return result, domain.ErrConflict
	}
	var dispatch string
	var sequence int64
	var state string
	err := tx.QueryRow(ctx, `SELECT encode(c.activation_sha256,'hex'),c.sequence,c.state FROM elitea_runtime.code_platform_calls c JOIN elitea_runtime.code_platform_jobs j USING(execution_id,activation_sha256)
WHERE c.execution_id=$1 AND j.original_generation=$2 AND c.effect_id=$3 FOR SHARE OF c,j`, execution, int64(generation), effect).Scan(&dispatch, &sequence, &state)
	if err != nil || sequence < 1 || sequence > 4096 {
		return result, domain.ErrConflict
	}
	result, err = readCodePlatformOriginal(ctx, tx, execution, generation, dispatch)
	if err != nil {
		return storage.CodeCallOriginalIntent{}, err
	}
	// Recompute the owning effect ID from the original persisted call and job.
	var runtime string
	var policy, prepared []byte
	err = tx.QueryRow(ctx, `SELECT retained_runtime_id,prepared_sha256,policy_sha256 FROM elitea_runtime.code_platform_jobs WHERE execution_id=$1 AND activation_sha256=decode($2,'hex') FOR SHARE`, execution, dispatch).Scan(&runtime, &prepared, &policy)
	if err != nil {
		return storage.CodeCallOriginalIntent{}, domain.ErrConflict
	}
	var job domain.Job
	job.ExecutionID = execution
	job.OriginalGeneration = generation
	job.RuntimeID = runtime
	copy(job.Activation[:], mustCodeDigest(dispatch))
	copy(job.PreparedRequest[:], prepared)
	copy(job.Policy[:], policy)
	record, found, err := readCodeCall(ctx, tx, job, uint64(sequence))
	if err != nil || !found || record.EffectID != effect || record.State != state || uint64(sequence) > uint64(result.Broker.MaxCalls) {
		return storage.CodeCallOriginalIntent{}, domain.ErrConflict
	}
	result.CallEffectID = effect
	result.Sequence = uint64(sequence)
	result.State = state
	return result, nil
}
func mustCodeDigest(value string) []byte { raw, _ := hex.DecodeString(value); return raw }

// Missing registration is an error. Even a prepared observed call blocks a
// no-effect proof; only the Supervisor can prove whole-Code output or sealing.
func (r *CodePlatformCallsRepository) ReadOriginalCodeBrokerEffects(ctx context.Context, tx storage.CodeTransaction, execution string, generation uint64, dispatch, rawSHA, fingerprint string) (storage.CodeBrokerEffectFacts, error) {
	var facts storage.CodeBrokerEffectFacts
	if r == nil || !code.NonzeroDigest(fingerprint) || !code.NonzeroDigest(rawSHA) {
		return facts, domain.ErrConflict
	}
	original, err := readCodePlatformOriginal(ctx, tx, execution, generation, dispatch)
	if err != nil || original.PreparedFingerprint != fingerprint || original.Binding.PreparedSHA256 != rawSHA {
		return facts, domain.ErrConflict
	}
	err = tx.QueryRow(ctx, `SELECT TRUE,
EXISTS(SELECT 1 FROM elitea_runtime.code_platform_calls c WHERE c.execution_id=j.execution_id AND c.activation_sha256=j.activation_sha256),
EXISTS(SELECT 1 FROM elitea_runtime.code_platform_calls c WHERE c.execution_id=j.execution_id AND c.activation_sha256=j.activation_sha256 AND c.state<>'prepared'),
EXISTS(SELECT 1 FROM elitea_runtime.code_platform_calls c WHERE c.execution_id=j.execution_id AND c.activation_sha256=j.activation_sha256 AND c.state IN ('dispatching','uncertain')),
EXISTS(SELECT 1 FROM elitea_runtime.code_platform_calls c JOIN elitea_runtime.code_platform_toolkit_children relation USING(effect_id) JOIN elitea_runtime.execution_jobs child ON child.execution_id=relation.child_execution_id AND child.generation=relation.child_generation WHERE c.execution_id=j.execution_id AND c.activation_sha256=j.activation_sha256 AND child.state IN ('PENDING','DISPATCHED','CLAIMED','RUNNING','SETTLING'))
FROM elitea_runtime.code_platform_jobs j WHERE j.execution_id=$1 AND j.original_generation=$2 AND encode(j.activation_sha256,'hex')=$3 AND encode(j.prepared_sha256,'hex')=$4`, execution, int64(generation), dispatch, fingerprint).Scan(&facts.Registered, &facts.HasObservedCalls, &facts.HasDispatchedEffects, &facts.HasUncertainEffects, &facts.HasPendingToolkitChildren)
	if err != nil {
		return storage.CodeBrokerEffectFacts{}, domain.ErrConflict
	}
	return facts, nil
}

var _ storage.CodeCallEffectResolver = (*CodePlatformCallsRepository)(nil)
var _ storage.CodeBrokerEffectReader = (*CodePlatformCallsRepository)(nil)
var _ storage.CodePlatformJobRegistrar = (*CodePlatformCallsRepository)(nil)
