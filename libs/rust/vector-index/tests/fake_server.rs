//! The client, the filter translation and the index tools against the
//! in-process fake of `elitea-vector` (`elitea_vector_index::testing`), over
//! a real gRPC connection.
//!
//! This is NOT the real service with a real Qdrant: the service's own
//! harness (`services/elitea-vector/tests/isolation.rs`) links the service
//! crate and its pinned Qdrant client, and a library workspace that depends
//! on a service would break the split of their lock files (libs/rust/Cargo.toml).
//! The fake keeps the service's request validation shape and the filter
//! semantics the translation relies on (`must` / `should` / `must_not` on point
//! fields and `metadata.<path>`); the real-service proof is the worker
//! integration that follows this crate.

#![allow(clippy::too_many_lines, reason = "scenario tests read top to bottom")]

use elitea_vector_index::client::{toolkit_namespace, toolkit_scope};
use elitea_vector_index::format::Doctype;
use elitea_vector_index::testing::{FakeVector, status};
use elitea_vector_index::{
    ClientConfig, DeleteSelector, Error, ErrorClass, HybridSearchParams, IndexCatalog, IndexEntry,
    IndexTools, QueryEmbedder, SearchArgs, SearchOutput, SearchParams, StepbackModel, VectorClient,
    VectorSession, filter, pb,
};
use serde_json::{Value, json};
use tonic::Code;

const NS: &str = "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b";
const OTHER_NS: &str = "6f1e2d3c-4b5a-4968-8776-5a4b3c2d1e0f";
const TOKEN_A: &str = "elvc_token-for-project-a";
const TOKEN_B: &str = "elvc_token-for-project-b";

fn space() -> pb::EmbeddingSpace {
    pb::EmbeddingSpace {
        model_slug: "test-model".to_owned(),
        dimension: 3,
    }
}

async fn connect() -> (VectorClient, FakeVector) {
    let fake = FakeVector::new(&[(TOKEN_A, 1), (TOKEN_B, 2)]);
    let (address, _server) = fake.clone().serve().await.expect("serve");
    let client = VectorClient::connect_lazy(&ClientConfig::new(format!("http://{address}")))
        .expect("client");
    (client, fake)
}

fn point(
    document: &str,
    chunk: &str,
    chunk_type: &str,
    text: &str,
    metadata: &Value,
    vector: [f32; 3],
) -> pb::Point {
    pb::Point {
        document_key: document.to_owned(),
        document_version: "v1".to_owned(),
        chunk_id: chunk.to_owned(),
        chunk_type: chunk_type.to_owned(),
        text: text.to_owned(),
        metadata_json: metadata.to_string(),
        vector: vector.to_vec(),
        ..pb::Point::default()
    }
}

fn token(value: &str) -> String {
    value.to_owned()
}

async fn seed(client: &VectorClient) {
    let points = vec![
        point(
            "readme",
            "0",
            "document",
            "install the platform on kubernetes",
            &json!({"filename": "README.md", "author": "ann", "year": 2024, "draft": false, "lang": "en"}),
            [1.0, 0.0, 0.0],
        ),
        point(
            "guide",
            "0",
            "document",
            "deployment guide for operators",
            &json!({"filename": "guide.md", "author": "bob", "year": 2025, "draft": true, "lang": "de"}),
            [0.9, 0.1, 0.0],
        ),
        point(
            "api",
            "0",
            "document",
            "rest api reference",
            &json!({"filename": "api.md", "author": "ann", "year": 2025, "draft": false, "lang": "en", "config": {"timeout": 30}}),
            [0.0, 1.0, 0.0],
        ),
        point(
            "guide",
            "0",
            "title",
            "Deployment",
            &json!({"filename": "guide.md"}),
            [0.95, 0.05, 0.0],
        ),
    ];
    client
        .upsert(&token(TOKEN_A), space(), toolkit_namespace(NS), points)
        .await
        .expect("upsert");
}

