package localturn

// Several agents in a desktop thread (ADR-0050 decision 7f, issue 1806).
//
// MAIN DECIDES THE FLOOR, THE DESKTOP RUNS IT. A person's message in a
// conversation with two or more agent participants is routed at start by
// the same rule a cloud message is (turntaking.Route: the addressed agents
// in order, else the lead under policy `lead`, else the composer's target):
// the first agent answers this turn, and the start's answer names the rest
// in order (`floor.targets`). The desktop then starts each of them in turn,
// one at a time, as a later turn of the message (`floor_turn`: the message's
// question id and the target's position). Main admits a later turn only for
// the agent at that position of a message it routed, and only while no
// newer person's message has started in the conversation.
//
// A later addressed agent answers the person's own question: its commit
// writes no question of its own and its answer replies to the message, as a
// cloud floor turn's does.
//
// WHERE AN AGENT RUNS. An agent participant whose mapping says
// `entity_settings.executor: cloud` is a CLOUD agent of the thread: the
// desktop does not run it with the workspace. It relays the conversation
// text to the cloud as a child turn of this execution (the local child turn
// route admits the turn's own agent for a cloud executor) and commits the
// answer, marked `executed_by: cloud`. Every other agent runs on the
// desktop with the workspace (`executor: local`, the default).
//
// The agent-to-agent hand-off (`hand_floor`) is not offered to a desktop
// turn yet; the guards it needs (turntaking.Hand) stay with the cloud.

import (
	"context"
	"errors"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

// Executors of an agent participant in a desktop thread.
const (
	ExecutorLocal = "local"
	ExecutorCloud = "cloud"
	// ExecutedByCloud is the `executed_by` meta value of a committed turn a
	// cloud agent answered.
	ExecutedByCloud = "cloud"
)

var (
	// ErrAgentTargetRequired: the conversation holds several agents and the
	// message names none, has no lead, and no participant. The route
	// answers 422 `agent_target_required`.
	ErrAgentTargetRequired = errors.New("conversation holds several agents and the start names none")
	// ErrAddressee: an addressed agent is not an agent of the conversation.
	// The route answers 422 `addressee_not_in_conversation`.
	ErrAddressee = errors.New("an addressed agent is not in the conversation")
	// ErrFloorTurn: the later turn names no routed message of this caller,
	// a position the message has no target at, or another agent than the
	// one at that position. The route answers 409 `floor_turn_refused`.
	ErrFloorTurn = errors.New("the floor turn does not match the routed message")
	// ErrFloorSuperseded: a newer person's message started in the
	// conversation; the older message's remaining agents do not answer.
	// The route answers 409 `floor_turn_superseded`.
	ErrFloorSuperseded = errors.New("a newer message superseded the floor turn")
)

// FloorTurnRef names a later turn of a routed message.
type FloorTurnRef struct {
	// QuestionID is the person's message: the question id of its first turn.
	QuestionID string
	// Position is the target's index in the routed order, from 1.
	Position int
}

// FloorAgent is one agent participant of a desktop thread.
type FloorAgent struct {
	ParticipantID int64
	// Name is the persona's display name, else the participant's name.
	Name string
	// Executor is ExecutorLocal or ExecutorCloud.
	Executor string
}

// ConversationFloor is a conversation's floor settings and agents.
type ConversationFloor struct {
	Settings turntaking.Settings
	Agents   []FloorAgent
}

// FloorReader reads a conversation's floor (*repos.LocalTurnsRepo). A
// conversation the caller cannot see answers ErrNotFound or an empty floor;
// the start's own target check refuses it either way.
type FloorReader interface {
	LocalTurnFloor(ctx context.Context, projectID, actorUserID int64, conversationUUID string) (ConversationFloor, error)
}

// WithFloor turns the floor on. Nil leaves it off: every start answers on
// the participant it names, as before.
func (s *Service) WithFloor(reader FloorReader) *Service {
	if s != nil {
		s.floor = reader
	}
	return s
}

// FloorStarted is the start's answer about the floor.
type FloorStarted struct {
	// QuestionID is the person's message the turn answers.
	QuestionID string
	// Position is this turn's index in Targets.
	Position int
	// Targets are the agents that answer the message, in order.
	Targets []int64
	// Agents are the conversation's agents.
	Agents []FloorAgent
}

// floorPlan is one start's routing.
type floorPlan struct {
	participant int64
	targets     []int64
	agents      []FloorAgent
}

// planFloor routes a person's message, or checks the shape of a later
// turn. It answers the participant that answers this turn: the request's
// own when the floor does not apply.
func (s *Service) planFloor(ctx context.Context, request StartRequest) (floorPlan, error) {
	plan := floorPlan{participant: request.ParticipantID}
	if s.floor == nil {
		if len(request.AddressedTo) > 0 || request.FloorTurn != nil {
			// A client that addresses agents is told the deployment cannot
			// route, rather than having its address dropped.
			return floorPlan{}, ErrAddressee
		}
		return plan, nil
	}
	conversation, err := s.floor.LocalTurnFloor(ctx, request.ProjectID, request.ActorUserID, request.ConversationUUID)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) {
			return floorPlan{}, err
		}
		s.logger.ErrorContext(ctx, "local turn: floor unreadable", "err", err)
		return floorPlan{}, ErrUnavailable
	}
	plan.agents = conversation.Agents
	ids := make([]int64, 0, len(conversation.Agents))
	for _, agent := range conversation.Agents {
		ids = append(ids, agent.ParticipantID)
	}
	if request.FloorTurn != nil {
		// The store checks the turn against the message it continues.
		if len(ids) < 2 || !containsID(ids, request.ParticipantID) {
			return floorPlan{}, ErrFloorTurn
		}
		return plan, nil
	}
	if len(ids) < 2 {
		if len(request.AddressedTo) > 0 {
			// One agent: an address may only name it.
			for _, id := range request.AddressedTo {
				if !containsID(ids, id) {
					return floorPlan{}, ErrAddressee
				}
			}
		}
		return plan, nil
	}
	participant := request.ParticipantID
	if len(request.AddressedTo) > 0 && !containsID(request.AddressedTo, participant) {
		// The composer's sticky target is not one the person addressed:
		// the address wins.
		participant = 0
	}
	if participant > 0 && !containsID(ids, participant) {
		participant = 0
	}
	targets, err := turntaking.Route(conversation.Settings, participant, request.AddressedTo, ids)
	switch {
	case errors.Is(err, turntaking.ErrNoTarget):
		return floorPlan{}, ErrAgentTargetRequired
	case errors.Is(err, turntaking.ErrNotInConversation):
		return floorPlan{}, ErrAddressee
	case err != nil:
		return floorPlan{}, err
	}
	plan.participant = targets[0]
	plan.targets = targets
	return plan, nil
}

