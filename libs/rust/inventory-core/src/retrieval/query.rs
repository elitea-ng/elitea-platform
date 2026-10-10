//! The graph queries the tools share — `KnowledgeGraph.search` and its
//! scoring, `search_advanced`, `get_relations`, `impact_analysis`,
//! `get_cross_source_relations`, `get_stats`, the type/layer listings,
//! `find_bridging_nodes` — and the entity references of the handlers
//! (`_parse_entity_reference`, `_find_entity_by_reference`), over a
//! [`GraphView`].
//!
//! # Order
//!
//! Python iterated several SETS here, whose order depends on the hash seed:
//! the ids of a name or a type (`_entity_index`, `_type_index`), the types
//! of a layer (`LAYER_TYPE_MAPPING`), the toolkits of an entity, the
//! components and bridging nodes of `find_bridging_nodes`. The port fixes one
//! order for each and the goldens (frozen; generator in
//! `tests/fixtures/PROVENANCE.md`) ran Python with insertion-ordered sets
//! that follow it:
//!
//! * the ids of a name or a type: node order;
//! * the types of a layer: `LAYER_TYPE_MAPPING` source order, then node order
//!   within a type;
//! * a set filled while walking (toolkits, BFS layers, components, bridging
//!   nodes): first-insertion order.
//!
//! # Entity rows carry their id
//!
//! A node read from `graph.json` has no `id` attribute (networkx drops it),
//! so Python's `dict(data)` rows (`get_entities_by_layer`, `search_advanced`,
//! the linear-scan fallback of `get_entities_by_type`) had none, while
//! `get_entity` rows appended it. Every row here is the attributes with `id`
//! appended ([`entity`]) — the shape a live Python graph had.

use crate::graph::{layer_names, layer_of, layer_types};
use crate::retrieval::view::GraphView;
use elitea_engine_core::pyjson::float_repr;
use elitea_engine_core::pystr;
use elitea_engine_core::pyvalue::{py_repr, py_truthy};
use indexmap::{IndexMap, IndexSet};
use serde_json::{Map, Value, json};
use std::collections::{HashMap, VecDeque};

/// One node as the tools answer it: its attributes, `id` appended.
pub type Entity = Map<String, Value>;

/// The entity `id` (`KnowledgeGraph.get_entity`), `None` when absent.
#[must_use]
pub fn entity(view: &GraphView, id: &str) -> Option<Entity> {
    view.node(id).map(|attributes| {
        let mut row = attributes.clone();
        crate::map::shift_remove(&mut row, "id");
        row.insert("id".to_owned(), Value::String(id.to_owned()));
        row
    })
}

/// `{'id': id, **data}`: the row with `id` FIRST (`_expand_entities_by_depth`).
#[must_use]
pub fn entity_id_first(view: &GraphView, id: &str) -> Option<Entity> {
    view.node(id).map(|attributes| {
        let mut row = Map::new();
        row.insert("id".to_owned(), Value::String(id.to_owned()));
        for (key, value) in attributes {
            if key != "id" {
                row.insert(key.clone(), value.clone());
            }
        }
        row
    })
}

/// `data.get(key, '')` for a string attribute; anything else reads as `''`.
#[must_use]
pub fn text<'a>(map: &'a Map<String, Value>, key: &str) -> &'a str {
    map.get(key).and_then(Value::as_str).unwrap_or("")
}

/// Python `str(value)`.
#[must_use]
pub fn py_display(value: &Value) -> String {
    match value {
        Value::String(text) => text.clone(),
        other => py_repr_value(other),
    }
}

/// Python `repr(value)` of a JSON value (`['a', 1]`, `{'k': None}`).
#[must_use]
pub fn py_repr_value(value: &Value) -> String {
    match value {
        Value::Null => "None".to_owned(),
        Value::Bool(true) => "True".to_owned(),
        Value::Bool(false) => "False".to_owned(),
        Value::Number(number) => {
            if number.is_f64() {
                number
                    .as_f64()
                    .map_or_else(|| number.to_string(), float_repr)
            } else {
                number.to_string()
            }
        }
        Value::String(text) => py_repr(text),
        Value::Array(items) => {
            let inner: Vec<String> = items.iter().map(py_repr_value).collect();
            format!("[{}]", inner.join(", "))
        }
        Value::Object(fields) => {
            let inner: Vec<String> = fields
                .iter()
                .map(|(key, value)| format!("{}: {}", py_repr(key), py_repr_value(value)))
                .collect();
            format!("{{{}}}", inner.join(", "))
        }
    }
}

/// `str(map.get(key))`: `None` when absent.
#[must_use]
pub fn get_display(map: &Map<String, Value>, key: &str) -> String {
    map.get(key).map_or_else(|| "None".to_owned(), py_display)
}

/// `str(map.get(key, default))`.
#[must_use]
pub fn get_display_or(map: &Map<String, Value>, key: &str, default: &str) -> String {
    map.get(key).map_or_else(|| default.to_owned(), py_display)
}

/// `items[:n]` with Python's slice bounds (a negative `n` counts from the end).
#[must_use]
pub fn head_len(len: usize, n: i64) -> usize {
    if n >= 0 {
        usize::try_from(n).map_or(len, |n| n.min(len))
    } else {
        let back = usize::try_from(n.unsigned_abs()).unwrap_or(usize::MAX);
        len.saturating_sub(back)
    }
}

/// The citations Python reads: `citations`, or `[citation]` when that list
/// is empty and a legacy `citation` key exists.
#[must_use]
pub fn citations_of(map: &Map<String, Value>) -> Vec<&Value> {
    let listed: Vec<&Value> = match map.get("citations") {
        Some(Value::Array(items)) => items.iter().collect(),
        _ => Vec::new(),
    };
    if listed.is_empty()
        && let Some(legacy) = map.get("citation")
    {
        return vec![legacy];
    }
    listed
}

/// Whether one of the entity's citations is from `source_toolkit`.
#[must_use]
pub fn cited_by(map: &Map<String, Value>, source_toolkit: &str) -> bool {
    citations_of(map).iter().any(|citation| {
        citation.get("source_toolkit").and_then(Value::as_str) == Some(source_toolkit)
    })
}

/// The description the scorer and the text answers read: `description`, or
/// the nested `properties.description` of a model-extracted entity.
#[must_use]
pub fn description_of(map: &Map<String, Value>) -> &str {
    let own = text(map, "description");
    if own.is_empty()
        && let Some(Value::Object(properties)) = map.get("properties")
    {
        return text(properties, "description");
    }
    own
}

