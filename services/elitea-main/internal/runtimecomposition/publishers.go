package runtimecomposition

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/depfailure"
)

// Runner restart bounds (FU-275). A runner that stops on a dependency failure
// that heals on its own is started again after runnerRestartMinDelay, doubling
// to runnerRestartMaxDelay. The delay keeps growing across restarts and
// resets only after the runner has run for runnerCleanRunDuration without
// stopping, so a runner that fails right after every restart backs off to the
// cap instead of restarting every second.
const (
	runnerRestartMinDelay  = time.Second
	runnerRestartMaxDelay  = 30 * time.Second
	runnerCleanRunDuration = time.Minute
)

// namedRunner gives a runner the name operators read in logs and errors.
type namedRunner struct {
	name   string
	runner publisherRunner
}

func (n namedRunner) Run(ctx context.Context) error { return n.runner.Run(ctx) }

// named labels a runner for the publisher set. A nil runner stays nil so the
// set's own completeness check still refuses it.
func named(name string, runner publisherRunner) publisherRunner {
	if runner == nil {
		return nil
	}
	return namedRunner{name: name, runner: runner}
}

// publisherSet gives a fixed set of durable runners one lifecycle owner.
//
// A runner that stops on a dependency failure that heals on its own
// (depfailure.Transient) is restarted after a bounded backoff, one copy at a
// time, and is reported as degraded until it has run cleanly again. Any other
// stop cancels every sibling and is returned, naming the runner, only after
// all runner goroutines have exited.
type publisherSet struct {
	publishers []namedRunner
	logger     *slog.Logger

	// Seams for tests; production uses the real clock and jitter.
	now    func() time.Time
	wait   func(context.Context, time.Duration) error
	jitter func(time.Duration) time.Duration

	mu        sync.Mutex
	restarted map[string]time.Time
}

func newConfiguredPublisherSet(indexEnabled bool, validation, index publisherRunner) (*publisherSet, error) {
	if validation == nil {
		return nil, errors.New("validation publisher is required")
	}
	if indexEnabled {
		if index == nil {
			return nil, errors.New("enabled index ingest publisher is required")
		}
		return newPublisherSet(named("validation outbox publisher", validation), named("index ingest outbox publisher", index))
	}
	if index != nil {
		return nil, errors.New("index ingest publisher requires explicit enablement")
	}
	return newPublisherSet(named("validation outbox publisher", validation))
}

// newPublisherSet flattens nested sets so every runner is supervised once, by
// name. An unnamed runner is named after its type.
func newPublisherSet(publishers ...publisherRunner) (*publisherSet, error) {
	if len(publishers) == 0 {
		return nil, errors.New("at least one runtime publisher is required")
	}
	owned := make([]namedRunner, 0, len(publishers))
	for _, publisher := range publishers {
		switch runner := publisher.(type) {
		case nil:
			return nil, errors.New("runtime publisher is required")
		case *publisherSet:
			if runner == nil {
				return nil, errors.New("runtime publisher is required")
			}
			owned = append(owned, runner.publishers...)
		case namedRunner:
			if runner.runner == nil {
				return nil, errors.New("runtime publisher is required")
			}
			owned = append(owned, runner)
		default:
			owned = append(owned, namedRunner{name: fmt.Sprintf("%T", publisher), runner: publisher})
		}
	}
	return &publisherSet{publishers: owned}, nil
}

// withLogger sets where restarts and fatal stops are reported.
func (s *publisherSet) withLogger(logger *slog.Logger) *publisherSet {
	s.logger = logger
	return s
}

