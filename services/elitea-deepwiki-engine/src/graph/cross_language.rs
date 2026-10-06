//! Phase 1c: the cross-language linker (`cross_language_linker.py`).
//!
//! A four-level cascade that RETURNS edges (the caller adds them):
//!
//! * L0 — parser-supplied cross-language relationships, promoted. The
//!   Python engine's live path passes none (its parsers emit none).
//! * L1 — nodes in different languages that define or consume the same
//!   `contract` node (or, with no contract node in the graph, share a
//!   surface key): weight `0.7 · locality · 1/ln(1 + n)`.
//! * L2 — fused FTS + vector hits restricted to other languages. Phase 1c
//!   runs before any index exists, so Python passes no hits.
//! * L3 — members of an L0..L2 pair whose simple names match.
//!
//! Every weight is clamped to `[0.3, 0.8]`. Python notes the clamp is
//! overwritten by Phase 2's inverse-in-degree weighting; it is kept for
//! the rows, which store it.
//!
//! The arithmetic is evaluated in Python's order (`0.7 * locality *
//! specificity` is `(0.7 · locality) · specificity`) so the stored weights
//! are bit-identical.

use super::api_surface::SurfacesByNode;
use super::{CodeGraph, EdgeData};
use indexmap::{IndexMap, IndexSet};
use serde_json::{Map, Value, json};
use std::collections::HashSet;

/// One edge the linker proposes: source, target, attributes.
pub type LinkEdge = (String, String, EdgeData);

const WEIGHT_FLOOR: f64 = 0.3;
const WEIGHT_CEIL: f64 = 0.8;

/// `max(0.3, min(0.8, w))`. Not `f64::clamp`: Python's form maps NaN to
/// the ceiling, `clamp` would keep it.
#[allow(clippy::manual_clamp)]
fn clamp(weight: f64) -> f64 {
    f64::max(WEIGHT_FLOOR, f64::min(WEIGHT_CEIL, weight))
}

fn node_language(graph: &CodeGraph, id: &str) -> String {
    graph
        .node(id)
        .map(|data| data.language.to_lowercase())
        .unwrap_or_default()
}

/// `_locality_factor`: 1.0 when both paths share their top directory, 0.7
/// when not, 0.5 when either path is unknown.
fn locality(graph: &CodeGraph, source: &str, target: &str) -> f64 {
    let top = |id: &str| -> String {
        graph
            .node(id)
            .map(|data| data.rel_path.split('/').next().unwrap_or("").to_owned())
            .unwrap_or_default()
    };
    let path = |id: &str| graph.node(id).map_or("", |data| data.rel_path.as_str());
    if path(source).is_empty() || path(target).is_empty() {
        return 0.5;
    }
    if top(source) == top(target) { 1.0 } else { 0.7 }
}

fn edge(
    rel_type: &str,
    weight: f64,
    annotations: Map<String, Value>,
    provenance: Value,
) -> EdgeData {
    let mut extra = Map::new();
    extra.insert("provenance".to_owned(), provenance);
    EdgeData {
        rel_type: rel_type.to_owned().into(),
        edge_class: "cross_language".into(),
        weight,
        annotations: annotations.into(),
        extra: extra.into(),
        ..EdgeData::default()
    }
}

/// A parser-supplied cross-language relationship (L0 input).
#[derive(Debug, Clone, PartialEq)]
pub struct CrossLanguageRelationship {
    pub source: String,
    pub target: String,
    /// Defaults to 0.8 in Python.
    pub confidence: f64,
    /// Defaults to `parser_exact` in Python.
    pub matcher: String,
}

/// `link_l0_exact`.
#[must_use]
pub fn link_l0_exact(
    graph: &CodeGraph,
    relationships: &[CrossLanguageRelationship],
) -> Vec<LinkEdge> {
    relationships
        .iter()
        .filter(|rel| {
            !rel.source.is_empty()
                && !rel.target.is_empty()
                && graph.has_node(&rel.source)
                && graph.has_node(&rel.target)
                && node_language(graph, &rel.source) != node_language(graph, &rel.target)
        })
        .map(|rel| {
            let provenance = json!({
                "source": "cross_language_linker",
                "level": "L0",
                "matcher": rel.matcher,
            });
            (
                rel.source.clone(),
                rel.target.clone(),
                edge(
                    "cross_language_L0",
                    clamp(rel.confidence),
                    Map::new(),
                    provenance,
                ),
            )
        })
        .collect()
}

/// `link_l1_api_surface`: through the contract nodes when the graph has
/// any, else through `surfaces_by_node`.
#[must_use]
pub fn link_l1_api_surface(
    graph: &CodeGraph,
    surfaces_by_node: Option<&SurfacesByNode>,
) -> Vec<LinkEdge> {
    let contracts: Vec<&str> = graph
        .nodes()
        .filter(|(_, data)| data.symbol_type == "contract")
        .map(|(id, _)| id)
        .collect();
    if !contracts.is_empty() {
        return link_l1_via_contract_nodes(graph, &contracts);
    }
    match surfaces_by_node {
        Some(surfaces) if !surfaces.is_empty() => link_l1_legacy(graph, surfaces),
        _ => Vec::new(),
    }
}

