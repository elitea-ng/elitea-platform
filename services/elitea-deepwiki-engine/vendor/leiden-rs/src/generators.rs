//! Random graph generators for benchmarking and testing.
//!
//! Provides generators for common random graph models:
//! - **Erdős-Rényi G(n,p)**: each edge included independently with probability p
//! - **Erdős-Rényi G(n,m)**: exactly m uniformly sampled edges
//! - **Planted Partition**: groups of nodes with tunable intra/inter-group edge probability
//! - **Barabási-Albert**: preferential attachment scale-free graph

use rand::rngs::StdRng;
use rand::{Rng, SeedableRng};
use rustc_hash::FxHashSet;

use crate::error::{LeidenError, Result};
use crate::graph::{GraphData, GraphDataBuilder};

fn init_rng(seed: Option<u64>) -> StdRng {
    match seed {
        Some(s) => StdRng::seed_from_u64(s),
        None => StdRng::from_rng(&mut rand::rng()),
    }
}

/// Generate an Erdős-Rényi G(n,p) random graph.
///
/// Each possible undirected edge between `n` nodes is independently included
/// with probability `p`. All edge weights are 1.0.
///
/// # Errors
///
/// Returns [`LeidenError::InvalidParameter`] if `n == 0` or `p` is outside `[0.0, 1.0]`.
///
/// # Examples
///
/// ```
/// use leiden_rs::generators::generate_er_graph;
///
/// let graph = generate_er_graph(10, 0.5, Some(42)).unwrap();
/// assert_eq!(graph.node_count(), 10);
/// ```
#[must_use = "graph generation is expensive"]
pub fn generate_er_graph(n: usize, p: f64, seed: Option<u64>) -> Result<GraphData> {
    if n == 0 {
        return Err(LeidenError::InvalidParameter {
            message: "n must be positive".to_string(),
        });
    }
    if !(0.0..=1.0).contains(&p) {
        return Err(LeidenError::InvalidParameter {
            message: format!("p must be in [0.0, 1.0], got {p}"),
        });
    }

    let mut rng = init_rng(seed);
    let mut builder = GraphDataBuilder::new(n);

    for i in 0..n {
        for j in (i + 1)..n {
            if rng.random::<f64>() < p {
                builder.add_edge(i, j, 1.0)?;
            }
        }
    }

    builder.build()
}

/// Generate an Erdős-Rényi G(n,m) random graph with exactly `m` edges.
///
/// Uniformly samples `m` unique undirected edges from all possible pairs.
/// All edge weights are 1.0.
///
/// # Errors
///
/// Returns [`LeidenError::InvalidParameter`] if `n == 0` or `m > n*(n-1)/2`.
///
/// # Examples
///
/// ```
/// use leiden_rs::generators::generate_er_graph_exact;
///
/// let graph = generate_er_graph_exact(10, 15, Some(42)).unwrap();
/// assert_eq!(graph.node_count(), 10);
/// assert!((graph.total_weight() - 15.0).abs() < 1e-10);
/// ```
#[must_use = "graph generation is expensive"]
pub fn generate_er_graph_exact(n: usize, m: usize, seed: Option<u64>) -> Result<GraphData> {
    if n == 0 {
        return Err(LeidenError::InvalidParameter {
            message: "n must be positive".to_string(),
        });
    }

    let max_edges = n * (n - 1) / 2;
    if m > max_edges {
        return Err(LeidenError::InvalidParameter {
            message: format!("m ({m}) exceeds maximum edges ({max_edges}) for n={n}"),
        });
    }

    let mut rng = init_rng(seed);
    let mut builder = GraphDataBuilder::new(n);

    if m == 0 {
        return builder.build();
    }

    if m <= max_edges / 2 {
        let mut edges: FxHashSet<(usize, usize)> = FxHashSet::default();
        while edges.len() < m {
            let i = rng.random_range(0..n);
            let j = rng.random_range(0..n);
            if i != j {
                edges.insert((i.min(j), i.max(j)));
            }
        }
        for (u, v) in edges {
            builder.add_edge(u, v, 1.0)?;
        }
    } else {
        let mut all_edges: Vec<(usize, usize)> = Vec::with_capacity(max_edges);
        for i in 0..n {
            for j in (i + 1)..n {
                all_edges.push((i, j));
            }
        }
        for i in (1..all_edges.len()).rev() {
            let j = rng.random_range(0..=i);
            all_edges.swap(i, j);
        }
        for &(u, v) in &all_edges[..m] {
            builder.add_edge(u, v, 1.0)?;
        }
    }

    builder.build()
}

