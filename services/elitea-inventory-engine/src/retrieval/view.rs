//! A loaded graph, indexed for reading: what the Python tools read off
//! their in-memory `KnowledgeGraph` (its node and edge order, its
//! predecessor lists, its name index).
//!
//! Python always read a graph loaded from `graph.json`, so a node's
//! incoming edges come in the order their links appear in the document
//! (source node order, then each source's edge order) — which is the order
//! [`Graph::edges`] yields. `GraphView::in_edges` keeps that order.

use crate::graph::Graph;
use serde_json::{Map, Value};
use std::collections::HashMap;

/// A read-only graph with its indices.
#[derive(Debug, Clone, Default)]
pub struct GraphView {
    pub graph: Graph,
    /// Target → the sources of its incoming edges, in document order.
    predecessors: HashMap<String, Vec<String>>,
    /// Lowercased name → ids, in node order (`_entity_index`).
    by_name: HashMap<String, Vec<String>>,
    /// Lowercased type → ids, in node order (`_type_index`).
    by_type: HashMap<String, Vec<String>>,
    /// The graph's revision in the store.
    pub revision: i64,
}

impl GraphView {
    /// Index `graph` (stored at `revision`).
    #[must_use]
    pub fn new(graph: Graph, revision: i64) -> Self {
        let mut predecessors: HashMap<String, Vec<String>> = HashMap::new();
        for (source, target, _) in graph.edges() {
            predecessors
                .entry(target.to_owned())
                .or_default()
                .push(source.to_owned());
        }
        let mut by_name: HashMap<String, Vec<String>> = HashMap::new();
        let mut by_type: HashMap<String, Vec<String>> = HashMap::new();
        for (id, node) in graph.nodes() {
            if let Some(name) = node
                .get("name")
                .and_then(Value::as_str)
                .filter(|n| !n.is_empty())
            {
                by_name
                    .entry(name.to_lowercase())
                    .or_default()
                    .push(id.to_owned());
            }
            if let Some(kind) = node
                .get("type")
                .and_then(Value::as_str)
                .filter(|t| !t.is_empty())
            {
                by_type
                    .entry(kind.to_lowercase())
                    .or_default()
                    .push(id.to_owned());
            }
        }
        Self {
            graph,
            predecessors,
            by_name,
            by_type,
            revision,
        }
    }

    /// A node's attributes.
    #[must_use]
    pub fn node(&self, id: &str) -> Option<&Map<String, Value>> {
        self.graph.node(id)
    }

    /// `get_entity`: the node's attributes with its `id`.
    #[must_use]
    pub fn entity(&self, id: &str) -> Option<Map<String, Value>> {
        self.graph.node(id).map(|node| {
            let mut entity = node.clone();
            entity.insert("id".to_owned(), Value::String(id.to_owned()));
            entity
        })
    }

    /// Outgoing edges of `id`: `(target, attributes)`, in insertion order.
    pub fn out_edges<'a>(
        &'a self,
        id: &'a str,
    ) -> impl Iterator<Item = (&'a str, &'a Map<String, Value>)> + 'a {
        self.graph.successors(id)
    }

    /// Incoming edges of `id`: `(source, attributes)`, in document order.
    pub fn in_edges<'a>(
        &'a self,
        id: &'a str,
    ) -> impl Iterator<Item = (&'a str, &'a Map<String, Value>)> + 'a {
        self.predecessors
            .get(id)
            .into_iter()
            .flatten()
            .filter_map(move |source| {
                self.graph
                    .edge(source, id)
                    .map(|edge| (source.as_str(), edge))
            })
    }

    /// Ids of the entities named `name` (any case), in node order.
    #[must_use]
    pub fn ids_named(&self, name: &str) -> &[String] {
        self.by_name
            .get(&name.to_lowercase())
            .map_or(&[], Vec::as_slice)
    }

    /// Ids of the entities of `kind` (any case), in node order.
    #[must_use]
    pub fn ids_of_type(&self, kind: &str) -> &[String] {
        self.by_type
            .get(&kind.to_lowercase())
            .map_or(&[], Vec::as_slice)
    }
}
