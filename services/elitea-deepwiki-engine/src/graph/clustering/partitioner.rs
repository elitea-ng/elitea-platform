//! Community detection behind one trait.
//!
//! Python calls `leidenalg.find_partition(graph,
//! RBConfigurationVertexPartition, resolution_parameter=γ,
//! weights="weight", seed=42)` twice per Phase 3 level. No other
//! implementation reproduces leidenalg bit for bit (ADR-0026 decision 6), so
//! the call is a [`Partitioner`]:
//!
//! * [`LeidenPartitioner`] — the production one: the shared
//!   `elitea-graph-algos` Leiden (the vendored `leiden-rs`, two seeded
//!   passes; its module docs list the gaps to leidenalg). Its gate is
//!   partition QUALITY (modularity, agreement), not equality.
//! * [`ReplayPartitioner`] — parity only: it answers with the memberships
//!   leidenalg returned for the same input, recorded by
//!   `parity/python_phase3_dump.py`. With it, every other step of Phase 3
//!   must give Python's result exactly.

pub use elitea_graph_algos::leiden::number_by_size;
use elitea_graph_algos::leiden::{self, LeidenRequest};
use std::collections::HashMap;

/// The Phase 3 pass a call belongs to.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Level {
    /// Pass 1: the file graph.
    Section,
    /// Pass 2: one section's node graph.
    Page,
}

/// One partition call: an undirected weighted graph whose vertex `i` is
/// `names[i]` (sorted, as `_nx_to_igraph` orders them).
#[derive(Debug, Clone, Copy)]
pub struct PartitionRequest<'a> {
    pub level: Level,
    pub names: &'a [&'a str],
    /// `(u, v, weight)`, one per undirected edge; `u == v` is a self-loop.
    pub edges: &'a [(usize, usize, f64)],
    pub resolution: f64,
    pub seed: u64,
}

/// A partition call that failed.
#[derive(Debug, Clone, thiserror::Error, PartialEq)]
pub enum PartitionError {
    #[error("leiden: {0}")]
    Leiden(String),
    #[error("no recorded {level:?} partition for a graph of {nodes} vertices starting {first:?}")]
    NotRecorded {
        level: Level,
        nodes: usize,
        first: String,
    },
    #[error("recorded {level:?} graph differs from the request: {detail}")]
    InputMismatch { level: Level, detail: String },
    #[error("partitioner returned {got} labels for {expected} vertices")]
    WrongLength { got: usize, expected: usize },
}

