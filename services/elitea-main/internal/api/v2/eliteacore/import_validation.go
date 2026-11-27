package eliteacore

// Import validation (issue 3137, the blueprint format's section 3).
//
// Both import routes, POST import (ExportImportPost) and POST team_import,
// check EVERY agent entry of the document before anything is written. The
// first entry that fails answers 422 with one body:
//
//	{"error": "applications[0].versions[0].meta.run_caps.max_tool_calls: ...",
//	 "code": "import_invalid", "field": "applications[0].versions[0].meta.run_caps.max_tool_calls",
//	 "rule": "RUN_CAP_ABOVE_TIER", "index": 0, "name": "Research Scout"}
//
// Before this, each check ran inside the write loop: the import route wrote
// the agents before the failing one and answered 207 with the rest of the
// failure on a channel, and several of the version save's checks (the memory,
// context and capabilities blocks, the run caps, the reasoning setting, the
// variable and tag shapes) did not run at all, so an imported file could store
// a version the editor could never have saved and the worker refuses. A file
// is one thing to its author: it is good, or it names the one place it is
// not. `field` is a JSON path into the document so a client can point at it.
//
// The checks are the version save's own (internal/api/v2/applications), run
// against the decoded entry; nothing here restates a rule. `index` counts the
// flat entity list, which for the envelope body is the `applications` array's
// own index (the applications come first), so `applications[i]` in `field`
// and `index` agree.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentmediaapi"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerruncaps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmedia"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentproposal"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentsoul"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/citations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/contextview"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/outputcontract"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/proactivity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolenvelope"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// importInvalidCode is the `code` of every refusal this file writes.
const importInvalidCode = "import_invalid"

// The rules this file names itself. Every other rule is the domain package's
// own code (RUN_CAP_ABOVE_TIER, OUTPUT_CONTRACT_INVALID, proactivity_invalid,
// avatar_pack_invalid, a soul code, a pipeline limit code, ...).
const (
	ruleShape            = "IMPORT_SHAPE_INVALID"
	ruleLLMSettings      = "LLM_SETTINGS_INVALID"
	ruleReasoningEffort  = "REASONING_EFFORT_INVALID"
	ruleMemoryBlock      = "memory_block_invalid"
	ruleContextBlock     = "context_block_invalid"
	ruleCapabilities     = "capabilities_block_invalid"
	ruleCitations        = "CITATIONS_INVALID"
	ruleVariables        = "VARIABLES_INVALID"
	ruleTags             = "TAGS_INVALID"
	ruleEvaluation       = "EVALUATION_INVALID"
	ruleMemorySeed       = "MEMORY_SEED_INVALID"
	ruleRunCapsUnchecked = "RUN_CAPS_UNAVAILABLE"
)

// MaxImportedMemorySeeds bounds one agent's `memory_seeds`. Each seed is a
// pending entry a person must review, so a file cannot bury its owner in a
// review queue; 50 is the most entries one recall can carry
// (agentmemory.MaxRecallEntries).
const MaxImportedMemorySeeds = agentmemory.MaxRecallEntries

// importRefusal is the 422 body.
type importRefusal struct {
	Error string `json:"error"`
	Code  string `json:"code"`
	Field string `json:"field"`
	Rule  string `json:"rule"`
	Index int    `json:"index"`
	Name  string `json:"name"`
}

// fieldError is one failed check, before the entry's index and name are known.
type fieldError struct {
	field   string
	rule    string
	message string
}

func (e *fieldError) Error() string { return e.field + ": " + e.message }

func refuseField(field, rule, message string) *fieldError {
	return &fieldError{field: field, rule: rule, message: message}
}

// writeImportRefusal answers the 422.
func writeImportRefusal(w http.ResponseWriter, refusal *importRefusal) {
	writeJSON(w, http.StatusUnprocessableEntity, refusal)
}

