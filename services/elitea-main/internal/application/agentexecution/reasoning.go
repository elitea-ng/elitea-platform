package agentexecution

import (
	"encoding/json"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// reasoning.go — the thinking policy at admission (ADR-0038 decisions 1-3,
// #1452).
//
// Admission does three things with the model's reasoning, and nothing else:
//
//  1. It reads the stored reasoning_effort as the canonical value: "none",
//     the value an agent saved before ADR-0038 holds, becomes "off". A value
//     that is not canonical is left as it is, and the worker refuses it, as
//     before.
//  2. It refuses a setting the model's control form cannot honour (off on a
//     model that always thinks, a level on one that cannot think), with the
//     translation table's own text. The setting is never dropped in silence.
//  3. It freezes the control form and the tool-step rule into the version as
//     `model_reasoning`, so the worker translates the setting for this model
//     and applies the `auto` policy. The worker reads it from the execution
//     input (field 69) for the top-level agent and from the nested version
//     for an agent tool. A model that declares no form freezes nothing, and
//     the worker keeps the form it used before.
//
// The worker must understand field 69 before Main sends it: workers are
// upgraded before Main (docs/UPGRADING.md).

const currentModelReasoningKey = "model_reasoning"

func freezeCurrentModelReasoning(
	version map[string]any,
	settings map[string]any,
	model configurationapp.CurrentModelCatalogItem,
) error {
	// An authored value never reaches the worker: the catalogue owns it.
	delete(version, currentModelReasoningKey)

	control := currentModelReasoningControl(model)
	if effort, isString := settings["reasoning_effort"].(string); isString && effort != "" {
		if canonical, known := configurationapp.CanonicalReasoningEffort(effort); known {
			settings["reasoning_effort"] = canonical
			if err := configurationapp.ReasoningEffortConflict(control, canonical); err != nil {
				return unsupportedStart(err.Error())
			}
		}
	}
	if control == "" {
		return nil
	}
	version[currentModelReasoningKey] = currentModelReasoningSnapshot(model)
	return nil
}

// currentModelReasoningSnapshot is the catalogue's reasoning data as the
// runtime message, or nil when the model declares no control form.
func currentModelReasoningSnapshot(model configurationapp.CurrentModelCatalogItem) *runtimev1.ModelReasoningV1 {
	if model.ReasoningControl == nil {
		return nil
	}
	return &runtimev1.ModelReasoningV1{
		Control:   *model.ReasoningControl,
		ToolSteps: model.ReasoningToolSteps != nil && *model.ReasoningToolSteps,
	}
}

// currentFrozenModelReasoning reads the output of freezeCurrentModelReasoning.
// A version frozen before ADR-0038 carries none.
func currentFrozenModelReasoning(version map[string]any) (*runtimev1.ModelReasoningV1, error) {
	value, exists := version[currentModelReasoningKey]
	if !exists || value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	var reasoning runtimev1.ModelReasoningV1
	if json.Unmarshal(encoded, &reasoning) != nil || !configurationapp.IsReasoningControl(reasoning.Control) {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	return &reasoning, nil
}
