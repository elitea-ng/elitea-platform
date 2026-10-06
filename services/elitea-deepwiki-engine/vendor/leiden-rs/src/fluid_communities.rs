//! Fluid Communities algorithm for community detection.
//!
//! An agglomerative community detection algorithm based on the idea of
//! fluids propagating through a network. Each community acts like a fluid
//! that competes for nodes, with density inversely proportional to
//! community size — smaller communities exert stronger pull than larger
//! ones, which helps maintain balanced community sizes.
//!
//! Uses asynchronous (sequential) node updates within each iteration:
//! each node immediately sees the updated community assignments of
//! previously-processed nodes in the same iteration.
//!
//! # Example
//!
//! ```
//! use leiden_rs::{GraphDataBuilder, fluid_communities::{FluidCommunities, FluidCommunitiesConfig}};
//!
//! let mut b = GraphDataBuilder::new(6);
//! // Community A: 0-2
//! b.add_edge(0, 1, 1.0).unwrap();
//! b.add_edge(1, 2, 1.0).unwrap();
//! b.add_edge(0, 2, 1.0).unwrap();
//! // Community B: 3-5
//! b.add_edge(3, 4, 1.0).unwrap();
//! b.add_edge(4, 5, 1.0).unwrap();
//! b.add_edge(3, 5, 1.0).unwrap();
//! // Bridge
//! b.add_edge(2, 3, 1.0).unwrap();
//! let graph = b.build().unwrap();
//!
//! let fc = FluidCommunities::new(FluidCommunitiesConfig::new(2, Some(42), 100));
//! let result = fc.run(&graph).unwrap();
//! assert_eq!(result.partition.num_communities(), 2);
//! ```

use std::collections::VecDeque;

use rand::rngs::StdRng;
use rand::seq::SliceRandom;
use rand::Rng;
use rand::SeedableRng;
use rustc_hash::FxHashMap;

use crate::error::{LeidenError, Result};
use crate::graph::GraphData;
use crate::partition::Partition;

/// Configuration for the Fluid Communities algorithm.
#[derive(Debug, Clone, PartialEq)]
pub struct FluidCommunitiesConfig {
    /// Number of communities to detect (required).
    pub k: usize,
    /// Optional RNG seed for reproducible results.
    pub seed: Option<u64>,
    /// Maximum number of iterations (default: 100).
    pub max_iterations: usize,
}

impl FluidCommunitiesConfig {
    /// Create a new configuration with the given number of communities, seed, and max iterations.
    #[must_use = "constructor returns a new instance"]
    pub fn new(k: usize, seed: Option<u64>, max_iterations: usize) -> Self {
        Self {
            k,
            seed,
            max_iterations,
        }
    }
}

/// Result of running the Fluid Communities algorithm.
#[derive(Debug, Clone, PartialEq)]
pub struct FluidCommunitiesOutput {
    /// The detected community partition.
    pub partition: Partition,
    /// Number of iterations performed.
    pub iterations: usize,
    /// Whether the algorithm converged (no node moves in the final iteration).
    pub converged: bool,
}

/// The Fluid Communities community detection algorithm.
///
/// Each community is a "fluid" that spreads through the network. The density
/// of a community is `1.0 / community_size`, so smaller communities have
/// higher density and exert stronger pull on neighboring nodes.
#[derive(Debug, Clone)]
pub struct FluidCommunities {
    config: FluidCommunitiesConfig,
}

impl FluidCommunities {
    /// Create a new Fluid Communities instance with the given configuration.
    #[must_use = "constructor returns a new instance"]
    pub fn new(config: FluidCommunitiesConfig) -> Self {
        Self { config }
    }

