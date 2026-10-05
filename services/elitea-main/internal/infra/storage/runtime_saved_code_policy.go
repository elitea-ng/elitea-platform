package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"reflect"

	"gopkg.in/yaml.v3"
)

// Code declaration inspection does not evaluate graph state or run a graph.
func SavedCodePolicies(instructions string) (map[string][]byte, error) {
	nodes, err := savedCodeNodes(instructions)
	if err != nil {
		return nil, err
	}
	policies := map[string][]byte{}
	total := 0
	for _, node := range nodes.Content {
		fields, err := savedCodeShallowFields(node)
		if err != nil {
			return nil, err
		}
		kind, _ := savedCodeScalar(fields["type"])
		if kind != "code" {
			continue
		}
		id, raw, err := decodeSavedCodePolicy(node)
		if err != nil {
			return nil, err
		}
		if _, present := policies[id]; present {
			return nil, ErrContentRejected
		}
		total += len(raw)
		if total > 16*1024*1024 {
			return nil, ErrContentRejected
		}
		policies[id] = raw
	}
	return policies, nil
}

// Parse one bounded document through the same declaration normalization owner.
func savedCodeRoot(instructions string) (*yaml.Node, error) {
	if len(instructions) == 0 || len(instructions) > 1024*1024 {
		return nil, ErrContentRejected
	}
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewBufferString(instructions))
	if decoder.Decode(&document) != nil || len(document.Content) != 1 {
		return nil, ErrContentRejected
	}
	var trailing yaml.Node
	if decoder.Decode(&trailing) != io.EOF {
		return nil, ErrContentRejected
	}
	root, err := savedCodeResolvedNode(document.Content[0])
	if err != nil || root.Kind != yaml.MappingNode {
		return nil, ErrContentRejected
	}
	if validateDebugYAML(root, 0, new(int)) != nil {
		return nil, ErrContentRejected
	}
	return root, nil
}
func savedCodeNodes(instructions string) (*yaml.Node, error) {
	root, err := savedCodeRoot(instructions)
	if err != nil {
		return nil, err
	}
	fields, err := savedCodeShallowFields(root)
	if err != nil {
		return nil, ErrContentRejected
	}
	nodes, err := savedCodeResolvedNode(fields["nodes"])
	if err != nil || nodes.Kind != yaml.SequenceNode || len(nodes.Content) > 128 {
		return nil, ErrContentRejected
	}
	return nodes, nil
}

// Document validation traverses aliases by identity without materializing them.
// Selected declaration validation expands each occurrence with a strict budget.
type savedCodeYAMLBudget struct{ nodes, bytes int }

