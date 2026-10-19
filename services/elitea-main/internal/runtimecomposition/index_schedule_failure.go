package runtimecomposition

import (
	"context"
	"errors"
	"math"
	"time"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	indexmetaapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexmeta"
	indexscheduleapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexschedule"
)

type currentScheduleFailureHistory interface {
	MaterializeScheduledFailure(
		context.Context,
		indexmetaapp.ResolvedTarget,
		indexscheduleapp.FailureEffect,
	) error
}

type currentScheduleFailureNotifications interface {
	Persist(context.Context, indexscheduleapp.FailureEffect) error
}

type currentIndexScheduleFailureRecorder struct {
	toolkits      indexingapp.CurrentToolkitReader
	settings      indexingapp.CurrentToolkitSettingsValidator
	history       currentScheduleFailureHistory
	notifications currentScheduleFailureNotifications
	now           func() time.Time
}

func newCurrentIndexScheduleFailureRecorderWithClock(
	toolkits indexingapp.CurrentToolkitReader,
	settings indexingapp.CurrentToolkitSettingsValidator,
	history currentScheduleFailureHistory,
	notifications currentScheduleFailureNotifications,
	now func() time.Time,
) (*currentIndexScheduleFailureRecorder, error) {
	if toolkits == nil || settings == nil || history == nil ||
		notifications == nil || now == nil {
		return nil, errors.New(
			"current index schedule failure dependencies are required",
		)
	}
	return &currentIndexScheduleFailureRecorder{
		toolkits: toolkits, settings: settings, history: history,
		notifications: notifications, now: now,
	}, nil
}

func (recorder *currentIndexScheduleFailureRecorder) RecordScheduleFailure(
	ctx context.Context,
	candidate indexscheduleapp.Candidate,
	safeReason string,
	occurrence time.Time,
) error {
	if recorder == nil || recorder.toolkits == nil ||
		recorder.settings == nil || recorder.history == nil ||
		recorder.notifications == nil || recorder.now == nil || ctx == nil ||
		candidate.ProjectID <= 0 || candidate.ProjectID > math.MaxInt32 ||
		candidate.ToolkitID <= 0 || candidate.ToolkitID > math.MaxInt32 ||
		candidate.Schedule.CreatedBy <= 0 ||
		candidate.Schedule.CreatedBy > math.MaxInt32 ||
		occurrence.IsZero() {
		return indexscheduleapp.ErrInvalidScheduleFailure
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	creatorID := candidate.Schedule.CreatedBy
	toolkit, found, err := recorder.toolkits.GetCurrentToolkit(
		ctx,
		int32(candidate.ProjectID),
		int32(creatorID),
		int32(candidate.ToolkitID),
	)
	if err != nil {
		return err
	}
	if !found || toolkit.ID != int32(candidate.ToolkitID) ||
		toolkit.Type != candidate.ToolkitType || toolkit.Settings == nil {
		return indexscheduleapp.ErrScheduleDependency
	}
	target, err := indexmetaapp.ResolveCurrentTargetSnapshot(
		ctx,
		recorder.settings,
		indexmetaapp.Request{
			ProjectID:   candidate.ProjectID,
			ActorUserID: creatorID,
			ToolkitID:   candidate.ToolkitID,
		},
		toolkit,
		2,
	)
	if err != nil {
		return err
	}
	effect := indexscheduleapp.FailureEffect{
		EffectID: indexscheduleapp.StableIdempotencyKey(
			candidate,
			occurrence,
		),
		ProjectID:   candidate.ProjectID,
		UserID:      creatorID,
		ToolkitID:   candidate.ToolkitID,
		IndexMetaID: candidate.IndexMetaID,
		SafeReason:  safeReason,
		OccurredAt:  recorder.now().UTC(),
	}
	if err := effect.Validate(); err != nil {
		return err
	}
	if err := recorder.history.MaterializeScheduledFailure(
		ctx,
		target,
		effect,
	); err != nil {
		return err
	}
	return recorder.notifications.Persist(ctx, effect)
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

var (
	_ indexscheduleapp.FailureRecorder = (*currentIndexScheduleFailureRecorder)(nil)
	_ indexscheduleapp.Availability    = currentIndexSchedulingAvailability{}
)
