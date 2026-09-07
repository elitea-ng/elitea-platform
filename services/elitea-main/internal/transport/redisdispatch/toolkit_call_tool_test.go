package redisdispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

func validToolkitCallToolProducerConfig() ToolkitCallToolProducerConfig {
	base := validProducerConfig()
	return ToolkitCallToolProducerConfig{
		Stream:                 "commands.v1.index.ingest.indexing.shared.1.0",
		ConsumerGroup:          "elitea-indexer-worker-v1",
		ValidationStream:       base.Stream,
		ProtocolRevision:       base.ProtocolRevision,
		EnvelopeSchemaRevision: base.EnvelopeSchemaRevision,
		CapabilityVersion:      "1",
		Limits:                 base.Limits,
		AllowTestOnlyHMAC:      true,
	}
}

func validToolkitCallToolDispatch() toolkitcalltoolapp.Dispatch {
	return toolkitcalltoolapp.Dispatch{
		OutboxID:              "tool-outbox-1",
		CommandID:             "tool-command-1",
		ExecutionID:           "tool-execution-1",
		Generation:            1,
		DispatchOrdinal:       1,
		TenantID:              "1",
		ResourceProjectID:     "1",
		ProjectionProjectID:   "1",
		PrincipalRef:          "7",
		InputBundleID:         "bundle-1",
		InputBundleVersion:    "admission:bundle-1",
		InputBundleMediaType:  executiondomain.InputBundleManifestMediaType,
		InputBundleByteLength: 512,
		InputBundleDigest:     runtimedomain.SHA256([]byte("manifest")),
		CapabilityID:          executiondomain.ToolkitCallToolCapability,
		CapabilityVersion:     "1",
		ResourceClass:         "indexing",
		IsolationClass:        "project",
		Priority:              1,
		Deadline:              time.Now().Add(time.Hour).UTC(),
		LimitsRevision:        "limits-v1",
		ToolkitType:           "github",
		ToolName:              "list_issues",
		ToolkitID:             "19",
		ToolkitVersion:        "3",
		SettingsEntryID:       toolkitcalltoolapp.SettingsEntryID,
		ArgumentsEntryID:      toolkitcalltoolapp.ArgumentsEntryID,
	}
}

