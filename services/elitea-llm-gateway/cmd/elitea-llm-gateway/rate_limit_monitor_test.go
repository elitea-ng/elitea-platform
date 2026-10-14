package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/governance"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/llmproxy"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/policy"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/server"
)

type fakeCounter struct {
	mu   sync.Mutex
	down bool
	n    map[string]int64
}

func (c *fakeCounter) set(down bool) { c.mu.Lock(); c.down = down; c.mu.Unlock() }
func (c *fakeCounter) IncrRateLimit(_ context.Context, s string, d int64) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return 0, errors.New("down")
	}
	if c.n == nil {
		c.n = map[string]int64{}
	}
	c.n[s] += d
	return c.n[s], nil
}
func (c *fakeCounter) ReadRateLimit(_ context.Context, s string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return 0, errors.New("down")
	}
	return c.n[s], nil
}

// ratesNATS is a NATSClient that is also a RateCounter.
type ratesNATS struct {
	server.NATSClient
	*fakeCounter
}

type fakeRedialer struct {
	up atomic.Bool
	c  *fakeCounter
}

func (r *fakeRedialer) RedialNATS(context.Context) (server.NATSClient, error) {
	if !r.up.Load() {
		return nil, errors.New("connection refused")
	}
	return ratesNATS{fakeCounter: r.c}, nil
}

func newMonLimiter() *policy.Limiter {
	return policy.NewLimiter(policy.LimiterConfig{
		Subject: func(k, key string, w int64) string { return k + "." + key },
	})
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Start without NATS -> unbound; NATS appears -> enforced with no restart; NATS
// drops with NO traffic -> the probe alone notices; it comes back -> enforced.
// Independent of any budget loop.
func TestRateLimitMonitor_LifecycleWithoutTraffic(t *testing.T) {
	c := &fakeCounter{}
	r := &fakeRedialer{c: c}
	l := newMonLimiter()
	m := rateLimitMonitor{srv: r, limiter: l, logger: quiet(), interval: time.Hour}

	if l.Status() != policy.RateStatusUnbound {
		t.Fatalf("boot = %s", l.Status())
	}
	m.step(context.Background())
	if l.Status() != policy.RateStatusUnbound {
		t.Fatalf("NATS down: %s", l.Status())
	}
	r.up.Store(true)
	m.step(context.Background())
	if l.Status() != policy.RateStatusEnforced {
		t.Fatalf("NATS up: %s", l.Status())
	}
	def := policy.RateLimitDef{Name: "r", RequestsPerMin: 1}
	l.Admit(context.Background(), def, policy.Subject{ProjectID: 1}, true)
	if l.Admit(context.Background(), def, policy.Subject{ProjectID: 1}, true).Allowed {
		t.Fatal("limit not enforced after late bind")
	}

	c.set(true)
	m.step(context.Background()) // no request in between
	if l.Status() != policy.RateStatusDegraded {
		t.Fatalf("drop undetected without traffic: %s", l.Status())
	}
	c.set(false)
	m.step(context.Background())
	if l.Status() != policy.RateStatusEnforced {
		t.Fatalf("recovery not visible: %s", l.Status())
	}
}

// A request that fails because the CALLER cancelled must not flip the state,
// and neither may a cancelled probe context.
func TestRateLimit_CallerCancelDoesNotFlipState(t *testing.T) {
	c := &fakeCounter{}
	l := newMonLimiter()
	l.Bind(c)
	c.set(true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l.Admit(ctx, policy.RateLimitDef{Name: "r", RequestsPerMin: 1}, policy.Subject{ProjectID: 1}, true)
	if l.Status() != policy.RateStatusEnforced {
		t.Fatalf("request failure changed state: %s", l.Status())
	}
	l.Probe(ctx) // cancelled prober context: ignored entirely
	if l.Status() != policy.RateStatusEnforced {
		t.Fatalf("cancelled probe changed state: %s", l.Status())
	}
	l.Probe(context.Background())
	if l.Status() != policy.RateStatusDegraded {
		t.Fatalf("real probe failure ignored: %s", l.Status())
	}
}

func TestRateLimitMonitor_StopsOnCancelNoLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m := rateLimitMonitor{srv: &fakeRedialer{c: &fakeCounter{}}, limiter: newMonLimiter(), logger: quiet(), interval: 5 * time.Millisecond}
	go func() { m.run(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("monitor did not stop")
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("goroutines %d -> %d", before, n)
	}
}

// NATS deliberately unset: reported as off, and no gauge is exported at all.
func TestRateLimitReporting_NotConfigured(t *testing.T) {
	l := newMonLimiter()
	if m := rateLimitsMetric(l, false); len(m) != 0 {
		t.Fatalf("gauge exported for a NATS-less deployment: %v", m)
	}
	rec := httptest.NewRecorder()
	makeReadyzHandler(nil, nil, rateLimitsCheck(l, false)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"rate_limits":"off (not configured)"`) {
		t.Fatalf("readyz = %d %s", rec.Code, rec.Body.String())
	}
}

func TestRateLimitReporting_Configured(t *testing.T) {
	l := newMonLimiter()
	g := rateLimitsMetric(l, true)
	if len(g) != 1 || g[0].v.String() != "0" {
		t.Fatalf("unbound gauge = %v", g)
	}
	k, v := rateLimitsCheck(l, true)()
	if k != "rate_limits" || v != "unbound" {
		t.Fatalf("check = %q %q", k, v)
	}
	l.Bind(&fakeCounter{})
	if g[0].v.String() != "1" {
		t.Fatal("gauge not 1 when enforced")
	}
	if k, _ := rateLimitsCheck(l, true)(); k != "" {
		t.Fatal("check reported while enforced")
	}
}

// Budget enforcement already installed by another path makes the budget loop
// finish immediately; the limiter must still bind, because it has its own loop.
func TestRateLimitMonitor_BindsEvenWhenBudgetInstalledElsewhere(t *testing.T) {
	handler := llmproxy.NewHandler(nil, quiet(), nil)
	if !handler.InstallBudgetEnforcement(llmproxy.BudgetEnforcement{Gate: &governance.GovernanceStore{}}) {
		t.Fatal("could not simulate an installed gate")
	}
	if !(budgetRecovery{handler: handler, logger: quiet()}).attempt(context.Background()) {
		t.Fatal("budget loop should finish when a gate is already installed")
	}
	r := &fakeRedialer{c: &fakeCounter{}}
	r.up.Store(true)
	l := newMonLimiter()
	rateLimitMonitor{srv: r, limiter: l, logger: quiet(), interval: time.Hour}.step(context.Background())
	if l.Status() != policy.RateStatusEnforced {
		t.Fatalf("limiter status = %s", l.Status())
	}
}
