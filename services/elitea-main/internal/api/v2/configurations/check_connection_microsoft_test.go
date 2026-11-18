package configurations

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The delegated check of ADR-0057, for all five Microsoft types: the 401
// `requires_authorization` answer, the identity read when a token comes with
// the check, and the refusals (non-Entra authority, hostile discovery document,
// bad Dataverse host). SharePoint's own cases stay in check_connection_oauth_test.go.

const (
	entraTenant    = "https://login.microsoftonline.com/contoso.onmicrosoft.com"
	dataverseOrg   = "https://org.crm4.dynamics.com"
	plantedToken   = "eyJ-planted-access-token-for-the-test"
	plantedSecret  = "planted-client-secret-for-the-test"
	discoverySuffx = "/v2.0/.well-known/openid-configuration"
)

// entraDiscovery is the document login.microsoftonline.com publishes for the
// tenant, with the endpoint fields a test can corrupt.
func entraDiscovery(tenant, authorizationEndpoint, tokenEndpoint string) map[string]string {
	return map[string]string{tenant + discoverySuffx: `{
		"issuer": "` + tenant + `/v2.0",
		"authorization_endpoint": "` + authorizationEndpoint + `",
		"token_endpoint": "` + tokenEndpoint + `"
	}`}
}

func goodEntraDiscovery() map[string]string {
	return entraDiscovery(entraTenant, entraTenant+"/oauth2/v2.0/authorize", entraTenant+"/oauth2/v2.0/token")
}

type microsoftCase struct {
	configType string
	body       map[string]any // the credential's data, without a token
	scopeWant  []string       // scopes_supported
	resource   string
	probeURL   string
}

func microsoftCases() []microsoftCase {
	custom := func(extra map[string]any) map[string]any {
		data := map[string]any{
			"client_id": "app-id", "client_secret": plantedSecret, "oauth_discovery_endpoint": entraTenant,
		}
		for key, value := range extra {
			data[key] = value
		}
		return data
	}
	return []microsoftCase{
		{"outlook", custom(map[string]any{"scopes": []string{"Mail.Send", "Mail.Read"}}),
			[]string{"offline_access", "Mail.Read", "Mail.Send"}, "https://graph.microsoft.com",
			"https://graph.microsoft.com/v1.0/me?$select=id"},
		{"teams", custom(map[string]any{"scopes": []string{"Chat.Read"}}),
			[]string{"offline_access", "Chat.Read"}, "https://graph.microsoft.com",
			"https://graph.microsoft.com/v1.0/me?$select=id"},
		{"ms_graph", custom(map[string]any{"scopes": "Mail.Read https://graph.microsoft.com/Files.Read"}),
			[]string{"offline_access", "Mail.Read", "Files.Read"}, "https://graph.microsoft.com",
			"https://graph.microsoft.com/v1.0/me?$select=id"},
		{"dataverse", map[string]any{
			"url": dataverseOrg, "tenant_id": "contoso.onmicrosoft.com", "client_id": "app-id", "client_secret": plantedSecret,
		}, []string{"offline_access", dataverseOrg + "/user_impersonation"}, dataverseOrg,
			dataverseOrg + "/api/data/v9.2/WhoAmI"},
	}
}

func withToken(data map[string]any) map[string]any {
	copied := map[string]any{}
	for key, value := range data {
		copied[key] = value
	}
	copied["access_token"] = plantedToken
	return copied
}

func encode(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// probeRecorder is the guarded client the identity read goes through.
type probeRecorder struct {
	status   int
	err      error
	requests []*http.Request
}

func (p *probeRecorder) do(request *http.Request) (*http.Response, error) {
	p.requests = append(p.requests, request)
	if p.err != nil {
		return nil, p.err
	}
	return &http.Response{StatusCode: p.status, Body: io.NopCloser(strings.NewReader(`{"id":"x"}`))}, nil
}

type checkBody struct {
	Success               bool   `json:"success"`
	Reason                string `json:"reason"`
	Message               string `json:"message"`
	RequiresAuthorization bool   `json:"requires_authorization"`
	AuthMetadata          struct {
		ServerURL        string `json:"server_url"`
		ResourceMetadata struct {
			Resource      string         `json:"resource"`
			ResourceName  string         `json:"resource_name"`
			Servers       []string       `json:"authorization_servers"`
			AuthServer    map[string]any `json:"oauth_authorization_server"`
			ScopesSupport []string       `json:"scopes_supported"`
		} `json:"resource_metadata"`
	} `json:"auth_metadata"`
}

func decodeCheck(t *testing.T, raw []byte) checkBody {
	t.Helper()
	var body checkBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("not JSON: %v: %s", err, raw)
	}
	return body
}