fn search_params(vector: [f32; 3], filter: Option<pb::Filter>, limit: u32) -> SearchParams {
    SearchParams {
        scope: toolkit_scope(vec![NS.to_owned()]),
        space: space(),
        vector: vector.to_vec(),
        filter,
        limit,
        score_threshold: None,
    }
}

async fn keys(client: &VectorClient, filter_json: Value) -> Vec<String> {
    let translated = filter::translate(&filter_json).expect("translates");
    let found = client
        .search(
            &token(TOKEN_A),
            search_params([1.0, 0.0, 0.0], translated, 10),
        )
        .await
        .expect("search");
    let mut keys: Vec<String> = found
        .iter()
        .filter(|point| point.chunk_type == "document")
        .map(|point| point.document_key.clone())
        .collect();
    keys.sort();
    keys
}

#[tokio::test]
async fn upsert_batches_and_every_call_carries_the_bearer() {
    let (client, fake) = connect().await;
    let points: Vec<pb::Point> = (0..1100)
        .map(|n| {
            point(
                "big",
                &n.to_string(),
                "document",
                "t",
                &json!({}),
                [1.0, 0.0, 0.0],
            )
        })
        .collect();
    let summary = client
        .upsert(&token(TOKEN_A), space(), toolkit_namespace(NS), points)
        .await
        .expect("upsert");
    assert_eq!(summary.upserted, 1100);
    assert_eq!(summary.collection, "emb_test-model_3");
    assert_eq!(fake.point_count(), 1100);
    client
        .count(
            &token(TOKEN_A),
            toolkit_scope(vec![NS.to_owned()]),
            None,
            None,
        )
        .await
        .expect("count");
    assert!(
        fake.bearers()
            .iter()
            .all(|bearer| *bearer == format!("Bearer {TOKEN_A}"))
    );
}

#[tokio::test]
async fn an_upsert_with_a_wrong_dimension_is_refused_before_anything_is_sent() {
    let (client, fake) = connect().await;
    let bad = pb::Point {
        vector: vec![1.0, 2.0],
        chunk_id: "x".to_owned(),
        ..pb::Point::default()
    };
    let error = client
        .upsert(&token(TOKEN_A), space(), toolkit_namespace(NS), vec![bad])
        .await
        .expect_err("refused");
    assert!(matches!(error, Error::InvalidArgument(_)), "{error}");
    assert!(fake.bearers().is_empty(), "nothing was sent");
    let none = client
        .upsert(&token(TOKEN_A), space(), toolkit_namespace(NS), Vec::new())
        .await
        .unwrap();
    assert_eq!(none.upserted, 0);
}

#[tokio::test]
async fn search_orders_by_cosine_and_a_project_never_sees_another() {
    let (client, _fake) = connect().await;
    seed(&client).await;
    let found = client
        .search(&token(TOKEN_A), search_params([1.0, 0.0, 0.0], None, 2))
        .await
        .unwrap();
    assert_eq!(found.len(), 2);
    assert_eq!(found[0].document_key, "readme");
    assert!((found[0].score - 1.0).abs() < 1e-6);
    let other = client
        .search(&token(TOKEN_B), search_params([1.0, 0.0, 0.0], None, 10))
        .await
        .unwrap();
    assert!(
        other.is_empty(),
        "project B has no points in this namespace"
    );
    let mut threshold = search_params([1.0, 0.0, 0.0], None, 10);
    threshold.score_threshold = Some(0.99);
    let strict = client.search(&token(TOKEN_A), threshold).await.unwrap();
    assert!(strict.iter().all(|p| p.score >= 0.99));
}

