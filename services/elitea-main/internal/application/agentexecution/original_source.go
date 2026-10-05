package agentexecution

import (
	"context"
	"encoding/json"

	scope "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
)

type RootSourceCaptureRequest struct {
	ProjectID, ActorID       int64
	ApplicationID, VersionID uint64
	VersionDetails           json.RawMessage
}
type RootSourceRestoreRequest struct {
	ProjectID, ActorID                                     int64
	ConversationID, ResponseMessageID, ExecutionGeneration string
}

// The producer invokes this Main-only port after its existing authenticated
// resolver, before provider/credential freeze. No Worker body can call capture.
type OriginalRootSourceOwner interface {
	CaptureOriginalRootSource(context.Context, RootSourceCaptureRequest) (scope.SourceReference, error)
	RestoreOriginalRootSource(context.Context, RootSourceRestoreRequest) (scope.SourceDefinition, error)
}

func (service *CurrentApplicationStartService) WithOriginalSourceDefinitions(owner OriginalRootSourceOwner) *CurrentApplicationStartService {
	if service != nil {
		service.originalSources = owner
	}
	return service
}
func (service *CurrentApplicationStartService) captureApplicationSource(ctx context.Context, project, actor int64, target *CurrentApplicationTarget) error {
	// Always overwrite a resolver/editable marker. Nil preserves the old input.
	target.OriginalSource = nil
	if service.originalSources == nil {
		return nil
	}
	// Production resolvers retain the exact admitted version before nested skills.
	// An enabled producer refuses a resolver that discarded that source.
	raw := target.SourceVersionDetails
	if len(raw) == 0 {
		return ErrUnsupportedCurrentAgentStart
	}
	ref, err := service.originalSources.CaptureOriginalRootSource(ctx, RootSourceCaptureRequest{project, actor, uint64(target.ApplicationID), uint64(target.ApplicationVersionID), raw})
	if err != nil || ref.Validate() != nil || ref.Kind != "saved_application" || ref.ApplicationID != uint64(target.ApplicationID) || ref.VersionID != uint64(target.ApplicationVersionID) {
		return ErrUnsupportedCurrentAgentStart
	}
	target.OriginalSource = &ref
	return nil
}
func (service *CurrentApplicationStartService) captureAdhocSource(ctx context.Context, project, actor int64, target *CurrentAdhocTarget, snapshot json.RawMessage) error {
	target.OriginalSource = nil
	if service.originalSources == nil {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(snapshot, &fields) != nil || fields == nil {
		return ErrUnsupportedCurrentAgentStart
	}
	// The source snapshot precedes runtime memory/project additions, exactly as
	// the existing Main-resolved ephemeral instructions and static tools stand.
	fields["instructions"], _ = json.Marshal(target.Instructions)
	fields["agent_type"] = json.RawMessage(`"agent"`)
	raw, err := json.Marshal(fields)
	if err != nil {
		return ErrUnsupportedCurrentAgentStart
	}
	ref, err := service.originalSources.CaptureOriginalRootSource(ctx, RootSourceCaptureRequest{ProjectID: project, ActorID: actor, VersionDetails: raw})
	if err != nil || ref.Validate() != nil || ref.Kind != "ephemeral_definition" {
		return ErrUnsupportedCurrentAgentStart
	}
	target.OriginalSource = &ref
	return nil
}
func (service *CurrentApplicationStartService) restoreSource(ctx context.Context, request RootSourceRestoreRequest) (*scope.SourceDefinition, error) {
	if service.originalSources == nil {
		return nil, nil
	}
	source, err := service.originalSources.RestoreOriginalRootSource(ctx, request)
	if err != nil || source.Reference.Validate() != nil || source.ResourceProjectID != request.ProjectID || source.ActorID != request.ActorID {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	verified, err := scope.DecodeSourceWire(source.CanonicalWire, source.PreRedemptionVersion, source.Reference, request.ProjectID, request.ActorID)
	if err != nil || verified.Instructions != source.Instructions {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	return &verified, nil
}
