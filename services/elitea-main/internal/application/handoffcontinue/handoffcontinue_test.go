package handoffcontinue

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
)

const (
	handoffID = "0b7e4c1a-2d3f-4a5b-9c6d-7e8f9a0b1c2d"
	thread    = "6f1c2d3e-4a5b-4c6d-8e7f-901234567890"
	root      = "9f8e7d6c5b4a39281706f5e4d3c2b1a0"
	grantID   = "1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f"
)

type fixedTargets struct {
	target Target
	err    error
}

func (f fixedTargets) HandoffContinueTarget(context.Context, int64, string, int64) (Target, error) {
	return f.target, f.err
}

type fakeChains struct {
	chain   unattended.Chain
	found   bool
	refused []string
}

func (f *fakeChains) LatestChainLink(context.Context, string) (unattended.Chain, bool, error) {
	return f.chain, f.found, nil
}

func (f *fakeChains) EndRefusedChain(_ context.Context, root string, refusal *unattended.ChainLimitError) error {
	f.refused = append(f.refused, root+":"+refusal.Limit)
	return nil
}

type recordingStarter struct {
	applications []agentexecution.CurrentApplicationStartRequest
	adhoc        []agentexecution.CurrentAdhocStartRequest
	err          error
}

func (s *recordingStarter) StartCurrentApplication(
	_ context.Context, request agentexecution.CurrentApplicationStartRequest,
) (agentexecution.CurrentApplicationStartOutcome, error) {
	s.applications = append(s.applications, request)
	return agentexecution.CurrentApplicationStartOutcome{ExecutionID: "exec", ResponseMessageID: "response"}, s.err
}

func (s *recordingStarter) StartCurrentAdhoc(
	_ context.Context, request agentexecution.CurrentAdhocStartRequest,
) (agentexecution.CurrentApplicationStartOutcome, error) {
	s.adhoc = append(s.adhoc, request)
	return agentexecution.CurrentApplicationStartOutcome{ExecutionID: "exec", ResponseMessageID: "response"}, s.err
}

func start(grant *unattended.Request) workspacehandoff.ContinueStart {
	return workspacehandoff.ContinueStart{
		ProjectID: 1, HandoffID: handoffID, ActorUserID: 7, Epoch: 3, HandoffRef: "refs/elitea/handoff/1",
		SHA: "4b825dc642cb6eb9a060e54bf8d69288fbee4904", TakeBackWhenDone: true, Prompt: "Finish it.", Grant: grant,
	}
}

func TestStartContinueStartsTheTargetParticipantsTurnWithTheCompletion(t *testing.T) {
	for _, agent := range []bool{true, false} {
		starter := &recordingStarter{}
		service := New(fixedTargets{target: Target{ConversationUUID: thread, ParticipantID: 21, Agent: agent}},
			&fakeChains{}, starter, nil)
		turn, err := service.StartContinue(context.Background(), start(nil))
		if err != nil {
			t.Fatal(err)
		}
		question := agentexecution.HandoffContinueQuestionID(handoffID, 3)
		if turn.ExecutionID != "exec" || turn.ResponseMessageID != "response" || turn.QuestionID != question ||
			turn.ConversationUUID != thread {
			t.Fatalf("turn = %+v", turn)
		}
		var completion *agentexecution.HandoffCompletion
		if agent {
			if len(starter.applications) != 1 || len(starter.adhoc) != 0 {
				t.Fatal("an agent thread did not start an agent turn")
			}
			request := starter.applications[0]
			if request.TargetParticipantID != 21 || request.QuestionID != question || request.UserInput != "Finish it." ||
				request.ConversationUUID != thread || request.Unattended != nil || request.UnattendedContinuation != nil {
				t.Fatalf("agent request = %+v", request)
			}
			completion = request.HandoffCompletion
		} else {
			if len(starter.adhoc) != 1 || len(starter.applications) != 0 {
				t.Fatal("a model thread did not start a model turn")
			}
			request := starter.adhoc[0]
			if request.TargetParticipantID != 21 || !json.Valid(request.LLMSettings) {
				t.Fatalf("model request = %+v", request)
			}
			completion = request.HandoffCompletion
		}
		if completion == nil || completion.HandoffID != handoffID || completion.Epoch != 3 || !completion.TakeBackWhenDone ||
			!completion.Valid() {
			t.Fatalf("completion = %+v", completion)
		}
	}
}

