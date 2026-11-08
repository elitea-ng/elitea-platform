package agentexecution

// The turn-taking engine at the start of a turn (ADR-0050 decisions 7c and
// 7d, issue 1611).
//
// A human message in a conversation with two or more agents is routed by
// the conversation's floor (turntaking.Route): the first agent answers now,
// the other addressed agents queue behind it (agent_floor_turns, cause
// human_message) in the same admission transaction, with the exchange row
// that hand_floor and the after-settle hook (floor_hook.go) work on. Every
// turn of such a conversation carries AgentExecutionInputV1.floor: the cause
// of the turn and the peers the agent may hand the floor to.
//
// A conversation with one agent, or a service without WithFloor, is
// unchanged.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/google/uuid"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

// ErrFloorAddressee refuses a message that addresses an agent that is not
// in the conversation. The route answers 422 `addressee_not_in_conversation`.
var ErrFloorAddressee = errors.New("an addressed agent is not in the conversation")

// ErrFloorTurnNotQueued refuses starting a floor turn that is no longer
// QUEUED (a newer human message superseded it, or a failure skipped it).
var ErrFloorTurnNotQueued = errors.New("the floor turn is no longer queued")

// maxFloorPeers bounds AgentFloorV1.peers.
const maxFloorPeers = 16

// FloorTurn is a later agent turn of a human message that Main starts
// (floor_hook.go): another addressed agent, or an agent handed the floor.
// Set only by Main's floor hook; never from a request body.
type FloorTurn struct {
	// Cause is turntaking.CauseHumanMessage or turntaking.CauseAgentMention.
	Cause string
	// FromParticipantID is the agent that handed the floor; 0 for an
	// addressed agent of the person's message.
	FromParticipantID int64
	// FromName is the handing agent's display name (the peer message fence).
	FromName string
	// ReplyToQuestionID is the person's question an addressed agent answers;
	// empty for an agent-to-agent turn, which asks a question of its own.
	ReplyToQuestionID string
	// Position is the turn's agent_floor_turns position.
	Position int
}

func (turn *FloorTurn) valid() bool {
	if turn == nil {
		return true
	}
	switch turn.Cause {
	case turntaking.CauseHumanMessage:
		return turn.FromParticipantID == 0 && validUUID(turn.ReplyToQuestionID) && turn.Position > 0
	case turntaking.CauseAgentMention:
		return turn.FromParticipantID > 0 && turn.ReplyToQuestionID == "" && turn.Position > 0
	}
	return false
}

// FloorAgent is one agent participant of a conversation.
type FloorAgent struct {
	ParticipantID int64
	// Name is the persona's display name, else the participant's name.
	Name string
	Role string
}

// ConversationFloor is a conversation's floor settings and agents.
type ConversationFloor struct {
	Settings turntaking.Settings
	Agents   []FloorAgent
}

// FloorStore reads a conversation's floor (repos.AgentFloorRepository).
type FloorStore interface {
	ConversationFloor(ctx context.Context, projectID int64, conversationUUID string) (ConversationFloor, error)
}

// WithFloor turns the turn-taking engine on. Nil leaves it off.
func (service *CurrentApplicationStartService) WithFloor(store FloorStore) *CurrentApplicationStartService {
	if service != nil {
		service.floor = store
	}
	return service
}

// FloorAdmission is what the admission transaction writes for turn-taking.
type FloorAdmission struct {
	// Exchange is the exchange a new human message starts, with the
	// addressed agents queued behind its first. It supersedes the QUEUED
	// turns of the conversation's earlier exchanges.
	Exchange *FloorExchange
	// StartTurnQuestionID is the QUEUED floor turn this admission starts
	// (QUEUED -> STARTED with the new execution id); the admission is
	// refused (ErrFloorTurnNotQueued) when it is no longer QUEUED.
	StartTurnQuestionID string
}

// FloorExchange is one human message's exchange.
type FloorExchange struct {
	QuestionID         string
	OwnerUserID        int64
	TriggerOrigin      string
	FirstParticipantID int64
	// Queued are the agents that answer after the first, in order.
	Queued []int64
}

// Clone copies the admission.
func (a *FloorAdmission) Clone() *FloorAdmission {
	if a == nil {
		return nil
	}
	clone := *a
	if a.Exchange != nil {
		exchange := *a.Exchange
		exchange.Queued = append([]int64(nil), a.Exchange.Queued...)
		clone.Exchange = &exchange
	}
	return &clone
}

var floorTurnNamespace = uuid.MustParse("2b1f8d9c-4a0e-4f6b-9c39-0d5e1b7a6c11")

// FloorTurnQuestionID is the question id of the floor turn at position of
// the exchange of questionID: stable, so a replayed start is the same
// request.
func FloorTurnQuestionID(questionID string, position int) string {
	return uuid.NewSHA1(floorTurnNamespace, []byte(questionID+"\x00"+strconv.Itoa(position))).String()
}

// FloorPeerUserInput is the model-facing user input of an agent-to-agent
// turn: the peer's message, fenced as data (ADR-0044 decision 4 framing).
func FloorPeerUserInput(fromName, message string) string {
	encoded, _ := json.Marshal(message)
	from, _ := json.Marshal(fromName)
	// from is a JSON string; the attribute takes its quoted form.
	return turntaking.UntrustedPreamble + "<peer_message from=" + string(from) + ">\n" + string(encoded) + "\n</peer_message>"
}