    /// Run the Fluid Communities algorithm on the given graph.
    ///
    /// # Errors
    ///
    /// Returns [`LeidenError::InvalidParameter`] if:
    /// - The graph is empty
    /// - `k` is 0
    /// - `k` exceeds the number of nodes
    /// - The graph is not connected
    pub fn run(&self, graph: &GraphData) -> Result<FluidCommunitiesOutput> {
        let n = graph.node_count();
        let k = self.config.k;

        if n == 0 {
            return Err(LeidenError::InvalidParameter {
                message: "graph must have at least one node".to_string(),
            });
        }

        if k == 0 {
            return Err(LeidenError::InvalidParameter {
                message: "k must be at least 1".to_string(),
            });
        }

        if k > n {
            return Err(LeidenError::InvalidParameter {
                message: format!("k ({k}) cannot exceed node count ({n})"),
            });
        }

        if !is_connected(graph, n) {
            return Err(LeidenError::InvalidParameter {
                message: "graph must be connected".to_string(),
            });
        }

        if n == 1 {
            return Ok(FluidCommunitiesOutput {
                partition: Partition::new(1),
                iterations: 0,
                converged: true,
            });
        }

        let mut rng: StdRng = match self.config.seed {
            Some(s) => StdRng::seed_from_u64(s),
            None => StdRng::from_rng(&mut rand::rng()),
        };

        let mut community: Vec<usize> = vec![0; n];
        let mut community_sizes: Vec<usize> = vec![0; k];

        let mut all_nodes: Vec<usize> = (0..n).collect();
        all_nodes.shuffle(&mut rng);

        for (comm_id, &node) in all_nodes[..k].iter().enumerate() {
            community[node] = comm_id;
            community_sizes[comm_id] = 1;
        }

        for &node in &all_nodes[k..] {
            let comm = rng.random_range(..k);
            community[node] = comm;
            community_sizes[comm] += 1;
        }

        let mut density: Vec<f64> = community_sizes
            .iter()
            .map(|&s| {
                if s == 0 {
                    0.0
                } else {
                    1.0 / s as f64
                }
            })
            .collect();

        let mut partition = Partition::from_membership(community.clone());

        let mut iterations = 0;
        let mut converged = false;

        for _ in 0..self.config.max_iterations {
            iterations += 1;
            let mut any_changed = false;

            let mut order: Vec<usize> = (0..n).collect();
            order.shuffle(&mut rng);

            for &node in &order {
                let mut scores: FxHashMap<usize, f64> = FxHashMap::default();

                let current_comm = community[node];
                *scores.entry(current_comm).or_insert(0.0) += density[current_comm];

                for (neighbor, _weight) in graph.neighbors(node) {
                    let neighbor_comm = community[neighbor];
                    *scores.entry(neighbor_comm).or_insert(0.0) += density[neighbor_comm];
                }

                let max_score = scores
                    .values()
                    .copied()
                    .fold(f64::NEG_INFINITY, f64::max);

                let current_score = scores.get(&current_comm).copied().unwrap_or(0.0);
                if (current_score - max_score).abs() < 1e-12 {
                    continue;
                }

                let best_comms: Vec<usize> = scores
                    .iter()
                    .filter(|&(_, &score)| (score - max_score).abs() < 1e-12)
                    .map(|(&comm, _)| comm)
                    .collect();

                let new_comm = best_comms[rng.random_range(..best_comms.len())];

                // Move node
                let old_comm = community[node];
                if new_comm != old_comm {
                    community_sizes[old_comm] -= 1;
                    community_sizes[new_comm] += 1;

                    density[old_comm] = if community_sizes[old_comm] == 0 {
                        0.0
                    } else {
                        1.0 / community_sizes[old_comm] as f64
                    };
                    density[new_comm] = 1.0 / community_sizes[new_comm] as f64;

                    community[node] = new_comm;
                    partition.move_node(node, new_comm);

                    any_changed = true;
                }
            }

            if !any_changed {
                converged = true;
                break;
            }
        }

        partition.renumber();

        Ok(FluidCommunitiesOutput {
            partition,
            iterations,
            converged,
        })
    }
}

