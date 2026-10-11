package commandbus

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

type failingAppenderStub struct {
	err   error
	calls int
}

func (a *failingAppenderStub) Append(context.Context, string, string, []byte) (string, error) {
	a.calls++
	return "", a.err
}

func TestIndexIngestProducerUsesDedicatedBoundedRoute(t *testing.T) {
	signer := &signerStub{}
	appender := &appenderStub{}
	config := validIndexIngestProducerConfig()
	producer, err := NewIndexIngestProducer(config, signer, appender)
	if err != nil {
		t.Fatal(err)
	}
	if producer.Stream() != config.Stream || producer.Consumer() != config.Consumer {
		t.Fatalf("unexpected index ingest route: stream=%q group=%q", producer.Stream(), producer.Consumer())
	}

	command := validIndexIngestCommand()
	prepared, err := producer.Prepare(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if appender.calls != 0 {
		t.Fatal("preparation reached the bus before durable envelope selection")
	}
	if encodedTransportMessageBytes(prepared.Bytes) >= 64<<10 {
		t.Fatalf("index ingest message is not strictly below 64 KiB: %d", encodedTransportMessageBytes(prepared.Bytes))
	}
	if err := producer.AppendPrepared(context.Background(), command.GetIdempotencyKey(), prepared); err != nil {
		t.Fatal(err)
	}
	if appender.calls != 1 || appender.stream != config.Stream || appender.deliveryID != command.GetIdempotencyKey() {
		t.Fatalf("unexpected index ingest append: %+v", appender)
	}

	envelope := &runtimev1.SignedWorkerCommandEnvelopeV1{}
	if err := proto.Unmarshal(appender.value, envelope); err != nil {
		t.Fatal(err)
	}
	wire := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(envelope.GetWorkerCommandBytes(), wire); err != nil {
		t.Fatal(err)
	}
	if wire.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_INDEX_INGEST || wire.GetCapabilityId() != indexIngestCapabilityID || wire.GetIndexIngest() == nil {
		t.Fatalf("unexpected index ingest wire command: %+v", wire)
	}
}

func TestIndexIngestProducerBuildsTypedReferenceOnlyCommand(t *testing.T) {
	appender := &appenderStub{}
	producer, err := NewIndexIngestProducer(validIndexIngestProducerConfig(), &signerStub{}, appender)
	if err != nil {
		t.Fatal(err)
	}
	command := validIndexIngestCommand()
	dispatch := indexingapp.IndexIngestDispatch{
		OutboxID:                    command.GetIdempotencyKey(),
		CommandID:                   command.GetCommandId(),
		ExecutionID:                 command.GetExecutionId(),
		Generation:                  command.GetGeneration(),
		DispatchOrdinal:             command.GetDispatchOrdinal(),
		TenantID:                    command.GetTenantId(),
		ResourceProjectID:           command.GetResourceProjectId(),
		ProjectionProjectID:         command.GetProjectionProjectId(),
		PrincipalRef:                command.GetPrincipalRef(),
		InputBundleID:               command.GetInputBundleRef().GetInputBundleId(),
		InputBundleVersion:          command.GetInputBundleRef().GetImmutableVersion(),
		InputBundleMediaType:        command.GetInputBundleRef().GetMediaType(),
		InputBundleByteLength:       command.GetInputBundleRef().GetByteLength(),
		InputBundleDigest:           runtimedomain.SHA256([]byte("index manifest")),
		CapabilityVersion:           command.GetCapabilityVersion(),
		ResourceClass:               command.GetResourceClass(),
		IsolationClass:              command.GetIsolationClass(),
		Priority:                    command.GetPriority(),
		Deadline:                    time.UnixMilli(command.GetDeadlineUnixMillis()).UTC(),
		LimitsRevision:              command.GetLimitsRevision(),
		ToolkitConfigurationEntryID: command.GetIndexIngest().GetToolkitConfigurationEntryId(),
		ToolParametersEntryID:       command.GetIndexIngest().GetToolParametersEntryId(),
		LLMModelEntryID:             command.GetIndexIngest().GetLlmModelEntryId(),
		LLMConfigurationEntryID:     command.GetIndexIngest().GetLlmConfigurationEntryId(),
		MCPTokensEntryID:            command.GetIndexIngest().GetMcpTokensEntryId(),
		ClientStreamID:              command.GetIndexIngest().GetClientStreamId(),
		ClientMessageID:             command.GetIndexIngest().GetClientMessageId(),
		SIOEvent:                    command.GetIndexIngest().GetSioEvent(),
		Initiator:                   executiondomain.IndexIngestInitiator(command.GetIndexIngest().GetInitiator()),
		// Rust mode: the stamp of the index, which the worker must match.
		ExpectedEmbeddingModel:     "Org/Model.A",
		ExpectedEmbeddingDimension: 1536,
	}
	prepared, err := producer.PrepareIndexIngest(context.Background(), dispatch)
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.AppendPrepared(context.Background(), dispatch.OutboxID, prepared); err != nil {
		t.Fatal(err)
	}
	envelope := &runtimev1.SignedWorkerCommandEnvelopeV1{}
	if err := proto.Unmarshal(appender.value, envelope); err != nil {
		t.Fatal(err)
	}
	wire := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(envelope.GetWorkerCommandBytes(), wire); err != nil {
		t.Fatal(err)
	}
	if wire.GetInputBundleRef().GetInputBundleId() != dispatch.InputBundleID || wire.GetIndexIngest().GetToolkitConfigurationEntryId() != dispatch.ToolkitConfigurationEntryID || wire.GetIndexIngest().GetToolParametersEntryId() != dispatch.ToolParametersEntryID || wire.GetIndexIngest().GetClientStreamId() != dispatch.ClientStreamID || wire.GetIndexIngest().GetClientMessageId() != dispatch.ClientMessageID || wire.GetIndexIngest().GetSioEvent() != dispatch.SIOEvent || wire.GetIndexIngest().GetInitiator() != string(dispatch.Initiator) {
		t.Fatalf("typed dispatch changed reference identities: %+v", wire)
	}
	if wire.GetIndexIngest().GetExpectedEmbeddingModel() != "Org/Model.A" ||
		wire.GetIndexIngest().GetExpectedEmbeddingDimension() != 1536 {
		t.Fatalf("the expected embedding space was not carried: %+v", wire.GetIndexIngest())
	}
}

func TestIndexIngestDispatchRefusesHalfAnExpectedEmbeddingSpace(t *testing.T) {
	for name, mutate := range map[string]func(*indexingapp.IndexIngestDispatch){
		"model without dimension": func(d *indexingapp.IndexIngestDispatch) { d.ExpectedEmbeddingModel = "m" },
		"dimension without model": func(d *indexingapp.IndexIngestDispatch) { d.ExpectedEmbeddingDimension = 8 },
		"oversized dimension": func(d *indexingapp.IndexIngestDispatch) {
			d.ExpectedEmbeddingModel, d.ExpectedEmbeddingDimension = "m", 70000
		},
		"model with a newline": func(d *indexingapp.IndexIngestDispatch) {
			d.ExpectedEmbeddingModel, d.ExpectedEmbeddingDimension = "m\n", 8
		},
	} {
		t.Run(name, func(t *testing.T) {
			dispatch := validIndexIngestDispatchForExpectedEmbedding()
			mutate(&dispatch)
			if err := dispatch.Validate(); !errors.Is(err, indexingapp.ErrInvalidIndexIngestDispatch) {
				t.Fatalf("validate = %v, want ErrInvalidIndexIngestDispatch", err)
			}
		})
	}
	if err := validIndexIngestDispatchForExpectedEmbedding().Validate(); err != nil {
		t.Fatalf("a dispatch without an expected space (python mode, unstamped index) = %v", err)
	}
}

func validIndexIngestDispatchForExpectedEmbedding() indexingapp.IndexIngestDispatch {
	return indexingapp.IndexIngestDispatch{
		OutboxID: "outbox-1", CommandID: "command-1", ExecutionID: "execution-1", Generation: 1, DispatchOrdinal: 1,
		TenantID: "1", ResourceProjectID: "1", ProjectionProjectID: "1", PrincipalRef: "principal-1",
		InputBundleID: "bundle-1", InputBundleVersion: "1", InputBundleMediaType: "application/x-protobuf",
		InputBundleByteLength: 1, InputBundleDigest: runtimedomain.SHA256([]byte("m")),
		CapabilityVersion: "1", ResourceClass: "indexing", IsolationClass: "shared", Priority: 1,
		Deadline: time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC), LimitsRevision: "limits-v1",
		ToolkitConfigurationEntryID: "toolkit-config", ToolParametersEntryID: "tool-params",
		Initiator: executiondomain.IndexIngestInitiatorUser,
	}
}

