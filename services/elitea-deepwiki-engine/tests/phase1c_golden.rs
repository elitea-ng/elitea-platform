//! The index graph (Phase 1 + Phase 1c) of a small polyglot repository
//! against the Python engine's.
//!
//! `tests/fixtures/phase1c/repo` exercises every Phase 1c pass: `FastAPI`
//! routes under a router prefix, a `ctypes` library, a Pylon class API with
//! a `metadata.json` plugin name, `fetch` / `axios` / request-options
//! clients, a Go gRPC server interface and `gin` routes, a Rust `extern
//! "C"` export, data shapes shared by four languages, and markdown files
//! with links and backtick names (one headerless). `expected/` is what
//! `parity/python_reference.py` wrote for it — the rows the Python index
//! stores. This engine's own parsers run here, so the test covers the
//! parsers, the builder and Phase 1c together; every row must match.

use elitea_deepwiki_engine::graph::builder;
use elitea_deepwiki_engine::graph::discover;
use elitea_deepwiki_engine::graph::flags::Phase1cFlags;
use serde_json::{Map, Value};
use std::collections::BTreeMap;
use std::path::PathBuf;

fn fixture(path: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures/phase1c")
        .join(path)
}

/// Rows as canonical text (keys sorted), sorted, so ties in the reference's
/// row order do not matter.
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

fn expected(name: &str) -> Vec<String> {
    let text = std::fs::read_to_string(fixture(&format!("expected/{name}"))).unwrap();
    canonical(text.lines().map(|line| serde_json::from_str(line).unwrap()))
}

fn rows<T: serde::Serialize>(rows: Vec<T>, drop: &[&str]) -> Vec<String> {
    canonical(rows.into_iter().map(|row| {
        let mut value = serde_json::to_value(row).unwrap();
        if let Some(map) = value.as_object_mut() {
            for key in drop {
                map.remove(*key);
            }
        }
        value
    }))
}

#[test]
fn the_index_graph_matches_the_python_reference() {
    let root = std::fs::canonicalize(fixture("repo")).unwrap();
    let root = root.to_str().unwrap();
    let discovery = discover::discover_files(root);
    let parses = builder::parse_repository(&discovery);
    let (graph, _, report) =
        builder::build_index_graph(root, &discovery, parses, &Phase1cFlags::default());
    assert_eq!(report.contract_nodes, 34);
    assert_eq!(report.cross_language_edges, 6);
    assert_eq!(report.markdown.documents_synthesized, 3);

    let got_nodes = rows(graph.node_rows(), &[]);
    let want_nodes = expected("nodes.jsonl");
    for (got, want) in got_nodes.iter().zip(&want_nodes) {
        assert_eq!(got, want);
    }
    assert_eq!(got_nodes.len(), want_nodes.len());

    let got_edges = rows(graph.edge_rows(), &[]);
    let want_edges = expected("edges.jsonl");
    for (got, want) in got_edges.iter().zip(&want_edges) {
        assert_eq!(got, want);
    }
    assert_eq!(got_edges.len(), want_edges.len());

    // The in-memory attributes later phases read: surfaces on their owners.
    // (A decorated Python function's slice starts at `def`, below its
    // route decorator, so `read_me` has none — in Python too.)
    assert!(
        !graph
            .node("python::users::read_me")
            .unwrap()
            .extra
            .contains_key("api_surface")
    );
    let owner = graph.node("python::users::hash_user").unwrap();
    let surfaces = owner.extra["api_surface"].as_array().unwrap();
    assert_eq!(surfaces[0]["surface"], "ffi:compute_hash");
    let _: &Map<String, Value> = surfaces[0]["metadata"].as_object().unwrap();
}

#[test]
fn without_phase1c_no_contract_or_markdown_structure_is_added() {
    let root = std::fs::canonicalize(fixture("repo")).unwrap();
    let root = root.to_str().unwrap();
    let discovery = discover::discover_files(root);
    let parses = builder::parse_repository(&discovery);
    let (graph, _, report) =
        builder::build_index_graph(root, &discovery, parses, &Phase1cFlags::none());
    assert!(report.timings.is_empty());
    assert!(
        graph
            .nodes()
            .all(|(id, _)| !id.starts_with("contract::") && !id.starts_with("markdown_document::"))
    );
    assert_eq!(
        graph.edges().filter(|e| e.data.edge_class == "doc").count(),
        0
    );
}

#[test]
fn the_per_language_build_is_the_same_graph() {
    let root = std::fs::canonicalize(fixture("repo")).unwrap();
    let root = root.to_str().unwrap();
    let discovery = discover::discover_files(root);
    let flags = Phase1cFlags::default();
    let parses = builder::parse_repository(&discovery);
    let (whole, _, _) = builder::build_index_graph(root, &discovery, parses, &flags);
    let (streamed, report, _) = builder::build_index_graph_parsed(root, &discovery, &flags);
    assert!(report.rich_files > 0);
    assert_eq!(streamed.node_rows(), whole.node_rows());
    assert_eq!(streamed.edge_rows(), whole.edge_rows());
}