// validateImportEntities checks every agent entry of the document. nil means
// the document may be written. A non-nil error that is not a refusal is a
// source the check could not read (the platform run caps), which the caller
// answers 500: admitting the file without the check would store a cap the
// platform refuses.
func (h *Handler) validateImportEntities(ctx context.Context, projectID string, entities []any) (*importRefusal, error) {
	project, _ := strconv.ParseInt(projectID, 10, 64)
	for index, raw := range entities {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue // reported by runImport on the agents channel, as before
		}
		if entity, _ := entry["entity"].(string); entity == "toolkits" || entity == "skills" {
			continue
		}
		name, _ := entry["name"].(string)
		prefix := fmt.Sprintf("applications[%d]", index)
		failure, err := h.validateAgentEntry(ctx, project, prefix, entry)
		if err != nil {
			return nil, err
		}
		if failure != nil {
			return &importRefusal{
				Error: failure.Error(), Code: importInvalidCode, Field: failure.field,
				Rule: failure.rule, Index: index, Name: name,
			}, nil
		}
	}
	return nil, nil
}

// validateAgentEntry checks one agent entry: each version, then the entry's
// carried definitions.
func (h *Handler) validateAgentEntry(ctx context.Context, project int64, prefix string, entry map[string]any) (*fieldError, error) {
	if versions, ok := entry["versions"].([]any); ok {
		for position, raw := range versions {
			version, isObject := raw.(map[string]any)
			if !isObject {
				continue // runImport reports a version that is not an object
			}
			failure, err := h.validateImportedVersion(ctx, project, fmt.Sprintf("%s.versions[%d]", prefix, position), version)
			if failure != nil || err != nil {
				return failure, err
			}
		}
	}
	if failure := validateImportedEvaluation(prefix+".evaluation", entry["evaluation"]); failure != nil {
		return failure, nil
	}
	return validateImportedMemorySeeds(prefix+".memory_seeds", entry["memory_seeds"]), nil
}

// validateImportedVersion runs the version save's checks on one version.
func (h *Handler) validateImportedVersion(ctx context.Context, project int64, path string, version map[string]any) (*fieldError, error) {
	agentType, _ := version["agent_type"].(string)
	if stored, known := storedAgentType(agentType); known {
		agentType = stored
	}
	instructions, _ := version["instructions"].(string)
	if failure := validateImportedLLMSettings(path+".llm_settings", version["llm_settings"]); failure != nil {
		return failure, nil
	}
	meta, failure := importedMetaObject(path+".meta", version["meta"])
	if failure != nil {
		return failure, nil
	}
	if failure := validateImportedMeta(path+".meta", agentType, meta, instructions); failure != nil {
		return failure, nil
	}
	if failure, err := h.validateImportedRunCaps(ctx, project, path+".meta.run_caps", meta); failure != nil || err != nil {
		return failure, err
	}
	for _, check := range []func() *fieldError{
		func() *fieldError { return validateImportedPipeline(path+".instructions", agentType, instructions) },
		func() *fieldError { return validateImportedSoul(path+".soul", version["soul"]) },
		func() *fieldError { return validateImportedAvatar(path, version) },
		func() *fieldError { return validateImportedVariables(path+".variables", version["variables"]) },
		func() *fieldError { return validateImportedTags(path+".tags", version["tags"]) },
		func() *fieldError {
			return validateImportedArray(path+".conversation_starters", version["conversation_starters"])
		},
	} {
		if failure := check(); failure != nil {
			return failure, nil
		}
	}
	return nil, nil
}

