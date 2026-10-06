package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"

	toolkit "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
	"github.com/jackc/pgx/v5"
)

type codeToolkitQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Lock the persisted call and live parent in native toolkit admission's transaction.
// Normal toolkit admission has no Code relation and retains its existing path.
func lockCodeToolkitParent(ctx context.Context, tx pgx.Tx, admission toolkit.Admission) error {
	p := admission.CodeParent
	if p == nil {
		return nil
	}
	if p.Validate() != nil || admission.Record.IdempotencyKey != "code-platform-"+p.EffectID || admission.Record.Job.ResourceProjectID != strconv.FormatInt(p.Admission.Job.ProjectID, 10) || admission.Record.Job.ActorID != strconv.FormatInt(p.Admission.Job.ActorID, 10) || admission.Binding.ToolkitVersion == "" {
		return domain.ErrUnauthorized
	}
	version := admission.Binding.ToolkitVersion
	if len(version) != 72 || version[:8] != "code-v1:" {
		return domain.ErrConflict
	}
	resource, err := json.Marshal(map[string]string{"kind": "toolkit", "id": strconv.FormatInt(admission.Binding.ToolkitID, 10), "revision": version[8:], "tool": admission.Binding.ToolName})
	if err != nil || sha256.Sum256(resource) != p.Intent.Resource {
		return domain.ErrConflict
	}
	var effect string
	err = tx.QueryRow(ctx, `SELECT call.effect_id
FROM elitea_runtime.code_platform_calls call
JOIN elitea_runtime.code_platform_jobs original USING(execution_id,activation_sha256)
JOIN elitea_runtime.execution_claims current ON current.execution_id=original.execution_id
JOIN elitea_runtime.execution_jobs parent ON parent.execution_id=current.execution_id AND parent.generation=current.generation
JOIN elitea_runtime.workload_sessions session ON session.workload_session_id=current.workload_session_id AND session.workload_identity=current.workload_identity AND session.producer_id=current.producer_id
WHERE call.effect_id=$1 AND call.sequence=$2 AND call.call_sha256=$3 AND call.arguments_sha256=$4 AND call.resource_sha256=$5
AND call.operation='toolkit_call' AND call.state='dispatching'
AND original.execution_id=$6 AND original.activation_sha256=$7 AND original.prepared_sha256=$8 AND original.policy_sha256=$9 AND original.retained_runtime_id=$10
AND original.project_id=$11 AND original.actor_id=$12 AND original.original_generation=$13
AND current.claim_id=$14 AND current.generation=$15 AND current.lease_epoch=$16 AND current.workload_identity=$17 AND current.fence_token=$18
AND current.released_at IS NULL AND current.lease_expires_at>clock_timestamp()
AND session.revoked_at IS NULL AND session.issued_at<=clock_timestamp() AND session.expires_at>clock_timestamp()
AND parent.desired_state='RUNNING' AND parent.state='RUNNING' AND parent.resource_project_id=original.project_id AND parent.actor_id=original.actor_id::text
AND parent.capability_id IN ('agent.execute.application.v1','agent.execute.adhoc.v1')
FOR UPDATE OF call FOR SHARE OF current,parent,session`, p.EffectID, int64(p.Intent.Sequence), p.Intent.Call[:], p.Intent.Arguments[:], p.Intent.Resource[:], p.Admission.Job.ExecutionID, p.Admission.Job.Activation[:], p.Admission.Job.PreparedRequest[:], p.Admission.Job.Policy[:], p.Admission.Job.RuntimeID, p.Admission.Job.ProjectID, p.Admission.Job.ActorID, int64(p.Admission.Job.OriginalGeneration), p.Admission.ClaimID, int64(p.Admission.Generation), int64(p.Admission.LeaseEpoch), p.Admission.WorkloadIdentity, p.Admission.FenceToken).Scan(&effect)
	if err != nil || effect != p.EffectID {
		return domain.ErrUnauthorized
	}
	return nil
}

type CodeToolkitChild = domain.ToolkitChild

