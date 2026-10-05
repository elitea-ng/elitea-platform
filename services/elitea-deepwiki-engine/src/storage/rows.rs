//! Graph rows mapped onto the ADR-0022 columns.
//!
//! `storage/publish.py` read the `.wiki.db` the Python engine wrote
//! (`repo_nodes`, `repo_edges`) and wrote `wiki_nodes` / `wiki_edges`. The
//! graph's [`NodeRow`] / [`EdgeRow`] are exactly those `.wiki.db` rows
//! (`graph::node_row`), so the mapping here is `publish.py`'s, column for
//! column:
//!
//! * nodes: the 18 `_SOURCE_COLUMNS`; a NULL text becomes `""` (`or ""`),
//!   `parent_symbol` and `chunk_type` keep NULL, the flags become booleans.
//!   `analysis_level`, `parameters`, `return_type`, `is_hub` and
//!   `hub_assignment` are not columns of `wiki_nodes` (0001 drops them).
//! * edges: `source_id`, `target_id`, `rel_type` (`or ""`), `edge_class`,
//!   `weight` (`float(weight or 1.0)`: a weight of 0 becomes 1.0). The
//!   legacy table had no key, so parallel edges were several rows; the
//!   publisher's `ON CONFLICT (wiki_id, source_id, target_id, rel_type) DO
//!   UPDATE` kept the FIRST row's position and the LAST row's `edge_class`
//!   and `weight`. [`collapse_edges`] does the same. `metadata` stays the
//!   column default `{}`: the publisher never wrote it, so the edge
//!   annotations (`EdgeRow::annotations`) are not carried, as before.

use crate::graph::{EdgeRow, NodeRow};
use crate::storage::{Result, StorageError};
use indexmap::IndexMap;

/// One `wiki_nodes` row, without its `wiki_id` (or `build_id`).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct IndexNode {
    pub node_id: String,
    pub rel_path: String,
    pub file_name: String,
    pub language: String,
    pub start_line: i32,
    pub end_line: i32,
    pub symbol_name: String,
    pub symbol_type: String,
    pub parent_symbol: Option<String>,
    pub source_text: String,
    pub docstring: String,
    pub signature: String,
    pub chunk_type: Option<String>,
    pub macro_cluster: Option<i32>,
    pub micro_cluster: Option<i32>,
    pub is_architectural: bool,
    pub is_doc: bool,
    pub is_test: bool,
}

/// One `wiki_edges` row, without its `wiki_id` (or `build_id`).
#[derive(Debug, Clone, PartialEq)]
pub struct IndexEdge {
    pub source_id: String,
    pub target_id: String,
    pub rel_type: String,
    pub edge_class: Option<String>,
    pub weight: f64,
}

/// An `INTEGER` column. Python passed the integer through and PostgreSQL
/// refused an out-of-range one ("integer out of range"); this refuses it
/// before the `COPY` with the node named.
fn integer(node_id: &str, column: &str, value: i64) -> Result<i32> {
    i32::try_from(value).map_err(|_| {
        StorageError::Publish(format!(
            "node {node_id}: {column} {value} does not fit the INTEGER column"
        ))
    })
}

impl IndexNode {
    /// `publish._iter_nodes` over one graph row.
    ///
    /// # Errors
    ///
    /// [`StorageError::Publish`] for a line number or cluster that does not
    /// fit a PostgreSQL `INTEGER`.
    pub fn from_row(row: NodeRow) -> Result<Self> {
        let start_line = integer(&row.node_id, "start_line", row.start_line)?;
        let end_line = integer(&row.node_id, "end_line", row.end_line)?;
        let coarse = row
            .macro_cluster
            .map(|value| integer(&row.node_id, "macro_cluster", value))
            .transpose()?;
        let fine = row
            .micro_cluster
            .map(|value| integer(&row.node_id, "micro_cluster", value))
            .transpose()?;
        Ok(Self {
            node_id: row.node_id,
            rel_path: row.rel_path,
            file_name: row.file_name,
            language: row.language,
            start_line,
            end_line,
            symbol_name: row.symbol_name,
            symbol_type: row.symbol_type,
            parent_symbol: row.parent_symbol,
            source_text: row.source_text.unwrap_or_default(),
            docstring: row.docstring,
            signature: row.signature,
            chunk_type: row.chunk_type,
            macro_cluster: coarse,
            micro_cluster: fine,
            is_architectural: row.is_architectural != 0,
            is_doc: row.is_doc != 0,
            is_test: row.is_test != 0,
        })
    }
}

