package redisdispatch

import (
	"context"
	"errors"
	"strings"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	toolkitdiscoveryapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitdiscovery"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

var ErrInvalidToolkitAvailableToolsCommand = errors.New("invalid toolkit discovery command")

type ToolkitAvailableToolsProducerConfig struct {
	Stream                 string
	ConsumerGroup          string
	ValidationStream       string
	ProtocolRevision       string
	EnvelopeSchemaRevision string
	CapabilityVersion      string
	Limits                 Limits
	AllowTestOnlyHMAC      bool
}

type ToolkitAvailableToolsProducer struct {
	producer          *Producer
	stream            string
	consumerGroup     string
	protocolRevision  string
	capabilityVersion string
}

func NewToolkitAvailableToolsProducer(
	config ToolkitAvailableToolsProducerConfig,
	signer CommandSigner,
	appender StreamAppender,
) (*ToolkitAvailableToolsProducer, error) {
	for _, route := range []string{config.Stream, config.ConsumerGroup, config.ValidationStream} {
		if !validAgentRoutingName(route) {
			return nil, errors.New("invalid toolkit discovery Redis route")
		}
	}
	if redisRouteKeysOverlap(config.Stream, config.ValidationStream) {
		return nil, errors.New("a toolkit discovery cannot share the configuration-validation Redis stream")
	}
	if config.CapabilityVersion == "" ||
		len(config.CapabilityVersion) > config.Limits.MaxStringBytes ||
		strings.ContainsAny(config.CapabilityVersion, "\r\n\x00") {
		return nil, errors.New("invalid toolkit discovery capability version")
	}
	if config.Limits.MaxRedisEntryBytes <= 0 ||
		config.Limits.MaxRedisEntryBytes > maxAgentRedisEntryBytes {
		return nil, errors.New("toolkit discovery Redis entry limit must be less than 64 KiB")
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
	return &ToolkitAvailableToolsProducer{
		producer:          producer,
		stream:            config.Stream,
		consumerGroup:     config.ConsumerGroup,
		protocolRevision:  config.ProtocolRevision,
		capabilityVersion: config.CapabilityVersion,
	}, nil
}

func (p *ToolkitAvailableToolsProducer) Stream() string { return p.stream }

func (p *ToolkitAvailableToolsProducer) ConsumerGroup() string { return p.consumerGroup }

func (p *ToolkitAvailableToolsProducer) PrepareToolkitAvailableTools(
	ctx context.Context,
	dispatch toolkitdiscoveryapp.Dispatch,
) (executionapp.PreparedCommandEnvelope, error) {
	if dispatch.CapabilityVersion != p.capabilityVersion ||
		dispatch.LimitsRevision != p.producer.config.Limits.Revision {
		return executionapp.PreparedCommandEnvelope{}, toolkitdiscoveryapp.ErrInvalidDiscoveryDispatch
	}
	command, err := toolkitAvailableToolsWorkerCommand(p.protocolRevision, dispatch)
	if err != nil {
		return executionapp.PreparedCommandEnvelope{}, err
	}
	return p.Prepare(ctx, command)
}

func (p *ToolkitAvailableToolsProducer) Prepare(
	ctx context.Context,
	command *runtimev1.WorkerCommandV1,
) (executionapp.PreparedCommandEnvelope, error) {
	if ctx == nil || command == nil || hasUnknownFields(command.ProtoReflect()) {
		return executionapp.PreparedCommandEnvelope{}, ErrInvalidToolkitAvailableToolsCommand
	}
	if err := p.validateCommand(command); err != nil {
		return executionapp.PreparedCommandEnvelope{}, err
	}
	return p.producer.prepareCommand(ctx, command)
}

func (p *ToolkitAvailableToolsProducer) AppendPrepared(
	ctx context.Context,
	deliveryID string,
	prepared executionapp.PreparedCommandEnvelope,
) error {
	command, err := p.producer.preparedCommand(prepared)
	if err != nil {
		return err
	}
	if err := p.validateCommand(command); err != nil {
		return executionapp.ErrInvalidPreparedEnvelope
	}
	return p.producer.appendPreparedCommand(ctx, deliveryID, prepared, command)
}

func (p *ToolkitAvailableToolsProducer) validateCommand(command *runtimev1.WorkerCommandV1) error {
	if command.GetProtocolRevision() != p.protocolRevision ||
		command.GetLimitsRevision() != p.producer.config.Limits.Revision ||
		command.GetCapabilityId() != executiondomain.ToolkitAvailableToolsCapability ||
		command.GetCapabilityVersion() != p.capabilityVersion {
		return ErrInvalidToolkitAvailableToolsCommand
	}
	input := command.GetInputBundleRef()
	run := command.GetToolkitAvailableTools()
	if command.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_TOOLKIT_AVAILABLE_TOOLS ||
		input == nil || run == nil ||
		command.GetRootExecutionId() != command.GetExecutionId() ||
		command.GetParentExecutionId() != "" || command.GetParentCallId() != "" {
		return ErrInvalidToolkitAvailableToolsCommand
	}
	values := []string{
		command.GetCommandId(), command.GetIdempotencyKey(), command.GetExecutionId(),
		command.GetTenantId(), command.GetResourceProjectId(), command.GetProjectionProjectId(),
		command.GetPrincipalRef(), command.GetResourceClass(), command.GetIsolationClass(),
		input.GetInputBundleId(), input.GetImmutableVersion(), input.GetMediaType(),
		run.GetToolkitType(),
		run.GetSettingsEntryId(),
	}
	for _, value := range values {
		if !validAgentText(value, p.producer.config.Limits.MaxStringBytes) {
			return ErrInvalidToolkitAvailableToolsCommand
		}
	}
	if command.GetGeneration() == 0 || command.GetDispatchOrdinal() == 0 ||
		command.GetPriority() == 0 || command.GetDeadlineUnixMillis() <= 0 ||
		input.GetByteLength() == 0 || !validSHA256Digest(input.GetDigest()) {
		return ErrInvalidToolkitAvailableToolsCommand
	}
	if err := validateBoundedStrings(command, p.producer.config.Limits.MaxStringBytes); err != nil {
		return ErrInvalidToolkitAvailableToolsCommand
	}
	return nil
}

func toolkitAvailableToolsWorkerCommand(
	protocolRevision string,
	dispatch toolkitdiscoveryapp.Dispatch,
) (*runtimev1.WorkerCommandV1, error) {
	if protocolRevision == "" || len(protocolRevision) > 128 {
		return nil, errors.New("invalid protocol revision")
	}
	if err := dispatch.Validate(); err != nil {
		return nil, err
	}
	return &runtimev1.WorkerCommandV1{
		ProtocolRevision:    protocolRevision,
		CommandId:           dispatch.CommandID,
		IdempotencyKey:      dispatch.OutboxID,
		CommandType:         runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_TOOLKIT_AVAILABLE_TOOLS,
		ExecutionId:         dispatch.ExecutionID,
		Generation:          dispatch.Generation,
		DispatchOrdinal:     dispatch.DispatchOrdinal,
		RootExecutionId:     dispatch.ExecutionID,
		TenantId:            dispatch.TenantID,
		ResourceProjectId:   dispatch.ResourceProjectID,
		ProjectionProjectId: dispatch.ProjectionProjectID,
		PrincipalRef:        dispatch.PrincipalRef,
		InputBundleRef: &runtimev1.ExecutionInputBundleReferenceV1{
			InputBundleId:    dispatch.InputBundleID,
			ImmutableVersion: dispatch.InputBundleVersion,
			Digest:           digestProto(dispatch.InputBundleDigest),
			ByteLength:       dispatch.InputBundleByteLength,
			MediaType:        dispatch.InputBundleMediaType,
		},
		CapabilityId:       dispatch.CapabilityID,
		CapabilityVersion:  dispatch.CapabilityVersion,
		ResourceClass:      dispatch.ResourceClass,
		IsolationClass:     dispatch.IsolationClass,
		Priority:           dispatch.Priority,
		DeadlineUnixMillis: dispatch.Deadline.UTC().UnixMilli(),
		Traceparent:        dispatch.Traceparent,
		Tracestate:         dispatch.Tracestate,
		LimitsRevision:     dispatch.LimitsRevision,
		CapabilityCommand: &runtimev1.WorkerCommandV1_ToolkitAvailableTools{
			ToolkitAvailableTools: &runtimev1.ToolkitAvailableToolsCommandV1{
				ToolkitType:     dispatch.ToolkitType,
				SettingsEntryId: dispatch.SettingsEntryID,
			},
		},
	}, nil
}

var _ toolkitdiscoveryapp.ReferenceCommandProducer = (*ToolkitAvailableToolsProducer)(nil)
