package handoffcontinue

// Schedules and webhook triggers that target a handed-off workspace thread
// (ADR-0045 decision 9, #1440).
//
// Each occurrence starts a cloud turn on the thread as a message that a
// person posts on a CLOUD_IDLE thread starts one: the same chat start use
// case, the same Local work fence, and the same admission transaction. That
// transaction takes the handoff row's share lock and checks CLOUD_IDLE again
// (repos, CurrentApplicationTurn.CloudThread), then takes the conversation
// lock and refuses a second answer while one streams. So an occurrence and
// a posted message (or two occurrences) never both start a turn.
//
// The turn addresses the participant that the thread's last turn
// addressed, as a send's continue does. It runs from the thread's latest
// snapshot: the worker's claim starts it from the handoff's cloud ref.
//
// A thread that is not CLOUD_IDLE (HANDING_OFF, CLOUD_RUNNING, RECLAIMING,
// released, erased, a take back pending, or an answer that still streams)
// answers ErrThreadBusy and starts nothing. A thread that is gone, or that
// was never handed off, answers workspacehandoff.ErrNotFound.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

var (
	// ErrThreadBusy: the thread is not CLOUD_IDLE, so the occurrence starts
	// no turn (skipped_thread_busy, 409 thread_busy).
	ErrThreadBusy = errors.New("handoffcontinue: the workspace thread is busy")
	// ErrThreadVersion: a governed occurrence names a version, and the
	// thread's participant is not an agent at that version. The mandate
	// binds the version, so the run would not be the one it covers.
	ErrThreadVersion = errors.New("handoffcontinue: the thread's agent is not the version the mandate binds")
)

// ThreadTarget is a handed-off thread and the participant its next turn
// addresses.
type ThreadTarget struct {
	Target
	// VersionID is the agent participant's mapped version; 0 for the
	// model participant.
	VersionID int64
	// Idle: the handoff is CLOUD_IDLE with no take back pending, and no
	// answer of the thread streams.
	Idle bool
}

// Threads reads handed-off threads for a schedule or a trigger
// (repos.WorkspaceHandoffsRepository).
type Threads interface {
	// ThreadTarget answers workspacehandoff.ErrNotFound when the thread is
	// gone, has no handoff row or is erased.
	ThreadTarget(ctx context.Context, projectID int64, conversationUUID string) (ThreadTarget, error)
	// RunActive reports whether the run of an execution is still going:
	// its execution is not settled, or the chain it is the root of is open.
	RunActive(ctx context.Context, projectID int64, executionID string) (bool, error)
}

// ThreadTurn is one occurrence of a schedule or a webhook trigger on a
// handed-off thread.
type ThreadTurn struct {
	ProjectID        int64
	ConversationUUID string
	// ActorUserID is the schedule's or the trigger's run identity.
	ActorUserID int64
	// QuestionID is the occurrence's message id (the start's idempotency
	// key).
	QuestionID string
	// Prompt is the message the turn starts with.
	Prompt string
	// Grant is the occurrence's grant; nil starts the turn with no grant.
	// Each occurrence starts a new chain.
	Grant *unattended.Request
	// TriggerOrigin is schedule or webhook.
	TriggerOrigin executiondomain.TriggerOrigin
	// VersionID is set for a governed occurrence: the thread's participant
	// must be an agent at this version (ErrThreadVersion).
	VersionID int64
	// Run is what the admission records with the turn: the row that
	// started it and the run's approval snapshot (agentexecution.ThreadRun).
	Run *agentexecution.ThreadRun
}

// ThreadTurnStarted is the turn an occurrence started.
type ThreadTurnStarted struct {
	ExecutionID       string
	QuestionID        string
	ResponseMessageID string
	ConversationUUID  string
}

// WithThreads lets the starter start schedule and webhook turns on a
// handed-off thread. Without it StartThreadTurn answers
// workspacehandoff.ErrUnavailable.
func (s *Service) WithThreads(threads Threads) *Service {
	if s != nil && threads != nil {
		s.threads = threads
	}
	return s
}

// HandedOffThread reads a thread that was handed off at least once (it has
// a handoff row that is not erased), and the participant its next turn
// addresses. Its current state does not matter. false: never handed off,
// or gone.
func (s *Service) HandedOffThread(
	ctx context.Context, projectID int64, conversationUUID string,
) (ThreadTarget, bool, error) {
	if s == nil || s.threads == nil {
		return ThreadTarget{}, false, workspacehandoff.ErrUnavailable
	}
	target, err := s.threads.ThreadTarget(ctx, projectID, conversationUUID)
	switch {
	case errors.Is(err, workspacehandoff.ErrNotFound), errors.Is(err, workspacehandoff.ErrNotLocalWork):
		return ThreadTarget{}, false, nil
	case err != nil:
		return ThreadTarget{}, false, err
	}
	return target, true, nil
}

