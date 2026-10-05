//! The Phase 1 graph build (`EnhancedUnifiedGraphBuilder.analyze_repository`
//! without the parsing itself).
//!
//! Order of work, as in Python:
//!
//! 1. discovery;
//! 2. one graph per rich language from its `ParseResult`s, merged in
//!    language order — symbols first (`_process_file_symbols`), then
//!    relationships (`_add_relationships_bulk`);
//! 3. documentation chunks as nodes;
//! 4. noise-node contraction (`contract_graph_inplace`);
//! 5. the SQL subgraph, then ORM model → table links.
//!
//! # What a parser must give the builder
//!
//! * `ParseResult`s keyed by ABSOLUTE path `<repo_root>/<rel>`, the same
//!   string discovery returns; the builder derives `rel_path` from it and
//!   the node id's file part from its stem.
//! * Symbols: `name` (the node's `symbol_name`), `full_name` dotted with
//!   `.` and starting with the file stem when it is module-qualified (the
//!   id becomes `{language}::{stem}::{full_name minus the stem}`), and
//!   `parent_symbol` in one of the forms contraction resolves (dotted
//!   `module.Class`, bare `Type` in the same file, or empty).
//! * Relationships: `source_symbol` is a local qualified name in the source
//!   file, the file stem for file-level relationships (imports), or a name
//!   the registry knows; `target_symbol` is resolved through the
//!   strategies of `resolve_target` — an unresolved target drops the
//!   edge. `target_file` (any path form; only its stem is used) steers the
//!   lookup to that file first. `source_range` becomes the `via` callsite.
//!
//! Every ordering decision follows the reference's sorted file order, so the
//! output is deterministic.
//!
//! Not ported: the basic tier (Ruby, Kotlin, Scala through
//! `code_splitter.py`, `_build_basic_language_graph`). Those files are
//! discovered but produce no nodes until a parser for them lands; injected
//! results for them would be built as rich results. The lookup indexes of
//! `_build_graph_indexes` / `_build_node_index` are not ported either: they
//! are in-memory attributes of the Python graph, never rows, and no
//! Phase 1c pass reads them.

use super::contraction::{self, ContractionReport};
use super::discover::{self, Discovery};
use super::documents::{self, DocResult};
use super::pystr;
use super::{Attributes, CodeGraph, EdgeData, NodeData, NodeSymbol, SharedStr, orm, sql};
use crate::parsers::model::{ParseResult, Relationship, Symbol};
use indexmap::IndexMap;
use serde_json::{Map, Value};
use std::collections::{BTreeMap, HashMap};
use std::time::{Duration, Instant};

/// Language → absolute path → parse result.
pub type ParseResultsByLanguage = BTreeMap<String, BTreeMap<String, ParseResult>>;