// started is the start's floor answer; nil when the floor does not apply.
func (plan floorPlan) started(turn StartedTurn) *FloorStarted {
	if len(turn.FloorTargets) == 0 {
		return nil
	}
	position := 0
	for index, id := range turn.FloorTargets {
		if id == turn.ParticipantID {
			position = index
			break
		}
	}
	agents := plan.agents
	if agents == nil {
		agents = []FloorAgent{}
	}
	return &FloorStarted{
		QuestionID: turn.FloorQuestionID, Position: position,
		Targets: append([]int64(nil), turn.FloorTargets...), Agents: agents,
	}
}

func executorOrLocal(executor string) string {
	if executor == ExecutorCloud {
		return ExecutorCloud
	}
	return ExecutorLocal
}

func validAddressedTo(ids []int64) bool {
	if len(ids) > turntaking.MaxAddressed {
		return false
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || id > 2147483647 || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

// validFloorTurn: a later turn names a message and a position, answers an
// agent, and carries no address or grant of its own (it is the message's).
func validFloorTurn(request StartRequest) bool {
	turn := request.FloorTurn
	if turn == nil {
		return true
	}
	return ValidUUID(turn.QuestionID) && turn.QuestionID != request.QuestionID &&
		turn.Position >= 1 && turn.Position < turntaking.MaxAddressed &&
		request.ParticipantID > 0 && len(request.AddressedTo) == 0 && request.Unattended == nil
}

func containsID(ids []int64, id int64) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}
