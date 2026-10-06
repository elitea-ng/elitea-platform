package contract_test

import (
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

func TestAgentExecutionUsesOneReferenceOnlyCommandForTwoCurrentSemantics(t *testing.T) {
	command := &runtimev1.WorkerCommandV1{
		CapabilityCommand: &runtimev1.WorkerCommandV1_AgentExecution{
			AgentExecution: &runtimev1.AgentExecutionCommandV1{
				RequestEntryId:  "agent-request",
				ClientStreamId:  "stream-1",
				ClientMessageId: "message-1",
				SioEvent:        "chat_predict",
			},
		},
	}

	if command.GetAgentExecution() == nil || command.GetAgentExecution().GetRequestEntryId() != "agent-request" {
		t.Fatal("agent execution command is not available through the public runtime contract")
	}
	if runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION == runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_ADHOC {
		t.Fatal("configured and ad-hoc agent semantics must remain distinguishable")
	}
	if command.GetAgentExecution().ProtoReflect().Descriptor().Fields().ByName("llm") != nil {
		t.Fatal("model settings must not be present on the bus command")
	}
	if command.GetAgentExecution().ProtoReflect().Descriptor().Fields().ByName("tools") != nil {
		t.Fatal("tool settings must not be present on the bus command")
	}
}
