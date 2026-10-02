package conversations

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/contextsettings"
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
