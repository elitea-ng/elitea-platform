package agentexecution

import (
	"context"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// Steering during execution (client contract 1.27). A user sends a message
// while a cloud turn runs. The message waits in a durable queue until the
// worker reaches its next safe point, after the tool results of a step and
// before the next model request. The worker claims it over gRPC and adds it to
// the model request as a user message of the same turn. When the model
// checkpoint that holds the request is persisted, the worker acknowledges the
// message, and only then is it a conversation message (injected).

// MaxSteeringContentBytes is the bound of one steering message (UTF-8 bytes).
const MaxSteeringContentBytes = 16 << 10

// MaxPendingSteeringMessages is the bound of the queue of one turn.
const MaxPendingSteeringMessages = 10

// MaxClaimedSteeringMessages is the bound of one claim. It equals the queue
// bound, so one safe point takes every pending message.
const MaxClaimedSteeringMessages = MaxPendingSteeringMessages

// MaxClaimedSteeringBytes bounds the text of one claim, so the answer stays
// inside the worker's 80 KiB control response bound. A claim always takes
// at least the oldest message (at most MaxSteeringContentBytes).
const MaxClaimedSteeringBytes = 48 << 10

// MaxAcknowledgedSteeringIDs bounds the acknowledgements of one call. A turn
// holds at most a few queues of MaxPendingSteeringMessages each.
const MaxAcknowledgedSteeringIDs = 64

// SteeringState is the state of one steering message on the wire.
type SteeringState string

const (
	SteeringPending  SteeringState = "pending"
	SteeringInjected SteeringState = "injected"
	SteeringRemoved  SteeringState = "removed"
	// SteeringHandedOff is final: the turn ended first, and a client (or the
	// server's hand-off routine) took the message as the next turn's message.
	SteeringHandedOff SteeringState = "handed_off"
	// SteeringTurnEnded is read, not stored: a pending message whose turn is
	// no longer running. No claim can reach it, so the client sends it as the
	// next turn's message.
	SteeringTurnEnded SteeringState = "turn_ended"
)

// The steering event types of the execution event stream.
const (
	SteeringEventQueued   = "steer.queued"
	SteeringEventInjected = "steer.injected"
	SteeringEventDropped  = "steer.dropped"

	// SteeringDropRemoved is the steer.dropped reason of a message its author
	// removed, the only reason there is.
	SteeringDropRemoved = "removed"
)

var (
	ErrInvalidSteering             = errors.New("invalid steering request")
	ErrSteeringIdempotencyRequired = errors.New("steering idempotency key is required")
	ErrSteeringTurnNotRunning      = errors.New("the turn is not running")
	ErrSteeringNotSupported        = errors.New("the turn cannot be steered")
	ErrSteeringQueueFull           = errors.New("the steering queue is full")
	ErrSteeringIdempotencyConflict = errors.New("the steering idempotency key names another message")
	ErrSteeringNotFound            = errors.New("no such steering message")
	ErrSteeringNotPending          = errors.New("the steering message is not pending")
	ErrSteeringFailed              = errors.New("steering is unavailable")
)

var steeringIdempotencyKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// SteeringMessage is one queued message as the API and the events show it.
type SteeringMessage struct {
	ID                string
	ResponseMessageID string
	AuthorUserID      int64
	Content           string
	State             SteeringState
	Revision          int32
	CreatedAt         time.Time
	UpdatedAt         time.Time
	InjectedAt        *time.Time
	MessageID         string
}

// SteeringTarget names the running answer and the caller.
type SteeringTarget struct {
	ProjectID         int64
	ActorUserID       int64
	ResponseMessageID string
}

func (target SteeringTarget) Validate() error {
	if target.ProjectID <= 0 || target.ProjectID > math.MaxInt32 ||
		target.ActorUserID <= 0 || !validUUID(target.ResponseMessageID) {
		return ErrInvalidSteering
	}
	return nil
}

type SteeringQueueRequest struct {
	SteeringTarget
	IdempotencyKey string
	Content        string
}

type SteeringEditRequest struct {
	SteeringTarget
	SteeringID string
	Content    string
}

type SteeringRemoveRequest struct {
	SteeringTarget
	SteeringID string
}

// SteeringEffect is one event the store appended to an execution's durable
// event stream. The service wakes the stream readers with it after commit.
type SteeringEffect struct {
	ProjectID   int64
	ExecutionID string
	Cursor      uint64
}

// SteeringHandOffRequest is one client hand-off of turn_ended messages.
type SteeringHandOffRequest struct {
	SteeringTarget
	IdempotencyKey string
}

// SteeringClaimRequest is one call of the worker at a safe point: the
// messages its persisted checkpoints hold (Acknowledged), then, unless
// AcknowledgeOnly, a claim of the messages that wait.
type SteeringClaimRequest struct {
	Fence           runtimedomain.Fence
	Acknowledged    []string
	AcknowledgeOnly bool
}

// ClaimedSteeringMessage is one message the worker injects now.
type ClaimedSteeringMessage struct {
	ID       string
	Content  string
	QueuedAt time.Time
}

// SteeringStore is the durable queue. Each method runs in one transaction.
type SteeringStore interface {
	QueueSteering(context.Context, SteeringQueueRequest) (SteeringMessage, []SteeringEffect, error)
	ListSteering(context.Context, SteeringTarget) ([]SteeringMessage, error)
	EditSteering(context.Context, SteeringEditRequest) (SteeringMessage, []SteeringEffect, error)
	RemoveSteering(context.Context, SteeringRemoveRequest) ([]SteeringEffect, error)
	// HandOffSteering takes the caller's turn_ended messages of the answer
	// as the next turn's message (state handed_off), in one guarded update,
	// and answers them. A retry with the same key answers the same messages.
	HandOffSteering(ctx context.Context, request SteeringHandOffRequest) ([]SteeringMessage, error)
	// ClaimSteering first injects the acknowledged messages (writes each as
	// a conversation message), then claims the messages that wait for this
	// lease epoch. It locks the claim named by the fence and refuses a stale
	// one (runtime.ErrStaleFence).
	ClaimSteering(ctx context.Context, request SteeringClaimRequest) ([]ClaimedSteeringMessage, []SteeringEffect, error)
}

// SteeringWaker wakes the event stream readers of one execution. It never
// blocks; a lost wake costs latency only (the readers poll).
type SteeringWaker interface {
	Notify(projectID, executionID string, cursor uint64)
}

// SteeringService validates steering calls and wakes the event stream.
type SteeringService struct {
	store SteeringStore
	waker SteeringWaker
}

func NewSteeringService(store SteeringStore, waker SteeringWaker) (*SteeringService, error) {
	if store == nil {
		return nil, errors.New("steering store is required")
	}
	return &SteeringService{store: store, waker: waker}, nil
}

// ValidSteeringContent reports whether content is a message the queue takes:
// valid UTF-8, at most MaxSteeringContentBytes, and not only white space.
func ValidSteeringContent(content string) bool {
	return content != "" && len(content) <= MaxSteeringContentBytes &&
		utf8.ValidString(content) && strings.TrimSpace(content) != ""
}

func (service *SteeringService) Queue(ctx context.Context, request SteeringQueueRequest) (SteeringMessage, error) {
	if err := service.ready(ctx, request.SteeringTarget); err != nil {
		return SteeringMessage{}, err
	}
	if !steeringIdempotencyKey.MatchString(request.IdempotencyKey) {
		return SteeringMessage{}, ErrSteeringIdempotencyRequired
	}
	if !ValidSteeringContent(request.Content) {
		return SteeringMessage{}, ErrInvalidSteering
	}
	message, effects, err := service.store.QueueSteering(ctx, request)
	if err != nil {
		return SteeringMessage{}, service.safe(ctx, err)
	}
	service.wake(effects)
	return message, nil
}

func (service *SteeringService) List(ctx context.Context, target SteeringTarget) ([]SteeringMessage, error) {
	if err := service.ready(ctx, target); err != nil {
		return nil, err
	}
	messages, err := service.store.ListSteering(ctx, target)
	if err != nil {
		return nil, service.safe(ctx, err)
	}
	return messages, nil
}

func (service *SteeringService) Edit(ctx context.Context, request SteeringEditRequest) (SteeringMessage, error) {
	if err := service.ready(ctx, request.SteeringTarget); err != nil {
		return SteeringMessage{}, err
	}
	if !validUUID(request.SteeringID) || !ValidSteeringContent(request.Content) {
		return SteeringMessage{}, ErrInvalidSteering
	}
	message, effects, err := service.store.EditSteering(ctx, request)
	if err != nil {
		return SteeringMessage{}, service.safe(ctx, err)
	}
	service.wake(effects)
	return message, nil
}

func (service *SteeringService) Remove(ctx context.Context, request SteeringRemoveRequest) error {
	if err := service.ready(ctx, request.SteeringTarget); err != nil {
		return err
	}
	if !validUUID(request.SteeringID) {
		return ErrInvalidSteering
	}
	effects, err := service.store.RemoveSteering(ctx, request)
	if err != nil {
		return service.safe(ctx, err)
	}
	service.wake(effects)
	return nil
}

// HandOff takes the caller's turn_ended messages of the answer, so the client
// sends them as one message. The server's hand-off routine takes rows the same
// way, so a message is taken once.
func (service *SteeringService) HandOff(ctx context.Context, request SteeringHandOffRequest) ([]SteeringMessage, error) {
	if err := service.ready(ctx, request.SteeringTarget); err != nil {
		return nil, err
	}
	if !steeringIdempotencyKey.MatchString(request.IdempotencyKey) || strings.HasPrefix(request.IdempotencyKey, ServerHandOffKey) {
		return nil, ErrSteeringIdempotencyRequired
	}
	messages, err := service.store.HandOffSteering(ctx, request)
	if err != nil {
		return nil, service.safe(ctx, err)
	}
	if messages == nil {
		messages = []SteeringMessage{}
	}
	return messages, nil
}

// Claim is the worker's half: the gRPC control server calls it after it has
// authenticated the peer and verified the claim fence.
func (service *SteeringService) Claim(ctx context.Context, request SteeringClaimRequest) ([]ClaimedSteeringMessage, error) {
	if service == nil || service.store == nil || ctx == nil || request.Fence.Validate() != nil ||
		len(request.Acknowledged) > MaxAcknowledgedSteeringIDs {
		return nil, ErrInvalidSteering
	}
	for _, id := range request.Acknowledged {
		if !validUUID(id) {
			return nil, ErrInvalidSteering
		}
	}
	messages, effects, err := service.store.ClaimSteering(ctx, request)
	if err != nil {
		if errors.Is(err, runtimedomain.ErrStaleFence) {
			return nil, runtimedomain.ErrStaleFence
		}
		return nil, service.safe(ctx, err)
	}
	service.wake(effects)
	return messages, nil
}

func (service *SteeringService) ready(ctx context.Context, target SteeringTarget) error {
	if service == nil || service.store == nil || ctx == nil {
		return ErrSteeringFailed
	}
	if err := target.Validate(); err != nil {
		return err
	}
	return ctx.Err()
}

// WakeEffects wakes the event streams of steering events that another
// writer (the hand-off routine) appended.
func (service *SteeringService) WakeEffects(effects []SteeringEffect) { service.wake(effects) }

func (service *SteeringService) wake(effects []SteeringEffect) {
	if service.waker == nil {
		return
	}
	for _, effect := range effects {
		if effect.Cursor == 0 || effect.ExecutionID == "" || effect.ProjectID <= 0 {
			continue
		}
		service.waker.Notify(strconv.FormatInt(effect.ProjectID, 10), effect.ExecutionID, effect.Cursor)
	}
}

// safe keeps the typed outcomes and folds every other error into
// ErrSteeringFailed, so no raw cause crosses the API boundary.
func (service *SteeringService) safe(ctx context.Context, err error) error {
	if contextError := ctx.Err(); contextError != nil {
		return contextError
	}
	for _, known := range []error{
		context.Canceled, context.DeadlineExceeded,
		ErrInvalidSteering, ErrSteeringTurnNotRunning, ErrSteeringNotSupported,
		ErrSteeringQueueFull, ErrSteeringIdempotencyConflict, ErrSteeringNotFound,
		ErrSteeringNotPending,
	} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrSteeringFailed
}

// ValidSteeringID reports whether value is a canonical lowercase UUID.
func ValidSteeringID(value string) bool {
	return validUUID(value)
}
