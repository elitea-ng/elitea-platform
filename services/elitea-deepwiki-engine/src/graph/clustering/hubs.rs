//! `reintegrate_hubs`: give each hub the section, and the page in it, that
//! most of its edges reach.
//!
//! WHY hubs are left out and put back: a logger or base class touched by
//! half the repository would glue unrelated communities together, but it
//! still belongs on SOME page.
//!
//! Python counts with a `Counter` and takes `most_common(1)`, which is the
//! first maximal key in insertion order — the order the hub's out-edges,
//! then its in-edges, first reached that cluster. The counts here are kept
//! in an insertion-ordered map for the same tie-break.

use super::ClusterGraph;
use indexmap::IndexMap;

/// The first key with the largest count (`Counter.most_common(1)`).
fn most_common(counts: &IndexMap<usize, usize>) -> Option<usize> {
    let mut best: Option<(usize, usize)> = None;
    for (&key, &count) in counts {
        if best.is_none_or(|(_, top)| count > top) {
            best = Some((key, count));
        }
    }
    best.map(|(key, _)| key)
}

/// Every edge of `hub` (out-edges, then in-edges, parallel edges one by
/// one) whose other end `label` knows, counted per label.
fn neighbour_counts(
    graph: &ClusterGraph,
    hub: usize,
    label: impl Fn(usize) -> Option<usize>,
) -> IndexMap<usize, usize> {
    let mut counts: IndexMap<usize, usize> = IndexMap::new();
    for entry in graph.successors(hub) {
        if let Some(cluster) = label(entry.target) {
            *counts.entry(cluster).or_insert(0) += entry.weights.len();
        }
    }
    for &(source, parallel) in graph.predecessors(hub) {
        if let Some(cluster) = label(source) {
            *counts.entry(cluster).or_insert(0) += parallel;
        }
    }
    counts
}

/// Hub → (section, page). A hub with no clustered neighbour goes to the
/// largest section (the first of the largest, by first appearance among
/// the assignments), and to its largest page; the page is `None` only when
/// there are no sections at all.
#[must_use]
pub fn reintegrate_hubs(
    graph: &ClusterGraph,
    hubs: &[usize],
    section_of: &IndexMap<usize, usize>,
    page_of: &IndexMap<usize, IndexMap<usize, usize>>,
) -> IndexMap<usize, (usize, Option<usize>)> {
    let mut sizes: IndexMap<usize, usize> = IndexMap::new();
    for section in section_of.values() {
        *sizes.entry(*section).or_insert(0) += 1;
    }
    let largest = most_common(&sizes).unwrap_or(0);

    let mut out = IndexMap::new();
    for &hub in hubs {
        let sections = neighbour_counts(graph, hub, |n| section_of.get(&n).copied());
        let section = most_common(&sections).unwrap_or(largest);
        let page = if page_of.is_empty() {
            None
        } else {
            page_of.get(&section).and_then(|pages| {
                let counts = neighbour_counts(graph, hub, |n| pages.get(&n).copied());
                most_common(&counts).or_else(|| {
                    let mut page_sizes: IndexMap<usize, usize> = IndexMap::new();
                    for page in pages.values() {
                        *page_sizes.entry(*page).or_insert(0) += 1;
                    }
                    most_common(&page_sizes)
                })
            })
        };
        out.insert(hub, (section, page));
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::graph::clustering::ClusterNode;

    fn graph(edges: &[(&str, &str)], preds: &[(&str, &[&str])]) -> ClusterGraph {
        let ids = ["h", "a", "b", "c", "d"];
        let nodes = ids
            .iter()
            .map(|id| ClusterNode {
                id: (*id).to_owned(),
                rel_path: String::new(),
                file_name: String::new(),
                is_doc: false,
            })
            .collect();
        let edges: Vec<(String, String, f64)> = edges
            .iter()
            .map(|(u, v)| ((*u).to_owned(), (*v).to_owned(), 1.0))
            .collect();
        let preds: Vec<Vec<String>> = ids
            .iter()
            .map(|id| {
                preds
                    .iter()
                    .find(|(n, _)| n == id)
                    .map(|(_, list)| list.iter().map(|s| (*s).to_owned()).collect())
                    .unwrap_or_default()
            })
            .collect();
        ClusterGraph::from_parts(nodes, &edges, &preds).unwrap()
    }

    #[test]
    fn a_hub_joins_the_cluster_most_of_its_edges_reach() {
        // h → a (section 1, page 0); b, c → h (section 2, pages 0 and 1).
        let g = graph(
            &[("h", "a"), ("b", "h"), ("c", "h")],
            &[("a", &["h"]), ("h", &["c", "b"])],
        );
        let macro_ = IndexMap::from([(1, 1), (2, 2), (3, 2), (4, 1)]);
        let micro = IndexMap::from([
            (1, IndexMap::from([(1, 0), (4, 0)])),
            (2, IndexMap::from([(2, 0), (3, 1)])),
        ]);
        let out = reintegrate_hubs(&g, &[0], &macro_, &micro);
        // Section 2 (two edges); page tie 1 vs 0 goes to the predecessor
        // seen first, c (page 1).
        assert_eq!(out[&0], (2, Some(1)));
    }

    #[test]
    fn a_ties_goes_to_the_first_cluster_reached_and_an_isolated_hub_to_the_largest() {
        let g = graph(&[("h", "b"), ("h", "a")], &[("a", &["h"]), ("b", &["h"])]);
        let macro_ = IndexMap::from([(1, 5), (2, 7), (3, 7)]);
        let micro = IndexMap::from([
            (5, IndexMap::from([(1, 0)])),
            (7, IndexMap::from([(2, 0), (3, 1), (4, 1)])),
        ]);
        // h → b (section 7) is the first edge: 7 wins the 1-1 tie.
        assert_eq!(
            reintegrate_hubs(&g, &[0], &macro_, &micro)[&0],
            (7, Some(0))
        );
        // d has no edges: largest section (7, two nodes), largest page (1).
        assert_eq!(
            reintegrate_hubs(&g, &[4], &macro_, &micro)[&4],
            (7, Some(1))
        );
        // Nothing clustered: section 0, no page.
        let empty = reintegrate_hubs(&g, &[0], &IndexMap::new(), &IndexMap::new());
        assert_eq!(empty[&0], (0, None));
    }
}
