package eliteacore

// The `elitea.agent/v1` half of the export (issue 3137, the blueprint
// format's section 1): what an export carries beyond the legacy document, and
// how the markdown rendering writes it.
//
//   - The JSON document gains `evaluation.datasets` on the application entry:
//     the datasets whose application_id is the agent, with their cases. An
//     import writes them back linked to the new agent (import_carried.go).
//   - The markdown front matter always starts with `schema: elitea.agent/v1`
//     and carries reasoning_effort, the version's tag names, every rich meta
//     block verbatim (memory, context, proactivity, tool_envelope,
//     capabilities, run_caps, output_contract, citations), the avatar and the
//     evaluation datasets.
//   - The avatar is `default:<name>` when the pack's only slot is a drawn
//     default still; when the pack names uploaded files it is a map of slot to
//     `avatars/<dir>/<slot>.<ext>`, and the response becomes a ZIP holding the
//     markdown and those files.
//
// Never carried, by design: memory entries (privacy; a file's `memory_seeds`
// is import-only), heartbeat enablement and mandate (a per-install security
// decision), run history and eval runs, and credentials.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentmediaapi"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmedia"
)

// AgentFileSchema is the `schema` key every markdown export writes first.
const AgentFileSchema = "elitea.agent/v1"

// richMetaKeys are the VersionMeta blocks the front matter carries verbatim,
// in the order the format lists them.
var richMetaKeys = []string{
	"memory", "context", "proactivity", "tool_envelope",
	"capabilities", "run_caps", "output_contract", "citations",
}

