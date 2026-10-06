//! `_ranked_truncation` with `document_ranker.DocumentRanker`: the
//! documents of an over-budget page, best first, cut at the budget.
//!
//! The live ranker is graph-based, not embedding-based: a score from the
//! page's target locations, the symbol type, the size, and (for a
//! document whose `symbol_name` is a graph node id, and a page name that
//! is one too — in practice never) relationship weights and BFS distance.
//! Tiers come from token budgets 50k / 30k×0.35 / 10k×0.10; a document
//! that fits no tier is dropped.

use super::doc::ContextDoc;
use super::index::PageIndex;
use super::spec::PageSpec;
use crate::errors::{EngineError, ErrorType};
use crate::graph::pystr;
use std::collections::{HashSet, VecDeque};

const TIER_BUDGETS: [f64; 3] = [50_000.0, 30_000.0, 10_000.0];
const COMPRESSION: [f64; 3] = [1.0, 0.35, 0.10];

/// `DocumentRanker.SYMBOL_TYPE_SCORES`, default 1.
fn type_score(symbol_type: &str) -> f64 {
    match symbol_type {
        "class" | "interface" | "struct" | "trait" | "protocol" | "readme" => 10.0,
        "documentation" | "impl" => 9.0,
        "markdown" | "text_chunk" | "config" | "enum" => 8.0,
        "type_alias" | "function" | "constant" => 7.0,
        "method" => 6.0,
        "namespace" | "module" => 5.0,
        "variable" | "macro" => 3.0,
        _ => 1.0,
    }
}

/// `DocumentRanker.RELATIONSHIP_WEIGHTS`.
fn relationship_weight(rel_type: &str) -> f64 {
    match rel_type {
        "defines" | "contains" | "has_member" => 15.0,
        "calls" | "invokes" | "instantiates" | "creates" => 12.0,
        "inheritance" | "extends" | "implements" | "implementation" => 10.0,
        "uses" | "composition" | "aggregation" | "references" => 8.0,
        _ => 0.0,
    }
}

/// `PurePosixPath(path).match(pattern)` (Python 3.12): relative patterns
/// match from the right, one `fnmatch` per component.
fn path_match(path: &str, pattern: &str) -> Result<bool, EngineError> {
    if pattern.is_empty() {
        return Err(EngineError::new(ErrorType::Value, "empty pattern"));
    }
    let parts = |p: &str| -> Vec<String> {
        let absolute = p.starts_with('/');
        let mut out: Vec<String> = p
            .split('/')
            .filter(|s| !s.is_empty() && *s != ".")
            .map(str::to_owned)
            .collect();
        if absolute {
            out.insert(0, "/".to_owned());
        }
        out
    };
    let path_parts = parts(path);
    let pattern_parts = parts(pattern);
    if pattern_parts.is_empty() {
        return Err(EngineError::new(ErrorType::Value, "empty pattern"));
    }
    let absolute = pattern.starts_with('/');
    if (absolute && path_parts.len() != pattern_parts.len())
        || pattern_parts.len() > path_parts.len()
    {
        return Ok(false);
    }
    Ok(path_parts
        .iter()
        .rev()
        .zip(pattern_parts.iter().rev())
        .all(|(part, pat)| pystr::fnmatch(part, pat)))
}

/// `_is_in_target_locations`.
fn in_target_locations(file_path: &str, page: &PageSpec) -> Result<bool, EngineError> {
    if page
        .target_folders
        .iter()
        .any(|f| file_path.contains(f.as_str()))
    {
        return Ok(true);
    }
    for pattern in &page.key_files {
        if file_path.contains(pattern.as_str()) || path_match(file_path, pattern)? {
            return Ok(true);
        }
    }
    Ok(false)
}