/// Generate a Planted Partition model graph.
///
/// Splits `n` nodes into `k` groups of approximately equal size. Edges within
/// the same group are added with probability `p_in`; edges between different
/// groups with probability `p_out`.
///
/// Returns a tuple of `(graph, ground_truth)` where `ground_truth[i]` is the
/// community assignment of node `i`.
///
/// # Errors
///
/// Returns [`LeidenError::InvalidParameter`] if `n == 0`, `k == 0`, `k > n`,
/// or `p_in`/`p_out` are outside `[0.0, 1.0]`.
///
/// # Examples
///
/// ```
/// use leiden_rs::generators::generate_planted_partition;
///
/// let (graph, communities) = generate_planted_partition(20, 2, 0.8, 0.1, Some(42)).unwrap();
/// assert_eq!(graph.node_count(), 20);
/// assert_eq!(communities.len(), 20);
/// ```
#[must_use = "graph generation is expensive"]
pub fn generate_planted_partition(
    n: usize,
    k: usize,
    p_in: f64,
    p_out: f64,
    seed: Option<u64>,
) -> Result<(GraphData, Vec<usize>)> {
    if n == 0 {
        return Err(LeidenError::InvalidParameter {
            message: "n must be positive".to_string(),
        });
    }
    if k == 0 {
        return Err(LeidenError::InvalidParameter {
            message: "k must be positive".to_string(),
        });
    }
    if k > n {
        return Err(LeidenError::InvalidParameter {
            message: format!("k ({k}) must not exceed n ({n})"),
        });
    }
    if !(0.0..=1.0).contains(&p_in) {
        return Err(LeidenError::InvalidParameter {
            message: format!("p_in must be in [0.0, 1.0], got {p_in}"),
        });
    }
    if !(0.0..=1.0).contains(&p_out) {
        return Err(LeidenError::InvalidParameter {
            message: format!("p_out must be in [0.0, 1.0], got {p_out}"),
        });
    }

    let mut rng = init_rng(seed);
    let ground_truth: Vec<usize> = (0..n).map(|i| i * k / n).collect();
    let mut builder = GraphDataBuilder::new(n);

    for i in 0..n {
        for j in (i + 1)..n {
            let prob = if ground_truth[i] == ground_truth[j] {
                p_in
            } else {
                p_out
            };
            if rng.random::<f64>() < prob {
                builder.add_edge(i, j, 1.0)?;
            }
        }
    }

    let graph = builder.build()?;
    Ok((graph, ground_truth))
}

/// Generate a Barabási-Albert preferential attachment graph.
///
/// Starts with `m0` fully connected nodes (defaults to `m` when `m0_option` is
/// `None`), then adds `n - m0` nodes one at a time. Each new node connects to
/// `m` existing nodes with probability proportional to their degree.
///
/// # Errors
///
/// Returns [`LeidenError::InvalidParameter`] if `m == 0`, `n < m0`, or `m0 < m`.
///
/// # Examples
///
/// ```
/// use leiden_rs::generators::generate_ba_graph;
///
/// let graph = generate_ba_graph(20, 2, None, Some(42)).unwrap();
/// assert_eq!(graph.node_count(), 20);
/// ```
#[must_use = "graph generation is expensive"]
pub fn generate_ba_graph(
    n: usize,
    m: usize,
    m0_option: Option<usize>,
    seed: Option<u64>,
) -> Result<GraphData> {
    let m0 = m0_option.unwrap_or(m);

    if m == 0 {
        return Err(LeidenError::InvalidParameter {
            message: "m must be at least 1".to_string(),
        });
    }
    if n < m0 {
        return Err(LeidenError::InvalidParameter {
            message: format!("n ({n}) must be >= m0 ({m0})"),
        });
    }
    if m0 < m {
        return Err(LeidenError::InvalidParameter {
            message: format!("m0 ({m0}) must be >= m ({m})"),
        });
    }

    let mut rng = init_rng(seed);
    let mut builder = GraphDataBuilder::new(n);

    // Initial clique of m0 nodes
    for i in 0..m0 {
        for j in (i + 1)..m0 {
            builder.add_edge(i, j, 1.0)?;
        }
    }

    // Stub list for preferential attachment: each node appears degree[node] times
    let mut stub_list: Vec<usize> = Vec::new();
    if m0 > 1 {
        for i in 0..m0 {
            for _ in 0..(m0 - 1) {
                stub_list.push(i);
            }
        }
    } else {
        stub_list.push(0);
    }

    // Add remaining nodes via preferential attachment
    for new_node in m0..n {
        let mut targets: FxHashSet<usize> = FxHashSet::default();
        let mut attempts = 0;
        while targets.len() < m && attempts < m * 100 {
            let idx = rng.random_range(0..stub_list.len());
            let target = stub_list[idx];
            if target != new_node {
                targets.insert(target);
            }
            attempts += 1;
        }

        for &target in &targets {
            builder.add_edge(new_node, target, 1.0)?;
            stub_list.push(new_node);
            stub_list.push(target);
        }
    }

    builder.build()
}

