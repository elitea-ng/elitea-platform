package eliteacore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMCPSyncToolsUsesOnlyTheBoundAccessToken(t *testing.T) {
	for _, authorized := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrelated token", true: "bound token"}[authorized], func(t *testing.T) {
			var seen []string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = append(seen, r.Header.Get("Authorization"))
				if r.Header.Get("Authorization") != "Bearer bound-access" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				var rpc struct {
					ID     int    `json:"id"`
					Method string `json:"method"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&rpc))
				if rpc.Method == "notifications/initialized" {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": rpc.ID,
					"result": map[string]any{"tools": []any{map[string]any{"name": "echo_marker", "inputSchema": map[string]any{"type": "object"}}}}})
			}))
			defer server.Close()
			tokens := map[string]any{"https://unrelated.invalid/mcp": map[string]any{"access_token": "unrelated-secret"}}
			if authorized {
				tokens[server.URL+"/mcp"] = map[string]any{"access_token": "bound-access", "refresh_token": "never-forward"}
			}
			body, err := json.Marshal(map[string]any{"url": server.URL + "/mcp", "mcp_tokens": tokens})
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			NewHandler(nil, WithHTTPClient(server.Client())).MCPSyncTools(recorder, syncRequest(string(body)))
			require.Equal(t, http.StatusOK, recorder.Code)
			var result map[string]any
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &result))
			require.Equal(t, authorized, result["success"])
			for _, header := range seen {
				if authorized {
					require.Equal(t, "Bearer bound-access", header)
				} else {
					require.Empty(t, header)
				}
			}
			require.NotContains(t, recorder.Body.String(), "unrelated-secret")
			require.NotContains(t, recorder.Body.String(), "never-forward")
		})
	}
}

func TestMCPSyncToolsReturnsBoundAuthorizationMetadata(t *testing.T) {
	var base string
	var metadataRequests int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mcp":
			w.Header().Set("WWW-Authenticate", `Bearer realm="tools, public", resource_metadata="`+base+`/resource"`)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("provider body must remain private"))
		case "/resource", "/.well-known/oauth-authorization-server/issuer":
			metadataRequests++
			require.Empty(t, r.Header.Get("Authorization"))
			require.Empty(t, r.Header.Get("X-Private-Header"))
			if r.URL.Path == "/resource" {
				writeJSON(w, http.StatusOK, map[string]any{"resource": base + "/mcp", "authorization_servers": []string{base + "/issuer"}, "scopes_supported": []string{"records.read"}, "private_extension": "do-not-project"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"issuer": base + "/issuer", "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token", "registration_endpoint": base + "/register", "code_challenge_methods_supported": []string{"S256"}, "private_extension": "do-not-project"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base = server.URL
	body, err := json.Marshal(map[string]any{"url": base + "/mcp", "headers": map[string]string{"X-Private-Header": "private-value"}})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	NewHandler(nil, WithHTTPClient(server.Client())).MCPSyncTools(recorder, syncRequest(string(body)))
	require.Equal(t, http.StatusOK, recorder.Code)
	var result map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &result))
	require.Equal(t, true, result["requires_authorization"])
	require.Equal(t, 2, metadataRequests)
	metadata := result["response_metadata"].(map[string]any)
	require.Equal(t, base+"/mcp", metadata["server_url"])
	resource := metadata["resource_metadata"].(map[string]any)
	require.Equal(t, []any{base + "/issuer"}, resource["authorization_servers"])
	require.Equal(t, base+"/register", resource["oauth_authorization_server"].(map[string]any)["registration_endpoint"])
	for _, private := range []string{"provider body", "private_extension", "do-not-project", "private-value"} {
		require.NotContains(t, recorder.Body.String(), private)
	}
}

func TestMCPSyncToolsBoundsTheRequestBody(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewHandler(nil).MCPSyncTools(recorder, syncRequest(`{"url":"https://mcp.invalid","padding":"`+strings.Repeat("x", 1<<20)+`"}`))
	require.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code)
}

func TestMCPSyncToolsRejectsUnboundOrInvalidMetadata(t *testing.T) {
	for _, failure := range []string{"resource mismatch", "issuer mismatch", "oversized", "redirect", "invalid endpoint"} {
		t.Run(failure, func(t *testing.T) {
			redirectTarget := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("cross-origin metadata redirect was followed")
			}))
			defer redirectTarget.Close()
			var base string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/mcp":
					w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/resource"`)
					w.WriteHeader(http.StatusUnauthorized)
				case "/resource":
					if failure == "redirect" {
						http.Redirect(w, r, redirectTarget.URL+"/must-not-follow", http.StatusFound)
						return
					}
					if failure == "oversized" {
						writeJSON(w, http.StatusOK, map[string]any{"padding": strings.Repeat("x", 1<<17)})
						return
					}
					resource := base + "/mcp"
					if failure == "resource mismatch" {
						resource = base + "/another-resource"
					}
					writeJSON(w, http.StatusOK, map[string]any{"resource": resource, "authorization_servers": []string{base + "/issuer"}})
				case "/.well-known/oauth-authorization-server/issuer":
					issuer, tokenEndpoint := base+"/issuer", base+"/token"
					if failure == "issuer mismatch" {
						issuer = base + "/other-issuer"
					}
					if failure == "invalid endpoint" {
						tokenEndpoint = base + "/token#fragment"
					}
					writeJSON(w, http.StatusOK, map[string]any{"issuer": issuer, "authorization_endpoint": base + "/authorize", "token_endpoint": tokenEndpoint})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			base = server.URL
			recorder := httptest.NewRecorder()
			NewHandler(nil, WithHTTPClient(server.Client())).MCPSyncTools(recorder, syncRequest(`{"url":"`+base+`/mcp"}`))
			require.Equal(t, http.StatusOK, recorder.Code)
			var result map[string]any
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &result))
			require.Equal(t, false, result["success"])
			require.NotContains(t, result, "requires_authorization")
			require.NotContains(t, result, "response_metadata")
		})
	}
}

func TestMCPDiscoveryHeadersReplaceCaseVariantsWithoutMutatingInput(t *testing.T) {
	headers := map[string]string{"authorization": "old", "MCP-SESSION-ID": "old-session", "X-Private": "keep"}
	tokens := map[string]mcpDiscoveryToken{
		"https://example.test/mcp": {AccessToken: "bound", SessionID: "session"},
		"mcp_prebuilt_test":        {AccessToken: "alias"},
	}
	result := mcpDiscoveryHeaders(headers, tokens, "https://example.test/mcp", "mcp_prebuilt_test")
	require.Equal(t, map[string]string{"Authorization": "Bearer bound", "Mcp-Session-Id": "session", "X-Private": "keep"}, result)
	require.Equal(t, "old", headers["authorization"])
	require.Equal(t, "Bearer alias", mcpDiscoveryHeaders(nil, tokens, "https://other.test/mcp", "mcp_prebuilt_test")["Authorization"])
	require.Empty(t, mcpDiscoveryHeaders(nil, tokens, "https://other.test/mcp", "mcp")["Authorization"])
}
