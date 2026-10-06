package commandbus

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

type Signature struct {
	Profile runtimev1.SignatureProfileV1
	KeyID   string
	Value   []byte
}

const maxCommandSigningKeyIDBytes = 256

// CommandSigner receives the exact deterministic WorkerCommandV1 bytes and
// applies its profile-defined signature input. A production signer must return
// ED25519; the public HMAC profile is accepted only when the enclosing
// ProducerConfig explicitly opts into conformance.
type CommandSigner interface {
	SignWorkerCommand(ctx context.Context, exactCommandBytes []byte) (Signature, error)
}

// StreamAppender publishes one prepared envelope for one delivery. The
// production implementation is JetStreamAppender; it returns the stored
// message's "<stream>:<sequence>".
type StreamAppender interface {
	Append(ctx context.Context, stream, deliveryID string, value []byte) (entryID string, err error)
}

type ProducerConfig struct {
	Stream                 string
	ProtocolRevision       string
	EnvelopeSchemaRevision string
	Limits                 Limits
	// AllowTestOnlyHMAC must be set only by offline conformance composition.
	// Production leaves it false and accepts only ED25519.
	AllowTestOnlyHMAC bool
}

type Producer struct {
	config   ProducerConfig
	signer   CommandSigner
	appender StreamAppender
}

func NewProducer(config ProducerConfig, signer CommandSigner, appender StreamAppender) (*Producer, error) {
	if signer == nil || appender == nil {
		return nil, errors.New("command signer and stream appender are required")
	}
	if config.Stream == "" || len(config.Stream) > 256 || strings.ContainsAny(config.Stream, " \r\n\x00") {
		return nil, errors.New("invalid command stream")
	}
	if config.ProtocolRevision == "" || config.EnvelopeSchemaRevision == "" {
		return nil, errors.New("protocol and envelope revisions are required")
	}
	if err := config.Limits.validate(); err != nil {
		return nil, err
	}
	return &Producer{config: config, signer: signer, appender: appender}, nil
}

func (p *Producer) PrepareValidation(ctx context.Context, dispatch executionapp.ValidationDispatch) (executionapp.PreparedCommandEnvelope, error) {
	if dispatch.LimitsRevision != p.config.Limits.Revision {
		return executionapp.PreparedCommandEnvelope{}, executionapp.ErrInvalidDispatch
	}
	command, err := validationWorkerCommand(p.config.ProtocolRevision, dispatch)
	if err != nil {
		return executionapp.PreparedCommandEnvelope{}, err
	}
	return p.prepareCommand(ctx, command)
}

func (p *Producer) prepareCommand(ctx context.Context, command *runtimev1.WorkerCommandV1) (executionapp.PreparedCommandEnvelope, error) {
	if err := validateBoundedStrings(command, p.config.Limits.MaxStringBytes); err != nil {
		return executionapp.PreparedCommandEnvelope{}, err
	}

	commandBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(command)
	if err != nil {
		return executionapp.PreparedCommandEnvelope{}, fmt.Errorf("encode worker command: %w", err)
	}
	if len(commandBytes) > p.config.Limits.MaxWorkerCommandBytes {
		return executionapp.PreparedCommandEnvelope{}, ErrControlMessageLimitExceeded
	}
	commandDigest := runtimedomain.SHA256(commandBytes)

	signature, err := p.signer.SignWorkerCommand(ctx, append([]byte(nil), commandBytes...))
	if err != nil {
		return executionapp.PreparedCommandEnvelope{}, fmt.Errorf("sign worker command: %w", err)
	}
	if !p.acceptsSignature(signature.Profile, len(signature.Value)) || signature.KeyID == "" || len(signature.KeyID) > p.config.Limits.MaxStringBytes || len(signature.KeyID) > maxCommandSigningKeyIDBytes || strings.ContainsRune(signature.KeyID, '\x00') || len(signature.Value) == 0 || len(signature.Value) > p.config.Limits.MaxSignatureBytes {
		return executionapp.PreparedCommandEnvelope{}, errors.New("invalid worker command signature")
	}
	envelope := &runtimev1.SignedWorkerCommandEnvelopeV1{
		EnvelopeSchemaRevision: p.config.EnvelopeSchemaRevision,
		SignatureProfile:       signature.Profile,
		KeyId:                  signature.KeyID,
		WorkerCommandBytes:     commandBytes,
		WorkerCommandDigest:    digestProto(commandDigest),
		Signature:              append([]byte(nil), signature.Value...),
	}
	envelopeBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(envelope)
	if err != nil {
		return executionapp.PreparedCommandEnvelope{}, fmt.Errorf("encode signed worker command: %w", err)
	}
	if len(envelopeBytes) > p.config.Limits.MaxSignedEnvelopeBytes || len(envelopeBytes) > p.config.Limits.MaxTransportPayloadBytes || encodedTransportMessageBytes(envelopeBytes) > p.config.Limits.MaxTransportMessageBytes {
		return executionapp.PreparedCommandEnvelope{}, ErrControlMessageLimitExceeded
	}
	prepared := executionapp.PreparedCommandEnvelope{
		Bytes:            append([]byte(nil), envelopeBytes...),
		Digest:           runtimedomain.SHA256(envelopeBytes),
		SignatureProfile: int32(signature.Profile),
		KeyID:            signature.KeyID,
	}
	if err := p.validatePrepared(prepared); err != nil {
		return executionapp.PreparedCommandEnvelope{}, err
	}
	return prepared, nil
}

