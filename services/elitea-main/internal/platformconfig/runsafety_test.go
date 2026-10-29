package platformconfig

import (
	"context"
	"errors"
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