func newMicrosoftHandler(served map[string]string, probe *probeRecorder) (*Handler, *[]string) {
	var requested []string
	handler := NewHandler(nil)
	handler.oauthDiscovery = discoveryFetcher(served, &requested)
	if probe != nil {
		handler.microsoftProbe = probe.do
	}
	return handler, &requested
}

func TestMicrosoftTypesWithNoTokenAnswerTheSignInRequest(t *testing.T) {
	for _, tc := range microsoftCases() {
		t.Run(tc.configType, func(t *testing.T) {
			handler, _ := newMicrosoftHandler(goodEntraDiscovery(), nil)
			recorder := postDelegatedCheck(t, handler, tc.configType, encode(t, tc.body))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (%s)", recorder.Code, recorder.Body.String())
			}
			body := decodeCheck(t, recorder.Body.Bytes())
			metadata := body.AuthMetadata.ResourceMetadata
			if !body.RequiresAuthorization || body.AuthMetadata.ServerURL != entraTenant ||
				metadata.Resource != tc.resource || metadata.ResourceName == "" ||
				len(metadata.Servers) != 1 || metadata.Servers[0] != entraTenant+"/v2.0" ||
				metadata.AuthServer["authorization_endpoint"] != entraTenant+"/oauth2/v2.0/authorize" ||
				metadata.AuthServer["token_endpoint"] != entraTenant+"/oauth2/v2.0/token" {
				t.Fatalf("body = %s", recorder.Body.String())
			}
			if strings.Join(metadata.ScopesSupport, " ") != strings.Join(tc.scopeWant, " ") {
				t.Errorf("scopes_supported = %v, want %v", metadata.ScopesSupport, tc.scopeWant)
			}
			if strings.Contains(recorder.Body.String(), plantedSecret) {
				t.Error("the client secret came back in the answer")
			}
		})
	}
}

func TestMicrosoftTypesWithAValidTokenReadTheIdentity(t *testing.T) {
	for _, tc := range microsoftCases() {
		t.Run(tc.configType, func(t *testing.T) {
			probe := &probeRecorder{status: http.StatusOK}
			handler, requested := newMicrosoftHandler(goodEntraDiscovery(), probe)
			recorder := postDelegatedCheck(t, handler, tc.configType, encode(t, withToken(tc.body)))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
			}
			if body := decodeCheck(t, recorder.Body.Bytes()); !body.Success || body.Reason != ToolkitCheckReasonOK {
				t.Fatalf("body = %s", recorder.Body.String())
			}
			if len(probe.requests) != 1 || probe.requests[0].URL.String() != tc.probeURL ||
				probe.requests[0].Method != http.MethodGet ||
				probe.requests[0].Header.Get("Authorization") != "Bearer "+plantedToken {
				t.Fatalf("probe = %+v", probe.requests)
			}
			if len(*requested) != 0 {
				t.Errorf("a successful identity read fetched discovery documents: %v", *requested)
			}
			if strings.Contains(recorder.Body.String(), plantedToken) {
				t.Error("the token came back in the answer")
			}
		})
	}
}

func TestMicrosoftTypesWithARejectedTokenAskForASignInAgain(t *testing.T) {
	for _, tc := range microsoftCases() {
		t.Run(tc.configType, func(t *testing.T) {
			probe := &probeRecorder{status: http.StatusUnauthorized}
			handler, _ := newMicrosoftHandler(goodEntraDiscovery(), probe)
			recorder := postDelegatedCheck(t, handler, tc.configType, encode(t, withToken(tc.body)))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
			}
			if body := decodeCheck(t, recorder.Body.Bytes()); !body.RequiresAuthorization || body.AuthMetadata.ServerURL != entraTenant {
				t.Fatalf("body = %s", recorder.Body.String())
			}
		})
	}
}

