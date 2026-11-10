package runtimecomposition

import (
	"log/slog"
	"sync"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/depfailure"
)

// cycleReporters holds one faults.CycleReporter per background runner, so
// each runner's once-a-minute state lives as long as the runtime (FU-299).
type cycleReporters struct {
	logger  *slog.Logger
	outages *faults.Transitions

	mu        sync.Mutex
	reporters map[string]*faults.CycleReporter
}

func newCycleReporters(dependencies Dependencies) *cycleReporters {
	logger := dependencies.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	outages := dependencies.Outages
	if outages == nil {
		outages = faults.NewTransitions()
	}
	return &cycleReporters{logger: logger, outages: outages, reporters: map[string]*faults.CycleReporter{}}
}

// report logs one failed pass of runner.
func (c *cycleReporters) report(runner string, err error) {
	c.mu.Lock()
	reporter, ok := c.reporters[runner]
	if !ok {
		reporter = faults.NewCycleReporter(runner, c.logger, c.outages, depfailure.Dependencies)
		c.reporters[runner] = reporter
	}
	c.mu.Unlock()
	reporter.Report(err)
}
