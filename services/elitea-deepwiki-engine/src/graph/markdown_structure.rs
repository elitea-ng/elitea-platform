//! Phase 1c: markdown document structure (`markdown_structure.py`).
//!
//! The markdown chunker emits one `markdown_section` node per heading and
//! no edges between them. This pass adds:
//!
//! * `contains` — from the file's `markdown_document` node (the first one
//!   with that `rel_path`, or a synthesized `markdown_document::<rel_path>`
//!   node) to each of its sections;
//! * `references` — from every markdown node to the nodes its markdown
//!   links (`[x](path)`, resolved against the file's directory) and
//!   backtick names (`` `Name` ``, at most two nodes per name) point at.
//!
//! The reference resolution is `graph_orphan_cascade_v2`'s, reused verbatim
//! by Python. Its indexes are built AFTER the synthesized documents and
//! the contract nodes exist, and the path index keeps the LAST node per
//! `rel_path` — so a link to a source file resolves to the last node of
//! that file in node order, often a contract node. Kept: it is what the
//! Python index stores.

use super::pystr;
use super::{CodeGraph, EdgeData, NodeData};
use regex::Regex;
use serde_json::{Map, Value};
use std::collections::{HashMap, HashSet};
use std::sync::LazyLock;

const SECTION_TYPE: &str = "markdown_section";
const DOCUMENT_TYPE: &str = "markdown_document";
const SYNTHETIC_DOC_PREFIX: &str = "markdown_document::";

static MD_LINK: LazyLock<Regex> =
    LazyLock::new(|| super::pyre::compile(r"\[(?:[^\]]*)\]\(([^)]+)\)", ""));
static BACKTICK_REF: LazyLock<Regex> =
    LazyLock::new(|| super::pyre::compile(r"`([A-Za-z_]\w+(?:\.\w+)*)`", ""));

/// What [`wire_markdown_structure`] did.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct MarkdownStats {
    pub markdown_nodes: usize,
    pub documents_synthesized: usize,
    pub contains_edges: usize,
    pub references_edges: usize,
}

/// `graph_orphan_cascade_v2._is_doc_node`.
fn is_doc_node(data: &NodeData) -> bool {
    if matches!(
        data.symbol_type.to_lowercase().as_str(),
        "file_doc" | "module_doc" | "doc"
    ) {
        return true;
    }
    let rel_path = data.rel_path.to_lowercase();
    [".md", ".rst", ".txt"]
        .iter()
        .any(|suffix| rel_path.ends_with(suffix))
}

/// `_build_path_index`: `rel_path` → the last node with it.
fn build_path_index(graph: &CodeGraph) -> HashMap<String, String> {
    let mut index = HashMap::new();
    for (id, data) in graph.nodes() {
        if !data.rel_path.is_empty() {
            index.insert(data.rel_path.to_string(), id.to_owned());
        }
    }
    index
}

/// `_build_simple_name_index`: `symbol_name` → non-document nodes.
fn build_simple_name_index(graph: &CodeGraph) -> HashMap<String, Vec<String>> {
    let mut index: HashMap<String, Vec<String>> = HashMap::new();
    for (id, data) in graph.nodes() {
        if is_doc_node(data) || data.symbol_name.is_empty() {
            continue;
        }
        index
            .entry(data.symbol_name.clone())
            .or_default()
            .push(id.to_owned());
    }
    index
}

/// `_normalize_path`: a link target relative to the linking file's
/// directory, or `""` for an external or anchor-only link.
fn normalize_link(reference: &str, source_dir: &str) -> String {
    if ["http://", "https://", "mailto:", "#"]
        .iter()
        .any(|p| reference.starts_with(p))
    {
        return String::new();
    }
    let reference = reference.split('#').next().unwrap_or("");
    if reference.is_empty() {
        return String::new();
    }
    let reference = reference.strip_prefix("./").unwrap_or(reference);
    pystr::normpath(&pystr::join(source_dir, reference)).replace('\\', "/")
}

/// A resolved reference: target node and matcher.
struct Hit {
    node_id: String,
    matcher: &'static str,
}

/// `_resolve_doc_orphan_links`: markdown links first, then backtick names.
fn resolve_doc_links(
    node_id: &str,
    source_text: &str,
    source_dir: &str,
    path_index: &HashMap<String, String>,
    name_index: &HashMap<String, Vec<String>>,
) -> Vec<Hit> {
    let mut out = Vec::new();
    let mut seen: HashSet<String> = HashSet::new();
    for m in MD_LINK.captures_iter(source_text) {
        let raw = m.get(1).map_or("", |g| g.as_str());
        let reference = pystr::strip(raw.split('#').next().unwrap_or(""));
        if reference.is_empty() {
            continue;
        }
        let resolved = normalize_link(reference, source_dir);
        if resolved.is_empty() {
            continue;
        }
        let Some(target) = path_index.get(&resolved) else {
            continue;
        };
        if target == node_id || !seen.insert(target.clone()) {
            continue;
        }
        out.push(Hit {
            node_id: target.clone(),
            matcher: "md_link",
        });
    }
    for m in BACKTICK_REF.captures_iter(source_text) {
        let symbol = m.get(1).map_or("", |g| g.as_str());
        let mut targets = name_index.get(symbol).map_or(&[][..], Vec::as_slice);
        if targets.is_empty() && symbol.contains('.') {
            let last = symbol.rsplit('.').next().unwrap_or("");
            targets = name_index.get(last).map_or(&[][..], Vec::as_slice);
        }
        for target in targets.iter().take(2) {
            if target == node_id || !seen.insert(target.clone()) {
                continue;
            }
            out.push(Hit {
                node_id: target.clone(),
                matcher: "backtick",
            });
        }
    }
    out
}