/// Check if the graph is connected using BFS from node 0.
fn is_connected(graph: &GraphData, n: usize) -> bool {
    if n == 0 {
        return true;
    }

    let mut visited = vec![false; n];
    let mut queue = VecDeque::with_capacity(n);
    visited[0] = true;
    queue.push_back(0);
    let mut count = 1;

    while let Some(node) = queue.pop_front() {
        for (neighbor, _) in graph.neighbors(node) {
            if !visited[neighbor] {
                visited[neighbor] = true;
                count += 1;
                queue.push_back(neighbor);
            }
        }
    }

    count == n
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::graph::GraphDataBuilder;
    use crate::metrics::nmi;

    // ── Config tests ──

    #[test]
    fn test_fluid_config_default() {
        let cfg = FluidCommunitiesConfig::new(3, None, 100);
        assert_eq!(cfg.k, 3);
        assert_eq!(cfg.seed, None);
        assert_eq!(cfg.max_iterations, 100);
    }

    #[test]
    fn test_fluid_config_custom() {
        let cfg = FluidCommunitiesConfig::new(5, Some(42), 200);
        assert_eq!(cfg.k, 5);
        assert_eq!(cfg.seed, Some(42));
        assert_eq!(cfg.max_iterations, 200);
    }

    // ── Output struct tests ──

    #[test]
    fn test_fluid_output_fields() {
        let partition = Partition::from_membership(vec![0, 0, 1, 1]);
        let output = FluidCommunitiesOutput {
            partition,
            iterations: 7,
            converged: true,
        };
        assert_eq!(output.iterations, 7);
        assert!(output.converged);
        assert_eq!(output.partition.community_of(0), 0);
        assert_eq!(output.partition.community_of(2), 1);
    }

    // ── Basic graph tests ──

    #[test]
    fn test_fluid_basic() {
        // Two planted communities with dense intra-community edges and a bridge
        let mut b = GraphDataBuilder::new(10);
        // Community A: nodes 0-4 (complete graph)
        for i in 0..5 {
            for j in (i + 1)..5 {
                b.add_edge(i, j, 1.0).unwrap();
            }
        }
        // Community B: nodes 5-9 (complete graph)
        for i in 5..10 {
            for j in (i + 1)..10 {
                b.add_edge(i, j, 1.0).unwrap();
            }
        }
        // Bridge
        b.add_edge(4, 5, 1.0).unwrap();
        let graph = b.build().unwrap();

        let fc = FluidCommunities::new(FluidCommunitiesConfig::new(2, Some(42), 100));
        let result = fc.run(&graph).unwrap();

        // Should detect 2 communities
        assert_eq!(result.partition.num_communities(), 2);

        // NMI against ground truth should be high
        let ground_truth: Vec<usize> = vec![0, 0, 0, 0, 0, 1, 1, 1, 1, 1];
        let score = nmi(&ground_truth, result.partition.as_slice());
        assert!(score > 0.8, "NMI = {score}, expected > 0.8");
    }

    #[test]
    fn test_fluid_deterministic() {
        let mut b = GraphDataBuilder::new(10);
        // Ring graph
        for i in 0..10 {
            b.add_edge(i, (i + 1) % 10, 1.0).unwrap();
        }
        let graph = b.build().unwrap();

        let cfg = FluidCommunitiesConfig::new(3, Some(123), 100);
        let r1 = FluidCommunities::new(cfg.clone()).run(&graph).unwrap();
        let r2 = FluidCommunities::new(cfg).run(&graph).unwrap();

        // Same seed → identical result
        assert_eq!(r1.partition.as_slice(), r2.partition.as_slice());
        assert_eq!(r1.iterations, r2.iterations);
        assert_eq!(r1.converged, r2.converged);
    }

    #[test]
    fn test_fluid_k_one() {
        // k=1 → all nodes in one community
        let mut b = GraphDataBuilder::new(5);
        for i in 0..4 {
            b.add_edge(i, i + 1, 1.0).unwrap();
        }
        let graph = b.build().unwrap();

        let fc = FluidCommunities::new(FluidCommunitiesConfig::new(1, Some(42), 100));
        let result = fc.run(&graph).unwrap();

        assert_eq!(result.partition.num_communities(), 1);
        for i in 0..5 {
            assert_eq!(result.partition.community_of(i), 0);
        }
    }

    #[test]
    fn test_fluid_k_equals_n() {
        // k=n → each node in its own community
        let mut b = GraphDataBuilder::new(4);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(2, 3, 1.0).unwrap();
        b.add_edge(3, 0, 1.0).unwrap();
        let graph = b.build().unwrap();

        let fc = FluidCommunities::new(FluidCommunitiesConfig::new(4, Some(42), 100));
        let result = fc.run(&graph).unwrap();

        // With k=n, each seed node gets its own community and density is very high,
        // so each node should stay in its own community
        assert_eq!(result.partition.num_communities(), 4);
    }

    #[test]
    fn test_fluid_disconnected_rejected() {
        // Two disconnected components
        let mut b = GraphDataBuilder::new(6);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(0, 2, 1.0).unwrap();
        b.add_edge(3, 4, 1.0).unwrap();
        b.add_edge(4, 5, 1.0).unwrap();
        b.add_edge(3, 5, 1.0).unwrap();
        let graph = b.build().unwrap();

        let fc = FluidCommunities::new(FluidCommunitiesConfig::new(2, Some(42), 100));
        let result = fc.run(&graph);

        assert!(result.is_err());
        let err = result.unwrap_err();
        match err {
            LeidenError::InvalidParameter { message } => {
                assert!(
                    message.contains("connected"),
                    "Expected 'connected' in error message, got: {message}"
                );
            }
            _ => panic!("Expected InvalidParameter error, got: {err:?}"),
        }
    }

    #[test]
    fn test_fluid_k_greater_than_n() {
        let mut b = GraphDataBuilder::new(3);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(0, 2, 1.0).unwrap();
        let graph = b.build().unwrap();

        let fc = FluidCommunities::new(FluidCommunitiesConfig::new(5, Some(42), 100));
        let result = fc.run(&graph);

        assert!(result.is_err());
        let err = result.unwrap_err();
        match err {
            LeidenError::InvalidParameter { message } => {
                assert!(
                    message.contains("cannot exceed"),
                    "Expected 'cannot exceed' in error, got: {message}"
                );
            }
            _ => panic!("Expected InvalidParameter error, got: {err:?}"),
        }
    }

    #[test]
    fn test_fluid_k_zero() {
        let mut b = GraphDataBuilder::new(3);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        let graph = b.build().unwrap();

        let fc = FluidCommunities::new(FluidCommunitiesConfig::new(0, Some(42), 100));
        let result = fc.run(&graph);

        assert!(result.is_err());
        match result.unwrap_err() {
            LeidenError::InvalidParameter { message } => {
                assert!(
                    message.contains("at least 1"),
                    "Expected 'at least 1' in error, got: {message}"
                );
            }
            _ => panic!("Expected InvalidParameter error"),
        }
    }

    #[test]
    fn test_fluid_empty_graph() {
        let graph = GraphDataBuilder::new(0).build().unwrap();
        let fc = FluidCommunities::new(FluidCommunitiesConfig::new(1, None, 100));
        let result = fc.run(&graph);

        assert!(result.is_err());
        match result.unwrap_err() {
            LeidenError::InvalidParameter { message } => {
                assert!(
                    message.contains("at least one node"),
                    "Expected 'at least one node' in error, got: {message}"
                );
            }
            _ => panic!("Expected InvalidParameter error"),
        }
    }

    #[test]
    fn test_fluid_single_node() {
        let graph = GraphDataBuilder::new(1).build().unwrap();
        let fc = FluidCommunities::new(FluidCommunitiesConfig::new(1, Some(42), 100));
        let result = fc.run(&graph).unwrap();

        assert_eq!(result.partition.num_communities(), 1);
        assert_eq!(result.iterations, 0);
        assert!(result.converged);
    }

    #[test]
    fn test_fluid_triangle() {
        let mut b = GraphDataBuilder::new(3);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(0, 2, 1.0).unwrap();
        let graph = b.build().unwrap();

        let fc = FluidCommunities::new(FluidCommunitiesConfig::new(2, Some(42), 100));
        let result = fc.run(&graph).unwrap();

        assert_eq!(result.partition.num_communities(), 2);
        assert!(result.iterations <= 100);
    }

    #[test]
    fn test_fluid_complete_graph() {
        let n = 6;
        let mut b = GraphDataBuilder::new(n);
        for i in 0..n {
            for j in (i + 1)..n {
                b.add_edge(i, j, 1.0).unwrap();
            }
        }
        let graph = b.build().unwrap();

        let fc = FluidCommunities::new(FluidCommunitiesConfig::new(3, Some(42), 100));
        let result = fc.run(&graph).unwrap();

        assert_eq!(result.partition.num_communities(), 3);
        assert!(result.iterations <= 100);
    }

    #[test]
    fn test_fluid_max_iterations_limit() {
        let mut b = GraphDataBuilder::new(4);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(2, 3, 1.0).unwrap();
        b.add_edge(3, 0, 1.0).unwrap();
        let graph = b.build().unwrap();

        let fc = FluidCommunities::new(FluidCommunitiesConfig::new(2, Some(42), 3));
        let result = fc.run(&graph).unwrap();

        assert!(result.iterations <= 3);
    }

    #[test]
    fn test_fluid_path_graph() {
        let mut b = GraphDataBuilder::new(5);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(2, 3, 1.0).unwrap();
        b.add_edge(3, 4, 1.0).unwrap();
        let graph = b.build().unwrap();

        let fc = FluidCommunities::new(FluidCommunitiesConfig::new(2, Some(42), 100));
        let result = fc.run(&graph).unwrap();

        assert!(result.partition.num_communities() <= 2);
        assert!(result.iterations <= 100);
    }

    #[test]
    fn test_fluid_star_graph() {
        let mut b = GraphDataBuilder::new(6);
        for i in 1..6 {
            b.add_edge(0, i, 1.0).unwrap();
        }
        let graph = b.build().unwrap();

        let fc = FluidCommunities::new(FluidCommunitiesConfig::new(2, Some(42), 100));
        let result = fc.run(&graph).unwrap();

        assert!(result.partition.num_communities() <= 2);
        assert!(result.iterations <= 100);
    }

    #[test]
    fn test_fluid_partition_integrity() {
        let mut b = GraphDataBuilder::new(8);
        // Two squares connected by a bridge
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(2, 3, 1.0).unwrap();
        b.add_edge(3, 0, 1.0).unwrap();
        b.add_edge(4, 5, 1.0).unwrap();
        b.add_edge(5, 6, 1.0).unwrap();
        b.add_edge(6, 7, 1.0).unwrap();
        b.add_edge(7, 4, 1.0).unwrap();
        b.add_edge(3, 4, 1.0).unwrap(); // bridge
        let graph = b.build().unwrap();

        let fc = FluidCommunities::new(FluidCommunitiesConfig::new(2, Some(42), 100));
        let result = fc.run(&graph).unwrap();

        // Every node must have a valid community
        for i in 0..8 {
            let comm = result.partition.community_of(i);
            assert!(comm < 2, "Community {comm} out of range for node {i}");
        }
    }

    #[test]
    fn test_fluid_different_seeds_may_differ() {
        let mut b = GraphDataBuilder::new(10);
        for i in 0..9 {
            b.add_edge(i, i + 1, 1.0).unwrap();
        }
        b.add_edge(0, 9, 1.0).unwrap();
        let graph = b.build().unwrap();

        let r1 =
            FluidCommunities::new(FluidCommunitiesConfig::new(3, Some(1), 100)).run(&graph).unwrap();
        let r2 = FluidCommunities::new(FluidCommunitiesConfig::new(3, Some(999), 100))
            .run(&graph)
            .unwrap();

        // Different seeds may produce different results (not guaranteed, but allowed)
        assert!(r1.partition.num_communities() <= 3);
        assert!(r2.partition.num_communities() <= 3);
    }

    #[test]
    fn test_fluid_single_node_k_too_large() {
        let graph = GraphDataBuilder::new(1).build().unwrap();
        let fc = FluidCommunities::new(FluidCommunitiesConfig::new(2, Some(42), 100));
        let result = fc.run(&graph);

        assert!(result.is_err());
        match result.unwrap_err() {
            LeidenError::InvalidParameter { message } => {
                assert!(
                    message.contains("cannot exceed"),
                    "Expected 'cannot exceed' in error, got: {message}"
                );
            }
            _ => panic!("Expected InvalidParameter error"),
        }
    }
}
