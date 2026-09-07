package output

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	configurationdomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/configurations"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/nodeevent"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type WorkloadAuthorizer interface {
	// AuthorizeOutput derives the authenticated connection peer from ctx and
	// verifies that it owns the referenced persisted workload session and
	// producer. The frame fields are never treated as authenticated identity.
	AuthorizeOutput(ctx context.Context, workloadSessionID, producerID string) (workloadIdentity string, err error)
}

type ValidationIngestor interface {
	Ingest(ctx context.Context, frame outputapp.ConfigurationValidationFrame) (outputapp.ProjectionOutcome, error)
}

type RuntimeFailureIngestor interface {
	IngestFailure(ctx context.Context, frame outputapp.RuntimeFailureFrame) (outputapp.ProjectionOutcome, error)
}

type IndexIngestIngestor interface {
	IngestIndex(ctx context.Context, frame outputapp.IndexIngestFrame) (outputapp.ProjectionOutcome, error)
}

type AgentExecutionIngestor interface {
	IngestAgent(ctx context.Context, frame outputapp.AgentExecutionFrame) (outputapp.ProjectionOutcome, error)
}

type NodeEventIngestor interface {
	IngestNodeEvent(ctx context.Context, frame outputapp.NodeEventFrame) (outputapp.ProjectionOutcome, error)
}

type ServerConfig struct {
	OutputSchemaRevision string
	MaxFrameBytes        int
	CreditFrames         uint32
	CreditBytes          uint64
}

type Server struct {
	runtimev1.UnimplementedExecutionOutputServiceServer

	config     ServerConfig
	authorizer WorkloadAuthorizer
	ingestor   ValidationIngestor
	failures   RuntimeFailureIngestor
	indexes    IndexIngestIngestor
	agents     AgentExecutionIngestor
	nodeEvents NodeEventIngestor
	// toolRuns is composed through WithToolkitCallTool rather than positionally;
	// see toolkit_call_tool.go for why.
	toolRuns ToolkitCallToolIngestor
}

func NewServer(config ServerConfig, authorizer WorkloadAuthorizer, ingestor ValidationIngestor, failures RuntimeFailureIngestor) (*Server, error) {
	return newServer(config, authorizer, ingestor, failures, nil, nil, nil)
}

// NewServerWithIndexIngest adds the typed index.ingest.v1 terminal-output
// boundary. The original constructor remains available so production
// composition can be migrated separately and explicitly.
func NewServerWithIndexIngest(config ServerConfig, authorizer WorkloadAuthorizer, ingestor ValidationIngestor, failures RuntimeFailureIngestor, indexes IndexIngestIngestor) (*Server, error) {
	if indexes == nil {
		return nil, errors.New("index ingest output ingestor is required")
	}
	return newServer(config, authorizer, ingestor, failures, indexes, nil, nil)
}

// NewServerWithIndexIngestAndNodeEvents adds current-NodeEvent progress to the
// index output stream while retaining the existing terminal result boundary.
func NewServerWithIndexIngestAndNodeEvents(config ServerConfig, authorizer WorkloadAuthorizer, ingestor ValidationIngestor, failures RuntimeFailureIngestor, indexes IndexIngestIngestor, nodeEvents NodeEventIngestor) (*Server, error) {
	if indexes == nil || nodeEvents == nil {
		return nil, errors.New("index ingest and node event output ingestors are required")
	}
	return newServer(config, authorizer, ingestor, failures, indexes, nil, nodeEvents)
}

// NewServerWithAgentAndNodeEvents composes agent execution independently of
// indexing. Both capabilities may share the authenticated output listener,
// but neither is an enablement prerequisite for the other.
func NewServerWithAgentAndNodeEvents(config ServerConfig, authorizer WorkloadAuthorizer, ingestor ValidationIngestor, failures RuntimeFailureIngestor, agents AgentExecutionIngestor, nodeEvents NodeEventIngestor) (*Server, error) {
	if agents == nil || nodeEvents == nil {
		return nil, errors.New("agent and node event output ingestors are required")
	}
	return newServer(config, authorizer, ingestor, failures, nil, agents, nodeEvents)
}

// NewServerWithIndexAgentAndNodeEvents composes the two progress-capable
// worker capabilities on the same authenticated output listener.
func NewServerWithIndexAgentAndNodeEvents(config ServerConfig, authorizer WorkloadAuthorizer, ingestor ValidationIngestor, failures RuntimeFailureIngestor, indexes IndexIngestIngestor, agents AgentExecutionIngestor, nodeEvents NodeEventIngestor) (*Server, error) {
	if indexes == nil || agents == nil || nodeEvents == nil {
		return nil, errors.New("index, agent and node event output ingestors are required")
	}
	return newServer(config, authorizer, ingestor, failures, indexes, agents, nodeEvents)
}

func newServer(config ServerConfig, authorizer WorkloadAuthorizer, ingestor ValidationIngestor, failures RuntimeFailureIngestor, indexes IndexIngestIngestor, agents AgentExecutionIngestor, nodeEvents NodeEventIngestor) (*Server, error) {
	if authorizer == nil || ingestor == nil || failures == nil {
		return nil, errors.New("output authorizer, validation ingestor and runtime failure ingestor are required")
	}
	if config.OutputSchemaRevision == "" || config.MaxFrameBytes <= 0 || config.CreditFrames == 0 || config.CreditBytes == 0 {
		return nil, errors.New("output schema and limits are required")
	}
	return &Server{config: config, authorizer: authorizer, ingestor: ingestor, failures: failures, indexes: indexes, agents: agents, nodeEvents: nodeEvents}, nil
}

