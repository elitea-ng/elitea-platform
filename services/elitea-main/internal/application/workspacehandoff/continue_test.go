package workspacehandoff

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
)

const continueHandoff = "0b7e4c1a-2d3f-4a5b-9c6d-7e8f9a0b1c2d"

type recordingStarter struct {
	starts []ContinueStart
	err    error
}

func (s *recordingStarter) StartContinue(_ context.Context, start ContinueStart) (ContinuedTurn, error) {
	s.starts = append(s.starts, start)
	if s.err != nil {
		return ContinuedTurn{}, s.err
	}
	return ContinuedTurn{ExecutionID: "3e9a1b7c5d2f4e6a8b0c1d2e3f4a5b6c", QuestionID: "q", ResponseMessageID: "r",
		ConversationUUID: forgeThread}, nil
}

// completeRepo answers the row a continue's admission left, and fails a
// plain complete: a complete with continue never completes through it.
type completeRepo struct {
	Repository
	row       Handoff
	completes []CompleteRecord
}

func (r *completeRepo) Read(context.Context, Thread) (Handoff, bool, error) { return r.row, true, nil }

func (r *completeRepo) Complete(_ context.Context, record CompleteRecord) (Handoff, error) {
	r.completes = append(r.completes, record)
	return r.row, nil
}

func continueService(t *testing.T, row Handoff) (*Service, *completeRepo, *auditLog, *countingStateEvents) {
	t.Helper()
	repo := &completeRepo{row: row}
	recorder := &auditLog{}
	service, err := NewService(repo, fixedPolicy(Policy{Enabled: true}), noEraser{}, noCanceller{}, recorder, nil)
	if err != nil {
		t.Fatal(err)
	}
	events := &countingStateEvents{}
	service.WithStateEvents(events)
	return service, repo, recorder, events
}

func completeWith(next *Continue, takeBack bool) CompleteRequest {
	return CompleteRequest{
		ProjectID: 1, HandoffID: continueHandoff, ActorUserID: 7, Epoch: 3, HandoffRef: workspacestore.RefHandoff1,
		SHA: strings.Repeat("a", 40), AuditRoute: "/complete", TakeBackWhenDone: takeBack, Continue: next,
	}
}

func TestCompleteStoresTakeBackWhenDone(t *testing.T) {
	service, repo, _, _ := continueService(t, Handoff{ProjectID: 1, ConversationUUID: forgeThread,
		State: workspacestore.StateCloudIdle, Epoch: 4, TakeBackWhenDone: true})
	if _, err := service.Complete(context.Background(), completeWith(nil, true)); err != nil {
		t.Fatal(err)
	}
	if len(repo.completes) != 1 || !repo.completes[0].TakeBackWhenDone {
		t.Fatalf("completes = %+v, want take_back_when_done stored", repo.completes)
	}
}

func TestCompleteWithContinueStartsTheCloudTurn(t *testing.T) {
	running := Handoff{ProjectID: 1, ConversationUUID: forgeThread, OwnerUserID: 7,
		State: workspacestore.StateCloudRunning, Epoch: 4, TakeBackWhenDone: true}
	service, repo, recorder, events := continueService(t, running)
	// Without a starter, nothing changes.
	next := &Continue{Prompt: "Finish the migration.", Mode: ModeBuild,
		Grant: &unattended.Request{GrantID: continueHandoff, ChainRootExecutionID: "9f8e7d6c5b4a39281706f5e4d3c2b1a0"}}
	if _, err := service.Complete(context.Background(), completeWith(next, true)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("continue without a starter = %v, want ErrUnavailable", err)
	}
	starter := &recordingStarter{}
	service.WithContinueStarter(starter)
	handoff, err := service.Complete(context.Background(), completeWith(next, true))
	if err != nil {
		t.Fatal(err)
	}
	if len(repo.completes) != 0 {
		t.Fatal("a complete with continue completed outside the turn's admission")
	}
	if len(starter.starts) != 1 {
		t.Fatalf("starts = %d", len(starter.starts))
	}
	start := starter.starts[0]
	if start.HandoffID != continueHandoff || start.Epoch != 3 || !start.TakeBackWhenDone ||
		start.Prompt != next.Prompt || start.Grant != next.Grant || start.SHA != strings.Repeat("a", 40) {
		t.Fatalf("start = %+v", start)
	}
	if handoff.Continued == nil || handoff.Continued.ExecutionID != "3e9a1b7c5d2f4e6a8b0c1d2e3f4a5b6c" ||
		handoff.State != workspacestore.StateCloudRunning {
		t.Fatalf("answer = %+v", handoff)
	}
	if len(events.events) != 1 {
		t.Fatalf("live events = %d, want the complete's one", len(events.events))
	}
	if len(recorder.actions) == 0 {
		t.Fatal("the complete with continue is not audited")
	}

	// A refused start fails the complete with its cause.
	starter.err = unattended.Refused("grant_id", "", unattended.RefusedInvalid)
	if _, err := service.Complete(context.Background(), completeWith(next, false)); err == nil {
		t.Fatal("a refused start completed")
	} else if _, ok := unattended.AsRefusal(err); !ok {
		t.Fatalf("a refused start = %v, want the grant refusal", err)
	}
}

