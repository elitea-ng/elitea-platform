//! The index the page path reads: the `repo_nodes` / `repo_edges` rows
//! after Phase 3, held in memory.
//!
//! Python's page path queried its SQLite `.wiki.db` (`cluster_expansion`,
//! `shared_expansion`, `language_heuristics`) and walked the networkx graph
//! (`_get_imports_from_graph`, the doc-node scans, the split's
//! `_name_index`). The native engine has both in memory after Phase 3
//! (there is no SQLite, ADR-0026), so each query is answered here, in the
//! ORDER SQLite returned rows:
//!
//! * a table scan, or an equality lookup through a one-column index, is
//!   rowid order — insertion order, which is graph order for nodes and
//!   edges (`from_networkx`, Phase 2's edge replacement);
//! * `node_id IN (…)` through the primary-key index is node-id order,
//!   duplicates dropped ([`PageIndex::nodes_by_ids`]);
//! * `ORDER BY` is a stable sort of the rows in that order.
//!
//! The full-text searches are not here: see [`super::search`].

use crate::graph::{CodeGraph, EdgeRow, NodeRow};
use serde::Deserialize;
use std::collections::HashMap;

/// One `repo_nodes` row, the columns the page path reads.
#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
pub struct IndexNode {
    pub node_id: String,
    #[serde(default)]
    pub rel_path: String,
    #[serde(default)]
    pub file_name: String,
    #[serde(default)]
    pub language: Option<String>,
    #[serde(default)]
    pub start_line: Option<i64>,
    #[serde(default)]
    pub end_line: Option<i64>,
    #[serde(default)]
    pub symbol_name: String,
    #[serde(default)]
    pub symbol_type: String,
    #[serde(default)]
    pub source_text: Option<String>,
    #[serde(default)]
    pub docstring: Option<String>,
    #[serde(default)]
    pub signature: Option<String>,
    #[serde(default, deserialize_with = "flag")]
    pub is_architectural: bool,
    #[serde(default, deserialize_with = "flag")]
    pub is_doc: bool,
    #[serde(default, deserialize_with = "flag")]
    pub is_test: bool,
    #[serde(default)]
    pub chunk_type: Option<String>,
    #[serde(default)]
    pub macro_cluster: Option<i64>,
    #[serde(default)]
    pub micro_cluster: Option<i64>,
}

fn flag<'de, D: serde::Deserializer<'de>>(deserializer: D) -> Result<bool, D::Error> {
    let value = Option::<i64>::deserialize(deserializer)?;
    Ok(value.unwrap_or(0) != 0)
}

impl IndexNode {
    /// `node.get("source_text") or ""`.
    #[must_use]
    pub fn text(&self) -> &str {
        self.source_text.as_deref().unwrap_or("")
    }

    /// `end_line - start_line`, `None` when either is NULL (SQLite's
    /// arithmetic on NULL).
    fn span(&self) -> Option<i64> {
        Some(self.end_line? - self.start_line?)
    }
}

/// One `repo_edges` row, the columns the page path reads.
#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
pub struct IndexEdge {
    pub source_id: String,
    pub target_id: String,
    #[serde(default)]
    pub rel_type: Option<String>,
    #[serde(default)]
    pub edge_class: Option<String>,
    #[serde(default)]
    pub weight: Option<f64>,
}

impl IndexEdge {
    /// `(row["rel_type"] or "")`.
    #[must_use]
    pub fn rel_type(&self) -> &str {
        self.rel_type.as_deref().unwrap_or("")
    }

    /// `row["weight"] or 1.0`: NULL and 0 are 1.0.
    #[must_use]
    pub fn weight_or_one(&self) -> f64 {
        match self.weight {
            Some(w) if w != 0.0 => w,
            _ => 1.0,
        }
    }
}

