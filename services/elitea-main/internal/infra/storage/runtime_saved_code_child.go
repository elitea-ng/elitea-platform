package storage

import (
	"encoding/json"
	"strings"

	scope "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
)

// SavedCodeChildScopeSchema names the owner scope a Code node inside a saved
// pipeline carries on its visit (W3-2b): the saved pipeline is run by an
// Agent node of the run root, directly or as a Map worker or Parallel branch
// of a root fan-out node.
const SavedCodeChildScopeSchema = "elitea.runtime.saved-code-child-scope.v1"

// maxSavedCodeChildThread bounds a saved child's graph thread (the Worker's
// subgraph thread: its parent's thread, "/", the Agent node's id).
const maxSavedCodeChildThread = 512

// SavedCodeChildEdge is a pure declaration check of the run root's frozen
// source (its instructions, parsed as doc, and its pre-redemption version
// bytes): the root declares the Agent node agentNodeID running a saved
// application, and returns that application and version. With kind empty the
// Agent node runs at the root and no Map or Parallel owns it; with kind
// `map_item` or `parallel_branch` the root fan-out node nodeID owns it at
// ordinal inside a frozen family of itemCount children (the W3-2a checks).
// It grants nothing; the caller applies it inside the visit's transaction.
func SavedCodeChildEdge(doc *SavedCodeDocument, version []byte, project int64, kind, nodeID, agentNodeID string, ordinal, itemCount uint32) ([2]uint64, error) {
	var root struct {
		Instructions string `json:"instructions"`
	}
	if doc == nil || json.Unmarshal(version, &root) != nil || root.Instructions != doc.instructions || !codeDebugNodeID(agentNodeID) {
		return [2]uint64{}, ErrContentUnauthorized
	}
	family := scope.OriginalFamily{Kind: kind, NodeID: nodeID, OwnedNodeID: agentNodeID, Ordinal: ordinal}
	switch kind {
	case "":
		if nodeID != "" || ordinal != 0 || itemCount != 0 {
			return [2]uint64{}, ErrContentUnauthorized
		}
		nodes, err := savedPipelineNodes(root.Instructions)
		if err != nil {
			return [2]uint64{}, ErrContentUnauthorized
		}
		owned, err := savedOwnedNodeIDs(nodes)
		if err != nil || owned[agentNodeID] {
			// A fan-out owned Agent node never runs at the root.
			return [2]uint64{}, ErrContentUnauthorized
		}
		family = scope.OriginalFamily{Kind: "agent_graph", NodeID: agentNodeID}
	case "map_item", "parallel_branch":
		if err := doc.fanoutChildOwner(kind, nodeID, agentNodeID, "agent", ordinal, itemCount); err != nil {
			return [2]uint64{}, err
		}
	default:
		return [2]uint64{}, ErrContentUnauthorized
	}
	selected, err := savedChildEdge(version, family, project)
	if err != nil {
		return [2]uint64{}, ErrContentUnauthorized
	}
	return selected, nil
}

// SavedCodeChildThread reports whether child is the subgraph thread the
// Worker runs the Agent node's saved pipeline on: parent, "/", agentNodeID.
func SavedCodeChildThread(parent, agentNodeID, child string) bool {
	return parent != "" && codeDebugNodeID(agentNodeID) && len(child) <= maxSavedCodeChildThread && child == parent+"/"+agentNodeID && !strings.Contains(agentNodeID, "/")
}
