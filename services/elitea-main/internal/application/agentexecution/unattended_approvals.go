package agentexecution

import (
	"context"
	"errors"
	"log/slog"
	"time"

	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

// Unattended approval bounds (ADR-0041 decision 5, #1491).
const (
	// UnattendedApprovalSweepBatch bounds one pass: at most this many paused
	// runs are answered, and at most this many waits expired.
	UnattendedApprovalSweepBatch = 16
	// UnattendedApprovalSweepInterval is how often the worker looks for a
	// skip to answer and a wait that expired.
	UnattendedApprovalSweepInterval = 15 * time.Second
	// UnattendedApprovalLease is how long a claimed skip is left to its
	// claimer before another pass may try it again.
	UnattendedApprovalLease = time.Minute
	// UnattendedApprovalMaxAttempts ends a skip whose continuation keeps
	// failing: the run then ends with approval_required_unattended.
	UnattendedApprovalMaxAttempts = 10
)

// UnattendedSkipTarget is one paused unattended run whose action is skip:
// every pending pause of the run, each with its refusal.
type UnattendedSkipTarget struct {
	ProjectID         int64
	OwnerUserID       int64
	ConversationUUID  string
	ResponseMessageID string
	ExecutionID       string
	TriggerOrigin     executiondomain.TriggerOrigin
	Decisions         []CurrentHITLDecision
	Attempts          int
}

// UnattendedApprovalStore holds the durable skip and wait intents.
type UnattendedApprovalStore interface {
	// ClaimUnattendedSkips claims at most limit paused runs with QUEUED skip
	// intents that no pass tried within lease, and counts the attempt.
	ClaimUnattendedSkips(ctx context.Context, limit int, lease time.Duration) ([]UnattendedSkipTarget, error)
	// CloseUnattendedSkip ends the intents of a run whose pause is already
	// answered (a person decided it, or a Stop ended it).
	CloseUnattendedSkip(ctx context.Context, target UnattendedSkipTarget) error
	// FailUnattendedSkip ends a skip that cannot be applied: the run ends
	// with approval_required_unattended, and each pause gets its
	// unattended_failed ledger row.
	FailUnattendedSkip(ctx context.Context, target UnattendedSkipTarget, failureCode string) error
	// ExpireUnattendedWaits closes at most limit expired waits as timed_out
	// and ends their runs with approval_timed_out.
	ExpireUnattendedWaits(ctx context.Context, limit int) (int, error)
}

// UnattendedContinuer is the HITL continuation use case.
type UnattendedContinuer interface {
	ContinueCurrentAgent(ctx context.Context, request CurrentContinuationRequest) (CurrentApplicationStartOutcome, error)
}

// UnattendedApprovalWorker applies the skip and notify_and_wait actions of
// unattended runs. A skip is answered with the refusal the pause recorded,
// through the ordinary HITL continuation, as the run's owner (the identity
// the run executes as) and with the run's automated trigger origin, so the
// admission transaction records the decision as Main's unattended_skipped,
// never as the owner's. It never approves.
type UnattendedApprovalWorker struct {
	store     UnattendedApprovalStore
	continuer UnattendedContinuer
	logger    *slog.Logger
	interval  time.Duration
}

// NewUnattendedApprovalWorker builds the worker.
func NewUnattendedApprovalWorker(store UnattendedApprovalStore, continuer UnattendedContinuer, logger *slog.Logger) (*UnattendedApprovalWorker, error) {
	if store == nil || continuer == nil || logger == nil {
		return nil, errors.New("unattended approval worker dependencies are required")
	}
	return &UnattendedApprovalWorker{store: store, continuer: continuer, logger: logger,
		interval: UnattendedApprovalSweepInterval}, nil
}

// Run sweeps until ctx ends.
func (w *UnattendedApprovalWorker) Run(ctx context.Context) error {
	if w == nil || ctx == nil {
		return errors.New("unattended approval worker is not configured")
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if err := w.Sweep(ctx); err != nil && ctx.Err() == nil {
			w.logger.Error("unattended approval pass failed", "reason", "unavailable")
		}
	}
}

// Sweep runs one bounded pass: it answers the queued skips, then expires
// the waits that ran out.
func (w *UnattendedApprovalWorker) Sweep(ctx context.Context) error {
	targets, err := w.store.ClaimUnattendedSkips(ctx, UnattendedApprovalSweepBatch, UnattendedApprovalLease)
	if err != nil {
		return err
	}
	for _, target := range targets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.skip(ctx, target)
	}
	if _, err := w.store.ExpireUnattendedWaits(ctx, UnattendedApprovalSweepBatch); err != nil {
		return err
	}
	return nil
}

func (w *UnattendedApprovalWorker) skip(ctx context.Context, target UnattendedSkipTarget) {
	_, err := w.continuer.ContinueCurrentAgent(ctx, CurrentContinuationRequest{
		ProjectID: target.ProjectID, ActorUserID: target.OwnerUserID,
		ConversationUUID: target.ConversationUUID, ResponseMessageID: target.ResponseMessageID,
		Kind: CurrentContinuationHITL, HITLDecisions: target.Decisions,
		TriggerOrigin: target.TriggerOrigin,
	})
	switch {
	case err == nil:
		// The admission transaction recorded the decisions and ended the
		// intents.
	case errors.Is(err, ErrCurrentAgentHITLAlreadyResolved):
		if closeErr := w.store.CloseUnattendedSkip(ctx, target); closeErr != nil {
			w.logger.Warn("unattended skip close deferred", "reason", "unavailable")
		}
	case errors.Is(err, ErrInvalidCurrentAgentStart), errors.Is(err, ErrUnsupportedCurrentAgentStart),
		target.Attempts >= UnattendedApprovalMaxAttempts:
		code := "continuation_refused"
		if target.Attempts >= UnattendedApprovalMaxAttempts {
			code = "attempts_exhausted"
		}
		if failErr := w.store.FailUnattendedSkip(ctx, target, code); failErr != nil {
			w.logger.Warn("unattended skip failure deferred", "reason", "unavailable")
		}
	default:
		// Capacity or a database error: the lease spaces the next try.
		w.logger.Warn("unattended skip deferred", "reason", "admission_unavailable")
	}
}
