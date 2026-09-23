package mcp

import (
	"context"
	"encoding/json"
	toolkits "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	discovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitdiscovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"net/http"
	"testing"
)

type toolkitDiscoveryStub struct {
	calls   int
	request discovery.Request
}

func (s *toolkitDiscoveryStub) AvailableTools(_ context.Context, r discovery.Request) (discovery.Result, error) {
	s.calls++
	s.request = r
	return discovery.Result{Tools: []discovery.Tool{{Name: "list_issues"}}, ArgsSchemas: map[string]json.RawMessage{"list_issues": json.RawMessage(`{"type":"object"}`)}}, nil
}
func TestInternalToolkitDiscoveryUsesSameHandlerAndPermission(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		t.Run(map[bool]string{true: "allowed", false: "denied"}[allowed], func(t *testing.T) {
			source := &toolkitDiscoveryStub{}
			handler := toolkits.NewHandler(nil, toolkits.WithDiscovery(source))
			executor := newHandlerInternalToolkitExecutor(handler)
			permission := "models.applications.tool.details"
			permissions := &recordingInternalPermissionResolver{resolution: auth.PermissionResolution{UserID: 41}}
			if allowed {
				permissions.resolution.Permissions = []string{permission}
			}
			router := internalToolkitRouter(t, internalToolkitTool(internalAvailableTools, permission), executor, permissions)
			response := post(t, router, "/app/7/mcp/elitea_core/toolkits", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_toolkit_test","arguments":{"project_id":7,"toolkit_id":19}}}`)
			if response.Code != http.StatusOK {
				t.Fatalf("HTTP %d", response.Code)
			}
			if allowed && (source.calls != 1 || source.request != (discovery.Request{ProjectID: 7, ActorUserID: 41, ToolkitID: 19})) {
				t.Fatal("discovery scope drift")
			}
			if !allowed && source.calls != 0 {
				t.Fatal("denied MCP invocation dispatched")
			}
		})
	}
}
func TestInternalToolkitDiscoveryCatalogueRequiresRuntime(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		found := false
		for _, tool := range internalToolkitTools(enabled) {
			if tool.internalToolkitOperation == internalAvailableTools {
				found = true
			}
		}
		if found != enabled {
			t.Fatal("runtime activation did not control catalogue")
		}
	}
}
