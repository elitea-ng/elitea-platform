package middleware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/modelruntoken"
)

type leaseFunc func(context.Context, modelruntoken.Binding) (bool, error)

func (f leaseFunc) ClaimLive(ctx context.Context, b modelruntoken.Binding) (bool, error) { return f(ctx, b) }

func modelRunTokenFixture(t *testing.T, live *bool) (string, *modelruntoken.Validator) {
	t.Helper()
	signer, err := modelruntoken.NewSigner(modelruntoken.DeriveKey([]byte("pat-signing-key")))
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Issue(modelruntoken.Binding{
		ExecutionID: "exec-1", Generation: 1, ClaimID: "claim-1",
		FenceToken: bytes.Repeat([]byte{1}, 32), ProjectID: 42, UserID: 9, TokenID: 11,
	})
	if err != nil {
		t.Fatal(err)
	}
	validator, err := modelruntoken.NewValidator(signer, leaseFunc(func(context.Context, modelruntoken.Binding) (bool, error) {
		return *live, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return token, validator
}

// TestModelRunTokenIsRefusedOutsideTheModelGateway: a governed run's token
// (#1461) is 403 on every route group that does not carry ModelRunTokens —
// every group but /llm — before any token validator runs, as a bearer and as
// an API key.
func TestModelRunTokenIsRefusedOutsideTheModelGateway(t *testing.T) {
	live := true
	token, _ := modelRunTokenFixture(t, &live)
	validatorCalls := 0
	pat := tokenValidatorFunc(func(context.Context, string) (auth.User, error) {
		validatorCalls++
		return auth.User{ID: "9", UserID: "9", TokenID: "11", AuthType: "token"}, nil
	})
	principals := principalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) { return user, nil })
	for _, header := range []struct{ name, value string }{
		{"Authorization", "Bearer " + token},
		{"X-API-Key", token},
	} {
		reached := false
		handler := apimw.Auth(apimw.AuthConfig{Validator: pat, PrincipalValidator: principals})(
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
		request := httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/applications/prompt_lib/42", nil)
		request.Header.Set(header.name, header.value)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden || reached {
			t.Fatalf("%s: status = %d reached = %v, want 403 before the handler", header.name, response.Code, reached)
		}
		var body struct {
			Error struct{ Code string } `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != "model_run_token_audience" {
			t.Fatalf("%s: body = %s, want code model_run_token_audience", header.name, response.Body.String())
		}
	}
	if validatorCalls != 0 {
		t.Fatalf("the PAT validator read a model run token %d times", validatorCalls)
	}
}

// TestModelRunTokenIsAcceptedOnTheModelGateway: the /llm group accepts it as
// the run's actor and the actor's token row, bills the token's project, and
// stops accepting it when the claim's lease is gone.
func TestModelRunTokenIsAcceptedOnTheModelGateway(t *testing.T) {
	live := true
	token, validator := modelRunTokenFixture(t, &live)
	principalCalls := 0
	principals := principalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
		principalCalls++
		return user, nil
	})
	var seen auth.User
	var selector string
	handler := apimw.Auth(apimw.AuthConfig{
		Validator: unusedTokenValidator(), PrincipalValidator: principals, ModelRunTokens: validator,
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = auth.RuntimePrincipalFromContext(r.Context())
		selector = r.Header.Get(apimw.HeaderProjectSelector)
		w.WriteHeader(http.StatusOK)
	}))
	serve := func(mutate func(*http.Request)) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		if mutate != nil {
			mutate(request)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	if response := serve(nil); response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", response.Code, response.Body.String())
	}
	if seen.UserID != "9" || seen.ID != "9" || seen.TokenID != "11" || seen.AuthType != "token" || principalCalls != 1 {
		t.Fatalf("principal = %+v (principal checks %d), want the actor's validated token principal", seen, principalCalls)
	}
	if selector != "42" {
		t.Fatalf("selector = %q, want the token's project", selector)
	}
	if response := serve(func(r *http.Request) {
		r.Header.Set(apimw.HeaderProjectSelector, "42")
		r.Header.Set("X-Elitea-Execution-Id", "exec-1")
	}); response.Code != http.StatusOK {
		t.Fatalf("own project and execution: status = %d, want 200", response.Code)
	}
	for name, mutate := range map[string]func(*http.Request){
		"another project":   func(r *http.Request) { r.Header.Set(apimw.HeaderProjectSelector, "43") },
		"another execution": func(r *http.Request) { r.Header.Set("X-Elitea-Execution-Id", "exec-2") },
	} {
		if response := serve(mutate); response.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403", name, response.Code)
		}
	}

	live = false
	if response := serve(nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("lapsed lease: status = %d, want 401", response.Code)
	}
}
