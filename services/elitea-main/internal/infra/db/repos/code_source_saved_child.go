package repos

import (
	"context"
	"encoding/json"
	"math"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	httpapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/httpaction"
	code "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	scope "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"google.golang.org/protobuf/proto"
)

// savedCodeChildScope is the owner scope of a Code node inside a saved
// pipeline that an Agent node of the run root runs (W3-2b): directly, or as
// the Map worker or Parallel branch of a root fan-out node (fanout). It is a
// selector, not a grant: every field is resolved against Main's own state
// inside the caller's transaction (readSavedCodeChildSource).
type savedCodeChildScope struct {
	Schema           string                     `json:"schema"`
	ApplicationID    uint64                     `json:"application_id"`
	VersionID        uint64                     `json:"version_id"`
	DefinitionSHA256 string                     `json:"definition_sha256"`
	AgentNodeID      string                     `json:"agent_node_id"`
	Fanout           *savedCodeChildFanoutScope `json:"fanout"`
	ParentThreadID   string                     `json:"parent_thread_id"`
	ChildThreadID    string                     `json:"child_thread_id"`
	CodeNodeID       string                     `json:"code_node_id"`
}

// savedCodeChildFanoutScope names the root fan-out member that runs the
// Agent node (the W3-2a family fields).
type savedCodeChildFanoutScope struct {
	Kind      string `json:"kind"`
	NodeID    string `json:"node_id"`
	Ordinal   uint32 `json:"ordinal"`
	ItemCount uint32 `json:"item_count"`
}

func decodeSavedCodeChildScope(raw json.RawMessage) (savedCodeChildScope, bool) {
	var child savedCodeChildScope
	if code.Decode(raw, &child, 4096) != nil || child.Schema != storage.SavedCodeChildScopeSchema {
		return savedCodeChildScope{}, false
	}
	return child, true
}

