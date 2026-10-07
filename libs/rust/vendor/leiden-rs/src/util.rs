//! Utility functions for loading and evaluating graphs.

use crate::error::Result;
use crate::graph::GraphDataBuilder;
use crate::partition::Partition;
use crate::quality::{Modularity, QualityFunction};

/// Load an undirected unweighted graph from an edge-list string.
///
/// Input format: one edge per line as `source target` (0-indexed integer node IDs).
/// Blank lines and lines starting with `#` are skipped. Duplicate edges are
/// silently collapsed — only the first occurrence of each `(u, v)` pair is kept.
///
/// Returns a [`GraphData`](crate::graph::GraphData) with `node_count` vertices.
pub fn load_edgelist(data: &str, node_count: usize) -> Result<crate::graph::GraphData> {
    let mut builder = GraphDataBuilder::new(node_count);

    let mut seen = rustc_hash::FxHashSet::default();
    for line in data.lines() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let parts: Vec<&str> = line.split_whitespace().collect();
        if parts.len() < 2 {
            continue;
        }
        let src: usize = match parts[0].parse() {
            Ok(v) => v,
            Err(_) => continue,
        };
        let dst: usize = match parts[1].parse() {
            Ok(v) => v,
            Err(_) => continue,
        };
        if src >= node_count || dst >= node_count || src == dst {
            continue;
        }
        let key = (src.min(dst), src.max(dst));
        if seen.insert(key) {
            builder.add_edge(src, dst, 1.0)?;
        }
    }
    builder.build()
}

/// Compute the Newman-Girvan modularity (γ = 1.0) of a partition.
pub fn modularity(data: &crate::graph::GraphData, partition: &Partition) -> f64 {
    Modularity::with_resolution(1.0).total_quality(data, partition)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::graph::GraphDataBuilder;
    use crate::partition::Partition;

    #[test]
    fn test_load_edgelist_triangle() {
        let data = "0 1 1.0\n1 2 1.0\n2 0 1.0\n";
        let graph = load_edgelist(data, 3).expect("valid edge list");
        assert_eq!(graph.node_count(), 3);
        assert!(
            (graph.total_weight() - 3.0).abs() < 1e-10,
            "expected 3 edges, got {}",
            graph.total_weight()
        );
    }

    #[test]
    fn test_load_edgelist_empty() {
        let graph = load_edgelist("", 0).expect("empty input");
        assert_eq!(graph.node_count(), 0);
        assert_eq!(graph.total_weight(), 0.0);
    }

    #[test]
    fn test_load_edgelist_skips_single_column() {
        // Lines with fewer than 2 columns are skipped (parts.len() < 2).
        let graph = load_edgelist("0\n", 1).expect("valid");
        assert_eq!(
            graph.total_weight(),
            0.0,
            "single-column line should be skipped"
        );
    }

    #[test]
    fn test_load_edgelist_self_loop() {
        // Self-loops (src == dst) are skipped.
        let graph = load_edgelist("0 0 1.0\n", 1).expect("valid");
        assert_eq!(
            graph.total_weight(),
            0.0,
            "self-loop should be skipped"
        );
    }

    #[test]
    fn test_load_edgelist_comment() {
        // Lines starting with # are skipped.
        let graph = load_edgelist("# comment\n0 1 1.0\n", 2).expect("valid");
        assert_eq!(graph.node_count(), 2);
        assert!(
            (graph.total_weight() - 1.0).abs() < 1e-10,
            "expected 1 edge after skipping comment, got {}",
            graph.total_weight()
        );
    }

    #[test]
    fn test_modularity_triangle_one_community() {
        let mut builder = GraphDataBuilder::new(3);
        builder.add_edge(0, 1, 1.0).unwrap();
        builder.add_edge(1, 2, 1.0).unwrap();
        builder.add_edge(2, 0, 1.0).unwrap();
        let graph = builder.build().unwrap();
        let partition = Partition::from_membership(vec![0, 0, 0]);
        let q = modularity(&graph, &partition);
        // K3 with all nodes in one community: modularity = 0 (by definition)
        assert!((q - 0.0).abs() < 1e-10, "K3 one-community modularity is 0, got {}", q);
    }

    #[test]
    fn test_modularity_empty_graph() {
        let graph = GraphDataBuilder::new(0).build().unwrap();
        let partition = Partition::new(0);
        let q = modularity(&graph, &partition);
        assert_eq!(q, 0.0, "empty graph modularity should be 0.0");
    }
}