func (s *Server) Publish(stream grpc.BidiStreamingServer[runtimev1.ExecutionOutputFrameV1, runtimev1.ExecutionOutputAckV1]) error {
	// The worker must receive credit before it can legally send its first
	// frame. Sending this transport-only acknowledgement first avoids the
	// zero-credit bidi-stream deadlock without trusting caller identity.
	if err := stream.Send(&runtimev1.ExecutionOutputAckV1{
		CreditFrames: s.config.CreditFrames,
		CreditBytes:  s.config.CreditBytes,
		DesiredState: runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_RUNNING,
	}); err != nil {
		return err
	}

	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		identity := message.GetIdentity()
		wireFence := message.GetFence()
		if identity == nil || wireFence == nil {
			if err := stream.Send(rejectionAck(message, runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The output frame is malformed.", false)); err != nil {
				return err
			}
			return nil
		}
		workloadIdentity, err := s.authorizer.AuthorizeOutput(stream.Context(), wireFence.GetWorkloadSessionId(), wireFence.GetProducerId())
		if err != nil || workloadIdentity == "" {
			if err := stream.Send(rejectionAck(message, runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, "The output workload session is not accepted.", false)); err != nil {
				return err
			}
			return nil
		}

		outcome, err := s.ingestMessage(stream.Context(), message, workloadIdentity)
		if err != nil {
			code, safeMessage, retryable := safeOutputError(err)
			slog.WarnContext(
				stream.Context(),
				"runtime output frame rejected",
				"execution_id", identity.GetExecutionId(),
				"generation", identity.GetGeneration(),
				"event_type", message.GetEventType().String(),
				"sequence", message.GetSequence(),
				"handoff_watermark", message.GetClaimHandoffWatermark(),
				"terminal", message.GetTerminal(),
				"error_code", code.String(),
				"cause", err,
			)
			ack := rejectionAck(message, code, safeMessage, retryable)
			if errors.Is(err, outputapp.ErrOutputCancelled) {
				// This exact, fully bound response is the output linearization
				// result. A worker may replace its local terminal frame only after
				// validating every binding below; a generic stale/network error is
				// deliberately insufficient.
				ack.DesiredState = runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_CANCELLED
			} else if errors.Is(err, outputapp.ErrOutputDeadlineExceeded) {
				// The database clock rejected this exact first output while its
				// authority remained live. This bound marker permits replacement
				// only with the canonical deadline failure.
				ack.DesiredState = runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_RUNNING
			}
			if err := stream.Send(ack); err != nil {
				return err
			}
			return nil
		}
		if outcome.CommittedSequence != message.GetSequence() {
			if err := stream.Send(rejectionAck(message, runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The durable output sequence is not contiguous.", false)); err != nil {
				return err
			}
			return nil
		}

		ack := &runtimev1.ExecutionOutputAckV1{
			StreamId:                    message.GetStreamId(),
			Identity:                    proto.Clone(identity).(*runtimev1.ExecutionIdentityV1),
			Fence:                       proto.Clone(wireFence).(*runtimev1.ExecutionFenceV1),
			CommittedContiguousSequence: outcome.CommittedSequence,
			ClaimHandoffWatermark:       message.GetClaimHandoffWatermark(),
			CreditFrames:                s.config.CreditFrames,
			CreditBytes:                 s.config.CreditBytes,
			DesiredState:                runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_RUNNING,
		}
		if err := stream.Send(ack); err != nil {
			return err
		}
		if message.GetTerminal() {
			// A durable terminal acknowledgement closes the logical stream and
			// prevents a second terminal frame. Non-terminal NodeEvents continue
			// under the replenished credit returned above.
			return nil
		}
	}
}

func (s *Server) ingestMessage(ctx context.Context, message *runtimev1.ExecutionOutputFrameV1, workloadIdentity string) (outputapp.ProjectionOutcome, error) {
	switch message.GetEventType() {
	case runtimev1.ExecutionOutputEventTypeV1_EXECUTION_OUTPUT_EVENT_TYPE_V1_CONFIGURATION_VALIDATION_RESULT:
		frame, err := s.validationFrame(message, workloadIdentity)
		if err != nil {
			return outputapp.ProjectionOutcome{}, err
		}
		return s.ingestor.Ingest(ctx, frame)
	case runtimev1.ExecutionOutputEventTypeV1_EXECUTION_OUTPUT_EVENT_TYPE_V1_RUNTIME_ERROR:
		frame, err := s.runtimeFailureFrame(message, workloadIdentity)
		if err != nil {
			return outputapp.ProjectionOutcome{}, err
		}
		return s.failures.IngestFailure(ctx, frame)
	case runtimev1.ExecutionOutputEventTypeV1_EXECUTION_OUTPUT_EVENT_TYPE_V1_INDEX_INGEST_RESULT:
		if s.indexes == nil {
			return outputapp.ProjectionOutcome{}, outputapp.ErrInvalidIndexIngestOutput
		}
		frame, err := s.indexIngestFrame(message, workloadIdentity)
		if err != nil {
			return outputapp.ProjectionOutcome{}, err
		}
		return s.indexes.IngestIndex(ctx, frame)
	case runtimev1.ExecutionOutputEventTypeV1_EXECUTION_OUTPUT_EVENT_TYPE_V1_AGENT_EXECUTION_RESULT:
		if s.agents == nil {
			return outputapp.ProjectionOutcome{}, outputapp.ErrInvalidAgentExecutionOutput
		}
		frame, err := s.agentExecutionFrame(message, workloadIdentity)
		if err != nil {
			return outputapp.ProjectionOutcome{}, err
		}
		return s.agents.IngestAgent(ctx, frame)
	case runtimev1.ExecutionOutputEventTypeV1_EXECUTION_OUTPUT_EVENT_TYPE_V1_TOOLKIT_CALL_TOOL_RESULT:
		if s.toolRuns == nil {
			return outputapp.ProjectionOutcome{}, outputapp.ErrInvalidToolkitCallToolOutput
		}
		frame, err := s.toolkitCallToolFrame(message, workloadIdentity)
		if err != nil {
			return outputapp.ProjectionOutcome{}, err
		}
		return s.toolRuns.IngestToolkitCallTool(ctx, frame)
	case runtimev1.ExecutionOutputEventTypeV1_EXECUTION_OUTPUT_EVENT_TYPE_V1_NODE_EVENT:
		if s.nodeEvents == nil {
			return outputapp.ProjectionOutcome{}, outputapp.ErrInvalidNodeEventOutput
		}
		frame, err := s.nodeEventFrame(message, workloadIdentity)
		if err != nil {
			return outputapp.ProjectionOutcome{}, err
		}
		return s.nodeEvents.IngestNodeEvent(ctx, frame)
	default:
		return outputapp.ProjectionOutcome{}, outputapp.ErrInvalidValidationOutput
	}
}

func (s *Server) nodeEventFrame(message *runtimev1.ExecutionOutputFrameV1, workloadIdentity string) (outputapp.NodeEventFrame, error) {
	if message == nil || message.GetOutputSchemaRevision() != s.config.OutputSchemaRevision || hasUnknown(message.ProtoReflect()) || !validStreamIdentity(message) || message.GetOccurredAtUnixMillis() <= 0 {
		return outputapp.NodeEventFrame{}, outputapp.ErrInvalidNodeEventOutput
	}
	encodedFrame, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil || len(encodedFrame) > s.config.MaxFrameBytes {
		return outputapp.NodeEventFrame{}, outputapp.ErrInvalidNodeEventOutput
	}
	if message.GetEventType() != runtimev1.ExecutionOutputEventTypeV1_EXECUTION_OUTPUT_EVENT_TYPE_V1_NODE_EVENT || message.GetTerminal() || message.GetSettlementProposal() != nil || message.GetNodeEvent() == nil || message.GetConfigurationValidation() != nil || message.GetRuntimeError() != nil || message.GetToolkitAvailableTools() != nil || message.GetIndexIngest() != nil {
		return outputapp.NodeEventFrame{}, outputapp.ErrInvalidNodeEventOutput
	}
	identity := message.GetIdentity()
	fence, err := fenceDomain(identity, message.GetFence(), workloadIdentity)
	if err != nil {
		return outputapp.NodeEventFrame{}, err
	}
	payload := message.GetNodeEvent()
	encodedEvent, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil || !matchesDigest(message.GetPayloadDigest(), encodedEvent) {
		return outputapp.NodeEventFrame{}, outputapp.ErrInvalidNodeEventOutput
	}
	browserData, err := nodeevent.EncodeCurrentJSON(payload)
	if err != nil {
		return outputapp.NodeEventFrame{}, outputapp.ErrInvalidNodeEventOutput
	}
	frame := outputapp.NodeEventFrame{
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
		PayloadDigest:         runtimedomain.SHA256(encodedEvent),
		EncodedEvent:          encodedEvent,
		BrowserData:           browserData,
	}
	if err := frame.Validate(); err != nil {
		return outputapp.NodeEventFrame{}, err
	}
	return frame, nil
}

func (s *Server) indexIngestFrame(message *runtimev1.ExecutionOutputFrameV1, workloadIdentity string) (outputapp.IndexIngestFrame, error) {
	if message == nil || message.GetOutputSchemaRevision() != s.config.OutputSchemaRevision || hasUnknown(message.ProtoReflect()) || !validStreamIdentity(message) || message.GetOccurredAtUnixMillis() <= 0 {
		return outputapp.IndexIngestFrame{}, outputapp.ErrInvalidIndexIngestOutput
	}
	encodedFrame, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil || len(encodedFrame) > s.config.MaxFrameBytes {
		return outputapp.IndexIngestFrame{}, outputapp.ErrInvalidIndexIngestOutput
	}
	if message.GetEventType() != runtimev1.ExecutionOutputEventTypeV1_EXECUTION_OUTPUT_EVENT_TYPE_V1_INDEX_INGEST_RESULT || !message.GetTerminal() || message.GetIndexIngest() == nil || message.GetConfigurationValidation() != nil || message.GetRuntimeError() != nil || message.GetToolkitAvailableTools() != nil {
		return outputapp.IndexIngestFrame{}, outputapp.ErrInvalidIndexIngestOutput
	}
	identity := message.GetIdentity()
	fence, err := fenceDomain(identity, message.GetFence(), workloadIdentity)
	if err != nil {
		return outputapp.IndexIngestFrame{}, err
	}
	payload := message.GetIndexIngest()
	encodedResult, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil || !matchesDigest(message.GetPayloadDigest(), encodedResult) {
		return outputapp.IndexIngestFrame{}, outputapp.ErrInvalidIndexIngestOutput
	}
	result, err := indexIngestResultDomain(payload)
	if err != nil {
		return outputapp.IndexIngestFrame{}, err
	}
	expectedOutcome := executionapp.SettlementSucceeded
	if result.ResultSummary.Status == outputapp.IndexIngestStatusError {
		expectedOutcome = executionapp.SettlementFailed
	}
	settlement, encodedSettlement, err := s.settlementProposalDomain(message, fence, encodedResult, expectedOutcome)
	if err != nil {
		return outputapp.IndexIngestFrame{}, err
	}

	frame := outputapp.IndexIngestFrame{
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
		return outputapp.IndexIngestFrame{}, err
	}
	return frame, nil
}

func (s *Server) agentExecutionFrame(message *runtimev1.ExecutionOutputFrameV1, workloadIdentity string) (outputapp.AgentExecutionFrame, error) {
	if message == nil || message.GetOutputSchemaRevision() != s.config.OutputSchemaRevision || hasUnknown(message.ProtoReflect()) || !validStreamIdentity(message) || message.GetOccurredAtUnixMillis() <= 0 {
		return outputapp.AgentExecutionFrame{}, outputapp.ErrInvalidAgentExecutionOutput
	}
	encodedFrame, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil || len(encodedFrame) > s.config.MaxFrameBytes ||
		message.GetEventType() != runtimev1.ExecutionOutputEventTypeV1_EXECUTION_OUTPUT_EVENT_TYPE_V1_AGENT_EXECUTION_RESULT ||
		!message.GetTerminal() || message.GetAgentExecution() == nil ||
		message.GetConfigurationValidation() != nil || message.GetRuntimeError() != nil ||
		message.GetToolkitAvailableTools() != nil || message.GetIndexIngest() != nil || message.GetNodeEvent() != nil {
		return outputapp.AgentExecutionFrame{}, outputapp.ErrInvalidAgentExecutionOutput
	}
	identity := message.GetIdentity()
	fence, err := fenceDomain(identity, message.GetFence(), workloadIdentity)
	if err != nil {
		return outputapp.AgentExecutionFrame{}, err
	}
	payload := message.GetAgentExecution()
	encodedResult, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil || !matchesDigest(message.GetPayloadDigest(), encodedResult) {
		return outputapp.AgentExecutionFrame{}, outputapp.ErrInvalidAgentExecutionOutput
	}
	result, err := agentExecutionResultDomain(payload)
	if err != nil {
		return outputapp.AgentExecutionFrame{}, err
	}
	settlement, encodedSettlement, err := s.settlementProposalDomain(message, fence, encodedResult, executionapp.SettlementSucceeded)
	if err != nil {
		return outputapp.AgentExecutionFrame{}, outputapp.ErrInvalidAgentExecutionOutput
	}
	frame := outputapp.AgentExecutionFrame{
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
		return outputapp.AgentExecutionFrame{}, err
	}
	return frame, nil
}

func (s *Server) validationFrame(message *runtimev1.ExecutionOutputFrameV1, workloadIdentity string) (outputapp.ConfigurationValidationFrame, error) {
	if message == nil || message.GetOutputSchemaRevision() != s.config.OutputSchemaRevision || hasUnknown(message.ProtoReflect()) || !validStreamIdentity(message) || message.GetOccurredAtUnixMillis() <= 0 {
		return outputapp.ConfigurationValidationFrame{}, outputapp.ErrInvalidValidationOutput
	}
	encodedFrame, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil || len(encodedFrame) > s.config.MaxFrameBytes {
		return outputapp.ConfigurationValidationFrame{}, outputapp.ErrInvalidValidationOutput
	}
	if message.GetEventType() != runtimev1.ExecutionOutputEventTypeV1_EXECUTION_OUTPUT_EVENT_TYPE_V1_CONFIGURATION_VALIDATION_RESULT || !message.GetTerminal() || message.GetConfigurationValidation() == nil || message.GetRuntimeError() != nil {
		return outputapp.ConfigurationValidationFrame{}, outputapp.ErrInvalidValidationOutput
	}
	identity := message.GetIdentity()
	fence, err := fenceDomain(identity, message.GetFence(), workloadIdentity)
	if err != nil {
		return outputapp.ConfigurationValidationFrame{}, err
	}
	payload := message.GetConfigurationValidation()
	encodedResult, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil || !matchesDigest(message.GetPayloadDigest(), encodedResult) {
		return outputapp.ConfigurationValidationFrame{}, outputapp.ErrInvalidValidationOutput
	}
	settlement, encodedSettlement, err := s.settlementProposalDomain(message, fence, encodedResult, executionapp.SettlementSucceeded)
	if err != nil {
		return outputapp.ConfigurationValidationFrame{}, err
	}
	result, err := validationResultDomain(payload)
	if err != nil {
		return outputapp.ConfigurationValidationFrame{}, err
	}

	return outputapp.ConfigurationValidationFrame{
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
	}, nil
}

func (s *Server) runtimeFailureFrame(message *runtimev1.ExecutionOutputFrameV1, workloadIdentity string) (outputapp.RuntimeFailureFrame, error) {
	if message == nil || message.GetOutputSchemaRevision() != s.config.OutputSchemaRevision || hasUnknown(message.ProtoReflect()) || !validStreamIdentity(message) || message.GetOccurredAtUnixMillis() <= 0 {
		return outputapp.RuntimeFailureFrame{}, outputapp.ErrInvalidValidationOutput
	}
	encodedFrame, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil || len(encodedFrame) > s.config.MaxFrameBytes {
		return outputapp.RuntimeFailureFrame{}, outputapp.ErrInvalidValidationOutput
	}
	if message.GetEventType() != runtimev1.ExecutionOutputEventTypeV1_EXECUTION_OUTPUT_EVENT_TYPE_V1_RUNTIME_ERROR || !message.GetTerminal() || message.GetRuntimeError() == nil || message.GetConfigurationValidation() != nil {
		return outputapp.RuntimeFailureFrame{}, outputapp.ErrInvalidValidationOutput
	}
	identity := message.GetIdentity()
	fence, err := fenceDomain(identity, message.GetFence(), workloadIdentity)
	if err != nil {
		return outputapp.RuntimeFailureFrame{}, err
	}
	payload := message.GetRuntimeError()
	encodedFailure, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil || !matchesDigest(message.GetPayloadDigest(), encodedFailure) {
		return outputapp.RuntimeFailureFrame{}, outputapp.ErrInvalidValidationOutput
	}
	policy, ok := runtimeFailurePolicyFor(payload.GetCode())
	if !ok || payload.GetSafeMessage() != policy.SafeMessage || payload.GetRetryable() != policy.Retryable {
		return outputapp.RuntimeFailureFrame{}, outputapp.ErrInvalidValidationOutput
	}
	expectedOutcome := executionapp.SettlementFailed
	if payload.GetCode() == runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_CANCELLED {
		expectedOutcome = executionapp.SettlementCancelled
	}
	settlement, encodedSettlement, err := s.settlementProposalDomain(message, fence, encodedFailure, expectedOutcome)
	if err != nil {
		return outputapp.RuntimeFailureFrame{}, err
	}
	return outputapp.RuntimeFailureFrame{
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
		PayloadDigest:         runtimedomain.SHA256(encodedFailure),
		EncodedFailure:        encodedFailure,
		Settlement:            settlement,
		EncodedSettlement:     encodedSettlement,
		Failure: outputapp.RuntimeFailure{
			Code:        policy.Code,
			SafeMessage: payload.GetSafeMessage(),
			Retryable:   payload.GetRetryable(),
		},
	}, nil
}

func validStreamIdentity(frame *runtimev1.ExecutionOutputFrameV1) bool {
	identity := frame.GetIdentity()
	if identity == nil || identity.GetTenantId() == "" || identity.GetResourceProjectId() == "" || identity.GetProjectionProjectId() == "" || identity.GetCommandId() == "" || identity.GetExecutionId() == "" || identity.GetGeneration() == 0 {
		return false
	}
	want := identity.GetExecutionId() + ":" + strconv.FormatUint(identity.GetGeneration(), 10)
	return frame.GetStreamId() == want
}

func validationResultDomain(result *runtimev1.ConfigurationValidationResultV1) (configurationdomain.ValidationResult, error) {
	catalogDigest, err := digestDomain(result.GetCatalogDigest())
	if err != nil {
		return configurationdomain.ValidationResult{}, err
	}
	schemaDigest, err := digestDomain(result.GetSchemaDigest())
	if err != nil {
		return configurationdomain.ValidationResult{}, err
	}
	bundleDigest, err := digestDomain(result.GetInputBundleDigest())
	if err != nil {
		return configurationdomain.ValidationResult{}, err
	}
	contentDigest, err := digestDomain(result.GetSettingsContentDigest())
	if err != nil {
		return configurationdomain.ValidationResult{}, err
	}
	issues := make([]configurationdomain.ValidationIssue, len(result.GetIssues()))
	for i, issue := range result.GetIssues() {
		if issue == nil {
			return configurationdomain.ValidationResult{}, configurationdomain.ErrInvalidValidationResult
		}
		issues[i] = configurationdomain.ValidationIssue{Code: issue.GetCode(), JSONPointer: issue.GetJsonPointer(), SafeMessage: issue.GetSafeMessage()}
	}
	mapped := configurationdomain.ValidationResult{
		Binding: configurationdomain.ValidationBinding{
			Command: configurationdomain.ValidationCommand{
				ConfigurationRevisionID: result.GetConfigurationRevisionId(),
				ConfigurationType:       result.GetConfigurationType(),
				CatalogRevision:         result.GetCatalogRevision(),
				CatalogDigest:           catalogDigest,
				SchemaID:                result.GetSchemaId(),
				SchemaRevision:          result.GetSchemaRevision(),
				SchemaDigest:            schemaDigest,
				SettingsEntryID:         result.GetSettingsEntryId(),
			},
			InputBundleID:         result.GetInputBundleId(),
			InputBundleDigest:     bundleDigest,
			SettingsEntryVersion:  result.GetSettingsEntryVersion(),
			SettingsContentDigest: contentDigest,
		},
		Valid:  result.GetValid(),
		Issues: issues,
	}
	if err := mapped.Validate(); err != nil {
		return configurationdomain.ValidationResult{}, err
	}
	return mapped, nil
}

func indexIngestResultDomain(result *runtimev1.IndexIngestResultV1) (outputapp.IndexIngestResult, error) {
	if result == nil {
		return outputapp.IndexIngestResult{}, outputapp.ErrInvalidIndexIngestOutput
	}
	bundleDigest, err := digestDomain(result.GetInputBundleDigest())
	if err != nil {
		return outputapp.IndexIngestResult{}, outputapp.ErrInvalidIndexIngestOutput
	}
	toolkitConfiguration, err := indexInputBindingDomain(result.GetToolkitConfiguration())
	if err != nil {
		return outputapp.IndexIngestResult{}, err
	}
	toolParameters, err := indexInputBindingDomain(result.GetToolParameters())
	if err != nil {
		return outputapp.IndexIngestResult{}, err
	}
	llmModel, err := optionalIndexInputBindingDomain(result.GetLlmModel())
	if err != nil {
		return outputapp.IndexIngestResult{}, err
	}
	llmConfiguration, err := optionalIndexInputBindingDomain(result.GetLlmConfiguration())
	if err != nil {
		return outputapp.IndexIngestResult{}, err
	}
	mcpTokens, err := optionalIndexInputBindingDomain(result.GetMcpTokens())
	if err != nil {
		return outputapp.IndexIngestResult{}, err
	}
	embeddingBinding, err := optionalIndexInputBindingDomain(
		result.GetEmbeddingBinding(),
	)
	if err != nil {
		return outputapp.IndexIngestResult{}, err
	}
	artifact := outputapp.IndexArtifactReference{}
	if result.GetResultArtifact() != nil {
		artifact, err = indexArtifactReferenceDomain(result.GetResultArtifact())
		if err != nil {
			return outputapp.IndexIngestResult{}, err
		}
	}
	resultSummary := outputapp.IndexIngestSummary{}
	if result.GetResultSummary() != nil {
		resultSummary, err = indexIngestSummaryDomain(result.GetResultSummary())
		if err != nil {
			return outputapp.IndexIngestResult{}, err
		}
	}

	mapped := outputapp.IndexIngestResult{
		InputBundleID:     result.GetInputBundleId(),
		InputBundleDigest: bundleDigest,
		Bindings: outputapp.IndexIngestBindings{
			ToolkitConfiguration: toolkitConfiguration,
			ToolParameters:       toolParameters,
			LLMModel:             llmModel,
			LLMConfiguration:     llmConfiguration,
			MCPTokens:            mcpTokens,
			EmbeddingBinding:     embeddingBinding,
		},
		ResultArtifact: artifact,
		ResultSummary:  resultSummary,
	}
	if err := mapped.Validate(); err != nil {
		return outputapp.IndexIngestResult{}, err
	}
	return mapped, nil
}

func agentExecutionResultDomain(result *runtimev1.AgentExecutionResultV1) (outputapp.AgentExecutionResult, error) {
	if result == nil {
		return outputapp.AgentExecutionResult{}, outputapp.ErrInvalidAgentExecutionOutput
	}
	var terminalState outputapp.AgentExecutionTerminalState
	switch result.GetTerminalState() {
	case runtimev1.AgentExecutionTerminalStateV1_AGENT_EXECUTION_TERMINAL_STATE_V1_COMPLETED:
		terminalState = outputapp.AgentExecutionTerminalCompleted
	case runtimev1.AgentExecutionTerminalStateV1_AGENT_EXECUTION_TERMINAL_STATE_V1_PAUSED_HITL:
		terminalState = outputapp.AgentExecutionTerminalPausedHITL
	case runtimev1.AgentExecutionTerminalStateV1_AGENT_EXECUTION_TERMINAL_STATE_V1_PAUSED_MCP_AUTH:
		terminalState = outputapp.AgentExecutionTerminalPausedAuthorization
	default:
		return outputapp.AgentExecutionResult{}, outputapp.ErrInvalidAgentExecutionOutput
	}
	bundleDigest, err := digestDomain(result.GetInputBundleDigest())
	if err != nil {
		return outputapp.AgentExecutionResult{}, outputapp.ErrInvalidAgentExecutionOutput
	}
	requestDigest, err := digestDomain(result.GetRequestContentDigest())
	if err != nil {
		return outputapp.AgentExecutionResult{}, outputapp.ErrInvalidAgentExecutionOutput
	}
	artifact := result.GetResultArtifact()
	if artifact == nil {
		return outputapp.AgentExecutionResult{}, outputapp.ErrInvalidAgentExecutionOutput
	}
	artifactDigest, err := digestDomain(artifact.GetDigest())
	if err != nil {
		return outputapp.AgentExecutionResult{}, outputapp.ErrInvalidAgentExecutionOutput
	}
	mapped := outputapp.AgentExecutionResult{
		InputBundleID:           result.GetInputBundleId(),
		InputBundleDigest:       bundleDigest,
		RequestEntryID:          result.GetRequestEntryId(),
		RequestImmutableVersion: result.GetRequestImmutableVersion(),
		RequestContentDigest:    requestDigest,
		TerminalState:           terminalState,
		ResultArtifact: outputapp.AgentExecutionArtifactReference{
			ArtifactID:       artifact.GetArtifactId(),
			ImmutableVersion: artifact.GetImmutableVersion(),
			MediaType:        artifact.GetMediaType(),
			ByteLength:       artifact.GetByteLength(),
			Digest:           artifactDigest,
			Classification:   artifact.GetClassification(),
		},
		// #607. Absent on every ordinary turn. The entries are handed to the
		// domain's acceptance rule rather than being validated here, and that
		// rule DROPS what it refuses instead of failing the result -- the
		// reasoning is in AcceptedAgentExecutionAttachmentContents, and the
		// short form is that this frame carries the assistant's answer and a
		// bad attachment write-back must not cost the user that answer.
		//
		// Nothing is copied out of the proto message beyond the two scalars: a
		// `nil` entry in the repeated field is mapped to a zero-value struct,
		// which the acceptance rule then drops like any other malformed entry,
		// so a nil never has to be special-cased into a panic here.
		AttachmentContents: outputapp.AcceptedAgentExecutionAttachmentContents(
			agentExecutionAttachmentContentsDomain(result.GetAttachmentContents()),
		),
	}
	if err := mapped.Validate(); err != nil {
		return outputapp.AgentExecutionResult{}, err
	}
	return mapped, nil
}

func agentExecutionAttachmentContentsDomain(
	entries []*runtimev1.AgentExecutionAttachmentContentV1,
) []outputapp.AgentExecutionAttachmentContent {
	if len(entries) == 0 {
		return nil
	}
	mapped := make([]outputapp.AgentExecutionAttachmentContent, 0, len(entries))
	for _, entry := range entries {
		mapped = append(mapped, outputapp.AgentExecutionAttachmentContent{
			ItemID: entry.GetItemId(),
			// GetContent() aliases the decoded frame's buffer; the acceptance
			// rule clones what it keeps, so nothing that reaches a transaction
			// still points at protobuf-owned memory.
			Content: entry.GetContent(),
		})
	}
	return mapped
}

func indexIngestSummaryDomain(summary *runtimev1.IndexIngestSummaryV1) (outputapp.IndexIngestSummary, error) {
	if summary == nil {
		return outputapp.IndexIngestSummary{}, outputapp.ErrInvalidIndexIngestOutput
	}
	var status outputapp.IndexIngestStatus
	switch summary.GetStatus() {
	case runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_OK:
		status = outputapp.IndexIngestStatusOK
	case runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_PARTLY_INDEXED:
		status = outputapp.IndexIngestStatusPartlyIndexed
	case runtimev1.IndexIngestStatusV1_INDEX_INGEST_STATUS_V1_ERROR:
		status = outputapp.IndexIngestStatusError
	default:
		return outputapp.IndexIngestSummary{}, outputapp.ErrInvalidIndexIngestOutput
	}
	var terminalState outputapp.IndexIngestTerminalState
	switch summary.GetTerminalState() {
	case runtimev1.IndexIngestTerminalStateV1_INDEX_INGEST_TERMINAL_STATE_V1_UNSPECIFIED:
		terminalState = outputapp.IndexIngestTerminalUnspecified
	case runtimev1.IndexIngestTerminalStateV1_INDEX_INGEST_TERMINAL_STATE_V1_CREATED:
		terminalState = outputapp.IndexIngestTerminalCreated
	case runtimev1.IndexIngestTerminalStateV1_INDEX_INGEST_TERMINAL_STATE_V1_COMPLETED:
		terminalState = outputapp.IndexIngestTerminalCompleted
	case runtimev1.IndexIngestTerminalStateV1_INDEX_INGEST_TERMINAL_STATE_V1_SCHEDULED_REINDEX:
		terminalState = outputapp.IndexIngestTerminalScheduledReindex
	case runtimev1.IndexIngestTerminalStateV1_INDEX_INGEST_TERMINAL_STATE_V1_FAILED:
		terminalState = outputapp.IndexIngestTerminalFailed
	case runtimev1.IndexIngestTerminalStateV1_INDEX_INGEST_TERMINAL_STATE_V1_PARTLY_INDEXED:
		terminalState = outputapp.IndexIngestTerminalPartlyIndexed
	default:
		return outputapp.IndexIngestSummary{}, outputapp.ErrInvalidIndexIngestOutput
	}
	mapped := outputapp.IndexIngestSummary{
		Status:         status,
		Message:        summary.GetMessage(),
		TerminalState:  terminalState,
		Indexed:        summary.GetIndexed(),
		Updated:        summary.GetUpdated(),
		ReindexPresent: summary.Reindex != nil,
		Reindex:        summary.GetReindex(),
	}
	if err := mapped.Validate(); err != nil {
		return outputapp.IndexIngestSummary{}, err
	}
	return mapped, nil
}

func indexInputBindingDomain(binding *runtimev1.IndexIngestInputBindingV1) (outputapp.IndexInputBinding, error) {
	if binding == nil {
		return outputapp.IndexInputBinding{}, outputapp.ErrInvalidIndexIngestOutput
	}
	digest, err := digestDomain(binding.GetContentDigest())
	if err != nil {
		return outputapp.IndexInputBinding{}, outputapp.ErrInvalidIndexIngestOutput
	}
	mapped := outputapp.IndexInputBinding{
		EntryID:          binding.GetEntryId(),
		ImmutableVersion: binding.GetImmutableVersion(),
		ContentDigest:    digest,
	}
	if err := mapped.Validate(); err != nil {
		return outputapp.IndexInputBinding{}, err
	}
	return mapped, nil
}

func optionalIndexInputBindingDomain(binding *runtimev1.IndexIngestInputBindingV1) (outputapp.OptionalIndexInputBinding, error) {
	if binding == nil {
		return outputapp.OptionalIndexInputBinding{}, nil
	}
	mapped, err := indexInputBindingDomain(binding)
	if err != nil {
		return outputapp.OptionalIndexInputBinding{}, err
	}
	return outputapp.OptionalIndexInputBinding{Present: true, Binding: mapped}, nil
}

func indexArtifactReferenceDomain(artifact *runtimev1.IndexIngestArtifactReferenceV1) (outputapp.IndexArtifactReference, error) {
	if artifact == nil {
		return outputapp.IndexArtifactReference{}, outputapp.ErrInvalidIndexIngestOutput
	}
	digest, err := digestDomain(artifact.GetDigest())
	if err != nil {
		return outputapp.IndexArtifactReference{}, outputapp.ErrInvalidIndexIngestOutput
	}
	mapped := outputapp.IndexArtifactReference{
		ArtifactID:       artifact.GetArtifactId(),
		ImmutableVersion: artifact.GetImmutableVersion(),
		MediaType:        artifact.GetMediaType(),
		ByteLength:       artifact.GetByteLength(),
		Digest:           digest,
		Classification:   artifact.GetClassification(),
	}
	if err := mapped.Validate(); err != nil {
		return outputapp.IndexArtifactReference{}, err
	}
	return mapped, nil
}

func (s *Server) settlementProposalDomain(frame *runtimev1.ExecutionOutputFrameV1, fence runtimedomain.Fence, encodedPayload []byte, expectedOutcome executionapp.SettlementOutcome) (executionapp.SettlementProposal, []byte, error) {
	proposal := frame.GetSettlementProposal()
	wireOutcome := settlementOutcomeProto(expectedOutcome)
	if proposal == nil || proposal.GetProposalId() == "" || proposal.GetRequestedOutcome() != wireOutcome || proposal.GetPrepareIdempotencyKey() == "" || proposal.GetPrepareIdempotencyKey() != frame.GetIdentity().GetCommandId()+":prepare-settlement" || hasUnknown(proposal.ProtoReflect()) {
		return executionapp.SettlementProposal{}, nil, outputapp.ErrInvalidValidationOutput
	}
	if proposal.GetTerminalLogicalOutputId() != frame.GetLogicalOutputId() || proposal.GetTerminalEventId() != frame.GetEventId() || proposal.GetTerminalSequence() != frame.GetSequence() {
		return executionapp.SettlementProposal{}, nil, outputapp.ErrInvalidValidationOutput
	}
	if !matchesDigest(proposal.GetTerminalPayloadDigest(), encodedPayload) {
		return executionapp.SettlementProposal{}, nil, outputapp.ErrInvalidValidationOutput
	}
	encodedProposal, err := proto.MarshalOptions{Deterministic: true}.Marshal(proposal)
	if err != nil || len(encodedProposal) == 0 || len(encodedProposal) > s.config.MaxFrameBytes {
		return executionapp.SettlementProposal{}, nil, outputapp.ErrInvalidValidationOutput
	}
	payloadDigest, err := digestDomain(proposal.GetTerminalPayloadDigest())
	if err != nil {
		return executionapp.SettlementProposal{}, nil, outputapp.ErrInvalidValidationOutput
	}
	mapped := executionapp.SettlementProposal{
		Fence:                   fence,
		ProposalID:              proposal.GetProposalId(),
		Outcome:                 expectedOutcome,
		TerminalLogicalOutputID: proposal.GetTerminalLogicalOutputId(),
		TerminalEventID:         proposal.GetTerminalEventId(),
		TerminalSequence:        proposal.GetTerminalSequence(),
		TerminalPayloadDigest:   payloadDigest,
		ProposalDigest:          runtimedomain.SHA256(encodedProposal),
		IdempotencyKey:          proposal.GetPrepareIdempotencyKey(),
	}
	if err := mapped.Validate(); err != nil {
		return executionapp.SettlementProposal{}, nil, outputapp.ErrInvalidValidationOutput
	}
	return mapped, encodedProposal, nil
}

func settlementOutcomeProto(outcome executionapp.SettlementOutcome) runtimev1.ExecutionOutcomeV1 {
	switch outcome {
	case executionapp.SettlementSucceeded:
		return runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED
	case executionapp.SettlementFailed:
		return runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_FAILED
	case executionapp.SettlementCancelled:
		return runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_CANCELLED
	case executionapp.SettlementOutcomeUnknown:
		return runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_OUTCOME_UNKNOWN
	default:
		return runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_UNSPECIFIED
	}
}

type runtimeFailurePolicy struct {
	Code        string
	SafeMessage string
	Retryable   bool
}

func runtimeFailurePolicyFor(code runtimev1.RuntimeErrorCodeV1) (runtimeFailurePolicy, bool) {
	switch code {
	case runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY:
		return runtimeFailurePolicy{Code: "UNSUPPORTED_CAPABILITY", SafeMessage: "Configuration type is not supported."}, true
	case runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INCOMPATIBLE_VERSION:
		return runtimeFailurePolicy{Code: "INCOMPATIBLE_VERSION", SafeMessage: "The requested contract version is not compatible."}, true
	case runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INVALID_INPUT:
		return runtimeFailurePolicy{Code: "INVALID_INPUT", SafeMessage: "The execution input is invalid."}, true
	case runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_RESOURCE_EXHAUSTED:
		return runtimeFailurePolicy{Code: "RESOURCE_EXHAUSTED", SafeMessage: "The execution exceeded an approved resource limit."}, true
	case runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE:
		return runtimeFailurePolicy{Code: "DEPENDENCY_UNAVAILABLE", SafeMessage: "A required runtime dependency is unavailable.", Retryable: true}, true
	case runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED:
		return runtimeFailurePolicy{Code: "DEADLINE_EXCEEDED", SafeMessage: outputapp.DeadlineExceededSafeMessage, Retryable: true}, true
	case runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED:
		return runtimeFailurePolicy{Code: "AUTHORIZATION_FAILED", SafeMessage: "Execution authorization failed."}, true
	case runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_CANCELLED:
		return runtimeFailurePolicy{Code: "CANCELLED", SafeMessage: "Execution was cancelled."}, true
	case runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL:
		return runtimeFailurePolicy{Code: "INTERNAL", SafeMessage: "The runtime operation failed."}, true
	default:
		return runtimeFailurePolicy{}, false
	}
}

func fenceDomain(identity *runtimev1.ExecutionIdentityV1, fence *runtimev1.ExecutionFenceV1, workloadIdentity string) (runtimedomain.Fence, error) {
	var mapped runtimedomain.Fence
	if identity == nil || fence == nil || workloadIdentity == "" || len(fence.GetFenceToken()) != sha256.Size {
		return mapped, runtimedomain.ErrInvalidFence
	}
	mapped = runtimedomain.Fence{
		CommandID:         identity.GetCommandId(),
		ExecutionID:       identity.GetExecutionId(),
		Generation:        identity.GetGeneration(),
		WorkloadIdentity:  workloadIdentity,
		WorkloadSessionID: fence.GetWorkloadSessionId(),
		ProducerID:        fence.GetProducerId(),
		ClaimAttempt:      fence.GetClaimAttempt(),
		LeaseEpoch:        fence.GetLeaseEpoch(),
	}
	copy(mapped.Token[:], fence.GetFenceToken())
	if err := mapped.Validate(); err != nil {
		return runtimedomain.Fence{}, err
	}
	return mapped, nil
}

func digestDomain(digest *runtimev1.DigestV1) (runtimedomain.Digest, error) {
	var mapped runtimedomain.Digest
	if digest == nil || digest.GetAlgorithm() != runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256 || len(digest.GetValue()) != sha256.Size {
		return mapped, errors.New("invalid SHA-256 digest")
	}
	copy(mapped[:], digest.GetValue())
	return mapped, nil
}

func matchesDigest(digest *runtimev1.DigestV1, content []byte) bool {
	mapped, err := digestDomain(digest)
	return err == nil && mapped == runtimedomain.SHA256(content)
}

func hasUnknown(message protoreflect.Message) bool {
	if len(message.GetUnknown()) != 0 {
		return true
	}
	found := false
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Kind() != protoreflect.MessageKind {
			return true
		}
		if field.IsList() {
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				if hasUnknown(list.Get(i).Message()) {
					found = true
					return false
				}
			}
			return !found
		}
		found = hasUnknown(value.Message())
		return !found
	})
	return found
}

