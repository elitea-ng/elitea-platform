package runtimecomposition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func recoveryModeError() error {
	return fmt.Errorf("begin transaction: %w", &pgconn.PgError{Severity: "FATAL", Code: "57P03", Message: "the database system is in recovery mode"})
}

// scriptedRunner returns the scripted errors one per Run, then blocks until
// its context ends. It records how many copies ever ran at once.
type scriptedRunner struct {
	mu       sync.Mutex
	script   []error
	runs     int
	inFlight atomic.Int32
	maxSeen  atomic.Int32
	during   func() // optional, called inside every Run before it returns
}

func (r *scriptedRunner) Run(ctx context.Context) error {
	now := r.inFlight.Add(1)
	defer r.inFlight.Add(-1)
	for {
		seen := r.maxSeen.Load()
		if now <= seen || r.maxSeen.CompareAndSwap(seen, now) {
			break
		}
	}
	r.mu.Lock()
	r.runs++
	var next error
	if len(r.script) > 0 {
		next, r.script = r.script[0], r.script[1:]
	}
	r.mu.Unlock()
	if r.during != nil {
		r.during()
	}
	if next != nil {
		return next
	}
	<-ctx.Done()
	return ctx.Err()
}

func (r *scriptedRunner) Runs() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs
}

// fakeClock advances only when the set waits or a runner moves it.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type logRecords struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logRecords) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logRecords) entries(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, entry)
	}
	return out
}

// supervisedSet builds a set whose waits are instant, recorded and advance
// the fake clock by the delay.
func supervisedSet(t *testing.T, clock *fakeClock, records *logRecords, runners ...publisherRunner) (*publisherSet, *[]time.Duration) {
	t.Helper()
	set, err := newPublisherSet(runners...)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	delays := []time.Duration{}
	set.withLogger(slog.New(slog.NewJSONHandler(records, nil)))
	set.now = clock.Now
	set.jitter = func(time.Duration) time.Duration { return 0 }
	set.wait = func(ctx context.Context, delay time.Duration) error {
		mu.Lock()
		delays = append(delays, delay)
		mu.Unlock()
		clock.Advance(delay)
		return ctx.Err()
	}
	return set, &delays
}

func runUntil(t *testing.T, set *publisherSet, ready func() bool) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- set.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the publisher set did not reach the expected state")
		}
		time.Sleep(time.Millisecond)
	}
	return cancel, done
}

func TestPublisherSetRestartsARunnerAfterADependencyFailureAndKeepsItsSiblings(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	records := &logRecords{}
	failing := &scriptedRunner{script: []error{recoveryModeError(), recoveryModeError()}}
	sibling := &scriptedRunner{}
	set, delays := supervisedSet(t, clock, records, named("toolkit discovery housekeeping", failing), named("agent outbox publisher", sibling))

	cancel, done := runUntil(t, set, func() bool { return failing.Runs() == 3 && sibling.Runs() == 1 })
	if got := set.DegradedRunners(); len(got) != 1 || got[0] != "toolkit discovery housekeeping" {
		t.Fatalf("DegradedRunners = %v, want the restarted runner only", got)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want the owner's cancellation", err)
	}
	if sibling.Runs() != 1 {
		t.Fatalf("sibling ran %d times, want 1: a restart must not cancel siblings", sibling.Runs())
	}
	if got := *delays; len(got) != 2 || got[0] != time.Second || got[1] != 2*time.Second {
		t.Fatalf("restart delays = %v, want [1s 2s]", got)
	}

	var restarts int
	for _, entry := range records.entries(t) {
		if entry["level"] != "WARN" {
			continue
		}
		restarts++
		msg, _ := entry["msg"].(string)
		if !strings.Contains(msg, "Main restarts the toolkit discovery housekeeping runner after a dependency failure") {
			t.Fatalf("restart line %q does not say what happened", msg)
		}
		if entry["runner"] != "toolkit discovery housekeeping" || entry["cause"] != "the database system is in recovery mode (SQLSTATE 57P03)" {
			t.Fatalf("restart line fields = %v", entry)
		}
	}
	if restarts != 2 {
		t.Fatalf("restart lines = %d, want one per restart (2)", restarts)
	}

	clock.Advance(runnerCleanRunDuration)
	if got := set.DegradedRunners(); len(got) != 0 {
		t.Fatalf("DegradedRunners after a clean minute = %v, want none", got)
	}
}

