package pipelinelimits

import (
	"fmt"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// The Worker parses a stored pipeline with a budget on the document AFTER
// anchor and alias expansion (libs/rust/agent-runtime/src/graph/mod.rs
// PIPELINE_YAML_BUDGET, metered by src/bounded_yaml.rs). A definition inside
// the 512 KiB byte bound can still expand past it, and the Worker then refuses
// it on every run. Main applies the same budget at save, so such a definition
// is never stored. TestExpansionBudgetMatchesTheWorker pins these numbers to
// the Rust constant.
const (
	// MaxExpandedNodes bounds scalars, sequences, mappings and tags, counting
	// mapping keys and every node an alias replays.
	MaxExpandedNodes = 131_072
	// MaxExpandedScalarBytes bounds the bytes of all string scalars, mapping
	// keys and tags included, after expansion.
	MaxExpandedScalarBytes = 1024 * 1024
	// MaxExpandedDepth bounds nested sequences, mappings and tags.
	MaxExpandedDepth = 64
	// aliasReplayFactor is serde_yaml_ng's own guard, which the Worker reports
	// as the node budget: a document may replay at most this many aliases per
	// parser event it holds.
	aliasReplayFactor = 100
)

// ErrExpansionTooLarge refuses a definition whose expansion exceeds the
// Worker's budget. Like the other refusals it names the limit and the remedy
// and carries no pipeline content.
var ErrExpansionTooLarge error = &apierr.APIError{
	Status: http.StatusBadRequest,
	Code:   "PIPELINE_YAML_EXPANSION_TOO_LARGE",
	Message: fmt.Sprintf("The pipeline definition expands past the runtime limit of %d YAML nodes, %d KiB of text "+
		"or %d nesting levels once anchors and aliases are expanded. Reduce the repeated aliases before saving.",
		MaxExpandedNodes, MaxExpandedScalarBytes/1024, MaxExpandedDepth),
}

// expansionMeter replays a parsed document the way the Worker's counting pass
// visits it, without building anything, and stops at the first bound it
// crosses. Every visit consumes budget, so the walk costs at most
// MaxExpandedNodes visits plus the alias replays the guard allows, whatever
// the document expands to. A self-referencing anchor ends at the depth bound.
type expansionMeter struct {
	nodes       int
	scalarBytes int
	jumps       int
	jumpLimit   int
}

// checkExpansion is the save-time budget check of a decoded document's root.
func checkExpansion(root *yaml.Node) error {
	meter := expansionMeter{jumpLimit: aliasReplayFactor * parserEvents(root)}
	if !meter.visit(root, 0) {
		return ErrExpansionTooLarge
	}
	return nil
}

// parserEvents counts the events serde_yaml_ng's loader keeps for the document
// as written: one per scalar and alias, two per sequence and mapping. Aliases
// are not followed, so this is bounded by the parsed tree.
func parserEvents(node *yaml.Node) int {
	switch node.Kind {
	case yaml.SequenceNode, yaml.MappingNode:
		events := 2
		for _, child := range node.Content {
			events += parserEvents(child)
		}
		return events
	default:
		return 1
	}
}

// visit counts node at the given container depth, reporting false at the
// first bound crossed.
func (m *expansionMeter) visit(node *yaml.Node, depth int) bool {
	if node.Kind == yaml.AliasNode {
		m.jumps++
		if m.jumps > m.jumpLimit || node.Alias == nil {
			return false
		}
		return m.visit(node.Alias, depth)
	}
	// A custom tag is an enum to serde_yaml_ng: one container holding the tag
	// name and then the tagged value.
	if tag, ok := customTag(node); ok {
		if !m.enter(&depth) || !m.scalar(len(tag)) {
			return false
		}
	}
	switch node.Kind {
	case yaml.SequenceNode, yaml.MappingNode:
		if !m.enter(&depth) {
			return false
		}
		for _, child := range node.Content {
			if !m.visit(child, depth) {
				return false
			}
		}
		return true
	case yaml.ScalarNode:
		switch node.ShortTag() {
		case "!!null", "!!bool", "!!int", "!!float":
			return m.node()
		default:
			return m.scalar(len(node.Value))
		}
	default:
		return false
	}
}

func (m *expansionMeter) node() bool {
	m.nodes++
	return m.nodes <= MaxExpandedNodes
}

func (m *expansionMeter) scalar(bytes int) bool {
	if !m.node() {
		return false
	}
	m.scalarBytes += bytes
	return m.scalarBytes <= MaxExpandedScalarBytes
}

func (m *expansionMeter) enter(depth *int) bool {
	if !m.node() {
		return false
	}
	*depth++
	return *depth <= MaxExpandedDepth
}

// customTag answers the name of a non-core tag (`!name`), which serde_yaml_ng
// reads as an enum variant. Core tags (`!!str`, implicit resolution) are not
// enums.
func customTag(node *yaml.Node) (string, bool) {
	if node.Tag == "" || node.Tag == "!" || !strings.HasPrefix(node.Tag, "!") || strings.HasPrefix(node.Tag, "!!") {
		return "", false
	}
	return strings.TrimPrefix(node.Tag, "!"), true
}
