// Package pipelinelimits holds the one source of truth for the save-time
// admission of a pipeline definition: its size bounds and the node types the
// deployment's runtime can run. Main refuses an over-bound definition when it is
// saved and when a run starts, and the Worker compiler enforces the same
// numbers (services/elitea-worker-rust src/agents/graph/compiler.rs
// MAX_PIPELINE_YAML_BYTES / MAX_PIPELINE_NODES). A node type the Worker
// compiler has no arm for (parse_pipeline_node) is refused on every save, so a
// graph the runtime would refuse is never stored. The admitted types are the
// shared contract libs/proto/contracts/pipeline-node-admission-v1.json.
package pipelinelimits

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/outputcontract"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
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

// Node and toolkit refusal codes. The message is built per refusal because it
// names the node; callers that branch on any admission refusal use Refusal.
const (
	// CodeNodeTypeUnsupported is a node type no runtime build can run.
	CodeNodeTypeUnsupported = "PIPELINE_NODE_TYPE_UNSUPPORTED"
	// CodeToolkitNotAttached is a direct tool node whose toolkit is not one of
	// the version's attached tools; refused when a run starts.
	CodeToolkitNotAttached = "PIPELINE_TOOLKIT_NOT_ATTACHED"
	// CodeParallelBranchUnsupported is a fixed Parallel branch that names a
	// node the runtime cannot run as a branch: anything but an Agent node (an
	// inline LLM node until FU-247). The Worker compiler refuses the same
	// (parallel_compiler.rs validate_parallel_ownership).
	CodeParallelBranchUnsupported = "PIPELINE_PARALLEL_BRANCH_UNSUPPORTED"
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
	case CodeNodeTypeUnsupported, CodeToolkitNotAttached, CodeParallelBranchUnsupported, outputcontract.Code:
		return api
	}
	if errors.Is(err, ErrInstructionsTooLarge) || errors.Is(err, ErrTooManyNodes) || errors.Is(err, ErrExpansionTooLarge) {
		return api
	}
	return nil
}

// Check refuses a pipeline definition that exceeds MaxInstructionsBytes, whose
// anchor and alias expansion exceeds the Worker's YAML budget (expansion.go),
// whose top-level `nodes` list holds more than MaxNodes entries, one of
// whose nodes declares a `type` the deployment's runtime does not admit, or
// whose fixed Parallel branch names a declared node that is not an Agent. The
// size is tested first, on the byte length, so an over-size document is never
// parsed; the expansion, the node count and the node types are read in the
// same single parse, and the expansion is metered without being built.
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
	if err := checkExpansion(document.Content[0]); err != nil {
		return err
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
		declared := make([]map[string]*yaml.Node, 0, len(nodes.Content))
		for _, node := range nodes.Content {
			fields := nodeFields(aliased(node))
			if err := checkNodeType(fields); err != nil {
				return err
			}
			declared = append(declared, fields)
			if attached == nil {
				continue
			}
			if err := checkDirectToolkit(fields, *attached); err != nil {
				return err
			}
		}
		if err := checkParallelBranches(declared); err != nil {
			return err
		}
	}
	// ADR-0043 decision 2: the output contract of every LLM node. The
	// version save answers a typed 422 before this (applications
	// checkOutputContracts); this covers every other writer (imports, MCP).
	if err := outputcontract.CheckPipeline(instructions); err != nil {
		return &apierr.APIError{Status: http.StatusUnprocessableEntity, Code: outputcontract.Code, Message: err.Error()}
	}
	return nil
}

