package main

// budget_recovery_test.go — issue #315: the composition root re-establishes
// budget enforcement when NATS returns.
//
// RUN WITH -race. TestEnforcementPlane_ReadyzFlipsUnderLoad installs a store
// while /readyz requests run, which is the same hazard class the handler's gate
// had: a value the request path reads, written by another goroutine.

import (
	"context"
	"encoding/json"
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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/config"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/cost"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/governance"
	natsinfra "github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/infra/nats"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/llmproxy"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/policy"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/server"
)

// enforcingCfg is the posture issue #304 gates and issue #315 recovers from:
// NATS is configured, so a missing store is a fault and not a choice.
func enforcingCfg() config.Config {
	return config.Config{
		HTTPAddr:            "127.0.0.1:0",
		InitialPoolSize:     1,
		ProviderConcurrency: 1,
		NATSURL:             "nats://127.0.0.1:4222",
	}
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// waitForCount blocks until c reaches n, and fails the test after 5 s.
func waitForCount(t *testing.T, c *atomic.Int64, n int64, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for c.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%s reached %d in 5 s, want at least %d", what, c.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// readyzState performs one /readyz request and reports the status code and the
// body's status field.
func readyzState(t *testing.T, h http.HandlerFunc) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/readyz body is not valid JSON: %s", rec.Body.String())
	}
	return rec.Code, body.Status
}

// TestEnforcementPlane_ReadyzFlipsUnderLoad is the readiness half of issue
// #315. A recovery that installs the gate but leaves the pod not-ready forever
// serves nothing, so the SAME mounted handler must change its answer.
//
// The install runs while /readyz requests are in flight, because that is when
// it happens in production — the probe keeps polling through the outage.
func TestEnforcementPlane_ReadyzFlipsUnderLoad(t *testing.T) {
	plane := &enforcementPlane{cfg: enforcingCfg()}
	readyz := makeReadyzHandler(plane, plane.unwired)

	// Boot state: NATS configured, nothing wired (issue #304).
	if code, status := readyzState(t, readyz); code != http.StatusServiceUnavailable || status != "not_ready" {
		t.Fatalf("pre-install /readyz = %d %q, want 503 not_ready", code, status)
	}

	var (
		wg      sync.WaitGroup
		stop    = make(chan struct{})
		probes  atomic.Int64
		ready   atomic.Int64
		unknown atomic.Int64
	)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rec := httptest.NewRecorder()
				readyz.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
				switch rec.Code {
				case http.StatusOK:
					ready.Add(1)
				case http.StatusServiceUnavailable:
				default:
					unknown.Add(1)
				}
				probes.Add(1)
			}
		}()
	}
	waitForCount(t, &probes, 8, "probes before the install")

	// A zero-value store stands in for the one a re-dial builds: Ping is
	// nil-client safe, and this test is about WHEN the plane publishes, not
	// about what the store reports.
	if !plane.install(&governance.GovernanceStore{}) {
		t.Fatal("install refused the first store")
	}
	// The LIVE probes must see the change, not only a request issued after the
	// load stopped. A recovery the running probe never observes keeps the pod
	// out of the rotation for ever.
	waitForCount(t, &ready, 1, "ready answers after the install")
	close(stop)
	wg.Wait()

	if unknown.Load() != 0 {
		t.Errorf("/readyz answered %d requests with neither 200 nor 503", unknown.Load())
	}
	if code, status := readyzState(t, readyz); code != http.StatusOK || status != "ready" {
		t.Fatalf("post-install /readyz = %d %q, want 200 ready — the pod never returns to the rotation",
			code, status)
	}
}

// TestEnforcementPlane_InstallIsOnce pins the monotonic contract the readers
// depend on: a published store is never replaced.
func TestEnforcementPlane_InstallIsOnce(t *testing.T) {
	plane := &enforcementPlane{cfg: enforcingCfg()}
	first := &governance.GovernanceStore{}
	if !plane.install(first) {
		t.Fatal("the first install was refused")
	}
	if plane.install(&governance.GovernanceStore{}) {
		t.Error("a second install replaced the published store")
	}
	if plane.install(nil) {
		t.Error("a nil store was published")
	}
	if plane.current() != first {
		t.Error("current() is not the store installed first")
	}
	if plane.unwired() {
		t.Error("unwired stayed true after a store was published")
	}
}

// TestEnforcementPlane_UnwiredOnlyWhenConfigured keeps the issue #304 scope: a
// deployment with no GATEWAY_NATS_URL enforces nothing on purpose and must
// still report ready.
func TestEnforcementPlane_UnwiredOnlyWhenConfigured(t *testing.T) {
	plane := &enforcementPlane{cfg: config.Config{}}
	if plane.unwired() {
		t.Error("a gateway with no GATEWAY_NATS_URL reports enforcement unwired")
	}
	code, status := readyzState(t, makeReadyzHandler(plane, plane.unwired))
	if code != http.StatusOK || status != "ready" {
		t.Errorf("/readyz = %d %q, want 200 ready for a deliberately NATS-less gateway", code, status)
	}
}

