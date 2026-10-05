//! The code graph: a directed multigraph of symbols and documents.
//!
//! The Python engine builds an `nx.MultiDiGraph` and mirrors it into its
//! index with `UnifiedWikiDB.from_networkx`. This is the Rust graph, and
//! [`CodeGraph::node_rows`] / [`CodeGraph::edge_rows`] are that mirror: the
//! exact `repo_nodes` / `repo_edges` rows, derived columns included
//! (`is_architectural`, `is_doc`, `is_test`). The ADR-0026 graph gate
//! compares these rows with the Python engine's.
//!
//! The storage copies networkx's, because the passes depend on its ORDER,
//! not only on its content:
//!
//! * nodes iterate in insertion order; a removed node that is added again
//!   goes to the end;
//! * edges iterate by source node, then by target in the order that target
//!   was first connected to the source, then by key — NOT in insertion
//!   order. Contraction keeps the last of several parallel edges it sees,
//!   so this order decides which edge receives merged annotations;
//! * an edge without an explicit key gets the first free integer from
//!   `len(keydict)` up (`MultiGraph.new_edge_key`); an edge added with a key
//!   that exists replaces that edge's data.
//!
//! Removal leaves a tombstone in the slot vector, so contraction (which
//! removes tens of thousands of nodes) does not shift the vector each time.

#![cfg_attr(
    not(test),
    deny(clippy::expect_used, clippy::panic, clippy::unwrap_used)
)]

pub mod api_surface;
pub mod attributes;
pub mod builder;
pub mod constants;
pub mod contraction;
pub mod cross_language;
pub mod discover;
pub mod documents;
pub mod flags;
pub mod helpers;
pub mod markdown_structure;
pub mod orm;
pub mod phase1c;
pub mod pyre;
pub mod pystr;
pub mod repo_files;
pub mod shared_str;
pub mod sql;
pub mod test_linker;

use crate::parsers::model::Symbol;
pub use attributes::Attributes;
use serde::Serialize;
use serde_json::Value;
pub use shared_str::SharedStr;
use std::borrow::Cow;
use std::collections::HashMap;

/// The attributes of one node. The typed fields are the index columns; the
/// rest of what a pass attaches lives in `extra`.
///
/// An empty string stands for an attribute the Python node does not carry:
/// every Python reader uses `data.get(key, "")` or a truthiness test, which
/// cannot tell the two apart either.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct NodeData {
    pub rel_path: SharedStr,
    pub file_name: SharedStr,
    pub language: SharedStr,
    pub start_line: i64,
    pub end_line: i64,
    pub symbol_name: String,
    pub symbol_type: Label,
    pub parent_symbol: Option<String>,
    pub analysis_level: Label,
    pub source_text: String,
    pub docstring: String,
    pub signature: String,
    /// JSON text of the parameter list as the builder set it (`"[]"` for an
    /// empty list); `""` when no pass set it, as Python's `.get` default.
    pub parameters: String,
    pub return_type: String,
    pub chunk_type: Option<String>,
    /// The absolute path the node came from (`file_path`). Not a column, but
    /// the builder's collision rule compares it.
    pub file_path: SharedStr,
    /// The parser symbol of a rich-tier node (`symbol`), reduced to the
    /// attributes something reads: the index reads `source_text`,
    /// `docstring`, `signature` and `return_type` from it when the node has
    /// no attribute of that name, and the Phase 1c API surface pass reads
    /// its source text the same way.
    pub symbol: Option<Box<NodeSymbol>>,
    pub extra: Attributes,
}

/// The attributes of a parser [`Symbol`] that a rich node keeps: the ones
/// the index and the Phase 1c passes read (see [`NodeData::symbol`]). The
/// rest of the symbol (its path, metadata, comments, …) is in the node's
/// own columns or read by nothing, so it is not kept.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct NodeSymbol {
    pub source_text: Option<String>,
    pub docstring: Option<String>,
    pub signature: Option<String>,
    pub return_type: Option<String>,
}

