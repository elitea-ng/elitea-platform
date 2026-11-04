package handoffcontinue

import (
	"context"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

// fakeThreads answers a fixed thread; reads counts the ThreadTarget calls,
// and after answers the reads after the first.
type fakeThreads struct {
	target ThreadTarget
	err    error
	after  *ThreadTarget
	reads  int
	active bool
}

func (f *fakeThreads) ThreadTarget(context.Context, int64, string) (ThreadTarget, error) {
	f.reads++
	if f.reads > 1 && f.after != nil {
		return *f.after, f.err
	}
	return f.target, f.err
}

func (f *fakeThreads) RunActive(context.Context, int64, string) (bool, error) { return f.active, nil }

func idleThread(agent bool, version int64) ThreadTarget {
	return ThreadTarget{
		Target: Target{ConversationUUID: thread, ParticipantID: 21, Agent: agent}, VersionID: version, Idle: true,
	}
}

func threadTurn() ThreadTurn {
	return ThreadTurn{
		ProjectID: 1, ConversationUUID: thread, ActorUserID: 7, QuestionID: "q-1", Prompt: "Check the build.",
		TriggerOrigin: executiondomain.TriggerOriginSchedule,
	}
}

func threadService(threads *fakeThreads, starter *recordingStarter) *Service {
	return New(fixedTargets{}, &fakeChains{}, starter, nil).WithThreads(threads)
}

func TestStartThreadTurnStartsTheThreadsParticipantWithASystemOrigin(t *testing.T) {
	for _, agent := range []bool{true, false} {
		starter := &recordingStarter{}
		turn := threadTurn()
		turn.TriggerOrigin = executiondomain.TriggerOriginWebhook
		turn.Grant = &unattended.Request{
			GrantID: "stored", ChainRootExecutionID: root, Kind: "x", ContinueStepLimit: true,
			ContinueToolCallCap: true, HookAsks: true, RemoteConfirmations: true, GoalCheck: true,
		}
		started, err := threadService(&fakeThreads{target: idleThread(agent, 5)}, starter).StartThreadTurn(context.Background(), turn)
		if err != nil {
			t.Fatal(err)
		}
		if started.ExecutionID != "exec" || started.QuestionID != "q-1" || started.ConversationUUID != thread {
			t.Fatalf("started = %+v", started)
		}
		var grant *unattended.Request
		if agent {
			if len(starter.applications) != 1 || len(starter.adhoc) != 0 {
				t.Fatal("an agent thread did not start an agent turn")
			}
			request := starter.applications[0]
			if request.TargetParticipantID != 21 || request.UserInput != "Check the build." ||
				request.TriggerOrigin != executiondomain.TriggerOriginWebhook || request.HandoffCompletion != nil ||
				request.UnattendedContinuation != nil {
				t.Fatalf("agent request = %+v", request)
			}
			grant = request.Unattended
		} else {
			if len(starter.adhoc) != 1 || len(starter.applications) != 0 {
				t.Fatal("a model thread did not start a model turn")
			}
			grant = starter.adhoc[0].Unattended
		}
		// A new chain: no stored grant id, no chain root, no desktop parts.
		if grant == nil || grant.GrantID != "" || grant.ChainRootExecutionID != "" || grant.Kind != "" ||
			grant.ContinueToolCallCap || grant.HookAsks || grant.RemoteConfirmations ||
			!grant.ContinueStepLimit || !grant.GoalCheck || !grant.OnlyContinues() {
			t.Fatalf("grant = %+v", grant)
		}
	}
}

func TestStartThreadTurnRefusals(t *testing.T) {
	busy := idleThread(true, 5)
	busy.Idle = false
	cases := []struct {
		name    string
		threads *fakeThreads
		turn    func(*ThreadTurn)
		want    error
	}{
		{"busy", &fakeThreads{target: busy}, nil, ErrThreadBusy},
		{"gone", &fakeThreads{err: workspacehandoff.ErrNotFound}, nil, workspacehandoff.ErrNotFound},
		{"not local work", &fakeThreads{err: workspacehandoff.ErrNotLocalWork}, nil, workspacehandoff.ErrNotFound},
		{"empty prompt", &fakeThreads{target: idleThread(true, 5)}, func(turn *ThreadTurn) { turn.Prompt = " " },
			workspacehandoff.ErrInvalid},
		{"manual origin", &fakeThreads{target: idleThread(true, 5)},
			func(turn *ThreadTurn) { turn.TriggerOrigin = executiondomain.TriggerOriginManual }, workspacehandoff.ErrInvalid},
		{"governed, other version", &fakeThreads{target: idleThread(true, 6)},
			func(turn *ThreadTurn) { turn.VersionID = 5 }, ErrThreadVersion},
		{"governed, model thread", &fakeThreads{target: idleThread(false, 0)},
			func(turn *ThreadTurn) { turn.VersionID = 5 }, ErrThreadVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			starter := &recordingStarter{}
			turn := threadTurn()
			if tc.turn != nil {
				tc.turn(&turn)
			}
			_, err := threadService(tc.threads, starter).StartThreadTurn(context.Background(), turn)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(starter.applications)+len(starter.adhoc) != 0 {
				t.Fatal("a refused occurrence started a turn")
			}
		})
	}
}