// validateImportedLLMSettings checks the types of the settings the editor
// writes and the reasoning_effort enum (ADR-0038). The model-aware rules of
// the version save (a level on a model that cannot think) are not applied:
// the destination's catalogue decides those at run time, where an imported
// setting the model cannot honour runs as auto.
func validateImportedLLMSettings(path string, raw any) *fieldError {
	if raw == nil {
		return nil
	}
	llm, ok := raw.(map[string]any)
	if !ok {
		return refuseField(path, ruleLLMSettings, "must be an object")
	}
	if value, present := llm["model_name"]; present && value != nil {
		if _, isString := value.(string); !isString {
			return refuseField(path+".model_name", ruleLLMSettings, "must be a string")
		}
	}
	// model_project_id is a string or an integer (LLMSettings in v2.yaml).
	if value, present := llm["model_project_id"]; present && value != nil {
		switch typed := value.(type) {
		case string:
		case float64:
			if typed != float64(int64(typed)) {
				return refuseField(path+".model_project_id", ruleLLMSettings, "must be a string or an integer")
			}
		default:
			return refuseField(path+".model_project_id", ruleLLMSettings, "must be a string or an integer")
		}
	}
	for _, key := range []string{"temperature", "top_p", "max_tokens"} {
		value, present := llm[key]
		if !present || value == nil {
			continue
		}
		number, isNumber := value.(float64)
		if !isNumber || number < 0 {
			return refuseField(path+"."+key, ruleLLMSettings, "must be a non-negative number")
		}
		if key == "max_tokens" && number != float64(int64(number)) {
			return refuseField(path+"."+key, ruleLLMSettings, "must be a whole number")
		}
	}
	if value, present := llm["reasoning_effort"]; present && value != nil && value != "" {
		text, isString := value.(string)
		if _, known := configurationapp.CanonicalReasoningEffort(text); !isString || !known {
			return refuseField(path+".reasoning_effort", ruleReasoningEffort,
				"must be one of: "+strings.Join(configurationapp.ReasoningEfforts(), ", "))
		}
	}
	return nil
}

// importedMetaObject is the version's meta as an object; absent is nil.
func importedMetaObject(path string, raw any) (map[string]any, *fieldError) {
	if raw == nil {
		return nil, nil
	}
	meta, ok := raw.(map[string]any)
	if !ok {
		return nil, refuseField(path, ruleShape, "must be an object")
	}
	return meta, nil
}

// validateImportedMeta runs the meta block checks of checkOutputContracts
// (applications/output_contract.go), in its order.
func validateImportedMeta(path, agentType string, meta map[string]any, instructions string) *fieldError {
	if meta == nil {
		return nil
	}
	if err := citations.Check(meta); err != nil {
		return refuseField(path+"."+citations.MetaKey, ruleCitations, err.Error())
	}
	if _, err := agentmemory.Parse(meta[agentmemory.MetaKey]); err != nil {
		return refuseField(path+"."+agentmemory.MetaKey, ruleMemoryBlock, err.Error())
	}
	if _, err := contextview.Parse(meta[contextview.MetaKey]); err != nil {
		return refuseField(path+"."+contextview.MetaKey, ruleContextBlock, err.Error())
	}
	if err := toolenvelope.Check(meta); err != nil {
		return refuseField(path+"."+toolenvelope.MetaKey, toolenvelope.Code, err.Error())
	}
	if err := proactivity.Check(meta); err != nil {
		return refuseField(path+"."+proactivity.MetaKey, proactivity.Code, err.Error())
	}
	if err := agentproposal.CheckCapabilities(meta); err != nil {
		return refuseField(path+".capabilities", ruleCapabilities, err.Error())
	}
	if err := citations.CheckVersion(agentType, meta, instructions); err != nil {
		var refusal *outputcontract.Error
		if errors.As(err, &refusal) {
			field := path + "." + outputcontract.MetaKey
			if refusal.NodeID != "" {
				field = strings.TrimSuffix(path, ".meta") + ".instructions"
			}
			return refuseField(field, outputcontract.Code, refusal.Error())
		}
		return refuseField(path+"."+citations.MetaKey, ruleCitations, err.Error())
	}
	return nil
}

// validateImportedRunCaps checks meta.run_caps: each cap a positive number in
// its range, and none above the platform's cap for the tier of a NEW agent
// (low), which is what an imported agent is. Without a checker (a composition
// without the platform section) the shape is still checked.
func (h *Handler) validateImportedRunCaps(ctx context.Context, project int64, path string, meta map[string]any) (*fieldError, error) {
	raw, present := meta[workerruncaps.MetaKey]
	if !present || raw == nil {
		return nil, nil
	}
	if _, invalid := workerruncaps.AuthorCaps(raw); len(invalid) > 0 {
		refusal := &workerruncaps.CapRaiseError{Code: workerruncaps.CodeCapInvalid, Cap: firstSorted(invalid)}
		return refuseField(runCapField(path, refusal.Cap), refusal.Code, refusal.Error()), nil
	}
	if h.runCaps == nil {
		return nil, nil
	}
	err := h.runCaps.Check(ctx, project, 0, meta)
	var refusal *workerruncaps.CapRaiseError
	switch {
	case err == nil:
		return nil, nil
	case errors.As(err, &refusal):
		message := fmt.Sprintf("%g is above the %s tier's cap of %g", refusal.Requested, refusal.Tier, refusal.TierCap)
		return refuseField(runCapField(path, refusal.Cap), refusal.Code, message), nil
	default:
		return nil, fmt.Errorf("%s: %w", ruleRunCapsUnchecked, err)
	}
}

