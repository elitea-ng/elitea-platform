//! Phase 2: edge weights, hubs and synthetic edges
//! (`graph_topology.run_phase2`).
//!
//! Phase 2 reshapes the Phase 1c graph so that Phase 3's clustering finds
//! capability-shaped communities instead of hub-dominated ones. In order:
//!
//! 1. **Orphan resolution** ([`cascade`]): every node nothing points at
//!    gets synthetic edges, by a four-pass cascade — explicit references
//!    (markdown links, backtick names, imports), hybrid FTS + vector RRF,
//!    tiered lexical T1–T4 behind an IDF gate, then directory proximity.
//! 2. **Doc edges** ([`docs`]): hyperlink and directory-proximity edges
//!    from documentation nodes.
//! 3. **Component bridging** ([`bridge`]): a bridge edge pair from each
//!    smaller weakly connected component to the most directory-similar
//!    larger one.
//! 4. **Weights** ([`weights`]): `1 / ln(structural_in_degree + 2)` on
//!    every edge, synthetic edges floored per class.
//! 5. **Hubs**: nodes whose in-degree z-score exceeds the threshold.
//! 6. **Persist**: the index's edges are replaced by the graph's.
//!
//! Every index access goes through [`store::TopologyStore`], so the same
//! algorithm runs against the recorded Python answers (the parity gate,
//! [`replay`]) and against the `PostgreSQL` build space.
//!
//! # Fidelity
//!
//! With the index answering as Python's did, the rows (edges with their
//! weights and classes, `is_hub`) and the stats dict are byte-identical to
//! Python's. Python's quirks are kept and named where they are reproduced.
//! Three things are not reproduced:
//!
//! * **Modes B and C** of `resolve_orphans` (flat or tiered lexical, then
//!   batched vector search) run only when `orphan_cascade_v2` is off, and
//!   Python hard-codes it ON (the code calls them a kill-switch fallback).
//!   `DEEPWIKI_VEC_PREFIX_DEPTH` and `DEEPWIKI_VEC_CONCURRENCY` are read
//!   only by them, so neither variable has an effect here.
//! * **Storage failures fail the phase.** Python caught them per pass,
//!   logged at debug level, and published a poorer graph.
//! * **Set order is id order.** Component bridging takes each component's
//!   highest-degree node with `max()` over a Python `set`, so a tie went
//!   to whichever node the hash seed put first. Here a tie goes to the
//!   first node in id order (the parity reference sorts the same way).
//!
//! The per-edge `upsert_edge` writes Python made for doc and directory
//! edges are not made either: `persist_weights_to_db` deletes and rewrites
//! every edge at the end, and nothing reads edges from the index in
//! between, so they had no observable effect.
//!
//! # What Phase 3 consumes
//!
//! The graph itself (every edge's `weight` and `edge_class`, the synthetic
//! edges, and node `symbol_type` — Phase 2 re-types generic REST classes
//! to `rest_endpoint` in the GRAPH only, see [`Phase2Outcome::retyped`])
//! and the hub set. Python's indexer hands Phase 3
//! `set(stats["hubs"]["node_ids"])`, which `run_phase2` caps at the first
//! 20 hubs in id order: [`Phase2Outcome::hubs_for_phase3`] is that list;
//! [`Phase2Outcome::hubs`] is the full set the index flags.

pub mod bridge;
pub mod cascade;
pub mod digest;
pub mod docs;
pub mod hybrid;
pub mod lexical;
pub mod pynum;
pub mod replay;
pub mod store;
pub mod weights;

use super::{CodeGraph, EdgeData, Label};
use serde_json::{Map, Value, json};
use std::collections::HashMap;
pub use store::{SearchHit, StoreError, StoredNode, TextEmbedder, TopologyStore};

/// The variable that selects the weight calibration profile.
pub const CALIBRATION_PROFILE_ENV: &str = "DEEPWIKI_WEIGHT_CALIBRATION_PROFILE";

/// How many hub ids the stats dict lists (and Python's indexer passes on).
pub const HUB_IDS_IN_STATS: usize = 20;

/// `feature_flags.weight_calibration_profile`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum CalibrationProfile {
    /// Orphans are nodes with in-degree 0 and out-degree 0; every
    /// synthetic edge is floored at 0.5.
    Legacy,
    /// Orphans are nodes with in-degree 0; synthetic edges are floored per
    /// class, lifted to their `raw_similarity`. Python's default.
    #[default]
    Calibrated,
}

