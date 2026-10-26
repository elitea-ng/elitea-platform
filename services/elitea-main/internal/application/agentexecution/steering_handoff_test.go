package agentexecution

import (
	"context"
	"errors"
	"testing"
	"time"
)

type handOffStoreStub struct {
	handOffs []ServerHandOff
	effects  []SteeringEffect
	started  []string
	dropped  []string
}

func (s *handOffStoreStub) TakeServerHandOffs(context.Context, time.Duration, time.Duration, int) ([]ServerHandOff, []SteeringEffect, error) {
	return s.handOffs, s.effects, nil
}

func (s *handOffStoreStub) MarkServerHandOffStarted(_ context.Context, handOff ServerHandOff) error {
	s.started = append(s.started, handOff.QuestionID)
	return nil
}

func (s *handOffStoreStub) DropServerHandOff(_ context.Context, handOff ServerHandOff) error {
	s.dropped = append(s.dropped, handOff.QuestionID)
	return nil
}

type handOffStarterStub struct {
	applications []CurrentApplicationStartRequest
	adhoc        []CurrentAdhocStartRequest
	err          map[string]error
}

func (s *handOffStarterStub) StartCurrentApplication(_ context.Context, request CurrentApplicationStartRequest) (CurrentApplicationStartOutcome, error) {
	s.applications = append(s.applications, request)
	return CurrentApplicationStartOutcome{}, s.err[request.QuestionID]
}

func (s *handOffStarterStub) StartCurrentAdhoc(_ context.Context, request CurrentAdhocStartRequest) (CurrentApplicationStartOutcome, error) {
	s.adhoc = append(s.adhoc, request)
	return CurrentApplicationStartOutcome{}, s.err[request.QuestionID]
}

func TestSteeringHandOffRoutineStartsDropsAndRetries(t *testing.T) {
	store := &handOffStoreStub{handOffs: []ServerHandOff{
		{ProjectID: 7, AuthorUserID: 11, ConversationUUID: "c", ParticipantID: 3, QuestionID: "app", Texts: []string{"one", "two"}},
		{ProjectID: 7, AuthorUserID: 11, ConversationUUID: "c", ParticipantID: 4, Adhoc: true, QuestionID: "adhoc", Texts: []string{"x"}},
		{ProjectID: 7, AuthorUserID: 12, QuestionID: "lost-permission", Texts: []string{"y"}},
		{ProjectID: 7, AuthorUserID: 11, QuestionID: "transient", Texts: []string{"z"}},
		{ProjectID: 7, AuthorUserID: 11, QuestionID: "invalid", Texts: []string{"w"}},
		{ProjectID: 8, AuthorUserID: 11, QuestionID: "unknown-permission", Texts: []string{"v"}},
	}}
	starter := &handOffStarterStub{err: map[string]error{
		"transient": errors.New("dispatch unavailable"),
		"invalid":   ErrInvalidCurrentAgentStart,
	}}
	mayStart := func(_ context.Context, projectID, userID int64) (bool, error) {
		if projectID == 8 {
			return false, errors.New("permission store unavailable")
		}
		return userID == 11, nil
	}
	store.effects = []SteeringEffect{{ProjectID: 7, ExecutionID: "newer", Cursor: 9}}
	var woken []SteeringEffect
	routine, err := NewSteeringHandOffRoutine(store, starter, mayStart, func(effects []SteeringEffect) { woken = append(woken, effects...) }, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := routine.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result != (SteeringHandOffResult{Started: 2, Dropped: 2, Retried: 2, Carried: 1}) || len(woken) != 1 || woken[0].ExecutionID != "newer" {
		t.Fatalf("result=%+v", result)
	}
	if len(starter.applications) != 3 || starter.applications[0].UserInput != "one\n\ntwo" ||
		starter.applications[0].ActorUserID != 11 || starter.applications[0].TargetParticipantID != 3 ||
		starter.applications[0].QuestionID != "app" || starter.applications[0].MCPTokens != nil {
		t.Fatalf("applications=%+v", starter.applications)
	}
	if len(starter.adhoc) != 1 || string(starter.adhoc[0].LLMSettings) != "{}" || starter.adhoc[0].MCPTokens != nil {
		t.Fatalf("adhoc=%+v", starter.adhoc)
	}
	if len(store.started) != 2 || len(store.dropped) != 2 || store.dropped[0] != "lost-permission" || store.dropped[1] != "invalid" {
		t.Fatalf("started=%v dropped=%v", store.started, store.dropped)
	}
}
