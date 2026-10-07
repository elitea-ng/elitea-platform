//! Cluster-bounded page context: `cluster_expansion.expand_for_page`,
//! `code_graph/shared_expansion.py` and
//! `wiki_structure_planner/language_heuristics.py`.
//!
//! The live branch only: `feature_flags.smart_expansion` and
//! `language_hints` are hard-coded on, so the generic
//! `_collect_expansion_neighbors` pool (and with it `writer_edge_class_filter`
//! and the `via=` annotations) is never reached and is not ported.
//!
//! Order and budget arithmetic are Python's: the budget is in estimated
//! tokens (`int(chars / 3.5)`), may go negative after an augmentation,
//! and every loop breaks or skips exactly where Python's did.
//!
//! DELIBERATE DIFFERENCE (iteration order): Python walked `list(seen_ids)`
//! — a `set`, in hash-seed order — to find the orphan seeds for framework
//! discovery, so the order of those documents changed from run to run. The
//! port walks the seeds in the order they were added. The parity reference
//! is patched to insertion order the same way.

use super::doc::ContextDoc;
use super::index::{IndexNode, PageIndex};
use super::search::PageSearch;
use crate::errors::EngineError;
use std::collections::HashSet;

/// `MAX_NEIGHBORS_PER_SYMBOL`.
const MAX_NEIGHBORS_PER_SYMBOL: usize = 15;
/// `MAX_EXPANSION_TOTAL`.
const MAX_EXPANSION_TOTAL: usize = 200;
/// `MAX_FRAMEWORK_REFS_PER_ORPHAN`.
const MAX_FRAMEWORK_REFS_PER_ORPHAN: usize = 3;
/// `ORPHAN_EDGE_THRESHOLD`.
const ORPHAN_EDGE_THRESHOLD: usize = 1;
/// `_MIN_FTS_NAME_LEN`.
const MIN_FTS_NAME_LEN: usize = 4;

/// `shared_expansion.EXPANSION_WORTHY_TYPES` (not `constants.EXPANSION_SYMBOL_TYPES`).
const EXPANSION_WORTHY_TYPES: [&str; 11] = [
    "class",
    "interface",
    "struct",
    "enum",
    "trait",
    "function",
    "constant",
    "type_alias",
    "macro",
    "module_doc",
    "file_doc",
];

/// `shared_expansion.SKIP_RELATIONSHIPS`.
const SKIP_RELATIONSHIPS: [&str; 15] = [
    "defines",
    "imports",
    "decorates",
    "contains",
    "annotates",
    "exports",
    "overrides",
    "assigns",
    "returns",
    "parameter",
    "reads",
    "writes",
    "captures",
    "hides",
    "uses_type",
];

/// The `LanguageHints` fields expansion reads.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct LanguageHints {
    extra_expansion_rels: &'static [&'static str],
    skip_symbol_types: &'static [&'static str],
}

const DEFAULT_HINTS: LanguageHints = LanguageHints {
    extra_expansion_rels: &[],
    skip_symbol_types: &[],
};

/// `get_language_hints`.
fn language_hints(language: &str) -> LanguageHints {
    match language.to_lowercase().as_str() {
        "cpp" | "c" => LanguageHints {
            extra_expansion_rels: &["defines_body", "specializes"],
            skip_symbol_types: &["macro"],
        },
        "c_sharp" => LanguageHints {
            extra_expansion_rels: &["implementation"],
            skip_symbol_types: &[],
        },
        "go" => LanguageHints {
            extra_expansion_rels: &["defines_body"],
            skip_symbol_types: &[],
        },
        "java" => LanguageHints {
            extra_expansion_rels: &["implementation", "annotates"],
            skip_symbol_types: &[],
        },
        "javascript" => LanguageHints {
            extra_expansion_rels: &["exports"],
            skip_symbol_types: &["constant"],
        },
        "python" => LanguageHints {
            extra_expansion_rels: &["decorates"],
            skip_symbol_types: &[],
        },
        "rust" => LanguageHints {
            extra_expansion_rels: &["implementation", "defines_body"],
            skip_symbol_types: &[],
        },
        "typescript" => LanguageHints {
            extra_expansion_rels: &["exports"],
            skip_symbol_types: &[],
        },
        _ => DEFAULT_HINTS,
    }
}

