//! The graph's communities (ADR-0027 P3e): `communities.py` of the Python
//! engine.
//!
//! * [`detect`] — `CommunityAnalyzer.detect_communities` with
//!   `_build_community_data`: Leiden over the graph made undirected and
//!   weighted by relation type, then per community its centroids, stats,
//!   dominant types and layers and a heuristic label, in Python's
//!   `community_data` shape (what [`Graph::set_community_data`] stores);
//! * [`label_and_summarize_with`] — `generate_labels` then
//!   `generate_summaries`: a model's label and summary per community.
//!
//! Where it differs from Python, and why:
//!
//! * **Leiden** is `elitea_graph_algos::leiden::partition` (two seeded
//!   passes of `leiden-rs`, seed [`SEED`]) instead of igraph's
//!   `community_leiden(n_iterations=-1)`. No implementation reproduces
//!   igraph's partition bit for bit; both optimise the same objective
//!   (modularity at γ = `resolution`), so the gate is quality, not equality.
//!   igraph's Louvain and networkx fallbacks are not ported: a failed run
//!   is `None`, Python's "both failed".
//! * **Numbering:** Python names communities in igraph's membership order;
//!   here `community_0` is the largest, ties to the one holding the
//!   earliest node (`number_by_size`). Members are listed in node order,
//!   as igraph lists them.
//! * **Modularity** is computed here (igraph's formula, at γ =
//!   `resolution`, as `VertexClustering.modularity` does for Leiden).
//! * **Prompts walk centroids in centroid order.** Python walks a `set` of
//!   centroid ids, whose order follows the string hash, so its relationship
//!   lines (and which ten or fifteen survive the cut) changed from run to
//!   run; and a node's incoming edges are listed in edge export order
//!   (sources in node order), networkx in the order they were added.
//! * `detect_micro_clusters` (Infomap) is not ported: nothing called it.
//!   `micro_clusters` stays `null`.

mod centrality;
mod labels;
#[cfg(test)]
mod tests;

pub use labels::{label_and_summarize, label_and_summarize_with};

use crate::graph::Graph;
use centrality::Undirected;
use elitea_engine_core::pyvalue::py_str;
use elitea_graph_algos::leiden::{LeidenRequest, partition};
use indexmap::IndexMap;
use serde_json::{Map, Value, json};
use std::collections::HashSet;

/// The Leiden seed. Python's igraph run was unseeded (igraph's global
/// RNG); a fixed seed makes a re-run of the same graph the same.
pub const SEED: u64 = 42;

/// Graphs with fewer nodes are not partitioned (`MIN_NODES_FOR_DETECTION`).
pub const MIN_NODES_FOR_DETECTION: usize = 10;

const STRUCTURAL_RELATIONS: &[&str] = &[
    "contains",
    "extends",
    "implements",
    "defines",
    "exports",
    "decorates",
    "annotates",
    "part_of",
    "provides",
];

const BEHAVIORAL_RELATIONS: &[&str] = &[
    "calls",
    "returns",
    "triggers",
    "depends_on",
    "publishes",
    "subscribes_to",
    "stores_in",
    "reads_from",
    "maps_to",
    "transforms",
    "shown_on",
    "navigates_to",
    "validates",
    "tests",
    "covers",
    "reproduces",
    "blocks",
];

const SEMANTIC_RELATIONS: &[&str] = &[
    "uses",
    "references",
    "imports",
    "related_to",
    "documents",
    "duplicates",
    "contradicts",
    "synonym_of",
    "mentions",
    "owned_by",
    "maintained_by",
    "assigned_to",
    "reviewed_by",
    "introduced_in",
    "modified_in",
    "removed_in",
    "supersedes",
];

const STRUCTURAL_WEIGHT: f64 = 3.0;
const BEHAVIORAL_WEIGHT: f64 = 2.0;
const SEMANTIC_WEIGHT: f64 = 1.0;
const DEFAULT_EDGE_WEIGHT: f64 = 1.0;

const CENTROID_PAGERANK_WEIGHT: f64 = 0.5;
const CENTROID_DEGREE_WEIGHT: f64 = 0.3;
const CENTROID_BETWEENNESS_WEIGHT: f64 = 0.2;

/// `TYPE_PRIORITY`: architectural weight of an entity type.
const TYPE_PRIORITY: &[(&str, u8)] = &[
    ("class", 10),
    ("interface", 10),
    ("struct", 10),
    ("enum", 10),
    ("trait", 10),
    ("function", 9),
    ("module", 8),
    ("component", 8),
    ("service", 8),
    ("constant", 7),
    ("feature", 6),
    ("requirement", 6),
    ("test", 6),
    ("rule", 6),
    ("workflow", 6),
    ("source_file", 5),
    ("document_file", 5),
    ("config_file", 5),
    ("method", 4),
    ("property", 4),
    ("variable", 4),
    ("fact", 2),
    ("concept", 2),
    ("import", 1),
];

