package faults

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// cycleLogEvery is how often a runner that keeps failing logs an ERROR.
const cycleLogEvery = time.Minute

// CycleReporter reports a background runner's failed passes (FU-299 C3).
//
//   - While the dependency the failure belongs to is reported lost (its
//     Transitions entry is in the lost state, after asking its owner to
//     check now), the failure is logged at DEBUG: the one outage line
//     already says it.
//   - Otherwise it is an ERROR fault line (runner.cycle_failed), at most once
//     a minute, with the number of failures not logged since the previous
//     line. A query that times out every cycle on a healthy database is
//     therefore never hidden.
type CycleReporter struct {
	runner     string
	logger     *slog.Logger
	dependency func(error) string
	outages    *Transitions
	now        func() time.Time

	mu         sync.Mutex
	lastLogged time.Time
	suppressed int
}

// NewCycleReporter names the runner. dependency maps a failure to the
// Transitions name of the dependency it belongs to ("" when it is not a
// dependency failure that heals on its own).
func NewCycleReporter(runner string, logger *slog.Logger, outages *Transitions, dependency func(error) string) *CycleReporter {
	return &CycleReporter{runner: runner, logger: logger, dependency: dependency, outages: outages, now: time.Now}
}

// Report logs one failed pass.
func (r *CycleReporter) Report(err error) {
	ctx := context.Background()
	if r.dependency != nil {
		if name := r.dependency(err); name != "" && (r.outages.IsLost(name) || r.outages.Confirm(name)) {
			r.logger.Log(ctx, slog.LevelDebug, RunnerCycleFailed.Sentence(), "runner", r.runner, "dependency", name, "err", err)
			return
		}
	}
	r.mu.Lock()
	now := r.now()
	if !r.lastLogged.IsZero() && now.Sub(r.lastLogged) < cycleLogEvery {
		r.suppressed++
		r.mu.Unlock()
		return
	}
	suppressed := r.suppressed
	r.suppressed = 0
	r.lastLogged = now
	r.mu.Unlock()
	Log(ctx, r.logger, slog.LevelError, New(RunnerCycleFailed, err, slog.String("runner", r.runner), slog.Int("suppressed", suppressed)))
}
