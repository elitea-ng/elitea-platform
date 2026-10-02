package output

import (
	"context"
	"errors"
	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
	"time"
)

type ToolkitAvailableToolsIngestor interface {
	IngestToolkitAvailableTools(context.Context, outputapp.ToolkitAvailableToolsFrame) (outputapp.ProjectionOutcome, error)
}

func WithToolkitAvailableTools(ingestor ToolkitAvailableToolsIngestor) ServerOption {
	return func(s *Server) error {
		if ingestor == nil {
			return errors.New("discovery output ingestor is required")
		}
		s.discovery = ingestor
		return nil
	}
}
func (s *Server) toolkitAvailableToolsFrame(
	message *runtimev1.ExecutionOutputFrameV1,
	workloadIdentity string,
) (outputapp.ToolkitAvailableToolsFrame, error) {
	if message == nil || message.GetOutputSchemaRevision() != s.config.OutputSchemaRevision ||
		hasUnknown(message.ProtoReflect()) || !validStreamIdentity(message) ||
		message.GetOccurredAtUnixMillis() <= 0 {
		return outputapp.ToolkitAvailableToolsFrame{}, outputapp.ErrInvalidToolkitAvailableToolsOutput
	}
	encodedFrame, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil || len(encodedFrame) > s.config.MaxFrameBytes ||
		message.GetEventType() != runtimev1.ExecutionOutputEventTypeV1_EXECUTION_OUTPUT_EVENT_TYPE_V1_TOOLKIT_AVAILABLE_TOOLS_RESULT ||
		!message.GetTerminal() || message.GetToolkitAvailableTools() == nil ||
		message.GetConfigurationValidation() != nil || message.GetRuntimeError() != nil ||
		message.GetToolkitCallTool() != nil || message.GetToolkitExecuteRead() != nil || message.GetIndexIngest() != nil ||
		message.GetAgentExecution() != nil || message.GetNodeEvent() != nil {
		return outputapp.ToolkitAvailableToolsFrame{}, outputapp.ErrInvalidToolkitAvailableToolsOutput
	}
	identity := message.GetIdentity()
	fence, err := fenceDomain(identity, message.GetFence(), workloadIdentity)
	if err != nil {
		return outputapp.ToolkitAvailableToolsFrame{}, err
	}
	payload := message.GetToolkitAvailableTools()
	encodedResult, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil || !matchesDigest(message.GetPayloadDigest(), encodedResult) {
		return outputapp.ToolkitAvailableToolsFrame{}, outputapp.ErrInvalidToolkitAvailableToolsOutput
	}
	result, err := toolkitAvailableToolsResultDomain(payload)
	if err != nil {
		return outputapp.ToolkitAvailableToolsFrame{}, err
	}
	settlement, encodedSettlement, err := s.settlementProposalDomain(
		message, fence, encodedResult, executionapp.SettlementSucceeded,
	)
	if err != nil {
		return outputapp.ToolkitAvailableToolsFrame{}, outputapp.ErrInvalidToolkitAvailableToolsOutput
	}
	frame := outputapp.ToolkitAvailableToolsFrame{
		StreamID:              message.GetStreamId(),
		TenantID:              identity.GetTenantId(),
		ResourceProjectID:     identity.GetResourceProjectId(),
		ProjectionProjectID:   identity.GetProjectionProjectId(),
		WorkloadSessionID:     fence.WorkloadSessionID,
		ProducerID:            fence.ProducerID,
		EventID:               message.GetEventId(),
		LogicalOutputID:       message.GetLogicalOutputId(),
		Sequence:              message.GetSequence(),
		ClaimHandoffWatermark: message.GetClaimHandoffWatermark(),
		OccurredAt:            time.UnixMilli(message.GetOccurredAtUnixMillis()).UTC(),
		Fence:                 fence,
		PayloadDigest:         runtimedomain.SHA256(encodedResult),
		EncodedResult:         encodedResult,
		Settlement:            settlement,
		EncodedSettlement:     encodedSettlement,
		Result:                result,
	}
	if err := frame.Validate(); err != nil {
		return outputapp.ToolkitAvailableToolsFrame{}, err
	}
	return frame, nil
}

func toolkitAvailableToolsResultDomain(result *runtimev1.ToolkitAvailableToolsResultV1) (outputapp.ToolkitAvailableToolsResult, error) {
	if result == nil || hasUnknown(result.ProtoReflect()) || result.GetResultArtifact() == nil {
		return outputapp.ToolkitAvailableToolsResult{}, outputapp.ErrInvalidToolkitAvailableToolsOutput
	}
	bundleDigest, err := digestDomain(result.GetInputBundleDigest())
	if err != nil {
		return outputapp.ToolkitAvailableToolsResult{}, outputapp.ErrInvalidToolkitAvailableToolsOutput
	}
	settingsDigest, err := digestDomain(result.GetSettingsContentDigest())
	if err != nil {
		return outputapp.ToolkitAvailableToolsResult{}, outputapp.ErrInvalidToolkitAvailableToolsOutput
	}
	artifact := result.GetResultArtifact()
	digest, err := digestDomain(artifact.GetDigest())
	if err != nil {
		return outputapp.ToolkitAvailableToolsResult{}, outputapp.ErrInvalidToolkitAvailableToolsOutput
	}
	mapped := outputapp.ToolkitAvailableToolsResult{
		ToolkitType: result.GetToolkitType(), InputBundleID: result.GetInputBundleId(), InputBundleDigest: bundleDigest,
		Settings:       outputapp.ToolkitAvailableToolsInputBinding{EntryID: result.GetSettingsEntryId(), ImmutableVersion: result.GetSettingsEntryVersion(), ContentDigest: settingsDigest},
		ResultArtifact: outputapp.ToolkitAvailableToolsArtifact{ArtifactID: artifact.GetArtifactId(), ImmutableVersion: artifact.GetImmutableVersion(), MediaType: artifact.GetMediaType(), ByteLength: artifact.GetByteLength(), Digest: digest, Classification: artifact.GetClassification()},
	}
	if err := mapped.Validate(); err != nil {
		return outputapp.ToolkitAvailableToolsResult{}, err
	}
	return mapped, nil
}
