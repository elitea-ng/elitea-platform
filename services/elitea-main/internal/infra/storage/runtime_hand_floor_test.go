package storage

import (
	"context"
	"errors"
	"strings"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

// fakeHandFloorSink answers a fixed run and exchange and records the write.
type fakeHandFloorSink struct {
	run      HandFloorRun
	runErr   error
	view     FloorExchangeView
	writes   []FloorHandWrite
	applyErr error
}

func (f *fakeHandFloorSink) ResolveHandFloorRun(context.Context, int64, string, uint64) (HandFloorRun, error) {
	return f.run, f.runErr
}

func (f *fakeHandFloorSink) HandFloorInExchange(
	_ context.Context, _ HandFloorRun, decide func(FloorExchangeView) (FloorHandWrite, error),
) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	write, err := decide(f.view)
	if err != nil {
		return err
	}
	f.writes = append(f.writes, write)
	return nil
}

func handFloorService(t *testing.T, sink *fakeHandFloorSink) *RuntimeHandFloorService {
	t.Helper()
	authorizer := agentRuntimeContextAuthorizerFunc(func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
		return RuntimeContextAuthorization{ResourceProjectID: 7, ActorID: "3", ConversationID: "c-1"}, nil
	})
	service, err := NewRuntimeHandFloorService(authorizer, sink)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func handFloorSink() *fakeHandFloorSink {
	return &fakeHandFloorSink{
		run: HandFloorRun{ProjectID: 7, ExecutionID: "e-1", ConversationUUID: "c-1", ParticipantID: 21},
		view: FloorExchangeView{
			Exchange: turntaking.Exchange{First: 21},
			Agents: []agentexecutionapp.FloorAgent{
				{ParticipantID: 21, Name: "Ada"}, {ParticipantID: 22, Name: "Bo"}, {ParticipantID: 23, Name: "Cy"},
			},
		},
	}
}

var handFloorClaim = ContentClaim{ExecutionID: "e-1", Generation: 1}

func TestHandFloorQueuesTheHandedAgents(t *testing.T) {
	sink := handFloorSink()
	result, err := handFloorService(t, sink).Hand(context.Background(), handFloorClaim,
		RuntimeHandFloorRequest{ParticipantIDs: []int64{23, 22}, Message: "  please review  "})
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != RuntimeHandFloorSchemaVersion || result.Stopped != "" || len(result.HandedTo) != 2 ||
		result.HandedTo[0].ParticipantID != 23 || result.HandedTo[0].Name != "Cy" || result.Message == "" {
		t.Fatalf("result = %+v", result)
	}
	if len(sink.writes) != 1 || len(sink.writes[0].Queue) != 2 || sink.writes[0].Message != "please review" ||
		sink.writes[0].Stop != "" {
		t.Fatalf("writes = %+v", sink.writes)
	}
}

func TestHandFloorGuardStopsTheExchange(t *testing.T) {
	sink := handFloorSink()
	// Bo already took two turns for this message.
	sink.view.Exchange.Turns = []turntaking.Turn{{From: 21, To: 22}, {From: 23, To: 22}}
	result, err := handFloorService(t, sink).Hand(context.Background(), handFloorClaim,
		RuntimeHandFloorRequest{ParticipantIDs: []int64{22}, Message: "again"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stopped != turntaking.StopAgentTurns || len(result.HandedTo) != 0 ||
		!strings.Contains(result.Message, "Bo already took 2 turns") {
		t.Fatalf("result = %+v", result)
	}
	if len(sink.writes) != 1 || sink.writes[0].Stop != turntaking.StopAgentTurns || len(sink.writes[0].Queue) != 0 ||
		!strings.HasPrefix(sink.writes[0].StopText, "Loop guard stopped the exchange") {
		t.Fatalf("writes = %+v", sink.writes)
	}
}

func TestHandFloorInvalidCallHandsNothing(t *testing.T) {
	sink := handFloorSink()
	service := handFloorService(t, sink)
	for _, request := range []RuntimeHandFloorRequest{
		{ParticipantIDs: []int64{21}, Message: "me"},
		{ParticipantIDs: []int64{99}, Message: "who"},
		{ParticipantIDs: []int64{22}, Message: "   "},
		{ParticipantIDs: []int64{22}, Message: strings.Repeat("x", turntaking.MaxHandMessageBytes+1)},
	} {
		result, err := service.Hand(context.Background(), handFloorClaim, request)
		if err != nil {
			t.Fatal(err)
		}
		if result.Stopped != "" || len(result.HandedTo) != 0 || result.Message == "" {
			t.Fatalf("result = %+v", result)
		}
	}
	for _, write := range sink.writes {
		if len(write.Queue) != 0 || write.Stop != "" {
			t.Fatalf("an invalid call wrote %+v", write)
		}
	}
}

func TestHandFloorAlreadyStoppedExchange(t *testing.T) {
	sink := handFloorSink()
	sink.view.Stopped = turntaking.StopHops
	result, err := handFloorService(t, sink).Hand(context.Background(), handFloorClaim,
		RuntimeHandFloorRequest{ParticipantIDs: []int64{22}, Message: "go"})
	if err != nil || result.Stopped != turntaking.StopHops || len(sink.writes[0].Queue) != 0 || sink.writes[0].Stop != "" {
		t.Fatalf("result=%+v err=%v writes=%+v", result, err, sink.writes)
	}
}

func TestHandFloorRefusesARunWithoutAnExchangeOrAnotherConversation(t *testing.T) {
	sink := handFloorSink()
	sink.applyErr = ErrContentRejected
	if _, err := handFloorService(t, sink).Hand(context.Background(), handFloorClaim,
		RuntimeHandFloorRequest{ParticipantIDs: []int64{22}, Message: "go"}); !errors.Is(err, ErrContentRejected) {
		t.Fatalf("err = %v", err)
	}
	sink = handFloorSink()
	sink.run.ConversationUUID = "c-2"
	if _, err := handFloorService(t, sink).Hand(context.Background(), handFloorClaim,
		RuntimeHandFloorRequest{ParticipantIDs: []int64{22}, Message: "go"}); !errors.Is(err, ErrContentUnauthorized) {
		t.Fatalf("err = %v", err)
	}
	sink = handFloorSink()
	sink.applyErr = errors.New("db down")
	if _, err := handFloorService(t, sink).Hand(context.Background(), handFloorClaim,
		RuntimeHandFloorRequest{ParticipantIDs: []int64{22}, Message: "go"}); !errors.Is(err, ErrContentUnavailable) {
		t.Fatalf("err = %v", err)
	}
}
