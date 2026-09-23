package secrets

import (
	"context"
	"errors"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDefaultSecretPolicyRejectsInvalidSettings(t *testing.T) {
	for _, values := range []platformconfig.Values{
		{"ignore_default_secret_api": "true"}, {"default_secret_keys": "key"},
		{"default_secret_keys": []any{"bad-name"}}, {"default_secret_keys": []any{3}}, {"default_secret_keys": make([]any, 101)},
	} {
		if _, err := parseDefaultSecretPolicy(values); err == nil {
			t.Fatalf("accepted invalid policy %v", values)
		}
	}
}
func TestDefaultSecretPolicyHeaderOnlyControlsSuppression(t *testing.T) {
	for _, hidden := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			for _, supplied := range []string{"", "wrong", "test-header"} {
				policy, err := parseDefaultSecretPolicy(platformconfig.Values{"default_secret_keys": []any{"protected"}, "ignore_default_secret_api": enabled})
				if err != nil {
					t.Fatal(err)
				}
				vault := vaultData{Secrets: map[string]string{}, HiddenSecrets: map[string]string{}}
				if hidden {
					vault.HiddenSecrets[SecretsHeaderValueName] = "test-header"
				} else {
					vault.Secrets[SecretsHeaderValueName] = "test-header"
				}
				request := httptest.NewRequest(http.MethodPost, "/", nil)
				request.Header.Set("X-SECRET", supplied)
				want := enabled && supplied != "test-header"
				if got := policy.refuse(httptest.NewRecorder(), request, vault, "ordinary", "protected"); got != want {
					t.Fatalf("hidden=%v enabled=%v header=%q refusal=%v", hidden, enabled, supplied, got)
				}
				if policy.refuse(httptest.NewRecorder(), request, vault, "ordinary") {
					t.Fatal("ordinary name refused")
				}
			}
		}
	}
}
func TestDefaultSecretPolicyFailureStopsHandlersBeforeVaultAccess(t *testing.T) {
	handler := NewHandler(nil)
	handler.defaultSecretPolicy = func(context.Context) (defaultSecretPolicy, error) {
		return defaultSecretPolicy{}, errors.New("private database detail")
	}
	for _, endpoint := range []http.HandlerFunc{handler.List, handler.Get, handler.Create, handler.Update, handler.Delete} {
		response := httptest.NewRecorder()
		endpoint(response, httptest.NewRequest(http.MethodGet, "/", nil))
		if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "private") {
			t.Fatalf("status=%d body=%s", response.Code, response.Body)
		}
	}
}
