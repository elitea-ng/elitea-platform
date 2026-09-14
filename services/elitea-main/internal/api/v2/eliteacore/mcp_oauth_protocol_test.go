package eliteacore_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	toolkitexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
)

// This component fixture binds a preissued code to S256 and a callback URI.
// It does not simulate browser consent, discovery, or a real identity provider.
func TestMCPOAuthProtocolTLSGrantsAndProtectedOpenAPI(t *testing.T) {
	for _, mode := range []struct {
		name, clientSecret string
		dcr                bool
	}{
		{"stored OpenAPI", "stored-test-secret", false},
		{"public DCR", "", true},
		{"confidential DCR", "issued-test-secret", true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			const clientID = "test-client"
			const callback = "https://elitea.example/mcp-auth-callback"
			const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
			// RFC 7636 Appendix B vector, independent of the proxy implementation.
			const challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
			var mu sync.Mutex
			generation := 0
			registered := !mode.dcr
			issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" {
					t.Error("grant material must use the selected POST-body contract")
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				if r.URL.Path == "/register" {
					var registration map[string]any
					if err := json.NewDecoder(r.Body).Decode(&registration); err != nil {
						t.Error("invalid registration JSON")
						http.Error(w, "invalid request", http.StatusBadRequest)
						return
					}
					expectedMethod := "none"
					if mode.clientSecret != "" {
						expectedMethod = "client_secret_post"
					}
					if r.Header.Get("Content-Type") != "application/json" ||
						registration["token_endpoint_auth_method"] != expectedMethod ||
						fmt.Sprint(registration["redirect_uris"]) != "["+callback+"]" ||
						fmt.Sprint(registration["grant_types"]) != "[authorization_code refresh_token]" ||
						fmt.Sprint(registration["response_types"]) != "[code]" {
						t.Error("DCR fields changed at the provider boundary")
					}
					registered = true
					response := registration
					response["client_id"] = clientID
					if mode.clientSecret != "" {
						response["client_secret"] = mode.clientSecret
						response["client_secret_expires_at"] = 0
					}
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(response)
					return
				}
				if r.URL.Path != "/token" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
					t.Error("unexpected token request route or encoding")
					http.NotFound(w, r)
					return
				}
				if err := r.ParseForm(); err != nil {
					t.Error("invalid token form")
					return
				}
				reject := func(code string) {
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
				}
				if !registered || r.PostForm.Get("client_id") != clientID || r.PostForm.Get("client_secret") != mode.clientSecret {
					reject("invalid_client")
					return
				}
				expectedScope := "records.read"
				if r.PostForm.Get("grant_type") == "refresh_token" && !mode.dcr {
					// This client omits refresh scope: reuse the original grant,
					// rather than restoring potentially broader toolkit defaults.
					expectedScope = ""
					if r.PostForm.Has("scope") {
						t.Error("omitted refresh scope was filled from toolkit defaults")
					}
				}
				if r.PostForm.Get("scope") != expectedScope || r.PostForm.Has("toolkit_id") || r.PostForm.Has("used_dcr") {
					t.Error("scope or platform-only fields changed at the provider boundary")
				}
				switch r.PostForm.Get("grant_type") {
				case "authorization_code":
					digest := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
					if generation != 0 || r.PostForm.Get("code") != "issued-test-code" ||
						r.PostForm.Get("redirect_uri") != callback || base64.RawURLEncoding.EncodeToString(digest[:]) != challenge {
						reject("invalid_grant")
						return
					}
					if r.PostForm.Has("refresh_token") {
						t.Error("refresh credential leaked into the code grant")
					}
				case "refresh_token":
					if generation == 0 || r.PostForm.Get("refresh_token") != fmt.Sprintf("refresh-%d", generation) {
						reject("invalid_grant")
						return
					}
					if r.PostForm.Has("code") || r.PostForm.Has("redirect_uri") || r.PostForm.Has("code_verifier") {
						t.Error("code grant fields leaked into refresh")
					}
				default:
					reject("unsupported_grant_type")
					return
				}
				generation++
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": fmt.Sprintf("access-%d", generation), "refresh_token": fmt.Sprintf("refresh-%d", generation),
					"token_type": "Bearer", "expires_in": 60, "scope": "records.read",
				})
			}))
			defer issuer.Close()

			resolver := &delegatedAuthSettingsStub{found: true, result: toolkitexecutionapp.DelegatedAuthToolkitSettings{
				ToolkitType: "openapi", Settings: map[string]any{"openapi_configuration": map[string]any{
					"client_id": clientID, "client_secret": "stored-test-secret", "token_url": issuer.URL + "/token", "scope": "records.read",
				}},
			}}
			handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(issuer.Client()), eliteacore.WithDelegatedAuthToolkitSettingsResolver(resolver), eliteacore.WithMCPDCRClients(&dcrClientsStub{}))
			call := func(dcr bool, body map[string]any, status int) map[string]any {
				t.Helper()
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				recorder := httptest.NewRecorder()
				if dcr {
					handler.MCPDCRProxy(recorder, authenticatedOAuthRequest(t, string(raw)))
				} else {
					handler.MCPOAuthProxy(recorder, authenticatedOAuthRequest(t, string(raw)))
				}
				assertStatus(t, recorder, status)
				if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Pragma") != "no-cache" {
					t.Error("grant response is cacheable")
				}
				return decodeObj(t, recorder)
			}
			grant := map[string]any{"token_endpoint": issuer.URL + "/token", "toolkit_id": 19,
				"code": "issued-test-code", "redirect_uri": callback, "code_verifier": verifier}
			if mode.dcr {
				method := "none"
				if mode.clientSecret != "" {
					method = "client_secret_post"
				}
				registered := call(true, map[string]any{"registration_endpoint": issuer.URL + "/register",
					"token_endpoint": issuer.URL + "/token",
					"redirect_uris":  []string{callback}, "token_endpoint_auth_method": method}, http.StatusOK)
				grant["client_id"] = registered["client_id"]
				if secret, present := registered["client_secret"]; present {
					t.Fatalf("DCR secret crossed the browser boundary: %T", secret)
				}
				if reference, present := registered["client_reference"]; present {
					grant["client_reference"] = reference
				}
				grant["used_dcr"], grant["scope"] = true, "records.read"
			}
			for _, field := range []string{"client_id", "client_secret"} {
				original, present := grant[field]
				grant[field] = "different-client-credential"
				response := call(false, grant, http.StatusBadRequest)
				if grant["client_reference"] != nil {
					expected := "invalid_client"
					if field == "client_secret" {
						expected = "invalid_dcr_client_reference"
					}
					if response["error"] != expected {
						t.Error("client reference rejection was not preserved")
					}
				} else if response["error_description"] != "invalid_client" {
					t.Error("provider client rejection was not preserved")
				}
				if present {
					grant[field] = original
				} else {
					delete(grant, field)
				}
			}
			grant["code_verifier"] = strings.Repeat("x", 43)
			if response := call(false, grant, http.StatusBadRequest); response["error_description"] != "invalid_grant" {
				t.Error("PKCE rejection was not preserved")
			}
			grant["code_verifier"] = verifier
			grant["redirect_uri"] = "https://other.example/callback"
			call(false, grant, http.StatusBadRequest)
			grant["redirect_uri"] = callback
			tokens := call(false, grant, http.StatusOK)
			if tokens["access_token"] != "access-1" || tokens["refresh_token"] != "refresh-1" {
				t.Fatal("code exchange did not return the issued tokens")
			}
			call(false, grant, http.StatusBadRequest) // Authorization codes are single-use.

			resource := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method != http.MethodGet || r.URL.Path != "/records" || r.URL.RawQuery != "" || r.ContentLength > 0 {
					t.Error("protected request carried unexpected fields")
				}
				if r.Header.Get("Authorization") != fmt.Sprintf("Bearer access-%d", generation) {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				_, _ = io.WriteString(w, `{"records":[{"id":1}]}`)
			}))
			defer resource.Close()
			readRecords := func(token string, status int) {
				t.Helper()
				request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, resource.URL+"/records", nil)
				if err != nil {
					t.Fatal(err)
				}
				if token != "" {
					request.Header.Set("Authorization", "Bearer "+token)
				}
				response, err := resource.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := response.Body.Close(); err != nil {
						t.Error(err)
					}
				}()
				if response.StatusCode != status {
					t.Fatalf("protected resource status = %d, want %d", response.StatusCode, status)
				}
			}
			readRecords("", http.StatusUnauthorized)
			readRecords(tokens["access_token"].(string), http.StatusOK)
			grant["grant_type"], grant["refresh_token"] = "refresh_token", tokens["refresh_token"]
			rotated := call(false, grant, http.StatusOK)
			if rotated["access_token"] != "access-2" || rotated["refresh_token"] != "refresh-2" {
				t.Fatal("refresh rotation did not reach the client")
			}
			call(false, grant, http.StatusBadRequest) // Old refresh credential was consumed.
			readRecords(rotated["access_token"].(string), http.StatusOK)
			if mode.dcr && resolver.calls != 0 {
				t.Error("DCR loaded a stored OpenAPI client credential")
			}
			if !mode.dcr && (resolver.calls == 0 || resolver.projectID != 7 || resolver.actorID != 11 || resolver.toolkitID != 19) {
				t.Error("stored credential lookup lost project, actor, or toolkit identity")
			}
		})
	}
}

