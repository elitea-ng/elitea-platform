package storage

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// FanoutCodeChildScopeSchema names the owner scope a Code node run directly
// as a Map item or Parallel branch of the run root carries on its visit (W3-2).
const FanoutCodeChildScopeSchema = "elitea.runtime.fanout-code-child-scope.v1"

// maxFanoutChildren is the Worker's per-activation child bound
// (MAX_FANOUT_CHILDREN); a Map's max_items and a Parallel's branches never
// exceed it.
const maxFanoutChildren = 64

// FanoutCodeChildOwner is a pure declaration check of the run root's frozen
// source: fan-out node nodeID of the given family owns the Code node
// ownedNodeID at ordinal, inside a frozen family of itemCount children. It
// grants nothing; the caller applies it inside the visit's own transaction.
//
//   - map_item: nodeID is a Map whose worker is ownedNodeID, with
//     1 <= itemCount <= max_items and ordinal < itemCount.
//   - parallel_branch: nodeID is a Parallel whose branch at ordinal runs
//     ownedNodeID, and itemCount is its branch count.
//
// In both, ownedNodeID is a Code node. Anything else is ErrContentUnauthorized.
func FanoutCodeChildOwner(instructions, kind, nodeID, ownedNodeID string, ordinal, itemCount uint32) error {
	return ParseSavedCodeDocument(instructions).FanoutCodeChildOwner(kind, nodeID, ownedNodeID, ordinal, itemCount)
}

// FanoutCodeChildOwner is the package function on the parsed document.
func (d *SavedCodeDocument) FanoutCodeChildOwner(kind, nodeID, ownedNodeID string, ordinal, itemCount uint32) error {
	if itemCount == 0 || itemCount > maxFanoutChildren || ordinal >= itemCount || !codeDebugNodeID(nodeID) || !codeDebugNodeID(ownedNodeID) || nodeID == ownedNodeID {
		return ErrContentUnauthorized
	}
	nodes, err := d.Nodes()
	if err != nil {
		return ErrContentUnauthorized
	}
	var owner, owned map[string]*yaml.Node
	seen := map[string]bool{}
	for _, raw := range nodes.Content {
		fields, err := savedCodeShallowFields(raw)
		if err != nil {
			return ErrContentUnauthorized
		}
		id, err := savedCodeScalar(fields["id"])
		if err != nil {
			return ErrContentUnauthorized
		}
		id = normalizeCodeDebugID(id)
		if !codeDebugNodeID(id) || seen[id] {
			return ErrContentUnauthorized
		}
		seen[id] = true
		switch id {
		case nodeID:
			owner = fields
		case ownedNodeID:
			owned = fields
		}
	}
	if owner == nil || owned == nil {
		return ErrContentUnauthorized
	}
	if ownedType, err := savedCodeScalar(owned["type"]); err != nil || ownedType != "code" {
		return ErrContentUnauthorized
	}
	ownerType, err := savedCodeScalar(owner["type"])
	if err != nil {
		return ErrContentUnauthorized
	}
	switch {
	case kind == "map_item" && ownerType == "map":
		worker, err := savedCodeScalar(owner["worker"])
		if err != nil || normalizeCodeDebugID(worker) != ownedNodeID {
			return ErrContentUnauthorized
		}
		limit, err := savedCodeUint(owner["max_items"])
		if err != nil || uint64(itemCount) > limit {
			return ErrContentUnauthorized
		}
	case kind == "parallel_branch" && ownerType == "parallel":
		branches, err := savedCodeResolvedNode(owner["branches"])
		if err != nil || branches.Kind != yaml.SequenceNode || len(branches.Content) > maxFanoutChildren || uint64(len(branches.Content)) != uint64(itemCount) {
			return ErrContentUnauthorized
		}
		branch, err := savedCodeShallowFields(branches.Content[ordinal])
		if err != nil {
			return ErrContentUnauthorized
		}
		node, err := savedCodeScalar(branch["node"])
		if err != nil || normalizeCodeDebugID(node) != ownedNodeID {
			return ErrContentUnauthorized
		}
	default:
		// A forged family, or a family that does not match the node's type.
		return ErrContentUnauthorized
	}
	return nil
}

// FanoutCodeChildThread reports whether thread has the family's child-thread
// form (the Worker's `m1:`/`p1:` derived ids) and is not the root thread.
func FanoutCodeChildThread(kind, thread, root string) bool {
	prefix := map[string]string{"map_item": "m1:", "parallel_branch": "p1:"}[kind]
	return prefix != "" && thread != root && strings.HasPrefix(thread, prefix) && len(thread) > len(prefix) && len(thread) <= 512
}

func savedCodeUint(n *yaml.Node) (uint64, error) {
	n, err := savedCodeResolvedNode(n)
	if err != nil || n.Kind != yaml.ScalarNode || n.Tag != "!!int" {
		return 0, ErrContentRejected
	}
	var value uint64
	if n.Decode(&value) != nil {
		return 0, ErrContentRejected
	}
	return value, nil
}
