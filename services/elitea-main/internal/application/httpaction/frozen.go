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

	"gopkg.in/yaml.v3"
)

const SnapshotSchema = "elitea.runtime.http-action-snapshot.v1"

type FrozenSnapshot struct {
	SchemaVersion      string       `json:"schema_version"`
	ApplicationID      string       `json:"application_id"`
	VersionID          string       `json:"version_id"`
	InstructionsDigest string       `json:"instructions_digest"`
	Nodes              []FrozenNode `json:"nodes"`
}
type FrozenNode struct {
	NodeID         string   `json:"node_id"`
	RequestDigest  string   `json:"request_digest"`
	RequestWireB64 string   `json:"request_wire_b64"`
	BindingDigest  string   `json:"binding_digest"`
	Output         []string `json:"output"`
	Transition     *string  `json:"transition"`
}

// FreezeSnapshot emits one Main-owned wire image from saved YAML tokens.
func FreezeSnapshot(applicationID, versionID int64, instructions string) (*FrozenSnapshot, error) {
	if applicationID <= 0 || versionID <= 0 || len(instructions) > 64*1024 {
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
	if nodes == nil || nodes.Kind != yaml.SequenceNode || len(nodes.Content) > 128 {
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
		if len(fields) < 5 || len(fields) > 6 {
			return nil, ErrInvalid
		}
		for name := range fields {
			switch name {
			case "id", "type", "revision", "request", "output", "transition":
			default:
				return nil, ErrInvalid
			}
		}
		if !validFrozenID(id.Value) || fields["type"].Kind != yaml.ScalarNode || fields["type"].Anchor != "" || fields["type"].Tag != "!!str" || fields["revision"] == nil || fields["revision"].Kind != yaml.ScalarNode || fields["revision"].Anchor != "" || fields["revision"].Tag != "!!int" || fields["revision"].Value != "1" {
			return nil, ErrInvalid
		}
		output := fields["output"]
		if output == nil || output.Kind != yaml.SequenceNode || output.Anchor != "" || len(output.Content) != 1 || output.Content[0].Kind != yaml.ScalarNode || output.Content[0].Anchor != "" || output.Content[0].Tag != "!!str" || !validFrozenOutput(output.Content[0].Value) {
			return nil, ErrInvalid
		}
		var transition *string
		if target := fields["transition"]; target != nil {
			if target.Kind != yaml.ScalarNode || target.Anchor != "" || target.Tag != "!!str" || !validFrozenID(target.Value) {
				return nil, ErrInvalid
			}
			value := target.Value
			transition = &value
		}
		count := 0
		wire, err := yamlJSON(fields["request"], 0, &count)
		if err != nil || len(wire) > MaxRequest {
			return nil, ErrInvalid
		}
		var request Request
		if Decode(wire, &request) != nil || request.Validate() != nil {
			return nil, ErrInvalid
		}
		frozen := FrozenNode{NodeID: id.Value, RequestDigest: Digest(wire), RequestWireB64: base64.StdEncoding.EncodeToString(wire), Output: []string{output.Content[0].Value}, Transition: transition}
		frozen.BindingDigest = FrozenBindingDigest(*snapshot, frozen)
		// Use the complete typed request parser before publishing the receipt.
		inv := Invocation{SchemaVersion: Schema, NodeID: frozen.NodeID, ThreadID: "freeze", RequestDigest: frozen.RequestDigest, RequestWireB64: frozen.RequestWireB64, BindingDigest: frozen.BindingDigest}
		inv.ActivationID = VisitID("freeze", frozen.NodeID, 0, BindingBytes(frozen.BindingDigest))
		if _, err := Parse(inv); err != nil {
			return nil, err
		}
		snapshot.Nodes = append(snapshot.Nodes, frozen)
	}
	if len(snapshot.Nodes) == 0 {
		return nil, nil
	}
	return snapshot, nil
}

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
	h.Write([]byte("elitea.runtime.http-frozen-node.v1\x00"))
	for _, part := range []string{snapshot.ApplicationID, snapshot.VersionID, snapshot.InstructionsDigest, node.NodeID, node.RequestDigest, output, transition} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		h.Write(size[:])
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Verify checks immutable selected version and exact request receipt bytes.
func (s FrozenSnapshot) Verify(applicationID, versionID int64, instructions string, inv Invocation) error {
	if s.SchemaVersion != SnapshotSchema || s.ApplicationID != strconv.FormatInt(applicationID, 10) || s.VersionID != strconv.FormatInt(versionID, 10) || s.InstructionsDigest != Digest([]byte(instructions)) || len(s.Nodes) == 0 || len(s.Nodes) > 128 {
		return ErrUnauthorized
	}
	wire, err := inv.RequestBytes()
	if err != nil {
		return ErrUnauthorized
	}
	seen := make(map[string]bool)
	found := false
	for _, node := range s.Nodes {
		if seen[node.NodeID] || !validFrozenID(node.NodeID) || !ValidDigest(node.RequestDigest) || !ValidDigest(node.BindingDigest) || len(node.Output) != 1 || !validFrozenOutput(node.Output[0]) || node.Transition != nil && !validFrozenID(*node.Transition) || node.BindingDigest != FrozenBindingDigest(s, node) {
			return ErrUnauthorized
		}
		seen[node.NodeID] = true
		if node.NodeID != inv.NodeID {
			continue
		}
		saved, err := base64.StdEncoding.Strict().DecodeString(node.RequestWireB64)
		if err != nil || len(saved) > MaxRequest || Digest(saved) != node.RequestDigest || inv.RequestDigest != node.RequestDigest || inv.BindingDigest != node.BindingDigest || inv.RequestWireB64 != node.RequestWireB64 || !bytes.Equal(saved, wire) {
			return ErrUnauthorized
		}
		found = true
	}
	if !found {
		return ErrUnauthorized
	}
	return nil
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
