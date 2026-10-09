//! Leiden community detection over the vendored `leiden-rs`.
//!
//! The Python engines call `leidenalg.find_partition(graph,
//! RBConfigurationVertexPartition, resolution_parameter=γ, weights="weight",
//! seed=…)`. No other implementation reproduces leidenalg bit for bit
//! (ADR-0026 decision 6), so a port's gate is partition QUALITY (modularity,
//! agreement), not equality. Semantic gaps to leidenalg 0.12, documented
//! because they cannot be closed through the `leiden-rs` API:
//!
//! * leidenalg's `optimise_partition(n_iterations=2)` runs the whole
//!   multi-level Leiden pass twice, the second from the first's result. One
//!   `Leiden::run` is one such pass (it aggregates until local moving changes
//!   nothing), so [`partition`] runs it, then `run_with_initial_partition`
//!   from its result: two passes.
//! * Both move and refine greedily (best gain) in a random node order, but
//!   the orders come from different generators (igraph's RNG vs `StdRng`
//!   seeded with the same number) and different queue disciplines, so the
//!   same seed gives a different, equally valid partition.
//! * leidenalg stops a pass when aggregation no longer shrinks the graph;
//!   `leiden-rs` when local moving changes nothing or the quality gain is
//!   below 1e-10. Both converge to a local optimum of the same objective.
//! * leidenalg numbers communities by size, ties by its internal index;
//!   here ties go to the community holding the lowest vertex
//!   ([`number_by_size`]).

use leiden_rs::{GraphDataBuilder, Leiden, LeidenConfig, QualityType};
use std::collections::HashMap;

/// One call: an undirected weighted graph on vertices `0..vertices`.
#[derive(Debug, Clone, Copy)]
pub struct LeidenRequest<'a> {
    pub vertices: usize,
    /// `(u, v, weight)`, one per undirected edge; `u == v` is a self-loop.
    pub edges: &'a [(usize, usize, f64)],
    /// γ of the RB-configuration objective (1.0 is modularity).
    pub resolution: f64,
    pub seed: u64,
}

/// `leiden-rs` refused the graph or the run (its own message).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LeidenError(pub String);

impl std::fmt::Display for LeidenError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(&self.0)
    }
}

impl std::error::Error for LeidenError {}

/// A membership — community id per vertex — for `request`: two seeded
/// Leiden passes, single-threaded, numbered by [`number_by_size`].
///
/// # Errors
///
/// An edge naming a vertex outside the graph, a bad weight, or a failed run.
pub fn partition(request: &LeidenRequest<'_>) -> Result<Vec<usize>, LeidenError> {
    let error = |e: leiden_rs::LeidenError| LeidenError(e.to_string());
    let mut builder = GraphDataBuilder::new(request.vertices);
    for &(u, v, weight) in request.edges {
        builder.add_edge(u, v, weight).map_err(error)?;
    }
    let graph = builder.build().map_err(error)?;
    let config = LeidenConfig {
        resolution: request.resolution,
        seed: Some(request.seed),
        quality: QualityType::RBConfiguration,
        ..LeidenConfig::default()
    };
    let leiden = Leiden::new(config);
    let first = leiden.run(&graph).map_err(error)?;
    let second = leiden
        .run_with_initial_partition(&graph, first.partition)
        .map_err(error)?;
    Ok(number_by_size(second.partition.as_slice()))
}

/// Renumber a membership: largest community 0, ties to the community
/// holding the lowest vertex.
#[must_use]
pub fn number_by_size(membership: &[usize]) -> Vec<usize> {
    // (size, first vertex) per label, in first-vertex order.
    let mut seen: HashMap<usize, usize> = HashMap::new();
    let mut groups: Vec<(usize, usize)> = Vec::new();
    for (vertex, label) in membership.iter().enumerate() {
        let slot = *seen.entry(*label).or_insert_with(|| {
            groups.push((0, vertex));
            groups.len() - 1
        });
        groups[slot].0 += 1;
    }
    let mut order: Vec<usize> = (0..groups.len()).collect();
    order.sort_by(|a, b| {
        groups[*b]
            .0
            .cmp(&groups[*a].0)
            .then(groups[*a].1.cmp(&groups[*b].1))
    });
    let mut new_id = vec![0; groups.len()];
    for (rank, slot) in order.into_iter().enumerate() {
        new_id[slot] = rank;
    }
    membership.iter().map(|label| new_id[seen[label]]).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn two_triangles() -> Vec<(usize, usize, f64)> {
        vec![
            (0, 1, 1.0),
            (1, 2, 1.0),
            (0, 2, 1.0),
            (3, 4, 1.0),
            (4, 5, 1.0),
            (3, 5, 1.0),
            (2, 3, 0.1),
        ]
    }

    #[test]
    fn number_by_size_puts_the_largest_first_and_breaks_ties_by_first_vertex() {
        assert_eq!(
            number_by_size(&[7, 3, 3, 7, 9, 9, 9]),
            vec![1, 2, 2, 1, 0, 0, 0]
        );
        assert_eq!(number_by_size(&[]), Vec::<usize>::new());
    }

    #[test]
    fn two_triangles_split_and_a_seed_repeats() {
        let edges = two_triangles();
        let request = LeidenRequest {
            vertices: 6,
            edges: &edges,
            resolution: 1.0,
            seed: 42,
        };
        let first = partition(&request).unwrap_or_else(|e| panic!("{e}"));
        assert_eq!(first.len(), 6);
        assert_eq!(first[0], first[1]);
        assert_eq!(first[1], first[2]);
        assert_eq!(first[3], first[4]);
        assert_eq!(first[4], first[5]);
        assert_ne!(first[0], first[3]);
        for _ in 0..5 {
            assert_eq!(partition(&request).ok(), Some(first.clone()));
        }
    }

    #[test]
    fn an_edge_outside_the_graph_is_refused() {
        let edges = [(0, 7, 1.0)];
        let request = LeidenRequest {
            vertices: 2,
            edges: &edges,
            resolution: 1.0,
            seed: 1,
        };
        assert!(partition(&request).is_err());
    }
}