func safeOutputError(err error) (runtimev1.RuntimeErrorCodeV1, string, bool) {
	switch {
	case errors.Is(err, outputapp.ErrOutputCancelled):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_CANCELLED, "Execution cancellation won before this output became durable.", false
	case errors.Is(err, outputapp.ErrOutputDeadlineExceeded):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED, outputapp.DeadlineExceededSafeMessage, true
	case errors.Is(err, runtimedomain.ErrStaleFence), errors.Is(err, runtimedomain.ErrLeaseExpired):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE, "The output fence is no longer current.", false
	case errors.Is(err, configurationdomain.ErrValidationBindingMismatch):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INCOMPATIBLE_VERSION, "The output does not match the admitted validation input.", false
	case errors.Is(err, outputapp.ErrIndexIngestBindingMismatch):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INCOMPATIBLE_VERSION, "The output does not match the admitted index input.", false
	case errors.Is(err, outputapp.ErrAgentExecutionBindingMismatch):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INCOMPATIBLE_VERSION, "The output does not match the admitted agent input.", false
	case errors.Is(err, outputapp.ErrAgentExecutionResultMismatch):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The agent result does not match its durable full message.", false
	case errors.Is(err, outputapp.ErrIndexIngestArtifactUnavailable):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, "The referenced index artifact is not durably available.", true
	case errors.Is(err, outputapp.ErrIndexIngestArtifactMismatch):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The index artifact metadata is not accepted.", false
	case errors.Is(err, outputapp.ErrValidationOutputConflict):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The output identity conflicts with a durable output.", false
	case errors.Is(err, outputapp.ErrIndexIngestOutputConflict):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The index output identity conflicts with a durable output.", false
	case errors.Is(err, outputapp.ErrAgentExecutionOutputConflict):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The agent output identity conflicts with a durable output.", false
	case errors.Is(err, outputapp.ErrInvalidValidationOutput), errors.Is(err, outputapp.ErrInvalidIndexIngestOutput), errors.Is(err, outputapp.ErrInvalidAgentExecutionOutput), errors.Is(err, outputapp.ErrInvalidNodeEventOutput), errors.Is(err, outputapp.ErrNodeEventOutputConflict), errors.Is(err, configurationdomain.ErrInvalidValidationResult), errors.Is(err, runtimedomain.ErrInvalidFence):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The output frame is malformed.", false
	case errors.Is(err, context.Canceled):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_CANCELLED, "The output operation was cancelled.", false
	case errors.Is(err, context.DeadlineExceeded):
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, "The output deadline was exceeded.", true
	default:
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, "The output could not be durably accepted.", false
	}
}

func rejectionAck(frame *runtimev1.ExecutionOutputFrameV1, code runtimev1.RuntimeErrorCodeV1, safeMessage string, retryable bool) *runtimev1.ExecutionOutputAckV1 {
	ack := &runtimev1.ExecutionOutputAckV1{
		Rejection: &runtimev1.RuntimeErrorV1{Code: code, SafeMessage: safeMessage, Retryable: retryable},
	}
	if frame != nil {
		ack.StreamId = frame.GetStreamId()
		if frame.GetIdentity() != nil {
			ack.Identity = proto.Clone(frame.GetIdentity()).(*runtimev1.ExecutionIdentityV1)
		}
		if frame.GetFence() != nil {
			ack.Fence = proto.Clone(frame.GetFence()).(*runtimev1.ExecutionFenceV1)
		}
		ack.ClaimHandoffWatermark = frame.GetClaimHandoffWatermark()
	}
	return ack
}

var _ runtimev1.ExecutionOutputServiceServer = (*Server)(nil)
