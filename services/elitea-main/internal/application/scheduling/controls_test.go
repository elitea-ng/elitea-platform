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