impl From<Symbol> for NodeSymbol {
    fn from(symbol: Symbol) -> Self {
        Self {
            source_text: symbol.source_text,
            docstring: symbol.docstring,
            signature: symbol.signature,
            return_type: symbol.return_type,
        }
    }
}

/// A short edge attribute drawn from a fixed vocabulary (`rel_type`,
/// `edge_class`, …): a literal is borrowed, not allocated, once per edge.
pub type Label = Cow<'static, str>;

/// The attributes of one edge.
#[derive(Debug, Clone, PartialEq)]
pub struct EdgeData {
    pub rel_type: Label,
    pub edge_class: Label,
    pub analysis_level: Label,
    pub weight: f64,
    pub raw_similarity: Option<f64>,
    pub source_file: SharedStr,
    pub target_file: SharedStr,
    pub language: String,
    pub annotations: Attributes,
    pub created_by: Label,
    /// The `source_context` / `target_context` attributes of a structural
    /// edge: the parser's source and target symbol names, which later
    /// phases read (the Python wiki agents). Empty when the edge does not
    /// carry the attribute, as for [`NodeData`]. Typed fields rather than
    /// `extra` entries: every structural edge has them, and a map per edge
    /// costs several allocations.
    pub source_context: String,
    pub target_context: String,
    pub extra: Attributes,
}

impl Default for EdgeData {
    fn default() -> Self {
        Self {
            rel_type: Label::Borrowed(""),
            edge_class: Label::Borrowed("structural"),
            analysis_level: Label::Borrowed("comprehensive"),
            weight: 1.0,
            raw_similarity: None,
            source_file: SharedStr::default(),
            target_file: SharedStr::default(),
            language: String::new(),
            annotations: Attributes::new(),
            created_by: Label::Borrowed("ast"),
            source_context: String::new(),
            target_context: String::new(),
            extra: Attributes::new(),
        }
    }
}

/// A networkx edge key: an integer the graph chose, or a string a pass gave.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum EdgeKey {
    Auto(u64),
    Named(String),
}

/// One edge, borrowed from the graph.
#[derive(Debug, Clone, Copy)]
pub struct EdgeRef<'g> {
    pub source: &'g str,
    pub target: &'g str,
    pub key: &'g EdgeKey,
    pub data: &'g EdgeData,
}

/// The parallel edges from one node to another, by key, in insertion order
/// (a networkx keydict). Almost every node pair has ONE edge, so this is a
/// vector grown one element at a time with a linear key search, not an
/// `IndexMap`: an `IndexMap` reserves room for four inline `EdgeData`s and
/// a hash table per pair, which was most of the graph's memory. The data is
/// boxed so that an edge prepared outside the graph moves in without a copy.
#[derive(Debug, Clone, Default)]
struct KeyDict(Vec<(EdgeKey, Box<EdgeData>)>);

impl KeyDict {
    fn len(&self) -> usize {
        self.0.len()
    }

    fn is_empty(&self) -> bool {
        self.0.is_empty()
    }

    fn position(&self, key: &EdgeKey) -> Option<usize> {
        self.0.iter().position(|(k, _)| k == key)
    }

    fn contains_key(&self, key: &EdgeKey) -> bool {
        self.position(key).is_some()
    }

    fn get_mut(&mut self, key: &EdgeKey) -> Option<&mut EdgeData> {
        let index = self.position(key)?;
        self.0.get_mut(index).map(|(_, data)| &mut **data)
    }

    /// Insert or replace in place; the old data when the key existed.
    fn insert(&mut self, key: EdgeKey, data: Box<EdgeData>) -> Option<Box<EdgeData>> {
        if let Some(index) = self.position(&key) {
            return self
                .0
                .get_mut(index)
                .map(|(_, old)| std::mem::replace(old, data));
        }
        self.0.reserve_exact(1);
        self.0.push((key, data));
        None
    }

