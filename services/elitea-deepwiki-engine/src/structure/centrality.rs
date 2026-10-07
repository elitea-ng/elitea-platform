//! Central symbols of a page: `graph_clustering.select_central_symbols`
//! (`PageRank` on the page's undirected subgraph) over the graph
//! `ClusterStructurePlanner._get_cluster_graph` loads.
//!
//! The ranks feed a sort whose ties keep the subgraph's node order, and
//! pages are full of exact ties (isolated nodes, symmetric pairs), so the
//! port reproduces the arithmetic of networkx 3.6 / scipy 1.18 / numpy 2.5
//! operation for operation, not just the algorithm:
//!
//! * the subgraph's node order: networkx iterates the node LIST it was
//!   given when that list is under half the graph (`FilterAtlas`), else the
//!   graph's order; the same rule per node for its neighbours;
//! * `to_undirected`: one undirected edge per `(u, v, key)`; `u → v` and
//!   `v → u` with the same key are ONE edge whose weight is the last one
//!   written;
//! * `to_scipy_sparse_array`: the undirected edges once each, mirrored, the
//!   self-loops subtracted once; per row, entries sorted by column (stably)
//!   and duplicates summed in order;
//! * row sums as `np.add.reduceat` sums them (first value plus the
//!   pairwise sum of the rest), the convergence error as `np.sum` (pairwise,
//!   blocks of 8), `x @ A` row by row in index order;
//! * 100 iterations at most, tolerance `N · 1e-6`, damping 0.85; no
//!   convergence ranks every node equally.
//!
//! The node LIST the planner passes is a Python `set`, whose order follows
//! the hash seed. Here it is the insertion order (the Python reference pins
//! the same order to compare; see `parity/python_structure_dump.py`).

use super::index::PlannerIndex;
use indexmap::IndexMap;
use std::collections::{HashMap, HashSet};

/// `nx.pagerank`'s damping factor.
const ALPHA: f64 = 0.85;
/// `max_iter`.
const MAX_ITER: usize = 100;
/// `tol`.
const TOL: f64 = 1.0e-6;

/// The planner's `nx.MultiDiGraph`: the architectural nodes (non-test ones
/// under `exclude_tests`) in row order, and every edge between two of them
/// in row order, keyed per pair in arrival order.
#[derive(Debug, Clone, Default)]
pub struct CentralGraph {
    /// Graph position → index position.
    nodes: Vec<usize>,
    /// Index position → graph position.
    position: HashMap<usize, usize>,
    /// Per graph node: successor → the weights of its parallel edges.
    succ: Vec<IndexMap<usize, Vec<f64>>>,
}

impl CentralGraph {
    /// `_get_cluster_graph`.
    #[must_use]
    pub fn new(index: &PlannerIndex, exclude_tests: bool) -> Self {
        let mut nodes = Vec::new();
        let mut position = HashMap::new();
        for (at, node) in index.nodes().iter().enumerate() {
            if node.is_architectural && !(exclude_tests && node.is_test) {
                position.insert(at, nodes.len());
                nodes.push(at);
            }
        }
        let mut succ: Vec<IndexMap<usize, Vec<f64>>> = vec![IndexMap::new(); nodes.len()];
        for edge in index.edges() {
            if let (Some(&u), Some(&v)) = (position.get(&edge.source), position.get(&edge.target)) {
                succ[u].entry(v).or_default().push(edge.cluster_weight());
            }
        }
        Self {
            nodes,
            position,
            succ,
        }
    }

    #[must_use]
    pub fn contains(&self, index_position: usize) -> bool {
        self.position.contains_key(&index_position)
    }

    #[must_use]
    pub fn node_count(&self) -> usize {
        self.nodes.len()
    }

    /// `select_central_symbols(G, cluster_nodes, k)`: the `k` nodes of
    /// highest `PageRank` in the cluster's undirected subgraph, in
    /// descending rank (ties in subgraph order). `cluster` is the ordered
    /// node set, as index positions.
    #[must_use]
    pub fn select_central(&self, cluster: &[usize], k: usize) -> Vec<usize> {
        if cluster.is_empty() {
            return Vec::new();
        }
        let k = k.min(cluster.len());
        // `PowerIterationFailedConvergence`: every node of the cluster (in
        // or out of the graph) ranks 1 / len.
        let mut ranked = self.pagerank(cluster).unwrap_or_else(|| {
            #[allow(clippy::cast_precision_loss)]
            let uniform = 1.0 / cluster.len() as f64;
            cluster.iter().map(|&node| (node, uniform)).collect()
        });
        // `sorted(..., reverse=True)` is stable.
        ranked.sort_by(|a, b| b.1.total_cmp(&a.1));
        ranked.into_iter().take(k).map(|(node, _)| node).collect()
    }