func TestStartThreadTurnGovernedVersionMatches(t *testing.T) {
	starter := &recordingStarter{}
	turn := threadTurn()
	turn.VersionID = 5
	if _, err := threadService(&fakeThreads{target: idleThread(true, 5)}, starter).StartThreadTurn(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
}

// The fence in the admission transaction closes after the read: the start's
// refusal is a busy thread, not a failure.
func TestStartThreadTurnNamesALostRaceBusy(t *testing.T) {
	for _, refusal := range []error{agentexecution.ErrCloudTurnRunning, agentexecution.ErrLocalWorkThread} {
		starter := &recordingStarter{err: refusal}
		_, err := threadService(&fakeThreads{target: idleThread(true, 5)}, starter).StartThreadTurn(context.Background(), threadTurn())
		if !errors.Is(err, ErrThreadBusy) {
			t.Fatalf("%v: err = %v", refusal, err)
		}
	}
	// A second answer that streams: the start refuses as unsupported, and a
	// read again finds the thread busy.
	busy := idleThread(true, 5)
	busy.Idle = false
	starter := &recordingStarter{err: agentexecution.ErrUnsupportedCurrentAgentStart}
	_, err := threadService(&fakeThreads{target: idleThread(true, 5), after: &busy}, starter).
		StartThreadTurn(context.Background(), threadTurn())
	if !errors.Is(err, ErrThreadBusy) {
		t.Fatalf("err = %v", err)
	}
	// Still idle: the refusal is the start's own.
	starter = &recordingStarter{err: agentexecution.ErrUnsupportedCurrentAgentStart}
	_, err = threadService(&fakeThreads{target: idleThread(true, 5)}, starter).StartThreadTurn(context.Background(), threadTurn())
	if errors.Is(err, ErrThreadBusy) || !errors.Is(err, agentexecution.ErrUnsupportedCurrentAgentStart) {
		t.Fatalf("err = %v", err)
	}
}

func TestThreadSeamWithoutThreads(t *testing.T) {
	service := New(fixedTargets{}, &fakeChains{}, &recordingStarter{}, nil)
	if _, err := service.StartThreadTurn(context.Background(), threadTurn()); !errors.Is(err, workspacehandoff.ErrUnavailable) {
		t.Fatalf("start err = %v", err)
	}
	if _, err := service.ThreadHandedOff(context.Background(), 1, thread); !errors.Is(err, workspacehandoff.ErrUnavailable) {
		t.Fatalf("handed off err = %v", err)
	}
	var nilService *Service
	if nilService.WithThreads(&fakeThreads{}) != nil {
		t.Fatal("WithThreads on a nil service")
	}
}

func TestThreadHandedOffAndRunActive(t *testing.T) {
	service := threadService(&fakeThreads{target: idleThread(true, 5), active: true}, &recordingStarter{})
	if ok, err := service.ThreadHandedOff(context.Background(), 1, thread); err != nil || !ok {
		t.Fatalf("handed off = %v, %v", ok, err)
	}
	for _, missing := range []error{workspacehandoff.ErrNotFound, workspacehandoff.ErrNotLocalWork} {
		service := threadService(&fakeThreads{err: missing}, &recordingStarter{})
		if ok, err := service.ThreadHandedOff(context.Background(), 1, thread); err != nil || ok {
			t.Fatalf("%v: handed off = %v, %v", missing, ok, err)
		}
	}
	if active, err := service.RunActive(context.Background(), 1, ""); err != nil || active {
		t.Fatalf("no execution: active = %v, %v", active, err)
	}
	if active, err := service.RunActive(context.Background(), 1, "exec"); err != nil || !active {
		t.Fatalf("active = %v, %v", active, err)
	}
}
