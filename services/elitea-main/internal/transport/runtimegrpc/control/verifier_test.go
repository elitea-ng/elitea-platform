package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

var conformanceTestKey = []byte("ELITEA_RUNTIME_V1_TEST_ONLY_NOT_A_SECRET")

func TestConformanceVerifierAuthenticatesOpaqueBytesBeforeStrictDecode(t *testing.T) {
	verifier := newTestVerifier(t)
	raw := validRawWorkerCommand(t)
	envelope := signedEnvelope(raw)
	command, err := verifier.Verify(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	if command.GetConfigurationValidation().GetConfigurationType() != "openapi" {
		t.Fatalf("unexpected verified command: %v", command)
	}

	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{name: "duplicate top-level singular", mutate: func(raw []byte) []byte {
			raw = protowire.AppendTag(raw, 2, protowire.BytesType)
			return protowire.AppendString(raw, "other-command")
		}},
		{name: "unknown reserved tag", mutate: func(raw []byte) []byte {
			raw = protowire.AppendTag(raw, 63, protowire.VarintType)
			return protowire.AppendVarint(raw, 1)
		}},
		{name: "duplicate nested business tag", mutate: func(raw []byte) []byte {
			validation := &runtimev1.ConfigurationValidationCommandV1{}
			if err := proto.Unmarshal(findLengthDelimitedField(t, raw, 32), validation); err != nil {
				t.Fatal(err)
			}
			nested, err := proto.MarshalOptions{Deterministic: true}.Marshal(validation)
			if err != nil {
				t.Fatal(err)
			}
			nested = protowire.AppendTag(nested, 2, protowire.BytesType)
			nested = protowire.AppendString(nested, "other-type")
			return replaceLengthDelimitedField(t, raw, 32, nested)
		}},
		{name: "noncanonical field order", mutate: func(raw []byte) []byte {
			return moveFirstFieldToEnd(t, raw)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := test.mutate(append([]byte(nil), raw...))
			_, err := verifier.Verify(context.Background(), signedEnvelope(mutated))
			if !errors.Is(err, ErrMalformedWorkerCommand) {
				t.Fatalf("expected strict wire rejection, got %v", err)
			}
		})
	}
}

