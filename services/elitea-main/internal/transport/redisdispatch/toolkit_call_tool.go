package redisdispatch

import (
	"context"
	"errors"
	"strings"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

var ErrInvalidToolkitCallToolCommand = errors.New("invalid tool-run command")

// ToolkitCallToolProducerConfig binds one tool-run capability to one bounded
// control stream. The command carries entry REFERENCES only: the redeemed
// toolkit settings and the caller's arguments are input-bundle entries and
// never reach Redis.
type ToolkitCallToolProducerConfig struct {
	Stream                 string
	ConsumerGroup          string
	ValidationStream       string
	ProtocolRevision       string
	EnvelopeSchemaRevision string
	CapabilityVersion      string
	Limits                 Limits
	AllowTestOnlyHMAC      bool
}

type ToolkitCallToolProducer struct {
	producer          *Producer
	stream            string
	consumerGroup     string
	protocolRevision  string
	capabilityVersion string
}

func NewToolkitCallToolProducer(
	config ToolkitCallToolProducerConfig,
	signer CommandSigner,
	appender StreamAppender,
) (*ToolkitCallToolProducer, error) {
	for _, route := range []string{config.Stream, config.ConsumerGroup, config.ValidationStream} {
		if !validAgentRoutingName(route) {
			return nil, errors.New("invalid tool-run Redis route")
		}
	}
	if redisRouteKeysOverlap(config.Stream, config.ValidationStream) {
		return nil, errors.New("a tool run cannot share the configuration-validation Redis stream")
	}
	if config.CapabilityVersion == "" ||
		len(config.CapabilityVersion) > config.Limits.MaxStringBytes ||
		strings.ContainsAny(config.CapabilityVersion, "\r\n\x00") {
		return nil, errors.New("invalid tool-run capability version")
	}
	if config.Limits.MaxRedisEntryBytes <= 0 ||
		config.Limits.MaxRedisEntryBytes > maxAgentRedisEntryBytes {
		return nil, errors.New("tool-run Redis entry limit must be less than 64 KiB")
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
	return &ToolkitCallToolProducer{
		producer:          producer,
		stream:            config.Stream,
		consumerGroup:     config.ConsumerGroup,
		protocolRevision:  config.ProtocolRevision,
		capabilityVersion: config.CapabilityVersion,
	}, nil
}

func (p *ToolkitCallToolProducer) Stream() string { return p.stream }

func (p *ToolkitCallToolProducer) ConsumerGroup() string { return p.consumerGroup }

func (p *ToolkitCallToolProducer) PrepareToolkitCallTool(
	ctx context.Context,
	dispatch toolkitcalltoolapp.Dispatch,
) (executionapp.PreparedCommandEnvelope, error) {
	if dispatch.CapabilityVersion != p.capabilityVersion ||
		dispatch.LimitsRevision != p.producer.config.Limits.Revision {
		return executionapp.PreparedCommandEnvelope{}, toolkitcalltoolapp.ErrInvalidToolRunDispatch
	}
	command, err := toolkitCallToolWorkerCommand(p.protocolRevision, dispatch)
	if err != nil {
		return executionapp.PreparedCommandEnvelope{}, err
	}
	return p.Prepare(ctx, command)
}

func (p *ToolkitCallToolProducer) Prepare(
	ctx context.Context,
	command *runtimev1.WorkerCommandV1,
) (executionapp.PreparedCommandEnvelope, error) {
	if ctx == nil || command == nil || hasUnknownFields(command.ProtoReflect()) {
		return executionapp.PreparedCommandEnvelope{}, ErrInvalidToolkitCallToolCommand
	}
	if err := p.validateCommand(command); err != nil {
		return executionapp.PreparedCommandEnvelope{}, err
	}
	return p.producer.prepareCommand(ctx, command)
}

func (p *ToolkitCallToolProducer) AppendPrepared(
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

func (p *ToolkitCallToolProducer) validateCommand(command *runtimev1.WorkerCommandV1) error {
	if command.GetProtocolRevision() != p.protocolRevision ||
		command.GetLimitsRevision() != p.producer.config.Limits.Revision ||
		command.GetCapabilityId() != executiondomain.ToolkitCallToolCapability ||
		command.GetCapabilityVersion() != p.capabilityVersion {
		return ErrInvalidToolkitCallToolCommand
	}
	input := command.GetInputBundleRef()
	run := command.GetToolkitCallTool()
	if command.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_TOOLKIT_CALL_TOOL ||
		input == nil || run == nil ||
		command.GetRootExecutionId() != command.GetExecutionId() ||
		command.GetParentExecutionId() != "" || command.GetParentCallId() != "" {
		return ErrInvalidToolkitCallToolCommand
	}
	values := []string{
		command.GetCommandId(), command.GetIdempotencyKey(), command.GetExecutionId(),
		command.GetTenantId(), command.GetResourceProjectId(), command.GetProjectionProjectId(),
		command.GetPrincipalRef(), command.GetResourceClass(), command.GetIsolationClass(),
		input.GetInputBundleId(), input.GetImmutableVersion(), input.GetMediaType(),
		run.GetToolkitType(), run.GetToolName(), run.GetToolkitId(),
		run.GetSettingsEntryId(), run.GetArgumentsEntryId(),
	}
	for _, value := range values {
		if !validAgentText(value, p.producer.config.Limits.MaxStringBytes) {
			return ErrInvalidToolkitCallToolCommand
		}
	}
	if version := run.GetToolkitVersion(); version != "" &&
		!validAgentText(version, p.producer.config.Limits.MaxStringBytes) {
		return ErrInvalidToolkitCallToolCommand
	}
	// The two roles must be two entries. One entry serving as both would put
	// caller-supplied arguments where the platform's own redeemed settings
	// belong, which is the single thing this command's shape exists to prevent.
	if run.GetSettingsEntryId() == run.GetArgumentsEntryId() {
		return ErrInvalidToolkitCallToolCommand
	}
	if command.GetGeneration() == 0 || command.GetDispatchOrdinal() == 0 ||
		command.GetPriority() == 0 || command.GetDeadlineUnixMillis() <= 0 ||
		input.GetByteLength() == 0 || !validSHA256Digest(input.GetDigest()) {
		return ErrInvalidToolkitCallToolCommand
	}
	if err := validateBoundedStrings(command, p.producer.config.Limits.MaxStringBytes); err != nil {
		return ErrInvalidToolkitCallToolCommand
	}
	return nil
}

func toolkitCallToolWorkerCommand(
	protocolRevision string,
	dispatch toolkitcalltoolapp.Dispatch,
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
		CommandType:         runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_TOOLKIT_CALL_TOOL,
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
		CapabilityCommand: &runtimev1.WorkerCommandV1_ToolkitCallTool{
			ToolkitCallTool: &runtimev1.ToolkitCallToolCommandV1{
				ToolkitType:      dispatch.ToolkitType,
				SettingsEntryId:  dispatch.SettingsEntryID,
				ToolName:         dispatch.ToolName,
				ArgumentsEntryId: dispatch.ArgumentsEntryID,
				ToolkitId:        dispatch.ToolkitID,
				ToolkitVersion:   dispatch.ToolkitVersion,
			},
		},
	}, nil
}

var _ toolkitcalltoolapp.ReferenceCommandProducer = (*ToolkitCallToolProducer)(nil)
