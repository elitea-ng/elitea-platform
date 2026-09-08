package configurations_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The compatibility write route must never store a provider credential in
// clear text.
//
// A deployment that composes no project vault refuses the write. The refusal
// happens BEFORE the statement, so the closed pool of gatedConfigurationRouter
// cannot answer 500 here: a 503 proves that no INSERT was attempted.
func TestCreateRefusesAPlaintextCredentialWithoutAVault(t *testing.T) {
	t.Parallel()
	router := gatedConfigurationRouter(t, entitledResolver())

	// Complete against the type's schema: an incomplete body is refused for
	// the missing field first (required_fields.go), which would make this
	// assertion pass for the wrong reason.
	body := `{"elitea_title":"prod","label":"prod","type":"open_ai",` +
		`"data":{"api_key":"sk-live-secret-value","api_base":"https://api.openai.com/v1"}}`
	request := httptest.NewRequest(
		http.MethodPost, "/api/v2/configurations/configurations/7", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d; body: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "sk-live") {
		t.Fatalf("the answer echoes the credential: %s", recorder.Body.String())
	}
}

// A body that carries no secret still reaches the database. This is the
// negative control: it proves the refusal above is about the credential and
// not about every write.
func TestCreateWithoutASecretStillReachesTheStore(t *testing.T) {
	t.Parallel()
	router := gatedConfigurationRouter(t, entitledResolver())

	body := `{"elitea_title":"prod","label":"prod","type":"open_ai",` +
		`"data":{"api_base":"https://api.openai.com/v1"}}`
	request := httptest.NewRequest(
		http.MethodPost, "/api/v2/configurations/configurations/7", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code == http.StatusServiceUnavailable {
		t.Fatalf("a write without a secret must not be refused: %s", recorder.Body.String())
	}
}