impl IndexEdge {
    /// `publish._iter_edges` over one graph row.
    #[must_use]
    pub fn from_row(row: EdgeRow) -> Self {
        // `float(row["weight"] or 1.0)`: zero is falsy in Python.
        let weight = if row.weight == 0.0 { 1.0 } else { row.weight };
        Self {
            source_id: row.source_id,
            target_id: row.target_id,
            rel_type: row.rel_type,
            edge_class: Some(row.edge_class),
            weight,
        }
    }
}

/// Collapse parallel edges onto `(source_id, target_id, rel_type)`, as the
/// Python publisher's upsert did: the first edge's position, the last
/// edge's `edge_class` and `weight`.
pub fn collapse_edges(edges: impl IntoIterator<Item = IndexEdge>) -> Vec<IndexEdge> {
    let mut collapsed: IndexMap<(String, String, String), (Option<String>, f64)> = IndexMap::new();
    for edge in edges {
        // `insert` on an existing key keeps its position and replaces the
        // value: ON CONFLICT DO UPDATE.
        collapsed.insert(
            (edge.source_id, edge.target_id, edge.rel_type),
            (edge.edge_class, edge.weight),
        );
    }
    collapsed
        .into_iter()
        .map(
            |((source_id, target_id, rel_type), (edge_class, weight))| IndexEdge {
                source_id,
                target_id,
                rel_type,
                edge_class,
                weight,
            },
        )
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn edge(source: &str, target: &str, rel: &str, class: &str, weight: f64) -> IndexEdge {
        IndexEdge {
            source_id: source.into(),
            target_id: target.into(),
            rel_type: rel.into(),
            edge_class: Some(class.into()),
            weight,
        }
    }

    #[test]
    fn parallel_edges_collapse_first_position_last_value() {
        let edges = collapse_edges([
            edge("a", "b", "calls", "structural", 1.0),
            edge("a", "c", "calls", "structural", 1.0),
            edge("a", "b", "calls", "semantic", 0.5),
            edge("a", "b", "imports", "structural", 1.0),
        ]);
        assert_eq!(
            edges,
            vec![
                edge("a", "b", "calls", "semantic", 0.5),
                edge("a", "c", "calls", "structural", 1.0),
                edge("a", "b", "imports", "structural", 1.0),
            ]
        );
    }

    #[test]
    fn a_zero_weight_becomes_one() {
        let row = EdgeRow {
            source_id: "a".into(),
            target_id: "b".into(),
            rel_type: "calls".into(),
            edge_class: "structural".into(),
            analysis_level: "comprehensive".into(),
            weight: 0.0,
            raw_similarity: None,
            source_file: String::new(),
            target_file: String::new(),
            language: String::new(),
            annotations: "{}".into(),
            created_by: "ast".into(),
        };
        assert!((IndexEdge::from_row(row).weight - 1.0).abs() < f64::EPSILON);
    }

    #[test]
    fn node_rows_map_as_publish_py() {
        let row = NodeRow {
            node_id: "x.py::f".into(),
            rel_path: "x.py".into(),
            file_name: "x.py".into(),
            language: "python".into(),
            start_line: 3,
            end_line: 9,
            symbol_name: "f".into(),
            symbol_type: "function".into(),
            parent_symbol: None,
            analysis_level: "comprehensive".into(),
            source_text: None,
            docstring: "d".into(),
            signature: "def f()".into(),
            parameters: "[]".into(),
            return_type: String::new(),
            is_architectural: 1,
            is_doc: 0,
            is_test: 1,
            chunk_type: None,
            macro_cluster: None,
            micro_cluster: None,
            is_hub: 0,
            hub_assignment: None,
        };
        let node = IndexNode::from_row(row.clone()).ok();
        assert_eq!(
            node,
            Some(IndexNode {
                node_id: "x.py::f".into(),
                rel_path: "x.py".into(),
                file_name: "x.py".into(),
                language: "python".into(),
                start_line: 3,
                end_line: 9,
                symbol_name: "f".into(),
                symbol_type: "function".into(),
                source_text: String::new(),
                docstring: "d".into(),
                signature: "def f()".into(),
                is_architectural: true,
                is_test: true,
                ..IndexNode::default()
            })
        );
        let mut too_long = row;
        too_long.end_line = i64::from(i32::MAX) + 1;
        assert!(IndexNode::from_row(too_long).is_err());
    }
}
