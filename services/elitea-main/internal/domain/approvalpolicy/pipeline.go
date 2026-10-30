package approvalpolicy

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
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
	// exposed are the toolkit aliases an `llm` node gives its model: the
	// keys of `tool_names` with at least one tool.
	exposed []string
}

// childAliases answers the aliases of the children a node can run: the
// child an `agent` node selects, and each child an `llm` node gives its
// model as a tool.
func (node graphNode) childAliases() []string {
	switch node.nodeType {
	case "agent":
		if node.tool != "" {
			return []string{node.tool}
		}
	case "llm":
		return node.exposed
	}
	return nil
}

// childFor resolves an alias to a child as the worker binds a toolkit: the
// exact name, else the legacy key.
func childFor(children map[string]*PipelineGraph, alias string) (string, bool) {
	if _, ok := children[alias]; ok {
		return alias, true
	}
	key := legacyKey(alias)
	if key == "" {
		return "", false
	}
	for name := range children {
		if legacyKey(name) == key {
			return name, true
		}
	}
	return "", false
}

// PipelineGraph is one version of a pipeline composition the check walks:
// the root a run starts, or a saved pipeline it reaches as a child.
type PipelineGraph struct {
	// IsPipeline is false for an agent that is not a pipeline: it has no
	// graph, and the check reads only its Children.
	IsPipeline   bool
	Instructions string
	Toolkits     []PipelineToolkit
	// Tier is the tier whose policy requires the rule for this version's
	// tool nodes. Empty means the rule does not apply to them; the check
	// still walks the version to reach its children.
	Tier agentlifecycle.Tier
	// Children are the saved pipelines the version attaches as tools, by
	// the alias (`toolkit_name`) an `agent` node selects.
	Children map[string]*PipelineGraph
}

// CheckPipelineHITL refuses a pipeline that has a path from its entry point
// to a direct `toolkit` or `mcp` node whose tool is external_send or payment
// and that passes no `hitl` node. The refusal names the node, the tool and
// the path. tier is the tier whose policy requires the rule, for the message.
// It checks one version with no children; CheckPipelineGraph is the check
// of a composition.
func CheckPipelineHITL(instructions string, toolkits []PipelineToolkit, tier agentlifecycle.Tier) error {
	return CheckPipelineGraph(&PipelineGraph{IsPipeline: true, Instructions: instructions, Toolkits: toolkits, Tier: tier})
}

// CheckPipelineGraph refuses a composition with a path from the root's
// entry point to a gated tool node (external_send or payment) that no person
// approves, in a version whose Tier requires the rule.
//
// The graph's edges are every route the worker compiler follows: a node's
// `transition` and `default_output`, a decision node's `nodes`, a router's
// `routes`, a parallel node's branch nodes, a map node's `worker` and a
// recovery route (`recovery.on_failure.route`). A `hitl` node ends a path: a
// person decides there. A static pause ends a path too: a node the
// pipeline's `interrupt_before` names, and the successors of a node its
// `interrupt_after` names. A model's tool call inside an `llm` or `agent`
// node is not a tool node; the run's approval categories cover it at call
// time.
//
// A node that can run a saved child pipeline continues the path into the
// child: an `agent` node that selects it, and an `llm` node whose
// `tool_names` give it to the model. The child's paths from its own entry
// point are checked with the parent's path as their prefix, once for each
// such node a path reaches ungated. A child no node of the pipeline names
// (a pipeline attached as a tool of an agent) is checked from its own entry
// point. The caller bounds the composition: the graph has no cycle.
//
// A document that does not parse, has no entry point or names a node that
// does not exist is not refused here: the compiler refuses it when the run
// starts. A node whose toolkit is not attached is write (the worker refuses
// such a node anyway).
func CheckPipelineGraph(graph *PipelineGraph) error {
	return checkGraph(graph, nil)
}

// CompositionRefusal refuses a composition the check cannot read in full:
// saved pipelines in a cycle, nested too deep or too many. The worker
// refuses such a composition too.
func CompositionRefusal(reason string) error {
	return &apierr.APIError{
		Status: http.StatusUnprocessableEntity,
		Code:   CodeHITLRequired,
		Message: "The approval policy requires a hitl node before external_send and payment tools, and it cannot check this " +
			"pipeline: " + reason + ". Change the pipeline composition.",
	}
}

