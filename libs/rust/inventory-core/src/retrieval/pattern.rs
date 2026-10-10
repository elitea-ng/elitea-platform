//! `query_pattern` and `get_pattern_vocabulary`: the Cypher-like pattern
//! queries of `KnowledgeGraph` (`knowledge_graph.py`, the "Pattern Query
//! Engine"), answered as the chat agent's tools answered them.
//!
//! Neither tool is in a family table (`crate::tools`): the Python chat agent
//! built them as closures for `inventory_chat` / `investigate`, so this
//! module serves no routed tool ([`handle`] is `None`) and exposes the pure
//! functions `investigate` calls: [`query_pattern`] (the tool's text),
//! [`find_paths`] (the paths), [`pattern_vocabulary`].
//!
//! The grammar, synonyms, caps and messages are Python's (the synonyms, the
//! syntax help and the caps come from its source, `assets/python_retrieval.json`).
//!
//! One deliberate difference: ORDER. Python resolved a node specifier and
//! the dual-wildcard seeds to a `set`, and the breadth-first walk started
//! from the set's members in iteration order — which follows the
//! per-process string hash, so the same query listed its paths in a
//! different order from one process to the next, and at the result cap kept
//! different paths. Here those sets are walked in node order (the seeds
//! are still chosen as Python chose them: the first 500 distinct ones in
//! edge order). Within one start node the walk is Python's (outgoing edges
//! in insertion order, incoming in document order). Every Python run
//! produced some permutation of the start sets; this is the one in node
//! order, and `tests/retrieval_more.rs` holds the engine to the Python
//! code run with that order.

// The answers are built line by line, as the Python handlers built them.
#![allow(clippy::format_push_string)]

use super::admin_tools::tables;
use super::view::GraphView;
use super::{Call, Handled};
use crate::graph::layer_types;
use elitea_engine_core::pystr::{is_space, strip};
use elitea_engine_core::pyvalue::py_str;
use serde_json::{Map, Value};
use std::collections::{HashMap, HashSet, VecDeque};

/// How many results the chat tool asks for (`max_results=50`).
pub const CHAT_MAX_RESULTS: usize = 50;

/// Start nodes a dual-wildcard pattern collects at most (`_seed_from_edges`).
const MAX_SEEDS: usize = 500;

/// Endpoints of one chain segment fed to the next at most
/// (`_execute_chain`'s `MAX_ENDPOINT_CAP`).
const MAX_ENDPOINTS: usize = 50;

/// See [`super::dispatch`]: no routed tool is a pattern tool.
#[must_use]
pub fn handle(_call: &Call<'_>) -> Handled {
    None
}

/// A node specifier: `(?)`, `(?:class)`, `(Name)`, `(Name:type)`.
#[derive(Debug, Clone, PartialEq, Eq)]
struct NodeSpec {
    name: Option<String>,
    types: Option<Vec<String>>,
}

/// One parsed segment of a pattern.
#[derive(Debug, Clone, PartialEq, Eq)]
struct Segment {
    source: NodeSpec,
    target: NodeSpec,
    rel_types: Option<Vec<String>>,
    min_hops: i128,
    max_hops: i128,
    forward: bool,
}

/// One matched path: node ids and the relation type of each hop.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PatternPath {
    /// The node ids, in pattern order.
    pub path: Vec<String>,
    /// The lowercased relation type of each hop.
    pub edges: Vec<String>,
}

/// Python's `str.lower()` of a split item, stripped; empty items dropped.
fn split_types(text: &str) -> Vec<String> {
    text.split(',')
        .map(strip)
        .filter(|t| !t.is_empty())
        .map(str::to_lowercase)
        .collect()
}

/// `_parse_node_spec`.
fn parse_node_spec(spec: &str) -> NodeSpec {
    let spec = strip(spec);
    if spec.is_empty() || spec == "?" {
        return NodeSpec {
            name: None,
            types: None,
        };
    }
    if let Some(types) = spec.strip_prefix("?:") {
        let types = split_types(types);
        return NodeSpec {
            name: None,
            types: (!types.is_empty()).then_some(types),
        };
    }
    if let Some((name, types)) = spec.split_once(':') {
        let types = split_types(types);
        let name = strip(name);
        return NodeSpec {
            name: (!name.is_empty()).then(|| name.to_owned()),
            types: (!types.is_empty()).then_some(types),
        };
    }
    NodeSpec {
        name: Some(spec.to_owned()),
        types: None,
    }
}

