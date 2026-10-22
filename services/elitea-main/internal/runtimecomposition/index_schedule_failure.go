package runtimecomposition

import (
	"context"

	indexscheduleapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexschedule"
)

type currentScheduleFailureNotifications interface {
	Persist(context.Context, indexscheduleapp.FailureEffect) error
}

type currentIndexSchedulingAvailability struct{}

func (currentIndexSchedulingAvailability) SchedulingAvailable(
	ctx context.Context,
) (bool, error) {
	if ctx == nil {
		return false, indexscheduleapp.ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, nil
}

var _ indexscheduleapp.Availability = currentIndexSchedulingAvailability{}
