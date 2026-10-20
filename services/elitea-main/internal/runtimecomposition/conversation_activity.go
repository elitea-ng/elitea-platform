package runtimecomposition

import (
	"context"
	"strconv"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/conversationactivity"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
)

// The conversation activity frames (client contract 1.12) start where a
// turn's state commits, on the replica that committed it:
//
//   - an agent admission (the turn's question and its streaming answer are
//     written in the admission transaction): the conversation runs;
//   - an agent result projected (an answer, or a pause for input), before
//     the settlement arrives;
//   - every settlement (completion, pause, failure, cancellation), which
//     also covers a result that never projected;
//   - a stop of the turn.
//
// Each wrapper calls the notifier only after its inner call succeeded. The
// notifier reads the committed state and publishes detached from the
// caller, so a slow or failed publish never delays or fails the turn.

// activityAdmissionStore notifies after an agent admission commits.
type activityAdmissionStore struct {
	next     agentexecutionapp.AtomicAdmissionStore
	activity *conversationactivity.Notifier
}

func (s activityAdmissionStore) AdmitAgentExecution(ctx context.Context, admission agentexecutionapp.Admission) (executionapp.AdmissionOutcome, error) {
	outcome, err := s.next.AdmitAgentExecution(ctx, admission)
	if err == nil {
		if project, parseErr := strconv.ParseInt(admission.Record.Job.ProjectionProjectID, 10, 64); parseErr == nil {
			s.activity.ConversationChanged(ctx, project, admission.Binding.ClientStreamID)
		}
	}
	return outcome, err
}

// activityAgentExecutionIngestor notifies after an agent result projects.
type activityAgentExecutionIngestor struct {
	next     outputAgentExecutionIngestor
	activity *conversationactivity.Notifier
}

func (i activityAgentExecutionIngestor) IngestAgent(ctx context.Context, frame outputapp.AgentExecutionFrame) (outputapp.ProjectionOutcome, error) {
	outcome, err := i.next.IngestAgent(ctx, frame)
	if err == nil {
		i.activity.ExecutionChanged(ctx, frame.Fence.ExecutionID)
	}
	return outcome, err
}

// activityCancellationStore notifies after a stop of a turn commits.
type activityCancellationStore struct {
	next     agentexecutionapp.CurrentAgentCancellationStore
	activity *conversationactivity.Notifier
}

func (s activityCancellationStore) CancelCurrentAgent(ctx context.Context, request agentexecutionapp.CurrentAgentCancelRequest) (agentexecutionapp.CurrentAgentCancelOutcome, error) {
	outcome, err := s.next.CancelCurrentAgent(ctx, request)
	if err == nil && !outcome.Deleted {
		s.activity.MessageChanged(ctx, request.ProjectID, request.ResponseMessageID)
	}
	return outcome, err
}

// activitySettlementHooks notifies after every settlement commits.
func activitySettlementHooks(activity *conversationactivity.Notifier) []executionapp.SettlementOption {
	if activity == nil {
		return nil
	}
	return []executionapp.SettlementOption{executionapp.WithAfterSettleHooks(
		func(ctx context.Context, proposal executionapp.SettlementProposal, _ executionapp.SettlementReceipt) {
			activity.ExecutionChanged(ctx, proposal.Fence.ExecutionID)
		})}
}
