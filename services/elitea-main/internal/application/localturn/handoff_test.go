package localturn

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

type handFakeStore struct {
	view    HandView
	ids     []string
	write   HandWrite
	called  bool
	skipped []string
}

func (s *handFakeStore) HandLocalFloor(_ context.Context, _ HandScope, decide func(HandView) HandWrite) ([]string, error) {
	s.called = true
	s.write = decide(s.view)
	if len(s.write.Queue) > 0 && s.ids == nil {
		for range s.write.Queue {
			s.ids = append(s.ids, "5d6f0c1e-4c2a-4c1b-9a51-0c2d1d6b2a29")
		}
	}
	return s.ids, nil
}

func (s *handFakeStore) SkipLocalFloorHands(_ context.Context, _, _ int64, _ string, handIDs []string, _ string) ([]string, error) {
	s.skipped = handIDs
	return handIDs, nil
}

func handRequest() HandRequest {
	return HandRequest{ProjectID: 1, ActorUserID: 7, Credential: Credential{TokenID: "t"},
		ExecutionID: strings.Repeat("a", 32), ParticipantIDs: []int64{2}, Message: " over to you "}
}

func TestHandsQueueAndName(t *testing.T) {
	store := &handFakeStore{view: HandView{From: 1, Exchange: turntaking.Exchange{First: 1},
		Agents: []turntaking.Agent{{ParticipantID: 1, Name: "Ada"}, {ParticipantID: 2, Name: "Bo"}}}}
	hands, err := NewHands(store, commitFakePolicy{allowed: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := hands.Hand(context.Background(), handRequest())
	if err != nil || len(result.HandedTo) != 1 || result.HandedTo[0].Name != "Bo" || result.HandedTo[0].HandID == "" {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if store.write.Message != "over to you" || !slices.Equal(store.write.Queue, []int64{2}) {
		t.Fatalf("write = %+v", store.write)
	}
}

// The host's consults count when they are more than the committed ones.
func TestHandsCountTheLongerConsults(t *testing.T) {
	store := &handFakeStore{view: HandView{From: 1, Exchange: turntaking.Exchange{First: 1},
		Agents: []turntaking.Agent{{ParticipantID: 1}, {ParticipantID: 2}}}}
	hands, _ := NewHands(store, commitFakePolicy{allowed: true})
	request := handRequest()
	request.Consults = []turntaking.Turn{{From: 1, To: 2}, {From: 2, To: 1}, {From: 1, To: 2}}
	result, err := hands.Hand(context.Background(), request)
	if err != nil || result.Stopped != turntaking.StopPairs || store.write.Stop != turntaking.StopPairs || store.write.StopText == "" {
		t.Fatalf("result = %+v, write %+v, %v", result, store.write, err)
	}
	// The committed ones count when the host sends fewer.
	store = &handFakeStore{view: HandView{From: 1, Exchange: turntaking.Exchange{First: 1, Consults: request.Consults},
		Agents: []turntaking.Agent{{ParticipantID: 1}, {ParticipantID: 2}}}}
	hands, _ = NewHands(store, commitFakePolicy{allowed: true})
	if result, _ := hands.Hand(context.Background(), handRequest()); result.Stopped != turntaking.StopPairs {
		t.Fatalf("result = %+v", result)
	}
}

func TestHandsRefuseBeforeTheStore(t *testing.T) {
	cases := map[string]func(*HandRequest){
		"no project":     func(r *HandRequest) { r.ProjectID = 0 },
		"no token":       func(r *HandRequest) { r.Credential.TokenID = "" },
		"bad execution":  func(r *HandRequest) { r.ExecutionID = "x" },
		"no agent":       func(r *HandRequest) { r.ParticipantIDs = nil },
		"five agents":    func(r *HandRequest) { r.ParticipantIDs = []int64{2, 3, 4, 5, 6} },
		"negative agent": func(r *HandRequest) { r.ParticipantIDs = []int64{-1} },
		"self consult":   func(r *HandRequest) { r.Consults = []turntaking.Turn{{From: 2, To: 2}} },
		"nine consults":  func(r *HandRequest) { r.Consults = make([]turntaking.Turn, 9) },
		"a huge message": func(r *HandRequest) { r.Message = strings.Repeat("x", 4*turntaking.MaxHandMessageBytes+1) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			store := &handFakeStore{}
			hands, _ := NewHands(store, commitFakePolicy{allowed: true})
			request := handRequest()
			mutate(&request)
			if _, err := hands.Hand(context.Background(), request); !errors.Is(err, ErrInvalid) || store.called {
				t.Fatalf("err = %v, store called %v", err, store.called)
			}
		})
	}
	store := &handFakeStore{}
	hands, _ := NewHands(store, commitFakePolicy{allowed: false})
	if _, err := hands.Hand(context.Background(), handRequest()); !errors.Is(err, ErrLocalWorkDisabled) || store.called {
		t.Fatalf("disabled = %v", err)
	}
	hands, _ = NewHands(store, commitFakePolicy{err: errors.New("down")})
	if _, err := hands.Hand(context.Background(), handRequest()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreadable policy = %v", err)
	}
}

func TestHandsSkipValidates(t *testing.T) {
	store := &handFakeStore{}
	hands, _ := NewHands(store, commitFakePolicy{allowed: true})
	question := "6a1e4f9e-8d0c-4c3b-b1a2-3f4e5d6c7b8a"
	hand := "5d6f0c1e-4c2a-4c1b-9a51-0c2d1d6b2a29"
	if got, err := hands.Skip(context.Background(), 1, 7, question, []string{hand}, SkipSkippedByPerson); err != nil || len(got) != 1 {
		t.Fatalf("skip = %v, %v", got, err)
	}
	for name, call := range map[string]func() error{
		"bad code": func() error {
			_, err := hands.Skip(context.Background(), 1, 7, question, []string{hand}, SkipSuperseded)
			return err
		},
		"duplicate": func() error {
			_, err := hands.Skip(context.Background(), 1, 7, question, []string{hand, hand}, SkipStoppedByPerson)
			return err
		},
		"none": func() error {
			_, err := hands.Skip(context.Background(), 1, 7, question, nil, SkipStoppedByPerson)
			return err
		},
		"bad question": func() error {
			_, err := hands.Skip(context.Background(), 1, 7, "x", []string{hand}, SkipStoppedByPerson)
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestValidFloorTurnPositionOrHand(t *testing.T) {
	question := "6a1e4f9e-8d0c-4c3b-b1a2-3f4e5d6c7b8a"
	hand := "5d6f0c1e-4c2a-4c1b-9a51-0c2d1d6b2a29"
	request := func(turn FloorTurnRef) StartRequest {
		return StartRequest{QuestionID: "7b2f5a0d-9e1c-4d4b-8c3a-4f5e6d7c8b9a", ParticipantID: 2, FloorTurn: &turn}
	}
	for name, c := range map[string]struct {
		turn FloorTurnRef
		ok   bool
	}{
		"a position":         {FloorTurnRef{QuestionID: question, Position: 1}, true},
		"a hand-off":         {FloorTurnRef{QuestionID: question, HandID: hand}, true},
		"both":               {FloorTurnRef{QuestionID: question, Position: 1, HandID: hand}, false},
		"neither":            {FloorTurnRef{QuestionID: question}, false},
		"a malformed hand":   {FloorTurnRef{QuestionID: question, HandID: "x"}, false},
		"position 0 counted": {FloorTurnRef{QuestionID: question, Position: 0}, false},
	} {
		if got := validFloorTurn(request(c.turn)); got != c.ok {
			t.Errorf("%s: valid = %v, want %v", name, got, c.ok)
		}
	}
}

func TestValidConsultsAndTheDigest(t *testing.T) {
	ok := []Consult{{ParticipantID: 2, Question: "q", Reply: ""}}
	if !validConsults(ok) {
		t.Fatal("an empty reply is a reply")
	}
	for name, consults := range map[string][]Consult{
		"four":           make([]Consult, 4),
		"no agent":       {{Question: "q"}},
		"empty question": {{ParticipantID: 2}},
		"long reply":     {{ParticipantID: 2, Question: "q", Reply: strings.Repeat("x", MaxConsultReplyBytes+1)}},
		"long question":  {{ParticipantID: 2, Question: strings.Repeat("x", MaxConsultQuestionBytes+1)}},
	} {
		if validConsults(consults) {
			t.Errorf("%s: accepted", name)
		}
	}
	plain := CommitRequest{ExecutionID: "x", UserMessage: "q", AssistantMessage: "a"}
	with := plain
	with.Consults = ok
	a, _ := commitDigest(plain)
	b, _ := commitDigest(with)
	if a == b {
		t.Fatal("the digest must see consults")
	}
}
