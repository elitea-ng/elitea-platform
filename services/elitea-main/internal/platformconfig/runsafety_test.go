package platformconfig

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionPolicyFromValues(t *testing.T) {
	t.Parallel()
	if got := SessionPolicyFromValues(Values{}); got != (SessionPolicy{}) {
		t.Fatalf("never saved = %+v, want no maximum", got)
	}
	got := SessionPolicyFromValues(Values{
		KeySessionMaxIdleTimeoutSeconds:      float64(1800),
		KeySessionMaxAbsoluteLifetimeSeconds: float64(0),
	})
	if got.MaxIdleTimeout != 30*time.Minute || got.MaxAbsoluteLifetime != 0 {
		t.Fatalf("saved = %+v", got)
	}
	// A hand-written row out of range or of the wrong type tightens and
	// never removes a bound.
	got = SessionPolicyFromValues(Values{
		KeySessionMaxIdleTimeoutSeconds:      float64(5),
		KeySessionMaxAbsoluteLifetimeSeconds: "a week",
	})
	if got.MaxIdleTimeout != time.Minute || got.MaxAbsoluteLifetime != time.Minute {
		t.Fatalf("malformed = %+v, want the smallest maximum", got)
	}
	got = SessionPolicyFromValues(Values{KeySessionMaxAbsoluteLifetimeSeconds: float64(10 * MaxSessionAbsoluteLifetimeSeconds)})
	if got.MaxAbsoluteLifetime != MaxSessionAbsoluteLifetimeSeconds*time.Second {
		t.Fatalf("too large = %+v, want the largest maximum", got)
	}
}

func TestRunCapsFromValues(t *testing.T) {
	t.Parallel()
	if got := RunCapsFromValues(Values{}); got.MaxWall != 0 {
		t.Fatalf("never saved = %s, want off", got.MaxWall)
	}
	if got := RunCapsFromValues(Values{KeyRunMaxWallSeconds: float64(3600)}); got.MaxWall != time.Hour {
		t.Fatalf("saved = %s, want 1h", got.MaxWall)
	}
}

func TestValidBoundSeconds(t *testing.T) {
	t.Parallel()
	for raw, want := range map[any]bool{
		float64(0): true, float64(60): true, float64(3600): true,
		float64(59): false, float64(-1): false, float64(1.5): false, float64(3601): false,
		"60": false,
	} {
		if got := ValidBoundSeconds(raw, 60, 3600); got != want {
			t.Errorf("ValidBoundSeconds(%v) = %t, want %t", raw, got, want)
		}
	}
}

func TestCachedSection(t *testing.T) {
	t.Parallel()
	calls := 0
	fail := false
	cache := NewCachedSection(func(context.Context) (int, error) {
		calls++
		if fail {
			return 0, errors.New("down")
		}
		return calls, nil
	}, time.Minute)
	now := time.Date(2026, time.October, 29, 0, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }

	if v, _ := cache.Get(context.Background()); v != 1 {
		t.Fatalf("first read = %d", v)
	}
	if v, _ := cache.Get(context.Background()); v != 1 || calls != 1 {
		t.Fatalf("inside the TTL read again: v=%d calls=%d", v, calls)
	}
	cache.Invalidate()
	if v, _ := cache.Get(context.Background()); v != 2 {
		t.Fatalf("after Invalidate = %d, want a new read", v)
	}
	fail = true
	now = now.Add(2 * time.Minute)
	if v, err := cache.Get(context.Background()); err != nil || v != 2 {
		t.Fatalf("a failed refresh = (%d, %v), want the last good value", v, err)
	}

	cold := NewCachedSection(func(context.Context) (int, error) { return 0, errors.New("down") }, 0)
	if _, err := cold.Get(context.Background()); err == nil {
		t.Fatal("a cold cache with a failed read answered no error")
	}
}