    /// Remove keeping the order of the others (`IndexMap::shift_remove`).
    fn shift_remove(&mut self, key: &EdgeKey) -> Option<Box<EdgeData>> {
        let index = self.position(key)?;
        let (_, data) = self.0.remove(index);
        if self.0.capacity() > self.0.len() * 2 {
            self.0.shrink_to_fit();
        }
        Some(data)
    }

    fn iter(&self) -> impl Iterator<Item = (&EdgeKey, &EdgeData)> {
        self.0.iter().map(|(key, data)| (key, &**data))
    }
}

impl IntoIterator for KeyDict {
    type Item = (EdgeKey, Box<EdgeData>);
    type IntoIter = std::vec::IntoIter<(EdgeKey, Box<EdgeData>)>;

    fn into_iter(self) -> Self::IntoIter {
        self.0.into_iter()
    }
}

/// Push without the default growth to four elements: most adjacency lists
/// here hold one or two entries, and there are hundreds of thousands of
/// them. Past four the usual doubling keeps pushes amortised.
fn push_small<T>(items: &mut Vec<T>, item: T) {
    if items.len() == items.capacity() && items.capacity() < 4 {
        items.reserve_exact(1);
    }
    items.push(item);
}

/// The successors of a node: target slot → parallel edges, in
/// first-connection order (an `IndexMap` with a linear search; see
/// [`KeyDict`] for why not a map).
#[derive(Debug, Clone, Default)]
struct Successors(Vec<(usize, KeyDict)>);

impl Successors {
    fn position(&self, target: usize) -> Option<usize> {
        self.0.iter().position(|(t, _)| *t == target)
    }

    fn get(&self, target: usize) -> Option<&KeyDict> {
        self.0
            .iter()
            .find(|(t, _)| *t == target)
            .map(|(_, keys)| keys)
    }

    fn get_mut(&mut self, target: usize) -> Option<&mut KeyDict> {
        self.0
            .iter_mut()
            .find(|(t, _)| *t == target)
            .map(|(_, keys)| keys)
    }

    /// The keydict of `target`, appended empty when missing.
    fn entry(&mut self, target: usize) -> &mut KeyDict {
        let index = self.position(target).unwrap_or_else(|| {
            push_small(&mut self.0, (target, KeyDict::default()));
            self.0.len() - 1
        });
        &mut self.0[index].1
    }

    /// Remove keeping the order of the others.
    fn shift_remove(&mut self, target: usize) -> Option<KeyDict> {
        let index = self.position(target)?;
        Some(self.0.remove(index).1)
    }

    fn iter(&self) -> impl Iterator<Item = (usize, &KeyDict)> {
        self.0.iter().map(|(target, keys)| (*target, keys))
    }
}

/// The predecessors of a node: source slots in first-connection order (an
/// `IndexSet` with a linear search).
#[derive(Debug, Clone, Default)]
struct Predecessors(Vec<usize>);

impl Predecessors {
    fn insert(&mut self, source: usize) {
        if !self.0.contains(&source) {
            push_small(&mut self.0, source);
        }
    }

    fn shift_remove(&mut self, source: usize) {
        if let Some(index) = self.0.iter().position(|s| *s == source) {
            self.0.remove(index);
        }
    }

    fn iter(&self) -> impl Iterator<Item = usize> + '_ {
        self.0.iter().copied()
    }
}

#[derive(Debug, Clone)]
struct Slot {
    id: String,
    data: NodeData,
    /// Target slot → parallel edges, in first-connection order.
    succ: Successors,
    /// Source slots, in first-connection order.
    pred: Predecessors,
}

/// The code graph.
#[derive(Debug, Clone, Default)]
pub struct CodeGraph {
    /// Boxed: a tombstone then costs a pointer, and growing the vector
    /// moves pointers, not whole slots.
    slots: Vec<Option<Box<Slot>>>,
    index: HashMap<String, usize>,
    edge_count: usize,
}