func TestCompleteRefusesAMalformedContinue(t *testing.T) {
	service, _, _, _ := continueService(t, Handoff{})
	starter := &recordingStarter{}
	service.WithContinueStarter(starter)
	for name, next := range map[string]*Continue{
		"empty prompt":  {Prompt: "  ", Mode: ModeBuild},
		"long prompt":   {Prompt: strings.Repeat("x", MaxContinuePromptRunes+1), Mode: ModeBuild},
		"nul in prompt": {Prompt: "a\x00b", Mode: ModeBuild},
		"unknown mode":  {Prompt: "go", Mode: "auto"},
		"missing mode":  {Prompt: "go"},
		"invalid utf-8": {Prompt: "\xff", Mode: ModePlan},
	} {
		if _, err := service.Complete(context.Background(), completeWith(next, false)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if len(starter.starts) != 0 {
		t.Fatalf("a malformed continue started %d turns", len(starter.starts))
	}
	// The bound is in characters, not bytes.
	if _, err := service.Complete(context.Background(),
		completeWith(&Continue{Prompt: strings.Repeat("é", MaxContinuePromptRunes), Mode: ModeBypass}, false)); err != nil {
		t.Fatalf("a prompt at the bound = %v", err)
	}
}

type fixedCloudRuns struct {
	run   *CloudRun
	err   error
	calls int
}

func (f *fixedCloudRuns) LastCloudRun(context.Context, Handoff) (*CloudRun, error) {
	f.calls++
	return f.run, f.err
}

func TestStateAnswersTheLastCloudRunWhileTheCloudOwnsTheThread(t *testing.T) {
	finished := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	runs := &fixedCloudRuns{run: &CloudRun{Outcome: CloudRunCompleted, ExecutionID: "e", FinishedAt: finished}}
	for _, test := range []struct {
		state workspacestore.State
		want  bool
	}{
		{workspacestore.StateCloudIdle, true}, {workspacestore.StateCloudRunning, true},
		{workspacestore.StateHandingOff, false}, {workspacestore.StateReclaiming, false},
	} {
		service, _, _, _ := continueService(t, Handoff{ProjectID: 1, ConversationUUID: forgeThread, State: test.state, Epoch: 2})
		service.WithCloudRuns(runs)
		view, err := service.State(context.Background(), Thread{ProjectID: 1, ConversationUUID: forgeThread})
		if err != nil {
			t.Fatal(err)
		}
		if (view.LastCloudRun != nil) != test.want {
			t.Errorf("%s: last cloud run = %+v", test.state, view.LastCloudRun)
		}
	}
	// An unreadable run never fails the banner.
	runs.err = errors.New("down")
	service, _, _, _ := continueService(t, Handoff{ProjectID: 1, ConversationUUID: forgeThread,
		State: workspacestore.StateCloudIdle, Epoch: 2})
	service.WithCloudRuns(runs)
	if view, err := service.State(context.Background(), Thread{ProjectID: 1, ConversationUUID: forgeThread}); err != nil || view.LastCloudRun != nil {
		t.Fatalf("state with an unreadable run = %+v, %v", view, err)
	}
}