func validateDebugYAML(n *yaml.Node, depth int, count *int) error {
	budget := savedCodeYAMLBudget{}
	err := walkSavedCodeYAML(n, depth, &budget, map[*yaml.Node]bool{}, map[*yaml.Node]bool{}, false)
	*count = budget.nodes
	return err
}
func walkSavedCodeYAML(n *yaml.Node, depth int, budget *savedCodeYAMLBudget, active, seen map[*yaml.Node]bool, expand bool) error {
	if n == nil || depth > 64 || active[n] {
		return ErrContentRejected
	}
	if !expand && seen[n] {
		return nil
	}
	budget.nodes++
	budget.bytes += len(n.Value)
	if budget.nodes > 65536 || budget.bytes > 2*1024*1024 {
		return ErrContentRejected
	}
	active[n] = true
	defer delete(active, n)
	if n.Kind == yaml.AliasNode {
		if walkSavedCodeYAML(n.Alias, depth+1, budget, active, seen, expand) != nil {
			return ErrContentRejected
		}
	} else {
		if n.Kind == yaml.MappingNode {
			if len(n.Content)%2 != 0 {
				return ErrContentRejected
			}
			keys := map[string]bool{}
			for i := 0; i < len(n.Content); i += 2 {
				key, err := savedCodeResolvedNode(n.Content[i])
				if err != nil || key.Kind != yaml.ScalarNode || keys[key.Value] {
					return ErrContentRejected
				}
				keys[key.Value] = true
			}
		}
		for _, child := range n.Content {
			if walkSavedCodeYAML(child, depth+1, budget, active, seen, expand) != nil {
				return ErrContentRejected
			}
		}
	}
	seen[n] = true
	return nil
}
func savedCodeResolvedNode(n *yaml.Node) (*yaml.Node, error) {
	seen := map[*yaml.Node]bool{}
	for depth := 0; n != nil && depth <= 64; depth++ {
		if seen[n] {
			return nil, ErrContentRejected
		}
		seen[n] = true
		if n.Kind != yaml.AliasNode {
			return n, nil
		}
		n = n.Alias
	}
	return nil, ErrContentRejected
}
func savedCodeShallowFields(n *yaml.Node) (map[string]*yaml.Node, error) {
	n, err := savedCodeResolvedNode(n)
	if err != nil || n.Kind != yaml.MappingNode || len(n.Content)%2 != 0 {
		return nil, ErrContentRejected
	}
	fields := map[string]*yaml.Node{}
	for i := 0; i < len(n.Content); i += 2 {
		key, err := savedCodeScalar(n.Content[i])
		if err != nil {
			return nil, ErrContentRejected
		}
		if _, ok := fields[key]; ok {
			return nil, ErrContentRejected
		}
		fields[key] = n.Content[i+1]
	}
	return fields, nil
}
func savedCodeScalar(n *yaml.Node) (string, error) {
	n, err := savedCodeResolvedNode(n)
	if err != nil || n.Kind != yaml.ScalarNode {
		return "", ErrContentRejected
	}
	var value any
	if n.Decode(&value) != nil {
		return "", ErrContentRejected
	}
	text, ok := value.(string)
	if !ok {
		return "", ErrContentRejected
	}
	return text, nil
}
func decodeSavedCodePolicy(node *yaml.Node) (string, []byte, error) {
	if walkSavedCodeYAML(node, 0, &savedCodeYAMLBudget{}, map[*yaml.Node]bool{}, map[*yaml.Node]bool{}, true) != nil {
		return "", nil, ErrContentRejected
	}
	value, err := decodeSavedCodeYAMLValue(node, 0, &savedCodeYAMLBudget{}, map[*yaml.Node]bool{})
	fields, ok := value.(map[string]any)
	if err != nil || !ok || fields["type"] != "code" {
		return "", nil, ErrContentRejected
	}
	id, ok := fields["id"].(string)
	if ok {
		id = normalizeCodeDebugID(id)
		fields["id"] = id
	}
	if !ok || !codeDebugNodeID(id) {
		return "", nil, ErrContentRejected
	}
	if transition, ok := fields["transition"].(string); ok {
		fields["transition"] = normalizeCodeDebugID(transition)
	}
	if source, ok := fields["code"].(string); ok {
		fields["code"] = map[string]any{"type": "fixed", "value": source}
	}
	for key, def := range map[string]any{"language": "python", "input": []any{}, "output": []any{}, "structured_output": false, "debug": false, "transition": nil} {
		if _, ok := fields[key]; !ok {
			fields[key] = def
		}
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return "", nil, ErrContentRejected
	}
	if len(raw) > 2*1024*1024 {
		return "", nil, ErrContentRejected
	}
	return id, raw, nil
}

// Resolve aliases without yaml.v3's implicit merge-key application. The owning
// Rust compiler first reads Value and does not apply_merge, so << remains a
// literal saved field and must participate in exact configuration comparison.
func decodeSavedCodeYAMLValue(n *yaml.Node, depth int, budget *savedCodeYAMLBudget, active map[*yaml.Node]bool) (any, error) {
	if n == nil || depth > 64 || active[n] {
		return nil, ErrContentRejected
	}
	budget.nodes++
	budget.bytes += len(n.Value)
	if budget.nodes > 65536 || budget.bytes > 2*1024*1024 {
		return nil, ErrContentRejected
	}
	active[n] = true
	defer delete(active, n)
	switch n.Kind {
	case yaml.AliasNode:
		return decodeSavedCodeYAMLValue(n.Alias, depth+1, budget, active)
	case yaml.ScalarNode:
		var value any
		if n.Decode(&value) != nil {
			return nil, ErrContentRejected
		}
		return value, nil
	case yaml.SequenceNode:
		values := make([]any, 0, len(n.Content))
		for _, child := range n.Content {
			value, err := decodeSavedCodeYAMLValue(child, depth+1, budget, active)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, nil
	case yaml.MappingNode:
		if len(n.Content)%2 != 0 {
			return nil, ErrContentRejected
		}
		fields := make(map[string]any, len(n.Content)/2)
		for i := 0; i < len(n.Content); i += 2 {
			key, err := decodeSavedCodeYAMLValue(n.Content[i], depth+1, budget, active)
			name, ok := key.(string)
			if err != nil || !ok {
				return nil, ErrContentRejected
			}
			if _, present := fields[name]; present {
				return nil, ErrContentRejected
			}
			value, err := decodeSavedCodeYAMLValue(n.Content[i+1], depth+1, budget, active)
			if err != nil {
				return nil, err
			}
			fields[name] = value
		}
		return fields, nil
	default:
		return nil, ErrContentRejected
	}
}

func normalizeCodeDebugID(value string) string {
	if codeDebugNodeID(value) || value == "" || len(value) > 128 {
		return value
	}
	var result []byte
	for _, b := range []byte(value) {
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-' || b == '.' {
			if b == '.' {
				b = '_'
			}
			result = append(result, b)
		}
	}
	if len(result) == 0 || len(result) > 128 {
		return value
	}
	return string(result)
}

