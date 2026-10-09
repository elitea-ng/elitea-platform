// Package pipelinelimits holds the one source of truth for the save-time
// admission of a pipeline definition: its size bounds and the node types the
// deployment's runtime can run. Main refuses an over-bound definition when it is
// saved and when a run starts, and the Worker compiler enforces the same
// numbers (services/elitea-worker-rust src/agents/graph/compiler.rs
// MAX_PIPELINE_YAML_BYTES / MAX_PIPELINE_NODES). A node type the Worker
// compiler has no arm for (parse_pipeline_node_admitting) is refused on every
// save, so a graph the runtime would refuse is never stored.
package pipelinelimits

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

const (
	// MaxInstructionsBytes is the largest pipeline YAML, in bytes.
	MaxInstructionsBytes = 512 * 1024
	// MaxNodes is the largest number of top-level `nodes` entries.
	MaxNodes = 128
)

// The refusals are shared, immutable *apierr.APIError values so a caller can
// both write them as a 400 and branch on them with errors.Is. Their text names
// the limit and the remedy and never carries pipeline content.
var (
	ErrInstructionsTooLarge error = &apierr.APIError{
		Status:  http.StatusBadRequest,
		Code:    "PIPELINE_INSTRUCTIONS_TOO_LARGE",
		Message: fmt.Sprintf("The pipeline definition exceeds the %d KiB size limit. Reduce the YAML before saving.", MaxInstructionsBytes/1024),
	}
	ErrTooManyNodes error = &apierr.APIError{
		Status:  http.StatusBadRequest,
		Code:    "PIPELINE_TOO_MANY_NODES",
		Message: fmt.Sprintf("The pipeline has more than %d nodes. Split it into smaller pipelines before saving.", MaxNodes),
	}
)

// Node-type refusal codes. The message is built per refusal because it names
// the node; callers that branch on the kind use IsNodeTypeRefusal.
const (
	// CodeNodeTypeNotAvailable is a node type the runtime knows but this
	// deployment's build does not admit (SplitOut and Aggregate outside a
	// graph-extensions rehearsal build).
	CodeNodeTypeNotAvailable = "PIPELINE_NODE_TYPE_NOT_AVAILABLE"
	// CodeNodeTypeUnsupported is a node type no runtime build can run.
	CodeNodeTypeUnsupported = "PIPELINE_NODE_TYPE_UNSUPPORTED"
	// CodeToolkitNotAttached is a direct tool node whose toolkit is not one of
	// the version's attached tools; refused when a run starts.
	CodeToolkitNotAttached = "PIPELINE_TOOLKIT_NOT_ATTACHED"
	// maxNamedBytes bounds a node id, type or toolkit name echoed in a refusal.
	maxNamedBytes = 128
)

// Refusal answers the typed admission refusal err carries (from Check or
// CheckStart, possibly wrapped), or nil.
func Refusal(err error) *apierr.APIError {
	var api *apierr.APIError
	if !errors.As(err, &api) {
		return nil
	}
	switch api.Code {
	case CodeNodeTypeNotAvailable, CodeNodeTypeUnsupported, CodeToolkitNotAttached:
		return api
	}
	if errors.Is(err, ErrInstructionsTooLarge) || errors.Is(err, ErrTooManyNodes) {
		return api
	}
	return nil
}

// Check refuses a pipeline definition that exceeds MaxInstructionsBytes, whose
// top-level `nodes` list holds more than MaxNodes entries, or one of whose
// nodes declares a `type` the deployment's runtime does not admit. The size is
// tested first, on the byte length, so an over-size document is never parsed;
// the node count and the node types are read in the same single parse.
//
// Check is a save-time guard, not a full validator. A document that does not
// parse, has no `nodes` sequence, or is not a mapping is not refused here: the
// Worker compiler validates it when the pipeline runs, and the Web editor
// mirrors its remaining rules. Only the first YAML document is read, a node
// whose `type` is missing or not a string is left to the compiler (which
// refuses it), legacy numeric ids count like any other, and an aliased `nodes`
// list or entry is followed because the compiler reads it the same way.
func Check(instructions string) error {
	return check(instructions, nil)
}

// CheckStart is the start-time admission of a stored pipeline: everything
// Check refuses, plus a direct `toolkit`/`mcp` node whose `toolkit_name` names
// no toolkit in attached, the `toolkit_name`s of the version's frozen tools.
// The Worker refuses such a node too (pipeline.rs validate_tool_snapshot) but
// may only say "The execution input is invalid."; here the refusal names the
// node, the toolkit and the fix. The match mirrors the Worker's exact name
// and legacy key and never refuses a node the Worker could bind: anything it
// cannot judge exactly (non-ASCII names) is left to the Worker.
func CheckStart(instructions string, attached []string) error {
	return check(instructions, &attached)
}

func check(instructions string, attached *[]string) error {
	if len(instructions) > MaxInstructionsBytes {
		return ErrInstructionsTooLarge
	}
	var document yaml.Node
	if yaml.NewDecoder(strings.NewReader(instructions)).Decode(&document) != nil || len(document.Content) != 1 {
		return nil
	}
	root := aliased(document.Content[0])
	if root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if aliased(root.Content[i]).Value != "nodes" {
			continue
		}
		nodes := aliased(root.Content[i+1])
		if nodes.Kind != yaml.SequenceNode {
			continue
		}
		if len(nodes.Content) > MaxNodes {
			return ErrTooManyNodes
		}
		for _, node := range nodes.Content {
			fields := nodeFields(aliased(node))
			if err := checkNodeType(fields); err != nil {
				return err
			}
			if attached == nil {
				continue
			}
			if err := checkDirectToolkit(fields, *attached); err != nil {
				return err
			}
		}
	}
	return nil
}

