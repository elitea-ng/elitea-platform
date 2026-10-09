//! LFR benchmark graph generator.

use rand::rngs::StdRng;
use rand::seq::SliceRandom;
use rand::{Rng, SeedableRng};
use rustc_hash::FxHashSet;

use crate::error::{LeidenError, Result};

/// Configuration for generating an LFR benchmark graph.
///
/// The LFR (Lancichinetti-Fortunato-Radicchi) model generates synthetic networks
/// with power-law degree and community size distributions and a tunable mixing
/// parameter `mu` controlling the fraction of inter-community edges.
#[derive(Debug, Clone)]
pub struct LfrConfig {
    /// Number of nodes in the generated graph.
    pub n: usize,
    /// Power-law exponent for the degree distribution (must be > 1).
    pub tau1: f64,
    /// Power-law exponent for the community size distribution (must be > 1).
    pub tau2: f64,
    /// Mixing parameter: fraction of a node's edges that cross community boundaries (0..=1).
    pub mu: f64,
    /// Target average degree (used to compute `min_degree` if `min_degree` is `None`).
    pub average_degree: Option<f64>,
    /// Minimum node degree. Computed from `average_degree` if `None`.
    pub min_degree: Option<usize>,
    /// Maximum node degree (defaults to `n - 1`).
    pub max_degree: Option<usize>,
    /// Minimum community size (defaults to `max(min_degree, 4)`).
    pub min_community: Option<usize>,
    /// Maximum community size (defaults to `n`).
    pub max_community: Option<usize>,
    /// RNG seed for reproducible graph generation.
    pub seed: Option<u64>,
}

/// An LFR benchmark graph with known ground-truth community structure.
#[derive(Debug)]
pub struct LfrGraph {
    /// Weighted edge list: `(source, target, weight)`.
    pub edges: Vec<(usize, usize, f64)>,
    /// Number of nodes in the graph.
    pub node_count: usize,
    /// Ground-truth community assignment: `ground_truth[node] = community_id`.
    pub ground_truth: Vec<usize>,
    /// Size of each community.
    pub community_sizes: Vec<usize>,
}

/// Generate an LFR benchmark graph from the given configuration.
///
/// Returns an [`LfrGraph`] containing the generated edges, ground-truth community
/// assignments, and community sizes. All edge weights are 1.0.
///
/// # Errors
///
/// Returns an error if the configuration is invalid (e.g., `n == 0`,
/// `tau1 <= 1.0`, `tau2 <= 1.0`, or `mu` outside `[0, 1]`).
pub fn generate_lfr_graph(config: LfrConfig) -> Result<LfrGraph> {
    validate_config(&config)?;

    let mut rng = match config.seed {
        Some(seed) => StdRng::seed_from_u64(seed),
        None => StdRng::from_rng(&mut rand::rng()),
    };

    let max_degree = config.max_degree.unwrap_or(config.n.saturating_sub(1));
    let max_community = config.max_community.unwrap_or(config.n);

    let min_degree = if let Some(min_deg) = config.min_degree {
        min_deg
    } else if let Some(avg_deg) = config.average_degree {
        find_min_degree_for_average(config.tau1, avg_deg, max_degree)
    } else {
        5
    };

    let min_community = config.min_community.unwrap_or(std::cmp::max(min_degree, 4));

    let degree_seq =
        generate_degree_sequence(&mut rng, config.n, config.tau1, min_degree, max_degree);

    let community_sizes = generate_community_sizes(
        &mut rng,
        config.n,
        config.tau2,
        min_community,
        max_community,
    );

    // Cap degree sequence so intra-community wiring is feasible:
    // a node needs (1-mu)*degree internal neighbors, which must fit in (community_size-1).
    let min_comm = *community_sizes.iter().min().unwrap_or(&min_community);
    let degree_cap = if min_comm > 0 { min_comm - 1 } else { 0 };
    let mut degree_seq: Vec<usize> = degree_seq.into_iter().map(|d| d.min(degree_cap)).collect();
    let sum: usize = degree_seq.iter().sum();
    if sum % 2 != 0 {
        if let Some(d) = degree_seq.iter_mut().find(|d| **d < degree_cap) {
            *d += 1;
            *d = (*d).min(degree_cap);
        }
    }

    let ground_truth = assign_communities(&degree_seq, &community_sizes, config.mu, &mut rng)?;

    let edges = wire_edges(config.n, &degree_seq, &ground_truth, config.mu, &mut rng)?;

    Ok(LfrGraph {
        edges,
        node_count: config.n,
        ground_truth,
        community_sizes,
    })
}