/// Types at or above this priority define a community's identity.
const ARCHITECTURAL_MIN_PRIORITY: u8 = 5;
const MIN_CENTROIDS: usize = 1;
const MAX_CENTROIDS: usize = 15;
/// At least this share of the architectural members of one type gives a
/// type-named label.
const TYPE_DOMINANCE_THRESHOLD: f64 = 0.6;

/// `_get_edge_weight`: a relation type's weight.
fn edge_weight(relation_type: &str) -> f64 {
    let relation = elitea_engine_core::pystr::strip(relation_type).to_lowercase();
    let relation = relation.as_str();
    if STRUCTURAL_RELATIONS.contains(&relation) {
        STRUCTURAL_WEIGHT
    } else if BEHAVIORAL_RELATIONS.contains(&relation) {
        BEHAVIORAL_WEIGHT
    } else if SEMANTIC_RELATIONS.contains(&relation) {
        SEMANTIC_WEIGHT
    } else {
        DEFAULT_EDGE_WEIGHT
    }
}

/// An edge's relation type, `''` when it has none (or not a string).
fn relation_of(edge: &Map<String, Value>) -> &str {
    edge.get("relation_type")
        .and_then(Value::as_str)
        .unwrap_or("")
}

/// `TYPE_PRIORITY.get(t.lower(), 0)`.
fn type_priority(entity_type: &str) -> u8 {
    let lowered = entity_type.to_lowercase();
    TYPE_PRIORITY
        .iter()
        .find(|(known, _)| *known == lowered)
        .map_or(0, |(_, priority)| *priority)
}

/// A node's `type` (`'unknown'` when it has none), as Python reads it.
fn type_of(node: Option<&Map<String, Value>>) -> String {
    node.and_then(|node| node.get("type"))
        .map_or_else(|| "unknown".to_owned(), py_str)
}

/// Whether a node's type is architectural.
fn is_architectural(node: Option<&Map<String, Value>>) -> bool {
    type_priority(&type_of(node)) >= ARCHITECTURAL_MIN_PRIORITY
}

/// Python's `round(value, 4)`: correctly rounded, ties to even.
fn round4(value: f64) -> f64 {
    format!("{value:.4}").parse().unwrap_or(value)
}

/// `_normalize_scores`: min-max to `[0, 1]`; all equal → `1 / n` each.
#[allow(clippy::cast_precision_loss, reason = "a member count")]
fn normalize(scores: &[f64]) -> Vec<f64> {
    let low = scores.iter().copied().fold(f64::INFINITY, f64::min);
    let high = scores.iter().copied().fold(f64::NEG_INFINITY, f64::max);
    let range = high - low;
    if range == 0.0 || !range.is_finite() {
        return vec![1.0 / scores.len() as f64; scores.len()];
    }
    scores.iter().map(|score| (score - low) / range).collect()
}

/// `_nx_to_igraph`: one undirected edge per directed edge, weighted by its
/// relation type (an edge's own `weight` is NOT read here, as in Python).
fn undirected(graph: &Graph, index: &IndexMap<&str, usize>) -> Undirected {
    let edges = graph
        .edges()
        .filter_map(|(source, target, edge)| {
            Some((
                *index.get(source)?,
                *index.get(target)?,
                edge_weight(relation_of(edge)),
            ))
        })
        .collect();
    Undirected {
        vertices: index.len(),
        edges,
    }
}