impl CalibrationProfile {
    /// The name Python stores in the stats dict.
    #[must_use]
    pub fn name(self) -> &'static str {
        match self {
            Self::Legacy => "legacy",
            Self::Calibrated => "calibrated",
        }
    }

    /// Read the profile through `lookup` (the environment in production).
    ///
    /// Python's `_env_choice` falls back to the default for any value it
    /// does not know, so a typo silently selects `calibrated`. Here an
    /// unknown value is an error, as `flags.rs` treats booleans; empty or
    /// unset is the default; case and surrounding blanks are ignored.
    ///
    /// # Errors
    ///
    /// The offending value, for anything but `legacy` or `calibrated`.
    pub fn from_lookup(lookup: impl Fn(&str) -> Option<String>) -> Result<Self, String> {
        let value = lookup(CALIBRATION_PROFILE_ENV).unwrap_or_default();
        match value.trim().to_lowercase().as_str() {
            "" | "calibrated" => Ok(Self::Calibrated),
            "legacy" => Ok(Self::Legacy),
            _ => Err(format!(
                "{CALIBRATION_PROFILE_ENV} must be legacy or calibrated, got '{value}'"
            )),
        }
    }

    /// Read the profile from the process environment.
    ///
    /// # Errors
    ///
    /// See [`CalibrationProfile::from_lookup`].
    pub fn from_env() -> Result<Self, String> {
        Self::from_lookup(|name| std::env::var(name).ok())
    }
}

/// `run_phase2`'s parameters.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Phase2Config {
    /// Hub z-score threshold (Python default 3.0).
    pub z_threshold: f64,
    /// `vec_distance_threshold` (default 0.15). Accepted for the same
    /// signature, but only the dead Modes B and C read it: the hybrid pass
    /// does not threshold distances.
    pub vec_distance_threshold: f64,
    pub profile: CalibrationProfile,
}

impl Default for Phase2Config {
    fn default() -> Self {
        Self {
            z_threshold: 3.0,
            vec_distance_threshold: 0.15,
            profile: CalibrationProfile::Calibrated,
        }
    }
}

/// What [`run_phase2`] produced besides the graph changes.
#[derive(Debug, Clone, PartialEq)]
pub struct Phase2Outcome {
    /// The dict `run_phase2` returns (and stores as `phase2_stats`), in
    /// Python's key order.
    pub stats: Value,
    /// Every hub, in id order (the nodes the index flags `is_hub`).
    pub hubs: Vec<String>,
    /// Nodes re-typed `rest_endpoint` by the tiered lexical pass, with the
    /// `symbol_type` they had. Python changes the GRAPH attribute only; the
    /// index row keeps the old type. Phase 3 reads the graph's.
    pub retyped: Vec<(String, Label)>,
}

impl Phase2Outcome {
    /// The hub set Python's indexer passes to Phase 3:
    /// `set(stats["hubs"]["node_ids"])`, the first 20 hubs in id order.
    #[must_use]
    pub fn hubs_for_phase3(&self) -> &[String] {
        &self.hubs[..self.hubs.len().min(HUB_IDS_IN_STATS)]
    }
}

/// The state the passes share: the graph, the index, and the stored rows
/// read so far (they do not change during the phase).
pub struct Ctx<'a> {
    pub graph: &'a mut CodeGraph,
    pub store: &'a mut dyn TopologyStore,
    pub embedder: Option<&'a mut dyn TextEmbedder>,
    pub profile: CalibrationProfile,
    rows: HashMap<String, Option<StoredNode>>,
    retyped: Vec<(String, Label)>,
}

impl<'a> Ctx<'a> {
    pub fn new(
        graph: &'a mut CodeGraph,
        store: &'a mut dyn TopologyStore,
        embedder: Option<&'a mut dyn TextEmbedder>,
        profile: CalibrationProfile,
    ) -> Self {
        Self {
            graph,
            store,
            embedder,
            profile,
            rows: HashMap::new(),
            retyped: Vec::new(),
        }
    }

    /// Read the stored rows of `ids` that are not cached yet.
    ///
    /// # Errors
    ///
    /// The store's.
    pub fn fetch_rows(&mut self, ids: &[&str]) -> Result<(), StoreError> {
        let missing: Vec<&str> = ids
            .iter()
            .copied()
            .filter(|id| !self.rows.contains_key(*id))
            .collect();
        if missing.is_empty() {
            return Ok(());
        }
        let rows = self.store.get_nodes(&missing)?;
        if rows.len() != missing.len() {
            return Err(StoreError::new(format!(
                "get_nodes returned {} rows for {} ids",
                rows.len(),
                missing.len()
            )));
        }
        for (id, row) in missing.into_iter().zip(rows) {
            self.rows.insert(id.to_owned(), row);
        }
        Ok(())
    }

    /// `db.get_node(id)`, from the rows fetched so far.
    #[must_use]
    pub fn row(&self, id: &str) -> Option<&StoredNode> {
        self.rows.get(id).and_then(Option::as_ref)
    }

    /// Remember that the graph's `symbol_type` of `id` was changed.
    pub fn note_retyped(&mut self, id: &str, previous: Label) {
        self.retyped.push((id.to_owned(), previous));
    }
}

