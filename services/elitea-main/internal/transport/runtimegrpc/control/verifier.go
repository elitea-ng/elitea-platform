package control

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimegrpc "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

var (
	ErrCommandAuthentication  = errors.New("worker command authentication failed")
	ErrCommandIncompatible    = errors.New("worker command protocol is incompatible")
	ErrMalformedWorkerCommand = errors.New("worker command is malformed")
)

type CommandVerifier interface {
	Verify(ctx context.Context, envelope *runtimev1.SignedWorkerCommandEnvelopeV1) (*runtimev1.WorkerCommandV1, error)
}

type ConformanceVerifierConfig struct {
	EnvelopeSchemaRevision string
	ProtocolRevision       string
	CapabilityVersion      string
	LimitsRevision         string
	KeyID                  string
	HMACKey                []byte
	MaxWorkerCommandBytes  int
	MaxInputManifestBytes  uint64
	MaxStringBytes         int
}

// ConformanceCommandVerifier accepts only the public test-vector HMAC profile.
// It is intentionally impossible to construct as a production verifier.
type ConformanceCommandVerifier struct {
	config ConformanceVerifierConfig
}

func NewConformanceCommandVerifier(config ConformanceVerifierConfig) (*ConformanceCommandVerifier, error) {
	if config.EnvelopeSchemaRevision == "" || config.ProtocolRevision == "" || config.CapabilityVersion == "" || config.LimitsRevision == "" || config.KeyID == "" {
		return nil, errors.New("all conformance command revisions and key ID are required")
	}
	if len(config.HMACKey) < sha256.Size || config.MaxWorkerCommandBytes <= 0 || config.MaxInputManifestBytes == 0 || config.MaxStringBytes <= 0 {
		return nil, errors.New("invalid conformance verifier limits or key")
	}
	config.HMACKey = append([]byte(nil), config.HMACKey...)
	return &ConformanceCommandVerifier{config: config}, nil
}

func (v *ConformanceCommandVerifier) Verify(ctx context.Context, envelope *runtimev1.SignedWorkerCommandEnvelopeV1) (*runtimev1.WorkerCommandV1, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if envelope == nil || hasUnknownFields(envelope.ProtoReflect()) {
		return nil, ErrCommandAuthentication
	}
	if envelope.GetEnvelopeSchemaRevision() != v.config.EnvelopeSchemaRevision ||
		envelope.GetSignatureProfile() != runtimev1.SignatureProfileV1_SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256 ||
		envelope.GetKeyId() != v.config.KeyID {
		return nil, ErrCommandAuthentication
	}
	commandBytes := envelope.GetWorkerCommandBytes()
	if len(commandBytes) == 0 || len(commandBytes) > v.config.MaxWorkerCommandBytes {
		return nil, ErrMalformedWorkerCommand
	}
	if !validDigestProto(envelope.GetWorkerCommandDigest(), commandBytes) {
		return nil, ErrCommandAuthentication
	}
	expected := hmac.New(sha256.New, v.config.HMACKey)
	_, _ = expected.Write(commandBytes)
	if len(envelope.GetSignature()) != sha256.Size || !hmac.Equal(expected.Sum(nil), envelope.GetSignature()) {
		return nil, ErrCommandAuthentication
	}

	return decodeAndValidateCommand(commandBytes, commandValidationConfig{
		ProtocolRevision:      v.config.ProtocolRevision,
		CapabilityVersion:     v.config.CapabilityVersion,
		LimitsRevision:        v.config.LimitsRevision,
		MaxInputManifestBytes: v.config.MaxInputManifestBytes,
		MaxStringBytes:        v.config.MaxStringBytes,
	})
}

type commandValidationConfig struct {
	ProtocolRevision      string
	CapabilityVersion     string
	CapabilityVersions    map[string]string
	LimitsRevision        string
	MaxInputManifestBytes uint64
	MaxStringBytes        int
}

func decodeAndValidateCommand(commandBytes []byte, config commandValidationConfig) (*runtimev1.WorkerCommandV1, error) {
	descriptor := (&runtimev1.WorkerCommandV1{}).ProtoReflect().Descriptor()
	if err := runtimegrpc.ScanStrictMessage(commandBytes, descriptor); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedWorkerCommand, err)
	}
	command := &runtimev1.WorkerCommandV1{}
	if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(commandBytes, command); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedWorkerCommand, err)
	}
	if hasUnknownFields(command.ProtoReflect()) {
		return nil, ErrMalformedWorkerCommand
	}
	canonicalCommandBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(command)
	if err != nil || !bytes.Equal(canonicalCommandBytes, commandBytes) {
		return nil, ErrMalformedWorkerCommand
	}
	if err := validateCommand(command, config); err != nil {
		return nil, err
	}
	return command, nil
}