#[tokio::test]
async fn translated_filters_select_the_right_points() {
    let (client, _fake) = connect().await;
    seed(&client).await;
    assert_eq!(
        keys(&client, json!({"author": "ann"})).await,
        ["api", "readme"]
    );
    assert_eq!(
        keys(&client, json!({"author": {"$ne": "ann"}})).await,
        ["guide"]
    );
    assert_eq!(keys(&client, json!({"year": 2025})).await, ["api", "guide"]);
    assert_eq!(keys(&client, json!({"draft": true})).await, ["guide"]);
    assert_eq!(
        keys(&client, json!({"lang": {"$in": ["en", "de"]}})).await,
        ["api", "guide", "readme"]
    );
    assert_eq!(
        keys(&client, json!({"lang": {"$nin": ["de"]}})).await,
        ["api", "readme"]
    );
    assert_eq!(
        keys(&client, json!({"year": {"$in": [2024]}})).await,
        ["readme"]
    );
    assert_eq!(
        keys(&client, json!({"year": {"$nin": [2025]}})).await,
        ["readme"]
    );
    assert_eq!(keys(&client, json!({"config.timeout": 30})).await, ["api"]);
    assert_eq!(
        keys(&client, json!({"$or": [{"author": "bob"}, {"year": 2024}]})).await,
        ["guide", "readme"]
    );
    assert_eq!(
        keys(
            &client,
            json!({"$and": [{"author": "ann"}, {"year": 2025}]})
        )
        .await,
        ["api"]
    );
    assert_eq!(
        keys(&client, json!({"author": "ann", "$not": {"year": 2024}})).await,
        ["api"]
    );
    assert_eq!(
        keys(
            &client,
            json!({"$not": {"$or": [{"author": "bob"}, {"year": 2024}]}})
        )
        .await,
        ["api"]
    );
    assert_eq!(
        keys(&client, json!({"chunk_type": "document", "author": "bob"})).await,
        ["guide"]
    );
}

#[tokio::test]
async fn hybrid_search_fuses_text_with_dense() {
    let (client, _fake) = connect().await;
    seed(&client).await;
    // The vector points at "api", the text at "kubernetes" (the readme).
    let mut dense = search_params([0.0, 1.0, 0.0], None, 3);
    dense.score_threshold = None;
    let fused = client
        .hybrid_search(
            &token(TOKEN_A),
            HybridSearchParams {
                search: dense,
                text: "kubernetes".to_owned(),
                dense_weight: 0.3,
                text_weight: 0.7,
            },
        )
        .await
        .unwrap();
    assert_eq!(
        fused[0].document_key, "readme",
        "the text weight of 0.7 outranks one dense rank"
    );
}

#[tokio::test]
async fn delete_selectors_count_and_list() {
    let (client, _fake) = connect().await;
    seed(&client).await;
    let scope = toolkit_scope(vec![NS.to_owned()]);
    assert_eq!(
        client
            .count(&token(TOKEN_A), scope.clone(), None, None)
            .await
            .unwrap(),
        4
    );
    let listed = client
        .list_indexes(&token(TOKEN_A), Some(pb::Source::ToolkitIndex))
        .await
        .unwrap();
    assert_eq!(listed.len(), 1);
    assert_eq!(
        (listed[0].namespace_id.as_str(), listed[0].point_count),
        (NS, 4)
    );
    assert!(
        client
            .list_indexes(&token(TOKEN_B), None)
            .await
            .unwrap()
            .is_empty()
    );

    client
        .delete(
            &token(TOKEN_A),
            Some(space()),
            toolkit_namespace(NS),
            DeleteSelector::Documents(vec!["guide".into()]),
        )
        .await
        .unwrap();
    assert_eq!(
        client
            .count(&token(TOKEN_A), scope.clone(), None, None)
            .await
            .unwrap(),
        2
    );

    // A delete under project B's token touches nothing of A's.
    client
        .delete(
            &token(TOKEN_B),
            None,
            toolkit_namespace(NS),
            DeleteSelector::WholeNamespace,
        )
        .await
        .unwrap();
    assert_eq!(
        client
            .count(&token(TOKEN_A), scope.clone(), None, None)
            .await
            .unwrap(),
        2
    );

    let generation = pb::Namespace {
        generation: "g1".to_owned(),
        ..toolkit_namespace(OTHER_NS)
    };
    client
        .upsert(
            &token(TOKEN_A),
            space(),
            generation,
            vec![point(
                "x",
                "0",
                "document",
                "t",
                &json!({}),
                [1.0, 1.0, 1.0],
            )],
        )
        .await
        .unwrap();
    let other_scope = toolkit_scope(vec![OTHER_NS.to_owned()]);
    assert_eq!(
        client
            .count(&token(TOKEN_A), other_scope.clone(), None, None)
            .await
            .unwrap(),
        1
    );
    client
        .delete(
            &token(TOKEN_A),
            None,
            toolkit_namespace(OTHER_NS),
            DeleteSelector::GenerationsExcept("g1".into()),
        )
        .await
        .unwrap();
    assert_eq!(
        client
            .count(&token(TOKEN_A), other_scope.clone(), None, None)
            .await
            .unwrap(),
        1
    );
    client
        .delete(
            &token(TOKEN_A),
            None,
            toolkit_namespace(OTHER_NS),
            DeleteSelector::Generations(vec!["g1".into()]),
        )
        .await
        .unwrap();
    assert_eq!(
        client
            .count(&token(TOKEN_A), other_scope, None, None)
            .await
            .unwrap(),
        0
    );
    client
        .delete(
            &token(TOKEN_A),
            None,
            toolkit_namespace(NS),
            DeleteSelector::WholeNamespace,
        )
        .await
        .unwrap();
    assert_eq!(
        client
            .count(&token(TOKEN_A), scope, None, None)
            .await
            .unwrap(),
        0
    );
}