func TestPublisherSetNeverRunsTwoCopiesOfARunner(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	script := make([]error, 50)
	for i := range script {
		script[i] = recoveryModeError()
	}
	runner := &scriptedRunner{script: script, during: func() { time.Sleep(50 * time.Microsecond) }}
	set, _ := supervisedSet(t, clock, &logRecords{}, named("flapping", runner))
	cancel, done := runUntil(t, set, func() bool { return runner.Runs() == 51 })
	cancel()
	<-done
	if got := runner.maxSeen.Load(); got != 1 {
		t.Fatalf("at most %d copies ran at once, want 1", got)
	}
}

func TestRunnerFailingRightAfterEachRestartBacksOffToTheCapAndResetsOnlyAfterACleanRun(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	script := make([]error, 8)
	for i := range script {
		script[i] = recoveryModeError()
	}
	runner := &scriptedRunner{script: script}
	set, delays := supervisedSet(t, clock, &logRecords{}, named("flapping", runner))
	cancel, done := runUntil(t, set, func() bool { return runner.Runs() == 9 })
	cancel()
	<-done
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	if fmt.Sprint(*delays) != fmt.Sprint(want) {
		t.Fatalf("restart delays = %v, want %v (growing to the cap, never back to 1s)", *delays, want)
	}

	// A runner that ran a clean minute before failing starts again at 1s.
	clock2 := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	cleanThenFail := &scriptedRunner{script: []error{recoveryModeError(), recoveryModeError(), recoveryModeError()}}
	set2, delays2 := supervisedSet(t, clock2, &logRecords{}, named("recovering", cleanThenFail))
	runs := 0
	cleanThenFail.during = func() {
		runs++
		if runs == 3 {
			clock2.Advance(runnerCleanRunDuration)
		}
	}
	cancel2, done2 := runUntil(t, set2, func() bool { return cleanThenFail.Runs() == 4 })
	cancel2()
	<-done2
	want2 := []time.Duration{time.Second, 2 * time.Second, time.Second}
	if fmt.Sprint(*delays2) != fmt.Sprint(want2) {
		t.Fatalf("restart delays = %v, want %v (reset after a clean minute)", *delays2, want2)
	}
}

func TestRunnerRestartDelayStaysWithinItsBounds(t *testing.T) {
	maxJitter := func(limit time.Duration) time.Duration { return limit }
	for failures := 1; failures <= 64; failures++ {
		got := runnerRestartDelay(failures, maxJitter)
		if got < runnerRestartMinDelay || got > runnerRestartMaxDelay {
			t.Fatalf("delay(%d) = %s, outside [%s, %s]", failures, got, runnerRestartMinDelay, runnerRestartMaxDelay)
		}
	}
	if got := runnerRestartDelay(1, maxJitter); got > runnerRestartMinDelay+runnerRestartMinDelay/4 {
		t.Fatalf("first delay = %s, want about %s", got, runnerRestartMinDelay)
	}
	if got := runnerRestartDelay(1000, maxJitter); got != runnerRestartMaxDelay {
		t.Fatalf("delay after many failures = %s, want the cap %s", got, runnerRestartMaxDelay)
	}
	for _, broken := range []time.Duration{-time.Second, time.Hour} {
		if got := runnerRestartDelay(3, func(time.Duration) time.Duration { return broken }); got != 4*time.Second {
			t.Fatalf("delay with an out-of-contract jitter %s = %s, want the unjittered 4s", broken, got)
		}
	}
}

func TestPublisherSetStopsDuringARestartWaitWhenItsOwnerEnds(t *testing.T) {
	runner := &scriptedRunner{script: []error{recoveryModeError()}}
	set, err := newPublisherSet(named("waiting", runner))
	if err != nil {
		t.Fatal(err)
	}
	set.jitter = func(time.Duration) time.Duration { return 0 }
	set.wait = nil // the real timer: a 1 s wait
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- set.Run(ctx) }()
	for runner.Runs() != 1 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want the owner's cancellation", err)
		}
		if waited := time.Since(started); waited > 500*time.Millisecond {
			t.Fatalf("shutdown waited %s for the restart delay", waited)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the owner's cancellation did not end the restart wait")
	}
	if runner.Runs() != 1 {
		t.Fatalf("runner restarted after its owner ended (%d runs)", runner.Runs())
	}
}

