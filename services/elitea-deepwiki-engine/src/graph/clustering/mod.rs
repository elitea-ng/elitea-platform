//! Phase 3: partition the weighted code graph into wiki sections and pages
//! (`graph_clustering.py`, ADR-0026 phase 4).
//!
//! The live Python path is `run_phase3` → `_run_phase3_hierarchical_leiden`:
//! `feature_flags.hierarchical_leiden` is hard-coded on, so the Louvain /
//! Infomap / architectural-projection branch is unreachable and not ported.
//! The steps, each in its own module:
//!
//! 1. ([`run_phase3`]) Test nodes are left out when `exclude_tests` is set.
//!    Under the calibrated weight profile (the default) the section
//!    resolution is `auto_resolution(distinct rel_path count)`
//!    ([`sizing`]), else 1.0.
//! 2. ([`hierarchical`]) Hubs are left out. Pass 1 contracts the graph to
//!    one vertex per file and runs Leiden on it (`γ_sec`); a file without
//!    cross-file edges joins the section whose files share its directory
//!    most. Pass 2 runs Leiden per section on the original nodes (`γ_page` =
//!    1.0).
//! 3. ([`consolidate`]) The smallest sections, then the smallest pages, are
//!    merged into their most connected sibling until the counts reach the
//!    targets ([`sizing`]).
//! 4. ([`hubs`]) Each hub joins the section, and the page in it, that most
//!    of its edges reach.
//! 5. ([`Phase3Output::assignments`]) The `macro_cluster`, `micro_cluster`,
//!    `is_hub`, `hub_assignment` columns per node.
//!
//! Every step except the Leiden calls is EXACT: the parity gate replays
//! leidenalg's memberships through [`ReplayPartitioner`] and compares the
//! assignments with Python's row for row. The Leiden calls go through
//! [`Partitioner`]; production uses [`LeidenPartitioner`] (vendored
//! `leiden-rs`), whose gate is partition quality.
//!
//! Not ported, because nothing live reads it: `_detect_page_centroids`
//! (its output stays in an in-memory dict nobody reads, "A14") and the
//! Louvain pipeline. `select_central_symbols` (`PageRank`) lives in the same
//! Python module but is called by the structure planner on a graph it
//! loads from the index; it belongs to phase 5.

pub mod consolidate;
pub mod doc_clusters;
pub mod dump;
pub mod hierarchical;
pub mod hubs;
pub mod input;
pub mod partitioner;
pub mod sizing;

pub use input::{ClusterGraph, ClusterNode, InputError};
pub use partitioner::{
    LeidenPartitioner, Level, PartitionError, PartitionRequest, Partitioner, RecordedCall,
    ReplayPartitioner,
};

use crate::graph::constants;
use indexmap::{IndexMap, IndexSet};
use serde::Serialize;

/// The Leiden seed (`seed=42` in Python).
pub const SEED: u64 = 42;

/// `LEIDEN_PAGE_RESOLUTION`.
pub const PAGE_RESOLUTION: f64 = 1.0;

/// `LEIDEN_FILE_SECTION_RESOLUTION`, the section γ under the legacy weight
/// profile.
pub const LEGACY_SECTION_RESOLUTION: f64 = 1.0;

/// The pages of one section: page id → node indices.
pub type Pages = IndexMap<usize, Vec<usize>>;

/// The feature flags Phase 3 reads (`feature_flags.py`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Phase3Flags {
    /// `exclude_tests` (`DEEPWIKI_EXCLUDE_TESTS`, default off): test nodes
    /// get no cluster.
    pub exclude_tests: bool,
    /// `weight_calibration_profile == "calibrated"` (the default): the
    /// section γ follows the file count.
    pub calibrated_weights: bool,
}

impl Default for Phase3Flags {
    fn default() -> Self {
        Self {
            exclude_tests: false,
            calibrated_weights: true,
        }
    }
}

/// The sections and pages, as the Python `leiden_result` dict: every map
/// keeps Python's insertion order, because the merge passes break ties by
/// it.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Clustering {
    /// Section id → its pages.
    pub sections: IndexMap<usize, Pages>,
    /// Node → section id (hubs are added after re-integration).
    pub macro_assignments: IndexMap<usize, usize>,
    /// Section id → node → page id.
    pub micro_assignments: IndexMap<usize, IndexMap<usize, usize>>,
    pub metadata: AlgorithmMetadata,
}

/// `algorithm_metadata`, key for key.
#[derive(Debug, Clone, Default, PartialEq, Serialize)]
pub struct AlgorithmMetadata {
    pub algorithm: &'static str,
    pub section_resolution: f64,
    pub page_resolution: f64,
    pub sections: usize,
    pub pages: usize,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub seed: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub file_nodes: Option<usize>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub connected_files: Option<usize>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub isolated_files: Option<usize>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub note: Option<&'static str>,
}

