//! Component bridging (`bridge_disconnected_components`).
//!
//! Leiden yields at least one community per connected component, so
//! Phase 2 joins every smaller weakly connected component to a larger one:
//! the earlier (larger) component whose directory histogram overlaps most
//! (the sum over shared directories of the smaller count; ties keep the
//! earliest, so the largest component wins a tie at 0). The two
//! components' highest-degree nodes get a `component_bridge` edge each way.
//!
//! Components are ordered as networkx yields them (by their first node in
//! node order), then sorted by size, largest first, stably. Python picks a
//! component's representative with `max()` over a `set`, so between equal
//! degrees the hash seed decided; here the first in id order wins (the
//! parity reference sorts each component the same way).

use super::synthetic_edge;
use crate::graph::CodeGraph;
use serde_json::{Value, json};
use std::cmp::Reverse;
use std::collections::{BinaryHeap, HashMap};

/// Union-find with path halving.
struct Components {
    parent: Vec<usize>,
}

impl Components {
    fn find(&mut self, mut x: usize) -> usize {
        while self.parent[x] != x {
            self.parent[x] = self.parent[self.parent[x]];
            x = self.parent[x];
        }
        x
    }

    fn union(&mut self, a: usize, b: usize) {
        let (ra, rb) = (self.find(a), self.find(b));
        if ra != rb {
            self.parent[ra.max(rb)] = ra.min(rb);
        }
    }
}

/// `nx.weakly_connected_components(G)`: the member ids of each component,
/// components in order of their first node, members in node order.
#[must_use]
pub fn weakly_connected_components(graph: &CodeGraph) -> Vec<Vec<&str>> {
    let ids: Vec<&str> = graph.nodes().map(|(id, _)| id).collect();
    let position: HashMap<&str, usize> = ids.iter().enumerate().map(|(i, id)| (*id, i)).collect();
    let mut sets = Components {
        parent: (0..ids.len()).collect(),
    };
    for edge in graph.edges() {
        if let (Some(&u), Some(&v)) = (position.get(edge.source), position.get(edge.target)) {
            sets.union(u, v);
        }
    }
    let mut slot_of_root: HashMap<usize, usize> = HashMap::new();
    let mut components: Vec<Vec<&str>> = Vec::new();
    for (i, id) in ids.iter().enumerate() {
        let root = sets.find(i);
        let slot = *slot_of_root.entry(root).or_insert_with(|| {
            components.push(Vec::new());
            components.len() - 1
        });
        components[slot].push(id);
    }
    components
}

/// `_dir_sim`: the shared mass of two directory histograms. The walk in
/// [`best_earlier`] computes the same sums; this is its test reference.
#[cfg(test)]
fn dir_sim(a: &HashMap<String, usize>, b: &HashMap<String, usize>) -> usize {
    let (small, large) = if a.len() <= b.len() { (a, b) } else { (b, a) };
    small
        .iter()
        .filter_map(|(dir, &n)| large.get(dir).map(|&m| n.min(m)))
        .sum()
}

/// For each component after the first, the earlier component it bridges
/// to (index 0 holds 0 and is not used).
///
/// Python compares component i with every earlier one (quadratic in the
/// component count: 5,700 components on the platform repository). Only
/// components that share a directory can score above 0, so an inverted
/// index (directory → earlier components with their count) yields the same
/// scores, and the same winner: the first strictly greater score in
/// component order, starting from component 0's. See [`best_earlier`] for
/// how the walk stops early.
fn bridge_targets(histograms: &[HashMap<String, usize>]) -> Vec<usize> {
    let mut targets = vec![0; histograms.len()];
    let mut by_dir: HashMap<&str, DirList> = HashMap::new();
    for (i, histogram) in histograms.iter().enumerate() {
        if i > 0 {
            targets[i] = best_earlier(histogram, &by_dir);
        }
        for (dir, &count) in histogram {
            let list = by_dir.entry(dir.as_str()).or_default();
            list.entries.push((i, count));
            list.max_count = list.max_count.max(count);
        }
    }
    targets
}