/// `TYPE_TO_LAYER.get(kind)` (`kind` already lowercase), or `""`.
#[must_use]
pub fn type_layer(kind: &str) -> &'static str {
    layer_of(kind).unwrap_or("")
}

/// `kind in LAYER_TYPE_MAPPING[layer]`.
fn in_layer(layer: &str, kind: &str) -> bool {
    layer_types(layer).is_some_and(|types| types.contains(&kind))
}

// ---------------------------------------------------------------- search

/// One lexical hit.
#[derive(Debug, Clone, PartialEq)]
pub struct Hit {
    pub id: String,
    pub score: f64,
    pub match_field: &'static str,
}

impl Hit {
    /// `{'entity': …, 'score': …, 'match_field': …}`.
    #[must_use]
    pub fn to_value(&self, view: &GraphView) -> Value {
        json!({
            "entity": entity(view, &self.id).unwrap_or_default(),
            "score": self.score,
            "match_field": self.match_field,
        })
    }
}

/// `KnowledgeGraph._tokenize`: the lowercased text split on every
/// non-`[a-zA-Z0-9]` run, plus each word's letter and digit runs (the
/// camelCase split runs AFTER lowercasing, so it only separates digits).
#[must_use]
pub fn tokenize(text: &str) -> IndexSet<String> {
    let lowered = text.to_lowercase();
    let mut tokens = IndexSet::new();
    for word in lowered.split(|c: char| !c.is_ascii_alphanumeric()) {
        if word.is_empty() {
            continue;
        }
        tokens.insert(word.to_owned());
        let mut run = String::new();
        let mut digits = false;
        for c in word.chars() {
            let is_digit = c.is_ascii_digit();
            if !run.is_empty() && is_digit != digits {
                tokens.insert(std::mem::take(&mut run));
            }
            digits = is_digit;
            run.push(c);
        }
        if !run.is_empty() {
            tokens.insert(run);
        }
    }
    tokens
}

fn overlap(left: &IndexSet<String>, right: &IndexSet<String>) -> usize {
    left.iter().filter(|token| right.contains(*token)).count()
}

#[allow(clippy::cast_precision_loss)]
fn ratio(part: usize, whole: usize) -> f64 {
    part as f64 / whole as f64
}

/// `KnowledgeGraph._calculate_match_score`.
#[must_use]
pub fn match_score(
    query_tokens: &IndexSet<String>,
    query_lower: &str,
    name: &str,
    entity_type: &str,
    description: &str,
    file_path: &str,
) -> (f64, Option<&'static str>) {
    let name_lower = name.to_lowercase();
    let name_tokens = tokenize(name);
    if query_lower == name_lower {
        return (1.0, Some("name_exact"));
    }
    if name_lower.contains(query_lower) {
        let score = if name_lower.starts_with(query_lower) {
            0.85
        } else {
            0.75
        };
        return (score, Some("name_contains"));
    }
    if !query_tokens.is_empty() && !name_tokens.is_empty() {
        let shared = overlap(query_tokens, &name_tokens);
        if shared > 0 {
            let mut score = 0.6 * ratio(shared, query_tokens.len());
            if shared == query_tokens.len() {
                score = 0.7;
            }
            return (score, Some("name_tokens"));
        }
    }
    if !file_path.is_empty() && file_path.to_lowercase().contains(query_lower) {
        return (0.55, Some("file_path"));
    }
    if !description.is_empty() {
        if description.to_lowercase().contains(query_lower) {
            return (0.5, Some("description"));
        }
        let description_tokens = tokenize(description);
        if !query_tokens.is_empty() && !description_tokens.is_empty() {
            let shared = overlap(query_tokens, &description_tokens);
            if shared > 0 {
                return (
                    0.35 * ratio(shared, query_tokens.len()),
                    Some("description_tokens"),
                );
            }
        }
    }
    if entity_type.to_lowercase().contains(query_lower) {
        return (0.3, Some("type"));
    }
    (0.0, None)
}

fn sort_hits(view: &GraphView, hits: &mut [Hit]) {
    let name_of = |hit: &Hit| {
        view.node(&hit.id)
            .map(|node| text(node, "name").to_lowercase())
            .unwrap_or_default()
    };
    // Stable, as Python's list.sort: (-score, name.lower()).
    hits.sort_by(|left, right| {
        right
            .score
            .total_cmp(&left.score)
            .then_with(|| name_of(left).cmp(&name_of(right)))
    });
}

/// Optional filters of [`search`]; an empty string is no filter (Python
/// truthiness).
#[derive(Debug, Clone, Copy, Default)]
pub struct SearchFilters<'a> {
    pub entity_type: Option<&'a str>,
    pub layer: Option<&'a str>,
    pub file_pattern: Option<&'a str>,
}

/// `KnowledgeGraph.search`.
#[must_use]
pub fn search(view: &GraphView, query: &str, top_k: i64, filters: SearchFilters<'_>) -> Vec<Hit> {
    let query_lower = pystr::strip(&query.to_lowercase()).to_owned();
    let query_tokens = tokenize(query);
    let entity_type = filters.entity_type.filter(|t| !t.is_empty());
    let layer = filters
        .layer
        .filter(|l| !l.is_empty())
        .map(str::to_lowercase);
    let file_regex = filters
        .file_pattern
        .filter(|p| !p.is_empty())
        .and_then(|pattern| FileRegex::from_glob(pattern, GlobFlavor::Search));
    let mut hits = Vec::new();
    for (id, data) in view.graph.nodes() {
        let data_type = text(data, "type").to_lowercase();
        if let Some(wanted) = entity_type
            && data_type != wanted.to_lowercase()
        {
            continue;
        }
        if let Some(layer) = &layer
            && text(data, "layer").to_lowercase() != *layer
            && !in_layer(layer, &data_type)
        {
            continue;
        }
        let citations = citations_of(data);
        let file_paths: Vec<&str> = citations
            .iter()
            .filter_map(|citation| citation.as_object())
            .map(|citation| text(citation, "file_path"))
            .collect();
        let primary_file = file_paths
            .first()
            .copied()
            .unwrap_or_else(|| text(data, "file_path"));
        if let Some(regex) = &file_regex
            && !primary_file.is_empty()
            && !regex.search(primary_file)
        {
            continue;
        }
        let (score, field) = match_score(
            &query_tokens,
            &query_lower,
            text(data, "name"),
            &data_type,
            description_of(data),
            primary_file,
        );
        if let (true, Some(field)) = (score > 0.0, field) {
            hits.push(Hit {
                id: id.to_owned(),
                score,
                match_field: field,
            });
        }
    }
    sort_hits(view, &mut hits);
    hits.truncate(head_len(hits.len(), top_k));
    hits
}