// checkParallelBranches refuses a fixed Parallel branch whose `node` names a
// declared node with a string type other than `agent`. As the compiler, the
// branch runs the first node with that id; a missing node, a non-string type
// or a malformed branch is left to the compiler, which refuses it.
func checkParallelBranches(declared []map[string]*yaml.Node) error {
	var typeOf map[string]*yaml.Node
	for _, owner := range declared {
		branches := owner["branches"]
		if nodeType, _ := stringScalar(owner["type"]); nodeType != "parallel" || branches == nil || branches.Kind != yaml.SequenceNode {
			continue
		}
		if typeOf == nil {
			typeOf = make(map[string]*yaml.Node, len(declared))
			for _, fields := range declared {
				if id := fields["id"]; id != nil && id.Kind == yaml.ScalarNode {
					if _, seen := typeOf[id.Value]; !seen {
						typeOf[id.Value] = fields["type"]
					}
				}
			}
		}
		for _, branch := range branches.Content {
			target := branchNode(aliased(branch))
			if target == "" {
				continue
			}
			if nodeType, ok := stringScalar(typeOf[target]); ok && nodeType != "agent" && nodeType != "llm" {
				return &apierr.APIError{
					Status: http.StatusBadRequest,
					Code:   CodeParallelBranchUnsupported,
					Message: fmt.Sprintf("%s runs node %s as a Parallel branch, but a branch can only run an Agent node (a saved agent or pipeline) or an LLM node. Put the %s node in a saved pipeline and run that pipeline from an Agent node.",
						nodeLabel(owner["id"]), quoted(target), quoted(nodeType)),
				}
			}
		}
	}
	return nil
}

// branchNode is a Parallel branch's scalar `node`, or "".
func branchNode(branch *yaml.Node) string {
	if branch.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(branch.Content); i += 2 {
		if aliased(branch.Content[i]).Value == "node" {
			if value := aliased(branch.Content[i+1]); value.Kind == yaml.ScalarNode {
				return value.Value
			}
		}
	}
	return ""
}

// nodeFields reads the scalar fields admission looks at; nil for a non-mapping.
func nodeFields(node *yaml.Node) map[string]*yaml.Node {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	fields := make(map[string]*yaml.Node, 4)
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch key := aliased(node.Content[i]).Value; key {
		case "id", "type", "toolkit_name", "branches":
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
	if _, admitted := admittedNodeTypes[nodeType]; admitted {
		return nil
	}
	return &apierr.APIError{
		Status: http.StatusBadRequest,
		Code:   CodeNodeTypeUnsupported,
		Message: fmt.Sprintf("%s uses the %s node type, which the pipeline runtime does not support. Remove or replace that node before saving.",
			nodeLabel(fields["id"]), quoted(nodeType)),
	}
}

// checkDirectToolkit refuses a direct tool node whose toolkit is not attached.
func checkDirectToolkit(fields map[string]*yaml.Node, attached []string) error {
	var panel string
	switch nodeType, _ := stringScalar(fields["type"]); nodeType {
	case "toolkit":
		panel = "Toolkit"
	case "mcp":
		panel = "MCP"
	default:
		return nil
	}
	toolkit, ok := stringScalar(fields["toolkit_name"])
	if !ok || toolkitAttached(toolkit, attached) {
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

// admittedNodeTypes mirrors the arms of the Worker compiler's
// parse_pipeline_node (compiler.rs). There is no build tag or flag: every
// deployment admits the same set, and the shared admission-parity contract
// (libs/proto/contracts/pipeline-node-admission-v1.json) keeps it equal to the
// Worker's and the Web editor's. `map` and `parallel` are listed there as
// pending Wave 2a drift.
var admittedNodeTypes = map[string]struct{}{
	"aggregate": {}, "agent": {}, "code": {}, "decision": {}, "hitl": {}, "llm": {},
	"map": {}, "mcp": {}, "parallel": {}, "printer": {}, "router": {},
	"split_out": {}, "state_modifier": {}, "toolkit": {},
}

// AdmittedNodeTypes lists the admitted node types in sorted order, for the
// admission-parity test.
func AdmittedNodeTypes() []string {
	types := make([]string, 0, len(admittedNodeTypes))
	for nodeType := range admittedNodeTypes {
		types = append(types, nodeType)
	}
	sort.Strings(types)
	return types
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
