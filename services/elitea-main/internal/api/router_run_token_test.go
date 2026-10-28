package api_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/runtoken"
)

type liveLeases struct{}

func (liveLeases) ClaimLive(context.Context, runtoken.Binding) (bool, error) { return true, nil }

// TestRouterScopesARunTokenToItsRouteGroups is the #1461 acceptance at
// the composed router: a governed run's run token reaches the /llm proxy and is
// refused with 403 on an /api/v2 route, with the same RouterConfig.
func TestRouterScopesARunTokenToItsRouteGroups(t *testing.T) {
	signer, err := runtoken.NewSigner(runtoken.DeriveKey([]byte("pat-signing-key")))
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Issue(runtoken.Binding{
		ExecutionID: "exec-1", Generation: 1, ClaimID: "claim-1",
		FenceToken: bytes.Repeat([]byte{1}, 32), ProjectID: 42, UserID: 9, TokenID: 11,
	})
	if err != nil {
		t.Fatal(err)
	}
	validator, err := runtoken.NewValidator(signer, liveLeases{})
	if err != nil {
		t.Fatal(err)
	}
	proxy := &recordingHandler{}
	// The PAT validator would admit anything: the refusal below is the
	// audience, not a bad credential.
	cfg := buildGatewayRouterConfig(t, &stubTokenValidator{user: auth.User{
		ID: "9", UserID: "9", TokenID: "11", AuthType: "token",
	}}, &stubProjectResolver{id: 42}, proxy)
	cfg.RunTokens = validator
	router := api.NewRouter(cfg)

	request := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !proxy.reached || proxy.project.ProjectID != 42 {
		t.Fatalf("/llm: status = %d reached = %v project = %d body = %s, want the proxy for project 42",
			response.Code, proxy.reached, proxy.project.ProjectID, response.Body.String())
	}

	for _, path := range []string{
		"/api/v2/elitea_core/applications/prompt_lib/42",
		"/api/v2/projects/project/42",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d body = %s, want 403", path, response.Code, response.Body.String())
		}
	}
}
