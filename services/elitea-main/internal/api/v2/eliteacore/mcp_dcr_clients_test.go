package eliteacore_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpoauth"
	"github.com/stretchr/testify/require"
)

type dcrClientsStub struct {
	binding mcpoauth.Binding
	secret  string
	expires time.Time
	saveErr error
}

const dcrFixtureReference = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func (s *dcrClientsStub) Save(_ context.Context, binding mcpoauth.Binding, secret string, expires time.Time) (string, error) {
	s.binding, s.secret, s.expires = binding, secret, expires
	return dcrFixtureReference, s.saveErr
}
func (s *dcrClientsStub) Load(_ context.Context, reference string, binding mcpoauth.Binding) (string, error) {
	if reference != dcrFixtureReference || binding != s.binding {
		return "", mcpoauth.ErrClientUnavailable
	}
	return s.secret, nil
}

func TestMCPDCRSecretRemainsInMainAcrossBothGrants(t *testing.T) {
	store := &dcrClientsStub{}
	tokenRequests := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/register" {
			var wire map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&wire))
			require.NotContains(t, wire, "token_endpoint")
			require.NotContains(t, wire, "resource")
			return oauthResponse(r, 201, `{"client_id":"registered","client_secret":"private-fixture","client_secret_expires_at":0,"registration_access_token":"never-return"}`), nil
		}
		require.NoError(t, r.ParseForm())
		require.Equal(t, "private-fixture", r.Form.Get("client_secret"))
		require.Equal(t, "registered", r.Form.Get("client_id"))
		require.Equal(t, "https://resource.example/mcp", r.Form.Get("resource"))
		tokenRequests++
		return oauthResponse(r, 200, `{"access_token":"access","refresh_token":"refresh"}`), nil
	})}
	handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client), eliteacore.WithMCPDCRClients(store))
	recorder := httptest.NewRecorder()
	handler.MCPDCRProxy(recorder, authenticatedOAuthRequest(t, `{
 "registration_endpoint":"https://issuer.example/register",
 "token_endpoint":"https://issuer.example/token","resource":"https://resource.example/mcp",
 "redirect_uris":["https://elitea.example/callback"]
 }`))
	require.Equal(t, 200, recorder.Code, recorder.Body.String())
	result := decodeObj(t, recorder)
	require.Equal(t, dcrFixtureReference, result["client_reference"])
	require.NotContains(t, recorder.Body.String(), "private-fixture")
	require.NotContains(t, result, "client_secret")
	require.NotContains(t, result, "registration_access_token")
	require.Equal(t, int32(7), store.binding.ProjectID)
	require.Equal(t, int32(11), store.binding.ActorID)
	require.True(t, store.expires.IsZero())

	for _, grant := range []string{"authorization_code", "refresh_token"} {
		recorder = httptest.NewRecorder()
		handler.MCPOAuthProxy(recorder, authenticatedOAuthRequest(t, `{
   "token_endpoint":"https://issuer.example/token","resource":"https://resource.example/mcp",
   "code":"code","redirect_uri":"https://elitea.example/callback","refresh_token":"refresh",
   "client_id":"registered","client_reference":"`+dcrFixtureReference+`","used_dcr":true,"grant_type":"`+grant+`"
  }`))
		require.Equal(t, 200, recorder.Code, recorder.Body.String())
	}
	require.Equal(t, 2, tokenRequests)
}

func TestMCPDCRReferencesRejectSubstitutionBeforeEgress(t *testing.T) {
	store := &dcrClientsStub{binding: mcpoauth.Binding{ProjectID: 7, ActorID: 11, ClientID: "client", TokenEndpoint: "https://issuer.example/token", Resource: "https://resource.example/mcp"}, secret: "fixture"}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("rejected client reference reached token transport")
		return nil, nil
	})}
	handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client), eliteacore.WithMCPDCRClients(store))
	for name, change := range map[string]any{
		"client_id":        "another-client",
		"token_endpoint":   "https://collector.example/token",
		"resource":         "https://another.example/mcp",
		"client_reference": "unknown",
		"client_secret":    "competing-secret",
		"used_dcr":         false,
	} {
		t.Run(name, func(t *testing.T) {
			body := map[string]any{"client_id": "client", "token_endpoint": "https://issuer.example/token", "resource": "https://resource.example/mcp",
				"client_reference": dcrFixtureReference, "used_dcr": true, "grant_type": "refresh_token", "refresh_token": "refresh"}
			body[name] = change
			encoded, err := json.Marshal(body)
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			handler.MCPOAuthProxy(recorder, authenticatedOAuthRequest(t, string(encoded)))
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.NotContains(t, recorder.Body.String(), "fixture")
		})
	}
}

func TestMCPDCRStorageFailureNeverReturnsSecret(t *testing.T) {
	for _, store := range []*dcrClientsStub{nil, {saveErr: errors.New("database unavailable")}} {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return oauthResponse(r, 201, `{"client_id":"registered","client_secret":"private-fixture"}`), nil
		})}
		handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client))
		if store != nil {
			handler = eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client), eliteacore.WithMCPDCRClients(store))
		}
		recorder := httptest.NewRecorder()
		handler.MCPDCRProxy(recorder, authenticatedOAuthRequest(t, `{"registration_endpoint":"https://issuer.example/register","token_endpoint":"https://issuer.example/token","redirect_uris":["https://elitea.example/callback"]}`))
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
		require.NotContains(t, recorder.Body.String(), "private-fixture")
	}
}