/// Generate a Stochastic Block Model (SBM) random graph.
///
/// Partitions `n` nodes into `k` communities according to `community_sizes`.
/// Edges are added independently with probabilities given by the `k × k`
/// affinity matrix: an edge between node `i` (community `u`) and node `j`
/// (community `v`) is present with probability `affinity[u][v]`.
///
/// Returns a tuple of `(graph, ground_truth)` where `ground_truth[i]` is the
/// community assignment (0-indexed) of node `i`.
///
/// # Errors
///
/// Returns [`LeidenError::InvalidParameter`] if:
/// - `community_sizes` is empty
/// - any community size is zero
/// - `affinity` is not a `k × k` square matrix
/// - `affinity` is not symmetric
/// - any affinity value is outside `[0.0, 1.0]`
///
/// # Examples
///
/// ```
/// use leiden_rs::generators::generate_sbm;
///
/// let sizes = vec![5, 5];
/// let affinity = vec![vec![0.8, 0.2], vec![0.2, 0.8]];
/// let (graph, communities) = generate_sbm(&sizes, &affinity, Some(42)).unwrap();
/// assert_eq!(graph.node_count(), 10);
/// assert_eq!(communities.len(), 10);
/// ```
#[must_use = "graph generation is expensive"]
pub fn generate_sbm(
    community_sizes: &[usize],
    affinity: &[Vec<f64>],
    seed: Option<u64>,
) -> Result<(GraphData, Vec<usize>)> {
    let k = community_sizes.len();
    if k == 0 {
        return Err(LeidenError::InvalidParameter {
            message: "community_sizes must not be empty".to_string(),
        });
    }
    if community_sizes.contains(&0) {
        return Err(LeidenError::InvalidParameter {
            message: "all community sizes must be positive".to_string(),
        });
    }

    // Validate affinity matrix dimensions and values
    if affinity.len() != k {
        return Err(LeidenError::InvalidParameter {
            message: format!(
                "affinity must have {k} rows (matching community_sizes), got {}",
                affinity.len()
            ),
        });
    }
    for (i, row) in affinity.iter().enumerate() {
        if row.len() != k {
            return Err(LeidenError::InvalidParameter {
                message: format!(
                    "affinity row {i} has {} columns, expected {k}",
                    row.len()
                ),
            });
        }
        for (j, &val) in row.iter().enumerate() {
            if !(0.0..=1.0).contains(&val) {
                return Err(LeidenError::InvalidParameter {
                    message: format!("affinity[{i}][{j}] = {val} is outside [0.0, 1.0]"),
                });
            }
            // Symmetry check: only check upper triangle
            if i > j && (val - affinity[j][i]).abs() > 1e-12 {
                return Err(LeidenError::InvalidParameter {
                    message: format!(
                        "affinity must be symmetric: \
                         affinity[{i}][{j}] = {val} ≠ affinity[{j}][{i}] = {}",
                        affinity[j][i]
                    ),
                });
            }
        }
    }

    let n: usize = community_sizes.iter().sum();
    let mut rng = init_rng(seed);
    let mut ground_truth = Vec::with_capacity(n);
    for (comm, &size) in community_sizes.iter().enumerate() {
        for _ in 0..size {
            ground_truth.push(comm);
        }
    }

    let mut builder = GraphDataBuilder::new(n);
    for i in 0..n {
        for j in (i + 1)..n {
            let prob = affinity[ground_truth[i]][ground_truth[j]];
            if rng.random::<f64>() < prob {
                builder.add_edge(i, j, 1.0)?;
            }
        }
    }

    let graph = builder.build()?;
    Ok((graph, ground_truth))
}

