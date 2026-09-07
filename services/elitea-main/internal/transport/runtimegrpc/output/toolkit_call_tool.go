package output

import (
	"context"
	"errors"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

// ToolkitCallToolIngestor is the terminal boundary for `toolkit.call_tool.v1`.
type ToolkitCallToolIngestor interface {
	IngestToolkitCallTool(ctx context.Context, frame outputapp.ToolkitCallToolFrame) (outputapp.ProjectionOutcome, error)
}

// ServerOption composes an OPTIONAL capability onto the output listener.
//
// It exists because the five positional constructors above already enumerate
// every combination of three capabilities, and a sixth combination of four
// would have doubled that list to serve one new arm. A capability that is
// neither a prerequisite of nor prerequisite to the others belongs here.
type ServerOption func(*Server) error

// WithToolkitCallTool admits the tool-run terminal arm. Left unset, a tool-run
// frame is refused exactly as it was before this capability existed.
func WithToolkitCallTool(ingestor ToolkitCallToolIngestor) ServerOption {
	return func(server *Server) error {
		if ingestor == nil {
			return errors.New("tool-run output ingestor is required")
		}
		server.toolRuns = ingestor
		return nil
	}
}

// NewServerWithCapabilities is the option-taking constructor. Any of indexes,
// agents and nodeEvents may be nil; each optional capability then answers the
// same refusal it did before it was composed.
func NewServerWithCapabilities(
	config ServerConfig,
	authorizer WorkloadAuthorizer,
	ingestor ValidationIngestor,
	failures RuntimeFailureIngestor,
	indexes IndexIngestIngestor,
	agents AgentExecutionIngestor,
	nodeEvents NodeEventIngestor,
	options ...ServerOption,
) (*Server, error) {
	server, err := newServer(config, authorizer, ingestor, failures, indexes, agents, nodeEvents)
	if err != nil {
		return nil, err
	}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("output server option is required")
		}
		if err := option(server); err != nil {
			return nil, err
		}
	}
	return server, nil
}

func (s *Server) toolkitCallToolFrame(
	message *runtimev1.ExecutionOutputFrameV1,
	workloadIdentity string,
) (outputapp.ToolkitCallToolFrame, error) {
	if message == nil || message.GetOutputSchemaRevision() != s.config.OutputSchemaRevision ||
		hasUnknown(message.ProtoReflect()) || !validStreamIdentity(message) ||
		message.GetOccurredAtUnixMillis() <= 0 {
		return outputapp.ToolkitCallToolFrame{}, outputapp.ErrInvalidToolkitCallToolOutput
	}
	encodedFrame, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil || len(encodedFrame) > s.config.MaxFrameBytes ||
		message.GetEventType() != runtimev1.ExecutionOutputEventTypeV1_EXECUTION_OUTPUT_EVENT_TYPE_V1_TOOLKIT_CALL_TOOL_RESULT ||
		!message.GetTerminal() || message.GetToolkitCallTool() == nil ||
		message.GetConfigurationValidation() != nil || message.GetRuntimeError() != nil ||
		message.GetToolkitAvailableTools() != nil || message.GetIndexIngest() != nil ||
		message.GetAgentExecution() != nil || message.GetNodeEvent() != nil {
		return outputapp.ToolkitCallToolFrame{}, outputapp.ErrInvalidToolkitCallToolOutput
	}
	identity := message.GetIdentity()
	fence, err := fenceDomain(identity, message.GetFence(), workloadIdentity)
	if err != nil {
		return outputapp.ToolkitCallToolFrame{}, err
	}
	payload := message.GetToolkitCallTool()
	encodedResult, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil || !matchesDigest(message.GetPayloadDigest(), encodedResult) {
		return outputapp.ToolkitCallToolFrame{}, outputapp.ErrInvalidToolkitCallToolOutput
	}
	result, err := toolkitCallToolResultDomain(payload)
	if err != nil {
		return outputapp.ToolkitCallToolFrame{}, err
	}
	// DERIVED here, not read from the proposal. A tool that raised is a
	// completed run; only the two refusals made before any provider work settle
	// as FAILED. A worker whose proposal disagrees is refused by
	// settlementProposalDomain rather than believed.
	expectedOutcome := executionapp.SettlementSucceeded
	switch result.ResultSummary.Status {
	case outputapp.ToolkitCallToolStatusUnsupportedToolkit,
		outputapp.ToolkitCallToolStatusUnknownTool:
		expectedOutcome = executionapp.SettlementFailed
	}
	settlement, encodedSettlement, err := s.settlementProposalDomain(
		message, fence, encodedResult, expectedOutcome,
	)
	if err != nil {
		return outputapp.ToolkitCallToolFrame{}, outputapp.ErrInvalidToolkitCallToolOutput
	}
	frame := outputapp.ToolkitCallToolFrame{
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
		return outputapp.ToolkitCallToolFrame{}, err
	}
	return frame, nil
}