func TestPublisherSetNamesTheRunnerAndTheCauseWhenARunnerStopsForGood(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	broken := errors.New("runner invariant broken")
	cases := []struct {
		name      string
		err       error
		wantError string
	}{
		{"non-transient error", fmt.Errorf("sweep: %w", broken), `runtime runner "toolkit discovery housekeeping" stopped: sweep: runner invariant broken`},
		{"closed pool is not waited out", fmt.Errorf("acquire: %w", errors.New("closed pool")), `runtime runner "toolkit discovery housekeeping" stopped: acquire: closed pool`},
		{"nil while Main runs", nil, `runtime runner "toolkit discovery housekeeping" stopped unexpectedly`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records := &logRecords{}
			stopping := publisherRunnerFunc(func(context.Context) error { return tc.err })
			sibling := &scriptedRunner{}
			set, delays := supervisedSet(t, clock, records, named("toolkit discovery housekeeping", stopping), named("agent outbox publisher", sibling))
			err := set.Run(context.Background())
			if err == nil || err.Error() != tc.wantError {
				t.Fatalf("Run = %v, want %q", err, tc.wantError)
			}
			if strings.Contains(err.Error(), "owning cancellation") {
				t.Fatalf("Run = %v hides the cause behind a cancellation", err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("Run = %v does not wrap the cause", err)
			}
			if len(*delays) != 0 {
				t.Fatalf("a permanent stop was restarted (%v)", *delays)
			}
			var fatal int
			for _, entry := range records.entries(t) {
				if entry["level"] == "ERROR" && entry["runner"] == "toolkit discovery housekeeping" {
					fatal++
					if msg, _ := entry["msg"].(string); !strings.HasPrefix(msg, "The toolkit discovery housekeeping runner stopped and Main is exiting: ") {
						t.Fatalf("fatal line %q", msg)
					}
				}
			}
			if fatal != 1 {
				t.Fatalf("fatal lines = %d, want 1", fatal)
			}
		})
	}
}

// The field failure (FU-275): a runner's own 30 s timeout used to be read as
// an owner cancellation and ended Main as "stopped without an owning
// cancellation". It is now a dependency failure the runner is restarted from.
func TestARunnersOwnTimeoutIsADependencyFailureNotACancellation(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	records := &logRecords{}
	runner := &scriptedRunner{script: []error{fmt.Errorf("sweep toolkit discovery artifacts: %w", context.DeadlineExceeded)}}
	set, delays := supervisedSet(t, clock, records, named("toolkit discovery housekeeping", runner))
	cancel, done := runUntil(t, set, func() bool { return runner.Runs() == 2 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want the owner's cancellation after a restart", err)
	}
	if len(*delays) != 1 {
		t.Fatalf("restarts = %d, want 1", len(*delays))
	}
	entries := records.entries(t)
	if len(entries) != 1 || entries[0]["cause"] != "no answer in time" {
		t.Fatalf("log = %v, want one restart line with cause %q", entries, "no answer in time")
	}
}

type publisherRunnerFunc func(context.Context) error

func (f publisherRunnerFunc) Run(ctx context.Context) error { return f(ctx) }

func TestPublisherSetFlattensNestedSetsAndNamesUnnamedRunners(t *testing.T) {
	inner, err := newPublisherSet(named("validation outbox publisher", &scriptedRunner{}))
	if err != nil {
		t.Fatal(err)
	}
	outer, err := newPublisherSet(inner, named("toolkit discovery housekeeping", &scriptedRunner{}), &scriptedRunner{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, runner := range outer.publishers {
		names = append(names, runner.name)
	}
	want := []string{"validation outbox publisher", "toolkit discovery housekeeping", "*runtimecomposition.scriptedRunner"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("runner names = %v, want %v", names, want)
	}
}