#[tokio::test]
async fn statuses_are_classified() {
    let (client, fake) = connect().await;
    let scope = toolkit_scope(vec![NS.to_owned()]);
    let unknown = client
        .count(&token("elvc_unknown"), scope.clone(), None, None)
        .await
        .expect_err("refused");
    assert!(
        matches!(
            unknown,
            Error::Rpc {
                code: Code::Unauthenticated,
                ..
            }
        ),
        "{unknown}"
    );
    assert_eq!(unknown.class(), ErrorClass::NonRetryable);

    for (code, retryable) in [
        (Code::Unavailable, true),
        (Code::DeadlineExceeded, true),
        (Code::ResourceExhausted, true),
        (Code::Aborted, true),
        (Code::PermissionDenied, false),
        (Code::InvalidArgument, false),
        (Code::Internal, false),
    ] {
        fake.fail_next(status(code));
        let error = client
            .count(&token(TOKEN_A), scope.clone(), None, None)
            .await
            .expect_err("injected");
        assert_eq!(error.is_retryable(), retryable, "{code:?}");
    }

    let missing_header =
        VectorClient::connect_lazy(&ClientConfig::new("http://127.0.0.1:1")).unwrap();
    let down = missing_header
        .count(&token(TOKEN_A), scope, None, None)
        .await
        .expect_err("nothing listens");
    assert!(
        down.is_retryable(),
        "an unreachable service is retryable: {down}"
    );
}

// ── the index tools, end to end ────────────────────────────────────────────

/// Embeds by keyword, in the fake's 3-dimensional space.
struct KeywordEmbedder;

impl QueryEmbedder for KeywordEmbedder {
    async fn embed_query(&self, text: &str, space: &pb::EmbeddingSpace) -> Result<Vec<f32>, Error> {
        let text = text.to_lowercase();
        let mut vector = if text.contains("kubernetes") || text.contains("install") {
            vec![1.0, 0.0, 0.0]
        } else if text.contains("api") {
            vec![0.0, 1.0, 0.0]
        } else if text.contains("nothing") {
            vec![0.0, 0.0, 1.0]
        } else {
            vec![0.9, 0.1, 0.0]
        };
        vector.truncate(space.dimension as usize);
        Ok(vector)
    }
}

/// Answers the step-back prompt with a fixed query and the answer prompt
/// with a fixed reply, and keeps the prompts it saw.
struct ScriptedModel {
    prompts: std::sync::Mutex<Vec<String>>,
}