/// Python's `int(text)` for the decimal strings a hop count is written as:
/// surrounding whitespace, an optional sign, ASCII digits with single
/// underscores between them. `None` where Python raised `ValueError` (and,
/// unlike Python, for a number beyond `i128`).
fn py_int(text: &str) -> Option<i128> {
    let text = strip(text);
    let (negative, digits) = match text.as_bytes().first() {
        Some(b'-') => (true, &text[1..]),
        Some(b'+') => (false, &text[1..]),
        _ => (false, text),
    };
    if digits.is_empty()
        || digits.starts_with('_')
        || digits.ends_with('_')
        || digits.contains("__")
        || !digits.chars().all(|c| c.is_ascii_digit() || c == '_')
    {
        return None;
    }
    let value: i128 = digits.replace('_', "").parse().ok()?;
    Some(if negative { -value } else { value })
}

/// The text after a regex match, as one segment of `segment_re` matches it
/// at the start of `text`: `(source) arrow-and-relation (target)`.
struct SegmentMatch<'a> {
    source: &'a str,
    arrow: &'a str,
    relation: &'a str,
    target: &'a str,
    end: usize,
}

/// `segment_re.match(text)`:
/// `\(([^)]*)\)\s*(<?\-\[:([^\]]*)\]\-?>?)\s*\(([^)]*)\)`.
fn match_segment(text: &str) -> Option<SegmentMatch<'_>> {
    let skip_space = |at: usize| {
        text[at..]
            .char_indices()
            .find(|(_, c)| !is_space(*c))
            .map_or(text.len(), |(offset, _)| at + offset)
    };
    let rest = text.strip_prefix('(')?;
    let close = rest.find(')')?;
    let source = &rest[..close];
    let arrow_start = skip_space(1 + close + 1);
    let mut at = arrow_start;
    if text[at..].starts_with('<') {
        at += 1;
    }
    at += usize::from(text[at..].starts_with("-[:")) * 3;
    if !text[..at].ends_with("-[:") {
        return None;
    }
    let relation_start = at;
    let relation_len = text[at..].find(']')?;
    let relation = &text[relation_start..relation_start + relation_len];
    at = relation_start + relation_len + 1;
    if text[at..].starts_with('-') {
        at += 1;
    }
    if text[at..].starts_with('>') {
        at += 1;
    }
    let arrow = &text[arrow_start..at];
    at = skip_space(at);
    let rest = text[at..].strip_prefix('(')?;
    let close = rest.find(')')?;
    let target = &rest[..close];
    Some(SegmentMatch {
        source,
        arrow,
        relation,
        target,
        end: at + 1 + close + 1,
    })
}

fn syntax_help() -> &'static str {
    &tables().pattern_syntax_help
}

fn invalid_hops(hops: &str, relation: &str) -> String {
    format!(
        "Invalid hop specification '{hops}' in relation segment '{relation}'.\n{}",
        syntax_help()
    )
}

/// `_parse_single_segment`.
fn parse_segment(found: &SegmentMatch<'_>) -> Result<Segment, String> {
    let arrow = found.arrow;
    let forward = if arrow.starts_with("<-") && arrow.ends_with('-') {
        false
    } else if arrow.ends_with("->") {
        true
    } else {
        return Err(format!(
            "Ambiguous arrow in pattern: {arrow}\nUse -> for forward or <- for backward.\n{}",
            syntax_help()
        ));
    };
    let relation = found.relation;
    let mut rel_types = None;
    let (mut min_hops, mut max_hops) = (1, 1);
    if let Some((types, hops)) = relation.split_once('*') {
        let (types, hops) = (strip(types), strip(hops));
        if !types.is_empty() {
            rel_types = Some(split_types(types));
        }
        if let Some((low, high)) = hops.split_once("..") {
            min_hops = py_int(low).ok_or_else(|| invalid_hops(hops, relation))?;
            max_hops = py_int(high).ok_or_else(|| invalid_hops(hops, relation))?;
        } else {
            min_hops = py_int(hops).ok_or_else(|| invalid_hops(hops, relation))?;
            max_hops = min_hops;
        }
    } else if !strip(relation).is_empty() {
        rel_types = Some(split_types(relation));
    }
    let synonyms = &tables().relation_synonyms;
    let rel_types = rel_types.map(|types| {
        types
            .into_iter()
            .map(|t| synonyms.get(&t).cloned().unwrap_or(t))
            .collect::<Vec<_>>()
    });
    let max_allowed = i128::from(tables().max_pattern_hops);
    if min_hops < 1 {
        return Err(format!("Minimum hops must be >= 1, got {min_hops}"));
    }
    if max_hops > max_allowed {
        return Err(format!(
            "Maximum hops must be <= {max_allowed}, got {max_hops}"
        ));
    }
    if min_hops > max_hops {
        return Err(format!("min_hops ({min_hops}) > max_hops ({max_hops})"));
    }
    Ok(Segment {
        source: parse_node_spec(found.source),
        target: parse_node_spec(found.target),
        rel_types,
        min_hops,
        max_hops,
        forward,
    })
}

