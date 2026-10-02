package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	discovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitdiscovery"
	"github.com/stretchr/testify/require"
)

type externalDiscoveryFunc func(context.Context, discovery.Request) (discovery.Result, error)

func (f externalDiscoveryFunc) AvailableTools(ctx context.Context, r discovery.Request) (discovery.Result, error) {
	return f(ctx, r)
}

func TestExternalInstanceSchemasKeepExactRequiredAndNestedShapes(t *testing.T) {
	result := discovery.Result{Tools: []discovery.Tool{{Name: "query"}}, ArgsSchemas: map[string]json.RawMessage{
		"query": json.RawMessage(`{"type":"object","required":["payload"],"properties":{"payload":{"$ref":"#/$defs/Payload"}},"$defs":{"Payload":{"type":"object","properties":{"count":{"type":"integer","maximum":9007199254740993}}}}}`),
	}}
	schemas, available, err := externalInstanceSchemas(result)
	require.NoError(t, err)
	require.True(t, available["query"])
	encoded, err := json.Marshal(schemas["query"])
	require.NoError(t, err)
	require.JSONEq(t, string(result.ArgsSchemas["query"]), string(encoded))
	require.Contains(t, string(encoded), "9007199254740993")
}

func TestExternalInstanceSchemasRejectIncompleteOrAmbiguousDiscovery(t *testing.T) {
	for _, result := range []discovery.Result{
		{Tools: []discovery.Tool{{Name: "missing"}}},
		{Tools: []discovery.Tool{{Name: "same"}, {Name: "same"}}, ArgsSchemas: map[string]json.RawMessage{"same": json.RawMessage(`{"type":"object"}`)}},
		{Tools: []discovery.Tool{{Name: "bad"}}, ArgsSchemas: map[string]json.RawMessage{"bad": json.RawMessage(`{"type":"array"}`)}},
	} {
		_, _, err := externalInstanceSchemas(result)
		require.Error(t, err)
	}
}

func TestExternalLiveSchemaUsesSavedInstanceIdentityAndPropagatesCancellation(t *testing.T) {
	for _, family := range []string{"openapi", "mcp", "mcp_config", "mcp_github"} {
		t.Run(family, func(t *testing.T) {
			calls := 0
			h := &Handler{toolkitDiscovery: externalDiscoveryFunc(func(ctx context.Context, r discovery.Request) (discovery.Result, error) {
				calls++
				require.Equal(t, discovery.Request{ProjectID: 7, ActorUserID: 41, ToolkitID: 19}, r)
				return discovery.Result{}, ctx.Err()
			})}
			source := postgresToolSource{handler: h}
			_, _, live, err := source.toolkitInstanceSchemas(context.Background(), `"p_7"`, 41, 19, family)
			require.NoError(t, err)
			require.True(t, live)
			require.Equal(t, 1, calls)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, _, _, err = source.toolkitInstanceSchemas(ctx, `"p_7"`, 41, 19, family)
			require.ErrorIs(t, err, context.Canceled)
			_, _, _, err = source.toolkitInstanceSchemas(ctx, `"p_7";SELECT`, 41, 19, family)
			require.True(t, errors.Is(err, errExternalCatalogIdentity))
		})
	}
}
