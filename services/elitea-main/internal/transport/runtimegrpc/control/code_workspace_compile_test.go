package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcpeer "google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type workspaceCompileAuthorizerStub struct {
	calls       int
	ref         code.OriginalVisitRef
	fingerprint string
	deny        bool
}

func (s *workspaceCompileAuthorizerStub) AuthorizeCodeWorkspaceCompile(_ context.Context, certificate *x509.Certificate, fence domain.Fence, ref code.OriginalVisitRef, prepared []byte) (storage.CodeCompileVisitAccess, error) {
	s.calls++
	job, err := storage.ParseCodePreparedRequest(prepared)
	if s.deny || err != nil || certificate == nil || ref != s.ref || job.Fingerprint != s.fingerprint || job.Workspace == nil || job.Broker == nil || job.PolicyRevision != "cargo-broker-execute-v1" {
		return storage.CodeCompileVisitAccess{}, code.ErrRejected
	}
	return storage.CodeCompileVisitAccess{OriginalVisit: ref, ClaimID: "23456789abcdef0123456789abcdef01", ClaimAttempt: fence.ClaimAttempt, LeaseEpoch: fence.LeaseEpoch, FenceSHA256: bytes.Repeat([]byte{7}, 32), ExpiresAtUnixMillis: time.Unix(1000, 0).Add(20 * time.Second).UnixMilli()}, nil
}