/// `_parse_pattern`: the segments, or Python's `ValueError` message.
fn parse_pattern(pattern: &str) -> Result<Vec<Segment>, String> {
    let pattern = strip(pattern);
    let mut segments = Vec::new();
    let mut remaining = pattern.to_owned();
    while !remaining.is_empty() {
        let Some(found) = match_segment(&remaining) else {
            if segments.is_empty() {
                return Err(format!("Invalid pattern: {pattern}\n{}", syntax_help()));
            }
            let head: String = remaining.chars().take(40).collect();
            return Err(format!(
                "Invalid pattern continuation: ...{head}\n{}",
                syntax_help()
            ));
        };
        segments.push(parse_segment(&found)?);
        let rest = strip(&remaining[found.end..]);
        remaining = if rest.is_empty() {
            String::new()
        } else {
            format!("({}){rest}", found.target)
        };
    }
    if segments.is_empty() {
        return Err(format!("Invalid pattern: {pattern}\n{}", syntax_help()));
    }
    let cap = tables().max_chain_segments;
    if segments.len() > cap {
        return Err(format!(
            "Pattern has {} segments, maximum is {cap}. Simplify the pattern.",
            segments.len()
        ));
    }
    Ok(segments)
}

/// A string attribute of a node, `""` when absent or not a string.
fn text_of<'a>(node: &'a Map<String, Value>, key: &str) -> &'a str {
    node.get(key).and_then(Value::as_str).unwrap_or("")
}

/// An edge's relation type, lowercased (`''` when it has none).
fn relation_of(edge: &Map<String, Value>) -> String {
    text_of(edge, "relation_type").to_lowercase()
}

/// The pattern engine over one view.
struct Engine<'a> {
    view: &'a GraphView,
    position: HashMap<&'a str, usize>,
}

impl<'a> Engine<'a> {
    fn new(view: &'a GraphView) -> Self {
        let position = view
            .graph
            .nodes()
            .enumerate()
            .map(|(index, (id, _))| (id, index))
            .collect();
        Self { view, position }
    }

    /// A set of ids, in node order.
    fn ordered(&self, ids: HashSet<String>) -> Vec<String> {
        let mut ids: Vec<String> = ids.into_iter().collect();
        ids.sort_by_key(|id| {
            self.position
                .get(id.as_str())
                .copied()
                .unwrap_or(usize::MAX)
        });
        ids
    }

    /// `_resolve_pattern_nodes`: `None` for the full wildcard.
    fn resolve(&self, spec: &NodeSpec) -> Option<Vec<String>> {
        if spec.name.is_none() && spec.types.is_none() {
            return None;
        }
        if let Some(name) = &spec.name {
            let mut ids: HashSet<String> = self.view.ids_named(name).iter().cloned().collect();
            if ids.is_empty() {
                ids = lexical::search(self.view, name, 5)
                    .into_iter()
                    .map(|hit| hit.id)
                    .collect();
            }
            if let Some(types) = spec.types.as_ref().filter(|_| !ids.is_empty()) {
                ids.retain(|id| {
                    self.view
                        .node(id)
                        .is_some_and(|node| types.contains(&text_of(node, "type").to_lowercase()))
                });
            }
            return Some(self.ordered(ids));
        }
        let mut ids = HashSet::new();
        for kind in spec.types.iter().flatten() {
            ids.extend(self.view.ids_of_type(kind).iter().cloned());
            for layer_type in layer_types(kind).into_iter().flatten() {
                ids.extend(self.view.ids_of_type(layer_type).iter().cloned());
            }
        }
        Some(self.ordered(ids))
    }

