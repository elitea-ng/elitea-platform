package runtimecomposition

// The scheduled half of the personal-access-token expiry notice (#940 A3).
//
// This file is an ADAPTER and nothing else: the rule — who is due, what the
// notification says, how it is deduped — lives in
// internal/application/patexpiry and internal/infra/db/repos/
// pat_expiry_notification.go. The same notifier is reachable from
// `POST /api/v2/admin/background_jobs/administration/pat_expiry_notices:run`,
// deliberately, so that an operator's run-now and the scheduler's tick cannot
// be two different behaviours (admin/pat_expiry_notices.go's header).
//
// Mode is ModeLocalBounded, like the retention sweep and unlike the index
// scan: every effect — reading the candidates, writing the notification,
// writing its mark — completes inside Execute. There is no downstream durable
// system this handler merely admits work into.

import (
	"context"
	"errors"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/patexpiry"
	schedulingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/scheduling"
)

const (
	patExpirySweepCapability = "identity.pat.expiry.notice.v1"
	patExpirySweepRevision   = "pat-expiry-notice-r1"
	// Hourly, not every fifteen minutes. The window is a whole day wide and
	// the mark makes a repeat a no-op, so a finer cadence buys nothing but
	// table reads; an hour still leaves twenty-three hours of warning for a
	// token that becomes eligible a minute after a tick.
	patExpirySweepCadence        = "7 * * * *"
	patExpirySweepHandlerTimeout = 25 * time.Second
)

var errPATExpirySweepInvalidOccurrence = errors.New("invalid personal access token expiry sweep occurrence")

// patExpiryRunner is the one method this adapter needs from the notifier.
type patExpiryRunner interface {
	Run(ctx context.Context, now time.Time, within time.Duration) (patexpiry.Result, error)
}

type patExpirySweep struct {
	notifier patExpiryRunner
}

func newPATExpirySweep(notifier patExpiryRunner) (*patExpirySweep, error) {
	if notifier == nil {
		return nil, errors.New("personal access token expiry notifier is required")
	}
	return &patExpirySweep{notifier: notifier}, nil
}

func (*patExpirySweep) Name() string {
	return patExpirySweepCapability
}

func (s *patExpirySweep) Execute(
	ctx context.Context,
	occurrence schedulingapp.Occurrence,
) (schedulingapp.Outcome, error) {
	if s == nil || s.notifier == nil || ctx == nil ||
		occurrence.InvocationID == "" ||
		occurrence.JobID != patExpirySweepCapability ||
		occurrence.ScheduleRevision != patExpirySweepRevision ||
		occurrence.DueAt.IsZero() ||
		occurrence.LeaseEpoch <= 0 ||
		occurrence.ClaimFence == "" {
		return "", errPATExpirySweepInvalidOccurrence
	}
	// The production window, always. `within` is an operator control on the
	// run-now route; a scheduled tick has no one to have asked for anything
	// else.
	if _, err := s.notifier.Run(ctx, time.Now().UTC(), patexpiry.DefaultWindow); err != nil {
		return "", err
	}
	return schedulingapp.OutcomeLocalCompleted, nil
}

var _ schedulingapp.Handler = (*patExpirySweep)(nil)