// This exercises the actual owning control issuer. Its workspace store and
// snapshot index are offline fakes; source-derived broker bytes are not runtime proof.
func TestWorkspaceBrokerCompiledLifecycleUsesRoleSpecificAuthority(t *testing.T) {
	for _, name := range []string{"compile", "publish", "read", "execute", "compile missing visit", "compile unknown visit", "compile denied acquisition", "visit on publish", "visit on read", "visit on execute", "wrong measured profile", "forged descriptor", "profile drift after compile", "unverified compile peer", "foreign purpose"} {
		t.Run(name, func(t *testing.T) {
			calls := []string{}
			lease := validLease()
			lease.Fence.ExecutionID = "0123456789abcdef0123456789abcdef"
			identity := identityForFence(lease.Fence)
			identity.TenantId = "tenant"
			identity.ResourceProjectId = "2"
			command := &runtimev1.WorkerCommandV1{CommandId: lease.Fence.CommandID, ExecutionId: lease.Fence.ExecutionID, Generation: lease.Fence.Generation, TenantId: "tenant", ResourceProjectId: "2", CommandType: runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION}
			raw, err := os.ReadFile("testdata/compiled-snapshot-v1/binding.json")
			if err != nil {
				t.Fatal(err)
			}
			binding, err := domain.ParseRustSnapshotBinding(raw)
			if err != nil {
				t.Fatal(err)
			}
			binding.TenantID = "tenant"
			binding.ProjectID = 2
			binding.PolicyRevision = "cargo-broker-execute-v1"
			source := "fn main(){}"
			binding.SourceSHA256 = code.Digest([]byte(source))
			fixture, err := os.ReadFile("../../../../../../libs/proto/elitea/runtime/v1/code_prepared_workspace_both_v5.json")
			if err != nil {
				t.Fatal(err)
			}
			at := bytes.Index(fixture, []byte(`,"workspace":`))
			if at < 0 {
				t.Fatal("workspace fixture missing")
			}
			base := []byte(`{"revision":5,"language":"rust","source":"fn main(){}","input":{},"image_digest":"` + binding.ExecutionImageDigest + `","policy_revision":"cargo-broker-execute-v1","timeout_seconds":10}`)
			prepared := append(bytes.TrimSuffix(base, []byte("}")), fixture[at:]...)
			job, err := storage.ParseCodePreparedRequest(prepared)
			if err != nil {
				t.Fatal(err)
			}
			binding.BasePreparedRequestSHA256 = job.Fingerprint
			key, _ := binding.Key()
			bindingJSON, _ := domain.SnapshotJSON(binding)
			descriptor, _ := domain.SnapshotJSON(domain.RustSnapshotDescriptor{Revision: 1, Binding: binding, SnapshotKeySHA256: key, ExecutableSHA256: code.Digest([]byte("test")), ExecutableBytes: 4})
			root := code.Digest(descriptor)
			compileDigest, _ := domain.SnapshotJobDigest("compile", binding, "")
			activation := "node/compile"
			index := &compiledIndexStub{candidate: domain.SnapshotCandidate{Scope: domain.SnapshotScope{TenantID: "tenant", ProjectID: 2}, Key: key, Root: root, DescriptorJSON: descriptor, CompilationJobKey: domain.SnapshotActivationKey(lease.Fence.ExecutionID, activation), CompilationRequestDigest: compileDigest, CompilationRuntimeID: "original-runtime", CompilationLeaseEpoch: 3, ReceiptSHA256: code.Digest([]byte("receipt"))}}
			profiles, err := domain.NewRustSnapshotProfiles([]domain.RustSnapshotProfile{{Binding: binding, DependencyBundleSHA256: job.DependencyBundleSHA256}})
			if err != nil {
				t.Fatal(err)
			}
			reference := code.OriginalVisitRef{VisitID: strings.Repeat("4", 64), Revision: 1, DigestSHA256: strings.Repeat("5", 64)}
			workspace := &workspaceCompileAuthorizerStub{ref: reference, fingerprint: job.Fingerprint}
			issuer, err := NewSandboxGrantIssuer("key-1", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32)), []string{"sandbox-prod"}, func() time.Time { return time.Unix(1000, 0) })
			if err != nil {
				t.Fatal(err)
			}
			config := testControlServerConfig()
			config.SandboxGrants = issuer
			config.CompiledSnapshots = index
			config.CompiledProfiles = profiles
			config.OriginalCodeWorkspaces = workspace
			request := &runtimev1.AuthorizeRustCompiledSnapshotRequestV1{Identity: identity, Fence: fenceProto(lease.Fence), ActivationId: activation, Audience: "sandbox-prod", Purpose: runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_COMPILE, PreparedJobJson: prepared, BindingJson: bindingJSON, OriginalCodeVisit: &runtimev1.OriginalCodeVisitRefV1{VisitId: reference.VisitID, Revision: 1, DigestSha256: reference.DigestSHA256}}
			expected := codes.OK
			switch name {
			case "publish", "visit on publish":
				request.Purpose = runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH
				request.CompilationJobKey, _ = hex.DecodeString(index.candidate.CompilationJobKey)
			case "read", "visit on read":
				request.Purpose = runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_READ
			case "execute", "visit on execute", "forged descriptor", "profile drift after compile":
				request.Purpose = runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_EXECUTE
				request.SelectedDescriptorSha256, _ = hex.DecodeString(root)
			case "compile missing visit":
				request.OriginalCodeVisit = nil
				expected = codes.PermissionDenied
			case "compile unknown visit":
				request.OriginalCodeVisit.ProtoReflect().SetUnknown([]byte{0x20, 0x01})
				expected = codes.InvalidArgument
			case "compile denied acquisition":
				workspace.deny = true
				expected = codes.PermissionDenied
			case "wrong measured profile":
				binding.AdapterSHA256 = strings.Repeat("a", 64)
				request.BindingJson, _ = domain.SnapshotJSON(binding)
				expected = codes.PermissionDenied
			case "unverified compile peer":
				expected = codes.Unauthenticated
			case "foreign purpose":
				request.Purpose = runtimev1.RustCompiledSnapshotPurposeV1(99)
				expected = codes.InvalidArgument
			}
			if request.Purpose != runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_COMPILE && !strings.HasPrefix(name, "visit on ") {
				request.OriginalCodeVisit = nil
			}
			if strings.HasPrefix(name, "visit on ") {
				expected = codes.InvalidArgument
			}
			if name == "forged descriptor" {
				request.SelectedDescriptorSha256 = bytes.Repeat([]byte{1}, 32)
				expected = codes.FailedPrecondition
			}
			if name == "profile drift after compile" {
				binding.WrapperSHA256 = strings.Repeat("a", 64)
				request.BindingJson, _ = domain.SnapshotJSON(binding)
				expected = codes.PermissionDenied
			}
			uri, err := url.Parse(lease.Fence.WorkloadIdentity)
			if err != nil {
				t.Fatal(err)
			}
			certificate := &x509.Certificate{Raw: []byte{1}, URIs: []*url.URL{uri}}
			tlsState := tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}, VerifiedChains: [][]*x509.Certificate{{certificate}}}
			if name == "unverified compile peer" {
				tlsState.VerifiedChains = nil
			}
			ctx := grpcpeer.NewContext(t.Context(), &grpcpeer.Peer{AuthInfo: credentials.TLSInfo{State: tlsState}})
			server, err := NewServer(config, workloadAuthorizerStub{calls: &calls}, sandboxVerifierStub{command: command}, claimControllerStub{calls: &calls, lease: lease}, inputResolverStub{calls: &calls, manifest: validManifest()}, &settlementControllerStub{calls: &calls})
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.AuthorizeRustCompiledSnapshot(ctx, request)
			if status.Code(err) != expected {
				t.Fatalf("got %v expected %v", err, expected)
			}
			if expected != codes.OK {
				if response != nil {
					t.Fatal("denial returned grant")
				}
				return
			}
			var claims runtimev1.RustCompiledSnapshotGrantClaimsV1
			if proto.Unmarshal(response.Grant.ClaimsBytes, &claims) != nil || claims.Purpose != request.Purpose || claims.Revision != 4 {
				t.Fatal("role changed")
			}
			if name == "compile" {
				if workspace.calls != 1 || claims.OriginalCodeVisitAccess == nil || len(claims.DescriptorSha256) != 0 || claims.ExpiresAtUnixMillis-claims.IssuedAtUnixMillis != 20000 {
					t.Fatal("Compile gained descriptor or lost exact acquisition/current lease")
				}
			} else if workspace.calls != 0 || claims.OriginalCodeVisitAccess != nil || !bytes.Equal(response.DescriptorJson, descriptor) {
				t.Fatal("non-Compile received workspace access or changed original snapshot")
			}
			// These grants use only the compiled job domain and role. SignPlatform refuses
			// Compile purpose independently, so this path cannot grant platform calls.
		})
	}
}
