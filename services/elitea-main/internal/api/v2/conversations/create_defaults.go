package conversations

import (
	"context"
	"encoding/json"
	"net/http"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/contextsettings"
)

// ContextManagementGate reads the authoritative project setting without
// disclosing the underlying secret to a chat or an MCP response.
type ContextManagementGate interface {
	ContextManagementEnabled(context.Context, string) (bool, error)
}

// ReasoningModelReader reads model capability metadata, never credentials.
type ReasoningModelReader interface {
	SupportsReasoning(context.Context, string, string) (bool, error)
}

// ReasoningControlReader reads a model's reasoning control form (ADR-0038
// decision 3): "effort", "budget", "toggle", "fixed_on", "fixed_off", or ""
// when the model declares none. A ReasoningModelReader may implement it; one
// that does not reads as "", the rule before ADR-0038.
type ReasoningControlReader interface {
	ReasoningControl(context.Context, string, string) (string, error)
}

func (h *Handler) WithContextManagementGate(gate ContextManagementGate) *Handler {
	h.contextGate = gate
	return h
}
func (h *Handler) WithReasoningModels(reader ReasoningModelReader) *Handler {
	h.reasoningModels = reader
	return h
}
func (h *Handler) applyCreateContextDefaults(r *http.Request, projectID string, conv *Conversation) error {
	if h.contextGate == nil {
		return nil
	}
	enabled, err := h.contextGate.ContextManagementEnabled(r.Context(), projectID)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	strategy := contextsettings.Resolve(nil, h.contextDefaults(r))
	raw, err := json.Marshal(strategy)
	if err != nil {
		return err
	}
	var value map[string]any
	if err = json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if conv.Meta == nil {
		conv.Meta = map[string]any{}
	}
	conv.Meta["context_strategy"] = value
	return nil
}

// refuseParticipantReasoning applies the thinking policy to a chat
// participant's model settings (ADR-0038 decisions 1 and 3). It returns the
// readable refusal, or "" when the settings are admitted.
//
// On a reasoning model:
//   - no reasoning_effort becomes auto, the default of a chat with no agent;
//   - a setting the model's control form cannot honour (off on a model that
//     always thinks, a level on one that cannot think) is refused;
//   - a temperature is refused beside a setting that thinks, unless the form
//     takes one (toggle, fixed_on, fixed_off).
//
// A failed capability read admits the settings, as before: admission checks
// them again against the catalogue.
func (h *Handler) refuseParticipantReasoning(
	ctx context.Context, modelProject, name string, settings map[string]any,
) string {
	reasoning, err := h.reasoningModels.SupportsReasoning(ctx, modelProject, name)
	if err != nil || !reasoning {
		return ""
	}
	control := ""
	if reader, ok := h.reasoningModels.(ReasoningControlReader); ok {
		if value, readErr := reader.ReasoningControl(ctx, modelProject, name); readErr == nil {
			control = value
		}
	}
	effort, _ := settings["reasoning_effort"].(string)
	if effort == "" {
		effort = configurationapp.ReasoningEffortAuto
		settings["reasoning_effort"] = effort
	}
	if conflict := configurationapp.ReasoningEffortConflict(control, effort); conflict != nil {
		return conflict.Error()
	}
	if settings["temperature"] != nil && effort != configurationapp.ReasoningEffortOff &&
		configurationapp.ReasoningControlRefusesTemperature(control) {
		return "this model thinks by effort or budget and takes no temperature: remove the temperature or set reasoning_effort to off"
	}
	return ""
}
