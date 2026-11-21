package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// mintCredentialValidator authenticates `elnat_` bearers as the native
// access-token validator does (a principal carrying NativeClientID) and
// anything else as a personal access token.
type mintCredentialValidator struct{}

func (mintCredentialValidator) ValidateToken(_ context.Context, token string) (auth.User, error) {
	user := auth.User{ID: "7", UserID: "7", TokenID: "70", Email: "mint@example.test", AuthType: "token"}
	if strings.HasPrefix(token, "elnat_") {
		user.NativeClientID = "ai.elitea.desktop"
	}
	return user, nil
}

// TestTokenMintClassifiesTheCredentialThroughTheRealMiddleware is the seam
// the handler tests cannot see (issue 2559): the provenance and the native
// client the handler reads must survive Auth, the principal check and every
// gate in between.
//
// The router here has no database, so its permission resolver refuses every
// principal. That is exactly what makes the native case observable: a native
// session reaches the administration check and is refused with
// token_mint_caller_unresolved, while a personal access token is refused
// earlier, for its kind, with token_mint_browser_session_required.
func TestTokenMintClassifiesTheCredentialThroughTheRealMiddleware(t *testing.T) {
	router := NewRouter(RouterConfig{
		AuthValidator:      apimw.TokenValidator(mintCredentialValidator{}),
		PrincipalValidator: testPrincipalValidator{},
		SessionSecret:      "changeme-standalone",
	})
	for _, tc := range []struct {
		bearer string
		code   string
	}{
		{"eyJhbGciOiJIUzUxMiJ9.e30.sig", "token_mint_browser_session_required"},
		{"elnat_device", "token_mint_caller_unresolved"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v2/auth/token/",
				bytes.NewReader([]byte(`{"name":"x"}`)))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+tc.bearer)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			var refusal struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(recorder.Body.Bytes(), &refusal)
			if recorder.Code != http.StatusForbidden || refusal.Error.Code != tc.code {
				t.Fatalf("POST /api/v2/auth/token/ = %d %s, want 403 %s",
					recorder.Code, recorder.Body.String(), tc.code)
			}
		})
	}
}
