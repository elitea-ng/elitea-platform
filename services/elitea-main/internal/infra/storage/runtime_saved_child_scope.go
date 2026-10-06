package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	httpapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
	scope "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
	"gopkg.in/yaml.v3"
)

// CapturedSavedChildCatalog contains Main bytes only, before credential redemption.
// Registration requests cannot supply these definitions or their project/actor.
type CapturedSavedChildCatalog struct {
	Members     []scope.Member
	Definitions map[string][]byte
}
type SavedChildCatalogSource interface {
	CaptureSavedChildCatalog(context.Context, ContentClaim, HTTPActionInput, scope.OwningDefinition, scope.Registration) (CapturedSavedChildCatalog, error)
}

// Captures are populated only by Main's runtime-version freeze, before secrets.
type FrozenSavedChildVersionCapture interface {
	CaptureFrozenSavedChildVersion(context.Context, ContentClaim, uint64, uint64, string, json.RawMessage, json.RawMessage) error
}
type FrozenSavedChildVersionReader interface {
	ReadFrozenSavedChildVersion(context.Context, ContentClaim, uint64, uint64, string) (json.RawMessage, error)
}
type RuntimeSavedChildCatalogSource struct{ captures FrozenSavedChildVersionReader }

func NewRuntimeSavedChildCatalogSource(captures FrozenSavedChildVersionReader) (*RuntimeSavedChildCatalogSource, error) {
	if captures == nil {
		return nil, scope.ErrUnavailable
	}
	return &RuntimeSavedChildCatalogSource{captures}, nil
}
func (service *RuntimeApplicationVersionService) WithFrozenSavedChildVersionCapture(capture FrozenSavedChildVersionCapture) *RuntimeApplicationVersionService {
	if service != nil {
		service.savedChildCapture = capture
	}
	return service
}

// FreezeHTTPChildVersion overwrites any editable receipt before the existing
// Main definition hash and before materialization. Non-HTTP saved bytes retain
// their existing representation; there is no semantic numeric conversion.
func FreezeHTTPChildVersion(applicationID, versionID int64, raw []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	var instructions string
	if len(raw) == 0 || len(raw) > scope.MaxDefinitionBytes || json.Unmarshal(raw, &fields) != nil {
		return nil, scope.ErrDenied
	}
	_, hadReceipt := fields["http_action_snapshot"]
	delete(fields, "http_action_snapshot")
	if json.Unmarshal(fields["instructions"], &instructions) != nil {
		return nil, scope.ErrDenied
	}
	var kind string
	_ = json.Unmarshal(fields["agent_type"], &kind)
	if kind != "pipeline" {
		if hadReceipt {
			return json.Marshal(fields)
		}
		return bytes.Clone(raw), nil
	}
	snapshot, err := httpapp.FreezeSnapshot(applicationID, versionID, instructions)
	if err != nil {
		return nil, scope.ErrDenied
	}
	if snapshot == nil {
		if hadReceipt {
			return json.Marshal(fields)
		}
		return bytes.Clone(raw), nil
	}
	fields["http_action_snapshot"], err = json.Marshal(snapshot)
	if err != nil {
		return nil, scope.ErrUnavailable
	}
	return json.Marshal(fields)
}

