//! The undirected weighted multigraph `_nx_to_igraph` builds, and the igraph
//! measures `_build_community_data` takes over it: `PageRank`, betweenness,
//! strength and modularity.
//!
//! Every measure is computed on the WHOLE graph, once (Python's "pre-compute
//! global centrality arrays once"); a community's centroids read their
//! members' entries.
//!
//! igraph semantics kept, because they change the numbers:
//!
//! * the graph is a **multigraph**: `as_undirected(mode="each")` keeps one
//!   undirected edge per directed edge, so `a → b` plus `b → a` is two
//!   parallel edges. Strength and `PageRank` see their summed weight;
//!   betweenness counts a shortest path once per parallel edge it can take
//!   (networkx, which collapses parallel edges, gives different numbers);
//! * a self-loop counts twice in a vertex's strength and in `PageRank`'s
//!   transition weights, once in a community's internal weight;
//! * betweenness reads `1 / weight` as the edge length, compares path
//!   lengths with igraph's relative epsilon (1e-10) and halves the result
//!   (undirected, each pair once);
//! * `PageRank` is igraph's default (PRPACK): damping 0.85, transitions
//!   proportional to edge weight, a vertex without edges spreads its rank
//!   evenly over all vertices. PRPACK solves the system directly; here it is
//!   iterated to 1e-15, which agrees to better than 1e-12.

use std::cmp::Ordering;
use std::collections::BinaryHeap;

/// `PageRank`'s damping factor (igraph's default).
const DAMPING: f64 = 0.85;
/// The power iteration stops once an iteration moves the vector less than
/// this (L1).
const PAGERANK_TOLERANCE: f64 = 1e-15;
/// …or after this many iterations.
const PAGERANK_MAX_ITERATIONS: usize = 10_000;
/// igraph's `IGRAPH_SHORTEST_PATH_EPSILON`: two path lengths this close
/// (relatively) are equal.
const PATH_EPSILON: f64 = 1e-10;

/// An undirected weighted multigraph on vertices `0..vertices`.
#[derive(Debug, Clone, Default, PartialEq)]
pub(crate) struct Undirected {
    pub(crate) vertices: usize,
    /// `(u, v, weight)`, one per directed edge of the source graph, in
    /// its edge order; `u == v` is a self-loop.
    pub(crate) edges: Vec<(usize, usize, f64)>,
}

impl Undirected {
    /// Per vertex, its incident edges `(other end, weight)`; a self-loop
    /// is listed twice, as igraph's incidence lists list it.
    fn adjacency(&self) -> Vec<Vec<(usize, f64)>> {
        let mut adjacency = vec![Vec::new(); self.vertices];
        for &(u, v, weight) in &self.edges {
            adjacency[u].push((v, weight));
            adjacency[v].push((u, weight));
        }
        adjacency
    }

    /// `Graph.strength(weights="weight")`: the summed weight of a vertex's
    /// edges, a self-loop twice.
    pub(crate) fn strength(&self) -> Vec<f64> {
        let mut strength = vec![0.0; self.vertices];
        for &(u, v, weight) in &self.edges {
            strength[u] += weight;
            strength[v] += weight;
        }
        strength
    }

    /// `Graph.pagerank(weights="weight")`.
    #[allow(clippy::cast_precision_loss, reason = "a vertex count")]
    pub(crate) fn pagerank(&self) -> Vec<f64> {
        let n = self.vertices;
        if n == 0 {
            return Vec::new();
        }
        let adjacency = self.adjacency();
        let strength = self.strength();
        let uniform = 1.0 / n as f64;
        let mut rank = vec![uniform; n];
        for _ in 0..PAGERANK_MAX_ITERATIONS {
            let dangling: f64 = (0..n)
                .filter(|&vertex| strength[vertex] <= 0.0)
                .map(|vertex| rank[vertex])
                .sum();
            let base = (1.0 - DAMPING) * uniform + DAMPING * dangling * uniform;
            let mut next = vec![base; n];
            for (vertex, edges) in adjacency.iter().enumerate() {
                if strength[vertex] <= 0.0 {
                    continue;
                }
                let share = DAMPING * rank[vertex] / strength[vertex];
                for &(other, weight) in edges {
                    next[other] += share * weight;
                }
            }
            let total: f64 = next.iter().sum();
            if total > 0.0 {
                for value in &mut next {
                    *value /= total;
                }
            }
            let change: f64 = next.iter().zip(&rank).map(|(a, b)| (a - b).abs()).sum();
            rank = next;
            if change < PAGERANK_TOLERANCE {
                break;
            }
        }
        rank
    }