func TestMCPOAuthProtocolDoesNotForwardCredentialsAcrossOriginRedirect(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationCalls.Add(1)
		_, _ = io.WriteString(w, `{"access_token":"unexpected"}`)
	}))
	defer destination.Close()
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, destination.URL+"/collect", status)
			}))
			defer issuer.Close()
			handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(issuer.Client()))
			recorder := httptest.NewRecorder()
			handler.MCPOAuthProxy(recorder, authenticatedOAuthRequest(t, fmt.Sprintf(
				`{"token_endpoint":%q,"client_id":"client","client_secret":"test-secret","code":"test-code","redirect_uri":"https://elitea.example/callback"}`, issuer.URL+"/token")))
			assertStatus(t, recorder, http.StatusBadGateway)
			if destinationCalls.Load() != 0 {
				t.Error("cross-origin redirect reached a different credential server")
			}
		})
	}
}

func TestMCPOAuthProtocolCancellationReachesProviderRequest(t *testing.T) {
	var called bool
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		called = true
		if request.Context().Err() != context.Canceled {
			t.Error("token exchange detached from the cancelled request")
		}
		return nil, request.Context().Err()
	})}
	handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client))
	request := authenticatedOAuthRequest(t, `{"token_endpoint":"https://identity.example/token","client_id":"public","code":"test-code","redirect_uri":"https://elitea.example/callback"}`)
	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	recorder := httptest.NewRecorder()
	handler.MCPOAuthProxy(recorder, request.WithContext(ctx))
	assertStatus(t, recorder, http.StatusBadGateway)
	if !called {
		t.Fatal("cancellation check did not reach the transport")
	}
}
