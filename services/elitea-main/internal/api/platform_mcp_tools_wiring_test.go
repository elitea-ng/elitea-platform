package api

import (
	"testing"

	desktopopsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/desktopops"
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
	// A router composed without the bridge still builds.
	_ = NewRouter(RouterConfig{})
}
