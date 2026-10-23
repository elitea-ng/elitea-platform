package api

import (
	"testing"

	desktopopsapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/desktopops"
)

// The remote toolkit route runs a platform MCP tool through a bridge the
// ROUTER binds to the MCP server it builds (issue 1231). Both halves have unit
// tests; this is the composition root, which neither of them reaches.
func TestRouterBindsThePlatformMCPToolBridge(t *testing.T) {
	bridge := desktopopsapi.NewPlatformToolBridge()
	if bridge.Bound() {
		t.Fatal("a new bridge is bound")
	}
	_ = NewRouter(RouterConfig{PlatformMCPTools: bridge})
	if !bridge.Bound() {
		t.Fatal("NewRouter built the MCP server but did not bind the platform MCP tool bridge")
	}
	// The bound caller also lists a server's tools, so the resolved
	// definition names what a call runs (client contract 1.16).
	tools, err := bridge.PlatformTools(t.Context(), 7, "mcp_elitea_internal_applications")
	if err != nil || len(tools) == 0 {
		t.Fatalf("PlatformTools through the bound bridge = (%v, %v), want the category's tools", tools, err)
	}
	if unbound, err := desktopopsapi.NewPlatformToolBridge().PlatformTools(t.Context(), 7, "mcp_elitea_internal_applications"); unbound != nil || err != nil {
		t.Fatalf("an unbound bridge listed (%v, %v)", unbound, err)
	}
	// A router composed without the bridge still builds.
	_ = NewRouter(RouterConfig{})
}
