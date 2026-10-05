package storage

import (
	"bytes"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	"google.golang.org/protobuf/proto"
)

func workspaceAuthorityPrepared(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../../../../libs/proto/elitea/runtime/v1/code_prepared_workspace_only_v4.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func workspaceAuthorityTLS(t *testing.T, audience string) *tls.ConnectionState {
	t.Helper()
	identity, err := url.Parse(audience)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{Raw: []byte{1}, URIs: []*url.URL{identity}}
	return &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{leaf}}, PeerCertificates: []*x509.Certificate{leaf}}
}
func workspaceAuthorityJob(t *testing.T, signer *CodeOwnerGrantSigner, claims proto.Message) *runtimev1.SignedSandboxJobGrantV1 {
	t.Helper()
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	domain := "elitea.sandbox.job-grant.ed25519.v1\x00"
	framed := append([]byte(domain), make([]byte, 8)...)
	binary.BigEndian.PutUint64(framed[len(domain):], uint64(len(wire)))
	framed = append(framed, wire...)
	return &runtimev1.SignedSandboxJobGrantV1{KeyId: signer.keyID, ClaimsBytes: wire, Signature: ed25519.Sign(signer.key, framed)}
}
func workspaceAuthorityIntent(t *testing.T, s *CodeOwnerGrantSigner, job CodePreparedRequest) code.IntentClaims {
	t.Helper()
	activation := strings.Repeat("1", 64)
	dispatch := strings.Repeat("a", 64)
	return code.IntentClaims{Schema: "elitea.sandbox.original-code-intent.v1", Purpose: "whole_code_execute", TenantID: "7", ProjectID: 7, ExecutionID: "0123456789abcdef0123456789abcdef", OriginalGeneration: 1, ClaimID: "23456789abcdef0123456789abcdef01", ClaimAttempt: 2, LeaseEpoch: 3, FenceSHA256: strings.Repeat("2", 64), ActivationID: activation, NodeID: "run", GraphThread: "root", Step: 1, Attempt: 1, NodeDigest: strings.Repeat("3", 64), DispatchActivation: dispatch, JobKey: code.JobKey("0123456789abcdef0123456789abcdef", dispatch), RequestDigest: job.Fingerprint, SupervisorAudience: "spiffe://elitea/supervisor/one", SubmitterWorkloadIdentity: "spiffe://elitea/worker/one", Language: job.Language, PreparedSHA256: job.PreparedSHA256, SourceSHA256: code.Digest([]byte(job.Source)), InputSHA256: code.Digest(job.Input), IssuedAtMillis: 1000, ExpiresAtMillis: 21000}
}
func TestCodeWorkspacePlainReadSealsJointIntentGrantAndActualTLSPeer(t *testing.T) {
	signer, _, _ := codeGrantFixture(t)
	prepared := workspaceAuthorityPrepared(t)
	job, err := ParseCodePreparedRequest(prepared)
	if err != nil {
		t.Fatal(err)
	}
	intent := workspaceAuthorityIntent(t, signer, job)
	for _, name := range []string{"joint read", "missing intent", "changed exact input", "changed request", "changed worker", "changed Supervisor", "unverified TLS", "different verified leaf", "cancel grant", "unknown signed claim", "expired intent"} {
		t.Run(name, func(t *testing.T) {
			c := intent
			rawPrepared := bytes.Clone(prepared)
			connection := workspaceAuthorityTLS(t, c.SupervisorAudience)
			grantClaims := &runtimev1.SandboxJobGrantClaimsV1{Revision: 1, TenantId: c.TenantID, ProjectId: int32(c.ProjectID), ExecutionId: c.ExecutionID, Generation: c.OriginalGeneration, ActivationId: c.DispatchActivation, RequestDigest: digestCodeBytes(c.RequestDigest), SubmitterWorkloadIdentity: c.SubmitterWorkloadIdentity, Audience: c.SupervisorAudience, IssuedAtUnixMillis: c.IssuedAtMillis, ExpiresAtUnixMillis: c.ExpiresAtMillis}
			switch name {
			case "changed exact input":
				rawPrepared = bytes.Replace(rawPrepared, []byte("9007199254740993"), []byte("9007199254740994"), 1)
			case "changed request":
				grantClaims.RequestDigest = bytes.Repeat([]byte{9}, 32)
			case "changed worker":
				grantClaims.SubmitterWorkloadIdentity = "spiffe://elitea/worker/two"
			case "changed Supervisor":
				connection = workspaceAuthorityTLS(t, "spiffe://other/supervisor")
			case "unverified TLS":
				connection.VerifiedChains = nil
			case "different verified leaf":
				connection.VerifiedChains = [][]*x509.Certificate{{{Raw: []byte{2}}}}
			case "cancel grant":
				grantClaims.CancelOnly = true
			case "unknown signed claim":
				grantClaims.ProtoReflect().SetUnknown([]byte{0xf8, 0x01, 0x01})
			case "expired intent":
				c.ExpiresAtMillis = 1500
			}
			signed, err := signer.SignIntent(c)
			if err != nil {
				t.Fatal(err)
			}
			signedJSON, _ := json.Marshal(signed)
			if name == "missing intent" {
				signedJSON = nil
			}
			authority, err := signer.VerifyCodeWorkspaceRead(connection, workspaceAuthorityJob(t, signer, grantClaims), signedJSON, rawPrepared, time.UnixMilli(2000))
			if name == "joint read" {
				view, ok := authority.View()
				if err != nil || !ok || view.Mode != CodeWorkspaceExecuteMode || authority.JobActivation() != c.DispatchActivation || view.IntentBinding.ActivationID != c.ActivationID || view.PreparedSHA256 != job.PreparedSHA256 || view.PreparedFingerprint == view.PreparedSHA256 {
					t.Fatal("joint authority lost distinct original/final facts", view, err)
				}
				return
			}
			if _, ok := authority.View(); err == nil || ok || authority.JobActivation() != "" {
				t.Fatal("invalid peer/joint request sealed", err)
			}
		})
	}
}
func TestCodeWorkspaceCompileAccessHasDistinctRoleAndExactPreparedFingerprint(t *testing.T) {
	signer, _, _ := codeGrantFixture(t)
	fixture := workspaceAuthorityPrepared(t)
	offset := bytes.Index(fixture, []byte(`,"workspace":`))
	if offset < 0 {
		t.Fatal("fixture workspace")
	}
	base := []byte(`{"revision":4,"language":"rust","source":"fn main(){}","input":{},"image_digest":"sha256:` + strings.Repeat("a", 64) + `","policy_revision":"isolated-v1","timeout_seconds":10}`)
	prepared := append(bytes.TrimSuffix(base, []byte("}")), fixture[offset:]...)
	job, err := ParseCodePreparedRequest(prepared)
	if err != nil {
		t.Fatal(err)
	}
	ref := &runtimev1.OriginalCodeVisitRefV1{VisitId: strings.Repeat("4", 64), Revision: 1, DigestSha256: strings.Repeat("5", 64)}
	for _, name := range []string{"compile", "missing access", "raw SHA substitution", "access on Read", "access on Execute", "unknown access", "bad current claim", "bad revision", "descriptor on Compile", "intent on Compile"} {
		t.Run(name, func(t *testing.T) {
			access := &runtimev1.OriginalCodeVisitAccessV1{OriginalVisit: proto.Clone(ref).(*runtimev1.OriginalCodeVisitRefV1), ClaimId: "23456789abcdef0123456789abcdef01", ClaimAttempt: 2, LeaseEpoch: 3, FenceSha256: bytes.Repeat([]byte{7}, 32)}
			claims := &runtimev1.RustCompiledSnapshotGrantClaimsV1{Revision: 4, TenantId: "7", ProjectId: 7, ExecutionId: "0123456789abcdef0123456789abcdef", Generation: 1, ActivationId: "compile-activation", RequestDigest: bytes.Repeat([]byte{8}, 32), SubmitterWorkloadIdentity: "spiffe://elitea/worker/one", Audience: "spiffe://elitea/supervisor/one", IssuedAtUnixMillis: 1000, ExpiresAtUnixMillis: 21000, Purpose: runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_COMPILE, BasePreparedRequestSha256: digestCodeBytes(job.Fingerprint), SnapshotKeySha256: bytes.Repeat([]byte{9}, 32), OriginalCodeVisitAccess: access}
			var intentJSON []byte
			switch name {
			case "missing access":
				claims.OriginalCodeVisitAccess = nil
			case "raw SHA substitution":
				claims.BasePreparedRequestSha256 = digestCodeBytes(job.PreparedSHA256)
			case "access on Read":
				claims.Purpose = runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_READ
			case "access on Execute":
				claims.Purpose = runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_EXECUTE
				claims.DescriptorSha256 = bytes.Repeat([]byte{6}, 32)
			case "unknown access":
				access.ProtoReflect().SetUnknown([]byte{0x30, 0x01})
			case "bad current claim":
				access.ClaimId = "10000000-0000-4000-8000-000000000001"
			case "bad revision":
				access.OriginalVisit.Revision = 2
			case "descriptor on Compile":
				claims.DescriptorSha256 = bytes.Repeat([]byte{6}, 32)
			case "intent on Compile":
				intent := workspaceAuthorityIntent(t, signer, job)
				signed, _ := signer.SignIntent(intent)
				intentJSON, _ = json.Marshal(signed)
			}
			authority, err := signer.VerifyCodeWorkspaceRead(workspaceAuthorityTLS(t, claims.Audience), workspaceAuthorityJob(t, signer, claims), intentJSON, prepared, time.UnixMilli(2000))
			if name == "compile" {
				view, ok := authority.View()
				if err != nil || !ok || view.Mode != CodeWorkspaceCompileMode || view.OriginalVisit.VisitID != ref.VisitId || view.IntentBinding != (code.Binding{}) || view.PreparedFingerprint != job.Fingerprint {
					t.Fatal(view, err)
				}
				return
			}
			if _, ok := authority.View(); err == nil || ok {
				t.Fatal("cross-role/changed original Compile proof sealed", name, err)
			}
		})
	}
}
