package repos

// The stall canary of the handoff publish load gate (#2940), and the rule
// that decides which measured window the gate may judge. No build tag: the
// decision logic is unit-tested here by ci-go; the load gate itself
// (workspace_store_loadgate_postgres_integration_test.go, tag `loadgate`)
// uses it.
//
// A p99 latency gate measured in a pod without guaranteed CPU reads a stall
// of the runner (CPU contention, CFS throttling on a shared node) as a
// regression: one window on the shared pool read 954 ms where the same case
// reads 2-4 ms on every other pod, and the slow publishes started before the
// contention the case exercises. The canary makes such a window visible and
// the gate re-measures it; it never lets a window pass that it did not
// measure cleanly:
//
//   - a window whose canary saw a stall above loadGateStallThreshold is
//     INVALID: it is logged and measured again, in process, up to
//     loadGateAttempts times; unless its p99 minus the stall still breaches
//     the gate, which no stall explains, and fails at once;
//   - the FIRST clean window decides the case: a breach there fails as a
//     regression and is never re-measured (a retry must not become a second
//     chance for a slow publish path);
//   - when no attempt gave a judged window the case fails too, with
//     "measurement invalid: runner stalled N in every attempt", a failure
//     distinct from a regression, never a pass.

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	// loadGateCanaryInterval is the canary's tick: it wakes every 1 ms and
	// records how late each wake-up came.
	loadGateCanaryInterval = time.Millisecond
	// loadGateStallThreshold is the largest stall a valid window may show:
	// half the 50 ms p99 gate, so a stall the gate tolerates cannot by itself
	// carry a healthy publish (2-4 ms on the pool) over the gate. Healthy
	// windows show a canary lag well under a millisecond or two.
	loadGateStallThreshold = 25 * time.Millisecond
	// loadGateAttempts bounds the re-measurement of a stalled window.
	loadGateAttempts = 3
)

// stallCanary is a goroutine that ticks every interval during a measured
// window and records the largest lag of a tick: the time the process (its
// scheduler, or the whole pod's cgroup) could not run.
type stallCanary struct {
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
	maxLag   time.Duration
	// maxAt is when the largest lag began, after the canary started: a
	// stall in the middle of a window tells apart from the start of one.
	maxAt time.Duration
}