/// Generate a symmetric Stochastic Block Model (SBM) random graph.
///
/// Splits `n` nodes into `k` equally-sized communities. Edges within the same
/// community are added with probability `p_in`; edges between different
/// communities with probability `p_out`.
///
/// This is a convenience wrapper around [`generate_sbm`] that constructs a
/// uniform affinity matrix where diagonal entries are `p_in` and off-diagonal
/// entries are `p_out`.
///
/// # Errors
///
/// Returns [`LeidenError::InvalidParameter`] if:
/// - `n == 0`
/// - `k == 0` or `k > n`
/// - `n` is not divisible by `k`
/// - `p_in` or `p_out` is outside `[0.0, 1.0]`
///
/// # Examples
///
/// ```
/// use leiden_rs::generators::generate_sbm_symmetric;
///
/// let (graph, communities) = generate_sbm_symmetric(20, 4, 0.8, 0.1, Some(42)).unwrap();
/// assert_eq!(graph.node_count(), 20);
/// assert_eq!(communities.len(), 20);
/// ```
#[must_use = "graph generation is expensive"]
pub fn generate_sbm_symmetric(
    n: usize,
    k: usize,
    p_in: f64,
    p_out: f64,
    seed: Option<u64>,
) -> Result<(GraphData, Vec<usize>)> {
    if n == 0 {
        return Err(LeidenError::InvalidParameter {
            message: "n must be positive".to_string(),
        });
    }
    if k == 0 {
        return Err(LeidenError::InvalidParameter {
            message: "k must be positive".to_string(),
        });
    }
    if k > n {
        return Err(LeidenError::InvalidParameter {
            message: format!("k ({k}) must not exceed n ({n})"),
        });
    }
    if n % k != 0 {
        return Err(LeidenError::InvalidParameter {
            message: format!(
                "n ({n}) must be divisible by k ({k}) for equal community sizes"
            ),
        });
    }
    if !(0.0..=1.0).contains(&p_in) {
        return Err(LeidenError::InvalidParameter {
            message: format!("p_in must be in [0.0, 1.0], got {p_in}"),
        });
    }
    if !(0.0..=1.0).contains(&p_out) {
        return Err(LeidenError::InvalidParameter {
            message: format!("p_out must be in [0.0, 1.0], got {p_out}"),
        });
    }

    let community_sizes = vec![n / k; k];
    let mut affinity = vec![vec![p_out; k]; k];
    for (i, row) in affinity.iter_mut().enumerate() {
        row[i] = p_in;
    }

    generate_sbm(&community_sizes, &affinity, seed)
}

#[cfg(test)]
mod tests {
    use super::*;

    // ── ER G(n,p) tests ──────────────────────────────────────────────

    #[test]
    fn test_er_basic() {
        let graph = generate_er_graph(10, 0.5, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 10);
        // Expected ~22.5 edges for n=10, p=0.5 (max 45 edges)
        assert!(graph.total_weight() > 0.0);
        assert!(graph.total_weight() <= 45.0);
    }

    #[test]
    fn test_er_deterministic() {
        let g1 = generate_er_graph(10, 0.5, Some(42)).unwrap();
        let g2 = generate_er_graph(10, 0.5, Some(42)).unwrap();
        assert_eq!(g1.total_weight(), g2.total_weight());
    }

    #[test]
    fn test_er_p_zero() {
        let graph = generate_er_graph(10, 0.0, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 10);
        assert_eq!(graph.total_weight(), 0.0);
    }

