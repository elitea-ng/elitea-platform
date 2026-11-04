package agentexecution

import (
	"encoding/json"
	"strings"
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
	if err := freezeCurrentModelReasoning(version, settings, reasoningModel("toggle", true)); err != nil {
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
	if err := freezeCurrentModelReasoning(version, map[string]any{"reasoning_effort": "auto"}, reasoningModel("", false)); err != nil {
		t.Fatal(err)
	}
	if _, present := version["model_reasoning"]; present {
		t.Fatalf("an undeclared model freezes no form: %v", version)
	}
	if frozen, err := currentFrozenModelReasoning(version); frozen != nil || err != nil {
		t.Fatalf("a version without the key reads as none: %+v %v", frozen, err)
	}
}

func TestFreezeModelReasoningRefusesAConflict(t *testing.T) {
	err := freezeCurrentModelReasoning(map[string]any{}, map[string]any{"reasoning_effort": "off"}, reasoningModel("fixed_on", false))
	if err == nil || !strings.Contains(err.Error(), "this model always thinks") {
		t.Fatalf("off on fixed_on must be refused with the table's text: %v", err)
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
