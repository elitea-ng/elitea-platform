package eliteacore_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	toolkitexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
)

func TestMCPOAuthRefreshNeverExpandsScopeFromToolkitDefaults(t *testing.T) {
	for _, dcr := range []bool{false, true} {
		for _, scope := range []string{"", "records.read"} {
			t.Run(map[bool]string{false: "stored", true: "dcr"}[dcr]+"/"+scope, func(t *testing.T) {
				resolver := &delegatedAuthSettingsStub{found: true, result: toolkitexecutionapp.DelegatedAuthToolkitSettings{
					ToolkitType: "openapi",
					Settings: map[string]any{"openapi_configuration": map[string]any{
						"client_id": "stored-client", "client_secret": "stored-secret",
						"token_url": "https://issuer.example/token", "scopes": []string{"records.read", "offline_access"},
					}},
				}}
				client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					if err := request.ParseForm(); err != nil {
						t.Fatal(err)
					}
					if request.Form.Get("scope") != scope || request.Form.Has("scope") != (scope != "") {
						t.Error("refresh changed the requested scope or filled an omitted scope")
					}
					if request.Form.Get("client_secret") != map[bool]string{false: "stored-secret", true: "dcr-secret"}[dcr] {
						t.Error("refresh lost its bound client credentials")
					}
					return oauthResponse(request, http.StatusOK, `{"access_token":"refreshed"}`), nil
				})}
				handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client), eliteacore.WithDelegatedAuthToolkitSettingsResolver(resolver))
				body := map[string]any{"token_endpoint": "https://issuer.example/token", "grant_type": "refresh_token", "refresh_token": "fixture-refresh", "toolkit_id": 19, "used_dcr": dcr}
				if scope != "" {
					body["scope"] = scope
				}
				if dcr {
					body["client_id"], body["client_secret"] = "dcr-client", "dcr-secret"
				}
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				recorder := httptest.NewRecorder()
				handler.MCPOAuthProxy(recorder, authenticatedOAuthRequest(t, string(encoded)))
				assertStatus(t, recorder, http.StatusOK)
				if dcr && resolver.calls != 0 {
					t.Error("DCR refresh unnecessarily resolved toolkit defaults")
				}
			})
		}
	}
}
