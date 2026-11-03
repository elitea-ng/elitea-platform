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

// Hook is the after-settle hook.
type Hook struct {
	chains  Chains
	starter Starter
	logger  *slog.Logger
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
	case view.StepLimitReached && chain.Grant.ContinueStepLimit:
		return h.continued(ctx, chain, h.continueStepLimit(ctx, chain))
	}
	return h.end(ctx, chain, unattended.EndCompleted)
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

func (h *Hook) continueStepLimit(ctx context.Context, chain unattended.Chain) error {
	start := chain.Grant.Start
	if start == nil {
		return unattended.Refused("unattended", "", unattended.RefusedInvalid)
	}
	continuation := &agentexecution.UnattendedContinuation{Chain: chain, Kind: unattended.LinkStepLimit}
	questionID := agentexecution.ContinuationQuestionID(chain.ExecutionID)
	var err error
	switch start.Contract {
	case agentexecution.CloudStartContractApplication:
		_, err = h.starter.StartCurrentApplication(ctx, agentexecution.CurrentApplicationStartRequest{
			ProjectID: chain.ProjectID, ActorUserID: chain.OwnerUserID, ConversationUUID: chain.ConversationUUID,
			TargetParticipantID: start.ParticipantID, QuestionID: questionID,
			UserInput: unattended.ContinuePrompt, TriggerOrigin: triggerOrigin(chain),
			UnattendedContinuation: continuation,
		})
	case agentexecution.CloudStartContractAdhoc:
		_, err = h.starter.StartCurrentAdhoc(ctx, agentexecution.CurrentAdhocStartRequest{
			ProjectID: chain.ProjectID, ActorUserID: chain.OwnerUserID, ConversationUUID: chain.ConversationUUID,
			TargetParticipantID: start.ParticipantID, QuestionID: questionID,
			UserInput: unattended.ContinuePrompt, LLMSettings: start.LLMSettings,
			TriggerOrigin: triggerOrigin(chain), UnattendedContinuation: continuation,
		})
	default:
		err = unattended.Refused("unattended", "", unattended.RefusedInvalid)
	}
	return err
}
