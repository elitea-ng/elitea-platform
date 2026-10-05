package noderecovery

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
)

var (
	ErrInvalid     = errors.New("invalid node recovery request")
	ErrNotAllowed  = errors.New("node recovery is unavailable for this actor or visit")
	ErrUnavailable = errors.New("node recovery control is unavailable")
)

type Selector struct {
	ProjectID         int64
	ActorUserID       int64
	ResponseMessageID string
}

func (s Selector) Validate() error {
	if s.ProjectID <= 0 || s.ProjectID > math.MaxInt32 || s.ActorUserID <= 0 || !domain.ValidResponseMessageID(s.ResponseMessageID) {
		return ErrInvalid
	}
	return nil
}

type Submission struct {
	Selector
	Request domain.Request
}

func (s Submission) Validate() error {
	if s.Selector.Validate() != nil || s.Request.Validate() != nil {
		return ErrInvalid
	}
	return nil
}

type State struct {
	Schema      string          `json:"schema"`
	ExecutionID string          `json:"execution_id"`
	Generation  uint64          `json:"generation"`
	Status      string          `json:"status"`
	Receipt     json.RawMessage `json:"receipt"`
}

type Outcome struct {
	Schema           string `json:"schema"`
	ExecutionID      string `json:"execution_id"`
	Generation       uint64 `json:"generation"`
	RequestID        string `json:"request_id"`
	ActivationID     string `json:"activation_id"`
	ExpectedRevision uint64 `json:"expected_revision"`
	Action           string `json:"action"`
	Replay           bool   `json:"replay"`
}

type Store interface {
	Read(context.Context, Selector) (State, error)
	Submit(context.Context, Submission) (Outcome, error)
}

type Service struct{ store Store }

func New(store Store) (*Service, error) {
	if store == nil {
		return nil, ErrInvalid
	}
	return &Service{store: store}, nil
}

func (s *Service) Read(ctx context.Context, selector Selector) (State, error) {
	if s == nil || s.store == nil || ctx == nil || selector.Validate() != nil {
		return State{}, ErrInvalid
	}
	state, err := s.store.Read(ctx, selector)
	if err != nil {
		return State{}, safeError(err)
	}
	if state.Schema != "elitea.pipeline.node-recovery-state.v1" || !domain.ValidExecutionID(state.ExecutionID) || state.Generation == 0 || state.Generation > math.MaxInt64 || (state.Status != "suspended" && state.Status != "authorized" && state.Status != "running") {
		return State{}, ErrUnavailable
	}
	if _, err := domain.DecodeReceipt(state.Receipt); err != nil {
		return State{}, ErrUnavailable
	}
	state.Receipt = append(json.RawMessage(nil), state.Receipt...)
	return state, nil
}

func (s *Service) Submit(ctx context.Context, submission Submission) (Outcome, error) {
	if s == nil || s.store == nil || ctx == nil || submission.Validate() != nil {
		return Outcome{}, ErrInvalid
	}
	outcome, err := s.store.Submit(ctx, submission)
	if err != nil {
		return Outcome{}, safeError(err)
	}
	r := submission.Request
	if outcome.Schema != "elitea.pipeline.node-recovery-accepted.v1" || outcome.ExecutionID != r.ExecutionID || outcome.Generation != r.Generation || outcome.RequestID != r.RequestID || outcome.ActivationID != r.ActivationID || outcome.ExpectedRevision != r.ExpectedRevision || outcome.Action != r.Action {
		return Outcome{}, ErrUnavailable
	}
	return outcome, nil
}

func safeError(err error) error {
	if errors.Is(err, ErrNotAllowed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return ErrUnavailable
}
