package unattendedchain

import (
	"context"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

type chainsStub struct {
	view    SettleView
	ended   []string
	refused []string
}

func (c *chainsStub) SettleView(context.Context, string) (SettleView, bool, error) {
	return c.view, true, nil
}

func (c *chainsStub) EndChain(_ context.Context, _ string, state, _ string) (bool, error) {
	if c.view.Chain.EndState != "" {
		return false, nil
	}
	c.view.Chain.EndState = state
	c.ended = append(c.ended, state)
	return true, nil
}

func (c *chainsStub) EndRefusedChain(_ context.Context, _ string, refusal *unattended.ChainLimitError) error {
	c.view.Chain.EndState = unattended.EndLimitReached
	c.refused = append(c.refused, refusal.Limit)
	return nil
}

type starterStub struct {
	applications []agentexecution.CurrentApplicationStartRequest
	continues    []agentexecution.CurrentContinuationRequest
	err          error
}

func (s *starterStub) StartCurrentApplication(
	_ context.Context, request agentexecution.CurrentApplicationStartRequest,
) (agentexecution.CurrentApplicationStartOutcome, error) {
	s.applications = append(s.applications, request)
	return agentexecution.CurrentApplicationStartOutcome{}, s.err
}

func (s *starterStub) StartCurrentAdhoc(
	context.Context, agentexecution.CurrentAdhocStartRequest,
) (agentexecution.CurrentApplicationStartOutcome, error) {
	return agentexecution.CurrentApplicationStartOutcome{}, s.err
}

func (s *starterStub) ContinueCurrentAgent(
	_ context.Context, request agentexecution.CurrentContinuationRequest,
) (agentexecution.CurrentApplicationStartOutcome, error) {
	s.continues = append(s.continues, request)
	return agentexecution.CurrentApplicationStartOutcome{}, s.err
}

func cloudView() SettleView {
	return SettleView{Latest: true, Chain: unattended.Chain{
		RootExecutionID: "0123456789abcdef0123456789abcdef", ExecutionID: "fedcba9876543210fedcba9876543210",
		ProjectID: 1, OwnerUserID: 7, ConversationUUID: "3c5e7a9b-1d2f-4a6b-8c0d-2e4f6a8b0c1d",
		Origin: unattended.OriginCloud, TriggerOrigin: "manual", ResponseMessageID: "4d6f8b0c-2e3a-4b7c-9d1e-3f5a7b9c1d2e",
		Grant: unattended.Grant{
			ContinueStepLimit: true, ContinueOutputLimit: true,
			Start: &unattended.CloudStart{Contract: agentexecution.CloudStartContractApplication, ParticipantID: 5},
		},
	}}
}

func TestSettleStartsTheStepLimitContinuation(t *testing.T) {
	chains := &chainsStub{view: cloudView()}
	chains.view.StepLimitReached = true
	starter := &starterStub{}
	hook := New(chains, starter, nil)
	for range 2 { // a replayed settle starts the same continuation
		if err := hook.Settle(t.Context(), chains.view.Chain.ExecutionID, executionapp.SettlementSucceeded); err != nil {
			t.Fatal(err)
		}
	}
	if len(starter.applications) != 2 || starter.applications[0].QuestionID != starter.applications[1].QuestionID {
		t.Fatalf("starts = %+v", starter.applications)
	}
	start := starter.applications[0]
	if start.UserInput != unattended.ContinuePrompt || start.ActorUserID != 7 || start.TargetParticipantID != 5 ||
		start.QuestionID != agentexecution.ContinuationQuestionID(chains.view.Chain.ExecutionID) ||
		start.UnattendedContinuation == nil || start.UnattendedContinuation.Kind != unattended.LinkStepLimit {
		t.Fatalf("start = %+v", start)
	}
	if len(chains.ended) != 0 {
		t.Fatalf("the chain ended: %v", chains.ended)
	}
}

func TestSettleContinuesTheOutputLimitFirst(t *testing.T) {
	chains := &chainsStub{view: cloudView()}
	chains.view.OutputLimitReached, chains.view.OutputLimitSequence, chains.view.StepLimitReached = true, 1, true
	starter := &starterStub{}
	if err := New(chains, starter, nil).Settle(t.Context(), "x", executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
	if len(starter.continues) != 1 || len(starter.applications) != 0 ||
		starter.continues[0].Kind != agentexecution.CurrentContinuationOutputLimit {
		t.Fatalf("continues = %+v, starts = %+v", starter.continues, starter.applications)
	}
}

func TestSettleEndsTheChainOnce(t *testing.T) {
	cases := []struct {
		name    string
		outcome executionapp.SettlementOutcome
		mutate  func(*SettleView)
		want    string
	}{
		{"completed", executionapp.SettlementSucceeded, func(*SettleView) {}, unattended.EndCompleted},
		{"failed", executionapp.SettlementFailed, func(*SettleView) {}, unattended.EndFailed},
		{"error answer", executionapp.SettlementSucceeded, func(v *SettleView) { v.IsError = true }, unattended.EndFailed},
		{"cancelled", executionapp.SettlementCancelled, func(*SettleView) {}, unattended.EndCancelled},
		{"paused", executionapp.SettlementSucceeded, func(v *SettleView) { v.Paused = true }, unattended.EndWaiting},
		{"output limit without grant", executionapp.SettlementSucceeded, func(v *SettleView) {
			v.OutputLimitReached, v.OutputLimitSequence, v.Chain.Grant.ContinueOutputLimit = true, 1, false
		}, unattended.EndWaiting},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chains := &chainsStub{view: cloudView()}
			tc.mutate(&chains.view)
			hook := New(chains, &starterStub{}, nil)
			for range 2 {
				if err := hook.Settle(t.Context(), "x", tc.outcome); err != nil {
					t.Fatal(err)
				}
			}
			if len(chains.ended) != 1 || chains.ended[0] != tc.want {
				t.Fatalf("ended = %v, want [%s]", chains.ended, tc.want)
			}
		})
	}
}

func TestSettleLimitEndsTheChain(t *testing.T) {
	chains := &chainsStub{view: cloudView()}
	chains.view.StepLimitReached = true
	starter := &starterStub{err: &unattended.ChainLimitError{Limit: unattended.LimitSpend}}
	if err := New(chains, starter, nil).Settle(t.Context(), "x", executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
	if len(chains.refused) != 1 || chains.refused[0] != unattended.LimitSpend {
		t.Fatalf("refused = %v", chains.refused)
	}
}

func TestSettleIgnoresAnOlderRunAndDesktopChains(t *testing.T) {
	for _, mutate := range []func(*SettleView){
		func(v *SettleView) { v.Latest = false },
		func(v *SettleView) { v.Chain.Origin = unattended.OriginDesktop },
		func(v *SettleView) { v.Chain.EndState = unattended.EndCompleted },
	} {
		chains := &chainsStub{view: cloudView()}
		mutate(&chains.view)
		ended := chains.view.Chain.EndState
		if err := New(chains, &starterStub{}, nil).Settle(t.Context(), "x", executionapp.SettlementSucceeded); err != nil {
			t.Fatal(err)
		}
		if len(chains.ended) != 0 || chains.view.Chain.EndState != ended {
			t.Fatalf("the hook ended the chain: %v", chains.ended)
		}
	}
}
