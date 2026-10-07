//! The query tools over PostgreSQL (the live read path) against the Python
//! tools over their `.wiki.db`, for the same calls.
//!
//! The rows of the recorded run go through the build space and the publish
//! transaction; then every call of `tests/fixtures/ask/calls.json` runs
//! against [`PgIndex`] and is compared with the Python text.
//!
//! What may differ is pinned, call by call, in [`EXPECTED_DIFFERENCES`],
//! with the reason, and the test prints the report:
//!
//! * FTS ranking: FTS5's `bm25()` over its own tokens against the folded
//!   `tsvector` ranked by the `'fts'` statistics (Porter vs Snowball, ties
//!   on node id instead of rowid), so the ORDER of lexical hits, and the
//!   hits FTS5 read as a phrase, differ;
//! * the published edges carry no annotations (no `via` anchors) and
//!   parallel edges are collapsed (`publish.py` did both).
//!
//! Every other call must be byte-identical. Needs PostgreSQL with pgvector
//! (see `tests/storage_common`); CI runs it.

mod ask_common;
mod storage_common;

use ask_common as common;
use elitea_deepwiki_engine::ask::agent::Clock;
use elitea_deepwiki_engine::ask::embed::{Embedder, stand_in_embedding};
use elitea_deepwiki_engine::ask::store::{IndexStore, PgIndex, node_from_dump};
use elitea_deepwiki_engine::ask::tools::Codebase;
use elitea_deepwiki_engine::ask::{self, Limits, args};
use elitea_deepwiki_engine::runner::StopSignal;
use elitea_deepwiki_engine::storage::adapter::UnifiedDb;
use elitea_deepwiki_engine::storage::build::{BuildSpace, WikiRecord};
use elitea_deepwiki_engine::storage::rows::{IndexEdge, IndexNode};
use serde_json::{Value, json};

const WIKI: &str = "acme--notes--main";

/// `(tool, arguments as JSON text)` of the calls whose text may differ,
/// with why.
///
/// Measured 2026-10-05 (PostgreSQL 18.3, pgvector 0.8.1): 27 of 33 calls
/// identical.
const EXPECTED_DIFFERENCES: &[(&str, &str, &str)] = &[
    (
        "search_symbols",
        r#"{"query":"note store"}"#,
        "FTS ranking: the same 4 symbols, NoteStore and BaseStore swapped",
    ),
    (
        "get_relationships_tool",
        r#"{"symbol_name":"NoteStore"}"#,
        "no `via` anchors on published edges; the same 43 edges in the same order",
    ),
    (
        "get_relationships_tool",
        r#"{"symbol_name":"MAX_NOTES","max_depth":1}"#,
        "no `via` anchors on published edges",
    ),
    (
        "query_graph",
        r#"{"expression":"text:note"}"#,
        "FTS ranking: the same 20 symbols, two markdown sections ranked higher",
    ),
    (
        "search_codebase",
        r#"{"query":"note store save"}"#,
        "FTS ranking of the OR query: the same 10 items, three reordered",
    ),
    (
        "get_symbol_relationships",
        r#"{"symbol_name":"NoteStore"}"#,
        "edge order: the published table returns one outgoing edge earlier (no rowid)",
    ),
];

