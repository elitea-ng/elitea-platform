//! The index is scoped by project (migration 0005).
//!
//! A wiki id is derived from the repository name, not from the project.
//! Each index row belongs to one project, and every access is scoped by
//! `(project_id, wiki_id)`. These tests publish the SAME wiki id, with the
//! SAME node ids, into two projects and check that every read, the publish
//! and the publish lock stay inside one project.

mod storage_common;

use elitea_deepwiki_engine::ask::agent::Clock;
use elitea_deepwiki_engine::ask::store::{IndexStore, PgIndex};
use elitea_deepwiki_engine::ask::{self, Limits, QueryDeps};
use elitea_deepwiki_engine::errors::ErrorType;
use elitea_deepwiki_engine::llm::{EmbeddingOptions, Transport, TransportSettings};
use elitea_deepwiki_engine::runner::{Context, StopSignal};
use elitea_deepwiki_engine::storage::adapter::{Scope, UnifiedDb};
use elitea_deepwiki_engine::storage::build::{
    BuildSpace, PUBLISH_WIKI_LOCK, WikiRecord, publish_wiki_lock_object,
};
use elitea_deepwiki_engine::storage::rows::{IndexEdge, IndexNode};
use elitea_deepwiki_engine::storage::search::{Hybrid, IndexReader};
use elitea_deepwiki_engine::storage::{PROJECT_ARG, WikiKey};
use serde_json::{Value, json};
use sqlx::postgres::PgPool;
use std::time::Duration;
use storage_common as common;

/// One wiki id, the one both projects derive from `acme/widgets`.
const WIKI: &str = "acme--widgets--main";
const A: i32 = 101;
const B: i32 = 202;
/// A project that generated nothing.
const C: i32 = 303;

const DIM: usize = 4;

/// `count` nodes with the SAME ids in every project (`n0`, `n1`, …); the
/// text carries `tag` and the word `shared`.
fn nodes(tag: &str, count: usize) -> Vec<IndexNode> {
    (0..count)
        .map(|i| IndexNode {
            node_id: format!("n{i}"),
            rel_path: format!("src/n{i}.py"),
            file_name: format!("n{i}.py"),
            language: "python".into(),
            symbol_name: format!("{tag}{i}"),
            symbol_type: "function".into(),
            source_text: format!("def {tag}{i}():\n    return shared + {tag}\n"),
            ..IndexNode::default()
        })
        .collect()
}

fn vector(tag: &str, i: usize) -> Vec<f64> {
    let base = if tag == "alpha" { 1.0 } else { -1.0 };
    (0..DIM)
        .map(|d| base + f64::from(u32::try_from(i * DIM + d).expect("small")) / 1e3)
        .collect()
}

/// Publish `count` nodes tagged `tag` as `WIKI` of `project`.
async fn publish(space: &BuildSpace, project: i32, tag: &str, count: usize) {
    let nodes = nodes(tag, count);
    let edges: Vec<IndexEdge> = nodes
        .windows(2)
        .map(|pair| IndexEdge {
            source_id: pair[0].node_id.clone(),
            target_id: pair[1].node_id.clone(),
            rel_type: "calls".into(),
            edge_class: Some("structural".into()),
            weight: 1.0,
        })
        .collect();
    let vectors: Vec<(String, Vec<f64>)> = (0..count)
        .map(|i| (format!("n{i}"), vector(tag, i)))
        .collect();
    let mut build = space
        .begin(&common::key_in(project, WIKI))
        .await
        .expect("begin");
    build.stage_nodes(nodes).await.expect("nodes");
    build.stage_edges(&edges).await.expect("edges");
    build
        .stage_embeddings(vectors.iter().map(|(id, v)| (id.as_str(), v.as_slice())))
        .await
        .expect("embeddings");
    build
        .publish(&WikiRecord {
            repo: Some("acme/widgets".into()),
            branch: Some("main".into()),
            commit_hash: Some(tag.into()),
            ..WikiRecord::default()
        })
        .await
        .expect("publish");
}

/// Every row of `WIKI` that `project` owns, table by table.
async fn owned(pool: &PgPool, project: i32) -> Vec<i64> {
    let mut counts = Vec::new();
    for table in [
        "wikis",
        "wiki_nodes",
        "wiki_edges",
        "wiki_node_embeddings",
        "wiki_bm25_meta",
        "wiki_bm25_docs",
        "wiki_bm25_terms",
        "wiki_bm25_postings",
    ] {
        // A table name from the list above, never input.
        let count: i64 = sqlx::query_scalar(&format!(
            "SELECT count(*) FROM {table} WHERE project_id = $1 AND wiki_id = $2"
        ))
        .bind(project)
        .bind(WIKI)
        .fetch_one(pool)
        .await
        .expect("count");
        counts.push(count);
    }
    counts
}

fn db(pool: &PgPool, project: i32) -> UnifiedDb {
    UnifiedDb::new(pool.clone(), common::key_in(project, WIKI))
}