/// `_add_edge(..., skip_db=True)`: a synthetic edge on the graph (see the
/// module docs for the `skip_db=False` callers).
pub(crate) fn synthetic_edge(
    rel_type: &'static str,
    edge_class: &'static str,
    created_by: &'static str,
    raw_similarity: Option<f64>,
    provenance: Option<Value>,
) -> EdgeData {
    let mut data = EdgeData {
        rel_type: Label::Borrowed(rel_type),
        edge_class: Label::Borrowed(edge_class),
        created_by: Label::Borrowed(created_by),
        raw_similarity,
        ..EdgeData::default()
    };
    if let Some(provenance) = provenance {
        data.extra.insert("provenance", provenance);
    }
    data
}

/// In- and out-degree of every node (parallel edges and self-loops
/// counted, as `G.in_degree` / `G.out_degree` count them).
#[must_use]
pub fn degrees(graph: &CodeGraph) -> HashMap<&str, (usize, usize)> {
    let mut table: HashMap<&str, (usize, usize)> =
        graph.nodes().map(|(id, _)| (id, (0, 0))).collect();
    for edge in graph.edges() {
        if let Some(entry) = table.get_mut(edge.source) {
            entry.1 += 1;
        }
        if let Some(entry) = table.get_mut(edge.target) {
            entry.0 += 1;
        }
    }
    table
}

/// `find_orphans`: nodes nothing points at (`calibrated`), or with no edge
/// at all (`legacy`, or `strict`), in node order.
#[must_use]
pub fn find_orphans(graph: &CodeGraph, profile: CalibrationProfile, strict: bool) -> Vec<String> {
    let table = degrees(graph);
    let isolated_only = strict || profile == CalibrationProfile::Legacy;
    graph
        .nodes()
        .filter(|(id, _)| {
            let (inbound, outbound) = table.get(id).copied().unwrap_or_default();
            inbound == 0 && (!isolated_only || outbound == 0)
        })
        .map(|(id, _)| id.to_owned())
        .collect()
}

/// `run_phase2(db, G, embedding_fn, z_threshold, vec_distance_threshold)`.
///
/// # Errors
///
/// A [`StoreError`] when the index fails; the graph may then hold part of
/// the phase's edges. An embedder failure only costs that orphan its
/// vector (a warning is logged), as in Python.
pub fn run_phase2<'a>(
    graph: &'a mut CodeGraph,
    store: &'a mut dyn TopologyStore,
    embedder: Option<&'a mut dyn TextEmbedder>,
    config: &Phase2Config,
) -> Result<Phase2Outcome, StoreError> {
    let mut ctx = Ctx::new(graph, store, embedder, config.profile);
    let orphan_resolution = cascade::resolve_orphans(&mut ctx)?;
    let doc_edges = docs::inject_doc_edges(&mut ctx)?;
    let component_bridging = bridge::bridge_disconnected_components(ctx.graph);
    let weighting = weights::apply_edge_weights(ctx.graph, ctx.profile);
    let hubs = weights::detect_hubs(ctx.graph, config.z_threshold);
    let hub_refs: Vec<&str> = hubs.iter().map(String::as_str).collect();
    if !hub_refs.is_empty() {
        ctx.store.set_hubs(&hub_refs)?;
    }
    let capped: Vec<&str> = hub_refs.iter().copied().take(HUB_IDS_IN_STATS).collect();
    let edges_persisted = persist_weights(ctx.graph, ctx.store)?;

    let mut stats = Map::new();
    stats.insert("orphan_resolution".to_owned(), orphan_resolution);
    stats.insert("doc_edges".to_owned(), doc_edges);
    stats.insert("component_bridging".to_owned(), component_bridging);
    stats.insert("weighting".to_owned(), weighting);
    stats.insert(
        "hubs".to_owned(),
        json!({"count": hubs.len(), "node_ids": capped}),
    );
    stats.insert("edges_persisted".to_owned(), json!(edges_persisted));
    let stats = Value::Object(stats);
    ctx.store.set_meta("phase2_completed", &Value::Bool(true))?;
    ctx.store.set_meta("phase2_stats", &stats)?;
    let retyped = std::mem::take(&mut ctx.retyped);
    Ok(Phase2Outcome {
        stats,
        hubs,
        retyped,
    })
}

/// `persist_weights_to_db`: nothing for an edgeless graph (Python returns
/// 0 before deleting anything), else every edge in graph order.
fn persist_weights(graph: &CodeGraph, store: &mut dyn TopologyStore) -> Result<u64, StoreError> {
    if graph.edge_count() == 0 {
        return Ok(0);
    }
    let mut rows = graph.edges().map(super::edge_row);
    store.replace_edges(&mut rows)
}

#[cfg(test)]
#[allow(clippy::float_cmp)] // bit-exact parity is the point
mod tests;
