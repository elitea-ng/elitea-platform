package codesandbox

import (
	"math"

	recovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	runtime "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const PlatformDomain = "elitea.sandbox.platform-broker-owner-grant.ed25519.v1\x00"
const MaxPlatformResponseBytes = 3 * 1024 * 1024
const MaxPlatformCallBytes = 327688
const MaxCommittedPlatformReplyBytes = 2166800

// PlatformCompiledExecute contains only the original registered Execute selector.
// It carries no Compile, platform-call, or new descriptor selection authority.
type PlatformCompiledExecute struct {
	Binding                  runtime.RustSnapshotBinding `json:"binding"`
	SnapshotKeySHA256        string                      `json:"snapshot_key_sha256"`
	SelectedDescriptorSHA256 string                      `json:"selected_descriptor_sha256"`
}

func (s PlatformCompiledExecute) MatchesRequest(tenant string, project int64, request, fingerprint string) bool {
	if s.Binding.Validate() != nil || s.Binding.TenantID != tenant || int64(s.Binding.ProjectID) != project || s.Binding.PolicyRevision != "cargo-broker-execute-v1" || s.Binding.BasePreparedRequestSHA256 != fingerprint || !NonzeroDigest(s.SelectedDescriptorSHA256) {
		return false
	}
	key, err := s.Binding.Key()
	if err != nil || key != s.SnapshotKeySHA256 {
		return false
	}
	digest, err := runtime.SnapshotJobDigest("execute", s.Binding, s.SelectedDescriptorSHA256)
	return err == nil && digest == request
}
func SamePlatformCompiledExecute(a, b *PlatformCompiledExecute) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// PlatformGrantClaims selects only the original owner and a closed helper operation.
// Runtime IDs come from the authenticated owner response, never from these claims.
type PlatformGrantClaims struct {
	Schema                    string                   `json:"schema"`
	Purpose                   string                   `json:"purpose"`
	TenantID                  string                   `json:"tenant_id"`
	ProjectID                 int64                    `json:"project_id"`
	ExecutionID               string                   `json:"execution_id"`
	OriginalGeneration        uint64                   `json:"original_generation"`
	ClaimID                   string                   `json:"claim_id"`
	ClaimAttempt              uint64                   `json:"claim_attempt"`
	LeaseEpoch                uint64                   `json:"lease_epoch"`
	FenceSHA256               string                   `json:"fence_sha256"`
	ActivationID              string                   `json:"activation_id"`
	Attempt                   uint16                   `json:"attempt"`
	DispatchActivation        string                   `json:"dispatch_activation"`
	JobKey                    string                   `json:"job_key"`
	RequestDigest             string                   `json:"request_digest"`
	BindingSHA256             string                   `json:"binding_sha256"`
	PreparedSHA256            string                   `json:"prepared_job_sha256"`
	PreparedFingerprint       string                   `json:"prepared_fingerprint"`
	CompiledExecute           *PlatformCompiledExecute `json:"compiled_execute"`
	PolicySHA256              string                   `json:"policy_sha256"`
	MaxCalls                  uint32                   `json:"max_calls"`
	MaxTotalBytes             uint64                   `json:"max_total_bytes"`
	SupervisorAudience        string                   `json:"supervisor_audience"`
	RequesterWorkloadIdentity string                   `json:"requester_workload_identity"`
	Operation                 string                   `json:"operation"`
	Sequence                  *uint64                  `json:"sequence"`
	PlatformRequestSHA256     *string                  `json:"platform_request_sha256"`
	CommittedReplySHA256      *string                  `json:"committed_reply_sha256"`
	IssuedAtMillis            int64                    `json:"issued_at_unix_millis"`
	ExpiresAtMillis           int64                    `json:"expires_at_unix_millis"`
}

func (c PlatformGrantClaims) Validate() error {
	if c.Schema != "elitea.sandbox.code-platform-owner-grant.v1" || c.Purpose != "platform_broker_runtime" || !Identity(c.TenantID) || c.ProjectID <= 0 || c.ProjectID > math.MaxInt32 || !recovery.ValidExecutionID(c.ExecutionID) || c.OriginalGeneration == 0 || c.OriginalGeneration > math.MaxInt64 || !recovery.ValidExecutionID(c.ClaimID) || c.ClaimAttempt == 0 || c.ClaimAttempt > math.MaxInt64 || c.LeaseEpoch == 0 || c.LeaseEpoch > math.MaxInt64 || !NonzeroDigest(c.FenceSHA256) || !NonzeroDigest(c.ActivationID) || c.Attempt < 1 || c.Attempt > 16 || !NonzeroDigest(c.DispatchActivation) || c.JobKey != JobKey(c.ExecutionID, c.DispatchActivation) || !NonzeroDigest(c.RequestDigest) || !NonzeroDigest(c.BindingSHA256) || !NonzeroDigest(c.PreparedSHA256) || !NonzeroDigest(c.PreparedFingerprint) || !recovery.ValidID(c.PolicySHA256) || c.MaxCalls < 1 || c.MaxCalls > 4096 || c.MaxTotalBytes < 1 || c.MaxTotalBytes > 67108864 || !Identity(c.SupervisorAudience) || !Identity(c.RequesterWorkloadIdentity) || c.IssuedAtMillis <= 0 || c.ExpiresAtMillis <= c.IssuedAtMillis || c.ExpiresAtMillis-c.IssuedAtMillis > 30000 {
		return ErrRejected
	}
	if c.CompiledExecute == nil {
		if c.RequestDigest != c.PreparedFingerprint {
			return ErrRejected
		}
	} else if !c.CompiledExecute.MatchesRequest(c.TenantID, c.ProjectID, c.RequestDigest, c.PreparedFingerprint) {
		return ErrRejected
	}
	switch c.Operation {
	case "read_retained_runtime", "read_pending_platform_call":
		if c.Sequence != nil || c.PlatformRequestSHA256 != nil || c.CommittedReplySHA256 != nil {
			return ErrRejected
		}
	case "publish_committed_platform_reply":
		if c.Sequence == nil || *c.Sequence < 1 || *c.Sequence > uint64(c.MaxCalls) || c.PlatformRequestSHA256 == nil || !NonzeroDigest(*c.PlatformRequestSHA256) || c.CommittedReplySHA256 == nil || !NonzeroDigest(*c.CommittedReplySHA256) {
			return ErrRejected
		}
	default:
		return ErrRejected
	}
	return nil
}

type PlatformOwnerRequest struct {
	Schema                  string      `json:"schema"`
	Grant                   SignedGrant `json:"grant"`
	CommittedReplyBase64URL *string     `json:"committed_reply_base64url"`
}
type RetainedRuntime struct {
	Kind                string                   `json:"kind"`
	RuntimeID           string                   `json:"runtime_id"`
	OwnerEpoch          uint64                   `json:"owner_epoch"`
	BindingSHA256       string                   `json:"binding_sha256"`
	PreparedSHA256      string                   `json:"prepared_job_sha256"`
	PreparedFingerprint string                   `json:"prepared_fingerprint"`
	CompiledExecute     *PlatformCompiledExecute `json:"compiled_execute"`
	PolicySHA256        string                   `json:"policy_sha256"`
	MaxCalls            uint32                   `json:"max_calls"`
	MaxTotalBytes       uint64                   `json:"max_total_bytes"`
	Lifecycle           string                   `json:"lifecycle"`
}

func (r RetainedRuntime) Matches(c PlatformGrantClaims) bool {
	return (r.Kind == "docker" || r.Kind == "kubernetes") && Identity(r.RuntimeID) && r.OwnerEpoch > 0 && r.OwnerEpoch <= math.MaxInt64 && r.Lifecycle == "dispatched" && r.BindingSHA256 == c.BindingSHA256 && r.PreparedSHA256 == c.PreparedSHA256 && r.PreparedFingerprint == c.PreparedFingerprint && SamePlatformCompiledExecute(r.CompiledExecute, c.CompiledExecute) && r.PolicySHA256 == c.PolicySHA256 && r.MaxCalls == c.MaxCalls && r.MaxTotalBytes == c.MaxTotalBytes
}

type PlatformOwnerResponse struct {
	Schema               string          `json:"schema"`
	State                string          `json:"state"`
	Runtime              RetainedRuntime `json:"runtime"`
	PendingCallBase64URL *string         `json:"pending_call_base64url"`
	ReplyPublished       *bool           `json:"reply_published"`
}
