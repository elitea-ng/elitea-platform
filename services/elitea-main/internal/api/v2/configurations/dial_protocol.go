package configurations

import (
	"net/http"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

// refuseInvalidDialProtocol checks `data.dial_protocol` of an llm_model row
// that this route writes (legacy issue #6707).
//
// The compatibility routes store `data` as sent, so without this check an
// unknown value, or the openai protocol on a Claude model, would be stored and
// reported as a success. The gateway reads an unknown value as the default
// protocol, which is not what the operator chose, and DIAL refuses a Claude
// model on the openai route. Both are refused here, while the operator is
// still looking at the form.
//
// The rule is the one the create normalizer applies
// (application/configurations/dial_protocol.go), so the write paths cannot
// disagree. Create and update both call it by the body's `type` alone, whatever
// section the row is filed under. Only an llm_model row is checked: the field
// is not part of any other type, and a body without `data` writes no column.
func refuseInvalidDialProtocol(configType string, body map[string]any) *configurationWriteFailure {
	if configType != "llm_model" {
		return nil
	}
	data, isObject := body["data"].(map[string]any)
	if !isObject {
		return nil
	}
	if _, _, err := configurationapp.ValidateLLMModelDialProtocol(data); err != nil {
		return &configurationWriteFailure{status: http.StatusBadRequest, message: err.Error()}
	}
	return nil
}
