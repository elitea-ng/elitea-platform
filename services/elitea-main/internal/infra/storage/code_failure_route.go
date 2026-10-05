package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	"gopkg.in/yaml.v3"
)

var codeFailureClasses = []string{"dependency_unavailable", "rate_limited", "attempt_timeout", "worker_interrupted", "invalid_configuration", "invalid_input", "invalid_result", "model_output_incomplete", "authentication_denied", "authorization_denied", "sensitive_rejected", "cancelled", "lease_lost", "unknown"}

func codeFailureClassIndex(class string) int {
	for i, v := range codeFailureClasses {
		if class == v {
			return i
		}
	}
	return -1
}

// MatchCodeFailureRoute reproduces only the compiler-owned dedicated handler contract.
// It never runs graph state, evaluates templates, or grants failed Code execution.
func MatchCodeFailureRoute(instructions, yamlSHA, producer string, f *NodeRecoveryFailureRoute) (string, error) {
	if f == nil || f.Schema != "elitea.pipeline.node-recovery-failure-route.v1" || !code.NonzeroDigest(f.RouteID) {
		return "", ErrContentRejected
	}
	policies, err := SavedCodePolicies(instructions)
	if err != nil || CodeDebugSHA256([]byte(instructions)) != yamlSHA {
		return "", ErrContentRejected
	}
	producerRaw, ok := policies[producer]
	if !ok {
		return "", ErrContentRejected
	}
	var declaration struct {
		Recovery json.RawMessage `json:"recovery"`
	}
	if json.Unmarshal(producerRaw, &declaration) != nil {
		return "", ErrContentRejected
	}
	var recovery map[string]json.RawMessage
	if code.Decode(declaration.Recovery, &recovery, 8192) != nil {
		return "", ErrContentRejected
	}
	var route struct {
		Route      string   `json:"route"`
		ErrorInput string   `json:"error_input"`
		Classes    []string `json:"classes"`
	}
	if code.Decode(recovery["on_failure"], &route, 8192) != nil || !code.RequiredFields(recovery["on_failure"], []string{"route", "error_input", "classes"}) || route.Route == producer || !codeDebugNodeID(route.Route) || !codeDebugNodeID(route.ErrorInput) || len(route.Classes) == 0 {
		return "", ErrContentRejected
	}
	handlerRaw, ok := policies[route.Route]
	if !ok {
		return "", ErrContentRejected
	}
	var handler struct {
		Input      []string        `json:"input"`
		Output     []string        `json:"output"`
		Transition *string         `json:"transition"`
		Source     json.RawMessage `json:"code"`
		Dedicated  json.RawMessage `json:"failure_handler"`
	}
	if json.Unmarshal(handlerRaw, &handler) != nil || len(handler.Input) != 1 || handler.Input[0] != route.ErrorInput || len(handler.Output) == 0 || handler.Transition == nil || *handler.Transition != "END" {
		return "", ErrContentRejected
	}
	var source struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	if code.Decode(handler.Source, &source, 2*1024*1024) != nil || source.Type != "fixed" {
		return "", ErrContentRejected
	}
	var dedicated struct {
		ErrorInput string `json:"error_input"`
	}
	if code.Decode(handler.Dedicated, &dedicated, 8192) != nil || !code.RequiredFields(handler.Dedicated, []string{"error_input"}) || dedicated.ErrorInput != route.ErrorInput {
		return "", ErrContentRejected
	}
	root, err := savedCodeRoot(instructions)
	if err != nil {
		return "", ErrContentRejected
	}
	fields, err := savedCodeShallowFields(root)
	if err != nil {
		return "", ErrContentRejected
	}
	entry, err := savedCodeScalar(fields["entry_point"])
	if err != nil || normalizeCodeDebugID(entry) == route.Route {
		return "", ErrContentRejected
	}
	stateFields, err := savedCodeShallowFields(fields["state"])
	if err != nil {
		return "", ErrContentRejected
	}
	kind, err := savedCodeStateType(stateFields[route.ErrorInput])
	if err != nil || kind != "dict" {
		return "", ErrContentRejected
	}
	// Root scope admits neither a Map worker nor a fixed Parallel participant as handler.
	if _, err := OriginalRootSavedCodePolicy(instructions, yamlSHA, route.Route); err != nil {
		return "", ErrContentRejected
	}
	nodes, err := savedCodeNodes(instructions)
	if err != nil {
		return "", ErrContentRejected
	}
	budget := savedCodeYAMLBudget{}
	for _, rawNode := range nodes.Content {
		value, err := decodeSavedCodeYAMLValue(rawNode, 0, &budget, map[*yaml.Node]bool{})
		node, ok := value.(map[string]any)
		if err != nil || !ok {
			return "", ErrContentRejected
		}
		id, _ := node["id"].(string)
		id = normalizeCodeDebugID(id)
		nodeType, _ := node["type"].(string)
		if codeNodeSuccessRoutesContain(node, nodeType, route.Route) {
			return "", ErrContentRejected
		}
		// The compiler exposes no input keys on Map/Parallel/Printer, and no output
		// keys on Decision/HITL/Printer/Router. Stable branch IDs are never routes.
		if nodeType != "map" && nodeType != "parallel" && nodeType != "printer" && (containsCodeRoute(node["input"], route.ErrorInput) || containsCodeRoute(node["decisional_inputs"], route.ErrorInput)) && id != route.Route {
			return "", ErrContentRejected
		}
		if nodeType != "decision" && nodeType != "hitl" && nodeType != "printer" && nodeType != "router" && containsCodeRoute(node["output"], route.ErrorInput) {
			return "", ErrContentRejected
		}
	}

	classes := map[string]bool{}
	for _, class := range route.Classes {
		index := codeFailureClassIndex(class)
		if index < 0 || index >= 8 && index <= 12 {
			return "", ErrContentRejected
		}
		classes[class] = true
	}
	if !classes[f.Failed.FailureClass] {
		return "", ErrContentRejected
	}
	route.Classes = route.Classes[:0]
	for class := range classes {
		route.Classes = append(route.Classes, class)
	}
	sort.Slice(route.Classes, func(i, j int) bool {
		return codeFailureClassIndex(route.Classes[i]) < codeFailureClassIndex(route.Classes[j])
	})
	// Tuple and struct declaration order match Worker46 RawErrorRoute/RawFailureHandler.
	var raw bytes.Buffer
	encoder := json.NewEncoder(&raw)
	encoder.SetEscapeHTML(false)
	if encoder.Encode([]any{producer, route, dedicated}) != nil {
		return "", ErrContentRejected
	}
	exact := bytes.TrimSuffix(raw.Bytes(), []byte{'\n'})
	digest := sha256.Sum256(append([]byte("elitea.pipeline.node-error-route.v1\x00"), exact...))
	if hex.EncodeToString(digest[:]) != f.RouteID {
		return "", ErrContentRejected
	}
	return route.Route, nil
}
func containsCodeRoute(value any, target string) bool {
	switch v := value.(type) {
	case string:
		return normalizeCodeDebugID(v) == target
	case []any:
		for _, part := range v {
			if containsCodeRoute(part, target) {
				return true
			}
		}
	case map[string]any:
		for _, part := range v {
			if containsCodeRoute(part, target) {
				return true
			}
		}
	}
	return false
}

// Mirrors PipelineNodeDefinition::route_targets, not every string in a node.
func codeNodeSuccessRoutesContain(node map[string]any, nodeType, target string) bool {
	switch nodeType {
	case "decision":
		return containsCodeRoute(node["nodes"], target) || containsCodeRoute(node["default_output"], target)
	case "router":
		return containsCodeRoute(node["routes"], target) || containsCodeRoute(node["default_output"], target)
	case "hitl":
		return containsCodeRoute(node["routes"], target)
	default:
		return containsCodeRoute(node["transition"], target)
	}
}
