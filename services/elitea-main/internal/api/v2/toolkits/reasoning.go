package toolkits

import (
	"context"
	"net/http"
	"strings"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// reasoning.go — the thinking setting a toolkit saves (ADR-0038, one rule on
// every surface). A toolkit that calls a model (Inventory, DeepWiki) stores
// its model as settings.llm_model and its default thinking setting as
// settings.reasoning_effort, or both inside settings.llm_settings. At save, a
// setting the model cannot honour (a level on a model that cannot think, off
// on one that always thinks) is refused with the table's readable text. At
// run, a stored one runs as auto (providerhost/material.SettleReasoning).

// ReasoningModelCatalog reads a model's reasoning data in one catalogue read.
type ReasoningModelCatalog interface {
	ModelReasoning(ctx context.Context, projectID, name string) (configurationapp.ModelReasoningInfo, error)
}

// WithReasoningModels sets the catalogue reader of the save-time rule. Nil
// skips it.
func WithReasoningModels(models ReasoningModelCatalog) Option {
	return func(h *Handler) { h.reasoningModels = models }
}

// toolkitReasoningSetting reads the model and the thinking setting a
// toolkit's settings carry: the flat keys first, then llm_settings.
func toolkitReasoningSetting(settings map[string]any) (model, effort string) {
	model, _ = settings["llm_model"].(string)
	effort, _ = settings["reasoning_effort"].(string)
	if nested, ok := settings["llm_settings"].(map[string]any); ok {
		if model == "" {
			model, _ = nested["model_name"].(string)
		}
		if effort == "" {
			effort, _ = nested["reasoning_effort"].(string)
		}
	}
	return strings.TrimSpace(model), effort
}

// refuseToolkitReasoning writes the refusal and returns true when the saved
// setting conflicts with the toolkit's model. A failed or empty catalogue
// read admits the save: the run applies the run-time rule.
func (h *Handler) refuseToolkitReasoning(w http.ResponseWriter, r *http.Request, projectID string, settings map[string]any) bool {
	if h.reasoningModels == nil || settings == nil {
		return false
	}
	model, effort := toolkitReasoningSetting(settings)
	if model == "" || effort == "" {
		return false
	}
	canonical, known := configurationapp.CanonicalReasoningEffort(effort)
	if !known {
		return false
	}
	info, err := h.reasoningModels.ModelReasoning(r.Context(), projectID, model)
	if err != nil {
		return false
	}
	if conflict := configurationapp.SaveReasoningConflict(info, canonical); conflict != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": conflict.Error()})
		return true
	}
	return false
}
