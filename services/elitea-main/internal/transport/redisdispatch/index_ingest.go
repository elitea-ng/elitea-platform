package redisdispatch

import (
	"context"
	"errors"
	"strings"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	indexIngestCapabilityID        = "index.ingest.v1"
	maxIndexIngestRedisEntryBytes  = (64 << 10) - 1
	maxIndexIngestRoutingNameBytes = 256
)

var ErrInvalidIndexIngestCommand = errors.New("invalid index ingest command")

// IndexIngestProducerConfig binds index ingestion to a stream that is distinct
// from the configuration-validation stream. ConsumerGroup is retained as part
// of the same route contract so producer and worker composition cannot select
// the stream and group independently.
type IndexIngestProducerConfig struct {
	Stream                 string
	ConsumerGroup          string
	ValidationStream       string
	ProtocolRevision       string
	EnvelopeSchemaRevision string
	CapabilityVersion      string
	Limits                 Limits
	// AllowTestOnlyHMAC must be set only by offline conformance composition.
	AllowTestOnlyHMAC bool
}

// IndexIngestProducer prepares and publishes only index.ingest.v1 commands.
// It has no in-memory retry queue: saturation or Redis failure is returned
// without mutation, so its caller can retain the immutable prepared envelope
// in the durable outbox.
type IndexIngestProducer struct {
	producer          *Producer
	stream            string
	consumerGroup     string
	protocolRevision  string
	capabilityVersion string
}