// This DTO contains a saved user declaration. It contains no resolved runtime grants.
type SavedCodeDeclaration struct {
	ConfigurationDigest [32]byte
	Language            string
	Source              json.RawMessage
	Input               []string
	Output              []string
	StructuredOutput    bool
	Debug               bool
	PlatformClient      bool
	Workspace           json.RawMessage
	Recovery            json.RawMessage
	FailureHandler      json.RawMessage
}

// Compare semantic saved fields before hashing the exact Rust serde configuration bytes.
// Do not hash a Go reserialization: key order and string escaping differ.
func MatchOriginalSavedCodeConfiguration(policy []byte, configurationJSON string) (SavedCodeDeclaration, error) {
	empty := SavedCodeDeclaration{}
	if len(policy) > 2*1024*1024 || len(configurationJSON) > 2*1024*1024 {
		return empty, ErrContentRejected
	}
	original, err := codeDebugJSONObject(policy)
	if err != nil {
		return empty, err
	}
	// Worker46 excludes these catalog-owned fields from the Code configuration.
	delete(original, "recovery")
	delete(original, "failure_handler")
	// Producer skip_false preserves legacy Code JSON; remove only exact authored false.
	if original["platform_client"] == false {
		delete(original, "platform_client")
	}
	actual, err := codeDebugJSONObject([]byte(configurationJSON))
	if err != nil || !reflect.DeepEqual(original, actual) || original["type"] != "code" {
		return empty, ErrContentUnauthorized
	}
	hash := sha256.New()
	hash.Write([]byte("elitea.graph.code.config.v1\x00"))
	hash.Write([]byte(configurationJSON))
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	return ReadSavedCodeDeclaration(policy, digest)
}

// ReadSavedCodeDeclaration reconstructs only immutable saved semantics. The digest
// must come from an original visit that already matched exact Rust serde bytes.
// This function does not independently authorize that digest or a current claim.
func ReadSavedCodeDeclaration(policy []byte, validatedConfigurationDigest [32]byte) (SavedCodeDeclaration, error) {
	empty := SavedCodeDeclaration{}
	if len(policy) == 0 || len(policy) > 2*1024*1024 {
		return empty, ErrContentRejected
	}
	original, err := codeDebugJSONObject(policy)
	if err != nil || original["type"] != "code" {
		return empty, ErrContentRejected
	}
	for key := range original {
		switch key {
		case "id", "type", "language", "dependencies", "code", "input", "output", "structured_output", "debug", "transition", "workspace", "platform_client", "recovery", "failure_handler":
		default:
			// Preserve unknown saved bytes in inspection; never infer an admitted
			// typed Code declaration from a matching arbitrary JSON object.
			return empty, ErrContentRejected
		}
	}
	id, ok := original["id"].(string)
	if !ok || !codeDebugNodeID(id) {
		return empty, ErrContentRejected
	}
	recovery, _ := json.Marshal(original["recovery"])
	handler, _ := json.Marshal(original["failure_handler"])
	if _, present := original["recovery"]; !present {
		recovery = nil
	}
	if _, present := original["failure_handler"]; !present {
		handler = nil
	}
	for _, key := range []string{"debug", "structured_output"} {
		if _, ok := original[key].(bool); !ok {
			return empty, ErrContentRejected
		}
	}
	platformClient := false
	if raw, present := original["platform_client"]; present {
		value, ok := raw.(bool)
		if !ok {
			return empty, ErrContentRejected
		}
		platformClient = value
	}
	for _, key := range []string{"input", "output"} {
		values, ok := original[key].([]any)
		if !ok || len(values) > 256 {
			return empty, ErrContentRejected
		}
		for _, value := range values {
			if _, ok := value.(string); !ok {
				return empty, ErrContentRejected
			}
		}
	}
	var saved struct {
		Language         string          `json:"language"`
		Source           json.RawMessage `json:"code"`
		Input            []string        `json:"input"`
		Output           []string        `json:"output"`
		StructuredOutput bool            `json:"structured_output"`
		Debug            bool            `json:"debug"`
		Workspace        json.RawMessage `json:"workspace"`
	}
	if json.Unmarshal(policy, &saved) != nil || (saved.Language != "python" && saved.Language != "javascript" && saved.Language != "typescript" && saved.Language != "rust") {
		return empty, ErrContentRejected
	}
	var source struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	rawSource, err := codeDebugJSONObject(saved.Source)
	if err != nil || len(rawSource) != 2 {
		return empty, ErrContentRejected
	}
	if _, ok := rawSource["value"].(string); !ok {
		return empty, ErrContentRejected
	}
	if strictCodeDebugJSON(saved.Source, &source) != nil || (source.Type != "fixed" && source.Type != "variable" && source.Type != "fstring") {
		return empty, ErrContentRejected
	}
	if len(source.Value) > 256*1024 {
		return empty, ErrContentRejected
	}
	return SavedCodeDeclaration{ConfigurationDigest: validatedConfigurationDigest, Language: saved.Language, Source: saved.Source, Input: saved.Input, Output: saved.Output, StructuredOutput: saved.StructuredOutput, Debug: saved.Debug, PlatformClient: platformClient, Workspace: saved.Workspace, Recovery: recovery, FailureHandler: handler}, nil
}