/// The components that have one directory, in component order, with their
/// count in it; and the largest of those counts.
#[derive(Default)]
struct DirList {
    entries: Vec<(usize, usize)>,
    max_count: usize,
}

/// One directory `best_earlier` walks: the most it can add to a score, the
/// component's own count in it, and the earlier components that have it.
type SharedDir<'a> = (usize, usize, &'a [(usize, usize)]);

/// The earlier component `histogram` bridges to: the first with the
/// highest [`dir_sim`], component 0 when none scores above it.
///
/// The index lists of `histogram`'s directories are walked together in
/// component order (each list is in that order already), so each
/// candidate's score is complete when it is reached. A later candidate can
/// only collect from the lists that are not exhausted, at most
/// `min(own count, the list's largest count)` from each: once the best
/// score reaches that bound, no later candidate can be STRICTLY greater,
/// and the walk stops. The bound never exceeds the component's node count;
/// without it, many components in one directory made the walk quadratic.
fn best_earlier(histogram: &HashMap<String, usize>, by_dir: &HashMap<&str, DirList>) -> usize {
    // (cap, own count, list) per shared directory.
    let lists: Vec<SharedDir<'_>> = histogram
        .iter()
        .filter_map(|(dir, &count)| {
            by_dir
                .get(dir.as_str())
                .filter(|list| !list.entries.is_empty())
                .map(|list| (count.min(list.max_count), count, list.entries.as_slice()))
        })
        .collect();
    let mut cursor = vec![0usize; lists.len()];
    let mut heap: BinaryHeap<Reverse<(usize, usize)>> = lists
        .iter()
        .enumerate()
        .map(|(k, (_, _, list))| Reverse((list[0].0, k)))
        .collect();
    let mut remaining: usize = lists.iter().map(|(cap, _, _)| cap).sum();
    let (mut best_target, mut best_sim) = (0, 0);
    while let Some(&Reverse((j, _))) = heap.peek() {
        if best_sim >= remaining {
            break;
        }
        let mut sim = 0;
        while let Some(&Reverse((at, k))) = heap.peek() {
            if at != j {
                break;
            }
            heap.pop();
            let (cap, count, list) = lists[k];
            sim += count.min(list[cursor[k]].1);
            cursor[k] += 1;
            if let Some(&(next, _)) = list.get(cursor[k]) {
                heap.push(Reverse((next, k)));
            } else {
                remaining -= cap;
            }
        }
        if j == 0 {
            best_sim = sim;
        } else if sim > best_sim {
            best_sim = sim;
            best_target = j;
        }
    }
    best_target
}

/// `bridge_disconnected_components`.
#[must_use]
pub fn bridge_disconnected_components(graph: &mut CodeGraph) -> Value {
    let (bridges, before) = {
        let mut components = weakly_connected_components(graph);
        let before = components.len();
        if before <= 1 {
            return json!({
                "components_before": before,
                "components_after": before,
                "bridges_added": 0,
            });
        }
        components.sort_by_key(|component| Reverse(component.len()));
        let table = super::degrees(graph);
        let histograms: Vec<HashMap<String, usize>> = components
            .iter()
            .map(|component| {
                let mut histogram: HashMap<String, usize> = HashMap::new();
                for id in component {
                    let Some(data) = graph.node(id) else {
                        continue;
                    };
                    // `data.get("rel_path") or data.get("file_name") or ""`.
                    let path: &str = if data.rel_path.is_empty() {
                        &data.file_name
                    } else {
                        &data.rel_path
                    };
                    let dir = path.rsplit_once('/').map_or("<root>", |(dir, _)| dir);
                    *histogram.entry(dir.to_owned()).or_default() += 1;
                }
                histogram
            })
            .collect();
        let representatives: Vec<String> = components
            .iter()
            .map(|component| {
                let mut sorted = component.clone();
                sorted.sort_unstable();
                let mut best: Option<(&str, usize)> = None;
                for id in sorted {
                    let (inbound, outbound) = table.get(id).copied().unwrap_or_default();
                    let degree = inbound + outbound;
                    if best.is_none_or(|(_, top)| degree > top) {
                        best = Some((id, degree));
                    }
                }
                best.map(|(id, _)| id.to_owned()).unwrap_or_default()
            })
            .collect();
        let mut bridges: Vec<(String, String)> = Vec::new();
        for (i, target) in bridge_targets(&histograms).into_iter().enumerate().skip(1) {
            bridges.push((representatives[i].clone(), representatives[target].clone()));
        }
        (bridges, before)
    };
    let mut added = 0usize;
    for (source, target) in bridges {
        for (from, to) in [(&source, &target), (&target, &source)] {
            graph.add_edge(
                from,
                to,
                synthetic_edge(
                    "component_bridge",
                    "bridge",
                    "component_bridging",
                    None,
                    None,
                ),
            );
        }
        added += 2;
    }
    let after = weakly_connected_components(graph).len();
    json!({
        "components_before": before,
        "components_after": after,
        "bridges_added": added,
    })
}

