use super::*;
use crate::graph::{EdgeData, NodeData};

#[test]
fn the_profile_is_strict_about_its_value() {
    let lookup = |value: &'static str| move |_: &str| Some(value.to_owned());
    assert_eq!(
        CalibrationProfile::from_lookup(|_| None),
        Ok(CalibrationProfile::Calibrated)
    );
    assert_eq!(
        CalibrationProfile::from_lookup(lookup(" LEGACY ")),
        Ok(CalibrationProfile::Legacy)
    );
    assert!(CalibrationProfile::from_lookup(lookup("calibrate")).is_err());
}

#[test]
fn orphans_depend_on_the_profile() {
    let mut graph = CodeGraph::new();
    graph.add_node("lonely", NodeData::default());
    graph.add_edge("script", "lib", EdgeData::default());
    let calibrated = find_orphans(&graph, CalibrationProfile::Calibrated, false);
    assert_eq!(calibrated, ["lonely", "script"]);
    let legacy = find_orphans(&graph, CalibrationProfile::Legacy, false);
    assert_eq!(legacy, ["lonely"]);
    // `strict` ignores the profile.
    assert_eq!(
        find_orphans(&graph, CalibrationProfile::Calibrated, true),
        ["lonely"]
    );
}

#[test]
fn degrees_count_parallel_edges_and_self_loops() {
    let mut graph = CodeGraph::new();
    graph.add_edge("a", "b", EdgeData::default());
    graph.add_edge("a", "b", EdgeData::default());
    graph.add_edge("b", "b", EdgeData::default());
    let table = degrees(&graph);
    assert_eq!(table["a"], (0, 2));
    assert_eq!(table["b"], (3, 1));
}

#[test]
fn hubs_for_phase3_are_capped_at_twenty() {
    let outcome = Phase2Outcome {
        stats: Value::Null,
        hubs: (0..25).map(|i| format!("h{i:02}")).collect(),
        retyped: Vec::new(),
    };
    assert_eq!(outcome.hubs_for_phase3().len(), 20);
    assert_eq!(outcome.hubs_for_phase3()[19], "h19");
}
