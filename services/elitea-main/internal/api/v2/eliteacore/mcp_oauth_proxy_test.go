package eliteacore_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	toolkitexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type delegatedAuthSettingsStub struct {
	result    toolkitexecutionapp.DelegatedAuthToolkitSettings
	found     bool
	err       error
	calls     int
	projectID int32
	actorID   int32
	toolkitID int32
}

func (stub *delegatedAuthSettingsStub) ResolveDelegatedAuthToolkitSettings(
	_ context.Context,
	projectID int32,
	actorID int32,
	toolkitID int32,
) (toolkitexecutionapp.DelegatedAuthToolkitSettings, bool, error) {
	stub.calls++
	stub.projectID = projectID
	stub.actorID = actorID
	stub.toolkitID = toolkitID
	return stub.result, stub.found, stub.err
}

func authenticatedOAuthRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", "7")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{UserID: "11"}))
	return request
}

func oauthResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func TestMCPOAuthProxyResolvesStoredOpenAPISettingsAtBoundEndpoint(t *testing.T) {
	resolver := &delegatedAuthSettingsStub{
		found: true,
		result: toolkitexecutionapp.DelegatedAuthToolkitSettings{
			ToolkitType: "openapi",
			Settings: map[string]any{
				"openapi_configuration": map[string]any{
					"client_id":                "stored-client",
					"client_secret":            "stored-secret",
					"oauth_discovery_endpoint": "https://identity.example/tenant",
					"scope":                    "records.read records.write",
				},
			},
		},
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if request.Form.Get("client_id") != "stored-client" ||
			request.Form.Get("client_secret") != "stored-secret" ||
			request.Form.Get("scope") != "records.read records.write" {
			t.Fatalf("unexpected token form: %#v", request.Form)
		}
		return oauthResponse(request, http.StatusOK,
			`{"access_token":"access","refresh_token":"refresh","client_secret":"must-not-return"}`), nil
	})}
	handler := eliteacore.NewHandler(
		nil,
		eliteacore.WithHTTPClient(client),
		eliteacore.WithDelegatedAuthToolkitSettingsResolver(resolver),
	)
	request := authenticatedOAuthRequest(t, `{
		"token_endpoint":"https://identity.example/tenant/oauth2/v2.0/token",
		"code":"code",
		"redirect_uri":"https://elitea.example/mcp-auth-callback",
		"client_secret":"********",
		"toolkit_id":"19"
	}`)
	recorder := httptest.NewRecorder()

	handler.MCPOAuthProxy(recorder, request)

	assertStatus(t, recorder, http.StatusOK)
	response := decodeObj(t, recorder)
	if response["access_token"] != "access" || response["refresh_token"] != "refresh" {
		t.Fatalf("unexpected token response: %#v", response)
	}
	if _, leaked := response["client_secret"]; leaked {
		t.Fatalf("stored client secret crossed the browser boundary: %#v", response)
	}
	if resolver.calls != 1 || resolver.projectID != 7 || resolver.actorID != 11 || resolver.toolkitID != 19 {
		t.Fatalf("resolver call = %d identity = %d/%d/%d", resolver.calls, resolver.projectID, resolver.actorID, resolver.toolkitID)
	}
}

func TestMCPOAuthProxyRefusesStoredSecretAtUnboundEndpoint(t *testing.T) {
	resolver := &delegatedAuthSettingsStub{
		found: true,
		result: toolkitexecutionapp.DelegatedAuthToolkitSettings{
			ToolkitType: "sharepoint",
			Settings: map[string]any{
				"sharepoint_configuration": map[string]any{
					"client_id":                "stored-client",
					"client_secret":            "stored-secret",
					"oauth_discovery_endpoint": "https://login.example/tenant",
				},
			},
		},
	}
	transportCalled := false
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		transportCalled = true
		return oauthResponse(request, http.StatusOK, `{"access_token":"unexpected"}`), nil
	})}
	handler := eliteacore.NewHandler(
		nil,
		eliteacore.WithHTTPClient(client),
		eliteacore.WithDelegatedAuthToolkitSettingsResolver(resolver),
	)
	request := authenticatedOAuthRequest(t, `{
		"token_endpoint":"https://collector.example/token",
		"code":"code",
		"redirect_uri":"https://elitea.example/mcp-auth-callback",
		"toolkit_id":19
	}`)
	recorder := httptest.NewRecorder()

	handler.MCPOAuthProxy(recorder, request)

	assertStatus(t, recorder, http.StatusBadRequest)
	if transportCalled {
		t.Fatal("unbound token endpoint reached the transport")
	}
	if response := decodeObj(t, recorder); response["error"] != "untrusted_token_endpoint" {
		t.Fatalf("unexpected refusal: %#v", response)
	}
}