/// A `repo_nodes` row.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct NodeRow {
    pub node_id: String,
    pub rel_path: String,
    pub file_name: String,
    pub language: String,
    pub start_line: i64,
    pub end_line: i64,
    pub symbol_name: String,
    pub symbol_type: String,
    pub parent_symbol: Option<String>,
    pub analysis_level: String,
    /// `NULL` when a parser symbol carries no source text: the index falls
    /// back to `symbol.source_text`, which may be `None`.
    pub source_text: Option<String>,
    pub docstring: String,
    pub signature: String,
    pub parameters: String,
    pub return_type: String,
    pub is_architectural: u8,
    pub is_doc: u8,
    pub is_test: u8,
    pub chunk_type: Option<String>,
    pub macro_cluster: Option<i64>,
    pub micro_cluster: Option<i64>,
    pub is_hub: u8,
    pub hub_assignment: Option<String>,
}

/// A `repo_edges` row.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct EdgeRow {
    pub source_id: String,
    pub target_id: String,
    pub rel_type: String,
    pub edge_class: String,
    pub analysis_level: String,
    pub weight: f64,
    pub raw_similarity: Option<f64>,
    pub source_file: String,
    pub target_file: String,
    pub language: String,
    /// JSON text of the annotations object (Python `json.dumps`).
    pub annotations: String,
    pub created_by: String,
}

impl CodeGraph {
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    fn slot(&self, index: usize) -> Option<&Slot> {
        self.slots.get(index).and_then(Option::as_deref)
    }

    fn slot_mut(&mut self, index: usize) -> Option<&mut Slot> {
        self.slots.get_mut(index).and_then(Option::as_deref_mut)
    }

    /// The slot of `id`, created EMPTY when missing (networkx creates a
    /// missing edge endpoint with no attributes).
    fn ensure(&mut self, id: &str) -> usize {
        if let Some(&index) = self.index.get(id) {
            return index;
        }
        let index = self.slots.len();
        self.slots.push(Some(Box::new(Slot {
            id: id.to_owned(),
            data: NodeData::default(),
            succ: Successors::default(),
            pred: Predecessors::default(),
        })));
        self.index.insert(id.to_owned(), index);
        index
    }

    /// Add a node, or REPLACE the data of an existing one, keeping its
    /// position and merging `extra` — networkx `add_node(id, **attrs)`
    /// updates the attributes it names. Every builder call names the same
    /// attribute set for a given id, so replacing the typed fields is the
    /// same update.
    pub fn add_node(&mut self, id: impl AsRef<str>, data: NodeData) {
        let index = self.ensure(id.as_ref());
        if let Some(slot) = self.slot_mut(index) {
            let mut merged = data;
            let mut extra = std::mem::take(&mut slot.data.extra);
            extra.extend(std::mem::take(&mut merged.extra));
            merged.extra = extra;
            slot.data = merged;
        }
    }

    /// Add an edge with the next free integer key; a missing endpoint is
    /// created EMPTY, source first.
    pub fn add_edge(
        &mut self,
        source: impl AsRef<str>,
        target: impl AsRef<str>,
        data: EdgeData,
    ) -> EdgeKey {
        self.add_edge_boxed(source, target, Box::new(data))
    }

    /// [`CodeGraph::add_edge`] with the data already boxed (the graph
    /// stores it boxed; a builder that holds edges before adding them boxes
    /// them once).
    pub fn add_edge_boxed(
        &mut self,
        source: impl AsRef<str>,
        target: impl AsRef<str>,
        data: Box<EdgeData>,
    ) -> EdgeKey {
        let u = self.ensure(source.as_ref());
        let v = self.ensure(target.as_ref());
        let key = self.slot(u).map_or(EdgeKey::Auto(0), |slot| {
            slot.succ.get(v).map_or(EdgeKey::Auto(0), |keys| {
                let mut next = keys.len() as u64;
                while keys.contains_key(&EdgeKey::Auto(next)) {
                    next += 1;
                }
                EdgeKey::Auto(next)
            })
        });
        self.insert_edge(u, v, key.clone(), data);
        key
    }