impl StepbackModel for ScriptedModel {
    async fn invoke(&self, prompt: &str) -> Result<Value, Error> {
        self.prompts.lock().unwrap().push(prompt.to_owned());
        if prompt.starts_with("Your task is to convert") {
            Ok(
                json!([{"type": "thinking", "thinking": "..."}, {"type": "text", "text": "kubernetes install"}]),
            )
        } else {
            Ok(json!("## Answer\nUse kubernetes.\n\n## Score\n90\n"))
        }
    }
}

fn catalog() -> IndexCatalog {
    IndexCatalog::new(vec![
        IndexEntry {
            name: "docs".into(),
            namespace_id: NS.into(),
            space: space(),
        },
        IndexEntry {
            name: "empty".into(),
            namespace_id: OTHER_NS.into(),
            space: space(),
        },
    ])
}

fn args(value: Value) -> SearchArgs {
    serde_json::from_value(value).expect("args")
}

#[tokio::test]
async fn search_index_returns_the_sdk_shapes() {
    let (client, _fake) = connect().await;
    seed(&client).await;
    let session_token = token(TOKEN_A);
    let session = VectorSession::new(&client, &session_token);
    let catalog = catalog();
    let tools = IndexTools::new(&session, &KeywordEmbedder, &catalog, Doctype::Document);

    assert_eq!(tools.list_indexes(), "docs,empty");

    let found = tools
        .search_index(&args(
            json!({"query": "install on kubernetes", "index_name": "docs", "search_top": 2}),
        ))
        .await
        .unwrap()
        .into_value();
    let docs = found.as_array().expect("a list");
    assert_eq!(docs.len(), 2);
    assert_eq!(
        docs[0]["page_content"],
        "install the platform on kubernetes"
    );
    assert_eq!(docs[0]["metadata"]["filename"], "README.md");
    assert_eq!(docs[0]["metadata"]["chunk_type"], "document");
    let keys: Vec<&String> = docs[0].as_object().unwrap().keys().collect();
    assert_eq!(keys, ["page_content", "metadata", "score"]);
    assert!(docs[0]["score"].as_f64().unwrap() > 0.99);

    // A filter, output_fields, across all indexes (empty name).
    let projected = tools
        .search_index(&args(json!({
            "query": "install on kubernetes",
            "filter": "{\"author\": \"ann\"}",
            "output_fields": ["metadata.author", "score"],
        })))
        .await
        .unwrap()
        .into_value();
    assert_eq!(projected[0]["metadata"], json!({"author": "ann"}));
    assert!(projected[0]["score"].is_number());
    assert!(projected[0].get("page_content").is_none());

    // A refused filter is an error with the operator named.
    let error = tools
        .search_index(&args(
            json!({"query": "x", "filter": {"year": {"$gt": 2020}}}),
        ))
        .await
        .expect_err("refused");
    assert!(error.to_string().contains("$gt"), "{error}");
    let error = tools
        .search_index(&args(json!({"query": "x", "filter": {"project_id": 2}})))
        .await
        .expect_err("refused");
    assert!(error.to_string().contains("reserved"), "{error}");
}