async fn published(name: &str) -> Option<PgIndex> {
    let pool = storage_common::fresh_database(name).await?;
    let (nodes, edges) = common::rows();
    let space = BuildSpace::new(pool.clone(), "ask");
    let mut build = space.begin(WIKI).await.expect("begin");
    let nodes: Vec<IndexNode> = nodes
        .iter()
        .map(|row| {
            let n = node_from_dump(row).expect("node");
            IndexNode {
                node_id: n.node_id,
                rel_path: n.rel_path,
                file_name: n.file_name,
                language: n.language,
                start_line: n.start_line,
                end_line: n.end_line,
                symbol_name: n.symbol_name,
                symbol_type: n.symbol_type,
                parent_symbol: n.parent_symbol,
                source_text: n.source_text,
                docstring: n.docstring,
                signature: n.signature,
                chunk_type: n.chunk_type,
                macro_cluster: n.macro_cluster,
                micro_cluster: n.micro_cluster,
                is_architectural: n.is_architectural,
                is_doc: n.is_doc,
                is_test: n.is_test,
            }
        })
        .collect();
    build.stage_nodes(nodes).await.expect("stage nodes");
    let edges: Vec<IndexEdge> = edges
        .iter()
        .map(|e| IndexEdge {
            source_id: e["source_id"].as_str().unwrap_or_default().to_owned(),
            target_id: e["target_id"].as_str().unwrap_or_default().to_owned(),
            rel_type: e["rel_type"].as_str().unwrap_or_default().to_owned(),
            edge_class: e["edge_class"].as_str().map(str::to_owned),
            weight: e["weight"].as_f64().unwrap_or(1.0),
        })
        .collect();
    let edges = elitea_deepwiki_engine::storage::rows::collapse_edges(edges);
    build.stage_edges(&edges).await.expect("stage edges");
    let vectors: Vec<(String, Vec<f64>)> = common::jsonl("ask/ref/embeddings.jsonl")
        .iter()
        .map(|r| {
            (
                r["node_id"].as_str().expect("id").to_owned(),
                r["embedding"]
                    .as_array()
                    .expect("vector")
                    .iter()
                    .map(|v| v.as_f64().expect("number"))
                    .collect(),
            )
        })
        .collect();
    build
        .stage_embeddings(vectors.iter().map(|(id, v)| (id.as_str(), v.as_slice())))
        .await
        .expect("stage embeddings");
    build
        .publish(&WikiRecord::default())
        .await
        .expect("publish");
    Some(PgIndex::new(UnifiedDb::new(pool, WIKI)))
}

#[tokio::test(flavor = "multi_thread")]
async fn the_tools_over_postgresql_match_python() {
    let Some(index) = published("ask_tools").await else {
        return;
    };
    assert!(index.wiki_exists().await.expect("exists"));
    let embedder = Embedder::Fixed(stand_in_embedding);
    let stop = StopSignal::default();
    let codebase = Codebase {
        store: &index,
        embedder: &embedder,
        stop: &stop,
        doc_results: Limits::default().doc_results,
    };
    let mut identical = 0;
    let mut differing: Vec<(String, String)> = Vec::new();
    for record in common::jsonl("ask/ref/tools.jsonl") {
        let name = record["name"].as_str().expect("name").to_owned();
        let raw = record["arguments"].as_object().cloned().unwrap_or_default();
        let parsed = args::validate(args::params(&name).expect("tool"), &raw).expect("valid");
        let text = codebase.run(&name, &parsed).await.expect("runs");
        let key = Value::Object(raw).to_string();
        let python = record["result"].as_str().unwrap_or_default();
        if text.as_deref() == Some(python) {
            identical += 1;
        } else {
            let anchors = EXPECTED_DIFFERENCES
                .iter()
                .any(|(n, a, why)| *n == name && *a == key && why.starts_with("no `via`"));
            if anchors {
                // Only the anchors may differ.
                let stripped: Vec<&str> = python
                    .lines()
                    .map(|l| l.split(" via ").next().unwrap_or(l))
                    .collect();
                assert_eq!(
                    text.as_deref()
                        .unwrap_or_default()
                        .lines()
                        .collect::<Vec<_>>(),
                    stripped,
                    "{name} {key}: more than the `via` anchors differ"
                );
            }
            eprintln!(
                "DIFFERS {name} {key}\n--- python\n{}\n--- postgresql\n{}\n",
                record["result"].as_str().unwrap_or_default(),
                text.unwrap_or_default()
            );
            differing.push((name, key));
        }
    }
    eprintln!(
        "tool results: {identical} identical, {} differ (FTS ranking / edge annotations)",
        differing.len()
    );
    // A ranking can agree on another server; nothing else may differ.
    for (name, key) in &differing {
        assert!(
            EXPECTED_DIFFERENCES
                .iter()
                .any(|(n, a, _)| n == name && a == key),
            "{name} {key} differs from Python and is not a known difference"
        );
    }
    assert!(identical >= 27, "only {identical} calls are identical");
}