func runCapField(path, capName string) string {
	if capName == workerruncaps.MetaKey {
		return path
	}
	return path + "." + capName
}

func firstSorted(values []string) string {
	first := values[0]
	for _, value := range values[1:] {
		if value < first {
			first = value
		}
	}
	return first
}

// validateImportedPipeline applies the save-time pipeline bounds.
func validateImportedPipeline(path, agentType, instructions string) *fieldError {
	if agentType != "pipeline" {
		return nil
	}
	if err := pipelinelimits.Check(instructions); err != nil {
		rule := ruleShape
		var apiErr *apierr.APIError
		if errors.As(err, &apiErr) && apiErr.Code != "" {
			rule = apiErr.Code
		}
		return refuseField(path, rule, err.Error())
	}
	return nil
}

// validateImportedSoul checks the soul against elitea.soul/v1 (16 KiB).
func validateImportedSoul(path string, raw any) *fieldError {
	if _, err := agentsoul.Normalize(raw); err != nil {
		var refused *agentsoul.Error
		if errors.As(err, &refused) {
			return refuseField(path, refused.Code, refused.Message)
		}
		return refuseField(path, ruleShape, err.Error())
	}
	return nil
}

// validateImportedAvatar checks the pack as a version write does and every
// embedded file as an upload is (agentmedia.Inspect: 4 s, 512 px, 2 MiB, the
// type sniffed from the bytes, the name the digest of the bytes).
func validateImportedAvatar(path string, version map[string]any) *fieldError {
	if _, err := agentmedia.NormalizePack(version["avatar_pack"]); err != nil {
		var refused *agentmedia.PackError
		if errors.As(err, &refused) {
			return refuseField(path+".avatar_pack", agentmedia.PackCode, refused.Message)
		}
		return refuseField(path+".avatar_pack", agentmedia.PackCode, err.Error())
	}
	if _, err := agentmediaapi.DecodeEmbeddedMedia(version[agentmediaapi.EmbeddedMediaKey]); err != nil {
		return refuseField(path+"."+agentmediaapi.EmbeddedMediaKey, agentmedia.Code, err.Error())
	}
	return nil
}

// validateImportedVariables checks the variables' shape: an array of objects
// whose name is a non-empty string and whose value, when present, a string.
func validateImportedVariables(path string, raw any) *fieldError {
	if raw == nil {
		return nil
	}
	entries, ok := raw.([]any)
	if !ok {
		return refuseField(path, ruleVariables, "must be an array")
	}
	for position, entry := range entries {
		variable, isObject := entry.(map[string]any)
		field := fmt.Sprintf("%s[%d]", path, position)
		if !isObject {
			return refuseField(field, ruleVariables, "must be an object with a name")
		}
		if name, _ := variable["name"].(string); strings.TrimSpace(name) == "" {
			return refuseField(field+".name", ruleVariables, "must be a non-empty string")
		}
		if value, present := variable["value"]; present && value != nil {
			if _, isString := value.(string); !isString {
				return refuseField(field+".value", ruleVariables, "must be a string")
			}
		}
	}
	return nil
}

