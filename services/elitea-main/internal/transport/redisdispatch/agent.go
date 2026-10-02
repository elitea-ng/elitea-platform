package redisdispatch

import (
	"context"
	"errors"
	"strings"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	toolkitexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

const maxAgentRedisEntryBytes = (64 << 10) - 1

var ErrInvalidAgentExecutionCommand = errors.New("invalid agent execution command")

// AgentExecutionProducerConfig binds both current agent semantics to one
// bounded control stream. The capability ID and command type select the
// semantic entry point without exposing request content on Redis. Runtime
// composition may deliberately share this stream with the same worker pool
// that executes indexing; configuration validation remains isolated.
type AgentExecutionProducerConfig struct {
	Stream                       string
	ConsumerGroup                string
	ValidationStream             string
	IndexIngestStream            string
	ProtocolRevision             string
	EnvelopeSchemaRevision       string
	ApplicationCapabilityVersion string
	AdhocCapabilityVersion       string
	ToolkitReadCapabilityVersion string
	Limits                       Limits
	AllowTestOnlyHMAC            bool
}

type AgentExecutionProducer struct {
	producer           *Producer
	stream             string
	consumerGroup      string
	protocolRevision   string
	capabilityVersions map[string]string
}

func NewAgentExecutionProducer(config AgentExecutionProducerConfig, signer CommandSigner, appender StreamAppender) (*AgentExecutionProducer, error) {
	for _, route := range []string{config.Stream, config.ConsumerGroup, config.ValidationStream} {
		if !validAgentRoutingName(route) {
			return nil, errors.New("invalid agent execution Redis route")
		}
	}
	if config.IndexIngestStream != "" && !validAgentRoutingName(config.IndexIngestStream) {
		return nil, errors.New("invalid agent execution Redis route")
	}
	if redisRouteKeysOverlap(config.Stream, config.ValidationStream) {
		return nil, errors.New("agent execution cannot share the configuration-validation Redis stream")
	}
	if config.IndexIngestStream != "" && config.Stream != config.IndexIngestStream &&
		redisRouteKeysOverlap(config.Stream, config.IndexIngestStream) {
		return nil, errors.New("agent execution Redis stream overlaps the index delivery index")
	}
	for _, version := range []string{
		config.ApplicationCapabilityVersion,
		config.AdhocCapabilityVersion,
		config.ToolkitReadCapabilityVersion,
	} {
		if version == "" || len(version) > config.Limits.MaxStringBytes || strings.ContainsAny(version, "\r\n\x00") {
			return nil, errors.New("invalid agent execution capability version")
		}
	}
	if config.Limits.MaxRedisEntryBytes <= 0 || config.Limits.MaxRedisEntryBytes > maxAgentRedisEntryBytes {
		return nil, errors.New("agent execution Redis entry limit must be less than 64 KiB")
	}
	producer, err := NewProducer(ProducerConfig{
		Stream:                 config.Stream,
		ProtocolRevision:       config.ProtocolRevision,
		EnvelopeSchemaRevision: config.EnvelopeSchemaRevision,
		Limits:                 config.Limits,
		AllowTestOnlyHMAC:      config.AllowTestOnlyHMAC,
	}, signer, appender)
	if err != nil {
		return nil, err
	}
	return &AgentExecutionProducer{
		producer:         producer,
		stream:           config.Stream,
		consumerGroup:    config.ConsumerGroup,
		protocolRevision: config.ProtocolRevision,
		capabilityVersions: map[string]string{
			executiondomain.AgentApplicationCapability:   config.ApplicationCapabilityVersion,
			executiondomain.AgentAdhocCapability:         config.AdhocCapabilityVersion,
			executiondomain.ToolkitExecuteReadCapability: config.ToolkitReadCapabilityVersion,
		},
	}, nil
}

func (p *AgentExecutionProducer) PrepareToolkitExecuteRead(
	ctx context.Context,
	dispatch toolkitexecutionapp.ToolkitExecuteReadDispatch,
) (executionapp.PreparedCommandEnvelope, error) {
	version, ok := p.capabilityVersions[dispatch.CapabilityID]
	if !ok || dispatch.CapabilityVersion != version ||
		dispatch.LimitsRevision != p.producer.config.Limits.Revision {
		return executionapp.PreparedCommandEnvelope{}, toolkitexecutionapp.ErrInvalidToolkitExecuteReadDispatch
	}
	command, err := toolkitExecuteReadWorkerCommand(p.protocolRevision, dispatch)
	if err != nil {
		return executionapp.PreparedCommandEnvelope{}, err
	}
	return p.Prepare(ctx, command)
}

func (p *AgentExecutionProducer) Stream() string {
	return p.stream
}

func (p *AgentExecutionProducer) ConsumerGroup() string {
	return p.consumerGroup
}

func (p *AgentExecutionProducer) PrepareAgentExecution(ctx context.Context, dispatch agentexecutionapp.AgentExecutionDispatch) (executionapp.PreparedCommandEnvelope, error) {
	version, ok := p.capabilityVersions[dispatch.CapabilityID]
	if !ok || dispatch.CapabilityVersion != version ||
		dispatch.LimitsRevision != p.producer.config.Limits.Revision {
		return executionapp.PreparedCommandEnvelope{}, agentexecutionapp.ErrInvalidAgentExecutionDispatch
	}
	command, err := agentExecutionWorkerCommand(p.protocolRevision, dispatch)
	if err != nil {
		return executionapp.PreparedCommandEnvelope{}, err
	}
	return p.Prepare(ctx, command)
}

func (p *AgentExecutionProducer) Prepare(ctx context.Context, command *runtimev1.WorkerCommandV1) (executionapp.PreparedCommandEnvelope, error) {
	if ctx == nil || command == nil || hasUnknownFields(command.ProtoReflect()) {
		return executionapp.PreparedCommandEnvelope{}, ErrInvalidAgentExecutionCommand
	}
	if err := p.validateCommand(command); err != nil {
		return executionapp.PreparedCommandEnvelope{}, err
	}
	return p.producer.prepareCommand(ctx, command)
}

func (p *AgentExecutionProducer) AppendPrepared(ctx context.Context, deliveryID string, prepared executionapp.PreparedCommandEnvelope) error {
	command, err := p.producer.preparedCommand(prepared)
	if err != nil {
		return err
	}
	if err := p.validateCommand(command); err != nil {
		return executionapp.ErrInvalidPreparedEnvelope
	}
	return p.producer.appendPreparedCommand(ctx, deliveryID, prepared, command)
}

func (p *AgentExecutionProducer) validateCommand(command *runtimev1.WorkerCommandV1) error {
	if command.GetProtocolRevision() != p.protocolRevision ||
		command.GetLimitsRevision() != p.producer.config.Limits.Revision {
		return ErrInvalidAgentExecutionCommand
	}
	expectedVersion, ok := p.capabilityVersions[command.GetCapabilityId()]
	if !ok || command.GetCapabilityVersion() != expectedVersion {
		return ErrInvalidAgentExecutionCommand
	}
	if command.GetCapabilityId() == executiondomain.ToolkitExecuteReadCapability {
		return p.validateToolkitExecuteReadCommand(command)
	}
	expectedType := runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION
	if command.GetCapabilityId() == executiondomain.AgentAdhocCapability {
		expectedType = runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_ADHOC
	}
	input := command.GetInputBundleRef()
	agent := command.GetAgentExecution()
	if command.GetCommandType() != expectedType || input == nil || agent == nil ||
		command.GetRootExecutionId() != command.GetExecutionId() ||
		command.GetParentExecutionId() != "" || command.GetParentCallId() != "" {
		return ErrInvalidAgentExecutionCommand
	}
	values := []string{
		command.GetCommandId(), command.GetIdempotencyKey(), command.GetExecutionId(),
		command.GetTenantId(), command.GetResourceProjectId(), command.GetProjectionProjectId(),
		command.GetPrincipalRef(), command.GetResourceClass(), command.GetIsolationClass(),
		input.GetInputBundleId(), input.GetImmutableVersion(), input.GetMediaType(),
		agent.GetRequestEntryId(), agent.GetClientStreamId(), agent.GetClientMessageId(), agent.GetSioEvent(),
	}
	for _, value := range values {
		if !validAgentText(value, p.producer.config.Limits.MaxStringBytes) {
			return ErrInvalidAgentExecutionCommand
		}
	}
	if agent.GetSioEvent() != "chat_predict" && agent.GetSioEvent() != "chat_continue_predict" {
		return ErrInvalidAgentExecutionCommand
	}
	if command.GetGeneration() == 0 || command.GetDispatchOrdinal() == 0 ||
		command.GetPriority() == 0 || command.GetDeadlineUnixMillis() <= 0 ||
		input.GetByteLength() == 0 || !validSHA256Digest(input.GetDigest()) {
		return ErrInvalidAgentExecutionCommand
	}
	if err := validateBoundedStrings(command, p.producer.config.Limits.MaxStringBytes); err != nil {
		return ErrInvalidAgentExecutionCommand
	}
	return nil
}

func (p *AgentExecutionProducer) validateToolkitExecuteReadCommand(command *runtimev1.WorkerCommandV1) error {
	input := command.GetInputBundleRef()
	toolkit := command.GetToolkitExecuteRead()
	if command.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_TOOLKIT_EXECUTE_READ ||
		input == nil || toolkit == nil || command.GetRootExecutionId() != command.GetExecutionId() ||
		command.GetParentExecutionId() != "" || command.GetParentCallId() != "" {
		return ErrInvalidAgentExecutionCommand
	}
	values := []string{
		command.GetCommandId(), command.GetIdempotencyKey(), command.GetExecutionId(),
		command.GetTenantId(), command.GetResourceProjectId(), command.GetProjectionProjectId(),
		command.GetPrincipalRef(), command.GetResourceClass(), command.GetIsolationClass(),
		input.GetInputBundleId(), input.GetImmutableVersion(), input.GetMediaType(),
		toolkit.GetRequestEntryId(),
	}
	for _, value := range values {
		if !validAgentText(value, p.producer.config.Limits.MaxStringBytes) {
			return ErrInvalidAgentExecutionCommand
		}
	}
	if command.GetGeneration() == 0 || command.GetDispatchOrdinal() == 0 ||
		command.GetPriority() == 0 || command.GetDeadlineUnixMillis() <= 0 ||
		input.GetByteLength() == 0 || !validSHA256Digest(input.GetDigest()) {
		return ErrInvalidAgentExecutionCommand
	}
	if err := validateBoundedStrings(command, p.producer.config.Limits.MaxStringBytes); err != nil {
		return ErrInvalidAgentExecutionCommand
	}
	return nil
}

func validAgentRoutingName(value string) bool {
	return value != "" && len(value) <= maxIndexIngestRoutingNameBytes &&
		!strings.ContainsAny(value, " \r\n\x00")
}

func validAgentText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && !strings.ContainsAny(value, "\r\n\x00")
}