/// What the page path reads from the in-memory graph besides the rows.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct GraphFacts {
    /// `relationship_graph.number_of_nodes()` (the split's size signal).
    pub node_count: usize,
    /// `_get_imports_from_graph(path)` for every file that has an answer.
    pub imports: HashMap<String, String>,
    /// The file of the split's `_name_index[symbol][0]`.
    pub name_paths: HashMap<String, String>,
}

impl GraphFacts {
    /// The facts of a built graph.
    ///
    /// * `imports`: `_get_imports_from_graph` for every file key — the
    ///   node's `rel_path`, else its absolute `file_path` (Python's key, so
    ///   a node without a `rel_path` answers only for its absolute path,
    ///   which no page context asks for) — from the nodes' `imports` lists
    ///   and their `imports` / `file_imports` edges, sorted;
    /// * `name_paths`: the first entry of `_name_index[symbol]` (nodes with
    ///   a name, a `file_path` and a language, highest type priority
    ///   first, then graph order), its `rel_path` else `file_path`.
    ///   APPROXIMATION: Python built that index right after contraction,
    ///   before the SQL / ORM / contract nodes were added; those types are
    ///   left out here, which is the same set.
    #[must_use]
    pub fn from_graph(graph: &CodeGraph) -> Self {
        let mut imports: HashMap<String, std::collections::BTreeSet<String>> = HashMap::new();
        let mut names: HashMap<String, Vec<(u8, usize, String)>> = HashMap::new();
        for (order, (id, data)) in graph.nodes().enumerate() {
            let key = if data.rel_path.is_empty() {
                data.file_path.to_string()
            } else {
                data.rel_path.to_string()
            };
            if !key.is_empty() {
                let entry = imports.entry(key.clone()).or_default();
                if let Some(serde_json::Value::Array(list)) = data.extra.get("imports") {
                    entry.extend(
                        list.iter()
                            .filter_map(serde_json::Value::as_str)
                            .filter(|s| !s.is_empty())
                            .map(str::to_owned),
                    );
                }
            }
            let symbol_type = data.symbol_type.to_lowercase();
            let name = if data.symbol_name.is_empty() {
                data.extra
                    .get("name")
                    .and_then(serde_json::Value::as_str)
                    .unwrap_or("")
                    .to_owned()
            } else {
                data.symbol_name.clone()
            };
            let post_contraction = symbol_type == "contract" || symbol_type.starts_with("sql_");
            if !name.is_empty()
                && !data.file_path.is_empty()
                && !data.language.is_empty()
                && !post_contraction
            {
                let path = if data.rel_path.is_empty() {
                    data.file_path.to_string()
                } else {
                    data.rel_path.to_string()
                };
                names.entry(name).or_default().push((
                    crate::graph::helpers::type_priority(&symbol_type),
                    order,
                    path,
                ));
            }
            let _ = id;
        }
        for edge in graph.edges() {
            if edge.data.rel_type != "imports" && edge.data.rel_type != "file_imports" {
                continue;
            }
            let Some(source) = graph.node(edge.source) else {
                continue;
            };
            let key = if source.rel_path.is_empty() {
                source.file_path.to_string()
            } else {
                source.rel_path.to_string()
            };
            if key.is_empty() {
                continue;
            }
            let target_name = graph
                .node(edge.target)
                .map(|t| {
                    if t.symbol_name.is_empty() {
                        t.extra
                            .get("name")
                            .and_then(serde_json::Value::as_str)
                            .unwrap_or("")
                            .to_owned()
                    } else {
                        t.symbol_name.clone()
                    }
                })
                .filter(|n| !n.is_empty())
                .unwrap_or_else(|| edge.target.to_owned());
            imports.entry(key).or_default().insert(target_name);
        }
        let name_paths = names
            .into_iter()
            .filter_map(|(name, mut entries)| {
                entries.sort_by_key(|(priority, order, _)| (std::cmp::Reverse(*priority), *order));
                entries.into_iter().next().map(|(_, _, path)| (name, path))
            })
            .collect();
        Self {
            node_count: graph.node_count(),
            imports: imports
                .into_iter()
                .filter(|(_, set)| !set.is_empty())
                .map(|(key, set)| (key, set.into_iter().collect::<Vec<_>>().join("\n")))
                .collect(),
            name_paths,
        }
    }
}