/// The feature flags expansion reads (`DEEPWIKI_EXCLUDE_TESTS`).
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct ExpansionFlags {
    pub exclude_tests: bool,
}

impl ExpansionFlags {
    /// `DEEPWIKI_EXCLUDE_TESTS` from the environment, strictly parsed as
    /// the engine's other flags are (`1`/`true`/`yes`, `0`/`false`/`no`,
    /// empty or unset for off).
    ///
    /// # Errors
    ///
    /// Any other value.
    pub fn from_env() -> Result<Self, EngineError> {
        let raw = std::env::var("DEEPWIKI_EXCLUDE_TESTS").unwrap_or_default();
        let exclude_tests = match raw.trim().to_lowercase().as_str() {
            "" | "0" | "false" | "no" => false,
            "1" | "true" | "yes" => true,
            _ => {
                return Err(EngineError::new(
                    crate::errors::ErrorType::Value,
                    format!(
                        "DEEPWIKI_EXCLUDE_TESTS must be one of 1/true/yes/0/false/no, got '{raw}'"
                    ),
                ));
            }
        };
        Ok(Self { exclude_tests })
    }
}

/// What the page asks expansion for.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ExpansionRequest<'a> {
    pub page_symbols: &'a [String],
    pub macro_id: Option<i64>,
    pub micro_id: Option<i64>,
    /// `PageSpec.metadata["cluster_node_ids"]`; empty means none.
    pub cluster_node_ids: &'a [String],
    pub token_budget: i64,
}

/// `_estimate_tokens`: `max(1, int(len(text) / 3.5))`, 0 for "".
fn estimate_tokens(text: &str) -> i64 {
    if text.is_empty() {
        return 0;
    }
    #[allow(clippy::cast_precision_loss, clippy::cast_possible_truncation)]
    let estimate = (text.chars().count() as f64 / 3.5) as i64;
    estimate.max(1)
}

/// `_node_to_document`.
fn node_document(node: &IndexNode, is_initial: bool) -> ContextDoc {
    ContextDoc {
        content: node.text().to_owned(),
        source: node.rel_path.clone(),
        rel_path: node.rel_path.clone(),
        file_path: String::new(),
        symbol_name: node.symbol_name.clone(),
        symbol_type: node.symbol_type.clone(),
        start_line: node.start_line.unwrap_or(0),
        end_line: node.end_line.unwrap_or(0),
        is_documentation: node.is_doc,
        expansion_via: String::new(),
        is_initial,
        node_id: node.node_id.clone(),
        language: node.language.clone().unwrap_or_default(),
    }
}

/// `_augment_document`: append cross-file implementation bodies (C/C++
/// `defines_body`, Go / Rust methods in other files). Returns the extra
/// estimated tokens.
fn augment_document(index: &PageIndex, doc: &mut ContextDoc) -> i64 {
    let language = doc.language.to_lowercase();
    if doc.node_id.is_empty() {
        return 0;
    }
    let symbol_type = doc.symbol_type.to_lowercase();
    let decl_file = if doc.rel_path.is_empty() {
        doc.source.clone()
    } else {
        doc.rel_path.clone()
    };
    let parts = match language.as_str() {
        "cpp" | "c" => augment_cpp(index, &doc.node_id, &symbol_type, &decl_file),
        "go" | "rust" => augment_go_rust(index, &doc.node_id, &symbol_type, &decl_file),
        _ => return 0,
    };
    if parts.is_empty() {
        return 0;
    }
    let extra = parts.join("\n\n");
    doc.content = format!("{}\n\n{extra}", doc.content);
    estimate_tokens(&extra)
}

