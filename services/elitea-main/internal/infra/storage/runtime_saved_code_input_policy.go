package storage

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// A saved approval surface, never a graph checkpoint or a supplied input schema.
// Private fields prevent callers from minting policy from PreparedJob keys.
type SavedCodeInputPolicy struct {
	types    map[string]string
	selected map[string]bool
	all      bool
}

// Use only exact instructions from immutable root input or the registered owning
// child definition. This function establishes saved membership, not authority.
func OriginalSavedCodeInputPolicy(instructions, yamlSHA256, nodeID string) (SavedCodeInputPolicy, error) {
	return ParseSavedCodeDocument(instructions).SavedCodeInputPolicy(yamlSHA256, nodeID)
}

// SavedCodeInputPolicy is OriginalSavedCodeInputPolicy on the parsed document.
func (d *SavedCodeDocument) SavedCodeInputPolicy(yamlSHA256, nodeID string) (SavedCodeInputPolicy, error) {
	return d.savedCodeInputPolicy(yamlSHA256, nodeID, nil)
}

// mapItemInputKind is a Map item channel a Code worker reads (W3-2): any JSON
// the frozen source holds. It is never a business root.
const mapItemInputKind = "__map_item_json"

// OriginalMapItemCodeInputPolicy is the input policy of a Code node run as the
// worker of Map node mapNodeID: the declared business roots plus the Map's
// item channel (any JSON) and index channel (int), as the Worker's Code
// boundary admits them. The caller has already resolved the Map as the owner
// of nodeID inside its own transaction (FanoutCodeChildOwner).
func OriginalMapItemCodeInputPolicy(instructions, yamlSHA256, nodeID, mapNodeID string) (SavedCodeInputPolicy, error) {
	return ParseSavedCodeDocument(instructions).MapItemCodeInputPolicy(yamlSHA256, nodeID, mapNodeID)
}

// MapItemCodeInputPolicy is OriginalMapItemCodeInputPolicy on the parsed
// document.
func (d *SavedCodeDocument) MapItemCodeInputPolicy(yamlSHA256, nodeID, mapNodeID string) (SavedCodeInputPolicy, error) {
	empty := SavedCodeInputPolicy{}
	nodes, err := d.Nodes()
	if err != nil {
		return empty, ErrContentUnauthorized
	}
	var item, index string
	for _, raw := range nodes.Content {
		fields, err := savedCodeShallowFields(raw)
		if err != nil {
			return empty, ErrContentUnauthorized
		}
		id, err := savedCodeScalar(fields["id"])
		if err != nil || normalizeCodeDebugID(id) != mapNodeID {
			continue
		}
		if item, err = savedCodeScalar(fields["item"]); err != nil {
			return empty, ErrContentUnauthorized
		}
		if index, err = savedCodeScalar(fields["index"]); err != nil {
			return empty, ErrContentUnauthorized
		}
	}
	if !savedCodeStateKey(item) || !savedCodeStateKey(index) || item == index || savedCodeReservedStateKey(item) || savedCodeReservedStateKey(index) {
		return empty, ErrContentUnauthorized
	}
	return d.savedCodeInputPolicy(yamlSHA256, nodeID, map[string]string{item: mapItemInputKind, index: "int"})
}