func startStallCanary(interval time.Duration) *stallCanary {
	canary := &stallCanary{interval: interval, stop: make(chan struct{}), done: make(chan struct{})}
	ready := make(chan struct{})
	go func() {
		defer close(canary.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		began := time.Now()
		last := began
		close(ready)
		for {
			select {
			case <-canary.stop:
				canary.observe(time.Since(last), last.Sub(began))
				return
			case <-ticker.C:
				// The receive time, not the tick's scheduled time: a ticker
				// drops the ticks it could not deliver, so the gap between two
				// wake-ups is the stall.
				now := time.Now()
				canary.observe(now.Sub(last), last.Sub(began))
				last = now
			}
		}
	}()
	<-ready
	return canary
}

// observe records a gap between two wake-ups that began at `at`.
func (c *stallCanary) observe(gap, at time.Duration) {
	if lag := gap - c.interval; lag > c.maxLag {
		c.maxLag, c.maxAt = lag, at
	}
}

// Stop ends the window and returns the largest lag the canary saw and when
// it began.
func (c *stallCanary) Stop() (lag, at time.Duration) {
	close(c.stop)
	<-c.done
	return c.maxLag, c.maxAt
}

// loadGateStall is what the canaries saw in one window: the in-process
// scheduler lag, and the largest gap between two pg_stat_activity samples
// beyond their 2 ms sleep (the database server, a separate process in the
// same pod, stalling).
type loadGateStall struct {
	scheduler time.Duration
	// schedulerAt is when the largest scheduler lag began in the window.
	schedulerAt time.Duration
	sampler     time.Duration
}

// worst is the stall that decides the window's validity.
func (s loadGateStall) worst() time.Duration {
	return max(s.scheduler, s.sampler)
}

func (s loadGateStall) String() string {
	return fmt.Sprintf("scheduler-lag=%s (at %s) sampler-gap=%s", s.scheduler.Round(10*time.Microsecond),
		s.schedulerAt.Round(time.Millisecond), s.sampler.Round(10*time.Microsecond))
}

// loadGateReading is what one window measured: the gated p99 and the stall.
type loadGateReading struct {
	p99   time.Duration
	stall loadGateStall
}

// explains says whether the window's stall can account for a breach of the
// gate. A stall delays a publish by at most its length (a publish takes a few
// milliseconds, so it spans one stall), so p99 minus the stall still at or
// over the gate is slow code, stalled runner or not.
func (r loadGateReading) explains(gate time.Duration) bool {
	return r.p99-r.stall.worst() < gate
}

func (r loadGateReading) String() string {
	return fmt.Sprintf("p99=%s %s", r.p99.Round(10*time.Microsecond), r.stall)
}

// loadGateStalledError is the "no clean window" failure: distinct from a
// regression, and never a pass.
type loadGateStalledError struct {
	threshold time.Duration
	readings  []loadGateReading
}

func (e *loadGateStalledError) Error() string {
	least := e.readings[0].stall.worst()
	parts := make([]string, 0, len(e.readings))
	for index, reading := range e.readings {
		least = min(least, reading.stall.worst())
		parts = append(parts, fmt.Sprintf("attempt %d: %s", index+1, reading))
	}
	return fmt.Sprintf("measurement invalid: runner stalled %s in every attempt (%d attempts, threshold %s; %s)",
		least.Round(time.Millisecond), len(e.readings), e.threshold, strings.Join(parts, "; "))
}

// measureStallFree measures up to attempts windows and returns the one the
// gate judges, its reading and its 1-based attempt number:
//
//   - a window whose stall is within threshold is clean and judged;
//   - a stalled window whose breach the stall cannot explain (p99 minus the
//     stall still at or over the gate) is judged too, and fails: a regression
//     that also stalls the process is not re-rolled until a quiet window;
//   - any other stalled window is invalid and measured again.
//
// No judged window in any attempt returns a *loadGateStalledError. Every
// window's reading is logged. measure runs one complete window (its own
// fixture).
func measureStallFree[R any](logf func(string, ...any), name string, attempts int, threshold, gate time.Duration,
	measure func(attempt int) (R, loadGateReading)) (R, loadGateReading, int, error) {
	readings := make([]loadGateReading, 0, attempts)
	for attempt := 1; attempt <= attempts; attempt++ {
		result, reading := measure(attempt)
		switch {
		case reading.stall.worst() <= threshold:
			logf("%s: stall canary attempt %d/%d: %s: clean window, gated", name, attempt, attempts, reading)
			return result, reading, attempt, nil
		case !reading.explains(gate):
			logf("%s: stall canary attempt %d/%d: %s: above %s, but the stall cannot explain the p99; gated",
				name, attempt, attempts, reading, threshold)
			return result, reading, attempt, nil
		}
		logf("%s: stall canary attempt %d/%d: %s: above %s, window invalid", name, attempt, attempts, reading, threshold)
		readings = append(readings, reading)
	}
	var zero R
	return zero, loadGateReading{}, 0, &loadGateStalledError{threshold: threshold, readings: readings}
}

// checkP99 is the gate on a judged window.
func checkP99(label string, reading loadGateReading, gate time.Duration) error {
	if reading.p99 >= gate {
		return fmt.Errorf("%s p99 = %s, gate is under %s (a judged window: %s)", label, reading.p99, gate, reading.stall)
	}
	return nil
}

const testGate = 50 * time.Millisecond

func reading(p99 time.Duration, stall loadGateStall) loadGateReading {
	return loadGateReading{p99: p99, stall: stall}
}

// A stalled window is measured again, even when its p99 passed the gate;
// the first clean window is the one judged.
func TestLoadGateStallReMeasuresAStalledWindow(t *testing.T) {
	calls := 0
	_, judged, attempt, err := measureStallFree(t.Logf, "case", loadGateAttempts, loadGateStallThreshold, testGate,
		func(int) (int, loadGateReading) {
			calls++
			if calls == 1 {
				return calls, reading(2*time.Millisecond, loadGateStall{scheduler: 900 * time.Millisecond})
			}
			return calls, reading(3*time.Millisecond, loadGateStall{scheduler: time.Millisecond, sampler: 2 * time.Millisecond})
		})
	if err != nil || calls != 2 || attempt != 2 || judged.p99 != 3*time.Millisecond {
		t.Fatalf("judged=%s attempt=%d calls=%d err=%v; want the second window (3ms, attempt 2), no error",
			judged, attempt, calls, err)
	}
	if err := checkP99("case", judged, testGate); err != nil {
		t.Fatal(err)
	}
	// The issue's window (#2940): p99 954 ms behind a ~1 s stall is invalid,
	// and so is a database-side stall (the sampler's gap).
	for _, stall := range []loadGateReading{
		reading(954*time.Millisecond, loadGateStall{scheduler: time.Second}),
		reading(time.Millisecond, loadGateStall{sampler: 400 * time.Millisecond}),
	} {
		calls = 0
		_, _, attempt, err = measureStallFree(t.Logf, "case", loadGateAttempts, loadGateStallThreshold, testGate,
			func(int) (int, loadGateReading) {
				calls++
				if calls == 1 {
					return calls, stall
				}
				return calls, reading(2*time.Millisecond, loadGateStall{})
			})
		if err != nil || attempt != 2 {
			t.Fatalf("%s: attempt=%d err=%v; want it re-measured", stall, attempt, err)
		}
	}
}

// A breach in a clean window fails as a regression and is never measured
// again: the retry is no second chance for a slow publish path.
func TestLoadGateStallCleanBreachFails(t *testing.T) {
	calls := 0
	_, judged, attempt, err := measureStallFree(t.Logf, "case", loadGateAttempts, loadGateStallThreshold, testGate,
		func(int) (int, loadGateReading) {
			calls++
			// At the threshold is still clean.
			return calls, reading(60*time.Millisecond, loadGateStall{scheduler: loadGateStallThreshold})
		})
	if err != nil || calls != 1 || attempt != 1 {
		t.Fatalf("attempt=%d calls=%d err=%v; want the first (clean) window judged, measured once", attempt, calls, err)
	}
	if gateErr := checkP99("other threads'", judged, testGate); gateErr == nil || !strings.Contains(gateErr.Error(), "p99 = 60ms") {
		t.Fatalf("checkP99 = %v; want a regression failure", gateErr)
	}
	if checkP99("case", reading(49*time.Millisecond, loadGateStall{}), testGate) != nil {
		t.Fatal("49ms under a 50ms gate must pass")
	}
	if checkP99("case", reading(testGate, loadGateStall{}), testGate) == nil {
		t.Fatal("50ms is not under a 50ms gate")
	}
}

// A stalled window whose breach the stall cannot explain is judged at once:
// a regression that also stalls the process must not be re-rolled until a
// quiet window passes.
func TestLoadGateStallUnexplainedBreachIsJudged(t *testing.T) {
	calls := 0
	_, judged, attempt, err := measureStallFree(t.Logf, "case", loadGateAttempts, loadGateStallThreshold, testGate,
		func(int) (int, loadGateReading) {
			calls++
			if calls == 1 {
				// 100 ms p99 behind a 30 ms stall: 70 ms of it is the code.
				return calls, reading(100*time.Millisecond, loadGateStall{scheduler: 30 * time.Millisecond})
			}
			return calls, reading(40*time.Millisecond, loadGateStall{})
		})
	if err != nil || calls != 1 || attempt != 1 {
		t.Fatalf("attempt=%d calls=%d err=%v; want the stalled breach judged, measured once", attempt, calls, err)
	}
	if checkP99("case", judged, testGate) == nil {
		t.Fatal("a 100ms p99 behind a 30ms stall must fail the gate")
	}
}

// No judged window in any attempt is a distinct failure, never a pass, and
// names the smallest stall seen (the runner stalled at least that much in
// every attempt) and every attempt's p99.
func TestLoadGateStallEveryAttemptStalledIsInvalid(t *testing.T) {
	calls := 0
	_, _, attempt, err := measureStallFree(t.Logf, "case", loadGateAttempts, loadGateStallThreshold, testGate,
		func(int) (int, loadGateReading) {
			calls++
			return calls, reading(time.Millisecond, loadGateStall{scheduler: time.Duration(calls) * 100 * time.Millisecond})
		})
	if calls != loadGateAttempts || attempt != 0 {
		t.Fatalf("measured %d times, attempt %d; want %d measurements and no judged attempt", calls, attempt, loadGateAttempts)
	}
	var stalled *loadGateStalledError
	if !errors.As(err, &stalled) {
		t.Fatalf("err = %v; want a loadGateStalledError", err)
	}
	want := "measurement invalid: runner stalled 100ms in every attempt (3 attempts"
	if !strings.HasPrefix(err.Error(), want) || !strings.Contains(err.Error(), "attempt 3: p99=1ms") {
		t.Fatalf("err = %q; want prefix %q and every attempt's p99", err, want)
	}
}

// The canary turns a late wake-up into lag, and Stop right after start
// reports none.
func TestLoadGateStallCanaryMeasuresLag(t *testing.T) {
	if lag, _ := startStallCanary(time.Hour).Stop(); lag > 0 {
		t.Fatalf("lag = %s before the first tick; want none", lag)
	}
	canary := &stallCanary{interval: time.Millisecond}
	canary.observe(500*time.Microsecond, 0)
	canary.observe(41*time.Millisecond, 300*time.Millisecond)
	canary.observe(2*time.Millisecond, 400*time.Millisecond)
	if canary.maxLag != 40*time.Millisecond || canary.maxAt != 300*time.Millisecond {
		t.Fatalf("lag = %s at %s after a 41ms gap at 300ms on a 1ms tick; want 40ms at 300ms", canary.maxLag, canary.maxAt)
	}
}
