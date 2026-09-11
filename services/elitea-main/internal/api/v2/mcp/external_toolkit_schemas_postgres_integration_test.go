package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/mcp"
	discovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitdiscovery"
	"github.com/stretchr/testify/require"
)

type externalInstanceDiscoveryStub struct{ requests []discovery.Request }

func (s *externalInstanceDiscoveryStub) AvailableTools(ctx context.Context, r discovery.Request) (discovery.Result, error) {
	if err := ctx.Err(); err != nil {
		return discovery.Result{}, err
	}
	s.requests = append(s.requests, r)
	return discovery.Result{Tools: []discovery.Tool{{Name: "search"}}, ArgsSchemas: map[string]json.RawMessage{
		"search": json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"query":{"type":"string"},"instance":{"const":%d}},"required":["query"],"additionalProperties":false}`, r.ToolkitID)),
	}}, nil
}

func TestExternalCatalogueUsesLiveSavedInstanceSchemas(t *testing.T) {
	pool := newMCPPool(t)
	live := &externalInstanceDiscoveryStub{}
	handler := mcp.NewHandler(pool, apimw.NewDBPersonalProjectResolver(pool), nil, nil, mcp.WithToolkitDiscovery(live))
	router := newRouter(handler, callerUserID)
	for _, family := range []string{"openapi", "mcp", "mcp_config", "mcp_records"} {
		id := seedToolkit(t, pool, homeSchema, family, family, availableByMCP, "search", "withdrawn")
		tool := listedTool(t, router, fmt.Sprintf("/app/1/mcp/toolkit/%d", id), family+"_search")
		schema, ok := tool["inputSchema"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, []any{"query"}, schema["required"])
		require.Equal(t, false, schema["additionalProperties"])
		require.Equal(t, discovery.Request{ProjectID: 1, ActorUserID: callerUserID, ToolkitID: id}, live.requests[len(live.requests)-1])
		listed := listToolNames(t, router, fmt.Sprintf("/app/1/mcp/toolkit/%d", id))
		require.NotContains(t, listed, family+"_withdrawn")
	}
	calls := len(live.requests)
	id := seedToolkit(t, pool, homeSchema, "private", "openapi", `{}`, "search")
	require.Empty(t, listToolNames(t, router, fmt.Sprintf("/app/1/mcp/toolkit/%d", id)))
	require.Equal(t, calls, len(live.requests))
	// A foreign project has no matching saved instance and starts no discovery.
	require.Empty(t, listToolNames(t, router, fmt.Sprintf("/app/2/mcp/toolkit/%d", id)))
	require.Equal(t, calls, len(live.requests))
}
