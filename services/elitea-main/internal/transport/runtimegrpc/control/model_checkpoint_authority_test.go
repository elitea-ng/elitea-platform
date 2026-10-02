package control

import (
	"context"
	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"testing"
)

type checkpointControllerStub struct {
	claimControllerStub
	digest runtimedomain.Digest
}

func (s checkpointControllerStub) AuthorizeAgentModelCheckpoint(ctx context.Context, fence runtimedomain.Fence, digest runtimedomain.Digest) (executionapp.AuthorizeInvocationDisposition, error) {
	if digest != s.digest {
		return "", executionapp.ErrInvalidClaim
	}
	return s.AuthorizeInvocation(ctx, fence)
}
func TestModelCheckpointAuthorizationValidatesWireEvidence(t *testing.T) {
	calls := []string{}
	lease := validLease()
	digest := runtimedomain.SHA256([]byte("checkpoint"))
	controller := checkpointControllerStub{claimControllerStub: claimControllerStub{calls: &calls, lease: lease}, digest: digest}
	server, err := NewServer(testControlServerConfig(), workloadAuthorizerStub{calls: &calls}, newTestVerifier(t), controller, inputResolverStub{calls: &calls, manifest: validManifest()}, &settlementControllerStub{calls: &calls})
	if err != nil {
		t.Fatal(err)
	}
	request := &runtimev1.AuthorizeAgentModelCheckpointRequestV1{
		Identity: &runtimev1.ExecutionIdentityV1{CommandId: lease.Fence.CommandID, ExecutionId: lease.Fence.ExecutionID, Generation: lease.Fence.Generation}, Fence: fenceProto(lease.Fence), CheckpointDigest: digestProto(digest),
	}
	response, err := server.AuthorizeAgentModelCheckpoint(context.Background(), request)
	if err != nil || response.GetRejection() != nil || response.GetDisposition() != runtimev1.AuthorizeInvocationDispositionV1_AUTHORIZE_INVOCATION_DISPOSITION_V1_AUTHORIZED_NOW {
		t.Fatalf("valid evidence rejected: %v %v", response, err)
	}
	calls = nil
	request.CheckpointDigest = nil
	response, err = server.AuthorizeAgentModelCheckpoint(context.Background(), request)
	if err != nil || response.GetRejection() == nil || len(calls) != 1 || calls[0] != "authorize-peer" {
		t.Fatalf("malformed digest reached authority: %v %v %v", response, err, calls)
	}
	request.CheckpointDigest = digestProto(digest)
	request.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
	calls = nil
	response, err = server.AuthorizeAgentModelCheckpoint(context.Background(), request)
	if err != nil || response.GetRejection() == nil || len(calls) != 0 {
		t.Fatalf("unknown fields accepted: %v %v", response, err)
	}
}