/// The filters of [`search_advanced`] (`None` = not given).
#[derive(Debug, Clone, Default)]
pub struct AdvancedFilters {
    pub query: Option<String>,
    pub entity_types: Vec<String>,
    pub layers: Vec<String>,
    pub file_patterns: Vec<String>,
    pub has_relations: Option<bool>,
}

/// `KnowledgeGraph.search_advanced`.
#[must_use]
pub fn search_advanced(view: &GraphView, filters: &AdvancedFilters, top_k: i64) -> Vec<Hit> {
    let mut type_filter: IndexSet<String> = IndexSet::new();
    for kind in &filters.entity_types {
        let lowered = kind.to_lowercase();
        if let Some(types) = layer_types(&lowered) {
            type_filter.extend(types.iter().map(|t| (*t).to_owned()));
        }
        type_filter.insert(lowered);
    }
    let layer_filter: IndexSet<String> = filters.layers.iter().map(|l| l.to_lowercase()).collect();
    let regexes: Vec<FileRegex> = filters
        .file_patterns
        .iter()
        .filter_map(|pattern| FileRegex::from_glob(pattern, GlobFlavor::Advanced))
        .collect();
    let query = filters.query.as_deref().filter(|q| !q.is_empty());
    let query_tokens = query.map(tokenize).unwrap_or_default();
    let query_lower = query
        .map(|q| pystr::strip(&q.to_lowercase()).to_owned())
        .unwrap_or_default();
    let mut hits = Vec::new();
    for (id, data) in view.graph.nodes() {
        let data_type = text(data, "type").to_lowercase();
        let mut data_layer = text(data, "layer").to_lowercase();
        if data_layer.is_empty() {
            type_layer(&data_type).clone_into(&mut data_layer);
        }
        if !type_filter.is_empty() && !type_filter.contains(&data_type) {
            continue;
        }
        if !layer_filter.is_empty() && !layer_filter.contains(&data_layer) {
            continue;
        }
        let file_path = text(data, "file_path");
        if !regexes.is_empty() && !regexes.iter().any(|regex| regex.search(file_path)) {
            continue;
        }
        if let Some(wanted) = filters.has_relations {
            let has_edges =
                view.in_edges(id).next().is_some() || view.out_edges(id).next().is_some();
            if wanted != has_edges {
                continue;
            }
        }
        let (mut score, mut field) = (1.0, "filter");
        if query.is_some() {
            let (found, matched) = match_score(
                &query_tokens,
                &query_lower,
                text(data, "name"),
                &data_type,
                description_of(data),
                file_path,
            );
            match matched {
                Some(matched) if found != 0.0 => (score, field) = (found, matched),
                _ => continue,
            }
        }
        hits.push(Hit {
            id: id.to_owned(),
            score,
            match_field: field,
        });
    }
    sort_hits(view, &mut hits);
    hits.truncate(head_len(hits.len(), top_k));
    hits
}

// ------------------------------------------------------- file patterns

/// Which glob-to-regex translation the Python call site used.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum GlobFlavor {
    /// `search`: `.`→`\.`, `*`→`.*`, `?`→`.`.
    Search,
    /// `search_advanced`: `.`→`\.`, `**`→`.*`, `*`→`[^/]*` (so `**` ends
    /// up `.[^/]*`), and `?` stays a regex quantifier.
    Advanced,
}

#[derive(Debug, Clone)]
enum Atom {
    Literal(char),
    Any,
    Class {
        negated: bool,
        ranges: Vec<(char, char)>,
    },
    Start,
    End,
}

#[derive(Debug, Clone)]
struct Piece {
    atom: Atom,
    min: usize,
    max: usize,
}

/// The `re.compile(translated, re.IGNORECASE).search` Python built from a
/// file glob. Only the regex syntax the translation produces is interpreted
/// (`\.`, `.`, `[...]`, `*`, `+`, `?`, `^`, `$`); any other character a
/// caller's pattern holds (`(`, `|`, `{`) is matched literally where Python
/// would have read regex syntax.
#[derive(Debug, Clone)]
pub struct FileRegex {
    pieces: Vec<Piece>,
}

impl FileRegex {
    /// Translate `glob` as `flavor`'s call site did; `None` where Python's
    /// `re.compile` raised (`re.error`).
    #[must_use]
    pub fn from_glob(glob: &str, flavor: GlobFlavor) -> Option<Self> {
        let escaped = glob.replace('.', "\\.");
        let translated = match flavor {
            GlobFlavor::Search => escaped.replace('*', ".*").replace('?', "."),
            GlobFlavor::Advanced => escaped.replace("**", ".*").replace('*', "[^/]*"),
        };
        Self::parse(&translated)
    }

    fn parse(regex: &str) -> Option<Self> {
        let chars: Vec<char> = regex.chars().collect();
        let mut pieces: Vec<Piece> = Vec::new();
        // Whether the last piece already carries a quantifier.
        let mut quantified = false;
        let mut i = 0;
        while i < chars.len() {
            let c = chars[i];
            match c {
                '*' | '+' | '?' => {
                    let last = pieces.last_mut()?;
                    if quantified {
                        if c == '?' {
                            // A lazy quantifier: the same matches exist.
                            i += 1;
                            continue;
                        }
                        return None; // "multiple repeat"
                    }
                    if matches!(last.atom, Atom::Start | Atom::End) {
                        return None; // "nothing to repeat"
                    }
                    (last.min, last.max) = match c {
                        '*' => (0, usize::MAX),
                        '+' => (1, usize::MAX),
                        _ => (0, 1),
                    };
                    quantified = true;
                    i += 1;
                    continue;
                }
                '\\' => {
                    let escaped = *chars.get(i + 1)?;
                    pieces.push(Piece::one(Atom::Literal(lower(escaped))));
                    i += 2;
                }
                '.' => {
                    pieces.push(Piece::one(Atom::Any));
                    i += 1;
                }
                '^' => {
                    pieces.push(Piece::one(Atom::Start));
                    i += 1;
                }
                '$' => {
                    pieces.push(Piece::one(Atom::End));
                    i += 1;
                }
                '[' => {
                    let (atom, next) = parse_class(&chars, i)?;
                    pieces.push(Piece::one(atom));
                    i = next;
                }
                other => {
                    pieces.push(Piece::one(Atom::Literal(lower(other))));
                    i += 1;
                }
            }
            quantified = false;
        }
        Some(Self { pieces })
    }

