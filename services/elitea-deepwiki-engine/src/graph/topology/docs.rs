//! Doc edge injection: Tier 1 hyperlinks and Tier 2 directory proximity
//! (`graph_topology.inject_doc_edges`).
//!
//! This pass has its OWN notion of a documentation node
//! ([`is_topology_doc_node`]), not the explicit-reference pass's: an
//! `is_doc` attribute equal to 1, or a `symbol_type` (as the graph holds
//! it, not lower-cased) in a fixed list or ending in `_document` /
//! `_section`. A `.md` file parsed as code is a doc for Pass 1 and not
//! here.
//!
//! Proximity, as Python does it:
//!
//! * a doc under `docs/X` (or `doc/`, `documentation/`) links to the first
//!   five code nodes of every directory equal to `X` or ending in `/X`;
//! * a doc at the repository root links to one code node per top-level
//!   directory (at most 20), picked by `md5(doc id)` so that different
//!   root docs pick different anchors.

use super::digest::md5_prefix_u64;
use super::{Ctx, StoreError, synthetic_edge};
use crate::graph::markdown_structure::{BACKTICK_REF, MD_LINK};
use crate::graph::{CodeGraph, NodeData, constants, pystr};
use serde_json::{Map, Value, json};
use std::collections::{HashMap, HashSet};

/// `_DOC_KINDS`.
const DOC_KINDS: &[&str] = &[
    "module_doc",
    "file_doc",
    "doc",
    "readme",
    "documentation",
    "markdown",
    "rst",
    "text",
];

/// `_DOC_PREFIXES`.
const DOC_PREFIXES: &[&str] = &["docs/", "doc/", "documentation/"];

/// `_ROOT_DOC_TOP_LEVEL_CAP`.
const ROOT_DOC_TOP_LEVEL_CAP: usize = 20;

/// Whether the Python node lacks one of `rel_path`, `source_text`,
/// `symbol_type` — the nodes `_enrich_graph_nodes_from_db` reads from the
/// index. In Python graphs those are the nodes a relationship created
/// before (or without) its symbol: no `rel_path`, no parser symbol.
#[must_use]
pub fn lacks_index_attributes(node: &NodeData) -> bool {
    node.rel_path.is_empty() && node.symbol.is_none()
}

/// `_is_doc_node` of `graph_topology`.
#[must_use]
pub fn is_topology_doc_node(node: &NodeData) -> bool {
    // `data.get("is_doc") == 1`: True and 1 qualify, False and 0 do not.
    let flagged = match node.extra.get("is_doc") {
        Some(Value::Bool(flag)) => *flag,
        Some(Value::Number(number)) => number.as_f64() == Some(1.0),
        _ => false,
    };
    if flagged {
        return true;
    }
    // `data.get("symbol_type", "") or data.get("kind", "")`.
    let symbol_type: &str = if node.symbol_type.is_empty() {
        node.extra.get("kind").and_then(Value::as_str).unwrap_or("")
    } else {
        &node.symbol_type
    };
    DOC_KINDS.contains(&symbol_type)
        || symbol_type.ends_with("_document")
        || symbol_type.ends_with("_section")
}

/// `_enrich_graph_nodes_from_db`: a node without its index attributes
/// takes the stored ones it lacks. Only `is_doc` can change anything (the
/// stored `rel_path`, `source_text`, `symbol_name` and `language` of such a
/// node are the empty values the graph already reads), and it is added as
/// the stored integer.
fn enrich_from_index(ctx: &mut Ctx<'_>) -> Result<(), StoreError> {
    let ids: Vec<String> = ctx
        .graph
        .nodes()
        .filter(|(_, data)| lacks_index_attributes(data))
        .map(|(id, _)| id.to_owned())
        .collect();
    let refs: Vec<&str> = ids.iter().map(String::as_str).collect();
    ctx.fetch_rows(&refs)?;
    for id in &ids {
        let Some(row) = ctx.row(id).cloned() else {
            continue;
        };
        let Some(node) = ctx.graph.node_mut(id) else {
            continue;
        };
        if node.rel_path.is_empty() {
            node.rel_path = row.rel_path.as_str().into();
        }
        if node.source_text.is_empty() {
            node.source_text = row.source_text.unwrap_or_default();
        }
        if node.symbol_name.is_empty() {
            node.symbol_name = row.symbol_name;
        }
        if node.language.is_empty() {
            node.language = row.language.as_str().into();
        }
        if !node.extra.contains_key("is_doc") {
            node.extra.insert("is_doc", json!(row.is_doc));
        }
    }
    Ok(())
}

/// `_build_path_index`: `rel_path` → the last node with it.
fn path_index(graph: &CodeGraph) -> HashMap<&str, &str> {
    graph
        .nodes()
        .filter(|(_, data)| !data.rel_path.is_empty())
        .map(|(id, data)| (&*data.rel_path, id))
        .collect()
}

