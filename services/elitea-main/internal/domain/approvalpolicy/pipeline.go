package approvalpolicy

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// CodeHITLRequired is the refusal of a pipeline whose tier requires a hitl
// node on every path to an external_send or payment tool node, and which
// has a path without one.
const CodeHITLRequired = "PIPELINE_HITL_REQUIRED"

// maxInstructionsBytes bounds the document this check parses: the pipeline
// size limit (domain/pipelinelimits), which admission applies first.
const maxInstructionsBytes = 512 * 1024

// maxNamedBytes bounds a node id or a tool name echoed in a refusal.
const maxNamedBytes = 128

// PipelineToolkit is one toolkit a pipeline version attaches: the name a
// direct `toolkit` or `mcp` node binds (`toolkit_name`), its toolkit type,
// and, for an approved custom MCP server, the categories its pins declare
// by tool name.
type PipelineToolkit struct {
	Name           string
	Type           string
	PinsCategories map[string][]toolcategory.Category
}

// categoriesOf answers the categories of one tool of the toolkit. A pinned
// MCP tool answers its pins' categories (write when a pin declares none); any
// other tool answers its contract category.
func (t PipelineToolkit) categoriesOf(tool string) []toolcategory.Category {
	if pinned, ok := t.PinsCategories[tool]; ok {
		if len(pinned) == 0 {
			return []toolcategory.Category{toolcategory.Default}
		}
		return pinned
	}
	return []toolcategory.Category{toolcategory.Of(t.Type, tool)}
}

// Refusal answers the typed refusal err carries, or nil.
func Refusal(err error) *apierr.APIError {
	var api *apierr.APIError
	if !errors.As(err, &api) || api.Code != CodeHITLRequired {
		return nil
	}
	return api
}

// graphNode is the part of a pipeline node the check reads.
type graphNode struct {
	id       string
	nodeType string
	toolkit  string
	tool     string
	targets  []string
}

// CheckPipelineHITL refuses a pipeline that has a path from its entry point
// to a direct `toolkit` or `mcp` node whose tool is external_send or payment
// and that passes no `hitl` node. The refusal names the node, the tool and
// the path. tier is the tier whose policy requires the rule, for the message.
//
// The graph's edges are every route the worker compiler follows: a node's
// `transition` and `default_output`, a decision node's `nodes`, a router's
// `routes`, a parallel node's branch nodes, a map node's `worker` and a
// recovery route (`recovery.on_failure.route`). A `hitl` node ends a path: a
// person decides there. A model's tool call inside an `llm` or `agent` node
// is not a tool node; the run's approval categories cover it at call time.
//
// A document that does not parse, has no entry point or names a node that
// does not exist is not refused here: the compiler refuses it when the run
// starts. A node whose toolkit is not attached is write (the worker refuses
// such a node anyway).
func CheckPipelineHITL(instructions string, toolkits []PipelineToolkit, tier agentlifecycle.Tier) error {
	if len(instructions) > maxInstructionsBytes {
		return nil
	}
	var document yaml.Node
	if yaml.NewDecoder(strings.NewReader(instructions)).Decode(&document) != nil || len(document.Content) != 1 {
		return nil
	}
	root := aliased(document.Content[0])
	if root.Kind != yaml.MappingNode {
		return nil
	}
	entry := ""
	var nodes []graphNode
	for i := 0; i+1 < len(root.Content); i += 2 {
		switch aliased(root.Content[i]).Value {
		case "entry_point":
			entry = scalar(aliased(root.Content[i+1]))
		case "nodes":
			list := aliased(root.Content[i+1])
			if list.Kind != yaml.SequenceNode {
				return nil
			}
			for _, item := range list.Content {
				if node, ok := readNode(aliased(item)); ok {
					nodes = append(nodes, node)
				}
			}
		}
	}
	if entry == "" || len(nodes) == 0 {
		return nil
	}
	byID := map[string]int{}
	byLegacy := map[string]int{}
	for index, node := range nodes {
		if _, seen := byID[node.id]; !seen {
			byID[node.id] = index
		}
		if key := legacyKey(node.id); key != "" {
			if _, seen := byLegacy[key]; !seen {
				byLegacy[key] = index
			}
		}
	}
	resolve := func(target string) (int, bool) {
		if index, ok := byID[target]; ok {
			return index, true
		}
		index, ok := byLegacy[legacyKey(target)]
		return index, ok
	}
	start, ok := resolve(entry)
	if !ok {
		return nil
	}
	parent := make([]int, len(nodes))
	for i := range parent {
		parent[i] = -2
	}
	parent[start] = -1
	queue := []int{start}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		node := nodes[current]
		if node.nodeType == "hitl" {
			continue
		}
		if category, gated := gatedTool(node, toolkits); gated {
			return refusal(nodes, parent, current, category, tier)
		}
		for _, target := range node.targets {
			next, ok := resolve(target)
			if !ok || parent[next] != -2 {
				continue
			}
			parent[next] = current
			queue = append(queue, next)
		}
	}
	return nil
}