    /// Add an edge under `key`; an edge with that key is REPLACED (networkx
    /// updates its data dict; every caller passes the full attribute set).
    pub fn add_edge_keyed(
        &mut self,
        source: impl AsRef<str>,
        target: impl AsRef<str>,
        key: EdgeKey,
        data: EdgeData,
    ) {
        self.add_edge_keyed_boxed(source, target, key, Box::new(data));
    }

    fn add_edge_keyed_boxed(
        &mut self,
        source: impl AsRef<str>,
        target: impl AsRef<str>,
        key: EdgeKey,
        data: Box<EdgeData>,
    ) {
        let u = self.ensure(source.as_ref());
        let v = self.ensure(target.as_ref());
        self.insert_edge(u, v, key, data);
    }

    fn insert_edge(&mut self, u: usize, v: usize, key: EdgeKey, data: Box<EdgeData>) {
        let mut added = false;
        if let Some(slot) = self.slot_mut(u) {
            let keys = slot.succ.entry(v);
            added = keys.insert(key, data).is_none();
        }
        if let Some(slot) = self.slot_mut(v) {
            slot.pred.insert(u);
        }
        if added {
            self.edge_count += 1;
        }
    }

    #[must_use]
    pub fn has_node(&self, id: &str) -> bool {
        self.index.contains_key(id)
    }

    #[must_use]
    pub fn node(&self, id: &str) -> Option<&NodeData> {
        let index = *self.index.get(id)?;
        self.slot(index).map(|slot| &slot.data)
    }

    pub fn node_mut(&mut self, id: &str) -> Option<&mut NodeData> {
        let index = *self.index.get(id)?;
        self.slot_mut(index).map(|slot| &mut slot.data)
    }

    /// Nodes in insertion order.
    pub fn nodes(&self) -> impl Iterator<Item = (&str, &NodeData)> {
        self.slots
            .iter()
            .flatten()
            .map(|slot| (slot.id.as_str(), &slot.data))
    }

