package toolkits

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

type fakeToolkitModels configurationapp.ModelReasoningInfo

func (f fakeToolkitModels) ModelReasoning(context.Context, string, string) (configurationapp.ModelReasoningInfo, error) {
	return configurationapp.ModelReasoningInfo(f), nil
}

// ADR-0038, one rule on every surface: a toolkit's saved level on a model that
// cannot think is refused, flat or inside llm_settings; auto passes.
func TestToolkitReasoningTakesTheOneSaveRule(t *testing.T) {
	h := &Handler{reasoningModels: fakeToolkitModels{Found: true, Control: "fixed_off"}}
	for name, settings := range map[string]map[string]any{
		"flat":   {"llm_model": "gpt-4o", "reasoning_effort": "high"},
		"nested": {"llm_settings": map[string]any{"model_name": "gpt-4o", "reasoning_effort": "low"}},
	} {
		w := httptest.NewRecorder()
		if !h.refuseToolkitReasoning(w, httptest.NewRequest(http.MethodPost, "/", nil), "1", settings) ||
			w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "this model cannot think") {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	if h.refuseToolkitReasoning(w, httptest.NewRequest(http.MethodPost, "/", nil), "1",
		map[string]any{"llm_model": "gpt-4o", "reasoning_effort": "auto"}) {
		t.Fatal("auto on a model that cannot think is admitted")
	}
	always := &Handler{reasoningModels: fakeToolkitModels{Found: true, SupportsReasoning: true, Control: "fixed_on"}}
	w = httptest.NewRecorder()
	if !always.refuseToolkitReasoning(w, httptest.NewRequest(http.MethodPost, "/", nil), "1",
		map[string]any{"llm_model": "qwq", "reasoning_effort": "none"}) {
		t.Fatal("off (stored none) on a model that always thinks is refused")
	}
}