fn validate_config(config: &LfrConfig) -> Result<()> {
    if config.n == 0 {
        return Err(LeidenError::InvalidParameter {
            message: "n must be positive".to_string(),
        });
    }
    if config.tau1 <= 1.0 {
        return Err(LeidenError::InvalidParameter {
            message: "tau1 must be > 1".to_string(),
        });
    }
    if config.tau2 <= 1.0 {
        return Err(LeidenError::InvalidParameter {
            message: "tau2 must be > 1".to_string(),
        });
    }
    if config.mu < 0.0 || config.mu > 1.0 {
        return Err(LeidenError::InvalidParameter {
            message: "mu must be in [0, 1]".to_string(),
        });
    }

    if config.mu < 1.0 {
        if let (Some(max_degree), Some(min_community)) = (config.max_degree, config.min_community) {
            let max_intra = ((1.0 - config.mu) * max_degree as f64).ceil() as usize;
            if max_intra >= min_community {
                return Err(LeidenError::InvalidParameter {
                    message: format!(
                        "max intra-degree ({}) >= min_community ({}); \
                         increase min_community, decrease max_degree, or increase mu",
                        max_intra, min_community
                    ),
                });
            }
        }
    }

    Ok(())
}

fn powerlaw_sample(rng: &mut StdRng, exponent: f64, min_val: usize, max_val: usize) -> usize {
    if min_val >= max_val {
        return min_val;
    }
    loop {
        let u: f64 = rng.random();
        let x = (min_val as f64) * (1.0 - u).powf(1.0 / (1.0 - exponent));
        let x = x.round() as usize;
        if x >= min_val && x <= max_val {
            return x;
        }
    }
}

fn find_min_degree_for_average(tau1: f64, target_avg: f64, max_degree: usize) -> usize {
    let mut low = 1;
    let mut high = max_degree;

    for _ in 0..50 {
        if low >= high {
            break;
        }
        let mid = (low + high) / 2;
        let mut test_rng = StdRng::seed_from_u64(42);
        let mut sum = 0usize;
        for _ in 0..1000 {
            sum += powerlaw_sample(&mut test_rng, tau1, mid, max_degree);
        }
        let test_avg = sum as f64 / 1000.0;
        if test_avg > target_avg {
            high = mid;
        } else {
            low = mid + 1;
        }
    }

    low.max(1)
}

fn generate_degree_sequence(
    rng: &mut StdRng,
    n: usize,
    tau1: f64,
    min_degree: usize,
    max_degree: usize,
) -> Vec<usize> {
    let mut degrees: Vec<usize> = (0..n)
        .map(|_| powerlaw_sample(rng, tau1, min_degree, max_degree))
        .collect();

    let sum: usize = degrees.iter().sum();
    if sum % 2 != 0 {
        degrees[0] += 1;
    }

    degrees
}

fn generate_community_sizes(
    rng: &mut StdRng,
    n: usize,
    tau2: f64,
    min_comm: usize,
    max_comm: usize,
) -> Vec<usize> {
    let mut total = 0;
    let mut sizes: Vec<usize> = std::iter::from_fn(|| {
        if total >= n {
            return None;
        }
        let remaining = n - total;
        let effective_max = std::cmp::min(remaining, max_comm);
        let effective_min = std::cmp::min(min_comm, remaining);
        let s = powerlaw_sample(rng, tau2, effective_min, effective_max).min(remaining);
        total += s;
        Some(s)
    })
    .collect();

    while sizes.len() > 1 && *sizes.last().expect("sizes is non-empty (loop guard)") < min_comm {
        let last = sizes.pop().expect("sizes is non-empty (loop guard ensures len > 1)");
        *sizes.last_mut().expect("sizes has at least one element after pop") += last;
    }

    sizes
}

