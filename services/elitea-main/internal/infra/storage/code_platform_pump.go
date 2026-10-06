package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
)

// CodePlatformOwner is implemented by the fixed authenticated Main owner client.
// No method accepts a Worker-selected runtime ID, path, endpoint or executable.
type CodePlatformOwner interface {
	ReadRetainedRuntime(context.Context, code.PlatformGrantClaims, code.SignedGrant) (code.RetainedRuntime, error)
	ReadPendingPlatformCall(context.Context, code.PlatformGrantClaims, code.SignedGrant) (code.RetainedRuntime, []byte, error)
	PublishCommittedPlatformReply(context.Context, code.PlatformGrantClaims, code.SignedGrant, []byte) (code.RetainedRuntime, error)
}
type CodePlatformOwnerSigner interface {
	SignPlatform(code.PlatformGrantClaims) (code.SignedGrant, error)
}
type CodePlatformCallService interface {
	Call(context.Context, domain.Admission, []byte) ([]byte, error)
}

// The factory binds the concrete native readers to the current server claim.
type CodePlatformCallFactory interface {
	ForCodePlatformCall(ContentClaim, domain.Admission) (CodePlatformCallService, error)
}
type CodePlatformPump struct {
	intents   CodePlatformIntentConsumer
	owner     CodePlatformOwner
	signer    CodePlatformOwnerSigner
	calls     CodePlatformCallFactory
	jobs      CodePlatformJobRegistrar
	requester string
}

func NewCodePlatformPump(intents CodePlatformIntentConsumer, owner CodePlatformOwner, signer CodePlatformOwnerSigner, calls CodePlatformCallFactory, jobs CodePlatformJobRegistrar, configuredMainIdentity string) (*CodePlatformPump, error) {
	if intents == nil || owner == nil || signer == nil || calls == nil || jobs == nil || !code.Identity(configuredMainIdentity) {
		return nil, domain.ErrUnavailable
	}
	return &CodePlatformPump{intents: intents, owner: owner, signer: signer, calls: calls, jobs: jobs, requester: configuredMainIdentity}, nil
}

// PumpResult contains no secret reply, mailbox metadata or replacement runtime selector.
type CodePlatformPumpResult struct {
	State    string
	EffectID string
}
type codePlatformSnapshot struct {
	intent RegisteredCodeIntent
	broker RegisteredCodeBrokerBinding
	claims code.PlatformGrantClaims
}

