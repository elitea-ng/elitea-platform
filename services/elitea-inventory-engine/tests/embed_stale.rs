//! A changed entity text re-embeds (ADR-0031 decision 7, phase C0).
//!
//! `embed_graph` embedded only the entities without a vector, so an entity
//! whose name or description changed kept the vector of its old text. The
//! graph now records a hash of the composed text beside each vector.
//!
//! The first three tests need no database; the last needs
//! `INVENTORY_TEST_DSN` (see `graph_store.rs`).

mod common;

use axum::extract::State;
use elitea_engine_core::stream::StopSignal;
use elitea_inventory_engine::embed::{compose_text, embed_graph};
use elitea_inventory_engine::graph::Graph;
use elitea_inventory_engine::store::{self, GraphKey};
use elitea_model_client::embeddings::{EmbeddingClient, EmbeddingOptions};
use elitea_model_client::settings::ModelSettings;
use elitea_model_client::transport::{Transport, TransportSettings};
use serde_json::{Value, json};
use std::sync::{Arc, Mutex};

type Seen = Arc<Mutex<Vec<String>>>;

/// A gateway whose `/v1/embeddings` records every input it is asked to
/// embed, and answers a 4-wide vector per input. Returns the client over it
/// and the record.
async fn client(model: &str) -> (EmbeddingClient, Seen) {
    async fn embeddings(
        State(seen): State<Seen>,
        axum::Json(body): axum::Json<Value>,
    ) -> axum::Json<Value> {
        let inputs = body["input"].as_array().cloned().unwrap_or_default();
        seen.lock().expect("lock").extend(
            inputs
                .iter()
                .map(|input| input.as_str().unwrap_or_default().to_owned()),
        );
        let data: Vec<Value> = inputs
            .iter()
            .enumerate()
            .map(|(index, _)| json!({"object": "embedding", "index": index, "embedding": [0.1, 0.2, 0.3, 0.4]}))
            .collect();
        axum::Json(
            json!({"object": "list", "data": data, "model": body["model"], "usage": {"prompt_tokens": 1, "total_tokens": 1}}),
        )
    }
    let seen: Seen = Arc::default();
    let app = axum::Router::new()
        .route("/v1/embeddings", axum::routing::post(embeddings))
        .with_state(Arc::clone(&seen));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    let port = listener.local_addr().expect("addr").port();
    tokio::spawn(async move { axum::serve(listener, app).await.expect("serve") });
    let settings = ModelSettings::from_llm_settings(&json!({
        "api_base": format!("http://127.0.0.1:{port}/v1"),
        "api_key": "k",
        "model_name": model,
        "streaming": false,
    }))
    .expect("llm settings");
    let transport = Transport::new(&TransportSettings::default()).expect("transport");
    (
        EmbeddingClient::new(transport, settings, model, EmbeddingOptions::default()),
        seen,
    )
}

fn graph() -> Graph {
    let mut graph = Graph::new();
    graph.add_entity("a", "Alpha", "class", None, None);
    graph.add_entity("b", "Beta", "function", None, None);
    graph
}

fn describe(graph: &mut Graph, id: &str, description: &str) {
    let (_, node) = graph
        .nodes_mut()
        .find(|(node_id, _)| *node_id == id)
        .expect("the entity");
    node.insert("description".to_owned(), json!(description));
}

async fn embed(graph: &mut Graph, client: &EmbeddingClient) -> usize {
    embed_graph(graph, client, "2026-10-10T00:00:00", &StopSignal::default())
        .await
        .expect("embeds")
}

#[tokio::test]
async fn an_unchanged_graph_is_not_embedded_twice() {
    let (client, seen) = client("embed-model").await;
    let mut graph = graph();
    assert_eq!(embed(&mut graph, &client).await, 2);
    assert_eq!(seen.lock().expect("lock").len(), 2);
    assert_eq!(embed(&mut graph, &client).await, 0, "nothing changed");
    assert_eq!(seen.lock().expect("lock").len(), 2, "no call went out");
}

