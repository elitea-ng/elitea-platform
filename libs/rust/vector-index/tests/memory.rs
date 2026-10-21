//! The memory tools against the in-process fake of `elitea-vector`, over a
//! real gRPC connection: put, search, get and delete per namespace and scope,
//! versions in time, the limits and the failure sentences by cause.
//!
//! WHO MAY SEE A MEMORY is the service's rule, not this crate's (the owner is
//! stamped from the token and every call is narrowed by it), and its real
//! proof is `services/elitea-vector/tests/isolation.rs` against a live Qdrant.
//! The fake applies the same rule so the tools are exercised end to end here.

#![allow(clippy::too_many_lines, reason = "scenario tests read top to bottom")]

use std::sync::Arc;
use std::sync::atomic::{AtomicI64, Ordering};

use elitea_vector_index::MemoryTarget;
use elitea_vector_index::client::memory_namespace;
use elitea_vector_index::memory::{
    GetMemoryArgs, MAX_CONTENT_BYTES, MAX_KEY_BYTES, MAX_MEMORIES_PER_NAMESPACE,
    MAX_METADATA_BYTES, MAX_VERSIONS_PER_KEY, ManageArgs, MemoryEmbedder, MemoryScope, MemoryTools,
    SearchMemoryArgs, describe, namespace_id,
};
use elitea_vector_index::testing::{FakeVector, status};
use elitea_vector_index::{ClientConfig, Error, Result, VectorClient, VectorSession, pb};
use serde_json::{Value, json};
use tonic::Code;

const TOKEN_A: &str = "elvc_project-a-user-10";
const TOKEN_A2: &str = "elvc_project-a-user-11";
const TOKEN_A_SHARED: &str = "elvc_project-a-user-10-shared-chat";
const TOKEN_A_ANON: &str = "elvc_project-a-no-user";
const TOKEN_B: &str = "elvc_project-b-user-10";
const SLUG: &str = "memory-model";
/// The memory toolkit every tool here belongs to.
const TOOLKIT: i64 = 41;

const DAY_MS: i64 = 86_400_000;
/// 2026-01-01T00:00:00Z
const T0: i64 = 1_767_225_600_000;

/// A bag of three topics: cooking, travel, code. Deterministic and offline.
struct TopicEmbedder;

#[allow(
    clippy::unused_async_trait_impl,
    reason = "a synchronous implementation of an async trait; its work stays lazy (runs when polled, as in the async implementations)"
)]
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
    let fake = FakeVector::new(&[
        (TOKEN_A, 1),
        (TOKEN_A2, 1),
        (TOKEN_A_SHARED, 1),
        (TOKEN_A_ANON, 1),
        (TOKEN_B, 2),
    ])
    .with_users(&[
        (TOKEN_A, 10),
        (TOKEN_A2, 11),
        (TOKEN_A_SHARED, 10),
        (TOKEN_B, 10),
    ])
    .with_shared_conversation(TOKEN_A_SHARED);
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
        as_of: None,
        include_history: None,
    }
}

fn gm(key: &str) -> GetMemoryArgs {
    gm_at(key, None, false)
}

fn gm_at(key: &str, as_of: Option<&str>, history: bool) -> GetMemoryArgs {
    GetMemoryArgs {
        key: key.to_owned(),
        as_of: as_of.map(str::to_owned),
        include_history: Some(history),
    }
}

fn list(text: &str) -> Vec<Value> {
    serde_json::from_str::<Value>(text)
        .expect("json")
        .as_array()
        .expect("list")
        .clone()
}

fn clocked<'a>(
    session: &'a VectorSession<'a>,
    scope: MemoryScope,
    ns: &str,
    now: &Arc<AtomicI64>,
) -> MemoryTools<'a, VectorSession<'a>, TopicEmbedder> {
    let clock = Arc::clone(now);
    MemoryTools::new(session, &TopicEmbedder, SLUG, scope, TOOLKIT, ns)
        .with_clock(Arc::new(move || clock.load(Ordering::SeqCst)))
}

