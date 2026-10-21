package spi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/echo"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
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
