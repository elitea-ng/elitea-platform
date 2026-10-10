package runtimecomposition

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	indexscheduleapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexschedule"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This file composes the rust indexing runtime's side effects on the index
// registry (ELITEA_INDEXING_RUNTIME=rust, ADR-0030 decision 6). Each constructor
// is the registry counterpart of a python-mode constructor of the same role and
// returns the same processor type, so the reconcilers that drive them are
// shared and python mode does not change.

// newIndexRegistryComposition builds the registry dependencies of the index
// runtime. vectors is the elitea-vector deletion hook; until elitea-main has a
// client for it the composition root passes indexingapp.DeferredIndexVectorDeleter.
func newIndexRegistryComposition(
	pool *pgxpool.Pool,
	vectors indexingapp.IndexVectorDeleter,
	report func(error),
) (*indexRegistryComposition, error) {
	if vectors == nil || report == nil {
		return nil, errors.New("index registry composition dependencies are required")
	}
	repo, err := repos.NewIndexRegistryRepository(pool)
	if err != nil {
		return nil, err
	}
	return &indexRegistryComposition{repo: repo, vectors: vectors, report: report}, nil
}

// newRegistryIndexMetaTerminalProcessor is newCurrentIndexMetaTerminalProcessor
// for the registry. It needs no Configurations runtime: the registry row is
// addressed by the admission binding, so no frozen toolkit is redeemed.
func newRegistryIndexMetaTerminalProcessor(
	pool *pgxpool.Pool,
	registry *repos.IndexRegistryRepository,
	reportItemFailure func(error),
) (*currentIndexMetaTerminalProcessor, error) {
	if pool == nil || registry == nil || reportItemFailure == nil {
		return nil, errors.New("index registry terminal effect dependencies are required")
	}
	bindings, err := repos.NewCurrentIndexMetaTerminalBindingsRepository(pool)
	if err != nil {
		return nil, fmt.Errorf("construct current index metadata terminal bindings: %w", err)
	}
	terminalizer, err := indexingapp.NewRegistryIndexMetaTerminalizer(bindings, registry)
	if err != nil {
		return nil, err
	}
	concurrency, err := currentIndexMetaTerminalConcurrency(pool.Config().MaxConns)
	if err != nil {
		return nil, err
	}
	return &currentIndexMetaTerminalProcessor{
		terminalizer:      terminalizer,
		store:             bindings,
		newClaimID:        currentRuntimeID,
		claimLease:        2 * time.Minute,
		concurrency:       concurrency,
		reportItemFailure: reportItemFailure,
	}, nil
}

// newRegistryIndexManualStopCleanupProcessor is
// newCurrentIndexManualStopCleanupProcessor for the registry.
func newRegistryIndexManualStopCleanupProcessor(
	pool *pgxpool.Pool,
	registry *repos.IndexRegistryRepository,
	vectors indexingapp.IndexVectorDeleter,
	reportItemFailure func(error),
	reportDeferred func(indexingapp.RegistryManualStop),
) (*currentIndexManualStopCleanupProcessor, error) {
	if pool == nil || registry == nil || vectors == nil ||
		reportItemFailure == nil || reportDeferred == nil {
		return nil, errors.New("index registry manual Stop cleanup dependencies are required")
	}
	bindings, err := repos.NewCurrentIndexMetaTerminalBindingsRepository(pool)
	if err != nil {
		return nil, fmt.Errorf("construct current index manual Stop cleanup bindings: %w", err)
	}
	store, err := repos.NewCurrentIndexManualStopCleanupRepository(pool)
	if err != nil {
		return nil, fmt.Errorf("construct current index manual Stop cleanup store: %w", err)
	}
	cleaner, err := indexingapp.NewRegistryManualStopCleaner(bindings, registry, vectors, reportDeferred)
	if err != nil {
		return nil, err
	}
	return &currentIndexManualStopCleanupProcessor{
		cleaner:           cleaner,
		store:             store,
		newClaimID:        currentRuntimeID,
		claimLease:        2 * time.Minute,
		concurrency:       2,
		reportItemFailure: reportItemFailure,
	}, nil
}

type registryScheduleFailureWriter interface {
	RecordScheduledFailure(
		ctx context.Context,
		projectID, toolkitID int32,
		name, effectID, safeReason string,
		occurredAt time.Time,
	) error
}

// registryIndexScheduleFailureRecorder is currentIndexScheduleFailureRecorder
// for the registry. A scheduled run that could not start leaves a failed entry
// in the index's history and a notification for the schedule's creator. The
// python recorder resolves the toolkit's pgvector target first; the registry
// needs only the visibility check.
type registryIndexScheduleFailureRecorder struct {
	toolkits      indexingapp.CurrentToolkitReader
	history       registryScheduleFailureWriter
	notifications currentScheduleFailureNotifications
	now           func() time.Time
}

func newRegistryIndexScheduleFailureRecorder(
	toolkits indexingapp.CurrentToolkitReader,
	history registryScheduleFailureWriter,
	notifications currentScheduleFailureNotifications,
) (*registryIndexScheduleFailureRecorder, error) {
	if toolkits == nil || history == nil || notifications == nil {
		return nil, errors.New("index registry schedule failure dependencies are required")
	}
	return &registryIndexScheduleFailureRecorder{
		toolkits: toolkits, history: history, notifications: notifications, now: time.Now,
	}, nil
}

func (recorder *registryIndexScheduleFailureRecorder) RecordScheduleFailure(
	ctx context.Context,
	candidate indexscheduleapp.Candidate,
	safeReason string,
	occurrence time.Time,
) error {
	if recorder == nil || recorder.toolkits == nil || recorder.history == nil ||
		recorder.notifications == nil || recorder.now == nil || ctx == nil ||
		candidate.ProjectID <= 0 || candidate.ProjectID > math.MaxInt32 ||
		candidate.ToolkitID <= 0 || candidate.ToolkitID > math.MaxInt32 ||
		candidate.Schedule.CreatedBy <= 0 || candidate.Schedule.CreatedBy > math.MaxInt32 ||
		occurrence.IsZero() {
		return indexscheduleapp.ErrInvalidScheduleFailure
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	creatorID := candidate.Schedule.CreatedBy
	toolkit, found, err := recorder.toolkits.GetCurrentToolkit(
		ctx, int32(candidate.ProjectID), int32(creatorID), int32(candidate.ToolkitID),
	)
	if err != nil {
		return err
	}
	if !found || toolkit.ID != int32(candidate.ToolkitID) ||
		toolkit.Type != candidate.ToolkitType || toolkit.Settings == nil {
		return indexscheduleapp.ErrScheduleDependency
	}
	effect := indexscheduleapp.FailureEffect{
		EffectID:    indexscheduleapp.StableIdempotencyKey(candidate, occurrence),
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
	if err := recorder.history.RecordScheduledFailure(
		ctx,
		int32(candidate.ProjectID), int32(candidate.ToolkitID),
		effect.IndexMetaID, effect.EffectID, effect.SafeReason, effect.OccurredAt,
	); err != nil {
		return err
	}
	return recorder.notifications.Persist(ctx, effect)
}

var _ indexscheduleapp.FailureRecorder = (*registryIndexScheduleFailureRecorder)(nil)
