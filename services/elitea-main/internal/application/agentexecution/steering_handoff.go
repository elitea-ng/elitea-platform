package agentexecution

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

// The server's hand-off of leftover steering messages (client contract 1.27).
//
// A turn that ends before a safe point takes its steering messages leaves them
// `turn_ended`. A client normally takes them at once (HandOff) and sends them
// as the next message. When no client acts within ServerHandOffDelay of the
// settlement, this routine takes them the same way and starts the next turn
// itself: as the author, with the previous turn's participant and the
// participant's stored settings, after it resolves the author's permission
// again. Both take rows with one guarded update, so a message is taken once
// and no turn starts twice.
//
// A NEWER MESSAGE. When the author already sent a newer message in the
// conversation after the turn ended, the routine starts no turn for this
// answer: it carries the messages over to the author's newer turn
// (steer.queued on that turn's stream). A running newer turn injects them
// at its next safe point, before its own steering messages. A newer turn
// that ended, or a pipeline turn (no safe point), leaves them turn_ended on
// the newer answer, and that answer's hand-off starts the next turn with
// them after it. Nothing is dropped.
//
// STOPPED OR FAILED. A turn that the user stopped, or that failed, is never
// continued by the server: its messages are held (they stay turn_ended),
// and the author's client offers them. The carry-over above still applies
// when the author had already sent a newer message.
//
// NO LOST TEXT. A client hand-off names the question id of the client's
// next send. When no message with that id exists ClientHandOffGrace later
// (the tab closed first), this routine starts that turn with that question
// id, so a late client send reaches the same turn.
//
// MCP. The client's MCP tokens are never stored, so this turn has none. An MCP
// server that needs a token asks for authorization in the turn, the same way
// as for a client that holds no token.

// ServerHandOffDelay is how long a client has to hand off by itself.
const ServerHandOffDelay = 10 * time.Second

// ServerHandOffRetryAfter is when a server hand-off whose start was not
// confirmed is started again (with the same question id, so it is the same
// turn).
const ServerHandOffRetryAfter = time.Minute

// ServerHandOffGiveUpAfter removes a server hand-off whose start keeps
// failing (for example a permission store that refuses the author with an
// error): the messages are not retried for ever.
const ServerHandOffGiveUpAfter = time.Hour

// ClientHandOffGrace is how long a client has, after its hand-off, to send
// the message with the question id it named. After it, the routine starts
// that turn itself (a tab that closed first loses nothing).
const ClientHandOffGrace = time.Minute

// ServerHandOffKey prefixes the hand-off key of the server's own hand-offs. A
// client key may not use it.
const ServerHandOffKey = "server"

// MaxServerHandOffsPerPass bounds one pass of the routine.
const MaxServerHandOffsPerPass = 32

// SteeringHandOffPermission is what the author must still hold: the send's.
const SteeringHandOffPermission = "models.chat.messages.create"

// ServerHandOff is one group the routine took: the turn_ended messages of one
// author on one ended answer.
type ServerHandOff struct {
	ProjectID         int64
	ResponseMessageID string
	AuthorUserID      int64
	ConversationUUID  string
	ParticipantID     int64
	// Adhoc is a model chat turn (no application version).
	Adhoc       bool
	QuestionID  string
	SteeringIDs []string
	Texts       []string
}

// SteeringHandOffStore is the routine's half of the durable queue.
type SteeringHandOffStore interface {
	// TakeServerHandOffs takes the groups whose answer settled before
	// readyBefore and returns them, with the groups it took before whose
	// start was not confirmed before retryBefore. A group whose author sent
	// a newer message is carried over to that turn instead; the effects
	// name the steer.queued events to wake.
	TakeServerHandOffs(ctx context.Context, readyBefore, retryBefore time.Duration, limit int) ([]ServerHandOff, []SteeringEffect, error)
	// MarkServerHandOffStarted records that the start was admitted.
	MarkServerHandOffStarted(ctx context.Context, handOff ServerHandOff) error
	// DropServerHandOff removes the messages of a group that can never start
	// (the author lost the permission, the conversation is gone).
	DropServerHandOff(ctx context.Context, handOff ServerHandOff) error
}

// SteeringTurnStarter is the chat composer's start (CurrentApplicationStartService).
type SteeringTurnStarter interface {
	StartCurrentApplication(context.Context, CurrentApplicationStartRequest) (CurrentApplicationStartOutcome, error)
	StartCurrentAdhoc(context.Context, CurrentAdhocStartRequest) (CurrentApplicationStartOutcome, error)
}

// SteeringAuthorCheck reports whether the author may still send in the
// project. An error is "cannot decide now"; the group is tried again later.
type SteeringAuthorCheck func(ctx context.Context, projectID, userID int64) (bool, error)