// AppendPrepared appends only a previously selected exact envelope. It never
// signs or re-encodes the command, so an unknown publish success and every
// competing publisher retry use byte-identical control-plane data.
func (p *Producer) AppendPrepared(ctx context.Context, deliveryID string, prepared executionapp.PreparedCommandEnvelope) error {
	command, err := p.preparedCommand(prepared)
	if err != nil {
		return err
	}
	if command.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_CONFIGURATION_VALIDATE || command.GetCapabilityId() != executiondomain.ConfigurationValidationCapability || command.GetConfigurationValidation() == nil {
		return executionapp.ErrInvalidPreparedEnvelope
	}
	return p.appendPreparedCommand(ctx, deliveryID, prepared, command)
}

func (p *Producer) appendPreparedCommand(ctx context.Context, deliveryID string, prepared executionapp.PreparedCommandEnvelope, command *runtimev1.WorkerCommandV1) error {
	if !validDeliveryID(deliveryID) || command == nil || command.GetIdempotencyKey() != deliveryID {
		return executionapp.ErrInvalidPreparedEnvelope
	}
	if _, err := p.appender.Append(ctx, p.config.Stream, deliveryID, append([]byte(nil), prepared.Bytes...)); err != nil {
		return fmt.Errorf("append worker command reference: %w", err)
	}
	return nil
}

func (p *Producer) preparedCommand(prepared executionapp.PreparedCommandEnvelope) (*runtimev1.WorkerCommandV1, error) {
	if err := p.validatePrepared(prepared); err != nil {
		return nil, err
	}
	envelope := &runtimev1.SignedWorkerCommandEnvelopeV1{}
	if err := proto.Unmarshal(prepared.Bytes, envelope); err != nil {
		return nil, executionapp.ErrInvalidPreparedEnvelope
	}
	commandBytes := envelope.GetWorkerCommandBytes()
	command := &runtimev1.WorkerCommandV1{}
	if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(commandBytes, command); err != nil || hasUnknownFields(command.ProtoReflect()) {
		return nil, executionapp.ErrInvalidPreparedEnvelope
	}
	canonical, err := proto.MarshalOptions{Deterministic: true}.Marshal(command)
	if err != nil || !bytes.Equal(canonical, commandBytes) {
		return nil, executionapp.ErrInvalidPreparedEnvelope
	}
	if err := validateBoundedStrings(command, p.config.Limits.MaxStringBytes); err != nil {
		return nil, executionapp.ErrInvalidPreparedEnvelope
	}
	return command, nil
}

func validDeliveryID(deliveryID string) bool {
	return deliveryID != "" && len(deliveryID) <= 256 && !strings.ContainsAny(deliveryID, "\r\n\x00")
}

func (p *Producer) validatePrepared(prepared executionapp.PreparedCommandEnvelope) error {
	if err := prepared.Validate(); err != nil {
		return err
	}
	if len(prepared.Bytes) > p.config.Limits.MaxSignedEnvelopeBytes || len(prepared.Bytes) > p.config.Limits.MaxTransportPayloadBytes || encodedTransportMessageBytes(prepared.Bytes) > p.config.Limits.MaxTransportMessageBytes {
		return ErrControlMessageLimitExceeded
	}

	envelope := &runtimev1.SignedWorkerCommandEnvelopeV1{}
	if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(prepared.Bytes, envelope); err != nil || len(envelope.ProtoReflect().GetUnknown()) != 0 {
		return executionapp.ErrInvalidPreparedEnvelope
	}
	canonical, err := proto.MarshalOptions{Deterministic: true}.Marshal(envelope)
	if err != nil || !bytes.Equal(canonical, prepared.Bytes) {
		return executionapp.ErrInvalidPreparedEnvelope
	}
	if envelope.GetEnvelopeSchemaRevision() != p.config.EnvelopeSchemaRevision || envelope.GetSignatureProfile() != runtimev1.SignatureProfileV1(prepared.SignatureProfile) || !p.acceptsSignature(envelope.GetSignatureProfile(), len(envelope.GetSignature())) || envelope.GetKeyId() != prepared.KeyID || strings.ContainsRune(envelope.GetKeyId(), '\x00') {
		return executionapp.ErrInvalidPreparedEnvelope
	}
	if len(envelope.GetKeyId()) > p.config.Limits.MaxStringBytes || len(envelope.GetKeyId()) > maxCommandSigningKeyIDBytes || len(envelope.GetSignature()) == 0 || len(envelope.GetSignature()) > p.config.Limits.MaxSignatureBytes || len(envelope.GetWorkerCommandBytes()) == 0 || len(envelope.GetWorkerCommandBytes()) > p.config.Limits.MaxWorkerCommandBytes {
		return executionapp.ErrInvalidPreparedEnvelope
	}
	commandDigest := envelope.GetWorkerCommandDigest()
	actualCommandDigest := sha256.Sum256(envelope.GetWorkerCommandBytes())
	if commandDigest == nil || len(commandDigest.ProtoReflect().GetUnknown()) != 0 || commandDigest.GetAlgorithm() != runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256 || !bytes.Equal(commandDigest.GetValue(), actualCommandDigest[:]) {
		return executionapp.ErrInvalidPreparedEnvelope
	}
	return nil
}

func (p *Producer) acceptsSignature(profile runtimev1.SignatureProfileV1, size int) bool {
	switch profile {
	case runtimev1.SignatureProfileV1_SIGNATURE_PROFILE_V1_ED25519:
		return size == ed25519.SignatureSize
	case runtimev1.SignatureProfileV1_SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256:
		return p.config.AllowTestOnlyHMAC && size == sha256.Size
	default:
		return false
	}
}