/// Community detection: a membership (community id per vertex) for a
/// request.
///
/// Contract, as leidenalg: ids are dense `0..k`, numbered by community size,
/// largest first. Phase 3 persists section ids as they come and breaks
/// ties by them, so the numbering is part of the result.
pub trait Partitioner {
    /// # Errors
    ///
    /// When the implementation cannot partition the graph.
    fn partition(&mut self, request: &PartitionRequest<'_>) -> Result<Vec<usize>, PartitionError>;
}

/// The production partitioner: the shared Leiden
/// ([`elitea_graph_algos::leiden::partition`]) — RB-configuration objective
/// (modularity with resolution γ), edge weights, the request's seed,
/// single-threaded, two passes, communities numbered by size.
#[derive(Debug, Clone, Copy, Default)]
pub struct LeidenPartitioner;

impl Partitioner for LeidenPartitioner {
    fn partition(&mut self, request: &PartitionRequest<'_>) -> Result<Vec<usize>, PartitionError> {
        leiden::partition(&LeidenRequest {
            vertices: request.names.len(),
            edges: request.edges,
            resolution: request.resolution,
            seed: request.seed,
        })
        .map_err(|e| PartitionError::Leiden(e.0))
    }
}

/// One recorded leidenalg call.
#[derive(Debug, Clone, serde::Deserialize)]
pub struct RecordedCall {
    pub level: Level,
    pub nodes: Vec<String>,
    /// `[u, v, weight]` per igraph edge.
    pub edges: Vec<(usize, usize, f64)>,
    pub resolution: f64,
    pub seed: u64,
    pub membership: Vec<usize>,
}

/// Parity only: answers each request with the membership leidenalg returned
/// for the same graph (looked up by level and vertex names), after checking
/// that the recorded graph is the requested one.
///
/// Edges are compared as sets of unordered pairs; weights within a relative
/// 1e-9, because Python sums parallel edge weights in a hash-dependent order
/// (`G.subgraph(small_set)` iterates the set), which moves the last bits.
#[derive(Debug, Default)]
pub struct ReplayPartitioner {
    calls: HashMap<(Level, Vec<String>), RecordedCall>,
    /// The largest relative weight difference seen.
    pub max_weight_difference: f64,
    /// Calls answered.
    pub replayed: usize,
}

impl ReplayPartitioner {
    #[must_use]
    pub fn new(calls: Vec<RecordedCall>) -> Self {
        let calls = calls
            .into_iter()
            .map(|call| ((call.level, call.nodes.clone()), call))
            .collect();
        Self {
            calls,
            max_weight_difference: 0.0,
            replayed: 0,
        }
    }
}

impl Partitioner for ReplayPartitioner {
    fn partition(&mut self, request: &PartitionRequest<'_>) -> Result<Vec<usize>, PartitionError> {
        let key = (
            request.level,
            request
                .names
                .iter()
                .map(|n| (*n).to_owned())
                .collect::<Vec<_>>(),
        );
        let Some(call) = self.calls.get(&key) else {
            return Err(PartitionError::NotRecorded {
                level: request.level,
                nodes: request.names.len(),
                first: request
                    .names
                    .first()
                    .map(|n| (*n).to_owned())
                    .unwrap_or_default(),
            });
        };
        let mismatch = |detail: String| PartitionError::InputMismatch {
            level: request.level,
            detail,
        };
        if call.resolution.to_bits() != request.resolution.to_bits() || call.seed != request.seed {
            return Err(mismatch(format!(
                "resolution/seed {}/{} vs {}/{}",
                call.resolution, call.seed, request.resolution, request.seed
            )));
        }
        let canonical = |edges: &[(usize, usize, f64)]| {
            let mut out: Vec<(usize, usize, f64)> = edges
                .iter()
                .map(|&(u, v, w)| (u.min(v), u.max(v), w))
                .collect();
            out.sort_by_key(|e| (e.0, e.1));
            out
        };
        let recorded = canonical(&call.edges);
        let requested = canonical(request.edges);
        if recorded.len() != requested.len() {
            return Err(mismatch(format!(
                "{} edges vs {}",
                recorded.len(),
                requested.len()
            )));
        }
        for (a, b) in recorded.iter().zip(&requested) {
            if (a.0, a.1) != (b.0, b.1) {
                return Err(mismatch(format!(
                    "edge {:?} vs {:?}",
                    (a.0, a.1),
                    (b.0, b.1)
                )));
            }
            let difference = (a.2 - b.2).abs() / a.2.abs().max(1.0);
            if difference > 1e-9 {
                return Err(mismatch(format!(
                    "weight {} vs {} on {:?}",
                    a.2,
                    b.2,
                    (a.0, a.1)
                )));
            }
            self.max_weight_difference = self.max_weight_difference.max(difference);
        }
        if call.membership.len() != request.names.len() {
            return Err(PartitionError::WrongLength {
                got: call.membership.len(),
                expected: request.names.len(),
            });
        }
        self.replayed += 1;
        Ok(call.membership.clone())
    }
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
    fn leiden_splits_two_triangles_and_is_deterministic() {
        let names = ["a", "b", "c", "d", "e", "f"];
        let edges = two_triangles();
        let request = PartitionRequest {
            level: Level::Page,
            names: &names,
            edges: &edges,
            resolution: 1.0,
            seed: 42,
        };
        let first = LeidenPartitioner.partition(&request).unwrap();
        assert_eq!(first.len(), 6);
        assert_eq!(first[0], first[1]);
        assert_eq!(first[1], first[2]);
        assert_eq!(first[3], first[4]);
        assert_ne!(first[0], first[3]);
        for _ in 0..5 {
            assert_eq!(LeidenPartitioner.partition(&request).unwrap(), first);
        }
    }

    #[test]
    fn replay_answers_the_recorded_membership_and_checks_the_graph() {
        let names = ["a", "b", "c", "d", "e", "f"];
        let edges = two_triangles();
        let recorded = RecordedCall {
            level: Level::Section,
            nodes: names.iter().map(|n| (*n).to_owned()).collect(),
            // Reversed orientation and order: still the same graph.
            edges: edges.iter().rev().map(|&(u, v, w)| (v, u, w)).collect(),
            resolution: 0.5,
            seed: 42,
            membership: vec![1, 1, 1, 0, 0, 0],
        };
        let mut replay = ReplayPartitioner::new(vec![recorded]);
        let mut request = PartitionRequest {
            level: Level::Section,
            names: &names,
            edges: &edges,
            resolution: 0.5,
            seed: 42,
        };
        assert_eq!(replay.partition(&request).unwrap(), vec![1, 1, 1, 0, 0, 0]);
        request.level = Level::Page;
        assert!(matches!(
            replay.partition(&request),
            Err(PartitionError::NotRecorded { .. })
        ));
        request.level = Level::Section;
        let heavier: Vec<_> = edges.iter().map(|&(u, v, w)| (u, v, w * 2.0)).collect();
        request.edges = &heavier;
        assert!(matches!(
            replay.partition(&request),
            Err(PartitionError::InputMismatch { .. })
        ));
    }
}
