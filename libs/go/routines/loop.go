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
// local clock, which is the behaviour before the registry existed. The clock
// starts from the last run the registry showed (or from the moment the outage
// began), so an outage does not make every replica run the pass at once, and a
// routine the last read showed disabled stays off. A loop that never read its
// row (a fresh process on an unmigrated table) runs at once, like the old
// tickers did.
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
	// seen is the last row read, the standing instruction during an outage.
	seen *Row
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
			l.noteDegraded("register", err, now)
		} else {
			l.registered = true
		}
	}
	if l.registered {
		row, err := l.Backend.Get(ctx, l.Def.ID)
		if err == nil {
			l.degraded = false
			l.seen = &row
			return l.stepRegistered(ctx, pass, row, now)
		}
		l.noteDegraded("read", err, now)
	}
	l.stepLocal(ctx, pass, now)
	return l.poll()
}

func (l *Loop) noteDegraded(what string, err error, now time.Time) {
	if l.degraded {
		return
	}
	l.degraded = true
	if l.seen != nil {
		l.localLast = now
		if l.seen.LastRunAt != nil {
			l.localLast = *l.seen.LastRunAt
		}
	}
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
		return min(l.poll(), max(row.NextRunAt.Sub(now), 100*time.Millisecond))
	}
	// The claim holds the slot while the pass runs; the slot after it is
	// planned from the pass's end below, so a pass longer than the interval
	// does not leave the next run already due.
	held := schedule.Next(now)
	if held.IsZero() {
		l.logger().Warn("routines: schedule never fires; not running", "routine", l.Def.ID,
			"schedule", schedule.String())
		return l.poll()
	}
	claimed, err := l.Backend.Claim(ctx, l.Def.ID, row.NextRunAt, held)
	if err != nil {
		l.logger().Warn("routines: could not claim the next run", "routine", l.Def.ID, "err", err)
		return l.poll()
	}
	if !claimed {
		return l.poll()
	}
	outcome := l.run(ctx, pass)
	ranAt := now
	l.seen.LastRunAt = &ranAt
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := l.Backend.RecordRun(settleCtx, l.Def.ID, now, outcome); err != nil {
		l.logger().Warn("routines: could not record the run", "routine", l.Def.ID, "err", err)
	}
	// Moves only our own slot: an operator's edit during the pass re-planned
	// next_run_at and wins.
	if next := schedule.Next(l.now()); next.After(held) {
		if _, err := l.Backend.Claim(settleCtx, l.Def.ID, &held, next); err != nil {
			l.logger().Warn("routines: could not plan the next run", "routine", l.Def.ID, "err", err)
		}
	}
	return l.poll()
}

func (l *Loop) stepLocal(ctx context.Context, pass Pass, now time.Time) {
	if l.seen != nil && !l.seen.Enabled {
		return
	}
	schedule, err := ParseSchedule(l.Def.DefaultSchedule)
	if err != nil {
		return
	}
	if !l.localLast.IsZero() {
		next := schedule.Next(l.localLast)
		if next.IsZero() || next.After(now) {
			return
		}
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