fn assign_communities(
    degree_seq: &[usize],
    community_sizes: &[usize],
    mu: f64,
    rng: &mut StdRng,
) -> Result<Vec<usize>> {
    let n = degree_seq.len();
    if n == 0 {
        return Ok(Vec::new());
    }
    let num_comms = community_sizes.len();
    let mut community_members: Vec<Vec<usize>> = vec![Vec::new(); num_comms];
    let mut node_community: Vec<usize> = vec![0; n];
    let mut unassigned: Vec<usize> = (0..n).collect();
    unassigned.shuffle(rng);

    while let Some(node) = unassigned.pop() {
        let intra_degree = ((1.0 - mu) * degree_seq[node] as f64).round() as usize;

        let mut assigned = false;
        for _ in 0..num_comms {
            let c = rng.random_range(0..num_comms);
            if community_members[c].len() < community_sizes[c] && community_sizes[c] > intra_degree
            {
                community_members[c].push(node);
                node_community[node] = c;
                assigned = true;
                break;
            }
        }

        if !assigned {
            for c in 0..num_comms {
                if community_members[c].len() < community_sizes[c]
                    && community_sizes[c] > intra_degree
                {
                    community_members[c].push(node);
                    node_community[node] = c;
                    assigned = true;
                    break;
                }
            }
        }

        if !assigned {
            // Fallback: pick the community with the most remaining capacity
            // that is also large enough for the node's intra-degree.
            let best = community_sizes
                .iter()
                .enumerate()
                .filter(|(c, s)| community_members[*c].len() < **s && **s > intra_degree)
                .max_by_key(|(_, s)| **s)
                .map(|(i, _)| i);

            match best {
                Some(c) => {
                    community_members[c].push(node);
                    node_community[node] = c;
                }
                None => {
                    return Err(LeidenError::InvalidParameter {
                        message: format!(
                            "cannot assign node {} (intra_degree={}) to any community; \
                             consider increasing min_community or decreasing max_degree",
                            node, intra_degree
                        ),
                    });
                }
            }
        }
    }

    Ok(node_community)
}

fn wire_edges(
    n: usize,
    degree_seq: &[usize],
    communities: &[usize],
    mu: f64,
    rng: &mut StdRng,
) -> Result<Vec<(usize, usize, f64)>> {
    let mut edges = Vec::new();
    let mut adjacency: FxHashSet<(usize, usize)> = FxHashSet::default();

    let num_comms = communities.iter().copied().max().unwrap_or(0) + 1;
    let mut by_community: Vec<Vec<usize>> = vec![Vec::new(); num_comms];
    for node in 0..n {
        by_community[communities[node]].push(node);
    }

    // Precompute cumulative community sizes for two-level weighted selection.
    // comm_cumulative[c] = sum of sizes of communities 0..c
    let mut comm_cumulative: Vec<usize> = Vec::with_capacity(num_comms + 1);
    comm_cumulative.push(0);
    for members in &by_community {
        comm_cumulative.push(comm_cumulative.last().copied().unwrap_or(0) + members.len());
    }

    for node in 0..n {
        let total_degree = degree_seq[node];
        let intra_degree = ((1.0 - mu) * total_degree as f64).round() as usize;
        let inter_degree = total_degree.saturating_sub(intra_degree);
        let comm = communities[node];
        let comm_size = by_community[comm].len();

        let same_community: Vec<usize> = by_community
            .get(comm)
            .map(|v| v.iter().filter(|&&v| v != node).copied().collect())
            .unwrap_or_default();

        let mut wired = 0;
        for _ in 0..intra_degree * 10 {
            if wired >= intra_degree || same_community.is_empty() {
                break;
            }
            let v = same_community[rng.random_range(0..same_community.len())];
            let key = if node < v { (node, v) } else { (v, node) };
            if adjacency.insert(key) {
                edges.push((node, v, 1.0));
                wired += 1;
            }
        }

        let total_inter = n - comm_size;
        let mut wired = 0;
        for _ in 0..inter_degree * 10 {
            if wired >= inter_degree || total_inter == 0 {
                break;
            }
            let v = pick_inter_node(
                comm,
                comm_size,
                &by_community,
                &comm_cumulative,
                total_inter,
                rng,
            );
            let key = if node < v { (node, v) } else { (v, node) };
            if adjacency.insert(key) {
                edges.push((node, v, 1.0));
                wired += 1;
            }
        }
    }

    // Post-wiring validation: check actual degrees vs targets.
    let mut actual_degree = vec![0usize; n];
    for &(u, v, _) in &edges {
        actual_degree[u] += 1;
        actual_degree[v] += 1;
    }
    for node in 0..n {
        let target = degree_seq[node];
        let actual = actual_degree[node];
        if actual < target.saturating_sub(2) {
            return Err(LeidenError::InvalidParameter {
                message: format!(
                    "node {} degree shortfall: target={}, actual={}; \
                     community may be too small for the requested intra-degree",
                    node, target, actual
                ),
            });
        }
    }

    Ok(edges)
}