// stubNATS is a server.NATSClient that does nothing. The recovery loop only
// passes it on, so no budget method is exercised. Close IS implemented: the
// server closes the client at shutdown, and the embedded nil interface would
// panic there.
type stubNATS struct{ server.NATSClient }

func (s *stubNATS) Close() {}

// newTestServer builds a Server whose NATS connector answers with connectErr
// until flip is closed, then answers with a stub client. It reproduces the
// #315 sequence: the boot dial fails, and a later dial succeeds.
func newTestServer(t *testing.T, connectErr error, flip <-chan struct{}, dials *atomic.Int64) *server.Server {
	t.Helper()
	srv, err := server.New(
		context.Background(), enforcingCfg(), testLogger(), new(slog.LevelVar), nil, http.NewServeMux(),
		server.WithNATSConnector(func(context.Context, natsinfra.Config) (server.NATSClient, error) {
			dials.Add(1)
			select {
			case <-flip:
				return &stubNATS{}, nil
			default:
				return nil, connectErr
			}
		}),
	)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv
}

// TestBudgetRecovery_InstallsWhenNATSReturns is the recovery half of issue
// #315, end to end through attempt(): the dial fails, then succeeds, and the
// success installs the gate on the RUNNING handler, publishes the store for
// /readyz and the shutdown drain, and moves the enforcement gauge.
//
// buildGovernance itself is stubbed because it needs a live database pool. The
// production wiring of the real one is asserted by TestMainWiring.
func TestBudgetRecovery_InstallsWhenNATSReturns(t *testing.T) {
	flip := make(chan struct{})
	var dials atomic.Int64
	srv := newTestServer(t, errors.New("connection refused"), flip, &dials)
	// The #304 boot state: the dial failed, so there is no client at all.
	if srv.NATS() != nil {
		t.Fatal("srv.NATS() = non-nil, want nil after a failed boot dial")
	}

	handler := llmproxy.NewHandler(nil, testLogger(), nil)
	plane := &enforcementPlane{cfg: enforcingCfg()}
	built := &governance.GovernanceStore{}
	var builds atomic.Int64

	r := budgetRecovery{
		cfg:     enforcingCfg(),
		srv:     srv,
		pool:    (*pgxpool.Pool)(nil),
		plane:   plane,
		handler: handler,
		logger:  testLogger(),
		build: func(context.Context, config.Config, server.NATSClient, *pgxpool.Pool, *slog.Logger) (*governance.GovernanceStore, *cost.Calculator, error) {
			builds.Add(1)
			return built, nil, nil
		},
	}

	// NATS is still down: the attempt fails and changes nothing.
	if r.attempt(context.Background()) {
		t.Fatal("attempt reported the loop finished while NATS was unreachable")
	}
	if handler.BudgetEnforcementInstalled() {
		t.Fatal("a failed attempt installed enforcement")
	}
	if !plane.unwired() {
		t.Fatal("a failed attempt cleared the readiness gate")
	}
	if builds.Load() != 0 {
		t.Fatalf("governance was assembled %d times without a NATS client", builds.Load())
	}

	// NATS comes back.
	close(flip)
	if !r.attempt(context.Background()) {
		t.Fatal("attempt did not report the loop finished after a successful dial")
	}
	if !handler.BudgetEnforcementInstalled() {
		t.Error("the gate was not installed on the running handler")
	}
	if plane.current() != built {
		t.Error("the store the handler bills through was not published for /readyz and the drain")
	}
	if plane.unwired() {
		t.Error("the readiness gate stayed closed after enforcement was installed")
	}
	if builds.Load() != 1 {
		t.Errorf("governance was assembled %d times, want 1", builds.Load())
	}

	// A further attempt is a no-op: the loop has stopped, and nothing swaps a
	// live gate.
	if !r.attempt(context.Background()) {
		t.Error("attempt did not report finished once enforcement was installed")
	}
	if builds.Load() != 1 {
		t.Errorf("a post-install attempt assembled governance again (%d builds)", builds.Load())
	}
}

// TestBudgetRecovery_StopsWhenServerClosed proves the loop ends at shutdown. A
// re-dial after Close would open a connection nothing will ever close.
func TestBudgetRecovery_StopsWhenServerClosed(t *testing.T) {
	flip := make(chan struct{})
	close(flip) // NATS is reachable; only the close must stop the loop
	var dials atomic.Int64
	srv := newTestServer(t, nil, flip, &dials)
	srv.Close()

	r := budgetRecovery{
		cfg:     enforcingCfg(),
		srv:     srv,
		plane:   &enforcementPlane{cfg: enforcingCfg()},
		handler: llmproxy.NewHandler(nil, testLogger(), nil),
		logger:  testLogger(),
		build: func(context.Context, config.Config, server.NATSClient, *pgxpool.Pool, *slog.Logger) (*governance.GovernanceStore, *cost.Calculator, error) {
			t.Error("governance was assembled after the server closed")
			return nil, nil, nil
		},
	}
	if !r.attempt(context.Background()) {
		t.Error("attempt kept retrying after the server closed")
	}
}