func TestIndexIngestProducerKeepsBulkAndProtectedValuesOffTheBus(t *testing.T) {
	producer, err := NewIndexIngestProducer(validIndexIngestProducerConfig(), &signerStub{}, &appenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	canaries := [][]byte{
		[]byte(`{"source":"confluence-page-with-images"}`),
		[]byte(`{"toolkit_config":{"password":"CONFIG_SECRET_CANARY"}}`),
		[]byte(`{"result":"32-MIB-INDEX-RESULT-CANARY"}`),
	}
	command := validIndexIngestCommand()
	command.InputBundleRef.Digest = digestProto(runtimedomain.SHA256(bytes.Join(canaries, nil)))
	prepared, err := producer.Prepare(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range canaries {
		if bytes.Contains(prepared.Bytes, canary) {
			t.Fatalf("index control envelope contains forbidden data-plane bytes %q", canary)
		}
	}
	for _, fragment := range [][]byte{[]byte("CONFIG_SECRET_CANARY"), []byte("32-MIB-INDEX-RESULT-CANARY"), []byte("confluence-page-with-images")} {
		if bytes.Contains(prepared.Bytes, fragment) {
			t.Fatalf("index control envelope contains forbidden data-plane fragment %q", fragment)
		}
	}
}

func TestIndexIngestProducerRejectsWrongOrMalformedContractBeforeSigning(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*runtimev1.WorkerCommandV1)
	}{
		{
			name: "wrong capability",
			mutate: func(command *runtimev1.WorkerCommandV1) {
				command.CapabilityId = "configuration.validate.v1"
			},
		},
		{
			name: "wrong command type",
			mutate: func(command *runtimev1.WorkerCommandV1) {
				command.CommandType = runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_CONFIGURATION_VALIDATE
			},
		},
		{
			name: "wrong oneof",
			mutate: func(command *runtimev1.WorkerCommandV1) {
				command.CapabilityCommand = &runtimev1.WorkerCommandV1_ConfigurationValidation{
					ConfigurationValidation: &runtimev1.ConfigurationValidationCommandV1{},
				}
			},
		},
		{
			name: "missing required toolkit reference",
			mutate: func(command *runtimev1.WorkerCommandV1) {
				command.GetIndexIngest().ToolkitConfigurationEntryId = ""
			},
		},
		{
			name: "duplicate input reference",
			mutate: func(command *runtimev1.WorkerCommandV1) {
				command.GetIndexIngest().LlmModelEntryId = command.GetIndexIngest().ToolParametersEntryId
			},
		},
		{
			name: "unknown initiator",
			mutate: func(command *runtimev1.WorkerCommandV1) {
				command.GetIndexIngest().Initiator = "automation"
			},
		},
		{
			name: "unknown nested field",
			mutate: func(command *runtimev1.WorkerCommandV1) {
				command.GetIndexIngest().ProtoReflect().SetUnknown([]byte{0xa2, 0x06, 0x03, 'r', 'a', 'w'})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			signer := &signerStub{}
			appender := &appenderStub{}
			producer, err := NewIndexIngestProducer(validIndexIngestProducerConfig(), signer, appender)
			if err != nil {
				t.Fatal(err)
			}
			command := proto.Clone(validIndexIngestCommand()).(*runtimev1.WorkerCommandV1)
			test.mutate(command)
			if _, err := producer.Prepare(context.Background(), command); !errors.Is(err, ErrInvalidIndexIngestCommand) {
				t.Fatalf("malformed index command error = %v", err)
			}
			if signer.calls != 0 || appender.calls != 0 {
				t.Fatalf("malformed command crossed signing/bus boundary: signer=%d appender=%d", signer.calls, appender.calls)
			}
		})
	}
}

