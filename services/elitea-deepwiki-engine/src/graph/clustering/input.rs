//! The graph Phase 3 reads: the Phase 2 output, reduced to what clustering
//! uses.
//!
//! Phase 3 reads only the node ids, `rel_path` / `file_name`, `is_doc`, the
//! edge weights and the ADJACENCY ORDER. The order is part of the contract:
//! Python breaks ties by the order `Counter`s and `dict`s first saw a key,
//! and those see the networkx order. So the graph keeps:
//!
//! * nodes in insertion order;
//! * per node, its successors in first-connection order, each with the
//!   weights of its parallel edges in key order (`G._succ[u]`);
//! * per node, its distinct predecessors in first-connection order with the
//!   number of parallel edges from each (`G._pred[v]`). The predecessor
//!   order is the order edges were first ADDED, which the edge list alone
//!   cannot restore, so it is an input of its own.
//!
//! [`ClusterGraph::from_code_graph`] builds it from the engine's
//! [`CodeGraph`]; [`ClusterGraph::from_parts`] from the rows the parity
//! script `python_phase3_dump.py` writes.

use crate::graph::CodeGraph;
use crate::graph::constants;
use std::collections::HashMap;

/// One node.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ClusterNode {
    pub id: String,
    /// `rel_path`, `""` when absent.
    pub rel_path: String,
    /// `file_name`, `""` when absent.
    pub file_name: String,
    /// The index's `is_doc` column.
    pub is_doc: bool,
}

/// The successors of one node with one target: the weights of the parallel
/// edges, in key order.
#[derive(Debug, Clone, PartialEq)]
pub struct SuccEntry {
    pub target: usize,
    pub weights: Vec<f64>,
}

/// The weighted, ordered multigraph Phase 3 clusters.
#[derive(Debug, Clone, Default)]
pub struct ClusterGraph {
    nodes: Vec<ClusterNode>,
    index: HashMap<String, usize>,
    succ: Vec<Vec<SuccEntry>>,
    /// `(source, parallel edge count)` in first-connection order.
    pred: Vec<Vec<(usize, usize)>>,
    edge_count: usize,
}

/// A graph input that does not describe a graph.
#[derive(Debug, Clone, thiserror::Error, PartialEq, Eq)]
pub enum InputError {
    #[error("duplicate node id {0}")]
    DuplicateNode(String),
    #[error("edge endpoint {0} is not a node")]
    UnknownNode(String),
    #[error("predecessor list of {node} disagrees with the edges: {detail}")]
    Predecessors { node: String, detail: String },
}

impl ClusterGraph {
    /// From the engine's graph after Phase 2.
    #[must_use]
    pub fn from_code_graph(graph: &CodeGraph) -> Self {
        let mut nodes = Vec::with_capacity(graph.node_count());
        let mut index = HashMap::with_capacity(graph.node_count());
        for (id, data) in graph.nodes() {
            index.insert(id.to_owned(), nodes.len());
            nodes.push(ClusterNode {
                id: id.to_owned(),
                rel_path: data.rel_path.to_string(),
                file_name: data.file_name.to_string(),
                is_doc: constants::is_doc_symbol(&data.symbol_type.to_lowercase()),
            });
        }
        let mut succ: Vec<Vec<SuccEntry>> = vec![Vec::new(); nodes.len()];
        let mut edge_count = 0;
        for edge in graph.edges() {
            let (Some(&u), Some(&v)) = (index.get(edge.source), index.get(edge.target)) else {
                continue;
            };
            push_edge(&mut succ[u], v, edge.data.weight);
            edge_count += 1;
        }
        let mut pred = Vec::with_capacity(nodes.len());
        for (v, node) in nodes.iter().enumerate() {
            let list = graph
                .predecessors(&node.id)
                .filter_map(|source| index.get(source).copied())
                .map(|u| (u, multiplicity(&succ[u], v)))
                .collect();
            pred.push(list);
        }
        Self {
            nodes,
            index,
            succ,
            pred,
            edge_count,
        }
    }

    /// From nodes, edges in networkx `G.edges(keys=True)` order, and each
    /// node's distinct predecessors in `G._pred` order (`preds[i]` belongs
    /// to `nodes[i]`).
    ///
    /// # Errors
    ///
    /// A duplicate node, an edge to an unknown node, or a predecessor list
    /// that does not name exactly the sources of the node's in-edges.
    pub fn from_parts(
        nodes: Vec<ClusterNode>,
        edges: &[(String, String, f64)],
        preds: &[Vec<String>],
    ) -> Result<Self, InputError> {
        let mut index = HashMap::with_capacity(nodes.len());
        for (i, node) in nodes.iter().enumerate() {
            if index.insert(node.id.clone(), i).is_some() {
                return Err(InputError::DuplicateNode(node.id.clone()));
            }
        }
        let lookup = |id: &str| {
            index
                .get(id)
                .copied()
                .ok_or_else(|| InputError::UnknownNode(id.to_owned()))
        };
        let mut succ: Vec<Vec<SuccEntry>> = vec![Vec::new(); nodes.len()];
        for (source, target, weight) in edges {
            let u = lookup(source)?;
            let v = lookup(target)?;
            push_edge(&mut succ[u], v, *weight);
        }
        let mut sources: Vec<Vec<usize>> = vec![Vec::new(); nodes.len()];
        for (u, entries) in succ.iter().enumerate() {
            for entry in entries {
                sources[entry.target].push(u);
            }
        }
        let mut pred = Vec::with_capacity(nodes.len());
        for (v, node) in nodes.iter().enumerate() {
            let listed = preds.get(v).map_or(&[][..], Vec::as_slice);
            let mut list = Vec::with_capacity(listed.len());
            for source in listed {
                let u = lookup(source)?;
                let count = multiplicity(&succ[u], v);
                if count == 0 {
                    return Err(InputError::Predecessors {
                        node: node.id.clone(),
                        detail: format!("{source} has no edge to it"),
                    });
                }
                list.push((u, count));
            }
            if list.len() != sources[v].len() {
                return Err(InputError::Predecessors {
                    node: node.id.clone(),
                    detail: format!("{} listed, {} in the edges", list.len(), sources[v].len()),
                });
            }
            pred.push(list);
        }
        Ok(Self {
            nodes,
            index,
            succ,
            pred,
            edge_count: edges.len(),
        })
    }

