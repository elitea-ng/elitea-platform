//! A loaded graph, indexed for reading: what the Python tools read off
//! their in-memory `KnowledgeGraph` (its node and edge order, its
//! predecessor lists, its name index).
//!
//! Python always read a graph loaded from `graph.json`, so a node's
//! incoming edges come in the order their links appear in the document
//! (source node order, then each source's edge order) — which is the order
//! [`Graph::edges`] yields. [`GraphView::in_edges`] keeps that order.

use crate::graph::Graph;
use crate::store::GraphRead;
use elitea_engine_core::pyvalue::py_truthy;
use serde_json::{Map, Value};
use std::collections::{HashMap, HashSet};

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
    /// The graph's restricted documents (ADR-0028 D3): `(source name,
    /// document key)` → readers. Empty for a graph of project-wide sources.
    pub restricted: HashMap<(String, String), elitea_content_source::Acl>,
    /// The entities that have a vector. The vectors themselves are not in
    /// the view (they are most of a graph's bytes and nothing here compares
    /// them: the store ranks); what a reader asks is whether and how many.
    embedded: HashSet<String>,
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
        let mut embedded: HashSet<String> = HashSet::new();
        for (id, node) in graph.nodes() {
            if node.get("embedding").is_some_and(py_truthy) {
                embedded.insert(id.to_owned());
            }
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
            restricted: HashMap::new(),
            embedded,
        }
    }

    /// The view of a stored graph read without its vectors
    /// ([`crate::store::GraphStore::load_view`]).
    #[must_use]
    pub fn from_read(read: GraphRead) -> Self {
        let mut view = Self::new(read.graph, read.revision);
        view.embedded = read.embedded.into_iter().collect();
        view
    }

    /// A view over `graph` with its vectors taken off the nodes and kept
    /// only as the set of embedded entities: what a graph just built (it
    /// carries its vectors, for the store) is read as.
    #[must_use]
    pub fn without_vectors(mut graph: Graph, revision: i64) -> Self {
        let mut embedded = HashSet::new();
        for (id, node) in graph.nodes_mut() {
            if node
                .remove("embedding")
                .is_some_and(|vector| py_truthy(&vector))
            {
                embedded.insert(id.to_owned());
            }
        }
        let mut view = Self::new(graph, revision);
        view.embedded = embedded;
        view
    }

    /// Whether the entity has a vector.
    #[must_use]
    pub fn is_embedded(&self, id: &str) -> bool {
        self.embedded.contains(id)
    }

    /// How many entities have a vector (`get_stats()['embeddings_count']`).
    #[must_use]
    pub fn embedded_count(&self) -> usize {
        self.embedded.len()
    }

    /// Whether any entity has a vector (`get_stats()['has_embeddings']`).
    #[must_use]
    pub fn has_embeddings(&self) -> bool {
        !self.embedded.is_empty()
    }

    /// The view `caller` may read, or `None` when it is this one (no
    /// restricted document hides anything from them).
    ///
    /// What a hidden document said goes, as an incremental run removes a
    /// deleted file ([`Graph::remove_file`]): its citations, the edges
    /// found in it, and the entities only it cited. A community that loses
    /// a member also loses its model-written label and summary — they were
    /// written from the hidden content — and keeps a neutral label.
    #[must_use]
    pub fn for_caller(&self, caller: &elitea_content_source::Caller) -> Option<Self> {
        let hidden: Vec<&(String, String)> = self
            .restricted
            .iter()
            .filter(|(_, acl)| !acl.admits(caller))
            .map(|(document, _)| document)
            .collect();
        if hidden.is_empty() {
            return None;
        }
        let mut graph = self.graph.clone();
        for (source, key) in hidden {
            graph.remove_file(source, key);
        }
        let kept: std::collections::HashSet<String> =
            graph.nodes().map(|(id, _)| id.to_owned()).collect();
        if let Some(Value::Object(communities)) = graph
            .metadata
            .get_mut("community_data")
            .and_then(|data| data.get_mut("communities"))
        {
            let present: std::collections::HashSet<String> =
                self.graph.nodes().map(|(id, _)| id.to_owned()).collect();
            for (index, (community_id, community)) in communities.iter_mut().enumerate() {
                let Some(fields) = community.as_object_mut() else {
                    continue;
                };
                let before = fields
                    .get("members")
                    .and_then(Value::as_array)
                    .map_or(0, Vec::len);
                if let Some(Value::Array(members)) = fields.get_mut("members") {
                    members.retain(|member| {
                        member
                            .as_str()
                            .is_none_or(|id| !present.contains(id) || kept.contains(id))
                    });
                }
                if let Some(Value::Array(centroids)) = fields.get_mut("centroids") {
                    centroids.retain(|centroid| {
                        centroid["id"].as_str().is_none_or(|id| kept.contains(id))
                    });
                }
                let after = fields
                    .get("members")
                    .and_then(Value::as_array)
                    .map_or(0, Vec::len);
                if after < before {
                    let _ = community_id;
                    fields.insert(
                        "label".to_owned(),
                        Value::String(format!("Community {index}")),
                    );
                    fields.insert("summary".to_owned(), Value::Null);
                }
            }
        }
        let mut filtered = Self::new(graph, self.revision);
        filtered.restricted.clone_from(&self.restricted);
        filtered.embedded = self
            .embedded
            .iter()
            .filter(|id| kept.contains(*id))
            .cloned()
            .collect();
        Some(filtered)
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