#[tokio::test]
async fn search_index_messages() {
    let (client, _fake) = connect().await;
    seed(&client).await;
    let session_token = token(TOKEN_A);
    let session = VectorSession::new(&client, &session_token);
    let catalog = catalog();
    let tools = IndexTools::new(&session, &KeywordEmbedder, &catalog, Doctype::Document);

    // Nothing clears the cut-off: the SDK's message, with its filter text.
    let none = tools
        .search_index(&args(
            json!({"query": "nothing", "index_name": "docs", "cut_off": 0.9}),
        ))
        .await
        .unwrap();
    assert_eq!(
        none,
        SearchOutput::Message(
            "No documents found by query 'nothing' and filter '{'$and': [{'collection': {'$eq': 'docs'}}, {'$or': [{'type': {'$exists': False}}, {'type': {'$ne': 'index_meta'}}]}]}'".to_owned()
        )
    );
    let missing = tools
        .search_index(&args(json!({"query": "x", "index_name": "nope"})))
        .await
        .unwrap();
    assert_eq!(
        missing,
        SearchOutput::Message("Index 'nope' not found. Available indexes: docs,empty".to_owned())
    );
    let wrong_dimension = IndexCatalog::new(vec![IndexEntry {
        name: "wide".into(),
        namespace_id: NS.into(),
        space: pb::EmbeddingSpace {
            model_slug: "test-model".into(),
            dimension: 8,
        },
    }]);
    let tools = IndexTools::new(
        &session,
        &KeywordEmbedder,
        &wrong_dimension,
        Doctype::Document,
    );
    let error = tools
        .search_index(&args(json!({"query": "kubernetes"})))
        .await
        .expect_err("refused");
    assert!(error.to_string().contains("dimensions"), "{error}");
    let mixed = IndexCatalog::new(vec![
        IndexEntry {
            name: "a".into(),
            namespace_id: NS.into(),
            space: space(),
        },
        IndexEntry {
            name: "b".into(),
            namespace_id: OTHER_NS.into(),
            space: pb::EmbeddingSpace {
                model_slug: "other".into(),
                dimension: 3,
            },
        },
    ]);
    let tools = IndexTools::new(&session, &KeywordEmbedder, &mixed, Doctype::Document);
    let error = tools
        .search_index(&args(json!({"query": "kubernetes"})))
        .await
        .expect_err("refused");
    assert!(
        error
            .to_string()
            .starts_with("Global search cannot be completed"),
        "{error}"
    );
}

#[tokio::test]
async fn code_toolkits_format_results_as_file_method_and_a_block() {
    let (client, _fake) = connect().await;
    let code = point(
        "src/app.py",
        "0",
        "document",
        "def run():\n    pass",
        &json!({"filename": "src/app.py", "method_name": "run"}),
        [1.0, 0.0, 0.0],
    );
    client
        .upsert(&token(TOKEN_A), space(), toolkit_namespace(NS), vec![code])
        .await
        .unwrap();
    let session_token = token(TOKEN_A);
    let session = VectorSession::new(&client, &session_token);
    let catalog = catalog();
    let tools = IndexTools::new(&session, &KeywordEmbedder, &catalog, Doctype::Code);
    let found = tools
        .search_index(&args(json!({"query": "kubernetes", "index_name": "docs"})))
        .await
        .unwrap()
        .into_value();
    let content = found[0]["page_content"].as_str().unwrap();
    assert!(
        content.starts_with("src/app.py -> run (score: 1.0"),
        "{content}"
    );
    assert!(
        content.ends_with("\n\n```python\ndef run():\n    pass\n```\n\n"),
        "{content:?}"
    );
}

#[tokio::test]
async fn full_text_search_now_works() {
    let (client, _fake) = connect().await;
    seed(&client).await;
    let session_token = token(TOKEN_A);
    let session = VectorSession::new(&client, &session_token);
    let catalog = catalog();
    let tools = IndexTools::new(&session, &KeywordEmbedder, &catalog, Doctype::Document);
    // The dense side prefers "api"; the text side finds the readme.
    let found = tools
        .search_index(&args(json!({
            "query": "api kubernetes",
            "index_name": "docs",
            "cut_off": 0.0,
            "full_text_search": {"enabled": true, "fields": ["text"]},
        })))
        .await
        .unwrap()
        .into_value();
    assert!(found.as_array().unwrap().len() >= 2, "{found}");
    assert!(
        found[0]["score"].as_f64().unwrap() < 0.1,
        "a fused score, not a cosine"
    );
}

#[tokio::test]
async fn extended_search_through_the_tool() {
    let (client, _fake) = connect().await;
    seed(&client).await;
    let session_token = token(TOKEN_A);
    let session = VectorSession::new(&client, &session_token);
    let catalog = catalog();
    let tools = IndexTools::new(&session, &KeywordEmbedder, &catalog, Doctype::Document);
    let found = tools
        .search_index(&args(json!({
            "query": "deployment",
            "index_name": "docs",
            "extended_search": ["title"],
            "search_top": 5,
        })))
        .await
        .unwrap()
        .into_value();
    let contents: Vec<&str> = found
        .as_array()
        .unwrap()
        .iter()
        .map(|d| d["page_content"].as_str().unwrap())
        .collect();
    assert!(
        contents.contains(&"deployment guide for operators"),
        "{contents:?}"
    );
    assert!(
        !contents.contains(&"Deployment"),
        "the title chunk itself is not returned: {contents:?}"
    );
}

