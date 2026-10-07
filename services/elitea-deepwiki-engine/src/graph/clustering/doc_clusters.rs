//! `detect_doc_clusters` (Phase 7G): the sections that are mostly
//! documentation.
//!
//! Ported because it is part of the clustering module's surface, but NO
//! live Python code calls it (a repository-wide search finds only its
//! definition); the structure planner classifies doc pages itself. Kept so
//! phase 5 can call it if the planner port needs it.

use super::ClusterGraph;
use indexmap::IndexMap;

/// `DOC_DOMINANT_THRESHOLD`: more than 70 % documentation nodes.
pub const DOC_DOMINANT_THRESHOLD: f64 = 0.7;

/// Section → documentation ratio, for each section whose ratio of
/// `is_doc` nodes exceeds [`DOC_DOMINANT_THRESHOLD`], in `cluster_map`
/// order. Ids the graph does not know count as non-documentation, as a
/// missing index row does in Python.
#[must_use]
pub fn detect_doc_clusters(
    cluster_map: &IndexMap<usize, IndexMap<usize, Vec<String>>>,
    graph: &ClusterGraph,
) -> IndexMap<usize, f64> {
    let mut out = IndexMap::new();
    for (&section, pages) in cluster_map {
        let mut total = 0_u32;
        let mut docs = 0_u32;
        for id in pages.values().flatten() {
            total += 1;
            if graph.index_of(id).is_some_and(|n| graph.node(n).is_doc) {
                docs += 1;
            }
        }
        if total > 0 {
            let ratio = f64::from(docs) / f64::from(total);
            if ratio > DOC_DOMINANT_THRESHOLD {
                out.insert(section, ratio);
            }
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::graph::clustering::ClusterNode;

    #[test]
    fn only_sections_above_seventy_percent_are_doc_dominant() {
        let nodes = (0..10)
            .map(|i| ClusterNode {
                id: format!("n{i}"),
                rel_path: String::new(),
                file_name: String::new(),
                is_doc: i < 8,
            })
            .collect();
        let graph = ClusterGraph::from_parts(nodes, &[], &vec![Vec::new(); 10]).unwrap();
        let ids = |r: std::ops::Range<i32>| r.map(|i| format!("n{i}")).collect::<Vec<_>>();
        let map = IndexMap::from([
            // 4 of 4 docs.
            (3, IndexMap::from([(0, ids(0..2)), (1, ids(2..4))])),
            // 7 docs of 10 = exactly 0.7: not above.
            (
                5,
                IndexMap::from([(0, [ids(1..8), ids(8..10), vec!["n8".to_owned()]].concat())]),
            ),
            // 0 of 2.
            (6, IndexMap::from([(0, ids(8..10))])),
            (7, IndexMap::new()),
        ]);
        let out = detect_doc_clusters(&map, &graph);
        assert_eq!(out.len(), 1);
        assert!((out[&3] - 1.0).abs() < f64::EPSILON);
    }
}
