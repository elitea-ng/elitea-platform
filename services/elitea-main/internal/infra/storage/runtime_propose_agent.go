package storage

import (
	"context"
	"errors"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentproposal"
)

// The propose_agent tool (ADR-0050 decision 5a, issue 1615), served on the
// private claim-bound listener for the reason the builder writes are
// (runtime_entity_builder.go).
//
// WHAT AUTHORIZES A CALL. The claim, through the agent-scoped authorizer.
// The project, the proposing agent (agent_execution_jobs.application_id),
// the person the run served (execution_jobs.actor_id), the conversation and
// the run all come from the claimed execution; the request names only the
// spawn it promotes, the rationale, the profile and memory seeds.
//
// WHAT MAIN DECIDES, not the worker:
//
//   - the capability: the frozen version's meta.capabilities
//     .can_propose_agents, read from the run's input, not from the request;
//   - the person's models.applications.applications.create in the project,
//     resolved now;
//   - the spec digest: computed here from the reported profile with the
//     runtime's own algorithm (agentproposal.SpecDigest), so it is the
//     profile_digest the spawn's ephemeral_agents row carries once the run
//     settles. The evidence is that row, joined on read by (execution, agent
//     id); a proposal whose row never arrives, or carries another digest,
//     shows as unverified in the inbox;
//   - the limits (three open per agent, thirty days after a dismissal),
//     checked under the proposing agent's lock.
//
// NOTHING IS SAVED AS AN AGENT. The row is a proposal; Create is a person's
// click on the ordinary create route.
const (
	RuntimeProposeAgentSchemaVersion = "elitea.runtime.propose-agent.v1"

	maxRuntimeProposeAgentRequestBytes  = 32 * 1024
	maxRuntimeProposeAgentResponseBytes = 8 * 1024

	runtimeContextStageProposeAgentRun   = "propose_agent_run"
	runtimeContextStageProposeAgentWrite = "propose_agent_write"
)

// RuntimeProposeAgentResult is the propose_agent response. Its fields are
// the complete set the worker accepts.
type RuntimeProposeAgentResult struct {
	SchemaVersion string `json:"schema_version"`
	// Status is an agentproposal Outcome code.
	Status     string `json:"status"`
	ProposalID string `json:"proposal_id,omitempty"`
	// Message is the text the model reads.
	Message string `json:"message"`
}

// ProposeAgentRun is what the claimed execution says about a call.
type ProposeAgentRun struct {
	ProjectID        int64
	ExecutionID      string
	ConversationUUID string
	// ApplicationID is the proposing agent; 0 for an ad hoc run.
	ApplicationID int64
	ActorID       int64
	// CanPropose is the frozen version's capability.
	CanPropose bool
}

// ProposalWrite is one proposal to store.
type ProposalWrite struct {
	Request    agentproposal.Request
	SpecDigest string
	Draft      map[string]any
}

// ProposalOutcome is what the store answers.
type ProposalOutcome struct {
	Status     string
	ProposalID int64
}

// ProposeAgentSink is the shared side (repos.AgentProposals).
type ProposeAgentSink interface {
	ResolveProposeAgentRun(ctx context.Context, projectID int64, executionID string, generation uint64) (ProposeAgentRun, error)
	// Propose stores the proposal under the limits and answers the outcome.
	Propose(ctx context.Context, run ProposeAgentRun, write ProposalWrite) (ProposalOutcome, error)
}

// RuntimeProposeAgentService serves the propose_agent route.
type RuntimeProposeAgentService struct {
	authorizer  AgentRuntimeContextAuthorizer
	sink        ProposeAgentSink
	permissions auth.PermissionResolver
}

// NewRuntimeProposeAgentService builds the service.
func NewRuntimeProposeAgentService(authorizer AgentRuntimeContextAuthorizer, sink ProposeAgentSink, permissions auth.PermissionResolver) (*RuntimeProposeAgentService, error) {
	if authorizer == nil || sink == nil || permissions == nil {
		return nil, errors.New("runtime propose agent dependencies are required")
	}
	return &RuntimeProposeAgentService{authorizer: authorizer, sink: sink, permissions: permissions}, nil
}

func proposeResult(status, message string) RuntimeProposeAgentResult {
	return RuntimeProposeAgentResult{SchemaVersion: RuntimeProposeAgentSchemaVersion, Status: status, Message: message}
}

