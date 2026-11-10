package agentexecution

import (
	"context"
	"encoding/json"
	"slices"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// ADR-0050 decision 4 (#1825): a sub-agent's profile.model may name another
// model of the project catalogue the agent's own model comes from. When the
// agent can start sub-agents, the freeze writes the catalogue's other models
// into the version as subagentModelsKey, each in the summary model's shape;
// the input builders move them to AgentExecutionInputV1.subagent_policy
// (field 77) and admission stamps the depth beside them. The worker binds a
// named model only from this list, so a name the catalogue does not hold for
// this project is refused at spawn.

// subagentModelsKey is the version key the freeze writes. A saved version
// cannot supply it: the freeze deletes whatever the version carried.
const subagentModelsKey = "subagent_models"

// maxSubagentModels is the most models one input names (the worker refuses
// more).
const maxSubagentModels = 64

// subagentModelOutputCap is the output cap of a sub-agent on a named model
// when the model allows more.
const subagentModelOutputCap = 8192

// subagentsInternalTool is the agent's sub-agents toggle.
const subagentsInternalTool = "subagents"

// freezeSubagentModels writes the models a sub-agent may name. It writes
// nothing for an agent that cannot start sub-agents. A catalogue model whose
// limits are not valid is left out, never sent with other limits.
func (service *CurrentApplicationToolSnapshotService) freezeSubagentModels(
	ctx context.Context, projectID int32, internalTools json.RawMessage, version map[string]any,
) error {
	delete(version, subagentModelsKey)
	if !startsSubagents(internalTools, version) {
		return nil
	}
	catalog, err := service.models.Get(ctx, configurationapp.CurrentModelCatalogQuery{
		Section: configurationapp.CurrentModelSectionLLM, ProjectID: projectID,
		PublicProjectID: service.publicProjectID, IncludeShared: true,
	})
	if err != nil {
		return err
	}
	own := ""
	if settings, ok := version["llm_settings"].(map[string]any); ok {
		own, _ = settings["model_name"].(string)
	}
	var models []*runtimev1.SubAgentModelV1
	seen := map[string]bool{own: true}
	for _, item := range catalog.Items {
		if len(models) == maxSubagentModels {
			break
		}
		if item.Name == "" || seen[item.Name] {
			continue
		}
		// The same name in this project and the public one: the project's
		// own wins, as selectCurrentAgentModel picks it.
		selected, found := selectCurrentAgentModel(catalog.Items, item.Name, 0, false, projectID, service.publicProjectID)
		if !found {
			continue
		}
		seen[item.Name] = true
		model, ok := subagentModel(selected)
		if ok {
			models = append(models, model)
		}
	}
	if len(models) > 0 {
		version[subagentModelsKey] = models
	}
	return nil
}

// subagentModel is one catalogue model in the summary model's shape.
func subagentModel(selected configurationapp.CurrentModelCatalogItem) (*runtimev1.SubAgentModelV1, bool) {
	limitsVersion := map[string]any{}
	if freezeCurrentModelContextLimits(limitsVersion, selected) != nil {
		return nil, false
	}
	limits, err := currentFrozenModelContextLimits(limitsVersion)
	if err != nil || limits == nil {
		return nil, false
	}
	settings := map[string]any{
		"model_name": selected.Name, "model_project_id": selected.ProjectID,
		"max_tokens":        min(int64(subagentModelOutputCap), int64(limits.MaxOutputTokens)),
		"openai_compatible": selected.OpenAICompatible != nil && *selected.OpenAICompatible,
		"temperature":       nil,
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return nil, false
	}
	return &runtimev1.SubAgentModelV1{
		LlmSettings: encoded, ModelContextLimits: limits,
		ModelReasoning: currentModelReasoningSnapshot(selected),
	}, true
}

// startsSubagents reports the sub-agents toggle: the application's internal
// tools, or an ad-hoc chat's meta.internal_tools.
func startsSubagents(internalTools json.RawMessage, version map[string]any) bool {
	var names []string
	if len(internalTools) > 0 && json.Unmarshal(internalTools, &names) == nil &&
		slices.Contains(names, subagentsInternalTool) {
		return true
	}
	meta, _ := version["meta"].(map[string]any)
	configured, _ := meta["internal_tools"].([]any)
	return slices.Contains(configured, any(subagentsInternalTool))
}

// currentFrozenSubagentPolicy reads the freeze's models into the input
// field; nil when the freeze wrote none. Admission stamps the depth.
func currentFrozenSubagentPolicy(version map[string]any) (*runtimev1.SubAgentPolicyV1, error) {
	value, exists := version[subagentModelsKey]
	if !exists {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	var models []*runtimev1.SubAgentModelV1
	if json.Unmarshal(encoded, &models) != nil || len(models) > maxSubagentModels {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	for _, model := range models {
		if model == nil || model.ModelContextLimits == nil || !validJSONObject(model.LlmSettings) {
			return nil, ErrUnsupportedCurrentAgentStart
		}
	}
	return &runtimev1.SubAgentPolicyV1{Models: models}, nil
}
