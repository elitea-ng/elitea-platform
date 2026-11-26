package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/health"
)

type mockChecker struct {
	err error
}

func (m *mockChecker) Ping(_ context.Context) error {
	return m.err
}

// probeRequest is a request from a probe inside the container: a loopback
// peer with no forwarding header.
func probeRequest(path string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.RemoteAddr = "127.0.0.1:41000"
	return request
}

// With the browser edge OFF (these routes alone), readiness and startup stay
// container-local: a remote peer, or a loopback peer a proxy relayed, gets the
// 404 an unknown path gets. /healthz stays public.
func TestReadinessAndStartupAreContainerLocal(t *testing.T) {
	r := health.RoutesWithDeps(health.Deps{})
	callers := map[string]func(*http.Request){
		"a remote peer":                 func(req *http.Request) { req.RemoteAddr = "203.0.113.7:5000" },
		"a pod-network peer":            func(req *http.Request) { req.RemoteAddr = "10.1.2.3:5000" },
		"loopback with X-Forwarded-For": func(req *http.Request) { req.Header.Set("X-Forwarded-For", "198.51.100.1") },
		"loopback with Forwarded":       func(req *http.Request) { req.Header.Set("Forwarded", "for=198.51.100.1") },
		"loopback with X-Real-IP":       func(req *http.Request) { req.Header.Set("X-Real-IP", "198.51.100.1") },
		"an IPv6 remote peer":           func(req *http.Request) { req.RemoteAddr = "[2001:db8::1]:5000" },
		"a mesh sidecar relaying browser": func(req *http.Request) {
			req.RemoteAddr = "127.0.0.6:5000"
			req.Header.Set("X-Forwarded-For", "198.51.100.1")
		},
	}
	for _, path := range []string{"/readyz", "/startupz"} {
		for name, shape := range callers {
			req := probeRequest(path)
			shape(req)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s from %s: got %d, want 404", path, name, rec.Code)
			}
		}
		for _, local := range []string{"127.0.0.1:41000", "[::1]:41000"} {
			req := probeRequest(path)
			req.RemoteAddr = local
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("%s from the container probe %s: got %d, want 200", path, local, rec.Code)
			}
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = "203.0.113.7:5000"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Errorf("/healthz from a remote peer: got %d %q, want 200 {\"status\":\"ok\"}", rec.Code, rec.Body.String())
	}
}

func TestLiveness(t *testing.T) {
	r := health.RoutesWithDeps(health.Deps{})
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var s health.Status
	_ = json.NewDecoder(rec.Body).Decode(&s)
	if s.Status != "ok" {
		t.Errorf("expected status ok, got %q", s.Status)
	}
}

func TestReadiness_NoDeps(t *testing.T) {
	r := health.RoutesWithDeps(health.Deps{})
	req := probeRequest("/readyz")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestReadiness_AllHealthy(t *testing.T) {
	r := health.RoutesWithDeps(health.Deps{
		DB: &mockChecker{},
	})
	req := probeRequest("/readyz")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var s health.Status
	_ = json.NewDecoder(rec.Body).Decode(&s)
	if s.Status != "ready" {
		t.Errorf("expected status ready, got %q", s.Status)
	}
	if s.Checks["db"] != "ok" {
		t.Errorf("expected db ok, got %q", s.Checks["db"])
	}
	// Redis no longer decides readiness: the Form sign-in state is in the
	// database (shared migration 0153).
	if _, present := s.Checks["redis"]; present || len(s.Checks) != 1 {
		t.Errorf("readiness checks = %v, want the database alone", s.Checks)
	}
}

func TestReadiness_DBDown(t *testing.T) {
	r := health.RoutesWithDeps(health.Deps{
		DB: &mockChecker{err: errors.New("connection refused")},
	})
	req := probeRequest("/readyz")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	var s health.Status
	_ = json.NewDecoder(rec.Body).Decode(&s)
	if s.Status != "not_ready" {
		t.Errorf("expected status not_ready, got %q", s.Status)
	}
	if s.Checks["db"] != "unavailable" {
		t.Errorf("expected sanitized db error, got %q", s.Checks["db"])
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Fatal("readiness response leaked the dependency error")
	}
}

func TestStartup(t *testing.T) {
	r := health.RoutesWithDeps(health.Deps{})
	req := probeRequest("/startupz")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}
