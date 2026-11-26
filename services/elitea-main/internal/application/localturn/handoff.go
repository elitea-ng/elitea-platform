package localturn

// Agents hand the floor to each other in a desktop thread (ADR-0061
// decisions 3c and 3f, issue 3063).
//
// MAIN DECIDES, THE DESKTOP RUNS. A local agent's `hand_floor` call is
// handLocalFloor, bound to the caller's live local turn: Main resolves the
// calling agent from the execution (never from the body), and decides with
// the cloud's own function (turntaking.DecideHand: the cloud's guards,
// limits and texts) on the routed message's exchange, under a row lock on
// the message's first local turn. The exchange is the routed targets plus
// the hand-offs already queued (local_floor_hands) plus the message's
// consults, so a local exchange counts hops exactly as a cloud one does.
// The handed agents are queued in Main's order; the desktop starts each,
// once the handing turn is committed, with `floor_turn.hand_id`, and Main
// admits it only for the agent it was handed to, only while the message is
// the thread's newest. A handed turn's input is the handing agent's message
// framed as untrusted (agentexecution.FloorPeerUserInput, the cloud's
// framing): Main answers it at start, the desktop never builds it.
//
// A cloud member of the thread is a leaf in v1 (ADR-0061 owner decision 3):
// its turn cannot hand the floor on.

import (
	"context"
	"errors"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
)

// Bounds of the hand-off calls.
const (
	// MaxHandConsults is the consult pairs one handLocalFloor call reports
	// (the host's per-message consult limit, ADR-0061 decision 3f).
	MaxHandConsults = 8
	// MaxSkipHands is the hand-offs one skip names.
	MaxSkipHands = 16
	// SkipStoppedByPerson and SkipSkippedByPerson are the skip codes a
	// person's Stop exchange and Skip write (ADR-0061 decision 2c).
	SkipStoppedByPerson = "stopped_by_person"
	SkipSkippedByPerson = "skipped_by_person"
	// SkipSuperseded is a hand-off a newer person's message superseded.
	SkipSuperseded = "superseded"
	// HandFloorSchemaVersion is the answer's schema: the worker's
	// (storage.RuntimeHandFloorSchemaVersion), so the model reads one shape.
	HandFloorSchemaVersion = "elitea.runtime.hand-floor.v1"
)

// ErrNotLive: the handing turn is committed or past its deadline. The route
// answers 409 `local_turn_not_live`.
var ErrNotLive = errors.New("the local turn is no longer live")

// HandRequest is one handLocalFloor call.
type HandRequest struct {
	ProjectID      int64
	ActorUserID    int64
	Credential     Credential
	ExecutionID    string
	ParticipantIDs []int64
	Message        string
	// Consults are the consult pairs the message ran so far on the host.
	Consults []turntaking.Turn
}

// HandedAgent is one agent the floor was handed to.
type HandedAgent struct {
	ParticipantID int64
	Name          string
	HandID        string
}

// HandResult answers handLocalFloor: the worker's RuntimeHandFloorResult
// with each queued turn's id.
type HandResult struct {
	HandedTo []HandedAgent
	Stopped  string
	Message  string
}

// HandScope names the calling turn.
type HandScope struct {
	ProjectID   int64
	ActorUserID int64
	Credential  Credential
	ExecutionID string
}

// HandView is the locked exchange of the calling turn.
type HandView struct {
	// From is the calling turn's agent.
	From     int64
	Exchange turntaking.Exchange
	Agents   []turntaking.Agent
	// Stopped is the guard that already stopped the message.
	Stopped string
}

// HandWrite is what the store writes for one decision.
type HandWrite struct {
	Queue    []int64
	Message  string
	Stop     string
	StopText string
}

// HandStore is the repository side (repos.LocalTurnsRepo).
type HandStore interface {
	// HandLocalFloor locks the calling turn's message, runs decide on its
	// exchange and applies the write. It answers the queued hand-offs' ids,
	// in Queue order. ErrNotFound: no such turn of this caller and
	// credential; ErrNotLive: committed or expired; ErrFloorTurn: the turn
	// is no agent turn of a routed message, or its agent runs in the
	// cloud; ErrFloorSuperseded: a newer message started.
	HandLocalFloor(ctx context.Context, scope HandScope, decide func(HandView) HandWrite) ([]string, error)
	// SkipLocalFloorHands skips the named QUEUED hand-offs of a message the
	// caller owns and answers the ones it skipped. ErrNotFound: no such
	// message of this caller.
	SkipLocalFloorHands(ctx context.Context, projectID, actorUserID int64, questionID string, handIDs []string, code string) ([]string, error)
}

// Hands is the hand-off use case.
type Hands struct {
	store  HandStore
	policy PolicyReader
}

// NewHands builds the use case.
func NewHands(store HandStore, policy PolicyReader) (*Hands, error) {
	if store == nil || policy == nil {
		return nil, errors.New("local hand-off dependencies are required")
	}
	return &Hands{store: store, policy: policy}, nil
}

