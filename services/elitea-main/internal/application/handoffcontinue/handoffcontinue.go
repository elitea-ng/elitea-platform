// Package handoffcontinue starts the cloud turn a send continues with
// (ADR-0045 decision 8, #1439).
//
// A complete with `continue` names the prompt and, when the thread runs
// unattended, its grant. The starter starts the turn as a message posted on
// the handed-off thread would, against the participant the thread's last
// local turn addressed, with the send's completion
// (agentexecution.HandoffCompletion) so the admission transaction completes
// the send and starts the cloud turn with it.
//
// The grant:
//
//   - with chain_root_execution_id, the turn continues that chain as a
//     `handoff` link: the chain's grant, owner and limits, which count across
//     both executors. The grant id must be the chain's, and a desktop link
//     kind has no place here. A chain at a limit refuses the turn and ends
//     as limit_reached.
//   - without one, the turn starts a new cloud chain. The desktop-only parts
//     of the grant (the tool call cap, hook asks, remote confirmations) are
//     dropped: a cloud run has nothing that answers them.
package handoffcontinue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
)

// Target is the thread a send continues and the participant that answers.
type Target struct {
	ConversationUUID string
	ParticipantID    int64
	// Agent: the participant is an agent (application); else the model
	// (dummy) participant.
	Agent bool
}

// Targets resolves a send's target (repos.WorkspaceHandoffsRepository).
// The caller must own the send.
type Targets interface {
	HandoffContinueTarget(ctx context.Context, projectID int64, handoffID string, actorUserID int64) (Target, error)
}

// Chains reads a chain's latest link and ends a refused one
// (repos.RunChainsRepository).
type Chains interface {
	LatestChainLink(ctx context.Context, root string) (unattended.Chain, bool, error)
	EndRefusedChain(ctx context.Context, root string, refusal *unattended.ChainLimitError) error
}

// Starter is the chat start use case.
type Starter interface {
	StartCurrentApplication(
		ctx context.Context, request agentexecution.CurrentApplicationStartRequest,
	) (agentexecution.CurrentApplicationStartOutcome, error)
	StartCurrentAdhoc(
		ctx context.Context, request agentexecution.CurrentAdhocStartRequest,
	) (agentexecution.CurrentApplicationStartOutcome, error)
}

// Service is the workspacehandoff.ContinueStarter.
type Service struct {
	targets Targets
	chains  Chains
	starter Starter
	// threads serves schedule and webhook turns (thread.go); nil without
	// WithThreads.
	threads Threads
	logger  *slog.Logger
}

var _ workspacehandoff.ContinueStarter = (*Service)(nil)

// New builds the starter. It answers nil when a part is missing.
func New(targets Targets, chains Chains, starter Starter, logger *slog.Logger) *Service {
	if targets == nil || chains == nil || starter == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{targets: targets, chains: chains, starter: starter, logger: logger}
}

// emptyLLMSettings: a model turn takes its settings from the participant.
var emptyLLMSettings = json.RawMessage(`{}`)

