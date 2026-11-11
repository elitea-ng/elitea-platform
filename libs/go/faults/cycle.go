package faults

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// cycleLogEvery is how often a runner that keeps failing the same way logs
// an ERROR.
const cycleLogEvery = time.Minute

// CycleReporter reports a background runner's failed passes (FU-299 C3).
//
//   - While a dependency the failure may belong to is reported lost (its
//     Transitions entry is in the lost state, after asking its owner to
//     check now), the failure is logged at DEBUG: the one outage line
//     already says it.
//   - Otherwise it is an ERROR fault line (runner.cycle_failed), at most once
//     a minute per runner and failure class (each dependency, and "other"
//     for everything else), with the number of failures of that class not
//     logged at ERROR since. A permanent fault is therefore never hidden
//     behind a stream of timeouts, a query that times out every cycle is
//     never hidden at all, and the failures between two ERROR lines are
//     still written at DEBUG.
type CycleReporter struct {
	runner       string
	logger       *slog.Logger
	dependencies func(error) []string
	outages      *Transitions
	now          func() time.Time

	mu      sync.Mutex
	windows map[string]*cycleWindow
}

type cycleWindow struct {
	lastLogged time.Time
	suppressed int
}

// NewCycleReporter names the runner. dependencies maps a failure to the
// Transitions names of the dependencies it may belong to (none when it is not
// a dependency failure that heals on its own; both "db" and "nats" when it
// cannot tell, such as a timeout).
func NewCycleReporter(runner string, logger *slog.Logger, outages *Transitions, dependencies func(error) []string) *CycleReporter {
	return &CycleReporter{runner: runner, logger: logger, dependencies: dependencies, outages: outages, now: time.Now, windows: map[string]*cycleWindow{}}
}

// Report logs one failed pass.
func (r *CycleReporter) Report(err error) {
	ctx := context.Background()
	class := "other"
	if r.dependencies != nil {
		names := r.dependencies(err)
		for _, name := range names {
			if r.outages.IsLost(name) {
				r.debug(ctx, err, name)
				return
			}
		}
		for _, name := range names {
			if r.outages.Confirm(name) {
				r.debug(ctx, err, name)
				return
			}
		}
		if len(names) > 0 {
			class = names[0]
		}
	}
	r.mu.Lock()
	window, ok := r.windows[class]
	if !ok {
		window = &cycleWindow{}
		r.windows[class] = window
	}
	now := r.now()
	if !window.lastLogged.IsZero() && now.Sub(window.lastLogged) < cycleLogEvery {
		window.suppressed++
		r.mu.Unlock()
		r.logger.Log(ctx, slog.LevelDebug, RunnerCycleFailed.Sentence(), "runner", r.runner, "class", class, "err", err)
		return
	}
	suppressed := window.suppressed
	window.suppressed = 0
	window.lastLogged = now
	r.mu.Unlock()
	Log(ctx, r.logger, slog.LevelError, New(RunnerCycleFailed, err, slog.String("runner", r.runner), slog.String("class", class), slog.Int("suppressed", suppressed)))
}

func (r *CycleReporter) debug(ctx context.Context, err error, dependency string) {
	r.logger.Log(ctx, slog.LevelDebug, RunnerCycleFailed.Sentence(), "runner", r.runner, "dependency", dependency, "err", err)
}
