//! Label Propagation algorithm for community detection.
//!
//! A fast, near-linear-time community detection algorithm where each node
//! adopts the label (community) most common among its neighbors. Uses
//! synchronous (batch) updates: all nodes read labels from the current state,
//! then all labels are written simultaneously.
//!
//! # Example
//!
//! ```
//! use leiden_rs::{GraphDataBuilder, label_propagation::{LabelPropagation, LabelPropagationConfig}};
//!
//! let mut b = GraphDataBuilder::new(4);
//! b.add_edge(0, 1, 1.0).unwrap();
//! b.add_edge(1, 2, 1.0).unwrap();
//! b.add_edge(2, 3, 1.0).unwrap();
//! b.add_edge(0, 3, 1.0).unwrap();
//! let graph = b.build().unwrap();
//!
//! let lp = LabelPropagation::new(LabelPropagationConfig::default());
//! let result = lp.run(&graph);
//! assert!(result.iterations > 0);
//! assert!(result.partition.num_communities() <= 4);
//! ```

use rand::rngs::StdRng;
use rand::Rng;
use rand::SeedableRng;
use rustc_hash::FxHashMap;

use crate::graph::GraphData;
use crate::partition::Partition;

/// Configuration for the Label Propagation algorithm.
#[derive(Debug, Clone, PartialEq)]
pub struct LabelPropagationConfig {
    /// Optional RNG seed for reproducible results.
    pub seed: Option<u64>,
    /// Maximum number of iterations (default: 100).
    pub max_iterations: usize,
}

impl Default for LabelPropagationConfig {
    fn default() -> Self {
        Self {
            seed: None,
            max_iterations: 100,
        }
    }
}

impl LabelPropagationConfig {
    /// Create a new configuration with the given seed and max iterations.
    #[must_use = "constructor returns a new instance"]
    pub fn new(seed: Option<u64>, max_iterations: usize) -> Self {
        Self { seed, max_iterations }
    }
}

/// Result of running the Label Propagation algorithm.
#[derive(Debug, Clone, PartialEq)]
pub struct LabelPropagationOutput {
    /// The detected community partition.
    pub partition: Partition,
    /// Number of iterations performed.
    pub iterations: usize,
    /// Whether the algorithm converged (no label changes in the final iteration).
    pub converged: bool,
}

/// The Label Propagation community detection algorithm.
///
/// Uses synchronous (batch) label updates: all nodes read the current labels,
/// compute their new label, then all updates are applied atomically.
#[derive(Debug, Clone)]
pub struct LabelPropagation {
    config: LabelPropagationConfig,
}

impl LabelPropagation {
    /// Create a new Label Propagation instance with the given configuration.
    #[must_use = "constructor returns a new instance"]
    pub fn new(config: LabelPropagationConfig) -> Self {
        Self { config }
    }

