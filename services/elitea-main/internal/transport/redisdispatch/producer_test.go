package redisdispatch

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	configurationdomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/configurations"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

type signerStub struct {
	exact []byte
	calls int
}

func (s *signerStub) SignWorkerCommand(_ context.Context, exact []byte) (Signature, error) {
	s.calls++
	s.exact = append([]byte(nil), exact...)
	return Signature{
		Profile: runtimev1.SignatureProfileV1_SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256,
		KeyID:   "conformance-key-v1",
		Value:   bytes.Repeat([]byte{0x5a}, 32),
	}, nil
}

type appenderStub struct {
	stream     string
	field      string
	deliveryID string
	value      []byte
	calls      int
}

func (a *appenderStub) Append(_ context.Context, stream, field, deliveryID string, value []byte) (string, error) {
	a.calls++
	a.stream = stream
	a.field = field
	a.deliveryID = deliveryID
	a.value = append([]byte(nil), value...)
	return "1-0", nil
}

func TestProducerEmitsBoundedReferenceOnlyGeneratedContract(t *testing.T) {
	signer := &signerStub{}
	appender := &appenderStub{}
	producer, err := NewProducer(validProducerConfig(), signer, appender)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := validTransportDispatch()
	prepared, err := producer.PrepareValidation(context.Background(), dispatch)
	if err != nil {
		t.Fatal(err)
	}
	if appender.calls != 0 {
		t.Fatal("preparation reached Redis before durable envelope selection")
	}
	if err := producer.AppendPrepared(context.Background(), dispatch.OutboxID, prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.Digest != runtimedomain.SHA256(appender.value) || appender.stream != "commands.v1.configuration.validate.cpu-small.credential-free.1.0" || appender.field != redisEnvelopeField || appender.deliveryID != dispatch.OutboxID {
		t.Fatalf("unexpected append: stream=%q field=%q digest=%s", appender.stream, appender.field, prepared.Digest)
	}

	var envelope runtimev1.SignedWorkerCommandEnvelopeV1
	if err := proto.Unmarshal(appender.value, &envelope); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(envelope.GetWorkerCommandBytes(), signer.exact) {
		t.Fatal("signer did not receive the exact emitted command bytes")
	}
	expectedCommandDigest := runtimedomain.SHA256(signer.exact)
	if !bytes.Equal(envelope.GetWorkerCommandDigest().GetValue(), expectedCommandDigest[:]) {
		t.Fatal("signed envelope command digest mismatch")
	}

	var command runtimev1.WorkerCommandV1
	if err := proto.Unmarshal(envelope.GetWorkerCommandBytes(), &command); err != nil {
		t.Fatal(err)
	}
	validation := command.GetConfigurationValidation()
	if validation == nil || validation.GetConfigurationRevisionId() != dispatch.Command.ConfigurationRevisionID || command.GetInputBundleRef().GetInputBundleId() != dispatch.InputBundleID {
		t.Fatalf("unexpected worker command: %v", &command)
	}
	if command.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_CONFIGURATION_VALIDATE || command.GetIdempotencyKey() != dispatch.OutboxID {
		t.Fatalf("wrong command identity/type: %v", &command)
	}
	for _, forbidden := range [][]byte{[]byte(`{"auth_type":"Digest"}`), []byte("normalized_settings"), []byte("client_secret"), []byte("result")} {
		if bytes.Contains(envelope.GetWorkerCommandBytes(), forbidden) || bytes.Contains(appender.value, forbidden) {
			t.Fatalf("Redis control entry contains forbidden body material %q", forbidden)
		}
	}
}

func TestProducerRejectsCompleteRedisEntryAboveBoundBeforeAppend(t *testing.T) {
	baselineSigner := &signerStub{}
	baselineAppender := &appenderStub{}
	baseline, err := NewProducer(validProducerConfig(), baselineSigner, baselineAppender)
	if err != nil {
		t.Fatal(err)
	}
	baselinePrepared, err := baseline.PrepareValidation(context.Background(), validTransportDispatch())
	if err != nil {
		t.Fatal(err)
	}
	if err := baseline.AppendPrepared(context.Background(), validTransportDispatch().OutboxID, baselinePrepared); err != nil {
		t.Fatal(err)
	}

	config := validProducerConfig()
	config.Limits.MaxWorkerCommandBytes = len(baselineSigner.exact)
	config.Limits.MaxSignedEnvelopeBytes = len(baselineAppender.value)
	config.Limits.MaxRedisFieldBytes = len(baselineAppender.value)
	config.Limits.MaxRedisEntryBytes = encodedRedisEntryBytes(redisEnvelopeField, baselineAppender.value) - 1
	appender := &appenderStub{}
	producer, err := NewProducer(config, &signerStub{}, appender)
	if err != nil {
		t.Fatal(err)
	}
	_, err = producer.PrepareValidation(context.Background(), validTransportDispatch())
	if !errors.Is(err, ErrControlMessageLimitExceeded) {
		t.Fatalf("expected complete-entry limit rejection, got %v", err)
	}
	if appender.calls != 0 {
		t.Fatal("oversized control entry reached Redis")
	}
}

func TestProducerRejectsStringBoundBeforeSigning(t *testing.T) {
	config := validProducerConfig()
	config.Limits.MaxStringBytes = 8
	signer := &signerStub{}
	appender := &appenderStub{}
	producer, err := NewProducer(config, signer, appender)
	if err != nil {
		t.Fatal(err)
	}
	_, err = producer.PrepareValidation(context.Background(), validTransportDispatch())
	if !errors.Is(err, ErrControlMessageLimitExceeded) {
		t.Fatalf("expected string limit rejection, got %v", err)
	}
	if signer.calls != 0 || appender.calls != 0 {
		t.Fatal("unbounded command reached signer or Redis")
	}
}

func TestProducerRejectsCorruptPreparedBytesBeforeRedisAppend(t *testing.T) {
	appender := &appenderStub{}
	producer, err := NewProducer(validProducerConfig(), &signerStub{}, appender)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := producer.PrepareValidation(context.Background(), validTransportDispatch())
	if err != nil {
		t.Fatal(err)
	}
	prepared.Bytes[0] ^= 0xff
	if err := producer.AppendPrepared(context.Background(), validTransportDispatch().OutboxID, prepared); !errors.Is(err, executionapp.ErrInvalidPreparedEnvelope) {
		t.Fatalf("expected corrupt prepared envelope rejection, got %v", err)
	}
	if appender.calls != 0 {
		t.Fatal("corrupt prepared bytes reached Redis")
	}
}

func TestProducerRejectsTestProfileWithoutExplicitConformanceSignatureOptIn(t *testing.T) {
	config := validProducerConfig()
	config.AllowTestOnlyHMAC = false
	signer := &signerStub{}
	appender := &appenderStub{}
	producer, err := NewProducer(config, signer, appender)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := producer.PrepareValidation(context.Background(), validTransportDispatch()); err == nil {
		t.Fatal("production producer accepted the public test-only HMAC profile")
	}
	if signer.calls != 1 || appender.calls != 0 {
		t.Fatal("rejected test-only signature reached Redis")
	}
}

func TestLimitsFromProtoMatchesCheckedConformanceProfile(t *testing.T) {
	_, sourceFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	path := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "../../../../../testdata/proto/runtime/v1/configuration-validation/conformance-limits.pb"))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	profile := &runtimev1.ProtocolLimitsV1{}
	if err := proto.Unmarshal(raw, profile); err != nil {
		t.Fatal(err)
	}
	limits, err := LimitsFromProto(profile)
	if err != nil {
		t.Fatal(err)
	}
	if limits.Revision != "elitea.runtime.limits.conformance.v2" || limits.MaxWorkerCommandBytes != 32768 || limits.MaxSignedEnvelopeBytes != 49152 || limits.MaxRedisFieldBytes != 49152 || limits.MaxRedisEntryBytes != 65536 || limits.MaxStringBytes != 256 {
		t.Fatalf("Go Redis limits drifted from checked protocol profile: %+v", limits)
	}
}