#[tokio::test(flavor = "multi_thread")]
async fn ask_and_deep_research_run_over_postgresql() {
    let Some(index) = published("ask_agents").await else {
        return;
    };
    let script = common::json_file("ask/script.json");
    let payload = json!({
        "question": script["question"],
        "repo_config": {"repository": "acme/notes", "branch": "main"},
        "repo_identifier_override": "acme/notes:main:01234567",
    });
    let request = ask::parse_request(payload.as_object().expect("object")).expect("request");
    let clock = Clock::Fixed {
        date: "2026-01-02".into(),
        timestamp: "T".into(),
    };
    let embedder = Embedder::Fixed(stand_in_embedding);
    for (tool, spec) in [
        (
            "ask",
            ask::ask_spec(
                &request,
                None,
                "gpt-4o",
                false,
                true,
                Limits::default(),
                clock.clone(),
            ),
        ),
        (
            "deep_research",
            ask::research_spec(
                &request,
                None,
                "gpt-4o",
                false,
                Limits::default(),
                clock.clone(),
            ),
        ),
    ] {
        let spec = spec.expect("spec");
        let model = common::ScriptedModel::new(&script[tool]);
        let (context, _lines, _) = common::context();
        let result = ask::run_agent(&spec, &request, &model, &index, &embedder, &context)
            .await
            .expect("runs");
        assert_eq!(result["success"], json!(true), "{tool}: {result}");
        let python = common::jsonl(&format!("ask/ref/{tool}/requests.jsonl"));
        let ours = model.bodies();
        assert_eq!(ours.len(), python.len(), "{tool}");
        // The prompts and tool definitions are the backend's to keep.
        for (a, b) in ours.iter().zip(&python) {
            assert_eq!(a["tools"], b["tools"]);
            assert_eq!(a["messages"][0], b["messages"][0]);
            assert_eq!(a["messages"][1], b["messages"][1]);
        }
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn an_unpublished_wiki_is_reported_not_found() {
    let Some(pool) = storage_common::fresh_database("ask_missing").await else {
        return;
    };
    let index = PgIndex::new(UnifiedDb::new(pool, "nobody--nothing--main"));
    let payload = json!({"question": "q", "repo_config": {"repository": "nobody/nothing"}});
    let request = ask::parse_request(payload.as_object().expect("object")).expect("request");
    let spec = ask::ask_spec(
        &request,
        None,
        "gpt-4o",
        false,
        true,
        Limits::default(),
        Clock::System,
    )
    .expect("spec");
    let model = common::ScriptedModel::new(&json!([]));
    let (context, _lines, _) = common::context();
    let result = ask::run_agent(&spec, &request, &model, &index, &Embedder::None, &context)
        .await
        .expect("runs");
    assert_eq!(result["success"], json!(false));
    assert_eq!(result["error_category"], json!("resource_not_found"));
    assert_eq!(
        result["error"],
        json!("No wiki index found for nobody/nothing:main. Please generate a wiki first.")
    );
    assert!(
        model.bodies().is_empty(),
        "no model call for a missing wiki"
    );
}

/// A published wiki of `nodes` only, staged in this order.
async fn published_nodes(name: &str, wiki: &str, nodes: &[(&str, &str)]) -> Option<PgIndex> {
    let pool = storage_common::fresh_database(name).await?;
    let space = BuildSpace::new(pool.clone(), "ask");
    let mut build = space.begin(wiki).await.expect("begin");
    let nodes: Vec<IndexNode> = nodes
        .iter()
        .map(|(id, symbol)| IndexNode {
            node_id: (*id).to_owned(),
            rel_path: "src/lib.py".to_owned(),
            file_name: "lib.py".to_owned(),
            language: "python".to_owned(),
            start_line: 1,
            end_line: 2,
            symbol_name: (*symbol).to_owned(),
            symbol_type: "class".to_owned(),
            source_text: format!("class {symbol}: pass"),
            ..IndexNode::default()
        })
        .collect();
    build.stage_nodes(nodes).await.expect("stage nodes");
    build
        .publish(&WikiRecord::default())
        .await
        .expect("publish");
    Some(PgIndex::new(UnifiedDb::new(pool, wiki)))
}

fn ids(rows: &[elitea_deepwiki_engine::ask::store::NodeRecord]) -> Vec<&str> {
    rows.iter().map(|n| n.node_id.as_str()).collect()
}

#[tokio::test(flavor = "multi_thread")]
async fn a_name_with_a_non_ascii_capital_is_found() {
    let Some(index) = published_nodes(
        "ask_unicode",
        "acme--umlaut--main",
        &[("n-arger", "Ärger"), ("n-other", "Other")],
    )
    .await
    else {
        return;
    };
    // Both sides go through the same lower(): the name as written and an
    // ASCII-only change of case match under any server locale.
    for name in ["Ärger", "ÄRGER"] {
        let exact = index.name_rows(name, true, 5).await.expect("exact");
        assert_eq!(ids(&exact), ["n-arger"], "{name}");
    }
    let like = index.name_rows("RGE", false, 5).await.expect("like");
    assert_eq!(ids(&like), ["n-arger"]);
    let shortest = index.shortest_like("Ärg").await.expect("shortest");
    assert_eq!(shortest.map(|n| n.node_id).as_deref(), Some("n-arger"));
    // LIKE's own wildcards keep their meaning (Python passed them on), and
    // a backslash is literal.
    let wildcard = index.name_rows("Ä_ger", false, 5).await.expect("wildcard");
    assert_eq!(ids(&wildcard), ["n-arger"]);
    let backslash = index.name_rows("\\", false, 5).await.expect("backslash");
    assert!(backslash.is_empty());
}

#[tokio::test(flavor = "multi_thread")]
async fn the_shortest_name_ties_break_on_node_id() {
    // Staged (and so stored) in the reverse of the id order: the heap
    // order would give "z-box".
    let Some(index) = published_nodes(
        "ask_ties",
        "acme--ties--main",
        &[("z-box", "BoxB"), ("a-box", "BoxA"), ("m-box", "LongBox")],
    )
    .await
    else {
        return;
    };
    let shortest = index.shortest_like("box").await.expect("shortest");
    assert_eq!(shortest.map(|n| n.node_id).as_deref(), Some("a-box"));
}

/// An embedding of the wrong length: pgvector refuses the vector search.
fn wrong_length_embedding(_: &str) -> Vec<f64> {
    vec![1.0, 0.0, 0.0]
}

#[tokio::test(flavor = "multi_thread")]
async fn a_failed_document_search_keeps_the_keyword_results() {
    let Some(index) = published("ask_doc_search_fails").await else {
        return;
    };
    let embedder = Embedder::Fixed(wrong_length_embedding);
    let stop = StopSignal::default();
    let codebase = Codebase {
        store: &index,
        embedder: &embedder,
        stop: &stop,
        doc_results: Limits::default().doc_results,
    };
    let mut raw = serde_json::Map::new();
    raw.insert(
        "query".to_owned(),
        Value::String("note store save".to_owned()),
    );
    let parsed =
        args::validate(args::params("search_codebase").expect("tool"), &raw).expect("valid");
    let text = codebase
        .run("search_codebase", &parsed)
        .await
        .expect("runs")
        .unwrap_or_default();
    assert!(!text.starts_with("Search failed"), "{text}");
    assert!(text.contains("(unified_db_fts)"), "{text}");
    assert!(text.contains("`save` (method)"), "{text}");
}
