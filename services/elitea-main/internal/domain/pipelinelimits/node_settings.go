package pipelinelimits

import (
	"net/http"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// Per-node `settings:` (5b-1). The single source is
// libs/proto/contracts/pipeline-node-settings-v1.json; the Worker compiler
// (services/elitea-worker-rust src/agents/graph/node_settings_definition.rs)
// and the Web editor read the same file and refuse with the same text. The
// Worker's text is not shown to the author, so Main must say it on save.
const (
	// CodeNodeSettingsInvalid is a node whose settings block is refused.
	CodeNodeSettingsInvalid = "PIPELINE_NODE_SETTINGS_INVALID"

	MinNodeTries               = 2
	MaxNodeTries               = 5
	MinWaitBetweenTriesMs      = 100
	MaxWaitBetweenTriesMs      = 300_000
	DefaultWaitBetweenTriesMs  = 1_000
	MinNodeTimeoutMs           = 1_000
	MaxNodeTimeoutMs           = 900_000
	MaxNodeErrors              = 64
	MaxNodeErrorEntryBytes     = 1_024
	MaxNodeErrorObjectBytes    = 512
	MaxExecutedOnceNodes       = 128
	NodeErrorsKey              = "node_errors"
	settingsKey                = "settings"
	settingsErrorOutput        = "error_output"
	settingsContinue           = "continue"
	nodeSettingsDocumentMarker = ""
)

// NodeSettingsRefusalTexts are the contract's refusal texts by key.
var NodeSettingsRefusalTexts = map[string]string{
	"malformed":                     "Its settings are malformed: use a mapping of the known settings with plain values.",
	"family_not_yet":                "Settings are not available for this node type yet.",
	"family_pause":                  "Settings are not available for a node that pauses the run.",
	"fanout_owned":                  "Settings are not available for a node inside a Parallel or Map yet.",
	"with_recovery":                 "Settings cannot be combined with recovery or failure_handler on one node.",
	"retry_not_allowed":             "Retry on fail is not available for this node type.",
	"timeout_not_allowed":           "Timeout is not available for this node type.",
	"max_tries_range":               "Max tries must be between 2 and 5.",
	"wait_range":                    "Wait between tries must be between 100 and 300000 ms.",
	"timeout_range":                 "Timeout must be between 1000 and 900000 ms.",
	"retry_fields_need_retry":       "Max tries and wait between tries need Retry on fail.",
	"error_transition_required":     "On error 'error output' needs an error transition.",
	"error_transition_unexpected":   "An error transition needs On error 'error output'.",
	"error_transition_target":       "The error transition must name END or another node of this pipeline outside any Parallel or Map.",
	"always_output_reducer":         "Always output data cannot write an output that uses the sum_int or merge reducer.",
	"router_continue_needs_default": "On error 'continue' on a Router needs an explicit default output.",
	"node_errors_type":              "The state key node_errors must be a list with no reducer or the append reducer when a node continues on error.",
	"node_errors_nested_mapping":    "An Agent node cannot map node_errors into or out of a nested pipeline.",
}

// settingsFamilies are the node types that admit a block, and the keys each
// admits (the contract's `families`).
var settingsFamilies = map[string][]string{
	"aggregate":      {"on_error", "error_transition", "always_output_data", "execute_once"},
	"router":         {"on_error", "error_transition", "always_output_data", "execute_once"},
	"split_out":      {"on_error", "error_transition", "always_output_data", "execute_once"},
	"state_modifier": {"on_error", "error_transition", "always_output_data", "execute_once"},
}

var pauseFamilies = map[string]struct{}{"hitl": {}, "printer": {}}

// SettingsFamilies returns each admitted node type's settings keys.
func SettingsFamilies() map[string][]string {
	families := make(map[string][]string, len(settingsFamilies))
	for family, keys := range settingsFamilies {
		families[family] = append([]string{}, keys...)
	}
	return families
}

// PauseFamilies returns the node types that never take settings, sorted.
func PauseFamilies() []string { return sortedKeys(pauseFamilies) }

var graphID = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// nodeSettings is one parsed block; the zero value is the default.
type nodeSettings struct {
	retryOnFail      bool
	maxTries         *int64
	wait             *int64
	onError          string
	errorTransition  *string
	alwaysOutputData bool
	executeOnce      bool
	timeoutMs        *int64
}

func (s nodeSettings) usesNodeErrors() bool {
	return s.onError == settingsContinue || s.onError == settingsErrorOutput
}

func settingsRefusal(id, key string) error {
	message := NodeSettingsRefusalTexts[key]
	if id != nodeSettingsDocumentMarker {
		label := "A pipeline node"
		if graphID.MatchString(id) {
			label = "Node " + id
		}
		message = label + ": " + message
	}
	return &apierr.APIError{Status: http.StatusBadRequest, Code: CodeNodeSettingsInvalid, Message: message}
}

// parseNodeSettings mirrors NodeSettings::parse: strict keys, scalar values of
// the declared kind, the bounds and the couplings between fields. It answers
// the refusal key, or "".
func parseNodeSettings(block *yaml.Node) (nodeSettings, string) {
	settings := nodeSettings{onError: "stop"}
	if block.Kind != yaml.MappingNode {
		return settings, "malformed"
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(block.Content); i += 2 {
		key := aliased(block.Content[i]).Value
		value := aliased(block.Content[i+1])
		if seen[key] {
			return settings, "malformed"
		}
		seen[key] = true
		ok := true
		switch key {
		case "retry_on_fail":
			settings.retryOnFail, ok = boolScalar(value)
		case "always_output_data":
			settings.alwaysOutputData, ok = boolScalar(value)
		case "execute_once":
			settings.executeOnce, ok = boolScalar(value)
		case "max_tries":
			settings.maxTries, ok = optionalInt(value)
		case "wait_between_tries_ms":
			settings.wait, ok = optionalInt(value)
		case "timeout_ms":
			settings.timeoutMs, ok = optionalInt(value)
		case "on_error":
			var onError string
			onError, ok = stringScalar(value)
			if ok && onError != "stop" && onError != settingsContinue && onError != settingsErrorOutput {
				ok = false
			}
			settings.onError = onError
		case "error_transition":
			if present(value) {
				var target string
				target, ok = stringScalar(value)
				settings.errorTransition = &target
			}
		default:
			ok = false
		}
		if !ok {
			return settings, "malformed"
		}
	}
	switch {
	case settings.maxTries != nil && (*settings.maxTries < MinNodeTries || *settings.maxTries > MaxNodeTries):
		return settings, "max_tries_range"
	case settings.wait != nil && (*settings.wait < MinWaitBetweenTriesMs || *settings.wait > MaxWaitBetweenTriesMs):
		return settings, "wait_range"
	case settings.timeoutMs != nil && (*settings.timeoutMs < MinNodeTimeoutMs || *settings.timeoutMs > MaxNodeTimeoutMs):
		return settings, "timeout_range"
	case !settings.retryOnFail && (settings.maxTries != nil || settings.wait != nil):
		return settings, "retry_fields_need_retry"
	case settings.onError == settingsErrorOutput && settings.errorTransition == nil:
		return settings, "error_transition_required"
	case settings.onError != settingsErrorOutput && settings.errorTransition != nil:
		return settings, "error_transition_unexpected"
	case settings.errorTransition != nil && *settings.errorTransition != "END" && !graphID.MatchString(*settings.errorTransition):
		return settings, "error_transition_target"
	}
	return settings, ""
}

func boolScalar(node *yaml.Node) (bool, bool) {
	var value bool
	if node.Kind != yaml.ScalarNode || node.ShortTag() != "!!bool" || node.Decode(&value) != nil {
		return false, false
	}
	return value, true
}

// optionalInt reads a non-negative integer scalar, or a YAML null as absent.
func optionalInt(node *yaml.Node) (*int64, bool) {
	if !present(node) {
		return nil, true
	}
	if node.Kind != yaml.ScalarNode || node.ShortTag() != "!!int" {
		return nil, false
	}
	var value int64
	if node.Decode(&value) != nil || value < 0 {
		return nil, false
	}
	return &value, true
}

type declaredSettings struct {
	id       string
	fields   map[string]*yaml.Node
	settings nodeSettings
}

// checkNodeSettings is the save-time admission of every `settings:` block,
// in the Worker compiler's order: each block's shape and bounds (document
// order), the `node_errors` declaration, the nested mapping, then each node
// (by id) against its family, fan-out ownership, the error route, the
// Router default and the reducers. A document without blocks costs one map
// lookup per node.
func checkNodeSettings(declared []map[string]*yaml.Node, state *yaml.Node) error {
	var blocks []declaredSettings
	usesNodeErrors := false
	for _, fields := range declared {
		block := fields[settingsKey]
		if block == nil {
			continue
		}
		id, _ := stringScalar(fields["id"])
		settings, refused := parseNodeSettings(block)
		if refused != "" {
			return settingsRefusal(id, refused)
		}
		usesNodeErrors = usesNodeErrors || settings.usesNodeErrors()
		blocks = append(blocks, declaredSettings{id: id, fields: fields, settings: settings})
	}
	if len(blocks) == 0 {
		return nil
	}
	reducers := stateReducers(state)
	if usesNodeErrors {
		if declaredType, found := stateType(state, NodeErrorsKey); found {
			reducer := reducers[NodeErrorsKey]
			if declaredType != "list" || (reducer != "" && reducer != "append") {
				return settingsRefusal(nodeSettingsDocumentMarker, "node_errors_type")
			}
		}
		for _, fields := range declared {
			if nodeType, _ := stringScalar(fields["type"]); nodeType != "agent" {
				continue
			}
			if mappingHasKey(fields["input_mapping"], NodeErrorsKey) || sequenceHas(fields["output"], NodeErrorsKey) {
				id, _ := stringScalar(fields["id"])
				return settingsRefusal(id, "node_errors_nested_mapping")
			}
		}
	}
	ids := map[string]struct{}{}
	owned := map[string]struct{}{}
	for _, fields := range declared {
		if id, ok := stringScalar(fields["id"]); ok {
			ids[id] = struct{}{}
		}
		nodeType, _ := stringScalar(fields["type"])
		switch nodeType {
		case "map":
			if worker, ok := stringScalar(fields["worker"]); ok {
				owned[worker] = struct{}{}
			}
		case "parallel":
			if branches := fields["branches"]; branches != nil && branches.Kind == yaml.SequenceNode {
				for _, branch := range branches.Content {
					if target := branchNode(aliased(branch)); target != "" {
						owned[target] = struct{}{}
					}
				}
			}
		}
	}
	sort.SliceStable(blocks, func(i, j int) bool { return blocks[i].id < blocks[j].id })
	for _, block := range blocks {
		if err := checkOneNodeSettings(block, ids, owned, reducers); err != nil {
			return err
		}
	}
	return nil
}

func checkOneNodeSettings(block declaredSettings, ids, owned map[string]struct{}, reducers map[string]string) error {
	id, settings, fields := block.id, block.settings, block.fields
	if fields["recovery"] != nil || fields["failure_handler"] != nil {
		return settingsRefusal(id, "with_recovery")
	}
	nodeType, _ := stringScalar(fields["type"])
	admittedKeys, admitted := settingsFamilies[nodeType]
	if _, pause := pauseFamilies[nodeType]; pause {
		return settingsRefusal(id, "family_pause")
	}
	if !admitted {
		return settingsRefusal(id, "family_not_yet")
	}
	if settings.retryOnFail && !contains(admittedKeys, "retry_on_fail") {
		return settingsRefusal(id, "retry_not_allowed")
	}
	if settings.timeoutMs != nil && !contains(admittedKeys, "timeout_ms") {
		return settingsRefusal(id, "timeout_not_allowed")
	}
	if _, isOwned := owned[id]; isOwned {
		return settingsRefusal(id, "fanout_owned")
	}
	if target := settings.errorTransition; target != nil && *target != "END" {
		_, declared := ids[*target]
		_, targetOwned := owned[*target]
		if *target == id || targetOwned || !declared {
			return settingsRefusal(id, "error_transition_target")
		}
	}
	// C5: the raw key, never the parser's END default.
	if nodeType == "router" && settings.onError == settingsContinue && fields["default_output"] == nil {
		return settingsRefusal(id, "router_continue_needs_default")
	}
	if settings.alwaysOutputData {
		if outputs := fields["output"]; outputs != nil && outputs.Kind == yaml.SequenceNode {
			for _, output := range outputs.Content {
				if reducer := reducers[aliased(output).Value]; reducer == "sum_int" || reducer == "merge" {
					return settingsRefusal(id, "always_output_reducer")
				}
			}
		}
	}
	return nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func mappingHasKey(node *yaml.Node, key string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if aliased(node.Content[i]).Value == key {
			return true
		}
	}
	return false
}

func sequenceHas(node *yaml.Node, want string) bool {
	if node == nil || node.Kind != yaml.SequenceNode {
		return false
	}
	for _, item := range node.Content {
		if value, ok := stringScalar(aliased(item)); ok && value == want {
			return true
		}
	}
	return false
}

// stateType is a state key's normalized declared type, as the Worker's
// validate_state reads it: a scalar type name or a mapping's `type`.
func stateType(state *yaml.Node, key string) (string, bool) {
	if state == nil || state.Kind != yaml.MappingNode {
		return "", false
	}
	for i := 0; i+1 < len(state.Content); i += 2 {
		if aliased(state.Content[i]).Value != key {
			continue
		}
		declared := aliased(state.Content[i+1])
		name := declared.Value
		if declared.Kind == yaml.MappingNode {
			name = ""
			for j := 0; j+1 < len(declared.Content); j += 2 {
				if aliased(declared.Content[j]).Value == "type" {
					name = aliased(declared.Content[j+1]).Value
				}
			}
		}
		switch name {
		case "str", "string":
			return "str", true
		case "int", "number":
			return "int", true
		}
		return name, true
	}
	return "", false
}

// stateReducers are the declared non-overwrite reducers by key.
func stateReducers(state *yaml.Node) map[string]string {
	reducers := map[string]string{}
	if state == nil || state.Kind != yaml.MappingNode {
		return reducers
	}
	for i := 0; i+1 < len(state.Content); i += 2 {
		declared := aliased(state.Content[i+1])
		if declared.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(declared.Content); j += 2 {
			if aliased(declared.Content[j]).Value == "reducer" {
				if name := aliased(declared.Content[j+1]).Value; name != "overwrite" {
					reducers[aliased(state.Content[i]).Value] = name
				}
			}
		}
	}
	return reducers
}
