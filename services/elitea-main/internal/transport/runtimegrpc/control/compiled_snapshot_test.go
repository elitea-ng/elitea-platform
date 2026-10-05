package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"os"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type compiledIndexStub struct {
	candidate   domain.SnapshotCandidate
	err         error
	lookups     int
	original    *domain.SnapshotExecution
	originalErr error
}

func (i *compiledIndexStub) OriginalExecution(context.Context, domain.SnapshotScope, string, string, string) (domain.SnapshotExecution, bool, error) {
	if i.original != nil {
		return *i.original, true, i.originalErr
	}
	return domain.SnapshotExecution{}, false, i.originalErr
}
func (i *compiledIndexStub) Candidate(context.Context, domain.SnapshotScope, string, string, bool) (domain.SnapshotCandidate, error) {
	i.lookups++
	return i.candidate, i.err
}
func (i *compiledIndexStub) Ready(context.Context, domain.SnapshotScope, string) (domain.SnapshotCandidate, error) {
	i.lookups++
	return i.candidate, i.err
}
func (*compiledIndexStub) Reserve(context.Context, domain.SnapshotCandidate) error { return nil }
func (*compiledIndexStub) WithPublishing(context.Context, domain.SnapshotCandidate, func() error) error {
	return nil
}
func (*compiledIndexStub) WithReady(context.Context, domain.SnapshotScope, string, string, func(domain.SnapshotCandidate) error) error {
	return nil
}
func (*compiledIndexStub) CommitReady(context.Context, domain.SnapshotCandidate, func() error) error {
	return nil
}
func TestCompiledSnapshotIssuerRolesMissAndPin(t *testing.T) {
	for _, name := range []string{"compile", "publish", "read", "execute", "miss", "pinned-miss", "execute-miss", "pin-changed", "expired", "default-off", "profile", "scope", "cancelled", "audience", "publish-other-activation", "execute-unpinned", "execute-recovery-index-expired", "execute-recovery-conflict"} {
		t.Run(name, func(t *testing.T) {
			calls := []string{}
			lease := validLease()
			command := &runtimev1.WorkerCommandV1{CommandId: lease.Fence.CommandID, ExecutionId: lease.Fence.ExecutionID, Generation: lease.Fence.Generation, TenantId: "tenant", ResourceProjectId: "2", CommandType: runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION}
			identity := identityForFence(lease.Fence)
			identity.TenantId = "tenant"
			identity.ResourceProjectId = "2"
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
			source := "fn main(){}"
			binding.SourceSHA256 = domain.SnapshotContentSHA256([]byte(source))
			prepared, _ := domain.SnapshotJSON(map[string]any{"revision": 1, "language": "rust", "source": source, "input": map[string]any{}, "image_digest": binding.ExecutionImageDigest, "policy_revision": binding.PolicyRevision, "timeout_seconds": 10})
			binding.BasePreparedRequestSHA256 = domain.SnapshotHash("elitea.sandbox.prepared-job.v1\x00", prepared)
			bindingJSON, _ := domain.SnapshotJSON(binding)
			key, _ := binding.Key()
			descriptor, _ := domain.SnapshotJSON(domain.RustSnapshotDescriptor{Revision: 1, Binding: binding, SnapshotKeySHA256: key, ExecutableSHA256: domain.SnapshotContentSHA256([]byte("test")), ExecutableBytes: 4})
			root := domain.SnapshotContentSHA256(descriptor)
			digest, _ := domain.SnapshotJobDigest("compile", binding, "")
			activation := "node/compile"
			candidate := domain.SnapshotCandidate{Scope: domain.SnapshotScope{TenantID: "tenant", ProjectID: 2}, Key: key, Root: root, DescriptorJSON: descriptor, CompilationJobKey: domain.SnapshotActivationKey(lease.Fence.ExecutionID, activation), CompilationRequestDigest: digest, CompilationRuntimeID: "original-runtime", CompilationLeaseEpoch: 3, ReceiptSHA256: domain.SnapshotContentSHA256([]byte("receipt"))}
			index := &compiledIndexStub{candidate: candidate}
			profiles, _ := domain.NewRustSnapshotProfiles([]domain.RustSnapshotProfile{{Binding: binding}})
			signer := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
			issuer, _ := NewSandboxGrantIssuer("key-1", signer, []string{"sandbox-prod"}, func() time.Time { return time.Unix(1000, 0) })
			config := testControlServerConfig()
			config.SandboxGrants = issuer
			config.CompiledSnapshots = index
			config.CompiledProfiles = profiles
			request := &runtimev1.AuthorizeRustCompiledSnapshotRequestV1{Identity: identity, Fence: fenceProto(lease.Fence), ActivationId: activation, Audience: "sandbox-prod", Purpose: runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_READ, PreparedJobJson: prepared, BindingJson: bindingJSON}
			expected := codes.OK
			switch name {
			case "compile":
				request.Purpose = runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_COMPILE
			case "publish", "publish-other-activation":
				request.Purpose = runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH
				request.CompilationJobKey, _ = hex.DecodeString(candidate.CompilationJobKey)
				if name == "publish-other-activation" {
					request.ActivationId = "other"
					expected = codes.PermissionDenied
				}
			case "execute", "execute-miss", "execute-unpinned", "execute-recovery-index-expired", "execute-recovery-conflict":
				request.Purpose = runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_EXECUTE
				request.SelectedDescriptorSha256, _ = hex.DecodeString(root)
				if name == "execute-recovery-index-expired" || name == "execute-recovery-conflict" {
					index.err = domain.ErrSnapshotUnavailable
					index.original = &domain.SnapshotExecution{DescriptorJSON: descriptor, Root: root, Key: key, RuntimeID: "original-execution-runtime"}
					if name == "execute-recovery-conflict" {
						index.originalErr = domain.ErrSnapshotConflict
						expected = codes.FailedPrecondition
					}
				}
				if name == "execute-miss" {
					index.err = domain.ErrSnapshotMiss
					expected = codes.FailedPrecondition
				}
				if name == "execute-unpinned" {
					request.SelectedDescriptorSha256 = nil
					expected = codes.InvalidArgument
				}
			case "miss":
				index.err = domain.ErrSnapshotMiss
				expected = codes.NotFound
			case "pinned-miss":
				index.err = domain.ErrSnapshotMiss
				request.SelectedDescriptorSha256, _ = hex.DecodeString(root)
				expected = codes.FailedPrecondition
			case "pin-changed":
				request.SelectedDescriptorSha256 = bytes.Repeat([]byte{1}, 32)
				expected = codes.FailedPrecondition
			case "expired":
				index.err = domain.ErrSnapshotUnavailable
				expected = codes.FailedPrecondition
			case "default-off":
				config.CompiledSnapshots = nil
				expected = codes.Unimplemented
			case "profile":
				binding.AdapterSHA256 = domain.SnapshotContentSHA256([]byte("untrusted"))
				request.BindingJson, _ = domain.SnapshotJSON(binding)
				expected = codes.PermissionDenied
			case "scope":
				identity.TenantId = "other"
				expected = codes.PermissionDenied
			case "cancelled":
				lease.DesiredState = domain.DesiredCancelled
				expected = codes.PermissionDenied
			case "audience":
				request.Audience = "other"
				expected = codes.PermissionDenied
			}
			server, err := NewServer(config, workloadAuthorizerStub{calls: &calls}, sandboxVerifierStub{command: command}, claimControllerStub{calls: &calls, lease: lease}, inputResolverStub{calls: &calls, manifest: validManifest()}, &settlementControllerStub{calls: &calls})
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.AuthorizeRustCompiledSnapshot(context.Background(), request)
			if status.Code(err) != expected {
				t.Fatalf("got %v want %v", err, expected)
			}
			if expected != codes.OK {
				return
			}
			grant := response.Grant
			input := append([]byte(sandboxGrantDomain), make([]byte, 8)...)
			binary.BigEndian.PutUint64(input[len(sandboxGrantDomain):], uint64(len(grant.ClaimsBytes)))
			input = append(input, grant.ClaimsBytes...)
			if !ed25519.Verify(signer.Public().(ed25519.PublicKey), input, grant.Signature) {
				t.Fatal("bad signature")
			}
			claims := new(runtimev1.RustCompiledSnapshotGrantClaimsV1)
			if proto.Unmarshal(grant.ClaimsBytes, claims) != nil || claims.Revision != 4 || claims.Purpose != request.Purpose || claims.ExpiresAtUnixMillis-claims.IssuedAtUnixMillis != 30000 || hex.EncodeToString(claims.SnapshotKeySha256) != key {
				t.Fatal("bad claims")
			}
			if name == "compile" {
				if len(claims.DescriptorSha256) != 0 || len(response.DescriptorJson) != 0 || index.lookups != 0 {
					t.Fatal("compile selected snapshot")
				}
			} else if hex.EncodeToString(claims.DescriptorSha256) != root || !bytes.Equal(response.DescriptorJson, descriptor) {
				t.Fatal("selected root differs")
			}
			if name == "execute-recovery-index-expired" && index.lookups != 0 {
				t.Fatal("original recovery consulted expired cache")
			}
			if name == "publish" && claims.CompilationLeaseEpoch != 3 {
				t.Fatal("publication omitted fence")
			}
		})
	}
}