    /// The subgraph's node order: the given order when the set is under
    /// half the graph, else graph order (networkx's `FilterAtlas`).
    fn subgraph_order(&self, ok: &[usize], ok_set: &HashSet<usize>) -> Vec<usize> {
        if 2 * ok.len() < self.nodes.len() {
            ok.to_vec()
        } else {
            (0..self.nodes.len())
                .filter(|g| ok_set.contains(g))
                .collect()
        }
    }

    /// `nx.pagerank(G.subgraph(cluster).to_undirected(), weight="weight")`
    /// as `(index position, rank)` in node order; `None` when the power
    /// iteration does not converge.
    fn pagerank(&self, cluster: &[usize]) -> Option<Vec<(usize, f64)>> {
        // `nbunch_iter`: the given nodes that are in the graph, once each.
        let mut ok = Vec::new();
        let mut ok_set = HashSet::new();
        for node in cluster {
            if let Some(&g) = self.position.get(node)
                && ok_set.insert(g)
            {
                ok.push(g);
            }
        }
        let order = self.subgraph_order(&ok, &ok_set);
        if order.is_empty() {
            return Some(Vec::new());
        }
        let undirected = self.undirected(&order, &ok, &ok_set);
        let (rows, dangling) = undirected.transition_rows();
        let ranks = power_iteration(&rows, &dangling)?;
        Some(order.iter().map(|&g| self.nodes[g]).zip(ranks).collect())
    }

    /// `subgraph.to_undirected()` (see the module docs).
    fn undirected(&self, order: &[usize], ok: &[usize], ok_set: &HashSet<usize>) -> Undirected {
        let local: HashMap<usize, usize> = order.iter().enumerate().map(|(i, &g)| (g, i)).collect();
        let mut graph = Undirected {
            adjacency: vec![IndexMap::new(); order.len()],
            keydicts: Vec::new(),
        };
        for (lu, &u) in order.iter().enumerate() {
            let successors = &self.succ[u];
            // `FilterMultiInner.__iter__`: the same shorter-side rule.
            let neighbours: Vec<usize> = if 2 * ok.len() < successors.len() {
                ok.iter()
                    .copied()
                    .filter(|v| successors.contains_key(v))
                    .collect()
            } else {
                successors
                    .keys()
                    .copied()
                    .filter(|v| ok_set.contains(v))
                    .collect()
            };
            for v in neighbours {
                let lv = local[&v];
                for (key, &weight) in successors[&v].iter().enumerate() {
                    graph.add_edge(lu, lv, key, weight);
                }
            }
        }
        graph
    }
}

/// An undirected multigraph as networkx keeps one: per node, neighbour →
/// key dict (shared by both ends), and per key dict, key → weight.
struct Undirected {
    adjacency: Vec<IndexMap<usize, usize>>,
    keydicts: Vec<IndexMap<usize, f64>>,
}

impl Undirected {
    /// `MultiGraph.add_edge(u, v, key, weight=w)`: an existing key keeps
    /// its place and takes the new weight.
    fn add_edge(&mut self, u: usize, v: usize, key: usize, weight: f64) {
        if let Some(&dict) = self.adjacency[u].get(&v) {
            self.keydicts[dict].insert(key, weight);
        } else {
            let dict = self.keydicts.len();
            let mut keys = IndexMap::new();
            keys.insert(key, weight);
            self.keydicts.push(keys);
            self.adjacency[u].insert(v, dict);
            self.adjacency[v].insert(u, dict);
        }
    }