    /// `regex.search(text)`, case-insensitively.
    #[must_use]
    pub fn search(&self, text: &str) -> bool {
        let chars: Vec<char> = text.chars().map(lower).collect();
        (0..=chars.len()).any(|start| self.matches_at(0, &chars, start))
    }

    fn matches_at(&self, index: usize, text: &[char], pos: usize) -> bool {
        let Some(piece) = self.pieces.get(index) else {
            return true;
        };
        match piece.atom {
            Atom::Start => return pos == 0 && self.matches_at(index + 1, text, pos),
            Atom::End => return pos == text.len() && self.matches_at(index + 1, text, pos),
            _ => {}
        }
        let mut count = 0;
        while count < piece.max && pos + count < text.len() && piece.atom.accepts(text[pos + count])
        {
            count += 1;
        }
        loop {
            if count >= piece.min && self.matches_at(index + 1, text, pos + count) {
                return true;
            }
            if count == 0 || count <= piece.min {
                return false;
            }
            count -= 1;
        }
    }
}

impl Piece {
    fn one(atom: Atom) -> Self {
        Self {
            atom,
            min: 1,
            max: 1,
        }
    }
}

impl Atom {
    fn accepts(&self, c: char) -> bool {
        match self {
            Self::Literal(expected) => *expected == c,
            Self::Any => c != '\n',
            Self::Class { negated, ranges } => {
                let upper = c.to_uppercase().next().unwrap_or(c);
                let hit = ranges.iter().any(|(low, high)| {
                    (*low..=*high).contains(&c) || (*low..=*high).contains(&upper)
                });
                hit != *negated
            }
            Self::Start | Self::End => false,
        }
    }
}

fn lower(c: char) -> char {
    let mut lowered = c.to_lowercase();
    match (lowered.next(), lowered.next()) {
        (Some(single), None) => single,
        _ => c,
    }
}

/// `[...]` from `chars[start] == '['`: the class and the index after `]`.
fn parse_class(chars: &[char], start: usize) -> Option<(Atom, usize)> {
    let mut i = start + 1;
    let negated = chars.get(i) == Some(&'^');
    if negated {
        i += 1;
    }
    let mut ranges = Vec::new();
    let mut first = true;
    loop {
        let c = *chars.get(i)?;
        if c == ']' && !first {
            return Some((Atom::Class { negated, ranges }, i + 1));
        }
        first = false;
        let low = if c == '\\' {
            i += 1;
            *chars.get(i)?
        } else {
            c
        };
        i += 1;
        if chars.get(i) == Some(&'-') && chars.get(i + 1).is_some_and(|next| *next != ']') {
            let high = *chars.get(i + 1)?;
            if high < low {
                return None; // "bad character range"
            }
            ranges.push((low, high));
            i += 2;
        } else {
            ranges.push((low, low));
        }
    }
}

// ------------------------------------------------------------ relations

/// One edge as `get_relations` reports it.
#[derive(Debug, Clone)]
pub struct Relation<'a> {
    pub source: &'a str,
    pub target: &'a str,
    pub attributes: &'a Map<String, Value>,
}

impl Relation<'_> {
    /// `data.get('relation_type')` (`null` when absent).
    #[must_use]
    pub fn relation_type(&self) -> Value {
        self.attributes
            .get("relation_type")
            .cloned()
            .unwrap_or(Value::Null)
    }

    /// `{source, target, relation_type, properties}`.
    #[must_use]
    pub fn to_value(&self) -> Value {
        json!({
            "source": self.source,
            "target": self.target,
            "relation_type": self.relation_type(),
            "properties": properties_without(self.attributes, &["relation_type"]),
        })
    }
}

/// The edge's attributes without `skip`.
#[must_use]
pub fn properties_without(attributes: &Map<String, Value>, skip: &[&str]) -> Map<String, Value> {
    attributes
        .iter()
        .filter(|(key, _)| !skip.contains(&key.as_str()))
        .map(|(key, value)| (key.clone(), value.clone()))
        .collect()
}

/// `KnowledgeGraph.get_relations`: outgoing (insertion order) then incoming
/// (document order); `direction` other than `outgoing`/`incoming`/`both`
/// selects nothing.
#[must_use]
pub fn relations<'a>(view: &'a GraphView, id: &'a str, direction: &str) -> Vec<Relation<'a>> {
    let mut found = Vec::new();
    if matches!(direction, "outgoing" | "both") {
        found.extend(view.out_edges(id).map(|(target, attributes)| Relation {
            source: id,
            target,
            attributes,
        }));
    }
    if matches!(direction, "incoming" | "both") {
        found.extend(view.in_edges(id).map(|(source, attributes)| Relation {
            source,
            target: id,
            attributes,
        }));
    }
    found
}

/// `{"source", "target", "type", "properties"}` of every edge touching
/// `ids` (the first half of `_get_edges_for_entities`), and the endpoints
/// outside `ids` in first-seen order (its second half).
#[must_use]
pub fn edges_touching(view: &GraphView, ids: &IndexSet<String>) -> (Vec<Value>, IndexSet<String>) {
    let mut edges = Vec::new();
    let mut connected = IndexSet::new();
    for (source, target, attributes) in view.graph.edges() {
        if ids.contains(source) || ids.contains(target) {
            edges.push(edge_row(source, target, attributes));
            if !ids.contains(source) {
                connected.insert(source.to_owned());
            }
            if !ids.contains(target) {
                connected.insert(target.to_owned());
            }
        }
    }
    (edges, connected)
}

/// `{"source", "target", "type", "properties"}` (`_get_all_edges` row).
#[must_use]
pub fn edge_row(source: &str, target: &str, attributes: &Map<String, Value>) -> Value {
    json!({
        "source": source,
        "target": target,
        "type": attributes.get("relation_type").cloned().unwrap_or_else(|| json!("RELATED")),
        "properties": properties_without(attributes, &["relation_type"]),
    })
}

/// One impacted entity.
#[derive(Debug, Clone)]
pub struct Impacted {
    pub id: String,
    pub depth: i64,
    pub path: Vec<String>,
}