/// What a build did, for logs and the parity tool.
#[derive(Debug, Clone, Default)]
pub struct BuildReport {
    pub rich_files: usize,
    pub doc_files: usize,
    pub sql_files: usize,
    pub relationships_attempted: usize,
    pub relationships_added: usize,
    pub contraction: ContractionReport,
    pub sql: sql::SqlStats,
    pub orm_edges: usize,
    pub timings: Vec<(&'static str, Duration)>,
}

/// Build the Phase 1 graph of `repo_root` from injected parse results.
///
/// `repo_root` is the absolute repository path (canonicalised by the
/// caller, as Python's `Path.resolve()`).
#[must_use]
pub fn build_graph(repo_root: &str, parse_results: ParseResultsByLanguage) -> CodeGraph {
    let discovery = discover::discover_files(repo_root);
    build_graph_with_report(repo_root, &discovery, parse_results).0
}

/// The graph the index stores: the Phase 1 build, then Phase 1c
/// ([`super::phase1c::run_phase1c`]) under `flags`, as
/// `filesystem_indexer._write_unified_db` runs them before
/// `from_networkx`.
#[must_use]
pub fn build_index_graph(
    repo_root: &str,
    discovery: &Discovery,
    parse_results: ParseResultsByLanguage,
    flags: &super::flags::Phase1cFlags,
) -> (CodeGraph, BuildReport, super::phase1c::Phase1cReport) {
    let (mut graph, report) = build_graph_with_report(repo_root, discovery, parse_results);
    let phase1c = super::phase1c::run_phase1c(&mut graph, repo_root.trim_end_matches('/'), flags);
    (graph, report, phase1c)
}

/// [`build_index_graph`] with this engine's own parsers, one language at a
/// time: a language is parsed, built into the graph and its parse results
/// dropped before the next language is parsed, so the peak memory holds
/// one language's results, not all of them.
///
/// The graph is the one [`parse_repository`] + [`build_index_graph`]
/// build: languages run in the same sorted order, and every parser labels
/// each of its results with its own language (`ParseResult.language`), so
/// the grouping by label of [`build_graph_with_report`] is this per-language
/// order too.
#[must_use]
pub fn build_index_graph_parsed(
    repo_root: &str,
    discovery: &Discovery,
    flags: &super::flags::Phase1cFlags,
) -> (CodeGraph, BuildReport, super::phase1c::Phase1cReport) {
    let groups = discovery
        .files_by_language
        .iter()
        .filter(|(language, _)| !matches!(language.as_str(), "documentation" | "sql" | "unknown"))
        .filter_map(|(language, files)| {
            let parser = crate::parsers::parser_for(language)?;
            let results: Vec<(String, ParseResult)> =
                parser.parse_files(files).into_iter().collect();
            debug_assert!(results.iter().all(|(_, r)| r.language == *language));
            (!results.is_empty()).then(|| (language.clone(), results))
        });
    let (mut graph, report) = build_from_groups(repo_root, discovery, groups);
    let phase1c = super::phase1c::run_phase1c(&mut graph, repo_root.trim_end_matches('/'), flags);
    (graph, report, phase1c)
}

/// Parse every discovered file with this engine's own parsers
/// ([`crate::parsers::parser_for`]); languages without a parser are left
/// out, as Python skips C and PHP.
#[must_use]
pub fn parse_repository(discovery: &Discovery) -> ParseResultsByLanguage {
    let mut results = ParseResultsByLanguage::new();
    for (language, files) in &discovery.files_by_language {
        if matches!(language.as_str(), "documentation" | "sql" | "unknown") {
            continue;
        }
        if let Some(parser) = crate::parsers::parser_for(language) {
            results.insert(language.clone(), parser.parse_files(files));
        }
    }
    results
}

/// [`build_graph`] with an explicit discovery and a report.
#[must_use]
pub fn build_graph_with_report(
    repo_root: &str,
    discovery: &Discovery,
    parse_results: ParseResultsByLanguage,
) -> (CodeGraph, BuildReport) {
    build_from_groups(
        repo_root,
        discovery,
        group_rich_results(parse_results).into_iter(),
    )
}

/// The Phase 1 build over the rich results grouped by language, in group
/// order. Each group is consumed (built and merged) before the next one is
/// taken from `groups`.
fn build_from_groups(
    repo_root: &str,
    discovery: &Discovery,
    groups: impl Iterator<Item = (String, Vec<(String, ParseResult)>)>,
) -> (CodeGraph, BuildReport) {
    let repo_root = repo_root.trim_end_matches('/');
    let mut report = BuildReport::default();
    let mut clock = Instant::now();
    let mut lap = |report: &mut BuildReport, name: &'static str| {
        report.timings.push((name, clock.elapsed()));
        clock = Instant::now();
    };

    let docs = documents::parse_documentation_files(discovery.files("documentation"), repo_root);
    report.doc_files = docs.len();
    lap(&mut report, "documents");

    let mut graph = CodeGraph::new();
    for (language, files) in groups {
        report.rich_files += files.len();
        let (language_graph, attempted, added) = build_language_graph(&language, files, repo_root);
        report.relationships_attempted += attempted;
        report.relationships_added += added;
        graph.merge(language_graph);
    }
    lap(&mut report, "rich graph");

    add_document_nodes(&mut graph, docs);
    lap(&mut report, "document nodes");

    report.contraction = contraction::contract_graph_inplace(&mut graph);
    lap(&mut report, "contraction");

    let sql_files = discovery.files("sql");
    report.sql_files = sql_files.len();
    if !sql_files.is_empty() {
        let (sql_graph, stats) = sql::build_sql_graph(sql_files, repo_root);
        report.sql = stats;
        if sql_graph.node_count() > 0 {
            graph.merge(sql_graph);
        }
    }
    lap(&mut report, "sql");

    report.orm_edges = orm::link_orm_models(&mut graph);
    lap(&mut report, "orm");
    (graph, report)
}