/// `nx.shortest_path_length(G, topic, symbol)` over the index edges.
fn distance(index: &PageIndex, from: &str, to: &str) -> Option<usize> {
    let mut seen: HashSet<&str> = HashSet::from([from]);
    let mut queue: VecDeque<(&str, usize)> = VecDeque::from([(from, 0)]);
    while let Some((node, depth)) = queue.pop_front() {
        if node == to {
            return Some(depth);
        }
        for edge in index.edges_from(node) {
            if seen.insert(edge.target_id.as_str()) {
                queue.push_back((edge.target_id.as_str(), depth + 1));
            }
        }
    }
    None
}

/// `_calculate_document_score`.
fn score(index: &PageIndex, doc: &ContextDoc, page: &PageSpec) -> Result<f64, EngineError> {
    let topic = page.page_name.as_str();
    let symbol = doc.symbol_name.as_str();
    let mut total = 0.0;
    if in_target_locations(&doc.file_path, page)? {
        total += 20.0;
    }
    let both_nodes = !topic.is_empty()
        && !symbol.is_empty()
        && index.node(symbol).is_some()
        && index.node(topic).is_some();
    if both_nodes {
        total += index
            .edges_from(symbol)
            .filter(|e| e.target_id == topic)
            .chain(index.edges_from(topic).filter(|e| e.target_id == symbol))
            .map(|e| relationship_weight(e.rel_type()))
            .sum::<f64>();
    }
    total += type_score(&doc.symbol_type.to_lowercase());
    // Proximity compares the topic node's absolute `file_path` with the
    // document's relative one: never equal, never siblings; always 0.
    if both_nodes {
        #[allow(clippy::cast_precision_loss)]
        let penalty = distance(index, topic, symbol).map_or(-5.0, |d| -0.5 * d as f64);
        total += penalty;
    }
    let length = doc.content.chars().count();
    total += if length < 5000 {
        0.0
    } else if length < 10_000 {
        -1.0
    } else {
        -2.0
    };
    Ok(total)
}

/// `_ranked_truncation(docs, page_spec, budget)`.
///
/// # Errors
///
/// An empty `key_files` pattern reaching `Path.match` (Python raised
/// `ValueError` and the page failed), or a broken tokenizer.
pub fn ranked_truncation(
    index: &PageIndex,
    docs: &[ContextDoc],
    page: &PageSpec,
    budget: usize,
) -> Result<Vec<ContextDoc>, EngineError> {
    let mut scored: Vec<(usize, f64)> = Vec::with_capacity(docs.len());
    for (i, doc) in docs.iter().enumerate() {
        scored.push((i, score(index, doc, page)?));
    }
    scored.sort_by(|a, b| b.1.total_cmp(&a.1));
    let mut used = [0.0f64; 3];
    let mut tiered: Vec<(usize, usize, f64)> = Vec::new();
    for (i, s) in scored {
        #[allow(clippy::cast_precision_loss)]
        let tokens = docs[i].content_tokens()? as f64;
        for tier in 0..3 {
            let cost = tokens * COMPRESSION[tier];
            if used[tier] + cost <= TIER_BUDGETS[tier] {
                used[tier] += cost;
                tiered.push((i, tier, s));
                break;
            }
        }
    }
    tiered.sort_by(|a, b| a.1.cmp(&b.1).then(b.2.total_cmp(&a.2)));
    let mut included = Vec::new();
    let mut tokens_used = 0usize;
    for (i, _, _) in tiered {
        let doc_tokens = docs[i].tokens()?;
        if tokens_used + doc_tokens <= budget {
            included.push(docs[i].clone());
            tokens_used += doc_tokens;
        }
    }
    Ok(included)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn path_match_is_right_anchored_fnmatch() {
        assert!(path_match("a/b/c.md", "c.md").unwrap());
        assert!(path_match("a/b/c.md", "b/*.md").unwrap());
        assert!(!path_match("a/b/c.md", "a/*.md").unwrap());
        assert!(!path_match("", "c.md").unwrap());
        assert!(path_match("x", "").is_err());
    }
}
