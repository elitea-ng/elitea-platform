package agentexecution

import (
	"context"
	"errors"
	"log/slog"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
)

// Wake admission bounds (fanout-interrupt-decisions v1.1 §8.1).
const (
	// FanoutWakeSweepBatch bounds one pass: at most this many wake rows are
	// claimed per statement.
	FanoutWakeSweepBatch = 16
	// FanoutWakeSweepInterval is the periodic sweep for rows whose
	// after-commit admission was lost (crash between commit and admit).
	FanoutWakeSweepInterval = 5 * time.Second
	// FanoutWakeAdmissionLease is how long a claimed row is skipped by other
	// admitters before it may be claimed again (database clock).
	FanoutWakeAdmissionLease = 30 * time.Second
	// FanoutWakeMaxAttempts ends a wake that keeps failing admission with
	// the typed failure admission_exhausted (cards stay DECIDED and visible).
	FanoutWakeMaxAttempts = 20
)

// FanoutWakeStore claims QUEUED wake rows and records typed failures.
//
// ClaimFanoutWakes claims at most limit QUEUED rows that no other admitter
// holds (FOR UPDATE SKIP LOCKED; a claimed row is skipped for lease on the
// database clock), rechecks the parked actor's permission and the response
// ownership, fails rows whose actor lost access or whose attempts are
// exhausted, and returns the rest with their frozen request.
type FanoutWakeStore interface {
	ClaimFanoutWakes(ctx context.Context, limit int, lease time.Duration, maxAttempts int) ([]FanoutWakeTarget, error)
	FailFanoutWake(ctx context.Context, turn CurrentFanoutWakeTurn, failureCode string) error
}

// FanoutWakeSubmitter is the ordinary agent admission (AdmissionService).
type FanoutWakeSubmitter interface {
	Submit(context.Context, SubmitRequest) (executionapp.AdmissionOutcome, error)
}

// FanoutWakeAdmitter turns a QUEUED execution_interrupt_wakes row into
// exactly one fanout_wake continuation job. It is the single owner of wake
// admission in this process: Kick runs a pass right after the transaction
// that queued a wake commits, and Run also sweeps periodically for rows
// whose kick was lost. Exactly one job per wake holds across processes
// because the admission transaction moves the row QUEUED → ADMITTED with the
// job, under the row lock, behind the idempotency key
// continue-interrupt/{response}/{revision}.
type FanoutWakeAdmitter struct {
	store    FanoutWakeStore
	submit   FanoutWakeSubmitter
	logger   *slog.Logger
	interval time.Duration
	kick     chan struct{}
}

func NewFanoutWakeAdmitter(store FanoutWakeStore, submit FanoutWakeSubmitter, logger *slog.Logger) (*FanoutWakeAdmitter, error) {
	if store == nil || submit == nil || logger == nil {
		return nil, errors.New("fanout wake admitter dependencies are required")
	}
	return &FanoutWakeAdmitter{store: store, submit: submit, logger: logger,
		interval: FanoutWakeSweepInterval, kick: make(chan struct{}, 1)}, nil
}

// Kick asks for a pass now. It never blocks: kicks coalesce, and a kick that
// finds a pass already pending is absorbed by it.
func (a *FanoutWakeAdmitter) Kick() {
	if a == nil {
		return
	}
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// Run serves kicks and the periodic sweep until ctx ends. One goroutine, one
// pass at a time.
func (a *FanoutWakeAdmitter) Run(ctx context.Context) error {
	if a == nil || ctx == nil {
		return errors.New("fanout wake admitter is not configured")
	}
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-a.kick:
		case <-ticker.C:
		}
		if _, err := a.Sweep(ctx); err != nil && ctx.Err() == nil {
			a.logger.Error("fanout wake admission pass failed", "reason", "unavailable")
		}
	}
}

// Sweep runs one bounded pass and reports how many jobs it created. A row it
// cannot admit for a transient reason (capacity, database) stays QUEUED for
// a later pass; a typed refusal ends the row FAILED with its reason.
func (a *FanoutWakeAdmitter) Sweep(ctx context.Context) (int, error) {
	targets, err := a.store.ClaimFanoutWakes(ctx, FanoutWakeSweepBatch, FanoutWakeAdmissionLease, FanoutWakeMaxAttempts)
	if err != nil {
		return 0, err
	}
	created := 0
	for _, target := range targets {
		if ctx.Err() != nil {
			return created, ctx.Err()
		}
		outcome, err := a.admit(ctx, target)
		switch {
		case err == nil:
			if outcome.Created {
				created++
			}
		case errors.Is(err, ErrFanoutWakeNotQueued):
			// Admitted elsewhere, or closed by stop, regenerate or a new turn.
		case errors.Is(err, ErrFanoutWakeActorNotAllowed):
			a.fail(ctx, target.Turn, FanoutWakeFailureActorNotAllowed)
		case errors.Is(err, ErrFanoutWakeTargetGone):
			a.fail(ctx, target.Turn, FanoutWakeFailureResponseNotParked)
		case errors.Is(err, errInvalidFanoutWake):
			a.fail(ctx, target.Turn, FanoutWakeFailureParkedInput)
		case errors.Is(err, ErrInvalidAgentAdmission), errors.Is(err, executionapp.ErrInvalidAdmission),
			errors.Is(err, executionapp.ErrIdempotencyConflict):
			a.fail(ctx, target.Turn, FanoutWakeFailureAdmission)
		case errors.Is(err, executionapp.ErrAdmissionCapacityExhausted):
			// Stays QUEUED; the next pass after the lease retries.
		default:
			if ctx.Err() != nil {
				return created, ctx.Err()
			}
			// Fixed reason only: never the response, decision or input.
			a.logger.Warn("fanout wake admission deferred", "reason", "admission_unavailable")
		}
	}
	return created, nil
}

func (a *FanoutWakeAdmitter) admit(ctx context.Context, target FanoutWakeTarget) (executionapp.AdmissionOutcome, error) {
	input, err := FanoutWakeInput(target)
	if err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	turn := target.Turn
	return a.submit.Submit(ctx, SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID: target.TenantID, ResourceProjectID: target.ResourceProjectID,
			ProjectionProjectID: target.ProjectionProjectID, ActorID: target.ActorID,
		},
		IdempotencyKey:        FanoutWakeIdempotencyKey(turn.ResponseMessageID, turn.DecisionRevision),
		CapabilityID:          target.CapabilityID,
		ClientStreamID:        turn.ConversationUUID,
		ClientMessageID:       turn.ResponseMessageID,
		SIOEvent:              "chat_continue_predict",
		Input:                 input,
		CurrentFanoutWakeTurn: turn.Clone(),
		TriggerOrigin:         target.TriggerOrigin,
	})
}

func (a *FanoutWakeAdmitter) fail(ctx context.Context, turn CurrentFanoutWakeTurn, code string) {
	if err := a.store.FailFanoutWake(ctx, turn, code); err != nil && ctx.Err() == nil {
		a.logger.Warn("fanout wake failure was not recorded", "reason", "unavailable", "failure_code", code)
		return
	}
	a.logger.Info("fanout wake ended without a continuation", "failure_code", code)
}
