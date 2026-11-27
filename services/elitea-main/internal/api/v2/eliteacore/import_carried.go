package eliteacore

// What an imported agent file carries beside its versions (issue 3137, the
// blueprint format's section 1): evaluation datasets with their cases, memory
// seeds, and toolkits whose credentials never travel.
//
// All three are written through q, the querier runImport writes the agent
// through: the pool for the import route, the one transaction of the team
// import, so a refused team keeps none of them.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/ownership"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// importCarriedDefinitions writes an agent entry's `evaluation.datasets` and
// `memory_seeds` for the agent just created as appID, and answers one message
// for each that could not be written. The entry passed validateAgentEntry, so
// a shape fault here is impossible; each read still reports one rather than
// dropping it.
func (h *Handler) importCarriedDefinitions(
	ctx context.Context, q relationQuerier, s, projectID string, appID int, userID ownership.UserID, entry map[string]any,
) []string {
	var messages []string
	datasets, failure := importedEvaluationDatasets("evaluation", entry["evaluation"])
	if failure != nil {
		messages = append(messages, "unable to import the evaluation datasets: "+failure.Error())
	}
	for _, dataset := range datasets {
		if err := importEvaluationDataset(ctx, q, s, appID, dataset); err != nil {
			messages = append(messages, fmt.Sprintf("unable to import evaluation dataset %q: %s", dataset.name, err.Error()))
		}
	}
	seeds, failure := importedMemorySeeds("memory_seeds", entry["memory_seeds"])
	if failure != nil {
		messages = append(messages, "unable to import the memory seeds: "+failure.Error())
	}
	project, _ := strconv.ParseInt(projectID, 10, 64)
	for position, seed := range seeds {
		if _, err := repos.InsertAgentMemorySeed(ctx, q, project, int64(appID), userID.Int64(), repos.AgentMemorySeed{
			Kind: seed.kind, Content: seed.content, Pinned: seed.pinned,
		}); err != nil {
			messages = append(messages, fmt.Sprintf("unable to import memory seed %d: %s", position, err.Error()))
		}
	}
	return messages
}

// importEvaluationDataset writes one dataset linked to the new agent
// (application_id) and its cases in file order, with source_type `import`:
// the provenance the case vocabulary reserves for exactly this
// (evaluation.CaseSourceImport). It writes the rows the dataset and case
// routes write, under the same rules (validateImportedEvaluation ran them),
// and not through evaluation's repository: that one owns its pool and its own
// transactions, and the team import must keep all of a team or none.
func importEvaluationDataset(ctx context.Context, q relationQuerier, s string, appID int, dataset importedDataset) error {
	input := evaluation.DatasetWriteInput{Name: dataset.name, Description: dataset.description, ApplicationID: &appID}
	input.Normalize()
	var datasetID int
	if err := q.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %s.eval_datasets (name, description, application_id, is_shared)
		VALUES ($1, NULLIF($2, ''), $3, false) RETURNING id`, s),
		input.Name, input.Description, appID).Scan(&datasetID); err != nil {
		return err
	}
	for order, item := range dataset.cases {
		caseInput := item.input
		caseInput.Normalize()
		variables, err := json.Marshal(caseInput.Variables)
		if err != nil {
			return err
		}
		excluded := caseInput.Excluded != nil && *caseInput.Excluded
		if _, err := q.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s.eval_dataset_cases
				(dataset_id, input, variables, expected_output, source_type, order_index, excluded)
			VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7)`, s),
			datasetID, caseInput.Input, string(variables), caseInput.ExpectedOutput,
			evaluation.CaseSourceImport, order, excluded); err != nil {
			return err
		}
	}
	return nil
}

// unboundToolkitSettings is a toolkit's settings with everything that names a
// credential removed, and the sorted names of the keys it removed.
//
// A file never carries a credential to its destination (the blueprint
// format's section 3): the toolkit lands unbound and the person connects it.
// Removed, at any depth:
//
//   - every key the toolkit plane classifies as a secret
//     (toolkits.IsSecretSettingKey, the word rule the read redaction and the
//     credential form share), whatever its value: a `{{secret.NAME}}`
//     reference names a secret of the SOURCE project and cannot resolve here;
//   - every configuration reference: an object that names a saved
//     configuration (`elitea_title`, `configuration_uuid`,
//     `configuration_project_id`, or the frozen-configuration marker), which
//     points at a credential of the source project;
//   - the project-local ids settingsExcludeKeys names (export_markdown.go)
//     and the retired pgvector block.
//
// `selected_tools` and `available_tools` are kept at the top level: the tool
// selection is not a credential.
func unboundToolkitSettings(settings map[string]any) (map[string]any, []string) {
	dropped := map[string]bool{}
	kept := stripCredentials(settings, "", dropped)
	names := make([]string, 0, len(dropped))
	for name := range dropped {
		names = append(names, name)
	}
	sort.Strings(names)
	return kept, names
}

func stripCredentials(settings map[string]any, prefix string, dropped map[string]bool) map[string]any {
	out := make(map[string]any, len(settings))
	for key, value := range settings {
		path := prefix + key
		topLevelSelection := prefix == "" && (key == "selected_tools" || key == "available_tools")
		switch {
		case topLevelSelection:
			out[key] = value
		case key == configurationapp.RetiredPgvectorToolkitSetting,
			toolkits.IsSecretSettingKey(key),
			settingsExcludeKeys[key],
			isConfigurationReference(value):
			dropped[path] = true
		default:
			if nested, isObject := value.(map[string]any); isObject {
				out[key] = stripCredentials(nested, path+".", dropped)
				continue
			}
			out[key] = value
		}
	}
	return out
}

// isConfigurationReference reports whether a settings value is an object that
// names a saved configuration of the source project.
func isConfigurationReference(value any) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for _, key := range []string{"elitea_title", "configuration_uuid", "configuration_project_id", configurationapp.CurrentFrozenConfigurationMarker} {
		if _, present := object[key]; present {
			return true
		}
	}
	return false
}
