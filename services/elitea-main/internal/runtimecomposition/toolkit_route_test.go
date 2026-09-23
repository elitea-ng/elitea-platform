package runtimecomposition

import (
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestStandaloneRustToolkitsUseAgentRuntimeWithoutIndexing(t *testing.T) {
	env := validEnvironment()
	env["ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED"] = "true"
	env["ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL"] = "https://elitea-gateway"
	env["ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"] = "commands.v1.agent.execute.agents.shared.1.0"
	env["ELITEA_RUNTIME_AGENT_EXECUTION_CONSUMER_GROUP"] = "elitea-agent-worker-v1"
	env["ELITEA_RUNTIME_AGENT_EXECUTION_STREAM_MAX_ENTRIES"] = "64"
	env["ELITEA_RUNTIME_TOOLKIT_DISCOVERY_ENABLED"] = "true"
	config, err := ConfigFromEnv(mapLookup(env))
	require.NoError(t, err)
	require.False(t, config.IndexIngestDispatchEnabled)
	require.False(t, config.IndexSchedulingEnabled)
	worker, err := LoadPinnedWorkerToolkitCapability(RustWorkerImplementation)
	require.NoError(t, err)
	route, err := configuredToolkitRoute(config, worker)
	require.NoError(t, err)
	require.True(t, route.enabled)
	require.True(t, route.rust)
	require.Equal(t, config.AgentExecutionCommandStream, route.stream)
	require.Equal(t, config.AgentExecutionConsumerGroup, route.consumerGroup)
	require.Equal(t, agentResourceClass, route.resourceClass)
	versions := configuredWorkerCapabilityVersions(config, route)
	require.Equal(t, "1", versions[executiondomain.ToolkitCallToolCapability])
	require.Equal(t, "1", versions[executiondomain.ToolkitAvailableToolsCapability])
	require.NotContains(t, versions, executiondomain.IndexIngestCapability)
	// Enabling indexing later cannot redirect the selected Rust worker's tools.
	config.IndexIngestDispatchEnabled = true
	config.IndexIngestCommandStream = "index"
	config.IndexIngestConsumerGroup = "indexers"
	config.IndexIngestStreamMaxEntries = 64
	again, err := configuredToolkitRoute(config, worker)
	require.NoError(t, err)
	require.Equal(t, route, again)
}

func TestStandalonePythonToolkitsKeepIndexWorkerRoute(t *testing.T) {
	worker, err := LoadPinnedWorkerToolkitCapability(PythonWorkerImplementation)
	require.NoError(t, err)
	config := Config{ToolkitDiscoveryEnabled: true, IndexIngestDispatchEnabled: true, IndexIngestCommandStream: "index", IndexIngestConsumerGroup: "indexers", IndexIngestStreamMaxEntries: 64,
		AgentExecutionDispatchEnabled: true, AgentExecutionCommandStream: "agents", AgentExecutionConsumerGroup: "agents", AgentExecutionStreamMaxEntries: 32}
	route, err := configuredToolkitRoute(config, worker)
	require.NoError(t, err)
	require.True(t, route.enabled)
	require.False(t, route.rust)
	require.Equal(t, "index", route.stream)
	require.Equal(t, indexResourceClass, route.resourceClass)
	config.IndexIngestDispatchEnabled = false
	_, err = configuredToolkitRoute(config, worker)
	require.Error(t, err)
	config.ToolkitDiscoveryEnabled = false
	route, err = configuredToolkitRoute(config, worker)
	require.NoError(t, err)
	require.False(t, route.enabled)
}
