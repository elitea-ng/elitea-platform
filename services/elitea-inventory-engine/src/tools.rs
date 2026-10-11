//! Which tools the sidecar serves, per toolkit family.
//!
//! The legacy plugin's routing table (provenance:
//! `tests/fixtures/PROVENANCE.md`): the `inventory` family (ingestion, graph
//! management, retrieval, presets, cache, status, maintenance, and — since
//! descriptor revision `legacy-v2` — graph transfer) and the
//! `inventory_search` family the agent surfaces call. The five tools the descriptor advertises and no
//! implementation ever served (`delta_update` and four never-routed ones) are
//! not here: the Go host refuses them before the socket (`DeferredTools` in
//! `internal/apps/inventory/run`). `tests/descriptor.rs` holds this table to
//! the descriptor the host serves.

/// The `inventory` family.
pub const INVENTORY_TOOLS: [&str; 29] = [
    "run_ingestion",
    "remove_source_entities",
    "list_ingested_sources",
    "list_graphs",
    "load_graph",
    "get_graph_info",
    "search_graph",
    "get_entity",
    "get_entity_content",
    "impact_analysis",
    "get_related_entities",
    "get_cross_source_relations",
    "get_stats",
    "list_entities_by_type",
    "list_entities_by_layer",
    "list_entities_by_source",
    "list_presets",
    "get_preset_info",
    "get_cache_stats",
    "cleanup_cache",
    "get_ingestion_status",
    "get_sources_status",
    "get_entities_by_ids",
    "get_entity_neighbors",
    "normalize_types",
    "rebuild_indices",
    "smart_normalize_types",
    "import_graph",
    "export_graph",
];

/// The `inventory_search` family.
pub const SEARCH_TOOLS: [&str; 6] = [
    "search_knowledge_graph",
    "get_entity_details",
    "get_related_entities",
    "query_graph",
    "list_entity_types",
    "investigate",
];

/// The `platform` family (issue #1244): the two deletions the platform
/// makes through the host's gRPC platform service. They are NOT toolkit
/// tools: the host's admission table and the descriptor name neither, so no
/// toolkit offers them to a user or an agent, and the engine's socket is the
/// host's alone. `delete_graph` deletes one toolkit's graph (the platform
/// calls it when an Inventory toolkit is deleted), `delete_project_graphs`
/// every graph of the project (project deletion).
pub const PLATFORM_TOOLS: [&str; 2] = [DELETE_GRAPH, DELETE_PROJECT_GRAPHS];

/// The family's name, as the host sends it.
pub const PLATFORM_FAMILY: &str = "platform";
/// Delete one `(project_id, application_id)` graph.
pub const DELETE_GRAPH: &str = "delete_graph";
/// Delete every graph of `project_id`.
pub const DELETE_PROJECT_GRAPHS: &str = "delete_project_graphs";

/// The tools of a family, or `None` for a family that does not exist.
#[must_use]
pub fn family(name: &str) -> Option<&'static [&'static str]> {
    match name {
        "inventory" => Some(&INVENTORY_TOOLS),
        "inventory_search" => Some(&SEARCH_TOOLS),
        PLATFORM_FAMILY => Some(&PLATFORM_TOOLS),
        _ => None,
    }
}

/// Whether any family routes `tool` — what the socket admits.
#[must_use]
pub fn serves(tool: &str) -> bool {
    INVENTORY_TOOLS.contains(&tool)
        || SEARCH_TOOLS.contains(&tool)
        || PLATFORM_TOOLS.contains(&tool)
}
