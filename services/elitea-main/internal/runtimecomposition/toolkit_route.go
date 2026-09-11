package runtimecomposition

import (
	"errors"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

type standaloneToolkitRoute struct {
	enabled                                              bool
	rust                                                 bool
	stream, consumerGroup, resourceClass, isolationClass string
	maxEntries                                           int64
}

// A worker consumes one configured command stream. Standalone toolkit commands
// follow that worker, without activating the separate indexing capability.
func configuredToolkitRoute(config Config, capability *WorkerToolkitCapability) (standaloneToolkitRoute, error) {
	route := standaloneToolkitRoute{}
	if capability != nil && capability.Implementation() == RustWorkerImplementation {
		route = standaloneToolkitRoute{enabled: config.AgentExecutionDispatchEnabled, rust: true, stream: config.AgentExecutionCommandStream,
			consumerGroup: config.AgentExecutionConsumerGroup, maxEntries: config.AgentExecutionStreamMaxEntries, resourceClass: agentResourceClass, isolationClass: agentIsolationClass}
	} else {
		route = standaloneToolkitRoute{enabled: config.IndexIngestDispatchEnabled, stream: config.IndexIngestCommandStream,
			consumerGroup: config.IndexIngestConsumerGroup, maxEntries: config.IndexIngestStreamMaxEntries, resourceClass: toolkitCallToolResourceClass, isolationClass: toolkitCallToolIsolationClass}
	}
	if config.ToolkitDiscoveryEnabled && !route.enabled {
		return standaloneToolkitRoute{}, errors.New("toolkit discovery requires the configured worker command stream")
	}
	return route, nil
}

func configuredWorkerCapabilityVersions(config Config, toolkit standaloneToolkitRoute) map[string]string {
	versions := map[string]string{executiondomain.ConfigurationValidationCapability: capabilityVersion}
	if config.IndexIngestDispatchEnabled {
		versions[executiondomain.IndexIngestCapability] = indexCapabilityVersion
	}
	if config.AgentExecutionDispatchEnabled {
		versions[executiondomain.AgentApplicationCapability] = agentCapabilityVersion
		versions[executiondomain.AgentAdhocCapability] = agentCapabilityVersion
		versions[executiondomain.ToolkitExecuteReadCapability] = agentCapabilityVersion
	}
	if toolkit.enabled {
		versions[executiondomain.ToolkitCallToolCapability] = toolkitCallToolCapabilityVersion
	}
	if config.ToolkitDiscoveryEnabled {
		versions[executiondomain.ToolkitAvailableToolsCapability] = toolkitCallToolCapabilityVersion
	}
	return versions
}
