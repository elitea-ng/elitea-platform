package runtimecomposition

import (
	"context"
	"errors"
	"time"
)

// The agent admission reservation reaper (#965).
//
// A two-phase agent start reserves its durable slot in a short
// synchronous-commit-off transaction and materializes the execution rows in a
// second one. A process that dies between the two commits leaks the slot: the
// unmaterialized reservation keeps counting toward the live cap forever. This
// janitor runs the reaper DELETE on a fixed interval so a leaked slot is
// reclaimed after the stale window. The DELETE is idempotent, so every
// replica may run its own janitor; no fencing is needed.

const (
	// A healthy start materializes within the same request, so only a start
	// that has been dead this long can still own an unmaterialized slot.
	agentAdmissionReservationStaleWindow = 60 * time.Second
	// A materialized reservation is a replay shortcut, not a source of truth:
	// execution_jobs owns idempotency. Keep one day for retries and
	// inspection, then collect the row.
	agentAdmissionReservationGcWindow = 24 * time.Hour
	// Worst case to a reclaimed slot: stale window plus one poll interval.
	agentAdmissionReservationReaperPollInterval = 30 * time.Second
)

type agentAdmissionReservationStore interface {
	ReapAgentAdmissionReservations(context.Context, int64, int64) (int64, error)
}

// agentAdmissionReservationReaper owns exactly one synchronous maintenance
// loop. The repository bounds each pass; keeping the call inline prevents
// overlapping passes when PostgreSQL is slow.
type agentAdmissionReservationReaper struct {
	store         agentAdmissionReservationStore
	pollInterval  time.Duration
	reportFailure func(error)
	wait          func(context.Context, time.Duration) error
}

func newAgentAdmissionReservationReaper(
	store agentAdmissionReservationStore,
	pollInterval time.Duration,
	reportFailure func(error),
) (*agentAdmissionReservationReaper, error) {
	if store == nil || pollInterval <= 0 || reportFailure == nil {
		return nil, errors.New("agent admission reservation reaper dependencies are incomplete")
	}
	return &agentAdmissionReservationReaper{
		store:         store,
		pollInterval:  pollInterval,
		reportFailure: reportFailure,
		wait:          waitAgentAdmissionReservationReap,
	}, nil
}

func (r *agentAdmissionReservationReaper) RunOnce(ctx context.Context) error {
	if r == nil || r.store == nil || ctx == nil {
		return errors.New("agent admission reservation reaper is incomplete")
	}
	_, err := r.store.ReapAgentAdmissionReservations(
		ctx,
		int64(agentAdmissionReservationStaleWindow.Seconds()),
		int64(agentAdmissionReservationGcWindow.Seconds()),
	)
	return err
}

func (r *agentAdmissionReservationReaper) Run(ctx context.Context) error {
	if r == nil || r.store == nil || r.wait == nil || ctx == nil {
		return errors.New("agent admission reservation reaper is incomplete")
	}
	for {
		if err := r.RunOnce(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			r.reportFailure(err)
		}
		if err := r.wait(ctx, r.pollInterval); err != nil {
			return err
		}
	}
}

func waitAgentAdmissionReservationReap(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var _ publisherRunner = (*agentAdmissionReservationReaper)(nil)
