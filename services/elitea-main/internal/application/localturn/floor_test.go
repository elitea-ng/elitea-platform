package localturn

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

// floorFakeStore records the start it was given and answers the routed
// order it was asked to store, as the repository does.
type floorFakeStore struct {
	commitFakeStore
	// reader is the floor the start's transaction reads (Admit).
	reader *floorFakeReader
	starts []StartRecord
}

func (s *floorFakeStore) StartLocalTurn(ctx context.Context, record StartRecord) (StartedTurn, error) {
	record, err := record.Admit(ctx, func() (ConversationFloor, error) {
		s.reader.calls++
		return s.reader.floor, s.reader.err
	})
	if err != nil {
		return StartedTurn{}, err
	}
	s.starts = append(s.starts, record)
	turn := StartedTurn{
		ExecutionID: record.ExecutionID, ParticipantID: record.ParticipantID, Created: true,
		FloorTargets: append([]int64(nil), record.FloorTargets...), Executor: ExecutorLocal,
	}
	if len(record.FloorTargets) > 0 {
		turn.FloorQuestionID = record.QuestionID
	}
	if record.FloorTurn != nil {
		turn.FloorQuestionID = record.FloorTurn.QuestionID
		turn.FloorTargets = []int64{11, 12, 13}
	}
	return record.Admitted(turn), nil
}

type floorFakeReader struct {
	floor ConversationFloor
	err   error
	calls int
}

func floorService(t *testing.T, store *floorFakeStore, reader *floorFakeReader) *Service {
	t.Helper()
	return floorServiceWith(t, store, reader, commitFakeMemories{})
}