// SteeringHandOffRoutine is one pass of the server's hand-off.
type SteeringHandOffRoutine struct {
	store    SteeringHandOffStore
	starter  SteeringTurnStarter
	mayStart SteeringAuthorCheck
	wake     func([]SteeringEffect)
	logger   *slog.Logger
}

// NewSteeringHandOffRoutine builds the routine. wake may be nil (no event
// stream wake-up is composed; the stream then reads the events on its next
// poll).
func NewSteeringHandOffRoutine(store SteeringHandOffStore, starter SteeringTurnStarter, mayStart SteeringAuthorCheck, wake func([]SteeringEffect), logger *slog.Logger) (*SteeringHandOffRoutine, error) {
	if store == nil || starter == nil || mayStart == nil {
		return nil, errors.New("steering hand-off routine dependencies are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if wake == nil {
		wake = func([]SteeringEffect) {}
	}
	return &SteeringHandOffRoutine{store: store, starter: starter, mayStart: mayStart, wake: wake, logger: logger}, nil
}

// SteeringHandOffResult counts the decisions of one pass.
type SteeringHandOffResult struct {
	Started int
	Dropped int
	Retried int
	// Carried counts the messages carried over to the author's newer turn.
	Carried int
}

// Pass takes the due groups and starts their turns.
func (routine *SteeringHandOffRoutine) Pass(ctx context.Context) (SteeringHandOffResult, error) {
	var result SteeringHandOffResult
	handOffs, effects, err := routine.store.TakeServerHandOffs(ctx, ServerHandOffDelay, ServerHandOffRetryAfter, MaxServerHandOffsPerPass)
	routine.wake(effects)
	result.Carried = len(effects)
	if err != nil {
		return result, err
	}
	for _, handOff := range handOffs {
		switch routine.start(ctx, handOff) {
		case handOffStarted:
			result.Started++
		case handOffDropped:
			result.Dropped++
		default:
			result.Retried++
		}
	}
	return result, nil
}

type handOffDecision int

const (
	handOffRetry handOffDecision = iota
	handOffStarted
	handOffDropped
)

func (routine *SteeringHandOffRoutine) start(ctx context.Context, handOff ServerHandOff) handOffDecision {
	log := routine.logger.With("project_id", handOff.ProjectID, "response_message_id", handOff.ResponseMessageID)
	allowed, err := routine.mayStart(ctx, handOff.ProjectID, handOff.AuthorUserID)
	if err != nil {
		log.Warn("steering hand-off: the author's permission is not known now", "err", err)
		return handOffRetry
	}
	if !allowed {
		return routine.drop(ctx, log, handOff, "the author can no longer send in this project")
	}
	input := strings.Join(handOff.Texts, "\n\n")
	if handOff.Adhoc {
		_, err = routine.starter.StartCurrentAdhoc(ctx, CurrentAdhocStartRequest{
			ProjectID: handOff.ProjectID, ActorUserID: handOff.AuthorUserID,
			ConversationUUID: handOff.ConversationUUID, TargetParticipantID: handOff.ParticipantID,
			QuestionID: handOff.QuestionID, UserInput: input,
			// The participant's stored settings, without a per-send override.
			LLMSettings:   []byte(`{}`),
			TriggerOrigin: executiondomain.TriggerOriginManual,
		})
	} else {
		_, err = routine.starter.StartCurrentApplication(ctx, CurrentApplicationStartRequest{
			ProjectID: handOff.ProjectID, ActorUserID: handOff.AuthorUserID,
			ConversationUUID: handOff.ConversationUUID, TargetParticipantID: handOff.ParticipantID,
			QuestionID: handOff.QuestionID, UserInput: input,
			TriggerOrigin: executiondomain.TriggerOriginManual,
		})
	}
	switch {
	case err == nil:
		if markErr := routine.store.MarkServerHandOffStarted(ctx, handOff); markErr != nil {
			// The next pass starts it again with the same question id: the
			// start answers the same turn.
			log.Warn("steering hand-off: the start is not recorded", "err", markErr)
		}
		return handOffStarted
	case errors.Is(err, ErrInvalidCurrentAgentStart), errors.Is(err, ErrUnsupportedCurrentAgentStart),
		errors.Is(err, ErrLocalWorkThread), errors.Is(err, ErrLocalWorkTurn):
		return routine.drop(ctx, log, handOff, "the next turn cannot be started")
	default:
		log.Warn("steering hand-off: the next turn did not start", "err", err)
		return handOffRetry
	}
}

func (routine *SteeringHandOffRoutine) drop(ctx context.Context, log *slog.Logger, handOff ServerHandOff, reason string) handOffDecision {
	log.Info("steering hand-off: messages dropped", "reason", reason, "messages", len(handOff.SteeringIDs))
	if err := routine.store.DropServerHandOff(ctx, handOff); err != nil {
		log.Warn("steering hand-off: the drop is not recorded", "err", err)
		return handOffRetry
	}
	return handOffDropped
}
