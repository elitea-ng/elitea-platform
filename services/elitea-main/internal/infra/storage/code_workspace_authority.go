package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"math"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/workloadidentity"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	recovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const CodeWorkspaceCompileMode = "compile"
const CodeWorkspaceExecuteMode = "execute"

// CodeWorkspaceReadAuthority can only be constructed by the local signature,
// role, actual Supervisor mTLS identity and joint prepared-request verifier.
type CodeWorkspaceReadAuthority struct {
	valid bool
	view  CodeWorkspaceReadView
}
type CodeWorkspaceReadView struct {
	Mode                string
	SupervisorIdentity  string
	WorkerIdentity      string
	TenantID            string
	ProjectID           int64
	ExecutionID         string
	Generation          uint64
	ClaimID             string
	ClaimAttempt        uint64
	LeaseEpoch          uint64
	FenceSHA256         string
	OriginalVisit       code.OriginalVisitRef
	JobActivation       string
	RequestDigest       string
	PreparedSHA256      string
	PreparedFingerprint string
	IntentBinding       code.Binding
	ExpiresAtUnixMillis int64
}

func (a CodeWorkspaceReadAuthority) View() (CodeWorkspaceReadView, bool) { return a.view, a.valid }
func (a CodeWorkspaceReadAuthority) JobActivation() string {
	if !a.valid {
		return ""
	}
	return a.view.JobActivation
}

type CodeWorkspaceReadConsumer interface {
	WithCodeWorkspaceRead(context.Context, CodeWorkspaceReadAuthority, []byte, func(context.Context, CodeTransaction, OriginalCodeVisit, CodePreparedRequest) error) error
}

// VerifyOriginalCodeIntent verifies exact signed bytes before strict decoding.
// The final stored record and current claim must still be joined by the repository.
func (s *CodeOwnerGrantSigner) VerifyOriginalCodeIntent(raw []byte, now time.Time) (code.IntentClaims, error) {
	var envelope code.SignedGrant
	if s == nil || code.Decode(raw, &envelope, 16384) != nil || !code.RequiredFields(raw, []string{"schema", "key_id", "claims_base64url", "signature_base64url"}) || envelope.Schema != "elitea.sandbox.original-code-intent-signed.v1" || envelope.KeyID != s.keyID {
		return code.IntentClaims{}, code.ErrRejected
	}
	claims, err := code.DecodeBase64(envelope.ClaimsBase64URL, 8192)
	if err != nil {
		return code.IntentClaims{}, code.ErrRejected
	}
	signature, err := code.DecodeBase64(envelope.SignatureBase64URL, 64)
	if err != nil || !verifyCodeSignature(s.key.Public().(ed25519.PublicKey), code.IntentDomain, claims, signature) {
		return code.IntentClaims{}, code.ErrRejected
	}
	var intent code.IntentClaims
	if code.Decode(claims, &intent, 8192) != nil || !signedCodeOwnerGrantMatches(envelope, "elitea.sandbox.original-code-intent-signed.v1", intent) {
		return code.IntentClaims{}, code.ErrRejected
	}
	// The owning signer performs the same closed bounds as original issuance.
	if _, err = s.SignIntent(intent); err != nil || !validWorkspaceGrantTime(intent.IssuedAtMillis, intent.ExpiresAtMillis, now) {
		return code.IntentClaims{}, code.ErrRejected
	}
	return intent, nil
}
func verifyCodeSignature(key ed25519.PublicKey, domain string, claims, signature []byte) bool {
	if len(key) != 32 || len(signature) != 64 {
		return false
	}
	framed := append([]byte(domain), make([]byte, 8)...)
	binary.BigEndian.PutUint64(framed[len(domain):], uint64(len(claims)))
	framed = append(framed, claims...)
	return ed25519.Verify(key, framed, signature)
}
func validWorkspaceGrantTime(issued, expires int64, now time.Time) bool {
	return issued > 0 && expires > issued && expires-issued <= 30000 && issued <= now.UnixMilli() && now.UnixMilli() < expires
}
func codeWorkspaceUnknown(m protoreflect.Message) bool {
	if len(m.GetUnknown()) > 0 {
		return true
	}
	bad := false
	m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if f.Kind() != protoreflect.MessageKind {
			return true
		}
		if f.IsList() {
			for i := 0; i < v.List().Len(); i++ {
				if codeWorkspaceUnknown(v.List().Get(i).Message()) {
					bad = true
					return false
				}
			}
		} else if f.IsMap() {
			v.Map().Range(func(_ protoreflect.MapKey, value protoreflect.Value) bool {
				if f.MapValue().Kind() == protoreflect.MessageKind && codeWorkspaceUnknown(value.Message()) {
					bad = true
					return false
				}
				return true
			})
		} else {
			bad = codeWorkspaceUnknown(v.Message())
		}
		return !bad
	})
	return bad
}