    #[test]
    fn test_er_p_one() {
        let graph = generate_er_graph(5, 1.0, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 5);
        // Complete graph K5: 5*4/2 = 10 edges
        assert!((graph.total_weight() - 10.0).abs() < 1e-10);
    }

    #[test]
    fn test_er_invalid_n_zero() {
        assert!(generate_er_graph(0, 0.5, None).is_err());
    }

    #[test]
    fn test_er_invalid_p_negative() {
        assert!(generate_er_graph(5, -0.1, None).is_err());
    }

    #[test]
    fn test_er_invalid_p_gt_one() {
        assert!(generate_er_graph(5, 1.1, None).is_err());
    }

    #[test]
    fn test_er_single_node() {
        let graph = generate_er_graph(1, 0.5, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 1);
        assert_eq!(graph.total_weight(), 0.0);
    }

    // ── ER G(n,m) tests ──────────────────────────────────────────────

    #[test]
    fn test_er_exact_basic() {
        let graph = generate_er_graph_exact(10, 15, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 10);
        assert!((graph.total_weight() - 15.0).abs() < 1e-10);
    }

    #[test]
    fn test_er_exact_deterministic() {
        let g1 = generate_er_graph_exact(10, 15, Some(42)).unwrap();
        let g2 = generate_er_graph_exact(10, 15, Some(42)).unwrap();
        assert_eq!(g1.total_weight(), g2.total_weight());
    }

    #[test]
    fn test_er_exact_m_zero() {
        let graph = generate_er_graph_exact(5, 0, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 5);
        assert_eq!(graph.total_weight(), 0.0);
    }

    #[test]
    fn test_er_exact_all_edges() {
        // Complete graph K4: 4*3/2 = 6 edges
        let graph = generate_er_graph_exact(4, 6, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 4);
        assert!((graph.total_weight() - 6.0).abs() < 1e-10);
    }

    #[test]
    fn test_er_exact_invalid_n_zero() {
        assert!(generate_er_graph_exact(0, 0, None).is_err());
    }

    #[test]
    fn test_er_exact_invalid_m_too_large() {
        // For n=5: max edges = 5*4/2 = 10
        assert!(generate_er_graph_exact(5, 11, None).is_err());
    }

    #[test]
    fn test_er_exact_single_node() {
        let graph = generate_er_graph_exact(1, 0, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 1);
        assert_eq!(graph.total_weight(), 0.0);
    }

    // ── Planted Partition tests ───────────────────────────────────────

    #[test]
    fn test_pp_basic() {
        let (graph, gt) = generate_planted_partition(20, 2, 0.8, 0.1, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 20);
        assert_eq!(gt.len(), 20);
        let unique: std::collections::HashSet<usize> = gt.iter().copied().collect();
        assert_eq!(unique.len(), 2);
    }

    #[test]
    fn test_pp_deterministic() {
        let (g1, gt1) = generate_planted_partition(20, 2, 0.8, 0.1, Some(42)).unwrap();
        let (g2, gt2) = generate_planted_partition(20, 2, 0.8, 0.1, Some(42)).unwrap();
        assert_eq!(g1.total_weight(), g2.total_weight());
        assert_eq!(gt1, gt2);
    }

    #[test]
    fn test_pp_ground_truth_sizes() {
        let (_, gt) = generate_planted_partition(20, 4, 0.8, 0.1, Some(42)).unwrap();
        let mut counts = [0usize; 4];
        for &c in &gt {
            assert!(c < 4, "community {c} out of range");
            counts[c] += 1;
        }
        // 20 nodes into 4 groups: 5 each (integer division)
        for (c, &count) in counts.iter().enumerate() {
            assert_eq!(count, 5, "community {c} has {count} nodes, expected 5");
        }
    }

    #[test]
    fn test_pp_strong_communities() {
        // High p_in, zero p_out should produce dense intra-community edges
        let (graph, gt) = generate_planted_partition(20, 2, 1.0, 0.0, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 20);
        // With p_in=1.0: each group of 10 has 10*9/2 = 45 edges, total = 90
        assert!((graph.total_weight() - 90.0).abs() < 1e-10);
        let _ = gt;
    }

