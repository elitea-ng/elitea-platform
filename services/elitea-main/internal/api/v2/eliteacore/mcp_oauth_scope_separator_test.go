package eliteacore_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	toolkitexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
)

// #2147: a SharePoint credential stores its scopes as "User.Read, Chat.Read".
// The token request must carry them space-separated; Entra reads `User.Read,`
// as a scope of that name and refuses the sign-in with AADSTS650053.
func TestMCPOAuthProxyTokenRequestScopeIsSpaceSeparated(t *testing.T) {
	cases := []struct {
		name   string
		stored string
		sent   string
		want   string
	}{
		{name: "stored comma list", stored: "User.Read, Chat.Read,Sites.ReadWrite.All", want: "User.Read Chat.Read Sites.ReadWrite.All"},
		{name: "caller-supplied comma list", sent: "User.Read, Chat.Read", want: "User.Read Chat.Read"},
		{name: "already space separated", stored: "User.Read Chat.Read", want: "User.Read Chat.Read"},
		{name: "trailing comma", stored: "User.Read,Chat.Read,", want: "User.Read Chat.Read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := map[string]any{"client_id": "stored-client", "client_secret": "stored-secret"}
			if tc.stored != "" {
				settings["scopes"] = tc.stored
			}
			resolver := &delegatedAuthSettingsStub{found: true, result: toolkitexecutionapp.DelegatedAuthToolkitSettings{
				ToolkitType: "sharepoint",
				Settings:    map[string]any{"sharepoint_configuration": settings},
			}}
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if err := request.ParseForm(); err != nil {
					t.Fatal(err)
				}
				if got := request.Form.Get("scope"); got != tc.want {
					t.Errorf("token request scope = %q, want %q", got, tc.want)
				}
				return oauthResponse(request, http.StatusOK, `{"access_token":"access"}`), nil
			})}
			handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client), eliteacore.WithDelegatedAuthToolkitSettingsResolver(resolver))
			body := `{"token_endpoint":"https://identity.example/token","code":"code","redirect_uri":"https://elitea.example/mcp-auth-callback","client_id":"dcr-client","client_secret":"dcr-secret","used_dcr":true,"toolkit_id":19`
			if tc.sent != "" {
				body += `,"scope":"` + tc.sent + `"`
			}
			recorder := httptest.NewRecorder()
			handler.MCPOAuthProxy(recorder, authenticatedOAuthRequest(t, body+`}`))
			assertStatus(t, recorder, http.StatusOK)
		})
	}
}