    #[must_use]
    pub fn node_count(&self) -> usize {
        self.nodes.len()
    }

    #[must_use]
    pub fn edge_count(&self) -> usize {
        self.edge_count
    }

    #[must_use]
    pub fn nodes(&self) -> &[ClusterNode] {
        &self.nodes
    }

    #[must_use]
    pub fn node(&self, index: usize) -> &ClusterNode {
        &self.nodes[index]
    }

    /// The index of `id`.
    #[must_use]
    pub fn index_of(&self, id: &str) -> Option<usize> {
        self.index.get(id).copied()
    }

    /// The successors of `node`, in first-connection order.
    #[must_use]
    pub fn successors(&self, node: usize) -> &[SuccEntry] {
        &self.succ[node]
    }

    /// The distinct predecessors of `node` with their parallel edge count,
    /// in first-connection order.
    #[must_use]
    pub fn predecessors(&self, node: usize) -> &[(usize, usize)] {
        &self.pred[node]
    }

    /// `data.get("rel_path") or data.get("file_name") or ""`.
    #[must_use]
    pub fn path_of(&self, node: usize) -> &str {
        let node = &self.nodes[node];
        if node.rel_path.is_empty() {
            &node.file_name
        } else {
            &node.rel_path
        }
    }
}

/// Append an edge to its target's keydict, or start one.
fn push_edge(entries: &mut Vec<SuccEntry>, target: usize, weight: f64) {
    if let Some(entry) = entries.iter_mut().find(|e| e.target == target) {
        entry.weights.push(weight);
    } else {
        entries.push(SuccEntry {
            target,
            weights: vec![weight],
        });
    }
}

fn multiplicity(entries: &[SuccEntry], target: usize) -> usize {
    entries
        .iter()
        .find(|e| e.target == target)
        .map_or(0, |e| e.weights.len())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::graph::{EdgeData, NodeData};

    fn node(id: &str, rel_path: &str) -> ClusterNode {
        ClusterNode {
            id: id.to_owned(),
            rel_path: rel_path.to_owned(),
            file_name: String::new(),
            is_doc: false,
        }
    }

    #[test]
    fn from_parts_keeps_parallel_edges_and_the_given_predecessor_order() {
        let edges = vec![
            ("a".to_owned(), "c".to_owned(), 1.0),
            ("a".to_owned(), "c".to_owned(), 2.0),
            ("b".to_owned(), "c".to_owned(), 0.5),
        ];
        // c was first reached from b, then from a.
        let preds = vec![vec![], vec![], vec!["b".to_owned(), "a".to_owned()]];
        let graph = ClusterGraph::from_parts(
            vec![node("a", "x.py"), node("b", "y.py"), node("c", "y.py")],
            &edges,
            &preds,
        )
        .unwrap();
        assert_eq!(graph.edge_count(), 3);
        assert_eq!(graph.successors(0)[0].weights, vec![1.0, 2.0]);
        assert_eq!(graph.predecessors(2), &[(1, 1), (0, 2)]);
    }

    #[test]
    fn from_parts_refuses_a_predecessor_list_that_disagrees() {
        let edges = vec![("a".to_owned(), "b".to_owned(), 1.0)];
        let missing = ClusterGraph::from_parts(
            vec![node("a", ""), node("b", "")],
            &edges,
            &[vec![], vec![]],
        );
        assert!(matches!(missing, Err(InputError::Predecessors { .. })));
        let unknown = ClusterGraph::from_parts(vec![node("a", "")], &edges, &[vec![]]);
        assert_eq!(
            unknown.unwrap_err(),
            InputError::UnknownNode("b".to_owned())
        );
    }

    #[test]
    fn from_code_graph_reads_the_networkx_orders() {
        let mut graph = CodeGraph::new();
        for id in ["a", "b", "c"] {
            graph.add_node(
                id,
                NodeData {
                    rel_path: format!("{id}.md").as_str().into(),
                    symbol_type: if id == "c" {
                        "markdown_section".into()
                    } else {
                        "class".into()
                    },
                    ..NodeData::default()
                },
            );
        }
        let weighted = |w: f64| EdgeData {
            weight: w,
            ..EdgeData::default()
        };
        graph.add_edge("b", "c", weighted(0.25));
        graph.add_edge("a", "c", weighted(1.0));
        graph.add_edge("a", "c", weighted(3.0));
        let clustered = ClusterGraph::from_code_graph(&graph);
        assert_eq!(clustered.edge_count(), 3);
        assert_eq!(clustered.predecessors(2), &[(1, 1), (0, 2)]);
        assert_eq!(clustered.successors(0)[0].weights, vec![1.0, 3.0]);
        assert!(clustered.node(2).is_doc);
        assert!(!clustered.node(0).is_doc);
        assert_eq!(clustered.path_of(0), "a.md");
    }
}