func (d *SavedCodeDocument) savedCodeInputPolicy(yamlSHA256, nodeID string, local map[string]string) (SavedCodeInputPolicy, error) {
	empty := SavedCodeInputPolicy{}
	raw, err := d.SavedCodePolicy(yamlSHA256, nodeID)
	if err != nil {
		return empty, err
	}
	declaration, err := codeDebugJSONObject(raw)
	if err != nil {
		return empty, err
	}
	selections, ok := declaration["input"].([]any)
	if !ok || len(selections) > 256 {
		return empty, ErrContentRejected
	}
	root, err := d.Root()
	if err != nil {
		return empty, err
	}
	fields, err := savedCodeShallowFields(root)
	if err != nil {
		return empty, err
	}
	types := map[string]string{"input": "str"}
	reducers := map[string]string{}
	if state, present := fields["state"]; present {
		state, err = savedCodeResolvedNode(state)
		if err != nil || state.Kind != yaml.MappingNode || len(state.Content)/2 > 256 {
			return empty, ErrContentRejected
		}
		entries, err := savedCodeShallowFields(state)
		if err != nil {
			return empty, err
		}
		for key, descriptor := range entries {
			if !savedCodeStateKey(key) || savedCodeReservedStateKey(key) {
				return empty, ErrContentRejected
			}
			kind, reducer, hasReducer, err := savedCodeStateDescriptor(descriptor)
			if err != nil || (key == "input" && kind != "str") || (key == "messages" && kind != "list") || ((key == "input" || key == "messages") && hasReducer) {
				return empty, ErrContentRejected
			}
			if key != "messages" {
				types[key] = kind
				if reducer != "" {
					reducers[key] = reducer
				}
			}
		}
	}
	usesNodeErrors, err := d.usesNodeErrors()
	if err != nil {
		return empty, err
	}
	if usesNodeErrors {
		if declaredKind, declared := types["node_errors"]; declared {
			if declaredKind != "list" || reducers["node_errors"] != "append" {
				return empty, ErrContentRejected
			}
		} else {
			types["node_errors"] = "list"
		}
	}
	// A Map's item and index are child-local: they never shadow a business root.
	for key, kind := range local {
		if _, declared := types[key]; declared || key == "messages" {
			return empty, ErrContentRejected
		}
		types[key] = kind
	}
	selected := make(map[string]bool, len(selections))
	for _, raw := range selections {
		key, ok := raw.(string)
		if !ok || selected[key] || (key != "messages" && types[key] == "") {
			return empty, ErrContentRejected
		}
		selected[key] = true
	}
	return SavedCodeInputPolicy{types: types, selected: selected, all: len(selected) == 0 || (len(selected) == 1 && selected["messages"])}, nil
}

// Match the existing CodeStateBoundary: missing approved values are allowed;
// empty selection or exactly [messages] means all present business roots. The
// messages/framework channels themselves are never user input. Nested list/dict
// elements stay opaque; their shapes are not invented or coerced here.
func (p SavedCodeInputPolicy) ValidateInput(raw json.RawMessage) error {
	if p.types == nil || len(raw) == 0 || len(raw) > 512*1024 {
		return ErrContentRejected
	}
	values, err := codeDebugJSONObject(raw)
	if err != nil || len(values) > 256 {
		return ErrContentRejected
	}
	for key, value := range values {
		kind, exists := p.types[key]
		if !exists || (!p.all && !p.selected[key]) || !savedCodeInputValueMatches(kind, value) {
			return ErrContentUnauthorized
		}
	}
	return nil
}

func (d *SavedCodeDocument) usesNodeErrors() (bool, error) {
	nodes, err := d.Nodes()
	if err != nil {
		return false, err
	}
	uses := false
	for _, rawNode := range nodes.Content {
		fields, err := savedCodeShallowFields(rawNode)
		if err != nil {
			return false, err
		}
		rawSettings, present := fields["settings"]
		if !present {
			continue
		}
		settingsNode, err := savedCodeResolvedNode(rawSettings)
		if err != nil || settingsNode.Kind != yaml.MappingNode {
			return false, ErrContentRejected
		}
		if len(settingsNode.Content) == 0 {
			continue
		}
		settingsFields, err := savedCodeShallowFields(settingsNode)
		if err != nil {
			return false, err
		}
		onErrorNode, ok := settingsFields["on_error"]
		if !ok {
			continue
		}
		onError, err := savedCodeScalar(onErrorNode)
		if err != nil {
			return false, ErrContentRejected
		}
		switch onError {
		case "continue", "error_output":
			uses = true
		case "stop":
		default:
			return false, ErrContentRejected
		}
	}
	return uses, nil
}

func savedCodeStateType(node *yaml.Node) (string, error) {
	kind, _, _, err := savedCodeStateDescriptor(node)
	return kind, err
}

