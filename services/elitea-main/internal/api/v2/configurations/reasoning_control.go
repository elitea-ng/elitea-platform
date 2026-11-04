package configurations

import (
	"net/http"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// refuseInvalidReasoningControl checks `data.reasoning_control` and
// `data.reasoning_tool_steps` of an llm_model row that this route writes
// (ADR-0038 decision 3).
//
// The compatibility routes store `data` as sent. Without this check an
// unknown control form would be stored and reported as a success, and the
// catalogue would then serve the model as if it declared nothing. The rule is
// the create normalizer's (application/configurations/reasoning_control.go),
// so the write paths cannot disagree. The global model routes reach this
// check too: they rewrite the body and delegate to the project routes.
func refuseInvalidReasoningControl(configType string, body map[string]any) *configurationWriteFailure {
	if configType != "llm_model" {
		return nil
	}
	data, isObject := body["data"].(map[string]any)
	if !isObject {
		return nil
	}
	if err := configurationapp.ValidateLLMModelReasoning(data); err != nil {
		return &configurationWriteFailure{status: http.StatusBadRequest, message: err.Error()}
	}
	return nil
}