    /// `_seed_from_edges`: the first 500 distinct endpoints of matching
    /// edges, in edge order — `from_source` takes each edge's source.
    fn seeds(&self, rel_types: Option<&HashSet<String>>, from_source: bool) -> Vec<String> {
        let mut seeds = HashSet::new();
        for (source, target, edge) in self.view.graph.edges() {
            if rel_types.is_some_and(|types| !types.contains(&relation_of(edge))) {
                continue;
            }
            seeds.insert(if from_source { source } else { target }.to_owned());
            if seeds.len() >= MAX_SEEDS {
                break;
            }
        }
        self.ordered(seeds)
    }

    /// The neighbours of `node` along outgoing (`outgoing`) or incoming
    /// edges, with the relation type.
    fn neighbours(&self, node: &str, outgoing: bool) -> Vec<(String, String)> {
        if outgoing {
            self.view
                .out_edges(node)
                .map(|(target, edge)| (target.to_owned(), relation_of(edge)))
                .collect()
        } else {
            self.view
                .in_edges(node)
                .map(|(source, edge)| (source.to_owned(), relation_of(edge)))
                .collect()
        }
    }

    /// `_execute_pattern`.
    fn execute(
        &self,
        segment: &Segment,
        max_results: usize,
        start_override: Option<Vec<String>>,
    ) -> Vec<PatternPath> {
        let source_nodes = match start_override {
            Some(ids) => Some(ids),
            None => self.resolve(&segment.source),
        };
        let target_nodes = self.resolve(&segment.target);
        let mut outgoing = segment.forward;
        let (mut start, mut end) = (source_nodes, target_nodes);
        let mut flipped = false;
        if start.is_none() && end.is_some() {
            std::mem::swap(&mut start, &mut end);
            flipped = true;
            outgoing = !outgoing;
        }
        let rel_types: Option<HashSet<String>> = segment
            .rel_types
            .as_ref()
            .filter(|types| !types.is_empty())
            .map(|types| types.iter().cloned().collect());
        let start = start.unwrap_or_else(|| self.seeds(rel_types.as_ref(), outgoing));
        let end: Option<HashSet<String>> = end.map(|ids| ids.into_iter().collect());

        let mut results = Vec::new();
        let mut queue: VecDeque<(Vec<String>, Vec<String>)> =
            start.into_iter().map(|id| (vec![id], Vec::new())).collect();
        while results.len() < max_results {
            let Some((path, edges)) = queue.pop_front() else {
                break;
            };
            let depth = i128::try_from(edges.len()).unwrap_or(i128::MAX);
            let Some(current) = path.last().cloned() else {
                continue;
            };
            if depth >= segment.min_hops && end.as_ref().is_none_or(|end| end.contains(&current)) {
                let found = if flipped {
                    PatternPath {
                        path: path.iter().rev().cloned().collect(),
                        edges: edges.iter().rev().cloned().collect(),
                    }
                } else {
                    PatternPath {
                        path: path.clone(),
                        edges: edges.clone(),
                    }
                };
                results.push(found);
                if results.len() >= max_results {
                    break;
                }
            }
            if depth >= segment.max_hops {
                continue;
            }
            for (neighbour, relation) in self.neighbours(&current, outgoing) {
                if rel_types
                    .as_ref()
                    .is_some_and(|types| !types.contains(&relation))
                {
                    continue;
                }
                if path.contains(&neighbour) {
                    continue;
                }
                let mut next_path = path.clone();
                next_path.push(neighbour);
                let mut next_edges = edges.clone();
                next_edges.push(relation);
                queue.push_back((next_path, next_edges));
            }
        }
        results
    }

