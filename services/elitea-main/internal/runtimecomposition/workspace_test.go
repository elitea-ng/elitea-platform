package runtimecomposition

import (
	"bytes"
	"context"
	"io"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
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

type recordingLifecycle struct {
	fenceSeam
	finished []string
}

func (r *recordingLifecycle) FinishExecution(_ context.Context, executionID string) error {
	r.finished = append(r.finished, executionID)
	return nil
}

type scriptedAgentIngestor struct{ err error }

func (s scriptedAgentIngestor) IngestAgent(context.Context, outputapp.AgentExecutionFrame) (outputapp.ProjectionOutcome, error) {
	return outputapp.ProjectionOutcome{Inserted: true}, s.err
}

// A projected terminal result ends the cloud turn at once; a refused one
// does not (the settlement hook and the reconciler remain).
func TestFinishingIngestorEndsTheCloudTurnOnlyAfterAProjectedResult(t *testing.T) {
	lifecycle := &recordingLifecycle{}
	frame := outputapp.AgentExecutionFrame{}
	frame.Fence.ExecutionID = "exec-1"
	ok := finishingAgentExecutionIngestor{next: scriptedAgentIngestor{}, lifecycle: lifecycle}
	if _, err := ok.IngestAgent(context.Background(), frame); err != nil || len(lifecycle.finished) != 1 || lifecycle.finished[0] != "exec-1" {
		t.Fatalf("finished = %v %v", lifecycle.finished, err)
	}
	refused := finishingAgentExecutionIngestor{next: scriptedAgentIngestor{err: outputapp.ErrOutputCancelled}, lifecycle: lifecycle}
	if _, err := refused.IngestAgent(context.Background(), frame); err == nil || len(lifecycle.finished) != 1 {
		t.Fatalf("a refused projection finished the turn: %v", lifecycle.finished)
	}
}
