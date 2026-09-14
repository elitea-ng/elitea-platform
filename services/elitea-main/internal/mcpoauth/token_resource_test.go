package mcpoauth_test

import (
	"context"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpoauth"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestToolkitResourceMatchesNativeEndpointAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		settings   map[string]any
		want       string
	}{
		{"mcp root", "mcp", map[string]any{"url": "https://EXAMPLE.test:443"}, "https://example.test/"},
		{"mcp double slash", "mcp", map[string]any{"url": "https://example.test/a//b"}, "https://example.test/a//b"},
		{"mcp path", "mcp", map[string]any{"url": "https://example.test/mcp/"}, "https://example.test/mcp/"},
		{"openapi override", "openapi", map[string]any{"base_url": "https://api.test/v1///", "base_url_override": "https://ignored.test", "spec": "https://not-resource.test/spec"}, "https://api.test/v1"},
		{"openapi second override", "openapi", map[string]any{"base_url": "", "base_url_override": "https://api.test/"}, "https://api.test"},
		{"openapi inline", "openapi", map[string]any{"spec": `{"servers":[{"url":"https://api.test/v1/"}],"paths":{"/x":{"servers":[{"url":"https://ignored.test"}]}}}`}, "https://api.test/v1"},
		{"openapi yaml variable", "openapi", map[string]any{"schema_settings": "servers:\n  - url: https://{region}.example.test/api\n    variables:\n      region:\n        default: north\n"}, "https://north.example.test/api"},
		{"openapi spec alias", "openapi", map[string]any{"openapi_spec": map[string]any{"servers": []any{map[string]any{"url": "https://api.test"}}}}, "https://api.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mcpoauth.ToolkitResource(tc.kind, tc.settings)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestToolkitResourceRejectsUnboundOrUnsafeEndpoints(t *testing.T) {
	for _, settings := range []map[string]any{
		{"base_url": "https://api.test?x=1"}, {"base_url": "https://user:secret@api.test"}, {"base_url": "http://api.test"}, {"base_url": "https://api.test/#"}, {"base_url": 123},
		{"spec": "https://api.test/spec.json"},
		{"spec": `{"servers":[{"url":"https://{x}.test","variables":{"x":{"default":"{x}"}}}]}`},
		{"spec": `{"servers":[{"url":"https://{x}.test"}]}`},
		{"spec": nil, "schema_settings": `{"servers":[{"url":"https://api.test"}]}`},
	} {
		_, err := mcpoauth.ToolkitResource("openapi", settings)
		require.ErrorIs(t, err, mcpoauth.ErrTokenUnavailable)
	}
	_, err := mcpoauth.ToolkitResource("mcp", map[string]any{"server_url": "https://unbound.test"})
	require.ErrorIs(t, err, mcpoauth.ErrTokenUnavailable)
}

func TestAbsentDelegatedStoreFailsClosed(t *testing.T) {
	var store *mcpoauth.Tokens
	binding := mcpoauth.TokenBinding{ProjectID: 1, ActorID: 2, ToolkitID: 3, Resource: "https://example.test"}
	_, err := store.Validate(context.Background(), strings.Repeat("a", 43), binding)
	require.ErrorIs(t, err, mcpoauth.ErrTokenUnavailable)
	_, err = store.Load(context.Background(), strings.Repeat("a", 43), binding)
	require.ErrorIs(t, err, mcpoauth.ErrTokenUnavailable)
	err = store.Revoke(context.Background(), strings.Repeat("a", 43), binding)
	require.ErrorIs(t, err, mcpoauth.ErrTokenUnavailable)
}