/// The `defines_body` implementations of `node_id` in other files.
fn implementations<'i>(
    index: &'i PageIndex,
    node_id: &str,
    decl_file: &str,
) -> Vec<(&'i str, &'i str)> {
    index
        .edges_to(node_id)
        .filter(|e| e.rel_type() == "defines_body")
        .filter_map(|e| index.node(&e.source_id))
        .map(|n| (n.text(), n.rel_path.as_str()))
        .filter(|(text, file)| !crate::graph::pystr::strip(text).is_empty() && *file != decl_file)
        .collect()
}

fn plural(count: usize) -> &'static str {
    if count == 1 { "" } else { "s" }
}

/// `_augment_cpp`.
fn augment_cpp(
    index: &PageIndex,
    node_id: &str,
    symbol_type: &str,
    decl_file: &str,
) -> Vec<String> {
    match symbol_type {
        "function" | "method" | "constructor" => implementations(index, node_id, decl_file)
            .into_iter()
            .map(|(text, file)| format!("/* Implementation from {file} */\n{text}"))
            .collect(),
        "class" | "struct" => {
            let mut by_file: Vec<(String, Vec<String>)> = Vec::new();
            let methods = index
                .edges_from(node_id)
                .filter(|e| e.rel_type() == "defines")
                .filter(|e| {
                    index.node(&e.target_id).is_some_and(|n| {
                        ["method", "constructor", "function"].contains(&n.symbol_type.as_str())
                    })
                });
            for method in methods {
                for (text, file) in implementations(index, &method.target_id, decl_file) {
                    match by_file.iter_mut().find(|(f, _)| f == file) {
                        Some(entry) => entry.1.push(text.to_owned()),
                        None => by_file.push((file.to_owned(), vec![text.to_owned()])),
                    }
                }
            }
            by_file.sort_by(|a, b| a.0.cmp(&b.0));
            by_file
                .into_iter()
                .map(|(file, impls)| {
                    format!(
                        "/* Implementations from {file} ({} method{}) */\n{}",
                        impls.len(),
                        plural(impls.len()),
                        impls.join("\n\n")
                    )
                })
                .collect()
        }
        _ => Vec::new(),
    }
}

/// `_augment_go_rust`.
fn augment_go_rust(
    index: &PageIndex,
    node_id: &str,
    symbol_type: &str,
    type_file: &str,
) -> Vec<String> {
    if !["struct", "class", "enum", "trait"].contains(&symbol_type) {
        return Vec::new();
    }
    let mut by_file: Vec<(String, Vec<String>)> = Vec::new();
    for edge in index
        .edges_from(node_id)
        .filter(|e| e.rel_type() == "defines")
    {
        let Some(method) = index.node(&edge.target_id) else {
            continue;
        };
        if !["method", "function", "constructor"].contains(&method.symbol_type.as_str())
            || method.rel_path == type_file
        {
            continue;
        }
        let text = method.text();
        if crate::graph::pystr::strip(text).is_empty() || method.rel_path.is_empty() {
            continue;
        }
        match by_file.iter_mut().find(|(f, _)| *f == method.rel_path) {
            Some(entry) => entry.1.push(text.to_owned()),
            None => by_file.push((method.rel_path.clone(), vec![text.to_owned()])),
        }
    }
    by_file.sort_by(|a, b| a.0.cmp(&b.0));
    by_file
        .into_iter()
        .map(|(file, texts)| {
            format!(
                "// Methods from {file} ({} method{})\n{}",
                texts.len(),
                plural(texts.len()),
                texts.join("\n\n")
            )
        })
        .collect()
}

/// The page boundary of smart expansion.
struct Boundary<'a> {
    page_ids: Option<HashSet<&'a str>>,
    macro_id: Option<i64>,
    exclude_tests: bool,
}

impl Boundary<'_> {
    /// `_is_valid_expansion`.
    fn admits(&self, node: &IndexNode) -> bool {
        if !node.is_architectural {
            return false;
        }
        if node.is_test && self.exclude_tests {
            return false;
        }
        if !EXPANSION_WORTHY_TYPES.contains(&node.symbol_type.to_lowercase().as_str()) {
            return false;
        }
        if let Some(ids) = &self.page_ids {
            return ids.contains(node.node_id.as_str());
        }
        if let Some(macro_id) = self.macro_id {
            return node.macro_cluster == Some(macro_id);
        }
        true
    }
}