func savedCodeStateDescriptor(node *yaml.Node) (string, string, bool, error) {
	node, err := savedCodeResolvedNode(node)
	if err != nil {
		return "", "", false, err
	}
	typeNode := node
	var reducerNode *yaml.Node
	reducerPresent := false
	if node.Kind == yaml.MappingNode {
		fields, err := savedCodeShallowFields(node)
		if err != nil {
			return "", "", false, err
		}
		for key := range fields {
			if key != "type" && key != "value" && key != "reducer" {
				return "", "", false, ErrContentRejected
			}
		}
		typeNode = fields["type"]
		reducerNode, reducerPresent = fields["reducer"]
	}
	rawKind, err := savedCodeScalar(typeNode)
	if err != nil {
		return "", "", false, err
	}
	var kind string
	switch rawKind {
	case "str", "string":
		kind = "str"
	case "int", "number":
		kind = "int"
	case "float", "bool", "list", "dict":
		kind = rawKind
	default:
		return "", "", false, ErrContentRejected
	}
	if !reducerPresent {
		return kind, "", false, nil
	}
	reducerNode, err = savedCodeResolvedNode(reducerNode)
	if err != nil {
		return "", "", false, ErrContentRejected
	}
	if reducerNode.Kind == yaml.ScalarNode && reducerNode.ShortTag() == "!!null" {
		return kind, "", false, nil
	}
	rawReducer, err := savedCodeScalar(reducerNode)
	if err != nil {
		return "", "", false, ErrContentRejected
	}
	switch rawReducer {
	case "overwrite":
		return kind, "", true, nil
	case "append":
		if kind != "list" {
			return "", "", false, ErrContentRejected
		}
		return kind, "append", true, nil
	case "sum_int":
		if kind != "int" {
			return "", "", false, ErrContentRejected
		}
		return kind, "sum_int", true, nil
	case "merge":
		if kind != "dict" {
			return "", "", false, ErrContentRejected
		}
		return kind, "merge", true, nil
	default:
		return "", "", false, ErrContentRejected
	}
}

func savedCodeStateKey(key string) bool {
	return len(key) > 0 && len(key) <= 256 && !strings.ContainsAny(key, "\x00\r\n")
}

// Source parity with compiler::reserved_user_state_key, including the reviewed
// terminal receipt amendment. This is a Code state contract, not an ACL resolver.
func savedCodeReservedStateKey(key string) bool {
	if strings.HasPrefix(key, "__elitea_application_variable_") {
		return true
	}
	switch key {
	case "__elitea_hitl_resume_v1", "__elitea_tool_resume_v1", "__elitea_llm_tool_resume_v1", "__elitea_parallel_resume_v1", "__elitea_parallel_agent_inputs_v1", "__elitea_parallel_llm_inputs_v1", "__elitea_pipeline_node_event_scope_v1", "__elitea_pipeline_terminal_json_producers_v1", "__elitea_pipeline_result_trace_v1", "__elitea_application_task_v1", "__elitea_application_messages_v1", "__elitea_application_result_v1", "__elitea_subgraph_result_v1", "__elitea_subgraph_entry_v1", "__elitea_executed_once_v1", "output", "result", "router_output", "elitea_response", "printer_output", "state_types", "context_info", "hitl_decisions", "hitl_interrupt", "parallel_tasks", "_pipeline_blocked", "session_id", "thread_id", "execution_finished", "chat_history":
		return true
	default:
		return false
	}
}

func savedCodeInputValueMatches(kind string, value any) bool {
	switch kind {
	case "str":
		_, ok := value.(string)
		return ok
	case "int":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		if _, err := number.Int64(); err == nil {
			return true
		}
		_, err := strconv.ParseUint(string(number), 10, 64)
		return err == nil
	case "float":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		parsed, err := number.Float64()
		return err == nil && !math.IsInf(parsed, 0) && !math.IsNaN(parsed)
	case "bool":
		_, ok := value.(bool)
		return ok
	case "list":
		_, ok := value.([]any)
		return ok
	case "dict":
		_, ok := value.(map[string]any)
		return ok
	case mapItemInputKind:
		return true
	default:
		return false
	}
}