/// Each project reads its own rows of the shared wiki id through every
/// read surface, and nothing of the other's.
async fn assert_sees_only(pool: &PgPool, project: i32, tag: &str, other: &str, count: usize) {
    let n = i64::try_from(count).expect("small");
    let db = db(pool, project);
    assert_eq!(db.node_count().await.expect("nodes"), n, "{project}");
    assert_eq!(db.edge_count().await.expect("edges"), n - 1, "{project}");
    assert!(db.vec_available().await.expect("vectors"));
    assert_eq!(
        db.get_meta("commit_hash").await.expect("meta").as_deref(),
        Some(tag)
    );
    let node = db.get_node("n0").await.expect("node").expect("present");
    assert_eq!(node.symbol_name, format!("{tag}0"));
    let many = db.get_nodes_by_ids(&["n0", "n1"]).await.expect("nodes");
    assert!(many.iter().all(|n| n.symbol_name.starts_with(tag)));
    let out = db.get_edges_from("n0", &[]).await.expect("edges");
    assert_eq!(
        out.len(),
        1,
        "{project}: one outgoing edge, not one per project"
    );
    assert_eq!(db.get_edges_to("n1").await.expect("edges").len(), 1);
    assert_eq!(
        db.get_embedding("n0")
            .await
            .expect("embedding")
            .map(|v| v[0] > 0.0),
        Some(tag == "alpha")
    );

    // The fused search: the shared word matches every node of THIS project.
    let embedding = vector(tag, 0);
    let fused = db
        .search_hybrid(
            "shared",
            Some(&embedding),
            &Scope::default(),
            &Hybrid::default(),
        )
        .await
        .expect("hybrid");
    assert_eq!(fused.len(), count, "{project}");
    assert!(
        fused
            .iter()
            .all(|row| row.node.source_text.contains(tag) && !row.node.source_text.contains(other)),
        "{project}"
    );

    // The branch searches, each with the other project's word.
    let reader = IndexReader::new(pool.clone(), common::key_in(project, WIKI));
    assert!(reader.search_fts(other, 10).await.expect("fts").is_empty());
    assert!(
        reader
            .search_bm25(other, 10)
            .await
            .expect("bm25")
            .is_empty()
    );
    assert_eq!(reader.search_fts(tag, 50).await.expect("fts").len(), count);
    let dense = reader.search_dense(&embedding, 50).await.expect("dense");
    assert_eq!(dense.len(), count, "{project}: dense hits of one project");
    let stats = reader.stats().await.expect("stats");
    assert_eq!((stats.node_count, stats.vector_count), (n, n));

    // The agent's store.
    let index = PgIndex::new(db);
    assert!(index.wiki_exists().await.expect("exists"));
    let names = index.name_rows(other, false, 50).await.expect("names");
    assert!(names.is_empty(), "{project}: {names:?}");
    let fts = index.search_fts(other, None, None, 50).await.expect("fts");
    assert!(fts.is_empty(), "{project}");
    let scanned = index.scan_rows(100, None, None).await.expect("scan");
    assert_eq!(scanned.len(), count, "{project}");
    let counts = index
        .connection_counts(&["n0".to_owned()])
        .await
        .expect("counts");
    assert_eq!(counts.get("n0"), Some(&1), "{project}");
}

#[tokio::test(flavor = "multi_thread")]
async fn two_projects_with_one_wiki_id_each_read_only_their_own_index() {
    let Some(pool) = common::fresh_database("tenancy_reads").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "tenancy");
    publish(&space, A, "alpha", 3).await;
    publish(&space, B, "beta", 5).await;

    assert_sees_only(&pool, A, "alpha", "beta", 3).await;
    assert_sees_only(&pool, B, "beta", "alpha", 5).await;

    // A project that generated nothing finds nothing under the same id.
    let none = db(&pool, C);
    assert_eq!(none.node_count().await.expect("nodes"), 0);
    assert!(!none.vec_available().await.expect("vectors"));
    assert_eq!(none.get_node("n0").await.expect("node"), None);
    assert_eq!(none.get_meta("commit_hash").await.expect("meta"), None);
    let hybrid = none
        .search_hybrid(
            "shared",
            Some(&vector("alpha", 0)),
            &Scope::default(),
            &Hybrid::default(),
        )
        .await
        .expect("hybrid");
    assert!(hybrid.is_empty());
    assert!(!PgIndex::new(none).wiki_exists().await.expect("exists"));
}

#[tokio::test(flavor = "multi_thread")]
async fn a_publish_replaces_only_its_own_projects_rows() {
    let Some(pool) = common::fresh_database("tenancy_publish").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "tenancy");
    publish(&space, A, "alpha", 3).await;
    publish(&space, B, "beta", 5).await;
    let a_before = owned(&pool, A).await;
    assert!(a_before.iter().all(|&n| n > 0), "{a_before:?}");

    // B regenerates the same wiki id, bigger: A's index is untouched.
    publish(&space, B, "gamma", 7).await;
    assert_eq!(owned(&pool, A).await, a_before);
    assert_sees_only(&pool, A, "alpha", "gamma", 3).await;
    assert_sees_only(&pool, B, "gamma", "alpha", 7).await;
    assert!(
        db(&pool, B)
            .search_hybrid("beta", None, &Scope::default(), &Hybrid::default())
            .await
            .expect("hybrid")
            .is_empty(),
        "B's old version is gone"
    );

    // Deleting B's wiki row removes B's whole graph and nothing of A's.
    sqlx::query("DELETE FROM wikis WHERE project_id = $1 AND wiki_id = $2")
        .bind(B)
        .bind(WIKI)
        .execute(&pool)
        .await
        .expect("delete");
    assert_eq!(db(&pool, B).node_count().await.expect("nodes"), 0);
    assert_eq!(owned(&pool, A).await, a_before);
}