/// One expansion candidate: `(node id, reason, weight)`.
type Candidate = (String, String, f64);

#[derive(Clone, Copy, PartialEq, Eq)]
enum Direction {
    Out,
    In,
}

/// `_collect_neighbors`.
#[allow(clippy::too_many_arguments)]
fn collect_neighbors(
    index: &PageIndex,
    node_id: &str,
    rel_types: &[&str],
    direction: Direction,
    seen: &HashSet<String>,
    boundary: &Boundary<'_>,
    limit: usize,
    prefix: &str,
) -> Vec<Candidate> {
    let edges: Box<dyn Iterator<Item = (&str, String, f64)>> = match direction {
        Direction::Out => Box::new(index.edges_from(node_id).map(|e| {
            (
                e.target_id.as_str(),
                e.rel_type().to_lowercase(),
                e.weight_or_one(),
            )
        })),
        Direction::In => Box::new(index.edges_to(node_id).map(|e| {
            (
                e.source_id.as_str(),
                e.rel_type().to_lowercase(),
                e.weight_or_one(),
            )
        })),
    };
    let mut result = Vec::new();
    for (nid, rtype, weight) in edges {
        if !rel_types.contains(&rtype.as_str()) {
            continue;
        }
        if seen.contains(nid) || result.len() >= limit {
            continue;
        }
        let Some(node) = index.node(nid) else {
            continue;
        };
        if !boundary.admits(node) {
            continue;
        }
        result.push((nid.to_owned(), format!("{prefix}{rtype}"), weight));
    }
    result
}

/// `resolve_alias_chain_db`.
fn resolve_alias_chain(index: &PageIndex, node_id: &str) -> Option<String> {
    let mut visited: HashSet<String> = HashSet::from([node_id.to_owned()]);
    let mut current = node_id.to_owned();
    for _ in 0..5 {
        let Some(edge) = index
            .edges_from(&current)
            .find(|e| e.rel_type() == "alias_of")
        else {
            break;
        };
        let target = edge.target_id.clone();
        if visited.contains(&target) {
            break;
        }
        visited.insert(target.clone());
        let Some(node) = index.node(&target) else {
            break;
        };
        if node.symbol_type.to_lowercase() == "type_alias" {
            current = target;
        } else {
            return Some(target);
        }
    }
    (current != node_id).then_some(current)
}