func floorServiceWith(t *testing.T, store *floorFakeStore, reader *floorFakeReader, memories agentexecutionapp.CurrentMemoryRecallResolver) *Service {
	t.Helper()
	service, err := NewService(store, commitFakePolicy{allowed: true}, memories, commitFakeAudit{},
		func() (string, error) { return strings.Repeat("c", 32), nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.reader = reader
	if reader != nil {
		service.WithFloor()
	}
	return service
}

// countingMemories counts the recalls a start asks for.
type countingMemories struct {
	commitFakeMemories
	recalls *int
}

func (m countingMemories) ResolveCurrentMemoryRecall(ctx context.Context, projectID, userID int64, input string) (agentexecutionapp.CurrentMemoryRecall, error) {
	*m.recalls++
	return agentexecutionapp.CurrentMemoryRecall{Text: "recalled", Count: 2, IDs: []string{"a", "b"}}, nil
}

// The routing comes first, inside the start's transaction (issue 1888): a
// refused routing pays for no recall, and an admitted start stores the
// recall's count and answers its text.
func TestStartRoutesBeforeTheRecall(t *testing.T) {
	recalls := 0
	memories := countingMemories{recalls: &recalls}
	reader := &floorFakeReader{floor: ConversationFloor{Settings: turntaking.Default, Agents: agents(11, 12)}}
	refused := floorStart()
	refused.ParticipantID, refused.AddressedTo = 11, []int64{11, 99}
	store := &floorFakeStore{}
	if _, err := floorServiceWith(t, store, reader, memories).Start(context.Background(), refused); !errors.Is(err, ErrAddressee) {
		t.Fatalf("Start = %v, want ErrAddressee", err)
	}
	if recalls != 0 || reader.calls != 1 {
		t.Fatalf("recalls %d, floor reads %d; want 0 and 1", recalls, reader.calls)
	}
	admitted := floorStart()
	admitted.ParticipantID, admitted.AddressedTo = 11, []int64{12}
	outcome, err := floorServiceWith(t, store, reader, memories).Start(context.Background(), admitted)
	if err != nil {
		t.Fatal(err)
	}
	if recalls != 1 || reader.calls != 2 {
		t.Fatalf("recalls %d, floor reads %d; want 1 and 2", recalls, reader.calls)
	}
	if got := store.starts[0]; got.ParticipantID != 12 || got.MemoriesUsed != 2 {
		t.Fatalf("stored %+v", got)
	}
	if outcome.Recall.Text != "recalled" || outcome.Recall.Count != 2 || outcome.Floor == nil {
		t.Fatalf("outcome %+v", outcome)
	}
	// Without the floor nothing is read, and the recall still runs.
	plain := floorStart()
	plain.ParticipantID = 11
	unrouted := &floorFakeStore{}
	if _, err := floorServiceWith(t, unrouted, nil, memories).Start(context.Background(), plain); err != nil {
		t.Fatal(err)
	}
	if recalls != 2 || unrouted.starts[0].MemoriesUsed != 2 {
		t.Fatalf("recalls %d, stored %+v", recalls, unrouted.starts[0])
	}
}

func floorStart() StartRequest {
	return StartRequest{
		ProjectID: 1, ActorUserID: 2, TokenID: "70",
		ConversationUUID: "00000000-0000-4000-8000-000000000001",
		QuestionID:       "00000000-0000-4000-8000-000000000002",
		UserInput:        "review this",
	}
}

func agents(ids ...int64) []FloorAgent {
	list := make([]FloorAgent, 0, len(ids))
	for _, id := range ids {
		list = append(list, FloorAgent{ParticipantID: id, Executor: ExecutorLocal})
	}
	return list
}

// A person's message in a thread with several agents is routed by the
// conversation's floor (ADR-0050 decision 7f): the same order a cloud
// message gets (turntaking.Route), the first answering now.
func TestStartRoutesByTheFloor(t *testing.T) {
	lead := turntaking.Settings{Policy: turntaking.PolicyLead, Lead: 12}
	for name, test := range map[string]struct {
		settings    turntaking.Settings
		agents      []FloorAgent
		participant int64
		addressed   []int64
		wantFirst   int64
		wantTargets []int64
		wantErr     error
	}{
		"one agent is not routed": {
			settings: turntaking.Default, agents: agents(11), participant: 11, wantFirst: 11,
		},
		"the model participant of a thread without agents": {
			settings: turntaking.Default, agents: nil, participant: 0, wantFirst: 0,
		},
		"addressed agents answer in order, the composer's target first": {
			settings: turntaking.Default, agents: agents(11, 12, 13), participant: 13,
			addressed: []int64{12, 13}, wantFirst: 13, wantTargets: []int64{13, 12},
		},
		"an address without the composer's target wins over it": {
			settings: turntaking.Default, agents: agents(11, 12, 13), participant: 11,
			addressed: []int64{13, 12}, wantFirst: 13, wantTargets: []int64{13, 12},
		},
		"no address goes to the lead": {
			settings: lead, agents: agents(11, 12), participant: 11, wantFirst: 12, wantTargets: []int64{12},
		},
		"no address and no lead goes to the composer's target": {
			settings: turntaking.Default, agents: agents(11, 12), participant: 11, wantFirst: 11, wantTargets: []int64{11},
		},
		"no address, no lead and no target is refused": {
			settings: turntaking.Default, agents: agents(11, 12), wantErr: ErrAgentTargetRequired,
		},
		"an addressee outside the thread is refused": {
			settings: turntaking.Default, agents: agents(11, 12), participant: 11,
			addressed: []int64{11, 99}, wantErr: ErrAddressee,
		},
		"an addressee outside a one-agent thread is refused": {
			settings: turntaking.Default, agents: agents(11), participant: 11,
			addressed: []int64{99}, wantErr: ErrAddressee,
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := &floorFakeStore{}
			reader := &floorFakeReader{floor: ConversationFloor{Settings: test.settings, Agents: test.agents}}
			request := floorStart()
			request.ParticipantID, request.AddressedTo = test.participant, test.addressed
			outcome, err := floorService(t, store, reader).Start(context.Background(), request)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Start = %v, want %v", err, test.wantErr)
			}
			if test.wantErr != nil {
				if len(store.starts) != 0 {
					t.Fatal("a refused start reached the store")
				}
				return
			}
			got := store.starts[0]
			if got.ParticipantID != test.wantFirst || !reflect.DeepEqual(got.FloorTargets, test.wantTargets) {
				t.Fatalf("stored participant %d targets %v, want %d %v",
					got.ParticipantID, got.FloorTargets, test.wantFirst, test.wantTargets)
			}
			if test.wantTargets == nil {
				if outcome.Floor != nil {
					t.Fatalf("floor = %+v, want none", outcome.Floor)
				}
				return
			}
			if outcome.Floor == nil || outcome.Floor.Position != 0 || outcome.Floor.QuestionID != request.QuestionID ||
				!reflect.DeepEqual(outcome.Floor.Targets, test.wantTargets) {
				t.Fatalf("floor = %+v", outcome.Floor)
			}
		})
	}
}

