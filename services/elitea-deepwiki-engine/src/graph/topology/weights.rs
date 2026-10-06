//! Inverse in-degree weights and hub detection (`apply_edge_weights`,
//! `detect_hubs`).
//!
//! Every edge `u → v` weighs `1 / ln(structural_in_degree(v) + 2)`: an edge
//! into a utility everything points at (a logger, a base class) weighs
//! little; a direct capability edge weighs up to `1 / ln 2 ≈ 1.44`. The
//! in-degree counts STRUCTURAL edges only, so the synthetic edges Phase 2
//! adds do not dilute an anchor's real edges.
//!
//! Synthetic edges (classes `directory`, `lexical`, `semantic`, `doc`,
//! `bridge` — Phase 1c's markdown edges are `doc` too) are floored so that
//! clustering groups on them: at 0.5 under the `legacy` profile; under
//! `calibrated` at 0.30 (directory, bridge), 0.40 (lexical, doc) or 0.50
//! (semantic), lifted to the edge's `raw_similarity` when it has one.
//!
//! A hub is a node whose in-degree (over ALL edges, synthetic included)
//! has a z-score above the threshold.

use super::CalibrationProfile;
use super::pynum::{count_f64, numpy_mean_std, py_round, py_sum};
use crate::graph::CodeGraph;
use serde_json::{Map, Value, json};
use std::collections::HashMap;

/// `SYNTHETIC_WEIGHT_FLOOR`.
pub const SYNTHETIC_WEIGHT_FLOOR: f64 = 0.5;

/// The edge classes Phase 2 treats as synthetic.
pub const SYNTHETIC_CLASSES: &[&str] = &["directory", "lexical", "semantic", "doc", "bridge"];

/// `_CALIBRATED_FLOOR_BY_CLASS`.
#[must_use]
pub fn calibrated_floor(edge_class: &str) -> f64 {
    match edge_class {
        "directory" | "bridge" => 0.30,
        "lexical" | "doc" => 0.40,
        "semantic" => 0.50,
        _ => SYNTHETIC_WEIGHT_FLOOR,
    }
}

/// Python's `max(a, b)`: `a` unless `b` is strictly greater.
fn py_max(a: f64, b: f64) -> f64 {
    if b > a { b } else { a }
}

/// The weight of one edge into a node with `structural_in` structural
/// in-edges.
#[must_use]
pub fn edge_weight(
    structural_in: usize,
    edge_class: &str,
    raw_similarity: Option<f64>,
    profile: CalibrationProfile,
) -> f64 {
    let weight = 1.0 / count_f64(structural_in + 2).ln();
    if !SYNTHETIC_CLASSES.contains(&edge_class) {
        return weight;
    }
    match profile {
        CalibrationProfile::Calibrated => {
            let mut floor = calibrated_floor(edge_class);
            if let Some(similarity) = raw_similarity {
                floor = py_max(floor, similarity);
            }
            py_max(weight, floor)
        }
        CalibrationProfile::Legacy => py_max(weight, SYNTHETIC_WEIGHT_FLOOR),
    }
}

/// `apply_edge_weights`: set every edge's `weight`; returns the stats dict.
pub fn apply_edge_weights(graph: &mut CodeGraph, profile: CalibrationProfile) -> Value {
    if graph.edge_count() == 0 {
        return json!({"edges_weighted": 0, "min": 0, "max": 0, "mean": 0});
    }
    let mut structural_in: HashMap<String, usize> = HashMap::new();
    for edge in graph.edges() {
        if !SYNTHETIC_CLASSES.contains(&&*edge.data.edge_class) {
            *structural_in.entry(edge.target.to_owned()).or_default() += 1;
        }
    }
    let mut weights: Vec<f64> = Vec::with_capacity(graph.edge_count());
    let mut synthetic = 0usize;
    let mut per_class: indexmap::IndexMap<String, usize> = indexmap::IndexMap::new();
    graph.for_each_edge_mut(|_, target, data| {
        let inbound = structural_in.get(target).copied().unwrap_or(0);
        let weight = edge_weight(inbound, &data.edge_class, data.raw_similarity, profile);
        if SYNTHETIC_CLASSES.contains(&&*data.edge_class) {
            synthetic += 1;
            *per_class.entry(data.edge_class.to_string()).or_default() += 1;
        }
        data.weight = weight;
        weights.push(weight);
    });
    let min = weights.iter().copied().fold(f64::INFINITY, f64::min);
    let max = weights.iter().copied().fold(f64::NEG_INFINITY, f64::max);
    let mean = py_sum(&weights) / count_f64(weights.len());
    let per_class: Map<String, Value> = per_class.into_iter().map(|(k, v)| (k, json!(v))).collect();
    json!({
        "edges_weighted": weights.len(),
        "synthetic_floored": synthetic,
        "synthetic_per_class": per_class,
        "calibration_profile": profile.name(),
        "min": py_round(min, 4),
        "max": py_round(max, 4),
        "mean": py_round(mean, 4),
    })
}

