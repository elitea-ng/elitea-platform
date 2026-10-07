//! The index rows the cluster planner reads: `repo_nodes` after Phase 3
//! and `repo_edges` after Phase 2, in row-id order.
//!
//! Python's planner opened the `.wiki.db` and ran SQL on it. Every query
//! it runs has no `ORDER BY`, so its answers come in row-id order (the
//! architectural-node scans use the partial index `idx_nodes_arch`, whose
//! order is the row id; the parity dump records the query plan). Row ids
//! are insertion order: nodes as `from_networkx` wrote the graph, edges as
//! Phase 2 re-wrote them. The planner here reads the same rows from memory:
//! the engine has them before it publishes, and the published index is
//! PostgreSQL, whose order is not the row id's.

use crate::graph::{EdgeRow, NodeRow};
use std::collections::HashMap;

/// One `repo_nodes` row, the columns the planner reads.
#[derive(Debug, Clone, PartialEq)]
pub struct IndexNode {
    pub node_id: String,
    pub rel_path: String,
    pub symbol_name: String,
    /// Lower-cased, as stored.
    pub symbol_type: String,
    pub signature: String,
    pub docstring: String,
    /// `None` is SQL `NULL`.
    pub source_text: Option<String>,
    pub is_architectural: bool,
    pub is_doc: bool,
    pub is_test: bool,
    pub macro_cluster: Option<i64>,
    pub micro_cluster: Option<i64>,
}

impl IndexNode {
    /// From a `repo_nodes` row (with its Phase 3 columns set).
    #[must_use]
    pub fn from_row(row: &NodeRow) -> Self {
        Self {
            node_id: row.node_id.clone(),
            rel_path: row.rel_path.clone(),
            symbol_name: row.symbol_name.clone(),
            symbol_type: row.symbol_type.clone(),
            signature: row.signature.clone(),
            docstring: row.docstring.clone(),
            source_text: row.source_text.clone(),
            is_architectural: row.is_architectural != 0,
            is_doc: row.is_doc != 0,
            is_test: row.is_test != 0,
            macro_cluster: row.macro_cluster,
            micro_cluster: row.micro_cluster,
        }
    }
}

/// One `repo_edges` row, by node position.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct IndexEdge {
    pub source: usize,
    pub target: usize,
    /// `weight`, NULL as 1.0 (the planner reads `weight or 1.0`, so 0.0 is
    /// 1.0 there too: see [`IndexEdge::cluster_weight`]).
    pub weight: f64,
}

impl IndexEdge {
    /// `row["weight"] or 1.0`.
    #[must_use]
    pub fn cluster_weight(&self) -> f64 {
        if self.weight == 0.0 { 1.0 } else { self.weight }
    }
}

/// The rows, in row-id order, with the lookups the planner's queries use.
#[derive(Debug, Clone, Default)]
pub struct PlannerIndex {
    nodes: Vec<IndexNode>,
    by_id: HashMap<String, usize>,
    edges: Vec<IndexEdge>,
    /// Per node: the edges whose source it is (`get_edges_from`), as
    /// positions in `edges`.
    out_edges: Vec<Vec<usize>>,
    /// `wiki_meta.repo_identifier`.
    pub repo_identifier: Option<String>,
}

impl PlannerIndex {
    /// From the rows. An edge whose endpoint is not a node is kept out
    /// (none exists: Phase 2 writes the graph's edges).
    #[must_use]
    pub fn new(nodes: Vec<IndexNode>, edges: &[EdgeRow], repo_identifier: Option<String>) -> Self {
        let mut by_id = HashMap::with_capacity(nodes.len());
        for (position, node) in nodes.iter().enumerate() {
            by_id.entry(node.node_id.clone()).or_insert(position);
        }
        let mut out_edges = vec![Vec::new(); nodes.len()];
        let mut kept = Vec::with_capacity(edges.len());
        for row in edges {
            let (Some(&source), Some(&target)) =
                (by_id.get(&row.source_id), by_id.get(&row.target_id))
            else {
                continue;
            };
            out_edges[source].push(kept.len());
            kept.push(IndexEdge {
                source,
                target,
                weight: row.weight,
            });
        }
        Self {
            nodes,
            by_id,
            edges: kept,
            out_edges,
            repo_identifier,
        }
    }

    #[must_use]
    pub fn nodes(&self) -> &[IndexNode] {
        &self.nodes
    }

    #[must_use]
    pub fn node(&self, position: usize) -> &IndexNode {
        &self.nodes[position]
    }

    #[must_use]
    pub fn position(&self, node_id: &str) -> Option<usize> {
        self.by_id.get(node_id).copied()
    }

    #[must_use]
    pub fn edges(&self) -> &[IndexEdge] {
        &self.edges
    }

    /// `len(get_edges_from(node))`: every row whose source is the node.
    #[must_use]
    pub fn out_degree(&self, position: usize) -> usize {
        self.out_edges[position].len()
    }

    /// The edges whose source is the node, in row order.
    pub fn edges_from(&self, position: usize) -> impl Iterator<Item = &IndexEdge> {
        self.out_edges[position].iter().map(|&e| &self.edges[e])
    }
}