#[tokio::test]
async fn put_search_get_delete_in_one_namespace() {
    let (client, _fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let tools = MemoryTools::new(
        &session,
        &TopicEmbedder,
        SLUG,
        MemoryScope::Project,
        TOOLKIT,
        "team-notes",
    );

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
    let parsed = list(&found);
    assert_eq!(parsed.len(), 2);
    assert_eq!(parsed[0]["key"], "pasta");
    assert_eq!(parsed[0]["metadata"]["topic"], "food");
    assert!(
        parsed[1].get("metadata").is_none(),
        "no metadata, no key: {found}"
    );
    // The SDK's text: indent 2, key then content then metadata, ASCII only.
    assert!(
        found.starts_with("[\n  {\n    \"key\": \"pasta\",\n    \"content\": \"Grandma's pasta"),
        "{found}"
    );
    assert!(found.contains("\\u00e9"), "ensure_ascii: {found}");

    assert_eq!(
        tools.get_memory(&gm("trip")).await.expect("get"),
        "{\n  \"key\": \"trip\",\n  \"content\": \"Book a flight to Paris\"\n}"
    );
    assert_eq!(
        tools.get_memory(&gm("absent")).await.expect("get"),
        "No memory found with key: absent"
    );
    assert_eq!(
        tools.delete_memory("trip").await.expect("delete"),
        "Successfully deleted memory with key: trip"
    );
    assert_eq!(
        tools.get_memory(&gm("trip")).await.expect("get"),
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
    let tools = MemoryTools::new(
        &session,
        &TopicEmbedder,
        SLUG,
        MemoryScope::Personal,
        TOOLKIT,
        "fresh",
    );
    assert_eq!(
        tools
            .search_memory(&search("anything", None))
            .await
            .expect("search"),
        "No memories found matching your query."
    );
    assert_eq!(
        tools.get_memory(&gm("k")).await.expect("get"),
        "No memory found with key: k"
    );
}

#[tokio::test]
async fn a_key_is_generated_and_an_empty_one_too() {
    let (client, fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let tools = MemoryTools::new(
        &session,
        &TopicEmbedder,
        SLUG,
        MemoryScope::Project,
        TOOLKIT,
        "n",
    );
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
    // An empty metadata object is dropped, as `if metadata:` does.
    tools
        .manage_memory(&manage("pasta again", Some(key), Some(json!({}))))
        .await
        .expect("rewrite");
    let got: Value =
        serde_json::from_str(&tools.get_memory(&gm(key)).await.expect("get")).expect("json");
    assert!(got.get("metadata").is_none());
}

#[tokio::test]
async fn the_namespace_id_is_stable_canonical_and_distinct() {
    let one = namespace_id(TOOLKIT, "a");
    assert_eq!(one, namespace_id(TOOLKIT, "a"));
    assert_ne!(one, namespace_id(TOOLKIT, "b"));
    assert_ne!(namespace_id(TOOLKIT, ""), namespace_id(TOOLKIT, " "));
    assert_eq!(one.len(), 36);
    assert_eq!(one.chars().nth(14), Some('8'), "version 8: {one}");
    assert!(
        one.chars()
            .all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase() || c == '-'),
        "canonical lower-case: {one}"
    );
    // The service accepts it as a namespace.
    let target = MemoryTarget {
        toolkit_id: TOOLKIT,
        namespace_id: one,
    };
    let _ = memory_namespace(&target, pb::MemoryScope::Project, "");
}

#[test]
fn the_namespace_id_includes_the_toolkit() {
    assert_ne!(namespace_id(1, "notes"), namespace_id(2, "notes"));
    assert_ne!(namespace_id(1, "notes"), namespace_id(1, "other"));
    // A toolkit id and a label cannot be confused with one another.
    assert_ne!(namespace_id(1, "2"), namespace_id(12, ""));
}

#[tokio::test]
async fn two_toolkits_with_the_same_namespace_label_never_share_memories() {
    let (client, _fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let one = MemoryTools::new(
        &session,
        &TopicEmbedder,
        SLUG,
        MemoryScope::Project,
        TOOLKIT,
        "notes",
    );
    let two = MemoryTools::new(
        &session,
        &TopicEmbedder,
        SLUG,
        MemoryScope::Project,
        TOOLKIT + 1,
        "notes",
    );
    one.manage_memory(&manage("pasta", Some("k"), None))
        .await
        .expect("put");
    assert_eq!(
        two.get_memory(&gm("k")).await.expect("get"),
        "No memory found with key: k"
    );
    assert_eq!(
        two.delete_memory("k").await.expect("delete"),
        "Successfully deleted memory with key: k"
    );
    assert!(
        one.get_memory(&gm("k"))
            .await
            .expect("get")
            .contains("pasta")
    );
}

#[tokio::test]
async fn personal_memory_belongs_to_one_user_and_project_memory_to_everyone() {
    let (client, _fake) = connect().await;
    let (a, a2, b) = (TOKEN_A.to_owned(), TOKEN_A2.to_owned(), TOKEN_B.to_owned());
    let alice_session = VectorSession::new(&client, &a);
    let bob_session = VectorSession::new(&client, &a2);
    let other_project = VectorSession::new(&client, &b);
    let personal = |session| {
        MemoryTools::new(
            session,
            &TopicEmbedder,
            SLUG,
            MemoryScope::Personal,
            TOOLKIT,
            "notes",
        )
    };
    let shared = |session| {
        MemoryTools::new(
            session,
            &TopicEmbedder,
            SLUG,
            MemoryScope::Project,
            TOOLKIT,
            "notes",
        )
    };
    let (alice, bob) = (personal(&alice_session), personal(&bob_session));
    let (alice_project, bob_project) = (shared(&alice_session), shared(&bob_session));
    // The same user id in another project is another store.
    let alice_elsewhere = personal(&other_project);

    alice
        .manage_memory(&manage("alice likes pasta", Some("pref"), None))
        .await
        .expect("put");
    alice_project
        .manage_memory(&manage("the team likes paris", Some("team"), None))
        .await
        .expect("put");

    // Bob sees none of Alice's, through every tool; Alice's survives his delete.
    assert_eq!(
        bob.search_memory(&search("pasta", None)).await.expect("s"),
        "No memories found matching your query."
    );
    assert_eq!(
        bob.get_memory(&gm("pref")).await.expect("g"),
        "No memory found with key: pref"
    );
    bob.delete_memory("pref").await.expect("delete");
    bob.manage_memory(&manage("bob's own pasta", Some("pref"), None))
        .await
        .expect("put");
    let kept: Value =
        serde_json::from_str(&alice.get_memory(&gm("pref")).await.expect("g")).expect("j");
    assert_eq!(
        kept["content"], "alice likes pasta",
        "bob's put under the same key is bob's"
    );

    // Alice does not see the project's through her personal tools, nor the
    // project Alice's.
    assert_eq!(
        alice.get_memory(&gm("team")).await.expect("g"),
        "No memory found with key: team"
    );
    assert_eq!(
        alice_project.get_memory(&gm("pref")).await.expect("g"),
        "No memory found with key: pref"
    );
    // Project memory: every user of the project reads and writes it.
    assert!(
        bob_project
            .get_memory(&gm("team"))
            .await
            .expect("g")
            .contains("the team likes paris")
    );
    bob_project
        .manage_memory(&manage("paris again", Some("team"), None))
        .await
        .expect("write");
    // Another project sees nothing of either.
    assert_eq!(
        alice_elsewhere.get_memory(&gm("pref")).await.expect("g"),
        "No memory found with key: pref"
    );
    assert_eq!(
        alice_elsewhere
            .search_memory(&search("pasta", None))
            .await
            .expect("s"),
        "No memories found matching your query."
    );
}

#[tokio::test]
async fn personal_memory_says_why_it_is_unavailable_and_project_memory_still_works() {
    let (client, _fake) = connect().await;
    for (token, expect) in [
        (TOKEN_A_ANON, "names no user"),
        (TOKEN_A_SHARED, "more than one person"),
    ] {
        let owned = token.to_owned();
        let session = VectorSession::new(&client, &owned);
        let personal = MemoryTools::new(
            &session,
            &TopicEmbedder,
            SLUG,
            MemoryScope::Personal,
            TOOLKIT,
            "n",
        );
        let project = MemoryTools::new(
            &session,
            &TopicEmbedder,
            SLUG,
            MemoryScope::Project,
            TOOLKIT,
            "n",
        );
        for error in [
            personal
                .manage_memory(&manage("pasta", Some("k"), None))
                .await
                .unwrap_err(),
            personal
                .search_memory(&search("pasta", None))
                .await
                .unwrap_err(),
            personal.get_memory(&gm("k")).await.unwrap_err(),
            personal.delete_memory("k").await.unwrap_err(),
        ] {
            let sentence = describe(&error);
            assert!(sentence.contains(expect), "{token}: {sentence}");
            assert!(sentence.contains("ersonal memory"), "{sentence}");
        }
        project
            .manage_memory(&manage("shared fact", Some(token), None))
            .await
            .expect("project works");
    }
}

#[tokio::test]
async fn the_limits_refuse_with_a_sentence() {
    let (client, _fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let tools = MemoryTools::new(
        &session,
        &TopicEmbedder,
        SLUG,
        MemoryScope::Project,
        TOOLKIT,
        "n",
    );

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
    let error = tools.get_memory(&gm("bad\nkey")).await.unwrap_err();
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
    let tools = MemoryTools::new(
        &session,
        &TopicEmbedder,
        SLUG,
        MemoryScope::Project,
        TOOLKIT,
        "n",
    );
    for index in 0..(elitea_vector_index::memory::MAX_SEARCH_LIMIT + 5) {
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
    assert_eq!(
        list(&found).len(),
        elitea_vector_index::memory::MAX_SEARCH_LIMIT as usize
    );
}

#[tokio::test]
async fn a_full_namespace_refuses_a_new_key_but_not_a_rewrite_that_frees_room() {
    let (client, fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let tools = MemoryTools::new(
        &session,
        &TopicEmbedder,
        SLUG,
        MemoryScope::Project,
        TOOLKIT,
        "full",
    );
    // Fill the namespace straight through the client, in batches.
    let ns = namespace_id(TOOLKIT, "full");
    let points: Vec<pb::Point> = (0..MAX_MEMORIES_PER_NAMESPACE)
        .map(|index| pb::Point {
            document_key: format!("seed{index}"),
            document_version: "1".to_owned(),
            chunk_id: "vseed".to_owned(),
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
            memory_namespace(
                &MemoryTarget {
                    toolkit_id: TOOLKIT,
                    namespace_id: ns,
                },
                pb::MemoryScope::Project,
                "",
            ),
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
    // A correction adds a version, so a full namespace refuses it too.
    let error = tools
        .manage_memory(&manage("pasta, corrected", Some("seed7"), None))
        .await
        .unwrap_err();
    assert!(describe(&error).contains("namespace is full"));
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
    let tools = MemoryTools::new(
        &session,
        &TopicEmbedder,
        SLUG,
        MemoryScope::Project,
        TOOLKIT,
        "n",
    );

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
    let tools = MemoryTools::new(
        &stranger,
        &TopicEmbedder,
        SLUG,
        MemoryScope::Project,
        TOOLKIT,
        "n",
    );
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
    let tools = MemoryTools::new(
        &session,
        &TopicEmbedder,
        SLUG,
        MemoryScope::Personal,
        TOOLKIT,
        "n",
    );
    tools
        .manage_memory(&manage("pasta", Some("k"), None))
        .await
        .expect("put");
    tools
        .search_memory(&search("pasta", None))
        .await
        .expect("s");
    tools.get_memory(&gm("k")).await.expect("g");
    tools.delete_memory("k").await.expect("d");
    let bearers = fake.bearers();
    assert!(!bearers.is_empty());
    assert!(
        bearers
            .iter()
            .all(|bearer| bearer == &format!("Bearer {TOKEN_A}"))
    );
}

#[tokio::test]
async fn no_call_lists_indexes_or_counts_the_project() {
    let (client, fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let tools = MemoryTools::new(
        &session,
        &TopicEmbedder,
        SLUG,
        MemoryScope::Personal,
        TOOLKIT,
        "n",
    );
    tools
        .manage_memory(&manage("pasta", Some("k"), None))
        .await
        .expect("put");
    tools.get_memory(&gm("k")).await.expect("get");
    tools
        .search_memory(&search("pasta", None))
        .await
        .expect("search");
    tools.delete_memory("k").await.expect("delete");
    let calls = fake.calls();
    assert!(!calls.contains(&"list_indexes"), "{calls:?}");
    // A write is: one read of the key's versions, one count of the namespace,
    // one upsert.
    assert_eq!(
        calls.iter().filter(|call| **call == "count").count(),
        1,
        "{calls:?}"
    );
}

// ---------------------------------------------------------------------------
// Time: supersede, history, as_of, recency, concurrency, deletion
// ---------------------------------------------------------------------------

#[tokio::test]
async fn a_rewrite_supersedes_and_history_as_of_and_deletion_follow() {
    let (client, fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let now = Arc::new(AtomicI64::new(T0));
    let tools = clocked(&session, MemoryScope::Project, "n", &now);

    tools
        .manage_memory(&manage(
            "the office is in paris",
            Some("office"),
            Some(json!({"src": "a"})),
        ))
        .await
        .expect("v1");
    now.store(T0 + 10 * DAY_MS, Ordering::SeqCst);
    tools
        .manage_memory(&manage("the office is in rust city", Some("office"), None))
        .await
        .expect("v2");
    now.store(T0 + 20 * DAY_MS, Ordering::SeqCst);
    tools
        .manage_memory(&manage("the office is in pasta town", Some("office"), None))
        .await
        .expect("v3");
    assert_eq!(fake.point_count(), 3, "three versions, none overwritten");

    // Current by default, in the SDK's shape (no temporal fields).
    assert_eq!(
        tools.get_memory(&gm("office")).await.expect("get"),
        "{\n  \"key\": \"office\",\n  \"content\": \"the office is in pasta town\"\n}"
    );
    // History: newest first, with the validity interval of each.
    let history = list(
        &tools
            .get_memory(&gm_at("office", None, true))
            .await
            .expect("history"),
    );
    assert_eq!(history.len(), 3);
    assert_eq!(history[0]["valid_to"], Value::Null);
    assert_eq!(history[0]["valid_from"], "2026-01-21T00:00:00.000Z");
    assert_eq!(history[1]["valid_from"], "2026-01-11T00:00:00.000Z");
    assert_eq!(history[1]["valid_to"], "2026-01-21T00:00:00.000Z");
    assert_eq!(history[2]["valid_to"], "2026-01-11T00:00:00.000Z");
    assert_eq!(history[2]["metadata"], json!({"src": "a"}));
    // as_of: what was true then; a boundary instant belongs to the newer version.
    for (at, expected) in [
        ("2026-01-05T00:00:00Z", "paris"),
        ("2026-01-11T00:00:00Z", "rust city"),
        ("2026-01-15T12:00:00+00:00", "rust city"),
        ("2026-02-01T00:00:00Z", "pasta town"),
    ] {
        let got: Value = serde_json::from_str(
            &tools
                .get_memory(&gm_at("office", Some(at), false))
                .await
                .expect("as_of"),
        )
        .expect("json");
        assert!(
            got["content"].as_str().expect("content").contains(expected),
            "{at}: {got}"
        );
        assert!(got.get("valid_from").is_some());
    }
    assert_eq!(
        tools
            .get_memory(&gm_at("office", Some("2025-12-31T00:00:00Z"), false))
            .await
            .expect("before"),
        "No memory found with key: office as of 2025-12-31T00:00:00Z"
    );
    // include_history with as_of: every version that existed by then (the
    // schema says "also return superseded versions").
    let by_then = list(
        &tools
            .get_memory(&gm_at("office", Some("2026-01-15T00:00:00Z"), true))
            .await
            .expect("history as of"),
    );
    assert_eq!(
        by_then.len(),
        2,
        "the version written on the 21st did not exist yet"
    );
    assert!(
        by_then[0]["content"]
            .as_str()
            .expect("c")
            .contains("rust city")
    );
    // Search sees the open version only by default, one per key; history shows all.
    let found = list(
        &tools
            .search_memory(&search("pasta paris rust", Some(10)))
            .await
            .expect("s"),
    );
    assert_eq!(found.len(), 1);
    assert!(
        found[0]["content"]
            .as_str()
            .expect("c")
            .contains("pasta town")
    );
    assert!(found[0].get("valid_from").is_none());
    let mut with_history = search("office", Some(10));
    with_history.include_history = Some(true);
    assert_eq!(
        list(&tools.search_memory(&with_history).await.expect("s")).len(),
        3
    );
    let mut then = search("paris", Some(10));
    then.as_of = Some("2026-01-05T00:00:00Z".to_owned());
    let at_then = list(&tools.search_memory(&then).await.expect("s"));
    assert_eq!(at_then.len(), 1);
    assert!(at_then[0]["content"].as_str().expect("c").contains("paris"));
    // A bad as_of is a sentence.
    let error = tools
        .get_memory(&gm_at("office", Some("yesterday"), false))
        .await
        .unwrap_err();
    assert!(describe(&error).contains("RFC 3339"));

    // Saying the same thing again is not a new version.
    tools
        .manage_memory(&manage("the office is in pasta town", Some("office"), None))
        .await
        .expect("same");
    assert_eq!(fake.point_count(), 3);

    // Deleting removes every version: privacy beats history.
    tools.delete_memory("office").await.expect("delete");
    assert_eq!(fake.point_count(), 0);
    assert_eq!(
        tools.get_memory(&gm("office")).await.expect("g"),
        "No memory found with key: office"
    );
}

#[tokio::test]
async fn concurrent_writers_lose_no_version_and_the_later_one_is_current() {
    let (client, fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let early = Arc::new(AtomicI64::new(T0 + DAY_MS));
    let late = Arc::new(AtomicI64::new(T0 + 2 * DAY_MS));
    let first = clocked(
        &session,
        MemoryScope::Project,
        "n",
        &Arc::new(AtomicI64::new(T0)),
    );
    first
        .manage_memory(&manage("pasta v0", Some("k"), None))
        .await
        .expect("v0");

    // Two writers read the same open version and both write.
    let (a, b) = (
        clocked(&session, MemoryScope::Project, "n", &early),
        clocked(&session, MemoryScope::Project, "n", &late),
    );
    let (early_args, late_args) = (
        manage("pasta from the early writer", Some("k"), None),
        manage("pasta from the late writer", Some("k"), None),
    );
    let (ra, rb) = tokio::join!(a.manage_memory(&early_args), b.manage_memory(&late_args));
    ra.expect("early");
    rb.expect("late");
    assert_eq!(
        fake.point_count(),
        3,
        "v0 and both racing versions are kept"
    );

    let history = list(
        &first
            .get_memory(&gm_at("k", None, true))
            .await
            .expect("history"),
    );
    assert_eq!(history.len(), 3);
    assert!(
        history[0]["content"]
            .as_str()
            .expect("c")
            .contains("late writer")
    );
    assert_eq!(history[0]["valid_to"], Value::Null);
    // The early writer's version reads as ended where the late one began, even
    // if its own close raced.
    assert!(
        history[1]["content"]
            .as_str()
            .expect("c")
            .contains("early writer")
    );
    assert_eq!(history[1]["valid_to"], history[0]["valid_from"]);
    let current: Value =
        serde_json::from_str(&first.get_memory(&gm("k")).await.expect("get")).expect("json");
    assert!(
        current["content"]
            .as_str()
            .expect("c")
            .contains("late writer")
    );
    // The next write closes whatever was left open.
    first
        .manage_memory(&manage("pasta v3", Some("k"), None))
        .await
        .expect("v3");
    let history = list(
        &first
            .get_memory(&gm_at("k", None, true))
            .await
            .expect("history"),
    );
    assert_eq!(history.len(), 4);
    assert!(
        history[1..]
            .iter()
            .all(|version| version["valid_to"] != Value::Null)
    );
}

#[tokio::test]
async fn history_is_capped_per_key_and_the_oldest_goes_first() {
    let (client, fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let now = Arc::new(AtomicI64::new(T0));
    let tools = clocked(&session, MemoryScope::Project, "n", &now);
    let cap = MAX_VERSIONS_PER_KEY;
    for round in 0..(cap + 5) {
        now.store(
            T0 + i64::try_from(round).expect("small") * DAY_MS,
            Ordering::SeqCst,
        );
        tools
            .manage_memory(&manage(
                &format!("pasta fact number {round}"),
                Some("k"),
                None,
            ))
            .await
            .expect("write");
    }
    assert_eq!(
        fake.point_count(),
        cap,
        "never more than the cap of versions"
    );
    let history = list(
        &tools
            .get_memory(&gm_at("k", None, true))
            .await
            .expect("history"),
    );
    assert_eq!(history.len(), cap);
    assert!(
        history[0]["content"]
            .as_str()
            .expect("c")
            .ends_with(&format!("{}", cap + 4))
    );
    assert!(
        history[cap - 1]["content"]
            .as_str()
            .expect("c")
            .ends_with("number 5"),
        "the five oldest were dropped: {}",
        history[cap - 1]["content"]
    );
    // Other keys are untouched by this key's cap.
    tools
        .manage_memory(&manage("rust other", Some("other"), None))
        .await
        .expect("other");
    assert_eq!(fake.point_count(), cap + 1);
}

#[tokio::test]
async fn recency_nudges_the_ranking_but_cosine_still_decides() {
    let (client, _fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let now = Arc::new(AtomicI64::new(T0));
    let tools = clocked(&session, MemoryScope::Project, "n", &now);
    // Two equally relevant memories, written a year apart.
    tools
        .manage_memory(&manage("old pasta recipe", Some("old"), None))
        .await
        .expect("old");
    now.store(T0 + 365 * DAY_MS, Ordering::SeqCst);
    tools
        .manage_memory(&manage("new pasta recipe", Some("new"), None))
        .await
        .expect("new");
    let found = list(
        &tools
            .search_memory(&search("pasta", Some(5)))
            .await
            .expect("s"),
    );
    assert_eq!(found[0]["key"], "new", "recency breaks the tie");
    assert_eq!(found[1]["key"], "old");
    // A strictly closer match from long ago still wins over a fresher, weaker one.
    now.store(T0 + 2 * 365 * DAY_MS, Ordering::SeqCst);
    tools
        .manage_memory(&manage("paris flight", Some("fresh-but-off"), None))
        .await
        .expect("fresh");
    let found = list(
        &tools
            .search_memory(&search("pasta", Some(5)))
            .await
            .expect("s"),
    );
    assert_ne!(found[0]["key"], "fresh-but-off");
}

#[tokio::test]
async fn time_travel_never_crosses_users_or_projects() {
    let (client, _fake) = connect().await;
    let (a, a2, b) = (TOKEN_A.to_owned(), TOKEN_A2.to_owned(), TOKEN_B.to_owned());
    let (alice_session, bob_session, elsewhere_session) = (
        VectorSession::new(&client, &a),
        VectorSession::new(&client, &a2),
        VectorSession::new(&client, &b),
    );
    let now = Arc::new(AtomicI64::new(T0));
    let alice = clocked(&alice_session, MemoryScope::Personal, "n", &now);
    let bob = clocked(&bob_session, MemoryScope::Personal, "n", &now);
    let shared = clocked(&alice_session, MemoryScope::Project, "n", &now);
    let other_project = clocked(&elsewhere_session, MemoryScope::Personal, "n", &now);

    alice
        .manage_memory(&manage("alice pasta v1", Some("k"), None))
        .await
        .expect("v1");
    now.store(T0 + DAY_MS, Ordering::SeqCst);
    alice
        .manage_memory(&manage("alice pasta v2", Some("k"), None))
        .await
        .expect("v2");
    shared
        .manage_memory(&manage("team pasta", Some("k"), None))
        .await
        .expect("shared");

    for other in [&bob, &other_project] {
        assert_eq!(
            other.get_memory(&gm_at("k", None, true)).await.expect("g"),
            "No memory found with key: k"
        );
        assert_eq!(
            other
                .get_memory(&gm_at("k", Some("2026-01-01T12:00:00Z"), false))
                .await
                .expect("g"),
            "No memory found with key: k as of 2026-01-01T12:00:00Z"
        );
        let mut every = search("pasta", Some(10));
        every.include_history = Some(true);
        assert!(
            !other
                .search_memory(&every)
                .await
                .expect("s")
                .contains("alice")
        );
    }
    let team = list(&shared.get_memory(&gm_at("k", None, true)).await.expect("g"));
    assert_eq!(team.len(), 1);
    assert_eq!(team[0]["content"], "team pasta");
    // Bob deleting "k" in his namespace leaves every version of Alice's.
    bob.delete_memory("k").await.expect("delete");
    assert_eq!(
        list(&alice.get_memory(&gm_at("k", None, true)).await.expect("g")).len(),
        2
    );
    // Alice's delete removes all of hers and none of the project's.
    alice.delete_memory("k").await.expect("delete");
    assert_eq!(
        alice.get_memory(&gm("k")).await.expect("g"),
        "No memory found with key: k"
    );
    assert!(
        shared
            .get_memory(&gm("k"))
            .await
            .expect("g")
            .contains("team pasta")
    );
}

#[tokio::test]
async fn a_run_reads_one_collection_once_it_has_embedded() {
    use std::sync::atomic::AtomicU32;
    let (client, fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let cache = Arc::new(AtomicU32::new(0));
    let tools = MemoryTools::new(
        &session,
        &TopicEmbedder,
        SLUG,
        MemoryScope::Project,
        TOOLKIT,
        "n",
    )
    .with_space_cache(Arc::clone(&cache));
    // Nothing embedded yet: a get reads every collection (dimension unknown).
    assert_eq!(
        tools.get_memory(&gm("k")).await.expect("g"),
        "No memory found with key: k"
    );
    assert_eq!(cache.load(Ordering::SeqCst), 0);
    tools
        .manage_memory(&manage("pasta", Some("k"), None))
        .await
        .expect("put");
    assert_eq!(
        cache.load(Ordering::SeqCst),
        3,
        "the run now knows its space"
    );
    assert!(
        tools
            .get_memory(&gm("k"))
            .await
            .expect("g")
            .contains("pasta")
    );
    assert_eq!(fake.point_count(), 1);
}
