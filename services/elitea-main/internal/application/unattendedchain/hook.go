// Package unattendedchain continues and ends unattended cloud chains
// (ADR-0045 decision 4, #1436).
//
// When a chain's run settles, the hook reads the run's response. A response
// that stopped at the output limit continues through the existing
// output_limit continuation when the grant asks for it. A run that stopped
// at its step limit continues with a new question (ContinuePrompt) when the
// grant asks for it. Each continuation passes the chain check in its
// admission transaction; a refused one ends the chain as limit_reached.
// Each continuation also checks approvals.unattended_grants again: when an
// administrator turned grants off, Main does not start it and the chain
// ends as cancelled.
// Every other settle ends the chain: completed, failed, waiting (a pause
// waits for a person) or cancelled.
//
// A run that would end the chain as completed first passes the goal check
// (ADR-0045 decision 11, runbrief) when the chain has a brief with
// conditions: met ends it completed, not_met starts a goal_not_met
// continuation with the check's text, blocked ends it failed, and a check
// that could not run ends it completed. A run that stopped at a limit
// without the grant to continue it ends the chain waiting, unchecked. With a ready brief, a step-limit
// continuation's text restates the brief (runbrief.ContinuationText).
//
// A settle hook fires on replays too. The hook is idempotent: it acts only
// on the chain's latest run of a chain that has not ended, each
// continuation's request is derived from the run it continues (the same
// question id, the same output-limit sequence), and the end state is
// written once.
package unattendedchain