    /// Edges in networkx order: by source node, then target, then key.
    pub fn edges(&self) -> impl Iterator<Item = EdgeRef<'_>> {
        self.slots.iter().flatten().flat_map(move |slot| {
            slot.succ.iter().flat_map(move |(target, keys)| {
                let target = self.slot(target).map_or("", |t| t.id.as_str());
                keys.iter().map(move |(key, data)| EdgeRef {
                    source: slot.id.as_str(),
                    target,
                    key,
                    data,
                })
            })
        })
    }

    /// The parallel edges from `source` to `target`, in key order.
    pub fn edges_between(
        &self,
        source: &str,
        target: &str,
    ) -> impl Iterator<Item = (&EdgeKey, &EdgeData)> {
        let keys = self
            .index
            .get(source)
            .zip(self.index.get(target))
            .and_then(|(u, v)| self.slot(*u).and_then(|slot| slot.succ.get(*v)));
        keys.into_iter().flat_map(KeyDict::iter)
    }

    /// The nodes with an edge into `id`, in first-connection order
    /// (`G.predecessors`).
    pub fn predecessors(&self, id: &str) -> impl Iterator<Item = &str> {
        let pred = self
            .index
            .get(id)
            .and_then(|index| self.slot(*index))
            .map(|slot| &slot.pred);
        pred.into_iter()
            .flat_map(Predecessors::iter)
            .filter_map(|source| self.slot(source).map(|slot| slot.id.as_str()))
    }

    /// Whether some `source → target` edge has `rel_type`.
    #[must_use]
    pub fn has_edge_of_type(&self, source: &str, target: &str, rel_type: &str) -> bool {
        self.edges_between(source, target)
            .any(|(_, data)| data.rel_type == rel_type)
    }

    #[must_use]
    pub fn has_edge(&self, source: &str, target: &str, key: &EdgeKey) -> bool {
        self.edges_between(source, target).any(|(k, _)| k == key)
    }

    pub fn edge_mut(&mut self, source: &str, target: &str, key: &EdgeKey) -> Option<&mut EdgeData> {
        let u = *self.index.get(source)?;
        let v = *self.index.get(target)?;
        self.slot_mut(u)?.succ.get_mut(v)?.get_mut(key)
    }

    /// Remove one edge; when it was the last from `source` to `target`, the
    /// two nodes stop being neighbours (so a later edge between them is
    /// ordered as a new neighbour, as in networkx).
    pub fn remove_edge(&mut self, source: &str, target: &str, key: &EdgeKey) -> Option<EdgeData> {
        let u = *self.index.get(source)?;
        let v = *self.index.get(target)?;
        let slot = self.slot_mut(u)?;
        let keys = slot.succ.get_mut(v)?;
        let removed = keys.shift_remove(key)?;
        if keys.is_empty() {
            slot.succ.shift_remove(v);
            if let Some(target_slot) = self.slot_mut(v) {
                target_slot.pred.shift_remove(u);
            }
        }
        self.edge_count -= 1;
        Some(*removed)
    }

    #[must_use]
    pub fn node_count(&self) -> usize {
        self.index.len()
    }

    #[must_use]
    pub fn edge_count(&self) -> usize {
        self.edge_count
    }

    /// Remove a node and every edge touching it.
    pub fn remove_node(&mut self, id: &str) -> Option<NodeData> {
        let index = self.index.remove(id)?;
        let slot = self.slots.get_mut(index)?.take()?;
        let mut removed_edges = 0;
        for (target, keys) in slot.succ.iter() {
            removed_edges += keys.len();
            if target != index
                && let Some(target_slot) = self.slot_mut(target)
            {
                target_slot.pred.shift_remove(index);
            }
        }
        for source in slot.pred.iter() {
            if source == index {
                continue;
            }
            if let Some(source_slot) = self.slot_mut(source)
                && let Some(keys) = source_slot.succ.shift_remove(index)
            {
                removed_edges += keys.len();
            }
        }
        self.edge_count -= removed_edges;
        Some(slot.data)
    }

    /// Append every node and edge of `other`, keeping its edge keys
    /// (`_merge_graph`: `add_node` merges, `add_edge(u, v, key=key)`).
    pub fn merge(&mut self, other: CodeGraph) {
        let ids: Vec<Option<String>> = other
            .slots
            .iter()
            .map(|slot| slot.as_ref().map(|s| s.id.clone()))
            .collect();
        let mut edges = Vec::new();
        for slot in other.slots.into_iter().flatten() {
            let Slot { id, data, succ, .. } = *slot;
            // `{**existing, **new}`: the typed fields are replaced.
            self.add_node(&id, data);
            edges.push((id, succ));
        }
        for (source, succ) in edges {
            for (target, keys) in succ.0 {
                let Some(Some(target)) = ids.get(target) else {
                    continue;
                };
                for (key, data) in keys {
                    self.add_edge_keyed_boxed(&source, target, key, data);
                }
            }
        }
    }

    /// The `repo_nodes` rows, in node order.
    #[must_use]
    pub fn node_rows(&self) -> Vec<NodeRow> {
        self.nodes().map(|(id, data)| node_row(id, data)).collect()
    }

    /// The `repo_edges` rows, in edge order.
    #[must_use]
    pub fn edge_rows(&self) -> Vec<EdgeRow> {
        self.edges().map(edge_row).collect()
    }
}