// Use only instructions returned by immutable root input or registered saved-child lookup.
func OriginalSavedCodePolicy(instructions, yamlSHA256, nodeID string) ([]byte, error) {
	if !debugHex(yamlSHA256) || !codeDebugNodeID(nodeID) || CodeDebugSHA256([]byte(instructions)) != yamlSHA256 {
		return nil, ErrContentUnauthorized
	}
	nodes, err := savedCodeNodes(instructions)
	if err != nil {
		return nil, ErrContentUnauthorized
	}
	var selected *yaml.Node
	seen := map[string]bool{}
	for _, node := range nodes.Content {
		fields, err := savedCodeShallowFields(node)
		if err != nil {
			return nil, ErrContentUnauthorized
		}
		kind, err := savedCodeScalar(fields["type"])
		if err != nil {
			return nil, ErrContentUnauthorized
		}
		if kind != "code" {
			continue
		}
		id, err := savedCodeScalar(fields["id"])
		if err != nil {
			return nil, ErrContentUnauthorized
		}
		id = normalizeCodeDebugID(id)
		if !codeDebugNodeID(id) || seen[id] {
			return nil, ErrContentUnauthorized
		}
		seen[id] = true
		if id == nodeID {
			selected = node
		}
	}
	if selected == nil {
		return nil, ErrContentUnauthorized
	}
	_, policy, err := decodeSavedCodePolicy(selected)
	if err != nil {
		return nil, ErrContentUnauthorized
	}
	return policy, nil
}

// Root declaration lookup excludes nodes structurally owned by Map or Parallel.
// Registered child lookup uses OriginalSavedCodePolicy after exact AllowsNode checks.
// This establishes declaration membership, not an active graph frontier or a grant.
func OriginalRootSavedCodePolicy(instructions, yamlSHA256, nodeID string) ([]byte, error) {
	policy, err := OriginalSavedCodePolicy(instructions, yamlSHA256, nodeID)
	if err != nil {
		return nil, err
	}
	nodes, err := savedCodeNodes(instructions)
	if err != nil {
		return nil, ErrContentUnauthorized
	}
	seen := map[string]bool{}
	for _, node := range nodes.Content {
		fields, err := savedCodeShallowFields(node)
		if err != nil {
			return nil, ErrContentUnauthorized
		}
		id, err := savedCodeScalar(fields["id"])
		if err != nil {
			return nil, ErrContentUnauthorized
		}
		id = normalizeCodeDebugID(id)
		if !codeDebugNodeID(id) || seen[id] {
			return nil, ErrContentUnauthorized
		}
		seen[id] = true
		kind, err := savedCodeScalar(fields["type"])
		if err != nil {
			return nil, ErrContentUnauthorized
		}
		switch kind {
		case "map":
			worker, err := savedCodeScalar(fields["worker"])
			if err != nil || !codeDebugNodeID(normalizeCodeDebugID(worker)) {
				return nil, ErrContentUnauthorized
			}
			if normalizeCodeDebugID(worker) == nodeID {
				return nil, ErrContentUnauthorized
			}
		case "parallel":
			branches, err := savedCodeResolvedNode(fields["branches"])
			if err != nil || branches.Kind != yaml.SequenceNode || len(branches.Content) > 64 {
				return nil, ErrContentUnauthorized
			}
			for _, branch := range branches.Content {
				item, err := savedCodeShallowFields(branch)
				if err != nil {
					return nil, ErrContentUnauthorized
				}
				owned, err := savedCodeScalar(item["node"])
				if err != nil || !codeDebugNodeID(normalizeCodeDebugID(owned)) {
					return nil, ErrContentUnauthorized
				}
				if normalizeCodeDebugID(owned) == nodeID {
					return nil, ErrContentUnauthorized
				}
			}
		}
	}
	return policy, nil
}