func (source *RuntimeSavedChildCatalogSource) CaptureSavedChildCatalog(ctx context.Context, claim ContentClaim, input HTTPActionInput, parent scope.OwningDefinition, request scope.Registration) (CapturedSavedChildCatalog, error) {
	if source == nil || source.captures == nil || request.Validate() != nil || parent.ResourceProjectID != input.ProjectID || parent.ActorID != input.ActorID {
		return CapturedSavedChildCatalog{}, scope.ErrDenied
	}
	selected, err := savedChildEdge(parent.PreRedemptionVersion, request.Family, input.ProjectID)
	owned := request.Family.Kind == "map_item" || request.Family.Kind == "parallel_branch"
	rootID := selected
	if owned {
		rootID = [2]uint64{parent.ApplicationID, parent.VersionID}
	}
	if err != nil || rootID[0] != request.Selected.ApplicationID || rootID[1] != request.Selected.VersionID || request.Selected.MemberPath != "" {
		return CapturedSavedChildCatalog{}, scope.ErrDenied
	}
	expected := map[string]string{}
	for _, member := range request.Members {
		expected[member.MemberPath] = member.FrozenDefinitionSHA256
	}
	capture := catalogCapture{source: source, claim: claim, expected: expected, input: input, cache: map[[2]uint64][]byte{}, resolving: map[[2]uint64]bool{}, definitions: map[string][]byte{}, members: map[string]scope.Member{}, blocked: map[string]bool{}}
	if owned {
		capture.rootOwnedNode = request.Family.OwnedNodeID
		capture.cache[rootID] = bytes.Clone(parent.PreRedemptionVersion)
	}
	if err = capture.saved(ctx, rootID, "", 0); err != nil {
		return CapturedSavedChildCatalog{}, err
	}
	if len(capture.members) != len(request.Members) {
		return CapturedSavedChildCatalog{}, scope.ErrDenied
	}
	result := CapturedSavedChildCatalog{Definitions: capture.definitions}
	for _, wanted := range request.Members {
		member, ok := capture.members[wanted.MemberPath]
		expectedThread := request.Family.ChildThreadID
		if wanted.MemberPath != "" {
			expectedThread += "/" + wanted.MemberPath
		}
		if !ok || wanted.ThreadID != expectedThread || wanted.FrozenDefinitionSHA256 != member.FrozenDefinitionSHA256 {
			return CapturedSavedChildCatalog{}, scope.ErrDenied
		}
		member.ThreadID = wanted.ThreadID
		result.Members = append(result.Members, member)
	}
	root := capture.members[""]
	if root.FrozenDefinitionSHA256 != request.Selected.FrozenDefinitionSHA256 {
		return CapturedSavedChildCatalog{}, scope.ErrDenied
	}
	return result, nil
}

type catalogCapture struct {
	source        *RuntimeSavedChildCatalogSource
	claim         ContentClaim
	expected      map[string]string
	input         HTTPActionInput
	cache         map[[2]uint64][]byte
	resolving     map[[2]uint64]bool
	definitions   map[string][]byte
	members       map[string]scope.Member
	hops          int
	rootOwnedNode string
	blocked       map[string]bool
	total         int
}