func NewIndexIngestProducer(config IndexIngestProducerConfig, signer CommandSigner, appender StreamAppender) (*IndexIngestProducer, error) {
	if !validIndexIngestRoutingName(config.Stream) || !validIndexIngestRoutingName(config.ConsumerGroup) || !validIndexIngestRoutingName(config.ValidationStream) {
		return nil, errors.New("invalid index ingest Redis route")
	}
	if redisRouteKeysOverlap(config.Stream, config.ValidationStream) {
		return nil, errors.New("index ingest requires a dedicated Redis stream and delivery index")
	}
	if config.CapabilityVersion == "" || len(config.CapabilityVersion) > config.Limits.MaxStringBytes || strings.ContainsAny(config.CapabilityVersion, "\r\n\x00") {
		return nil, errors.New("invalid index ingest capability version")
	}
	if config.Limits.MaxTransportMessageBytes <= 0 || config.Limits.MaxTransportMessageBytes > maxIndexIngestRedisEntryBytes {
		return nil, errors.New("index ingest Redis entry limit must be less than 64 KiB")
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
	return &IndexIngestProducer{
		producer:          producer,
		stream:            config.Stream,
		consumerGroup:     config.ConsumerGroup,
		protocolRevision:  config.ProtocolRevision,
		capabilityVersion: config.CapabilityVersion,
	}, nil
}

func (p *IndexIngestProducer) Stream() string {
	return p.stream
}

func (p *IndexIngestProducer) ConsumerGroup() string {
	return p.consumerGroup
}

// PrepareIndexIngest builds the wire command from the application-owned typed
// reference contract before applying the same strict protocol validation used
// for decoded commands.
func (p *IndexIngestProducer) PrepareIndexIngest(ctx context.Context, dispatch indexingapp.IndexIngestDispatch) (executionapp.PreparedCommandEnvelope, error) {
	if dispatch.LimitsRevision != p.producer.config.Limits.Revision || dispatch.CapabilityVersion != p.capabilityVersion {
		return executionapp.PreparedCommandEnvelope{}, indexingapp.ErrInvalidIndexIngestDispatch
	}
	command, err := indexIngestWorkerCommand(p.protocolRevision, dispatch)
	if err != nil {
		return executionapp.PreparedCommandEnvelope{}, err
	}
	return p.Prepare(ctx, command)
}

// Prepare accepts the typed reference-only protocol command. Source files,
// toolkit/LLM configuration values, credentials and results have no field in
// this message and remain behind immutable input or artifact references.
func (p *IndexIngestProducer) Prepare(ctx context.Context, command *runtimev1.WorkerCommandV1) (executionapp.PreparedCommandEnvelope, error) {
	if ctx == nil || command == nil || hasUnknownFields(command.ProtoReflect()) {
		return executionapp.PreparedCommandEnvelope{}, ErrInvalidIndexIngestCommand
	}
	if err := p.validateCommand(command); err != nil {
		return executionapp.PreparedCommandEnvelope{}, err
	}
	return p.producer.prepareCommand(ctx, command)
}

func (p *IndexIngestProducer) AppendPrepared(ctx context.Context, deliveryID string, prepared executionapp.PreparedCommandEnvelope) error {
	command, err := p.producer.preparedCommand(prepared)
	if err != nil {
		return err
	}
	if err := p.validateCommand(command); err != nil {
		return executionapp.ErrInvalidPreparedEnvelope
	}
	return p.producer.appendPreparedCommand(ctx, deliveryID, prepared, command)
}

func (p *IndexIngestProducer) validateCommand(command *runtimev1.WorkerCommandV1) error {
	if command.GetProtocolRevision() != p.protocolRevision || command.GetLimitsRevision() != p.producer.config.Limits.Revision {
		return ErrInvalidIndexIngestCommand
	}
	if command.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_INDEX_INGEST || command.GetCapabilityId() != indexIngestCapabilityID || command.GetCapabilityVersion() != p.capabilityVersion {
		return ErrInvalidIndexIngestCommand
	}
	input := command.GetInputBundleRef()
	index := command.GetIndexIngest()
	if input == nil || index == nil {
		return ErrInvalidIndexIngestCommand
	}
	values := []string{
		command.GetCommandId(), command.GetIdempotencyKey(), command.GetExecutionId(), command.GetRootExecutionId(),
		command.GetTenantId(), command.GetResourceProjectId(), command.GetProjectionProjectId(), command.GetPrincipalRef(),
		command.GetResourceClass(), command.GetIsolationClass(), input.GetInputBundleId(), input.GetImmutableVersion(),
		input.GetMediaType(), index.GetToolkitConfigurationEntryId(), index.GetToolParametersEntryId(),
	}
	for _, value := range values {
		if !validIndexIngestText(value, p.producer.config.Limits.MaxStringBytes) {
			return ErrInvalidIndexIngestCommand
		}
	}
	optional := []string{index.GetLlmModelEntryId(), index.GetLlmConfigurationEntryId(), index.GetMcpTokensEntryId()}
	for _, value := range optional {
		if value != "" && !validIndexIngestText(value, p.producer.config.Limits.MaxStringBytes) {
			return ErrInvalidIndexIngestCommand
		}
	}
	for _, value := range []string{index.GetClientStreamId(), index.GetClientMessageId()} {
		if value != "" && (len(value) > 512 || !validIndexIngestText(value, p.producer.config.Limits.MaxStringBytes)) {
			return ErrInvalidIndexIngestCommand
		}
	}
	if !validIndexIngestSIOEvent(index.GetSioEvent()) {
		return ErrInvalidIndexIngestCommand
	}
	switch index.GetInitiator() {
	case "user", "llm", "schedule":
	default:
		return ErrInvalidIndexIngestCommand
	}
	entryIDs := []string{
		index.GetToolkitConfigurationEntryId(), index.GetToolParametersEntryId(), index.GetLlmModelEntryId(),
		index.GetLlmConfigurationEntryId(), index.GetMcpTokensEntryId(),
	}
	for position, entryID := range entryIDs {
		if entryID == "" {
			continue
		}
		for previous := 0; previous < position; previous++ {
			if entryID == entryIDs[previous] {
				return ErrInvalidIndexIngestCommand
			}
		}
	}
	if command.GetGeneration() == 0 || command.GetDispatchOrdinal() == 0 || command.GetPriority() == 0 || command.GetDeadlineUnixMillis() <= 0 || input.GetByteLength() == 0 || !validSHA256Digest(input.GetDigest()) {
		return ErrInvalidIndexIngestCommand
	}
	if err := validateBoundedStrings(command, p.producer.config.Limits.MaxStringBytes); err != nil {
		return ErrInvalidIndexIngestCommand
	}
	return nil
}

func validIndexIngestSIOEvent(value string) bool {
	return value == "" || value == "chat_predict" || value == "test_toolkit_tool"
}

func validIndexIngestRoutingName(value string) bool {
	return value != "" && len(value) <= maxIndexIngestRoutingNameBytes && !strings.ContainsAny(value, " \r\n\x00")
}

func redisRouteKeysOverlap(firstStream, secondStream string) bool {
	return firstStream == secondStream ||
		firstStream == deliveryIndexKey(secondStream) ||
		deliveryIndexKey(firstStream) == secondStream
}

func validIndexIngestText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && !strings.ContainsAny(value, "\r\n\x00")
}

func validSHA256Digest(digest *runtimev1.DigestV1) bool {
	return digest != nil && digest.GetAlgorithm() == runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256 && len(digest.GetValue()) == 32 && !hasUnknownFields(digest.ProtoReflect())
}

func hasUnknownFields(message protoreflect.Message) bool {
	if len(message.GetUnknown()) != 0 {
		return true
	}
	unknown := false
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Kind() != protoreflect.MessageKind {
			return true
		}
		if field.IsList() {
			list := value.List()
			for index := 0; index < list.Len(); index++ {
				if hasUnknownFields(list.Get(index).Message()) {
					unknown = true
					return false
				}
			}
			return !unknown
		}
		unknown = hasUnknownFields(value.Message())
		return !unknown
	})
	return unknown
}
