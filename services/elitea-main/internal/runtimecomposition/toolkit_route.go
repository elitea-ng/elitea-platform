package runtimecomposition

import (
	"errors"

	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

type standaloneToolkitRoute struct {
	enabled                                         bool
	stream, consumer, resourceClass, isolationClass string
}

// A worker consumes one configured command stream. Standalone toolkit commands
// follow the Rust worker's agent stream, without activating the separate
// indexing capability. The capability is accepted for the composition root's
// symmetry; the route no longer depends on it (the Rust worker is the only one).
func configuredToolkitRoute(config Config, _ *WorkerToolkitCapability) (standaloneToolkitRoute, error) {
	route := standaloneToolkitRoute{enabled: config.AgentExecutionDispatchEnabled, stream: config.AgentExecutionCommandStream,
		consumer: consumerFor(config.AgentExecutionCommandStream), resourceClass: agentResourceClass, isolationClass: agentIsolationClass}
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