// Propose stores one proposal for the claimed run's agent.
func (service *RuntimeProposeAgentService) Propose(ctx context.Context, claim ContentClaim, request agentproposal.Request) (RuntimeProposeAgentResult, error) {
	if service == nil || service.authorizer == nil || service.sink == nil || service.permissions == nil {
		return RuntimeProposeAgentResult{}, runtimeContextUnavailable(runtimeContextStageProposeAgentRun)
	}
	checked, err := agentproposal.Check(request)
	if err != nil {
		var refused *agentproposal.Error
		if errors.As(err, &refused) {
			return proposeResult(refused.Code, refused.Message), nil
		}
		return RuntimeProposeAgentResult{}, ErrContentRejected
	}
	builders := RuntimeEntityBuilderService{authorizer: service.authorizer}
	authorization, err := builders.authorizeBuilder(ctx, claim)
	if err != nil {
		return RuntimeProposeAgentResult{}, err
	}
	run, err := service.sink.ResolveProposeAgentRun(ctx, authorization.ResourceProjectID, claim.ExecutionID, claim.Generation)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return RuntimeProposeAgentResult{}, contextErr
		}
		if errors.Is(err, ErrContentUnauthorized) || errors.Is(err, ErrContentRejected) {
			return RuntimeProposeAgentResult{}, err
		}
		return RuntimeProposeAgentResult{}, runtimeContextUnavailable(runtimeContextStageProposeAgentRun)
	}
	if run.ProjectID != authorization.ResourceProjectID ||
		(authorization.ConversationID != "" && run.ConversationUUID != authorization.ConversationID) ||
		strconv.FormatInt(run.ActorID, 10) != authorization.ActorID {
		return RuntimeProposeAgentResult{}, ErrContentUnauthorized
	}
	if run.ApplicationID <= 0 || !run.CanPropose {
		return proposeResult(agentproposal.OutcomeNotAllowed,
			"This agent may not propose agents: its version does not set capabilities.can_propose_agents."), nil
	}
	allowed, err := service.holdsCreate(ctx, run)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return RuntimeProposeAgentResult{}, contextErr
		}
		return RuntimeProposeAgentResult{}, runtimeContextUnavailable(runtimeContextStageProposeAgentRun)
	}
	if !allowed {
		return proposeResult(agentproposal.OutcomeNotAllowed,
			"The person you work for cannot create agents in this project, so no proposal was made."), nil
	}
	digest := agentproposal.SpecDigest(checked.Profile)
	outcome, err := service.sink.Propose(ctx, run, ProposalWrite{
		Request: checked, SpecDigest: digest, Draft: agentproposal.Draft(checked),
	})
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return RuntimeProposeAgentResult{}, contextErr
		}
		return RuntimeProposeAgentResult{}, runtimeContextUnavailable(runtimeContextStageProposeAgentWrite)
	}
	result := proposeResult(outcome.Status, proposeMessage(outcome.Status))
	if outcome.ProposalID > 0 {
		result.ProposalID = strconv.FormatInt(outcome.ProposalID, 10)
	}
	return result, nil
}

func (service *RuntimeProposeAgentService) holdsCreate(ctx context.Context, run ProposeAgentRun) (bool, error) {
	actor := strconv.FormatInt(run.ActorID, 10)
	resolution, err := service.permissions.ResolvePermissions(ctx, auth.User{ID: actor, UserID: actor},
		auth.PermissionModeDefault, strconv.FormatInt(run.ProjectID, 10))
	if errors.Is(err, auth.ErrPermissionDenied) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if resolution.UserID != run.ActorID {
		return false, nil
	}
	for _, permission := range resolution.Permissions {
		if permission == agentproposal.CreatePermission {
			return true, nil
		}
	}
	return false, nil
}

func proposeMessage(status string) string {
	switch status {
	case agentproposal.OutcomeProposed:
		return "Proposed. A person reviews it in the chat or the agents inbox and decides; nothing is created until they press Create. Tell the person you proposed it and why."
	case agentproposal.OutcomeDuplicate:
		return "This sub-agent is already proposed. Do not propose it again."
	case agentproposal.OutcomeLimit:
		return "You already have three open proposals in this project, the most you may. Wait until a person decides one."
	case agentproposal.OutcomeSuppressed:
		return "A person dismissed this same sub-agent less than 30 days ago. Do not propose it again."
	}
	return "No proposal was made."
}
