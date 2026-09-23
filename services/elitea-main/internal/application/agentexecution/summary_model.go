package agentexecution

import (
	"context"
	"encoding/json"
	"math"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

// freezeCurrentSummaryModel replaces authored snapshots with catalogue authority.
// An absent selection retains the worker's isolated same-model summarizer.
func (service *CurrentApplicationToolSnapshotService) freezeCurrentSummaryModel(
	ctx context.Context, projectID int32, requested map[string]any, version map[string]any,
) error {
	delete(version, "summary_model")
	if len(requested) == 0 {
		return nil
	}
	// Stored strategies decode numbers as float64. Normalize through JSON's
	// integer representation before applying the shared model admission rules.
	encodedSelection, encodeErr := json.Marshal(requested)
	var normalized map[string]any
	if encodeErr != nil || decodeCurrentJSON(encodedSelection, &normalized) != nil {
		return unsupportedStart("the summary model settings are invalid")
	}
	requested = normalized
	for key := range requested {
		switch key {
		case "model_name", "model_project_id", "max_tokens", "temperature":
		default:
			return unsupportedStart("summary settings contain an unsupported field")
		}
	}
	name, _ := requested["model_name"].(string)
	owner, ownerSet := positiveCurrentAgentJSONInteger(requested["model_project_id"])
	if name == "" {
		// Output-only settings select the already authorized task model.
		settings, ok := version["llm_settings"].(map[string]any)
		if !ok || requested["model_name"] != nil || requested["model_project_id"] != nil {
			return unsupportedStart("the summary model selection is incomplete")
		}
		name, _ = settings["model_name"].(string)
		owner, ownerSet = positiveCurrentAgentJSONInteger(settings["model_project_id"])
	} else if requested["model_project_id"] != nil && !ownerSet {
		return unsupportedStart("the summary model project is invalid")
	}
	catalog, err := service.models.Get(ctx, configurationapp.CurrentModelCatalogQuery{
		Section: configurationapp.CurrentModelSectionLLM, ProjectID: projectID,
		PublicProjectID: service.publicProjectID, IncludeShared: true,
	})
	if err != nil {
		return err
	}
	selected, found := selectCurrentAgentModel(catalog.Items, name, owner, ownerSet, projectID, service.publicProjectID)
	if !found {
		// A requested summarizer never silently becomes the catalogue default.
		return unsupportedStart("the summary model is not in the authorized project catalogue")
	}
	limitsVersion := map[string]any{}
	if err := freezeCurrentModelContextLimits(limitsVersion, selected); err != nil {
		return err
	}
	limits, err := currentFrozenModelContextLimits(limitsVersion)
	if err != nil {
		return err
	}
	output := min(int64(8192), int64(limits.MaxOutputTokens))
	if value := requested["max_tokens"]; value != nil {
		var ok bool
		output, ok = positiveCurrentAgentJSONInteger(value)
		if !ok || output > math.MaxInt32 || output > int64(limits.MaxOutputTokens) {
			return unsupportedStart("the summary output cap exceeds the model limits")
		}
	}
	settings := map[string]any{
		"model_name": selected.Name, "model_project_id": selected.ProjectID,
		"max_tokens": output, "openai_compatible": selected.OpenAICompatible != nil && *selected.OpenAICompatible,
		"temperature": nil,
	}
	if value := requested["temperature"]; value != nil {
		encoded, err := json.Marshal(value)
		var temperature float64
		if err != nil || json.Unmarshal(encoded, &temperature) != nil || temperature < 0 || temperature > 1 {
			return unsupportedStart("the summary temperature is invalid for the selected model")
		}
		settings["temperature"] = temperature
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return ErrUnsupportedCurrentAgentStart
	}
	version["summary_model"] = &runtimev1.SummaryModelSnapshotV1{LlmSettings: encoded, ModelContextLimits: limits}
	return nil
}

// currentFrozenSummaryModel only reads the output of the authorized freezer.
func currentFrozenSummaryModel(version map[string]any) (*runtimev1.SummaryModelSnapshotV1, error) {
	value, exists := version["summary_model"]
	if !exists {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	var snapshot runtimev1.SummaryModelSnapshotV1
	if json.Unmarshal(encoded, &snapshot) != nil || snapshot.ModelContextLimits == nil || !validJSONObject(snapshot.LlmSettings) {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	return &snapshot, nil
}
