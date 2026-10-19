package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-scheduler/internal/budgetwriteback"
)

// The health port the binary composes serves the write-back metrics: an
// operator control with no route is one nobody can read.
func TestHealthMuxServesTheWriteBackMetrics(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := httptest.NewServer(newHealthMux(ok, budgetwriteback.NewStatus()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("GET /metrics = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(string(body), "# TYPE budget_writeback_dropped_total counter") {
		t.Fatalf("GET /metrics does not declare budget_writeback_dropped_total:\n%s", body)
	}
	for _, path := range []string{"/healthz", "/readyz/budget-writeback"} {
		r, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		if r.StatusCode == http.StatusNotFound {
			t.Errorf("GET %s is not mounted", path)
		}
	}
}
