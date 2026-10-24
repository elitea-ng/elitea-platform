package routines

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Backend is what a [Loop] needs from the registry. [Store] implements it; a
// test substitutes an in-memory one.
type Backend interface {
	Register(ctx context.Context, definitions ...Definition) error
	Get(ctx context.Context, id string) (Row, error)
	Acquire(ctx context.Context, id string, expected *time.Time, held time.Time, holder string, ttl time.Duration) (Lease, bool, error)
	Renew(ctx context.Context, id string, lease Lease, ttl time.Duration) error
	Release(ctx context.Context, id string, lease Lease, done Completion) error
}

var _ Backend = (*Store)(nil)

// Pass is one run of a worker. It returns one of the Outcome words and the
// error that explains an [OutcomeError].
type Pass func(ctx context.Context) (string, error)

// DefaultPoll is how often a loop re-reads its registry row. A change made in
// the admin screen takes effect within one poll, without a restart.
const DefaultPoll = 15 * time.Second

// DefaultLeaseTTL is how long a pass's lease lasts without a renewal. A
// running pass renews it every third of that, so a live holder keeps it
// however long the pass runs, and a crashed one loses it within this long.
const DefaultLeaseTTL = 2 * time.Minute

// Loop runs one elitea-scheduler worker on the registry's schedule.
//
// It replaces the worker's fixed ticker. Every poll it reads the routine's row;
// a disabled routine does not run; an enabled one runs when `next_run_at` has
// passed, after the replica has claimed that slot in the database so two
// replicas do not both run it.
//
// The claim also takes a lease on the row (#1414), renewed while the pass
// runs and dropped when it ends. No replica claims a routine whose lease is
// live, so a pass that outlasts its interval does not start again elsewhere;
// a holder that crashes stops renewing, and once its lease runs out another
// replica takes the routine over. A holder that loses its lease (renewals
// failed for a whole TTL, or the token moved on) has its pass's context
// cancelled with [ErrLeaseLost].
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
	// LeaseTTL is the lease's length without a renewal; zero means
	// [DefaultLeaseTTL]. A pass renews it every LeaseTTL/3.
	LeaseTTL time.Duration
	// Holder names this replica in the lease; empty means host, pid and a
	// random suffix.
	Holder string

	// renewEvery overrides the renewal interval (ttl/3); tests only.
	renewEvery time.Duration

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

func (l *Loop) leaseTTL() time.Duration {
	if l.LeaseTTL > 0 {
		return l.LeaseTTL
	}
	return DefaultLeaseTTL
}

func (l *Loop) holder() string {
	if l.Holder == "" {
		l.Holder = defaultHolder()
	}
	return l.Holder
}

func defaultHolder() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), hex.EncodeToString(suffix))
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
	ttl := l.leaseTTL()
	// Stamped before the statement is sent: the lease the database grants
	// expires ttl after it EXECUTES, so ttl after this instant is the
	// earliest the lease can run out.
	issued := time.Now()
	lease, claimed, err := l.Backend.Acquire(ctx, l.Def.ID, row.NextRunAt, held, l.holder(), ttl)
	if err != nil {
		l.logger().Warn("routines: could not claim the next run", "routine", l.Def.ID, "err", err)
		return l.poll()
	}
	if !claimed {
		return l.poll()
	}
	outcome := l.runLeased(ctx, pass, lease, ttl, issued)
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	// Moves only our own slot: an operator's edit during the pass re-planned
	// next_run_at and wins.
	done := Completion{RanAt: now, Outcome: outcome, Held: held}
	if next := schedule.Next(l.now()); next.After(held) {
		done.Next = next
	}
	switch err := l.Backend.Release(settleCtx, l.Def.ID, lease, done); {
	case err == nil:
		ranAt := now
		l.seen.LastRunAt = &ranAt
	case errors.Is(err, ErrLeaseLost):
		// Fencing working as designed: the lease ran out and another replica
		// took the routine over, so this pass's outcome is not ours to write.
		l.logger().Info("routines: lease was taken over by another replica; this run is not recorded",
			"routine", l.Def.ID, "lease_token", lease.Token)
	default:
		ranAt := now
		l.seen.LastRunAt = &ranAt
		l.logger().Warn("routines: could not record the run and release the lease", "routine", l.Def.ID,
			"lease_token", lease.Token, "err", err)
	}
	return l.poll()
}

// runLeased runs the pass while a goroutine renews the lease every ttl/3.
//
// The holder gives up strictly before its lease can run out. `renewed` is the
// instant the last successful renewal was ISSUED (the database stamps the new
// expiry when the statement executes, so no earlier than that), and the lease
// therefore lives at least until renewed+ttl. The pass's context is cancelled
// with [ErrLeaseLost] once renewed+ttl-every is reached without a success,
// a margin of one renewal interval before the earliest expiry, and each
// renewal's own timeout ends at that same deadline, so a slow renewal cannot
// carry the holder past it. The context is also cancelled when the row's
// token moved on (another replica took the lease).
//
// When the pass ends, the renewer is cancelled before it is waited for: a
// renewal in flight does not hold up the end of the pass.
func (l *Loop) runLeased(ctx context.Context, pass Pass, lease Lease, ttl time.Duration, issued time.Time) string {
	passCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	renewerCtx, stopRenewer := context.WithCancel(passCtx)
	every := l.renewEvery
	if every <= 0 {
		every = ttl / 3
	}
	every = max(every, time.Millisecond)
	giveUpAfter := ttl - every
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		renewed := issued
		for {
			select {
			case <-renewerCtx.Done():
				return
			case <-ticker.C:
			}
			deadline := renewed.Add(giveUpAfter)
			if !time.Now().Before(deadline) {
				l.logger().Warn("routines: lease could not be renewed before it may run out; stopping the pass",
					"routine", l.Def.ID, "lease_token", lease.Token)
				cancel(ErrLeaseLost)
				return
			}
			attempt := time.Now()
			renewCtx, renewCancel := context.WithDeadline(renewerCtx, deadline)
			err := l.Backend.Renew(renewCtx, l.Def.ID, lease, ttl)
			renewCancel()
			switch {
			case err == nil:
				renewed = attempt
			case renewerCtx.Err() != nil:
				return
			case errors.Is(err, ErrLeaseLost):
				l.logger().Warn("routines: lease taken over; stopping the pass", "routine", l.Def.ID,
					"lease_token", lease.Token)
				cancel(ErrLeaseLost)
				return
			default:
				if !time.Now().Before(deadline) {
					l.logger().Warn("routines: lease could not be renewed before it may run out; stopping the pass",
						"routine", l.Def.ID, "lease_token", lease.Token, "err", err)
					cancel(ErrLeaseLost)
					return
				}
				l.logger().Warn("routines: could not renew the lease", "routine", l.Def.ID, "err", err)
			}
		}
	})
	outcome := l.run(passCtx, pass)
	stopRenewer()
	wg.Wait()
	return outcome
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