    #[test]
    fn test_pp_invalid_n_zero() {
        assert!(generate_planted_partition(0, 2, 0.5, 0.1, None).is_err());
    }

    #[test]
    fn test_pp_invalid_k_zero() {
        assert!(generate_planted_partition(10, 0, 0.5, 0.1, None).is_err());
    }

    #[test]
    fn test_pp_invalid_k_gt_n() {
        assert!(generate_planted_partition(5, 10, 0.5, 0.1, None).is_err());
    }

    #[test]
    fn test_pp_invalid_p_in_negative() {
        assert!(generate_planted_partition(10, 2, -0.1, 0.1, None).is_err());
    }

    #[test]
    fn test_pp_invalid_p_out_gt_one() {
        assert!(generate_planted_partition(10, 2, 0.5, 1.5, None).is_err());
    }

    // ── Barabási-Albert tests ─────────────────────────────────────────

    #[test]
    fn test_ba_basic() {
        let graph = generate_ba_graph(20, 2, None, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 20);
        assert!(graph.total_weight() > 0.0);
    }

    #[test]
    fn test_ba_deterministic() {
        let g1 = generate_ba_graph(20, 2, None, Some(42)).unwrap();
        let g2 = generate_ba_graph(20, 2, None, Some(42)).unwrap();
        assert_eq!(g1.total_weight(), g2.total_weight());
    }

    #[test]
    fn test_ba_m0_default() {
        let g_default = generate_ba_graph(10, 2, None, Some(42)).unwrap();
        let g_explicit = generate_ba_graph(10, 2, Some(2), Some(42)).unwrap();
        assert_eq!(g_default.total_weight(), g_explicit.total_weight());
    }

    #[test]
    fn test_ba_edge_count() {
        // BA(20, 2, m0=2): initial 1 edge, 18 new nodes each adding 2 edges = 1 + 36 = 37
        let graph = generate_ba_graph(20, 2, Some(2), Some(42)).unwrap();
        assert!((graph.total_weight() - 37.0).abs() < 1e-10);
    }

    #[test]
    fn test_ba_invalid_m_zero() {
        assert!(generate_ba_graph(10, 0, None, None).is_err());
    }

    #[test]
    fn test_ba_invalid_n_lt_m0() {
        assert!(generate_ba_graph(5, 2, Some(10), None).is_err());
    }

    #[test]
    fn test_ba_invalid_m0_lt_m() {
        assert!(generate_ba_graph(10, 3, Some(2), None).is_err());
    }

    #[test]
    fn test_ba_single_node() {
        let graph = generate_ba_graph(1, 1, Some(1), Some(42)).unwrap();
        assert_eq!(graph.node_count(), 1);
        assert_eq!(graph.total_weight(), 0.0);
    }

    #[test]
    fn test_ba_two_nodes() {
        let graph = generate_ba_graph(2, 1, Some(1), Some(42)).unwrap();
        assert_eq!(graph.node_count(), 2);
        assert!((graph.total_weight() - 1.0).abs() < 1e-10);
    }

    // ── SBM general tests ─────────────────────────────────────────────

    #[test]
    fn test_sbm_general_basic() {
        let sizes = vec![5, 5];
        let affinity = vec![vec![0.8, 0.2], vec![0.2, 0.8]];
        let (graph, gt) = generate_sbm(&sizes, &affinity, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 10);
        assert_eq!(gt.len(), 10);
        // Two distinct communities
        let unique: std::collections::HashSet<usize> = gt.iter().copied().collect();
        assert_eq!(unique.len(), 2);
    }

    #[test]
    fn test_sbm_general_deterministic() {
        let sizes = vec![4, 3, 3];
        let affinity = vec![
            vec![0.9, 0.1, 0.1],
            vec![0.1, 0.8, 0.2],
            vec![0.1, 0.2, 0.7],
        ];
        let (g1, gt1) = generate_sbm(&sizes, &affinity, Some(42)).unwrap();
        let (g2, gt2) = generate_sbm(&sizes, &affinity, Some(42)).unwrap();
        assert_eq!(g1.total_weight(), g2.total_weight());
        assert_eq!(gt1, gt2);
    }