#[tokio::test]
async fn a_changed_entity_text_is_embedded_again_and_only_that_entity() {
    let (client, seen) = client("embed-model").await;
    let mut graph = graph();
    embed(&mut graph, &client).await;
    seen.lock().expect("lock").clear();

    describe(&mut graph, "b", "now it does something");
    assert_eq!(embed(&mut graph, &client).await, 1);
    let asked = seen.lock().expect("lock").clone();
    assert_eq!(asked, vec!["Beta function now it does something"]);
    // The hash follows the new text, so the next pass is quiet again.
    let node = graph.node("b").expect("b").clone();
    assert_eq!(
        graph.embedding_hash("b"),
        Some(elitea_inventory_engine::embed::text_hash(&compose_text(&node)).as_str())
    );
    assert_eq!(embed(&mut graph, &client).await, 0);
}

#[tokio::test]
async fn a_vector_with_no_recorded_hash_is_embedded_once() {
    let (client, seen) = client("embed-model").await;
    let mut graph = graph();
    // Embedded before the hash existed: a vector, a stamp, and no hash.
    for id in ["a", "b"] {
        assert!(graph.set_embedding(id, &[1.0, 2.0, 3.0, 4.0]));
    }
    graph
        .metadata
        .insert("embeddings_model".to_owned(), json!("embed-model"));
    assert!(graph.embedding_hash("a").is_none());
    assert_eq!(embed(&mut graph, &client).await, 2, "once, to record it");
    assert_eq!(seen.lock().expect("lock").len(), 2);
    assert!(graph.embedding_hash("a").is_some());
    assert_eq!(embed(&mut graph, &client).await, 0, "and not again");
}

#[tokio::test]
async fn the_hash_is_stored_with_the_vector_and_not_without_one() {
    let Some(pool) = common::database("embed_hash").await else {
        return;
    };
    let key = GraphKey::new(1, 1).expect("key");
    let (client, _) = client("embed-model").await;
    let mut graph = graph();
    graph.add_entity("c", "Gamma", "module", None, None);
    // Embed a and b only: c has no vector, so no hash.
    let mut partial = Graph::new();
    partial.add_entity("a", "Alpha", "class", None, None);
    partial.add_entity("b", "Beta", "function", None, None);
    embed(&mut partial, &client).await;
    for id in ["a", "b"] {
        graph.set_embedding(id, &[0.1, 0.2, 0.3, 0.4]);
        graph.set_embedding_hash(id, partial.embedding_hash(id).expect("hash").to_owned());
    }
    // A stale hash on an entity without a vector is not stored.
    graph.set_embedding_hash("c", "stale".to_owned());
    store::save(&pool, key, &graph).await.expect("save");

    let columns: Vec<(String, Option<String>)> = sqlx::query_as(
        "SELECT entity_id, embedding_text_hash FROM inventory_graph.entities ORDER BY ordinal",
    )
    .fetch_all(&pool)
    .await
    .expect("rows");
    assert_eq!(columns[0].1.as_deref(), partial.embedding_hash("a"));
    assert_eq!(columns[1].1.as_deref(), partial.embedding_hash("b"));
    assert_eq!(columns[2], ("c".to_owned(), None));

    // A full load brings the hashes back, so the next ingestion can compare.
    let (loaded, _) = store::load(&pool, key)
        .await
        .expect("load")
        .expect("a graph");
    assert_eq!(loaded.embedding_hash("a"), partial.embedding_hash("a"));
    assert_eq!(loaded.embedding_hash("c"), None);
    // ... and the loaded graph embeds nothing but the entity without a vector.
    let (second, seen) = self::client("embed-model").await;
    let mut loaded = loaded;
    assert_eq!(embed(&mut loaded, &second).await, 1);
    assert_eq!(seen.lock().expect("lock").clone(), vec!["Gamma module"]);
}