/// Group the rich results by `ParseResult.language`, in order of first
/// appearance over the sorted languages and paths (`rich_languages_found`).
fn group_rich_results(
    parse_results: ParseResultsByLanguage,
) -> IndexMap<String, Vec<(String, ParseResult)>> {
    let mut grouped: IndexMap<String, Vec<(String, ParseResult)>> = IndexMap::new();
    for (language, files) in parse_results {
        if matches!(language.as_str(), "documentation" | "sql" | "unknown") {
            continue;
        }
        for (path, result) in files {
            grouped
                .entry(result.language.clone())
                .or_default()
                .push((path, result));
        }
    }
    grouped
}

/// The three registries of `_build_graph_sync`.
#[derive(Debug, Default)]
struct Registry {
    /// Simple name → node ids, in insertion order (duplicates kept).
    by_name: HashMap<String, Vec<String>>,
    /// Local qualified name and `full_name` → node id (last wins).
    by_qualified_name: HashMap<String, String>,
}

/// `_get_local_qualified_name`: `full_name` without a leading file-stem
/// part, else the simple name.
#[must_use]
pub fn local_qualified_name(symbol: &Symbol, file_name: &str) -> String {
    match symbol.full_name.as_deref().filter(|f| !f.is_empty()) {
        Some(full_name) => {
            let parts: Vec<&str> = full_name.split('.').collect();
            if parts.len() >= 2 && parts[0] == file_name {
                parts[1..].join(".")
            } else {
                full_name.to_owned()
            }
        }
        None => symbol.name.clone(),
    }
}

/// The `node_id_style == "rel_path"` collision suffix: `__` + the relative
/// path with `/` → `__` and `.` → `_`.
#[must_use]
pub fn collision_suffix(rel_path: &str) -> String {
    rel_path
        .replace('\\', "/")
        .replace('/', "__")
        .replace('.', "_")
}

/// Build one language's graph (`_build_comprehensive_language_graph`).
/// Returns the graph and the relationships attempted / added.
fn build_language_graph(
    language: &str,
    files: Vec<(String, ParseResult)>,
    repo_root: &str,
) -> (CodeGraph, usize, usize) {
    // One copy of the language name for every node.
    let language = &SharedStr::from(language);
    let mut graph = CodeGraph::new();
    let mut registry = Registry::default();
    let mut relationships = Vec::with_capacity(files.len());
    for (path, mut result) in files {
        // One copy of the path for every node and edge of the file.
        let path = SharedStr::from(path);
        let symbols = std::mem::take(&mut result.symbols);
        process_file_symbols(
            &mut graph,
            &path,
            symbols,
            language,
            &mut registry,
            repo_root,
        );
        // Keep only what resolution reads; the rest of the relationship
        // (its paths, context, …) is freed now, not after every file.
        let pending: Vec<PendingRelationship> = std::mem::take(&mut result.relationships)
            .into_iter()
            .map(PendingRelationship::from)
            .collect();
        relationships.push((path, pending));
    }
    let (attempted, added) = add_relationships(&mut graph, relationships, language, &registry);
    (graph, attempted, added)
}

/// `_process_file_symbols`.
///
/// What a node keeps of its parser symbol is COPIED, not moved, and the
/// same holds for the strings and maps an edge keeps of its relationship:
/// the parse results of a language are then freed whole once its graph is
/// built, instead of leaving moved strings scattered through their memory,
/// which the allocator could not reuse for the next language.
fn process_file_symbols(
    graph: &mut CodeGraph,
    file_path: &SharedStr,
    symbols: Vec<Symbol>,
    language: &SharedStr,
    registry: &mut Registry,
    repo_root: &str,
) {
    let file_name = pystr::stem(file_path);
    let shared_name = SharedStr::from(file_name);
    let rel_path = SharedStr::from(documents::relative_path(file_path, repo_root));
    for symbol in symbols {
        if pystr::strip(&symbol.name).is_empty() {
            continue;
        }
        let symbol_type = symbol.symbol_type.as_str();
        if matches!(symbol_type, "module" | "namespace") {
            continue;
        }
        let local = local_qualified_name(&symbol, file_name);
        let mut node_id = format!("{language}::{file_name}::{local}");
        if let Some(existing) = graph.node(&node_id) {
            if existing.file_path == *file_path {
                // The same symbol twice in one file: the first wins.
                continue;
            }
            // Another file with the same stem (a declaration and its body,
            // two `index.ts`, …). The suffix is NOT checked again: a second
            // colliding symbol of this file overwrites the suffixed node,
            // as networkx `add_node` does.
            node_id = format!("{node_id}__{}", collision_suffix(&rel_path));
        }
        registry
            .by_name
            .entry(symbol.name.clone())
            .or_default()
            .push(node_id.clone());
        registry
            .by_qualified_name
            .insert(local.clone(), node_id.clone());
        if let Some(full_name) = symbol.full_name.as_deref().filter(|f| !f.is_empty()) {
            registry
                .by_qualified_name
                .insert(full_name.to_owned(), node_id.clone());
        }
        graph.add_node(
            &node_id,
            NodeData {
                file_path: file_path.clone(),
                rel_path: rel_path.clone(),
                file_name: shared_name.clone(),
                language: language.clone(),
                symbol_type: symbol_type.into(),
                symbol_name: symbol.name.clone(),
                start_line: i64::from(symbol.range.start.line),
                end_line: i64::from(symbol.range.end.line),
                parent_symbol: symbol.parent_symbol.clone(),
                analysis_level: "comprehensive".into(),
                symbol: Some(Box::new(NodeSymbol {
                    source_text: symbol.source_text.clone(),
                    docstring: symbol.docstring.clone(),
                    signature: symbol.signature.clone(),
                    return_type: symbol.return_type.clone(),
                })),
                ..NodeData::default()
            },
        );
    }
}