    #[test]
    fn test_sbm_general_unequal_sizes() {
        let sizes = vec![2, 5, 3];
        let affinity = vec![
            vec![0.9, 0.1, 0.1],
            vec![0.1, 0.8, 0.2],
            vec![0.1, 0.2, 0.7],
        ];
        let (graph, gt) = generate_sbm(&sizes, &affinity, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 10);
        // Check ground truth labels match community sizes
        assert_eq!(&gt[0..2], &[0, 0]);
        assert_eq!(&gt[2..7], &[1, 1, 1, 1, 1]);
        assert_eq!(&gt[7..10], &[2, 2, 2]);
    }

    #[test]
    fn test_sbm_general_core_periphery() {
        // Core community 0: high p_in, periphery community 1: lower p_in
        let sizes = vec![5, 10];
        let affinity = vec![vec![0.9, 0.3], vec![0.3, 0.1]];
        let (graph, gt) = generate_sbm(&sizes, &affinity, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 15);
        assert_eq!(gt.len(), 15);
        assert!(graph.total_weight() > 0.0);
    }

    #[test]
    fn test_sbm_general_bipartite() {
        // Bipartite: intra-community edges with p=0, inter-community with p=0.8
        let sizes = vec![5, 5];
        let affinity = vec![vec![0.0, 0.8], vec![0.8, 0.0]];
        let (graph, gt) = generate_sbm(&sizes, &affinity, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 10);
        // With p=0 intra-community: edges only between communities
        // For 2 groups of 5: max cross edges = 5*5 = 25
        assert!(graph.total_weight() > 0.0);
        assert!(graph.total_weight() <= 25.0);
        let _ = gt;
    }

    #[test]
    fn test_sbm_general_p_zero() {
        let sizes = vec![4, 4];
        let affinity = vec![vec![0.0, 0.0], vec![0.0, 0.0]];
        let (graph, gt) = generate_sbm(&sizes, &affinity, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 8);
        assert_eq!(graph.total_weight(), 0.0);
        let _ = gt;
    }

    #[test]
    fn test_sbm_general_p_one() {
        let sizes = vec![3, 2];
        let affinity = vec![vec![1.0, 1.0], vec![1.0, 1.0]];
        let (graph, gt) = generate_sbm(&sizes, &affinity, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 5);
        // Complete graph K5: 5*4/2 = 10 edges
        assert!((graph.total_weight() - 10.0).abs() < 1e-10);
        let _ = gt;
    }

    #[test]
    fn test_sbm_general_invalid_empty_sizes() {
        let affinity: Vec<Vec<f64>> = vec![];
        assert!(generate_sbm(&[], &affinity, Some(42)).is_err());
    }

    #[test]
    fn test_sbm_general_invalid_zero_size_community() {
        let sizes = vec![5, 0, 5];
        let affinity = vec![
            vec![0.8, 0.2, 0.1],
            vec![0.2, 0.7, 0.1],
            vec![0.1, 0.1, 0.6],
        ];
        assert!(generate_sbm(&sizes, &affinity, Some(42)).is_err());
    }

    #[test]
    fn test_sbm_general_invalid_non_square() {
        let sizes = vec![5, 5];
        // Only 1 row instead of 2
        let affinity = vec![vec![0.8, 0.2]];
        assert!(generate_sbm(&sizes, &affinity, Some(42)).is_err());
    }

    #[test]
    fn test_sbm_general_invalid_affinity_dim() {
        let sizes = vec![5, 5];
        // Row has 3 columns instead of 2
        let affinity = vec![
            vec![0.8, 0.2, 0.1],
            vec![0.2, 0.8, 0.1],
        ];
        assert!(generate_sbm(&sizes, &affinity, Some(42)).is_err());
    }

    #[test]
    fn test_sbm_general_invalid_asymmetric() {
        let sizes = vec![5, 5];
        let affinity = vec![
            vec![0.8, 0.2],
            vec![0.3, 0.8], // [1][0] = 0.3 ≠ [0][1] = 0.2
        ];
        assert!(generate_sbm(&sizes, &affinity, Some(42)).is_err());
    }

    #[test]
    fn test_sbm_general_invalid_prob_negative() {
        let sizes = vec![5, 5];
        let affinity = vec![
            vec![0.8, -0.1],
            vec![-0.1, 0.8],
        ];
        assert!(generate_sbm(&sizes, &affinity, Some(42)).is_err());
    }

