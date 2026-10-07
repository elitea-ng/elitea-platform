//! Phase 2 of the Phase 1c fixture repository against the Python engine's,
//! with the index answering from a recording of the Python run.
//!
//! `tests/fixtures/phase2/` is what `parity/python_reference.py --through
//! phase2` wrote for `tests/fixtures/phase1c/repo`: `recording.jsonl`
//! (every index read Python's `run_phase2` made, with its answer),
//! `edges.jsonl` (the index's edges after Phase 2: weights, classes, the
//! orphan, doc and bridge edges) and `stats.json` (the dict `run_phase2`
//! returned). The node rows are Phase 1c's (`phase1c/expected`) with the
//! hubs flagged: Phase 2 writes nothing else to them.
//!
//! The fixture exercises the hybrid RRF pass (stored vectors and the
//! stand-in embedding fallback), the tiered lexical pass, directory
//! proximity, doc proximity (including the md5-picked root-doc anchors),
//! component bridging, the calibrated weights and a hub.

use elitea_deepwiki_engine::graph::builder;
use elitea_deepwiki_engine::graph::discover;
use elitea_deepwiki_engine::graph::flags::Phase1cFlags;
use elitea_deepwiki_engine::graph::topology::replay::{ReplayStore, StandinEmbedder};
use elitea_deepwiki_engine::graph::topology::{self, Phase2Config};
use elitea_deepwiki_engine::pyjson;
use serde_json::Value;
use std::collections::BTreeMap;
use std::path::PathBuf;

fn fixture(path: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures")
        .join(path)
}

fn read(path: &str) -> String {
    std::fs::read_to_string(fixture(path)).unwrap()
}

/// Rows as canonical text (keys sorted), sorted.
fn canonical(rows: impl IntoIterator<Item = Value>) -> Vec<String> {
    let mut out: Vec<String> = rows
        .into_iter()
        .map(|row| {
            let sorted: BTreeMap<String, Value> = match row {
                Value::Object(map) => map.into_iter().collect(),
                _ => BTreeMap::new(),
            };
            serde_json::to_string(&sorted).unwrap()
        })
        .collect();
    out.sort();
    out
}

fn lines(text: &str) -> Vec<Value> {
    text.lines()
        .map(|line| serde_json::from_str(line).unwrap())
        .collect()
}

#[test]
fn phase2_matches_the_python_reference() {
    let root = std::fs::canonicalize(fixture("phase1c/repo")).unwrap();
    let root = root.to_str().unwrap();
    let discovery = discover::discover_files(root);
    let parses = builder::parse_repository(&discovery);
    let (mut graph, _, _) =
        builder::build_index_graph(root, &discovery, parses, &Phase1cFlags::default());

    let mut store = ReplayStore::from_jsonl(&read("phase2/recording.jsonl")).unwrap();
    let mut embedder = StandinEmbedder;
    let outcome = topology::run_phase2(
        &mut graph,
        &mut store,
        Some(&mut embedder),
        &Phase2Config::default(),
    )
    .unwrap();

    // The stats dict, byte for byte as Python's `json.dumps`.
    assert_eq!(
        format!("{}\n", pyjson::dumps(&outcome.stats)),
        read("phase2/stats.json")
    );
    assert_eq!(outcome.hubs, ["go::server::Routes"]);
    assert_eq!(store.hubs, outcome.hubs);
    assert_eq!(
        store
            .meta
            .iter()
            .map(|(k, _)| k.as_str())
            .collect::<Vec<_>>(),
        ["phase2_completed", "phase2_stats"]
    );

    // The persisted edges.
    let got = canonical(
        store
            .edges
            .iter()
            .map(|row| serde_json::to_value(row).unwrap()),
    );
    let want = canonical(lines(&read("phase2/edges.jsonl")));
    for (got, want) in got.iter().zip(&want) {
        assert_eq!(got, want);
    }
    assert_eq!(got.len(), want.len());

    // The node rows: Phase 1c's, hubs flagged. A node the lexical pass
    // re-typed keeps its old type in the index.
    for (id, previous) in &outcome.retyped {
        graph.node_mut(id).unwrap().symbol_type = previous.clone();
    }
    let got = canonical(graph.node_rows().into_iter().map(|mut row| {
        if outcome.hubs.contains(&row.node_id) {
            row.is_hub = 1;
        }
        serde_json::to_value(row).unwrap()
    }));
    let want = canonical(
        lines(&read("phase1c/expected/nodes.jsonl"))
            .into_iter()
            .map(|mut row| {
                if outcome.hubs.iter().any(|h| row["node_id"] == h.as_str()) {
                    row["is_hub"] = Value::from(1);
                }
                row
            }),
    );
    assert_eq!(got, want);
}

#[test]
fn a_call_python_did_not_make_fails_the_replay() {
    let root = std::fs::canonicalize(fixture("phase1c/repo")).unwrap();
    let root = root.to_str().unwrap();
    let discovery = discover::discover_files(root);
    let parses = builder::parse_repository(&discovery);
    let (mut graph, _, _) =
        builder::build_index_graph(root, &discovery, parses, &Phase1cFlags::default());
    // An empty recording answers nothing.
    let mut store = ReplayStore::from_jsonl("").unwrap();
    let error =
        topology::run_phase2(&mut graph, &mut store, None, &Phase2Config::default()).unwrap_err();
    assert!(error.to_string().contains("not in the recording: get_node"));
}
