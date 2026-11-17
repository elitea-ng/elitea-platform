package configurations

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// #2095: the form's secret picker puts `{{secret.NAME}}` into the unsaved
// payload. The probe must see the redeemed key, not the template string.
func TestUnsavedCheckRedeemsASecretReferenceBeforeTheProbe(t *testing.T) {
	resolver := &recordingStoredResolver{resolved: map[string]any{
		"api_base": "https://api.openai.com/v1", "api_key": "sk-from-the-vault",
	}}
	checker := &recordingChecker{result: ConnectionCheckResult{Success: true, Message: "Connection successful"}}
	handler := NewHandler(nil, WithStoredConfigurationResolver(resolver), WithConnectionChecker(checker))

	recorder := postDelegatedCheck(t, handler, "open_ai",
		`{"api_base":"https://api.openai.com/v1","api_key":"{{secret.openai_key}}"}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", recorder.Code, recorder.Body.String())
	}
	if len(resolver.requests) != 1 || resolver.requests[0].ProjectID != 7 {
		t.Fatalf("resolver requests = %+v, want one for project 7", resolver.requests)
	}
	if len(checker.data) != 1 || checker.data[0]["api_key"] != "sk-from-the-vault" {
		t.Fatalf("the probe received %v, want the redeemed key, not the reference", checker.data)
	}
}

func TestUnsavedCheckWithoutAReferenceNeverTouchesTheResolver(t *testing.T) {
	resolver := &recordingStoredResolver{resolved: map[string]any{}}
	checker := &recordingChecker{result: ConnectionCheckResult{Success: true}}
	handler := NewHandler(nil, WithStoredConfigurationResolver(resolver), WithConnectionChecker(checker))

	postDelegatedCheck(t, handler, "open_ai", `{"api_base":"https://api.openai.com/v1","api_key":"sk-typed"}`)

	if len(resolver.requests) != 0 {
		t.Fatalf("resolver was called %d times for a payload with no reference", len(resolver.requests))
	}
	if len(checker.data) != 1 || checker.data[0]["api_key"] != "sk-typed" {
		t.Fatalf("the probe received %v", checker.data)
	}
}

func TestUnsavedCheckSaysSoWhenTheSecretCannotBeRedeemed(t *testing.T) {
	resolver := &recordingStoredResolver{err: errors.New("vault: not found")}
	checker := &recordingChecker{result: ConnectionCheckResult{Success: true}}
	handler := NewHandler(nil, WithStoredConfigurationResolver(resolver), WithConnectionChecker(checker))

	recorder := postDelegatedCheck(t, handler, "open_ai", `{"api_key":"{{secret.gone}}"}`)

	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "secret could not be resolved") {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if len(checker.data) != 0 {
		t.Fatal("the provider was probed with an unresolved reference")
	}
	if strings.Contains(recorder.Body.String(), "vault: not found") {
		t.Fatal("the resolver's error text reached the browser")
	}
}