/// `expand_symbol_smart`.
fn expand_symbol_smart(
    index: &PageIndex,
    node_id: &str,
    symbol_type: &str,
    seen: &HashSet<String>,
    boundary: &Boundary<'_>,
    extra_rel_types: &[&str],
) -> Vec<(String, String)> {
    use Direction::{In, Out};
    let n = |rels: &[&str], dir, limit, prefix| {
        collect_neighbors(index, node_id, rels, dir, seen, boundary, limit, prefix)
    };
    let mut candidates: Vec<Candidate> = Vec::new();
    match symbol_type.to_lowercase().as_str() {
        "class" | "interface" | "struct" | "enum" | "trait" => {
            candidates.extend(n(&["inheritance", "implementation"], Out, 3, "base:"));
            candidates.extend(n(&["inheritance", "implementation"], In, 3, "derived:"));
            candidates.extend(n(&["creates", "instantiates"], Out, 2, "creates:"));
            candidates.extend(n(&["composition", "aggregation"], Out, 2, "composes:"));
            candidates.extend(n(&["composition", "aggregation"], In, 2, "composed_by:"));
            candidates.extend(n(&["references"], Out, 2, "refs:"));
        }
        "function" => {
            candidates.extend(n(&["calls"], Out, 3, "calls:"));
            candidates.extend(n(&["calls"], In, 3, "called_by:"));
            candidates.extend(n(&["creates", "instantiates"], Out, 2, "creates:"));
            candidates.extend(n(&["references", "composition"], Out, 2, "refs:"));
        }
        "constant" => {
            candidates.extend(n(&["references"], In, 3, "referenced_by:"));
            candidates.extend(n(&["alias_of", "references"], Out, 2, "type:"));
        }
        "type_alias" => {
            if let Some(concrete) = resolve_alias_chain(index, node_id)
                && concrete != node_id
                && !seen.contains(&concrete)
                && index.node(&concrete).is_some_and(|c| boundary.admits(c))
            {
                candidates.push((concrete, "alias_resolves_to".to_owned(), 5.0));
            }
            candidates.extend(n(&["references"], In, 3, "used_by:"));
            candidates.extend(n(&["references", "composition"], Out, 2, "refs:"));
        }
        "macro" => {
            candidates.extend(n(&["references", "calls"], In, 3, "used_by:"));
            candidates.extend(n(&["references"], Out, 2, "refs:"));
        }
        _ => {
            for edge in index.edges_from(node_id) {
                let rtype = edge.rel_type().to_lowercase();
                if SKIP_RELATIONSHIPS.contains(&rtype.as_str()) || seen.contains(&edge.target_id) {
                    continue;
                }
                if let Some(node) = index.node(&edge.target_id)
                    && boundary.admits(node)
                {
                    candidates.push((edge.target_id.clone(), rtype, edge.weight_or_one()));
                }
                if candidates.len() >= 5 {
                    break;
                }
            }
        }
    }
    if !extra_rel_types.is_empty() {
        candidates.extend(n(extra_rel_types, Out, 5, "lang_hint:"));
        candidates.extend(n(extra_rel_types, In, 5, "lang_hint:"));
    }
    // `sorted(candidates, key=lambda x: -x[3])`: stable, highest first.
    candidates.sort_by(|a, b| b.2.total_cmp(&a.2));
    let mut result = Vec::new();
    let mut taken: HashSet<String> = HashSet::new();
    for (nid, reason, _) in candidates {
        if taken.contains(&nid) || seen.contains(&nid) {
            continue;
        }
        taken.insert(nid.clone());
        result.push((nid, reason));
        if result.len() >= MAX_NEIGHBORS_PER_SYMBOL {
            break;
        }
    }
    result
}

/// `_count_structural_edges`.
fn structural_edges(index: &PageIndex, node_id: &str) -> usize {
    let structural = |class: &Option<String>| class.as_deref() == Some("structural");
    index
        .edges_from(node_id)
        .filter(|e| structural(&e.edge_class))
        .count()
        + index
            .edges_to(node_id)
            .filter(|e| structural(&e.edge_class))
            .count()
}

/// `_collect_search_terms`.
fn search_terms(index: &PageIndex, orphan: &IndexNode) -> Vec<String> {
    let name = crate::graph::pystr::strip(&orphan.symbol_name);
    let mut terms = Vec::new();
    if name.chars().count() >= MIN_FTS_NAME_LEN {
        terms.push(name.to_owned());
    }
    if ["class", "interface", "struct"].contains(&orphan.symbol_type.to_lowercase().as_str()) {
        let children: Vec<String> = index
            .edges_from(&orphan.node_id)
            .filter(|e| {
                e.rel_type.as_deref() == Some("defines")
                    && e.edge_class.as_deref() == Some("structural")
            })
            .map(|e| e.target_id.clone())
            .collect();
        for child in index.nodes_by_ids(&children) {
            let child_name = crate::graph::pystr::strip(&child.symbol_name);
            if child_name.chars().count() >= MIN_FTS_NAME_LEN
                && !terms.iter().any(|t| t == child_name)
            {
                terms.push(child_name.to_owned());
            }
        }
    }
    terms
}