func (capture *catalogCapture) saved(ctx context.Context, id [2]uint64, path string, depth int) error {
	if depth > 3 || capture.hops == 25 || len(capture.members) == scope.MaxFamilyMembers || capture.resolving[id] {
		return scope.ErrDenied
	}
	capture.hops++
	capture.resolving[id] = true
	defer delete(capture.resolving, id)
	raw := capture.cache[id]
	if raw == nil {
		expected := capture.expected[path]
		if !scope.ValidDigest(expected) {
			return scope.ErrDenied
		}
		var err error
		raw, err = capture.source.captures.ReadFrozenSavedChildVersion(ctx, capture.claim, id[0], id[1], expected)
		if err != nil {
			return err
		}
		if scope.FrozenDefinitionDigest(capture.input.ProjectID, id[0], id[1], raw) != expected {
			return scope.ErrDenied
		}
		if len(raw) > scope.MaxDefinitionBytes || capture.total+len(raw) > scope.MaxCatalogBytes {
			return scope.ErrDenied
		}
		capture.total += len(raw)
		capture.cache[id] = bytes.Clone(raw)
	}
	if scope.FrozenDefinitionDigest(capture.input.ProjectID, id[0], id[1], raw) != capture.expected[path] {
		return scope.ErrDenied
	}
	var version struct {
		Instructions string `json:"instructions"`
		AgentType    string `json:"agent_type"`
	}
	if json.Unmarshal(raw, &version) != nil {
		return scope.ErrDenied
	}
	digest := scope.FrozenDefinitionDigest(capture.input.ProjectID, id[0], id[1], raw)
	capture.definitions[digest] = bytes.Clone(raw)
	if _, exists := capture.members[path]; exists {
		return scope.ErrDenied
	}
	capture.members[path] = scope.Member{MemberPath: path, ApplicationID: id[0], VersionID: id[1], FrozenDefinitionSHA256: digest, YAMLSHA256: scope.Digest([]byte(version.Instructions))}
	if version.AgentType != "pipeline" {
		return nil
	}
	nodes, err := savedPipelineNodes(version.Instructions)
	if err != nil {
		return err
	}
	owned, err := savedOwnedNodeIDs(nodes)
	if err != nil {
		return err
	}
	member := capture.members[path]
	for nodeID := range nodes {
		if path == "" && capture.rootOwnedNode != "" {
			if nodeID == capture.rootOwnedNode {
				member.AllowedNodeIDs = append(member.AllowedNodeIDs, nodeID)
			}
			continue
		}
		if !capture.blocked[path] && !owned[nodeID] {
			member.AllowedNodeIDs = append(member.AllowedNodeIDs, nodeID)
		}
	}
	sort.Strings(member.AllowedNodeIDs)
	capture.members[path] = member
	for nodeID, node := range nodes {
		if path == "" && capture.rootOwnedNode != "" && nodeID != capture.rootOwnedNode {
			continue
		}
		if scalar(node["type"]) != "agent" {
			continue
		}
		child, err := savedToolSelection(raw, scalar(node["tool"]), capture.input.ProjectID)
		if err != nil {
			return err
		}
		next := nodeID
		if path != "" {
			next = path + "/" + nodeID
		}
		if capture.blocked[path] || owned[nodeID] && (path != "" || capture.rootOwnedNode != nodeID) {
			capture.blocked[next] = true
		}
		if err = capture.saved(ctx, child, next, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// The caller's selector is checked against the exact immutable parent tool.
// Same names without matching selected IDs never authorize another application.
func savedToolSelection(raw []byte, alias string, project int64) ([2]uint64, error) {
	return savedToolSelectionBy(raw, alias, project, false)
}

// Native call names are generated from selected IDs by application_tool_name.
// The editable display alias does not select an application for a model call.
func savedNativeToolSelection(raw []byte, callName string, project int64) ([2]uint64, error) {
	return savedToolSelectionBy(raw, callName, project, true)
}

func SavedApplicationCallName(application, version uint64) string {
	return fmt.Sprintf("elitea_agent_%d_v_%d", application, version)
}

func savedToolSelectionBy(raw []byte, alias string, project int64, native bool) ([2]uint64, error) {
	var version struct {
		Tools []struct {
			Kind     string `json:"type"`
			Alias    string `json:"toolkit_name"`
			Settings struct {
				ApplicationID uint64 `json:"application_id"`
				VersionID     uint64 `json:"application_version_id"`
				ProjectID     *int64 `json:"application_project_id"`
			} `json:"settings"`
		} `json:"tools"`
	}
	if alias == "" || json.Unmarshal(raw, &version) != nil {
		return [2]uint64{}, scope.ErrDenied
	}
	var selected [2]uint64
	found := false
	for _, tool := range version.Tools {
		name := tool.Alias
		if native {
			name = SavedApplicationCallName(tool.Settings.ApplicationID, tool.Settings.VersionID)
		}
		if name != alias {
			continue
		}
		if found || tool.Kind != "application" || tool.Settings.ApplicationID == 0 || tool.Settings.VersionID == 0 || tool.Settings.ApplicationID > 2147483647 || tool.Settings.VersionID > 2147483647 || tool.Settings.ProjectID != nil && *tool.Settings.ProjectID != project {
			return [2]uint64{}, scope.ErrDenied
		}
		found = true
		selected = [2]uint64{tool.Settings.ApplicationID, tool.Settings.VersionID}
	}
	if !found {
		return [2]uint64{}, scope.ErrDenied
	}
	return selected, nil
}
func savedChildEdge(raw []byte, family scope.OriginalFamily, project int64) ([2]uint64, error) {
	if family.Kind == "agent_native" {
		return savedNativeToolSelection(raw, family.NodeID, project)
	}
	var version struct {
		Instructions string `json:"instructions"`
	}
	if json.Unmarshal(raw, &version) != nil {
		return [2]uint64{}, scope.ErrDenied
	}
	nodes, err := savedPipelineNodes(version.Instructions)
	if err != nil {
		return [2]uint64{}, err
	}
	node := nodes[family.NodeID]
	if node == nil {
		return [2]uint64{}, scope.ErrDenied
	}
	switch family.Kind {
	case "agent_graph":
		if scalar(node["type"]) != "agent" {
			return [2]uint64{}, scope.ErrDenied
		}
		return savedToolSelection(raw, scalar(node["tool"]), project)
	case "map_item":
		if scalar(node["type"]) != "map" || scalar(node["worker"]) != family.OwnedNodeID {
			return [2]uint64{}, scope.ErrDenied
		}
	case "parallel_branch":
		if scalar(node["type"]) != "parallel" {
			return [2]uint64{}, scope.ErrDenied
		}
		branches := node["branches"]
		if branches == nil || branches.Kind != yaml.SequenceNode || int(family.Ordinal) >= len(branches.Content) {
			return [2]uint64{}, scope.ErrDenied
		}
		branch, err := scopeYAMLFields(branches.Content[family.Ordinal])
		if err != nil || scalar(branch["node"]) != family.OwnedNodeID {
			return [2]uint64{}, scope.ErrDenied
		}
	default:
		return [2]uint64{}, scope.ErrDenied
	}
	owned := nodes[family.OwnedNodeID]
	if owned == nil || scalar(owned["type"]) != "agent" {
		return [2]uint64{}, scope.ErrDenied
	}
	return savedToolSelection(raw, scalar(owned["tool"]), project)
}

// SavedChildEdge is a pure declaration selector for the registering repository.
// It does not grant execution; the current-claim transaction and registry do.
func SavedChildEdge(raw []byte, family scope.OriginalFamily, project int64) ([2]uint64, error) {
	return savedChildEdge(raw, family, project)
}
func scalar(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" || node.Anchor != "" {
		return ""
	}
	return node.Value
}
func scopeYAMLFields(node *yaml.Node) (map[string]*yaml.Node, error) {
	if node == nil || node.Kind != yaml.MappingNode || node.Anchor != "" || len(node.Content)%2 != 0 {
		return nil, scope.ErrDenied
	}
	fields := map[string]*yaml.Node{}
	for i := 0; i < len(node.Content); i += 2 {
		key := scalar(node.Content[i])
		if key == "" || fields[key] != nil {
			return nil, scope.ErrDenied
		}
		fields[key] = node.Content[i+1]
	}
	return fields, nil
}
func savedPipelineNodes(instructions string) (map[string]map[string]*yaml.Node, error) {
	if len(instructions) > 64*1024 {
		return nil, scope.ErrDenied
	}
	decoder := yaml.NewDecoder(bytes.NewBufferString(instructions))
	var document yaml.Node
	if decoder.Decode(&document) != nil || len(document.Content) != 1 || decoder.Decode(new(yaml.Node)) != io.EOF {
		return nil, scope.ErrDenied
	}
	count := 0
	var walk func(*yaml.Node, int) bool
	walk = func(node *yaml.Node, depth int) bool {
		count++
		if count > 16000 || depth > 32 || node.Anchor != "" || node.Kind == yaml.AliasNode {
			return false
		}
		for _, child := range node.Content {
			if !walk(child, depth+1) {
				return false
			}
		}
		return true
	}
	if !walk(&document, 0) {
		return nil, scope.ErrDenied
	}
	root, err := scopeYAMLFields(document.Content[0])
	if err != nil {
		return nil, err
	}
	nodes := root["nodes"]
	if nodes == nil || nodes.Kind != yaml.SequenceNode || len(nodes.Content) > 128 {
		return nil, scope.ErrDenied
	}
	result := map[string]map[string]*yaml.Node{}
	for _, raw := range nodes.Content {
		fields, err := scopeYAMLFields(raw)
		id := scalar(fields["id"])
		if err != nil || !scope.ValidNodeID(id) || result[id] != nil {
			return nil, scope.ErrDenied
		}
		result[id] = fields
	}
	return result, nil
}

func savedOwnedNodeIDs(nodes map[string]map[string]*yaml.Node) (map[string]bool, error) {
	owned := map[string]bool{}
	for _, node := range nodes {
		switch scalar(node["type"]) {
		case "map":
			worker := scalar(node["worker"])
			if worker == "" || nodes[worker] == nil || owned[worker] {
				return nil, scope.ErrDenied
			}
			owned[worker] = true
		case "parallel":
			branches := node["branches"]
			if branches == nil || branches.Kind != yaml.SequenceNode {
				return nil, scope.ErrDenied
			}
			for _, raw := range branches.Content {
				branch, err := scopeYAMLFields(raw)
				id := scalar(branch["node"])
				if err != nil || id == "" || nodes[id] == nil || owned[id] {
					return nil, scope.ErrDenied
				}
				owned[id] = true
			}
		}
	}
	return owned, nil
}