// DegradedRunners names the runners restarted after a dependency failure that
// have not yet run for runnerCleanRunDuration since, in name order.
func (s *publisherSet) DegradedRunners() []string {
	if s == nil {
		return nil
	}
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.restarted))
	for name, at := range s.restarted {
		if now.Sub(at) < runnerCleanRunDuration {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (s *publisherSet) Run(ctx context.Context) error {
	if ctx == nil || len(s.publishers) == 0 {
		return errors.New("runtime publisher lifecycle is incomplete")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(s.publishers))
	for _, runner := range s.publishers {
		go func() {
			results <- result{name: runner.name, err: s.supervise(runCtx, runner)}
		}()
	}

	first := <-results
	ownerCancellation := ctx.Err()
	cancel()
	collected := make([]result, 0, len(s.publishers))
	collected = append(collected, first)
	for len(collected) < len(s.publishers) {
		collected = append(collected, <-results)
	}

	causes := make([]error, 0, len(collected))
	for _, outcome := range collected {
		if outcome.err == nil || errors.Is(outcome.err, errRunnerStoppedByOwner) {
			continue
		}
		causes = append(causes, outcome.err)
	}
	if len(causes) != 0 {
		return errors.Join(causes...)
	}
	if ownerCancellation != nil {
		return ownerCancellation
	}
	return errors.New("runtime runners stopped without a cause or an owning cancellation")
}

// errRunnerStoppedByOwner marks a runner that ended because the set's context
// ended; it is never a cause on its own.
var errRunnerStoppedByOwner = errors.New("runtime runner stopped by its owner")

// supervise runs one runner until its owner ends or it stops for a reason
// that does not heal on its own. It starts the runner again only after the
// previous Run has returned, so there is never more than one copy.
func (s *publisherSet) supervise(ctx context.Context, runner namedRunner) error {
	failures := 0
	for {
		startedAt := s.clock()
		err := runner.Run(ctx)
		if ctx.Err() != nil {
			return errRunnerStoppedByOwner
		}
		if err == nil {
			s.reportFatal(runner.name, errors.New("it returned without an error while Main was running"))
			return fmt.Errorf("runtime runner %q stopped unexpectedly", runner.name)
		}
		if !depfailure.Transient(err, ctx) {
			s.reportFatal(runner.name, err)
			return fmt.Errorf("runtime runner %q stopped: %w", runner.name, err)
		}
		if s.clock().Sub(startedAt) >= runnerCleanRunDuration {
			failures = 0
		}
		failures++
		delay := runnerRestartDelay(failures, s.jitterSource())
		s.markRestarted(runner.name)
		s.log().Warn(
			fmt.Sprintf("Main restarts the %s runner after a dependency failure, in %s", runner.name, delay.Round(time.Millisecond)),
			"runner", runner.name,
			"attempt", failures,
			"delay", delay.String(),
			"cause", depfailure.Describe(err),
		)
		if err := s.waitFor(ctx, delay); err != nil {
			return errRunnerStoppedByOwner
		}
	}
}

// runnerRestartDelay is runnerRestartMinDelay doubled per consecutive
// failure, plus up to a quarter of upward jitter, never above
// runnerRestartMaxDelay.
func runnerRestartDelay(failures int, jitter func(time.Duration) time.Duration) time.Duration {
	delay := runnerRestartMinDelay
	for i := 1; i < failures && delay < runnerRestartMaxDelay; i++ {
		delay *= 2
	}
	if delay >= runnerRestartMaxDelay {
		return runnerRestartMaxDelay
	}
	limit := min(delay/4, runnerRestartMaxDelay-delay)
	offset := jitter(limit)
	if offset < 0 || offset > limit {
		return delay
	}
	return delay + offset
}

func (s *publisherSet) markRestarted(name string) {
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.restarted == nil {
		s.restarted = make(map[string]time.Time)
	}
	s.restarted[name] = now
}

func (s *publisherSet) reportFatal(name string, err error) {
	s.log().Error(
		fmt.Sprintf("The %s runner stopped and Main is exiting: %s", name, depfailure.Describe(err)),
		"runner", name,
		"err", err,
	)
}

func (s *publisherSet) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *publisherSet) jitterSource() func(time.Duration) time.Duration {
	if s.jitter != nil {
		return s.jitter
	}
	return randomRestartJitter
}

// randomRestartJitter is a uniform offset in [0, maximum].
func randomRestartJitter(maximum time.Duration) time.Duration {
	if maximum <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(maximum) + 1))
}

func (s *publisherSet) waitFor(ctx context.Context, delay time.Duration) error {
	if s.wait != nil {
		return s.wait(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *publisherSet) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.New(slog.DiscardHandler)
}

var _ publisherRunner = (*publisherSet)(nil)
