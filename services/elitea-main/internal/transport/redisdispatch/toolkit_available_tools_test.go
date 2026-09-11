package redisdispatch

import (
	"bytes"
	"context"
	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	discovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitdiscovery"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

func validToolkitAvailableToolsProducerConfig() ToolkitAvailableToolsProducerConfig {
	base := validProducerConfig()
	return ToolkitAvailableToolsProducerConfig{
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

func validToolkitAvailableToolsDispatch() discovery.Dispatch {
	return discovery.Dispatch{
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
		CapabilityID:          executiondomain.ToolkitAvailableToolsCapability,
		CapabilityVersion:     "1",
		ResourceClass:         "indexing",
		IsolationClass:        "project",
		Priority:              1,
		Deadline:              time.Now().Add(time.Hour).UTC(),
		LimitsRevision:        "limits-v1",
		ToolkitType:           "github",
		SettingsEntryID:       discovery.SettingsEntryID,
	}
}

func TestToolkitDiscoverySignedCommandContainsOnlyReferences(t *testing.T) {
	producer, err := NewToolkitAvailableToolsProducer(validToolkitAvailableToolsProducerConfig(), &signerStub{}, &appenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	dispatch := validToolkitAvailableToolsDispatch()
	prepared, err := producer.PrepareToolkitAvailableTools(context.Background(), dispatch)
	if err != nil {
		t.Fatal(err)
	}
	var envelope runtimev1.SignedWorkerCommandEnvelopeV1
	var command runtimev1.WorkerCommandV1
	if proto.Unmarshal(prepared.Bytes, &envelope) != nil || proto.Unmarshal(envelope.WorkerCommandBytes, &command) != nil {
		t.Fatal("bad signed envelope")
	}
	payload := command.GetToolkitAvailableTools()
	if payload == nil || payload.ToolkitType != "github" || payload.SettingsEntryId != discovery.SettingsEntryID || command.GetToolkitCallTool() != nil || command.CapabilityId != executiondomain.ToolkitAvailableToolsCapability {
		t.Fatal("capability mismatch")
	}
	if payload.ProtoReflect().Descriptor().Fields().Len() != 2 {
		t.Fatal("unexpected inline discovery payload")
	}
	mutated := proto.Clone(&command).(*runtimev1.WorkerCommandV1)
	mutated.CapabilityId = executiondomain.ToolkitCallToolCapability
	if _, err := producer.Prepare(context.Background(), mutated); err == nil {
		t.Fatal("cross-capability command accepted")
	}
	retry, err := producer.PrepareToolkitAvailableTools(context.Background(), dispatch)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prepared.Bytes, retry.Bytes) {
		t.Fatal("same intent encoded differently")
	}
}
