package applications

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

type fakeReasoningCatalog struct {
	reasoning bool
	control   string
}

func (f fakeReasoningCatalog) ModelReasoning(context.Context, string, string) (configurationapp.ModelReasoningInfo, error) {
	control := f.control
	if !f.reasoning && control == "" {
		control = configurationapp.ReasoningControlFixedOff
	}
	return configurationapp.ModelReasoningInfo{Found: true, SupportsReasoning: f.reasoning, Control: control}, nil
}

func checkReasoning(h *Handler, llm map[string]any, creating bool) (bool, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	return h.checkReasoningSetting(w, r, "7", llm, creating), w
}

func TestReasoningSettingIsCanonicalAtSave(t *testing.T) {
	h := NewHandler(nil)
	llm := map[string]any{"reasoning_effort": "none"}
	if ok, _ := checkReasoning(h, llm, false); !ok || llm["reasoning_effort"] != "off" {
		t.Fatalf("none must be stored as off: %v", llm)
	}
	ok, w := checkReasoning(h, map[string]any{"reasoning_effort": "minimal"}, false)
	if ok || w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "auto, off, low, medium, high") {
		t.Fatalf("a value that is not canonical must be refused: %d %s", w.Code, w.Body.String())
	}
}

func TestReasoningConflictIsRefusedAtSave(t *testing.T) {
	h := NewHandler(nil).WithReasoningModels(fakeReasoningCatalog{reasoning: true, control: "fixed_on"})
	ok, w := checkReasoning(h, map[string]any{"model_name": "deepseek-reasoner", "reasoning_effort": "off"}, false)
	if ok || !strings.Contains(w.Body.String(), "this model always thinks") {
		t.Fatalf("off on a fixed_on model must be refused: %d %s", w.Code, w.Body.String())
	}
	h = NewHandler(nil).WithReasoningModels(fakeReasoningCatalog{reasoning: true, control: "fixed_off"})
	ok, w = checkReasoning(h, map[string]any{"model_name": "m", "reasoning_effort": "high"}, false)
	if ok || !strings.Contains(w.Body.String(), "this model cannot think") {
		t.Fatalf("a level on a fixed_off model must be refused: %d %s", w.Code, w.Body.String())
	}
}

// ADR-0038, one rule on every surface: a level on a model that does not
// reason at all is refused at save, as on a fixed_off one; auto and off pass.
func TestALevelOnANonReasoningModelIsRefusedAtSave(t *testing.T) {
	h := NewHandler(nil).WithReasoningModels(fakeReasoningCatalog{reasoning: false})
	ok, w := checkReasoning(h, map[string]any{"model_name": "gpt-4o", "reasoning_effort": "high"}, false)
	if ok || w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "this model cannot think") {
		t.Fatalf("a level on a non-reasoning model must be refused: %d %s", w.Code, w.Body.String())
	}
	for _, effort := range []string{"auto", "off"} {
		if ok, _ := checkReasoning(h, map[string]any{"model_name": "gpt-4o", "reasoning_effort": effort}, false); !ok {
			t.Fatalf("%s on a non-reasoning model must pass", effort)
		}
	}
}

func TestANewAgentOnAControllableModelGetsAuto(t *testing.T) {
	h := NewHandler(nil).WithReasoningModels(fakeReasoningCatalog{reasoning: true, control: "toggle"})
	llm := map[string]any{"model_name": "Qwen/Qwen3-8B"}
	if ok, _ := checkReasoning(h, llm, true); !ok || llm["reasoning_effort"] != "auto" {
		t.Fatalf("a new agent takes auto: %v", llm)
	}

	// A version save of an existing agent never adds the key.
	llm = map[string]any{"model_name": "Qwen/Qwen3-8B"}
	if ok, _ := checkReasoning(h, llm, false); !ok || llm["reasoning_effort"] != nil {
		t.Fatalf("an existing agent keeps what it stored: %v", llm)
	}

	// An explicit value wins.
	llm = map[string]any{"model_name": "Qwen/Qwen3-8B", "reasoning_effort": "high"}
	if ok, _ := checkReasoning(h, llm, true); !ok || llm["reasoning_effort"] != "high" {
		t.Fatalf("an explicit value wins: %v", llm)
	}

	// A model that cannot switch, and one that does not reason, get nothing.
	for _, catalog := range []fakeReasoningCatalog{{reasoning: true, control: "fixed_on"}, {reasoning: false}} {
		llm = map[string]any{"model_name": "m"}
		h = NewHandler(nil).WithReasoningModels(catalog)
		if ok, _ := checkReasoning(h, llm, true); !ok || llm["reasoning_effort"] != nil {
			t.Fatalf("%+v: got %v", catalog, llm)
		}
	}
}