/// The algorithm name Python records.
pub const ALGORITHM: &str = "hierarchical_leiden_file_contracted";

/// The Phase 3 stats dict (`phase3_stats` in the index meta table).
#[derive(Debug, Clone, Default, PartialEq, Serialize)]
pub struct Phase3Stats {
    pub graph: GraphStats,
    #[serde(rename = "macro")]
    pub macro_: MacroStats,
    pub micro: MicroStats,
    pub algorithm_metadata: AlgorithmMetadata,
    pub hubs: HubStats,
    pub persistence: PersistenceStats,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct GraphStats {
    pub total_nodes: usize,
    pub total_edges: usize,
    pub hubs: usize,
    pub test_nodes_excluded: usize,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct MacroStats {
    pub cluster_count: usize,
    pub cluster_count_raw: usize,
    pub nodes_assigned: usize,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct MicroStats {
    pub total_pages: usize,
    pub total_pages_raw: usize,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct HubStats {
    pub total: usize,
    pub assigned_to_cluster: usize,
    pub global_core: usize,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct PersistenceStats {
    pub nodes_clustered: usize,
    pub macro_clusters: usize,
    pub micro_clusters: usize,
    pub hubs_assigned: usize,
    pub hubs_in_clusters: usize,
    pub hubs_global_core: usize,
}

/// The cluster columns of one node's `repo_nodes` row.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, serde::Deserialize)]
pub struct ClusterAssignment {
    pub node_id: String,
    pub macro_cluster: Option<usize>,
    pub micro_cluster: Option<usize>,
    pub is_hub: bool,
    /// The hub's section id as text (`str(macro_id)`), for hubs only.
    pub hub_assignment: Option<String>,
}

/// What Phase 3 produces.
#[derive(Debug, Clone, PartialEq)]
pub struct Phase3Output {
    /// After consolidation and hub re-integration.
    pub clustering: Clustering,
    /// Hub node → (section, page).
    pub hub_assignments: IndexMap<usize, (usize, Option<usize>)>,
    pub stats: Phase3Stats,
}

/// Phase 3 failed.
#[derive(Debug, Clone, thiserror::Error, PartialEq)]
pub enum ClusterError {
    #[error(transparent)]
    Partition(#[from] PartitionError),
}

/// Run Phase 3 on the Phase 2 graph.
///
/// `hubs` is what the caller hands Python's `run_phase3`. NOTE the live
/// caller (`filesystem_indexer`) passes `phase2_stats["hubs"]["node_ids"]`,
/// which `run_phase2` caps at the FIRST 20 SORTED hub ids: hubs past the
/// twentieth are clustered as ordinary nodes, and only those 20 end with
/// `is_hub = 1`. Pass the same list to get the same result. Ids that are
/// not graph nodes are ignored.
///
/// # Errors
///
/// When the partitioner fails.
pub fn run_phase3(
    graph: &ClusterGraph,
    hubs: &[String],
    flags: Phase3Flags,
    partitioner: &mut dyn Partitioner,
) -> Result<Phase3Output, ClusterError> {
    let hub_nodes: IndexSet<usize> = hubs.iter().filter_map(|id| graph.index_of(id)).collect();

    // Test exclusion: those nodes are out of G_cluster.
    let mut in_cluster = vec![true; graph.node_count()];
    let mut test_nodes_excluded = 0;
    if flags.exclude_tests {
        for (node, keep) in in_cluster.iter_mut().enumerate() {
            if constants::is_test_path(graph.path_of(node)) {
                *keep = false;
                test_nodes_excluded += 1;
            }
        }
    }

    let section_resolution = if flags.calibrated_weights {
        // Distinct non-empty rel_path (NOT file_name) over G_cluster.
        let files: IndexSet<&str> = graph
            .nodes()
            .iter()
            .zip(&in_cluster)
            .filter(|(node, keep)| **keep && !node.rel_path.is_empty())
            .map(|(node, _)| node.rel_path.as_str())
            .collect();
        let count = if files.is_empty() {
            in_cluster.iter().filter(|keep| **keep).count()
        } else {
            files.len()
        };
        sizing::auto_resolution(count)
    } else {
        LEGACY_SECTION_RESOLUTION
    };

    let mut clustering = hierarchical::hierarchical_leiden_cluster(
        graph,
        &in_cluster,
        &hub_nodes,
        section_resolution,
        PAGE_RESOLUTION,
        SEED,
        partitioner,
    )?;

    let n_files = clustering.metadata.file_nodes;
    consolidate::consolidate_sections(&mut clustering, graph, n_files);
    consolidate::consolidate_pages(&mut clustering, graph);

    let raw = (
        clustering.sections.len(),
        clustering.sections.values().map(IndexMap::len).sum(),
    );
    let hub_list: Vec<usize> = hub_nodes.iter().copied().collect();
    let hub_assignments = hubs::reintegrate_hubs(
        graph,
        &hub_list,
        &clustering.macro_assignments,
        &clustering.micro_assignments,
    );
    for (&hub, &(macro_id, micro_id)) in &hub_assignments {
        clustering.macro_assignments.insert(hub, macro_id);
        if let Some(micro_id) = micro_id {
            clustering
                .micro_assignments
                .entry(macro_id)
                .or_default()
                .insert(hub, micro_id);
        }
    }
    let stats = phase3_stats(
        &clustering,
        &hub_assignments,
        GraphStats {
            total_nodes: graph.node_count(),
            total_edges: graph.edge_count(),
            hubs: hub_nodes.len(),
            test_nodes_excluded,
        },
        raw,
    );
    Ok(Phase3Output {
        clustering,
        hub_assignments,
        stats,
    })
}

/// The stats dict. `consolidated` is (sections, pages) after
/// consolidation, before the hubs join.
fn phase3_stats(
    clustering: &Clustering,
    hub_assignments: &IndexMap<usize, (usize, Option<usize>)>,
    graph: GraphStats,
    consolidated: (usize, usize),
) -> Phase3Stats {
    let hubs_in_sections = hub_assignments.len();
    Phase3Stats {
        graph,
        macro_: MacroStats {
            cluster_count: consolidated.0,
            cluster_count_raw: clustering.metadata.sections,
            // Before the hubs joined `macro_assignments`.
            nodes_assigned: clustering.macro_assignments.len() - hubs_in_sections,
        },
        micro: MicroStats {
            total_pages: consolidated.1,
            total_pages_raw: clustering.metadata.pages,
        },
        algorithm_metadata: clustering.metadata.clone(),
        hubs: HubStats {
            total: hubs_in_sections,
            assigned_to_cluster: hubs_in_sections,
            global_core: 0,
        },
        persistence: PersistenceStats {
            // `persist_clusters` batches every assigned node, hubs
            // included, then the hubs once more.
            nodes_clustered: clustering.macro_assignments.len() + hubs_in_sections,
            macro_clusters: clustering
                .macro_assignments
                .values()
                .collect::<IndexSet<_>>()
                .len(),
            micro_clusters: clustering
                .micro_assignments
                .values()
                .map(|pages| pages.values().collect::<IndexSet<_>>().len())
                .sum(),
            hubs_assigned: hubs_in_sections,
            hubs_in_clusters: hubs_in_sections,
            hubs_global_core: 0,
        },
    }
}

impl Phase3Output {
    /// The cluster columns of every graph node, in graph order: what
    /// `persist_clusters` writes (it first resets every row to NULL / 0, so
    /// a node without a section gets `None`).
    #[must_use]
    pub fn assignments(&self, graph: &ClusterGraph) -> Vec<ClusterAssignment> {
        graph
            .nodes()
            .iter()
            .enumerate()
            .map(|(node, data)| {
                let section = self.clustering.macro_assignments.get(&node).copied();
                let page = section.and_then(|m| {
                    self.clustering
                        .micro_assignments
                        .get(&m)
                        .and_then(|pages| pages.get(&node).copied())
                });
                let hub = self.hub_assignments.get(&node);
                ClusterAssignment {
                    node_id: data.id.clone(),
                    macro_cluster: section,
                    micro_cluster: page,
                    is_hub: hub.is_some(),
                    hub_assignment: hub.map(|(macro_id, _)| macro_id.to_string()),
                }
            })
            .collect()
    }

    /// The section → page → node-id map of the persisted columns, as
    /// `UnifiedWikiDB.get_all_clusters` reads it back.
    #[must_use]
    pub fn cluster_map(
        &self,
        graph: &ClusterGraph,
    ) -> IndexMap<usize, IndexMap<usize, Vec<String>>> {
        let mut map: IndexMap<usize, IndexMap<usize, Vec<String>>> = IndexMap::new();
        for row in self.assignments(graph) {
            if let Some(macro_id) = row.macro_cluster {
                map.entry(macro_id)
                    .or_default()
                    .entry(row.micro_cluster.unwrap_or(0))
                    .or_default()
                    .push(row.node_id);
            }
        }
        map
    }
}

#[cfg(test)]
mod tests;