/// `_find_framework_references`: `(hit id, reason)` per hit.
fn framework_references<S: PageSearch>(
    index: &PageIndex,
    search: &S,
    orphans: &[&IndexNode],
    seen: &HashSet<String>,
    macro_id: Option<i64>,
) -> Result<Vec<String>, EngineError> {
    let mut results = Vec::new();
    let mut found: HashSet<String> = HashSet::new();
    let orphan_ids: HashSet<&str> = orphans.iter().map(|o| o.node_id.as_str()).collect();
    let limit = MAX_FRAMEWORK_REFS_PER_ORPHAN;
    for orphan in orphans {
        let terms = search_terms(index, orphan);
        if terms.is_empty() {
            continue;
        }
        let scopes: Vec<Option<i64>> = match macro_id {
            Some(m) => vec![None, Some(m)],
            None => vec![None],
        };
        let mut hits: Vec<String> = Vec::new();
        'terms: for term in &terms {
            if hits.len() >= limit {
                break;
            }
            let term_lower = term.to_lowercase();
            for scope in &scopes {
                if hits.len() >= limit {
                    break;
                }
                for hit_id in search.search(term, *scope, limit * 3)? {
                    let Some(node) = index.node(&hit_id) else {
                        continue;
                    };
                    if hit_id == orphan.node_id
                        || seen.contains(&hit_id)
                        || found.contains(&hit_id)
                        || orphan_ids.contains(hit_id.as_str())
                    {
                        continue;
                    }
                    if node.symbol_name.to_lowercase() == term_lower {
                        continue;
                    }
                    if !node.text().to_lowercase().contains(&term_lower) {
                        continue;
                    }
                    found.insert(hit_id.clone());
                    hits.push(hit_id);
                    if hits.len() >= limit {
                        continue 'terms;
                    }
                }
            }
        }
        results.extend(hits);
    }
    Ok(results)
}

/// `_resolve_symbols`: names to nodes, in name order, first occurrence
/// kept.
fn resolve_symbols<'i, S: PageSearch>(
    index: &'i PageIndex,
    search: &S,
    names: &[String],
    macro_id: Option<i64>,
) -> Result<Vec<&'i IndexNode>, EngineError> {
    let mut result: Vec<&IndexNode> = Vec::new();
    let mut ids: HashSet<&str> = HashSet::new();
    for name in names {
        if name.is_empty() {
            continue;
        }
        let mut rows = index.resolve_exact(name, macro_id);
        if rows.is_empty() {
            rows = search
                .symbol(name, macro_id)?
                .iter()
                .filter_map(|id| index.node(id))
                .collect();
        }
        for row in rows {
            if !row.node_id.is_empty() && ids.insert(row.node_id.as_str()) {
                result.push(row);
            }
        }
    }
    Ok(result)
}

