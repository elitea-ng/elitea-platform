package eliteacore

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/webhook"
)

// #6689, Box's real shape: the toolkit URL has no path, the protected-resource
// document names the resource and the issuer WITH a trailing slash, and the
// authorization-server document publishes the issuer WITHOUT one. Load Tools
// must reach the Authorize prompt, not "MCP tool discovery failed".
func TestMCPSyncToolsAcceptsBoxTrailingSlashResourceAndIssuer(t *testing.T) {
	var base string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			if r.Method == http.MethodPost {
				w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.NotFound(w, r)
		case "/.well-known/oauth-protected-resource":
			writeJSON(w, http.StatusOK, map[string]any{"resource": base + "/", "authorization_servers": []string{base + "/"}})
		case "/.well-known/oauth-authorization-server":
			writeJSON(w, http.StatusOK, map[string]any{
				"issuer": base, "authorization_endpoint": base + "/api/oauth2/authorize", "token_endpoint": base + "/oauth2/token",
				"code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"client_secret_post"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base = server.URL

	recorder := httptest.NewRecorder()
	NewHandler(nil, WithHTTPClient(server.Client())).MCPSyncTools(recorder, syncRequest(`{"url":"`+base+`"}`))
	require.Equal(t, http.StatusOK, recorder.Code)
	var result map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &result))
	require.Equal(t, true, result["requires_authorization"], recorder.Body.String())
	resource := result["response_metadata"].(map[string]any)["resource_metadata"].(map[string]any)
	require.Equal(t, base+"/oauth2/token", resource["oauth_authorization_server"].(map[string]any)["token_endpoint"])
}

// A real issuer mismatch on the RFC 8414 document is not the end: the OIDC
// candidate is tried next, and is used when its issuer matches.
func TestMCPAuthorizationServerMetadataTriesTheNextCandidateOnIssuerMismatch(t *testing.T) {
	var base string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server/tenant":
			writeJSON(w, http.StatusOK, map[string]any{"issuer": base + "/someone-else", "authorization_endpoint": base + "/wrong", "token_endpoint": base + "/wrong"})
		case "/tenant/.well-known/openid-configuration":
			writeJSON(w, http.StatusOK, map[string]any{"issuer": base + "/tenant", "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base = server.URL

	metadata, err := NewHandler(nil, WithHTTPClient(server.Client())).mcpAuthorizationServerMetadata(t.Context(), base+"/tenant/")
	require.NoError(t, err)
	require.Equal(t, base+"/token", metadata["token_endpoint"])
}

func TestEquivalentMCPMetadataURL(t *testing.T) {
	for _, same := range [][2]string{
		{"https://mcp.box.com/", "https://mcp.box.com"},
		{"https://api.box.com", "https://api.box.com/"},
		{"https://HOST.example/mcp/", "https://host.example/mcp"},
		{"https://host.example:443/mcp", "https://host.example/mcp"},
	} {
		require.True(t, equivalentMCPMetadataURL(same[0], same[1]), same)
	}
	for _, different := range [][2]string{
		{"https://host.example/mcp", "https://host.example/other"},
		{"https://host.example/mcp", "http://host.example/mcp"},
		{"https://host.example/mcp", "https://host.example:8443/mcp"},
		{"https://host.example/mcp", "https://evil.example/mcp"},
		{"https://host.example/mcp?a=1", "https://host.example/mcp"},
		{"https://host.example/a/mcp", "https://host.example/a"},
		{"not a url", "https://host.example"},
	} {
		require.False(t, equivalentMCPMetadataURL(different[0], different[1]), different)
	}
}

func TestMCPCatalogueOriginMatches(t *testing.T) {
	require.True(t, mcpCatalogueOriginMatches("https://api.githubcopilot.com/mcp/", "https://api.githubcopilot.com/mcp/x"))
	require.True(t, mcpCatalogueOriginMatches("https://mcp.example.com/{tenant}/mcp", "https://MCP.example.com:443/acme/mcp"))
	require.False(t, mcpCatalogueOriginMatches("https://api.githubcopilot.com/mcp/", "https://attacker.example/mcp"))
	require.False(t, mcpCatalogueOriginMatches("https://api.githubcopilot.com/mcp/", "https://api.githubcopilot.com:8443/mcp"))
	require.False(t, mcpCatalogueOriginMatches("", "https://api.githubcopilot.com/mcp"))
}

type fixedResolver map[string][]net.IPAddr

func (r fixedResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if addrs, ok := r[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// Without a WithHTTPClient override, Main's MCP requests dial through the
// egress guard: loopback, cloud metadata and a hostname that resolves to a
// private address are refused before a connection is made.
func TestMCPSyncToolsRefusesInternalDestinationsByDefault(t *testing.T) {
	// A raw listener counts TCP connections, so a refusal is proven to happen
	// before the dial, not at the TLS handshake.
	var hits atomic.Int32
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			_ = conn.Close()
		}
	}()
	loopbackURL := "https://" + listener.Addr().String()
	resolver := fixedResolver{"internal.example": {{IP: net.ParseIP("10.20.30.40")}}}

	for name, target := range map[string]string{
		"loopback https":        loopbackURL + "/mcp",
		"loopback http":         "http://127.0.0.1:1/mcp",
		"instance metadata":     "https://169.254.169.254/latest/meta-data",
		"private via hostname":  "https://internal.example/mcp",
		"loopback via hostname": "https://localhost:1/mcp",
	} {
		t.Run(name, func(t *testing.T) {
			handler := NewHandler(nil, WithMCPEgressGuard(webhook.NewDestinationGuardWithResolver(nil, resolver)))
			recorder := httptest.NewRecorder()
			handler.MCPSyncTools(recorder, syncRequest(`{"url":"`+target+`"}`))
			require.Equal(t, http.StatusOK, recorder.Code)
			var result map[string]any
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &result))
			require.Equal(t, false, result["success"])
			require.Equal(t, "MCP tool discovery failed", result["error"])
		})
	}
	require.Zero(t, hits.Load(), "the guard let a loopback request through")

	// NewHandler with no options gets the same deny-by-default guard.
	recorder := httptest.NewRecorder()
	NewHandler(nil).MCPSyncTools(recorder, syncRequest(`{"url":"`+loopbackURL+`/mcp"}`))
	require.Zero(t, hits.Load(), "the default handler client reached a loopback server")

	// Control: the same listener is reachable when private egress is allowed,
	// so the zero above is the guard's doing.
	allowlist, err := webhook.ParseDestinationAllowlist([]string{"127.0.0.0/8"})
	require.NoError(t, err)
	recorder = httptest.NewRecorder()
	NewHandler(nil, WithMCPEgressGuard(webhook.NewDestinationGuard(allowlist))).MCPSyncTools(recorder, syncRequest(`{"url":"`+loopbackURL+`/mcp"}`))
	require.Eventually(t, func() bool { return hits.Load() > 0 }, time.Second, 10*time.Millisecond)
}
