package runtimecomposition

import (
	"bytes"
	"context"
	"io"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/grpc/metadata"
)

type workspaceTestAuthorizer struct{}

func (workspaceTestAuthorizer) AuthorizeWorkload(context.Context, string, string) (string, error) {
	return "spiffe://elitea/worker", nil
}

type workspaceTestClaims struct{}

func (workspaceTestClaims) VerifyActive(context.Context, runtimedomain.Fence) error { return nil }

type capturedFetch struct {
	frames []*runtimev1.WorkspaceFetchFrameV1
}

func (c *capturedFetch) Send(frame *runtimev1.WorkspaceFetchFrameV1) error {
	c.frames = append(c.frames, frame)
	return nil
}
func (*capturedFetch) SetHeader(metadata.MD) error  { return nil }
func (*capturedFetch) SendHeader(metadata.MD) error { return nil }
func (*capturedFetch) SetTrailer(metadata.MD)       {}
func (*capturedFetch) Context() context.Context     { return context.Background() }
func (*capturedFetch) SendMsg(any) error            { return nil }
func (*capturedFetch) RecvMsg(any) error            { return io.EOF }

// The composition root builds the workspace-store service even when the
// handoff service is not composed, and the service then answers "feature
// unavailable" instead of failing or reaching storage. A unit test of the
// service alone cannot see this wiring (unit-suite-blind-to-composition-root).
func TestComposedWorkspaceStoreWithoutAHandoffServiceIsFeatureUnavailable(t *testing.T) {
	server, err := newWorkspaceStoreServer(workspaceTestAuthorizer{}, workspaceTestClaims{}, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	if server.Available() {
		t.Fatal("no store is composed")
	}
	stream := &capturedFetch{}
	request := &runtimev1.WorkspaceFetchPlanRequestV1{Claim: &runtimev1.WorkspaceClaimV1{
		Identity:   &runtimev1.ExecutionIdentityV1{CommandId: "c", ExecutionId: "e", Generation: 1},
		Fence:      &runtimev1.ExecutionFenceV1{WorkloadSessionId: "s", ProducerId: "p", ClaimAttempt: 1, LeaseEpoch: 1, FenceToken: bytes.Repeat([]byte{1}, 32)},
		ClaimId:    "claim",
		ThreadUuid: "0b9c6f4e-9a7f-4d4e-8f0e-1d2c3b4a5f60",
		Epoch:      2,
	}}
	if err := server.WorkspaceFetchPlan(request, stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.frames) != 1 || stream.frames[0].GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY {
		t.Fatalf("got %v", stream.frames)
	}
}