fn doc_edge(rel_type: &str, raw_similarity: Option<f64>, matcher: Option<&str>) -> EdgeData {
    let mut annotations = Map::new();
    annotations.insert(
        "confidence".to_owned(),
        Value::String("EXTRACTED".to_owned()),
    );
    if let Some(matcher) = matcher {
        annotations.insert("matcher".to_owned(), Value::String(matcher.to_owned()));
    }
    EdgeData {
        rel_type: rel_type.to_owned().into(),
        edge_class: "doc".into(),
        weight: 1.0,
        raw_similarity,
        language: "markdown".to_owned(),
        created_by: "markdown_structure".into(),
        annotations: annotations.into(),
        ..EdgeData::default()
    }
}

/// The synthesized parent document of `rel_path`.
fn synthetic_document(rel_path: &str) -> NodeData {
    let file_name = pystr::basename(rel_path);
    let name = if file_name.is_empty() {
        rel_path
    } else {
        file_name
    };
    let mut extra = Map::new();
    extra.insert("name".to_owned(), Value::String(name.to_owned()));
    NodeData {
        symbol_name: name.to_owned(),
        symbol_type: DOCUMENT_TYPE.into(),
        rel_path: rel_path.into(),
        file_name: file_name.into(),
        language: "markdown".into(),
        analysis_level: "documentation".into(),
        parameters: "[]".to_owned(),
        return_type: "markdown".to_owned(),
        extra: extra.into(),
        ..NodeData::default()
    }
}

/// `wire_markdown_structure` (the `markdown_structure` flag is checked by
/// the caller).
pub fn wire_markdown_structure(graph: &mut CodeGraph) -> MarkdownStats {
    let mut stats = MarkdownStats::default();
    // (id, symbol type, rel_path, source_text), captured before any
    // document is synthesized: the references pass skips those.
    let markdown: Vec<(String, String, String, String)> = graph
        .nodes()
        .filter_map(|(id, data)| {
            let symbol_type = data.symbol_type.to_lowercase();
            (symbol_type == SECTION_TYPE || symbol_type == DOCUMENT_TYPE).then(|| {
                (
                    id.to_owned(),
                    symbol_type,
                    data.rel_path.to_string(),
                    data.source_text.clone(),
                )
            })
        })
        .collect();
    if markdown.is_empty() {
        return stats;
    }
    stats.markdown_nodes = markdown.len();

    let mut sections_by_path: indexmap::IndexMap<&str, Vec<&str>> = indexmap::IndexMap::new();
    let mut document_by_path: HashMap<String, String> = HashMap::new();
    for (id, symbol_type, rel_path, _) in &markdown {
        if symbol_type == DOCUMENT_TYPE {
            document_by_path
                .entry(rel_path.clone())
                .or_insert_with(|| id.clone());
        } else {
            sections_by_path.entry(rel_path).or_default().push(id);
        }
    }
    for (rel_path, sections) in &sections_by_path {
        if rel_path.is_empty() {
            continue;
        }
        let parent = if let Some(parent) = document_by_path.get(*rel_path) {
            parent.clone()
        } else {
            let parent = format!("{SYNTHETIC_DOC_PREFIX}{rel_path}");
            if !graph.has_node(&parent) {
                graph.add_node(&parent, synthetic_document(rel_path));
                stats.documents_synthesized += 1;
            }
            document_by_path.insert((*rel_path).to_owned(), parent.clone());
            parent
        };
        for &section in sections {
            if section == parent || graph.has_edge_of_type(&parent, section, "contains") {
                continue;
            }
            graph.add_edge(&parent, section, doc_edge("contains", None, None));
            stats.contains_edges += 1;
        }
    }

    let path_index = build_path_index(graph);
    let name_index = build_simple_name_index(graph);
    for (id, _, rel_path, source_text) in &markdown {
        if source_text.is_empty() {
            continue;
        }
        let source_dir = pystr::dirname(rel_path);
        for hit in resolve_doc_links(id, source_text, source_dir, &path_index, &name_index) {
            if hit.node_id == *id
                || !graph.has_node(&hit.node_id)
                || graph.has_edge_of_type(id, &hit.node_id, "references")
            {
                continue;
            }
            graph.add_edge(
                id,
                &hit.node_id,
                doc_edge("references", Some(0.95), Some(hit.matcher)),
            );
            stats.references_edges += 1;
        }
    }
    stats
}

#[cfg(test)]
mod tests;