/// The parts of a parser [`Relationship`] the relationship pass reads.
struct PendingRelationship {
    source_symbol: String,
    target_symbol: String,
    relationship_type: crate::parsers::model::RelationshipType,
    /// The stem of a non-empty `target_file`: the file the lookup tries
    /// first.
    target_hint: Option<String>,
    /// The line of `source_range`, for the `via` callsite.
    line: Option<u32>,
    annotations: Map<String, Value>,
}

impl From<Relationship> for PendingRelationship {
    fn from(relationship: Relationship) -> Self {
        Self {
            target_hint: relationship
                .target_file
                .as_deref()
                .filter(|t| !t.is_empty())
                .map(|t| pystr::stem(t).to_owned()),
            line: relationship.source_range.map(|r| r.start.line),
            source_symbol: relationship.source_symbol,
            target_symbol: relationship.target_symbol,
            relationship_type: relationship.relationship_type,
            annotations: relationship.annotations,
        }
    }
}

/// An edge prepared by the resolution pass, added in a second pass.
struct PreparedEdge {
    source: String,
    target: String,
    data: Box<EdgeData>,
    /// Index into the relationships' files: (path, stem).
    file: usize,
}

/// `_add_relationships_bulk`. Returns (attempted, added).
fn add_relationships(
    graph: &mut CodeGraph,
    relationships: Vec<(SharedStr, Vec<PendingRelationship>)>,
    language: &SharedStr,
    registry: &Registry,
) -> (usize, usize) {
    // `_build_node_lookup_cache` is the nodes before any edge is added, plus
    // the `__file__` nodes source resolution creates. Until the edges are
    // added below, those are exactly the graph's nodes (resolution adds
    // nothing else), so the graph itself is the cache.
    let mut prepared = Vec::new();
    let mut attempted = 0;
    let mut files: Vec<(SharedStr, SharedStr)> = Vec::with_capacity(relationships.len());
    // Target file stems, one copy each.
    let mut stems: HashMap<String, SharedStr> = HashMap::new();
    for (file_path, file_relationships) in relationships {
        let file_name = SharedStr::from(pystr::stem(&file_path));
        let file = files.len();
        for relationship in file_relationships {
            attempted += 1;
            let rel_type = relationship.relationship_type.as_str();
            if rel_type == "defines" && is_container_definition(&relationship.annotations) {
                continue;
            }
            let source = resolve_source(
                &relationship.source_symbol,
                language,
                &file_name,
                &file_path,
                graph,
                registry,
            );
            let hint = relationship.target_hint.as_deref();
            let Some(target) = resolve_target(
                &relationship.target_symbol,
                language,
                &file_name,
                registry,
                graph,
                hint,
            ) else {
                // An unresolved target is an external reference.
                continue;
            };
            let target_file = if target.contains("::") {
                let stem = target.split("::").nth(1).unwrap_or_default();
                if let Some(shared) = stems.get(stem) {
                    shared.clone()
                } else {
                    let shared = SharedStr::from(stem);
                    stems.insert(stem.to_owned(), shared.clone());
                    shared
                }
            } else {
                file_name.clone()
            };
            let mut annotations = relationship.annotations;
            if let Some(line) = relationship.line {
                // `setdefault("via", [])`: a non-list `via` is left alone.
                if let Value::Array(via) = annotations
                    .entry("via")
                    .or_insert_with(|| Value::Array(Vec::with_capacity(1)))
                {
                    via.push(Value::String(format!("{rel_type}@L{line}")));
                }
            }
            // A copy with no spare capacity (see `process_file_symbols`).
            let annotations = Attributes::from(&annotations);
            prepared.push(PreparedEdge {
                source,
                target,
                data: Box::new(EdgeData {
                    rel_type: rel_type.into(),
                    source_file: file_name.clone(),
                    target_file,
                    annotations,
                    source_context: relationship.source_symbol.clone(),
                    target_context: relationship.target_symbol.clone(),
                    ..EdgeData::default()
                }),
                file,
            });
        }
        files.push((file_path, file_name));
    }
    let added = prepared.len();
    for edge in prepared {
        let (file_path, file_name) = &files[edge.file];
        add_prepared_edge(graph, edge, file_path, file_name, language);
    }
    (attempted, added)
}