// gatedTool answers the gated category of a direct tool node, if it has one.
func gatedTool(node graphNode, toolkits []PipelineToolkit) (toolcategory.Category, bool) {
	if node.nodeType != "toolkit" && node.nodeType != "mcp" {
		return "", false
	}
	toolkit, ok := attached(node.toolkit, toolkits)
	if !ok {
		return "", false
	}
	for _, category := range toolkit.categoriesOf(node.tool) {
		if GatedByHITL(category) {
			return category, true
		}
	}
	return "", false
}

// attached mirrors the worker's alias binding: the exact toolkit name, else
// the legacy key (lower case, no whitespace or underscores).
func attached(name string, toolkits []PipelineToolkit) (PipelineToolkit, bool) {
	for _, toolkit := range toolkits {
		if toolkit.Name == name {
			return toolkit, true
		}
	}
	key := legacyKey(name)
	if key == "" {
		return PipelineToolkit{}, false
	}
	for _, toolkit := range toolkits {
		if legacyKey(toolkit.Name) == key {
			return toolkit, true
		}
	}
	return PipelineToolkit{}, false
}

func refusal(nodes []graphNode, parent []int, at int, category toolcategory.Category, tier agentlifecycle.Tier) error {
	var path []string
	for index := at; index >= 0; index = parent[index] {
		path = append([]string{label(nodes[index].id)}, path...)
		if len(path) > 32 {
			path = append([]string{"..."}, path[len(path)-31:]...)
			break
		}
	}
	node := nodes[at]
	return &apierr.APIError{
		Status: http.StatusUnprocessableEntity,
		Code:   CodeHITLRequired,
		Message: fmt.Sprintf("Node %s calls %s, which is a tool in the %s category. The approval policy requires a hitl node on every "+
			"path to such a tool for %s-tier agents, and the path %s has none. Add a hitl node before %s.",
			label(node.id), label(node.tool), category, tier, strings.Join(path, " -> "), label(node.id)),
	}
}

// readNode reads one node mapping; false for a mapping with no scalar id.
func readNode(node *yaml.Node) (graphNode, bool) {
	if node.Kind != yaml.MappingNode {
		return graphNode{}, false
	}
	var out graphNode
	for i := 0; i+1 < len(node.Content); i += 2 {
		value := aliased(node.Content[i+1])
		switch aliased(node.Content[i]).Value {
		case "id":
			out.id = scalar(value)
		case "type":
			out.nodeType = scalar(value)
		case "toolkit_name":
			out.toolkit = scalar(value)
		case "tool":
			out.tool = scalar(value)
		case "transition", "default_output", "worker":
			out.targets = appendScalar(out.targets, value)
		case "nodes", "routes":
			switch value.Kind {
			case yaml.SequenceNode:
				for _, target := range value.Content {
					out.targets = appendScalar(out.targets, aliased(target))
				}
			case yaml.MappingNode:
				// A hitl node's routes; the node ends a path anyway.
				for j := 1; j < len(value.Content); j += 2 {
					out.targets = appendScalar(out.targets, aliased(value.Content[j]))
				}
			}
		case "branches":
			if value.Kind == yaml.SequenceNode {
				for _, branch := range value.Content {
					out.targets = appendScalar(out.targets, field(aliased(branch), "node"))
				}
			}
		case "recovery":
			out.targets = appendScalar(out.targets, field(field(value, "on_failure"), "route"))
		}
	}
	return out, out.id != ""
}

func field(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if aliased(node.Content[i]).Value == key {
			return aliased(node.Content[i+1])
		}
	}
	return nil
}

func appendScalar(targets []string, node *yaml.Node) []string {
	if value := scalar(node); value != "" && value != "END" {
		targets = append(targets, value)
	}
	return targets
}

// scalar answers a non-null scalar's text (an integer id included).
func scalar(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag == "!!null" {
		return ""
	}
	return strings.TrimSpace(node.Value)
}

func legacyKey(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '_' {
			return -1
		}
		return unicode.ToLower(r)
	}, value)
}

// label quotes a short, printable name the author wrote.
func label(value string) string {
	if value == "" || len(value) > maxNamedBytes || !utf8.ValidString(value) {
		return `"(unnamed)"`
	}
	for _, r := range value {
		if !unicode.IsPrint(r) || r == '"' {
			return `"(unnamed)"`
		}
	}
	return `"` + value + `"`
}

func aliased(node *yaml.Node) *yaml.Node {
	if node != nil && node.Kind == yaml.AliasNode && node.Alias != nil {
		return node.Alias
	}
	return node
}
