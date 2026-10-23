package routines

import (
	"context"
	"log/slog"
	"time"
)

// Backend is what a [Loop] needs from the registry. [Store] implements it; a
// test substitutes an in-memory one.
type Backend interface {
	Register(ctx context.Context, definitions ...Definition) error
	Get(ctx context.Context, id string) (Row, error)
	Claim(ctx context.Context, id string, expected *time.Time, next time.Time) (bool, error)
	RecordRun(ctx context.Context, id string, at time.Time, outcome string) error
}

var _ Backend = (*Store)(nil)

// Pass is one run of a worker. It returns one of the Outcome words and the
// error that explains an [OutcomeError].
type Pass func(ctx context.Context) (string, error)

// DefaultPoll is how often a loop re-reads its registry row. A change made in
// the admin screen takes effect within one poll, without a restart.
const DefaultPoll = 15 * time.Second

// Loop runs one elitea-scheduler worker on the registry's schedule.
//
// It replaces the worker's fixed ticker. Every poll it reads the routine's row;
// a disabled routine does not run; an enabled one runs when `next_run_at` has
// passed, after the replica has claimed that slot in the database so two
// replicas do not both run it.
//
// If the registry cannot be reached (the table is not migrated yet, or the
// database blips), the loop falls back to the routine's default schedule on a
// local clock, which is the behaviour before the registry existed. An
// operator's disable is not seen during such an outage; it applies again on
// the next successful read.
type Loop struct {
	Backend Backend
	Def     Definition
	Poll    time.Duration
	Logger  *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	registered bool
	degraded   bool
	localLast  time.Time
}

func (l *Loop) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Loop) poll() time.Duration {
	if l.Poll > 0 {
		return l.Poll
	}
	return DefaultPoll
}

func (l *Loop) logger() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.Default()
}

// Run blocks until ctx ends.
func (l *Loop) Run(ctx context.Context, pass Pass) {
	for {
		wait := l.Step(ctx, pass)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Step performs one poll and returns how long to wait before the next one.
// Exported so tests drive the loop with a clock they control.
func (l *Loop) Step(ctx context.Context, pass Pass) time.Duration {
	now := l.now()

	if !l.registered {
		if err := l.Backend.Register(ctx, l.Def); err != nil {
			l.noteDegraded("register", err)
		} else {
			l.registered = true
		}
	}
	if l.registered {
		row, err := l.Backend.Get(ctx, l.Def.ID)
		if err == nil {
			l.degraded = false
			return l.stepRegistered(ctx, pass, row, now)
		}
		l.noteDegraded("read", err)
	}
	l.stepLocal(ctx, pass, now)
	return l.poll()
}

func (l *Loop) noteDegraded(what string, err error) {
	if l.degraded {
		return
	}
	l.degraded = true
	l.logger().Warn("routines: registry unavailable; running on the default schedule",
		"routine", l.Def.ID, "step", what, "err", err)
}

func (l *Loop) stepRegistered(ctx context.Context, pass Pass, row Row, now time.Time) time.Duration {
	if !row.Enabled {
		return l.poll()
	}
	schedule, err := ParseSchedule(row.EffectiveSchedule())
	if err != nil {
		// An override that no longer parses (a hand edit of the table) must not
		// stop the routine: fall back to the code's own default.
		l.logger().Warn("routines: stored schedule is invalid; using the default",
			"routine", l.Def.ID, "schedule", row.EffectiveSchedule(), "err", err)
		if schedule, err = ParseSchedule(row.DefaultSchedule); err != nil {
			return l.poll()
		}
	}
	if row.NextRunAt != nil && row.NextRunAt.After(now) {
		return min(l.poll(), max(time.Until(*row.NextRunAt), 100*time.Millisecond))
	}
	claimed, err := l.Backend.Claim(ctx, l.Def.ID, row.NextRunAt, schedule.Next(now))
	if err != nil {
		l.logger().Warn("routines: could not claim the next run", "routine", l.Def.ID, "err", err)
		return l.poll()
	}
	if !claimed {
		return l.poll()
	}
	outcome := l.run(ctx, pass)
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := l.Backend.RecordRun(recordCtx, l.Def.ID, now, outcome); err != nil {
		l.logger().Warn("routines: could not record the run", "routine", l.Def.ID, "err", err)
	}
	return l.poll()
}

func (l *Loop) stepLocal(ctx context.Context, pass Pass, now time.Time) {
	schedule, err := ParseSchedule(l.Def.DefaultSchedule)
	if err != nil {
		return
	}
	if !l.localLast.IsZero() && schedule.Next(l.localLast).After(now) {
		return
	}
	l.localLast = now
	l.run(ctx, pass)
}

func (l *Loop) run(ctx context.Context, pass Pass) string {
	outcome, err := pass(ctx)
	if err != nil {
		return OutcomeError
	}
	if outcome == "" {
		return OutcomeOK
	}
	return outcome
}
