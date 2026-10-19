package httpaction

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
	"gopkg.in/yaml.v3"
)

// SnapshotSchema is the start-time freeze of every HTTP node's template
// (revision 2). The snapshot holds the rule NAME only, never a rule body: the
// live rule decides at each call.
const SnapshotSchema = "elitea.runtime.http-action-snapshot.v2"

type FrozenSnapshot struct {
	SchemaVersion      string       `json:"schema_version"`
	ApplicationID      string       `json:"application_id"`
	VersionID          string       `json:"version_id"`
	InstructionsDigest string       `json:"instructions_digest"`
	Nodes              []FrozenNode `json:"nodes"`
}
type FrozenNode struct {
	NodeID          string   `json:"node_id"`
	Operation       string   `json:"operation"`
	TemplateDigest  string   `json:"template_digest"`
	TemplateWireB64 string   `json:"template_wire_b64"`
	BindingDigest   string   `json:"binding_digest"`
	Output          []string `json:"output"`
	Transition      *string  `json:"transition"`
}

// The pipeline bounds (512 KiB, 128 nodes) live in pipelinelimits, shared with
// the save path; only the stricter HTTP-bearing cap is local.
const maxHTTPInstructionsBytes = 64 * 1024

// FreezeSnapshot emits one Main-owned snapshot from saved YAML tokens.
// Pipelines without a top-level `type: http` node return nil without passing
// through the HTTP snapshot grammar; the Worker owns their validation.
func FreezeSnapshot(applicationID, versionID int64, instructions string) (*FrozenSnapshot, error) {
	if applicationID <= 0 || versionID <= 0 {
		return nil, ErrInvalid
	}
	declared, err := declaresHTTPNode(instructions)
	if err != nil || !declared {
		return nil, err
	}
	if len(instructions) > maxHTTPInstructionsBytes {
		return nil, ErrInvalid
	}
	decoder := yaml.NewDecoder(bytes.NewBufferString(instructions))
	var document yaml.Node
	if decoder.Decode(&document) != nil || len(document.Content) != 1 || decoder.Decode(new(yaml.Node)) != io.EOF {
		return nil, ErrInvalid
	}
	root, err := yamlFields(document.Content[0])
	if err != nil {
		return nil, err
	}
	nodes := root["nodes"]
	if nodes == nil || nodes.Kind != yaml.SequenceNode || len(nodes.Content) > pipelinelimits.MaxNodes {
		return nil, ErrInvalid
	}
	snapshot := &FrozenSnapshot{SchemaVersion: SnapshotSchema, ApplicationID: strconv.FormatInt(applicationID, 10), VersionID: strconv.FormatInt(versionID, 10), InstructionsDigest: Digest([]byte(instructions))}
	seen := make(map[string]bool)
	for _, node := range nodes.Content {
		fields, err := yamlFields(node)
		if err != nil {
			return nil, err
		}
		id := fields["id"]
		if id == nil || id.Kind != yaml.ScalarNode || id.Anchor != "" || id.Tag != "!!str" || seen[id.Value] {
			return nil, ErrInvalid
		}
		seen[id.Value] = true
		if kind := fields["type"]; kind == nil || kind.Value != "http" {
			continue
		}
		count := 0
		wire, err := yamlJSON(node, 0, &count)
		if err != nil {
			return nil, ErrInvalid
		}
		parsed, err := ParseNode(wire)
		if err != nil {
			return nil, ErrInvalid
		}
		frozen := FrozenNode{NodeID: parsed.ID, Operation: parsed.Template.Operation, TemplateDigest: Digest(parsed.TemplateWire), TemplateWireB64: base64.StdEncoding.EncodeToString(parsed.TemplateWire), Output: []string{parsed.Output}, Transition: parsed.Transition}
		frozen.BindingDigest = FrozenBindingDigest(*snapshot, frozen)
		snapshot.Nodes = append(snapshot.Nodes, frozen)
	}
	if len(snapshot.Nodes) == 0 {
		return nil, nil
	}
	return snapshot, nil
}