// Hand decides one hand_floor call of a live local turn. A call Main
// refuses as a hand-off (no agent named, the caller itself, a message too
// long, a guard) is an answer the model reads, not an error.
func (h *Hands) Hand(ctx context.Context, request HandRequest) (HandResult, error) {
	if h == nil || !validHand(request) {
		return HandResult{}, ErrInvalid
	}
	if err := h.allowed(ctx); err != nil {
		return HandResult{}, err
	}
	message := strings.TrimSpace(request.Message)
	var (
		outcome turntaking.HandOutcome
		names   = map[int64]string{}
	)
	ids, err := h.store.HandLocalFloor(ctx, HandScope{
		ProjectID: request.ProjectID, ActorUserID: request.ActorUserID,
		Credential: request.Credential, ExecutionID: request.ExecutionID,
	}, func(view HandView) HandWrite {
		for _, agent := range view.Agents {
			names[agent.ParticipantID] = agent.Name
		}
		exchange := view.Exchange
		// The host's consults of the message so far, or the ones already
		// committed, whichever are more: neither side can undercount.
		if len(request.Consults) > len(exchange.Consults) {
			exchange.Consults = request.Consults
		}
		outcome = turntaking.DecideHand(exchange, view.Stopped, view.Agents, view.From, request.ParticipantIDs, message)
		switch {
		case outcome.StopText != "":
			return HandWrite{Stop: outcome.Stopped, StopText: outcome.StopText}
		case len(outcome.Handed) > 0:
			return HandWrite{Queue: outcome.Handed, Message: message}
		}
		return HandWrite{}
	})
	if err != nil {
		return HandResult{}, err
	}
	if len(ids) != len(outcome.Handed) {
		return HandResult{}, errors.Join(ErrUnavailable, errors.New("local hand-off: the store queued another number of turns"))
	}
	result := HandResult{HandedTo: []HandedAgent{}, Stopped: outcome.Stopped, Message: outcome.Message}
	for index, id := range outcome.Handed {
		result.HandedTo = append(result.HandedTo, HandedAgent{ParticipantID: id, Name: names[id], HandID: ids[index]})
	}
	return result, nil
}

// Skip skips queued hand-offs of the caller's message (Stop exchange and
// Skip, ADR-0061 decision 2c).
func (h *Hands) Skip(ctx context.Context, projectID, actorUserID int64, questionID string, handIDs []string, code string) ([]string, error) {
	if h == nil || !validSkip(projectID, actorUserID, questionID, handIDs, code) {
		return nil, ErrInvalid
	}
	if err := h.allowed(ctx); err != nil {
		return nil, err
	}
	return h.store.SkipLocalFloorHands(ctx, projectID, actorUserID, questionID, handIDs, code)
}

func (h *Hands) allowed(ctx context.Context) error {
	policy, err := h.policy.Policy(ctx)
	if err != nil {
		return ErrUnavailable
	}
	if !policy.LocalWork.Allowed {
		return ErrLocalWorkDisabled
	}
	return nil
}

func validHand(request HandRequest) bool {
	if request.ProjectID <= 0 || request.ProjectID > 2147483647 ||
		request.ActorUserID <= 0 || request.ActorUserID > 2147483647 ||
		request.Credential.TokenID == "" || !ValidExecutionID(request.ExecutionID) ||
		len(request.ParticipantIDs) == 0 || len(request.ParticipantIDs) > turntaking.MaxHandTargets ||
		len(request.Consults) > MaxHandConsults || !validText(request.Message, 4*turntaking.MaxHandMessageBytes, true) {
		return false
	}
	for _, id := range request.ParticipantIDs {
		if id <= 0 || id > 2147483647 {
			return false
		}
	}
	for _, consult := range request.Consults {
		if consult.From <= 0 || consult.From > 2147483647 || consult.To <= 0 || consult.To > 2147483647 ||
			consult.From == consult.To {
			return false
		}
	}
	return true
}

func validSkip(projectID, actorUserID int64, questionID string, handIDs []string, code string) bool {
	if projectID <= 0 || projectID > 2147483647 || actorUserID <= 0 || actorUserID > 2147483647 ||
		!ValidUUID(questionID) || len(handIDs) == 0 || len(handIDs) > MaxSkipHands ||
		(code != SkipStoppedByPerson && code != SkipSkippedByPerson) {
		return false
	}
	seen := make(map[string]bool, len(handIDs))
	for _, id := range handIDs {
		if !ValidUUID(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

// HandOffStarted is the start's answer about a handed turn.
type HandOffStarted struct {
	HandID            string
	FromParticipantID int64
	FromName          string
	Message           string
	// UserInput is the model-facing input of the turn: Message framed as
	// untrusted, exactly as a cloud hand-off turn's is.
	UserInput string
	Agents    []FloorAgent
}

// HandOff is the queued hand-off a handed turn's start admitted.
type HandOff struct {
	HandID            string
	FromParticipantID int64
	FromName          string
	Message           string
}
