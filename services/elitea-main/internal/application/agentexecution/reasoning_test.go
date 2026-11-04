package agentexecution

import (
	"context"
	"encoding/json"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

func reasoningModel(control string, toolSteps bool) configurationapp.CurrentModelCatalogItem {
	yes := true
	item := configurationapp.CurrentModelCatalogItem{Name: "m", SupportsReasoning: &yes}
	if control != "" {
		item.ReasoningControl = &control
		item.ReasoningToolSteps = &toolSteps
	}
	return item
}

func TestFreezeModelReasoningCanonicalisesAndFreezesTheForm(t *testing.T) {
	version := map[string]any{"model_reasoning": map[string]any{"control": "fixed_off"}}
	settings := map[string]any{"reasoning_effort": "none"}
	if err := freezeCurrentModelReasoning(context.Background(), version, settings, reasoningModel("toggle", true)); err != nil {
		t.Fatal(err)
	}
	if settings["reasoning_effort"] != "off" {
		t.Fatalf("none must be read as off: %v", settings)
	}
	frozen, err := currentFrozenModelReasoning(version)
	if err != nil || frozen == nil || frozen.Control != "toggle" || !frozen.ToolSteps {
		t.Fatalf("the catalogue's form must replace an authored one: %+v %v", frozen, err)
	}
	// The JSON a nested version carries is what the worker reads.
	encoded, _ := json.Marshal(version["model_reasoning"])
	if string(encoded) != `{"control":"toggle","tool_steps":true}` {
		t.Fatalf("nested JSON = %s", encoded)
	}
}

func TestFreezeModelReasoningWithoutAFormFreezesNothing(t *testing.T) {
	version := map[string]any{"model_reasoning": map[string]any{"control": "toggle"}}
	if err := freezeCurrentModelReasoning(context.Background(), version, map[string]any{"reasoning_effort": "auto"}, reasoningModel("", false)); err != nil {
		t.Fatal(err)
	}
	if _, present := version["model_reasoning"]; present {
		t.Fatalf("an undeclared model freezes no form: %v", version)
	}
	if frozen, err := currentFrozenModelReasoning(version); frozen != nil || err != nil {
		t.Fatalf("a version without the key reads as none: %+v %v", frozen, err)
	}
}

// ADR-0038, one rule on every surface: a stored setting the model cannot
// honour runs as auto at admission; it is never refused at run time.
func TestFreezeModelReasoningRunsAConflictAsAuto(t *testing.T) {
	settings := map[string]any{"reasoning_effort": "off"}
	if err := freezeCurrentModelReasoning(context.Background(), map[string]any{}, settings, reasoningModel("fixed_on", false)); err != nil {
		t.Fatalf("a stored conflict must not refuse the run: %v", err)
	}
	if settings["reasoning_effort"] != "auto" {
		t.Fatalf("off on fixed_on runs as auto: %v", settings)
	}

	// A model that cannot think: the stored level runs as auto, and the
	// frozen fixed_off form makes auto send nothing.
	no := false
	plain := configurationapp.CurrentModelCatalogItem{Name: "gpt-4o", SupportsReasoning: &no}
	version := map[string]any{}
	settings = map[string]any{"reasoning_effort": "high"}
	if err := freezeCurrentModelReasoning(context.Background(), version, settings, plain); err != nil {
		t.Fatal(err)
	}
	frozen, err := currentFrozenModelReasoning(version)
	if settings["reasoning_effort"] != "auto" || err != nil || frozen == nil || frozen.Control != "fixed_off" {
		t.Fatalf("settings=%v frozen=%+v err=%v, want auto and fixed_off", settings, frozen, err)
	}
}

func TestCurrentFrozenModelReasoningRefusesAnUnknownForm(t *testing.T) {
	if _, err := currentFrozenModelReasoning(map[string]any{"model_reasoning": map[string]any{"control": "adaptive"}}); err == nil {
		t.Fatal("an unknown form must be refused")
	}
}

func TestAToggleModelKeepsItsTemperature(t *testing.T) {
	settings := map[string]any{"temperature": 0.6, "reasoning_effort": "auto"}
	if currentAgentModelFamilyConflict(settings, reasoningModel("toggle", false)) {
		t.Fatal("a toggle model takes a temperature beside a thinking setting")
	}
	normalizeCurrentAgentModelFamily(settings, reasoningModel("toggle", false))
	if settings["temperature"] != 0.6 || settings["reasoning_effort"] != "auto" {
		t.Fatalf("toggle normalisation dropped a value: %v", settings)
	}

	for _, control := range []string{"effort", "budget", ""} {
		settings = map[string]any{"temperature": 0.6, "reasoning_effort": "auto"}
		if !currentAgentModelFamilyConflict(settings, reasoningModel(control, false)) {
			t.Fatalf("%q: a temperature beside auto is a conflict", control)
		}
		normalizeCurrentAgentModelFamily(settings, reasoningModel(control, false))
		if settings["temperature"] != nil {
			t.Fatalf("%q: the temperature must go: %v", control, settings)
		}
	}

	settings = map[string]any{"temperature": 0.6, "reasoning_effort": "off"}
	if currentAgentModelFamilyConflict(settings, reasoningModel("effort", false)) {
		t.Fatal("off thinks on no step and takes a temperature")
	}
}

// #1574 review: the GPT-5.6 family's off_with_tools reaches the worker.
func TestFreezeModelReasoningCarriesOffWithTools(t *testing.T) {
	model := reasoningModel("effort", false)
	yes := true
	model.ReasoningOffWithTools = &yes
	version := map[string]any{}
	if err := freezeCurrentModelReasoning(context.Background(), version, map[string]any{"reasoning_effort": "auto"}, model); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(version["model_reasoning"])
	if string(encoded) != `{"control":"effort","off_with_tools":true}` {
		t.Fatalf("nested JSON = %s", encoded)
	}
}
