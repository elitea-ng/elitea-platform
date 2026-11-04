package applications

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// reasoning.go — the thinking setting of an agent version, at save time
// (ADR-0038 decisions 1 and 3, #1452).
//
//   - reasoning_effort must be a canonical value: auto, off, low, medium or
//     high. The none of an older client is stored as off.
//   - A setting the model cannot honour (off on a model that always thinks,
//     a level on one that cannot think) is refused with the translation
//     table's own text. It is never stored to be dropped later.
//   - A NEW agent on a reasoning model that can switch its thinking gets auto
//     when the body names no setting. An existing agent keeps what it stored:
//     a version save never adds the key.

// ReasoningModelCatalog reads a model's reasoning metadata from the caller's
// catalogue. The API composition passes the chat defaults reader.
type ReasoningModelCatalog interface {
	SupportsReasoning(ctx context.Context, projectID, name string) (bool, error)
	ReasoningControl(ctx context.Context, projectID, name string) (string, error)
}

// WithReasoningModels sets the catalogue reader of the save-time check. Nil
// keeps the value check and skips the model-aware rules.
func (h *Handler) WithReasoningModels(models ReasoningModelCatalog) *Handler {
	h.reasoningModels = models
	return h
}

// checkReasoningSetting applies the rules above to the body's llm_settings,
// rewriting reasoning_effort in place. It writes the refusal and returns false
// when the save must stop.
func (h *Handler) checkReasoningSetting(
	w http.ResponseWriter, r *http.Request, projectID string, llm map[string]any, creating bool,
) bool {
	if llm == nil {
		return true
	}
	effort := ""
	if raw, present := llm["reasoning_effort"]; present && raw != nil && raw != "" {
		text, isString := raw.(string)
		canonical, known := configurationapp.CanonicalReasoningEffort(text)
		if !isString || !known {
			apierr.Write(w, apierr.BadRequest(
				"reasoning_effort must be one of: "+strings.Join(configurationapp.ReasoningEfforts(), ", ")))
			return false
		}
		effort = canonical
		llm["reasoning_effort"] = canonical
	}
	if h.reasoningModels == nil {
		return true
	}
	name, _ := llm["model_name"].(string)
	if name == "" {
		return true
	}
	modelProject := projectID
	if value := llm["model_project_id"]; value != nil && fmt.Sprint(value) != "" {
		modelProject = fmt.Sprint(value)
	}
	// A failed read admits the save: admission checks the frozen setting
	// against the catalogue again, and refuses a conflict there.
	reasoning, err := h.reasoningModels.SupportsReasoning(r.Context(), modelProject, name)
	if err != nil || !reasoning {
		return true
	}
	control, err := h.reasoningModels.ReasoningControl(r.Context(), modelProject, name)
	if err != nil {
		return true
	}
	if effort == "" && creating &&
		control != configurationapp.ReasoningControlFixedOn && control != configurationapp.ReasoningControlFixedOff {
		llm["reasoning_effort"] = configurationapp.ReasoningEffortAuto
		return true
	}
	if conflict := configurationapp.ReasoningEffortConflict(control, effort); conflict != nil {
		apierr.Write(w, apierr.BadRequest(conflict.Error()))
		return false
	}
	return true
}