#[tokio::test]
async fn remove_index_refuses_an_empty_name_and_deletes_a_named_one() {
    let (client, fake) = connect().await;
    seed(&client).await;
    let session_token = token(TOKEN_A);
    let session = VectorSession::new(&client, &session_token);
    let catalog = catalog();
    let tools = IndexTools::new(&session, &KeywordEmbedder, &catalog, Doctype::Document);

    for name in ["", "  "] {
        let error = tools.remove_index(name).await.expect_err("refused");
        assert!(matches!(error, Error::Refused(_)), "{error}");
    }
    assert_eq!(fake.point_count(), 4, "nothing was deleted");
    let error = tools.remove_index("nope").await.expect_err("unknown");
    assert_eq!(
        error.to_string(),
        "Index 'nope' not found. Available indexes: docs,empty"
    );

    let message = tools.remove_index("docs").await.unwrap();
    assert_eq!(
        message,
        "Index 'docs' has been removed from the vector store.\nAvailable indexes: empty"
    );
    assert_eq!(fake.point_count(), 0);

    let single = IndexCatalog::new(vec![IndexEntry {
        name: "only".into(),
        namespace_id: NS.into(),
        space: space(),
    }]);
    let tools = IndexTools::new(&session, &KeywordEmbedder, &single, Doctype::Document);
    assert_eq!(
        tools.remove_index("only").await.unwrap(),
        "Index 'only' has been removed from the vector store.\nAvailable indexes: No indexed collections"
    );
    let none = IndexCatalog::default();
    assert_eq!(none.list_indexes(), "No indexed collections");
}

#[tokio::test]
async fn stepback_tools_use_the_model_twice_for_a_summary() {
    let (client, _fake) = connect().await;
    seed(&client).await;
    let session_token = token(TOKEN_A);
    let session = VectorSession::new(&client, &session_token);
    let catalog = catalog();
    let tools = IndexTools::new(&session, &KeywordEmbedder, &catalog, Doctype::Document);
    let model = ScriptedModel {
        prompts: std::sync::Mutex::default(),
    };

    let text = tools
        .stepback_search_index(
            &args(json!({"query": "How do I install it?", "index_name": "docs", "search_top": 1})),
            &model,
        )
        .await
        .unwrap();
    assert!(text.starts_with("Found 1 documents matching the query\n[\n    {\n        \"page_content\": \"install the platform on kubernetes\""), "{text}");
    let first_prompt = model.prompts.lock().unwrap()[0].clone();
    assert!(
        first_prompt.contains("<input>\nHow do I install it? \n</input>"),
        "{first_prompt}"
    );

    let reply = tools
        .stepback_summary_index(
            &args(json!({"query": "How do I install it?", "index_name": "docs", "search_top": 1, "messages": [{"role": "user", "content": "hi"}]})),
            &model,
        )
        .await
        .unwrap();
    assert_eq!(reply, "## Answer\nUse kubernetes.\n\n## Score\n90\n");
    let prompts = model.prompts.lock().unwrap().clone();
    assert_eq!(prompts.len(), 3);
    assert!(prompts[2].contains(
        "<conversation_history>\n[{'role': 'user', 'content': 'hi'}]\n</conversation_history>"
    ));
    assert!(prompts[2].contains("'page_content': 'install the platform on kubernetes'"));

    let nothing = tools
        .stepback_search_index(
            &args(json!({"query": "q", "index_name": "unknown-index"})),
            &model,
        )
        .await
        .unwrap();
    assert_eq!(nothing, "No documents found matching the query.");
}
