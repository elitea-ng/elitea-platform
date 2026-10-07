//! WebAssembly bindings for the Leiden community detection algorithm.
//!
//! Enable with the `wasm` feature flag. Provides a JavaScript-friendly API
//! that takes a flat edge list and returns community assignments.

use crate::graph::GraphDataBuilder;
use crate::leiden::{Leiden, LeidenConfig};

/// Core Leiden algorithm from structured edge list (testable, non-wasm).
///
/// Builds a `GraphData` from edge tuples, runs Leiden with the given seed,
/// and returns the community assignment vector. Returns a singleton partition
/// (each node in its own community) on any error.
fn leiden_from_edgelist_core(edges: &[(usize, usize, f64)], num_nodes: usize, seed: u64) -> Vec<usize> {
    let mut builder = GraphDataBuilder::new(num_nodes);
    for &(u, v, w) in edges {
        if builder.add_edge(u, v, w).is_err() {
            return (0..num_nodes).collect();
        }
    }
    let graph_data = match builder.build() {
        Ok(g) => g,
        Err(_) => return (0..num_nodes).collect(),
    };

    let config = LeidenConfig {
        seed: Some(seed),
        ..Default::default()
    };

    match Leiden::new(config).run(&graph_data) {
        Ok(result) => (0..num_nodes)
            .map(|i| result.partition.community_of(i))
            .collect(),
        Err(_) => (0..num_nodes).collect(),
    }
}

/// Run Leiden community detection from a flat edge list.
///
/// # Arguments
/// * `edges` - Flat array of `[src0, dst0, weight0, src1, dst1, weight1, ...]`
/// * `num_nodes` - Total number of nodes (IDs must be in `0..num_nodes`)
/// * `seed` - Optional RNG seed for reproducibility
///
/// # Returns
/// Community assignment vector where `result[i]` is the community ID of node `i`.
/// Returns singleton partition (each node in its own community) on error.
#[cfg_attr(feature = "wasm", wasm_bindgen::prelude::wasm_bindgen)]
pub fn leiden_from_edgelist(edges: &[f64], num_nodes: usize, seed: Option<u64>) -> Vec<usize> {
    let edge_tuples: Vec<_> = edges
        .chunks_exact(3)
        .map(|chunk| (chunk[0] as usize, chunk[1] as usize, chunk[2]))
        .collect();
    let seed = seed.unwrap_or(0);
    leiden_from_edgelist_core(&edge_tuples, num_nodes, seed)
}

#[cfg(test)]
mod tests {
    use super::leiden_from_edgelist_core;
    use std::collections::HashSet;

    #[test]
    fn leiden_from_edgelist_core_triangle_graph() {
        // Triangle graph [(0,1,1.0),(1,2,1.0),(2,0,1.0)] → partition with ≥1 community
        let edges = [(0, 1, 1.0), (1, 2, 1.0), (2, 0, 1.0)];
        let result = leiden_from_edgelist_core(&edges, 3, 42);
        assert_eq!(result.len(), 3);
        let comms: HashSet<usize> = result.iter().copied().collect();
        assert!(comms.len() >= 1, "expected at least 1 community, got {}", comms.len());
    }

    #[test]
    fn leiden_from_edgelist_core_empty_edge_list() {
        // Empty edge list with num_nodes=3 → singleton partition (each node in own community)
        let edges: [(usize, usize, f64); 0] = [];
        let result = leiden_from_edgelist_core(&edges, 3, 42);
        assert_eq!(result.len(), 3);
        let comms: HashSet<usize> = result.iter().copied().collect();
        assert_eq!(comms.len(), 3, "each node should be in its own community");
    }

    #[test]
    fn leiden_from_edgelist_core_single_edge() {
        // Single edge [(0,1,1.0)] with num_nodes=2 → both nodes in same community
        let edges = [(0, 1, 1.0)];
        let result = leiden_from_edgelist_core(&edges, 2, 42);
        assert_eq!(result.len(), 2);
        assert_eq!(
            result[0], result[1],
            "both nodes should be in the same community"
        );
    }

    #[test]
    fn leiden_from_edgelist_core_disconnected() {
        // Disconnected components [(0,1,1.0),(2,3,1.0)] with num_nodes=4 → at least 2 communities
        let edges = [(0, 1, 1.0), (2, 3, 1.0)];
        let result = leiden_from_edgelist_core(&edges, 4, 42);
        assert_eq!(result.len(), 4);
        assert_eq!(result[0], result[1], "nodes 0,1 should be together");
        assert_eq!(result[2], result[3], "nodes 2,3 should be together");
        assert_ne!(result[0], result[2], "the two components should be in different communities");
    }
}