/// Pick a random node from a community different from `comm` using skip-zone
/// mapping over the precomputed cumulative sizes. Zero retries, O(log C).
fn pick_inter_node(
    comm: usize,
    comm_size: usize,
    by_community: &[Vec<usize>],
    comm_cumulative: &[usize],
    total_inter: usize,
    rng: &mut StdRng,
) -> usize {
    let r = rng.random_range(0..total_inter);
    let actual_idx = if r < comm_cumulative[comm] {
        r
    } else {
        r + comm_size
    };
    let target_comm = comm_cumulative[1..].partition_point(|&c| c <= actual_idx);
    let offset = actual_idx - comm_cumulative[target_comm];
    by_community[target_comm][offset]
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashSet;

    #[test]
    fn test_lfr_basic() {
        let config = LfrConfig {
            n: 100,
            tau1: 3.0,
            tau2: 1.5,
            mu: 0.1,
            average_degree: Some(5.0),
            min_degree: None,
            max_degree: None,
            min_community: Some(10),
            max_community: None,
            seed: Some(42),
        };
        let graph = generate_lfr_graph(config).unwrap();
        assert_eq!(graph.node_count, 100);
        assert!(!graph.edges.is_empty());
        assert_eq!(graph.ground_truth.len(), 100);
        let unique_comms: HashSet<usize> = graph.ground_truth.iter().copied().collect();
        assert!(unique_comms.len() >= 2);
    }

    #[test]
    fn test_lfr_with_leiden() {
        use crate::{Leiden, LeidenConfig};

        let config = LfrConfig {
            n: 250,
            tau1: 3.0,
            tau2: 1.5,
            mu: 0.1,
            average_degree: Some(5.0),
            min_degree: None,
            max_degree: None,
            min_community: Some(20),
            max_community: None,
            seed: Some(42),
        };
        let lfr = generate_lfr_graph(config).unwrap();

        let mut builder = crate::graph::GraphDataBuilder::new(lfr.node_count);
        for &(u, v, w) in &lfr.edges {
            if u < lfr.node_count && v < lfr.node_count && u != v {
                builder.add_edge(u, v, w).unwrap();
            }
        }
        let graph_data = builder.build().unwrap();

        let leiden_config = LeidenConfig {
            seed: Some(42),
            ..Default::default()
        };
        let result = Leiden::new(leiden_config).run(&graph_data).unwrap();

        assert!(
            result.partition.num_communities() >= 2,
            "expected at least 2 communities, got {}",
            result.partition.num_communities()
        );
    }

    #[test]
    fn test_lfr_invalid_params() {
        let config = LfrConfig {
            n: 100,
            tau1: 0.5,
            tau2: 1.5,
            mu: 0.1,
            average_degree: Some(5.0),
            min_degree: None,
            max_degree: None,
            min_community: None,
            max_community: None,
            seed: Some(42),
        };
        assert!(generate_lfr_graph(config).is_err());
    }

    #[test]
    fn test_lfr_invalid_mu() {
        let config = LfrConfig {
            n: 100,
            tau1: 3.0,
            tau2: 1.5,
            mu: 1.5,
            average_degree: Some(5.0),
            min_degree: None,
            max_degree: None,
            min_community: None,
            max_community: None,
            seed: Some(42),
        };
        assert!(generate_lfr_graph(config).is_err());
    }

    #[test]
    fn test_lfr_zero_nodes() {
        let config = LfrConfig {
            n: 0,
            tau1: 3.0,
            tau2: 1.5,
            mu: 0.1,
            average_degree: None,
            min_degree: Some(5),
            max_degree: None,
            min_community: None,
            max_community: None,
            seed: Some(42),
        };
        assert!(generate_lfr_graph(config).is_err());
    }

    #[test]
    fn test_lfr_deterministic_with_seed() {
        let config1 = LfrConfig {
            n: 50,
            tau1: 3.0,
            tau2: 1.5,
            mu: 0.1,
            average_degree: Some(5.0),
            min_degree: None,
            max_degree: None,
            min_community: Some(10),
            max_community: None,
            seed: Some(123),
        };
        let config2 = LfrConfig {
            n: 50,
            tau1: 3.0,
            tau2: 1.5,
            mu: 0.1,
            average_degree: Some(5.0),
            min_degree: None,
            max_degree: None,
            min_community: Some(10),
            max_community: None,
            seed: Some(123),
        };
        let g1 = generate_lfr_graph(config1).unwrap();
        let g2 = generate_lfr_graph(config2).unwrap();

        assert_eq!(g1.edges, g2.edges);
        assert_eq!(g1.ground_truth, g2.ground_truth);
        assert_eq!(g1.community_sizes, g2.community_sizes);
    }

    #[test]
    fn test_lfr_community_sizes_sum_to_n() {
        let config = LfrConfig {
            n: 200,
            tau1: 3.0,
            tau2: 1.5,
            mu: 0.1,
            average_degree: Some(5.0),
            min_degree: None,
            max_degree: None,
            min_community: Some(10),
            max_community: None,
            seed: Some(42),
        };
        let graph = generate_lfr_graph(config).unwrap();
        let total: usize = graph.community_sizes.iter().sum();
        assert_eq!(total, graph.node_count);
    }

    #[test]
    fn test_lfr_conflicting_max_degree_and_min_community() {
        let config = LfrConfig {
            n: 100,
            tau1: 3.0,
            tau2: 1.5,
            mu: 0.1,
            average_degree: None,
            min_degree: None,
            max_degree: Some(50),
            min_community: Some(10),
            max_community: None,
            seed: Some(42),
        };
        let err = generate_lfr_graph(config).unwrap_err();
        assert!(
            err.to_string().contains("max intra-degree"),
            "expected intra-degree validation error, got: {err}",
        );
    }

    #[test]
    fn test_lfr_degree_capping_prevents_shortfall() {
        let config = LfrConfig {
            n: 250,
            tau1: 3.0,
            tau2: 1.5,
            mu: 0.1,
            average_degree: Some(5.0),
            min_degree: None,
            max_degree: None,
            min_community: Some(20),
            max_community: None,
            seed: Some(42),
        };
        let lfr = generate_lfr_graph(config).unwrap();

        let mut actual_degree = vec![0usize; lfr.node_count];
        for &(u, v, _) in &lfr.edges {
            actual_degree[u] += 1;
            actual_degree[v] += 1;
        }
        for (node, &deg) in actual_degree.iter().enumerate() {
            assert!(
                deg > 0 || lfr.node_count <= 1,
                "node {} has zero degree after wiring",
                node,
            );
        }
    }

    #[test]
    fn test_lfr_parity_respects_degree_cap() {
        let config = LfrConfig {
            n: 50,
            tau1: 3.0,
            tau2: 1.5,
            mu: 0.1,
            average_degree: Some(2.0),
            min_degree: None,
            max_degree: None,
            min_community: Some(15),
            max_community: None,
            seed: Some(42),
        };
        let lfr = generate_lfr_graph(config).expect("LFR generation should succeed");
        let degree_cap = lfr.community_sizes.iter().min().copied().unwrap_or(10) - 1;
        let mut actual_degree = vec![0usize; lfr.node_count];
        for &(u, v, _) in &lfr.edges {
            actual_degree[u] += 1;
            actual_degree[v] += 1;
        }
        for (node, &degree) in actual_degree.iter().enumerate() {
            assert!(
                degree <= degree_cap,
                "node {} degree {} exceeds cap {} (min_comm={:?})",
                node,
                degree,
                degree_cap,
                lfr.community_sizes.iter().min(),
            );
        }
    }

    #[test]
    fn test_lfr_assign_communities_respects_community_sizes() {
        let config = LfrConfig {
            n: 200,
            tau1: 3.0,
            tau2: 1.5,
            mu: 0.2,
            average_degree: Some(5.0),
            min_degree: None,
            max_degree: None,
            min_community: Some(20),
            max_community: None,
            seed: Some(42),
        };
        let lfr = generate_lfr_graph(config).unwrap();

        let mut comm_counts = vec![0usize; lfr.community_sizes.len()];
        for &c in &lfr.ground_truth {
            assert!(
                c < lfr.community_sizes.len(),
                "community {} out of range",
                c
            );
            comm_counts[c] += 1;
        }
        for (c, &count) in comm_counts.iter().enumerate() {
            assert_eq!(
                count, lfr.community_sizes[c],
                "community {} has {} nodes but expected {}",
                c, count, lfr.community_sizes[c],
            );
        }
    }

    #[test]
    fn test_lfr_unwrap_replacements() {
        // Verify the community size loop handles edge cases without panicking
        // when degree_cap (min_comm - 1) is smaller than average_degree.
        let config = LfrConfig {
            n: 200,
            tau1: 3.0,
            tau2: 1.5,
            mu: 0.1,
            average_degree: Some(15.0),
            min_degree: None,
            max_degree: None,
            min_community: Some(5),
            max_community: Some(10),
            seed: Some(42),
        };
        let lfr = generate_lfr_graph(config).unwrap();
        assert_eq!(lfr.node_count, 200);
        assert!(!lfr.edges.is_empty());
        assert_eq!(lfr.ground_truth.len(), 200);
        let unique_comms: HashSet<usize> = lfr.ground_truth.iter().copied().collect();
        assert!(unique_comms.len() >= 2);
    }
}
