//! The knowledge graph, as the Python engine's `KnowledgeGraph` keeps it
//! (`engine/inventory/knowledge_graph.py`): a directed graph, one edge per
//! ordered pair, every node and edge an open attribute map.
//!
//! What is carried over exactly, because ingestion and every read tool
//! depend on it:
//!
//! * **Entity merge** ([`Graph::add_entity`]): a second entity with an
//!   existing id only adds its citation, if not already there. Name, type and
//!   properties of the first writer stay.
//! * **Property filtering:** `content`, `text`, `raw`, `body` and
//!   `source_content` are never stored, nor `null`, nor a string of 1000 or
//!   more characters. Properties are spread onto the node after `id`, `name`,
//!   `type`, `layer` and `citations`, so a property of one of those names
//!   replaces it, as in Python.
//! * **Relations** ([`Graph::add_relation`]): the type is lowercased; a
//!   second relation between the same ordered pair merges into the first
//!   (the type is replaced, older attributes stay); a relation to an unknown
//!   entity is not added.
//! * **The node-link document** ([`Graph::to_node_link`],
//!   [`Graph::from_node_link`]): `graph.json` as `dump_to_json` writes it,
//!   so a graph exported here loads in the Python engine and the other way
//!   round. Node and edge order is insertion order, which the lexical search
//!   ranks ties by.
//!
//! What is deliberately NOT carried over:
//!
//! * **Type normalisation.** Python canonicalises the type inside
//!   `add_entity`; here the caller passes a canonical type. Normalisation is
//!   ingestion's job (ADR-0027 P3c), and the store must not rewrite stored
//!   types behind its back.
//! * **Stale indices.** Python maintains its name/type/file/document indices
//!   incrementally and they drift: a merged citation never reaches the file
//!   index, and a `name` property renames the node but not its index entry.
//!   [`Graph::indices`] derives them from the nodes every time, which is
//!   what Python's own `_rebuild_indices` computes.
//! * **Edge provenance loss.** networkx writes an edge as
//!   `{**attributes, "source": u, "target": v}`, so an edge's own `source`
//!   attribute (`"parser"`, `"llm"`) is overwritten on every save. The
//!   document still says what Python's says (the format is the contract),
//!   but the graph and the database keep the attribute.

use indexmap::IndexMap;
use serde_json::{Map, Value, json};

/// A property never stored on a node: raw content stays in the source.
const EXCLUDED_PROPERTIES: [&str; 5] = ["content", "text", "raw", "body", "source_content"];

/// A string property this long (in characters) or longer is not stored.
const MAX_PROPERTY_CHARS: usize = 1000;

/// The `_metadata.version` `dump_to_json` writes.
pub const DOCUMENT_VERSION: &str = "2.1";

/// `KnowledgeGraph.LAYER_TYPE_MAPPING`, in its order: a type listed twice
/// (`test_function`) takes the later layer, as the Python reverse map does.
const LAYERS: &[(&str, &[&str])] = &[
    (
        "code",
        &[
            "class",
            "function",
            "method",
            "module",
            "import",
            "variable",
            "constant",
            "attribute",
            "decorator",
            "exception",
            "enum",
            "class_reference",
            "class_import",
            "function_import",
            "function_reference",
            "function_call",
            "method_call",
            "test_function",
            "pydanticmodel",
        ],
    ),
    (
        "service",
        &[
            "api_endpoint",
            "rpc_method",
            "route",
            "service",
            "handler",
            "controller",
            "middleware",
            "event",
            "sio",
            "rpc",
        ],
    ),
    (
        "data",
        &[
            "model",
            "schema",
            "field",
            "table",
            "database",
            "migration",
            "entity",
            "pydantic_model",
            "dictionary",
            "list",
            "object",
        ],
    ),
    (
        "product",
        &[
            "feature",
            "capability",
            "platform",
            "product",
            "application",
            "menu",
            "ui_element",
            "ui_component",
            "interface_element",
        ],
    ),
    (
        "domain",
        &[
            "concept",
            "process",
            "action",
            "use_case",
            "workflow",
            "requirement",
            "guideline",
            "best_practice",
        ],
    ),
    (
        "documentation",
        &[
            "document",
            "guide",
            "section",
            "subsection",
            "tip",
            "example",
            "resource",
            "reference",
            "documentation",
        ],
    ),
    (
        "configuration",
        &[
            "configuration",
            "configuration_option",
            "configuration_section",
            "setting",
            "credential",
            "secret",
            "integration",
        ],
    ),
    (
        "testing",
        &["test", "test_case", "test_function", "fixture", "mock"],
    ),
    (
        "tooling",
        &["tool", "toolkit", "command", "node_type", "node"],
    ),
    (
        "knowledge",
        &[
            "fact",
            "algorithm",
            "behavior",
            "validation",
            "dependency",
            "error_handling",
            "decision",
            "definition",
            "date",
            "contact",
        ],
    ),
    (
        "structure",
        &[
            "file",
            "source_file",
            "document_file",
            "config_file",
            "web_file",
            "directory",
            "package",
        ],
    ),
];

