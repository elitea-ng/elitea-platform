package middleware_test

// A token that could not be CHECKED is not a token that was REFUSED.
//
// Regression finding C2: a valid personal access token was answered
// `401 token_rejected` on two administration routes right after it passed
// `/social/author`. A 401 there says "your credential is wrong", and every
// caller acts on that: the SDK gives up, the harness switches credential, a
// person rotates a token that was fine. When the validator could not reach
// its store, the answer has to say so — the principal and the server-side
// session paths already do (503 with Retry-After).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

func TestAuthAnswersATokenStoreFaultWith503(t *testing.T) {
	unavailable := tokenValidatorFunc(func(context.Context, string) (auth.User, error) {
		return auth.User{}, fmt.Errorf("%w: db lookup failed: pool exhausted", auth.ErrCredentialValidationUnavailable)
	})
	rejected := tokenValidatorFunc(func(context.Context, string) (auth.User, error) {
		return auth.User{}, fmt.Errorf("%w: token not found", auth.ErrCredentialRejected)
	})
	principals := principalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
		return user, nil
	})

	for _, test := range []struct {
		name       string
		validator  apimw.TokenValidator
		header     string
		value      string
		wantStatus int
		wantCode   string
	}{
		{"bearer, store unavailable", unavailable, "Authorization", "Bearer t", http.StatusServiceUnavailable, "token_store_unavailable"},
		{"api key, store unavailable", unavailable, "X-API-Key", "t", http.StatusServiceUnavailable, "token_store_unavailable"},
		{"bearer, token refused", rejected, "Authorization", "Bearer t", http.StatusUnauthorized, "token_rejected"},
		{"api key, token refused", rejected, "X-API-Key", "t", http.StatusUnauthorized, "token_rejected"},
		// The composition defect behind F2 and C2. The caller still reads
		// token_rejected: the missing validator is the server's fact, and the
		// log line names it (reasonTokenValidatorAbsent).
		{"bearer, no validator composed", nil, "Authorization", "Bearer t", http.StatusUnauthorized, "token_rejected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reached := false
			handler := apimw.Auth(apimw.AuthConfig{Validator: test.validator, PrincipalValidator: principals})(
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
			request := httptest.NewRequest(http.MethodDelete, "/api/v2/projects/project/administration/8", nil)
			request.Header.Set(test.header, test.value)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if reached {
				t.Fatal("the protected handler ran for a credential nobody accepted")
			}
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body.String())
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %q: %v", response.Body.String(), err)
			}
			if body.Error.Code != test.wantCode {
				t.Fatalf("code = %q, want %q", body.Error.Code, test.wantCode)
			}
			if test.wantStatus == http.StatusServiceUnavailable {
				if response.Header().Get("Retry-After") == "" {
					t.Fatal("a 503 for a store fault carries no Retry-After")
				}
				if body.Error.Message != "token store unavailable" {
					t.Fatalf("message = %q: the cause must not cross the boundary", body.Error.Message)
				}
			}
		})
	}
}