    /// `_execute_chain`.
    fn chain(&self, segments: &[Segment], max_results: usize) -> Vec<PatternPath> {
        let Some((first, rest)) = segments.split_first() else {
            return Vec::new();
        };
        let mut partial = self.execute(first, max_results.saturating_mul(5), None);
        if partial.is_empty() {
            return partial;
        }
        for segment in rest {
            let mut endpoints: Vec<String> = Vec::new();
            let mut seen = HashSet::new();
            for found in &partial {
                if let Some(last) = found.path.last()
                    && seen.insert(last.clone())
                {
                    endpoints.push(last.clone());
                }
                if seen.len() >= MAX_ENDPOINTS {
                    break;
                }
            }
            if endpoints.is_empty() {
                return Vec::new();
            }
            if self
                .resolve(&segment.target)
                .is_some_and(|targets| targets.is_empty())
            {
                return Vec::new();
            }
            let mut by_start: HashMap<String, Vec<PatternPath>> = HashMap::new();
            for start in endpoints {
                let continuations = self.execute(
                    segment,
                    max_results.saturating_mul(3),
                    Some(vec![start.clone()]),
                );
                if !continuations.is_empty() {
                    by_start.insert(start, continuations);
                }
            }
            if by_start.is_empty() {
                return Vec::new();
            }
            let mut next = Vec::new();
            'stitch: for found in &partial {
                let Some(end) = found.path.last() else {
                    continue;
                };
                for continuation in by_start.get(end).into_iter().flatten() {
                    let mut path = found.path.clone();
                    path.extend(continuation.path.iter().skip(1).cloned());
                    let mut edges = found.edges.clone();
                    edges.extend(continuation.edges.iter().cloned());
                    next.push(PatternPath { path, edges });
                    if next.len() >= max_results {
                        break 'stitch;
                    }
                }
            }
            partial = next;
            if partial.is_empty() {
                return partial;
            }
        }
        partial.truncate(max_results);
        partial
    }
}

/// `KnowledgeGraph.query_pattern(pattern, max_results)`: the matching paths
/// (at most `min(max_results, 100)`), or the `ValueError` message of a
/// pattern that does not parse.
///
/// # Errors
///
/// The pattern is invalid (Python's message, syntax help included).
pub fn find_paths(
    view: &GraphView,
    pattern: &str,
    max_results: usize,
) -> Result<Vec<PatternPath>, String> {
    let max_results = max_results.min(tables().max_pattern_results);
    let segments = parse_pattern(pattern)?;
    let engine = Engine::new(view);
    Ok(match segments.as_slice() {
        [single] => engine.execute(single, max_results, None),
        chain => engine.chain(chain, max_results),
    })
}

/// A path node as `_format_path_result` names it: its `name` (the id when
/// it has none) and `type` (`''` when it has none).
fn name_and_type(view: &GraphView, id: &str) -> (String, String) {
    let node = view.node(id);
    let name = node
        .and_then(|node| node.get("name"))
        .map_or_else(|| id.to_owned(), py_str);
    let kind = node
        .and_then(|node| node.get("type"))
        .map_or_else(String::new, py_str);
    (name, kind)
}

fn plural(count: usize) -> &'static str {
    if count == 1 { "" } else { "s" }
}

/// The chat agent's `query_pattern` tool: the paths of `pattern_input` as
/// text, the syntax help for an empty pattern, Python's message for an
/// invalid one.
#[must_use]
pub fn query_pattern(view: &GraphView, pattern_input: &str) -> String {
    let pattern = strip(pattern_input);
    if pattern.is_empty() {
        return syntax_help().to_owned();
    }
    let results = match find_paths(view, pattern, CHAT_MAX_RESULTS) {
        Ok(results) => results,
        Err(message) => return message,
    };
    if results.is_empty() {
        return format!("No paths found matching pattern: {pattern}");
    }
    let mut output = format!(
        "# Pattern: {pattern}\nFound {} path{}\n\n",
        results.len(),
        plural(results.len())
    );
    for (index, found) in results.iter().enumerate() {
        let mut parts = Vec::new();
        for (step, id) in found.path.iter().enumerate() {
            let (name, kind) = name_and_type(view, id);
            parts.push(format!("**{name}** ({kind})"));
            if let Some(relation) = found.edges.get(step) {
                parts.push(format!("=[{relation}]="));
            }
        }
        let length = found.edges.len();
        output += &format!(
            "{:2}. {} ({length} hop{})\n",
            index + 1,
            parts.join(" "),
            plural(length)
        );
    }
    if results.len() >= CHAT_MAX_RESULTS {
        output += &format!(
            "\n_Showing first {CHAT_MAX_RESULTS} results. Narrow your pattern for more specific results._\n"
        );
    }
    output
}

/// Counts in first-seen order, then sorted by count descending (stable).
fn by_count(values: impl Iterator<Item = String>) -> Vec<(String, usize)> {
    let mut counts: Vec<(String, usize)> = Vec::new();
    let mut index: HashMap<String, usize> = HashMap::new();
    for value in values {
        if let Some(&at) = index.get(&value) {
            counts[at].1 += 1;
        } else {
            index.insert(value.clone(), counts.len());
            counts.push((value, 1));
        }
    }
    counts.sort_by_key(|count| std::cmp::Reverse(count.1));
    counts
}

