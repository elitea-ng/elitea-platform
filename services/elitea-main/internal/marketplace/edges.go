package marketplace

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentrelease"
)

// The internal edges of a listing version (ADR-0044 amendment 1, A1.2 rule 3,
// and ADR-0051). An installed agent's agents-as-tools are composition edges,
// and each edge's delegation grant lists what the agent at its end holds. A
// listing version snapshot carries the same information as the definition: for
// each nested agent, the requirement keys of its toolkits and the tools it
// selects on them. Comparing two versions' edges tells an upgrade or an
// upstream merge which edges widen, so the person who approves it sees them, the
// `auto` policy refuses them, and approving the request re-approves them.

// WidenDelegation is the widening kind of an edge whose callee holds more in
// the next version than in the pinned one.
const WidenDelegation = "delegation_widened"

// allTools marks a toolkit that selects every tool.
const allTools = "*"

// EdgeRequirement is what the agent at the end of one internal edge holds in a
// listing version: its toolkits by requirement key, each with the tools it
// selects (allTools for every tool).
type EdgeRequirement struct {
	// Path names the edge by the tool names from the installed agent down, so
	// the same edge is found again in the next version.
	Path  string
	Holds map[string][]string
}

// EdgesOf lists the internal edges of a definition, depth first from the root.
// An edge whose agent holds no toolkit is left out: it needs no grant.
func EdgesOf(root DefinitionNode) []EdgeRequirement {
	var out []EdgeRequirement
	var walk func(node DefinitionNode, prefix string)
	walk = func(node DefinitionNode, prefix string) {
		for _, nested := range node.NestedAgents {
			path := prefix + nested.ToolName
			holds := map[string][]string{}
			for _, tool := range nested.Node.Tools {
				names := append([]string(nil), tool.SelectedTools...)
				if len(names) == 0 {
					names = []string{allTools}
				}
				sort.Strings(names)
				holds[tool.Key] = names
			}
			if len(holds) > 0 {
				out = append(out, EdgeRequirement{Path: path, Holds: holds})
			}
			walk(nested.Node, path+"/")
		}
	}
	walk(root, "")
	return out
}

// edgeAdditions lists what next's edge holds that prev's edge at the same path
// does not: a toolkit it did not hold, or a tool of a toolkit it did. A toolkit
// that selects every tool where the pinned one selected some is one addition,
// and one that narrows is none. A new edge is all addition.
func edgeAdditions(prev, next EdgeRequirement) []string {
	var added []string
	keys := make([]string, 0, len(next.Holds))
	for key := range next.Holds {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		had, held := prev.Holds[key]
		if !held {
			added = append(added, key)
			continue
		}
		if contains(had, allTools) {
			continue
		}
		if contains(next.Holds[key], allTools) {
			added = append(added, key+": every tool")
			continue
		}
		for _, tool := range next.Holds[key] {
			if !contains(had, tool) {
				added = append(added, key+": "+tool)
			}
		}
	}
	return added
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// edgeWidenings compares the internal edges of two versions: one Widening per
// edge of next that holds more than the edge at the same path in prev (a new
// edge holds more than nothing). An edge that is gone, or holds less, is not a
// widening.
func edgeWidenings(prev, next []EdgeRequirement) []Widening {
	before := make(map[string]EdgeRequirement, len(prev))
	for _, edge := range prev {
		before[edge.Path] = edge
	}
	out := []Widening{}
	for _, edge := range next {
		added := edgeAdditions(before[edge.Path], edge)
		if len(added) == 0 {
			continue
		}
		out = append(out, Widening{Kind: WidenDelegation, Detail: "agent " + edge.Path + " may use " + strings.Join(added, ", ")})
	}
	return out
}

// WidensEdges reports whether next holds more than prev on any internal edge.
// Such an upgrade needs a person's approval whatever its tier (the approval
// is what re-approves the edge), and the `auto` policy never applies it.
func WidensEdges(prev, next Requirements) bool {
	return len(edgeWidenings(prev.Edges, next.Edges)) > 0
}

// edgeChanges is the same comparison as entries of a release diff, so the
// upgrade and upstream plan list the widened edges next to the definition's
// own changes. Path is a JSON Pointer under /delegation/edges.
func edgeChanges(prev, next []EdgeRequirement) []agentrelease.Change {
	before := make(map[string]EdgeRequirement, len(prev))
	for _, edge := range prev {
		before[edge.Path] = edge
	}
	out := []agentrelease.Change{}
	for _, edge := range next {
		added := edgeAdditions(before[edge.Path], edge)
		if len(added) == 0 {
			continue
		}
		change := agentrelease.Change{Path: "/delegation/edges/" + escapePointer(edge.Path), After: edge.Holds}
		if old, had := before[edge.Path]; had {
			change.Op, change.Before = "replace", old.Holds
		} else {
			change.Op = "add"
		}
		out = append(out, change)
	}
	return out
}

func escapePointer(path string) string {
	return strings.ReplaceAll(strings.ReplaceAll(path, "~", "~0"), "/", "~1")
}

// edgesOfDefinition reads the internal edges of a stored definition. A
// definition that does not parse has none.
func edgesOfDefinition(definition json.RawMessage) []EdgeRequirement {
	node, err := ParseDefinition(definition)
	if err != nil {
		return nil
	}
	return EdgesOf(node)
}
