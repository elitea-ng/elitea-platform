package agentexecution

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

func TestCurrentRoutesPassSessionTokensAfterPermissionCheck(t *testing.T) {
	const tokens = `{"credential:https://issuer.example":{"access_token":"test-access","session_id":"session"}}`
	for _, kind := range []string{"application", "adhoc", "regeneration"} {
		t.Run(kind, func(t *testing.T) {
			useCase := &currentStartUseCaseStub{}
			permissions := currentStartPermissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{UserID: 11, Permissions: []string{CurrentApplicationStartPermission, CurrentRegenerationPermission}}, nil
			})
			body, makeRequest := validCurrentStartBody(), currentStartRequest
			if kind == "adhoc" {
				body, makeRequest = validCurrentAdhocStartBody(), currentAdhocStartRequest
			}
			if kind == "regeneration" {
				body, makeRequest = validCurrentRegenerationBody(), currentRegenerationRequest
			}
			body = strings.Replace(body, `"mcp_tokens":{}`, `"mcp_tokens":`+tokens, 1)
			if !strings.Contains(body, tokens) {
				t.Fatal("test body has no session token map")
			}
			response := httptest.NewRecorder()
			newCurrentStartRoute(t, useCase, permissions).ServeHTTP(response, makeRequest(body))
			var received json.RawMessage
			switch kind {
			case "application":
				received = useCase.request.MCPTokens
			case "adhoc":
				received = useCase.adhocRequest.MCPTokens
			case "regeneration":
				received = useCase.regenerationRequest.MCPTokens
			}
			if response.Code != http.StatusOK || string(received) != tokens {
				t.Fatalf("status=%d, token map preserved=%t", response.Code, string(received) == tokens)
			}
			if strings.Contains(response.Body.String(), "test-access") {
				t.Fatal("session token leaked in route response")
			}
		})
	}
}
