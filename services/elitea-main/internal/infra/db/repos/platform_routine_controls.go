package repos

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	schedulingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/scheduling"
)

// PlatformRoutineControls is the platform scheduler's view of the routine
// registry (`elitea_runtime.platform_routines`, issue #1369): it registers each
// job's default schedule, reads the operator's enabled flag and schedule
// override, and writes back what the runner did.
type PlatformRoutineControls struct {
	store *routines.Store
}

// NewPlatformRoutineControls builds the controls over a pool.
func NewPlatformRoutineControls(pool *pgxpool.Pool) (*PlatformRoutineControls, error) {
	if pool == nil {
		return nil, errors.New("platform routine controls require a pool")
	}
	return &PlatformRoutineControls{store: routines.NewStore(pool)}, nil
}

var _ schedulingapp.Controls = (*PlatformRoutineControls)(nil)

// Register records every job's identity and default schedule. The operator's
// override and enabled flag on an existing row are kept.
func (c *PlatformRoutineControls) Register(ctx context.Context, jobs []schedulingapp.Job) error {
	definitions := make([]routines.Definition, 0, len(jobs))
	for _, job := range jobs {
		definitions = append(definitions, routines.Definition{
			ID:              job.ID,
			Owner:           routines.OwnerMain,
			Description:     job.Description,
			DefaultSchedule: job.Cadence,
			MinInterval:     job.MinInterval,
		})
	}
	return c.store.Register(ctx, definitions...)
}

// Resolve returns the standing instruction for every registered routine. A
// stored override that no longer parses is ignored, so the job keeps its code
// schedule rather than stopping.
func (c *PlatformRoutineControls) Resolve(ctx context.Context) (map[string]schedulingapp.Control, error) {
	rows, err := c.store.List(ctx)
	if err != nil {
		return nil, err
	}
	controls := make(map[string]schedulingapp.Control, len(rows))
	for _, row := range rows {
		control := schedulingapp.Control{Enabled: row.Enabled, NextRunAt: row.NextRunAt}
		if row.Overridden() {
			if parsed, parseErr := routines.ParseSchedule(*row.ScheduleOverride); parseErr == nil {
				control.Schedule = parsed
			}
		}
		controls[row.ID] = control
	}
	return controls, nil
}

// Report stores a run outcome and/or the next planned run.
func (c *PlatformRoutineControls) Report(ctx context.Context, report schedulingapp.RoutineReport) error {
	var err error
	if !report.RanAt.IsZero() {
		err = c.store.RecordRun(ctx, report.JobID, report.RanAt, report.Outcome)
	}
	if !report.NextRunAt.IsZero() {
		err = errors.Join(err, c.store.ObserveNext(ctx, report.JobID, report.NextRunAt))
	}
	return err
}
