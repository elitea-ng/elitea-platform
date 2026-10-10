//! The semantic search tool against the Python engine's own answers, over
//! entities ranked in PostgreSQL.
//!
//! A deleted generator (`fixtures/PROVENANCE.md`) built `graph.json` (and
//! the bare `bare.json`) with the Python `KnowledgeGraph`, loaded them back
//! as production did, and recorded in `goldens.json` what the real Python
//! code answered (see the generator at that commit for the two things it
//! arranged: start-set order, and the chat closures around the wrapper
//! methods).

mod common;

use elitea_inventory_engine::graph::Graph;
use elitea_inventory_engine::retrieval::semantic;
use elitea_inventory_engine::retrieval::view::GraphView;
use elitea_inventory_engine::store::{self, GraphKey, vectors};
use serde_json::Value;

// The fixtures and the store-free goldens over them (pattern, community,
// admin) moved with the retrieval tools to libs/rust/inventory-core
// (ADR-0029 decision 7); the semantic goldens rank in PostgreSQL, so they
// are here.
const GRAPH: &str =
    include_str!("../../../libs/rust/inventory-core/tests/fixtures/retrieval_more/graph.json");
const BARE: &str =
    include_str!("../../../libs/rust/inventory-core/tests/fixtures/retrieval_more/bare.json");
const GOLDENS: &str =
    include_str!("../../../libs/rust/inventory-core/tests/fixtures/retrieval_more/goldens.json");

fn view_of(document: &str) -> GraphView {
    let document: Value = serde_json::from_str(document).expect("graph json");
    GraphView::new(Graph::from_node_link(&document).expect("node-link"), 1)
}

/// `GRAPH` saved to a fresh database (`None` without one), to rank in
/// PostgreSQL.
async fn stored(name: &str) -> Option<(sqlx::PgPool, GraphKey)> {
    let pool = common::database(name).await?;
    let key = GraphKey::new(1, 1).expect("key");
    let document: Value = serde_json::from_str(GRAPH).expect("graph json");
    store::save(
        &pool,
        key,
        &Graph::from_node_link(&document).expect("node-link"),
    )
    .await
    .expect("save");
    Some((pool, key))
}

async fn rank(
    pool: &sqlx::PgPool,
    key: GraphKey,
    vector: &[f64],
    min_score: f64,
) -> vectors::Ranking {
    vectors::rank(pool, key, vector, min_score)
        .await
        .expect("rank")
}

fn goldens() -> Value {
    serde_json::from_str(GOLDENS).expect("goldens")
}

fn text(value: &Value) -> &str {
    value.as_str().expect("a string")
}

fn opt<'a>(case: &'a Value, key: &str) -> Option<&'a str> {
    case.get(key).and_then(Value::as_str)
}

#[tokio::test]
async fn semantic_search_answers_as_the_python_wrapper() {
    let Some((pool, key)) = stored("semantic_goldens").await else {
        return;
    };
    let view = view_of(GRAPH);
    let goldens = goldens();
    for case in goldens["semantic"].as_array().expect("semantic") {
        let vector: Vec<f64> = case["vector"]
            .as_array()
            .expect("vector")
            .iter()
            .map(|v| v.as_f64().expect("a float"))
            .collect();
        let query = semantic::SemanticQuery {
            query: text(&case["query"]),
            top_k: case
                .get("top_k")
                .and_then(Value::as_u64)
                .map_or(10, |k| usize::try_from(k).expect("top_k")),
            entity_type: opt(case, "entity_type"),
            layer: opt(case, "layer"),
            file_pattern: opt(case, "file_pattern"),
            min_score: case
                .get("min_score")
                .and_then(Value::as_f64)
                .unwrap_or(semantic::DEFAULT_MIN_SCORE),
        };
        let ranking = rank(&pool, key, &vector, query.min_score).await;
        assert_eq!(
            semantic::semantic_search_text(&view, &query, &ranking),
            text(&case["text"]),
            "case {case}"
        );
    }
    let bare = view_of(BARE);
    let query = semantic::SemanticQuery {
        query: "auth",
        top_k: 10,
        entity_type: None,
        layer: None,
        file_pattern: None,
        min_score: semantic::DEFAULT_MIN_SCORE,
    };
    assert_eq!(
        semantic::semantic_search_text(&bare, &query, &Ok(Vec::new())),
        text(&goldens["semantic_unavailable"])
    );
}

#[tokio::test]
async fn the_semantic_tool_checks_the_space_and_caps_top_k() {
    let Some((pool, key)) = stored("semantic_tool").await else {
        return;
    };
    let view = view_of(GRAPH);
    assert_eq!(semantic::stamped_model(&view), Some("text-embed-x"));
    let ranking = rank(
        &pool,
        key,
        &[1.0, 0.0, 0.0, 0.0],
        semantic::DEFAULT_MIN_SCORE,
    )
    .await;
    let refused = semantic::semantic_search_tool(
        &view,
        "auth",
        &ranking,
        10,
        None,
        None,
        Some("all-MiniLM-L6-v2"),
    );
    assert!(refused.is_err());
    let answered = semantic::semantic_search_tool(
        &view,
        "auth",
        &ranking,
        500,
        None,
        None,
        Some("text-embed-x"),
    )
    .expect("same space");
    assert!(answered.starts_with("# Semantic Search Results for 'auth'"));
    let everything = rank(&pool, key, &[1.0, 0.0, 0.0, 0.0], 0.0).await;
    let hits = semantic::semantic_search(
        &view,
        &semantic::SemanticQuery {
            query: "auth",
            top_k: 2,
            entity_type: None,
            layer: None,
            file_pattern: None,
            min_score: 0.0,
        },
        &everything,
    )
    .expect("hits");
    assert_eq!(hits.len(), 2);
    assert_eq!(hits[0].id, "n3");

    // Another width than the graph's is numpy's error, and the zero vector
    // ranks nothing.
    let wider = rank(&pool, key, &[1.0, 0.0, 0.0, 0.0, 0.0], 0.0).await;
    assert_eq!(
        wider,
        Err("shapes (5,) and (4,) not aligned: 5 (dim 0) != 4 (dim 0)".to_owned())
    );
    assert_eq!(rank(&pool, key, &[0.0; 4], 0.0).await, Ok(Vec::new()));
}
