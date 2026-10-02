package redisdispatch

import (
	"context"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestBothStandaloneToolkitContractsPublishToRustAgentStream(t *testing.T) {
	const stream = "commands.v1.agent.execute.agents.shared.1.0"
	const group = "elitea-agent-worker-v1"
	ctx := context.Background()
	t.Run("call_tool", func(t *testing.T) {
		config := validToolkitCallToolProducerConfig()
		config.Stream = stream
		config.ConsumerGroup = group
		appended := &appenderStub{}
		producer, err := NewToolkitCallToolProducer(config, &signerStub{}, appended)
		require.NoError(t, err)
		dispatch := validToolkitCallToolDispatch()
		dispatch.ResourceClass = "agents"
		prepared, err := producer.PrepareToolkitCallTool(ctx, dispatch)
		require.NoError(t, err)
		require.NoError(t, producer.AppendPrepared(ctx, dispatch.OutboxID, prepared))
		assertToolkitAgentCommand(t, appended, stream, "toolkit.call_tool.v1")
		require.NoError(t, producer.AppendPrepared(ctx, dispatch.OutboxID, prepared))
		require.Equal(t, 2, appended.calls)
	})
	t.Run("available_tools", func(t *testing.T) {
		config := validToolkitAvailableToolsProducerConfig()
		config.Stream = stream
		config.ConsumerGroup = group
		appended := &appenderStub{}
		producer, err := NewToolkitAvailableToolsProducer(config, &signerStub{}, appended)
		require.NoError(t, err)
		dispatch := validToolkitAvailableToolsDispatch()
		dispatch.ResourceClass = "agents"
		prepared, err := producer.PrepareToolkitAvailableTools(ctx, dispatch)
		require.NoError(t, err)
		require.NoError(t, producer.AppendPrepared(ctx, dispatch.OutboxID, prepared))
		assertToolkitAgentCommand(t, appended, stream, "toolkit.available_tools.v1")
		require.NoError(t, producer.AppendPrepared(ctx, dispatch.OutboxID, prepared))
		require.Equal(t, 2, appended.calls)
	})
}

func assertToolkitAgentCommand(t *testing.T, appended *appenderStub, stream, capability string) {
	t.Helper()
	require.Equal(t, stream, appended.stream)
	var envelope runtimev1.SignedWorkerCommandEnvelopeV1
	require.NoError(t, proto.Unmarshal(appended.value, &envelope))
	var command runtimev1.WorkerCommandV1
	require.NoError(t, proto.Unmarshal(envelope.WorkerCommandBytes, &command))
	require.Equal(t, capability, command.CapabilityId)
	require.Equal(t, "agents", command.ResourceClass)
	require.Equal(t, "project", command.IsolationClass)
	require.NotNil(t, command.InputBundleRef)
}
