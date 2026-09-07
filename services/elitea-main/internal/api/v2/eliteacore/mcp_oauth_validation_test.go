package eliteacore_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

func TestMCPProxiesValidateSuccessfulCredentialResponses(t *testing.T) {
	tests := []struct {
		name, provider string
		dcr            bool
		status         int
	}{
		{"token", `{"access_token":"access","token_type":"Bearer"}`, false, http.StatusOK},
		{"legacy token", `{"access_token":"access"}`, false, http.StatusOK},
		{"missing token", `{}`, false, http.StatusBadGateway},
		{"empty token", `{"access_token":""}`, false, http.StatusBadGateway},
		{"non-string token", `{"access_token":["access"]}`, false, http.StatusBadGateway},
		{"error with token", `{"access_token":"access","error":"invalid_grant"}`, false, http.StatusBadGateway},
		{"non-string refresh", `{"access_token":"access","refresh_token":{}}`, false, http.StatusBadGateway},
		{"non-string type", `{"access_token":"access","token_type":1}`, false, http.StatusBadGateway},
		{"duplicate form token", `access_token=one&access_token=two`, false, http.StatusBadGateway},
		{"oversized token body", `{"access_token":"` + strings.Repeat("x", 512*1024) + `"}`, false, http.StatusBadGateway},
		{"registration", `{"client_id":"registered","client_secret":"issued"}`, true, http.StatusOK},
		{"public registration", `{"client_id":"registered"}`, true, http.StatusOK},
		{"missing client", `{}`, true, http.StatusBadGateway},
		{"empty client", `{"client_id":""}`, true, http.StatusBadGateway},
		{"non-string client", `{"client_id":2}`, true, http.StatusBadGateway},
		{"non-string secret", `{"client_id":"registered","client_secret":[]}`, true, http.StatusBadGateway},
		{"error with client", `{"client_id":"registered","error":"invalid_redirect_uri"}`, true, http.StatusBadGateway},
		{"trailing registration", `{"client_id":"registered"}{}`, true, http.StatusBadGateway},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return oauthResponse(request, http.StatusOK, test.provider), nil
			})}
			handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client))
			recorder := httptest.NewRecorder()
			if test.dcr {
				handler.MCPDCRProxy(recorder, authenticatedOAuthRequest(t,
					`{"registration_endpoint":"https://identity.example/register","redirect_uris":["https://elitea.example/callback"]}`))
			} else {
				handler.MCPOAuthProxy(recorder, authenticatedOAuthRequest(t,
					`{"token_endpoint":"https://identity.example/token","client_id":"public","code":"code","redirect_uri":"https://elitea.example/callback"}`))
			}
			assertStatus(t, recorder, test.status)
			if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Pragma") != "no-cache" {
				t.Error("credential response can be cached")
			}
			if test.status == http.StatusBadGateway {
				expected := "invalid_token_response"
				if test.dcr {
					expected = "invalid_dcr_response"
				}
				if response := decodeObj(t, recorder); response["error"] != expected || len(response) != 1 {
					t.Error("invalid provider response was not replaced by a safe error")
				}
			}
		})
	}
}

func TestMCPOAuthProxyRedactsGrantMaterialBeforeTruncation(t *testing.T) {
	for _, test := range []struct {
		name, field, secret, prefix string
	}{
		{"verifier", "code_verifier", strings.Repeat("v", 43), "rejected "},
		{"code at truncation boundary", "code", "boundary-code-material", strings.Repeat("x", 1020)},
		{"short secret", "client_secret", "xyz", "rejected "},
		{"refresh", "refresh_token", "refresh-material", "rejected "},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider, err := json.Marshal(map[string]string{"error": "invalid_grant", "error_description": test.prefix + test.secret})
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return oauthResponse(request, http.StatusBadRequest, string(provider)), nil
			})}
			handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client))
			body := map[string]string{
				"token_endpoint": "https://identity.example/token", "client_id": "client",
				"code": "authorization-code", "redirect_uri": "https://elitea.example/callback",
			}
			body[test.field] = test.secret
			if test.field == "refresh_token" {
				body["grant_type"] = "refresh_token"
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			handler.MCPOAuthProxy(recorder, authenticatedOAuthRequest(t, string(raw)))
			assertStatus(t, recorder, http.StatusBadRequest)
			response := decodeObj(t, recorder)
			description, _ := response["error_description"].(string)
			expected := test.prefix + "[redacted]"
			if len(expected) > 1024 {
				expected = expected[:1024]
			}
			if description != expected {
				t.Error("provider error exposed grant material or truncated before redaction")
			}
		})
	}
}

func TestMCPOAuthProxyRedactsOverlappingGrantMaterial(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return oauthResponse(request, http.StatusBadRequest,
			`{"error":"invalid_grant","error_description":"common-code-material common"}`), nil
	})}
	handler := eliteacore.NewHandler(nil, eliteacore.WithHTTPClient(client))
	recorder := httptest.NewRecorder()
	handler.MCPOAuthProxy(recorder, authenticatedOAuthRequest(t,
		`{"token_endpoint":"https://identity.example/token","client_id":"client","client_secret":"common","code":"common-code-material","redirect_uri":"https://elitea.example/callback"}`))
	assertStatus(t, recorder, http.StatusBadRequest)
	if response := decodeObj(t, recorder); response["error_description"] != "[redacted] [redacted]" {
		t.Error("overlapping grant values left a credential suffix in the response")
	}
}