/// A `defines` from a module, namespace or package container is skipped.
fn is_container_definition(annotations: &Map<String, Value>) -> bool {
    annotations
        .get("container_type")
        .and_then(Value::as_str)
        .is_some_and(|t| {
            matches!(
                t.to_lowercase().as_str(),
                "module" | "namespace" | "package"
            )
        })
}

/// The bulk-add loop: create a missing endpoint as an inferred node, then
/// add the edge.
fn add_prepared_edge(
    graph: &mut CodeGraph,
    edge: PreparedEdge,
    file_path: &SharedStr,
    file_name: &SharedStr,
    language: &SharedStr,
) {
    if !graph.has_node(&edge.source) {
        // The relationship's source symbol (`source_context`).
        let source_symbol = &edge.data.source_context;
        let symbol_name = if source_symbol.is_empty() {
            edge.source.rsplit("::").next().unwrap_or_default()
        } else {
            source_symbol.rsplit('.').next().unwrap_or_default()
        };
        graph.add_node(
            &edge.source,
            NodeData {
                symbol_name: symbol_name.to_owned(),
                file_path: file_path.clone(),
                file_name: file_name.clone(),
                language: language.clone(),
                symbol_type: "inferred".into(),
                analysis_level: "comprehensive".into(),
                ..NodeData::default()
            },
        );
    }
    if !graph.has_node(&edge.target) {
        // Unreachable while targets resolve only to cached nodes; kept so
        // the two stay the same if resolution changes.
        let target_symbol = &edge.data.target_context;
        let parts: Vec<&str> = edge.target.split("::").collect();
        let target_name = parts.last().copied().unwrap_or(target_symbol);
        let target_file = parts.get(1).copied().unwrap_or(file_name);
        let scoped = target_symbol.contains('.') || target_symbol.contains("::");
        let target_type = match edge.data.rel_type.as_ref() {
            "inheritance" => "class",
            "calls" if scoped => "method",
            "calls" => "function",
            "imports" if scoped => "class",
            "imports" => "module",
            _ => "unknown",
        };
        graph.add_node(
            &edge.target,
            NodeData {
                symbol_name: target_name.to_owned(),
                file_path: file_path.clone(),
                file_name: target_file.into(),
                language: language.clone(),
                symbol_type: target_type.into(),
                analysis_level: "comprehensive".into(),
                ..NodeData::default()
            },
        );
    }
    graph.add_edge_boxed(&edge.source, &edge.target, edge.data);
}

/// Candidates for a dotted name: every suffix `parts[i..]` from the last
/// part back to the whole name, then every single part from the last back.
fn dotted_candidates(symbol: &str) -> impl Iterator<Item = String> + '_ {
    let parts: Vec<&str> = symbol.split('.').collect();
    let count = parts.len();
    let partials: Vec<String> = (0..count).rev().map(|i| parts[i..].join(".")).collect();
    let singles: Vec<String> = (0..count).rev().map(|i| parts[i].to_owned()).collect();
    partials.into_iter().chain(singles)
}

