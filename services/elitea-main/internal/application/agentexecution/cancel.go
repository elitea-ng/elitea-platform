package agentexecution

import (
	"context"
	"errors"
	"math"
	"strings"
)

var (
	ErrInvalidCurrentAgentCancel    = errors.New("invalid current agent cancel request")
	ErrCurrentAgentCancelNotAllowed = errors.New("current agent response cannot be stopped by this actor")
	ErrCurrentAgentCancelFailed     = errors.New("current agent cancellation is unavailable")
)

// CurrentAgentCancelRequest identifies the response projection and actor at
// the current UI compatibility boundary. The repository resolves the exact
// durable execution from the response instead of trusting a client task ID.
type CurrentAgentCancelRequest struct {
	ProjectID         int64
	ActorUserID       int64
	ResponseMessageID string
}

func (request CurrentAgentCancelRequest) Validate() error {
	if request.ProjectID <= 0 || request.ProjectID > math.MaxInt32 ||
		request.ActorUserID <= 0 || !validUUID(request.ResponseMessageID) {
		return ErrInvalidCurrentAgentCancel
	}
	return nil
}

// CanonicalCurrentAgentResponseMessageID lower-cases a canonical UUID of
// either case and returns anything else unchanged, for Validate to refuse.
// The stop's path parameter is declared `format: uuid`, which admits either
// case, and a client that re-serialises the id the server gave it (Swift's
// uuidString) sends it upper-case. Unlike a turn's question_id this id is not
// a client-minted idempotency key: the server minted and stored it in
// lowercase, so folding the case names the same stored answer and lets the
// replay check match it as text.
func CanonicalCurrentAgentResponseMessageID(value string) string {
	if validStoredUUID(value) {
		return strings.ToLower(value)
	}
	return value
}

type CurrentAgentCancelOutcome struct {
	Deleted  bool
	Salvaged bool
	Replay   bool
}

type CurrentAgentCancellationStore interface {
	CancelCurrentAgent(
		context.Context,
		CurrentAgentCancelRequest,
	) (CurrentAgentCancelOutcome, error)
}

type CurrentAgentCancellationService struct {
	store CurrentAgentCancellationStore
}

func NewCurrentAgentCancellationService(
	store CurrentAgentCancellationStore,
) (*CurrentAgentCancellationService, error) {
	if store == nil {
		return nil, errors.New("current agent cancellation store is required")
	}
	return &CurrentAgentCancellationService{store: store}, nil
}

func (service *CurrentAgentCancellationService) Cancel(
	ctx context.Context,
	request CurrentAgentCancelRequest,
) (CurrentAgentCancelOutcome, error) {
	if service == nil || service.store == nil || ctx == nil {
		return CurrentAgentCancelOutcome{}, ErrInvalidCurrentAgentCancel
	}
	if err := request.Validate(); err != nil {
		return CurrentAgentCancelOutcome{}, err
	}
	if err := ctx.Err(); err != nil {
		return CurrentAgentCancelOutcome{}, err
	}

	outcome, err := service.store.CancelCurrentAgent(ctx, request)
	if err == nil {
		return outcome, nil
	}
	if contextError := ctx.Err(); contextError != nil {
		return CurrentAgentCancelOutcome{}, contextError
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrCurrentAgentCancelNotAllowed) {
		return CurrentAgentCancelOutcome{}, err
	}
	return CurrentAgentCancelOutcome{}, ErrCurrentAgentCancelFailed
}
