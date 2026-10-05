package repos

import (
	"bytes"
	"context"
	"encoding/json"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	httpapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	scope "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
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
		if _, err = storage.OriginalRootSavedCodePolicy(source.Instructions, source.Reference.YAMLSHA256, node); err != nil {
			return scope.SourceDefinition{}, err
		}
		return source, nil
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

var _ CodeDefinitionSourceReader = (*CodeDefinitionSourceAdapter)(nil)