/// `KnowledgeGraph.impact_analysis`: breadth first from `id` over incoming
/// edges (`downstream`) or outgoing ones (anything else), to `max_depth`.
#[must_use]
pub fn impact_analysis(
    view: &GraphView,
    id: &str,
    direction: &str,
    max_depth: i64,
) -> Vec<Impacted> {
    if view.node(id).is_none() {
        return Vec::new();
    }
    let downstream = direction == "downstream";
    let mut visited: IndexSet<String> = IndexSet::from([id.to_owned()]);
    let mut impacted = Vec::new();
    let mut queue: VecDeque<(String, Vec<String>, i64)> =
        VecDeque::from([(id.to_owned(), vec![id.to_owned()], 0)]);
    while let Some((current, path, depth)) = queue.pop_front() {
        if depth >= max_depth {
            continue;
        }
        let neighbours: Vec<&str> = if downstream {
            view.in_edges(&current).map(|(source, _)| source).collect()
        } else {
            view.out_edges(&current).map(|(target, _)| target).collect()
        };
        for neighbour in neighbours {
            if visited.insert(neighbour.to_owned()) {
                let mut next = path.clone();
                next.push(neighbour.to_owned());
                impacted.push(Impacted {
                    id: neighbour.to_owned(),
                    depth: depth + 1,
                    path: next.clone(),
                });
                queue.push_back((neighbour.to_owned(), next, depth + 1));
            }
        }
    }
    impacted
}

/// The toolkits of an entity's citations (`citations` only), first-seen.
fn toolkits_of(node: Option<&Map<String, Value>>) -> Option<IndexSet<String>> {
    let citations = match node.and_then(|n| n.get("citations")) {
        Some(Value::Array(items)) if !items.is_empty() => items,
        _ => return None,
    };
    Some(
        citations
            .iter()
            .filter_map(|citation| citation.get("source_toolkit"))
            .filter(|toolkit| py_truthy(toolkit))
            .map(py_display)
            .collect(),
    )
}

/// `KnowledgeGraph.get_cross_source_relations`, in edge order; each side's
/// toolkits in first-seen citation order.
#[must_use]
pub fn cross_source_relations(view: &GraphView) -> Vec<Value> {
    let mut found = Vec::new();
    for (source, target, attributes) in view.graph.edges() {
        let (Some(from), Some(to)) = (
            toolkits_of(view.node(source)),
            toolkits_of(view.node(target)),
        ) else {
            continue;
        };
        let same = from.len() == to.len() && from.iter().all(|toolkit| to.contains(toolkit));
        if from.is_empty() || to.is_empty() || same {
            continue;
        }
        found.push(json!({
            "source": source,
            "target": target,
            "source_toolkits": from.into_iter().collect::<Vec<_>>(),
            "target_toolkits": to.into_iter().collect::<Vec<_>>(),
            "relation_type": attributes.get("relation_type").cloned().unwrap_or(Value::Null),
            "relation_source": attributes.get("source_toolkit").cloned().unwrap_or(Value::Null),
            "properties": properties_without(attributes, &["relation_type", "source_toolkit"]),
        }));
    }
    found
}

fn bump(counts: &mut IndexMap<String, i64>, key: String) {
    *counts.entry(key).or_insert(0) += 1;
}

fn counts_value(counts: IndexMap<String, i64>) -> Value {
    Value::Object(
        counts
            .into_iter()
            .map(|(key, count)| (key, json!(count)))
            .collect(),
    )
}

/// `KnowledgeGraph.get_stats`, keys in its order.
#[must_use]
pub fn stats(view: &GraphView) -> Map<String, Value> {
    let mut entity_types = IndexMap::new();
    let mut relation_types = IndexMap::new();
    let mut sources: Vec<String> = Vec::new();
    let mut relations_by_source = IndexMap::new();
    let add_source = |value: Option<&Value>, sources: &mut Vec<String>| {
        if let Some(value) = value.filter(|v| py_truthy(v)) {
            let name = py_display(value);
            if !sources.contains(&name) {
                sources.push(name);
            }
        }
    };
    for (_, data) in view.graph.nodes() {
        if let Some(kind) = data.get("type") {
            bump(&mut entity_types, py_display(kind));
        }
        add_source(data.get("source_toolkit"), &mut sources);
        if let Some(Value::Array(citations)) = data.get("citations") {
            for citation in citations {
                add_source(citation.get("source_toolkit"), &mut sources);
            }
        }
        if let Some(citation @ Value::Object(_)) = data.get("citation") {
            add_source(citation.get("source_toolkit"), &mut sources);
        }
    }
    for (_, _, data) in view.graph.edges() {
        if let Some(kind) = data.get("relation_type") {
            bump(&mut relation_types, py_display(kind));
        }
        // An edge two sources found counts for each, as the source status
        // and `Graph::source_counts` count it.
        let contributors = crate::graph::relation_sources(data);
        if contributors.len() > 1 {
            for source in contributors.into_iter().filter(|s| !s.is_empty()) {
                bump(&mut relations_by_source, source);
            }
        } else if let Some(source) = data.get("source_toolkit").filter(|v| py_truthy(v)) {
            bump(&mut relations_by_source, py_display(source));
        }
    }
    sources.sort();
    let mut edge_types: Vec<String> = relation_types.keys().cloned().collect();
    edge_types.sort();
    let metadata = &view.graph.metadata;
    let embedded = view
        .graph
        .nodes()
        .filter(|(_, data)| data.get("embedding").is_some_and(py_truthy))
        .count();
    let community_data = metadata.get("community_data");
    let mut stats = Map::new();
    stats.insert("node_count".to_owned(), json!(view.graph.node_count()));
    stats.insert("edge_count".to_owned(), json!(view.graph.edge_count()));
    stats.insert("entity_types".to_owned(), counts_value(entity_types));
    stats.insert("relation_types".to_owned(), counts_value(relation_types));
    stats.insert("edge_types".to_owned(), json!(edge_types));
    stats.insert("source_toolkits".to_owned(), json!(sources));
    stats.insert(
        "relations_by_source".to_owned(),
        counts_value(relations_by_source),
    );
    stats.insert(
        "cross_source_relations".to_owned(),
        json!(cross_source_relations(view).len()),
    );
    stats.insert(
        "last_saved".to_owned(),
        metadata.get("last_saved").cloned().unwrap_or(Value::Null),
    );
    stats.insert("has_embeddings".to_owned(), json!(embedded > 0));
    stats.insert("embeddings_count".to_owned(), json!(embedded));
    stats.insert(
        "embeddings_model".to_owned(),
        metadata
            .get("embeddings_model")
            .cloned()
            .unwrap_or(Value::Null),
    );
    stats.insert(
        "has_communities".to_owned(),
        json!(community_data.is_some_and(py_truthy)),
    );
    stats.insert(
        "num_communities".to_owned(),
        community_data
            .and_then(|data| data.get("num_communities"))
            .cloned()
            .unwrap_or_else(|| json!(0)),
    );
    stats
}