/// `_nx_node_to_dict` + `_upsert_nodes_batch`: the `repo_nodes` row of one
/// node. [`CodeGraph::node_rows`] is this over every node; a caller that
/// writes rows one at a time uses it to not hold them all.
#[must_use]
pub fn node_row(id: &str, data: &NodeData) -> NodeRow {
    let symbol_type = data.symbol_type.to_lowercase();
    let is_doc = constants::is_doc_symbol(&symbol_type);
    let is_architectural =
        constants::ARCHITECTURAL_SYMBOLS.contains(&symbol_type.as_str()) || is_doc;
    let symbol = data.symbol.as_deref();
    // `data.get(key, "")`, then the symbol's attribute when that is falsy.
    let fallback = |own: &str, of_symbol: Option<&Option<String>>| -> String {
        if own.is_empty() {
            of_symbol.and_then(Option::clone).unwrap_or_default()
        } else {
            own.to_owned()
        }
    };
    let source_text = if data.source_text.is_empty() {
        // `getattr(symbol, "source_text", "")` has no `or ""`: a `None`
        // there is stored as NULL.
        symbol.map_or(Some(String::new()), |s| s.source_text.clone())
    } else {
        Some(data.source_text.clone())
    };
    // A rich node has no `parameters` attribute and `Symbol` has no
    // `parameters` field either, so the fallback is always `[]`.
    let parameters = if data.parameters.is_empty() && symbol.is_some() {
        "[]".to_owned()
    } else {
        data.parameters.clone()
    };
    NodeRow {
        node_id: id.to_owned(),
        rel_path: data.rel_path.to_string(),
        file_name: data.file_name.to_string(),
        language: data.language.to_string(),
        start_line: data.start_line,
        end_line: data.end_line,
        symbol_name: data.symbol_name.clone(),
        symbol_type,
        parent_symbol: data.parent_symbol.clone(),
        analysis_level: if data.analysis_level.is_empty() {
            "comprehensive".to_owned()
        } else {
            data.analysis_level.to_string()
        },
        source_text,
        docstring: fallback(&data.docstring, symbol.map(|s| &s.docstring)),
        signature: fallback(&data.signature, symbol.map(|s| &s.signature)),
        parameters,
        return_type: fallback(&data.return_type, symbol.map(|s| &s.return_type)),
        is_architectural: u8::from(is_architectural),
        is_doc: u8::from(is_doc),
        is_test: u8::from(constants::is_test_path(&data.rel_path)),
        chunk_type: data.chunk_type.clone(),
        macro_cluster: None,
        micro_cluster: None,
        is_hub: 0,
        hub_assignment: None,
    }
}

