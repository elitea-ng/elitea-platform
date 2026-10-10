//! Deleting an index (ADR-0031 phase D0, issue #1243): one wiki, every wiki
//! of a project, the engine tools that expose both, the orphan listing, and
//! the model a publish records on the wiki row.

mod storage_common;

use elitea_deepwiki_engine::runner::maintenance;
use elitea_deepwiki_engine::storage::build::{Build, BuildSpace, PublishSettings, WikiRecord};
use elitea_deepwiki_engine::storage::delete;
use elitea_deepwiki_engine::storage::rows::{IndexEdge, IndexNode};
use elitea_deepwiki_engine::storage::{PROJECT_ARG, WikiKey};
use serde_json::{Value, json};
use sqlx::postgres::PgPool;
use std::collections::HashSet;
use storage_common as common;

const DIM: usize = 3;

/// Every table that holds a wiki's rows, with the filter column set.
const TABLES: [&str; 7] = [
    "wikis",
    "wiki_nodes",
    "wiki_edges",
    "wiki_node_embeddings",
    "wiki_bm25_meta",
    "wiki_bm25_docs",
    "wiki_bm25_postings",
];

fn nodes(tag: &str, count: usize) -> Vec<IndexNode> {
    (0..count)
        .map(|i| IndexNode {
            node_id: format!("n{i}"),
            symbol_name: format!("{tag}{i}"),
            source_text: format!("def {tag}{i}(): return shared {tag}"),
            ..IndexNode::default()
        })
        .collect()
}

async fn stage(space: &BuildSpace, key: &WikiKey, tag: &str, count: usize) -> Build {
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
        .map(|i| {
            (
                format!("n{i}"),
                vec![f64::from(u32::try_from(i).expect("small")); DIM],
            )
        })
        .collect();
    let mut build = space.begin(key).await.expect("begin");
    build.stage_nodes(nodes).await.expect("nodes");
    build.stage_edges(&edges).await.expect("edges");
    build
        .stage_embeddings(vectors.iter().map(|(id, v)| (id.as_str(), v.as_slice())))
        .await
        .expect("embeddings");
    build
}

async fn publish(space: &BuildSpace, key: &WikiKey, tag: &str, count: usize) {
    stage(space, key, tag, count)
        .await
        .publish(&WikiRecord {
            repo: Some("acme/widgets".into()),
            ..WikiRecord::default()
        })
        .await
        .expect("publish");
}

/// The rows of `(project, wiki)` in each of [`TABLES`].
async fn rows(pool: &PgPool, project: i32, wiki: &str) -> Vec<i64> {
    let mut counts = Vec::new();
    for table in TABLES {
        // A table name from the list above, never input.
        let count: i64 = sqlx::query_scalar(&format!(
            "SELECT count(*) FROM {table} WHERE project_id = $1 AND wiki_id = $2"
        ))
        .bind(project)
        .bind(wiki)
        .fetch_one(pool)
        .await
        .expect("count");
        counts.push(count);
    }
    counts
}

/// A published wiki of `count` nodes has rows in every table.
fn populated(counts: &[i64]) -> bool {
    counts.iter().all(|c| *c > 0)
}

#[tokio::test(flavor = "multi_thread")]
async fn deleting_a_wiki_removes_every_table_and_only_that_wiki() {
    let Some(pool) = common::fresh_database("delete_one").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "deleter");
    let doomed = common::key_in(1, "acme--doomed--main");
    let neighbour = common::key_in(1, "acme--neighbour--main");
    let other_project = common::key_in(2, "acme--doomed--main");
    publish(&space, &doomed, "alpha", 6).await;
    publish(&space, &neighbour, "beta", 4).await;
    publish(&space, &other_project, "gamma", 5).await;
    assert!(populated(&rows(&pool, 1, "acme--doomed--main").await));

    let before_neighbour = rows(&pool, 1, "acme--neighbour--main").await;
    let before_other = rows(&pool, 2, "acme--doomed--main").await;
    let deleted = delete::delete_wiki(&pool, &doomed, &PublishSettings::default())
        .await
        .expect("delete");

    assert!(deleted.existed);
    assert_eq!(deleted.rows.nodes, 6);
    assert_eq!(deleted.rows.edges, 5);
    assert_eq!(deleted.rows.embeddings, 6);
    assert!(deleted.rows.statistics > 0);
    assert_eq!(
        rows(&pool, 1, "acme--doomed--main").await,
        vec![0; TABLES.len()]
    );
    assert_eq!(
        rows(&pool, 1, "acme--neighbour--main").await,
        before_neighbour
    );
    assert_eq!(rows(&pool, 2, "acme--doomed--main").await, before_other);

    // A second delete is a no-op, not an error.
    let again = delete::delete_wiki(&pool, &doomed, &PublishSettings::default())
        .await
        .expect("delete again");
    assert!(!again.existed);
    assert_eq!(again.total_rows(), 0);
}