/// `CommunityAnalyzer(resolution).detect_communities(graph)`: the
/// `community_data` document, or `None` for a graph under
/// [`MIN_NODES_FOR_DETECTION`] nodes or a failed Leiden run.
#[must_use]
pub fn detect(graph: &Graph, resolution: f64) -> Option<Value> {
    if graph.node_count() < MIN_NODES_FOR_DETECTION {
        return None;
    }
    let index: IndexMap<&str, usize> = graph
        .nodes()
        .enumerate()
        .map(|(position, (id, _))| (id, position))
        .collect();
    let ids: Vec<&str> = index.keys().copied().collect();
    let multigraph = undirected(graph, &index);
    let leiden_edges: Vec<(usize, usize, f64)> = multigraph
        .edges
        .iter()
        .map(|&(u, v, weight)| (u.min(v), u.max(v), weight))
        .collect();
    let membership = match partition(&LeidenRequest {
        vertices: multigraph.vertices,
        edges: &leiden_edges,
        resolution,
        seed: SEED,
    }) {
        Ok(membership) => membership,
        Err(error) => {
            tracing::warn!(%error, "community detection failed");
            return None;
        }
    };
    let modularity = multigraph.modularity(&membership, resolution);
    let measures = Measures {
        pagerank: multigraph.pagerank(),
        betweenness: multigraph.betweenness(),
        degree: multigraph.strength(),
    };
    let count = membership.iter().max().map_or(0, |last| last + 1);
    let mut groups: Vec<Vec<usize>> = vec![Vec::new(); count];
    for (vertex, community) in membership.iter().enumerate() {
        groups[*community].push(vertex);
    }
    let mut communities = Map::new();
    for (number, members) in groups.iter().enumerate() {
        if members.is_empty() {
            continue;
        }
        let member_ids: Vec<&str> = members.iter().map(|&vertex| ids[vertex]).collect();
        let centroids = centroids(graph, members, &member_ids, &measures);
        let stats = stats(graph, &member_ids);
        let types = dominant_types(graph, &member_ids);
        let layers = dominant_layers(graph, &member_ids);
        let label = auto_label(graph, &member_ids, &centroids, &types, &layers);
        communities.insert(
            format!("community_{number}"),
            json!({
                "members": member_ids,
                "centroids": centroids,
                "stats": stats,
                "label": label,
                "summary": null,
                "dominant_types": types.iter().map(|(name, _)| name).collect::<Vec<_>>(),
                "dominant_layers": layers.iter().map(|(name, _)| name).collect::<Vec<_>>(),
                "micro_clusters": null,
            }),
        );
    }
    Some(json!({
        "algorithm": "leiden",
        "resolution": resolution,
        "modularity": modularity,
        "num_communities": communities.len(),
        "communities": communities,
    }))
}

/// The whole-graph centralities, by vertex.
struct Measures {
    pagerank: Vec<f64>,
    betweenness: Vec<f64>,
    degree: Vec<f64>,
}

/// `_compute_centroids_igraph`: the top `round(√n)` (1..=15) members by
/// 0.5 `PageRank` + 0.3 strength + 0.2 betweenness (each min-max
/// normalised within the community), drawn from the architectural members
/// when there are any; scores rescaled so the best is 1, rounded to 4.
#[allow(clippy::cast_precision_loss, reason = "a member count")]
#[allow(
    clippy::cast_possible_truncation,
    clippy::cast_sign_loss,
    reason = "the rounded square root of a member count"
)]
fn centroids(
    graph: &Graph,
    members: &[usize],
    member_ids: &[&str],
    measures: &Measures,
) -> Vec<Value> {
    if members.is_empty() {
        return Vec::new();
    }
    let k = ((members.len() as f64).sqrt().round() as usize).clamp(MIN_CENTROIDS, MAX_CENTROIDS);
    let pick = |values: &[f64]| {
        members
            .iter()
            .map(|&vertex| values[vertex])
            .collect::<Vec<_>>()
    };
    let pagerank = normalize(&pick(&measures.pagerank));
    let betweenness = normalize(&pick(&measures.betweenness));
    let degree = normalize(&pick(&measures.degree));
    // (id, score, name, type, priority)
    let scored: Vec<(&str, f64, Value, Value, u8)> = member_ids
        .iter()
        .enumerate()
        .map(|(position, &id)| {
            let composite = CENTROID_PAGERANK_WEIGHT * pagerank[position]
                + CENTROID_DEGREE_WEIGHT * degree[position]
                + CENTROID_BETWEENNESS_WEIGHT * betweenness[position];
            let node = graph.node(id);
            let name = node
                .and_then(|node| node.get("name"))
                .cloned()
                .unwrap_or_else(|| json!(id));
            let entity_type = node
                .and_then(|node| node.get("type"))
                .cloned()
                .unwrap_or_else(|| json!("unknown"));
            let priority = type_priority(&py_str(&entity_type));
            (id, composite, name, entity_type, priority)
        })
        .collect();
    let architectural: Vec<_> = scored
        .iter()
        .filter(|entry| entry.4 >= ARCHITECTURAL_MIN_PRIORITY)
        .cloned()
        .collect();
    let mut pool = if architectural.is_empty() {
        scored
    } else {
        architectural
    };
    // Stable and descending: equal scores keep member order, as Python's
    // `sort(reverse=True)` does.
    pool.sort_by(|a, b| b.1.total_cmp(&a.1));
    pool.truncate(k);
    let best = pool
        .iter()
        .map(|entry| entry.1)
        .fold(f64::NEG_INFINITY, f64::max);
    let best = if best == 0.0 || !best.is_finite() {
        1.0
    } else {
        best
    };
    pool.into_iter()
        .map(|(id, score, name, entity_type, _)| {
            json!({"id": id, "score": round4(score / best), "name": name, "type": entity_type})
        })
        .collect()
}

