package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runevidence"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auditexport"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// startStepRetention runs the run step routines (ADR-0042 decisions 1 and
// 4, issue #1499) on the routine registry, one replica at a time, each
// switchable on the admin Schedules page:
//
//   - main.run_step_anchor (every minute) anchors the runs whose settlement
//     asked for it, off the settlement path;
//   - main.step_content_retention (hourly) expires stored step content past
//     the step_content maximum (rows first), deletes the objects of every
//     expired side, including those a conversation deletion expired, and
//     the objects a failed persist left with no row (content journal);
//   - main.step_record_retention (hourly) anchors settled runs whose anchor
//     request was missed, then deletes whole runs past the step_record
//     maximum once every sink exporting run_steps passed them.
//
// All pause in maintenance; the sweeps skip what an active legal hold
// covers. With the seeded policies (keep forever) they expire nothing.
func startStepRetention(ctx context.Context, lookup func(string) (string, bool), pool *pgxpool.Pool, objectStore storage.ObjectStore, logger *slog.Logger) error {
	if pool == nil {
		return nil
	}
	// The sinks that export run_steps now: only their cursors hold a run's
	// deletion. The boot validated this configuration already.
	exportCfg, err := auditexport.ConfigFromEnv(lookup)
	if err != nil {
		return err
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
		runevidence.RetentionConfig{ExportSinks: exportCfg.RunStepsSinks()}, logger)
	if err != nil {
		return err
	}
	for _, routine := range []struct {
		id, description string
		schedule        string
		minInterval     time.Duration
		pass            func(context.Context) (string, error)
	}{
		{runevidence.AnchorRoutineID,
			"Anchors the step chains of settled runs into their tenant's security audit chain (head hash and step count), within about a minute of the run ending.",
			"1m", time.Minute, sweeper.AnchorPass},
		{runevidence.ContentRoutineID,
			"Expires run step content older than the step_content retention maximum, except under legal hold, deletes the stored content of expired steps, and deletes content objects no step record names an hour after they were written.",
			"1h", 5 * time.Minute, sweeper.ContentPass},
		{runevidence.RecordRoutineID,
			"Anchors settled runs' step chains whose anchor was missed, and deletes run step records older than the step_record retention maximum, except under legal hold.",
			"1h", 5 * time.Minute, sweeper.RecordPass},
	} {
		loop := &routines.Loop{
			Backend: routines.NewStore(pool),
			Def: routines.Definition{
				ID:              routine.id,
				Owner:           routines.OwnerMain,
				Description:     routine.description,
				DefaultSchedule: routine.schedule,
				MinInterval:     routine.minInterval,
			},
			Logger: logger,
		}
		go loop.Run(ctx, routine.pass)
	}
	return nil
}
