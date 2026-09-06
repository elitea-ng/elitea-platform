package redisdispatch

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	toolkitexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

func TestAgentProducerBuildsReferenceOnlyCommandsForBothCurrentSemantics(t *testing.T) {
	for _, test := range []struct {
		capabilityID string
		commandType  runtimev1.WorkerCommandTypeV1
	}{
		{executiondomain.AgentApplicationCapability, runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION},
		{executiondomain.AgentAdhocCapability, runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_ADHOC},
	} {
		t.Run(test.capabilityID, func(t *testing.T) {
			appender := &appenderStub{}
			producer, err := NewAgentExecutionProducer(validAgentProducerConfig(), &signerStub{}, appender)
			if err != nil {
				t.Fatal(err)
			}
			dispatch := validAgentTransportDispatch()
			dispatch.CapabilityID = test.capabilityID
			prepared, err := producer.PrepareAgentExecution(context.Background(), dispatch)
			if err != nil {
				t.Fatal(err)
			}
			if encodedRedisEntryBytes(redisEnvelopeField, prepared.Bytes) >= 64<<10 {
				t.Fatal("agent Redis entry reached the forbidden 64 KiB boundary")
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
			if wire.GetCapabilityId() != test.capabilityID || wire.GetCommandType() != test.commandType ||
				wire.GetAgentExecution().GetRequestEntryId() != dispatch.RequestEntryID ||
				wire.GetAgentExecution().GetClientStreamId() != dispatch.ClientStreamID ||
				wire.GetAgentExecution().GetClientMessageId() != dispatch.ClientMessageID ||
				wire.GetAgentExecution().GetSioEvent() != dispatch.SIOEvent {
				t.Fatalf("agent reference contract changed on the wire: %+v", wire)
			}
		})
	}
}

func TestAgentProducerKeepsRequestCredentialsAndOutputOffRedis(t *testing.T) {
	producer, err := NewAgentExecutionProducer(validAgentProducerConfig(), &signerStub{}, &appenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	canaries := [][]byte{
		[]byte("PROMPT_CONTENT_CANARY"),
		[]byte("MODEL_CREDENTIAL_CANARY"),
		[]byte("THINKING_OUTPUT_CANARY"),
	}
	dispatch := validAgentTransportDispatch()
	dispatch.InputBundleDigest = runtimedomain.SHA256(bytes.Join(canaries, nil))
	prepared, err := producer.PrepareAgentExecution(context.Background(), dispatch)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range canaries {
		if bytes.Contains(prepared.Bytes, canary) {
			t.Fatalf("agent control envelope contains data-plane content %q", canary)
		}
	}
}

func TestAgentProducerBuildsReferenceOnlyDirectToolkitCommand(t *testing.T) {
	appender := &appenderStub{}
	producer, err := NewAgentExecutionProducer(validAgentProducerConfig(), &signerStub{}, appender)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := toolkitexecutionapp.ToolkitExecuteReadDispatch{
		OutboxID: "toolkit-outbox-1", CommandID: "toolkit-command-1", ExecutionID: "toolkit-execution-1",
		Generation: 1, DispatchOrdinal: 1, TenantID: "tenant-1", ResourceProjectID: "2",
		ProjectionProjectID: "2", PrincipalRef: "actor-1", InputBundleID: "toolkit-bundle-1",
		InputBundleVersion: "admission:toolkit-bundle-1", InputBundleMediaType: executiondomain.InputBundleManifestMediaType,
		InputBundleByteLength: 512, InputBundleDigest: runtimedomain.SHA256([]byte("toolkit manifest")),
		CapabilityID: executiondomain.ToolkitExecuteReadCapability, CapabilityVersion: "1",
		ResourceClass: "toolkit-read", IsolationClass: "project", Priority: 1,
		Deadline: time.Now().UTC().Add(time.Minute), LimitsRevision: "limits-v1", RequestEntryID: "toolkit-read-request",
	}
	prepared, err := producer.PrepareToolkitExecuteRead(context.Background(), dispatch)
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.AppendPrepared(context.Background(), dispatch.OutboxID, prepared); err != nil {
		t.Fatal(err)
	}
	var envelope runtimev1.SignedWorkerCommandEnvelopeV1
	if err := proto.Unmarshal(appender.value, &envelope); err != nil {
		t.Fatal(err)
	}
	var command runtimev1.WorkerCommandV1
	if err := proto.Unmarshal(envelope.GetWorkerCommandBytes(), &command); err != nil {
		t.Fatal(err)
	}
	if command.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_TOOLKIT_EXECUTE_READ ||
		command.GetCapabilityId() != executiondomain.ToolkitExecuteReadCapability ||
		command.GetToolkitExecuteRead().GetRequestEntryId() != dispatch.RequestEntryID ||
		command.GetAgentExecution() != nil {
		t.Fatalf("direct toolkit reference contract changed: %+v", &command)
	}
}

func TestAgentProducerMayShareTheIndexWorkerControlStream(t *testing.T) {
	config := validAgentProducerConfig()
	config.Stream = config.IndexIngestStream
	producer, err := NewAgentExecutionProducer(config, &signerStub{}, &appenderStub{})
	if err != nil {
		t.Fatalf("shared index worker stream rejected: %v", err)
	}
	if producer.Stream() != config.IndexIngestStream {
		t.Fatalf("agent stream = %q, want %q", producer.Stream(), config.IndexIngestStream)
	}
}

func TestAgentProducerRejectsCapabilityCommandMismatchAndForeignPreparedEnvelope(t *testing.T) {
	producer, err := NewAgentExecutionProducer(validAgentProducerConfig(), &signerStub{}, &appenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	command, err := agentExecutionWorkerCommand(validAgentProducerConfig().ProtocolRevision, validAgentTransportDispatch())
	if err != nil {
		t.Fatal(err)
	}
	command.CommandType = runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_ADHOC
	if _, err := producer.Prepare(context.Background(), command); !errors.Is(err, ErrInvalidAgentExecutionCommand) {
		t.Fatalf("capability/command mismatch error = %v", err)
	}

	validation, err := NewProducer(validProducerConfig(), &signerStub{}, &appenderStub{})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := validation.PrepareValidation(context.Background(), validTransportDispatch())
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.AppendPrepared(context.Background(), "agent-outbox-1", prepared); !errors.Is(err, executionapp.ErrInvalidPreparedEnvelope) {
		t.Fatalf("foreign prepared envelope error = %v", err)
	}
}

func validAgentProducerConfig() AgentExecutionProducerConfig {
	base := validIndexIngestProducerConfig()
	return AgentExecutionProducerConfig{
		Stream:                       "elitea:runtime:agent:commands",
		ConsumerGroup:                "elitea-runtime-agent-workers",
		ValidationStream:             base.ValidationStream,
		IndexIngestStream:            base.Stream,
		ProtocolRevision:             base.ProtocolRevision,
		EnvelopeSchemaRevision:       base.EnvelopeSchemaRevision,
		ApplicationCapabilityVersion: base.CapabilityVersion,
		AdhocCapabilityVersion:       base.CapabilityVersion,
		ToolkitReadCapabilityVersion: base.CapabilityVersion,
		Limits:                       base.Limits,
		AllowTestOnlyHMAC:            true,
	}
}

func validAgentTransportDispatch() agentexecutionapp.AgentExecutionDispatch {
	return agentexecutionapp.AgentExecutionDispatch{
		OutboxID:              "agent-outbox-1",
		CommandID:             "agent-command-1",
		ExecutionID:           "agent-execution-1",
		Generation:            1,
		DispatchOrdinal:       1,
		TenantID:              "tenant-1",
		ResourceProjectID:     "2",
		ProjectionProjectID:   "2",
		PrincipalRef:          "actor-1",
		InputBundleID:         "agent-input-bundle-1",
		InputBundleVersion:    "admission:agent-input-bundle-1",
		InputBundleMediaType:  executiondomain.InputBundleManifestMediaType,
		InputBundleByteLength: 512,
		InputBundleDigest:     runtimedomain.SHA256([]byte("agent manifest")),
		CapabilityID:          executiondomain.AgentApplicationCapability,
		CapabilityVersion:     "1",
		ResourceClass:         "agent",
		IsolationClass:        "project",
		Priority:              1,
		Deadline:              time.Now().UTC().Add(time.Minute),
		LimitsRevision:        "limits-v1",
		RequestEntryID:        "agent-request",
		ClientStreamID:        "conversation-1",
		ClientMessageID:       "response-1",
		SIOEvent:              "chat_predict",
	}
}