fn link_l1_via_contract_nodes(graph: &CodeGraph, contracts: &[&str]) -> Vec<LinkEdge> {
    let mut out = Vec::new();
    for &contract in contracts {
        let Some(data) = graph.node(contract) else {
            continue;
        };
        let kind = data.signature.as_str();
        let surface = data.symbol_name.as_str();
        let implementors: Vec<&str> = graph
            .predecessors(contract)
            .filter(|source| {
                graph
                    .node(source)
                    .is_none_or(|d| d.symbol_type != "contract")
            })
            .filter(|source| {
                graph
                    .edges_between(source, contract)
                    .any(|(_, e)| matches!(&*e.rel_type, "defines" | "consumes"))
            })
            .collect();
        if implementors.len() < 2 {
            continue;
        }
        #[allow(clippy::cast_precision_loss)] // a node count
        let specificity = 1.0 / (1.0 + implementors.len() as f64).ln();
        for (i, &source) in implementors.iter().enumerate() {
            for &target in &implementors[i + 1..] {
                if node_language(graph, source) == node_language(graph, target) {
                    continue;
                }
                let weight = clamp(0.7 * locality(graph, source, target) * specificity);
                let mut annotations = Map::new();
                annotations.insert("via".to_owned(), json!([format!("contract={contract}")]));
                let provenance = json!({
                    "source": "cross_language_linker",
                    "level": "L1",
                    "matcher": format!("api_surface:{kind}"),
                    "surface": surface,
                });
                out.push((
                    source.to_owned(),
                    target.to_owned(),
                    edge("cross_language_L1", weight, annotations, provenance),
                ));
            }
        }
    }
    out
}

/// The legacy L1 pairing over `surfaces_by_node` (`_link_l1_legacy`), and
/// its same-language variant for cross-repo linking
/// (`link_api_surface_any_language`).
fn pair_by_surface(
    graph: &CodeGraph,
    surfaces_by_node: &SurfacesByNode,
    cross_language_only: bool,
    mut make: impl FnMut(&str, &str, f64) -> EdgeData,
) -> Vec<LinkEdge> {
    let mut by_surface: IndexMap<(&str, &str), Vec<(&str, f64)>> = IndexMap::new();
    for (id, surfaces) in surfaces_by_node {
        if !graph.has_node(id) {
            continue;
        }
        for surface in surfaces {
            by_surface
                .entry((surface.kind.as_str(), surface.surface.as_str()))
                .or_default()
                .push((id.as_str(), surface.weight_hint));
        }
    }
    let mut out = Vec::new();
    for ((kind, surface), nodes) in &by_surface {
        if nodes.len() < 2 {
            continue;
        }
        #[allow(clippy::cast_precision_loss)] // a node count
        let specificity = 1.0 / (1.0 + nodes.len() as f64).ln();
        for (i, &(source, source_hint)) in nodes.iter().enumerate() {
            for &(target, target_hint) in &nodes[i + 1..] {
                if cross_language_only
                    && node_language(graph, source) == node_language(graph, target)
                {
                    continue;
                }
                let base = 0.7 * (source_hint + target_hint) / 2.0;
                let weight = clamp(base * locality(graph, source, target) * specificity);
                let mut data = make(kind, surface, weight);
                data.weight = weight;
                out.push((source.to_owned(), target.to_owned(), data));
            }
        }
    }
    out
}

fn link_l1_legacy(graph: &CodeGraph, surfaces_by_node: &SurfacesByNode) -> Vec<LinkEdge> {
    pair_by_surface(graph, surfaces_by_node, true, |kind, surface, weight| {
        let provenance = json!({
            "source": "cross_language_linker",
            "level": "L1",
            "matcher": format!("api_surface:{kind}"),
            "surface": surface,
        });
        edge("cross_language_L1", weight, Map::new(), provenance)
    })
}

/// `link_api_surface_any_language` with its defaults: L1 pairing without
/// the different-language rule (Phase 8 cross-repo linking).
#[must_use]
pub fn link_api_surface_any_language(
    graph: &CodeGraph,
    surfaces_by_node: &SurfacesByNode,
) -> Vec<LinkEdge> {
    pair_by_surface(graph, surfaces_by_node, false, |kind, surface, weight| {
        let provenance = json!({
            "source": "cross_repo_linker",
            "level": "L1",
            "matcher": format!("api_surface_any:{kind}"),
            "surface": surface,
        });
        edge("api_surface_any_language", weight, Map::new(), provenance)
    })
}

/// Ranked hit lists per source node (node ids, best first), for L2.
pub type HitsPerNode = IndexMap<String, Vec<String>>;