func TestMCPOAuthProxyUsedDCRNeverLoadsStoredCredentials(t *testing.T) {
	resolver := &delegatedAuthSettingsStub{err: errors.New("must not be called")}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if request.Form.Get("client_id") != "dcr-client" || request.Form.Get("client_secret") != "dcr-secret" {
			t.Fatalf("DCR credentials changed: %#v", request.Form)
		}
		return oauthResponse(request, http.StatusOK, `{"access_token":"access"}`), nil
	})}
	handler := eliteacore.NewHandler(
		nil,
		eliteacore.WithHTTPClient(client),
		eliteacore.WithDelegatedAuthToolkitSettingsResolver(resolver),
	)
	request := authenticatedOAuthRequest(t, `{
		"token_endpoint":"https://identity.example/token",
		"code":"code",
		"redirect_uri":"https://elitea.example/mcp-auth-callback",
		"client_id":"dcr-client",
		"client_secret":"dcr-secret",
		"scope":"dcr.scope",
		"toolkit_id":19,
		"used_dcr":true
	}`)
	recorder := httptest.NewRecorder()

	handler.MCPOAuthProxy(recorder, request)

	assertStatus(t, recorder, http.StatusOK)
	if resolver.calls != 0 {
		t.Fatalf("DCR request loaded stored toolkit credentials %d times", resolver.calls)
	}
}

func TestMCPOAuthProxyUsedDCRLoadsOnlyStoredScope(t *testing.T) {
	resolver := &delegatedAuthSettingsStub{
		found: true,
		result: toolkitexecutionapp.DelegatedAuthToolkitSettings{
			ToolkitType: "sharepoint",
			Settings: map[string]any{
				"sharepoint_configuration": map[string]any{
					"client_id":     "stored-client",
					"client_secret": "stored-secret",
					"scopes":        []any{"records.read", "records.write"},
				},
			},
		},
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if request.Form.Get("client_id") != "dcr-client" || request.Form.Get("client_secret") != "dcr-secret" {
			t.Fatalf("DCR credentials changed: %#v", request.Form)
		}
		if request.Form.Get("scope") != "records.read records.write" {
			t.Fatalf("stored scope was not applied: %#v", request.Form)
		}
		return oauthResponse(request, http.StatusOK, `{"access_token":"access"}`), nil
	})}
	handler := eliteacore.NewHandler(
		nil,
		eliteacore.WithHTTPClient(client),
		eliteacore.WithDelegatedAuthToolkitSettingsResolver(resolver),
	)
	request := authenticatedOAuthRequest(t, `{
		"token_endpoint":"https://identity.example/token",
		"code":"code",
		"redirect_uri":"https://elitea.example/mcp-auth-callback",
		"client_id":"dcr-client",
		"client_secret":"dcr-secret",
		"toolkit_id":19,
		"used_dcr":true
	}`)
	recorder := httptest.NewRecorder()

	handler.MCPOAuthProxy(recorder, request)

	assertStatus(t, recorder, http.StatusOK)
	if resolver.calls != 1 || resolver.projectID != 7 || resolver.actorID != 11 || resolver.toolkitID != 19 {
		t.Fatalf("resolver call = %d identity = %d/%d/%d", resolver.calls, resolver.projectID, resolver.actorID, resolver.toolkitID)
	}
}

func TestMCPOAuthProxyRedactsStoredSecretFromProviderFailure(t *testing.T) {
	resolver := &delegatedAuthSettingsStub{
		found: true,
		result: toolkitexecutionapp.DelegatedAuthToolkitSettings{
			ToolkitType: "openapi",
			Settings: map[string]any{
				"openapi_configuration": map[string]any{
					"client_id":     "stored-client",
					"client_secret": "stored-secret-value",
					"token_url":     "https://identity.example/token",
				},
			},
		},
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return oauthResponse(request, http.StatusUnauthorized,
			`{"error":"invalid_client","error_description":"rejected stored-secret-value"}`), nil
	})}
	handler := eliteacore.NewHandler(
		nil,
		eliteacore.WithHTTPClient(client),
		eliteacore.WithDelegatedAuthToolkitSettingsResolver(resolver),
	)
	request := authenticatedOAuthRequest(t, `{
		"token_endpoint":"https://identity.example/token",
		"refresh_token":"refresh-value",
		"grant_type":"refresh_token",
		"toolkit_id":19
	}`)
	recorder := httptest.NewRecorder()

	handler.MCPOAuthProxy(recorder, request)

	assertStatus(t, recorder, http.StatusBadRequest)
	responseText := recorder.Body.String()
	if strings.Contains(responseText, "stored-secret-value") || strings.Contains(responseText, "refresh-value") {
		t.Fatalf("sensitive value crossed provider failure boundary: %s", responseText)
	}
	if !strings.Contains(responseText, "[redacted]") {
		t.Fatalf("redaction marker absent: %s", responseText)
	}
}

func TestMCPOAuthProxyAcceptsFormEncodedTokenResponse(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return oauthResponse(request, http.StatusOK, "access_token=form-token&token_type=bearer&expires_in=60"), nil
	})}
	handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client))
	request := authenticatedOAuthRequest(t, `{
		"token_endpoint":"https://identity.example/token",
		"code":"code",
		"redirect_uri":"https://elitea.example/mcp-auth-callback",
		"client_id":"public-client"
	}`)
	recorder := httptest.NewRecorder()

	handler.MCPOAuthProxy(recorder, request)

	assertStatus(t, recorder, http.StatusOK)
	if response := decodeObj(t, recorder); response["access_token"] != "form-token" {
		t.Fatalf("unexpected form token response: %#v", response)
	}
}