/// The types of a layer (`LAYER_TYPE_MAPPING[layer]`), the layer name
/// matched exactly; `None` for a name that is not a layer.
#[must_use]
pub fn layer_types(layer: &str) -> Option<&'static [&'static str]> {
    LAYERS
        .iter()
        .find(|(name, _)| *name == layer)
        .map(|(_, types)| *types)
}

/// The layer `add_entity` assigns to a type (`TYPE_TO_LAYER`), matched
/// lowercase; `None` for a type outside every layer.
#[must_use]
pub fn layer_of(entity_type: &str) -> Option<&'static str> {
    let wanted = entity_type.to_lowercase();
    LAYERS
        .iter()
        .rev()
        .find(|(_, types)| types.contains(&wanted.as_str()))
        .map(|(layer, _)| *layer)
}

/// Where an entity was found: `Citation.to_dict()`.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Citation {
    pub file_path: String,
    pub line_start: Option<i64>,
    pub line_end: Option<i64>,
    pub source_toolkit: Option<String>,
    pub doc_id: Option<String>,
    pub content_hash: Option<String>,
}

impl Citation {
    /// The stored form, keys in `to_dict` order.
    #[must_use]
    pub fn to_value(&self) -> Value {
        json!({
            "file_path": self.file_path,
            "line_start": self.line_start,
            "line_end": self.line_end,
            "source_toolkit": self.source_toolkit,
            "doc_id": self.doc_id,
            "content_hash": self.content_hash,
        })
    }

    /// `Citation.from_dict`: missing keys are `None`, a missing path is
    /// empty; a value of the wrong type counts as missing.
    #[must_use]
    pub fn from_value(value: &Value) -> Self {
        let text = |key: &str| value.get(key).and_then(Value::as_str).map(str::to_owned);
        let line = |key: &str| value.get(key).and_then(Value::as_i64);
        Self {
            file_path: text("file_path").unwrap_or_default(),
            line_start: line("line_start"),
            line_end: line("line_end"),
            source_toolkit: text("source_toolkit"),
            doc_id: text("doc_id"),
            content_hash: text("content_hash"),
        }
    }
}

/// A node-link document this graph cannot hold.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FormatError(pub String);

impl std::fmt::Display for FormatError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(formatter, "not an Inventory graph document: {}", self.0)
    }
}

impl std::error::Error for FormatError {}

/// The derived lookup indices `_indices` holds, each key to its node ids in
/// node order.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Indices {
    /// Lowercased name → ids.
    pub entity_index: IndexMap<String, Vec<String>>,
    /// Type → ids.
    pub type_index: IndexMap<String, Vec<String>>,
    /// A `file_path` attribute or a citation's path → ids.
    pub file_index: IndexMap<String, Vec<String>>,
    /// A citation's `doc_id` → ids.
    pub source_doc_index: IndexMap<String, Vec<String>>,
}

impl Indices {
    fn to_value(&self) -> Value {
        let section = |index: &IndexMap<String, Vec<String>>| {
            Value::Object(
                index
                    .iter()
                    .map(|(key, ids)| (key.clone(), json!(ids)))
                    .collect(),
            )
        };
        json!({
            "entity_index": section(&self.entity_index),
            "type_index": section(&self.type_index),
            "file_index": section(&self.file_index),
            "source_doc_index": section(&self.source_doc_index),
        })
    }
}