/// The rows, with the lookups the page path needs.
#[derive(Debug, Clone, Default)]
pub struct PageIndex {
    nodes: Vec<IndexNode>,
    edges: Vec<IndexEdge>,
    by_id: HashMap<String, usize>,
    by_name: HashMap<String, Vec<usize>>,
    out_edges: HashMap<String, Vec<usize>>,
    in_edges: HashMap<String, Vec<usize>>,
    facts: GraphFacts,
}

impl PageIndex {
    /// The index over `nodes` and `edges`, each in rowid order.
    #[must_use]
    pub fn new(nodes: Vec<IndexNode>, edges: Vec<IndexEdge>, facts: GraphFacts) -> Self {
        let mut by_id = HashMap::with_capacity(nodes.len());
        let mut by_name: HashMap<String, Vec<usize>> = HashMap::new();
        for (i, node) in nodes.iter().enumerate() {
            by_id.insert(node.node_id.clone(), i);
            by_name.entry(node.symbol_name.clone()).or_default().push(i);
        }
        let mut out_edges: HashMap<String, Vec<usize>> = HashMap::new();
        let mut in_edges: HashMap<String, Vec<usize>> = HashMap::new();
        for (i, edge) in edges.iter().enumerate() {
            out_edges.entry(edge.source_id.clone()).or_default().push(i);
            in_edges.entry(edge.target_id.clone()).or_default().push(i);
        }
        Self {
            nodes,
            edges,
            by_id,
            by_name,
            out_edges,
            in_edges,
            facts,
        }
    }

    /// The index of a built graph: its `repo_nodes` / `repo_edges` rows
    /// with the Phase 3 cluster columns, and the graph facts.
    #[must_use]
    pub fn from_graph(
        graph: &CodeGraph,
        clusters: &HashMap<String, (Option<i64>, Option<i64>)>,
        facts: GraphFacts,
    ) -> Self {
        let nodes = graph
            .node_rows()
            .into_iter()
            .map(|row| node_from_row(row, clusters))
            .collect();
        let edges = graph.edge_rows().into_iter().map(edge_from_row).collect();
        Self::new(nodes, edges, facts)
    }

    #[must_use]
    pub fn facts(&self) -> &GraphFacts {
        &self.facts
    }

    /// Every node, in rowid (graph) order.
    #[must_use]
    pub fn nodes(&self) -> &[IndexNode] {
        &self.nodes
    }

    /// `SELECT * FROM repo_nodes WHERE node_id = ?`.
    #[must_use]
    pub fn node(&self, node_id: &str) -> Option<&IndexNode> {
        self.by_id.get(node_id).map(|&i| &self.nodes[i])
    }

    /// `SELECT * FROM repo_nodes WHERE node_id IN (…)`: through the
    /// primary-key index, so in node-id order with duplicates dropped.
    #[must_use]
    pub fn nodes_by_ids(&self, ids: &[String]) -> Vec<&IndexNode> {
        let mut unique: Vec<&String> = ids.iter().collect();
        unique.sort();
        unique.dedup();
        unique.into_iter().filter_map(|id| self.node(id)).collect()
    }

    /// The outgoing edges of a node, in rowid order.
    pub fn edges_from(&self, node_id: &str) -> impl Iterator<Item = &IndexEdge> {
        self.out_edges
            .get(node_id)
            .into_iter()
            .flatten()
            .map(|&i| &self.edges[i])
    }

    /// The incoming edges of a node, in rowid order.
    pub fn edges_to(&self, node_id: &str) -> impl Iterator<Item = &IndexEdge> {
        self.in_edges
            .get(node_id)
            .into_iter()
            .flatten()
            .map(|&i| &self.edges[i])
    }