// readSavedCodeChildSource resolves the owner of a Code node inside a saved
// pipeline, inside the caller's transaction, and fails closed. Each refusal
// names its stage (never request or definition content):
//
//   - saved_child_scope: the scope is well formed, its child thread is the
//     visit's thread and its Code node the visit's node.
//   - saved_child_parent: the parent thread is this run's root thread, or a
//     root fan-out child thread of the scope's family (W3-2a form).
//   - saved_child_thread: the child thread is the Worker's subgraph thread
//     of that parent and the Agent node.
//   - saved_child_edge: the run root's frozen source declares that Agent
//     node, at the root (owned by no Map or Parallel) or as the fan-out
//     node's worker or branch at the ordinal inside the frozen family, and
//     it selects exactly the scope's application and version (B1: a capture
//     row is evidence only of a freeze under this claim, not of reachability,
//     so reachability is proved here from the root down).
//   - saved_child_capture: Main captured that application at that version
//     and definition digest for this execution, generation, project, actor
//     and root input at freeze, and the captured definition bytes hash to it.
//   - saved_child_code: the owned node is declared in the captured saved
//     pipeline with `type: code`, outside any Map or Parallel of it.
//
// The owning source is the saved pipeline's captured pre-redemption source.
func (reader *CodeDefinitionSourceAdapter) readSavedCodeChildSource(ctx context.Context, tx sqlExecutor, execution string, generation uint64, original storage.HTTPActionInput, access codeAccess, child savedCodeChildScope, thread, node string) (scope.SourceDefinition, error) {
	if child.ApplicationID == 0 || child.ApplicationID > math.MaxInt32 || child.VersionID == 0 || child.VersionID > math.MaxInt32 || !scope.ValidDigest(child.DefinitionSHA256) || !scope.ValidNodeID(child.AgentNodeID) || !scope.ValidNodeID(child.CodeNodeID) || child.ChildThreadID != thread || child.CodeNodeID != node {
		return scope.SourceDefinition{}, staged("saved_child_scope", code.ErrRejected)
	}
	var input runtimev1.AgentExecutionInputV1
	if proto.Unmarshal(original.Payload, &input) != nil {
		return scope.SourceDefinition{}, staged("saved_child_parent", code.ErrRejected)
	}
	root := httpapp.RootGraphThread(access.tenant, access.project, access.projection, input.GetThreadId())
	kind, fanoutNode, ordinal, itemCount := "", "", uint32(0), uint32(0)
	if child.Fanout == nil {
		if child.ParentThreadID != root {
			return scope.SourceDefinition{}, staged("saved_child_parent", code.ErrRejected)
		}
	} else {
		kind, fanoutNode, ordinal, itemCount = child.Fanout.Kind, child.Fanout.NodeID, child.Fanout.Ordinal, child.Fanout.ItemCount
		if !storage.FanoutCodeChildThread(kind, child.ParentThreadID, root) {
			return scope.SourceDefinition{}, staged("saved_child_parent", code.ErrRejected)
		}
	}
	if !storage.SavedCodeChildThread(child.ParentThreadID, child.AgentNodeID, child.ChildThreadID) {
		return scope.SourceDefinition{}, staged("saved_child_thread", code.ErrRejected)
	}
	rootSource, err := reader.registry.readOriginalRootSourceUnderOriginalAccess(ctx, tx, original)
	if err != nil {
		return scope.SourceDefinition{}, staged("saved_child_root", err)
	}
	selected, err := storage.SavedCodeChildEdge(storage.SavedCodeDocumentFor(ctx, rootSource.Instructions), rootSource.PreRedemptionVersion, access.project, kind, fanoutNode, child.AgentNodeID, ordinal, itemCount)
	if err != nil || selected != [2]uint64{child.ApplicationID, child.VersionID} {
		return scope.SourceDefinition{}, staged("saved_child_edge", code.ErrRejected)
	}
	// C5: one read of the capture: the frozen definition (its digest is
	// recomputed) and, through the same row, the pre-redemption source.
	var definition, sourceWire, sourceDefinition []byte
	if tx.QueryRow(ctx, `SELECT d.definition_bytes,r.canonical_wire,s.definition_bytes FROM elitea_runtime.execution_saved_child_captures c JOIN elitea_runtime.execution_captured_definitions d USING(resource_project_id,application_id,version_id,definition_sha256) JOIN elitea_runtime.execution_definition_source_refs r ON r.source_id=c.source_id AND r.digest_sha256=c.source_digest_sha256 JOIN elitea_runtime.execution_captured_definitions s ON s.resource_project_id=r.resource_project_id AND s.application_id=r.application_id AND s.version_id=r.version_id AND s.definition_sha256=r.definition_sha256 WHERE c.execution_id=$1 AND c.generation=$2 AND c.application_id=$3 AND c.version_id=$4 AND c.definition_sha256=$5 AND c.root_input_sha256=$6 AND c.resource_project_id=$7 AND c.actor_id=$8 AND r.resource_project_id=c.resource_project_id AND r.actor_id=c.actor_id AND r.application_id=c.application_id AND r.version_id=c.version_id FOR SHARE OF c,d,r,s`, execution, int64(generation), int64(child.ApplicationID), int64(child.VersionID), child.DefinitionSHA256, scope.Digest(original.Payload), original.ProjectID, original.ActorID).Scan(&definition, &sourceWire, &sourceDefinition) != nil || len(definition) == 0 || len(definition) > scope.MaxDefinitionBytes || scope.FrozenDefinitionDigest(original.ProjectID, child.ApplicationID, child.VersionID, definition) != child.DefinitionSHA256 {
		return scope.SourceDefinition{}, staged("saved_child_capture", code.ErrRejected)
	}
	var wire scope.SourceWire
	if json.Unmarshal(sourceWire, &wire) != nil || wire.ApplicationID != child.ApplicationID || wire.VersionID != child.VersionID {
		return scope.SourceDefinition{}, staged("saved_child_capture", code.ErrRejected)
	}
	source, err := scope.DecodeSourceWire(sourceWire, sourceDefinition, wire.Reference(), original.ProjectID, original.ActorID)
	if err != nil || source.Reference.ApplicationID != child.ApplicationID || source.Reference.VersionID != child.VersionID {
		return scope.SourceDefinition{}, staged("saved_child_capture", code.ErrRejected)
	}
	// Declared Code, never one a Map or Parallel of the saved pipeline owns
	// (that stays refused, `graph.fanout.code_below_root`).
	if _, err := storage.SavedCodeDocumentFor(ctx, source.Instructions).RootSavedCodePolicy(source.Reference.YAMLSHA256, node); err != nil {
		return scope.SourceDefinition{}, staged("saved_child_code", err)
	}
	return source, nil
}
