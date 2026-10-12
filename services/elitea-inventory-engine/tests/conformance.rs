//! The third Inventory fixture runner, held to the goldens the other two are
//! (`conformance/provider/fixtures/inventory/`, README there): the Go host's
//! `fixture_parity_test.go` and the Python engine's `test_fixture_parity.py`
//! read the same files and assert the same shapes.

use elitea_inventory_engine::fixture::{self, FixtureGraph, PACKAGED_GRAPH};
use serde_json::{Map, Value, json};
use sha2::{Digest, Sha256};
use std::path::PathBuf;

fn conformance(parts: &[&str]) -> PathBuf {
    let mut path = PathBuf::from(env!("CARGO_MANIFEST_DIR"));
    path.extend([
        "..",
        "..",
        "conformance",
        "provider",
        "fixtures",
        "inventory",
    ]);
    path.extend(parts);
    path
}

fn read(path: &PathBuf) -> Value {
    let text = std::fs::read_to_string(path).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
    serde_json::from_str(&text).unwrap_or_else(|e| panic!("{}: {e}", path.display()))
}

fn without_comment(mut value: Value) -> Value {
    if let Value::Object(fields) = &mut value {
        fields.remove("_comment");
    }
    value
}

/// The packaged copy is the conformance file — editing one alone is the
/// drift these fixtures exist to catch.
#[test]
fn the_packaged_graph_is_the_conformance_graph() {
    let packaged: Value = serde_json::from_str(PACKAGED_GRAPH).unwrap_or_else(|e| panic!("{e}"));
    assert_eq!(
        without_comment(packaged),
        without_comment(read(&conformance(&["spi", "graph.json"])))
    );
}

/// `run_ingestion` is compared as the Python suite compares it: through the
/// artifacts it hands back.
fn ingestion_shape(result: &Value) -> Value {
    let mut shape = Map::new();
    shape.insert("result".to_owned(), result["result"].clone());
    for artifact in result["artifacts"].as_array().into_iter().flatten() {
        let name = artifact["name"].as_str().unwrap_or_default();
        let document: Value = serde_json::from_str(artifact["data"].as_str().unwrap_or("null"))
            .unwrap_or_else(|e| panic!("{name}: {e}"));
        if name == "graph.json" {
            shape.insert("graph_metadata".to_owned(), document["_metadata"].clone());
            shape.insert(
                "source_label".to_owned(),
                document["_metadata"]["ingested_source"].clone(),
            );
        } else if name == "sources_status.json" {
            shape.insert("sources_status".to_owned(), document);
        } else if name.starts_with(".ingestion-checkpoint-") {
            shape.insert("checkpoint".to_owned(), document);
        }
    }
    Value::Object(shape)
}

#[test]
fn every_golden_answer_is_this_runners_answer() {
    let graph = FixtureGraph::parse(PACKAGED_GRAPH).unwrap_or_else(|e| panic!("{e}"));
    let mut checked = 0;
    for kind in ["ingestion", "retrieval", "transfer"] {
        let mut files: Vec<PathBuf> = std::fs::read_dir(conformance(&[kind]))
            .unwrap_or_else(|e| panic!("{kind}: {e}"))
            .filter_map(Result::ok)
            .map(|entry| entry.path())
            .filter(|path| path.extension().is_some_and(|ext| ext == "json"))
            .collect();
        files.sort();
        assert!(!files.is_empty(), "no {kind}/ fixtures found");
        for path in files {
            let case = read(&path);
            let tool = case["tool"].as_str().unwrap_or_default();
            let params = case["params"].as_object().cloned().unwrap_or_default();
            let handler = fixture::handler(tool).unwrap_or_else(|| panic!("{tool}: no handler"));
            let result = handler(&graph, &params);
            assert_eq!(result["success"], json!(true), "{tool}: {result}");
            let got = if tool == "run_ingestion" {
                ingestion_shape(&result)
            } else {
                serde_json::from_str(result["result"].as_str().unwrap_or("null"))
                    .unwrap_or_else(|e| panic!("{tool}: {e}"))
            };
            assert_eq!(got, case["expected"], "{}", path.display());
            if let Some(expected) = case.get("artifacts") {
                // The artifacts ride beside the result: name, type and the
                // sha256 of `data` (Python `json.dumps` bytes) are the golden.
                let ours: Vec<Value> = result["artifacts"]
                    .as_array()
                    .into_iter()
                    .flatten()
                    .map(|artifact| {
                        let digest = Sha256::digest(artifact["data"].as_str().unwrap_or_default());
                        json!({
                            "name": artifact["name"],
                            "type": artifact["type"],
                            "data_sha256": format!("{digest:x}"),
                        })
                    })
                    .collect();
                assert_eq!(json!(ours), *expected, "{} artifacts", path.display());
            }
            checked += 1;
        }
    }
    assert!(checked >= 11, "only {checked} golden files were checked");
}

/// A JSON answer is Python's `json.dumps` byte for byte (", " and ": "
/// separators, ASCII escapes), which is what the legacy result string was.
#[test]
fn a_json_answer_is_python_json_dumps() {
    let graph = FixtureGraph::parse(PACKAGED_GRAPH).unwrap_or_else(|e| panic!("{e}"));
    let handler = fixture::handler("get_preset_info").unwrap_or_else(|| unreachable!());
    let params = json!({"preset": "code", "output_format": "json"});
    let result = handler(&graph, params.as_object().unwrap_or_else(|| unreachable!()));
    let text = result["result"].as_str().unwrap_or_default();
    assert!(
        text.starts_with("{\"preset\": \"code\", \"description\": "),
        "{text}"
    );
}