// StartContinue completes the send and starts its cloud turn in one
// admission transaction.
func (s *Service) StartContinue(
	ctx context.Context, start workspacehandoff.ContinueStart,
) (workspacehandoff.ContinuedTurn, error) {
	target, err := s.targets.HandoffContinueTarget(ctx, start.ProjectID, start.HandoffID, start.ActorUserID)
	if err != nil {
		return workspacehandoff.ContinuedTurn{}, err
	}
	request, continuation, err := s.grant(ctx, start.Grant)
	if err != nil {
		return workspacehandoff.ContinuedTurn{}, err
	}
	questionID := agentexecution.HandoffContinueQuestionID(start.HandoffID, start.Epoch)
	completion := &agentexecution.HandoffCompletion{
		HandoffID: start.HandoffID, Epoch: start.Epoch, HandoffRef: start.HandoffRef, SHA: start.SHA,
		TakeBackWhenDone: start.TakeBackWhenDone,
	}
	var outcome agentexecution.CurrentApplicationStartOutcome
	if target.Agent {
		outcome, err = s.starter.StartCurrentApplication(ctx, agentexecution.CurrentApplicationStartRequest{
			ProjectID: start.ProjectID, ActorUserID: start.ActorUserID, ConversationUUID: target.ConversationUUID,
			TargetParticipantID: target.ParticipantID, QuestionID: questionID, UserInput: start.Prompt,
			Unattended: request, UnattendedContinuation: continuation, HandoffCompletion: completion,
		})
	} else {
		outcome, err = s.starter.StartCurrentAdhoc(ctx, agentexecution.CurrentAdhocStartRequest{
			ProjectID: start.ProjectID, ActorUserID: start.ActorUserID, ConversationUUID: target.ConversationUUID,
			TargetParticipantID: target.ParticipantID, QuestionID: questionID, UserInput: start.Prompt,
			LLMSettings: emptyLLMSettings,
			Unattended:  request, UnattendedContinuation: continuation, HandoffCompletion: completion,
		})
	}
	var limit *unattended.ChainLimitError
	if continuation != nil && errors.As(err, &limit) {
		// The chain is at a limit: it ends there, as a cloud continuation's
		// refused start ends it. The send stays HANDING_OFF.
		if endErr := s.chains.EndRefusedChain(ctx, continuation.Chain.RootExecutionID, limit); endErr != nil {
			s.logger.WarnContext(ctx, "workspace handoff: end the refused chain",
				"chain_root_execution_id", continuation.Chain.RootExecutionID, "err", endErr)
		}
	}
	if err != nil {
		return workspacehandoff.ContinuedTurn{}, handoffError(err)
	}
	return workspacehandoff.ContinuedTurn{
		ExecutionID: outcome.ExecutionID, QuestionID: questionID, ResponseMessageID: outcome.ResponseMessageID,
		ConversationUUID: target.ConversationUUID,
	}, nil
}

// handoffError names a refused start in the handoff's terms, keeping the
// cause: the grant and chain refusals pass as they are (the route answers
// them as a local turn's), the rest map onto the handoff's codes.
func handoffError(err error) error {
	var capacity *executionapp.AdmissionCapacityError
	switch {
	case errors.Is(err, agentexecution.ErrLocalWorkThread), errors.As(err, &capacity):
		// No cloud executor to run the turn, or no room for it now.
		return fmt.Errorf("%w: %w", workspacehandoff.ErrUnavailable, err)
	case errors.Is(err, agentexecution.ErrInvalidCurrentAgentStart), errors.Is(err, agentexecution.ErrInvalidAgentAdmission),
		errors.Is(err, agentexecution.ErrUnsupportedCurrentAgentStart), errors.Is(err, executionapp.ErrInvalidAdmission):
		return fmt.Errorf("%w: %w", workspacehandoff.ErrInvalid, err)
	case errors.Is(err, executionapp.ErrIdempotencyConflict):
		// The question of this send at this epoch is taken.
		return fmt.Errorf("%w: %w", workspacehandoff.ErrStaleEpoch, err)
	}
	return err
}

// grant answers the turn's new grant, or the chain it continues.
func (s *Service) grant(
	ctx context.Context, grant *unattended.Request,
) (*unattended.Request, *agentexecution.UnattendedContinuation, error) {
	if grant == nil {
		return nil, nil, nil
	}
	root := grant.ChainRootExecutionID
	if root == "" {
		request := *grant
		request.ContinueToolCallCap = false
		request.HookAsks = false
		request.RemoteConfirmations = false
		request.SensitiveTools = append([]unattended.Tool(nil), grant.SensitiveTools...)
		return &request, nil, nil
	}
	if grant.Kind != "" {
		return nil, nil, unattended.Refused("kind", "", unattended.RefusedInvalid)
	}
	chain, found, err := s.chains.LatestChainLink(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		return nil, nil, unattended.Refused("chain_root_execution_id", "", unattended.RefusedChainNotFound)
	}
	if chain.GrantID != grant.GrantID {
		return nil, nil, unattended.Refused("grant_id", "", unattended.RefusedInvalid)
	}
	return nil, &agentexecution.UnattendedContinuation{Chain: chain, Kind: unattended.LinkHandoff}, nil
}