    /// `_resolve_symbols`'s exact lookup: `symbol_name = ? [AND
    /// macro_cluster = ?] AND is_architectural = 1 ORDER BY end_line -
    /// start_line DESC LIMIT 5` (NULL spans sort last).
    #[must_use]
    pub fn resolve_exact(&self, name: &str, macro_id: Option<i64>) -> Vec<&IndexNode> {
        let mut rows: Vec<&IndexNode> = self
            .by_name
            .get(name)
            .into_iter()
            .flatten()
            .map(|&i| &self.nodes[i])
            .filter(|n| n.is_architectural && (macro_id.is_none() || n.macro_cluster == macro_id))
            .collect();
        // Stable: equal spans keep rowid order. DESC puts NULL last.
        rows.sort_by_key(|n| std::cmp::Reverse(n.span()));
        rows.truncate(5);
        rows
    }

    /// `detect_dominant_language`: the most common non-NULL language among
    /// `ids`; a tie goes to the smallest language (`GROUP BY` sorts the
    /// groups, the stable `ORDER BY cnt DESC` keeps that order).
    #[must_use]
    pub fn dominant_language(&self, ids: &[String]) -> Option<String> {
        let mut counts: Vec<(String, usize)> = Vec::new();
        for node in self.nodes_by_ids(ids) {
            let Some(language) = &node.language else {
                continue;
            };
            match counts.iter_mut().find(|(l, _)| l == language) {
                Some(entry) => entry.1 += 1,
                None => counts.push((language.clone(), 1)),
            }
        }
        counts.sort_by(|a, b| a.0.cmp(&b.0));
        counts.sort_by_key(|c| std::cmp::Reverse(c.1));
        counts.into_iter().next().map(|(l, _)| l)
    }

    /// `_get_cluster_docs` source 1: doc nodes of the page's cluster,
    /// `ORDER BY rel_path`.
    #[must_use]
    pub fn cluster_docs(&self, macro_id: i64, micro_id: Option<i64>) -> Vec<&IndexNode> {
        let mut rows: Vec<&IndexNode> = self
            .nodes
            .iter()
            .filter(|n| {
                n.is_doc
                    && n.macro_cluster == Some(macro_id)
                    && (micro_id.is_none() || n.micro_cluster == micro_id)
            })
            .collect();
        rows.sort_by(|a, b| a.rel_path.cmp(&b.rel_path));
        rows
    }

    /// `_get_cluster_docs` source 2: doc nodes with an edge to or from a
    /// seed, `ORDER BY rel_path`. SQLite looks the rows up from the
    /// sub-query's de-duplicated (sorted) id list, so equal paths come out
    /// in node-id order, not rowid order.
    #[must_use]
    pub fn adjacent_docs(&self, seeds: &[String]) -> Vec<&IndexNode> {
        let mut ids: Vec<&str> = Vec::new();
        for seed in seeds {
            ids.extend(self.edges_to(seed).map(|e| e.source_id.as_str()));
            ids.extend(self.edges_from(seed).map(|e| e.target_id.as_str()));
        }
        ids.sort_unstable();
        ids.dedup();
        let mut rows: Vec<&IndexNode> = ids
            .into_iter()
            .filter_map(|id| self.node(id))
            .filter(|n| n.is_doc)
            .collect();
        rows.sort_by(|a, b| a.rel_path.cmp(&b.rel_path));
        rows
    }
}