/// `expand_for_page`: the page's documents, initial symbols first, then
/// framework references, smart-expansion neighbours and cluster docs.
///
/// # Errors
///
/// A search failure (the replay without a recording).
#[allow(clippy::too_many_lines)]
pub fn expand_for_page<S: PageSearch>(
    index: &PageIndex,
    search: &S,
    request: &ExpansionRequest<'_>,
    flags: ExpansionFlags,
) -> Result<Vec<ContextDoc>, EngineError> {
    if request.page_symbols.is_empty() {
        return Ok(Vec::new());
    }
    let mut budget = request.token_budget;
    let mut seen: HashSet<String> = HashSet::new();
    let mut seen_order: Vec<String> = Vec::new();
    let mut docs: Vec<ContextDoc> = Vec::new();

    let mut matched = resolve_symbols(index, search, request.page_symbols, request.macro_id)?;
    if flags.exclude_tests {
        matched.retain(|n| !n.is_test);
    }
    if matched.is_empty() {
        return Ok(Vec::new());
    }

    // Step 2: the initial symbols.
    for node in &matched {
        let doc = node_document(node, true);
        let cost = estimate_tokens(&doc.content);
        if cost > budget {
            break;
        }
        docs.push(doc);
        seen.insert(node.node_id.clone());
        seen_order.push(node.node_id.clone());
        budget -= cost;
    }

    // Step 2.5: cross-file implementations of the initial symbols.
    for doc in &mut docs {
        budget -= augment_document(index, doc);
    }

    // Step 2.75: framework references for orphans.
    if budget > 0 {
        let matched_ids: HashSet<&str> = matched.iter().map(|n| n.node_id.as_str()).collect();
        let orphans: Vec<&IndexNode> = seen_order
            .iter()
            .filter(|id| matched_ids.contains(id.as_str()))
            .filter(|id| structural_edges(index, id) <= ORPHAN_EDGE_THRESHOLD)
            .filter_map(|id| index.node(id))
            .collect();
        if !orphans.is_empty() {
            for hit_id in framework_references(index, search, &orphans, &seen, request.macro_id)? {
                if budget <= 0 || docs.len() >= MAX_EXPANSION_TOTAL {
                    break;
                }
                if seen.contains(&hit_id) {
                    continue;
                }
                let Some(node) = index.node(&hit_id) else {
                    continue;
                };
                let doc = node_document(node, false);
                let cost = estimate_tokens(&doc.content);
                if cost > budget {
                    continue;
                }
                docs.push(doc);
                seen.insert(hit_id.clone());
                seen_order.push(hit_id);
                budget -= cost;
            }
        }
    }

    // Step 3: smart expansion inside the page boundary.
    let boundary = Boundary {
        page_ids: (!request.cluster_node_ids.is_empty()).then(|| {
            request
                .cluster_node_ids
                .iter()
                .map(String::as_str)
                .collect()
        }),
        macro_id: request.macro_id,
        exclude_tests: flags.exclude_tests,
    };
    let seeds_for_language: Vec<String> =
        matched.iter().take(50).map(|n| n.node_id.clone()).collect();
    let hints = index
        .dominant_language(&seeds_for_language)
        .filter(|l| !l.is_empty())
        .map(|l| language_hints(&l));
    let extra_rels: &[&str] = hints.map_or(&[], |h| h.extra_expansion_rels);
    for seed in &matched {
        if budget <= 0 || docs.len() >= MAX_EXPANSION_TOTAL {
            break;
        }
        let neighbours = expand_symbol_smart(
            index,
            &seed.node_id,
            &seed.symbol_type,
            &seen,
            &boundary,
            extra_rels,
        );
        for (nid, _reason) in neighbours {
            if budget <= 0 || docs.len() >= MAX_EXPANSION_TOTAL {
                break;
            }
            if seen.contains(&nid) {
                continue;
            }
            let Some(node) = index.node(&nid) else {
                continue;
            };
            if let Some(h) = hints
                && h.skip_symbol_types
                    .contains(&node.symbol_type.to_lowercase().as_str())
            {
                continue;
            }
            let mut doc = node_document(node, false);
            augment_document(index, &mut doc);
            let cost = estimate_tokens(&doc.content);
            if cost > budget {
                continue;
            }
            docs.push(doc);
            seen.insert(nid.clone());
            seen_order.push(nid);
            budget -= cost;
        }
    }

    // Step 4: the cluster's documentation, and docs next to a seed.
    if let Some(macro_id) = request.macro_id
        && budget > 0
    {
        let seeds: Vec<String> = matched
            .iter()
            .take(500)
            .map(|n| n.node_id.clone())
            .collect();
        let mut taken: HashSet<&str> = HashSet::new();
        let candidates: Vec<&IndexNode> = index
            .cluster_docs(macro_id, request.micro_id)
            .into_iter()
            .chain(index.adjacent_docs(&seeds))
            .filter(|n| {
                !(n.node_id.is_empty()
                    || seen.contains(&n.node_id)
                    || flags.exclude_tests && n.is_test)
                    && taken.insert(n.node_id.as_str())
            })
            .collect();
        for node in candidates {
            if budget <= 0 || docs.len() >= MAX_EXPANSION_TOTAL {
                break;
            }
            let doc = node_document(node, false);
            let cost = estimate_tokens(&doc.content);
            if cost > budget {
                continue;
            }
            docs.push(doc);
            seen.insert(node.node_id.clone());
            budget -= cost;
        }
    }
    Ok(docs)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn token_estimate_is_chars_over_three_and_a_half() {
        assert_eq!(estimate_tokens(""), 0);
        assert_eq!(estimate_tokens("ab"), 1);
        assert_eq!(estimate_tokens(&"x".repeat(7)), 2);
        assert_eq!(estimate_tokens(&"é".repeat(7)), 2);
    }
}
