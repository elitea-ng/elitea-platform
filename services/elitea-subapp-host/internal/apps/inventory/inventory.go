// Package inventory is the Inventory sub-application as the host sees it:
// its descriptor, its toolkit admission table, and whichever runner a
// deployment wires. The descriptor is the legacy-v2 document — a copy of
// conformance/provider/fixtures/inventory/descriptor/legacy-v2/
// provider_descriptor.json, pinned byte for byte by a test — with the service
// location the host is configured with written into it.
//
// It is the second application on this host, and it carries no code of its
// own: everything below is data. That is the point — ADR-0023's runner
// generalisation is falsified if a second provider needs host changes to be
// served, and the only host change this one needed was its registry entry.
//
// REVISION legacy-v1 (ADR-0023 H4c stage I3) adds four tools to the
// `inventory` family and changes nothing else. All four are implemented in the
// legacy plugin, ROUTED by it, and CALLED by the legacy UI — and none was ever
// declared in its descriptor:
//
//	get_entity_neighbors    the graph view's "expand connections" menu
//	get_entities_by_ids     the chat view's entity highlighting
//	get_ingestion_status    the sources view's run-in-flight indicator
//	smart_normalize_types   the LLM type normaliser, re-run on demand
//
// On the legacy platform they worked because the UI called the provider's own
// HTTP routes directly. Under ADR-0022/0023 the facade admits a tool only if
// the descriptor advertises it, so leaving them undeclared would have ported
// the provider and silently dropped three features of the product. legacy-v0 is
// kept beside v1 as the record of what the legacy plugin actually declared.
//
// REVISION legacy-v2 adds two tools to the `inventory` family, and corrects
// smart_normalize_types' description (no engine runs it after an ingestion):
//
//	import_graph   the toolkit bucket's graph.json (the Python engine's
//	               graph) into the native graph store
//	export_graph   the stored graph back to the bucket as graph.json
//
// They were the native engine's operator commands (`import-graph`,
// `export-graph`); declaring them lets a toolkit owner migrate a graph
// without cluster access. The host, not the engine, reads the bucket for
// import_graph (run.ResolveGraphDocument), because the host holds the
// artifact transport.
//
// The ENGINE reaches this host as a sidecar (services/elitea-inventory-engine) over a
// Unix socket; internal/apps/inventory/run is the runner in front of it. A host
// with no socket configured runs the unavailable runner (the default), so
// /descriptor and /health answer and every tool refuses in band with a reason
// a caller can read.
package inventory

import (
	_ "embed"
	"encoding/json"
	"regexp"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

//go:embed descriptor.json
var descriptorJSON []byte

// Name and Version are what /health reports.
const (
	Name    = "elitea-inventory"
	Version = "1.0.0"
)

// EnvPrefix is the settings namespace this application reads.
const EnvPrefix = "ELITEA_INVENTORY_"

// locationField is the descriptor's second key; only its value changes.
var locationField = regexp.MustCompile(`"service_location_url":\s*"[^"]*"`)

// Descriptor returns the descriptor for a service location. The document is
// emitted verbatim — key order included, which the conformance test pins —
// with the location substituted; nothing else is parsed or re-encoded.
func Descriptor(serviceLocationURL string) any {
	quoted, _ := json.Marshal(serviceLocationURL)
	return json.RawMessage(locationField.ReplaceAll(descriptorJSON, []byte(`"service_location_url": `+string(quoted))))
}

// Toolkits is the admission table: the two families the descriptor declares,
// with exactly the tools it declares for each. A test compares both lists
// against the descriptor, so a tool that is served and not advertised — or
// advertised and not served — is a failing test rather than a runtime
// surprise.
//
// ADMITTED is not the same as SERVED. Five of the names below are refused by
// the runner with a reason (run.DeferredTools): they are advertised, their
// legacy handlers exist, and the legacy router never carried them, so no
// implementation of them has ever run. They stay in this table because the
// table's job is to agree with the descriptor — dropping them here would make
// the refusal an "unknown tool" whose message says the tool does not exist,
// when what is true is that it is declared and unimplemented.
//
// No aliases beyond the declared names. The legacy plugin
// (legacy/plugins/inventory_plugin/methods/invoke.py::perform_invoke_request)
// accepted the two literals and nothing else — there were no renames to
// carry, which is why this table has none where DeepWiki's has thirteen.
//
// Both families refuse an unknown tool as invalid input: the legacy handler
// raised ValueError for one, which the classifier reads as invalid_input.
// The message is the host's ("Tool 'x' not available in inventory toolkit.
// Available: …"), not the legacy plugin's bare "Unknown tool: x" — the text
// is the host's mechanism and no fixture records Inventory's.
var Toolkits = spi.Toolkits{
	Families: []spi.Family{
		{
			Name:    "inventory",
			Aliases: []string{"inventory"},
			Tools: []string{
				"run_ingestion", "delta_update", "remove_source_entities", "list_ingested_sources",
				"list_graphs", "load_graph", "get_graph_info", "search_graph",
				"get_entity", "get_entity_content", "impact_analysis", "get_related_entities",
				"query_graph", "get_cross_source_relations", "get_stats", "list_entities_by_type",
				"list_entities_by_layer", "list_entities_by_source", "list_presets", "get_preset_info",
				"get_cache_stats", "cleanup_cache", "get_ingestion_status", "get_sources_status",
				"get_entities_by_ids", "get_entity_neighbors", "normalize_types", "rebuild_indices",
				"smart_normalize_types", "import_graph", "export_graph", "get_type_stats", "link_toolkits_to_tools",
				"connect_orphan_nodes", "validate_relationships",
			},
			UnknownToolIsInvalidInput: true,
			Label:                     "inventory",
		},
		{
			Name:    "inventory_search",
			Aliases: []string{"inventory_search"},
			Tools: []string{
				"search_knowledge_graph", "get_entity_details", "get_related_entities", "query_graph",
				"list_entity_types", "investigate",
			},
			UnknownToolIsInvalidInput: true,
			Label:                     "inventory_search",
		},
	},
	Advertised: []string{"inventory", "inventory_search"},
}

// App assembles the application over a runner.
func App(runner spi.Runner) spi.App {
	return spi.App{Name: Name, Version: Version, Descriptor: Descriptor, Toolkits: Toolkits, Runner: runner}
}