/// `_resolve_source_node_sync`. Always returns an id; an unknown source
/// becomes `{language}::{file}::{source}` and is created as an inferred
/// node when its edge is added.
fn resolve_source(
    source_symbol: &str,
    language: &SharedStr,
    file_name: &SharedStr,
    file_path: &SharedStr,
    graph: &mut CodeGraph,
    registry: &Registry,
) -> String {
    let direct = format!("{language}::{file_name}::{source_symbol}");
    if graph.has_node(&direct) {
        return direct;
    }
    if source_symbol == file_name.as_str() {
        let file_node = format!("{language}::{file_name}::__file__");
        if !graph.has_node(&file_node) {
            graph.add_node(
                &file_node,
                NodeData {
                    symbol_name: "__file__".to_owned(),
                    file_path: file_path.clone(),
                    file_name: file_name.clone(),
                    language: language.clone(),
                    symbol_type: "module".into(),
                    analysis_level: "comprehensive".into(),
                    ..NodeData::default()
                },
            );
        }
        return file_node;
    }
    if source_symbol.contains('.') {
        for name in dotted_candidates(source_symbol) {
            let candidate = format!("{language}::{file_name}::{name}");
            if graph.has_node(&candidate) {
                return candidate;
            }
        }
    }
    if let Some(candidates) = registry.by_name.get(source_symbol) {
        // `file_name in candidate` is a SUBSTRING test on the whole id.
        let in_file = candidates
            .iter()
            .find(|c| c.contains(file_name.as_str()) && graph.has_node(c));
        if let Some(found) = in_file.or_else(|| candidates.iter().find(|c| graph.has_node(c))) {
            return found.clone();
        }
    }
    if let Some(candidate) = registry.by_qualified_name.get(source_symbol)
        && graph.has_node(candidate)
    {
        return candidate.clone();
    }
    direct
}

/// The symbol types the by-name target lookup prefers.
const DEFINITION_TYPES: &[&str] = &[
    "class",
    "function",
    "method",
    "interface",
    "enum",
    "macro",
    "constant",
    "struct",
];

fn node_type_is(graph: &CodeGraph, id: &str, types: &[&str]) -> bool {
    graph
        .node(id)
        .is_some_and(|n| types.contains(&n.symbol_type.to_lowercase().as_str()))
}

/// `_resolve_target_node_sync`: `None` is an external reference.
fn resolve_target(
    target_symbol: &str,
    language: &str,
    file_name: &str,
    registry: &Registry,
    graph: &CodeGraph,
    target_file_hint: Option<&str>,
) -> Option<String> {
    let cached = |id: String| graph.has_node(&id).then_some(id);
    if let Some(hint) = target_file_hint.filter(|h| *h != file_name)
        && let Some(found) = cached(format!("{language}::{hint}::{target_symbol}"))
    {
        return Some(found);
    }
    if let Some(found) = cached(format!("{language}::{file_name}::{target_symbol}")) {
        return Some(found);
    }
    if let Some(attribute) = target_symbol.strip_prefix("self.")
        && let Some(found) = cached(format!("{language}::{file_name}::{attribute}"))
    {
        return Some(found);
    }
    if target_symbol.contains('.') {
        let parts: Vec<&str> = target_symbol.split('.').collect();
        if let [module, class] = parts.as_slice() {
            if let Some(found) = cached(format!("{language}::{module}::{class}")) {
                return Some(found);
            }
            if let Some(found) = cached(format!("{language}::{file_name}::{class}")) {
                return Some(found);
            }
        }
        for name in dotted_candidates(target_symbol) {
            if let Some(found) = cached(format!("{language}::{file_name}::{name}")) {
                return Some(found);
            }
        }
    }
    if let Some(candidates) = registry.by_name.get(target_symbol) {
        let definition = candidates
            .iter()
            .find(|c| graph.has_node(c) && node_type_is(graph, c, DEFINITION_TYPES));
        if let Some(found) = definition.or_else(|| candidates.iter().find(|c| graph.has_node(c))) {
            return Some(found.clone());
        }
    }
    if let Some(candidate) = registry.by_qualified_name.get(target_symbol)
        && graph.has_node(candidate)
    {
        return Some(candidate.clone());
    }
    if target_symbol.contains('.') {
        let method_name = target_symbol.rsplit('.').next().unwrap_or_default();
        if let Some(found) = cached(format!("{language}::{file_name}::{method_name}")) {
            return Some(found);
        }
        if let Some(candidates) = registry.by_name.get(method_name)
            && let Some(found) = candidates
                .iter()
                .find(|c| graph.has_node(c) && node_type_is(graph, c, &["method", "function"]))
        {
            return Some(found.clone());
        }
    }
    None
}

