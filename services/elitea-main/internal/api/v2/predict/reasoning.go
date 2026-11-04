package predict

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// reasoning.go — the thinking setting of a predict_llm or AI-draft call
// (ADR-0038 decisions 1 to 3, #1452).
//
// The caller sends the canonical setting (auto, off, low, medium, high; a
// stored none reads as off). PrepareReasoning reads the model's control form
// from the catalogue and refuses a setting the model cannot honour, before
// anything is sent. The gateway completer then writes the form the llm-wire
// table writes for one request (configurations.SingleCallOpenAIThinking):
// reasoning_effort, chat_template_kwargs.enable_thinking, or nothing.

// ReasoningError refuses a thinking setting: not a canonical value, or one
// the model cannot honour. Its message is the readable reason; a handler
// answers it with 400.
type ReasoningError struct {
	Message string
}

func (e *ReasoningError) Error() string { return e.Message }

// reasoningResolver is a completer that can read the model's control form:
// the default-model resolver.
type reasoningResolver interface {
	resolveReasoning(ctx context.Context, req CompletionRequest) (CompletionRequest, error)
}

// PrepareReasoning canonicalises req's setting and, when completer can read
// the catalogue, resolves the model and its control form and refuses a
// conflict. Call it before dispatch, so a refusal is a 400 before any
// response is written. A request with no setting passes unchanged.
func PrepareReasoning(ctx context.Context, completer Completer, req CompletionRequest) (CompletionRequest, error) {
	if req.ReasoningEffort == "" {
		return req, nil
	}
	canonical, ok := configurationapp.CanonicalReasoningEffort(req.ReasoningEffort)
	if !ok {
		return req, &ReasoningError{Message: configurationapp.ErrInvalidReasoningEffort.Error()}
	}
	req.ReasoningEffort = canonical
	if resolver, ok := completer.(reasoningResolver); ok {
		return resolver.resolveReasoning(ctx, req)
	}
	return req, nil
}

// itemReasoningControl is the control form a catalogue item declares, or
// resolves from the family defaults. A model that says supports_reasoning
// false and declares nothing cannot think: fixed_off. A missing item, or one
// that declares nothing, keeps the form from before ADR-0038 ("").
func itemReasoningControl(item *configurationapp.CurrentModelCatalogItem) string {
	if item == nil {
		return ""
	}
	if item.ReasoningControl != nil {
		return *item.ReasoningControl
	}
	if item.SupportsReasoning != nil && !*item.SupportsReasoning {
		return configurationapp.ReasoningControlFixedOff
	}
	return ""
}

// applyItemReasoning sets req's control form from item and refuses a
// conflict with the table's own text.
func applyItemReasoning(req CompletionRequest, item *configurationapp.CurrentModelCatalogItem) (CompletionRequest, error) {
	req.ReasoningControl = itemReasoningControl(item)
	req.reasoningResolved = true
	if conflict := configurationapp.ReasoningEffortConflict(req.ReasoningControl, req.ReasoningEffort); conflict != nil {
		return req, &ReasoningError{Message: conflict.Error()}
	}
	return req, nil
}

// encodeChatCompletions builds the gateway body. The setting is translated
// with the model's control form; a temperature the form refuses while the
// model thinks is left out, as the Rust callers leave it out.
func encodeChatCompletions(req CompletionRequest, stream bool) ([]byte, error) {
	body := chatCompletionsRequest{
		Model:       req.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      stream,
	}
	if req.ReasoningEffort != "" {
		setting, ok := configurationapp.CanonicalReasoningEffort(req.ReasoningEffort)
		if !ok {
			return nil, &ReasoningError{Message: configurationapp.ErrInvalidReasoningEffort.Error()}
		}
		if conflict := configurationapp.ReasoningEffortConflict(req.ReasoningControl, setting); conflict != nil {
			return nil, &ReasoningError{Message: conflict.Error()}
		}
		form := configurationapp.SingleCallOpenAIThinking(req.ReasoningControl, setting)
		body.ReasoningEffort = form.ReasoningEffort
		if form.EnableThinking != nil {
			body.ChatTemplateKwargs = map[string]any{"enable_thinking": *form.EnableThinking}
		}
		if form.RefusesTemperature {
			body.Temperature = nil
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("predict: encode request: %w", err)
	}
	return raw, nil
}

// IsReasoningError reports a refused thinking setting and its message.
func IsReasoningError(err error) (string, bool) {
	var refusal *ReasoningError
	if errors.As(err, &refusal) {
		return refusal.Message, true
	}
	return "", false
}
