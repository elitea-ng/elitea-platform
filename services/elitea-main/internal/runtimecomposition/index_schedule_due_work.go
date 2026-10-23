package runtimecomposition

import (
	"context"
	"errors"
	"time"

	indexscheduleapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexschedule"
	schedulingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/scheduling"
)

const (
	currentIndexScheduleCapability     = "index.schedule.scan.v1"
	currentIndexScheduleRevision       = "current-index-scheduling-r1"
	currentIndexScheduleCadence        = "* * * * *"
	currentIndexScheduleLeaseDuration  = 2 * time.Minute
	currentIndexScheduleHandlerTimeout = 30 * time.Second
)

type currentIndexScheduleScanner interface {
	RunDue(context.Context, time.Time) (indexscheduleapp.TickResult, error)
}

// currentIndexScheduleDueWork is the typed indexing adapter behind the generic
// platform scheduler. It owns product-aware discovery and durable admission;
// it does not own a clock, replica claim, or occurrence ledger.
type currentIndexScheduleDueWork struct {
	indexes currentIndexScheduleScanner
}

func newCurrentIndexScheduleDueWork(
	indexes currentIndexScheduleScanner,
) (*currentIndexScheduleDueWork, error) {
	if indexes == nil {
		return nil, errors.New("current index schedule due work is required")
	}
	return &currentIndexScheduleDueWork{indexes: indexes}, nil
}

func (*currentIndexScheduleDueWork) Name() string {
	return currentIndexScheduleCapability
}

func (work *currentIndexScheduleDueWork) Execute(
	ctx context.Context,
	occurrence schedulingapp.Occurrence,
) (schedulingapp.Outcome, error) {
	if work == nil || work.indexes == nil || ctx == nil ||
		occurrence.InvocationID == "" ||
		occurrence.JobID != currentIndexScheduleCapability ||
		occurrence.ScheduleRevision != currentIndexScheduleRevision ||
		occurrence.DueAt.IsZero() ||
		occurrence.LeaseEpoch <= 0 ||
		occurrence.ClaimFence == "" {
		return "", indexscheduleapp.ErrInvalidRequest
	}
	result, err := work.indexes.RunDue(ctx, occurrence.DueAt)
	if err != nil {
		return "", err
	}
	if result.SkippedOverlap || result.SkippedUnavailable ||
		result.DependencyErrors > 0 {
		return "", indexscheduleapp.ErrScheduleDependency
	}
	return schedulingapp.OutcomeDurablyAdmitted, nil
}

var _ schedulingapp.Handler = (*currentIndexScheduleDueWork)(nil)