fn node_from_row(
    row: NodeRow,
    clusters: &HashMap<String, (Option<i64>, Option<i64>)>,
) -> IndexNode {
    let (macro_cluster, micro_cluster) =
        clusters.get(&row.node_id).copied().unwrap_or((None, None));
    IndexNode {
        node_id: row.node_id,
        rel_path: row.rel_path,
        file_name: row.file_name,
        language: Some(row.language),
        start_line: Some(row.start_line),
        end_line: Some(row.end_line),
        symbol_name: row.symbol_name,
        symbol_type: row.symbol_type,
        source_text: row.source_text,
        docstring: Some(row.docstring),
        signature: Some(row.signature),
        is_architectural: row.is_architectural != 0,
        is_doc: row.is_doc != 0,
        is_test: row.is_test != 0,
        chunk_type: row.chunk_type,
        macro_cluster,
        micro_cluster,
    }
}

fn edge_from_row(row: EdgeRow) -> IndexEdge {
    IndexEdge {
        source_id: row.source_id,
        target_id: row.target_id,
        rel_type: Some(row.rel_type),
        edge_class: Some(row.edge_class),
        weight: Some(row.weight),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn node(id: &str, name: &str, start: i64, end: i64) -> IndexNode {
        IndexNode {
            node_id: id.into(),
            symbol_name: name.into(),
            start_line: Some(start),
            end_line: Some(end),
            is_architectural: true,
            macro_cluster: Some(1),
            ..IndexNode::default()
        }
    }

    #[test]
    fn exact_lookup_orders_by_span_then_rowid() {
        let index = PageIndex::new(
            vec![
                node("a", "X", 1, 2),
                node("b", "X", 1, 9),
                node("c", "X", 3, 4),
            ],
            Vec::new(),
            GraphFacts::default(),
        );
        let ids: Vec<&str> = index
            .resolve_exact("X", Some(1))
            .iter()
            .map(|n| n.node_id.as_str())
            .collect();
        assert_eq!(ids, ["b", "a", "c"]);
        assert!(index.resolve_exact("X", Some(2)).is_empty());
    }

    #[test]
    fn graph_facts_follow_the_python_keys() {
        use crate::graph::{EdgeData, NodeData};
        let mut graph = CodeGraph::new();
        let node = |rel: &str, abs: &str, name: &str, kind: &'static str| NodeData {
            rel_path: rel.into(),
            file_path: abs.into(),
            language: "python".into(),
            symbol_name: name.into(),
            symbol_type: kind.into(),
            ..NodeData::default()
        };
        graph.add_node("f::a", node("a.py", "/r/a.py", "helper", "function"));
        graph.add_node("c::a", node("a.py", "/r/a.py", "helper", "class"));
        graph.add_node("m::x", node("", "/r/x.py", "os", "module"));
        graph.add_node("t", node("b.py", "/r/b.py", "Target", "class"));
        let imports = |kind: &'static str| EdgeData {
            rel_type: kind.into(),
            ..EdgeData::default()
        };
        graph.add_edge("f::a", "t", imports("imports"));
        graph.add_edge("m::x", "t", imports("file_imports"));
        graph.add_edge("f::a", "m::x", imports("calls"));
        let facts = GraphFacts::from_graph(&graph);
        assert_eq!(facts.node_count, 4);
        assert_eq!(
            facts.imports.get("a.py").map(String::as_str),
            Some("Target")
        );
        assert_eq!(
            facts.imports.get("/r/x.py").map(String::as_str),
            Some("Target")
        );
        assert!(!facts.imports.contains_key("b.py"));
        // The class outranks the function of the same name.
        assert_eq!(
            facts.name_paths.get("helper").map(String::as_str),
            Some("a.py")
        );
    }

    #[test]
    fn in_lists_come_back_in_id_order_without_duplicates() {
        let index = PageIndex::new(
            vec![node("b", "B", 0, 0), node("a", "A", 0, 0)],
            Vec::new(),
            GraphFacts::default(),
        );
        let ids: Vec<&str> = index
            .nodes_by_ids(&["b".into(), "a".into(), "b".into(), "zz".into()])
            .iter()
            .map(|n| n.node_id.as_str())
            .collect();
        assert_eq!(ids, ["a", "b"]);
    }
}