// The command must carry entry REFERENCES and nothing else: neither the
// redeemed settings nor the caller's arguments may reach Redis.
func TestToolkitCallToolCommandCarriesOnlyReferences(t *testing.T) {
	producer, err := NewToolkitCallToolProducer(
		validToolkitCallToolProducerConfig(), &signerStub{}, &appenderStub{},
	)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := validToolkitCallToolDispatch()
	prepared, err := producer.PrepareToolkitCallTool(context.Background(), dispatch)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	envelope := &runtimev1.SignedWorkerCommandEnvelopeV1{}
	if err := proto.Unmarshal(prepared.Bytes, envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	command := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(envelope.GetWorkerCommandBytes(), command); err != nil {
		t.Fatalf("decode command: %v", err)
	}
	if command.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_TOOLKIT_CALL_TOOL {
		t.Fatalf("command type %v", command.GetCommandType())
	}
	if command.GetCapabilityId() != executiondomain.ToolkitCallToolCapability {
		t.Fatalf("capability %q", command.GetCapabilityId())
	}
	run := command.GetToolkitCallTool()
	if run == nil {
		t.Fatal("the command carries no tool-run payload")
	}
	if run.GetToolkitType() != "github" || run.GetToolName() != "list_issues" ||
		run.GetToolkitId() != "19" || run.GetToolkitVersion() != "3" {
		t.Fatalf("tool-run identity is wrong: %+v", run)
	}
	if run.GetSettingsEntryId() != toolkitcalltoolapp.SettingsEntryID ||
		run.GetArgumentsEntryId() != toolkitcalltoolapp.ArgumentsEntryID {
		t.Fatalf("entry references are wrong: %q/%q",
			run.GetSettingsEntryId(), run.GetArgumentsEntryId())
	}
	// The bundle reference resolves the manifest the admission wrote: same id,
	// same immutable version, same digest, same length.
	input := command.GetInputBundleRef()
	if input.GetInputBundleId() != dispatch.InputBundleID ||
		input.GetImmutableVersion() != dispatch.InputBundleVersion ||
		input.GetByteLength() != dispatch.InputBundleByteLength ||
		string(input.GetDigest().GetValue()) != string(dispatch.InputBundleDigest[:]) {
		t.Fatalf("input bundle reference does not match the admitted bundle: %+v", input)
	}
	// The command names the ROOT execution as itself and no parent, which is
	// what the worker's own bounded scanner requires.
	if command.GetRootExecutionId() != command.GetExecutionId() ||
		command.GetParentExecutionId() != "" || command.GetParentCallId() != "" {
		t.Fatal("the command claims a parent execution")
	}
}

// The whole point of two entries is that one cannot serve both roles. A
// producer that signed such a command would defeat the four other places that
// refuse it.
//
// Both layers are exercised. Dispatch.Validate refuses it on the way IN, and
// the producer's own validateCommand refuses it on the way OUT — the second is
// what guards AppendPrepared, which reads a command back out of a durable
// envelope and never sees a Dispatch at all.
func TestToolkitCallToolProducerRefusesOneEntryForBothRoles(t *testing.T) {
	producer, err := NewToolkitCallToolProducer(
		validToolkitCallToolProducerConfig(), &signerStub{}, &appenderStub{},
	)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := validToolkitCallToolDispatch()
	dispatch.ArgumentsEntryID = dispatch.SettingsEntryID
	if _, err := producer.PrepareToolkitCallTool(context.Background(), dispatch); err == nil {
		t.Fatal("a dispatch naming one entry for both roles was accepted")
	}

	command, err := toolkitCallToolWorkerCommand("runtime-v1", validToolkitCallToolDispatch())
	if err != nil {
		t.Fatal(err)
	}
	command.GetToolkitCallTool().ArgumentsEntryId = command.GetToolkitCallTool().GetSettingsEntryId()
	if _, err := producer.Prepare(context.Background(), command); !errors.Is(err, ErrInvalidToolkitCallToolCommand) {
		t.Fatalf("a command naming one entry for both roles was signed: %v", err)
	}
}

func TestToolkitCallToolProducerRefusesAMismatchedCapabilityVersion(t *testing.T) {
	producer, err := NewToolkitCallToolProducer(
		validToolkitCallToolProducerConfig(), &signerStub{}, &appenderStub{},
	)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := validToolkitCallToolDispatch()
	dispatch.CapabilityVersion = "99"
	_, err = producer.PrepareToolkitCallTool(context.Background(), dispatch)
	if !errors.Is(err, toolkitcalltoolapp.ErrInvalidToolRunDispatch) {
		t.Fatalf("expected ErrInvalidToolRunDispatch, got %v", err)
	}
}

// A tool run must not share the configuration-validation stream: that stream is
// consumed by a worker with no toolkit SDK at all.
func TestToolkitCallToolProducerRefusesTheValidationStream(t *testing.T) {
	config := validToolkitCallToolProducerConfig()
	config.Stream = config.ValidationStream
	if _, err := NewToolkitCallToolProducer(config, &signerStub{}, &appenderStub{}); err == nil {
		t.Fatal("a tool-run producer was composed on the validation stream")
	}
}

// Preparation must not reach Redis: the durable winner is selected first, so a
// retry appends the same bytes rather than signing a second command.
func TestToolkitCallToolPreparationDoesNotReachRedis(t *testing.T) {
	appender := &appenderStub{}
	producer, err := NewToolkitCallToolProducer(
		validToolkitCallToolProducerConfig(), &signerStub{}, appender,
	)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := producer.PrepareToolkitCallTool(context.Background(), validToolkitCallToolDispatch())
	if err != nil {
		t.Fatal(err)
	}
	if appender.calls != 0 {
		t.Fatal("preparation reached Redis before durable envelope selection")
	}
	if err := producer.AppendPrepared(context.Background(), "tool-outbox-1", prepared); err != nil {
		t.Fatal(err)
	}
	if appender.calls != 1 || appender.stream != validToolkitCallToolProducerConfig().Stream {
		t.Fatalf("unexpected append: %+v", appender)
	}
	if encodedRedisEntryBytes(redisEnvelopeField, prepared.Bytes) >= 64<<10 {
		t.Fatalf("tool-run Redis entry is not strictly below 64 KiB: %d",
			encodedRedisEntryBytes(redisEnvelopeField, prepared.Bytes))
	}
}
