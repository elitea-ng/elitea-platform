package repos

import (
	"bytes"
	"context"
	"encoding/json"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	httpapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/httpaction"
	code "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	scope "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"google.golang.org/protobuf/proto"
)

// CodeDefinitionSourceAdapter is the concrete consumer of Main's sole source
// capture and saved-child registry. It creates no capture, registry or authority.
type CodeDefinitionSourceAdapter struct {
	registry *ExecutionChildScopesRepository
}

func NewCodeDefinitionSourceAdapter(registry *ExecutionChildScopesRepository) (*CodeDefinitionSourceAdapter, error) {
	if registry == nil {
		return nil, code.ErrRejected
	}
	return &CodeDefinitionSourceAdapter{registry: registry}, nil
}
func (reader *CodeDefinitionSourceAdapter) readOriginalCodeSource(ctx context.Context, tx sqlExecutor, execution string, generation uint64, access codeAccess, rawScope json.RawMessage, purpose, thread, node string) (scope.SourceDefinition, error) {
	if reader == nil || reader.registry == nil || tx == nil || !originalCodePurpose(purpose) || !scope.ValidNodeID(node) {
		return scope.SourceDefinition{}, code.ErrRejected
	}
	// These facts come only from the Code owner's already locked actual access.
	// A sealed Supervisor read therefore needs no fabricated Worker certificate.
	original := storage.HTTPActionInput{Payload: bytes.Clone(access.input), ProjectID: access.project, ProjectionProjectID: access.projection, ActorID: access.actorID, TenantID: access.tenant, LeaseEpoch: access.epoch, Deadline: access.deadline}
	if bytes.Equal(bytes.TrimSpace(rawScope), []byte("null")) {
		var input runtimev1.AgentExecutionInputV1
		if proto.Unmarshal(original.Payload, &input) != nil || thread != httpapp.RootGraphThread(access.tenant, access.project, access.projection, input.GetThreadId()) {
			return scope.SourceDefinition{}, code.ErrRejected
		}
		source, err := reader.registry.readOriginalRootSourceUnderOriginalAccess(ctx, tx, original)
		if err != nil {
			return scope.SourceDefinition{}, err
		}
		// Root membership excludes Map/Parallel-owned nodes via the one pure parser.
		if _, err = storage.SavedCodeDocumentFor(ctx, source.Instructions).RootSavedCodePolicy(source.Reference.YAMLSHA256, node); err != nil {
			return scope.SourceDefinition{}, err
		}
		return source, nil
	}
	if child, ok := decodeFanoutCodeChildScope(rawScope); ok {
		return reader.readFanoutCodeChildSource(ctx, tx, original, access, child, thread, node)
	}
	var reference scope.Reference
	if code.Decode(rawScope, &reference, 8192) != nil || reference.Validate() != nil {
		return scope.SourceDefinition{}, code.ErrRejected
	}
	owner, err := reader.registry.readRegisteredSavedChildUnderOriginalAccess(ctx, tx, execution, generation, original, reference, scope.Purpose(purpose), thread)
	if err != nil || !owner.AllowsNode(node) || owner.ExecutionID != execution || owner.Generation != generation || owner.ThreadID != thread || owner.RootInputSHA256 != code.Digest(access.input) || owner.SourceReference == nil {
		return scope.SourceDefinition{}, code.ErrRejected
	}
	if owner.OriginalSource == nil || owner.OriginalSource.Reference != *owner.SourceReference || owner.SourceReference.ApplicationID != owner.ApplicationID || owner.SourceReference.VersionID != owner.VersionID || owner.SourceReference.YAMLSHA256 != owner.YAMLSHA256 {
		return scope.SourceDefinition{}, code.ErrRejected
	}
	source, err := scope.DecodeSourceWire(owner.OriginalSource.CanonicalWire, owner.OriginalSource.PreRedemptionVersion, owner.OriginalSource.Reference, access.project, access.actorID)
	if err != nil || source.Instructions != owner.OriginalSource.Instructions || source.Instructions != owner.Instructions {
		return scope.SourceDefinition{}, code.ErrRejected
	}
	// The source full digest/bytes are intentionally distinct from the frozen
	// runtime snapshot; Main-owned static/HTTP freeze may change that snapshot.
	return source, nil
}

// fanoutCodeChildScope is the owner scope of a Code node run directly as a
// Map item or Parallel branch of the run root (W3-2). It is a selector, not a
// grant: every field is resolved against Main's own state below.
type fanoutCodeChildScope struct {
	Schema         string `json:"schema"`
	Kind           string `json:"kind"`
	NodeID         string `json:"node_id"`
	OwnedNodeID    string `json:"owned_node_id"`
	Ordinal        uint32 `json:"ordinal"`
	ItemCount      uint32 `json:"item_count"`
	ParentThreadID string `json:"parent_thread_id"`
	ChildThreadID  string `json:"child_thread_id"`
}

func decodeFanoutCodeChildScope(raw json.RawMessage) (fanoutCodeChildScope, bool) {
	var child fanoutCodeChildScope
	if code.Decode(raw, &child, 4096) != nil || child.Schema != storage.FanoutCodeChildScopeSchema {
		return fanoutCodeChildScope{}, false
	}
	return child, true
}

// readFanoutCodeChildSource resolves a fan-out Code child's owner inside the
// caller's transaction and fails closed (C1): the parent thread is this run's
// root thread, the visit runs on the scope's own child thread in the
// family's form, the visit's node is the scope's owned node, and the run's
// frozen root source declares that fan-out node owning that Code node at
// that ordinal inside the frozen family. The owning source is the run root's.
func (reader *CodeDefinitionSourceAdapter) readFanoutCodeChildSource(ctx context.Context, tx sqlExecutor, original storage.HTTPActionInput, access codeAccess, child fanoutCodeChildScope, thread, node string) (scope.SourceDefinition, error) {
	var input runtimev1.AgentExecutionInputV1
	if proto.Unmarshal(original.Payload, &input) != nil {
		return scope.SourceDefinition{}, code.ErrRejected
	}
	root := httpapp.RootGraphThread(access.tenant, access.project, access.projection, input.GetThreadId())
	if child.ParentThreadID != root || child.ChildThreadID != thread || child.OwnedNodeID != node || !storage.FanoutCodeChildThread(child.Kind, thread, root) {
		return scope.SourceDefinition{}, code.ErrRejected
	}
	source, err := reader.registry.readOriginalRootSourceUnderOriginalAccess(ctx, tx, original)
	if err != nil {
		return scope.SourceDefinition{}, err
	}
	if err := storage.SavedCodeDocumentFor(ctx, source.Instructions).FanoutCodeChildOwner(child.Kind, child.NodeID, child.OwnedNodeID, child.Ordinal, child.ItemCount); err != nil {
		return scope.SourceDefinition{}, err
	}
	return source, nil
}

var _ CodeDefinitionSourceReader = (*CodeDefinitionSourceAdapter)(nil)
