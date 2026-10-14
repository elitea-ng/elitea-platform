//! The memory tools against the in-process fake of `elitea-vector`, over a
//! real gRPC connection: put, search, get and delete per namespace, the
//! isolation between projects, namespaces and the index tools, the limits and
//! the failure sentences by cause.

use elitea_vector_index::client::toolkit_namespace;
use elitea_vector_index::memory::{
    self, MAX_CONTENT_BYTES, MAX_KEY_BYTES, MAX_MEMORIES_PER_NAMESPACE, MAX_METADATA_BYTES,
    ManageArgs, MemoryEmbedder, MemoryTools, SearchMemoryArgs, describe, namespace_id,
};
use elitea_vector_index::testing::{FakeVector, status};
use elitea_vector_index::{ClientConfig, Error, Result, VectorClient, VectorSession, pb};
use serde_json::{Value, json};
use tonic::Code;

const TOKEN_A: &str = "elvc_project-a";
const TOKEN_B: &str = "elvc_project-b";
const SLUG: &str = "memory-model";

/// A bag of three topics: cooking, travel, code. Deterministic and offline.
struct TopicEmbedder;

impl MemoryEmbedder for TopicEmbedder {
    async fn embed(&self, text: &str) -> Result<Vec<f32>> {
        if text.contains("fail-embedding") {
            return Err(Error::Embedding(
                "the model gateway refused (429)".to_owned(),
            ));
        }
        let lower = text.to_lowercase();
        let score = |words: &[&str]| -> f32 {
            let hits = words.iter().filter(|word| lower.contains(*word)).count();
            #[allow(clippy::cast_precision_loss, reason = "a handful of words")]
            let hits = hits as f32;
            hits
        };
        let mut vector = vec![
            score(&["pasta", "recipe", "cook"]),
            score(&["paris", "flight", "travel"]),
            score(&["rust", "code", "compile"]),
        ];
        if vector.iter().all(|value| *value == 0.0) {
            vector[0] = 0.01;
        }
        Ok(vector)
    }
}

async fn connect() -> (VectorClient, FakeVector) {
    let fake = FakeVector::new(&[(TOKEN_A, 1), (TOKEN_B, 2)]);
    let (address, _server) = fake.clone().serve().await.expect("serve");
    let mut config = ClientConfig::new(format!("http://{address}"));
    config.allow_insecure = true;
    (VectorClient::connect_lazy(&config).expect("client"), fake)
}

fn manage(content: &str, key: Option<&str>, metadata: Option<Value>) -> ManageArgs {
    ManageArgs {
        content: content.to_owned(),
        key: key.map(str::to_owned),
        metadata: metadata.map(|value| value.as_object().expect("an object").clone()),
    }
}

fn search(query: &str, limit: Option<i64>) -> SearchMemoryArgs {
    SearchMemoryArgs {
        query: query.to_owned(),
        limit,
    }
}