func TestMCPOAuthProxySendsRefreshGrantWithoutAuthorizationCodeFields(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if request.Form.Get("grant_type") != "refresh_token" ||
			request.Form.Get("refresh_token") != "refresh-value" ||
			request.Form.Has("code") || request.Form.Has("redirect_uri") {
			t.Fatalf("unexpected refresh form: %#v", request.Form)
		}
		return oauthResponse(request, http.StatusOK, `{"access_token":"refreshed"}`), nil
	})}
	handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client))
	request := authenticatedOAuthRequest(t, `{
		"token_endpoint":"https://identity.example/token",
		"grant_type":"refresh_token",
		"refresh_token":"refresh-value",
		"client_id":"public-client"
	}`)
	recorder := httptest.NewRecorder()

	handler.MCPOAuthProxy(recorder, request)

	assertStatus(t, recorder, http.StatusOK)
	if response := decodeObj(t, recorder); response["access_token"] != "refreshed" {
		t.Fatalf("unexpected refresh response: %#v", response)
	}
}

func TestMCPOAuthProxyRequiresToolkitForStoredSecretReference(t *testing.T) {
	transportCalled := false
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		transportCalled = true
		return oauthResponse(request, http.StatusOK, `{"access_token":"unexpected"}`), nil
	})}
	handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client))
	request := authenticatedOAuthRequest(t, `{
		"token_endpoint":"https://identity.example/token",
		"code":"code",
		"redirect_uri":"https://elitea.example/mcp-auth-callback",
		"client_id":"client",
		"client_secret":"{{secret.oauth_client}}"
	}`)
	recorder := httptest.NewRecorder()

	handler.MCPOAuthProxy(recorder, request)

	assertStatus(t, recorder, http.StatusBadRequest)
	if transportCalled {
		t.Fatal("unresolved secret reference reached the transport")
	}
	if response := decodeObj(t, recorder); response["error"] != "toolkit_required" {
		t.Fatalf("unexpected refusal: %#v", response)
	}
}

func TestMCPDCRProxyForwardsRFC7591Fields(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		for key, expected := range map[string]any{
			"client_name":                "Elitea",
			"token_endpoint_auth_method": "client_secret_post",
			"application_type":           "native",
			"scope":                      "mcp:read mcp:write",
			"software_id":                "elitea-web",
			"software_version":           "2",
		} {
			if body[key] != expected {
				t.Fatalf("%s = %#v, want %#v; body=%#v", key, body[key], expected, body)
			}
		}
		if len(body["response_types"].([]any)) != 1 || len(body["grant_types"].([]any)) != 1 {
			t.Fatalf("RFC 7591 lists were not forwarded: %#v", body)
		}
		return oauthResponse(request, http.StatusCreated, `{"client_id":"registered","client_secret":"issued"}`), nil
	})}
	handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client))
	request := authenticatedOAuthRequest(t, `{
		"registration_endpoint":"https://identity.example/register",
		"redirect_uris":["https://elitea.example/mcp-auth-callback"],
		"client_name":"Elitea",
		"grant_types":["authorization_code"],
		"response_types":["code"],
		"token_endpoint_auth_method":"client_secret_post",
		"application_type":"native",
		"scope":"mcp:read mcp:write",
		"software_id":"elitea-web",
		"software_version":"2"
	}`)
	recorder := httptest.NewRecorder()

	handler.MCPDCRProxy(recorder, request)

	assertStatus(t, recorder, http.StatusOK)
	response := decodeObj(t, recorder)
	if response["client_id"] != "registered" || response["client_secret"] != "issued" {
		t.Fatalf("unexpected DCR response: %#v", response)
	}
}

func TestMCPProxiesRejectMalformedAndOversizedRequests(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		handler func(*eliteacore.Handler, http.ResponseWriter, *http.Request)
		status  int
	}{
		{"OAuth malformed", `{`, (*eliteacore.Handler).MCPOAuthProxy, http.StatusBadRequest},
		{"DCR missing redirects", `{"registration_endpoint":"https://identity.example/register"}`, (*eliteacore.Handler).MCPDCRProxy, http.StatusBadRequest},
		{"OAuth unsupported grant", `{"token_endpoint":"https://identity.example/token","grant_type":"password"}`, (*eliteacore.Handler).MCPOAuthProxy, http.StatusBadRequest},
		{"OAuth oversized", `{"padding":"` + strings.Repeat("x", 70*1024) + `"}`, (*eliteacore.Handler).MCPOAuthProxy, http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			test.handler(eliteacore.NewHandler(nil), recorder, authenticatedOAuthRequest(t, test.body))
			assertStatus(t, recorder, test.status)
		})
	}
}
