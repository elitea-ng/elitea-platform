package agentexecution

import (
	"encoding/json"
	"math"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// freezeCurrentSummaryModel replaces authored snapshots with catalogue authority.
// An absent selection retains the worker's isolated same-model summarizer.
func (service *CurrentApplicationToolSnapshotService) freezeCurrentSummaryModel(
	projectID int32, catalog configurationapp.CurrentModelCatalogResponse, requested map[string]any, version map[string]any,
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
	selected, found := selectCurrentAgentModel(catalog.Items, name, owner, ownerSet, projectID, service.publicProjectID)
	if !found {
		// A requested summarizer never silently becomes the catalogue default.
		return unsupportedStart("the summary model is not in the authorized project catalogue")
	}
	limits, err := catalogModelLimits(selected)
	if err != nil {
		return err
	}
	output := defaultFrozenModelOutput(limits)
	if value := requested["max_tokens"]; value != nil {
		var ok bool
		output, ok = positiveCurrentAgentJSONInteger(value)
		if !ok || output > math.MaxInt32 || output > int64(limits.MaxOutputTokens) {
			return unsupportedStart("the summary output cap exceeds the model limits")
		}
	}
	var temperature *float64
	if value := requested["temperature"]; value != nil {
		encoded, err := json.Marshal(value)
		var parsed float64
		if err != nil || json.Unmarshal(encoded, &parsed) != nil || parsed < 0 || parsed > 1 {
			return unsupportedStart("the summary temperature is invalid for the selected model")
		}
		temperature = &parsed
	}
	model, err := frozenCatalogModel(selected, limits, output, temperature)
	if err != nil {
		return ErrUnsupportedCurrentAgentStart
	}
	// The control form lets the worker switch thinking off for a summary
	// (ADR-0038 decision 4); a model that declares none keeps its form.
	version["summary_model"] = &runtimev1.SummaryModelSnapshotV1{
		LlmSettings: model.settings, ModelContextLimits: model.limits, ModelReasoning: model.reasoning,
	}
	return nil
}

// frozenModel is a catalogue model in the shape the worker's
// SummaryModelProfile::admit reads: the summary model and every sub-agent
// model (#1936) are built by frozenCatalogModel, so the two cannot drift.
type frozenModel struct {
	settings  json.RawMessage
	limits    *runtimev1.ModelContextLimitsV1
	reasoning *runtimev1.ModelReasoningV1
}

// frozenModelDefaultOutputCap is the output cap of a frozen summary or
// sub-agent model when the model allows more and nothing asks otherwise.
const frozenModelDefaultOutputCap = 8192

// catalogModelLimits freezes a catalogue model's context limits.
func catalogModelLimits(selected configurationapp.CurrentModelCatalogItem) (*runtimev1.ModelContextLimitsV1, error) {
	limitsVersion := map[string]any{}
	if err := freezeCurrentModelContextLimits(limitsVersion, selected); err != nil {
		return nil, err
	}
	limits, err := currentFrozenModelContextLimits(limitsVersion)
	if err != nil {
		return nil, err
	}
	if limits == nil {
		return nil, unsupportedStart("the model declares no context limits")
	}
	return limits, nil
}

// defaultFrozenModelOutput is min(frozenModelDefaultOutputCap, the model's
// output limit).
func defaultFrozenModelOutput(limits *runtimev1.ModelContextLimitsV1) int64 {
	return min(int64(frozenModelDefaultOutputCap), int64(limits.MaxOutputTokens))
}

// frozenCatalogModel builds the frozen shape. A nil temperature is written
// as null: the key must exist (see resolveCurrentAgentModel).
func frozenCatalogModel(
	selected configurationapp.CurrentModelCatalogItem, limits *runtimev1.ModelContextLimitsV1,
	maxTokens int64, temperature *float64,
) (frozenModel, error) {
	settings := map[string]any{
		"model_name": selected.Name, "model_project_id": selected.ProjectID,
		"max_tokens": maxTokens, "openai_compatible": selected.OpenAICompatible != nil && *selected.OpenAICompatible,
		"temperature": nil,
	}
	if temperature != nil {
		settings["temperature"] = *temperature
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return frozenModel{}, err
	}
	return frozenModel{settings: encoded, limits: limits, reasoning: currentModelReasoningSnapshot(selected)}, nil
}

// validFrozenModel is the check both frozen-model readers apply to what the
// freeze wrote.
func validFrozenModel(settings json.RawMessage, limits *runtimev1.ModelContextLimitsV1) bool {
	return limits != nil && validJSONObject(settings)
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
	if json.Unmarshal(encoded, &snapshot) != nil || !validFrozenModel(snapshot.LlmSettings, snapshot.ModelContextLimits) {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	return &snapshot, nil
}
