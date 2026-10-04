package configurations

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

// dial_protocol_test.go — legacy issue #6707 on the compatibility write routes.

func dialModelBody(name string, protocol any) map[string]any {
	return map[string]any{
		"elitea_title": "autotest_dial_model",
		"label":        "autotest dial model",
		"type":         "llm_model",
		"data": map[string]any{
			"name":           name,
			"ai_credentials": map[string]any{"elitea_title": "epam_dial", "private": false},
			"dial_protocol":  protocol,
		},
	}
}

func createWithPinnedCatalog(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	catalog, err := configurationapp.LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatalf("LoadPinnedCurrentAvailableCatalog() error = %v", err)
	}
	// No pool: a body that passes every check reaches the store and answers
	// 503. That is the control which proves the 400 below came from the rule.
	handler := &Handler{catalog: catalog}
	raw, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(raw)))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", "7")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
	recorder := httptest.NewRecorder()
	handler.Create(recorder, request)
	return recorder
}

func TestCreateRefusesAnInvalidDialProtocol(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"unknown value":    dialModelBody("gpt-5", "bedrock"),
		"not a string":     dialModelBody("gpt-5", 3),
		"openai on claude": dialModelBody("claude-sonnet-4-5", "openai"),
		"anthropic on gpt": dialModelBody("gpt-4o", "anthropic"),
	} {
		t.Run(name, func(t *testing.T) {
			recorder := createWithPinnedCatalog(t, body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "dial_protocol") {
				t.Errorf("the refusal must name dial_protocol: %s", recorder.Body.String())
			}
		})
	}
}

// TestCreateRefusesAnInvalidDialProtocolInAnotherSection: a row filed under a
// section that is not llm_model's own is checked as a ROW by the schema walk,
// but its data.dial_protocol is still stored and still read by the gateway.
// The update path checks by type alone, so the create path must too.
func TestCreateRefusesAnInvalidDialProtocolInAnotherSection(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"unknown value":    dialModelBody("gpt-5", "bogus"),
		"openai on claude": dialModelBody("claude-sonnet-4-5", "openai"),
		"anthropic on gpt": dialModelBody("gpt-4o", "anthropic"),
	} {
		t.Run(name, func(t *testing.T) {
			body["section"] = "ai_credentials"
			recorder := createWithPinnedCatalog(t, body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "dial_protocol") {
				t.Errorf("the refusal must name dial_protocol: %s", recorder.Body.String())
			}
		})
	}
	// The control: the same cross-section row with a valid protocol reaches
	// the store.
	valid := dialModelBody("gpt-5", "openai")
	valid["section"] = "ai_credentials"
	if recorder := createWithPinnedCatalog(t, valid); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the 503 of the absent store; body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestCreateAdmitsEveryDialProtocol(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"azure":         dialModelBody("gpt-5", "azure"),
		"openai on gpt": dialModelBody("gpt-5", "openai"),
		"anthropic":     dialModelBody("claude-sonnet-4-5", "anthropic"),
		"null":          dialModelBody("gpt-5", nil),
	} {
		t.Run(name, func(t *testing.T) {
			recorder := createWithPinnedCatalog(t, body)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want the 503 of the absent store; body = %s",
					recorder.Code, recorder.Body.String())
			}
		})
	}
}

// TestUpdateRuleChecksTheLLMModelDataOnly pins the update half: the rule runs
// on an llm_model body that carries `data`, and on nothing else.
func TestUpdateRuleChecksTheLLMModelDataOnly(t *testing.T) {
	bad := dialModelBody("claude-sonnet-4-5", "openai")
	if failure := refuseInvalidDialProtocol("llm_model", bad); failure == nil ||
		failure.status != http.StatusBadRequest || !strings.Contains(failure.message, "gpt models only") {
		t.Fatalf("failure = %+v, want a 400 that says the openai protocol is gpt-only", failure)
	}
	if failure := refuseInvalidDialProtocol("embedding_model", bad); failure != nil {
		t.Fatalf("embedding_model was checked: %+v", failure)
	}
	if failure := refuseInvalidDialProtocol("llm_model", map[string]any{"label": "rename only"}); failure != nil {
		t.Fatalf("a body without data was checked: %+v", failure)
	}
	if failure := refuseInvalidDialProtocol("llm_model", dialModelBody("gpt-5", "openai")); failure != nil {
		t.Fatalf("a valid protocol was refused: %+v", failure)
	}
}