// validateImportedTags checks the tags: strings, or objects with a string name.
func validateImportedTags(path string, raw any) *fieldError {
	if raw == nil {
		return nil
	}
	entries, ok := raw.([]any)
	if !ok {
		return refuseField(path, ruleTags, "must be an array")
	}
	for position, entry := range entries {
		switch typed := entry.(type) {
		case string:
			continue
		case map[string]any:
			if _, isString := typed["name"].(string); isString {
				continue
			}
		}
		return refuseField(fmt.Sprintf("%s[%d]", path, position), ruleTags, "must be a string or an object with a string name")
	}
	return nil
}

// validateImportedArray refuses a present value that is not an array.
func validateImportedArray(path string, raw any) *fieldError {
	if raw == nil {
		return nil
	}
	if _, ok := raw.([]any); !ok {
		return refuseField(path, ruleShape, "must be an array")
	}
	return nil
}

// maxImportedDatasets bounds the evaluation datasets one agent entry carries,
// as MaxCasesPerDataset bounds the cases of each.
const maxImportedDatasets = 50

// validateImportedEvaluation checks `evaluation.datasets` against the rules
// of the dataset and case routes (evaluation.DatasetWriteInput and
// CaseWriteInput, MaxCasesPerDataset).
func validateImportedEvaluation(path string, raw any) *fieldError {
	datasets, failure := importedEvaluationDatasets(path, raw)
	if failure != nil {
		return failure
	}
	if len(datasets) > maxImportedDatasets {
		return refuseField(path+".datasets", ruleEvaluation, fmt.Sprintf("an agent file carries at most %d evaluation datasets", maxImportedDatasets))
	}
	for position, dataset := range datasets {
		field := fmt.Sprintf("%s.datasets[%d]", path, position)
		input := evaluation.DatasetWriteInput{Name: dataset.name, Description: dataset.description}
		input.Normalize()
		if err := input.Validate(); err != nil {
			return refuseField(field+".name", ruleEvaluation, err.Error())
		}
		if len(dataset.cases) > evaluation.MaxCasesPerDataset {
			return refuseField(field+".cases", ruleEvaluation,
				fmt.Sprintf("a dataset holds at most %d cases", evaluation.MaxCasesPerDataset))
		}
		for casePosition, item := range dataset.cases {
			if item.failure != "" {
				return refuseField(fmt.Sprintf("%s.cases[%d]", field, casePosition), ruleEvaluation, item.failure)
			}
			check := item.input
			check.Normalize()
			if err := check.Validate(); err != nil {
				return refuseField(fmt.Sprintf("%s.cases[%d]", field, casePosition), ruleEvaluation, err.Error())
			}
		}
	}
	return nil
}

// importedDataset is one dataset of `evaluation.datasets`, as read.
type importedDataset struct {
	name        string
	description string
	cases       []importedCase
}

// importedCase is one case; failure names a shape fault the reader found.
type importedCase struct {
	input   evaluation.CaseWriteInput
	failure string
}

// importedEvaluationDatasets reads `evaluation.datasets` (absent is none).
func importedEvaluationDatasets(path string, raw any) ([]importedDataset, *fieldError) {
	if raw == nil {
		return nil, nil
	}
	section, ok := raw.(map[string]any)
	if !ok {
		return nil, refuseField(path, ruleEvaluation, "must be an object")
	}
	rawDatasets, present := section["datasets"]
	if !present || rawDatasets == nil {
		return nil, nil
	}
	list, ok := rawDatasets.([]any)
	if !ok {
		return nil, refuseField(path+".datasets", ruleEvaluation, "must be an array")
	}
	datasets := make([]importedDataset, 0, len(list))
	for position, entry := range list {
		field := fmt.Sprintf("%s.datasets[%d]", path, position)
		object, isObject := entry.(map[string]any)
		if !isObject {
			return nil, refuseField(field, ruleEvaluation, "must be an object")
		}
		name, nameOK := object["name"].(string)
		if !nameOK {
			return nil, refuseField(field+".name", ruleEvaluation, "must be a string")
		}
		description, _ := object["description"].(string)
		dataset := importedDataset{name: name, description: description}
		if rawCases, has := object["cases"]; has && rawCases != nil {
			caseList, isList := rawCases.([]any)
			if !isList {
				return nil, refuseField(field+".cases", ruleEvaluation, "must be an array")
			}
			for _, rawCase := range caseList {
				dataset.cases = append(dataset.cases, importedCaseFrom(rawCase))
			}
		}
		datasets = append(datasets, dataset)
	}
	return datasets, nil
}

