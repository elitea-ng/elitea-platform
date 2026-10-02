package eliteacore_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	toolkitexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpoauth"
	"github.com/stretchr/testify/require"
)

type delegatedTokensStub struct {
	calls   int
	binding mcpoauth.TokenBinding
	token   mcpoauth.AccessToken
	expires time.Time
	err     error
}

func (s *delegatedTokensStub) Save(_ context.Context, b mcpoauth.TokenBinding, t mcpoauth.AccessToken, e time.Time) (mcpoauth.TokenReference, error) {
	s.calls++
	s.binding = b
	s.token = t
	s.expires = e
	return mcpoauth.TokenReference{Reference: strings.Repeat("a", 43), Revision: 1, ExpiresAt: e}, s.err
}

func TestDelegatedTokenExchangeBindsAuthoritativeSavedResource(t *testing.T) {
	for _, kind := range []string{"mcp", "openapi"} {
		t.Run(kind, func(t *testing.T) {
			settings := map[string]any{"url": "https://resource.example/mcp"}
			resource := "https://resource.example/mcp"
			if kind == "openapi" {
				settings = map[string]any{"spec": `{"servers":[{"url":"https://resource.example/api/"}]}`}
				resource = "https://resource.example/api"
			}
			resolver := &delegatedAuthSettingsStub{found: true, result: toolkitexecutionapp.DelegatedAuthToolkitSettings{ToolkitType: kind, Settings: settings}}
			store := &delegatedTokensStub{}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return oauthResponse(r, 200, `{"access_token":"fixture-access","refresh_token":"fixture-refresh","token_type":"Bearer","session_id":"fixture-session","expires_in":3600}`), nil
			})}
			handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client), eliteacore.WithDelegatedAuthToolkitSettingsResolver(resolver), eliteacore.WithMCPDelegatedTokens(store))
			req := authenticatedOAuthRequest(t, `{"token_endpoint":"https://issuer.example/token","code":"code","redirect_uri":"https://elitea.example/callback","client_id":"client","used_dcr":true,"authorization_reference_only":true,"toolkit_id":4,"scope":"read","resource":"`+resource+`"}`)
			res := httptest.NewRecorder()
			handler.MCPOAuthProxy(res, req)
			require.Equal(t, 200, res.Code, res.Body.String())
			require.Equal(t, 1, store.calls)
			require.Equal(t, mcpoauth.TokenBinding{ProjectID: 7, ActorID: 11, ToolkitID: 4, Resource: resource}, store.binding)
			require.Equal(t, "fixture-access", store.token.AccessToken)
			require.Equal(t, "fixture-session", store.token.SessionID)
			require.WithinDuration(t, time.Now().Add(time.Hour), store.expires, 2*time.Second)
			var result map[string]any
			require.NoError(t, json.Unmarshal(res.Body.Bytes(), &result))
			require.Equal(t, strings.Repeat("a", 43), result["authorization_reference"])
			require.EqualValues(t, 1, result["authorization_revision"])
			for _, field := range []string{"access_token", "refresh_token", "id_token", "session_id"} {
				require.NotContains(t, result, field)
			}
			require.NotContains(t, res.Body.String(), "fixture-access")
			require.NotContains(t, res.Body.String(), "fixture-refresh")
			require.NotContains(t, res.Body.String(), "fixture-session")
			require.Equal(t, "no-store", res.Header().Get("Cache-Control"))
		})
	}
}