/// `rrf_fuse` over two ranked id lists: `Σ 1/(k + rank)`, best first,
/// ties by id descending.
#[must_use]
pub fn rrf_fuse(lists: &[&[String]], k: u32) -> Vec<(String, f64)> {
    let mut scores: IndexMap<&str, f64> = IndexMap::new();
    for list in lists {
        for (rank, id) in (1u32..).zip(list.iter()) {
            *scores.entry(id.as_str()).or_insert(0.0) += 1.0 / f64::from(k + rank);
        }
    }
    let mut fused: Vec<(String, f64)> = scores
        .into_iter()
        .map(|(id, s)| (id.to_owned(), s))
        .collect();
    fused.sort_by(|a, b| b.1.total_cmp(&a.1).then_with(|| b.0.cmp(&a.0)));
    fused
}

/// `link_l2_hybrid` with Python's defaults (`k=60`, threshold 0.02, top 10).
#[must_use]
pub fn link_l2_hybrid(graph: &CodeGraph, fts: &HitsPerNode, vec: &HitsPerNode) -> Vec<LinkEdge> {
    let sources: IndexSet<&String> = fts.keys().chain(vec.keys()).collect();
    let mut out = Vec::new();
    let mut seen: HashSet<(String, String)> = HashSet::new();
    let empty: Vec<String> = Vec::new();
    for source in sources {
        if !graph.has_node(source) {
            continue;
        }
        let source_language = node_language(graph, source);
        let fused = rrf_fuse(
            &[
                fts.get(source).unwrap_or(&empty),
                vec.get(source).unwrap_or(&empty),
            ],
            60,
        );
        for (target, score) in fused.into_iter().take(10) {
            if score < 0.02 {
                break;
            }
            if target.is_empty() || &target == source || !graph.has_node(&target) {
                continue;
            }
            if node_language(graph, &target) == source_language {
                continue;
            }
            if !seen.insert((source.clone(), target.clone())) {
                continue;
            }
            let weight = clamp(0.5 * score * locality(graph, source, &target));
            let provenance = json!({
                "source": "cross_language_linker",
                "level": "L2",
                "matcher": "hybrid_rrf",
                "rrf_score": score,
            });
            out.push((
                source.clone(),
                target,
                edge("cross_language_L2", weight, Map::new(), provenance),
            ));
        }
    }
    out
}

/// `link_l3_containment`: for each parent link A → B, the members of A and
/// B with the same simple name, in different languages.
#[must_use]
pub fn link_l3_containment(graph: &CodeGraph, parent_links: &[LinkEdge]) -> Vec<LinkEdge> {
    if parent_links.is_empty() {
        return Vec::new();
    }
    let mut members: IndexMap<&str, IndexMap<&str, &str>> = IndexMap::new();
    for (id, data) in graph.nodes() {
        let Some(parent) = data.parent_symbol.as_deref().filter(|p| !p.is_empty()) else {
            continue;
        };
        if !graph.has_node(parent) {
            continue;
        }
        let simple = data.symbol_name.rsplit('.').next().unwrap_or("");
        if !simple.is_empty() {
            members.entry(parent).or_default().insert(simple, id);
        }
    }
    let mut out = Vec::new();
    let mut seen: HashSet<(&str, &str)> = HashSet::new();
    for (source, target, data) in parent_links {
        let (Some(source_members), Some(target_members)) =
            (members.get(source.as_str()), members.get(target.as_str()))
        else {
            continue;
        };
        // A Python set intersection: its order is the hash seed's; it
        // reaches only the edge order.
        for (name, &child_source) in source_members {
            let Some(&child_target) = target_members.get(name) else {
                continue;
            };
            if child_source == child_target
                || node_language(graph, child_source) == node_language(graph, child_target)
                || !seen.insert((child_source, child_target))
            {
                continue;
            }
            let provenance = json!({
                "source": "cross_language_linker",
                "level": "L3",
                "matcher": "containment_member",
                "parent_pair": [source, target],
            });
            out.push((
                child_source.to_owned(),
                child_target.to_owned(),
                edge(
                    "cross_language_L3",
                    clamp(0.6 * data.weight),
                    Map::new(),
                    provenance,
                ),
            ));
        }
    }
    out
}

/// `run_cross_language_linker`: L0, L1, L2, then L3 over all of them;
/// duplicate `(source, target, rel_type)` triples dropped (first wins).
#[must_use]
pub fn run_cross_language_linker(
    graph: &CodeGraph,
    relationships: &[CrossLanguageRelationship],
    surfaces_by_node: Option<&SurfacesByNode>,
    fts_hits: &HitsPerNode,
    vec_hits: &HitsPerNode,
) -> Vec<LinkEdge> {
    let mut edges = link_l0_exact(graph, relationships);
    edges.extend(link_l1_api_surface(graph, surfaces_by_node));
    edges.extend(link_l2_hybrid(graph, fts_hits, vec_hits));
    let l3 = link_l3_containment(graph, &edges);
    edges.extend(l3);
    let mut seen: HashSet<(String, String, String)> = HashSet::new();
    edges.retain(|(source, target, data)| {
        seen.insert((source.clone(), target.clone(), data.rel_type.to_string()))
    });
    edges
}

#[cfg(test)]
mod tests;