    /// `to_scipy_sparse_array` then the row normalisation of
    /// `_pagerank_scipy`: per row, `(column, weight / row sum)` in column
    /// order, and the dangling rows (sum 0).
    fn transition_rows(&self) -> (Vec<Vec<(usize, f64)>>, Vec<usize>) {
        let n = self.adjacency.len();
        // COO in `G.edges(data=...)` order (each edge once, from the first
        // of its ends in node order), mirrored, self-loops subtracted.
        let mut half: Vec<(usize, usize, f64)> = Vec::new();
        let mut seen = vec![false; n];
        for (u, neighbours) in self.adjacency.iter().enumerate() {
            for (&v, &dict) in neighbours {
                if !seen[v] {
                    half.extend(self.keydicts[dict].values().map(|&w| (u, v, w)));
                }
            }
            seen[u] = true;
        }
        let mut rows: Vec<Vec<(usize, f64)>> = vec![Vec::new(); n];
        for &(u, v, w) in &half {
            rows[u].push((v, w));
        }
        for &(u, v, w) in &half {
            rows[v].push((u, w));
        }
        for (u, neighbours) in self.adjacency.iter().enumerate() {
            if let Some(&dict) = neighbours.get(&u) {
                rows[u].extend(self.keydicts[dict].values().map(|&w| (u, -w)));
            }
        }
        // COO → CSR keeps arrival order per row; then each row is sorted by
        // column (stably) and its duplicates summed in order.
        for row in &mut rows {
            row.sort_by_key(|&(c, _)| c);
            let mut merged: Vec<(usize, f64)> = Vec::with_capacity(row.len());
            for &(c, d) in row.iter() {
                match merged.last_mut() {
                    Some(last) if last.0 == c => last.1 += d,
                    _ => merged.push((c, d)),
                }
            }
            *row = merged;
        }
        // S = A.sum(axis=1) by `reduceat`; rows with S != 0 are scaled by
        // 1 / S (`Q @ A`, one product per entry).
        let mut dangling = Vec::new();
        for (i, row) in rows.iter_mut().enumerate() {
            let sum = match row.split_first() {
                Some((first, rest)) => {
                    let rest: Vec<f64> = rest.iter().map(|&(_, d)| d).collect();
                    first.1 + pairwise_sum(&rest)
                }
                None => 0.0,
            };
            if sum == 0.0 {
                dangling.push(i);
            } else {
                let inverse = 1.0 / sum;
                for entry in row.iter_mut() {
                    entry.1 *= inverse;
                }
            }
        }
        (rows, dangling)
    }
}

/// `_pagerank_scipy`'s power iteration with a uniform start, a uniform
/// personalisation and uniform dangling weights. `x @ A` adds row by row
/// in index order (scipy's `csc_matvec` over the transpose); the dangling
/// mass is a left-to-right `sum()`.
fn power_iteration(rows: &[Vec<(usize, f64)>], dangling: &[usize]) -> Option<Vec<f64>> {
    let n = rows.len();
    #[allow(clippy::cast_precision_loss)]
    let n_f = n as f64;
    let p = 1.0 / n_f;
    let mut x = vec![p; n];
    let mut y = vec![0.0; n];
    let mut deltas = vec![0.0; n];
    for _ in 0..MAX_ITER {
        y.fill(0.0);
        for (i, row) in rows.iter().enumerate() {
            for &(j, a) in row {
                y[j] += a * x[i];
            }
        }
        let mut dangling_sum = 0.0;
        for &d in dangling {
            dangling_sum += x[d];
        }
        for ((xj, yj), delta) in x.iter_mut().zip(&y).zip(&mut deltas) {
            let next = ALPHA * (yj + dangling_sum * p) + (1.0 - ALPHA) * p;
            *delta = (next - *xj).abs();
            *xj = next;
        }
        if pairwise_sum(&deltas) < n_f * TOL {
            return Some(x);
        }
    }
    None
}

/// numpy's pairwise summation (`pairwise_sum_DOUBLE`): under 8 values a
/// plain loop, up to 128 eight accumulators, above that two halves.
#[must_use]
pub fn pairwise_sum(values: &[f64]) -> f64 {
    let n = values.len();
    if n < 8 {
        let mut sum = 0.0;
        for &v in values {
            sum += v;
        }
        sum
    } else if n <= 128 {
        let mut r = [0.0; 8];
        r.copy_from_slice(&values[..8]);
        let mut i = 8;
        while i < n - (n % 8) {
            for j in 0..8 {
                r[j] += values[i + j];
            }
            i += 8;
        }
        let mut sum = ((r[0] + r[1]) + (r[2] + r[3])) + ((r[4] + r[5]) + (r[6] + r[7]));
        while i < n {
            sum += values[i];
            i += 1;
        }
        sum
    } else {
        let mut half = n / 2;
        half -= half % 8;
        pairwise_sum(&values[..half]) + pairwise_sum(&values[half..])
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    #[allow(clippy::float_cmp)] // the point is bit equality with numpy
    fn pairwise_matches_numpy_blocks() {
        // np.sum([1e16, 1, -1e16, 1, ...]) depends on the blocking; these
        // values were checked against numpy 2.5.2.
        let values: Vec<f64> = (0..20)
            .map(|i| if i % 3 == 0 { 1e16 } else { 1.0 })
            .collect();
        assert_eq!(pairwise_sum(&values), 7e16);
        let values: Vec<f64> = (0..300)
            .map(|i| if i % 3 == 0 { 1e16 } else { 1.0 })
            .collect();
        assert_eq!(pairwise_sum(&values), 1e18);
        assert_eq!(pairwise_sum(&[0.1, 0.2, 0.3]), 0.1 + 0.2 + 0.3);
    }
}