#[cfg(test)]
#[allow(clippy::float_cmp)] // bit-exact parity is the point
mod tests {
    use super::*;
    use crate::graph::{EdgeData, NodeData};

    fn node(rel_path: &str) -> NodeData {
        NodeData {
            rel_path: rel_path.into(),
            ..NodeData::default()
        }
    }

    #[test]
    fn smaller_components_join_the_most_similar_larger_one() {
        let mut graph = CodeGraph::new();
        // Component A (3 nodes, src/a), B (2 nodes, lib), C (1 node, lib).
        graph.add_node("a1", node("src/a/1.py"));
        graph.add_node("a2", node("src/a/2.py"));
        graph.add_node("a3", node("src/a/3.py"));
        graph.add_node("c", node("lib/c.py"));
        graph.add_node("b1", node("lib/b1.py"));
        graph.add_node("b2", node("lib/b2.py"));
        graph.add_edge("a1", "a2", EdgeData::default());
        graph.add_edge("a3", "a2", EdgeData::default());
        graph.add_edge("b2", "b1", EdgeData::default());
        let stats = bridge_disconnected_components(&mut graph);
        assert_eq!(stats["components_before"], 3);
        assert_eq!(stats["components_after"], 1);
        assert_eq!(stats["bridges_added"], 4);
        let bridges: Vec<(&str, &str)> = graph
            .edges()
            .filter(|e| e.data.edge_class == "bridge")
            .map(|e| (e.source, e.target))
            .collect();
        // B (sim 0 with A) bridges to A's a2; C to B (shared `lib`): b1
        // and b2 tie at degree 1, the first in id order wins.
        assert!(bridges.contains(&("b1", "a2")));
        assert!(bridges.contains(&("a2", "b1")));
        assert!(bridges.contains(&("c", "b1")));
        assert!(bridges.contains(&("b1", "c")));
    }

    #[test]
    fn the_indexed_targets_equal_the_quadratic_scan() {
        // Deterministic pseudo-random histograms over a few directories.
        let mut seed: u64 = 7;
        let mut next = move || {
            seed = seed.wrapping_mul(6_364_136_223_846_793_005).wrapping_add(1);
            usize::try_from(seed >> 59).unwrap()
        };
        let histograms: Vec<HashMap<String, usize>> = (0..200)
            .map(|_| {
                (0..next() % 4)
                    .map(|_| (format!("d{}", next() % 9), 1 + next() % 3))
                    .collect()
            })
            .collect();
        let targets = bridge_targets(&histograms);
        for i in 1..histograms.len() {
            let mut best = (0, dir_sim(&histograms[i], &histograms[0]));
            for j in 1..i {
                let sim = dir_sim(&histograms[i], &histograms[j]);
                if sim > best.1 {
                    best = (j, sim);
                }
            }
            assert_eq!(targets[i], best.0, "component {i}");
        }
    }

