package control

import (
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

func TestToolkitExecuteReadCommandAndInputBindingAreStrict(t *testing.T) {
	command := validVerifierToolkitReadCommand()
	if err := validateCommand(command, toolkitReadVerifierConfig()); err != nil {
		t.Fatal(err)
	}
	roles, err := expectedInputRoles(command)
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 1 || roles["toolkit-read-request"] != executiondomain.ToolkitExecuteReadRequestRole {
		t.Fatalf("roles = %#v", roles)
	}
	mediaType, maximum, err := inputContentContract(executiondomain.ToolkitExecuteReadRequestRole, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	if mediaType != executiondomain.ToolkitExecuteReadInputMediaType ||
		maximum != executiondomain.MaxToolkitExecuteReadInputBytes {
		t.Fatalf("content contract = %q/%d", mediaType, maximum)
	}

	command.CapabilityCommand = &runtimev1.WorkerCommandV1_AgentExecution{
		AgentExecution: &runtimev1.AgentExecutionCommandV1{
			RequestEntryId: "toolkit-read-request", ClientStreamId: "room", ClientMessageId: "message", SioEvent: "chat_predict",
		},
	}
	if !errors.Is(validateCommand(command, toolkitReadVerifierConfig()), ErrMalformedWorkerCommand) {
		t.Fatal("direct toolkit capability accepted an agent command payload")
	}
}

func TestToolkitExecuteReadCommandRejectsNestedOrUnsafeReferences(t *testing.T) {
	command := validVerifierToolkitReadCommand()
	command.ParentExecutionId = "parent"
	if !errors.Is(validateCommand(command, toolkitReadVerifierConfig()), ErrMalformedWorkerCommand) {
		t.Fatal("direct toolkit command accepted nested execution identity")
	}
	command = validVerifierToolkitReadCommand()
	command.GetToolkitExecuteRead().RequestEntryId = "request\nforged"
	if !errors.Is(validateCommand(command, toolkitReadVerifierConfig()), ErrMalformedWorkerCommand) {
		t.Fatal("direct toolkit command accepted an unsafe request entry")
	}
}

func toolkitReadVerifierConfig() commandValidationConfig {
	return commandValidationConfig{
		ProtocolRevision: "elitea.runtime.v1",
		CapabilityVersions: map[string]string{
			executiondomain.ToolkitExecuteReadCapability: "1",
		},
		LimitsRevision: "limits-v1", MaxInputManifestBytes: 64 << 10, MaxStringBytes: 1024,
	}
}

func validVerifierToolkitReadCommand() *runtimev1.WorkerCommandV1 {
	return &runtimev1.WorkerCommandV1{
		ProtocolRevision: "elitea.runtime.v1", CommandId: "command", IdempotencyKey: "outbox",
		CommandType: runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_TOOLKIT_EXECUTE_READ,
		ExecutionId: "execution", Generation: 1, DispatchOrdinal: 1, RootExecutionId: "execution",
		TenantId: "tenant", ResourceProjectId: "7", ProjectionProjectId: "7", PrincipalRef: "11",
		InputBundleRef: &runtimev1.ExecutionInputBundleReferenceV1{
			InputBundleId: "bundle", ImmutableVersion: "version", Digest: testDigest([]byte("manifest")),
			ByteLength: 128, MediaType: executiondomain.InputBundleManifestMediaType,
		},
		CapabilityId: executiondomain.ToolkitExecuteReadCapability, CapabilityVersion: "1",
		ResourceClass: "toolkit-read", IsolationClass: "project", Priority: 1,
		DeadlineUnixMillis: time.Now().UTC().Add(time.Minute).UnixMilli(), LimitsRevision: "limits-v1",
		CapabilityCommand: &runtimev1.WorkerCommandV1_ToolkitExecuteRead{
			ToolkitExecuteRead: &runtimev1.ToolkitExecuteReadCommandV1{RequestEntryId: "toolkit-read-request"},
		},
	}
}