// ------------------------------------------------------------- listings

/// The ids of the entities named `name` (any case), in node order
/// (`find_all_entities_by_name`).
#[must_use]
pub fn ids_named<'a>(view: &'a GraphView, name: &str) -> Vec<&'a str> {
    view.ids_named(name)
        .iter()
        .filter(|id| view.node(id).is_some())
        .map(String::as_str)
        .collect()
}

/// `KnowledgeGraph.get_entities_by_type`: a layer name lists the layer's
/// types (source order, node order within one); a type lists its entities;
/// a type no entity has falls back to a scan on the lowercased `type`.
/// `limit` is applied when truthy, with Python's slice bounds.
#[must_use]
pub fn ids_by_type(view: &GraphView, entity_type: &str, limit: i64) -> Vec<String> {
    let lowered = entity_type.to_lowercase();
    let mut ids: Vec<String> = if let Some(types) = layer_types(&lowered) {
        types
            .iter()
            .flat_map(|kind| view.ids_of_type(kind).iter().cloned())
            .collect()
    } else {
        let indexed = view.ids_of_type(&lowered);
        if indexed.is_empty() {
            view.graph
                .nodes()
                .filter(|(_, data)| text(data, "type").to_lowercase() == lowered)
                .map(|(id, _)| id.to_owned())
                .collect()
        } else {
            indexed.to_vec()
        }
    };
    if limit != 0 {
        ids.truncate(head_len(ids.len(), limit));
    }
    ids
}

/// `KnowledgeGraph.get_entities_by_layer`: node order, by the `layer`
/// attribute or a type of the layer.
#[must_use]
pub fn ids_by_layer(view: &GraphView, layer: &str, limit: i64) -> Vec<String> {
    let lowered = layer.to_lowercase();
    let mut ids: Vec<String> = view
        .graph
        .nodes()
        .filter(|(_, data)| {
            text(data, "layer").to_lowercase() == lowered
                || in_layer(&lowered, &text(data, "type").to_lowercase())
        })
        .map(|(id, _)| id.to_owned())
        .collect();
    if limit != 0 {
        ids.truncate(head_len(ids.len(), limit));
    }
    ids
}

/// The layer names, joined as the "Available layers" hint lists them.
#[must_use]
pub fn available_layers() -> String {
    layer_names().collect::<Vec<_>>().join(", ")
}

// ------------------------------------------------------------ references

/// `_parse_entity_reference`: `Name`, `Name (type)`, and either followed by
/// ` @ source - path`.
#[must_use]
pub fn parse_entity_reference(reference: &str) -> (String, Option<String>) {
    let mut reference = pystr::strip(reference);
    if let Some((head, _)) = reference.split_once(" @ ") {
        reference = pystr::strip(head);
    }
    if reference.ends_with(')')
        && let Some(open) = reference.rfind('(')
        && open > 0
    {
        let name = pystr::strip(&reference[..open]);
        let kind = pystr::strip(&reference[open + 1..reference.len() - 1]);
        if !name.is_empty() && !kind.is_empty() {
            return (name.to_owned(), Some(kind.to_owned()));
        }
    }
    (reference.to_owned(), None)
}

/// An entity a reference resolved to.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Resolved {
    pub id: String,
    /// Resolved as a node id (deviation D4): no entity has that name, but a
    /// node has that id. The text answers then describe it directly.
    pub by_id: bool,
}

/// `_find_entity_by_reference`, with the id fallback (D4): where Python
/// found no entity of the parsed name, a reference that is a node id
/// resolves to that node. `Err` carries Python's message.
///
/// # Errors
///
/// The reference names nothing, names several entities without a type, or
/// names none of the given type.
pub fn find_entity_by_reference(view: &GraphView, reference: &str) -> Result<Resolved, String> {
    let (name, kind) = parse_entity_reference(reference);
    let matches = ids_named(view, &name);
    let by_id = || {
        let candidate = pystr::strip(reference);
        view.node(candidate).map(|_| Resolved {
            id: candidate.to_owned(),
            by_id: true,
        })
    };
    let resolved = |id: &str| Resolved {
        id: id.to_owned(),
        by_id: false,
    };
    if let Some(kind) = kind {
        let wanted = kind.to_lowercase();
        if let Some(id) = matches.iter().find(|id| {
            view.node(id)
                .is_some_and(|node| text(node, "type").to_lowercase() == wanted)
        }) {
            return Ok(resolved(id));
        }
        if matches.is_empty() {
            return by_id().ok_or_else(|| {
                format!("Entity '{name}' not found. Try search_knowledge_graph to find the correct name.")
            });
        }
        let available: Vec<String> = matches
            .iter()
            .take(10)
            .filter_map(|id| entity(view, id))
            .map(|e| {
                format!(
                    "  - {} ({})",
                    get_display(&e, "name"),
                    get_display_or(&e, "type", "unknown")
                )
            })
            .collect();
        return Err(format!(
            "Entity '{name}' not found with type '{kind}'.\n\nAvailable entities with this name:\n{}",
            available.join("\n")
        ));
    }
    match matches.as_slice() {
        [] => by_id().ok_or_else(|| {
            format!("Entity '{reference}' not found. Try search_knowledge_graph to find the correct name.")
        }),
        [only] => Ok(resolved(only)),
        several => {
            let options: Vec<String> = several
                .iter()
                .take(10)
                .filter_map(|id| entity(view, id))
                .map(|e| {
                    format!(
                        "  - {} ({}) @ {} - {}",
                        get_display(&e, "name"),
                        get_display_or(&e, "type", "unknown"),
                        get_display_or(&e, "source_toolkit", "?"),
                        get_display_or(&e, "file_path", "?"),
                    )
                })
                .collect();
            Err(format!(
                "Multiple entities named '{name}' found. Please specify the type:\n\n{}\n\nUse format: \"Name (type)\" or copy full reference from above.",
                options.join("\n")
            ))
        }
    }
}