/// The chat agent's `get_pattern_vocabulary` tool
/// (`KnowledgeGraph.get_pattern_vocabulary` as text).
#[must_use]
pub fn pattern_vocabulary(view: &GraphView) -> String {
    let entity_types = by_count(
        view.graph
            .nodes()
            .filter_map(|(_, node)| node.get("type").map(py_str)),
    );
    let relation_types = by_count(
        view.graph
            .edges()
            .filter_map(|(_, _, edge)| edge.get("relation_type").map(py_str)),
    );
    let mut examples = Vec::new();
    if let (Some((relation, _)), Some((entity, _))) = (relation_types.first(), entity_types.first())
    {
        examples.push(format!("(?:{entity})-[:{relation}*1..2]->(?)"));
        if let Some((second, _)) = relation_types.get(1) {
            examples.push(format!("(?:{entity})-[:{second}]->(?)"));
        }
        if let Some((second, _)) = entity_types.get(1) {
            examples.push(format!("(?:{entity})-[:{relation}*1..3]->(?:{second})"));
        }
    }
    let mut output = "# Graph Vocabulary for Pattern Queries\n\n## Entity Types\n".to_owned();
    for (kind, count) in &entity_types {
        output += &format!("- **{kind}**: {count}\n");
    }
    output += "\n## Relation Types\n";
    for (kind, count) in &relation_types {
        output += &format!("- **{kind}**: {count}\n");
    }
    if !examples.is_empty() {
        output += "\n## Example Patterns\n";
        for example in &examples {
            output += &format!("- `{example}`\n");
        }
    }
    output
}

/// `KnowledgeGraph.search(query, top_k)` without filters — the lexical
/// ranking the pattern engine's fuzzy fallback and the community tools
/// read. (A private copy: the search tools own the full search.)
pub(super) mod lexical {
    use super::{GraphView, text_of};
    use elitea_engine_core::pystr::strip;
    use serde_json::Value;
    use std::collections::HashSet;

    /// One ranked entity.
    #[derive(Debug, Clone, PartialEq)]
    pub struct Hit {
        pub id: String,
        pub score: f64,
    }

    /// `_tokenize`: the lowercased alphanumeric runs, each also split into
    /// its letter and digit runs.
    pub fn tokenize(text: &str) -> HashSet<String> {
        let lowered = text.to_lowercase();
        let mut tokens = HashSet::new();
        for word in lowered
            .split(|c: char| !c.is_ascii_alphanumeric())
            .filter(|w| !w.is_empty())
        {
            tokens.insert(word.to_owned());
            let mut run = String::new();
            let mut digits = false;
            for c in word.chars() {
                if !run.is_empty() && c.is_ascii_digit() != digits {
                    tokens.insert(std::mem::take(&mut run));
                }
                digits = c.is_ascii_digit();
                run.push(c);
            }
            if !run.is_empty() {
                tokens.insert(run);
            }
        }
        tokens
    }

    #[allow(clippy::cast_precision_loss, reason = "token counts")]
    fn share(overlap: usize, total: usize) -> f64 {
        overlap as f64 / total as f64
    }

    /// `_calculate_match_score` (the score only).
    fn score(
        query_tokens: &HashSet<String>,
        query: &str,
        name: &str,
        kind: &str,
        description: &str,
        file_path: &str,
    ) -> f64 {
        let name_lower = name.to_lowercase();
        if query == name_lower {
            return 1.0;
        }
        if name_lower.contains(query) {
            return if name_lower.starts_with(query) {
                0.85
            } else {
                0.75
            };
        }
        let name_tokens = tokenize(name);
        if !query_tokens.is_empty() && !name_tokens.is_empty() {
            let overlap = query_tokens.intersection(&name_tokens).count();
            if overlap > 0 {
                if overlap == query_tokens.len() {
                    return 0.7;
                }
                return 0.6 * share(overlap, query_tokens.len());
            }
        }
        if !file_path.is_empty() && file_path.to_lowercase().contains(query) {
            return 0.55;
        }
        if !description.is_empty() {
            if description.to_lowercase().contains(query) {
                return 0.5;
            }
            let description_tokens = tokenize(description);
            if !query_tokens.is_empty() && !description_tokens.is_empty() {
                let overlap = query_tokens.intersection(&description_tokens).count();
                if overlap > 0 {
                    return 0.35 * share(overlap, query_tokens.len());
                }
            }
        }
        if kind.to_lowercase().contains(query) {
            return 0.3;
        }
        0.0
    }