func TestIndexIngestProducerRejectsInclusive64KiBLimitAndOversizedCommand(t *testing.T) {
	config := validIndexIngestProducerConfig()
	config.Limits = Limits{
		Revision:                 config.Limits.Revision,
		MaxWorkerCommandBytes:    32 * 1024,
		MaxSignedEnvelopeBytes:   48 * 1024,
		MaxTransportPayloadBytes: 48 * 1024,
		MaxTransportMessageBytes: 64 * 1024,
		MaxSignatureBytes:        128,
		MaxStringBytes:           50 * 1024,
	}
	if _, err := NewIndexIngestProducer(config, &signerStub{}, &appenderStub{}); err == nil || !strings.Contains(err.Error(), "less than 64 KiB") {
		t.Fatalf("inclusive 64 KiB index entry limit was accepted: %v", err)
	}

	config.Limits.MaxTransportMessageBytes = (64 * 1024) - 1
	config.Limits.MaxWorkerCommandBytes = 1024
	producer, err := NewIndexIngestProducer(config, &signerStub{}, &appenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	command := validIndexIngestCommand()
	command.Traceparent = strings.Repeat("t", 2048)
	if _, err := producer.Prepare(context.Background(), command); !errors.Is(err, ErrControlMessageLimitExceeded) {
		t.Fatalf("oversized index command error = %v", err)
	}
}

func TestIndexIngestProducerRejectsValidationEnvelopeOnDedicatedStream(t *testing.T) {
	validation, err := NewProducer(validProducerConfig(), &signerStub{}, &appenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	dispatch := validTransportDispatch()
	prepared, err := validation.PrepareValidation(context.Background(), dispatch)
	if err != nil {
		t.Fatal(err)
	}
	indexAppender := &appenderStub{}
	index, err := NewIndexIngestProducer(validIndexIngestProducerConfig(), &signerStub{}, indexAppender)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.AppendPrepared(context.Background(), dispatch.OutboxID, prepared); !errors.Is(err, executionapp.ErrInvalidPreparedEnvelope) {
		t.Fatalf("validation envelope on index stream error = %v", err)
	}
	if indexAppender.calls != 0 {
		t.Fatal("validation envelope reached the dedicated index stream")
	}
}

func TestIndexIngestProducerPropagatesNonDroppingBackpressureAndPublishFailure(t *testing.T) {
	tests := []struct {
		name    string
		failure error
		check   func(error) bool
	}{
		{
			name: "saturated",
			failure: &ControlStreamSaturatedError{
				Stream:          "ELITEA_RT_V1_INDEX",
				CurrentMessages: 8,
				MaxMessages:     8,
			},
			check: func(err error) bool {
				return errors.Is(err, ErrControlStreamSaturated) && errors.Is(err, executionapp.ErrDispatchBackpressured)
			},
		},
		{
			name:    "bus unavailable",
			failure: errors.New("test bus unavailable"),
			check: func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "append worker command reference")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			appender := &failingAppenderStub{err: test.failure}
			producer, err := NewIndexIngestProducer(validIndexIngestProducerConfig(), &signerStub{}, appender)
			if err != nil {
				t.Fatal(err)
			}
			command := validIndexIngestCommand()
			prepared, err := producer.Prepare(context.Background(), command)
			if err != nil {
				t.Fatal(err)
			}
			if err := producer.AppendPrepared(context.Background(), command.GetIdempotencyKey(), prepared); !test.check(err) {
				t.Fatalf("append failure = %v", err)
			}
			if appender.calls != 1 {
				t.Fatalf("append attempts = %d, want 1", appender.calls)
			}
		})
	}
}