// importedCaseFrom reads one case: input (required), variables, expected_output
// (absent or null is no expected answer), excluded.
func importedCaseFrom(raw any) importedCase {
	object, ok := raw.(map[string]any)
	if !ok {
		return importedCase{failure: "must be an object with an input"}
	}
	input, isString := object["input"].(string)
	if !isString {
		return importedCase{failure: "a case needs an input string"}
	}
	item := importedCase{input: evaluation.CaseWriteInput{Input: input}}
	if value, present := object["variables"]; present && value != nil {
		variables, isObject := value.(map[string]any)
		if !isObject {
			return importedCase{failure: "variables must be an object"}
		}
		item.input.Variables = variables
	}
	if value, present := object["expected_output"]; present && value != nil {
		expected, isText := value.(string)
		if !isText {
			return importedCase{failure: "expected_output must be a string or null"}
		}
		item.input.ExpectedOutput = &expected
	}
	if value, present := object["excluded"]; present && value != nil {
		excluded, isBool := value.(bool)
		if !isBool {
			return importedCase{failure: "excluded must be a boolean"}
		}
		item.input.Excluded = &excluded
	}
	return item
}

// importedMemorySeed is one entry of `memory_seeds`, as read and checked.
type importedMemorySeed struct {
	kind    agentmemory.Kind
	content string
	pinned  bool
}

// validateImportedMemorySeeds checks `memory_seeds`.
func validateImportedMemorySeeds(path string, raw any) *fieldError {
	_, failure := importedMemorySeeds(path, raw)
	return failure
}

// importedMemorySeeds reads `memory_seeds`: each a kind the agent may write
// (fact, preference or episode; never the heartbeat's cursor), content that
// passes the one content rule of every memory write (agentmemory.CheckContent,
// the secret scanner included), and an optional pinned flag.
func importedMemorySeeds(path string, raw any) ([]importedMemorySeed, *fieldError) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, refuseField(path, ruleMemorySeed, "must be an array")
	}
	if len(list) > MaxImportedMemorySeeds {
		return nil, refuseField(path, ruleMemorySeed, fmt.Sprintf("at most %d seeds are allowed", MaxImportedMemorySeeds))
	}
	seeds := make([]importedMemorySeed, 0, len(list))
	for position, entry := range list {
		field := fmt.Sprintf("%s[%d]", path, position)
		object, isObject := entry.(map[string]any)
		if !isObject {
			return nil, refuseField(field, ruleMemorySeed, "must be an object")
		}
		kindText, _ := object["kind"].(string)
		kind := agentmemory.Kind(kindText)
		if !kind.AgentWritable() {
			return nil, refuseField(field+".kind", ruleMemorySeed, "must be fact, preference or episode")
		}
		contentText, _ := object["content"].(string)
		content, err := agentmemory.CheckContent(contentText)
		if err != nil {
			return nil, refuseField(field+".content", ruleMemorySeed, err.Error())
		}
		seed := importedMemorySeed{kind: kind, content: content}
		if value, present := object["pinned"]; present && value != nil {
			pinned, isBool := value.(bool)
			if !isBool {
				return nil, refuseField(field+".pinned", ruleMemorySeed, "must be a boolean")
			}
			seed.pinned = pinned
		}
		seeds = append(seeds, seed)
	}
	return seeds, nil
}

// admitImport runs validateImportEntities for a route and writes the answer
// when the document is refused (422) or the check could not run (500). It
// answers whether the route may go on to write.
func (h *Handler) admitImport(w http.ResponseWriter, r *http.Request, projectID string, entities []any) bool {
	refusal, err := h.validateImportEntities(r.Context(), projectID, entities)
	if err != nil {
		slog.ErrorContext(r.Context(), "import: the document could not be checked", "project_id", projectID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the platform run caps could not be read"})
		return false
	}
	if refusal != nil {
		writeImportRefusal(w, refusal)
		return false
	}
	return true
}
