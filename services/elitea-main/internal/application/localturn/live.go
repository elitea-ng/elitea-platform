package localturn

import (
	"context"
	"errors"
	"log/slog"
)

// BindingStore reads what a started local turn is bound to.
type BindingStore interface {
	// ReadLocalTurnBinding answers the turn's state and its answering agent.
	// A turn that does not exist for this (project, actor) is ErrNotFound.
	ReadLocalTurnBinding(ctx context.Context, projectID, actorUserID int64, executionID string) (StoredBinding, error)
}

// StoredBinding is one turn's stored state. ApplicationID and VersionID name
// the agent version the answering participant was mapped to when the turn
// STARTED (pinned on the execution row); both are 0 for the conversation's
// model (dummy) participant.
type StoredBinding struct {
	ApplicationID int64
	VersionID     int64
	// Credential is the family that started the turn.
	Credential Credential
	Committed     bool
	Expired       bool
}

// LiveTurn is a turn that may still act: started by the caller in the
// project with the same credential family, not committed, not past its
// deadline, and local work allowed.
type LiveTurn struct {
	ExecutionID   string
	ApplicationID int64
	VersionID     int64
}

// LiveTurns answers whether a turn is live — the checks start and commit make
// (the native client policy's local_work.allowed, the owner, the project, the
// deadline, the commit), for an operation that runs INSIDE a turn: the remote
// toolkit call (ADR-0029 decision 5b) borrows exactly the authority of the
// turn it names.
type LiveTurns struct {
	store  BindingStore
	policy PolicyReader
	logger *slog.Logger
}

func NewLiveTurns(store BindingStore, policy PolicyReader, logger *slog.Logger) (*LiveTurns, error) {
	if store == nil || policy == nil {
		return nil, errors.New("live turn dependencies are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &LiveTurns{store: store, policy: policy, logger: logger}, nil
}

// Live answers the live turn, or ErrInvalid, ErrUnavailable,
// ErrLocalWorkDisabled, ErrNotFound, ErrAlreadyCommitted or ErrExpired. A turn
// another credential family started is ErrNotFound, like another caller's.
func (l *LiveTurns) Live(ctx context.Context, projectID, actorUserID int64, credential Credential, executionID string) (LiveTurn, error) {
	if l == nil || projectID <= 0 || projectID > 2147483647 ||
		actorUserID <= 0 || actorUserID > 2147483647 || credential.TokenID == "" || !ValidExecutionID(executionID) {
		return LiveTurn{}, ErrInvalid
	}
	policy, err := l.policy.Policy(ctx)
	if err != nil {
		l.logger.ErrorContext(ctx, "local turn: native client policy unreadable", "err", err)
		return LiveTurn{}, ErrUnavailable
	}
	if !policy.LocalWork.Allowed {
		return LiveTurn{}, ErrLocalWorkDisabled
	}
	binding, err := l.store.ReadLocalTurnBinding(ctx, projectID, actorUserID, executionID)
	if err != nil {
		return LiveTurn{}, err
	}
	if binding.Credential != credential {
		return LiveTurn{}, ErrNotFound
	}
	switch {
	case binding.Committed:
		return LiveTurn{}, ErrAlreadyCommitted
	case binding.Expired:
		return LiveTurn{}, ErrExpired
	}
	return LiveTurn{ExecutionID: executionID, ApplicationID: binding.ApplicationID, VersionID: binding.VersionID}, nil
}
