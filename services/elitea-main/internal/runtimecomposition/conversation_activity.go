package runtimecomposition

import (
	"context"
	"strconv"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/conversationactivity"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
)

// The conversation activity frames (client contract 1.12) start where a
// turn's state commits, on the replica that committed it:
//
//   - an agent admission (the turn's question and its streaming answer are
//     written in the admission transaction): the conversation runs;
//   - the settlement of an agent execution (completion, pause, failure,
//     cancellation). The settlement protocol commits only after the
//     execution's terminal output (its answer, its pause or its failure)
//     projected, so the settlement alone sees every end of a turn: the
//     projection itself sends nothing, or each end would send two frames;
//   - a stop of the turn.
//
// Each wrapper calls the notifier only after its inner call succeeded. The
// notifier reads the committed state and publishes detached from the
// caller, so a slow or failed publish never delays or fails the turn, and it
// drops a frame identical to the one it last sent for the conversation (an
// idempotent replay of a settlement).

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

// agentOutputPrefix is the logical output id prefix of an agent
// execution's terminal output (output.AgentExecutionFrame). A settlement
// commits only against the durable terminal output it names, so the prefix
// tells an agent (chat) execution apart from every other kind (an index
// ingest, a toolkit call, a configuration validation) without a read.
const agentOutputPrefix = "agent-execution:"

// activitySettlementHooks notifies after an agent execution's settlement
// commits. Other execution kinds have no conversation, and an unknown
// outcome (runtime recovery may run the execution again) changes nothing a
// list row shows.
func activitySettlementHooks(activity *conversationactivity.Notifier) []executionapp.SettlementOption {
	if activity == nil {
		return nil
	}
	return []executionapp.SettlementOption{executionapp.WithAfterSettleHooks(
		func(ctx context.Context, proposal executionapp.SettlementProposal, _ executionapp.SettlementReceipt) {
			if !changesConversationActivity(proposal) {
				return
			}
			activity.ExecutionChanged(ctx, proposal.Fence.ExecutionID)
		})}
}

// changesConversationActivity: the settlement ends a turn of an agent
// execution.
func changesConversationActivity(proposal executionapp.SettlementProposal) bool {
	return proposal.Outcome != executionapp.SettlementOutcomeUnknown &&
		proposal.TerminalLogicalOutputID == agentOutputPrefix+proposal.Fence.ExecutionID
}