func TestStartContinueGrant(t *testing.T) {
	target := fixedTargets{target: Target{ConversationUUID: thread, ParticipantID: 21, Agent: true}}

	// No root: a new cloud chain, without the desktop-only parts.
	starter := &recordingStarter{}
	service := New(target, &fakeChains{}, starter, nil)
	grant := &unattended.Request{GrantID: grantID, ContinueStepLimit: true, ContinueToolCallCap: true,
		HookAsks: true, RemoteConfirmations: true}
	if _, err := service.StartContinue(context.Background(), start(grant)); err != nil {
		t.Fatal(err)
	}
	sent := starter.applications[0].Unattended
	if sent == nil || sent.GrantID != grantID || !sent.ContinueStepLimit || sent.ContinueToolCallCap ||
		sent.HookAsks || sent.RemoteConfirmations || !grant.HookAsks {
		t.Fatalf("new chain grant = %+v (the caller's %+v)", sent, grant)
	}

	// A root: a handoff link of the chain's latest run.
	chain := unattended.Chain{RootExecutionID: root, ExecutionID: "1f2e3d4c5b6a79881706f5e4d3c2b1a0", GrantID: grantID,
		Origin: unattended.OriginDesktop}
	chains := &fakeChains{chain: chain, found: true}
	starter = &recordingStarter{}
	service = New(target, chains, starter, nil)
	continued := &unattended.Request{GrantID: grantID, ChainRootExecutionID: root}
	if _, err := service.StartContinue(context.Background(), start(continued)); err != nil {
		t.Fatal(err)
	}
	request := starter.applications[0]
	if request.Unattended != nil || request.UnattendedContinuation == nil ||
		request.UnattendedContinuation.Kind != unattended.LinkHandoff ||
		request.UnattendedContinuation.Chain.ExecutionID != chain.ExecutionID {
		t.Fatalf("continuation = %+v", request.UnattendedContinuation)
	}

	// Refusals before any start.
	for name, test := range map[string]struct {
		grant  *unattended.Request
		chains *fakeChains
		reason string
	}{
		"a kind":          {&unattended.Request{GrantID: grantID, ChainRootExecutionID: root, Kind: "step_limit"}, chains, unattended.RefusedInvalid},
		"another grant":   {&unattended.Request{GrantID: handoffID, ChainRootExecutionID: root}, chains, unattended.RefusedInvalid},
		"an absent chain": {continued, &fakeChains{}, unattended.RefusedChainNotFound},
	} {
		starter := &recordingStarter{}
		_, err := New(target, test.chains, starter, nil).StartContinue(context.Background(), start(test.grant))
		refusal, ok := unattended.AsRefusal(err)
		if !ok || refusal.Reason != test.reason || len(starter.applications) != 0 {
			t.Errorf("%s: %v (starts %d), want refused %s", name, err, len(starter.applications), test.reason)
		}
	}

	// A chain at a limit ends there.
	chains = &fakeChains{chain: chain, found: true}
	starter = &recordingStarter{err: &unattended.ChainLimitError{Limit: unattended.LimitSpend}}
	_, err := New(target, chains, starter, nil).StartContinue(context.Background(), start(continued))
	var limit *unattended.ChainLimitError
	if !errors.As(err, &limit) || len(chains.refused) != 1 || chains.refused[0] != root+":"+unattended.LimitSpend {
		t.Fatalf("a chain at a limit = %v, ended %v", err, chains.refused)
	}
}

func TestStartContinueNamesRefusedStartsInHandoffTerms(t *testing.T) {
	target := fixedTargets{target: Target{ConversationUUID: thread, ParticipantID: 21}}
	for cause, want := range map[error]error{
		agentexecution.ErrLocalWorkThread:                workspacehandoff.ErrUnavailable,
		&executionapp.AdmissionCapacityError{}:           workspacehandoff.ErrUnavailable,
		agentexecution.ErrInvalidCurrentAgentStart:       workspacehandoff.ErrInvalid,
		agentexecution.ErrUnsupportedCurrentAgentStart:   workspacehandoff.ErrInvalid,
		executionapp.ErrIdempotencyConflict:              workspacehandoff.ErrStaleEpoch,
		workspacehandoff.ErrRefsMissing:                  workspacehandoff.ErrRefsMissing,
		unattended.Refused("unattended", "", "disabled"): nil,
	} {
		_, err := New(target, &fakeChains{}, &recordingStarter{err: cause}, nil).StartContinue(context.Background(), start(nil))
		if !errors.Is(err, cause) && !errors.As(err, new(*executionapp.AdmissionCapacityError)) {
			t.Errorf("%v: the cause is lost: %v", cause, err)
		}
		if want != nil && !errors.Is(err, want) {
			t.Errorf("%v = %v, want %v", cause, err, want)
		}
	}
	// The target's own error passes as it is.
	_, err := New(fixedTargets{err: workspacehandoff.ErrNotFound}, &fakeChains{}, &recordingStarter{}, nil).
		StartContinue(context.Background(), start(nil))
	if !errors.Is(err, workspacehandoff.ErrNotFound) {
		t.Fatalf("an unknown send = %v", err)
	}
}