func toolkitCallToolResultDomain(
	result *runtimev1.ToolkitCallToolResultV1,
) (outputapp.ToolkitCallToolResult, error) {
	if result == nil {
		return outputapp.ToolkitCallToolResult{}, outputapp.ErrInvalidToolkitCallToolOutput
	}
	// result_artifact is refused rather than ignored. This platform has no
	// artifact writer, so nothing can have produced one, and accepting an
	// artifact reference here would admit a result whose body no reader on this
	// side can fetch.
	if result.GetResultArtifact() != nil || result.GetResultSummary() == nil {
		return outputapp.ToolkitCallToolResult{}, outputapp.ErrInvalidToolkitCallToolOutput
	}
	bundleDigest, err := digestDomain(result.GetInputBundleDigest())
	if err != nil {
		return outputapp.ToolkitCallToolResult{}, outputapp.ErrInvalidToolkitCallToolOutput
	}
	settingsDigest, err := digestDomain(result.GetSettingsContentDigest())
	if err != nil {
		return outputapp.ToolkitCallToolResult{}, outputapp.ErrInvalidToolkitCallToolOutput
	}
	argumentsDigest, err := digestDomain(result.GetArgumentsContentDigest())
	if err != nil {
		return outputapp.ToolkitCallToolResult{}, outputapp.ErrInvalidToolkitCallToolOutput
	}
	summary, err := toolkitCallToolSummaryDomain(result.GetResultSummary())
	if err != nil {
		return outputapp.ToolkitCallToolResult{}, err
	}
	return outputapp.ToolkitCallToolResult{
		ToolkitType:       result.GetToolkitType(),
		ToolName:          result.GetToolName(),
		InputBundleID:     result.GetInputBundleId(),
		InputBundleDigest: bundleDigest,
		Settings: outputapp.ToolkitCallToolInputBinding{
			EntryID:          result.GetSettingsEntryId(),
			ImmutableVersion: result.GetSettingsEntryVersion(),
			ContentDigest:    settingsDigest,
		},
		Arguments: outputapp.ToolkitCallToolInputBinding{
			EntryID: result.GetArgumentsEntryId(),
			// ToolkitCallToolResultV1 carries no arguments_entry_version: the
			// digest is the version for that entry, and the admitted binding
			// derives the version from the same digest, so the two still
			// compare exactly.
			ImmutableVersion: argumentsDigest.String(),
			ContentDigest:    argumentsDigest,
		},
		ResultSummary: summary,
	}, nil
}

func toolkitCallToolSummaryDomain(
	summary *runtimev1.ToolkitCallToolSummaryV1,
) (outputapp.ToolkitCallToolSummary, error) {
	if summary == nil || hasUnknown(summary.ProtoReflect()) {
		return outputapp.ToolkitCallToolSummary{}, outputapp.ErrInvalidToolkitCallToolOutput
	}
	var status outputapp.ToolkitCallToolStatus
	switch summary.GetStatus() {
	case runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_OK:
		status = outputapp.ToolkitCallToolStatusOK
	case runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_TOOL_ERROR:
		status = outputapp.ToolkitCallToolStatusToolError
	case runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_UNSUPPORTED_TOOLKIT:
		status = outputapp.ToolkitCallToolStatusUnsupportedToolkit
	case runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_UNKNOWN_TOOL:
		status = outputapp.ToolkitCallToolStatusUnknownTool
	default:
		return outputapp.ToolkitCallToolSummary{}, outputapp.ErrInvalidToolkitCallToolOutput
	}
	domain := outputapp.ToolkitCallToolSummary{
		Status:       status,
		ResultJSON:   summary.GetResultJson(),
		Truncated:    summary.GetTruncated(),
		ErrorMessage: summary.GetErrorMessage(),
	}
	if err := domain.Validate(); err != nil {
		return outputapp.ToolkitCallToolSummary{}, err
	}
	return domain, nil
}