func validateCommand(command *runtimev1.WorkerCommandV1, config commandValidationConfig) error {
	if command.GetProtocolRevision() != config.ProtocolRevision || command.GetLimitsRevision() != config.LimitsRevision {
		return ErrCommandIncompatible
	}
	expectedCapabilityVersion := config.CapabilityVersion
	if len(config.CapabilityVersions) != 0 {
		var ok bool
		expectedCapabilityVersion, ok = config.CapabilityVersions[command.GetCapabilityId()]
		if !ok {
			return ErrCommandIncompatible
		}
	}
	if expectedCapabilityVersion == "" ||
		command.GetCapabilityVersion() != expectedCapabilityVersion {
		return ErrCommandIncompatible
	}
	input := command.GetInputBundleRef()
	if input == nil {
		return ErrMalformedWorkerCommand
	}
	values := []string{
		command.GetCommandId(), command.GetIdempotencyKey(), command.GetExecutionId(), command.GetRootExecutionId(),
		command.GetTenantId(), command.GetResourceProjectId(), command.GetProjectionProjectId(), command.GetPrincipalRef(),
		command.GetCapabilityId(), command.GetCapabilityVersion(), command.GetResourceClass(),
		command.GetIsolationClass(), input.GetInputBundleId(), input.GetImmutableVersion(), input.GetMediaType(),
	}
	for _, value := range values {
		if value == "" || len(value) > config.MaxStringBytes {
			return ErrMalformedWorkerCommand
		}
	}
	if command.GetGeneration() == 0 || command.GetDispatchOrdinal() == 0 || command.GetPriority() == 0 || command.GetDeadlineUnixMillis() <= 0 {
		return ErrMalformedWorkerCommand
	}
	if input.GetByteLength() == 0 || input.GetByteLength() > config.MaxInputManifestBytes {
		return ErrMalformedWorkerCommand
	}
	if !validDigestMessage(input.GetDigest()) {
		return ErrMalformedWorkerCommand
	}

	switch command.GetCapabilityId() {
	case executiondomain.ConfigurationValidationCapability:
		return validateConfigurationCommand(command, config)
	case executiondomain.IndexIngestCapability:
		return validateIndexIngestCommand(command, config)
	case executiondomain.AgentApplicationCapability, executiondomain.AgentAdhocCapability:
		return validateAgentExecutionCommand(command, config)
	case executiondomain.ToolkitExecuteReadCapability:
		return validateToolkitExecuteReadCommand(command, config)
	case executiondomain.ToolkitCallToolCapability:
		return validateToolkitCallToolCommand(command, config)
	default:
		return ErrCommandIncompatible
	}
}

func validateToolkitExecuteReadCommand(command *runtimev1.WorkerCommandV1, config commandValidationConfig) error {
	toolkit := command.GetToolkitExecuteRead()
	if command.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_TOOLKIT_EXECUTE_READ ||
		toolkit == nil || command.GetRootExecutionId() != command.GetExecutionId() ||
		command.GetParentExecutionId() != "" || command.GetParentCallId() != "" ||
		toolkit.GetRequestEntryId() == "" || len(toolkit.GetRequestEntryId()) > config.MaxStringBytes ||
		strings.ContainsAny(toolkit.GetRequestEntryId(), "\x00\r\n") {
		return ErrMalformedWorkerCommand
	}
	return nil
}

// validateToolkitCallToolCommand pins the one tool run this command may name.
//
// The two entry ids must DIFFER. Settings are redeemed by this service from the
// saved toolkit row and carry credentials; arguments come from the caller. One
// entry standing for both would let caller content be claimed where the
// platform's own authorized settings belong.
func validateToolkitCallToolCommand(command *runtimev1.WorkerCommandV1, config commandValidationConfig) error {
	if command.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_TOOLKIT_CALL_TOOL {
		return ErrCommandIncompatible
	}
	call := command.GetToolkitCallTool()
	if call == nil ||
		command.GetRootExecutionId() != command.GetExecutionId() ||
		command.GetParentExecutionId() != "" || command.GetParentCallId() != "" {
		return ErrMalformedWorkerCommand
	}
	for _, value := range []string{
		call.GetToolkitType(), call.GetToolName(),
		call.GetSettingsEntryId(), call.GetArgumentsEntryId(), call.GetToolkitId(),
	} {
		if value == "" || len(value) > config.MaxStringBytes || strings.ContainsAny(value, "\x00\r\n") {
			return ErrMalformedWorkerCommand
		}
	}
	if len(call.GetToolkitVersion()) > config.MaxStringBytes ||
		strings.ContainsAny(call.GetToolkitVersion(), "\x00\r\n") {
		return ErrMalformedWorkerCommand
	}
	if call.GetSettingsEntryId() == call.GetArgumentsEntryId() {
		return ErrMalformedWorkerCommand
	}
	return nil
}