/// A publish and a delete of one wiki, started together, in many rounds:
/// the wiki ends whole (the publish committed last) or absent (the delete
/// did), never with some tables emptied and others full.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_concurrent_publish_and_delete_leave_no_half_state() {
    let Some(pool) = common::fresh_database("delete_race").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "racer");
    let key = common::key_in(1, "acme--race--main");
    publish(&space, &key, "seed", 40).await;
    let (mut whole, mut absent) = (0, 0);
    for round in 0..12 {
        let mut build = stage(&space, &key, "next", 40 + round).await;
        let record = WikiRecord::default();
        let settings = PublishSettings::default();
        let (published, deleted) = tokio::join!(
            build.publish(&record),
            delete::delete_wiki(&pool, &key, &settings)
        );
        published.expect("publish");
        deleted.expect("delete");
        let counts = rows(&pool, 1, "acme--race--main").await;
        if counts.iter().all(|c| *c == 0) {
            absent += 1;
        } else {
            assert!(populated(&counts), "round {round}: a half state {counts:?}");
            let nodes: Vec<String> = sqlx::query_scalar(
                "SELECT DISTINCT left(symbol_name, 4) FROM wiki_nodes \
                 WHERE wiki_id = 'acme--race--main'",
            )
            .fetch_all(&pool)
            .await
            .expect("tags");
            assert!(
                nodes.len() <= 1,
                "round {round}: two builds mixed {nodes:?}"
            );
            whole += 1;
        }
        // Put a wiki back for the next round.
        publish(&space, &key, "seed", 40).await;
    }
    eprintln!("race: {whole} whole, {absent} absent");
}

#[tokio::test(flavor = "multi_thread")]
async fn deleting_a_project_removes_all_its_wikis_and_no_other_project() {
    let Some(pool) = common::fresh_database("delete_project").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "deleter");
    for wiki in ["w-a", "w-b", "w-c"] {
        publish(&space, &common::key_in(7, wiki), "alpha", 3).await;
    }
    publish(&space, &common::key_in(8, "w-a"), "beta", 3).await;
    // An unfinished generation of the doomed project.
    let _pending = stage(&space, &common::key_in(7, "w-d"), "alpha", 2).await;

    let deleted = delete::delete_project(&pool, common::project(7), &PublishSettings::default())
        .await
        .expect("delete project");

    assert_eq!(deleted.wikis, ["w-a", "w-b", "w-c"]);
    assert_eq!(deleted.rows.nodes, 9);
    assert_eq!(deleted.builds, 1);
    for table in TABLES {
        let left: i64 = sqlx::query_scalar(&format!(
            "SELECT count(*) FROM {table} WHERE project_id = 7"
        ))
        .fetch_one(&pool)
        .await
        .expect("count");
        assert_eq!(left, 0, "{table}");
    }
    let builds: i64 =
        sqlx::query_scalar("SELECT count(*) FROM deepwiki_build.builds WHERE project_id = 7")
            .fetch_one(&pool)
            .await
            .expect("builds");
    assert_eq!(builds, 0);
    assert!(
        populated(&rows(&pool, 8, "w-a").await),
        "project 8 untouched"
    );

    // Idempotent.
    let again = delete::delete_project(&pool, common::project(7), &PublishSettings::default())
        .await
        .expect("again");
    assert!(again.wikis.is_empty());
}

fn arguments(project: i32, extra: &Value) -> serde_json::Map<String, Value> {
    let mut map = extra.as_object().cloned().unwrap_or_default();
    map.insert(PROJECT_ARG.to_owned(), json!(project.to_string()));
    map
}