func TestDelegatedTokenExchangeRejectsForeignResourceAndFailedProvider(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resource string
		found    bool
		status   int
		provider string
		storeErr error
		want     int
		calls    int
	}{
		{"foreign resource", "https://foreign.example/mcp", true, 200, `{"access_token":"fixture"}`, nil, 400, 0},
		{"hidden toolkit", "https://resource.example/mcp", false, 200, `{"access_token":"fixture"}`, nil, 404, 0},
		{"provider rejection", "https://resource.example/mcp", true, 400, `{"error":"invalid_grant"}`, nil, 400, 0},
		{"malformed response", "https://resource.example/mcp", true, 200, `{"access_token":123}`, nil, 502, 0},
		{"expired response", "https://resource.example/mcp", true, 200, `{"access_token":"fixture","expires_in":0}`, nil, 502, 0},
		{"storage failure", "https://resource.example/mcp", true, 200, `{"access_token":"fixture"}`, errors.New("private-storage-detail"), 503, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &delegatedTokensStub{err: tc.storeErr}
			resolver := &delegatedAuthSettingsStub{found: tc.found, result: toolkitexecutionapp.DelegatedAuthToolkitSettings{ToolkitType: "mcp", Settings: map[string]any{"url": "https://resource.example/mcp"}}}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { return oauthResponse(r, tc.status, tc.provider), nil })}
			handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client), eliteacore.WithDelegatedAuthToolkitSettingsResolver(resolver), eliteacore.WithMCPDelegatedTokens(store))
			req := authenticatedOAuthRequest(t, `{"token_endpoint":"https://issuer.example/token","code":"code","redirect_uri":"https://elitea.example/callback","client_id":"client","used_dcr":true,"scope":"read","toolkit_id":4,"resource":"`+tc.resource+`"}`)
			res := httptest.NewRecorder()
			handler.MCPOAuthProxy(res, req)
			require.Equal(t, tc.want, res.Code, res.Body.String())
			require.Equal(t, tc.calls, store.calls)
			require.NotContains(t, res.Body.String(), "fixture")
			require.NotContains(t, res.Body.String(), "private-storage-detail")
		})
	}
}

func TestReferenceOnlyExchangeFailsClosedWithoutStorageOrSavedToolkit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		toolkit string
		want    int
	}{
		{"missing store", `,"toolkit_id":4`, http.StatusServiceUnavailable},
		{"unsaved toolkit", "", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return oauthResponse(r, 200, `{"access_token":"must-not-return","refresh_token":"must-not-return"}`), nil
			})}
			handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client))
			req := authenticatedOAuthRequest(t, `{"authorization_reference_only":true,"token_endpoint":"https://issuer.example/token","code":"code","redirect_uri":"https://elitea.example/callback","client_id":"client","used_dcr":true,"scope":"read","resource":"https://resource.example/mcp"`+tc.toolkit+`}`)
			res := httptest.NewRecorder()
			handler.MCPOAuthProxy(res, req)
			require.Equal(t, tc.want, res.Code, res.Body.String())
			require.NotContains(t, res.Body.String(), "must-not-return")
		})
	}
}

func TestOpenAPIReferenceUsesSavedResourceWithoutOAuthIndicator(t *testing.T) {
	for _, tc := range []struct {
		name, kind, submitted string
		want                  int
	}{
		{"OpenAPI omitted", "openapi", "", 200},
		{"OpenAPI mismatch", "openapi", "https://foreign.example/api", 400},
		{"MCP omitted remains refused", "mcp", "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resource := "https://resource.example/api"
			store := &delegatedTokensStub{}
			resolver := &delegatedAuthSettingsStub{found: true, result: toolkitexecutionapp.DelegatedAuthToolkitSettings{ToolkitType: tc.kind, Settings: map[string]any{"base_url": resource, "url": resource}}}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				require.NoError(t, r.ParseForm())
				require.Equal(t, tc.submitted, r.Form.Get("resource"))
				return oauthResponse(r, 200, `{"access_token":"fixture-access","expires_in":3600}`), nil
			})}
			handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client), eliteacore.WithDelegatedAuthToolkitSettingsResolver(resolver), eliteacore.WithMCPDelegatedTokens(store))
			req := authenticatedOAuthRequest(t, `{"token_endpoint":"https://issuer.example/token","code":"code","redirect_uri":"https://elitea.example/callback","client_id":"client","used_dcr":true,"authorization_reference_only":true,"toolkit_id":4,"resource":"`+tc.submitted+`"}`)
			res := httptest.NewRecorder()
			handler.MCPOAuthProxy(res, req)
			require.Equal(t, tc.want, res.Code, res.Body.String())
			if tc.want != 200 {
				require.Zero(t, store.calls)
				return
			}
			require.Equal(t, mcpoauth.TokenBinding{ProjectID: 7, ActorID: 11, ToolkitID: 4, Resource: resource}, store.binding)
			var result map[string]any
			require.NoError(t, json.Unmarshal(res.Body.Bytes(), &result))
			require.Equal(t, resource, result["authorization_resource"])
			require.NotContains(t, result, "access_token")
		})
	}
}
