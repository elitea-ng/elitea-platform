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
	empty := SavedCodeInputPolicy{}
	raw, err := OriginalSavedCodePolicy(instructions, yamlSHA256, nodeID)
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
	root, err := savedCodeRoot(instructions)
	if err != nil {
		return empty, err
	}
	fields, err := savedCodeShallowFields(root)
	if err != nil {
		return empty, err
	}
	types := map[string]string{"input": "str"}
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
			kind, err := savedCodeStateType(descriptor)
			if err != nil || (key == "input" && kind != "str") || (key == "messages" && kind != "list") {
				return empty, ErrContentRejected
			}
			if key != "messages" {
				types[key] = kind
			}
		}
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

func savedCodeStateType(node *yaml.Node) (string, error) {
	node, err := savedCodeResolvedNode(node)
	if err != nil {
		return "", err
	}
	if node.Kind == yaml.MappingNode {
		fields, err := savedCodeShallowFields(node)
		if err != nil {
			return "", err
		}
		for key := range fields {
			if key != "type" && key != "value" {
				return "", ErrContentRejected
			}
		}
		node = fields["type"]
	}
	kind, err := savedCodeScalar(node)
	if err != nil {
		return "", err
	}
	switch kind {
	case "str", "string":
		return "str", nil
	case "int", "number":
		return "int", nil
	case "float", "bool", "list", "dict":
		return kind, nil
	default:
		return "", ErrContentRejected
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
	case "__elitea_hitl_resume_v1", "__elitea_tool_resume_v1", "__elitea_llm_tool_resume_v1", "__elitea_parallel_resume_v1", "__elitea_parallel_agent_inputs_v1", "__elitea_pipeline_node_event_scope_v1", "__elitea_pipeline_terminal_json_producers_v1", "__elitea_application_task_v1", "__elitea_application_messages_v1", "__elitea_application_result_v1", "__elitea_subgraph_result_v1", "__elitea_subgraph_entry_v1", "output", "result", "router_output", "elitea_response", "printer_output", "state_types", "context_info", "hitl_decisions", "hitl_interrupt", "parallel_tasks", "_pipeline_blocked", "session_id", "thread_id", "execution_finished", "chat_history":
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
	default:
		return false
	}
}