/// How the retrieval wrapper's text methods find an entity again from the
/// raw name: the first entity of that name, else the best lexical hit
/// (`find_entity_by_name` then `search(name, top_k=1)`).
#[must_use]
pub fn wrapper_lookup(view: &GraphView, name: &str) -> Option<String> {
    if let Some(id) = ids_named(view, name).first() {
        return Some((*id).to_owned());
    }
    search(view, name, 1, SearchFilters::default())
        .into_iter()
        .next()
        .map(|hit| hit.id)
}

// -------------------------------------------------------------- bridging

/// The undirected adjacency `DiGraph.to_undirected()` builds: each node's
/// neighbours in the order its edges first appear in edge order.
#[must_use]
pub fn undirected(view: &GraphView) -> HashMap<&str, IndexSet<&str>> {
    let mut adjacency: HashMap<&str, IndexSet<&str>> = HashMap::new();
    for (source, target, _) in view.graph.edges() {
        adjacency.entry(source).or_default().insert(target);
        adjacency.entry(target).or_default().insert(source);
    }
    adjacency
}

/// `nx.shortest_path` on an unweighted undirected graph: networkx's
/// bidirectional BFS (`_bidirectional_pred_succ`), so a tie resolves to the
/// same path as Python's.
#[must_use]
#[allow(clippy::implicit_hasher)]
pub fn shortest_path<'a>(
    adjacency: &HashMap<&'a str, IndexSet<&'a str>>,
    source: &'a str,
    target: &'a str,
) -> Option<Vec<&'a str>> {
    if source == target {
        return Some(vec![source]);
    }
    let empty = IndexSet::new();
    let neighbours = |node: &str| adjacency.get(node).unwrap_or(&empty);
    let mut pred: HashMap<&str, Option<&str>> = HashMap::from([(source, None)]);
    let mut succ: HashMap<&str, Option<&str>> = HashMap::from([(target, None)]);
    let mut forward = vec![source];
    let mut reverse = vec![target];
    let meeting = 'search: loop {
        if forward.is_empty() || reverse.is_empty() {
            return None;
        }
        if forward.len() <= reverse.len() {
            let level = std::mem::take(&mut forward);
            for v in level {
                for &w in neighbours(v) {
                    if !pred.contains_key(w) {
                        forward.push(w);
                        pred.insert(w, Some(v));
                    }
                    if succ.contains_key(w) {
                        break 'search w;
                    }
                }
            }
        } else {
            let level = std::mem::take(&mut reverse);
            for v in level {
                for &w in neighbours(v) {
                    if !succ.contains_key(w) {
                        succ.insert(w, Some(v));
                        reverse.push(w);
                    }
                    if pred.contains_key(w) {
                        break 'search w;
                    }
                }
            }
        }
    };
    let mut path = Vec::new();
    let mut walk = Some(meeting);
    while let Some(node) = walk {
        path.push(node);
        walk = pred.get(node).copied().flatten();
    }
    path.reverse();
    let mut walk = succ.get(meeting).copied().flatten();
    while let Some(node) = walk {
        path.push(node);
        walk = succ.get(node).copied().flatten();
    }
    Some(path)
}

/// What `find_bridging_nodes` returns.
#[derive(Debug, Clone, Default)]
pub struct Bridging {
    pub nodes: Vec<String>,
    /// `{"source", "target", "type"}` rows.
    pub edges: Vec<Value>,
    pub clusters: usize,
}

/// `KnowledgeGraph.find_bridging_nodes`: connect the clusters of `ids` by
/// the shortest undirected paths of at most `max_length` nodes.
#[must_use]
pub fn find_bridging_nodes(
    view: &GraphView,
    ids: &[String],
    max_length: i64,
    max_bridges: usize,
) -> Bridging {
    let valid: Vec<&str> = ids
        .iter()
        .map(String::as_str)
        .filter(|id| view.node(id).is_some())
        .collect();
    if valid.len() < 2 {
        return Bridging {
            clusters: valid.len(),
            ..Bridging::default()
        };
    }
    let adjacency = undirected(view);
    let targets: IndexSet<&str> = valid.iter().copied().collect();
    let mut components: Vec<IndexSet<&str>> = Vec::new();
    let mut visited: IndexSet<&str> = IndexSet::new();
    for &start in &valid {
        if visited.contains(start) {
            continue;
        }
        let mut component = IndexSet::new();
        let mut queue = VecDeque::from([start]);
        while let Some(current) = queue.pop_front() {
            if !visited.insert(current) {
                continue;
            }
            if targets.contains(current) {
                component.insert(current);
                for &neighbour in adjacency.get(current).into_iter().flatten() {
                    if !visited.contains(neighbour) {
                        queue.push_back(neighbour);
                    }
                }
            }
        }
        if !component.is_empty() {
            components.push(component);
        }
    }
    if components.len() <= 1 {
        return Bridging {
            clusters: 1,
            ..Bridging::default()
        };
    }
    components.sort_by_key(IndexSet::len);
    let mut bridging_nodes: IndexSet<String> = IndexSet::new();
    let mut bridging_edges = Vec::new();
    let mut bridges = 0;
    for (i, first) in components.iter().enumerate() {
        if bridges >= max_bridges {
            break;
        }
        let mut best: Option<Vec<&str>> = None;
        for second in &components[i + 1..] {
            for &n1 in first {
                if bridges >= max_bridges {
                    break;
                }
                for &n2 in second {
                    let Some(path) = shortest_path(&adjacency, n1, n2) else {
                        continue;
                    };
                    let length = i64::try_from(path.len()).unwrap_or(i64::MAX);
                    if length <= max_length && best.as_ref().is_none_or(|b| path.len() < b.len()) {
                        best = Some(path);
                    }
                }
            }
        }
        let Some(path) = best.filter(|path| path.len() > 2) else {
            continue;
        };
        bridges += 1;
        for node in &path[1..path.len() - 1] {
            bridging_nodes.insert((*node).to_owned());
        }
        for pair in path.windows(2) {
            let (source, target) = (pair[0], pair[1]);
            let row = |s: &str, t: &str, attributes: &Map<String, Value>| {
                json!({
                    "source": s,
                    "target": t,
                    "type": attributes.get("relation_type").cloned().unwrap_or_else(|| json!("RELATED")),
                })
            };
            if let Some(attributes) = view.graph.edge(source, target) {
                bridging_edges.push(row(source, target, attributes));
            } else if let Some(attributes) = view.graph.edge(target, source) {
                bridging_edges.push(row(target, source, attributes));
            }
        }
    }
    Bridging {
        nodes: bridging_nodes.into_iter().collect(),
        edges: bridging_edges,
        clusters: components.len(),
    }
}

