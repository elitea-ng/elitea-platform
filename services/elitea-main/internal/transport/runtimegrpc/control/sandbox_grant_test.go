package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type sandboxVerifierStub struct {
	command *runtimev1.WorkerCommandV1
	err     error
}

func (v sandboxVerifierStub) Verify(context.Context, *runtimev1.SignedWorkerCommandEnvelopeV1) (*runtimev1.WorkerCommandV1, error) {
	return v.command, v.err
}

func TestSandboxGrantBindsVerifiedScopePeerAudienceAndRequest(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	now := time.Unix(1000, 0)
	issuer, err := NewSandboxGrantIssuer("key-1", key, []string{"sandbox-prod"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"valid", "scope", "audience", "stale-fence", "cancelled", "signature", "capability", "disabled"} {
		t.Run(name, func(t *testing.T) {
			calls := []string{}
			lease := validLease()
			command := &runtimev1.WorkerCommandV1{CommandId: lease.Fence.CommandID, ExecutionId: lease.Fence.ExecutionID, Generation: lease.Fence.Generation, TenantId: "tenant", ResourceProjectId: "2", CommandType: runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION}
			identity := identityForFence(lease.Fence)
			identity.TenantId = "tenant"
			identity.ResourceProjectId = "2"
			request := &runtimev1.AuthorizeSandboxJobRequestV1{Identity: identity, Fence: fenceProto(lease.Fence), ActivationId: "node/activation-1", RequestDigest: bytes.Repeat([]byte{3}, 32), Audience: "sandbox-prod"}
			verifier := sandboxVerifierStub{command: command}
			config := testControlServerConfig()
			config.SandboxGrants = issuer
			expected := codes.PermissionDenied
			switch name {
			case "scope":
				request.Identity.TenantId = "other"
			case "audience":
				request.Audience = "other"
			case "stale-fence":
				request.Fence.LeaseEpoch++
			case "cancelled":
				lease.DesiredState = "CANCELLED"
			case "signature":
				verifier.err = ErrCommandAuthentication
			case "capability":
				command.CommandType = runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_TOOLKIT_CALL_TOOL
			case "disabled":
				config.SandboxGrants = nil
				expected = codes.Unimplemented
			}
			server, err := NewServer(config, workloadAuthorizerStub{calls: &calls}, verifier, claimControllerStub{calls: &calls, lease: lease}, inputResolverStub{calls: &calls, manifest: validManifest()}, &settlementControllerStub{calls: &calls})
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.AuthorizeSandboxJob(context.Background(), request)
			if name != "valid" {
				if status.Code(err) != expected {
					t.Fatalf("got %v; expected %v", err, expected)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			grant := response.GetGrant()
			if grant == nil {
				t.Fatal("missing grant")
			}
			input := append([]byte(sandboxGrantDomain), make([]byte, 8)...)
			binary.BigEndian.PutUint64(input[len(sandboxGrantDomain):], uint64(len(grant.ClaimsBytes)))
			input = append(input, grant.ClaimsBytes...)
			if !ed25519.Verify(key.Public().(ed25519.PublicKey), input, grant.Signature) {
				t.Fatal("invalid signature")
			}
			claims := &runtimev1.SandboxJobGrantClaimsV1{}
			if err := proto.Unmarshal(grant.ClaimsBytes, claims); err != nil {
				t.Fatal(err)
			}
			if claims.TenantId != "tenant" || claims.ProjectId != 2 || claims.SubmitterWorkloadIdentity != lease.Fence.WorkloadIdentity || claims.Audience != "sandbox-prod" || claims.ExpiresAtUnixMillis-claims.IssuedAtUnixMillis != 30000 || !bytes.Equal(claims.RequestDigest, request.RequestDigest) {
				t.Fatal("incorrect grant bindings")
			}
		})
	}
}