// A later turn of a routed message names the message and its position; it
// carries no address or grant of its own, and is checked against the
// message by the store.
func TestStartAdmitsALaterFloorTurn(t *testing.T) {
	reader := &floorFakeReader{floor: ConversationFloor{Settings: turntaking.Default, Agents: agents(11, 12, 13)}}
	later := floorStart()
	later.QuestionID = "00000000-0000-4000-8000-000000000003"
	later.ParticipantID = 12
	later.FloorTurn = &FloorTurnRef{QuestionID: "00000000-0000-4000-8000-000000000002", Position: 1}

	store := &floorFakeStore{}
	outcome, err := floorService(t, store, reader).Start(context.Background(), later)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.starts[0]; got.FloorTurn == nil || got.FloorTurn.Position != 1 || got.ParticipantID != 12 {
		t.Fatalf("stored %+v", got)
	}
	if outcome.Floor == nil || outcome.Floor.Position != 1 || outcome.Floor.QuestionID != later.FloorTurn.QuestionID {
		t.Fatalf("floor = %+v", outcome.Floor)
	}

	for name, mutate := range map[string]func(*StartRequest){
		"with an address":      func(r *StartRequest) { r.AddressedTo = []int64{12} },
		"at position 0":        func(r *StartRequest) { r.FloorTurn.Position = 0 },
		"for its own question": func(r *StartRequest) { r.FloorTurn.QuestionID = r.QuestionID },
		"for the model":        func(r *StartRequest) { r.ParticipantID = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			request := later
			turn := *later.FloorTurn
			request.FloorTurn = &turn
			mutate(&request)
			if _, err := floorService(t, &floorFakeStore{}, reader).Start(context.Background(), request); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Start = %v, want ErrInvalid", err)
			}
		})
	}
	t.Run("for an agent outside the thread", func(t *testing.T) {
		request := later
		request.ParticipantID = 99
		if _, err := floorService(t, &floorFakeStore{}, reader).Start(context.Background(), request); !errors.Is(err, ErrFloorTurn) {
			t.Fatalf("Start = %v, want ErrFloorTurn", err)
		}
	})
}

// Without the floor reader a start that addresses agents is refused rather
// than silently answered by one; an unreadable floor refuses the start.
func TestStartWithoutAReadableFloor(t *testing.T) {
	request := floorStart()
	request.ParticipantID, request.AddressedTo = 11, []int64{11, 12}
	if _, err := floorService(t, &floorFakeStore{}, nil).Start(context.Background(), request); !errors.Is(err, ErrAddressee) {
		t.Fatalf("no reader: %v, want ErrAddressee", err)
	}
	plain := floorStart()
	plain.ParticipantID = 11
	if _, err := floorService(t, &floorFakeStore{}, nil).Start(context.Background(), plain); err != nil {
		t.Fatalf("no reader, no address: %v", err)
	}
	broken := &floorFakeReader{err: errors.New("db down")}
	if _, err := floorService(t, &floorFakeStore{}, broken).Start(context.Background(), plain); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreadable floor: %v, want ErrUnavailable", err)
	}
	missing := &floorFakeReader{err: ErrNotFound}
	if _, err := floorService(t, &floorFakeStore{}, missing).Start(context.Background(), plain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing conversation: %v, want ErrNotFound", err)
	}
}

// An unreadable floor is logged with its cause: the answer is a bare 503.
func TestAnUnreadableFloorIsLogged(t *testing.T) {
	var logs bytes.Buffer
	store := &floorFakeStore{reader: &floorFakeReader{err: errors.New("db down")}}
	service, err := NewService(store, commitFakePolicy{allowed: true}, commitFakeMemories{}, commitFakeAudit{},
		func() (string, error) { return strings.Repeat("c", 32), nil }, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	service.WithFloor()
	request := floorStart()
	request.ParticipantID = 11
	if _, err := service.Start(context.Background(), request); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreadable floor: %v, want ErrUnavailable", err)
	}
	if !strings.Contains(logs.String(), "floor unreadable") || !strings.Contains(logs.String(), "db down") {
		t.Fatalf("log = %q, want the floor's read error", logs.String())
	}
}