// nodeFields reads the scalar fields admission looks at; nil for a non-mapping.
func nodeFields(node *yaml.Node) map[string]*yaml.Node {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	fields := make(map[string]*yaml.Node, 3)
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch key := aliased(node.Content[i]).Value; key {
		case "id", "type", "toolkit_name":
			fields[key] = aliased(node.Content[i+1])
		}
	}
	return fields
}

func stringScalar(node *yaml.Node) (string, bool) {
	if node == nil || node.Kind != yaml.ScalarNode || node.ShortTag() != "!!str" {
		return "", false
	}
	return node.Value, true
}

// checkNodeType refuses one node whose string `type` the runtime has no arm for.
func checkNodeType(fields map[string]*yaml.Node) error {
	nodeType, ok := stringScalar(fields["type"])
	if !ok {
		return nil
	}
	switch admission(nodeType) {
	case nodeTypeAdmitted:
		return nil
	case nodeTypeGated:
		return &apierr.APIError{
			Status: http.StatusBadRequest,
			Code:   CodeNodeTypeNotAvailable,
			Message: fmt.Sprintf("%s uses the %s node type, which is not available on this deployment. Remove or replace that node before saving.",
				nodeLabel(fields["id"]), quoted(nodeType)),
		}
	default:
		return &apierr.APIError{
			Status: http.StatusBadRequest,
			Code:   CodeNodeTypeUnsupported,
			Message: fmt.Sprintf("%s uses the %s node type, which the pipeline runtime does not support. Remove or replace that node before saving.",
				nodeLabel(fields["id"]), quoted(nodeType)),
		}
	}
}

// checkDirectToolkit refuses a direct tool node whose toolkit is not attached.
func checkDirectToolkit(fields map[string]*yaml.Node, attached []string) error {
	nodeType, _ := stringScalar(fields["type"])
	panel := map[string]string{"toolkit": "Toolkit", "mcp": "MCP"}[nodeType]
	toolkit, ok := stringScalar(fields["toolkit_name"])
	if panel == "" || !ok || toolkitAttached(toolkit, attached) {
		return nil
	}
	name := quoted(toolkit)
	if !displayable(toolkit) {
		name = "it"
	}
	return &apierr.APIError{
		Status: http.StatusUnprocessableEntity,
		Code:   CodeToolkitNotAttached,
		Message: fmt.Sprintf("%s uses the %s toolkit, which is not attached to this pipeline. Attach %s under Tools → %s, then run the pipeline again.",
			nodeLabel(fields["id"]), quoted(toolkit), name, panel),
	}
}

// toolkitAttached mirrors the Worker's alias binding: an exact toolkit name,
// else the legacy key (lower case, no whitespace or underscores). A name the
// two languages might fold differently counts as attached.
func toolkitAttached(toolkit string, attached []string) bool {
	if !ascii(toolkit) {
		return true
	}
	key := legacyToolkitKey(toolkit)
	for _, name := range attached {
		if name == toolkit || !ascii(name) || key != "" && legacyToolkitKey(name) == key {
			return true
		}
	}
	return false
}

func legacyToolkitKey(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '_' {
			return -1
		}
		return unicode.ToLower(r)
	}, value)
}

func ascii(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

type nodeTypeAdmission int

const (
	nodeTypeUnsupported nodeTypeAdmission = iota
	nodeTypeAdmitted
	nodeTypeGated
)

// admission mirrors the arms of the Worker compiler's
// parse_pipeline_node_admitting (compiler.rs). SplitOut and Aggregate are
// admitted only when Main is built with the graph_extensions_rehearsal tag,
// the counterpart of the Worker's graph-extensions-rehearsal Cargo feature and
// the Web's VITE_GRAPH_EXTENSIONS_REHEARSAL; the three flip together.
func admission(nodeType string) nodeTypeAdmission {
	switch nodeType {
	case "code", "decision", "map", "parallel", "agent", "toolkit", "mcp",
		"hitl", "llm", "printer", "router", "state_modifier":
		return nodeTypeAdmitted
	case "split_out", "aggregate":
		if shapingNodesAdmitted {
			return nodeTypeAdmitted
		}
		return nodeTypeGated
	default:
		return nodeTypeUnsupported
	}
}

// nodeLabel names the node by its id when the id is a short, printable scalar.
func nodeLabel(id *yaml.Node) string {
	if id == nil || id.Kind != yaml.ScalarNode || id.Tag == "!!null" || !displayable(id.Value) {
		return "A pipeline node"
	}
	return "Node " + quoted(id.Value)
}

func quoted(value string) string {
	if !displayable(value) {
		return "requested"
	}
	return `"` + value + `"`
}

// displayable keeps a refusal to short, printable text the author wrote.
func displayable(value string) bool {
	if value == "" || len(value) > maxNamedBytes || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if !unicode.IsPrint(r) || r == '"' {
			return false
		}
	}
	return true
}

func aliased(node *yaml.Node) *yaml.Node {
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		return node.Alias
	}
	return node
}
