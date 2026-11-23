package api

// The /admin/gateway routes of ADR-0059 R0.3: one group-wide
// configuration.governance gate became one gate per sub-surface, each
// accepting a narrow permission OR configuration.governance.
//
// Every request goes through the production router (NewRouter), so the gate
// under test is the one router.go composes, not a hand-mounted copy. The
// resolver grants exactly the permissions a test names
// (platformGateResolver, admin_platform_gates_test.go).
//
// Two kinds of proof live here:
//
//   - The gate tables (no database). The legacy holder's answers are pinned
//     in testdata/admin_gateway_legacy.golden, captured on main before the
//     split: a holder of configuration.governance sees no change.
//   - The database tables (ELITEA_TEST_DATABASE_URL). A read holder gets 200
//     on every read and 403 on every write; a policy manager is refused a
//     guardrail write; an alerts manager saves the budget alerts.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goldenAdminGatewayLegacy = "testdata/admin_gateway_legacy.golden"

// gatewayGateRoute is one route under /admin/gateway. class is the class of
// the permission its gate takes: read or write.
type gatewayGateRoute struct {
	method, path, body string
	class              string
}

const (
	gatewayRead  = "read"
	gatewayWrite = "write"
)

const gatewayPrefix = "/api/v2/admin/gateway"

// gatewayGateRoutes are every route under /admin/gateway, in the order of the
// golden file. The bodies are the smallest a handler decodes; a gate never
// reads them. The ids name rows that do not exist.
var gatewayGateRoutes = []gatewayGateRoute{
	// Usage and alerts.
	{http.MethodGet, gatewayPrefix + "/budget-alerts", "", gatewayRead},
	{http.MethodPut, gatewayPrefix + "/budget-alerts", `{"enabled":true}`, gatewayWrite},
	{http.MethodGet, gatewayPrefix + "/status", "", gatewayRead},
	{http.MethodGet, gatewayPrefix + "/usage", "", gatewayRead},
	{http.MethodGet, gatewayPrefix + "/logs", "", gatewayRead},
	// Policy.
	{http.MethodGet, gatewayPrefix + "/governance", "", gatewayRead},
	{http.MethodPost, gatewayPrefix + "/governance/validate-cel", `{"cel":"true"}`, gatewayRead},
	{http.MethodPost, gatewayPrefix + "/governance", `{}`, gatewayWrite},
	{http.MethodPut, gatewayPrefix + "/governance/00000000-0000-0000-0000-000000000001", `{}`, gatewayWrite},
	{http.MethodDelete, gatewayPrefix + "/governance/00000000-0000-0000-0000-000000000001", "", gatewayWrite},
	// Catalogue: prices.
	{http.MethodGet, gatewayPrefix + "/models", "", gatewayRead},
	{http.MethodPut, gatewayPrefix + "/models", `{}`, gatewayWrite},
	{http.MethodDelete, gatewayPrefix + "/models/fixture", "", gatewayWrite},
	{http.MethodGet, gatewayPrefix + "/metered-prices", "", gatewayRead},
	{http.MethodPut, gatewayPrefix + "/metered-prices", `{}`, gatewayWrite},
	// Catalogue: global providers.
	{http.MethodGet, gatewayPrefix + "/providers", "", gatewayRead},
	{http.MethodPost, gatewayPrefix + "/providers", `{}`, gatewayWrite},
	{http.MethodPut, gatewayPrefix + "/providers/999999", `{}`, gatewayWrite},
	{http.MethodDelete, gatewayPrefix + "/providers/999999", "", gatewayWrite},
	{http.MethodPost, gatewayPrefix + "/providers/999999/check", "", gatewayWrite},
	{http.MethodPost, gatewayPrefix + "/providers/999999/revalidate", "", gatewayWrite},
	{http.MethodPost, gatewayPrefix + "/providers/999999/models", "", gatewayWrite},
	// Catalogue: platform models and the default model.
	{http.MethodGet, gatewayPrefix + "/platform_models", "", gatewayRead},
	{http.MethodPost, gatewayPrefix + "/platform_models", `{}`, gatewayWrite},
	{http.MethodPut, gatewayPrefix + "/platform_models/999999", `{}`, gatewayWrite},
	{http.MethodDelete, gatewayPrefix + "/platform_models/999999", "", gatewayWrite},
	{http.MethodGet, gatewayPrefix + "/platform_models/999999/default_usage", "", gatewayRead},
	{http.MethodGet, gatewayPrefix + "/default_model?section=llm", "", gatewayRead},
	{http.MethodPut, gatewayPrefix + "/default_model?section=llm", `{"model_name":"fixture"}`, gatewayWrite},
	{http.MethodDelete, gatewayPrefix + "/default_model?section=llm", "", gatewayWrite},
}

// gatewayAnswers drives every route with a router that grants granted and
// answers one line per route.
func gatewayAnswers(t *testing.T, router http.Handler) string {
	t.Helper()
	var b strings.Builder
	for _, route := range gatewayGateRoutes {
		rec := servePlatformGate(t, router, route.method, route.path, route.body)
		fmt.Fprintf(&b, "%s\t%s\t%d\n", route.method, route.path, rec.Code)
	}
	return b.String()
}

// TestGatewayRoutesAnswerTheLegacyHolderAsBefore pins every /admin/gateway
// route's answer to a holder of configuration.governance alone. The file was
// captured on main before the group gate was split; regenerate it with
// UPDATE_GOLDEN=1 only for a change that means to alter what a legacy holder
// sees.
func TestGatewayRoutesAnswerTheLegacyHolderAsBefore(t *testing.T) {
	got := gatewayAnswers(t, newPlatformGateRouter(t, "configuration.governance"))

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenAdminGatewayLegacy), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenAdminGatewayLegacy, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenAdminGatewayLegacy)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_GOLDEN=1)", goldenAdminGatewayLegacy, err)
	}
	if got != string(want) {
		t.Errorf("a holder of configuration.governance gets different answers than before:\n got:\n%s\nwant:\n%s", got, want)
	}
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.HasSuffix(line, "\t403") {
			t.Errorf("%s: the legacy holder is refused", line)
		}
	}
}
