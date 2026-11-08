package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

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
		result, write = decideHandFloor(view, run.ParticipantID, request.ParticipantIDs, message)
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

// decideHandFloor is the decision of one call on a locked exchange: the
// tool result and the write.
func decideHandFloor(
	view FloorExchangeView, from int64, targets []int64, message string,
) (RuntimeHandFloorResult, FloorHandWrite) {
	result := RuntimeHandFloorResult{SchemaVersion: RuntimeHandFloorSchemaVersion, HandedTo: []RuntimeHandFloorTarget{}}
	if view.Stopped != "" {
		result.Stopped = view.Stopped
		result.Message = "The loop guard already stopped this exchange. Answer the person yourself."
		return result, FloorHandWrite{}
	}
	switch {
	case message == "":
		result.Message = "The hand-off was not made: say what you need from the other agents in message."
		return result, FloorHandWrite{}
	case len(message) > turntaking.MaxHandMessageBytes || !utf8.ValidString(message) || strings.ContainsRune(message, 0):
		result.Message = fmt.Sprintf("The hand-off was not made: message is longer than %d bytes.", turntaking.MaxHandMessageBytes)
		return result, FloorHandWrite{}
	}
	names := make(map[int64]string, len(view.Agents))
	agents := make([]int64, 0, len(view.Agents))
	for _, agent := range view.Agents {
		names[agent.ParticipantID] = agent.Name
		agents = append(agents, agent.ParticipantID)
	}
	decision, err := turntaking.Hand(view.Exchange, from, targets, agents)
	var invalid *turntaking.InvalidHandError
	if errors.As(err, &invalid) {
		result.Message = invalidHandText(invalid.Code)
		return result, FloorHandWrite{}
	}
	if err != nil {
		result.Message = "The hand-off was not made."
		return result, FloorHandWrite{}
	}
	if decision.Stopped != "" {
		text := turntaking.StopText(decision.Stopped, decision.Limit, names[decision.At],
			[2]string{names[from], names[decision.At]})
		result.Stopped = decision.Stopped
		result.Message = text + " Answer the person yourself."
		return result, FloorHandWrite{Stop: decision.Stopped, StopText: text}
	}
	for _, id := range decision.Handed {
		result.HandedTo = append(result.HandedTo, RuntimeHandFloorTarget{ParticipantID: id, Name: names[id]})
	}
	result.Message = "Handed the floor. They answer after your turn, in order. Finish your answer to the person now."
	return result, FloorHandWrite{Queue: decision.Handed, Message: message}
}

func invalidHandText(code string) string {
	switch code {
	case turntaking.StopEmpty:
		return "The hand-off was not made: name at least one agent."
	case turntaking.StopTooMany:
		return fmt.Sprintf("The hand-off was not made: name at most %d agents.", turntaking.MaxHandTargets)
	case turntaking.StopSelf:
		return "The hand-off was not made: you cannot hand the floor to yourself."
	}
	return "The hand-off was not made: name only agents of this conversation, each once."
}
