package runtimecomposition

import (
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/connectorapproval"
	indexregistryapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexregistry"
	toolkitcalltoolapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
)

// This file hands the Rust indexing runtime's index catalog (ADR-0030 I1) to
// the two freeze paths that serve index tools (agent execution and
// toolkit.call_tool.v1). Each takes an optional resolver; a nil *CatalogResolver
// must reach them as a nil INTERFACE (or no option at all), not as a typed nil
// that reads as "configured" and then fails every freeze.

// indexCatalogResolver is the agent freezer's view of the catalog resolver, or
// nil when the deployment indexes with Python.
func indexCatalogResolver(
	resolver *indexregistryapp.CatalogResolver,
) agentexecutionapp.CurrentAgentIndexCatalogResolver {
	if resolver == nil {
		return nil
	}
	return resolver
}

// indexCatalogOption is the tool-run resolver option for the catalog, or a
// no-op when the deployment indexes with Python.
func indexCatalogOption(resolver *indexregistryapp.CatalogResolver) toolkitcalltoolapp.ResolverOption {
	if resolver == nil {
		return func(*toolkitcalltoolapp.CurrentAuthoritativeInputResolver) {}
	}
	return toolkitcalltoolapp.WithIndexCatalog(resolver)
}

// toolkitConnectorOption makes the tool-run resolver pin an approved custom
// MCP server's tools (#1501). A nil gate is the open mode: no option.
func toolkitConnectorOption(gate *connectorapproval.Gate) toolkitcalltoolapp.ResolverOption {
	if gate == nil {
		return func(*toolkitcalltoolapp.CurrentAuthoritativeInputResolver) {}
	}
	return toolkitcalltoolapp.WithConnectorGate(gate)
}