// TestBudgetRecovery_SkipsWhenAlreadyWired proves the loop never starts on a
// gateway that already enforces. Without this the recovery would be a second
// path that could swap a live gate.
func TestBudgetRecovery_SkipsWhenAlreadyWired(t *testing.T) {
	plane := &enforcementPlane{cfg: enforcingCfg()}
	plane.install(&governance.GovernanceStore{})

	var dials atomic.Int64
	flip := make(chan struct{})
	close(flip)
	srv := newTestServer(t, nil, flip, &dials)
	before := dials.Load()

	startBudgetRecovery(context.Background(), budgetRecovery{
		cfg:     enforcingCfg(),
		srv:     srv,
		plane:   plane,
		handler: llmproxy.NewHandler(nil, testLogger(), nil),
		logger:  testLogger(),
		build: func(context.Context, config.Config, server.NATSClient, *pgxpool.Pool, *slog.Logger) (*governance.GovernanceStore, *cost.Calculator, error) {
			t.Error("governance was assembled for a gateway that already enforces")
			return nil, nil, nil
		},
	})
	if dials.Load() != before {
		t.Errorf("the recovery loop re-dialled NATS on a gateway that already enforces (%d dials)",
			dials.Load()-before)
	}
}

// rateNATS is a stub NATS client that is also a policy.RateCounter.
type rateNATS struct{ stubNATS }

func (rateNATS) IncrRateLimit(context.Context, string, int64) (int64, error) { return 1, nil }
func (rateNATS) ReadRateLimit(context.Context, string) (int64, error)        { return 0, nil }

// TestBudgetRecovery_RebindsRateLimiter is the rate-limit half of the outage:
// the limiter is built unbound at boot (NATS down), reports disabled on the
// gauge and /readyz, and enforces again once the recovery loop's re-dial
// succeeds — even when governance assembly itself fails.
func TestBudgetRecovery_RebindsRateLimiter(t *testing.T) {
	flipped := make(chan struct{})
	srv, err := server.New(
		context.Background(), enforcingCfg(), testLogger(), new(slog.LevelVar), nil, http.NewServeMux(),
		server.WithNATSConnector(func(context.Context, natsinfra.Config) (server.NATSClient, error) {
			select {
			case <-flipped:
				return &rateNATS{}, nil
			default:
				return nil, errors.New("connection refused")
			}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	limiter := policy.NewLimiter(policy.LimiterConfig{Subject: natsinfra.RateLimitSubject})
	trackRateLimiter(limiter)
	t.Cleanup(func() { trackRateLimiter(nil) })

	if k, v := rateLimitsCheck(); k != "rate_limits" || v != "disabled" {
		t.Fatalf("boot check = %q=%q, want rate_limits=disabled", k, v)
	}
	if got := rateLimitsEnforced.String(); got != "0" {
		t.Fatalf("gauge = %s, want 0 while degraded", got)
	}

	plane := &enforcementPlane{cfg: enforcingCfg()}
	r := budgetRecovery{
		cfg: enforcingCfg(), srv: srv, plane: plane,
		handler: llmproxy.NewHandler(nil, testLogger(), nil),
		limiter: limiter, logger: testLogger(),
		build: func(context.Context, config.Config, server.NATSClient, *pgxpool.Pool, *slog.Logger) (*governance.GovernanceStore, *cost.Calculator, error) {
			return nil, nil, errors.New("assembly failed")
		},
	}
	if r.attempt(context.Background()) || limiter.Enabled() {
		t.Fatal("limiter enabled while NATS is unreachable")
	}

	close(flipped)
	r.attempt(context.Background()) // governance fails; limiter must still bind
	if !limiter.Enabled() || limiter.Status() != policy.RateStatusOK {
		t.Fatalf("limiter status = %s after NATS returned, want ok", limiter.Status())
	}
	if got := rateLimitsEnforced.String(); got != "1" {
		t.Fatalf("gauge = %s, want 1 after recovery", got)
	}
	if k, _ := rateLimitsCheck(); k != "" {
		t.Fatal("rate_limits check still reported after recovery")
	}

	// /readyz carries the detail without changing the status code.
	limiter2 := policy.NewLimiter(policy.LimiterConfig{Subject: natsinfra.RateLimitSubject})
	trackRateLimiter(limiter2)
	rec := httptest.NewRecorder()
	makeReadyzHandler(nil, nil, rateLimitsCheck).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"rate_limits":"disabled"`) {
		t.Fatalf("readyz = %d %s", rec.Code, rec.Body.String())
	}
}

// TestBudgetRecovery_LoopStopsOnCancel: the background loop must not outlive
// its context (no goroutine leak when the gateway shuts down mid-outage).
func TestBudgetRecovery_LoopStopsOnCancel(t *testing.T) {
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { budgetRecovery{logger: testLogger()}.run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery loop did not stop after ctx cancel")
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("goroutines %d -> %d after loop stopped", before, n)
	}
}
