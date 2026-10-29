package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runevidence"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// startStepRetention runs the step evidence retention sweeps (ADR-0042
// decision 4, issue #1499) on the routine registry, one replica at a time,
// hourly by default and switchable on the admin Schedules page:
//
//   - main.step_content_retention expires stored step content past the
//     step_content maximum (rows first) and deletes the objects of every
//     expired side, including those a conversation deletion expired;
//   - main.step_record_retention anchors settled runs whose anchor was
//     missed, then deletes whole runs past the step_record maximum.
//
// Both skip what an active legal hold covers and pause in maintenance. With
// the seeded policies (keep forever) they delete nothing.
func startStepRetention(ctx context.Context, pool *pgxpool.Pool, objectStore storage.ObjectStore, logger *slog.Logger) error {
	if pool == nil {
		return nil
	}
	store, err := dbrepos.ComposeRunSteps(pool)
	if err != nil {
		return err
	}
	var content runevidence.ContentStore
	if objectStore != nil {
		if content, err = storage.NewRunEvidenceContent(objectStore); err != nil {
			return err
		}
	}
	// Fail closed: an unreadable maintenance switch stops the pass.
	gate := func(ctx context.Context) (bool, error) {
		state, err := platformconfig.LoadMaintenance(ctx, pool)
		if err != nil {
			return false, err
		}
		return state.Enabled, nil
	}
	sweeper, err := runevidence.NewSweeper(dbrepos.NewRetentionPolicyRepo(pool), store, content, gate,
		runevidence.RetentionConfig{}, logger)
	if err != nil {
		return err
	}
	for _, routine := range []struct {
		id, description string
		pass            func(context.Context) (string, error)
	}{
		{runevidence.ContentRoutineID,
			"Expires run step content older than the step_content retention maximum, except under legal hold, and deletes the stored content of expired steps.",
			sweeper.ContentPass},
		{runevidence.RecordRoutineID,
			"Anchors settled runs' step chains whose anchor was missed, and deletes run step records older than the step_record retention maximum, except under legal hold.",
			sweeper.RecordPass},
	} {
		loop := &routines.Loop{
			Backend: routines.NewStore(pool),
			Def: routines.Definition{
				ID:              routine.id,
				Owner:           routines.OwnerMain,
				Description:     routine.description,
				DefaultSchedule: "1h",
				MinInterval:     5 * time.Minute,
			},
			Logger: logger,
		}
		go loop.Run(ctx, routine.pass)
	}
	return nil
}