    /// The inverted-index walk this module used before [`best_earlier`]:
    /// every earlier component sharing a directory is scored. The
    /// reference the bounded walk must equal.
    fn reference_bridge_targets(histograms: &[HashMap<String, usize>]) -> Vec<usize> {
        let mut targets = vec![0; histograms.len()];
        let mut by_dir: HashMap<&str, Vec<(usize, usize)>> = HashMap::new();
        for (i, histogram) in histograms.iter().enumerate() {
            if i > 0 {
                let mut scores: HashMap<usize, usize> = HashMap::new();
                for (dir, &count) in histogram {
                    for &(j, other) in by_dir.get(dir.as_str()).into_iter().flatten() {
                        *scores.entry(j).or_default() += count.min(other);
                    }
                }
                let mut best_target = 0;
                let mut best_sim = scores.get(&0).copied().unwrap_or(0);
                let mut candidates: Vec<(usize, usize)> =
                    scores.into_iter().filter(|&(j, _)| j > 0).collect();
                candidates.sort_unstable();
                for (j, sim) in candidates {
                    if sim > best_sim {
                        best_sim = sim;
                        best_target = j;
                    }
                }
                targets[i] = best_target;
            }
            for (dir, &count) in histogram {
                by_dir.entry(dir.as_str()).or_default().push((i, count));
            }
        }
        targets
    }

    #[test]
    fn the_bounded_walk_equals_the_full_index_scan_on_random_histograms() {
        let mut seed: u64 = 99;
        let mut next = move |bound: usize| {
            seed = seed
                .wrapping_mul(6_364_136_223_846_793_005)
                .wrapping_add(1_442_695_040_888_963_407);
            usize::try_from(seed >> 33).unwrap() % bound
        };
        for round in 0..40 {
            let dirs = 1 + next(12);
            let max_count = 1 + next(5);
            let histograms: Vec<HashMap<String, usize>> = (0..=next(300))
                .map(|_| {
                    (0..next(5))
                        .map(|_| (format!("d{}", next(dirs)), 1 + next(max_count)))
                        .collect()
                })
                .collect();
            assert_eq!(
                bridge_targets(&histograms),
                reference_bridge_targets(&histograms),
                "round {round}"
            );
        }
    }

    #[test]
    fn many_components_in_one_directory_bridge_in_linear_time() {
        // The full index scan took 8.9 s for 20k single-node components.
        const COMPONENTS: usize = 20_000;
        let histograms: Vec<HashMap<String, usize>> = (0..COMPONENTS)
            .map(|i| {
                // Every third has 2 nodes in `src`: the walk stops at the
                // largest earlier count, not at its own.
                let mut histogram =
                    HashMap::from([("src".to_owned(), 1 + usize::from(i % 3 == 1))]);
                if i % 5 == 0 {
                    histogram.insert(format!("own{i}"), 1);
                }
                histogram
            })
            .collect();
        let started = std::time::Instant::now();
        let targets = bridge_targets(&histograms);
        let elapsed = started.elapsed();
        for (i, &target) in targets.iter().enumerate() {
            assert_eq!(target, usize::from(i > 1 && i % 3 == 1), "component {i}");
        }
        assert!(
            elapsed < std::time::Duration::from_secs(2),
            "{elapsed:?} for {COMPONENTS} components"
        );
    }

    #[test]
    fn a_connected_graph_is_left_alone() {
        let mut graph = CodeGraph::new();
        graph.add_edge("x", "y", EdgeData::default());
        let stats = bridge_disconnected_components(&mut graph);
        assert_eq!(
            crate::pyjson::dumps(&stats),
            "{\"components_before\": 1, \"components_after\": 1, \"bridges_added\": 0}"
        );
        assert_eq!(graph.edge_count(), 1);
    }
}
