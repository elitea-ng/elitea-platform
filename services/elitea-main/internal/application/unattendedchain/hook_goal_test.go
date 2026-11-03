package unattendedchain

import (
	"context"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runbrief"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// goalsStub is the run brief service as the hook sees it.
type goalsStub struct {
	result  runbrief.Result
	checked bool
	err     error
	text    string
	checks  []string
}

func (g *goalsStub) ContinueText(context.Context, string) string { return g.text }

func (g *goalsStub) CheckCloud(_ context.Context, _ string, executionID string) (runbrief.Result, bool, error) {
	g.checks = append(g.checks, executionID)
	return g.result, g.checked, g.err
}

func settleWithGoals(t *testing.T, chains *chainsStub, starter *starterStub, goals *goalsStub) {
	t.Helper()
	hook := New(chains, starter, nil).WithGoals(goals)
	if err := hook.Settle(t.Context(), chains.view.Chain.ExecutionID, executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
}

func TestSettleGoalOutcomesEndTheChain(t *testing.T) {
	cases := []struct {
		name    string
		goals   goalsStub
		wantEnd string
	}{
		{"met", goalsStub{checked: true, result: runbrief.Result{Outcome: runbrief.OutcomeMet}}, unattended.EndCompleted},
		{"blocked", goalsStub{checked: true, result: runbrief.Result{Outcome: runbrief.OutcomeBlocked}}, unattended.EndFailed},
		{"not checked", goalsStub{checked: true, result: runbrief.Result{Outcome: runbrief.OutcomeNotChecked}},
			unattended.EndCompleted},
		{"no check", goalsStub{}, unattended.EndCompleted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chains := &chainsStub{view: cloudView()}
			starter := &starterStub{}
			settleWithGoals(t, chains, starter, &tc.goals)
			if len(chains.ended) != 1 || chains.ended[0] != tc.wantEnd || len(starter.applications) != 0 {
				t.Fatalf("ended = %v, starts = %d", chains.ended, len(starter.applications))
			}
			if len(tc.goals.checks) != 1 || tc.goals.checks[0] != chains.view.Chain.ExecutionID {
				t.Fatalf("checks = %v", tc.goals.checks)
			}
		})
	}
}

func TestSettleGoalNotMetStartsAGoalContinuation(t *testing.T) {
	chains := &chainsStub{view: cloudView()}
	starter := &starterStub{}
	goals := &goalsStub{checked: true, result: runbrief.Result{
		Outcome: runbrief.OutcomeNotMet, ContinueText: "Continue from where you stopped.\n\n<run_brief>…</run_brief>",
	}}
	settleWithGoals(t, chains, starter, goals)
	if len(chains.ended) != 0 || len(starter.applications) != 1 {
		t.Fatalf("ended = %v, starts = %+v", chains.ended, starter.applications)
	}
	start := starter.applications[0]
	if start.UserInput != goals.result.ContinueText ||
		start.QuestionID != agentexecution.GoalContinuationQuestionID(chains.view.Chain.ExecutionID) ||
		start.QuestionID == agentexecution.ContinuationQuestionID(chains.view.Chain.ExecutionID) ||
		start.UnattendedContinuation == nil || start.UnattendedContinuation.Kind != unattended.LinkGoalNotMet {
		t.Fatalf("start = %+v", start)
	}
}

func TestSettleGoalNotMetAtTheLimitEndsLimitReached(t *testing.T) {
	chains := &chainsStub{view: cloudView()}
	starter := &starterStub{err: &unattended.ChainLimitError{Limit: unattended.LimitContinuations}}
	settleWithGoals(t, chains, starter, &goalsStub{checked: true, result: runbrief.Result{
		Outcome: runbrief.OutcomeNotMet, ContinueText: "Continue from where you stopped.",
	}})
	if len(chains.refused) != 1 || chains.refused[0] != unattended.LimitContinuations {
		t.Fatalf("refused = %v", chains.refused)
	}
}

func TestSettleGoalCheckErrorLeavesTheChainForAReplay(t *testing.T) {
	chains := &chainsStub{view: cloudView()}
	hook := New(chains, &starterStub{}, nil).WithGoals(&goalsStub{err: errors.New("database down")})
	if err := hook.Settle(t.Context(), "x", executionapp.SettlementSucceeded); err == nil {
		t.Fatal("want the error")
	}
	if len(chains.ended) != 0 {
		t.Fatalf("ended = %v", chains.ended)
	}
}

func TestSettleStepLimitRestatesTheBrief(t *testing.T) {
	chains := &chainsStub{view: cloudView()}
	chains.view.StepLimitReached = true
	starter := &starterStub{}
	goals := &goalsStub{text: "Continue from where you stopped.\n\n<run_brief>\nGoal: g\n</run_brief>"}
	settleWithGoals(t, chains, starter, goals)
	if len(starter.applications) != 1 || starter.applications[0].UserInput != goals.text ||
		starter.applications[0].UnattendedContinuation.Kind != unattended.LinkStepLimit {
		t.Fatalf("starts = %+v", starter.applications)
	}
	if len(goals.checks) != 0 {
		t.Fatalf("a step-limit continuation runs no check: %v", goals.checks)
	}
}

func TestSettleFailedRunRunsNoCheck(t *testing.T) {
	chains := &chainsStub{view: cloudView()}
	goals := &goalsStub{checked: true, result: runbrief.Result{Outcome: runbrief.OutcomeNotMet, ContinueText: "x"}}
	hook := New(chains, &starterStub{}, nil).WithGoals(goals)
	if err := hook.Settle(t.Context(), "x", executionapp.SettlementFailed); err != nil {
		t.Fatal(err)
	}
	if len(goals.checks) != 0 || len(chains.ended) != 1 || chains.ended[0] != unattended.EndFailed {
		t.Fatalf("checks = %v, ended = %v", goals.checks, chains.ended)
	}
}