/// Add the documentation chunks as nodes. A repeated id gets `#1`, `#2`, …
/// against the WHOLE graph.
fn add_document_nodes(graph: &mut CodeGraph, docs: Vec<DocResult>) {
    for doc in docs {
        let file_name = SharedStr::from(pystr::stem(&doc.file_path));
        let file_path = SharedStr::from(doc.file_path.as_str());
        let language = SharedStr::from(doc.language.as_str());
        for symbol in doc.symbols {
            let base = format!("{}::{file_name}::{}", doc.language, symbol.name);
            let mut node_id = base.clone();
            let mut counter = 1;
            while graph.has_node(&node_id) {
                node_id = format!("{base}#{counter}");
                counter += 1;
            }
            let rel_path = if symbol.rel_path.is_empty() {
                doc.file_path.clone()
            } else {
                symbol.rel_path
            };
            graph.add_node(
                &node_id,
                NodeData {
                    symbol_name: symbol.name,
                    symbol_type: symbol.symbol_type.to_lowercase().into(),
                    file_path: file_path.clone(),
                    rel_path: rel_path.into(),
                    file_name: file_name.clone(),
                    language: language.clone(),
                    start_line: symbol.start_line,
                    end_line: symbol.end_line,
                    analysis_level: "documentation".into(),
                    source_text: symbol.source_text,
                    docstring: symbol.docstring,
                    parameters: "[]".to_owned(),
                    return_type: symbol.return_type,
                    ..NodeData::default()
                },
            );
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::parsers::model::{Range, RelationshipType, Scope, SymbolType};

    fn symbol(name: &str, kind: SymbolType, full_name: Option<&str>, path: &str) -> Symbol {
        let mut symbol = Symbol::new(name, kind, Scope::Global, Range::new(3, 0, 9, 1), path);
        symbol.full_name = full_name.map(str::to_owned);
        symbol
    }

    fn relationship(
        source: &str,
        target: &str,
        kind: RelationshipType,
        path: &str,
    ) -> Relationship {
        let mut relationship = Relationship::new(source, target, kind, path);
        relationship.source_range = Some(Range::new(5, 4, 5, 10));
        relationship
    }

    fn result(
        path: &str,
        symbols: Vec<Symbol>,
        relationships: Vec<Relationship>,
    ) -> (String, ParseResult) {
        let mut parse = ParseResult::new(path, "python");
        parse.symbols = symbols;
        parse.relationships = relationships;
        (path.to_owned(), parse)
    }

    fn ids(graph: &CodeGraph) -> Vec<&str> {
        graph.nodes().map(|(id, _)| id).collect()
    }

    #[test]
    fn node_ids_strip_the_file_stem_and_suffix_collisions() {
        let files = vec![
            result(
                "/r/a/mod.py",
                vec![
                    symbol("C", SymbolType::Class, Some("mod.C"), "/r/a/mod.py"),
                    symbol("m", SymbolType::Method, Some("mod.C.m"), "/r/a/mod.py"),
                    symbol("C", SymbolType::Class, Some("mod.C"), "/r/a/mod.py"),
                    symbol("pkg", SymbolType::Module, None, "/r/a/mod.py"),
                    symbol(" ", SymbolType::Function, None, "/r/a/mod.py"),
                ],
                vec![],
            ),
            result(
                "/r/b/mod.py",
                vec![
                    symbol("C", SymbolType::Class, Some("mod.C"), "/r/b/mod.py"),
                    symbol("C", SymbolType::Class, Some("mod.C"), "/r/b/mod.py"),
                    symbol("x", SymbolType::Variable, Some("other.x"), "/r/b/mod.py"),
                ],
                vec![],
            ),
        ];
        let (graph, _, _) = build_language_graph("python", files, "/r");
        assert_eq!(
            ids(&graph),
            [
                "python::mod::C",
                "python::mod::C.m",
                "python::mod::C__b__mod_py",
                "python::mod::other.x",
            ]
        );
        assert_eq!(
            graph.node("python::mod::C__b__mod_py").unwrap().rel_path,
            "b/mod.py"
        );
    }

    #[test]
    fn relationships_resolve_like_the_python_strategies() {
        let files = vec![
            result(
                "/r/svc.py",
                vec![
                    symbol("Svc", SymbolType::Class, Some("svc.Svc"), "/r/svc.py"),
                    symbol("run", SymbolType::Method, Some("svc.Svc.run"), "/r/svc.py"),
                ],
                vec![
                    // File-level import: source is the stem → `__file__`.
                    relationship("svc", "util.helper", RelationshipType::Imports, "/r/svc.py"),
                    // Dotted source and target, qualified match first.
                    relationship(
                        "Svc.run",
                        "util.helper",
                        RelationshipType::Calls,
                        "/r/svc.py",
                    ),
                    // Unresolvable target: dropped.
                    relationship("Svc.run", "print", RelationshipType::Calls, "/r/svc.py"),
                    // Unknown source: an inferred node.
                    relationship("lambda_1", "Svc", RelationshipType::References, "/r/svc.py"),
                ],
            ),
            result(
                "/r/util.py",
                vec![symbol(
                    "helper",
                    SymbolType::Function,
                    Some("util.helper"),
                    "/r/util.py",
                )],
                vec![],
            ),
        ];
        let (graph, attempted, added) = build_language_graph("python", files, "/r");
        assert_eq!((attempted, added), (4, 3));
        let edges: Vec<(String, String, String)> = graph
            .edges()
            .map(|e| {
                (
                    e.source.to_owned(),
                    e.target.to_owned(),
                    e.data.rel_type.to_string(),
                )
            })
            .collect();
        assert_eq!(
            edges,
            [
                (
                    "python::svc::Svc.run".into(),
                    "python::util::helper".into(),
                    "calls".into()
                ),
                (
                    "python::svc::__file__".into(),
                    "python::util::helper".into(),
                    "imports".into()
                ),
                (
                    "python::svc::lambda_1".into(),
                    "python::svc::Svc".into(),
                    "references".into()
                ),
            ]
        );
        let file_node = graph.node("python::svc::__file__").unwrap();
        assert_eq!(
            (&*file_node.symbol_type, file_node.rel_path.as_str()),
            ("module", "")
        );
        let inferred = graph.node("python::svc::lambda_1").unwrap();
        assert_eq!(inferred.symbol_type, "inferred");
        let call = graph.edges().next().unwrap().data;
        assert_eq!(call.source_file, "svc");
        assert_eq!(call.target_file, "util");
        assert_eq!(
            call.annotations.get("via"),
            Some(&serde_json::json!(["calls@L5"]))
        );
    }

    #[test]
    fn a_target_file_hint_wins_and_definitions_beat_other_types() {
        let files = vec![
            result(
                "/r/a.py",
                vec![symbol("f", SymbolType::Function, Some("a.f"), "/r/a.py")],
                vec![{
                    let mut r = relationship("f", "g", RelationshipType::Calls, "/r/a.py");
                    r.target_file = Some("c.py".into());
                    r
                }],
            ),
            result(
                "/r/b.py",
                vec![symbol("g", SymbolType::Variable, Some("b.g"), "/r/b.py")],
                vec![relationship(
                    "b",
                    "g",
                    RelationshipType::References,
                    "/r/b.py",
                )],
            ),
            result(
                "/r/c.py",
                vec![symbol("g", SymbolType::Function, Some("c.g"), "/r/c.py")],
                vec![relationship(
                    "c",
                    "a.f",
                    RelationshipType::Imports,
                    "/r/c.py",
                )],
            ),
        ];
        let (graph, _, _) = build_language_graph("python", files, "/r");
        let targets: Vec<&str> = graph.edges().map(|e| e.target).collect();
        // a→g with hint c; b's own g (direct match beats the registry);
        // c imports a.f via the two-part module.class rule.
        assert_eq!(targets, ["python::c::g", "python::b::g", "python::a::f"]);
    }

    #[test]
    fn container_defines_are_skipped() {
        let mut defines = relationship("m", "C", RelationshipType::Defines, "/r/m.py");
        defines
            .annotations
            .insert("container_type".into(), Value::String("Module".into()));
        let files = vec![result(
            "/r/m.py",
            vec![symbol("C", SymbolType::Class, Some("m.C"), "/r/m.py")],
            vec![defines],
        )];
        let (graph, attempted, added) = build_language_graph("python", files, "/r");
        assert_eq!((attempted, added, graph.edge_count()), (1, 0, 0));
    }

    #[test]
    fn document_ids_get_counter_suffixes() {
        let mut graph = CodeGraph::new();
        let doc = documents::document_result("/r/README.md", "/r", "# A\nx\n\n## B\ny\n\n# A\nz");
        add_document_nodes(&mut graph, vec![doc]);
        assert_eq!(
            ids(&graph),
            [
                "markdown::README::A",
                "markdown::README::B",
                "markdown::README::A#1"
            ]
        );
        let node = graph.node("markdown::README::A").unwrap();
        assert_eq!(node.analysis_level, "documentation");
        assert_eq!(node.return_type, "markdown");
        assert_eq!(node.rel_path, "README.md");
    }
}