    /// `Graph.betweenness(weights=[1 / w for each edge])` (Brandes over
    /// Dijkstra), with a non-positive weight read as length 1 as Python
    /// does.
    pub(crate) fn betweenness(&self) -> Vec<f64> {
        let n = self.vertices;
        let mut adjacency: Vec<Vec<(usize, f64)>> = vec![Vec::new(); n];
        for &(u, v, weight) in &self.edges {
            if u == v {
                // A self-loop is never on a shortest path.
                continue;
            }
            let length = if weight > 0.0 { 1.0 / weight } else { 1.0 };
            adjacency[u].push((v, length));
            adjacency[v].push((u, length));
        }
        let mut score = vec![0.0; n];
        let mut distance = vec![f64::INFINITY; n];
        let mut paths = vec![0.0_f64; n];
        let mut predecessors: Vec<Vec<usize>> = vec![Vec::new(); n];
        let mut settled = vec![false; n];
        let mut dependency = vec![0.0_f64; n];
        let mut order = Vec::with_capacity(n);
        for source in 0..n {
            distance.fill(f64::INFINITY);
            paths.fill(0.0);
            settled.fill(false);
            dependency.fill(0.0);
            for list in &mut predecessors {
                list.clear();
            }
            order.clear();
            distance[source] = 0.0;
            paths[source] = 1.0;
            let mut queue = BinaryHeap::new();
            queue.push(Pending {
                distance: 0.0,
                vertex: source,
            });
            while let Some(Pending { vertex, .. }) = queue.pop() {
                if settled[vertex] {
                    continue;
                }
                settled[vertex] = true;
                order.push(vertex);
                for &(other, length) in &adjacency[vertex] {
                    if settled[other] {
                        continue;
                    }
                    let candidate = distance[vertex] + length;
                    match compare(candidate, distance[other]) {
                        Ordering::Less => {
                            distance[other] = candidate;
                            paths[other] = paths[vertex];
                            predecessors[other].clear();
                            predecessors[other].push(vertex);
                            queue.push(Pending {
                                distance: candidate,
                                vertex: other,
                            });
                        }
                        Ordering::Equal => {
                            paths[other] += paths[vertex];
                            predecessors[other].push(vertex);
                        }
                        Ordering::Greater => {}
                    }
                }
            }
            for &vertex in order.iter().rev() {
                for &predecessor in &predecessors[vertex] {
                    dependency[predecessor] +=
                        paths[predecessor] / paths[vertex] * (1.0 + dependency[vertex]);
                }
                if vertex != source {
                    score[vertex] += dependency[vertex];
                }
            }
        }
        for value in &mut score {
            *value /= 2.0;
        }
        score
    }

    /// `Graph.modularity(membership, weights="weight", resolution=γ)`:
    /// `Σ_c (e_c − γ k_c² / 2m) / 2m`, `e_c` twice the weight inside `c`
    /// and `k_c` its summed strength. `NaN` without edge weight, as igraph.
    pub(crate) fn modularity(&self, membership: &[usize], resolution: f64) -> f64 {
        let communities = membership.iter().max().map_or(0, |last| last + 1);
        let mut inside = vec![0.0; communities];
        let mut strength = vec![0.0; communities];
        let mut total = 0.0;
        for &(u, v, weight) in &self.edges {
            let (a, b) = (membership[u], membership[v]);
            if a == b {
                inside[a] += 2.0 * weight;
            }
            strength[a] += weight;
            strength[b] += weight;
            total += weight;
        }
        if total <= 0.0 {
            return f64::NAN;
        }
        let mut modularity = 0.0;
        for (e, k) in inside.iter().zip(&strength) {
            modularity += e;
            modularity -= resolution * k * k / (2.0 * total);
        }
        modularity / (2.0 * total)
    }
}

/// `igraph_cmp_epsilon`: `a` against `b`, equal within a relative epsilon.
fn compare(a: f64, b: f64) -> Ordering {
    if b.is_infinite() {
        return a.total_cmp(&b);
    }
    let difference = a - b;
    let scale = a.abs() + b.abs();
    if difference.abs() < PATH_EPSILON * scale.max(f64::MIN_POSITIVE) {
        Ordering::Equal
    } else {
        a.total_cmp(&b)
    }
}

/// A Dijkstra queue entry, nearest first.
#[derive(Debug, Clone, Copy)]
struct Pending {
    distance: f64,
    vertex: usize,
}

impl PartialEq for Pending {
    fn eq(&self, other: &Self) -> bool {
        self.cmp(other) == Ordering::Equal
    }
}

impl Eq for Pending {}

impl PartialOrd for Pending {
    fn partial_cmp(&self, other: &Self) -> Option<Ordering> {
        Some(self.cmp(other))
    }
}

impl Ord for Pending {
    fn cmp(&self, other: &Self) -> Ordering {
        // Reversed: `BinaryHeap` is a max-heap.
        other
            .distance
            .total_cmp(&self.distance)
            .then(other.vertex.cmp(&self.vertex))
    }
}
