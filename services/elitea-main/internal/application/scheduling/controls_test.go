package scheduling

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeControls is an in-memory operator registry.
type fakeControls struct {
	mu         sync.Mutex
	controls   map[string]Control
	registered []string
	reports    []RoutineReport
	resolveErr error
}

func (f *fakeControls) Register(_ context.Context, jobs []Job) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, job := range jobs {
		f.registered = append(f.registered, job.ID)
	}
	return nil
}

func (f *fakeControls) Resolve(context.Context) (map[string]Control, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	out := make(map[string]Control, len(f.controls))
	for id, control := range f.controls {
		out[id] = control
	}
	return out, nil
}

func (f *fakeControls) Report(_ context.Context, report RoutineReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, report)
	return nil
}

func (f *fakeControls) set(id string, control Control) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.controls[id] = control
}

func controlledRunner(t *testing.T, defaultEvery time.Duration, controls *fakeControls) (*Runner, *memoryStore, *atomic.Int32, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore(now)
	var calls atomic.Int32
	job := Job{
		ID: "sweep.v1", Revision: "r1", Mode: ModeLocalBounded,
		Schedule: intervalSchedule(defaultEvery), Timeout: time.Second,
		Handler: HandlerFunc(func(context.Context, Occurrence) (Outcome, error) {
			calls.Add(1)
			return OutcomeLocalCompleted, nil
		}),
		Cadence: "1h", Description: "a sweep",
	}
	config := testConfig("main-a")
	registry, err := NewRegistry(config.LeaseDuration, job)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(store, registry, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	return runner.WithControls(controls), store, &calls, now
}

func tickEvery(t *testing.T, runner *Runner, from time.Time, step time.Duration, count int) {
	t.Helper()
	for i := 1; i <= count; i++ {
		if _, err := runner.Tick(context.Background(), from.Add(time.Duration(i)*step)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestControlsOverrideScheduleIsHonoured(t *testing.T) {
	controls := &fakeControls{controls: map[string]Control{}}
	runner, _, calls, now := controlledRunner(t, time.Hour, controls)

	tickEvery(t, runner, now, time.Minute, 5)
	if got := calls.Load(); got > 1 {
		t.Fatalf("calls with the hourly default = %d, want at most 1", got)
	}

	before := calls.Load()
	controls.set("sweep.v1", Control{Enabled: true, Schedule: intervalSchedule(time.Minute)})
	tickEvery(t, runner, now.Add(5*time.Minute), time.Minute, 5)
	if got := calls.Load() - before; got < 4 {
		t.Fatalf("calls with a one-minute override = %d, want at least 4", got)
	}
	if len(controls.registered) != 1 || controls.registered[0] != "sweep.v1" {
		t.Fatalf("registered = %v, want the job once", controls.registered)
	}
}

func TestControlsDisabledJobDoesNotRunAndDoesNotReplayWhenEnabled(t *testing.T) {
	controls := &fakeControls{controls: map[string]Control{"sweep.v1": {Enabled: false}}}
	runner, _, calls, now := controlledRunner(t, time.Minute, controls)

	tickEvery(t, runner, now, time.Minute, 10)
	if got := calls.Load(); got != 0 {
		t.Fatalf("a disabled job ran %d times", got)
	}

	controls.set("sweep.v1", Control{Enabled: true})
	tickEvery(t, runner, now.Add(10*time.Minute), time.Minute, 1)
	if got := calls.Load(); got > 1 {
		t.Fatalf("calls right after enabling = %d: ten missed minutes must not be replayed", got)
	}
}

func TestControlsUnreadableRegistryRunsNothing(t *testing.T) {
	controls := &fakeControls{controls: map[string]Control{}, resolveErr: errors.New("table missing")}
	runner, _, calls, now := controlledRunner(t, time.Minute, controls)

	tickEvery(t, runner, now, time.Minute, 5)
	if got := calls.Load(); got != 0 {
		t.Fatalf("calls with an unreadable registry = %d, want 0", got)
	}
}

func TestControlsReceiveRunOutcomes(t *testing.T) {
	controls := &fakeControls{controls: map[string]Control{}}
	runner, _, calls, now := controlledRunner(t, time.Minute, controls)

	tickEvery(t, runner, now, time.Minute, 3)
	if calls.Load() == 0 {
		t.Fatal("the job never ran")
	}
	var ok, next int
	for _, report := range controls.reports {
		if report.Outcome == "ok" {
			ok++
		}
		if !report.NextRunAt.IsZero() {
			next++
		}
	}
	if ok == 0 || next == 0 {
		t.Fatalf("reports = %+v, want run outcomes and next-run times", controls.reports)
	}
}

// gapSchedule fires a fixed gap after the instant it is given, like a
// routines.Schedule interval override (`30m`).
type gapSchedule time.Duration

func (g gapSchedule) Next(after time.Time) time.Time { return after.Add(time.Duration(g)) }
func (g gapSchedule) Interval() time.Duration        { return time.Duration(g) }

// neverSchedule is a cron that names no real instant (`0 0 31 2 *`).
type neverSchedule struct{}

func (neverSchedule) Next(time.Time) time.Time { return time.Time{} }

func TestControlsIntervalOverrideFiresOnAOneMinuteTick(t *testing.T) {
	controls := &fakeControls{controls: map[string]Control{
		"sweep.v1": {Enabled: true, Schedule: gapSchedule(30 * time.Minute)},
	}}
	runner, _, calls, now := controlledRunner(t, time.Hour, controls)

	tickEvery(t, runner, now, time.Minute, 90)
	if got := calls.Load(); got < 2 || got > 3 {
		t.Fatalf("calls over 90 one-minute ticks with a 30m interval = %d, want 2..3", got)
	}
}

func TestControlsAScheduleThatNeverFiresDoesNotStopOtherJobs(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore(now)
	var calls atomic.Int32
	handler := HandlerFunc(func(context.Context, Occurrence) (Outcome, error) {
		calls.Add(1)
		return OutcomeLocalCompleted, nil
	})
	config := testConfig("main-a")
	registry, err := NewRegistry(config.LeaseDuration,
		Job{ID: "a.never", Revision: "r1", Mode: ModeLocalBounded, Schedule: intervalSchedule(time.Minute),
			Timeout: time.Second, Handler: handler, Cadence: "1m"},
		Job{ID: "b.sweep", Revision: "r1", Mode: ModeLocalBounded, Schedule: intervalSchedule(time.Minute),
			Timeout: time.Second, Handler: handler, Cadence: "1m"},
	)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(store, registry, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	controls := &fakeControls{controls: map[string]Control{
		"a.never": {Enabled: true, Schedule: neverSchedule{}},
	}}
	runner.WithControls(controls)

	tickEvery(t, runner, now, time.Minute, 3)
	if got := calls.Load(); got < 2 {
		t.Fatalf("calls = %d: the other job must keep running", got)
	}
}

func TestControlsDisableSupersedesPlannedOccurrences(t *testing.T) {
	controls := &fakeControls{controls: map[string]Control{}}
	runner, store, calls, now := controlledRunner(t, time.Minute, controls)

	// An occurrence planned before the switch but not yet run (another
	// replica planned it, or it was released for retry).
	seed := seedFor(Job{ID: "sweep.v1", Revision: "r1", Mode: ModeLocalBounded}, now)
	store.occurrences[seed.InvocationID] = &memoryOccurrence{seed: seed, state: "PENDING"}

	controls.set("sweep.v1", Control{Enabled: false})
	tickEvery(t, runner, now, time.Minute, 1)
	controls.set("sweep.v1", Control{Enabled: true})
	before := calls.Load()
	tickEvery(t, runner, now.Add(time.Minute), time.Minute, 1)

	if got := store.occurrences[seed.InvocationID].state; got != "SUPERSEDED" {
		t.Fatalf("pre-switch occurrence state = %s, want SUPERSEDED", got)
	}
	if got := calls.Load() - before; got > 1 {
		t.Fatalf("calls right after re-enabling = %d: the old occurrence must not run late", got)
	}
}

func TestControlsNextRunIsWrittenOnlyWhenItChanges(t *testing.T) {
	controls := &fakeControls{controls: map[string]Control{}}
	runner, _, _, now := controlledRunner(t, time.Hour, controls)

	tickEvery(t, runner, now, time.Minute, 1)
	var first time.Time
	for _, report := range controls.reports {
		if !report.NextRunAt.IsZero() {
			first = report.NextRunAt
		}
	}
	if first.IsZero() {
		t.Fatalf("reports = %+v, want a next-run time", controls.reports)
	}
	controls.set("sweep.v1", Control{Enabled: true, NextRunAt: &first})
	controls.reports = nil
	tickEvery(t, runner, now.Add(time.Minute), time.Minute, 5)
	for _, report := range controls.reports {
		if !report.NextRunAt.IsZero() {
			t.Fatalf("next run rewritten although unchanged: %+v", report)
		}
	}
}