// Execute has a signed final intent; Compile has a signed pre-stage visit access.
// Neither can stand in for the other's role. This method performs no IO.
func (s *CodeOwnerGrantSigner) VerifyCodeWorkspaceRead(connection *tls.ConnectionState, grant *runtimev1.SignedSandboxJobGrantV1, intentJSON, prepared []byte, now time.Time) (CodeWorkspaceReadAuthority, error) {
	if s == nil || grant == nil || grant.KeyId != s.keyID || len(grant.ClaimsBytes) == 0 || len(grant.ClaimsBytes) > 4096 || codeWorkspaceUnknown(grant.ProtoReflect()) || !verifyCodeSignature(s.key.Public().(ed25519.PublicKey), "elitea.sandbox.job-grant.ed25519.v1\x00", grant.ClaimsBytes, grant.Signature) {
		return CodeWorkspaceReadAuthority{}, code.ErrRejected
	}
	if connection == nil || len(connection.VerifiedChains) == 0 || len(connection.VerifiedChains[0]) == 0 || len(connection.PeerCertificates) == 0 || connection.PeerCertificates[0] == nil || connection.VerifiedChains[0][0] == nil || len(connection.PeerCertificates[0].Raw) == 0 || !bytes.Equal(connection.PeerCertificates[0].Raw, connection.VerifiedChains[0][0].Raw) {
		return CodeWorkspaceReadAuthority{}, ErrContentUnauthorized
	}
	audience, err := workloadidentity.Certificate(connection.PeerCertificates[0])
	if err != nil || !s.audiences[audience] {
		return CodeWorkspaceReadAuthority{}, ErrContentUnauthorized
	}
	job, err := ParseCodePreparedRequest(prepared)
	if err != nil || job.Workspace == nil {
		return CodeWorkspaceReadAuthority{}, code.ErrRejected
	}
	var prefix runtimev1.SandboxJobGrantClaimsV1
	if proto.Unmarshal(grant.ClaimsBytes, &prefix) != nil {
		return CodeWorkspaceReadAuthority{}, code.ErrRejected
	}
	view := CodeWorkspaceReadView{SupervisorIdentity: audience, PreparedSHA256: job.PreparedSHA256, PreparedFingerprint: job.Fingerprint}
	if prefix.Revision == 1 {
		if codeWorkspaceUnknown(prefix.ProtoReflect()) || prefix.CancelOnly || len(prefix.DependencyBundleSha256) != 0 {
			return CodeWorkspaceReadAuthority{}, code.ErrRejected
		}
		intent, err := s.VerifyOriginalCodeIntent(intentJSON, now)
		if err != nil || intent.Language != job.Language || intent.PreparedSHA256 != job.PreparedSHA256 || intent.SourceSHA256 != code.Digest([]byte(job.Source)) || intent.InputSHA256 != code.Digest(job.Input) || intent.RequestDigest != job.Fingerprint {
			return CodeWorkspaceReadAuthority{}, code.ErrRejected
		}
		view = workspaceExecuteView(intent, job)
		if !workspaceJobMatches(view, prefix.TenantId, int64(prefix.ProjectId), prefix.ExecutionId, prefix.Generation, prefix.ActivationId, prefix.SubmitterWorkloadIdentity, prefix.Audience, prefix.RequestDigest, prefix.IssuedAtUnixMillis, prefix.ExpiresAtUnixMillis, audience, now) {
			return CodeWorkspaceReadAuthority{}, code.ErrRejected
		}
		if prefix.ExpiresAtUnixMillis < view.ExpiresAtUnixMillis {
			view.ExpiresAtUnixMillis = prefix.ExpiresAtUnixMillis
		}
	} else if prefix.Revision == 4 {
		var compiled runtimev1.RustCompiledSnapshotGrantClaimsV1
		if proto.Unmarshal(grant.ClaimsBytes, &compiled) != nil || codeWorkspaceUnknown(compiled.ProtoReflect()) || job.Language != "rust" || job.Broker != nil && job.PolicyRevision != "cargo-broker-execute-v1" || !bytes.Equal(compiled.BasePreparedRequestSha256, digestCodeBytes(job.Fingerprint)) {
			return CodeWorkspaceReadAuthority{}, code.ErrRejected
		}
		if compiled.Purpose == runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_COMPILE {
			access := compiled.OriginalCodeVisitAccess
			if len(intentJSON) != 0 || access == nil || access.OriginalVisit == nil || len(compiled.DescriptorSha256) != 0 || !recovery.ValidExecutionID(access.ClaimId) || access.ClaimAttempt == 0 || access.ClaimAttempt > math.MaxInt64 || access.LeaseEpoch == 0 || access.LeaseEpoch > math.MaxInt64 || len(access.FenceSha256) != 32 || access.OriginalVisit.Revision != 1 {
				return CodeWorkspaceReadAuthority{}, code.ErrRejected
			}
			ref := code.OriginalVisitRef{VisitID: access.OriginalVisit.VisitId, Revision: 1, DigestSHA256: access.OriginalVisit.DigestSha256}
			if ref.Validate() != nil {
				return CodeWorkspaceReadAuthority{}, code.ErrRejected
			}
			view = CodeWorkspaceReadView{Mode: CodeWorkspaceCompileMode, SupervisorIdentity: audience, WorkerIdentity: compiled.SubmitterWorkloadIdentity, TenantID: compiled.TenantId, ProjectID: int64(compiled.ProjectId), ExecutionID: compiled.ExecutionId, Generation: compiled.Generation, ClaimID: access.ClaimId, ClaimAttempt: access.ClaimAttempt, LeaseEpoch: access.LeaseEpoch, FenceSHA256: hex.EncodeToString(access.FenceSha256), OriginalVisit: ref, JobActivation: compiled.ActivationId, RequestDigest: hex.EncodeToString(compiled.RequestDigest), PreparedSHA256: job.PreparedSHA256, PreparedFingerprint: job.Fingerprint, ExpiresAtUnixMillis: compiled.ExpiresAtUnixMillis}
		} else if compiled.Purpose == runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_EXECUTE {
			if compiled.OriginalCodeVisitAccess != nil || len(compiled.DescriptorSha256) != 32 {
				return CodeWorkspaceReadAuthority{}, code.ErrRejected
			}
			intent, err := s.VerifyOriginalCodeIntent(intentJSON, now)
			if err != nil || intent.Language != job.Language || intent.PreparedSHA256 != job.PreparedSHA256 || intent.SourceSHA256 != code.Digest([]byte(job.Source)) || intent.InputSHA256 != code.Digest(job.Input) {
				return CodeWorkspaceReadAuthority{}, code.ErrRejected
			}
			view = workspaceExecuteView(intent, job)
		} else {
			return CodeWorkspaceReadAuthority{}, code.ErrRejected
		}
		if len(compiled.SnapshotKeySha256) != 32 || len(compiled.CompilationJobKey) != 0 || compiled.CompilationRuntimeId != "" || len(compiled.CompilationRequestDigest) != 0 || compiled.CompilationLeaseEpoch != 0 || !workspaceJobMatches(view, compiled.TenantId, int64(compiled.ProjectId), compiled.ExecutionId, compiled.Generation, compiled.ActivationId, compiled.SubmitterWorkloadIdentity, compiled.Audience, compiled.RequestDigest, compiled.IssuedAtUnixMillis, compiled.ExpiresAtUnixMillis, audience, now) {
			return CodeWorkspaceReadAuthority{}, code.ErrRejected
		}
		if compiled.ExpiresAtUnixMillis < view.ExpiresAtUnixMillis {
			view.ExpiresAtUnixMillis = compiled.ExpiresAtUnixMillis
		}
	} else {
		return CodeWorkspaceReadAuthority{}, code.ErrRejected
	}
	return CodeWorkspaceReadAuthority{valid: true, view: view}, nil
}
func workspaceExecuteView(intent code.IntentClaims, job CodePreparedRequest) CodeWorkspaceReadView {
	binding := code.Binding{Schema: "elitea.sandbox.whole-code-binding.v1", Purpose: intent.Purpose, ExecutionID: intent.ExecutionID, OriginalGeneration: intent.OriginalGeneration, ActivationID: intent.ActivationID, NodeID: intent.NodeID, GraphThread: intent.GraphThread, Step: intent.Step, Attempt: intent.Attempt, DispatchActivation: intent.DispatchActivation, JobKey: intent.JobKey, RequestDigest: intent.RequestDigest, SupervisorAudience: intent.SupervisorAudience, NodeDigest: intent.NodeDigest, Language: intent.Language, PreparedSHA256: intent.PreparedSHA256, SourceSHA256: intent.SourceSHA256, InputSHA256: intent.InputSHA256}
	return CodeWorkspaceReadView{Mode: CodeWorkspaceExecuteMode, SupervisorIdentity: intent.SupervisorAudience, WorkerIdentity: intent.SubmitterWorkloadIdentity, TenantID: intent.TenantID, ProjectID: intent.ProjectID, ExecutionID: intent.ExecutionID, Generation: intent.OriginalGeneration, ClaimID: intent.ClaimID, ClaimAttempt: intent.ClaimAttempt, LeaseEpoch: intent.LeaseEpoch, FenceSHA256: intent.FenceSHA256, JobActivation: intent.DispatchActivation, RequestDigest: intent.RequestDigest, PreparedSHA256: job.PreparedSHA256, PreparedFingerprint: job.Fingerprint, IntentBinding: binding, ExpiresAtUnixMillis: intent.ExpiresAtMillis}
}
func digestCodeBytes(hexadecimal string) []byte { raw, _ := hex.DecodeString(hexadecimal); return raw }
func workspaceJobMatches(view CodeWorkspaceReadView, tenant string, project int64, execution string, generation uint64, activation, worker, audience string, request []byte, issued, expires int64, peer string, now time.Time) bool {
	return code.Identity(tenant) && project > 0 && project <= math.MaxInt32 && recovery.ValidExecutionID(execution) && generation > 0 && generation <= math.MaxInt64 && code.Identity(activation) && code.Identity(worker) && audience == peer && view.SupervisorIdentity == peer && view.TenantID == tenant && view.ProjectID == project && view.ExecutionID == execution && view.Generation == generation && view.JobActivation == activation && view.WorkerIdentity == worker && len(request) == 32 && view.RequestDigest == hex.EncodeToString(request) && validWorkspaceGrantTime(issued, expires, now)
}
