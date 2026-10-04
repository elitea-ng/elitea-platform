package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type errValidator struct{ err error }

func (v errValidator) ValidateToken(context.Context, string) (auth.User, error) {
	return auth.User{}, v.err
}

func bearerStatus(t *testing.T, err error) *httptest.ResponseRecorder {
	t.Helper()
	handler := Auth(AuthConfig{Validator: errValidator{err: err}})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("a refused credential reached the handler")
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/v2/x", nil)
	request.Header.Set("Authorization", "Bearer elnat_x")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// ADR-0025 decision 4 / coordinator decision 8: the flat body, the bearer
// challenge, and nothing else changes for any other refusal.
func TestAuthAnswersDeviceRevokedWithTheFlatBody(t *testing.T) {
	recorder := bearerStatus(t, fmt.Errorf("wrapped: %w", auth.ErrDeviceRevoked))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got := recorder.Header().Get("WWW-Authenticate"); got != `Bearer error="invalid_token", error_description="device_revoked"` {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body["error"] != "device_revoked" {
		t.Fatalf("body = %s", recorder.Body.String())
	}

	// A plain rejection keeps the nested envelope: `error` is an OBJECT, the
	// client's "refresh once" signal.
	rejected := bearerStatus(t, auth.ErrCredentialRejected)
	var nested struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if rejected.Code != http.StatusUnauthorized || json.Unmarshal(rejected.Body.Bytes(), &nested) != nil ||
		nested.Error.Code != reasonTokenRejected {
		t.Fatalf("plain rejection = %d %s", rejected.Code, rejected.Body.String())
	}
	if unavailable := bearerStatus(t, auth.ErrCredentialValidationUnavailable); unavailable.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable = %d", unavailable.Code)
	}
	if !errors.Is(auth.ErrDeviceRevoked, auth.ErrCredentialRejected) {
		t.Fatal("ErrDeviceRevoked must stay a rejection for every caller that only knows rejected")
	}
}
