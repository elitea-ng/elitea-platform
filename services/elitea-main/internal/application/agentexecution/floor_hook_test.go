package agentexecution

import (
	"context"
	"errors"
	"testing"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

type floorSettleStoreStub struct {
	view    FloorSettleView
	found   bool
	skipped []string
}

func (stub *floorSettleStoreStub) FloorSettle(context.Context, string) (FloorSettleView, bool, error) {
	return stub.view, stub.found, nil
}

func (stub *floorSettleStoreStub) SkipFloorTurns(_ context.Context, _ FloorExchangeKey, code string) error {
	stub.skipped = append(stub.skipped, code)
	return nil
}

type floorStarterStub struct {
	requests []CurrentApplicationStartRequest
	started  map[string]bool
	err      error
}

func (stub *floorStarterStub) StartCurrentApplication(_ context.Context, request CurrentApplicationStartRequest) (CurrentApplicationStartOutcome, error) {
	stub.requests = append(stub.requests, request)
	if stub.err != nil {
		return CurrentApplicationStartOutcome{}, stub.err
	}
	if stub.started == nil {
		stub.started = map[string]bool{}
	}
	created := !stub.started[request.QuestionID]
	stub.started[request.QuestionID] = true
	return CurrentApplicationStartOutcome{ExecutionID: "e", Created: created}, nil
}

type floorChainsStub struct{ ended, refused []string }

func (stub *floorChainsStub) EndChain(_ context.Context, root, state, _ string) (bool, error) {
	stub.ended = append(stub.ended, root+":"+state)
	return true, nil
}

func (stub *floorChainsStub) EndRefusedChain(_ context.Context, root string, refusal *unattended.ChainLimitError) error {
	stub.refused = append(stub.refused, root+":"+refusal.Limit)
	return nil
}

func settleView(next *QueuedFloorTurn) FloorSettleView {
	return FloorSettleView{
		Exchange: FloorExchangeKey{
			ProjectID: 7, ConversationUUID: "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
			QuestionID: "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
		},
		OwnerUserID: 11, TriggerOrigin: "manual", Next: next,
	}
}

func newFloorHook(t *testing.T, store FloorSettleStore, starter FloorStarter, chains FloorChainEnder) *FloorSettleHook {
	t.Helper()
	hook, err := NewFloorSettleHook(store, starter, chains, nil)
	if err != nil {
		t.Fatal(err)
	}
	return hook
}

func TestFloorHookStartsTheNextQueuedTurn(t *testing.T) {
	question := FloorTurnQuestionID("ee92ccbd-3312-4c72-b20b-fddf224e7c0e", 1)
	store := &floorSettleStoreStub{found: true, view: settleView(&QueuedFloorTurn{
		Position: 1, TurnQuestionID: question, ToParticipantID: 22, FromParticipantID: 21, FromName: "Ada",
		Cause: turntaking.CauseAgentMention, Message: "check this",
	})}
	starter := &floorStarterStub{}
	hook := newFloorHook(t, store, starter, nil)
	if err := hook.Settle(context.Background(), "x", executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
	if len(starter.requests) != 1 {
		t.Fatalf("starts = %d", len(starter.requests))
	}
	request := starter.requests[0]
	if request.QuestionID != question || request.TargetParticipantID != 22 || request.ActorUserID != 11 ||
		request.UserInput != "check this" || request.FloorTurn == nil || request.FloorTurn.FromParticipantID != 21 ||
		request.FloorTurn.ReplyToQuestionID != "" || request.UnattendedContinuation != nil {
		t.Fatalf("request = %+v floor=%+v", request, request.FloorTurn)
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("the floor turn request is invalid: %v", err)
	}
}

func TestFloorHookAddressedTurnAnswersThePersonsQuestion(t *testing.T) {
	store := &floorSettleStoreStub{found: true, view: settleView(&QueuedFloorTurn{
		Position: 1, TurnQuestionID: FloorTurnQuestionID("ee92ccbd-3312-4c72-b20b-fddf224e7c0e", 1),
		ToParticipantID: 22, Cause: turntaking.CauseHumanMessage,
	})}
	chain := unattended.Chain{RootExecutionID: "root"}
	store.view.Chain = &chain
	starter := &floorStarterStub{}
	if err := newFloorHook(t, store, starter, nil).Settle(context.Background(), "x", executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
	request := starter.requests[0]
	if request.FloorTurn.ReplyToQuestionID != store.view.Exchange.QuestionID || request.UserInput != turntaking.AddressedNote ||
		request.UnattendedContinuation == nil || request.UnattendedContinuation.Kind != unattended.LinkAgentTurn {
		t.Fatalf("request = %+v", request)
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFloorHookSkipsTheRestOnFailurePauseOrCancel(t *testing.T) {
	cases := []struct {
		outcome executionapp.SettlementOutcome
		paused  bool
		want    string
	}{
		{executionapp.SettlementFailed, false, FloorSkipFailed},
		{executionapp.SettlementCancelled, false, FloorSkipCancelled},
		{executionapp.SettlementSucceeded, true, FloorSkipPaused},
	}
	for _, tc := range cases {
		store := &floorSettleStoreStub{found: true, view: settleView(&QueuedFloorTurn{TurnQuestionID: "q", ToParticipantID: 22})}
		store.view.Paused = tc.paused
		starter := &floorStarterStub{}
		if err := newFloorHook(t, store, starter, nil).Settle(context.Background(), "x", tc.outcome); err != nil {
			t.Fatal(err)
		}
		if len(starter.requests) != 0 || len(store.skipped) != 1 || store.skipped[0] != tc.want {
			t.Fatalf("%s: starts=%d skipped=%v", tc.want, len(starter.requests), store.skipped)
		}
	}
}

func TestFloorHookNeverStartsATurnTwice(t *testing.T) {
	store := &floorSettleStoreStub{found: true, view: settleView(&QueuedFloorTurn{
		Position: 1, TurnQuestionID: FloorTurnQuestionID("ee92ccbd-3312-4c72-b20b-fddf224e7c0e", 1),
		ToParticipantID: 22, Cause: turntaking.CauseHumanMessage,
	})}
	starter := &floorStarterStub{}
	hook := newFloorHook(t, store, starter, nil)
	for range 2 {
		if err := hook.Settle(context.Background(), "x", executionapp.SettlementSucceeded); err != nil {
			t.Fatal(err)
		}
	}
	// Both replays ask with the same idempotency key; the admission writes
	// one run.
	if len(starter.requests) != 2 || starter.requests[0].QuestionID != starter.requests[1].QuestionID {
		t.Fatalf("requests = %+v", starter.requests)
	}
	// A turn already STARTED is not QUEUED any more: the replay is done.
	starter.err = ErrFloorTurnNotQueued
	if err := hook.Settle(context.Background(), "x", executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
	if len(store.skipped) != 0 {
		t.Fatalf("a replay skipped turns: %v", store.skipped)
	}
}

func TestFloorHookDoesNothingWhileAnotherTurnRunsOrForANonFloorRun(t *testing.T) {
	store := &floorSettleStoreStub{found: true, view: settleView(&QueuedFloorTurn{TurnQuestionID: "q", ToParticipantID: 22})}
	store.view.Busy = true
	starter := &floorStarterStub{}
	hook := newFloorHook(t, store, starter, nil)
	if err := hook.Settle(context.Background(), "x", executionapp.SettlementSucceeded); err != nil || len(starter.requests) != 0 {
		t.Fatalf("err=%v starts=%d", err, len(starter.requests))
	}
	store.found = false
	if err := hook.Settle(context.Background(), "x", executionapp.SettlementFailed); err != nil || len(store.skipped) != 0 {
		t.Fatalf("err=%v skipped=%v", err, store.skipped)
	}
}

func TestFloorHookChainLimitEndsTheChainAndSkips(t *testing.T) {
	store := &floorSettleStoreStub{found: true, view: settleView(&QueuedFloorTurn{
		Position: 1, TurnQuestionID: FloorTurnQuestionID("ee92ccbd-3312-4c72-b20b-fddf224e7c0e", 1),
		ToParticipantID: 22, Cause: turntaking.CauseHumanMessage,
	})}
	chain := unattended.Chain{RootExecutionID: "root"}
	store.view.Chain = &chain
	chains := &floorChainsStub{}
	starter := &floorStarterStub{err: &unattended.ChainLimitError{Limit: unattended.LimitContinuations}}
	if err := newFloorHook(t, store, starter, chains).Settle(context.Background(), "x", executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
	if len(chains.refused) != 1 || chains.refused[0] != "root:continuations" || len(store.skipped) != 1 ||
		store.skipped[0] != FloorSkipRefused {
		t.Fatalf("refused=%v skipped=%v", chains.refused, store.skipped)
	}
	starter.err = errors.New("db down")
	store.skipped = nil
	if err := newFloorHook(t, store, starter, chains).Settle(context.Background(), "x", executionapp.SettlementSucceeded); err == nil ||
		len(store.skipped) != 0 {
		t.Fatalf("an infrastructure error was swallowed: skipped=%v", store.skipped)
	}
}

func TestFloorHookWaitsForTheChainToContinueALimitedTurn(t *testing.T) {
	store := &floorSettleStoreStub{found: true, view: settleView(&QueuedFloorTurn{TurnQuestionID: "q", ToParticipantID: 22})}
	store.view.StepLimitReached = true
	chain := unattended.Chain{RootExecutionID: "root", Grant: unattended.Grant{ContinueStepLimit: true}}
	store.view.Chain = &chain
	starter := &floorStarterStub{}
	if err := newFloorHook(t, store, starter, nil).Settle(context.Background(), "x", executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
	if len(starter.requests) != 0 || len(store.skipped) != 0 {
		t.Fatalf("starts=%d skipped=%v", len(starter.requests), store.skipped)
	}
	// Without the grant to continue, the person continues: the rest is skipped.
	store.view.Chain = nil
	if err := newFloorHook(t, store, starter, nil).Settle(context.Background(), "x", executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
	if len(starter.requests) != 0 || len(store.skipped) != 1 || store.skipped[0] != FloorSkipPaused {
		t.Fatalf("starts=%d skipped=%v", len(starter.requests), store.skipped)
	}
}