#[tokio::test(flavor = "multi_thread")]
async fn the_engine_tools_delete_inside_the_stamped_project() {
    let Some(pool) = common::fresh_database("delete_tools").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "deleter");
    publish(&space, &common::key_in(1, "w-a"), "alpha", 3).await;
    publish(&space, &common::key_in(1, "w-b"), "alpha", 3).await;
    publish(&space, &common::key_in(2, "w-a"), "beta", 3).await;
    let settings = PublishSettings::default();

    // Another project's stamp cannot reach project 1's wiki.
    let wrong = maintenance::run(
        "delete_wiki_index",
        &arguments(3, &json!({"wiki_id": "w-a"})),
        &pool,
        &settings,
    )
    .await
    .expect("run");
    assert_eq!(wrong["deleted"], false);
    assert!(populated(&rows(&pool, 1, "w-a").await));

    let one = maintenance::run(
        "delete_wiki_index",
        &arguments(1, &json!({"wiki_id": "w-a"})),
        &pool,
        &settings,
    )
    .await
    .expect("run");
    assert_eq!(one["success"], true);
    assert_eq!(one["deleted"], true);
    assert_eq!(one["rows"]["nodes"], 3);
    assert!(populated(&rows(&pool, 1, "w-b").await));

    let all = maintenance::run(
        "delete_project_wikis",
        &arguments(1, &json!({})),
        &pool,
        &settings,
    )
    .await
    .expect("run");
    assert_eq!(all["wikis"], json!(["w-b"]));
    assert!(populated(&rows(&pool, 2, "w-a").await));

    // No project, no wiki id: refused.
    for (tool, args) in [
        ("delete_wiki_index", json!({"wiki_id": "w-a"})),
        ("delete_project_wikis", json!({})),
    ] {
        let refused =
            maintenance::run(tool, args.as_object().expect("object"), &pool, &settings).await;
        assert!(refused.is_err(), "{tool} without a project");
    }
    let nameless = maintenance::run(
        "delete_wiki_index",
        &arguments(1, &json!({"wiki_id": "  "})),
        &pool,
        &settings,
    )
    .await;
    assert!(nameless.is_err());
}

#[tokio::test(flavor = "multi_thread")]
async fn orphans_are_the_indexed_projects_that_no_longer_exist() {
    let Some(pool) = common::fresh_database("delete_orphans").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "deleter");
    publish(&space, &common::key_in(1, "w-a"), "alpha", 2).await;
    publish(&space, &common::key_in(2, "w-a"), "alpha", 2).await;
    publish(&space, &common::key_in(2, "w-b"), "alpha", 2).await;
    publish(&space, &common::key_in(3, "w-a"), "alpha", 2).await;

    let existing: HashSet<i32> = [1, 3, 99].into_iter().collect();
    let orphans = delete::orphans(&pool, &existing).await.expect("orphans");
    assert_eq!(orphans.len(), 1);
    assert_eq!((orphans[0].project_id, orphans[0].wikis), (2, 2));
    // Reading deleted nothing.
    assert!(populated(&rows(&pool, 2, "w-a").await));
}

#[tokio::test(flavor = "multi_thread")]
async fn a_publish_records_the_model_and_a_later_one_without_replaces_it() {
    let Some(pool) = common::fresh_database("delete_model").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "recorder");
    let key = common::key_in(1, "w-a");
    let recorded = |pool: PgPool| async move {
        sqlx::query_as::<_, (Option<String>, Option<i32>)>(
            "SELECT embedding_model, embedding_dim FROM wikis WHERE wiki_id = 'w-a'",
        )
        .fetch_one(&pool)
        .await
        .expect("row")
    };

    // A wiki from before the columns: both NULL.
    publish(&space, &key, "alpha", 2).await;
    assert_eq!(recorded(pool.clone()).await, (None, None));

    stage(&space, &key, "alpha", 2)
        .await
        .publish(&WikiRecord {
            embedding_model: Some("text-embedding-3-small".into()),
            embedding_dim: Some(1536),
            ..WikiRecord::default()
        })
        .await
        .expect("publish");
    assert_eq!(
        recorded(pool.clone()).await,
        (Some("text-embedding-3-small".into()), Some(1536))
    );

    // The next publish replaces the vectors, so it replaces the model too.
    publish(&space, &key, "alpha", 2).await;
    assert_eq!(recorded(pool).await, (None, None));
}