// floorPlan is one start's turn-taking view.
type floorPlan struct {
	agents []FloorAgent
	cause  string
	// targets are the agents a human message is routed to; nil for any
	// other turn.
	targets []int64
}

// planFloor reads the conversation's floor and, for a new human message,
// routes it: request.TargetParticipantID becomes the first target. It
// answers nil when the engine is off or the conversation has fewer than two
// agents.
func (service *CurrentApplicationStartService) planFloor(
	ctx context.Context, request *CurrentApplicationStartRequest,
) (*floorPlan, error) {
	if service.floor == nil {
		return nil, nil
	}
	conversation, err := service.floor.ConversationFloor(ctx, request.ProjectID, request.ConversationUUID)
	if err != nil {
		return nil, err
	}
	if len(conversation.Agents) < 2 {
		return nil, nil
	}
	plan := &floorPlan{agents: conversation.Agents, cause: turntaking.CauseHumanMessage}
	if request.FloorTurn != nil {
		plan.cause = request.FloorTurn.Cause
		return plan, nil
	}
	if request.UnattendedContinuation != nil || request.HandoffCompletion != nil || request.ThreadRun != nil {
		return plan, nil
	}
	switch request.TriggerOrigin {
	case "", executiondomain.TriggerOriginManual, executiondomain.TriggerOriginAPI:
	default:
		// An automated start addresses its own participant.
		return plan, nil
	}
	ids := make([]int64, 0, len(conversation.Agents))
	for _, agent := range conversation.Agents {
		ids = append(ids, agent.ParticipantID)
	}
	participant := request.TargetParticipantID
	if len(request.AddressedTo) > 0 && !containsInt64(request.AddressedTo, participant) {
		// The composer's sticky target is not one the person addressed:
		// the address wins.
		participant = 0
	}
	targets, err := turntaking.Route(conversation.Settings, participant, request.AddressedTo, ids)
	switch {
	case errors.Is(err, turntaking.ErrNoTarget):
		return nil, ErrAgentTargetRequired
	case errors.Is(err, turntaking.ErrNotInConversation):
		return nil, ErrFloorAddressee
	case err != nil:
		return nil, err
	}
	request.TargetParticipantID = targets[0]
	plan.targets = targets
	return plan, nil
}

// input is the turn's AgentFloorV1 for the agent that answers.
func (plan *floorPlan) input(self int64) *runtimev1.AgentFloorV1 {
	if plan == nil {
		return nil
	}
	floor := &runtimev1.AgentFloorV1{
		Cause:               plan.cause,
		UnattendedToolsOnly: plan.cause == turntaking.CauseAgentMention,
	}
	for _, agent := range plan.agents {
		if agent.ParticipantID == self {
			continue
		}
		if len(floor.Peers) == maxFloorPeers {
			break
		}
		floor.Peers = append(floor.Peers, &runtimev1.AgentFloorPeerV1{
			ParticipantId: agent.ParticipantID, Name: agent.Name, Role: agent.Role,
		})
	}
	return floor
}

// admission is what the turn's admission writes for the floor.
func (plan *floorPlan) admission(request CurrentApplicationStartRequest) *FloorAdmission {
	switch {
	case plan == nil:
		return nil
	case request.FloorTurn != nil:
		return &FloorAdmission{StartTurnQuestionID: request.QuestionID}
	case len(plan.targets) == 0:
		return nil
	}
	return &FloorAdmission{Exchange: &FloorExchange{
		QuestionID: request.QuestionID, OwnerUserID: request.ActorUserID,
		TriggerOrigin: request.TriggerOrigin.Stored(), FirstParticipantID: plan.targets[0],
		Queued: append([]int64(nil), plan.targets[1:]...),
	}}
}

// applyFloorTurn sets the question fields of the turn: who asks, to whom,
// and whether the person's question is reused.
func applyFloorTurn(turn *CurrentApplicationTurn, plan *floorPlan, request CurrentApplicationStartRequest) {
	floor := request.FloorTurn
	switch {
	case floor != nil && floor.Cause == turntaking.CauseAgentMention:
		turn.QuestionAuthorParticipantID = floor.FromParticipantID
		turn.QuestionAddressedTo = []int64{request.TargetParticipantID}
		turn.QuestionVisibility = "side"
		turn.QuestionCause = turntaking.CauseAgentMention
	case floor != nil:
		turn.ReplyToQuestionID = floor.ReplyToQuestionID
	case plan != nil && len(plan.targets) > 0:
		turn.QuestionAddressedTo = append([]int64(nil), plan.targets...)
	}
}

// floorModelInput is the user input the model sees: a peer's message
// fenced as untrusted data on an agent-to-agent turn, else the request's.
func floorModelInput(request CurrentApplicationStartRequest) string {
	if request.FloorTurn != nil && request.FloorTurn.Cause == turntaking.CauseAgentMention {
		return FloorPeerUserInput(request.FloorTurn.FromName, request.UserInput)
	}
	return request.UserInput
}

func validAddressedTo(ids []int64) bool {
	if len(ids) > turntaking.MaxAddressed {
		return false
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func containsInt64(ids []int64, id int64) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}