func validProducerConfig() ProducerConfig {
	return ProducerConfig{
		Stream:                 "commands.v1.configuration.validate.cpu-small.credential-free.1.0",
		ProtocolRevision:       "runtime-v1",
		EnvelopeSchemaRevision: "signed-command-v1",
		AllowTestOnlyHMAC:      true,
		Limits: Limits{
			Revision:               "limits-v1",
			MaxWorkerCommandBytes:  8 * 1024,
			MaxSignedEnvelopeBytes: 12 * 1024,
			MaxRedisFieldBytes:     12 * 1024,
			MaxRedisEntryBytes:     16 * 1024,
			MaxSignatureBytes:      128,
			MaxStringBytes:         512,
		},
	}
}

func validTransportDispatch() executionapp.ValidationDispatch {
	return executionapp.ValidationDispatch{
		OutboxID:              "outbox-1",
		CommandID:             "command-1",
		ExecutionID:           "execution-1",
		Generation:            1,
		DispatchOrdinal:       1,
		TenantID:              "tenant-1",
		ResourceProjectID:     "project-1",
		ProjectionProjectID:   "project-1",
		PrincipalRef:          "actor-1",
		InputBundleID:         "bundle-1",
		InputBundleVersion:    "bundle-v1",
		InputBundleMediaType:  "application/x-protobuf",
		InputBundleByteLength: 256,
		InputBundleDigest:     runtimedomain.SHA256([]byte("manifest")),
		CapabilityVersion:     "v1",
		ResourceClass:         "cpu-small",
		IsolationClass:        "credential-free",
		Priority:              1,
		Deadline:              time.Date(2026, time.July, 16, 12, 1, 0, 0, time.UTC),
		LimitsRevision:        "limits-v1",
		Command: configurationdomain.ValidationCommand{
			ConfigurationRevisionID: "revision-1",
			ConfigurationType:       "openapi",
			CatalogRevision:         "sdk-commit",
			CatalogDigest:           runtimedomain.SHA256([]byte("catalog")),
			SchemaID:                "openapi",
			SchemaRevision:          "schema-v1",
			SchemaDigest:            runtimedomain.SHA256([]byte("schema")),
			SettingsEntryID:         "settings",
		},
	}
}
