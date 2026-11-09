package storage

import (
	"context"
	"errors"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentworkforce"
)

// The propose_dismissal tool (ADR-0050 decision 5j, issue 1616), served on
// the private claim-bound listener as propose_agent is
// (runtime_propose_agent.go).
//
// WHAT AUTHORIZES A CALL. The claim, through the agent-scoped authorizer.
// The project, the proposing agent and the person the run served come from
// the claimed execution; the request names only the target agent, the
// stage, the rationale and a demotion's narrowing.
//
// WHAT MAIN DECIDES, not the worker: that the proposer is an HR agent (the
// frozen version's meta.capabilities.can_review_agents), and every guard of
// decision 5j against the target (repos.AgentWorkforce.ProposeDismissal).
// NOTHING IS APPLIED: the row is a proposal a person decides.
const (
	RuntimeProposeDismissalSchemaVersion = "elitea.runtime.propose-dismissal.v1"

	maxRuntimeProposeDismissalRequestBytes  = 16 * 1024
	maxRuntimeProposeDismissalResponseBytes = 4 * 1024

	runtimeContextStageProposeDismissalRun   = "propose_dismissal_run"
	runtimeContextStageProposeDismissalWrite = "propose_dismissal_write"
)

// DismissalRequest is the propose_dismissal request document.
type DismissalRequest struct {
	ApplicationID int64                    `json:"application_id"`
	Stage         string                   `json:"stage"`
	Rationale     string                   `json:"rationale"`
	Narrowing     agentworkforce.Narrowing `json:"narrowing"`
}

// RuntimeProposeDismissalResult is the response. Its fields are the
// complete set the worker accepts.
type RuntimeProposeDismissalResult struct {
	SchemaVersion string `json:"schema_version"`
	// Status is an agentworkforce Outcome code.
	Status     string `json:"status"`
	ProposalID string `json:"proposal_id,omitempty"`
	Message    string `json:"message"`
}

// ProposeDismissalRun is what the claimed execution says about a call.
type ProposeDismissalRun struct {
	ProjectID        int64
	ExecutionID      string
	ConversationUUID string
	ApplicationID    int64
	ActorID          int64
	IsHRAgent        bool
}

// DismissalOutcome is what the store answers.
type DismissalOutcome struct {
	Status     string
	ProposalID int64
	Message    string
}

// ProposeDismissalSink is the shared side (repos.AgentWorkforce).
type ProposeDismissalSink interface {
	ResolveProposeDismissalRun(ctx context.Context, projectID int64, executionID string, generation uint64) (ProposeDismissalRun, error)
	ProposeDismissal(ctx context.Context, run ProposeDismissalRun, request DismissalRequest) (DismissalOutcome, error)
}

// RuntimeProposeDismissalService serves the propose_dismissal route.
type RuntimeProposeDismissalService struct {
	authorizer AgentRuntimeContextAuthorizer
	sink       ProposeDismissalSink
}

// NewRuntimeProposeDismissalService builds the service.
func NewRuntimeProposeDismissalService(authorizer AgentRuntimeContextAuthorizer, sink ProposeDismissalSink) (*RuntimeProposeDismissalService, error) {
	if authorizer == nil || sink == nil {
		return nil, errors.New("runtime propose dismissal dependencies are required")
	}
	return &RuntimeProposeDismissalService{authorizer: authorizer, sink: sink}, nil
}

func dismissalResult(status, message string) RuntimeProposeDismissalResult {
	return RuntimeProposeDismissalResult{SchemaVersion: RuntimeProposeDismissalSchemaVersion, Status: status, Message: message}
}

// Propose stores one proposal from the claimed run's HR agent.
func (service *RuntimeProposeDismissalService) Propose(ctx context.Context, claim ContentClaim, request DismissalRequest) (RuntimeProposeDismissalResult, error) {
	if service == nil || service.authorizer == nil || service.sink == nil {
		return RuntimeProposeDismissalResult{}, runtimeContextUnavailable(runtimeContextStageProposeDismissalRun)
	}
	if request.ApplicationID <= 0 {
		return dismissalResult(agentworkforce.OutcomeInvalid, "application_id names the agent, a positive id."), nil
	}
	builders := RuntimeEntityBuilderService{authorizer: service.authorizer}
	authorization, err := builders.authorizeBuilder(ctx, claim)
	if err != nil {
		return RuntimeProposeDismissalResult{}, err
	}
	run, err := service.sink.ResolveProposeDismissalRun(ctx, authorization.ResourceProjectID, claim.ExecutionID, claim.Generation)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return RuntimeProposeDismissalResult{}, contextErr
		}
		if errors.Is(err, ErrContentUnauthorized) || errors.Is(err, ErrContentRejected) {
			return RuntimeProposeDismissalResult{}, err
		}
		return RuntimeProposeDismissalResult{}, runtimeContextUnavailable(runtimeContextStageProposeDismissalRun)
	}
	if run.ProjectID != authorization.ResourceProjectID ||
		(authorization.ConversationID != "" && run.ConversationUUID != authorization.ConversationID) ||
		strconv.FormatInt(run.ActorID, 10) != authorization.ActorID {
		return RuntimeProposeDismissalResult{}, ErrContentUnauthorized
	}
	if run.ApplicationID <= 0 || !run.IsHRAgent {
		return dismissalResult(agentworkforce.OutcomeNotAllowed,
			"This agent may not propose dismissals: its version does not set capabilities.can_review_agents."), nil
	}
	outcome, err := service.sink.ProposeDismissal(ctx, run, request)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return RuntimeProposeDismissalResult{}, contextErr
		}
		return RuntimeProposeDismissalResult{}, runtimeContextUnavailable(runtimeContextStageProposeDismissalWrite)
	}
	message := outcome.Message
	if outcome.Status == agentworkforce.OutcomeProposed {
		message = "Proposed. An agent manager reviews it and decides; nothing changes until a person approves. Tell the person you proposed it and why."
	}
	if message == "" {
		message = "No proposal was made."
	}
	result := dismissalResult(outcome.Status, message)
	if outcome.ProposalID > 0 {
		result.ProposalID = strconv.FormatInt(outcome.ProposalID, 10)
	}
	return result, nil
}