// declaresHTTPNode is the structural pre-scan that scopes the strict grammar
// to pipelines authoring a top-level HTTP node. It over-approximates (aliases
// are followed and a merge key counts as a possible HTTP node) so every
// document the Worker could read as HTTP still reaches the strict grammar, and
// it rejects only documents the Worker compiler rejects as well.
func declaresHTTPNode(instructions string) (bool, error) {
	if len(instructions) > pipelinelimits.MaxInstructionsBytes {
		return false, pipelinelimits.ErrInstructionsTooLarge
	}
	decoder := yaml.NewDecoder(strings.NewReader(instructions))
	var document yaml.Node
	if decoder.Decode(&document) != nil || len(document.Content) != 1 || decoder.Decode(new(yaml.Node)) != io.EOF {
		return false, ErrInvalid
	}
	root := yamlAliased(document.Content[0])
	if root.Kind != yaml.MappingNode {
		return false, ErrInvalid
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if key := yamlAliased(root.Content[i]); key.Tag == "!!merge" {
			return true, nil
		} else if key.Value != "nodes" {
			continue
		}
		nodes := yamlAliased(root.Content[i+1])
		if len(nodes.Content) > pipelinelimits.MaxNodes {
			return false, pipelinelimits.ErrTooManyNodes
		}
		for _, node := range nodes.Content {
			if node = yamlAliased(node); node.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(node.Content); j += 2 {
				key := yamlAliased(node.Content[j])
				if key.Tag == "!!merge" || key.Value == "type" && yamlAliased(node.Content[j+1]).Value == "http" {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func yamlAliased(node *yaml.Node) *yaml.Node {
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		return node.Alias
	}
	return node
}

// FrozenBindingDigest binds one node's template to the exact saved version.
func FrozenBindingDigest(snapshot FrozenSnapshot, node FrozenNode) string {
	transition := ""
	if node.Transition != nil {
		transition = *node.Transition
	}
	output := ""
	if len(node.Output) == 1 {
		output = node.Output[0]
	}
	h := sha256.New()
	h.Write([]byte("elitea.runtime.http-frozen-node.v2\x00"))
	for _, part := range []string{snapshot.ApplicationID, snapshot.VersionID, snapshot.InstructionsDigest, node.NodeID, node.Operation, node.TemplateDigest, output, transition} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		h.Write(size[:])
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Verify checks the snapshot belongs to the immutable selected version and
// returns the frozen template of the invoked node. The invocation must carry
// that node's binding digest.
func (s FrozenSnapshot) Verify(applicationID, versionID int64, instructions string, inv Invocation) (NodeTemplate, error) {
	if s.SchemaVersion != SnapshotSchema || s.ApplicationID != strconv.FormatInt(applicationID, 10) || s.VersionID != strconv.FormatInt(versionID, 10) || s.InstructionsDigest != Digest([]byte(instructions)) || len(s.Nodes) == 0 || len(s.Nodes) > pipelinelimits.MaxNodes {
		return NodeTemplate{}, ErrUnauthorized
	}
	seen := make(map[string]bool)
	var selected *FrozenNode
	for i := range s.Nodes {
		node := &s.Nodes[i]
		if seen[node.NodeID] || !validFrozenID(node.NodeID) || !ValidDigest(node.TemplateDigest) || !ValidDigest(node.BindingDigest) || len(node.Output) != 1 || !validFrozenOutput(node.Output[0]) || node.Transition != nil && !validFrozenID(*node.Transition) || node.BindingDigest != FrozenBindingDigest(s, *node) {
			return NodeTemplate{}, ErrUnauthorized
		}
		seen[node.NodeID] = true
		if node.NodeID == inv.NodeID {
			selected = node
		}
	}
	if selected == nil || inv.BindingDigest != selected.BindingDigest {
		return NodeTemplate{}, ErrUnauthorized
	}
	wire, err := base64.StdEncoding.Strict().DecodeString(selected.TemplateWireB64)
	if err != nil || len(wire) > MaxTemplateBytes || Digest(wire) != selected.TemplateDigest {
		return NodeTemplate{}, ErrUnauthorized
	}
	template, err := DecodeTemplate(wire)
	if err != nil || template.Operation != selected.Operation {
		return NodeTemplate{}, ErrUnauthorized
	}
	return template, nil
}

func yamlFields(node *yaml.Node) (map[string]*yaml.Node, error) {
	if node == nil || node.Kind != yaml.MappingNode || node.Tag != "!!map" || node.Anchor != "" || len(node.Content)%2 != 0 {
		return nil, ErrInvalid
	}
	fields := make(map[string]*yaml.Node, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Anchor != "" {
			return nil, ErrInvalid
		}
		if _, exists := fields[key.Value]; exists {
			return nil, ErrInvalid
		}
		fields[key.Value] = node.Content[i+1]
	}
	return fields, nil
}

var jsonNumberToken = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)
var frozenID = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

func validFrozenID(value string) bool { return frozenID.MatchString(value) }

// Numeric YAML leaves never pass through native integers or floating point.
func yamlJSON(node *yaml.Node, depth int, count *int) ([]byte, error) {
	*count++
	if node == nil || depth > 32 || *count > 16000 || node.Anchor != "" {
		return nil, ErrInvalid
	}
	var out bytes.Buffer
	switch node.Kind {
	case yaml.MappingNode:
		fields, err := yamlFields(node)
		if err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			encoded, _ := json.Marshal(key)
			out.Write(encoded)
			out.WriteByte(':')
			value, err := yamlJSON(fields[key], depth+1, count)
			if err != nil {
				return nil, err
			}
			out.Write(value)
			if out.Len() > MaxRequest {
				return nil, ErrInvalid
			}
		}
		out.WriteByte('}')
	case yaml.SequenceNode:
		if node.Tag != "!!seq" {
			return nil, ErrInvalid
		}
		out.WriteByte('[')
		for i, child := range node.Content {
			if i > 0 {
				out.WriteByte(',')
			}
			value, err := yamlJSON(child, depth+1, count)
			if err != nil {
				return nil, err
			}
			out.Write(value)
			if out.Len() > MaxRequest {
				return nil, ErrInvalid
			}
		}
		out.WriteByte(']')
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!str":
			encoded, _ := json.Marshal(node.Value)
			out.Write(encoded)
		case "!!null":
			out.WriteString("null")
		case "!!bool":
			if node.Value != "true" && node.Value != "false" {
				return nil, ErrInvalid
			}
			out.WriteString(node.Value)
		case "!!int", "!!float":
			if len(node.Value) > 4096 || !jsonNumberToken.MatchString(node.Value) {
				return nil, ErrInvalid
			}
			out.WriteString(node.Value)
		default:
			return nil, ErrInvalid
		}
	default:
		return nil, ErrInvalid
	}
	if out.Len() > MaxRequest {
		return nil, ErrInvalid
	}
	return out.Bytes(), nil
}

// RootGraphThread preserves the current worker session scope byte framing.
func RootGraphThread(tenant string, project, projection int64, thread string) string {
	h := sha256.New()
	h.Write([]byte("elitea.adk.session.v1\x00"))
	for _, part := range []string{tenant, strconv.FormatInt(project, 10), strconv.FormatInt(projection, 10), thread} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		h.Write(size[:])
		h.Write([]byte(part))
	}
	return "e1:" + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func validFrozenOutput(value string) bool {
	return len(value) > 0 && len(value) <= 256 && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}
