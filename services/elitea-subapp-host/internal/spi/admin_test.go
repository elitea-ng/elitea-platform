package spi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/apps/echo"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// The admin port's /readyz is the readiness the provider service's Health
// reports: a lost owner claim answers 503 and names the failing check, so
// the kubelet takes the replica out of the Service. /healthz stays 200: the
// process is alive, and restarting it would not bring the claim back.
func TestTheAdminPortReportsReadinessOverPlainHTTP(t *testing.T) {
	s, _ := spi.SettingsFromEnv("ELITEA_ECHO_", env(map[string]string{}))
	if s.AdminAddr != spi.DefaultAdminAddr {
		t.Fatalf("the admin address defaults to %q, want %q", s.AdminAddr, spi.DefaultAdminAddr)
	}
	var claim atomic.Bool
	claim.Store(true)
	server, err := spi.NewServer(s, echo.App(0), nil, spi.WithReadiness(func(context.Context) map[string]bool {
		return map[string]bool{"owner_claim": claim.Load()}
	}))
	if err != nil {
		t.Fatal(err)
	}
	admin := spi.AdminHandler(server)
	get := func(path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		admin.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder
	}
	if r := get("/readyz"); r.Code != http.StatusOK {
		t.Fatalf("readyz with the claim held: %d %s", r.Code, r.Body)
	}
	claim.Store(false)
	if r := get("/readyz"); r.Code != http.StatusServiceUnavailable || !strings.Contains(r.Body.String(), "owner_claim") {
		t.Fatalf("readyz with the claim lost: %d %s", r.Code, r.Body)
	}
	if r := get("/healthz"); r.Code != http.StatusOK {
		t.Fatalf("healthz with the claim lost: %d %s", r.Code, r.Body)
	}
	// Health and nothing else: no SPI route answers on this port.
	if r := get("/tools/Echo/echo/invoke"); r.Code != http.StatusNotFound {
		t.Fatalf("an SPI route on the admin port: %d", r.Code)
	}
}

// /metrics serves the gauges an autoscaler reads: the invocations this
// replica runs now and the slots one replica has. An invocation in flight
// raises the first, and its end lowers it again.
func TestTheAdminPortPublishesTheInFlightGauge(t *testing.T) {
	s, err := spi.SettingsFromEnv("ELITEA_ECHO_", env(map[string]string{"ELITEA_ECHO_MAX_PARALLEL_WORKERS": "3"}))
	if err != nil {
		t.Fatal(err)
	}
	server, err := spi.NewServer(s, echo.App(0), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Stop)
	admin := spi.AdminHandler(server)
	scrape := func() string {
		recorder := httptest.NewRecorder()
		admin.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/plain") {
			t.Fatalf("/metrics: %d %q", recorder.Code, recorder.Header().Get("Content-Type"))
		}
		return recorder.Body.String()
	}
	app := `{app="` + echo.App(0).Name + `"}`
	for _, line := range []string{
		"# TYPE " + spi.MetricInvocationsInFlight + " gauge",
		spi.MetricInvocationsInFlight + app + " 0",
		"# TYPE " + spi.MetricInvocationSlots + " gauge",
		spi.MetricInvocationSlots + app + " 3",
	} {
		if body := scrape(); !strings.Contains(body, line+"\n") {
			t.Fatalf("/metrics lacks %q:\n%s", line, body)
		}
	}
	release := make(chan struct{})
	invocation, err := server.Manager().Submit(context.Background(), "Echo", "echo",
		func(ctx context.Context, _ *spi.Context) (map[string]any, error) {
			<-release
			return map[string]any{}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if body := scrape(); !strings.Contains(body, spi.MetricInvocationsInFlight+app+" 1\n") {
		t.Fatalf("one invocation %s in flight:\n%s", invocation.ID, body)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(scrape(), spi.MetricInvocationsInFlight+app+" 0\n") {
		if time.Now().After(deadline) {
			t.Fatalf("the gauge stayed up after the invocation ended:\n%s", scrape())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// WaitIdle is the drain of a terminating replica: it returns when the last
// invocation ends, and at its deadline it gives up without cancelling.
func TestWaitIdleWaitsForTheRunsAndCancelsNothing(t *testing.T) {
	s, _ := spi.SettingsFromEnv("ELITEA_ECHO_", env(map[string]string{}))
	if s.DrainTimeoutSeconds != spi.DefaultDrainTimeoutSeconds {
		t.Fatalf("the drain defaults to %d s, want %d", s.DrainTimeoutSeconds, spi.DefaultDrainTimeoutSeconds)
	}
	server, err := spi.NewServer(s, echo.App(0), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Stop)
	manager := server.Manager()
	if err := manager.WaitIdle(context.Background()); err != nil {
		t.Fatalf("an idle manager: %v", err)
	}
	release := make(chan struct{})
	cancelled := make(chan struct{})
	if _, err := manager.Submit(context.Background(), "Echo", "echo",
		func(ctx context.Context, _ *spi.Context) (map[string]any, error) {
			select {
			case <-release:
			case <-ctx.Done():
				close(cancelled)
			}
			return map[string]any{}, nil
		}); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := manager.WaitIdle(short); err == nil {
		t.Fatal("WaitIdle returned while an invocation ran")
	}
	select {
	case <-cancelled:
		t.Fatal("the drain deadline cancelled the invocation")
	default:
	}
	go func() { time.Sleep(50 * time.Millisecond); close(release) }()
	long, cancelLong := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelLong()
	if err := manager.WaitIdle(long); err != nil {
		t.Fatalf("WaitIdle after the run ended: %v", err)
	}
}