// ReadCodeToolkitChild reads the exact originally admitted child under the current
// parent claim. Absence or an unsettled child never grants another submission.
func (r *CodePlatformCallsRepository) ReadCodeToolkitChild(ctx context.Context, a domain.Admission, record domain.Record) (CodeToolkitChild, bool, error) {
	if r == nil || ctx == nil || a.Validate() != nil || record.Intent.Operation != "toolkit_call" || record.EffectID != domain.EffectID(a.Job, record.Intent) {
		return CodeToolkitChild{}, false, domain.ErrUnauthorized
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return CodeToolkitChild{}, false, domain.ErrUnavailable
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err = lockCodeCallClaim(ctx, tx, a); err != nil {
		return CodeToolkitChild{}, false, err
	}
	if _, _, err = lockCodeCallJob(ctx, tx, a); err != nil {
		return CodeToolkitChild{}, false, err
	}
	saved, found, err := readCodeCall(ctx, tx, a.Job, record.Intent.Sequence)
	if err != nil {
		return CodeToolkitChild{}, false, err
	}
	if !found || !codeIntentEqual(saved.Intent, record.Intent) || saved.EffectID != record.EffectID {
		return CodeToolkitChild{}, false, domain.ErrConflict
	}
	child, found, err := readCodeToolkitChild(ctx, tx, a, saved)
	if err != nil {
		return CodeToolkitChild{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return CodeToolkitChild{}, false, domain.ErrUnavailable
	}
	return child, found, nil
}
func readCodeToolkitChild(ctx context.Context, query codeToolkitQuerier, a domain.Admission, record domain.Record) (CodeToolkitChild, bool, error) {
	var child CodeToolkitChild
	var arguments, payload []byte
	var kind *string
	err := query.QueryRow(ctx, `SELECT relation.child_execution_id,relation.toolkit_id,relation.toolkit_revision,relation.arguments_sha256,result.payload_type,result.payload_bytes
FROM elitea_runtime.code_platform_toolkit_children relation
JOIN elitea_runtime.execution_jobs child ON child.execution_id=relation.child_execution_id AND child.generation=relation.child_generation
LEFT JOIN LATERAL (SELECT payload_type,payload_bytes FROM elitea_runtime.output_inbox WHERE execution_id=child.execution_id AND generation=child.generation AND payload_type IN ('TOOLKIT_CALL_TOOL_RESULT','RUNTIME_FAILURE') ORDER BY sequence DESC LIMIT 1) result ON TRUE
WHERE relation.effect_id=$1 AND relation.child_generation=1 AND child.capability_id='toolkit.call_tool.v1'
AND child.resource_project_id=$2 AND child.actor_id=$3`, record.EffectID, a.Job.ProjectID, strconv.FormatInt(a.Job.ActorID, 10)).Scan(&child.ExecutionID, &child.ToolkitID, &child.Revision, &arguments, &kind, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return child, false, nil
	}
	if err != nil {
		return child, false, domain.ErrUnavailable
	}
	if len(arguments) != 32 || child.ToolkitID < 1 || len(child.ExecutionID) < 1 || len(child.ExecutionID) > 128 || len(child.Revision) != 64 || !bytes.Equal(arguments, record.Intent.Arguments[:]) {
		return child, false, domain.ErrConflict
	}
	copy(child.ArgumentsSHA256[:], arguments)
	if kind != nil {
		if (*kind != "TOOLKIT_CALL_TOOL_RESULT" && *kind != "RUNTIME_FAILURE") || len(payload) < 1 || len(payload) > 2*1024*1024 {
			return child, false, domain.ErrConflict
		}
		hash := sha256.Sum256(payload)
		child.OwnerReceipt = "code-toolkit:" + child.ExecutionID + ":" + hex.EncodeToString(hash[:])
		child.Settled = true
	}
	return child, true, nil
}

// CommitCodeToolkitReconciliation is an explicit original-child observation path.
// It does not grant dispatch, infer no-effect, or accept a browser-provided receipt.
func (r *CodePlatformCallsRepository) CommitCodeToolkitReconciliation(ctx context.Context, a domain.Admission, record domain.Record) error {
	if r == nil || ctx == nil || a.Validate() != nil || !record.Committed() || record.Intent.Operation != "toolkit_call" {
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
	_, total, err := lockCodeCallJob(ctx, tx, a)
	if err != nil {
		return err
	}
	saved, found, err := readCodeCall(ctx, tx, a.Job, record.Intent.Sequence)
	if err != nil {
		return err
	}
	if !found || !codeIntentEqual(saved.Intent, record.Intent) || saved.EffectID != record.EffectID {
		return domain.ErrConflict
	}
	if saved.Committed() {
		if saved.Response == record.Response && saved.ResponseReference == record.ResponseReference && saved.OwnerReceipt == record.OwnerReceipt {
			return tx.Commit(ctx)
		}
		return domain.ErrConflict
	}
	if saved.State != "dispatching" && saved.State != "uncertain" {
		return domain.ErrUnknown
	}
	child, found, err := readCodeToolkitChild(ctx, tx, a, saved)
	if err != nil {
		return err
	}
	if !found || !child.Settled || child.OwnerReceipt != record.OwnerReceipt {
		return domain.ErrUnknown
	}
	if total > a.Job.MaxTotalBytes || record.ResponseBytes > a.Job.MaxTotalBytes-total {
		return domain.ErrConflict
	}
	tag, err := tx.Exec(ctx, `UPDATE elitea_runtime.code_platform_calls SET state='committed',response_ref=$4,response_sha256=$5,response_bytes=$6,owner_receipt=$7,resolved_at=clock_timestamp()
WHERE execution_id=$1 AND activation_sha256=$2 AND sequence=$3 AND state IN ('dispatching','uncertain')`, a.Job.ExecutionID, a.Job.Activation[:], int64(record.Intent.Sequence), record.ResponseReference, record.Response[:], int64(record.ResponseBytes), record.OwnerReceipt)
	if err != nil || tag.RowsAffected() != 1 {
		return domain.ErrUnknown
	}
	_, err = tx.Exec(ctx, `UPDATE elitea_runtime.code_platform_jobs SET total_bytes=total_bytes+$3 WHERE execution_id=$1 AND activation_sha256=$2`, a.Job.ExecutionID, a.Job.Activation[:], int64(record.ResponseBytes))
	if err != nil {
		return domain.ErrUnavailable
	}
	return tx.Commit(ctx)
}

func verifyCodeToolkitReplay(ctx context.Context, query codeToolkitQuerier, admission toolkit.Admission, child string) error {
	p := admission.CodeParent
	if p == nil {
		return nil
	}
	if p.Validate() != nil {
		return domain.ErrUnauthorized
	}
	var actual string
	var arguments []byte
	var toolkitID int64
	var revision string
	err := query.QueryRow(ctx, `SELECT child_execution_id,toolkit_id,toolkit_revision,arguments_sha256 FROM elitea_runtime.code_platform_toolkit_children WHERE effect_id=$1`, p.EffectID).Scan(&actual, &toolkitID, &revision, &arguments)
	if err != nil || actual != child || toolkitID != admission.Binding.ToolkitID || admission.Binding.ToolkitVersion != "code-v1:"+revision || !bytes.Equal(arguments, p.Intent.Arguments[:]) {
		return domain.ErrConflict
	}
	return nil
}
func bindCodeToolkitChild(ctx context.Context, tx pgx.Tx, admission toolkit.Admission) error {
	p := admission.CodeParent
	if p == nil {
		return nil
	}
	revision := admission.Binding.ToolkitVersion
	if len(revision) != 72 || revision[:8] != "code-v1:" || admission.Record.Job.Generation != 1 {
		return domain.ErrConflict
	}
	decoded, err := hex.DecodeString(revision[8:])
	if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != revision[8:] {
		return domain.ErrConflict
	}
	// The binding is committed with the actual new child, never inferred from a name.
	var args [32]byte
	// The application supplies the exact arguments digest. The repository also checks
	// its immutable entry using the semantic role, rather than relying on entry order.
	found := false
	for _, entry := range admission.Record.InputBundle.Entries {
		if entry.SemanticRole == "toolkit.call_tool.arguments" {
			args = sha256.Sum256(entry.Content)
			found = true
			break
		}
	}
	if !found || args != p.Intent.Arguments {
		return domain.ErrConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO elitea_runtime.code_platform_toolkit_children(effect_id,child_execution_id,child_generation,toolkit_id,toolkit_revision,arguments_sha256)
VALUES($1,$2,1,$3,$4,$5)`, p.EffectID, admission.Record.Job.ID, admission.Binding.ToolkitID, revision[8:], args[:])
	return err
}

// Stop uses the execution owner's desired-state fence for exact linked children.
// The parent's authenticated cancellation transaction owns this call.
func cancelCodeToolkitChildren(ctx context.Context, tx sqlExecutor, parent string, generation int64) error {
	_, err := tx.Exec(ctx, `UPDATE elitea_runtime.execution_jobs child SET desired_state='CANCELLED'
FROM elitea_runtime.code_platform_toolkit_children relation
JOIN elitea_runtime.code_platform_calls call ON call.effect_id=relation.effect_id
JOIN elitea_runtime.code_platform_jobs original USING(execution_id,activation_sha256)
JOIN elitea_runtime.execution_jobs parent ON parent.execution_id=original.execution_id AND parent.generation=$2
WHERE original.execution_id=$1 AND parent.desired_state='CANCELLED'
AND parent.resource_project_id=original.project_id AND parent.actor_id=original.actor_id::text
AND child.execution_id=relation.child_execution_id AND child.generation=relation.child_generation
AND child.capability_id='toolkit.call_tool.v1' AND child.resource_project_id=original.project_id AND child.actor_id=original.actor_id::text
AND child.desired_state='RUNNING' AND child.state IN ('PENDING','DISPATCHED','CLAIMED','RUNNING','SETTLING')`, parent, generation)
	return err
}
