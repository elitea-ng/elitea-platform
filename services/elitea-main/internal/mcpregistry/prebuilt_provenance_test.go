package mcpregistry

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveReportSaysWhatTheCatalogueFilled(t *testing.T) {
	entry := &PrebuiltServer{Enabled: true, ClientID: "cat-client", ClientSecretRef: "ref"}
	lookup := func(string) (string, error) { return "cat-secret", nil }

	// Blank and whitespace-only values are the catalogue's to fill.
	for _, blank := range []any{nil, "", "   ", "\t\n"} {
		resolved, filled := ResolveReport(map[string]any{"client_secret": blank}, "mcp_x", entry, lookup)
		require.True(t, filled.ClientSecret, "%#v", blank)
		require.Equal(t, "cat-secret", resolved["client_secret"])
	}

	// A value the toolkit holds is its own: nothing is reported as filled.
	resolved, filled := ResolveReport(map[string]any{"client_secret": "mine", "client_id": "me"}, "mcp_x", entry, lookup)
	require.False(t, filled.ClientSecret)
	require.False(t, filled.ClientID)
	require.Equal(t, "mine", resolved["client_secret"])

	// A vault miss fills nothing, and says so.
	_, filled = ResolveReport(map[string]any{}, "mcp_x", entry,
		func(string) (string, error) { return "", ErrNoPool })
	require.False(t, filled.ClientSecret)

	// Disabled entries and non-prebuilt types fill nothing.
	_, filled = ResolveReport(map[string]any{}, "mcp_x", &PrebuiltServer{ClientSecretRef: "ref"}, lookup)
	require.False(t, filled.ClientSecret)
	_, filled = ResolveReport(map[string]any{}, "openapi", entry, lookup)
	require.False(t, filled.ClientSecret)
}

func TestPinnedTokenURLMustBeAnExactHTTPSAddress(t *testing.T) {
	for _, ok := range []string{"", "https://idp.example/oauth/token"} {
		require.NoError(t, ValidatePrebuiltTokenURL(ok), ok)
	}
	for _, bad := range []string{
		"http://idp.example/token", "https://{tenant}.example/token", "https://user:pw@idp.example/token",
		"https://idp.example/token#frag", "/relative", "https://idp.example/to ken",
	} {
		require.Error(t, ValidatePrebuiltTokenURL(bad), bad)
	}
}
