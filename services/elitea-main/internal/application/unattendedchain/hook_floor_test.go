package unattendedchain

import (
	"context"
	"testing"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

type floorStub struct{ pending bool }

func (f floorStub) PendingFloorTurns(context.Context, int64, string, string) (bool, error) {
	return f.pending, nil
}

// A chain whose message still owes an agent turn (ADR-0050 decision 7c)
// stays open for that turn; with none owed it ends completed.
func TestSettleKeepsTheChainOpenForAQueuedFloorTurn(t *testing.T) {
	chains := &chainsStub{view: cloudView()}
	hook := New(chains, &starterStub{}, nil).WithFloor(floorStub{pending: true})
	if err := hook.Settle(t.Context(), "x", executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
	if len(chains.ended) != 0 {
		t.Fatalf("the chain ended with a turn queued: %v", chains.ended)
	}
	hook = New(chains, &starterStub{}, nil).WithFloor(floorStub{})
	if err := hook.Settle(t.Context(), "x", executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
	if len(chains.ended) != 1 || chains.ended[0] != unattended.EndCompleted {
		t.Fatalf("ended = %v", chains.ended)
	}
	// A failed run ends the chain whatever is queued.
	chains = &chainsStub{view: cloudView()}
	hook = New(chains, &starterStub{}, nil).WithFloor(floorStub{pending: true})
	if err := hook.Settle(t.Context(), "x", executionapp.SettlementFailed); err != nil {
		t.Fatal(err)
	}
	if len(chains.ended) != 1 || chains.ended[0] != unattended.EndFailed {
		t.Fatalf("ended = %v", chains.ended)
	}
}

// A floor turn's step-limit continuation goes to the agent that stopped,
// not the agent the chain started with.
func TestSettleContinuesTheAgentThatStopped(t *testing.T) {
	chains := &chainsStub{view: cloudView()}
	chains.view.StepLimitReached = true
	chains.view.ParticipantID = 9
	starter := &starterStub{}
	if err := New(chains, starter, nil).Settle(t.Context(), chains.view.Chain.ExecutionID, executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
	if len(starter.applications) != 1 || starter.applications[0].TargetParticipantID != 9 {
		t.Fatalf("starts = %+v", starter.applications)
	}
	if chains.view.Chain.Grant.Start.ParticipantID != 5 {
		t.Fatal("the grant's start was mutated")
	}
}
