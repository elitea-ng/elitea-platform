package output

import (
	"context"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/nodeevent"
	"google.golang.org/protobuf/proto"
)

type nodeEventIngestorStub struct {
	frames []outputapp.NodeEventFrame
	err    error
}

func (s *nodeEventIngestorStub) IngestNodeEvent(_ context.Context, frame outputapp.NodeEventFrame) (outputapp.ProjectionOutcome, error) {
	s.frames = append(s.frames, frame)
	return outputapp.ProjectionOutcome{Inserted: true, Cursor: 11, CommittedSequence: frame.Sequence}, s.err
}

func TestOutputServerAcceptsNodeEventThenContiguousTerminal(t *testing.T) {
	progress := validNodeEventWireFrame(t)
	terminal := validIndexWireFrame(t)
	terminal.Sequence = 2
	terminal.EventId = terminal.GetIdentity().GetCommandId() + ":2"
	terminal.ClaimHandoffWatermark = 0
	terminal.SettlementProposal.TerminalSequence = 2
	terminal.SettlementProposal.TerminalEventId = terminal.EventId

	authorizer := &outputAuthorizerStub{}
	validations := &validationIngestorStub{}
	failures := &failureIngestorStub{}
	indexes := &indexIngestorStub{}
	nodes := &nodeEventIngestorStub{}
	server, err := NewServerWithIndexIngestAndNodeEvents(ServerConfig{
		OutputSchemaRevision: "elitea.runtime.execution-output.v1",
		MaxFrameBytes:        64 * 1024,
		CreditFrames:         8,
		CreditBytes:          64 * 1024,
	}, authorizer, validations, failures, indexes, nodes)
	if err != nil {
		t.Fatal(err)
	}
	stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{progress, terminal}}
	if err := server.Publish(stream); err != nil {
		t.Fatal(err)
	}
	if authorizer.calls != 2 || stream.index != 2 || len(stream.acks) != 3 {
		t.Fatalf("progress stream did not continue through terminal: auth=%d recv=%d acks=%d", authorizer.calls, stream.index, len(stream.acks))
	}
	if stream.acks[1].GetCommittedContiguousSequence() != 1 || stream.acks[2].GetCommittedContiguousSequence() != 2 || stream.acks[1].GetRejection() != nil || stream.acks[2].GetRejection() != nil {
		t.Fatalf("unexpected contiguous acknowledgements: %v", stream.acks)
	}
	if len(nodes.frames) != 1 || len(indexes.frames) != 1 || indexes.frames[0].Sequence != 2 || len(validations.frames) != 0 || len(failures.frames) != 0 {
		t.Fatalf("output routing changed: nodes=%d validations=%d indexes=%d failures=%d", len(nodes.frames), len(validations.frames), len(indexes.frames), len(failures.frames))
	}
	wantJSON, err := nodeevent.EncodeCurrentJSON(progress.GetNodeEvent())
	if err != nil {
		t.Fatal(err)
	}
	if string(nodes.frames[0].BrowserData) != string(wantJSON) {
		t.Fatalf("current UI JSON changed: got %s want %s", nodes.frames[0].BrowserData, wantJSON)
	}
}

func TestOutputServerRejectsTerminalOrSettlementNodeEvent(t *testing.T) {
	for _, name := range []string{"terminal", "settlement"} {
		t.Run(name, func(t *testing.T) {
			frame := validNodeEventWireFrame(t)
			if name == "terminal" {
				frame.Terminal = true
			} else {
				frame.SettlementProposal = proto.Clone(readCorpusFrame(t, "valid").GetSettlementProposal()).(*runtimev1.SettlementProposalV1)
			}
			nodes := &nodeEventIngestorStub{}
			server, err := NewServerWithIndexIngestAndNodeEvents(ServerConfig{
				OutputSchemaRevision: "elitea.runtime.execution-output.v1",
				MaxFrameBytes:        64 * 1024,
				CreditFrames:         8,
				CreditBytes:          64 * 1024,
			}, &outputAuthorizerStub{}, &validationIngestorStub{}, &failureIngestorStub{}, &indexIngestorStub{}, nodes)
			if err != nil {
				t.Fatal(err)
			}
			stream := &outputStreamStub{context: context.Background(), frames: []*runtimev1.ExecutionOutputFrameV1{frame}}
			if err := server.Publish(stream); err != nil {
				t.Fatal(err)
			}
			if len(stream.acks) != 2 || stream.acks[1].GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION || len(nodes.frames) != 0 {
				t.Fatalf("invalid node event reached ingestion: acks=%v frames=%d", stream.acks, len(nodes.frames))
			}
		})
	}
}

func validNodeEventWireFrame(t *testing.T) *runtimev1.ExecutionOutputFrameV1 {
	t.Helper()
	base := validIndexWireFrame(t)
	streamID := "index-stream-1"
	createdAt := "2026-07-22T12:00:00Z"
	payload := &runtimev1.NodeEventV1{
		Type:             "agent_index_data_status",
		StreamId:         &streamID,
		Content:          []byte(`{"state":"in_progress"}`),
		ResponseMetadata: []byte(`{"index_name":"docs"}`),
		References:       []byte(`[]`),
		CreatedAt:        &createdAt,
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return &runtimev1.ExecutionOutputFrameV1{
		OutputSchemaRevision:  base.GetOutputSchemaRevision(),
		StreamId:              base.GetStreamId(),
		Identity:              proto.Clone(base.GetIdentity()).(*runtimev1.ExecutionIdentityV1),
		Fence:                 proto.Clone(base.GetFence()).(*runtimev1.ExecutionFenceV1),
		LogicalOutputId:       outputapp.NodeEventLogicalOutputID(base.GetIdentity().GetExecutionId(), 1),
		EventId:               base.GetIdentity().GetCommandId() + ":1",
		Sequence:              1,
		ClaimHandoffWatermark: 0,
		EventType:             runtimev1.ExecutionOutputEventTypeV1_EXECUTION_OUTPUT_EVENT_TYPE_V1_NODE_EVENT,
		OccurredAtUnixMillis:  base.GetOccurredAtUnixMillis(),
		PayloadDigest:         testOutputDigest(encoded),
		Terminal:              false,
		Payload:               &runtimev1.ExecutionOutputFrameV1_NodeEvent{NodeEvent: payload},
	}
}