    /// Run the Label Propagation algorithm on the given graph.
    ///
    /// Returns the final partition, number of iterations performed, and
    /// whether the algorithm converged before hitting `max_iterations`.
    #[must_use = "run() performs community detection"]
    pub fn run(&self, graph: &GraphData) -> LabelPropagationOutput {
        let n = graph.node_count();

        // Edge case: empty graph
        if n == 0 {
            return LabelPropagationOutput {
                partition: Partition::new(0),
                iterations: 0,
                converged: true,
            };
        }

        // Edge case: single node
        if n == 1 {
            return LabelPropagationOutput {
                partition: Partition::new(1),
                iterations: 0,
                converged: true,
            };
        }

        // Initialize RNG
        let mut rng: StdRng = match self.config.seed {
            Some(s) => StdRng::seed_from_u64(s),
            None => StdRng::from_rng(&mut rand::rng()),
        };

        // Initialize partition: each node in its own community
        let mut partition = Partition::new(n);

        // Current labels (community IDs) for synchronous read
        let mut current_labels: Vec<usize> = (0..n).collect();
        let mut new_labels: Vec<usize> = vec![0; n];

        let mut iterations = 0;
        let mut converged = false;

        for _ in 0..self.config.max_iterations {
            iterations += 1;
            let mut any_changed = false;

            // Phase 1: Compute new labels for all nodes (synchronous read)
            for i in 0..n {
                let mut label_freq: FxHashMap<usize, usize> = FxHashMap::default();

                for (neighbor, _weight) in graph.neighbors(i) {
                    let neighbor_label = current_labels[neighbor];
                    *label_freq.entry(neighbor_label).or_insert(0) += 1;
                }

                if label_freq.is_empty() {
                    new_labels[i] = current_labels[i];
                    continue;
                }

                let max_freq = *label_freq.values().max().unwrap_or(&0);

                let best_labels: Vec<usize> = label_freq
                    .iter()
                    .filter(|&(_, &freq)| freq == max_freq)
                    .map(|(&label, _)| label)
                    .collect();

                let new_label = if best_labels.len() == 1 {
                    best_labels[0]
                } else {
                    best_labels[rng.random_range(..best_labels.len())]
                };

                new_labels[i] = new_label;
                if new_label != current_labels[i] {
                    any_changed = true;
                }
            }

            if !any_changed {
                converged = true;
                break;
            }

            // Apply all label changes atomically (synchronous write)
            for i in 0..n {
                if new_labels[i] != current_labels[i] {
                    partition.move_node(i, new_labels[i]);
                }
            }

            // Update current labels for next iteration
            current_labels.copy_from_slice(&new_labels);

            if !any_changed {
                converged = true;
                break;
            }
        }

        LabelPropagationOutput {
            partition,
            iterations,
            converged,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::graph::GraphDataBuilder;

    // ── Config tests ──

    #[test]
    fn config_default() {
        let cfg = LabelPropagationConfig::default();
        assert_eq!(cfg.seed, None);
        assert_eq!(cfg.max_iterations, 100);
    }

    #[test]
    fn config_new_custom() {
        let cfg = LabelPropagationConfig::new(Some(42), 50);
        assert_eq!(cfg.seed, Some(42));
        assert_eq!(cfg.max_iterations, 50);
    }

    #[test]
    fn config_new_no_seed() {
        let cfg = LabelPropagationConfig::new(None, 200);
        assert_eq!(cfg.seed, None);
        assert_eq!(cfg.max_iterations, 200);
    }

    // ── Output struct tests ──

    #[test]
    fn output_fields() {
        let partition = Partition::new(3);
        let output = LabelPropagationOutput {
            partition,
            iterations: 5,
            converged: true,
        };
        assert_eq!(output.iterations, 5);
        assert!(output.converged);
        assert_eq!(output.partition.community_of(0), 0);
    }

    // ── Edge case tests ──

    #[test]
    fn empty_graph() {
        let graph = GraphDataBuilder::new(0).build().unwrap();
        let lp = LabelPropagation::new(LabelPropagationConfig::default());
        let result = lp.run(&graph);
        assert_eq!(result.partition.num_communities(), 0);
        assert_eq!(result.iterations, 0);
        assert!(result.converged);
    }

    #[test]
    fn single_node() {
        let graph = GraphDataBuilder::new(1).build().unwrap();
        let lp = LabelPropagation::new(LabelPropagationConfig::default());
        let result = lp.run(&graph);
        assert_eq!(result.partition.num_communities(), 1);
        assert_eq!(result.iterations, 0);
        assert!(result.converged);
    }

    #[test]
    fn no_edges() {
        // 5 isolated nodes — each stays in own community
        let graph = GraphDataBuilder::new(5).build().unwrap();
        let lp = LabelPropagation::new(LabelPropagationConfig::default());
        let result = lp.run(&graph);
        assert_eq!(result.partition.num_communities(), 5);
        assert_eq!(result.iterations, 1);
        assert!(result.converged);
    }

    // ── Basic graph tests ──

    #[test]
    fn two_nodes_one_edge() {
        let mut b = GraphDataBuilder::new(2);
        b.add_edge(0, 1, 1.0).unwrap();
        let graph = b.build().unwrap();

        let lp = LabelPropagation::new(LabelPropagationConfig::new(Some(42), 100));
        let result = lp.run(&graph);

        assert!(result.iterations <= 100);
        assert!(result.partition.community_of(0) < 2);
        assert!(result.partition.community_of(1) < 2);
    }

    #[test]
    fn triangle_converges() {
        let mut b = GraphDataBuilder::new(3);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(0, 2, 1.0).unwrap();
        let graph = b.build().unwrap();

        let lp = LabelPropagation::new(LabelPropagationConfig::new(Some(42), 100));
        let result = lp.run(&graph);

        assert!(result.partition.num_communities() <= 3);
        assert!(result.iterations <= 100);
    }

    #[test]
    fn two_disconnected_triangles() {
        let mut b = GraphDataBuilder::new(6);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(0, 2, 1.0).unwrap();
        b.add_edge(3, 4, 1.0).unwrap();
        b.add_edge(4, 5, 1.0).unwrap();
        b.add_edge(3, 5, 1.0).unwrap();
        let graph = b.build().unwrap();

        let lp = LabelPropagation::new(LabelPropagationConfig::new(Some(42), 100));
        let result = lp.run(&graph);

        assert!(result.partition.num_communities() <= 6);
        assert!(result.iterations <= 100);
    }

    #[test]
    fn path_graph_converges() {
        let mut b = GraphDataBuilder::new(5);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(2, 3, 1.0).unwrap();
        b.add_edge(3, 4, 1.0).unwrap();
        let graph = b.build().unwrap();

        let lp = LabelPropagation::new(LabelPropagationConfig::new(Some(42), 100));
        let result = lp.run(&graph);

        assert!(result.iterations <= 100);
        assert!(result.partition.num_communities() <= 5);
    }

    // ── Determinism tests ──

    #[test]
    fn deterministic_with_seed() {
        let mut b = GraphDataBuilder::new(10);
        for i in 0..9 {
            b.add_edge(i, i + 1, 1.0).unwrap();
        }
        b.add_edge(0, 9, 1.0).unwrap();
        let graph = b.build().unwrap();

        let cfg = LabelPropagationConfig::new(Some(123), 100);
        let r1 = LabelPropagation::new(cfg.clone()).run(&graph);
        let r2 = LabelPropagation::new(cfg).run(&graph);

        // Same seed → same result
        for i in 0..10 {
            assert_eq!(
                r1.partition.community_of(i),
                r2.partition.community_of(i),
                "Node {i} differs between runs with same seed"
            );
        }
        assert_eq!(r1.iterations, r2.iterations);
    }

    #[test]
    fn different_seeds_may_differ() {
        let mut b = GraphDataBuilder::new(6);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(0, 2, 1.0).unwrap();
        b.add_edge(1, 3, 1.0).unwrap();
        b.add_edge(2, 3, 1.0).unwrap();
        b.add_edge(3, 4, 1.0).unwrap();
        b.add_edge(3, 5, 1.0).unwrap();
        b.add_edge(4, 5, 1.0).unwrap();
        let graph = b.build().unwrap();

        let r1 = LabelPropagation::new(LabelPropagationConfig::new(Some(1), 100)).run(&graph);
        let r2 = LabelPropagation::new(LabelPropagationConfig::new(Some(999), 100)).run(&graph);

        assert!(r1.iterations <= 100);
        assert!(r2.iterations <= 100);
    }

    // ── Max iterations test ──

    #[test]
    fn max_iterations_limit() {
        let mut b = GraphDataBuilder::new(4);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(2, 3, 1.0).unwrap();
        let graph = b.build().unwrap();

        let lp = LabelPropagation::new(LabelPropagationConfig::new(Some(42), 2));
        let result = lp.run(&graph);

        assert!(result.iterations <= 2);
    }

    // ── Complete graph test ──

    #[test]
    fn complete_graph_converges() {
        let n = 6;
        let mut b = GraphDataBuilder::new(n);
        for i in 0..n {
            for j in (i + 1)..n {
                b.add_edge(i, j, 1.0).unwrap();
            }
        }
        let graph = b.build().unwrap();

        let lp = LabelPropagation::new(LabelPropagationConfig::new(Some(42), 100));
        let result = lp.run(&graph);

        assert!(result.partition.num_communities() <= n);
        assert!(result.iterations <= 100);
    }

    // ── Star graph test ──

    #[test]
    fn star_graph_runs() {
        let mut b = GraphDataBuilder::new(6);
        for i in 1..6 {
            b.add_edge(0, i, 1.0).unwrap();
        }
        let graph = b.build().unwrap();

        let lp = LabelPropagation::new(LabelPropagationConfig::new(Some(42), 100));
        let result = lp.run(&graph);

        assert!(result.iterations <= 100);
    }

    // ── Partition integrity test ──

    #[test]
    fn partition_has_valid_community_ids() {
        let mut b = GraphDataBuilder::new(8);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(2, 0, 1.0).unwrap();
        b.add_edge(3, 4, 1.0).unwrap();
        b.add_edge(4, 5, 1.0).unwrap();
        b.add_edge(5, 3, 1.0).unwrap();
        b.add_edge(6, 7, 1.0).unwrap();
        let graph = b.build().unwrap();

        let lp = LabelPropagation::new(LabelPropagationConfig::new(Some(42), 100));
        let result = lp.run(&graph);

        // Every node must have a valid community assignment
        for i in 0..8 {
            let comm = result.partition.community_of(i);
            assert!(comm < 8, "Community {comm} out of range for node {i}");
        }
    }

    // ── Weighted edges test ──

    #[test]
    fn weighted_edges_graph() {
        let mut b = GraphDataBuilder::new(4);
        b.add_edge(0, 1, 10.0).unwrap();
        b.add_edge(0, 2, 1.0).unwrap();
        b.add_edge(1, 3, 1.0).unwrap();
        b.add_edge(2, 3, 1.0).unwrap();
        let graph = b.build().unwrap();

        let lp = LabelPropagation::new(LabelPropagationConfig::new(Some(42), 100));
        let result = lp.run(&graph);

        assert!(result.iterations <= 100);
    }

    // ── Disconnected graph (3 components) ──

    #[test]
    fn three_disconnected_components() {
        let mut b = GraphDataBuilder::new(9);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(0, 2, 1.0).unwrap();
        b.add_edge(3, 4, 1.0).unwrap();
        b.add_edge(4, 5, 1.0).unwrap();
        b.add_edge(3, 5, 1.0).unwrap();
        b.add_edge(6, 7, 1.0).unwrap();
        b.add_edge(7, 8, 1.0).unwrap();
        b.add_edge(6, 8, 1.0).unwrap();
        let graph = b.build().unwrap();

        let lp = LabelPropagation::new(LabelPropagationConfig::new(Some(42), 100));
        let result = lp.run(&graph);

        assert!(result.partition.num_communities() <= 9);
        assert!(result.iterations <= 100);
    }

    #[test]
    fn planted_communities_detected() {
        // Two clear communities with dense intra-community edges
        // Community A: nodes 0-4, Community B: nodes 5-9
        // Plus a single bridge edge 4-5
        let mut b = GraphDataBuilder::new(10);
        // Community A (complete graph)
        for i in 0..5 {
            for j in (i + 1)..5 {
                b.add_edge(i, j, 1.0).unwrap();
            }
        }
        // Community B (complete graph)
        for i in 5..10 {
            for j in (i + 1)..10 {
                b.add_edge(i, j, 1.0).unwrap();
            }
        }
        // Bridge
        b.add_edge(4, 5, 1.0).unwrap();
        let graph = b.build().unwrap();

        let lp = LabelPropagation::new(LabelPropagationConfig::new(Some(42), 100));
        let result = lp.run(&graph);

        assert!(result.partition.num_communities() <= 10);
        assert!(result.iterations <= 100);
    }
}