/// `detect_hubs`: the nodes whose in-degree z-score exceeds `z_threshold`,
/// in id order (numpy's population mean and standard deviation).
#[must_use]
pub fn detect_hubs(graph: &CodeGraph, z_threshold: f64) -> Vec<String> {
    if graph.node_count() < 3 {
        return Vec::new();
    }
    let table = super::degrees(graph);
    let nodes: Vec<(&str, usize)> = graph
        .nodes()
        .map(|(id, _)| (id, table.get(id).map_or(0, |d| d.0)))
        .collect();
    let in_degrees: Vec<usize> = nodes.iter().map(|(_, d)| *d).collect();
    let (mean, std) = numpy_mean_std(&in_degrees);
    if std == 0.0 {
        return Vec::new();
    }
    let mut hubs: Vec<String> = nodes
        .iter()
        .filter(|(_, degree)| (count_f64(*degree) - mean) / std > z_threshold)
        .map(|(id, _)| (*id).to_owned())
        .collect();
    hubs.sort_unstable();
    hubs
}

#[cfg(test)]
#[allow(clippy::float_cmp)] // bit-exact parity is the point
mod tests {
    use super::*;
    use crate::graph::EdgeData;

    fn edge(edge_class: &'static str, raw: Option<f64>) -> EdgeData {
        EdgeData {
            edge_class: edge_class.into(),
            raw_similarity: raw,
            ..EdgeData::default()
        }
    }

    #[test]
    fn weights_fall_with_structural_in_degree_and_synthetic_edges_are_floored() {
        let cal = CalibrationProfile::Calibrated;
        assert_eq!(edge_weight(0, "structural", None, cal), 1.0 / 2f64.ln());
        assert_eq!(edge_weight(98, "calls", None, cal), 1.0 / 100f64.ln());
        // 1/ln(100) = 0.217: floored per class.
        assert_eq!(edge_weight(98, "directory", None, cal), 0.30);
        assert_eq!(edge_weight(98, "doc", None, cal), 0.40);
        assert_eq!(edge_weight(98, "semantic", None, cal), 0.50);
        // Lifted to the similarity.
        assert_eq!(edge_weight(98, "doc", Some(0.95), cal), 0.95);
        assert_eq!(edge_weight(98, "lexical", Some(0.1), cal), 0.40);
        let legacy = CalibrationProfile::Legacy;
        assert_eq!(edge_weight(98, "directory", Some(0.95), legacy), 0.5);
    }

    #[test]
    fn synthetic_edges_do_not_count_toward_in_degree() {
        let mut graph = CodeGraph::new();
        graph.add_edge("a", "t", edge("structural", None));
        graph.add_edge("b", "t", edge("directory", None));
        graph.add_edge("c", "t", edge("bridge", None));
        let stats = apply_edge_weights(&mut graph, CalibrationProfile::Calibrated);
        let weights: Vec<f64> = graph.edges().map(|e| e.data.weight).collect();
        let structural = 1.0 / 3f64.ln();
        assert_eq!(weights, [structural, structural, structural]);
        assert_eq!(
            crate::pyjson::dumps(&stats),
            "{\"edges_weighted\": 3, \"synthetic_floored\": 2, \"synthetic_per_class\": \
             {\"directory\": 1, \"bridge\": 1}, \"calibration_profile\": \"calibrated\", \
             \"min\": 0.9102, \"max\": 0.9102, \"mean\": 0.9102}"
        );
    }

    #[test]
    fn hubs_are_in_degree_outliers() {
        let mut graph = CodeGraph::new();
        for i in 0..30 {
            graph.add_edge(format!("n{i:02}"), "logger", EdgeData::default());
            graph.add_edge(format!("n{i:02}"), format!("m{i:02}"), EdgeData::default());
        }
        assert_eq!(detect_hubs(&graph, 3.0), ["logger"]);
        let mut flat = CodeGraph::new();
        flat.add_edge("a", "b", EdgeData::default());
        flat.add_edge("b", "c", EdgeData::default());
        flat.add_edge("c", "a", EdgeData::default());
        assert!(detect_hubs(&flat, 3.0).is_empty());
    }
}
