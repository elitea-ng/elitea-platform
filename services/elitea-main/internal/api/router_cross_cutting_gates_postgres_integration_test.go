package api

// The maintenance half of router_cross_cutting_gates_test.go.
//
// The gate reads its switch from centry.platform_config, and a nil pool turns
// it into a pass-through by design, so the only way to ask the router "is this
// route closed during a window" is with the switch really on. The route list
// and the exemptions are the same ones the no-store and client-version checks
// walk; the maintenance allowlist (apimw.MaintenanceExempt) is the one further
// exemption, because a window deliberately leaves sign-in, SCIM, the admin
// panel and platform_settings open.
//
// ci-go.yml supplies ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

func TestEveryAPIRouteIsClosedDuringAMaintenanceWindow(t *testing.T) {
	pool := newStatusOKIntegrationPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	source := filepath.Join("..", "infra", "db", "migrations", "001_initial.sql")
	initial, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	if _, err := pool.Exec(ctx, string(initial)); err != nil {
		t.Fatalf("apply %s: %v", source, err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO centry.platform_config (section, key, value)
VALUES ($1, $2, 'true'::jsonb)`, platformconfig.SectionMaintenance, platformconfig.KeyMaintenanceEnabled); err != nil {
		t.Fatalf("open the maintenance window: %v", err)
	}

	cfg := gatesRouterConfig(t)
	cfg.Pool = pool
	router := NewRouter(cfg)
	all, gated := apiRoutesUnderTest(t, router)
	requireRootMountedRoutesWalked(t, all)

	call := func(method, path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader("{}"))
		request.Header.Set("Content-Type", "application/json")
		// A personal access token: the client version gate passes it, so a
		// refusal here can only be the maintenance window.
		request.Header.Set("Authorization", "Bearer pat-gates")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}

	var failures []string
	checked := 0
	for _, route := range gated {
		path := concretePath(route.pattern)
		if apimw.MaintenanceExempt(path) {
			continue
		}
		checked++
		response := call(route.method, path)
		var body struct {
			Maintenance bool `json:"maintenance"`
		}
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		if response.Code != http.StatusServiceUnavailable || !body.Maintenance {
			failures = append(failures, fmt.Sprintf("%s %s -> %d", route.method, route.pattern, response.Code))
		}
	}
	failRoutes(t, "stayed open to a non-administrator during a maintenance window", gateRemedy, failures)
	t.Logf("%d /api/v2 routes answered 503 maintenance", checked)

	// The control: the allowlist still lets the platform settings read
	// through, so the 503s above are the gate's rule and not a broken router.
	if response := call(http.MethodGet, "/api/v2/elitea_core/platform_settings"); response.Code == http.StatusServiceUnavailable {
		t.Fatalf("platform_settings answered 503 during a window: the maintenance allowlist no longer applies (%s)",
			response.Body.String())
	}
}