// RunActive reports whether the previous occurrence's run still goes.
func (s *Service) RunActive(ctx context.Context, projectID int64, executionID string) (bool, error) {
	if s == nil || s.threads == nil {
		return false, workspacehandoff.ErrUnavailable
	}
	if executionID == "" {
		return false, nil
	}
	return s.threads.RunActive(ctx, projectID, executionID)
}

// StartThreadTurn starts the occurrence's cloud turn on the thread.
func (s *Service) StartThreadTurn(ctx context.Context, turn ThreadTurn) (ThreadTurnStarted, error) {
	if s == nil || s.threads == nil {
		return ThreadTurnStarted{}, workspacehandoff.ErrUnavailable
	}
	if strings.TrimSpace(turn.Prompt) == "" || !unattended.IsSystemTrigger(string(turn.TriggerOrigin)) {
		return ThreadTurnStarted{}, workspacehandoff.ErrInvalid
	}
	target, err := s.threads.ThreadTarget(ctx, turn.ProjectID, turn.ConversationUUID)
	if errors.Is(err, workspacehandoff.ErrNotLocalWork) {
		return ThreadTurnStarted{}, fmt.Errorf("%w: %w", workspacehandoff.ErrNotFound, err)
	}
	if err != nil {
		return ThreadTurnStarted{}, err
	}
	if turn.VersionID > 0 && (!target.Agent || target.VersionID != turn.VersionID) {
		return ThreadTurnStarted{}, ErrThreadVersion
	}
	if !target.Idle {
		return ThreadTurnStarted{}, ErrThreadBusy
	}
	grant := threadGrant(turn.Grant)
	var outcome agentexecution.CurrentApplicationStartOutcome
	if target.Agent {
		outcome, err = s.starter.StartCurrentApplication(ctx, agentexecution.CurrentApplicationStartRequest{
			ProjectID: turn.ProjectID, ActorUserID: turn.ActorUserID, ConversationUUID: target.ConversationUUID,
			TargetParticipantID: target.ParticipantID, QuestionID: turn.QuestionID, UserInput: turn.Prompt,
			TriggerOrigin: turn.TriggerOrigin, Unattended: grant, ThreadRun: turn.Run,
		})
	} else {
		outcome, err = s.starter.StartCurrentAdhoc(ctx, agentexecution.CurrentAdhocStartRequest{
			ProjectID: turn.ProjectID, ActorUserID: turn.ActorUserID, ConversationUUID: target.ConversationUUID,
			TargetParticipantID: target.ParticipantID, QuestionID: turn.QuestionID, UserInput: turn.Prompt,
			LLMSettings: emptyLLMSettings, TriggerOrigin: turn.TriggerOrigin, Unattended: grant, ThreadRun: turn.Run,
		})
	}
	if err != nil {
		return ThreadTurnStarted{}, s.threadStartError(ctx, turn, err)
	}
	return ThreadTurnStarted{
		ExecutionID: outcome.ExecutionID, QuestionID: turn.QuestionID,
		ResponseMessageID: outcome.ResponseMessageID, ConversationUUID: target.ConversationUUID,
	}, nil
}

// threadStartError names a refused start that a busy thread caused. The
// fence closed after the read (a take back, a release or a turn that
// started), or the conversation's answer streams: the occurrence is
// skipped, not failed.
func (s *Service) threadStartError(ctx context.Context, turn ThreadTurn, err error) error {
	switch {
	case errors.Is(err, agentexecution.ErrCloudTurnRunning), errors.Is(err, agentexecution.ErrLocalWorkThread):
		return fmt.Errorf("%w: %w", ErrThreadBusy, err)
	case errors.Is(err, agentexecution.ErrUnsupportedCurrentAgentStart):
		again, readErr := s.threads.ThreadTarget(ctx, turn.ProjectID, turn.ConversationUUID)
		if readErr == nil && !again.Idle {
			return fmt.Errorf("%w: %w", ErrThreadBusy, err)
		}
	}
	return err
}

// threadGrant is the occurrence's grant for a new cloud chain: the desktop
// parts (the tool call cap, hook asks, remote confirmations) are dropped,
// and the grant id is derived by the start from the occurrence's question.
// Named sensitive tools stay: the start refuses them for a schedule or a
// webhook (ADR-0041 decision 5), so a stored grant cannot widen a run.
func threadGrant(grant *unattended.Request) *unattended.Request {
	if grant == nil {
		return nil
	}
	request := *grant
	request.GrantID = ""
	request.ChainRootExecutionID = ""
	request.Kind = ""
	request.ContinueToolCallCap = false
	request.HookAsks = false
	request.RemoteConfirmations = false
	request.SensitiveTools = append([]unattended.Tool(nil), grant.SensitiveTools...)
	request.BrowserSubmits = append([]string(nil), grant.BrowserSubmits...)
	return &request
}