// TestCachedSectionOneReadInFlight: with no value yet, concurrent callers
// share ONE read and wait for it; with a stale value, a slow read does not
// hold anybody up — the others get the stale value at once.
func TestCachedSectionOneReadInFlight(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 16)
	cache := NewCachedSection(func(context.Context) (int, error) {
		n := calls.Add(1)
		started <- struct{}{}
		<-release
		return int(n), nil
	}, time.Minute)
	var mu sync.Mutex
	now := time.Date(2026, time.October, 29, 0, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }

	const callers = 8
	results := make(chan int, callers)
	for range callers {
		go func() {
			v, err := cache.Get(context.Background())
			if err != nil {
				t.Errorf("Get: %v", err)
			}
			results <- v
		}()
	}
	<-started
	time.Sleep(20 * time.Millisecond) // let the others join the read in flight
	close(release)
	for range callers {
		if v := <-results; v != 1 {
			t.Fatalf("cold Get = %d, want the one shared read", v)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("cold reads = %d, want 1", got)
	}

	// Stale: the read blocks, yet every other caller answers at once.
	release = make(chan struct{})
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	refreshed := make(chan int, 1)
	go func() {
		v, _ := cache.Get(context.Background())
		refreshed <- v
	}()
	<-started
	for range callers {
		done := make(chan int, 1)
		go func() { v, _ := cache.Get(context.Background()); done <- v }()
		select {
		case v := <-done:
			if v != 1 {
				t.Fatalf("stale Get during a refresh = %d, want the stale 1", v)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a caller with a stale value waited for the read in flight")
		}
	}
	close(release)
	if v := <-refreshed; v != 2 {
		t.Fatalf("refreshing caller = %d, want 2", v)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("reads = %d, want 2", got)
	}
}

// TestCachedSectionFailureBacksOff: a failing read is retried once per
// backoff, not once per call, and the last good value is served meanwhile.
func TestCachedSectionFailureBacksOff(t *testing.T) {
	t.Parallel()
	calls := 0
	fail := false
	cache := NewCachedSection(func(context.Context) (int, error) {
		calls++
		if fail {
			return 0, errors.New("down")
		}
		return 7, nil
	}, time.Minute)
	now := time.Date(2026, time.October, 29, 0, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }
	if v, err := cache.Get(context.Background()); err != nil || v != 7 {
		t.Fatalf("first read = (%d, %v)", v, err)
	}

	fail = true
	now = now.Add(2 * time.Minute)
	for range 50 {
		if v, err := cache.Get(context.Background()); err != nil || v != 7 {
			t.Fatalf("Get while failing = (%d, %v), want the last good 7", v, err)
		}
	}
	if calls != 2 {
		t.Fatalf("reads after a failure = %d, want 2 (one retry per backoff)", calls)
	}
	now = now.Add(sectionRefreshBackoff)
	_, _ = cache.Get(context.Background())
	if calls != 3 {
		t.Fatalf("reads once the backoff passed = %d, want 3", calls)
	}
	// The second failure doubles the wait.
	now = now.Add(sectionRefreshBackoff)
	_, _ = cache.Get(context.Background())
	if calls != 3 {
		t.Fatalf("reads inside the doubled backoff = %d, want 3", calls)
	}
	now = now.Add(sectionRefreshBackoff)
	_, _ = cache.Get(context.Background())
	if calls != 4 {
		t.Fatalf("reads after the doubled backoff = %d, want 4", calls)
	}

	// A save on this replica looks again at once, backoff or not.
	fail = false
	cache.Invalidate()
	if v, err := cache.Get(context.Background()); err != nil || v != 7 || calls != 5 {
		t.Fatalf("after Invalidate = (%d, %v) reads=%d", v, err, calls)
	}
}

// TestCachedSectionNeverLoadedFailsClosed: with no good read ever, every
// call answers an error (also inside the backoff), never a zero value.
func TestCachedSectionNeverLoadedFailsClosed(t *testing.T) {
	t.Parallel()
	calls := 0
	broken := errors.New("down")
	cache := NewCachedSection(func(context.Context) (int, error) {
		calls++
		return 0, broken
	}, time.Minute)
	now := time.Date(2026, time.October, 29, 0, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }
	for range 10 {
		if _, err := cache.Get(context.Background()); !errors.Is(err, broken) {
			t.Fatalf("cold Get = %v, want the read error", err)
		}
	}
	if calls != 1 {
		t.Fatalf("cold reads = %d, want 1 inside the backoff", calls)
	}
	// A cancelled caller with no value is refused too.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	now = now.Add(time.Hour)
	if _, err := cache.Get(cancelled); err == nil {
		t.Fatal("a cancelled cold Get answered no error")
	}
}
