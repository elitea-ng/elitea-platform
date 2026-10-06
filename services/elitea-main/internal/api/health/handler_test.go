package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/health"
)

type mockChecker struct {
	err error
}

func (m *mockChecker) Ping(_ context.Context) error {
	return m.err
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
	req := httptest.NewRequest("GET", "/readyz", nil)
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
	req := httptest.NewRequest("GET", "/readyz", nil)
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
	// database (shared migration 0145).
	if _, present := s.Checks["redis"]; present || len(s.Checks) != 1 {
		t.Errorf("readiness checks = %v, want the database alone", s.Checks)
	}
}

func TestReadiness_DBDown(t *testing.T) {
	r := health.RoutesWithDeps(health.Deps{
		DB: &mockChecker{err: errors.New("connection refused")},
	})
	req := httptest.NewRequest("GET", "/readyz", nil)
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
	req := httptest.NewRequest("GET", "/startupz", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}