func TestIndexIngestProducerRequiresDedicatedRoute(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*IndexIngestProducerConfig)
	}{
		{
			name: "shared validation stream",
			mutate: func(config *IndexIngestProducerConfig) {
				config.Stream = config.ValidationStream
			},
		},
		{
			name: "missing consumer",
			mutate: func(config *IndexIngestProducerConfig) {
				config.Consumer = ""
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validIndexIngestProducerConfig()
			test.mutate(&config)
			if _, err := NewIndexIngestProducer(config, &signerStub{}, &appenderStub{}); err == nil {
				t.Fatal("invalid index bus route was accepted")
			}
		})
	}
}

func validIndexIngestProducerConfig() IndexIngestProducerConfig {
	base := validProducerConfig()
	return IndexIngestProducerConfig{
		Stream:                 "commands.v1.index.ingest.indexing.shared.1.0",
		Consumer:               "elitea-indexer-worker-v1",
		ValidationStream:       base.Stream,
		ProtocolRevision:       base.ProtocolRevision,
		EnvelopeSchemaRevision: base.EnvelopeSchemaRevision,
		CapabilityVersion:      "1",
		Limits:                 base.Limits,
		AllowTestOnlyHMAC:      true,
	}
}

func validIndexIngestCommand() *runtimev1.WorkerCommandV1 {
	return &runtimev1.WorkerCommandV1{
		ProtocolRevision:    "runtime-v1",
		CommandId:           "index-command-1",
		IdempotencyKey:      "index-outbox-1",
		CommandType:         runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_INDEX_INGEST,
		ExecutionId:         "index-execution-1",
		Generation:          1,
		DispatchOrdinal:     1,
		RootExecutionId:     "index-execution-1",
		TenantId:            "tenant-1",
		ResourceProjectId:   "project-1",
		ProjectionProjectId: "project-1",
		PrincipalRef:        "principal-1",
		InputBundleRef: &runtimev1.ExecutionInputBundleReferenceV1{
			InputBundleId:    "index-bundle-1",
			ImmutableVersion: "1",
			Digest:           digestProto(runtimedomain.SHA256([]byte("index manifest"))),
			ByteLength:       512,
			MediaType:        "application/x-protobuf",
		},
		CapabilityId:       indexIngestCapabilityID,
		CapabilityVersion:  "1",
		ResourceClass:      "indexing",
		IsolationClass:     "shared",
		Priority:           1,
		DeadlineUnixMillis: time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC).UnixMilli(),
		LimitsRevision:     "limits-v1",
		CapabilityCommand: &runtimev1.WorkerCommandV1_IndexIngest{
			IndexIngest: &runtimev1.IndexIngestCommandV1{
				ToolkitConfigurationEntryId: "toolkit-config",
				ToolParametersEntryId:       "tool-params",
				LlmModelEntryId:             "llm-model",
				LlmConfigurationEntryId:     "llm-config",
				McpTokensEntryId:            "mcp-tokens",
				ClientStreamId:              "stream-1",
				ClientMessageId:             "message-1",
				SioEvent:                    "chat_predict",
				Initiator:                   "user",
			},
		},
	}
}