/// The neighbours (predecessors, then successors) of `id` not yet in `seen`,
/// in first-seen order — one BFS step of `_expand_entities_by_depth` and
/// `get_entity_neighbors`.
pub fn new_neighbours<'a>(
    view: &'a GraphView,
    id: &'a str,
    seen: &IndexSet<String>,
    into: &mut IndexSet<String>,
) {
    for neighbour in view
        .in_edges(id)
        .map(|(source, _)| source)
        .chain(view.out_edges(id).map(|(target, _)| target))
    {
        if !seen.contains(neighbour) {
            into.insert(neighbour.to_owned());
        }
    }
}

/// Remove raw `embedding` vectors from an answered row (deviation D5).
pub fn strip_embedding(row: &mut Map<String, Value>) {
    crate::map::shift_remove(row, "embedding");
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::graph::Graph;

    fn view(document: &Value) -> GraphView {
        GraphView::new(Graph::from_node_link(document).unwrap_or_default(), 1)
    }

    #[test]
    fn a_relation_two_sources_found_counts_for_each() {
        let view = view(&json!({
            "nodes": [{"id": "a"}, {"id": "b"}],
            "links": [
                {"source": "a", "target": "b", "relation_type": "calls",
                 "source_toolkit": "mirror", "discovered_in_file": "a.py",
                 "provenance": [
                     {"source_toolkit": "repo", "discovered_in_file": "a.py"},
                     {"source_toolkit": "mirror", "discovered_in_file": "a.py"},
                 ]},
                {"source": "b", "target": "a", "relation_type": "uses",
                 "source_toolkit": "mirror"},
            ],
        }));
        let stats = stats(&view);
        assert_eq!(stats["edge_count"], json!(2));
        assert_eq!(
            stats["relations_by_source"],
            json!({"mirror": 2, "repo": 1})
        );
        for source in ["repo", "mirror"] {
            assert_eq!(
                stats["relations_by_source"][source],
                json!(view.graph.source_counts(source).1)
            );
        }
    }

    #[test]
    fn tokens_split_on_non_alphanumerics_and_digit_runs_only() {
        let tokens: Vec<String> = tokenize("ChatMessage handler_v2").into_iter().collect();
        assert_eq!(tokens, ["chatmessage", "handler", "v2", "v", "2"]);
    }

    #[test]
    fn scores_follow_the_python_ladder() {
        let q = tokenize("user");
        assert_eq!(
            match_score(&q, "user", "User", "class", "", ""),
            (1.0, Some("name_exact"))
        );
        assert!((match_score(&q, "user", "UserService", "class", "", "").0 - 0.85).abs() < 1e-12);
        assert!((match_score(&q, "user", "get_user", "method", "", "").0 - 0.75).abs() < 1e-12);
        let q = tokenize("chat message");
        assert!(
            (match_score(&q, "chat message", "chat_handler", "c", "", "").0 - 0.3).abs() < 1e-12
        );
        assert_eq!(
            match_score(&q, "zz", "a", "class", "", "src/zz.py"),
            (0.55, Some("file_path"))
        );
    }

    #[test]
    fn globs_translate_as_each_call_site_did() {
        let search = |glob: &str, path: &str| {
            FileRegex::from_glob(glob, GlobFlavor::Search).is_some_and(|r| r.search(path))
        };
        assert!(search("*.py", "src/a.py"));
        assert!(search("src/*/x.py", "src/a/b/x.py"));
        assert!(!search("*.md", "src/a.py"));
        assert!(search("SRC/?.py", "src/a.py"));
        let advanced = |glob: &str, path: &str| {
            FileRegex::from_glob(glob, GlobFlavor::Advanced).is_some_and(|r| r.search(path))
        };
        assert!(advanced("src/data/*.py", "src/data/models.py"));
        // `*` stops at a slash (re.search agrees: no match).
        assert!(!advanced("src/*.py", "src/data/models.py"));
        assert!(advanced("data/*.py", "src/data/models.py"));
        assert!(!advanced("^data/*.py", "src/data/models.py"));
        // `**` became `.[^/]*`: one character, then no slash.
        assert!(advanced("src/**.py", "src/models.py"));
        assert!(!advanced("src/**.py", "src/x/a.py"));
        // `?` stayed a quantifier: `colou?r`.
        assert!(advanced("colou?r", "color.py"));
        assert!(FileRegex::from_glob("?x", GlobFlavor::Advanced).is_none());
        assert!(FileRegex::from_glob("[abc", GlobFlavor::Search).is_none());
    }

    #[test]
    fn references_parse_as_the_handlers_did() {
        assert_eq!(parse_entity_reference(" Foo "), ("Foo".to_owned(), None));
        assert_eq!(
            parse_entity_reference("Foo (class) @ repo - a.py"),
            ("Foo".to_owned(), Some("class".to_owned()))
        );
        assert_eq!(parse_entity_reference("(x)"), ("(x)".to_owned(), None));
        assert_eq!(
            parse_entity_reference("Foo ()"),
            ("Foo ()".to_owned(), None)
        );
    }

    #[test]
    fn shortest_paths_resolve_ties_as_networkx() {
        // networkx's own docstring example.
        let mut links = Vec::new();
        for (a, b) in [
            ("0", "1"),
            ("1", "2"),
            ("2", "3"),
            ("3", "0"),
            ("0", "4"),
            ("4", "5"),
            ("5", "6"),
            ("6", "7"),
            ("7", "4"),
        ] {
            links.push(json!({"source": a, "target": b}));
        }
        let nodes: Vec<Value> = (0..8).map(|n| json!({"id": n.to_string()})).collect();
        let view = view(&json!({"nodes": nodes, "links": links}));
        let adjacency = undirected(&view);
        assert_eq!(
            shortest_path(&adjacency, "2", "6"),
            Some(vec!["2", "1", "0", "4", "5", "6"])
        );
        assert_eq!(shortest_path(&adjacency, "2", "2"), Some(vec!["2"]));
    }

    #[test]
    fn python_reprs() {
        assert_eq!(
            py_repr_value(&json!(["a", 1, 1.0, null, true])),
            "['a', 1, 1.0, None, True]"
        );
        assert_eq!(py_display(&json!(1e-7)), "1e-07");
        assert_eq!(head_len(5, -2), 3);
        assert_eq!(head_len(5, 9), 5);
    }
}