    /// The node's first cited file, else its `file_path`.
    pub fn primary_file(node: &serde_json::Map<String, Value>) -> &str {
        let mut citations: Vec<&Value> = node
            .get("citations")
            .and_then(Value::as_array)
            .map(|items| items.iter().collect())
            .unwrap_or_default();
        if citations.is_empty()
            && let Some(citation) = node.get("citation")
        {
            citations.push(citation);
        }
        citations
            .iter()
            .find_map(|citation| citation.as_object())
            .map_or_else(
                || text_of(node, "file_path"),
                |citation| text_of(citation, "file_path"),
            )
    }

    /// The node's description, else its `properties`' one.
    pub fn description(node: &serde_json::Map<String, Value>) -> &str {
        let own = text_of(node, "description");
        if !own.is_empty() {
            return own;
        }
        node.get("properties")
            .and_then(Value::as_object)
            .map_or(own, |properties| text_of(properties, "description"))
    }

    /// The ranked entities: score descending, then lowercased name.
    pub fn search(view: &GraphView, query: &str, top_k: usize) -> Vec<Hit> {
        let lowered = query.to_lowercase();
        let query_lower = strip(&lowered);
        let query_tokens = tokenize(query);
        let mut hits: Vec<(Hit, String)> = Vec::new();
        for (id, node) in view.graph.nodes() {
            let kind = text_of(node, "type").to_lowercase();
            let name = text_of(node, "name");
            let score = score(
                &query_tokens,
                query_lower,
                name,
                &kind,
                description(node),
                primary_file(node),
            );
            if score > 0.0 {
                hits.push((
                    Hit {
                        id: id.to_owned(),
                        score,
                    },
                    name.to_lowercase(),
                ));
            }
        }
        hits.sort_by(|(a, a_name), (b, b_name)| {
            b.score.total_cmp(&a.score).then_with(|| a_name.cmp(b_name))
        });
        hits.into_iter().take(top_k).map(|(hit, _)| hit).collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hop_counts_read_as_python_int_reads_them() {
        assert_eq!(py_int(" 2 "), Some(2));
        assert_eq!(py_int("+3"), Some(3));
        assert_eq!(py_int("-1"), Some(-1));
        assert_eq!(py_int("1_0"), Some(10));
        assert_eq!(py_int("1__0"), None);
        assert_eq!(py_int(""), None);
        assert_eq!(py_int("x"), None);
        assert_eq!(py_int("_1"), None);
    }

    #[test]
    fn node_specifiers_parse_as_python_parses_them() {
        let wildcard = NodeSpec {
            name: None,
            types: None,
        };
        assert_eq!(parse_node_spec(" ? "), wildcard);
        assert_eq!(parse_node_spec("?:"), wildcard);
        assert_eq!(
            parse_node_spec("?:Class, function,"),
            NodeSpec {
                name: None,
                types: Some(vec!["class".into(), "function".into()])
            }
        );
        assert_eq!(
            parse_node_spec(":method"),
            NodeSpec {
                name: None,
                types: Some(vec!["method".into()])
            }
        );
        assert_eq!(
            parse_node_spec("User:"),
            NodeSpec {
                name: Some("User".into()),
                types: None
            }
        );
    }

    #[test]
    fn a_segment_matches_the_python_regex() {
        let found = match_segment("(a) <-[:x*1..2]- (b)rest").map(|m| (m.arrow, m.relation, m.end));
        assert_eq!(found, Some(("<-[:x*1..2]-", "x*1..2", 20)));
        assert!(match_segment("(a)-[:x]--(b)").is_none());
        assert!(match_segment("(a)[:x]->(b)").is_none());
        assert!(match_segment("(a)-[:x]>(b)").is_some());
    }

    #[test]
    fn tokens_split_letters_from_digits() {
        let mut tokens: Vec<String> = lexical::tokenize("getUser2FA v2").into_iter().collect();
        tokens.sort();
        assert_eq!(tokens, ["2", "fa", "getuser", "getuser2fa", "v", "v2"]);
    }
}
