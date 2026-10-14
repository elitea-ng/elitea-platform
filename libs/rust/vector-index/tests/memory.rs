//! The memory tools against the in-process fake of `elitea-vector`, over a
//! real gRPC connection: put, search, get and delete per namespace, the
//! isolation between projects, namespaces and the index tools, the limits and
//! the failure sentences by cause.

#![allow(clippy::too_many_lines, reason = "scenario tests read top to bottom")]

use elitea_vector_index::client::toolkit_namespace;
use elitea_vector_index::memory::{
    self, GetMemoryArgs, MAX_CONTENT_BYTES, MAX_KEY_BYTES, MAX_MEMORIES_PER_NAMESPACE,
    MAX_METADATA_BYTES, ManageArgs, MemoryEmbedder, MemoryOwner, MemoryTools, SearchMemoryArgs,
    describe, namespace_id,
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
        as_of: None,
        include_history: None,
    }
}

fn gm(key: &str) -> GetMemoryArgs {
    GetMemoryArgs {
        key: key.to_owned(),
        as_of: None,
        include_history: None,
    }
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
        MemoryOwner::Project,
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

    let one = tools.get_memory(&gm("trip")).await.expect("get");
    assert_eq!(
        one,
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
        MemoryOwner::Project,
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
async fn a_key_is_generated_and_a_rewrite_replaces() {
    let (client, fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let tools = MemoryTools::new(&session, &TopicEmbedder, SLUG, MemoryOwner::Project, "n");
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
    assert_eq!(fake.point_count(), 3, "the old version is kept, closed");
    let got: Value =
        serde_json::from_str(&tools.get_memory(&gm(key)).await.expect("get")).expect("json");
    assert_eq!(got["content"], "pasta again");
    assert_eq!(got["metadata"], json!({"v": 2}));
    // An empty metadata object is dropped, as `if metadata:` does.
    tools
        .manage_memory(&manage("pasta once more", Some(key), Some(json!({}))))
        .await
        .expect("rewrite");
    let got: Value =
        serde_json::from_str(&tools.get_memory(&gm(key)).await.expect("get")).expect("json");
    assert!(got.get("metadata").is_none());
}

#[tokio::test]
async fn namespaces_and_projects_are_separate() {
    let (client, _fake) = connect().await;
    let (a, b) = (TOKEN_A.to_owned(), TOKEN_B.to_owned());
    let session_a = VectorSession::new(&client, &a);
    let session_b = VectorSession::new(&client, &b);
    let a_one = MemoryTools::new(
        &session_a,
        &TopicEmbedder,
        SLUG,
        MemoryOwner::Project,
        "shared-name",
    );
    let a_two = MemoryTools::new(
        &session_a,
        &TopicEmbedder,
        SLUG,
        MemoryOwner::Project,
        "other-name",
    );
    let b_one = MemoryTools::new(
        &session_b,
        &TopicEmbedder,
        SLUG,
        MemoryOwner::Project,
        "shared-name",
    );

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
        a_two.get_memory(&gm("k")).await.expect("g"),
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
        b_one.get_memory(&gm("k")).await.expect("g"),
        "No memory found with key: k"
    );
    // And its writes and deletes never touch project A's memory.
    b_one
        .manage_memory(&manage("pasta of project B", Some("k"), None))
        .await
        .expect("put");
    b_one.delete_memory("k").await.expect("delete");
    a_two.delete_memory("k").await.expect("delete");
    let kept: Value =
        serde_json::from_str(&a_one.get_memory(&gm("k")).await.expect("g")).expect("j");
    assert_eq!(kept["content"], "secret pasta of project A");
}

#[tokio::test]
async fn the_namespace_id_is_stable_canonical_and_domain_separated() {
    let project = |ns: &str| namespace_id(MemoryOwner::Project, ns);
    let user = |id: i64, ns: &str| namespace_id(MemoryOwner::User(id), ns);
    let one = project("a");
    assert_eq!(one, project("a"));
    assert_ne!(one, project("b"));
    assert_ne!(project(""), project(" "));
    assert_eq!(one.len(), 36);
    assert_eq!(one.chars().nth(14), Some('8'), "version 8: {one}");
    assert!(
        one.chars()
            .all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase() || c == '-'),
        "canonical lower-case: {one}"
    );
    // Project, each user and each namespace are separate id spaces.
    let ids = [
        project("a"),
        user(1, "a"),
        user(2, "a"),
        user(1, "b"),
        project("b"),
        user(-1, "a"),
        user(0, "a"),
    ];
    for (index, id) in ids.iter().enumerate() {
        for other in &ids[index + 1..] {
            assert_ne!(id, other);
        }
    }
    // A namespace crafted to look like the owner bytes of another owner does
    // not collide: the string is length-prefixed after the owner.
    assert_ne!(project("\u{1}"), user(0, ""));
    assert_ne!(
        user(1, "x"),
        user(
            i64::from_be_bytes(*b"\x00\x00\x00\x00\x00\x00\x00\x01"),
            "y"
        )
    );
    // The service accepts it as a namespace (the fake validates the shape).
    let _ = toolkit_namespace(&one);
}

#[tokio::test]
async fn personal_memory_belongs_to_one_user_and_project_memory_to_everyone() {
    let (client, _fake) = connect().await;
    let (a, b) = (TOKEN_A.to_owned(), TOKEN_B.to_owned());
    let session = VectorSession::new(&client, &a);
    let other_project = VectorSession::new(&client, &b);
    // Same project, same namespace string, two users and the project scope.
    let alice = MemoryTools::new(
        &session,
        &TopicEmbedder,
        SLUG,
        MemoryOwner::User(10),
        "notes",
    );
    let bob = MemoryTools::new(
        &session,
        &TopicEmbedder,
        SLUG,
        MemoryOwner::User(11),
        "notes",
    );
    let shared = MemoryTools::new(
        &session,
        &TopicEmbedder,
        SLUG,
        MemoryOwner::Project,
        "notes",
    );
    // The same user id in another project is another store.
    let alice_elsewhere = MemoryTools::new(
        &other_project,
        &TopicEmbedder,
        SLUG,
        MemoryOwner::User(10),
        "notes",
    );

    alice
        .manage_memory(&manage("alice likes pasta", Some("pref"), None))
        .await
        .expect("put");
    shared
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

    // Alice does not see the project's, nor the project Alice's.
    assert_eq!(
        alice.get_memory(&gm("team")).await.expect("g"),
        "No memory found with key: team"
    );
    assert_eq!(
        shared.get_memory(&gm("pref")).await.expect("g"),
        "No memory found with key: pref"
    );
    let in_project = shared
        .search_memory(&search("pasta", None))
        .await
        .expect("s");
    assert!(
        !in_project.contains("alice") && !in_project.contains("bob"),
        "{in_project}"
    );
    // Project memory: every user of the project reads and writes it.
    assert!(
        shared
            .get_memory(&gm("team"))
            .await
            .expect("g")
            .contains("the team likes paris")
    );
    shared
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
async fn the_limits_refuse_with_a_sentence() {
    let (client, _fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let tools = MemoryTools::new(&session, &TopicEmbedder, SLUG, MemoryOwner::Project, "n");

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
    let tools = MemoryTools::new(&session, &TopicEmbedder, SLUG, MemoryOwner::Project, "n");
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
    let tools = MemoryTools::new(&session, &TopicEmbedder, SLUG, MemoryOwner::Project, "full");
    // Fill the namespace straight through the client, in batches.
    let ns = namespace_id(MemoryOwner::Project, "full");
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
    let tools = MemoryTools::new(&session, &TopicEmbedder, SLUG, MemoryOwner::Project, "n");

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
    let tools = MemoryTools::new(&stranger, &TopicEmbedder, SLUG, MemoryOwner::Project, "n");
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
    let tools = MemoryTools::new(&session, &TopicEmbedder, SLUG, MemoryOwner::Project, "n");
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

// ---------------------------------------------------------------------------
// Time: supersede, history, as_of, recency, deletion
// ---------------------------------------------------------------------------

use std::sync::Arc;
use std::sync::atomic::{AtomicI64, Ordering};

use elitea_vector_index::memory::MAX_VERSIONS_PER_KEY;

const DAY_MS: i64 = 86_400_000;
/// 2026-01-01T00:00:00Z
const T0: i64 = 1_767_225_600_000;

fn clocked<'a>(
    session: &'a VectorSession<'a>,
    owner: MemoryOwner,
    ns: &str,
    now: &Arc<AtomicI64>,
) -> MemoryTools<'a, VectorSession<'a>, TopicEmbedder> {
    let clock = Arc::clone(now);
    MemoryTools::new(session, &TopicEmbedder, SLUG, owner, ns)
        .with_clock(Arc::new(move || clock.load(Ordering::SeqCst)))
}

fn temporal(text: &str) -> Vec<Value> {
    serde_json::from_str::<Value>(text)
        .expect("json")
        .as_array()
        .expect("list")
        .clone()
}

fn gm_at(key: &str, as_of: Option<&str>, history: bool) -> GetMemoryArgs {
    GetMemoryArgs {
        key: key.to_owned(),
        as_of: as_of.map(str::to_owned),
        include_history: Some(history),
    }
}

#[tokio::test]
async fn a_rewrite_supersedes_and_history_as_of_and_deletion_follow() {
    let (client, fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let now = Arc::new(AtomicI64::new(T0));
    let tools = clocked(&session, MemoryOwner::Project, "n", &now);

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
    let current = tools.get_memory(&gm("office")).await.expect("get");
    assert_eq!(
        current,
        "{\n  \"key\": \"office\",\n  \"content\": \"the office is in pasta town\"\n}"
    );
    // History: newest first, with the validity interval of each.
    let history = temporal(
        &tools
            .get_memory(&gm_at("office", None, true))
            .await
            .expect("history"),
    );
    assert_eq!(history.len(), 3);
    assert_eq!(history[0]["version"], 3);
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
    // Search sees the current version only by default, one per key; history shows all.
    let found = temporal(
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
    assert!(found[0].get("version").is_none());
    let mut with_history = search("office", Some(10));
    with_history.include_history = Some(true);
    assert_eq!(
        temporal(&tools.search_memory(&with_history).await.expect("s")).len(),
        3
    );
    let mut then = search("paris", Some(10));
    then.as_of = Some("2026-01-05T00:00:00Z".to_owned());
    let at_then = temporal(&tools.search_memory(&then).await.expect("s"));
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
    assert_eq!(
        tools
            .get_memory(&gm_at("office", Some("2026-01-05T00:00:00Z"), true))
            .await
            .expect("g"),
        "No memory found with key: office as of 2026-01-05T00:00:00Z"
    );
}

#[tokio::test]
async fn history_is_capped_per_key_and_the_oldest_goes_first() {
    let (client, fake) = connect().await;
    let token = TOKEN_A.to_owned();
    let session = VectorSession::new(&client, &token);
    let now = Arc::new(AtomicI64::new(T0));
    let tools = clocked(&session, MemoryOwner::Project, "n", &now);
    let cap = usize::try_from(MAX_VERSIONS_PER_KEY).expect("small");
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
    let history = temporal(
        &tools
            .get_memory(&gm_at("k", None, true))
            .await
            .expect("history"),
    );
    assert_eq!(history.len(), cap);
    assert_eq!(history[0]["version"], cap + 5, "versions keep counting");
    assert_eq!(
        history[cap - 1]["version"],
        6,
        "the five oldest were dropped"
    );
    assert!(
        history[0]["content"]
            .as_str()
            .expect("c")
            .ends_with(&format!("{}", cap + 4))
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
    let tools = clocked(&session, MemoryOwner::Project, "n", &now);
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
    let found = temporal(
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
    let found = temporal(
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
    let (a, b) = (TOKEN_A.to_owned(), TOKEN_B.to_owned());
    let session = VectorSession::new(&client, &a);
    let elsewhere = VectorSession::new(&client, &b);
    let now = Arc::new(AtomicI64::new(T0));
    let alice = clocked(&session, MemoryOwner::User(1), "n", &now);
    let bob = clocked(&session, MemoryOwner::User(2), "n", &now);
    let shared = clocked(&session, MemoryOwner::Project, "n", &now);
    let other_project = clocked(&elsewhere, MemoryOwner::User(1), "n", &now);

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
    // The project's history holds the team's key only; Alice's versions are hers.
    let team = temporal(&shared.get_memory(&gm_at("k", None, true)).await.expect("g"));
    assert_eq!(team.len(), 1);
    assert_eq!(team[0]["content"], "team pasta");
    // Bob deleting "k" in his namespace leaves every version of Alice's.
    bob.delete_memory("k").await.expect("delete");
    assert_eq!(
        temporal(&alice.get_memory(&gm_at("k", None, true)).await.expect("g")).len(),
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
