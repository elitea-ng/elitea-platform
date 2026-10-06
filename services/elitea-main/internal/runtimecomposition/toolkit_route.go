package runtimecomposition

import (
	"errors"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

type standaloneToolkitRoute struct {
	enabled                                         bool
	rust                                            bool
	stream, consumer, resourceClass, isolationClass string
}

// A worker consumes one configured command stream. Standalone toolkit commands
// follow that worker, without activating the separate indexing capability.
func configuredToolkitRoute(config Config, capability *WorkerToolkitCapability) (standaloneToolkitRoute, error) {
	route := standaloneToolkitRoute{}
	if capability != nil && capability.Implementation() == RustWorkerImplementation {
		route = standaloneToolkitRoute{enabled: config.AgentExecutionDispatchEnabled, rust: true, stream: config.AgentExecutionCommandStream,
			consumer: consumerFor(config.AgentExecutionCommandStream), resourceClass: agentResourceClass, isolationClass: agentIsolationClass}
	} else {
		route = standaloneToolkitRoute{enabled: config.IndexIngestDispatchEnabled, stream: config.IndexIngestCommandStream,
			consumer: consumerFor(config.IndexIngestCommandStream), resourceClass: toolkitCallToolResourceClass, isolationClass: toolkitCallToolIsolationClass}
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
