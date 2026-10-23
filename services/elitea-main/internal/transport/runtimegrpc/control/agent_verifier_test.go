package control

import (
	"errors"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

func TestCommandVerifierAcceptsBothTypedAgentCommands(t *testing.T) {
	for _, test := range []struct {
		capabilityID string
		commandType  runtimev1.WorkerCommandTypeV1
	}{
		{executiondomain.AgentApplicationCapability, runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION},
		{executiondomain.AgentAdhocCapability, runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_ADHOC},
	} {
		t.Run(test.capabilityID, func(t *testing.T) {
			command := validVerifierAgentCommand(test.capabilityID, test.commandType)
			if err := validateCommand(command, agentVerifierConfig()); err != nil {
				t.Fatalf("valid agent command rejected: %v", err)
			}
		})
	}
}

func TestCommandVerifierRejectsAgentCapabilityTypeMismatchAndMissingCorrelation(t *testing.T) {
	command := validVerifierAgentCommand(
		executiondomain.AgentApplicationCapability,
		runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_ADHOC,
	)
	if !errors.Is(validateCommand(command, agentVerifierConfig()), ErrMalformedWorkerCommand) {
		t.Fatal("agent capability/command type mismatch was accepted")
	}

	command = validVerifierAgentCommand(
		executiondomain.AgentApplicationCapability,
		runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION,
	)
	command.GetAgentExecution().ClientMessageId = ""
	if !errors.Is(validateCommand(command, agentVerifierConfig()), ErrMalformedWorkerCommand) {
		t.Fatal("agent command without current message correlation was accepted")
	}
}

func agentVerifierConfig() commandValidationConfig {
	return commandValidationConfig{
		ProtocolRevision: "protocol-v1",
		CapabilityVersions: map[string]string{
			executiondomain.AgentApplicationCapability: "1",
			executiondomain.AgentAdhocCapability:       "1",
		},
		LimitsRevision:        "limits-v1",
		MaxInputManifestBytes: 64 * 1024,
		MaxStringBytes:        512,
	}
}

func validVerifierAgentCommand(capabilityID string, commandType runtimev1.WorkerCommandTypeV1) *runtimev1.WorkerCommandV1 {
	return &runtimev1.WorkerCommandV1{
		ProtocolRevision:    "protocol-v1",
		CommandId:           "agent-command-1",
		IdempotencyKey:      "agent-outbox-1",
		CommandType:         commandType,
		ExecutionId:         "agent-execution-1",
		Generation:          1,
		DispatchOrdinal:     1,
		RootExecutionId:     "agent-execution-1",
		TenantId:            "tenant-1",
		ResourceProjectId:   "2",
		ProjectionProjectId: "2",
		PrincipalRef:        "actor-1",
		InputBundleRef: &runtimev1.ExecutionInputBundleReferenceV1{
			InputBundleId:    "agent-input-bundle-1",
			ImmutableVersion: "admission:agent-input-bundle-1",
			Digest: &runtimev1.DigestV1{
				Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
				Value:     make([]byte, 32),
			},
			ByteLength: 512,
			MediaType:  executiondomain.InputBundleManifestMediaType,
		},
		CapabilityId:       capabilityID,
		CapabilityVersion:  "1",
		ResourceClass:      "agent",
		IsolationClass:     "project",
		Priority:           1,
		DeadlineUnixMillis: 1,
		LimitsRevision:     "limits-v1",
		CapabilityCommand: &runtimev1.WorkerCommandV1_AgentExecution{
			AgentExecution: &runtimev1.AgentExecutionCommandV1{
				RequestEntryId:  "agent-request",
				ClientStreamId:  "conversation-1",
				ClientMessageId: "response-1",
				SioEvent:        "chat_predict",
			},
		},
	}
}