func TestMicrosoftIdentityReadFailuresAreReportedNotAsSuccess(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		err        error
		wantReason string
		wantText   string
	}{
		{"403 names the missing permission", http.StatusForbidden, nil, ToolkitCheckReasonAuthFailed, "tick it in the credential scopes"},
		{"429", http.StatusTooManyRequests, nil, ToolkitCheckReasonUnreachable, ""},
		{"503", http.StatusServiceUnavailable, nil, ToolkitCheckReasonUnreachable, ""},
		{"404", http.StatusNotFound, nil, ToolkitCheckReasonUnreachable, ""},
		{"transport failure", 0, errors.New("dial tcp 10.0.0.1: " + plantedToken), ToolkitCheckReasonUnreachable, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := &probeRecorder{status: tc.status, err: tc.err}
			handler, _ := newMicrosoftHandler(goodEntraDiscovery(), probe)
			recorder := postDelegatedCheck(t, handler, "outlook", encode(t, withToken(microsoftCases()[0].body)))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
			}
			body := decodeCheck(t, recorder.Body.Bytes())
			if body.Success || body.Reason != tc.wantReason || !strings.Contains(body.Message, tc.wantText) {
				t.Fatalf("body = %s", recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), plantedToken) || strings.Contains(recorder.Body.String(), "10.0.0.1") {
				t.Error("an error detail or the token came back in the answer")
			}
		})
	}
}

func TestMicrosoftIdentityReadIsRefusedWithoutAGuardedClient(t *testing.T) {
	handler, _ := newMicrosoftHandler(goodEntraDiscovery(), nil) // no guard composed
	recorder := postDelegatedCheck(t, handler, "teams", encode(t, withToken(microsoftCases()[1].body)))
	if recorder.Code != http.StatusBadRequest || decodeCheck(t, recorder.Body.Bytes()).Success {
		t.Fatalf("status = %d (%s), want a refusal, never an unguarded read", recorder.Code, recorder.Body.String())
	}
}

func TestMicrosoftTypesRefuseAnAuthorityThatIsNotEntra(t *testing.T) {
	endpoints := []string{
		"https://login.example.test/contoso", "http://login.microsoftonline.com/contoso",
		"https://login.microsoftonline.com.evil.test/contoso", "https://login.microsoftonline.com@evil.test/contoso",
		"https://login.microsoftonline.us/contoso", "https://login.microsoftonline.com:8443/contoso",
		"https://login.microsoftonline.com/contoso/extra", "https://127.0.0.1/contoso",
	}
	for _, configType := range []string{"outlook", "teams", "ms_graph"} {
		for _, endpoint := range endpoints {
			probe := &probeRecorder{status: http.StatusOK}
			handler, requested := newMicrosoftHandler(map[string]string{}, probe)
			data := map[string]any{
				"client_id": "a", "client_secret": plantedSecret, "oauth_discovery_endpoint": endpoint, "access_token": plantedToken,
			}
			recorder := postDelegatedCheck(t, handler, configType, encode(t, data))
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "toolkit.oauth_discovery_failed") ||
				!strings.Contains(recorder.Body.String(), "login.microsoftonline.com") {
				t.Errorf("%s %q: status = %d body = %s", configType, endpoint, recorder.Code, recorder.Body.String())
			}
			if len(*requested) != 0 || len(probe.requests) != 0 {
				t.Errorf("%s %q: the check reached out (%v, %d probes)", configType, endpoint, *requested, len(probe.requests))
			}
		}
	}
}