/// `_compute_stats`: over the members' OUTGOING edges (an edge's own
/// `weight` when it has one, else its relation type's), the share of
/// weight that stays inside, and the internal edge density (directed).
#[allow(clippy::cast_precision_loss, reason = "edge and member counts")]
fn stats(graph: &Graph, member_ids: &[&str]) -> Value {
    let members: HashSet<&str> = member_ids.iter().copied().collect();
    let size = members.len();
    if size <= 1 {
        return json!({"size": size, "density": 0.0, "cohesion": 0.0});
    }
    let mut internal_edges = 0_usize;
    let mut internal_weight = 0.0;
    let mut total_weight = 0.0;
    for (source, target, edge) in graph.edges() {
        if !members.contains(source) {
            continue;
        }
        let weight = edge
            .get("weight")
            .and_then(Value::as_f64)
            .unwrap_or_else(|| edge_weight(relation_of(edge)));
        total_weight += weight;
        if members.contains(target) {
            internal_edges += 1;
            internal_weight += weight;
        }
    }
    let possible = size * (size - 1);
    let density = if possible > 0 {
        internal_edges as f64 / possible as f64
    } else {
        0.0
    };
    let cohesion = if total_weight > 0.0 {
        internal_weight / total_weight
    } else {
        0.0
    };
    json!({
        "size": size,
        "density": round4(density),
        "cohesion": round4(cohesion),
        "internal_edges": internal_edges,
    })
}

/// `_get_dominant_types`: the top 3 architectural types (or, with none,
/// the top 3 others) by priority then count, ties in first-seen order.
fn dominant_types(graph: &Graph, member_ids: &[&str]) -> Vec<(String, usize)> {
    let mut counter: IndexMap<String, usize> = IndexMap::new();
    for id in member_ids {
        *counter.entry(type_of(graph.node(id))).or_default() += 1;
    }
    let (mut architectural, mut noise): (Vec<_>, Vec<_>) = counter
        .into_iter()
        .partition(|(name, _)| type_priority(name) >= ARCHITECTURAL_MIN_PRIORITY);
    let key = |entry: &(String, usize)| (type_priority(&entry.0), entry.1);
    architectural.sort_by_key(|entry| std::cmp::Reverse(key(entry)));
    noise.sort_by_key(|entry| std::cmp::Reverse(key(entry)));
    let mut result = if architectural.is_empty() {
        noise
    } else {
        architectural
    };
    result.truncate(3);
    result
}

/// `_get_dominant_layers`: the 2 most common non-empty `layer` attributes,
/// ties in first-seen order (`Counter.most_common`).
fn dominant_layers(graph: &Graph, member_ids: &[&str]) -> Vec<(String, usize)> {
    let mut counter: IndexMap<String, usize> = IndexMap::new();
    for id in member_ids {
        if let Some(layer) = graph
            .node(id)
            .and_then(|node| node.get("layer"))
            .and_then(Value::as_str)
            .filter(|layer| !layer.is_empty())
        {
            *counter.entry(layer.to_owned()).or_default() += 1;
        }
    }
    let mut layers: Vec<(String, usize)> = counter.into_iter().collect();
    layers.sort_by_key(|entry| std::cmp::Reverse(entry.1));
    layers.truncate(2);
    layers
}

/// `_auto_label`: `"<Type> cluster: a, b, c"` when one architectural type
/// holds 60 % of the architectural members, `"Documentation: a, b, c"`
/// when the top layer is documentation, else `"<top centroid> & related
/// (<top type>)"`.
#[allow(clippy::cast_precision_loss, reason = "member counts")]
fn auto_label(
    graph: &Graph,
    member_ids: &[&str],
    centroids: &[Value],
    types: &[(String, usize)],
    layers: &[(String, usize)],
) -> String {
    let Some(top) = centroids.first() else {
        return "Empty community".to_owned();
    };
    let name_of = |centroid: &Value| centroid.get("name").map(py_str).unwrap_or_default();
    let first_three = || {
        centroids
            .iter()
            .take(3)
            .map(name_of)
            .collect::<Vec<_>>()
            .join(", ")
    };
    let architectural = member_ids
        .iter()
        .filter(|id| is_architectural(graph.node(id)))
        .count();
    if let Some((top_type, top_count)) = types.first()
        && architectural > 0
        && *top_count as f64 / architectural as f64 >= TYPE_DOMINANCE_THRESHOLD
    {
        return format!(
            "{} cluster: {}",
            elitea_engine_core::pystr::capitalize(top_type),
            first_three()
        );
    }
    if layers
        .first()
        .is_some_and(|(layer, _)| layer == "documentation")
    {
        return format!("Documentation: {}", first_three());
    }
    let top_type = types.first().map_or("entities", |(name, _)| name.as_str());
    format!("{} & related ({top_type})", name_of(top))
}
