package output

import (
	"context"
	"errors"
	"testing"
	"time"

	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type nodeEventProjectorStub struct {
	frames []NodeEventFrame
	err    error
}

func (s *nodeEventProjectorStub) ProjectNodeEvent(_ context.Context, frame NodeEventFrame) (ProjectionOutcome, error) {
	s.frames = append(s.frames, frame)
	return ProjectionOutcome{Inserted: true, Cursor: 9, CommittedSequence: frame.Sequence}, s.err
}

func TestNodeEventServiceVerifiesFenceAndCopiesDurableInputs(t *testing.T) {
	frame := validNodeEventFrame()
	projector := &nodeEventProjectorStub{}
	service, err := NewNodeEventService(fenceVerifierStub{expected: &frame.Fence}, projector)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := service.IngestNodeEvent(context.Background(), frame)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Cursor != 9 || outcome.CommittedSequence != 1 || len(projector.frames) != 1 {
		t.Fatalf("unexpected node event outcome: %+v", outcome)
	}
	projected := projector.frames[0]
	frame.EncodedEvent[0] ^= 0xff
	frame.BrowserData[0] ^= 0xff
	if projected.EncodedEvent[0] == frame.EncodedEvent[0] || projected.BrowserData[0] == frame.BrowserData[0] {
		t.Fatal("projected node event aliases caller buffers")
	}
}

func TestNodeEventFrameRejectsNonCanonicalIdentityDigestAndWatermark(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*NodeEventFrame)
	}{
		{name: "event ID", mutate: func(frame *NodeEventFrame) { frame.EventID = "other:1" }},
		{name: "logical output", mutate: func(frame *NodeEventFrame) { frame.LogicalOutputID = "other" }},
		{name: "sequence gap identity", mutate: func(frame *NodeEventFrame) { frame.Sequence = 2 }},
		{name: "handoff ahead", mutate: func(frame *NodeEventFrame) { frame.ClaimHandoffWatermark = 1 }},
		{name: "payload digest", mutate: func(frame *NodeEventFrame) { frame.PayloadDigest[0] ^= 0xff }},
		{name: "browser JSON", mutate: func(frame *NodeEventFrame) { frame.BrowserData = []byte("not-json") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frame := validNodeEventFrame()
			test.mutate(&frame)
			if err := frame.Validate(); !errors.Is(err, ErrInvalidNodeEventOutput) {
				t.Fatalf("expected invalid node event, got %v", err)
			}
		})
	}
}

func TestNodeEventServiceRejectsStaleFenceBeforeProjection(t *testing.T) {
	frame := validNodeEventFrame()
	stale := frame.Fence
	stale.Token[0] ^= 0xff
	projector := &nodeEventProjectorStub{}
	service, err := NewNodeEventService(fenceVerifierStub{expected: &stale}, projector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.IngestNodeEvent(context.Background(), frame); !errors.Is(err, runtimedomain.ErrStaleFence) {
		t.Fatalf("expected stale fence, got %v", err)
	}
	if len(projector.frames) != 0 {
		t.Fatal("stale node event reached projection")
	}
}

func validNodeEventFrame() NodeEventFrame {
	fence := runtimedomain.Fence{
		CommandID:         "command-node-1",
		ExecutionID:       "execution-node-1",
		Generation:        1,
		WorkloadIdentity:  "spiffe://elitea.test/workload/indexer-1",
		WorkloadSessionID: "workload-node-1",
		ProducerID:        "indexer-1",
		ClaimAttempt:      1,
		LeaseEpoch:        1,
		Token:             runtimedomain.FenceToken(runtimedomain.SHA256([]byte("node-event-fence-token"))),
	}
	encoded := []byte("deterministic-node-event-protobuf")
	return NodeEventFrame{
		StreamID:            "execution-node-1:1",
		TenantID:            "tenant-1",
		ResourceProjectID:   "42",
		ProjectionProjectID: "42",
		WorkloadSessionID:   fence.WorkloadSessionID,
		ProducerID:          fence.ProducerID,
		EventID:             "command-node-1:1",
		LogicalOutputID:     NodeEventLogicalOutputID(fence.ExecutionID, 1),
		Sequence:            1,
		OccurredAt:          time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC),
		Fence:               fence,
		PayloadDigest:       runtimedomain.SHA256(encoded),
		EncodedEvent:        encoded,
		BrowserData:         []byte(`{"type":"agent_index_data_status"}`),
	}
}