#[tokio::test(flavor = "multi_thread")]
async fn two_projects_publish_one_wiki_id_at_the_same_time() {
    let Some(pool) = common::fresh_database("tenancy_lock").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "tenancy");
    // Hold A's publish lock of the wiki, as a long publish of A would.
    let mut holder = pool.begin().await.expect("holder");
    sqlx::query("SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))")
        .bind(PUBLISH_WIKI_LOCK)
        .bind(publish_wiki_lock_object(&common::key_in(A, WIKI)))
        .execute(&mut *holder)
        .await
        .expect("lock");

    // B publishes the same wiki id without waiting for A.
    tokio::time::timeout(Duration::from_secs(30), publish(&space, B, "beta", 4))
        .await
        .expect("B's publish does not queue behind A's lock");

    // A's own publish queues behind it, and lands once it is released.
    let a = publish(&space, A, "alpha", 3);
    tokio::pin!(a);
    assert!(
        tokio::time::timeout(Duration::from_millis(500), &mut a)
            .await
            .is_err(),
        "A's publish must wait for A's lock"
    );
    holder.rollback().await.expect("release");
    tokio::time::timeout(Duration::from_secs(30), a)
        .await
        .expect("A's publish lands");
    assert_sees_only(&pool, A, "alpha", "beta", 3).await;
    assert_sees_only(&pool, B, "beta", "alpha", 4).await;
}

fn arguments(value: &Value) -> serde_json::Map<String, Value> {
    value.as_object().cloned().expect("object")
}

/// `ask` as the runner calls it, with the project the host stamped.
async fn ask(pool: &PgPool, project: Option<i32>, mut payload: Value) -> Result<Value, ErrorType> {
    if let Some(project) = project {
        payload[PROJECT_ARG] = json!(project.to_string());
    }
    payload["llm_settings"] =
        json!({"api_base": "http://127.0.0.1:1/llm/v1", "api_key": "k", "model_name": "m"});
    payload["embedding_model"] = json!("e");
    let deps = QueryDeps {
        pool: pool.clone(),
        transport: Transport::new(&TransportSettings::default()).expect("transport"),
        embedding_options: EmbeddingOptions::default(),
        limits: Limits::default(),
        clock: Clock::System,
    };
    let (sender, _receiver) = tokio::sync::mpsc::unbounded_channel();
    let context = Context::new(sender, StopSignal::default());
    ask::run_tool("ask", &arguments(&payload), &deps, None, &context)
        .await
        .map_err(|e| e.error_type)
}

/// An override resolves in the caller's project only: a wiki id that
/// exists only in A, given as `repo_identifier_override` or derived from
/// the repository, finds no index in B.
#[tokio::test(flavor = "multi_thread")]
async fn another_projects_wiki_id_finds_no_index() {
    let Some(pool) = common::fresh_database("tenancy_ask").await else {
        return;
    };
    publish(&BuildSpace::new(pool.clone(), "tenancy"), A, "alpha", 3).await;
    let not_found = |result: &Result<Value, ErrorType>| {
        result.as_ref().is_ok_and(|r| {
            r["success"] == json!(false)
                && r["error_category"] == json!("resource_not_found")
                && r["error"]
                    .as_str()
                    .is_some_and(|m| m.starts_with("No wiki index found"))
        })
    };
    for payload in [
        json!({"question": "What does it do?", "repo_identifier_override": "acme/widgets:main",
               "repo_config": {"repository": "someone/else", "branch": "main"}}),
        json!({"question": "What does it do?",
               "repo_config": {"repository": "acme/widgets", "branch": "main"}}),
    ] {
        let result = ask(&pool, Some(B), payload.clone()).await;
        assert!(not_found(&result), "{payload}: {result:?}");
        // The wiki id exists in A only, so B finds no index.
        let request = ask::parse_request(&arguments(&payload)).expect("request");
        assert_eq!(request.wiki_id, WIKI);
        let a = PgIndex::new(UnifiedDb::new(
            pool.clone(),
            WikiKey::new(common::project(A), request.wiki_id),
        ));
        assert!(
            a.wiki_exists().await.expect("exists"),
            "A's own index is there"
        );
    }
    // No project at all: refused before the index is read.
    let refused = ask(
        &pool,
        None,
        json!({"question": "q", "repo_identifier_override": "acme/widgets:main"}),
    )
    .await;
    assert_eq!(refused, Err(ErrorType::Value));
}
