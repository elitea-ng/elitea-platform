package agentexecution

// The after-settle floor hook (ADR-0050 decisions 7c and 7d, issue 1611).
//
// A human message's turns run one at a time: when an agent's run of an
// exchange settles, its floor turn is DONE and the exchange's lowest QUEUED
// turn starts, as the person, with the answering agent's own grants. A run
// that failed, was cancelled, or paused for a person skips the exchange's
// remaining turns: the person decides what happens next.
//
// The hook fires on replays too. It is idempotent: a QUEUED turn's start is
// keyed by its turn question id (the admission's idempotency key), and the
// admission moves the turn QUEUED -> STARTED in its own transaction, so a
// turn starts once.

import (
	"context"
	"errors"
	"log/slog"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

// Skip codes of the floor hook (agent_floor_turns.skip_code).
const (
	FloorSkipFailed    = "failed"
	FloorSkipCancelled = "cancelled"
	FloorSkipPaused    = "paused"
	FloorSkipRefused   = "refused"
)

// FloorExchangeKey names one exchange.
type FloorExchangeKey struct {
	ProjectID        int64
	ConversationUUID string
	QuestionID       string
}

// QueuedFloorTurn is an exchange's next QUEUED turn.
type QueuedFloorTurn struct {
	Position          int
	TurnQuestionID    string
	ToParticipantID   int64
	FromParticipantID int64
	FromName          string
	Cause             string
	// Message is the handing agent's message; empty for an addressed agent.
	Message string
}

// FloorSettleView is what the hook reads of a settled run of an exchange.
type FloorSettleView struct {
	Exchange      FloorExchangeKey
	OwnerUserID   int64
	TriggerOrigin string
	Paused        bool
	IsError       bool
	// StepLimitReached and OutputLimitReached: the run stopped at a limit
	// before its answer was done.
	StepLimitReached   bool
	OutputLimitReached bool
	// Busy: another turn of the exchange is still STARTED.
	Busy bool
	// Chain is the run's unattended chain link; nil without one.
	Chain *unattended.Chain
	// Next is the exchange's next QUEUED turn; nil when none, when the
	// exchange stopped, or when a newer human message superseded it.
	Next *QueuedFloorTurn
}

// FloorSettleStore is the hook's store (repos.AgentFloorRepository).
type FloorSettleStore interface {
	// FloorSettle marks the run's turn DONE and reads the view; false for
	// a run that is no turn of an exchange.
	FloorSettle(ctx context.Context, executionID string) (FloorSettleView, bool, error)
	SkipFloorTurns(ctx context.Context, exchange FloorExchangeKey, code string) error
}

// FloorStarter starts a floor turn (CurrentApplicationStartService).
type FloorStarter interface {
	StartCurrentApplication(ctx context.Context, request CurrentApplicationStartRequest) (CurrentApplicationStartOutcome, error)
}

// FloorSettleHook is the after-settle hook.
type FloorSettleHook struct {
	store   FloorSettleStore
	starter FloorStarter
	chains  FloorChainEnder
	logger  *slog.Logger
}

// FloorChainEnder ends the message's chain when its next floor turn cannot
// start: the unattended chain hook left it open for that turn.
type FloorChainEnder interface {
	EndChain(ctx context.Context, root, state, limit string) (bool, error)
	EndRefusedChain(ctx context.Context, root string, refusal *unattended.ChainLimitError) error
}

// NewFloorSettleHook builds the hook. chains may be nil (no chain ends).
func NewFloorSettleHook(store FloorSettleStore, starter FloorStarter, chains FloorChainEnder, logger *slog.Logger) (*FloorSettleHook, error) {
	if store == nil || starter == nil {
		return nil, errors.New("floor settle hook dependencies are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &FloorSettleHook{store: store, starter: starter, chains: chains, logger: logger}, nil
}

// AfterSettle is an executionapp.AfterSettleHook.
func (h *FloorSettleHook) AfterSettle(ctx context.Context, proposal executionapp.SettlementProposal, _ executionapp.SettlementReceipt) {
	if h == nil || proposal.Outcome == executionapp.SettlementOutcomeUnknown ||
		proposal.TerminalLogicalOutputID != "agent-execution:"+proposal.Fence.ExecutionID {
		return
	}
	if err := h.Settle(ctx, proposal.Fence.ExecutionID, proposal.Outcome); err != nil && ctx.Err() == nil {
		h.logger.WarnContext(ctx, "turn taking: settle", "execution_id", proposal.Fence.ExecutionID, "err", err)
	}
}

// Settle finishes the run's floor turn and starts the exchange's next one.
func (h *FloorSettleHook) Settle(ctx context.Context, executionID string, outcome executionapp.SettlementOutcome) error {
	view, found, err := h.store.FloorSettle(ctx, executionID)
	if err != nil || !found {
		return err
	}
	switch {
	case outcome == executionapp.SettlementFailed || view.IsError:
		return h.store.SkipFloorTurns(ctx, view.Exchange, FloorSkipFailed)
	case outcome == executionapp.SettlementCancelled:
		return h.store.SkipFloorTurns(ctx, view.Exchange, FloorSkipCancelled)
	case view.Paused:
		return h.store.SkipFloorTurns(ctx, view.Exchange, FloorSkipPaused)
	case view.StepLimitReached || view.OutputLimitReached:
		if continuesAtLimit(view) {
			// The unattended chain continues this turn; the settle of its
			// continuation (a run of the same exchange) starts the next.
			return nil
		}
		// The person continues it: the turn is not done.
		return h.store.SkipFloorTurns(ctx, view.Exchange, FloorSkipPaused)
	case view.Next == nil || view.Busy:
		return nil
	}
	next := *view.Next
	request := CurrentApplicationStartRequest{
		ProjectID: view.Exchange.ProjectID, ActorUserID: view.OwnerUserID,
		ConversationUUID:    view.Exchange.ConversationUUID,
		TargetParticipantID: next.ToParticipantID,
		QuestionID:          next.TurnQuestionID,
		UserInput:           next.Message,
		TriggerOrigin:       executiondomain.TriggerOrigin(view.TriggerOrigin),
		FloorTurn: &FloorTurn{
			Cause: next.Cause, FromParticipantID: next.FromParticipantID, FromName: next.FromName,
			Position: next.Position,
		},
	}
	if next.Cause == turntaking.CauseHumanMessage {
		// The person's question is in the agent's history already (the
		// first agent answered it); the turn's own input says it is for
		// this agent too.
		request.FloorTurn.ReplyToQuestionID = view.Exchange.QuestionID
		request.UserInput = turntaking.AddressedNote
	}
	if view.Chain != nil {
		request.UnattendedContinuation = &UnattendedContinuation{Chain: *view.Chain, Kind: unattended.LinkAgentTurn}
	}
	_, err = h.starter.StartCurrentApplication(ctx, request)
	return h.started(ctx, view, err)
}

// continuesAtLimit: the run's chain continues it at the limit it stopped at
// (the unattended chain hook's own rule).
func continuesAtLimit(view FloorSettleView) bool {
	if view.Chain == nil || view.Chain.EndState != "" {
		return false
	}
	if view.OutputLimitReached {
		return view.Chain.Grant.ContinueOutputLimit
	}
	return view.Chain.Grant.ContinueStepLimit
}

// started classifies a floor turn's start error as the unattended chain
// hook classifies a continuation's: a replay that finds the turn started
// is done, a chain limit ends the chain and skips the exchange, and any
// other refusal skips the exchange. An infrastructure error stays for a
// replay of the settle.
func (h *FloorSettleHook) started(ctx context.Context, view FloorSettleView, err error) error {
	var limit *unattended.ChainLimitError
	var refusal *unattended.RefusalError
	switch {
	case err == nil, errors.Is(err, executionapp.ErrIdempotencyConflict), errors.Is(err, ErrFloorTurnNotQueued):
		return nil
	case errors.As(err, &limit):
		if h.chains != nil && view.Chain != nil {
			if endErr := h.chains.EndRefusedChain(ctx, view.Chain.RootExecutionID, limit); endErr != nil {
				return endErr
			}
		}
		return h.store.SkipFloorTurns(ctx, view.Exchange, FloorSkipRefused)
	case errors.Is(err, unattended.ErrChainEnded), errors.As(err, &refusal),
		errors.Is(err, ErrInvalidCurrentAgentStart), errors.Is(err, ErrUnsupportedCurrentAgentStart):
		h.logger.WarnContext(ctx, "turn taking: the next turn was refused", "err", err)
		if h.chains != nil && view.Chain != nil && !errors.Is(err, unattended.ErrChainEnded) {
			if _, endErr := h.chains.EndChain(ctx, view.Chain.RootExecutionID, unattended.EndFailed, ""); endErr != nil {
				return endErr
			}
		}
		return h.store.SkipFloorTurns(ctx, view.Exchange, FloorSkipRefused)
	}
	return err
}