/// The `repo_edges` row of one edge (see [`node_row`]).
#[must_use]
pub fn edge_row(edge: EdgeRef<'_>) -> EdgeRow {
    EdgeRow {
        source_id: edge.source.to_owned(),
        target_id: edge.target.to_owned(),
        rel_type: edge.data.rel_type.to_string(),
        edge_class: edge.data.edge_class.to_string(),
        analysis_level: edge.data.analysis_level.to_string(),
        weight: edge.data.weight,
        raw_similarity: edge.data.raw_similarity,
        source_file: edge.data.source_file.to_string(),
        target_file: edge.data.target_file.to_string(),
        language: edge.data.language.clone(),
        annotations: crate::pyjson::dumps(&Value::Object(edge.data.annotations.to_map())),
        created_by: edge.data.created_by.to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::parsers::model::{Range, Scope, SymbolType};

    fn rel(rel_type: &str) -> EdgeData {
        EdgeData {
            rel_type: rel_type.to_owned().into(),
            ..EdgeData::default()
        }
    }

    #[test]
    fn an_edge_to_an_unknown_node_creates_it_empty() {
        let mut graph = CodeGraph::new();
        graph.add_edge("a", "b", EdgeData::default());
        assert_eq!(graph.node_count(), 2);
        let rows = graph.node_rows();
        assert_eq!(rows[1].node_id, "b");
        // Python stores the missing attribute's "" default, not "[]".
        assert_eq!(rows[1].parameters, "");
        assert_eq!(rows[1].source_text.as_deref(), Some(""));
        assert_eq!(rows[1].analysis_level, "comprehensive");
        assert_eq!(graph.edge_rows()[0].annotations, "{}");
    }

    #[test]
    fn re_adding_a_node_keeps_its_position() {
        let mut graph = CodeGraph::new();
        graph.add_node("a", NodeData::default());
        graph.add_node("b", NodeData::default());
        graph.add_node(
            "a",
            NodeData {
                symbol_type: "Class".into(),
                rel_path: "tests/x.py".into(),
                ..NodeData::default()
            },
        );
        let rows = graph.node_rows();
        assert_eq!(rows[0].node_id, "a");
        assert_eq!(
            (
                rows[0].symbol_type.as_str(),
                rows[0].is_architectural,
                rows[0].is_test
            ),
            ("class", 1, 1)
        );
    }

    #[test]
    fn removing_a_node_removes_its_edges() {
        let mut graph = CodeGraph::new();
        graph.add_edge("a", "b", EdgeData::default());
        graph.add_edge("b", "c", EdgeData::default());
        graph.add_edge("b", "b", EdgeData::default());
        graph.remove_node("b");
        assert_eq!((graph.node_count(), graph.edge_count()), (2, 0));
        assert_eq!(graph.edges().count(), 0);
        // A removed node added again goes to the end.
        graph.add_node("b", NodeData::default());
        let order: Vec<&str> = graph.nodes().map(|(id, _)| id).collect();
        assert_eq!(order, ["a", "c", "b"]);
    }

    #[test]
    fn edges_iterate_by_source_then_first_connected_target() {
        let mut graph = CodeGraph::new();
        graph.add_node("x", NodeData::default());
        graph.add_edge("y", "z", rel("1"));
        graph.add_edge("x", "z", rel("2"));
        graph.add_edge("y", "x", rel("3"));
        graph.add_edge("y", "z", rel("4"));
        let order: Vec<&str> = graph.edges().map(|e| &*e.data.rel_type).collect();
        assert_eq!(order, ["2", "1", "4", "3"]);
    }

    #[test]
    fn keys_follow_networkx_new_edge_key() {
        let mut graph = CodeGraph::new();
        assert_eq!(graph.add_edge("a", "b", rel("x")), EdgeKey::Auto(0));
        assert_eq!(graph.add_edge("a", "b", rel("x")), EdgeKey::Auto(1));
        graph.remove_edge("a", "b", &EdgeKey::Auto(0));
        // len(keydict) == 1 is taken, so 2.
        assert_eq!(graph.add_edge("a", "b", rel("x")), EdgeKey::Auto(2));
        // A named key that exists is replaced, not duplicated.
        graph.add_edge_keyed("a", "c", EdgeKey::Named("k".into()), rel("old"));
        graph.add_edge_keyed("a", "c", EdgeKey::Named("k".into()), rel("new"));
        assert_eq!(graph.edge_count(), 3);
        assert!(graph.has_edge("a", "c", &EdgeKey::Named("k".into())));
        // Removing the last a→b edge makes b a NEW neighbour of a next time.
        graph.remove_edge("a", "b", &EdgeKey::Auto(1));
        graph.remove_edge("a", "b", &EdgeKey::Auto(2));
        graph.add_edge("a", "b", rel("again"));
        let order: Vec<&str> = graph.edges().map(|e| &*e.data.rel_type).collect();
        assert_eq!(order, ["new", "again"]);
    }

    #[test]
    fn a_rich_node_reads_missing_columns_from_its_symbol() {
        let mut symbol = Symbol::new(
            "f",
            SymbolType::Function,
            Scope::Global,
            Range::new(1, 0, 2, 0),
            "/r/a.py",
        );
        symbol.docstring = Some("doc".into());
        let mut graph = CodeGraph::new();
        graph.add_node(
            "python::a::f",
            NodeData {
                symbol: Some(Box::new(symbol.into())),
                ..NodeData::default()
            },
        );
        let row = &graph.node_rows()[0];
        assert_eq!(row.source_text, None);
        assert_eq!(row.docstring, "doc");
        assert_eq!(row.parameters, "[]");
        assert_eq!(row.return_type, "");
    }

    #[test]
    fn merge_appends_nodes_and_keeps_keys() {
        let mut left = CodeGraph::new();
        left.add_edge("a", "b", rel("x"));
        let mut right = CodeGraph::new();
        right.add_edge_keyed("c", "a", EdgeKey::Named("sql::defines".into()), rel("y"));
        left.merge(right);
        let order: Vec<&str> = left.nodes().map(|(id, _)| id).collect();
        assert_eq!(order, ["a", "b", "c"]);
        assert!(left.has_edge("c", "a", &EdgeKey::Named("sql::defines".into())));
    }
}
