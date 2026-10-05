package configurations

import (
	"errors"
	"net/http"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

// llmModelDescriptionTooLongMessage names the field, so the web form puts the
// error under the Description input (credentialError.ts matches the field key
// in the message).
const llmModelDescriptionTooLongMessage = "description must be text of at most 40 characters"

// applyLLMModelDescriptionRule applies the llm_model description rule
// (application/configurations/model_description.go) to the `data` object of a
// compatibility create or update, in place. It writes the 400 and returns
// false when the value is refused. Any other type passes unchanged.
//
// The compatibility routes store `data` as sent. Without this call a create or
// an update through them would store a description of any length, while the
// reviewed mutation service refuses it.
func applyLLMModelDescriptionRule(w http.ResponseWriter, configType string, data map[string]any) bool {
	if configType != "llm_model" || data == nil {
		return true
	}
	if err := configurationapp.NormalizeLLMModelDescription(data); err != nil {
		message := llmModelDescriptionTooLongMessage
		if errors.Is(err, configurationapp.ErrLLMModelDescriptionHiddenCharacters) {
			message = err.Error()
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   message,
			"message": message,
			"field":   configurationapp.LLMModelDescriptionField,
		})
		return false
	}
	return true
}
