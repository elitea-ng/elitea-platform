package agentexecution

import (
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

// subagentsInternalTool is the agent's sub-agents toggle.
const subagentsInternalTool = "subagents"

// freezeSubagentModels writes the models a sub-agent may name. It writes
// nothing for an agent that cannot start sub-agents. A catalogue model whose
// limits are not valid is left out, never sent with other limits.
//
// The catalogue is the one FreezeCurrentApplicationVersion read (#1936). Each
// name resolves as selectCurrentAgentModel resolves a name with no project
// (this project's model, else the public project's), from one map built in
// a single pass rather than a scan per name.
func (service *CurrentApplicationToolSnapshotService) freezeSubagentModels(
	projectID int32, catalog configurationapp.CurrentModelCatalogResponse, internalTools json.RawMessage, version map[string]any,
) error {
	delete(version, subagentModelsKey)
	if !startsSubagents(internalTools, version) {
		return nil
	}
	selectable := selectableCatalogModels(catalog.Items, projectID, service.publicProjectID)
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
		selected, found := selectable[item.Name]
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

// selectableCatalogModels maps each name to the item
// selectCurrentAgentModel(items, name, 0, false, projectID, publicProjectID)
// returns: the first item of this project with the name, else the first of
// the public project. A name held by neither is absent.
func selectableCatalogModels(items []configurationapp.CurrentModelCatalogItem, projectID, publicProjectID int32) map[string]configurationapp.CurrentModelCatalogItem {
	own := map[string]configurationapp.CurrentModelCatalogItem{}
	public := map[string]configurationapp.CurrentModelCatalogItem{}
	for _, item := range items {
		if item.Name == "" {
			continue
		}
		switch item.ProjectID {
		case projectID:
			if _, set := own[item.Name]; !set {
				own[item.Name] = item
			}
		case publicProjectID:
			if _, set := public[item.Name]; !set {
				public[item.Name] = item
			}
		}
	}
	for name, item := range public {
		if _, set := own[name]; !set {
			own[name] = item
		}
	}
	return own
}

// subagentModel is one catalogue model in the summary model's shape, with
// the summary model's default output cap (frozenCatalogModel).
func subagentModel(selected configurationapp.CurrentModelCatalogItem) (*runtimev1.SubAgentModelV1, bool) {
	limits, err := catalogModelLimits(selected)
	if err != nil {
		return nil, false
	}
	model, err := frozenCatalogModel(selected, limits, defaultFrozenModelOutput(limits), nil)
	if err != nil {
		return nil, false
	}
	return &runtimev1.SubAgentModelV1{
		LlmSettings: model.settings, ModelContextLimits: model.limits, ModelReasoning: model.reasoning,
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
		if model == nil || !validFrozenModel(model.LlmSettings, model.ModelContextLimits) {
			return nil, ErrUnsupportedCurrentAgentStart
		}
	}
	return &runtimev1.SubAgentPolicyV1{Models: models}, nil
}
