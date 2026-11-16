package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentworkforce"
)

// The Marketplace tools of a workforce manager (ADR-0051 decision 6, issue
// 1632): marketplace_search, marketplace_listing, propose_hire and
// suggest_feedback. They are served on the private claim-bound listener, as
// propose_dismissal is (runtime_propose_dismissal.go), through one route that
// names the tool, so the claim, the run and the capability are resolved once.
//
// WHAT AUTHORIZES A CALL. The claim, through the agent-scoped authorizer. The
// project, the calling agent and the person the run served come from the
// claimed execution; the request names only the tool and its arguments.
//
// WHAT MAIN DECIDES, not the worker: that the agent is a workforce manager (the
// frozen version AND a published version set capabilities.can_review_agents),
// every guard of the two proposals, and the scorecard summary of a report.
// NOTHING IS HIRED OR PUBLISHED by a call: a proposal is a person's to decide
// (or the project's policy's, which a person set).
const (
	RuntimeMarketplaceWorkforceSchemaVersion = "elitea.runtime.marketplace-workforce.v1"

	maxRuntimeMarketplaceWorkforceRequestBytes  = 16 * 1024
	maxRuntimeMarketplaceWorkforceResponseBytes = 128 * 1024

	runtimeContextStageMarketplaceWorkforceRun  = "marketplace_workforce_run"
	runtimeContextStageMarketplaceWorkforceTool = "marketplace_workforce_tool"
)

// The tool names of the route.
const (
	MarketplaceToolSearch          = "marketplace_search"
	MarketplaceToolListing         = "marketplace_listing"
	MarketplaceToolProposeHire     = "propose_hire"
	MarketplaceToolSuggestFeedback = "suggest_feedback"
)

// MarketplaceWorkforceRequest is the route's request document.
type MarketplaceWorkforceRequest struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

// MarketplaceWorkforceSink is the shared side (repos.AgentWorkforce).
type MarketplaceWorkforceSink interface {
	ResolveProposeDismissalRun(ctx context.Context, projectID int64, executionID string, generation uint64) (ProposeDismissalRun, error)
	// MarketplaceWorkforce runs one tool for the claimed run. The answer holds
	// a status and a message, and the tool's data.
	MarketplaceWorkforce(ctx context.Context, run ProposeDismissalRun, tool string, arguments json.RawMessage) (map[string]any, error)
}

// RuntimeMarketplaceWorkforceService serves the route.
type RuntimeMarketplaceWorkforceService struct {
	authorizer AgentRuntimeContextAuthorizer
	sink       MarketplaceWorkforceSink
}

// NewRuntimeMarketplaceWorkforceService builds the service.
func NewRuntimeMarketplaceWorkforceService(authorizer AgentRuntimeContextAuthorizer, sink MarketplaceWorkforceSink) (*RuntimeMarketplaceWorkforceService, error) {
	if authorizer == nil || sink == nil {
		return nil, errors.New("runtime marketplace workforce dependencies are required")
	}
	return &RuntimeMarketplaceWorkforceService{authorizer: authorizer, sink: sink}, nil
}

func marketplaceWorkforceResult(status, message string) map[string]any {
	return map[string]any{"schema_version": RuntimeMarketplaceWorkforceSchemaVersion, "status": status, "message": message}
}

func knownMarketplaceTool(tool string) bool {
	switch tool {
	case MarketplaceToolSearch, MarketplaceToolListing, MarketplaceToolProposeHire, MarketplaceToolSuggestFeedback:
		return true
	}
	return false
}

// Call runs one tool for the claimed run's workforce manager.
func (service *RuntimeMarketplaceWorkforceService) Call(ctx context.Context, claim ContentClaim, request MarketplaceWorkforceRequest) (map[string]any, error) {
	if service == nil || service.authorizer == nil || service.sink == nil {
		return nil, runtimeContextUnavailable(runtimeContextStageMarketplaceWorkforceRun)
	}
	if !knownMarketplaceTool(request.Tool) {
		return marketplaceWorkforceResult(agentworkforce.OutcomeInvalid, "tool is marketplace_search, marketplace_listing, propose_hire or suggest_feedback."), nil
	}
	builders := RuntimeEntityBuilderService{authorizer: service.authorizer}
	authorization, err := builders.authorizeBuilder(ctx, claim)
	if err != nil {
		return nil, err
	}
	run, err := service.sink.ResolveProposeDismissalRun(ctx, authorization.ResourceProjectID, claim.ExecutionID, claim.Generation)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		if errors.Is(err, ErrContentUnauthorized) || errors.Is(err, ErrContentRejected) {
			return nil, err
		}
		return nil, runtimeContextUnavailable(runtimeContextStageMarketplaceWorkforceRun)
	}
	if run.ProjectID != authorization.ResourceProjectID ||
		(authorization.ConversationID != "" && run.ConversationUUID != authorization.ConversationID) ||
		strconv.FormatInt(run.ActorID, 10) != authorization.ActorID {
		return nil, ErrContentUnauthorized
	}
	if run.ApplicationID <= 0 || !run.IsWorkforceManager {
		return marketplaceWorkforceResult(agentworkforce.OutcomeNotAllowed,
			"This agent may not use the Marketplace tools: its version does not set capabilities.can_review_agents."), nil
	}
	result, err := service.sink.MarketplaceWorkforce(ctx, run, request.Tool, request.Arguments)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, runtimeContextUnavailable(runtimeContextStageMarketplaceWorkforceTool)
	}
	result["schema_version"] = RuntimeMarketplaceWorkforceSchemaVersion
	if _, ok := result["status"].(string); !ok {
		return nil, runtimeContextUnavailable(runtimeContextStageMarketplaceWorkforceTool)
	}
	return result, nil
}