func checkGraph(graph *PipelineGraph, prefix []string) error {
	if graph == nil {
		return nil
	}
	selected := map[string]bool{}
	if graph.IsPipeline {
		if err := walkGraph(graph, prefix, selected); err != nil {
			return err
		}
	}
	aliases := make([]string, 0, len(graph.Children))
	for alias := range graph.Children {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		if selected[alias] {
			continue
		}
		if err := checkGraph(graph.Children[alias], appendPath(prefix, label(alias))); err != nil {
			return err
		}
	}
	return nil
}

// walkGraph walks one pipeline version. selected collects every alias an
// `agent` node of the document selects, reached or not: such a child runs
// only through its node.
func walkGraph(graph *PipelineGraph, prefix []string, selected map[string]bool) error {
	if len(graph.Instructions) > maxInstructionsBytes {
		return nil
	}
	var document yaml.Node
	if yaml.NewDecoder(strings.NewReader(graph.Instructions)).Decode(&document) != nil || len(document.Content) != 1 {
		return nil
	}
	root := aliased(document.Content[0])
	if root.Kind != yaml.MappingNode {
		return nil
	}
	entry := ""
	var nodes []graphNode
	before, after := map[string]bool{}, map[string]bool{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		switch aliased(root.Content[i]).Value {
		case "entry_point":
			entry = scalar(aliased(root.Content[i+1]))
		case "interrupt_before":
			readNames(aliased(root.Content[i+1]), before)
		case "interrupt_after":
			readNames(aliased(root.Content[i+1]), after)
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
	for _, node := range nodes {
		for _, alias := range node.childAliases() {
			if name, ok := childFor(graph.Children, alias); ok {
				selected[name] = true
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
		// A person decides before a hitl node's routes and before a node
		// the static pause names: the path ends there.
		if node.nodeType == "hitl" || before[node.id] {
			continue
		}
		if graph.Tier != "" {
			if category, gated := gatedTool(node, graph.Toolkits); gated {
				return refusal(nodes, parent, current, prefix, category, graph.Tier)
			}
		}
		// The node can run a child: the child continues this path. Each
		// path that reaches a child checks it again.
		for _, alias := range node.childAliases() {
			if name, ok := childFor(graph.Children, alias); ok {
				if err := checkGraph(graph.Children[name], pathTo(nodes, parent, current, prefix)); err != nil {
					return err
				}
			}
		}
		// The node runs, then the run pauses: its successors are gated.
		if after[node.id] {
			continue
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

// readNames reads a static pause list: the exact stored node ids, as the
// compiler reads them.
func readNames(list *yaml.Node, into map[string]bool) {
	if list == nil || list.Kind != yaml.SequenceNode {
		return
	}
	for _, item := range list.Content {
		if name := scalar(aliased(item)); name != "" {
			into[name] = true
		}
	}
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

// maxPathLabels bounds the path a refusal prints.
const maxPathLabels = 32

func appendPath(prefix []string, labels ...string) []string {
	path := append(append([]string{}, prefix...), labels...)
	if len(path) > maxPathLabels {
		path = append([]string{"..."}, path[len(path)-maxPathLabels+1:]...)
	}
	return path
}

// pathTo answers the labels of the path to node at, after prefix.
func pathTo(nodes []graphNode, parent []int, at int, prefix []string) []string {
	var path []string
	for index := at; index >= 0; index = parent[index] {
		path = append([]string{label(nodes[index].id)}, path...)
		if len(path) > maxPathLabels {
			break
		}
	}
	return appendPath(prefix, path...)
}

func refusal(nodes []graphNode, parent []int, at int, prefix []string, category toolcategory.Category, tier agentlifecycle.Tier) error {
	path := pathTo(nodes, parent, at, prefix)
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
		case "tool_names":
			if value.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(value.Content); j += 2 {
					tools := aliased(value.Content[j+1])
					if alias := scalar(aliased(value.Content[j])); alias != "" && tools.Kind == yaml.SequenceNode && len(tools.Content) > 0 {
						out.exposed = append(out.exposed, alias)
					}
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
