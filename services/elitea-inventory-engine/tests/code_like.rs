//! Code files without a dedicated parser (`.sh`, `.rb`, `.lua`, C, …)
//! against what the Python engine's real ingestion path made of them
//! (its deleted generator ran `_process_file_with_chunks`; see
//! `fixtures/PROVENANCE.md`).
//!
//! Python's `run()` gave such a file its file node and the model stage
//! (with the code fact prompt), and nothing from a parser. Its "hybrid
//! fallback" (`TextParser` over the file) sat on a path only the never-served
//! `delta_update` reached, and `TextParser` raised on the first reference it
//! found anyway; the golden records that error. So the port selects these
//! files, gives them their file node and the model stage, and adds no
//! text-reference relations — the same graph.

use elitea_engine_core::stream::{Context, StopSignal};
use elitea_inventory_engine::extract::is_code_like;
use elitea_inventory_engine::graph::Graph;
use elitea_inventory_engine::ingest::files::has_supported_extension;
use elitea_inventory_engine::ingest::ingest_tree;
use elitea_inventory_engine::ingest::parse::language_of;
use elitea_inventory_engine::ingest::source::Source;
use serde_json::{Value, json};
use std::collections::BTreeMap;

const GOLDENS: &str = include_str!("fixtures/code_like/goldens.json");

fn goldens() -> Value {
    serde_json::from_str(GOLDENS).expect("goldens")
}

#[test]
fn code_like_paths_are_classified_as_python_classified_them() {
    for case in goldens()["classification"].as_array().expect("cases") {
        let path = case["path"].as_str().expect("path");
        let code_file = case["code_file"].as_bool().expect("code_file");
        assert_eq!(
            is_code_like(path),
            case["code_like"] == json!(true),
            "{path}"
        );
        // `_is_code_file` is exactly "a parser language exists".
        assert_eq!(language_of(path).is_some(), code_file, "{path}");
    }
}

#[test]
fn a_code_file_without_a_parser_gets_its_file_node_and_no_parser_relations() {
    let goldens = goldens();
    let files = goldens["files"].as_array().expect("files");
    let root = std::env::temp_dir().join(format!("inv-code-like-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&root);
    for file in files {
        let path = root.join(file["path"].as_str().expect("path"));
        std::fs::create_dir_all(path.parent().expect("parent")).expect("dirs");
        std::fs::write(&path, file["text"].as_str().expect("text")).expect("write");
        // TextParser raised on every one of them: Python added nothing.
        assert_eq!(file["text_parser"]["error_type"], json!("TypeError"));
        assert_eq!(file["relations"], json!([]));
    }
    let source = Source::parse(
        Some(&json!({"toolkit_id": 5, "type": "github", "name": "repo",
                     "github_configuration": {}, "repository": "o/r"})),
        &["github".to_owned()],
    )
    .expect("a source");
    let (sender, _receiver) = tokio::sync::mpsc::unbounded_channel();
    let context = Context::new(sender, StopSignal::default());
    let mut graph = Graph::new();
    let outcome =
        ingest_tree(&mut graph, &source, &root, &BTreeMap::new(), &context).expect("ingests");

    let mut selected = 0;
    for file in files {
        let path = file["path"].as_str().expect("path");
        // A Makefile has no extension: the SDK loader never selected it, so
        // the Python pipeline never saw one from a repository either.
        if !has_supported_extension(path) {
            assert_eq!(path, "Makefile");
            continue;
        }
        selected += 1;
        assert_eq!(
            outcome.hashes.get(path),
            file["content_hash"].as_str().map(str::to_owned).as_ref(),
            "{path}"
        );
        for entity in file["entities"].as_array().expect("entities") {
            let id = entity["id"].as_str().expect("id");
            let node = graph
                .node(id)
                .unwrap_or_else(|| panic!("{path}: entity {id}"));
            assert_eq!(node["name"], entity["name"], "{path}");
            // The type the graph keeps (`add_entity` normalises: `file` is
            // stored as `resource`).
            assert_eq!(node["type"], entity["stored_type"], "{path}");
        }
    }
    assert_eq!(outcome.documents_processed, selected);
    assert_eq!(graph.node_count(), selected, "file nodes only");
    assert_eq!(graph.edge_count(), 0, "no parser relations");
    assert_eq!(outcome.relations_added, 0);
    let _ = std::fs::remove_dir_all(&root);
}