fn index_into(index: &mut IndexMap<String, Vec<String>>, key: &str, id: &str) {
    let ids = index.entry(key.to_owned()).or_default();
    if !ids.iter().any(|known| known == id) {
        ids.push(id.to_owned());
    }
}

/// One Inventory knowledge graph.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Graph {
    /// Node id → attributes, in insertion order. The attributes hold `id`
    /// when the node was added here (Python keeps it too), not when it was
    /// loaded from a document (networkx drops it on load).
    nodes: IndexMap<String, Map<String, Value>>,
    /// Source id → target id → edge attributes; the edge order of a source
    /// is insertion order, and sources are walked in node order.
    edges: IndexMap<String, IndexMap<String, Map<String, Value>>>,
    /// The node-link `graph` attributes.
    pub attributes: Map<String, Value>,
    /// `_metadata`: embedding stamps, `community_data`, `last_saved`, …
    pub metadata: Map<String, Value>,
    /// `_schema`, when one was discovered.
    pub schema: Option<Value>,
}

impl Graph {
    /// An empty graph.
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// The number of nodes.
    #[must_use]
    pub fn node_count(&self) -> usize {
        self.nodes.len()
    }

    /// The number of edges.
    #[must_use]
    pub fn edge_count(&self) -> usize {
        self.edges.values().map(IndexMap::len).sum()
    }

    /// A node's attributes.
    #[must_use]
    pub fn node(&self, id: &str) -> Option<&Map<String, Value>> {
        self.nodes.get(id)
    }

    /// Every node, in insertion order.
    pub fn nodes(&self) -> impl Iterator<Item = (&str, &Map<String, Value>)> {
        self.nodes.iter().map(|(id, node)| (id.as_str(), node))
    }

    /// Every edge `(source, target, attributes)`, in export order.
    pub fn edges(&self) -> impl Iterator<Item = (&str, &str, &Map<String, Value>)> {
        self.nodes.keys().flat_map(move |source| {
            self.edges.get(source).into_iter().flat_map(move |targets| {
                targets
                    .iter()
                    .map(move |(target, edge)| (source.as_str(), target.as_str(), edge))
            })
        })
    }

    /// Insert a node as loaded (no merging, no filtering), or replace one.
    pub fn insert_node(&mut self, id: String, attributes: Map<String, Value>) {
        self.nodes.insert(id, attributes);
    }

    /// The edge from `source` to `target`, if there is one.
    #[must_use]
    pub fn edge(&self, source: &str, target: &str) -> Option<&Map<String, Value>> {
        self.edges.get(source)?.get(target)
    }