func TestConformanceVerifierAcceptsStrictReferenceOnlyIndexIngestCommand(t *testing.T) {
	verifier := newTestVerifier(t)
	raw := validRawIndexWorkerCommand(t)
	command, err := verifier.Verify(context.Background(), signedEnvelope(raw))
	if err != nil {
		t.Fatal(err)
	}
	if command.GetCapabilityId() != executiondomain.IndexIngestCapability || command.GetIndexIngest().GetToolParametersEntryId() != "tool-parameters" {
		t.Fatalf("unexpected verified index command: %v", command)
	}

	duplicateBinding := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(raw, duplicateBinding); err != nil {
		t.Fatal(err)
	}
	duplicateBinding.GetIndexIngest().ToolParametersEntryId = duplicateBinding.GetIndexIngest().ToolkitConfigurationEntryId
	duplicateRaw, err := proto.MarshalOptions{Deterministic: true}.Marshal(duplicateBinding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(context.Background(), signedEnvelope(duplicateRaw)); !errors.Is(err, ErrMalformedWorkerCommand) {
		t.Fatalf("expected duplicate index binding rejection, got %v", err)
	}
}

func moveFirstFieldToEnd(t *testing.T, raw []byte) []byte {
	t.Helper()
	number, wireType, tagLength := protowire.ConsumeTag(raw)
	if number <= 0 || tagLength < 0 {
		t.Fatal("malformed first command field")
	}
	valueLength := protowire.ConsumeFieldValue(number, wireType, raw[tagLength:])
	if valueLength < 0 {
		t.Fatal("malformed first command field value")
	}
	fieldLength := tagLength + valueLength
	reordered := make([]byte, 0, len(raw))
	reordered = append(reordered, raw[fieldLength:]...)
	return append(reordered, raw[:fieldLength]...)
}

func TestConformanceVerifierRejectsDigestSignatureAndParsedUnknownFields(t *testing.T) {
	verifier := newTestVerifier(t)
	raw := validRawWorkerCommand(t)

	badDigest := signedEnvelope(raw)
	badDigest.WorkerCommandDigest.Value[0] ^= 0xff
	if _, err := verifier.Verify(context.Background(), badDigest); !errors.Is(err, ErrCommandAuthentication) {
		t.Fatalf("expected digest rejection, got %v", err)
	}
	badSignature := signedEnvelope(raw)
	badSignature.Signature[0] ^= 0xff
	if _, err := verifier.Verify(context.Background(), badSignature); !errors.Is(err, ErrCommandAuthentication) {
		t.Fatalf("expected signature rejection, got %v", err)
	}
	unknownEnvelope := signedEnvelope(raw)
	unknown := protowire.AppendTag(nil, 15, protowire.VarintType)
	unknown = protowire.AppendVarint(unknown, 1)
	unknownEnvelope.ProtoReflect().SetUnknown(unknown)
	if _, err := verifier.Verify(context.Background(), unknownEnvelope); !errors.Is(err, ErrCommandAuthentication) {
		t.Fatalf("expected parsed unknown-field rejection, got %v", err)
	}
}

func newTestVerifier(t *testing.T) *ConformanceCommandVerifier {
	t.Helper()
	verifier, err := NewConformanceCommandVerifier(ConformanceVerifierConfig{
		EnvelopeSchemaRevision: "elitea.runtime.signed-worker-command.v1",
		ProtocolRevision:       "elitea.runtime.v1",
		CapabilityVersion:      "1",
		LimitsRevision:         "elitea.runtime.limits.conformance.v1",
		KeyID:                  "elitea-runtime-v1-conformance-hmac",
		HMACKey:                conformanceTestKey,
		MaxWorkerCommandBytes:  32 * 1024,
		MaxInputManifestBytes:  64 * 1024,
		MaxStringBytes:         64 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

func validRawWorkerCommand(t *testing.T) []byte {
	t.Helper()
	command := &runtimev1.WorkerCommandV1{
		ProtocolRevision:    "elitea.runtime.v1",
		CommandId:           "command-1",
		IdempotencyKey:      "outbox-1",
		CommandType:         runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_CONFIGURATION_VALIDATE,
		ExecutionId:         "execution-1",
		Generation:          1,
		DispatchOrdinal:     1,
		RootExecutionId:     "execution-1",
		TenantId:            "tenant-1",
		ResourceProjectId:   "project-1",
		ProjectionProjectId: "project-1",
		PrincipalRef:        "actor-1",
		InputBundleRef: &runtimev1.ExecutionInputBundleReferenceV1{
			InputBundleId:    "bundle-1",
			ImmutableVersion: "bundle-v1",
			Digest:           testDigest([]byte("manifest")),
			ByteLength:       128,
			MediaType:        "application/x-protobuf",
		},
		CapabilityId:       "configuration.validate.v1",
		CapabilityVersion:  "1",
		ResourceClass:      "cpu-small",
		IsolationClass:     "credential-free",
		Priority:           1,
		DeadlineUnixMillis: time.Date(2026, time.July, 16, 12, 1, 0, 0, time.UTC).UnixMilli(),
		LimitsRevision:     "elitea.runtime.limits.conformance.v1",
		CapabilityCommand: &runtimev1.WorkerCommandV1_ConfigurationValidation{
			ConfigurationValidation: &runtimev1.ConfigurationValidationCommandV1{
				ConfigurationRevisionId: "revision-1",
				ConfigurationType:       "openapi",
				CatalogRevision:         "sdk-commit",
				CatalogDigest:           testDigest([]byte("catalog")),
				SchemaId:                "openapi",
				SchemaRevision:          "schema-v1",
				SchemaDigest:            testDigest([]byte("schema")),
				SettingsEntryId:         "settings",
			},
		},
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func validRawIndexWorkerCommand(t *testing.T) []byte {
	t.Helper()
	command := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(validRawWorkerCommand(t), command); err != nil {
		t.Fatal(err)
	}
	command.CommandType = runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_INDEX_INGEST
	command.CapabilityId = executiondomain.IndexIngestCapability
	command.CapabilityCommand = &runtimev1.WorkerCommandV1_IndexIngest{
		IndexIngest: &runtimev1.IndexIngestCommandV1{
			ToolkitConfigurationEntryId: "toolkit-configuration",
			ToolParametersEntryId:       "tool-parameters",
			EmbeddingBinding: &runtimev1.IndexIngestInputBindingV1{
				EntryId:          "embedding-binding",
				ImmutableVersion: "revision-1",
				ContentDigest:    testDigest([]byte(`{}`)),
			},
			Initiator: "user",
		},
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func signedEnvelope(raw []byte) *runtimev1.SignedWorkerCommandEnvelopeV1 {
	digest := sha256.Sum256(raw)
	mac := hmac.New(sha256.New, conformanceTestKey)
	_, _ = mac.Write(raw)
	return &runtimev1.SignedWorkerCommandEnvelopeV1{
		EnvelopeSchemaRevision: "elitea.runtime.signed-worker-command.v1",
		SignatureProfile:       runtimev1.SignatureProfileV1_SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256,
		KeyId:                  "elitea-runtime-v1-conformance-hmac",
		WorkerCommandBytes:     append([]byte(nil), raw...),
		WorkerCommandDigest: &runtimev1.DigestV1{
			Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
			Value:     digest[:],
		},
		Signature: mac.Sum(nil),
	}
}

func testDigest(content []byte) *runtimev1.DigestV1 {
	digest := sha256.Sum256(content)
	return &runtimev1.DigestV1{Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256, Value: digest[:]}
}

func findLengthDelimitedField(t *testing.T, raw []byte, target protowire.Number) []byte {
	t.Helper()
	for len(raw) > 0 {
		number, wireType, tagLength := protowire.ConsumeTag(raw)
		if tagLength < 0 {
			t.Fatal("malformed test command")
		}
		value := raw[tagLength:]
		if number == target && wireType == protowire.BytesType {
			bytesValue, length := protowire.ConsumeBytes(value)
			if length < 0 {
				t.Fatal("malformed test nested command")
			}
			return bytesValue
		}
		length := protowire.ConsumeFieldValue(number, wireType, value)
		if length < 0 {
			t.Fatal("malformed test command value")
		}
		raw = value[length:]
	}
	t.Fatalf("field %d not found", target)
	return nil
}

func replaceLengthDelimitedField(t *testing.T, raw []byte, target protowire.Number, replacement []byte) []byte {
	t.Helper()
	result := make([]byte, 0, len(raw)+len(replacement))
	for len(raw) > 0 {
		number, wireType, tagLength := protowire.ConsumeTag(raw)
		if tagLength < 0 {
			t.Fatal("malformed test command")
		}
		value := raw[tagLength:]
		length := protowire.ConsumeFieldValue(number, wireType, value)
		if length < 0 {
			t.Fatal("malformed test command value")
		}
		if number == target {
			result = protowire.AppendTag(result, number, protowire.BytesType)
			result = protowire.AppendBytes(result, replacement)
		} else {
			result = append(result, raw[:tagLength+length]...)
		}
		raw = value[length:]
	}
	return result
}

// A tool run is provider work: it writes tickets and pushes commits. The
// verifier is the last place that can refuse one before a worker is authorized
// to hold the toolkit's credentials, so these are the facts it must check.
func TestConformanceVerifierAcceptsStrictReferenceOnlyToolkitCallToolCommand(t *testing.T) {
	verifier := newTestVerifier(t)
	raw := validRawToolkitCallToolWorkerCommand(t)

	command, err := verifier.Verify(context.Background(), signedEnvelope(raw))
	if err != nil {
		t.Fatal(err)
	}
	if command.GetCapabilityId() != executiondomain.ToolkitCallToolCapability ||
		command.GetToolkitCallTool().GetToolName() != "get_issue" {
		t.Fatalf("unexpected verified tool-run command: %v", command)
	}
}

func TestConformanceVerifierRejectsOneEntryServingAsSettingsAndArguments(t *testing.T) {
	// Settings are redeemed by this service and carry credentials. Arguments
	// come from the caller. One entry standing for both would let caller
	// content be claimed where the platform's own settings belong.
	verifier := newTestVerifier(t)
	command := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(validRawToolkitCallToolWorkerCommand(t), command); err != nil {
		t.Fatal(err)
	}
	command.GetToolkitCallTool().ArgumentsEntryId = command.GetToolkitCallTool().SettingsEntryId
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := verifier.Verify(context.Background(), signedEnvelope(raw)); !errors.Is(err, ErrMalformedWorkerCommand) {
		t.Fatalf("expected duplicate tool-run binding rejection, got %v", err)
	}
}

func TestConformanceVerifierRejectsToolkitCallToolWithTheWrongCommandType(t *testing.T) {
	// The capability id and the command type are two independent statements.
	// A command that disagrees with itself is not a command this kernel runs.
	verifier := newTestVerifier(t)
	command := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(validRawToolkitCallToolWorkerCommand(t), command); err != nil {
		t.Fatal(err)
	}
	command.CommandType = runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_INDEX_INGEST
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := verifier.Verify(context.Background(), signedEnvelope(raw)); !errors.Is(err, ErrCommandIncompatible) {
		t.Fatalf("expected incompatible command-type rejection, got %v", err)
	}
}

func TestConformanceVerifierRejectsToolkitCallToolMissingAName(t *testing.T) {
	verifier := newTestVerifier(t)
	for _, mutate := range []func(*runtimev1.ToolkitCallToolCommandV1){
		func(c *runtimev1.ToolkitCallToolCommandV1) { c.ToolName = "" },
		func(c *runtimev1.ToolkitCallToolCommandV1) { c.ToolkitType = "" },
		func(c *runtimev1.ToolkitCallToolCommandV1) { c.ToolkitId = "" },
		func(c *runtimev1.ToolkitCallToolCommandV1) { c.SettingsEntryId = "" },
		func(c *runtimev1.ToolkitCallToolCommandV1) { c.ArgumentsEntryId = "" },
		func(c *runtimev1.ToolkitCallToolCommandV1) { c.ToolName = "get\nissue" },
	} {
		command := &runtimev1.WorkerCommandV1{}
		if err := proto.Unmarshal(validRawToolkitCallToolWorkerCommand(t), command); err != nil {
			t.Fatal(err)
		}
		mutate(command.GetToolkitCallTool())
		raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(command)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := verifier.Verify(context.Background(), signedEnvelope(raw)); !errors.Is(err, ErrMalformedWorkerCommand) {
			t.Fatalf("expected malformed tool-run rejection, got %v", err)
		}
	}
}

func validRawToolkitCallToolWorkerCommand(t *testing.T) []byte {
	t.Helper()
	command := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(validRawWorkerCommand(t), command); err != nil {
		t.Fatal(err)
	}
	command.CommandType = runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_TOOLKIT_CALL_TOOL
	command.CapabilityId = executiondomain.ToolkitCallToolCapability
	command.CapabilityCommand = &runtimev1.WorkerCommandV1_ToolkitCallTool{
		ToolkitCallTool: &runtimev1.ToolkitCallToolCommandV1{
			ToolkitType:      "github",
			SettingsEntryId:  "toolkit-settings",
			ToolName:         "get_issue",
			ArgumentsEntryId: "tool-arguments",
			ToolkitId:        "17",
			ToolkitVersion:   "3",
		},
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