func validateAgentExecutionCommand(command *runtimev1.WorkerCommandV1, config commandValidationConfig) error {
	expectedType := runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION
	if command.GetCapabilityId() == executiondomain.AgentAdhocCapability {
		expectedType = runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_ADHOC
	}
	agent := command.GetAgentExecution()
	if command.GetCommandType() != expectedType || agent == nil ||
		command.GetRootExecutionId() != command.GetExecutionId() ||
		command.GetParentExecutionId() != "" || command.GetParentCallId() != "" {
		return ErrMalformedWorkerCommand
	}
	for _, value := range []string{
		agent.GetRequestEntryId(), agent.GetClientStreamId(),
		agent.GetClientMessageId(), agent.GetSioEvent(),
	} {
		if value == "" || len(value) > config.MaxStringBytes || strings.ContainsAny(value, "\x00\r\n") {
			return ErrMalformedWorkerCommand
		}
	}
	if agent.GetSioEvent() != "chat_predict" && agent.GetSioEvent() != "chat_continue_predict" {
		return ErrMalformedWorkerCommand
	}
	return nil
}

func validateConfigurationCommand(command *runtimev1.WorkerCommandV1, config commandValidationConfig) error {
	if command.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_CONFIGURATION_VALIDATE {
		return ErrCommandIncompatible
	}
	validation := command.GetConfigurationValidation()
	if validation == nil {
		return ErrMalformedWorkerCommand
	}
	values := []string{
		validation.GetConfigurationRevisionId(), validation.GetConfigurationType(), validation.GetCatalogRevision(),
		validation.GetSchemaId(), validation.GetSchemaRevision(), validation.GetSettingsEntryId(),
	}
	for _, value := range values {
		if value == "" || len(value) > config.MaxStringBytes {
			return ErrMalformedWorkerCommand
		}
	}
	if !validDigestMessage(validation.GetCatalogDigest()) || !validDigestMessage(validation.GetSchemaDigest()) {
		return ErrMalformedWorkerCommand
	}
	return nil
}

func validateIndexIngestCommand(command *runtimev1.WorkerCommandV1, config commandValidationConfig) error {
	if command.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_INDEX_INGEST {
		return ErrCommandIncompatible
	}
	indexing := command.GetIndexIngest()
	if indexing == nil || indexing.GetToolkitConfigurationEntryId() == "" || indexing.GetToolParametersEntryId() == "" {
		return ErrMalformedWorkerCommand
	}
	entryIDs := []string{
		indexing.GetToolkitConfigurationEntryId(), indexing.GetToolParametersEntryId(), indexing.GetLlmModelEntryId(),
		indexing.GetLlmConfigurationEntryId(), indexing.GetMcpTokensEntryId(),
	}
	seen := make(map[string]struct{}, len(entryIDs))
	for _, entryID := range entryIDs {
		if entryID == "" {
			continue
		}
		if len(entryID) > config.MaxStringBytes {
			return ErrMalformedWorkerCommand
		}
		if _, duplicate := seen[entryID]; duplicate {
			return ErrMalformedWorkerCommand
		}
		seen[entryID] = struct{}{}
	}
	for _, value := range []string{indexing.GetClientStreamId(), indexing.GetClientMessageId()} {
		if value != "" && (len(value) > 512 || len(value) > config.MaxStringBytes || strings.ContainsAny(value, "\x00\r\n")) {
			return ErrMalformedWorkerCommand
		}
	}
	switch indexing.GetSioEvent() {
	case "", "chat_predict", "test_toolkit_tool":
	default:
		return ErrMalformedWorkerCommand
	}
	switch indexing.GetInitiator() {
	case "user", "llm", "schedule":
	default:
		return ErrMalformedWorkerCommand
	}
	return nil
}

func validDigestProto(digest *runtimev1.DigestV1, content []byte) bool {
	if !validDigestMessage(digest) {
		return false
	}
	actual := sha256.Sum256(content)
	return hmac.Equal(actual[:], digest.GetValue())
}

func validDigestMessage(digest *runtimev1.DigestV1) bool {
	return digest != nil && digest.GetAlgorithm() == runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256 && len(digest.GetValue()) == sha256.Size
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
			for i := 0; i < list.Len(); i++ {
				if hasUnknownFields(list.Get(i).Message()) {
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