#[tokio::test]
async fn put_search_get_delete_in_one_namespace() {
    let (client, _fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let tools = MemoryTools::new(&session, &TopicEmbedder, SLUG, "team-notes");

    let stored = tools
        .manage_memory(&manage(
            "Grandma's pasta recipe uses lots of garlic",
            Some("pasta"),
            Some(json!({"topic": "food", "tags": ["a", "é"]})),
        ))
        .await
        .expect("put");
    assert_eq!(stored, "Successfully stored memory with key: pasta");
    tools
        .manage_memory(&manage("Book a flight to Paris", Some("trip"), None))
        .await
        .expect("put");
    tools
        .manage_memory(&manage(
            "Rust code must compile cleanly",
            Some("lang"),
            None,
        ))
        .await
        .expect("put");

    // Relevance, not recency: the cooking memory comes first for a cooking
    // query although it was written first.
    let found = tools
        .search_memory(&search("how do I cook pasta", Some(2)))
        .await
        .expect("search");
    let parsed: Value = serde_json::from_str(&found).expect("json");
    let list = parsed.as_array().expect("a list");
    assert_eq!(list.len(), 2);
    assert_eq!(list[0]["key"], "pasta");
    assert_eq!(list[0]["metadata"]["topic"], "food");
    assert!(
        list[1].get("metadata").is_none(),
        "no metadata, no key: {found}"
    );
    // The SDK's text: indent 2, key then content then metadata, ASCII only.
    assert!(
        found.starts_with("[\n  {\n    \"key\": \"pasta\",\n    \"content\": \"Grandma's pasta"),
        "{found}"
    );
    assert!(found.contains("\\u00e9"), "ensure_ascii: {found}");

    let one = tools.get_memory("trip").await.expect("get");
    assert_eq!(
        one,
        "{\n  \"key\": \"trip\",\n  \"content\": \"Book a flight to Paris\"\n}"
    );
    assert_eq!(
        tools.get_memory("absent").await.expect("get"),
        "No memory found with key: absent"
    );

    assert_eq!(
        tools.delete_memory("trip").await.expect("delete"),
        "Successfully deleted memory with key: trip"
    );
    assert_eq!(
        tools.get_memory("trip").await.expect("get"),
        "No memory found with key: trip"
    );
    // The SDK's store answers success for an absent key too.
    assert_eq!(
        tools.delete_memory("trip").await.expect("delete"),
        "Successfully deleted memory with key: trip"
    );
}

#[tokio::test]
async fn an_empty_namespace_answers_with_the_sdk_sentences() {
    let (client, _fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let tools = MemoryTools::new(&session, &TopicEmbedder, SLUG, "fresh");
    assert_eq!(
        tools
            .search_memory(&search("anything", None))
            .await
            .expect("search"),
        "No memories found matching your query."
    );
    assert_eq!(
        tools.get_memory("k").await.expect("get"),
        "No memory found with key: k"
    );
}

#[tokio::test]
async fn a_key_is_generated_and_a_rewrite_replaces() {
    let (client, fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let tools = MemoryTools::new(&session, &TopicEmbedder, SLUG, "n");
    let stored = tools
        .manage_memory(&manage("pasta", None, None))
        .await
        .expect("put");
    let key = stored
        .strip_prefix("Successfully stored memory with key: ")
        .expect("sentence");
    assert_eq!(key.len(), 36, "a uuid4: {key}");
    // An empty key is also generated, as `key or uuid4()` does.
    tools
        .manage_memory(&manage("travel", Some(""), None))
        .await
        .expect("put");
    assert_eq!(fake.point_count(), 2);
    tools
        .manage_memory(&manage("pasta again", Some(key), Some(json!({"v": 2}))))
        .await
        .expect("rewrite");
    assert_eq!(fake.point_count(), 2, "the key's point was replaced");
    let got: Value =
        serde_json::from_str(&tools.get_memory(key).await.expect("get")).expect("json");
    assert_eq!(got["content"], "pasta again");
    assert_eq!(got["metadata"], json!({"v": 2}));
    // An empty metadata object is dropped, as `if metadata:` does.
    tools
        .manage_memory(&manage("pasta once more", Some(key), Some(json!({}))))
        .await
        .expect("rewrite");
    let got: Value =
        serde_json::from_str(&tools.get_memory(key).await.expect("get")).expect("json");
    assert!(got.get("metadata").is_none());
}

#[tokio::test]
async fn namespaces_and_projects_are_separate() {
    let (client, _fake) = connect().await;
    let (a, b) = (TOKEN_A.to_owned(), TOKEN_B.to_owned());
    let session_a = VectorSession::new(&client, &a);
    let session_b = VectorSession::new(&client, &b);
    let a_one = MemoryTools::new(&session_a, &TopicEmbedder, SLUG, "shared-name");
    let a_two = MemoryTools::new(&session_a, &TopicEmbedder, SLUG, "other-name");
    let b_one = MemoryTools::new(&session_b, &TopicEmbedder, SLUG, "shared-name");

    a_one
        .manage_memory(&manage("secret pasta of project A", Some("k"), None))
        .await
        .expect("put");

    // Another namespace of the same project sees nothing.
    assert_eq!(
        a_two
            .search_memory(&search("pasta", None))
            .await
            .expect("s"),
        "No memories found matching your query."
    );
    assert_eq!(
        a_two.get_memory("k").await.expect("g"),
        "No memory found with key: k"
    );
    // The same namespace string in another project sees nothing, for every tool.
    assert_eq!(
        b_one
            .search_memory(&search("pasta", None))
            .await
            .expect("s"),
        "No memories found matching your query."
    );
    assert_eq!(
        b_one.get_memory("k").await.expect("g"),
        "No memory found with key: k"
    );
    // And its writes and deletes never touch project A's memory.
    b_one
        .manage_memory(&manage("pasta of project B", Some("k"), None))
        .await
        .expect("put");
    b_one.delete_memory("k").await.expect("delete");
    a_two.delete_memory("k").await.expect("delete");
    let kept: Value = serde_json::from_str(&a_one.get_memory("k").await.expect("g")).expect("j");
    assert_eq!(kept["content"], "secret pasta of project A");
}

#[tokio::test]
async fn the_namespace_id_is_stable_canonical_and_distinct() {
    let one = namespace_id("a");
    assert_eq!(one, namespace_id("a"));
    assert_ne!(one, namespace_id("b"));
    assert_ne!(namespace_id(""), namespace_id(" "));
    assert_eq!(one.len(), 36);
    assert_eq!(one.chars().nth(14), Some('8'), "version 8: {one}");
    assert!(
        one.chars()
            .all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase() || c == '-'),
        "canonical lower-case: {one}"
    );
    // The service accepts it as a namespace (the fake validates the shape).
    let _ = toolkit_namespace(&one);
}

#[tokio::test]
async fn the_limits_refuse_with_a_sentence() {
    let (client, _fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let tools = MemoryTools::new(&session, &TopicEmbedder, SLUG, "n");

    let long = "x".repeat(MAX_CONTENT_BYTES + 1);
    let error = tools
        .manage_memory(&manage(&long, None, None))
        .await
        .unwrap_err();
    assert!(
        describe(&error).contains("at most 16384 bytes"),
        "{}",
        describe(&error)
    );

    let big = json!({"blob": "y".repeat(MAX_METADATA_BYTES)});
    let error = tools
        .manage_memory(&manage("pasta", None, Some(big)))
        .await
        .unwrap_err();
    assert!(
        describe(&error).contains("metadata"),
        "{}",
        describe(&error)
    );

    let key = "k".repeat(MAX_KEY_BYTES + 1);
    let error = tools
        .manage_memory(&manage("pasta", Some(&key), None))
        .await
        .unwrap_err();
    assert!(describe(&error).contains("the key is 257 bytes"));
    let error = tools.get_memory("bad\nkey").await.unwrap_err();
    assert!(describe(&error).contains("control characters"));

    let error = tools
        .manage_memory(&manage("   ", None, None))
        .await
        .unwrap_err();
    assert_eq!(describe(&error), "content must not be empty");
    let error = tools
        .search_memory(&search("pasta", Some(0)))
        .await
        .unwrap_err();
    assert_eq!(describe(&error), "limit must be at least 1");
    let error = tools.search_memory(&search(" ", None)).await.unwrap_err();
    assert_eq!(describe(&error), "query must not be empty");
}

#[tokio::test]
async fn a_search_limit_is_capped() {
    let (client, _fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let tools = MemoryTools::new(&session, &TopicEmbedder, SLUG, "n");
    for index in 0..(memory::MAX_SEARCH_LIMIT + 5) {
        tools
            .manage_memory(&manage(
                &format!("pasta {index}"),
                Some(&format!("k{index}")),
                None,
            ))
            .await
            .expect("put");
    }
    let found = tools
        .search_memory(&search("pasta", Some(10_000)))
        .await
        .expect("search");
    let parsed: Value = serde_json::from_str(&found).expect("json");
    assert_eq!(
        parsed.as_array().expect("list").len(),
        memory::MAX_SEARCH_LIMIT as usize
    );
}

#[tokio::test]
async fn a_full_namespace_refuses_a_new_key_but_not_a_rewrite() {
    let (client, fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let tools = MemoryTools::new(&session, &TopicEmbedder, SLUG, "full");
    // Fill the namespace straight through the client, in batches.
    let ns = namespace_id("full");
    let points: Vec<pb::Point> = (0..MAX_MEMORIES_PER_NAMESPACE)
        .map(|index| pb::Point {
            document_key: format!("seed{index}"),
            document_version: "1".to_owned(),
            chunk_id: "0".to_owned(),
            chunk_type: "memory".to_owned(),
            text: "seed".to_owned(),
            metadata_json: "{}".to_owned(),
            vector: vec![1.0, 0.0, 0.0],
            ..pb::Point::default()
        })
        .collect();
    client
        .upsert(
            &token,
            pb::EmbeddingSpace {
                model_slug: SLUG.to_owned(),
                dimension: 3,
            },
            toolkit_namespace(&ns),
            points,
        )
        .await
        .expect("seed");
    assert_eq!(fake.point_count() as u64, MAX_MEMORIES_PER_NAMESPACE);

    let error = tools
        .manage_memory(&manage("pasta", Some("one-too-many"), None))
        .await
        .unwrap_err();
    assert!(
        describe(&error).contains("namespace is full"),
        "{}",
        describe(&error)
    );
    assert_eq!(fake.point_count() as u64, MAX_MEMORIES_PER_NAMESPACE);
    // An existing key can still be corrected.
    tools
        .manage_memory(&manage("pasta, corrected", Some("seed7"), None))
        .await
        .expect("rewrite");
    assert_eq!(fake.point_count() as u64, MAX_MEMORIES_PER_NAMESPACE);
    // And a delete frees room for a new one.
    tools.delete_memory("seed8").await.expect("delete");
    tools
        .manage_memory(&manage("pasta", Some("one-too-many"), None))
        .await
        .expect("room again");
}

#[tokio::test]
async fn each_failure_cause_has_its_own_sentence() {
    let (client, fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let tools = MemoryTools::new(&session, &TopicEmbedder, SLUG, "n");

    let error = tools
        .manage_memory(&manage("fail-embedding pasta", None, None))
        .await
        .unwrap_err();
    assert_eq!(
        describe(&error),
        "the memory embedding model could not embed the text (the model gateway refused (429))"
    );
    assert_eq!(fake.point_count(), 0, "nothing was written");

    for (code, expected) in [
        (Code::Unauthenticated, "refused this run's token"),
        (
            Code::PermissionDenied,
            "does not allow memory for this run's project",
        ),
        (Code::Unavailable, "unavailable right now"),
        (Code::ResourceExhausted, "busy"),
        (Code::Internal, "failed the request (Internal)"),
    ] {
        fake.fail_next(status(code));
        let error = tools
            .search_memory(&search("pasta", None))
            .await
            .unwrap_err();
        let sentence = describe(&error);
        assert!(sentence.contains(expected), "{code:?}: {sentence}");
        assert!(
            !sentence.contains("injected") && !sentence.contains(TOKEN_A),
            "{sentence}"
        );
    }
    let unknown_token = "elvc_unknown".to_owned();
    let stranger = VectorSession::new(&client, &unknown_token);
    let tools = MemoryTools::new(&stranger, &TopicEmbedder, SLUG, "n");
    let error = tools
        .manage_memory(&manage("pasta", None, None))
        .await
        .unwrap_err();
    assert!(describe(&error).contains("refused this run's token"));
}

#[tokio::test]
async fn every_call_carries_the_claim_token_only() {
    let (client, fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let tools = MemoryTools::new(&session, &TopicEmbedder, SLUG, "n");
    tools
        .manage_memory(&manage("pasta", Some("k"), None))
        .await
        .expect("put");
    tools
        .search_memory(&search("pasta", None))
        .await
        .expect("s");
    tools.get_memory("k").await.expect("g");
    tools.delete_memory("k").await.expect("d");
    let bearers = fake.bearers();
    assert!(!bearers.is_empty());
    assert!(
        bearers
            .iter()
            .all(|bearer| bearer == &format!("Bearer {TOKEN_A}"))
    );
}