func TestMicrosoftTypesNeedAnAuthority(t *testing.T) {
	handler, _ := newMicrosoftHandler(goodEntraDiscovery(), nil)
	recorder := postDelegatedCheck(t, handler, "outlook", `{"client_id":"a","client_secret":"b"}`)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "OAuth discovery endpoint") {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestADefaultAppReferenceIsNotFollowedWhileThereIsNoRegistry(t *testing.T) {
	handler, requested := newMicrosoftHandler(goodEntraDiscovery(), &probeRecorder{status: http.StatusOK})
	recorder := postDelegatedCheck(t, handler, "outlook", `{"oauth_app_id":"7f8d0c1e-0000-4000-8000-000000000000"}`)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "Default apps are not available") {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if len(*requested) != 0 {
		t.Errorf("fetched %v", *requested)
	}
}

func TestMicrosoftTypesRefuseAHostileDiscoveryDocument(t *testing.T) {
	const evil = "https://evil.example.test"
	documents := map[string]map[string]string{
		"authorization endpoint on another host": entraDiscovery(entraTenant, evil+"/authorize", entraTenant+"/oauth2/v2.0/token"),
		"authorization endpoint over http":       entraDiscovery(entraTenant, "http://login.microsoftonline.com/authorize", entraTenant+"/oauth2/v2.0/token"),
		"authorization endpoint with user info":  entraDiscovery(entraTenant, "https://login.microsoftonline.com@evil.example.test/a", entraTenant+"/oauth2/v2.0/token"),
		"no authorization endpoint":              entraDiscovery(entraTenant, "", entraTenant+"/oauth2/v2.0/token"),
		"issuer of another tenant": {entraTenant + discoverySuffx: `{"issuer":"https://login.microsoftonline.com/other/v2.0",
			"authorization_endpoint":"` + entraTenant + `/oauth2/v2.0/authorize","token_endpoint":"` + entraTenant + `/oauth2/v2.0/token"}`},
		"issuer on another host": {entraTenant + discoverySuffx: `{"issuer":"` + evil + `/contoso.onmicrosoft.com/v2.0",
			"authorization_endpoint":"` + entraTenant + `/oauth2/v2.0/authorize"}`},
		"not JSON": {entraTenant + discoverySuffx: `<html>`},
		"nothing":  {},
	}
	for name, served := range documents {
		t.Run(name, func(t *testing.T) {
			handler, _ := newMicrosoftHandler(served, nil)
			recorder := postDelegatedCheck(t, handler, "outlook", encode(t, microsoftCases()[0].body))
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "toolkit.oauth_discovery_failed") {
				t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

// The token endpoint the browser is handed is computed, never the document's own:
// the platform posts a client secret to it.
func TestTheTokenEndpointServedIsAlwaysTheComputedOne(t *testing.T) {
	handler, _ := newMicrosoftHandler(entraDiscovery(entraTenant, entraTenant+"/oauth2/v2.0/authorize", "https://evil.example.test/token"), nil)
	recorder := postDelegatedCheck(t, handler, "outlook", encode(t, microsoftCases()[0].body))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	body := decodeCheck(t, recorder.Body.Bytes())
	if got := body.AuthMetadata.ResourceMetadata.AuthServer["token_endpoint"]; got != entraTenant+"/oauth2/v2.0/token" {
		t.Fatalf("token_endpoint = %v", got)
	}
	if strings.Contains(recorder.Body.String(), "evil.example.test") {
		t.Error("the document's own token endpoint was served")
	}
}

func TestDataverseRefusesABadHostAndNeverSendsTheTokenThere(t *testing.T) {
	hosts := []string{
		"https://evil.example.test", "http://org.crm4.dynamics.com", "https://org.crm4.dynamics.cn",
		"https://org.crm4.dynamics.com.evil.example.test", "https://org.crm4.dynamics.com:8443",
		"https://org.crm4.dynamics.com/api", "https://user@org.crm4.dynamics.com", "https://127.0.0.1", "",
	}
	for _, host := range hosts {
		probe := &probeRecorder{status: http.StatusOK}
		handler, requested := newMicrosoftHandler(goodEntraDiscovery(), probe)
		data := map[string]any{
			"url": host, "tenant_id": "contoso.onmicrosoft.com", "client_id": "a", "client_secret": plantedSecret,
			"access_token": plantedToken,
		}
		recorder := postDelegatedCheck(t, handler, "dataverse", encode(t, data))
		if recorder.Code != http.StatusBadRequest || decodeCheck(t, recorder.Body.Bytes()).Success ||
			!strings.Contains(recorder.Body.String(), "Dataverse URL") {
			t.Errorf("%q: status = %d body = %s", host, recorder.Code, recorder.Body.String())
		}
		if len(probe.requests) != 0 || len(*requested) != 0 {
			t.Errorf("%q: the check reached out (%d probes, %v)", host, len(probe.requests), *requested)
		}
	}
}

func TestDataverseRefusesABadTenant(t *testing.T) {
	for _, tenant := range []string{"", "a/b", "evil.test/x", "a b", "../x"} {
		handler, requested := newMicrosoftHandler(goodEntraDiscovery(), nil)
		data := map[string]any{"url": dataverseOrg, "tenant_id": tenant, "client_id": "a", "client_secret": "b"}
		recorder := postDelegatedCheck(t, handler, "dataverse", encode(t, data))
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "tenant_id") || len(*requested) != 0 {
			t.Errorf("%q: status = %d body = %s requested %v", tenant, recorder.Code, recorder.Body.String(), *requested)
		}
	}
}

func TestDataverseBuildsItsAuthorityFromTheTenantNotAnEndpoint(t *testing.T) {
	handler, requested := newMicrosoftHandler(goodEntraDiscovery(), nil)
	data := map[string]any{
		"url": dataverseOrg, "tenant_id": "contoso.onmicrosoft.com", "client_id": "a", "client_secret": "b",
		"oauth_discovery_endpoint": "https://evil.example.test/contoso",
	}
	recorder := postDelegatedCheck(t, handler, "dataverse", encode(t, data))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	for _, url := range *requested {
		if !strings.HasPrefix(url, "https://login.microsoftonline.com/") {
			t.Errorf("fetched %s", url)
		}
	}
}

func TestAMicrosoftScopeListThatCannotBeNormalizedIsRefused(t *testing.T) {
	handler, _ := newMicrosoftHandler(goodEntraDiscovery(), nil)
	for _, scopes := range []any{
		[]string{"https://outlook.office.com/Mail.Read"}, []string{".default", "Mail.Read"}, "not-a-permission",
	} {
		data := map[string]any{"client_id": "a", "client_secret": "b", "oauth_discovery_endpoint": entraTenant, "scopes": scopes}
		recorder := postDelegatedCheck(t, handler, "ms_graph", encode(t, data))
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "Scopes:") {
			t.Errorf("%v: status = %d body = %s", scopes, recorder.Code, recorder.Body.String())
		}
	}
}

func TestMicrosoftTokenRefreshOffDropsOfflineAccess(t *testing.T) {
	handler, _ := newMicrosoftHandler(goodEntraDiscovery(), nil)
	data := microsoftCases()[0].body
	data["auto_refresh_token"] = false
	recorder := postDelegatedCheck(t, handler, "outlook", encode(t, data))
	body := decodeCheck(t, recorder.Body.Bytes())
	if got := strings.Join(body.AuthMetadata.ResourceMetadata.ScopesSupport, " "); got != "Mail.Read Mail.Send" {
		t.Fatalf("scopes_supported = %q", got)
	}
}

// SharePoint, the fifth descriptor, is unchanged: any endpoint the guard lets
// through, the scopes as stored, and no identity read here.
func TestSharePointIsUnchangedByTheDescriptors(t *testing.T) {
	const tenant = "https://login.example.test/tenant-1"
	probe := &probeRecorder{status: http.StatusOK}
	handler, _ := newMicrosoftHandler(entraDiscovery(tenant, tenant+"/oauth2/v2.0/authorize", tenant+"/oauth2/v2.0/token"), probe)
	data := map[string]any{
		"oauth_discovery_endpoint": tenant, "scopes": []string{"Sites.Read.All"}, "access_token": plantedToken,
	}
	recorder := postDelegatedCheck(t, handler, "sharepoint", encode(t, data))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	body := decodeCheck(t, recorder.Body.Bytes())
	if !body.RequiresAuthorization || body.AuthMetadata.ServerURL != tenant ||
		body.AuthMetadata.ResourceMetadata.Resource != tenant ||
		strings.Join(body.AuthMetadata.ResourceMetadata.ScopesSupport, ",") != "Sites.Read.All" ||
		body.AuthMetadata.ResourceMetadata.ResourceName != "" {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	if len(probe.requests) != 0 {
		t.Error("SharePoint has no identity read in the check")
	}
}

// A token that is not Microsoft's own type's concern: other credential types
// carrying an oauth_discovery_endpoint are no longer treated as delegated.
func TestOnlyDescribedTypesAreDelegated(t *testing.T) {
	handler, requested := newMicrosoftHandler(goodEntraDiscovery(), nil)
	recorder := postDelegatedCheck(t, handler, "github", `{"oauth_discovery_endpoint":"`+entraTenant+`"}`)
	if recorder.Code == http.StatusUnauthorized || len(*requested) != 0 {
		t.Fatalf("github answered %d and fetched %v", recorder.Code, *requested)
	}
}