import (
	"context"
	"errors"
	"log/slog"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runbrief"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

// SettleView is a settled chain run (repos.ChainSettleView).
type SettleView struct {
	Chain               unattended.Chain
	Latest              bool
	OutputLimitReached  bool
	OutputLimitSequence int64
	StepLimitReached    bool
	Paused              bool
	IsError             bool
	// ParticipantID is the agent the settled run answered as; 0 when not
	// recorded.
	ParticipantID int64
}

// Chains reads and ends chains.
type Chains interface {
	SettleView(ctx context.Context, executionID string) (SettleView, bool, error)
	EndChain(ctx context.Context, root, state, limit string) (bool, error)
	EndRefusedChain(ctx context.Context, root string, refusal *unattended.ChainLimitError) error
}

// Starter starts the continuations.
type Starter interface {
	StartCurrentApplication(
		ctx context.Context, request agentexecution.CurrentApplicationStartRequest,
	) (agentexecution.CurrentApplicationStartOutcome, error)
	StartCurrentAdhoc(
		ctx context.Context, request agentexecution.CurrentAdhocStartRequest,
	) (agentexecution.CurrentApplicationStartOutcome, error)
	ContinueCurrentAgent(
		ctx context.Context, request agentexecution.CurrentContinuationRequest,
	) (agentexecution.CurrentApplicationStartOutcome, error)
}

// Goals is the run brief and the goal check (runbrief.Service).
type Goals interface {
	// ContinueText is the user text of a continuation of the chain.
	ContinueText(ctx context.Context, root string) string
	// CheckCloud checks the goal after a run that would end the chain as
	// completed; false when the chain has no check.
	CheckCloud(ctx context.Context, root, executionID string) (runbrief.Result, bool, error)
}

// Floor reports whether a conversation still owes a floor turn other than
// the settling run's (ADR-0050 decision 7c, repos.AgentFloorRepository): a
// chain whose message still owes an agent turn, queued or already started
// by the floor hook, does not end as completed; that turn is a link of the
// chain and its own settle ends it.
type Floor interface {
	PendingFloorTurns(ctx context.Context, projectID int64, conversationUUID, settlingExecutionID string) (bool, error)
}

// Hook is the after-settle hook.
type Hook struct {
	chains  Chains
	starter Starter
	goals   Goals
	floor   Floor
	logger  *slog.Logger
}

// WithFloor adds the turn-taking gate. Without it, a chain ends as
// completed when its run would end it.
func (h *Hook) WithFloor(floor Floor) *Hook {
	if h != nil {
		h.floor = floor
	}
	return h
}

// WithGoals adds the run brief and the goal check. Without it, every
// continuation says unattended.ContinuePrompt and a chain ends completed
// with no check.
func (h *Hook) WithGoals(goals Goals) *Hook {
	if h != nil {
		h.goals = goals
	}
	return h
}

// New builds the hook. It answers nil when a part is missing.
func New(chains Chains, starter Starter, logger *slog.Logger) *Hook {
	if chains == nil || starter == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Hook{chains: chains, starter: starter, logger: logger}
}

// agentOutputPrefix is the terminal output prefix of an agent execution.
const agentOutputPrefix = "agent-execution:"

// AfterSettle is the executionapp.AfterSettleHook.
func (h *Hook) AfterSettle(ctx context.Context, proposal executionapp.SettlementProposal, _ executionapp.SettlementReceipt) {
	if h == nil || proposal.Outcome == executionapp.SettlementOutcomeUnknown ||
		proposal.TerminalLogicalOutputID != agentOutputPrefix+proposal.Fence.ExecutionID {
		return
	}
	if err := h.Settle(ctx, proposal.Fence.ExecutionID, proposal.Outcome); err != nil {
		h.logger.ErrorContext(ctx, "unattended chain: settle", "execution_id", proposal.Fence.ExecutionID, "err", err)
	}
}

// Settle continues or ends the chain of a settled run.
func (h *Hook) Settle(ctx context.Context, executionID string, outcome executionapp.SettlementOutcome) error {
	view, found, err := h.chains.SettleView(ctx, executionID)
	if err != nil || !found {
		return err
	}
	chain := view.Chain
	if chain.EndState != "" || !view.Latest || chain.Origin != unattended.OriginCloud {
		// A desktop chain ends at its last commit; an ended chain or an
		// older run has nothing to do.
		return nil
	}
	switch {
	case outcome == executionapp.SettlementFailed:
		return h.end(ctx, chain, unattended.EndFailed)
	case outcome == executionapp.SettlementCancelled:
		return h.end(ctx, chain, unattended.EndCancelled)
	case view.IsError:
		return h.end(ctx, chain, unattended.EndFailed)
	case view.Paused:
		return h.end(ctx, chain, unattended.EndWaiting)
	case view.OutputLimitReached && view.OutputLimitSequence > 0:
		if !chain.Grant.ContinueOutputLimit {
			// The person continues it: the chain waits.
			return h.end(ctx, chain, unattended.EndWaiting)
		}
		return h.continued(ctx, chain, h.continueOutputLimit(ctx, chain))
	case view.StepLimitReached:
		if !chain.Grant.ContinueStepLimit {
			// The person continues it: the chain waits, with no goal check
			// (the run did not finish its work).
			return h.end(ctx, chain, unattended.EndWaiting)
		}
		return h.continued(ctx, chain, h.startNext(ctx, chain, view.ParticipantID, unattended.LinkStepLimit,
			agentexecution.ContinuationQuestionID(chain.ExecutionID), h.continueText(ctx, chain)))
	}
	return h.finish(ctx, chain, view.ParticipantID)
}

// finish ends a chain whose run would end it as completed, after the goal
// check when the chain has one. A check that an infrastructure error stopped
// ends the chain completed ("not checked"); only a continuation's start
// error stays for a replay.
func (h *Hook) finish(ctx context.Context, chain unattended.Chain, participant int64) error {
	if h.floor != nil {
		pending, err := h.floor.PendingFloorTurns(ctx, chain.ProjectID, chain.ConversationUUID, chain.ExecutionID)
		if err != nil {
			return err
		}
		if pending {
			// The message's next agent turn continues the chain.
			return nil
		}
	}
	if h.goals == nil {
		return h.end(ctx, chain, unattended.EndCompleted)
	}
	result, checked, err := h.goals.CheckCloud(ctx, chain.RootExecutionID, chain.ExecutionID)
	if err != nil {
		h.logger.WarnContext(ctx, "unattended chain: goal check failed; the chain ends not checked",
			"chain_root_execution_id", chain.RootExecutionID, "execution_id", chain.ExecutionID, "err", err)
		return h.end(ctx, chain, unattended.EndCompleted)
	}
	if !checked {
		return h.end(ctx, chain, unattended.EndCompleted)
	}
	switch result.Outcome {
	case runbrief.OutcomeBlocked:
		return h.end(ctx, chain, unattended.EndFailed)
	case runbrief.OutcomeNotMet:
		return h.continued(ctx, chain, h.startNext(ctx, chain, participant, unattended.LinkGoalNotMet,
			agentexecution.GoalContinuationQuestionID(chain.ExecutionID), result.ContinueText))
	}
	return h.end(ctx, chain, unattended.EndCompleted)
}

func (h *Hook) continueText(ctx context.Context, chain unattended.Chain) string {
	if h.goals == nil {
		return unattended.ContinuePrompt
	}
	return h.goals.ContinueText(ctx, chain.RootExecutionID)
}

func (h *Hook) end(ctx context.Context, chain unattended.Chain, state string) error {
	_, err := h.chains.EndChain(ctx, chain.RootExecutionID, state, "")
	return err
}

// continued classifies a continuation's start error: a limit ends the chain
// as limit_reached, an ended chain is done, grants that an administrator
// turned off end it as cancelled, and another refused start ends it as
// failed. Any other error stays: a replay of the settle tries again.
func (h *Hook) continued(ctx context.Context, chain unattended.Chain, err error) error {
	var limit *unattended.ChainLimitError
	var refusal *unattended.RefusalError
	switch {
	case err == nil, errors.Is(err, unattended.ErrChainEnded):
		return nil
	case errors.Is(err, executionapp.ErrIdempotencyConflict):
		// The question id comes from the settled run, so a replay that
		// finds it taken started this continuation already. The text can
		// differ when the brief became ready between the two settles.
		return nil
	case errors.As(err, &limit):
		return h.chains.EndRefusedChain(ctx, chain.RootExecutionID, limit)
	case errors.As(err, &refusal) && refusal.Reason == unattended.RefusedDisabled:
		h.logger.WarnContext(ctx, "unattended chain: grants are off, the chain is cancelled",
			"chain_root_execution_id", chain.RootExecutionID)
		return h.end(ctx, chain, unattended.EndCancelled)
	case errors.As(err, &refusal):
		h.logger.WarnContext(ctx, "unattended chain: continuation refused",
			"chain_root_execution_id", chain.RootExecutionID, "reason", refusal.Reason)
		return h.end(ctx, chain, unattended.EndFailed)
	}
	return err
}

func triggerOrigin(chain unattended.Chain) executiondomain.TriggerOrigin {
	return executiondomain.TriggerOrigin(chain.TriggerOrigin)
}

func (h *Hook) continueOutputLimit(ctx context.Context, chain unattended.Chain) error {
	_, err := h.starter.ContinueCurrentAgent(ctx, agentexecution.CurrentContinuationRequest{
		ProjectID: chain.ProjectID, ActorUserID: chain.OwnerUserID, ConversationUUID: chain.ConversationUUID,
		ResponseMessageID: chain.ResponseMessageID, Kind: agentexecution.CurrentContinuationOutputLimit,
		TriggerOrigin: triggerOrigin(chain),
		UnattendedContinuation: &agentexecution.UnattendedContinuation{
			Chain: chain, Kind: unattended.LinkOutputLimit,
		},
	})
	return err
}

// startNext starts a continuation that asks a new question: a step-limit
// or a goal_not_met continuation, with text as the question.
//
// participant is the agent of the settled run: in a multi-agent message's
// chain (ADR-0050 decision 7c) a later link answers as another agent than
// the root, and its continuation goes to it. 0 keeps the chain's start.
func (h *Hook) startNext(ctx context.Context, chain unattended.Chain, participant int64, kind, questionID, text string) error {
	if chain.Grant.Start == nil || text == "" {
		return unattended.Refused("unattended", "", unattended.RefusedInvalid)
	}
	start := *chain.Grant.Start
	if participant > 0 && start.Contract == agentexecution.CloudStartContractApplication {
		start.ParticipantID = participant
	}
	continuation := &agentexecution.UnattendedContinuation{Chain: chain, Kind: kind}
	var err error
	switch start.Contract {
	case agentexecution.CloudStartContractApplication:
		_, err = h.starter.StartCurrentApplication(ctx, agentexecution.CurrentApplicationStartRequest{
			ProjectID: chain.ProjectID, ActorUserID: chain.OwnerUserID, ConversationUUID: chain.ConversationUUID,
			TargetParticipantID: start.ParticipantID, QuestionID: questionID,
			UserInput: text, TriggerOrigin: triggerOrigin(chain),
			UnattendedContinuation: continuation,
			// A heartbeat chain's later runs keep the heartbeat floor:
			// unattended tools only (ADR-0050 decision 2).
			Heartbeat: start.Heartbeat,
		})
	case agentexecution.CloudStartContractAdhoc:
		_, err = h.starter.StartCurrentAdhoc(ctx, agentexecution.CurrentAdhocStartRequest{
			ProjectID: chain.ProjectID, ActorUserID: chain.OwnerUserID, ConversationUUID: chain.ConversationUUID,
			TargetParticipantID: start.ParticipantID, QuestionID: questionID,
			UserInput: text, LLMSettings: start.LLMSettings,
			TriggerOrigin: triggerOrigin(chain), UnattendedContinuation: continuation,
		})
	default:
		err = unattended.Refused("unattended", "", unattended.RefusedInvalid)
	}
	return err
}