    /// The outgoing edges of `source`: `(target, attributes)`, in insertion
    /// order.
    pub fn successors<'a>(
        &'a self,
        source: &str,
    ) -> impl Iterator<Item = (&'a str, &'a Map<String, Value>)> + 'a {
        self.edges
            .get(source)
            .into_iter()
            .flat_map(|targets| targets.iter().map(|(target, edge)| (target.as_str(), edge)))
    }

    /// Keep only the edges `keep` accepts (`source`, `target`, attributes);
    /// returns how many were removed.
    pub fn retain_edges(
        &mut self,
        mut keep: impl FnMut(&str, &str, &Map<String, Value>) -> bool,
    ) -> usize {
        let mut removed = 0;
        for (source, targets) in &mut self.edges {
            let before = targets.len();
            targets.retain(|target, edge| keep(source, target, edge));
            removed += before - targets.len();
        }
        self.edges.retain(|_, targets| !targets.is_empty());
        removed
    }

    /// Insert an edge as loaded, creating a missing endpoint as an empty
    /// node the way networkx's `add_edge` does; attributes merge into an
    /// existing edge.
    pub fn insert_edge(&mut self, source: &str, target: &str, attributes: Map<String, Value>) {
        for endpoint in [source, target] {
            if !self.nodes.contains_key(endpoint) {
                self.nodes.insert(endpoint.to_owned(), Map::new());
            }
        }
        self.edges
            .entry(source.to_owned())
            .or_default()
            .entry(target.to_owned())
            .or_default()
            .extend(attributes);
    }

    /// `KnowledgeGraph.add_entity` with an already-canonical type.
    ///
    /// Returns `entity_id`, as Python does.
    pub fn add_entity(
        &mut self,
        entity_id: &str,
        name: &str,
        entity_type: &str,
        citation: Option<&Citation>,
        properties: Option<&Map<String, Value>>,
    ) -> String {
        if let Some(existing) = self.nodes.get_mut(entity_id)
            && !existing.is_empty()
        {
            if let Some(citation) = citation {
                let new = citation.to_value();
                let mut citations = match existing.get("citations") {
                    Some(Value::Array(list)) => list.clone(),
                    _ => Vec::new(),
                };
                if let Some(legacy) = existing.get("citation").filter(|legacy| truthy(legacy))
                    && !citations.contains(legacy)
                {
                    citations.push(legacy.clone());
                }
                if !citations.contains(&new) {
                    citations.push(new);
                }
                existing.insert("citations".to_owned(), Value::Array(citations));
                existing.shift_remove("citation");
            }
            return entity_id.to_owned();
        }

        let mut node = Map::new();
        node.insert("id".to_owned(), json!(entity_id));
        node.insert("name".to_owned(), json!(name));
        node.insert("type".to_owned(), json!(entity_type));
        if let Some(layer) = layer_of(entity_type) {
            node.insert("layer".to_owned(), json!(layer));
        }
        if let Some(citation) = citation {
            node.insert("citations".to_owned(), json!([citation.to_value()]));
        }
        for (key, value) in properties.into_iter().flatten() {
            let stored = !EXCLUDED_PROPERTIES.contains(&key.as_str())
                && match value {
                    Value::Null => false,
                    Value::String(text) => text.chars().count() < MAX_PROPERTY_CHARS,
                    _ => true,
                };
            if stored {
                node.insert(key.clone(), value.clone());
            }
        }
        match self.nodes.get_mut(entity_id) {
            // networkx add_node on an attribute-less node updates it in place.
            Some(empty) => empty.extend(node),
            None => {
                self.nodes.insert(entity_id.to_owned(), node);
            }
        }
        entity_id.to_owned()
    }

    /// `KnowledgeGraph.add_relation`: `false` (nothing added) when either
    /// endpoint is unknown.
    pub fn add_relation(
        &mut self,
        source_id: &str,
        target_id: &str,
        relation_type: &str,
        properties: Option<&Map<String, Value>>,
    ) -> bool {
        if !self.nodes.contains_key(source_id) || !self.nodes.contains_key(target_id) {
            return false;
        }
        let mut edge = Map::new();
        edge.insert(
            "relation_type".to_owned(),
            json!(relation_type.to_lowercase()),
        );
        if let Some(properties) = properties {
            edge.extend(properties.clone());
        }
        self.insert_edge(source_id, target_id, edge);
        true
    }

    /// Forget what one file of one source said: its citations, the edges
    /// discovered in it, and every node left with no citation at all
    /// (with that node's edges). A node another file or source still
    /// cites stays, with its other citations. Returns the number of nodes
    /// removed.
    ///
    /// This is what an incremental run does to a changed or deleted file
    /// before reading it again. A node that never had citations (none
    /// added by ingestion) is not touched.
    pub fn remove_file(&mut self, source_toolkit: &str, file_path: &str) -> usize {
        let cites = |citation: &Value| {
            citation.get("file_path").and_then(Value::as_str) == Some(file_path)
                && citation.get("source_toolkit").and_then(Value::as_str) == Some(source_toolkit)
        };
        let mut orphaned = Vec::new();
        for (id, node) in &mut self.nodes {
            let Some(Value::Array(citations)) = node.get_mut("citations") else {
                continue;
            };
            let before = citations.len();
            citations.retain(|citation| !cites(citation));
            if citations.is_empty() && before > 0 {
                orphaned.push(id.clone());
            }
        }
        for id in &orphaned {
            self.nodes.shift_remove(id);
            self.edges.shift_remove(id);
        }
        let discovered_here = |edge: &Map<String, Value>| {
            edge.get("discovered_in_file").and_then(Value::as_str) == Some(file_path)
                && edge.get("source_toolkit").and_then(Value::as_str) == Some(source_toolkit)
        };
        for targets in self.edges.values_mut() {
            targets.retain(|target, edge| !orphaned.contains(target) && !discovered_here(edge));
        }
        self.edges.retain(|_, targets| !targets.is_empty());
        orphaned.len()
    }

    /// Set a node's embedding vector; `false` for an unknown node.
    pub fn set_embedding(&mut self, entity_id: &str, vector: &[f64]) -> bool {
        let Some(node) = self.nodes.get_mut(entity_id) else {
            return false;
        };
        node.insert("embedding".to_owned(), json!(vector));
        true
    }

    /// `KnowledgeGraph.set_community_data`: store the detection result and
    /// stamp `community_id` on every member that is a node.
    pub fn set_community_data(&mut self, community_data: Value) {
        if let Some(communities) = community_data.get("communities").and_then(Value::as_object) {
            for (community, info) in communities {
                for member in info
                    .get("members")
                    .and_then(Value::as_array)
                    .into_iter()
                    .flatten()
                    .filter_map(Value::as_str)
                {
                    if let Some(node) = self.nodes.get_mut(member) {
                        node.insert("community_id".to_owned(), json!(community));
                    }
                }
            }
        }
        self.metadata
            .insert("community_data".to_owned(), community_data);
    }

    /// The lookup indices, derived from the nodes (`_rebuild_indices`).
    #[must_use]
    pub fn indices(&self) -> Indices {
        let mut indices = Indices::default();
        for (id, node) in &self.nodes {
            if let Some(name) = node.get("name").and_then(Value::as_str)
                && !name.is_empty()
            {
                index_into(&mut indices.entity_index, &name.to_lowercase(), id);
            }
            if let Some(kind) = node.get("type").and_then(Value::as_str)
                && !kind.is_empty()
            {
                index_into(&mut indices.type_index, kind, id);
            }
            if let Some(path) = node.get("file_path").and_then(Value::as_str)
                && !path.is_empty()
            {
                index_into(&mut indices.file_index, path, id);
            }
            for citation in node
                .get("citations")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
            {
                if let Some(path) = citation.get("file_path").and_then(Value::as_str)
                    && !path.is_empty()
                {
                    index_into(&mut indices.file_index, path, id);
                }
                if let Some(doc) = citation.get("doc_id").and_then(Value::as_str)
                    && !doc.is_empty()
                {
                    index_into(&mut indices.source_doc_index, doc, id);
                }
            }
        }
        indices
    }

    /// The `graph.json` document `dump_to_json` writes, stamped `saved_at`
    /// (`_metadata.last_saved`).
    #[must_use]
    pub fn to_node_link(&self, saved_at: &str) -> Value {
        let nodes: Vec<Value> = self
            .nodes
            .iter()
            .map(|(id, attributes)| {
                let mut node = attributes.clone();
                node.insert("id".to_owned(), json!(id));
                Value::Object(node)
            })
            .collect();
        let links: Vec<Value> = self
            .edges()
            .map(|(source, target, attributes)| {
                let mut link = attributes.clone();
                link.insert("source".to_owned(), json!(source));
                link.insert("target".to_owned(), json!(target));
                Value::Object(link)
            })
            .collect();
        let mut document = Map::new();
        document.insert("directed".to_owned(), json!(true));
        document.insert("multigraph".to_owned(), json!(false));
        document.insert("graph".to_owned(), Value::Object(self.attributes.clone()));
        document.insert("nodes".to_owned(), Value::Array(nodes));
        document.insert("links".to_owned(), Value::Array(links));
        document.insert("_indices".to_owned(), self.indices().to_value());
        if let Some(schema) = self.schema.as_ref().filter(|schema| truthy(schema)) {
            document.insert("_schema".to_owned(), schema.clone());
        }
        let mut metadata = self.metadata.clone();
        metadata.insert("last_saved".to_owned(), json!(saved_at));
        metadata.insert("version".to_owned(), json!(DOCUMENT_VERSION));
        document.insert("_metadata".to_owned(), Value::Object(metadata));
        Value::Object(document)
    }

    /// `graph.json` text, byte-compatible with `json.dump(indent=2)`.
    #[must_use]
    pub fn to_json_text(&self, saved_at: &str) -> String {
        elitea_engine_core::pyjson::dumps_with(&self.to_node_link(saved_at), Some(2), true)
    }

    /// Read a `graph.json` document (`load_from_json`).
    ///
    /// `_indices` is ignored (they are derived), `edges` is read when there
    /// is no `links`, and relation types are lowercased.
    ///
    /// # Errors
    ///
    /// A document that is not a directed, simple node-link graph with
    /// string node ids.
    pub fn from_node_link(document: &Value) -> Result<Self, FormatError> {
        let Some(fields) = document.as_object() else {
            return Err(FormatError("the document is not a JSON object".to_owned()));
        };
        if fields.get("directed") == Some(&Value::Bool(false)) {
            return Err(FormatError("the graph is undirected".to_owned()));
        }
        if fields.get("multigraph") == Some(&Value::Bool(true)) {
            return Err(FormatError("the graph is a multigraph".to_owned()));
        }
        let mut graph = Self {
            attributes: object_field(fields, "graph")?,
            metadata: object_field(fields, "_metadata")?,
            schema: fields
                .get("_schema")
                .filter(|schema| !schema.is_null())
                .cloned(),
            ..Self::default()
        };
        for node in array_field(fields, "nodes")? {
            let Some(mut attributes) = node.as_object().cloned() else {
                return Err(FormatError("a node is not an object".to_owned()));
            };
            let id = take_id(&mut attributes, "id", "a node")?;
            graph.insert_node(id, attributes);
        }
        let links_key = if fields.contains_key("links") || !fields.contains_key("edges") {
            "links"
        } else {
            "edges"
        };
        for link in array_field(fields, links_key)? {
            let Some(mut attributes) = link.as_object().cloned() else {
                return Err(FormatError("a link is not an object".to_owned()));
            };
            let source = take_id(&mut attributes, "source", "a link")?;
            let target = take_id(&mut attributes, "target", "a link")?;
            if let Some(Value::String(kind)) = attributes.get_mut("relation_type") {
                *kind = kind.to_lowercase();
            }
            graph.insert_edge(&source, &target, attributes);
        }
        Ok(graph)
    }
}

