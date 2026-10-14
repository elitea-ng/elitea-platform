package policy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type togglingCounter struct {
	mu   sync.Mutex
	down bool
	n    map[string]int64
}

func (c *togglingCounter) setDown(d bool) { c.mu.Lock(); c.down = d; c.mu.Unlock() }

func (c *togglingCounter) IncrRateLimit(_ context.Context, s string, d int64) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return 0, errors.New("nats down")
	}
	if c.n == nil {
		c.n = map[string]int64{}
	}
	c.n[s] += d
	return c.n[s], nil
}

func (c *togglingCounter) ReadRateLimit(_ context.Context, s string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return 0, errors.New("nats down")
	}
	return c.n[s], nil
}

func lateLimiter() *Limiter {
	return NewLimiter(LimiterConfig{
		Subject: func(k, key string, w int64) string { return k + "." + key },
		Now:     func() time.Time { return time.Unix(1_000_000, 0) },
	})
}

// A limiter built with no counter is disabled, admits everything, and starts
// enforcing the moment a counter is bound — no restart.
func TestLimiter_BindLateStartsEnforcing(t *testing.T) {
	l := lateLimiter()
	def := RateLimitDef{Name: "r", RequestsPerMin: 1}
	sub := Subject{ProjectID: 1}
	if l.Status() != RateStatusDisabled || l.Enabled() {
		t.Fatalf("status = %s, want disabled", l.Status())
	}
	for i := 0; i < 3; i++ {
		if !l.Admit(context.Background(), def, sub, true).Allowed {
			t.Fatal("unbound limiter refused a request")
		}
	}
	c := &togglingCounter{}
	l.Bind(c)
	if l.Status() != RateStatusOK {
		t.Fatalf("status = %s, want ok", l.Status())
	}
	if !l.Admit(context.Background(), def, sub, true).Allowed {
		t.Fatal("first request after bind refused")
	}
	if l.Admit(context.Background(), def, sub, true).Allowed {
		t.Fatal("second request after bind admitted; limit not enforced")
	}
}

// NATS dropping mid-run flips the status to degraded (fail open, counted) and
// the next success flips it back.
func TestLimiter_StatusFollowsCounterHealth(t *testing.T) {
	c := &togglingCounter{}
	l := lateLimiter()
	l.Bind(c)
	def := RateLimitDef{Name: "r", RequestsPerMin: 1}
	sub := Subject{ProjectID: 1}
	l.Admit(context.Background(), def, sub, true)
	c.setDown(true)
	if !l.Admit(context.Background(), def, sub, true).Allowed {
		t.Fatal("must fail open while the counter is down")
	}
	if l.Status() != RateStatusDegraded || l.Degraded() != 1 {
		t.Fatalf("status=%s degraded=%d, want degraded/1", l.Status(), l.Degraded())
	}
	c.setDown(false)
	l.Admit(context.Background(), def, sub, true)
	if l.Status() != RateStatusOK {
		t.Fatalf("status = %s after recovery, want ok", l.Status())
	}
}

func TestLimiter_NilSafeStatus(t *testing.T) {
	var l *Limiter
	if l.Status() != RateStatusDisabled {
		t.Fatal("nil limiter must report disabled")
	}
	l.Bind(&togglingCounter{}) // must not panic
}