// exportedEvaluation reads the datasets authored for one application, with
// their cases in order. nil when there are none, so the key is left out.
//
// A lost read refuses the export, the rule every other read of the export
// follows (exportReadFailed): a backup that silently lost an agent's eval
// dataset is the defect #505 names.
func (h *Handler) exportedEvaluation(ctx context.Context, schema, applicationID string) (map[string]any, error) {
	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT d.id, d.name, COALESCE(d.description, ''),
			COALESCE(c.input, ''), COALESCE(c.variables::text, '{}'), c.expected_output, COALESCE(c.excluded, false), c.id IS NOT NULL
		FROM %[1]s.eval_datasets AS d
		LEFT JOIN %[1]s.eval_dataset_cases AS c ON c.dataset_id = d.id
		WHERE d.application_id = $1
		ORDER BY d.id, c.order_index, c.id`, schema), applicationID)
	if err != nil {
		return nil, fmt.Errorf("query the evaluation datasets of application %q in %q: %w", applicationID, schema, err)
	}
	defer rows.Close()

	datasets := make([]map[string]any, 0)
	position := map[int]int{}
	for rows.Next() {
		var datasetID int
		var name, description, input, variablesText string
		var expected *string
		var excluded, hasCase bool
		if err := rows.Scan(&datasetID, &name, &description, &input, &variablesText, &expected, &excluded, &hasCase); err != nil {
			return nil, fmt.Errorf("scan an evaluation dataset of application %q in %q: %w", applicationID, schema, err)
		}
		index, seen := position[datasetID]
		if !seen {
			index = len(datasets)
			position[datasetID] = index
			dataset := map[string]any{"name": name, "cases": make([]map[string]any, 0)}
			if description != "" {
				dataset["description"] = description
			}
			datasets = append(datasets, dataset)
		}
		if !hasCase {
			continue
		}
		var variables map[string]any
		if err := json.Unmarshal([]byte(variablesText), &variables); err != nil {
			return nil, fmt.Errorf("decode the variables of a case of dataset %d in %q: %w", datasetID, schema, err)
		}
		item := map[string]any{"input": input}
		if len(variables) > 0 {
			item["variables"] = variables
		}
		if expected != nil {
			item["expected_output"] = *expected
		}
		if excluded {
			item["excluded"] = true
		}
		datasets[index]["cases"] = append(datasets[index]["cases"].([]map[string]any), item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the evaluation datasets of application %q in %q: %w", applicationID, schema, err)
	}
	if len(datasets) == 0 {
		return nil, nil
	}
	return map[string]any{"datasets": datasets}, nil
}

// setBlueprintModelKeys writes the llm_settings keys of the front matter.
func setBlueprintModelKeys(front *orderedMap, version map[string]any) {
	llm, ok := version["llm_settings"].(map[string]any)
	if !ok {
		return
	}
	front.set("model", llm["model_name"])
	// `is not None`, not `if`: a temperature or top_p of 0 is a real
	// setting, and dropping it would re-default the agent on import.
	front.setAlways("temperature", llm["temperature"])
	front.set("max_tokens", llm["max_tokens"])
	front.setAlways("top_p", llm["top_p"])
	front.set("reasoning_effort", llm["reasoning_effort"])
}

// versionTagNames is the version's tags as the names the format writes.
func versionTagNames(version map[string]any) []any {
	names := make([]any, 0)
	for _, entry := range toAnySlice(version["tags"]) {
		if name, _ := importedTagFrom(entry); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// setRichMetaKeys writes every rich meta block verbatim.
func setRichMetaKeys(front *orderedMap, version map[string]any) {
	meta, ok := version["meta"].(map[string]any)
	if !ok {
		return
	}
	for _, key := range richMetaKeys {
		value, present := meta[key]
		if !present || value == nil {
			continue
		}
		// setAlways for the objects: an object block whose fields are all
		// defaults (`memory: {enabled: false}`) is still the author's choice.
		if object, isObject := value.(map[string]any); isObject && len(object) > 0 {
			front.setAlways(key, object)
			continue
		}
		front.set(key, value)
	}
}

// exportedAvatar renders the version's pack for the front matter, and the
// files a ZIP must carry for it.
//
// A pack whose only slot is a default still is `default:<name>`. Otherwise
// each slot whose file the export embedded becomes `avatars/<dir>/<slot>.<ext>`
// with the bytes beside it. A slot that cannot be written as a file is left
// out: a file of another project the export does not embed (it holds no
// bytes for it), and a video slot, whose poster frame the slot-to-path map
// cannot name, so that an import never meets a video without its poster. A
// map with no still is no avatar at all, because an import would refuse it.
func exportedAvatar(version map[string]any, dir string) (any, []exportedFile) {
	pack, _ := version["avatar_pack"].(map[string]any)
	slots, _ := pack["slots"].(map[string]any)
	if len(slots) == 0 {
		return nil, nil
	}
	still, _ := slots[agentmedia.SlotStill].(map[string]any)
	stillURL, _ := still["url"].(string)
	if len(slots) == 1 && agentmedia.IsDefaultAvatarKey(stillURL) {
		return stillURL, nil
	}
	media := embeddedMediaOf(version)
	paths := newOrderedMap()
	var files []exportedFile
	for _, name := range agentmedia.Slots {
		slot, _ := slots[name].(map[string]any)
		url, _ := slot["url"].(string)
		_, file, extension, ok := agentmedia.ParseURL(url)
		if !ok || extension == "mp4" || extension == "webm" {
			continue
		}
		data, embedded := media[file]
		if !embedded {
			continue
		}
		path := fmt.Sprintf("avatars/%s/%s.%s", dir, name, extension)
		paths.set(name, path)
		files = append(files, exportedFile{name: path, content: string(data)})
	}
	if _, hasStill := paths.values[agentmedia.SlotStill]; !hasStill {
		return nil, nil
	}
	return paths, files
}

// embeddedMediaOf decodes the version's `avatar_media` (file name -> base64),
// which exportedAvatarPack filled from the exporting project's bucket.
func embeddedMediaOf(version map[string]any) map[string][]byte {
	out := map[string][]byte{}
	switch typed := version[agentmediaapi.EmbeddedMediaKey].(type) {
	case map[string]string:
		for file, text := range typed {
			if data, err := base64.StdEncoding.DecodeString(text); err == nil {
				out[file] = data
			}
		}
	case map[string]any:
		for file, value := range typed {
			text, _ := value.(string)
			if data, err := base64.StdEncoding.DecodeString(text); err == nil {
				out[file] = data
			}
		}
	}
	return out
}

// evaluationFrontMatter renders the application's evaluation section with
// each dataset's and case's keys in the format's order.
func evaluationFrontMatter(app map[string]any) any {
	section, _ := app["evaluation"].(map[string]any)
	datasets := toAnySlice(section["datasets"])
	if len(datasets) == 0 {
		return nil
	}
	rendered := make([]any, 0, len(datasets))
	for _, entry := range datasets {
		dataset, _ := entry.(map[string]any)
		block := newOrderedMap()
		block.setAlways("name", dataset["name"])
		block.set("description", dataset["description"])
		cases := make([]any, 0)
		for _, rawCase := range toAnySlice(dataset["cases"]) {
			item, _ := rawCase.(map[string]any)
			caseBlock := newOrderedMap()
			caseBlock.setAlways("input", item["input"])
			caseBlock.set("variables", item["variables"])
			caseBlock.setAlways("expected_output", item["expected_output"])
			caseBlock.set("excluded", item["excluded"])
			cases = append(cases, caseBlock)
		}
		block.setAlways("cases", cases)
		rendered = append(rendered, block)
	}
	evaluation := newOrderedMap()
	evaluation.setAlways("datasets", rendered)
	return evaluation
}

// avatarDirFor is the folder of one version's avatar files inside the ZIP:
// the markdown file's own stem (`research-scout` for the base version,
// `research-scout.v2` for another), so two versions never share a path.
func avatarDirFor(filename string) string {
	stem := strings.TrimSuffix(filename, ".md")
	stem = strings.TrimSuffix(stem, ".agent")
	return strings.TrimSuffix(stem, ".pipeline")
}
