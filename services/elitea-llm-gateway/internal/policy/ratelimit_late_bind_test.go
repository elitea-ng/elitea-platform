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
	if l.Status() != RateStatusUnbound || l.Enabled() {
		t.Fatalf("status = %s, want unbound", l.Status())
	}
	for i := 0; i < 3; i++ {
		if !l.Admit(context.Background(), def, sub, true).Allowed {
			t.Fatal("unbound limiter refused a request")
		}
	}
	c := &togglingCounter{}
	l.Bind(c)
	if l.Status() != RateStatusEnforced {
		t.Fatalf("status = %s, want enforced", l.Status())
	}
	if !l.Admit(context.Background(), def, sub, true).Allowed {
		t.Fatal("first request after bind refused")
	}
	if l.Admit(context.Background(), def, sub, true).Allowed {
		t.Fatal("second request after bind admitted; limit not enforced")
	}
}

// Request-path failures fail open and are counted, but only Probe moves the
// state.
func TestLimiter_StatusFollowsProbeNotRequests(t *testing.T) {
	c := &togglingCounter{}
	l := lateLimiter()
	l.Bind(c)
	def := RateLimitDef{Name: "r", RequestsPerMin: 1}
	sub := Subject{ProjectID: 1}
	c.setDown(true)
	if !l.Admit(context.Background(), def, sub, true).Allowed {
		t.Fatal("must fail open while the counter is down")
	}
	if l.Status() != RateStatusEnforced || l.Degraded() != 1 {
		t.Fatalf("status=%s degraded=%d: request must count but not flip state", l.Status(), l.Degraded())
	}
	l.Probe(context.Background())
	if l.Status() != RateStatusDegraded {
		t.Fatalf("status = %s after failed probe", l.Status())
	}
	c.setDown(false)
	l.Probe(context.Background())
	if l.Status() != RateStatusEnforced {
		t.Fatalf("status = %s after recovery", l.Status())
	}
}

func TestLimiter_NilSafeStatus(t *testing.T) {
	var l *Limiter
	if l.Status() != RateStatusUnbound {
		t.Fatal("nil limiter must report disabled")
	}
	l.Bind(&togglingCounter{}) // must not panic
	l.Probe(context.Background())
}