/// `_build_name_index`: `symbol_name` → code nodes, in node order.
fn name_index(graph: &CodeGraph) -> HashMap<&str, Vec<&str>> {
    let mut index: HashMap<&str, Vec<&str>> = HashMap::new();
    for (id, data) in graph.nodes() {
        if is_topology_doc_node(data) || data.symbol_name.is_empty() {
            continue;
        }
        index.entry(&data.symbol_name).or_default().push(id);
    }
    index
}

/// `graph_topology._normalize_path`.
fn normalize_link(reference: &str, source_dir: &str) -> String {
    if ["http://", "https://", "mailto:", "#"]
        .iter()
        .any(|p| reference.starts_with(p))
    {
        return String::new();
    }
    let reference = reference.strip_prefix("./").unwrap_or(reference);
    pystr::normpath(&pystr::join(source_dir, reference)).replace('\\', "/")
}

/// Tier 1, `_extract_hyperlink_edges`: markdown links to indexed paths and
/// backtick names of code nodes (two per name), duplicates included.
fn hyperlink_edges(graph: &CodeGraph) -> Vec<(String, String)> {
    let paths = path_index(graph);
    let names = name_index(graph);
    let mut edges = Vec::new();
    for (id, data) in graph.nodes() {
        if !is_topology_doc_node(data) || data.source_text.is_empty() {
            continue;
        }
        let source_dir = pystr::dirname(&data.rel_path);
        for m in MD_LINK.captures_iter(&data.source_text) {
            let raw = m.get(1).map_or("", |g| g.as_str());
            let reference = pystr::strip(raw.split('#').next().unwrap_or(""));
            if reference.is_empty() {
                continue;
            }
            let resolved = normalize_link(reference, source_dir);
            if resolved.is_empty() {
                continue;
            }
            if let Some(&target) = paths.get(resolved.as_str())
                && target != id
            {
                edges.push((id.to_owned(), target.to_owned()));
            }
        }
        for m in BACKTICK_REF.captures_iter(&data.source_text) {
            let symbol = m.get(1).map_or("", |g| g.as_str());
            let mut targets = names.get(symbol).map_or(&[][..], Vec::as_slice);
            if targets.is_empty() && symbol.contains('.') {
                let last = symbol.rsplit('.').next().unwrap_or("");
                targets = names.get(last).map_or(&[][..], Vec::as_slice);
            }
            for &target in targets.iter().take(2) {
                if target != id {
                    edges.push((id.to_owned(), target.to_owned()));
                }
            }
        }
    }
    edges
}

/// Tier 2, `_extract_proximity_edges`.
fn proximity_edges(graph: &CodeGraph) -> Vec<(String, String)> {
    let mut dir_to_code: indexmap::IndexMap<String, Vec<&str>> = indexmap::IndexMap::new();
    for (id, data) in graph.nodes() {
        if is_topology_doc_node(data) || data.rel_path.is_empty() {
            continue;
        }
        let dir = pystr::dirname(&data.rel_path).replace('\\', "/");
        if !dir.is_empty() {
            dir_to_code.entry(dir).or_default().push(id);
        }
    }
    let mut edges = Vec::new();
    for (id, data) in graph.nodes() {
        if !is_topology_doc_node(data) || data.rel_path.is_empty() {
            continue;
        }
        let doc_dir = pystr::dirname(&data.rel_path).replace('\\', "/");
        if doc_dir.is_empty() {
            // A repository-root doc: one anchor per top-level directory.
            let doc_hash = md5_prefix_u64(id.as_bytes());
            let mut seen_top: HashSet<&str> = HashSet::new();
            for (code_dir, code_nodes) in &dir_to_code {
                let top = code_dir.split('/').next().unwrap_or("");
                if top.is_empty() || seen_top.contains(top) || code_nodes.is_empty() {
                    continue;
                }
                let pick = usize::try_from(doc_hash % code_nodes.len() as u64).unwrap_or(0);
                let anchor = code_nodes[pick];
                if anchor != id {
                    edges.push((id.to_owned(), anchor.to_owned()));
                }
                seen_top.insert(top);
                if seen_top.len() >= ROOT_DOC_TOP_LEVEL_CAP {
                    break;
                }
            }
            continue;
        }
        let topic = DOC_PREFIXES
            .iter()
            .find_map(|prefix| doc_dir.strip_prefix(prefix))
            .unwrap_or(&doc_dir);
        if topic.is_empty() {
            continue;
        }
        let suffix = format!("/{topic}");
        for (code_dir, code_nodes) in &dir_to_code {
            if code_dir == topic || code_dir.ends_with(&suffix) {
                for &anchor in code_nodes.iter().take(5) {
                    if anchor != id {
                        edges.push((id.to_owned(), anchor.to_owned()));
                    }
                }
            }
        }
    }
    edges
}