/// Python truthiness of a JSON value.
fn truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(flag) => *flag,
        Value::Number(number) => number.as_f64().is_some_and(|n| n != 0.0),
        Value::String(text) => !text.is_empty(),
        Value::Array(items) => !items.is_empty(),
        Value::Object(fields) => !fields.is_empty(),
    }
}

fn object_field(fields: &Map<String, Value>, key: &str) -> Result<Map<String, Value>, FormatError> {
    match fields.get(key) {
        None | Some(Value::Null) => Ok(Map::new()),
        Some(Value::Object(object)) => Ok(object.clone()),
        Some(_) => Err(FormatError(format!("`{key}` is not an object"))),
    }
}

fn array_field<'d>(fields: &'d Map<String, Value>, key: &str) -> Result<&'d [Value], FormatError> {
    match fields.get(key) {
        None => Ok(&[]),
        Some(Value::Array(items)) => Ok(items),
        Some(_) => Err(FormatError(format!("`{key}` is not a list"))),
    }
}

fn take_id(
    attributes: &mut Map<String, Value>,
    key: &str,
    what: &str,
) -> Result<String, FormatError> {
    match attributes.shift_remove(key) {
        Some(Value::String(id)) => Ok(id),
        Some(other) => Err(FormatError(format!(
            "{what} has a non-string `{key}` ({other})"
        ))),
        None => Err(FormatError(format!("{what} has no `{key}`"))),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn layers_follow_the_python_reverse_map() {
        assert_eq!(layer_of("class"), Some("code"));
        assert_eq!(layer_of("Class"), Some("code"));
        // Listed under code and testing; testing comes later and wins.
        assert_eq!(layer_of("test_function"), Some("testing"));
        assert_eq!(layer_of("source_file"), Some("structure"));
        assert_eq!(layer_of("widget_thing"), None);
    }

    #[test]
    fn a_loaded_document_rejects_what_the_model_cannot_hold() {
        for (document, needle) in [
            (json!([]), "not a JSON object"),
            (json!({"directed": false}), "undirected"),
            (json!({"multigraph": true}), "multigraph"),
            (json!({"nodes": [{"name": "x"}]}), "has no `id`"),
            (json!({"nodes": [{"id": 7}]}), "non-string `id`"),
            (json!({"links": [{"source": "a"}]}), "has no `target`"),
            (json!({"nodes": {}}), "`nodes` is not a list"),
        ] {
            let refused = Graph::from_node_link(&document);
            assert!(
                refused.as_ref().is_err_and(|e| e.0.contains(needle)),
                "{document}: {refused:?}"
            );
        }
    }

    #[test]
    fn a_link_to_an_unlisted_node_creates_it_and_edges_is_read_without_links() {
        let Ok(graph) = Graph::from_node_link(&json!({
            "nodes": [{"id": "a", "name": "A"}],
            "edges": [{"source": "a", "target": "b", "relation_type": "CALLS"}],
        })) else {
            panic!("loads");
        };
        assert_eq!(graph.node_count(), 2);
        assert_eq!(graph.node("b"), Some(&Map::new()));
        let edges: Vec<_> = graph.edges().collect();
        assert_eq!(edges.len(), 1);
        assert_eq!(edges[0].2.get("relation_type"), Some(&json!("calls")));
    }

    #[test]
    fn the_edge_source_attribute_survives_in_the_graph_but_not_the_document() {
        let mut graph = Graph::new();
        graph.add_entity("a", "A", "class", None, None);
        graph.add_entity("b", "B", "class", None, None);
        let provenance = json!({"source": "parser"});
        assert!(graph.add_relation("a", "b", "CALLS", provenance.as_object()));
        assert!(!graph.add_relation("a", "nowhere", "calls", None));
        let (_, _, edge) = graph.edges().next().unwrap_or_else(|| panic!("an edge"));
        assert_eq!(edge.get("source"), Some(&json!("parser")));
        let document = graph.to_node_link("now");
        assert_eq!(document["links"][0]["source"], json!("a"));
    }

    #[test]
    fn removing_a_file_keeps_what_others_still_cite() {
        let cite = |file: &str, source: &str| Citation {
            file_path: file.to_owned(),
            source_toolkit: Some(source.to_owned()),
            ..Citation::default()
        };
        let mut graph = Graph::new();
        graph.add_entity(
            "shared",
            "Shared",
            "concept",
            Some(&cite("a.py", "repo")),
            None,
        );
        graph.add_entity(
            "shared",
            "Shared",
            "concept",
            Some(&cite("b.py", "repo")),
            None,
        );
        graph.add_entity(
            "only_a",
            "OnlyA",
            "class",
            Some(&cite("a.py", "repo")),
            None,
        );
        graph.add_entity(
            "other_source",
            "X",
            "class",
            Some(&cite("a.py", "wiki")),
            None,
        );
        graph.add_entity("uncited", "U", "class", None, None);
        let found_in = |file: &str| json!({"discovered_in_file": file, "source_toolkit": "repo"});
        graph.add_relation("shared", "only_a", "uses", None);
        graph.add_relation(
            "shared",
            "other_source",
            "uses",
            found_in("a.py").as_object(),
        );
        graph.add_relation(
            "other_source",
            "shared",
            "uses",
            found_in("b.py").as_object(),
        );

        assert_eq!(graph.remove_file("repo", "a.py"), 1);
        assert!(graph.node("only_a").is_none());
        assert_eq!(
            graph.node("shared").and_then(|n| n.get("citations")),
            Some(&json!([cite("b.py", "repo").to_value()]))
        );
        assert!(
            graph.node("other_source").is_some(),
            "another source's citation"
        );
        assert!(graph.node("uncited").is_some());
        let edges: Vec<(&str, &str)> = graph.edges().map(|(s, t, _)| (s, t)).collect();
        assert_eq!(edges, [("other_source", "shared")]);
    }
}
