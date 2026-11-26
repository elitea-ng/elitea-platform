package storage

import (
	"context"
	"errors"
	"strings"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

// The turn-taking hand_floor tool (ADR-0050 decisions 7c and 7d, issue
// 1611), served on the private claim-bound listener for the reason the
// builder writes are (runtime_entity_builder.go).
//
// WHAT AUTHORIZES A CALL. The claim, through the agent-scoped authorizer.
// The project, the conversation and the calling agent (the run's
// participant) come from the claimed execution; the request carries only
// the agents named and the message.
//
// WHAT MAIN DECIDES, not the worker: who is an agent of the conversation,
// the order the handed agents answer in, and the loop guards
// (turntaking.Hand), checked under the exchange's row lock. A guard stops
// the whole exchange: Main posts the system message and ends the message's
// unattended chain limit_reached.
const (
	RuntimeHandFloorSchemaVersion = "elitea.runtime.hand-floor.v1"

	maxRuntimeHandFloorRequestBytes  = 8 * 1024
	maxRuntimeHandFloorResponseBytes = 16 * 1024

	runtimeContextStageHandFloorRun   = "hand_floor_run"
	runtimeContextStageHandFloorWrite = "hand_floor_write"
)

// RuntimeHandFloorRequest is the hand_floor tool's body.
type RuntimeHandFloorRequest struct {
	ParticipantIDs []int64 `json:"participant_ids"`
	Message        string  `json:"message"`
}

// RuntimeHandFloorTarget is one agent handed the floor.
type RuntimeHandFloorTarget struct {
	ParticipantID int64  `json:"participant_id"`
	Name          string `json:"name"`
}

// RuntimeHandFloorResult is the hand_floor response. Its fields are the
// complete set the worker accepts.
type RuntimeHandFloorResult struct {
	SchemaVersion string                   `json:"schema_version"`
	HandedTo      []RuntimeHandFloorTarget `json:"handed_to"`
	// Stopped names the loop guard that stopped the exchange; empty when
	// none did.
	Stopped string `json:"stopped"`
	// Message is the text the model reads.
	Message string `json:"message"`
}

// HandFloorRun is what the claimed execution says about a hand_floor call.
type HandFloorRun struct {
	ProjectID        int64
	ExecutionID      string
	ConversationUUID string
	// ParticipantID is the calling agent.
	ParticipantID int64
}

// FloorExchangeView is the locked exchange of the calling run.
type FloorExchangeView struct {
	Exchange turntaking.Exchange
	Agents   []agentexecutionapp.FloorAgent
	// Stopped is the guard that already stopped the exchange.
	Stopped string
}

// FloorHandWrite is what the sink writes for one decision.
type FloorHandWrite struct {
	// Queue are the agents handed the floor, in order.
	Queue   []int64
	Message string
	// Stop is the guard that stops the exchange, with its system message.
	Stop     string
	StopText string
}

// HandFloorSink is the tenant side (repos.AgentFloorRepository).
type HandFloorSink interface {
	ResolveHandFloorRun(ctx context.Context, projectID int64, executionID string, generation uint64) (HandFloorRun, error)
	// HandFloorInExchange runs decide on the run's exchange under its lock
	// and applies the write decide answers. ErrContentRejected: the run is
	// no turn of the conversation's newest exchange.
	HandFloorInExchange(ctx context.Context, run HandFloorRun, decide func(FloorExchangeView) (FloorHandWrite, error)) error
}

// RuntimeHandFloorService serves the hand_floor route.
type RuntimeHandFloorService struct {
	authorizer AgentRuntimeContextAuthorizer
	sink       HandFloorSink
}

// NewRuntimeHandFloorService builds the service.
func NewRuntimeHandFloorService(authorizer AgentRuntimeContextAuthorizer, sink HandFloorSink) (*RuntimeHandFloorService, error) {
	if authorizer == nil || sink == nil {
		return nil, errors.New("runtime hand floor dependencies are required")
	}
	return &RuntimeHandFloorService{authorizer: authorizer, sink: sink}, nil
}

// Hand hands the floor from the claimed run's agent.
func (service *RuntimeHandFloorService) Hand(ctx context.Context, claim ContentClaim, request RuntimeHandFloorRequest) (RuntimeHandFloorResult, error) {
	if service == nil || service.authorizer == nil || service.sink == nil {
		return RuntimeHandFloorResult{}, runtimeContextUnavailable(runtimeContextStageHandFloorRun)
	}
	message := strings.TrimSpace(request.Message)
	builders := RuntimeEntityBuilderService{authorizer: service.authorizer}
	authorization, err := builders.authorizeBuilder(ctx, claim)
	if err != nil {
		return RuntimeHandFloorResult{}, err
	}
	run, err := service.sink.ResolveHandFloorRun(ctx, authorization.ResourceProjectID, claim.ExecutionID, claim.Generation)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return RuntimeHandFloorResult{}, contextErr
		}
		if errors.Is(err, ErrContentUnauthorized) || errors.Is(err, ErrContentRejected) {
			return RuntimeHandFloorResult{}, err
		}
		return RuntimeHandFloorResult{}, runtimeContextUnavailable(runtimeContextStageHandFloorRun)
	}
	if run.ProjectID != authorization.ResourceProjectID ||
		(authorization.ConversationID != "" && run.ConversationUUID != authorization.ConversationID) {
		return RuntimeHandFloorResult{}, ErrContentUnauthorized
	}
	var result RuntimeHandFloorResult
	err = service.sink.HandFloorInExchange(ctx, run, func(view FloorExchangeView) (FloorHandWrite, error) {
		var write FloorHandWrite
		result, write = DecideHandFloor(view, run.ParticipantID, request.ParticipantIDs, message)
		return write, nil
	})
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return RuntimeHandFloorResult{}, contextErr
		}
		if errors.Is(err, ErrContentRejected) || errors.Is(err, ErrContentUnauthorized) {
			return RuntimeHandFloorResult{}, err
		}
		return RuntimeHandFloorResult{}, runtimeContextUnavailable(runtimeContextStageHandFloorWrite)
	}
	return result, nil
}

// DecideHandFloor is the decision of one call on a locked exchange: the
// tool result and the write. The decision is turntaking.DecideHand, which
// the desktop's handLocalFloor answers too (ADR-0061 decision 3c).
func DecideHandFloor(
	view FloorExchangeView, from int64, targets []int64, message string,
) (RuntimeHandFloorResult, FloorHandWrite) {
	agents := make([]turntaking.Agent, 0, len(view.Agents))
	names := make(map[int64]string, len(view.Agents))
	for _, agent := range view.Agents {
		agents = append(agents, turntaking.Agent{ParticipantID: agent.ParticipantID, Name: agent.Name})
		names[agent.ParticipantID] = agent.Name
	}
	outcome := turntaking.DecideHand(view.Exchange, view.Stopped, agents, from, targets, message)
	result := RuntimeHandFloorResult{
		SchemaVersion: RuntimeHandFloorSchemaVersion, HandedTo: []RuntimeHandFloorTarget{},
		Stopped: outcome.Stopped, Message: outcome.Message,
	}
	for _, id := range outcome.Handed {
		result.HandedTo = append(result.HandedTo, RuntimeHandFloorTarget{ParticipantID: id, Name: names[id]})
	}
	switch {
	case outcome.StopText != "":
		return result, FloorHandWrite{Stop: outcome.Stopped, StopText: outcome.StopText}
	case len(outcome.Handed) > 0:
		return result, FloorHandWrite{Queue: outcome.Handed, Message: message}
	}
	return result, FloorHandWrite{}
}