    #[test]
    fn test_sbm_general_invalid_prob_gt_one() {
        let sizes = vec![5, 5];
        let affinity = vec![
            vec![0.8, 1.5],
            vec![1.5, 0.8],
        ];
        assert!(generate_sbm(&sizes, &affinity, Some(42)).is_err());
    }

    // ── SBM symmetric tests ───────────────────────────────────────────

    #[test]
    fn test_sbm_symmetric_basic() {
        let (graph, gt) = generate_sbm_symmetric(10, 2, 0.8, 0.1, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 10);
        assert_eq!(gt.len(), 10);
        let unique: std::collections::HashSet<usize> = gt.iter().copied().collect();
        assert_eq!(unique.len(), 2);
    }

    #[test]
    fn test_sbm_symmetric_deterministic() {
        let (g1, gt1) = generate_sbm_symmetric(20, 4, 0.7, 0.05, Some(42)).unwrap();
        let (g2, gt2) = generate_sbm_symmetric(20, 4, 0.7, 0.05, Some(42)).unwrap();
        assert_eq!(g1.total_weight(), g2.total_weight());
        assert_eq!(gt1, gt2);
    }

    #[test]
    fn test_sbm_symmetric_ground_truth_sizes() {
        let (_, gt) = generate_sbm_symmetric(12, 3, 0.8, 0.1, Some(42)).unwrap();
        // 12/3 = 4 nodes per community
        let mut counts = [0usize; 3];
        for &c in &gt {
            assert!(c < 3);
            counts[c] += 1;
        }
        for (c, &count) in counts.iter().enumerate() {
            assert_eq!(count, 4, "community {c} has {count} nodes, expected 4");
        }
    }

    #[test]
    fn test_sbm_symmetric_n_not_divisible() {
        assert!(generate_sbm_symmetric(10, 3, 0.8, 0.1, None).is_err());
    }

    #[test]
    fn test_sbm_symmetric_invalid_n_zero() {
        assert!(generate_sbm_symmetric(0, 2, 0.5, 0.1, None).is_err());
    }

    #[test]
    fn test_sbm_symmetric_invalid_k_zero() {
        assert!(generate_sbm_symmetric(10, 0, 0.5, 0.1, None).is_err());
    }

    #[test]
    fn test_sbm_symmetric_invalid_k_gt_n() {
        assert!(generate_sbm_symmetric(5, 10, 0.5, 0.1, None).is_err());
    }

    #[test]
    fn test_sbm_symmetric_invalid_p_in_negative() {
        assert!(generate_sbm_symmetric(10, 2, -0.1, 0.1, None).is_err());
    }

    #[test]
    fn test_sbm_symmetric_invalid_p_in_gt_one() {
        assert!(generate_sbm_symmetric(10, 2, 1.5, 0.1, None).is_err());
    }

    #[test]
    fn test_sbm_symmetric_invalid_p_out_negative() {
        assert!(generate_sbm_symmetric(10, 2, 0.5, -0.1, None).is_err());
    }

    #[test]
    fn test_sbm_symmetric_invalid_p_out_gt_one() {
        assert!(generate_sbm_symmetric(10, 2, 0.5, 1.2, None).is_err());
    }

    #[test]
    fn test_sbm_symmetric_strong_communities() {
        // p_in=1.0, p_out=0.0 should produce dense intra edges and zero inter edges
        let (graph, gt) = generate_sbm_symmetric(12, 3, 1.0, 0.0, Some(42)).unwrap();
        // Each group of 4: 4*3/2 = 6 edges, 3 groups = 18 edges total
        assert!((graph.total_weight() - 18.0).abs() < 1e-10);
        let _ = gt;
    }

    #[test]
    fn test_sbm_symmetric_empty_graph() {
        let (graph, gt) = generate_sbm_symmetric(2, 2, 0.0, 0.0, Some(42)).unwrap();
        assert_eq!(graph.node_count(), 2);
        assert_eq!(graph.total_weight(), 0.0);
        assert_eq!(gt, vec![0, 1]);
    }
}
