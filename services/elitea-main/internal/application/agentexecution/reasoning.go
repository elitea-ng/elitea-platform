package agentexecution

import (
	"context"
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
//  2. A stored setting the model's control form cannot honour (off on a
//     model that always thinks, a level on one that cannot think, a
//     non-reasoning model included) runs as auto and is logged at warn with
//     the model (configurations.RunReasoningEffort, the one run-time rule of
//     every surface). The save refuses it; the run never does, because the
//     value was stored before the model or the rule changed.
//  3. It freezes the control form and the tool-step rule into the version as
//     `model_reasoning`, so the worker translates the setting for this model
//     and applies the `auto` policy. The worker reads it from the execution
//     input (field 69) for the top-level agent and from the nested version
//     for an agent tool. A model that cannot think freezes fixed_off, so auto
//     sends nothing there. A reasoning model that declares no form freezes
//     nothing, and the worker keeps the form it used before.
//
// The worker must understand field 69 before Main sends it: workers are
// upgraded before Main (docs/UPGRADING.md).

const currentModelReasoningKey = "model_reasoning"

func freezeCurrentModelReasoning(
	ctx context.Context,
	version map[string]any,
	settings map[string]any,
	model configurationapp.CurrentModelCatalogItem,
) error {
	// An authored value never reaches the worker: the catalogue owns it.
	delete(version, currentModelReasoningKey)

	control := configurationapp.ModelItemReasoningControl(&model)
	if effort, isString := settings["reasoning_effort"].(string); isString && effort != "" {
		settings["reasoning_effort"] = configurationapp.RunReasoningEffort(ctx, control, effort, model.Name, "agent")
	}
	if snapshot := currentModelReasoningSnapshot(model); snapshot != nil {
		version[currentModelReasoningKey] = snapshot
	}
	return nil
}

// currentModelReasoningSnapshot is the catalogue's reasoning data as the
// runtime message: the declared or family form, fixed_off for a model that
// cannot think; nil for a reasoning model that declares no form.
func currentModelReasoningSnapshot(model configurationapp.CurrentModelCatalogItem) *runtimev1.ModelReasoningV1 {
	control := configurationapp.ModelItemReasoningControl(&model)
	if control == "" {
		return nil
	}
	return &runtimev1.ModelReasoningV1{
		Control:   control,
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