func (p *CodePlatformPump) snapshot(ctx context.Context, claim ContentClaim, dispatch, request, operation string, sequence *uint64, requestSHA, replySHA *string, expected *codePlatformSnapshot, runtime *code.RetainedRuntime) (codePlatformSnapshot, error) {
	var result codePlatformSnapshot
	err := p.intents.WithRegisteredCodePlatformIntent(ctx, claim, dispatch, request, func(ctx context.Context, tx CodeTransaction, intent RegisteredCodeIntent) error {
		broker, err := ReadRegisteredCodeBroker(ctx, tx, intent)
		if err != nil {
			return err
		}
		result = codePlatformSnapshot{intent: intent, broker: broker}
		result.claims = codePlatformClaims(intent, broker, p.requester, operation, sequence, requestSHA, replySHA)
		if result.claims.Validate() != nil {
			return code.ErrRejected
		}
		if expected != nil && !sameCodePlatformSnapshot(*expected, result) {
			return code.ErrRejected
		}
		if runtime != nil {
			if !runtime.Matches(result.claims) {
				return code.ErrRejected
			}
			// Runtime kind/ID are immutable. Epoch is a fresh owner observation, not
			// permission to create another Code process after lease or Main loss.
			tag, err := tx.Exec(ctx, `UPDATE elitea_runtime.original_code_broker_bindings SET retained_runtime_id=$4,retained_runtime_kind=$5,last_owner_epoch=$6
WHERE execution_id=$1 AND original_generation=$2 AND visit_id=$3
AND (retained_runtime_id IS NULL OR (retained_runtime_id=$4 AND retained_runtime_kind=$5))
AND (last_owner_epoch IS NULL OR last_owner_epoch<=$6)`, intent.Original.ExecutionID, int64(intent.Original.OriginalGeneration), intent.Original.Reference.VisitID, runtime.RuntimeID, runtime.Kind, int64(runtime.OwnerEpoch))
			if err != nil || tag.RowsAffected() != 1 {
				return code.ErrRejected
			}
			admission, err := codePlatformAdmission(claim, result, *runtime)
			if err != nil {
				return err
			}
			if err = p.jobs.RegisterCodePlatformJob(ctx, tx, admission); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}
func codePlatformClaims(intent RegisteredCodeIntent, broker RegisteredCodeBrokerBinding, requester, operation string, sequence *uint64, requestSHA, replySHA *string) code.PlatformGrantClaims {
	b := intent.Binding
	a := intent.Access
	o := intent.Original
	claims := code.PlatformGrantClaims{Schema: "elitea.sandbox.code-platform-owner-grant.v1", Purpose: "platform_broker_runtime", TenantID: o.TenantID, ProjectID: o.ResourceProjectID, ExecutionID: b.ExecutionID, OriginalGeneration: b.OriginalGeneration, ClaimID: a.ClaimID, ClaimAttempt: a.ClaimAttempt, LeaseEpoch: a.LeaseEpoch, FenceSHA256: a.FenceSHA256, ActivationID: b.ActivationID, Attempt: b.Attempt, DispatchActivation: b.DispatchActivation, JobKey: b.JobKey, RequestDigest: b.RequestDigest, BindingSHA256: intent.BindingSHA256, PreparedSHA256: b.PreparedSHA256, PreparedFingerprint: broker.PreparedFingerprint, PolicySHA256: broker.Broker.PolicySHA256, MaxCalls: uint32(broker.Broker.MaxCalls), MaxTotalBytes: uint64(broker.Broker.MaxTotalBytes), SupervisorAudience: b.SupervisorAudience, RequesterWorkloadIdentity: requester, Operation: operation, Sequence: sequence, PlatformRequestSHA256: requestSHA, CommittedReplySHA256: replySHA, IssuedAtMillis: a.IssuedAtUnixMillis, ExpiresAtMillis: a.ExpiresAtUnixMillis}
	if selected := intent.CompiledExecute; selected != nil {
		claims.CompiledExecute = &code.PlatformCompiledExecute{Binding: selected.Binding, SnapshotKeySHA256: selected.SnapshotKeySHA256, SelectedDescriptorSHA256: selected.DescriptorSHA256}
	}
	return claims
}
func sameCodePlatformSnapshot(a, b codePlatformSnapshot) bool {
	// The server renews issued/expiry timestamps. Authority and immutable input
	// must remain exact across every short transaction and external owner call.
	return a.intent.BindingSHA256 == b.intent.BindingSHA256 && a.intent.Binding == b.intent.Binding && a.intent.Original.Reference == b.intent.Original.Reference && a.intent.Original.TenantID == b.intent.Original.TenantID && a.intent.Original.ResourceProjectID == b.intent.Original.ResourceProjectID && a.intent.Original.ActorID == b.intent.Original.ActorID && a.intent.Access.ClaimID == b.intent.Access.ClaimID && a.intent.Access.ClaimAttempt == b.intent.Access.ClaimAttempt && a.intent.Access.LeaseEpoch == b.intent.Access.LeaseEpoch && a.intent.Access.FenceSHA256 == b.intent.Access.FenceSHA256 && a.intent.Access.WorkloadIdentity == b.intent.Access.WorkloadIdentity && a.broker == b.broker && sameCodeBrokerCompiledExecute(a.intent.CompiledExecute, b.intent.CompiledExecute)
}
func sameCodePlatformRuntime(a, b code.RetainedRuntime) bool {
	if !code.SamePlatformCompiledExecute(a.CompiledExecute, b.CompiledExecute) {
		return false
	}
	a.CompiledExecute = nil
	b.CompiledExecute = nil
	return a == b
}
func codePlatformAdmission(claim ContentClaim, snapshot codePlatformSnapshot, runtime code.RetainedRuntime) (domain.Admission, error) {
	var activation, prepared, policy [32]byte
	for _, field := range []struct {
		text   string
		target *[32]byte
	}{{snapshot.intent.Binding.DispatchActivation, &activation}, {snapshot.broker.PreparedFingerprint, &prepared}, {snapshot.broker.Broker.PolicySHA256, &policy}} {
		raw, err := hex.DecodeString(field.text)
		if err != nil || len(raw) != 32 {
			return domain.Admission{}, code.ErrRejected
		}
		copy(field.target[:], raw)
	}
	o := snapshot.intent.Original
	a := snapshot.intent.Access
	result := domain.Admission{Job: domain.Job{TenantID: o.TenantID, ExecutionID: o.ExecutionID, OriginalGeneration: o.OriginalGeneration, Activation: activation, PreparedRequest: prepared, Policy: policy, RuntimeID: runtime.RuntimeID, ProjectID: o.ResourceProjectID, ActorID: o.ActorID, MaxCalls: uint64(snapshot.broker.Broker.MaxCalls), MaxTotalBytes: uint64(snapshot.broker.Broker.MaxTotalBytes)}, ClaimID: a.ClaimID, Generation: claim.Generation, LeaseEpoch: a.LeaseEpoch, WorkloadIdentity: a.WorkloadIdentity, FenceToken: append([]byte(nil), claim.FenceToken...)}
	if claim.PeerCertificate == nil || claim.ExecutionID != o.ExecutionID || claim.ClaimID != a.ClaimID || hex.EncodeToString(sha256Sum(claim.FenceToken)) != a.FenceSHA256 || result.Validate() != nil {
		return domain.Admission{}, domain.ErrUnauthorized
	}
	return result, nil
}
func sha256Sum(raw []byte) []byte { value := sha256.Sum256(raw); return value[:] }

// Step observes at most one bounded pending frame from the retained original Code.
// External IO never runs inside an original-intent transaction. Unknown effects
// retain their original journal and process; this method never submits Code.
func (p *CodePlatformPump) Step(ctx context.Context, claim ContentClaim, dispatch, request string) (CodePlatformPumpResult, error) {
	if p == nil || ctx == nil || claim.PeerCertificate == nil || !code.NonzeroDigest(dispatch) || !code.NonzeroDigest(request) {
		return CodePlatformPumpResult{}, domain.ErrUnauthorized
	}
	first, err := p.snapshot(ctx, claim, dispatch, request, "read_retained_runtime", nil, nil, nil, nil, nil)
	if err != nil {
		return CodePlatformPumpResult{}, err
	}
	grant, err := p.signer.SignPlatform(first.claims)
	if err != nil {
		return CodePlatformPumpResult{}, err
	}
	runtime, err := p.owner.ReadRetainedRuntime(ctx, first.claims, grant)
	if errors.Is(err, ErrCodePlatformNotReady) || errors.Is(err, ErrCodePlatformCompleted) || errors.Is(err, ErrCodePlatformCompleting) {
		if _, err = p.snapshot(ctx, claim, dispatch, request, "read_retained_runtime", nil, nil, nil, &first, nil); err != nil {
			return CodePlatformPumpResult{}, err
		}
		return CodePlatformPumpResult{State: "idle"}, nil
	}
	if err != nil || !runtime.Matches(first.claims) {
		return CodePlatformPumpResult{}, code.ErrRejected
	}
	pending, err := p.snapshot(ctx, claim, dispatch, request, "read_pending_platform_call", nil, nil, nil, &first, &runtime)
	if err != nil {
		return CodePlatformPumpResult{}, err
	}
	grant, err = p.signer.SignPlatform(pending.claims)
	if err != nil {
		return CodePlatformPumpResult{}, err
	}
	observed, frame, err := p.owner.ReadPendingPlatformCall(ctx, pending.claims, grant)
	if errors.Is(err, ErrCodePlatformCompleted) || errors.Is(err, ErrCodePlatformCompleting) {
		if _, err = p.snapshot(ctx, claim, dispatch, request, "read_pending_platform_call", nil, nil, nil, &first, nil); err != nil {
			return CodePlatformPumpResult{}, err
		}
		return CodePlatformPumpResult{State: "idle"}, nil
	}
	if err != nil || !observed.Matches(pending.claims) || !sameCodePlatformRuntime(runtime, observed) {
		return CodePlatformPumpResult{}, code.ErrRejected
	}
	// Recheck current authorization after owner IO and before native dispatch.
	current, err := p.snapshot(ctx, claim, dispatch, request, "read_retained_runtime", nil, nil, nil, &first, &observed)
	if err != nil {
		return CodePlatformPumpResult{}, err
	}
	if len(frame) == 0 {
		return CodePlatformPumpResult{State: "idle"}, nil
	}
	decoded, err := domain.DecodeRequest(frame)
	if err != nil || decoded.Sequence > uint64(current.broker.Broker.MaxCalls) {
		return CodePlatformPumpResult{}, domain.ErrFrame
	}
	admission, err := codePlatformAdmission(claim, current, observed)
	if err != nil {
		return CodePlatformPumpResult{}, err
	}
	effect := domain.EffectID(admission.Job, domain.Intent{Call: decoded.CallDigest(admission.Job.Activation, admission.Job.PreparedRequest, admission.Job.Policy)})
	service, err := p.calls.ForCodePlatformCall(claim, admission)
	if err != nil {
		return CodePlatformPumpResult{}, err
	}
	reply, err := service.Call(ctx, admission, frame)
	if errors.Is(err, domain.ErrUnknown) {
		return CodePlatformPumpResult{State: "unknown_effect", EffectID: effect}, nil
	}
	if err != nil {
		return CodePlatformPumpResult{}, err
	}
	if len(reply) < 16 || len(reply) > code.MaxCommittedPlatformReplyBytes {
		return CodePlatformPumpResult{}, domain.ErrConflict
	}
	sequence := decoded.Sequence
	frameHash := code.Digest(frame)
	replyHash := code.Digest(reply)
	publish, err := p.snapshot(ctx, claim, dispatch, request, "publish_committed_platform_reply", &sequence, &frameHash, &replyHash, &first, &observed)
	if err != nil {
		return CodePlatformPumpResult{}, err
	}
	grant, err = p.signer.SignPlatform(publish.claims)
	if err != nil {
		return CodePlatformPumpResult{}, err
	}
	published, err := p.owner.PublishCommittedPlatformReply(ctx, publish.claims, grant, reply)
	if err != nil || !published.Matches(publish.claims) || !sameCodePlatformRuntime(observed, published) {
		return CodePlatformPumpResult{State: "reply_delivery_unknown", EffectID: effect}, nil
	}
	if _, err = p.snapshot(ctx, claim, dispatch, request, "read_retained_runtime", nil, nil, nil, &first, &published); err != nil {
		return CodePlatformPumpResult{}, err
	}
	return CodePlatformPumpResult{State: "committed", EffectID: effect}, nil
}

var _ CodePlatformOwner = (*CodeOwnerClient)(nil)
var _ CodePlatformOwnerSigner = (*CodeOwnerGrantSigner)(nil)