/// `inject_doc_edges`.
///
/// # Errors
///
/// The store's (reading the rows of nodes without index attributes).
pub fn inject_doc_edges(ctx: &mut Ctx<'_>) -> Result<Value, StoreError> {
    enrich_from_index(ctx)?;
    let mut seen: HashSet<(String, String)> = HashSet::new();
    let mut hyperlinks = 0usize;
    for (source, target) in hyperlink_edges(ctx.graph) {
        if seen.contains(&(source.clone(), target.clone()))
            || ctx.graph.edges_between(&source, &target).next().is_some()
        {
            continue;
        }
        ctx.graph.add_edge(
            &source,
            &target,
            synthetic_edge("hyperlink", "doc", "md_hyperlink", None, None),
        );
        seen.insert((source, target));
        hyperlinks += 1;
    }
    let mut proximity = 0usize;
    for (source, target) in proximity_edges(ctx.graph) {
        if seen.contains(&(source.clone(), target.clone()))
            || ctx.graph.edges_between(&source, &target).next().is_some()
        {
            continue;
        }
        ctx.graph.add_edge(
            &source,
            &target,
            synthetic_edge("proximity", "doc", "dir_proximity", None, None),
        );
        seen.insert((source, target));
        proximity += 1;
    }
    let mut stats = Map::new();
    stats.insert("hyperlink_edges_added".to_owned(), json!(hyperlinks));
    stats.insert("proximity_edges_added".to_owned(), json!(proximity));
    stats.insert(
        "total_edges_added".to_owned(),
        json!(hyperlinks + proximity),
    );
    Ok(Value::Object(stats))
}

/// The stored `is_doc` of a symbol type, as the index writer derives it
/// (for tests of the enrichment rule).
#[must_use]
pub fn stored_is_doc(symbol_type: &str) -> i64 {
    i64::from(constants::is_doc_symbol(&symbol_type.to_lowercase()))
}

#[cfg(test)]
#[allow(clippy::float_cmp)] // bit-exact parity is the point
mod tests {
    use super::*;
    use crate::graph::EdgeData;

    fn node(symbol_type: &str, rel_path: &str, source_text: &str) -> NodeData {
        NodeData {
            symbol_type: symbol_type.to_owned().into(),
            rel_path: rel_path.into(),
            source_text: source_text.to_owned(),
            symbol_name: rel_path.rsplit('/').next().unwrap_or("").to_owned(),
            ..NodeData::default()
        }
    }

    #[test]
    fn the_topology_doc_rule_differs_from_the_cascade_rule() {
        assert!(is_topology_doc_node(&node("markdown_section", "a.md", "")));
        assert!(is_topology_doc_node(&node("readme", "x", "")));
        // Not lower-cased here (the cascade lower-cases).
        assert!(!is_topology_doc_node(&node("Module_Doc", "x", "")));
        // A `.md` path alone is not enough here.
        assert!(!is_topology_doc_node(&node("class", "a.md", "")));
        let mut contract = node("contract", "x.py", "");
        contract.extra.insert("is_doc", json!(false));
        assert!(!is_topology_doc_node(&contract));
        contract.extra.insert("is_doc", json!(1));
        assert!(is_topology_doc_node(&contract));
        assert_eq!(stored_is_doc("Markdown_Section"), 1);
    }

    #[test]
    fn docs_link_to_their_topic_directory_and_root_docs_spread() {
        let mut graph = CodeGraph::new();
        graph.add_node("doc", node("markdown_document", "docs/api/README.md", ""));
        for i in 0..7 {
            graph.add_node(
                format!("c{i}"),
                node("class", &format!("src/api/m{i}.py"), ""),
            );
        }
        graph.add_node("other", node("class", "lib/x.py", ""));
        graph.add_node("root", node("markdown_document", "NOTES.md", ""));
        let edges = proximity_edges(&graph);
        let from_doc: Vec<&str> = edges
            .iter()
            .filter(|(s, _)| s == "doc")
            .map(|(_, t)| t.as_str())
            .collect();
        // The first five of the matching directory.
        assert_eq!(from_doc, ["c0", "c1", "c2", "c3", "c4"]);
        let from_root: Vec<&str> = edges
            .iter()
            .filter(|(s, _)| s == "root")
            .map(|(_, t)| t.as_str())
            .collect();
        // One per top-level directory: src, lib. md5("root")[:8] is
        // 0x63a9f0ea7bb98050, which picks index 0 of 7 in src/api.
        assert_eq!(md5_prefix_u64(b"root") % 7, 0);
        assert_eq!(from_root, ["c0", "other"]);
    }

    #[test]
    fn an_existing_edge_blocks_a_doc_edge() {
        let mut graph = CodeGraph::new();
        graph.add_node("doc", node("markdown_document", "docs/api/a.md", ""));
        graph.add_node("c", node("class", "src/api/m.py", ""));
        graph.add_edge("doc", "c", EdgeData::default());
        let mut store = super::super::replay::ReplayStore::default();
        let mut ctx = Ctx::new(
            &mut graph,
            &mut store,
            None,
            super::super::CalibrationProfile::Calibrated,
        );
        let stats = inject_doc_edges(&mut ctx).unwrap();
        assert_eq!(stats["proximity_edges_added"], 0);
    }
}
