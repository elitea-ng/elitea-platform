//! Deleting an index (ADR-0031 phase D0, issue #1243): one wiki, every wiki
//! of a project, the engine tools that expose both, the orphan listing, and
//! the model a publish records on the wiki row.

mod storage_common;

use elitea_deepwiki_engine::runner::StopSignal;
use elitea_deepwiki_engine::runner::maintenance;
use elitea_deepwiki_engine::storage::StorageError;
use elitea_deepwiki_engine::storage::build::{
    Build, BuildSpace, PUBLISH_WIKI_LOCK, PublishSettings, WikiRecord, publish_wiki_lock_object,
};
use elitea_deepwiki_engine::storage::delete::{self, ProjectLimits, SweepOptions};
use elitea_deepwiki_engine::storage::rows::{IndexEdge, IndexNode};
use elitea_deepwiki_engine::storage::{PROJECT_ARG, WikiKey};
use serde_json::{Value, json};
use sqlx::postgres::PgPool;
use std::collections::HashSet;
use std::time::Duration;
use storage_common as common;

const DIM: usize = 3;

/// The staleness limit the tests use (the sweep's default is 2 h).
const STALE_AFTER: Duration = Duration::from_hours(1);

fn limits() -> ProjectLimits {
    ProjectLimits::new(STALE_AFTER)
}

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
    // A generation of the doomed project that is still running: its
    // heartbeat is live.
    let running = stage(&space, &common::key_in(7, "w-d"), "alpha", 2).await;

    let deleted = delete::delete_project(
        &pool,
        common::project(7),
        &PublishSettings::default(),
        &limits(),
    )
    .await
    .expect("delete project");

    assert_eq!(deleted.wikis, ["w-a", "w-b", "w-c"]);
    assert_eq!(deleted.rows.nodes, 9);
    // The running generation is left to its run.
    assert_eq!(deleted.builds, 0);
    assert_eq!(deleted.live_builds, 1);
    let builds: i64 =
        sqlx::query_scalar("SELECT count(*) FROM deepwiki_build.builds WHERE project_id = 7")
            .fetch_one(&pool)
            .await
            .expect("builds");
    assert_eq!(builds, 1, "a live-heartbeat build survives the deletion");

    // Its run stops beating (the replica went away): now it is stale, and
    // the next deletion removes it.
    drop(running);
    sqlx::query(
        "UPDATE deepwiki_build.builds SET heartbeat_at = now() - interval '3 hours' \
         WHERE project_id = 7",
    )
    .execute(&pool)
    .await
    .expect("age the heartbeat");
    let stale = delete::delete_project(
        &pool,
        common::project(7),
        &PublishSettings::default(),
        &limits(),
    )
    .await
    .expect("delete the stale build");
    assert!(stale.wikis.is_empty());
    assert_eq!((stale.builds, stale.live_builds), (1, 0));
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
    let again = delete::delete_project(
        &pool,
        common::project(7),
        &PublishSettings::default(),
        &limits(),
    )
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
        STALE_AFTER,
        &StopSignal::default(),
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
        STALE_AFTER,
        &StopSignal::default(),
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
        STALE_AFTER,
        &StopSignal::default(),
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
        let refused = maintenance::run(
            tool,
            args.as_object().expect("object"),
            &pool,
            &settings,
            STALE_AFTER,
            &StopSignal::default(),
        )
        .await;
        assert!(refused.is_err(), "{tool} without a project");
    }
    let nameless = maintenance::run(
        "delete_wiki_index",
        &arguments(1, &json!({"wiki_id": "  "})),
        &pool,
        &settings,
        STALE_AFTER,
        &StopSignal::default(),
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

/// `deleted` means ANY row was removed, with or without a `wikis` row: the
/// `wiki_bm25_*` tables have no foreign key, so statistics can outlive it.
#[tokio::test(flavor = "multi_thread")]
async fn deleting_stray_rows_with_no_wikis_row_is_a_deletion() {
    let Some(pool) = common::fresh_database("delete_stray").await else {
        return;
    };
    sqlx::raw_sql(
        "INSERT INTO wiki_bm25_meta (project_id, wiki_id, branch, doc_count, avgdl, k1, b)
             VALUES (1, 'stray', 'fts', 1, 1.0, 1.2, 0.75);",
    )
    .execute(&pool)
    .await
    .expect("a stray statistics row");
    let settings = PublishSettings::default();
    let key = common::key_in(1, "stray");
    let first = delete::delete_wiki(&pool, &key, &settings)
        .await
        .expect("delete");
    assert!(!first.existed, "there was no wikis row");
    assert!(first.deleted(), "but a row was removed");
    assert_eq!(first.rows.statistics, 1);

    let out = maintenance::run(
        "delete_wiki_index",
        &arguments(1, &json!({"wiki_id": "stray"})),
        &pool,
        &settings,
        STALE_AFTER,
        &StopSignal::default(),
    )
    .await
    .expect("run");
    assert_eq!(out["deleted"], false, "nothing is left now");
    assert_eq!(out["rows"]["wikis"], 0);

    sqlx::raw_sql(
        "INSERT INTO wiki_bm25_meta (project_id, wiki_id, branch, doc_count, avgdl, k1, b)
             VALUES (1, 'stray', 'fts', 1, 1.0, 1.2, 0.75);",
    )
    .execute(&pool)
    .await
    .expect("again");
    let out = maintenance::run(
        "delete_wiki_index",
        &arguments(1, &json!({"wiki_id": "stray"})),
        &pool,
        &settings,
        STALE_AFTER,
        &StopSignal::default(),
    )
    .await
    .expect("run");
    assert_eq!(out["deleted"], true);
    assert_eq!(out["rows"]["statistics"], 1);
    assert_eq!(out["rows"]["wikis"], 0);
}

/// `delete_project` loops until no wiki remains: more wikis than one round
/// lists are all deleted, and a project that keeps refilling past the cap is
/// an error naming how many remain.
#[tokio::test(flavor = "multi_thread")]
async fn a_project_deletion_loops_until_empty_or_fails_naming_the_remainder() {
    let Some(pool) = common::fresh_database("delete_cap").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "deleter");
    for wiki in ["w-1", "w-2", "w-3", "w-4", "w-5"] {
        publish(&space, &common::key_in(4, wiki), "alpha", 1).await;
    }
    let settings = PublishSettings::default();

    // Two rounds of one wiki cannot clear five: an error that says 3 remain.
    let capped = ProjectLimits {
        stale_after: STALE_AFTER,
        batch: 1,
        max_rounds: 2,
    };
    let error = delete::delete_project(&pool, common::project(4), &settings, &capped)
        .await
        .expect_err("the cap was hit");
    let message = error.to_string();
    assert!(
        message.contains("project 4") && message.contains("still holds 3 wiki(s)"),
        "{message}"
    );
    assert_eq!(
        delete::project_wikis(&pool, common::project(4))
            .await
            .expect("left")
            .len(),
        3
    );

    // A smaller batch than the project, with enough rounds: all of it.
    let batched = ProjectLimits {
        stale_after: STALE_AFTER,
        batch: 2,
        max_rounds: 10,
    };
    let done = delete::delete_project(&pool, common::project(4), &settings, &batched)
        .await
        .expect("finishes");
    assert_eq!(done.wikis.len(), 3);
    assert!(
        delete::project_wikis(&pool, common::project(4))
            .await
            .expect("left")
            .is_empty()
    );
    // The cap landing exactly on the last wiki is a success, not an error.
    publish(&space, &common::key_in(4, "w-x"), "alpha", 1).await;
    let exact = ProjectLimits {
        stale_after: STALE_AFTER,
        batch: 1,
        max_rounds: 1,
    };
    let done = delete::delete_project(&pool, common::project(4), &settings, &exact)
        .await
        .expect("the last wiki fits the cap");
    assert_eq!(done.wikis, ["w-x"]);
}

/// `ask` embeds with the model the wiki row recorded.
#[tokio::test(flavor = "multi_thread")]
async fn the_stored_embedding_model_is_read_from_the_wiki_row() {
    let Some(pool) = common::fresh_database("stored_model").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "asker");
    let with_model = common::key_in(1, "w-model");
    let without = common::key_in(1, "w-none");
    stage(&space, &with_model, "alpha", 2)
        .await
        .publish(&WikiRecord {
            embedding_model: Some("emb-1".into()),
            ..WikiRecord::default()
        })
        .await
        .expect("publish");
    publish(&space, &without, "beta", 2).await;
    let project = common::project(1);
    let stored = |wiki: &str| {
        let pool = pool.clone();
        let wiki = wiki.to_owned();
        async move {
            elitea_deepwiki_engine::ask::stored_embedding_model(&pool, project, &wiki)
                .await
                .expect("read")
        }
    };
    assert_eq!(stored("w-model").await.as_deref(), Some("emb-1"));
    // An older wiki (NULL) and a wiki that is not indexed: nothing stored.
    assert_eq!(stored("w-none").await, None);
    assert_eq!(stored("w-missing").await, None);
    // Another project's wiki of the same id is not read.
    assert_eq!(
        elitea_deepwiki_engine::ask::stored_embedding_model(&pool, common::project(2), "w-model")
            .await
            .expect("read"),
        None
    );
}

fn now_minus(minutes: i64) -> String {
    format!("now() - interval '{minutes} minutes'")
}

/// The database's RFC 3339 rendering of `now() - minutes`.
async fn listed_at(pool: &PgPool, minutes: i64) -> String {
    sqlx::query_scalar(&format!(
        "SELECT to_char(({}) AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')",
        now_minus(minutes)
    ))
    .fetch_one(pool)
    .await
    .expect("time")
}

#[tokio::test(flavor = "multi_thread")]
async fn a_stale_or_future_project_list_is_refused_for_a_deletion() {
    let Some(pool) = common::fresh_database("orphan_fresh").await else {
        return;
    };
    let fresh = listed_at(&pool, 1).await;
    delete::check_listing_fresh(&pool, &fresh, false)
        .await
        .expect("a minute-old list is fresh");

    let stale = listed_at(&pool, 11).await;
    let refused = delete::check_listing_fresh(&pool, &stale, false)
        .await
        .expect_err("11 minutes is stale");
    assert!(
        refused.to_string().contains("--allow-stale-list"),
        "{refused}"
    );
    delete::check_listing_fresh(&pool, &stale, true)
        .await
        .expect("--allow-stale-list accepts it");

    let future = listed_at(&pool, -30).await;
    let refused = delete::check_listing_fresh(&pool, &future, true)
        .await
        .expect_err("a list from the future is never accepted");
    assert!(refused.to_string().contains("future"), "{refused}");

    let nonsense = delete::check_listing_fresh(&pool, "2026-13-45T99:00:00Z", false)
        .await
        .expect_err("not a time");
    assert!(
        nonsense.to_string().contains("not a valid time"),
        "{nonsense}"
    );
}

/// A project created after the list was taken is not in it and looks like an
/// orphan; the sweep skips it, prints it as skipped, and deletes the rest.
#[tokio::test(flavor = "multi_thread")]
async fn the_orphan_sweep_skips_projects_with_activity_after_the_list() {
    let Some(pool) = common::fresh_database("orphan_sweep").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "sweeper");
    // Project 2: an old index of a deleted project.
    publish(&space, &common::key_in(2, "w-old"), "alpha", 2).await;
    sqlx::query(
        "UPDATE wikis SET created_at = now() - interval '2 hours', \
         updated_at = now() - interval '2 hours' WHERE project_id = 2",
    )
    .execute(&pool)
    .await
    .expect("age");
    // Project 3: a wiki published after the list was taken.
    publish(&space, &common::key_in(3, "w-new"), "beta", 2).await;
    // Project 4: only a build started after the list (a first generation).
    let _building = stage(&space, &common::key_in(4, "w-building"), "gamma", 1).await;
    sqlx::query(
        "INSERT INTO wikis (project_id, wiki_id, repo, branch, created_at, updated_at) \
         VALUES (4, 'w-prior', 'r', 'main', now() - interval '2 hours', now() - interval '2 hours')",
    )
    .execute(&pool)
    .await
    .expect("an old wiki of project 4");

    let existing: HashSet<i32> = [1].into_iter().collect();
    let at = listed_at(&pool, 5).await;
    let settings = PublishSettings::default();

    // A dry run reports, deletes nothing, and marks the skips.
    let dry = delete::sweep_orphans(
        &pool,
        &existing,
        &SweepOptions {
            listed_at: Some(&at),
            delete: false,
            allow_stale_list: false,
        },
        &settings,
        &limits(),
    )
    .await
    .expect("dry run");
    assert_eq!(
        dry.would_delete
            .iter()
            .map(|p| p.project_id)
            .collect::<Vec<_>>(),
        [2]
    );
    let skipped: Vec<i32> = dry.skipped.iter().map(|(p, _)| p.project_id).collect();
    assert_eq!(skipped, [3, 4]);
    assert!(dry.skipped[0].1.contains("w-new"), "{:?}", dry.skipped);
    assert!(dry.skipped[1].1.contains("build"), "{:?}", dry.skipped);
    assert!(populated(&rows(&pool, 2, "w-old").await));

    // Deleting needs a listing time.
    let missing = delete::sweep_orphans(
        &pool,
        &existing,
        &SweepOptions {
            listed_at: None,
            delete: true,
            allow_stale_list: false,
        },
        &settings,
        &limits(),
    )
    .await
    .expect_err("no listed-at");
    assert!(missing.to_string().contains("--listed-at"), "{missing}");

    let done = delete::sweep_orphans(
        &pool,
        &existing,
        &SweepOptions {
            listed_at: Some(&at),
            delete: true,
            allow_stale_list: false,
        },
        &settings,
        &limits(),
    )
    .await
    .expect("delete");
    assert_eq!(done.deleted.len(), 1);
    assert_eq!(done.deleted[0].0.project_id, 2);
    assert!(done.failed.is_empty());
    assert_eq!(done.skipped.len(), 2);
    assert_eq!(rows(&pool, 2, "w-old").await, vec![0; TABLES.len()]);
    assert!(populated(&rows(&pool, 3, "w-new").await));
    assert!(
        !delete::project_wikis(&pool, common::project(4))
            .await
            .expect("wikis")
            .is_empty()
    );
}

/// One project that cannot be cleared does not stop the others, and is
/// reported in `failed`, which the command turns into a non-zero exit.
#[tokio::test(flavor = "multi_thread")]
async fn the_orphan_sweep_reports_a_project_it_could_not_clear() {
    let Some(pool) = common::fresh_database("orphan_fail").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "sweeper");
    for wiki in ["w-1", "w-2", "w-3"] {
        publish(&space, &common::key_in(2, wiki), "alpha", 1).await;
    }
    publish(&space, &common::key_in(3, "w-1"), "alpha", 1).await;
    sqlx::query("UPDATE wikis SET created_at = now() - interval '2 hours', updated_at = now() - interval '2 hours'")
        .execute(&pool)
        .await
        .expect("age");
    let existing: HashSet<i32> = [1].into_iter().collect();
    let at = listed_at(&pool, 1).await;
    let capped = ProjectLimits {
        stale_after: STALE_AFTER,
        batch: 1,
        max_rounds: 2,
    };
    let done = delete::sweep_orphans(
        &pool,
        &existing,
        &SweepOptions {
            listed_at: Some(&at),
            delete: true,
            allow_stale_list: false,
        },
        &PublishSettings::default(),
        &capped,
    )
    .await
    .expect("sweep");
    assert_eq!(done.failed.len(), 1);
    assert_eq!(done.failed[0].0.project_id, 2);
    assert!(
        done.failed[0].1.contains("still holds 1 wiki(s)"),
        "{:?}",
        done.failed
    );
    // The project that did fit was deleted.
    assert_eq!(done.deleted.len(), 1);
    assert_eq!(done.deleted[0].0.project_id, 3);
}

/// Take a publish's per-wiki advisory lock in a transaction of its own, the
/// way a publish in progress holds it.
async fn hold_publish_lock(pool: &PgPool, key: &WikiKey) -> sqlx::Transaction<'static, sqlx::Postgres> {
    let mut holder = pool.begin().await.expect("holder");
    sqlx::query("SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))")
        .bind(PUBLISH_WIKI_LOCK)
        .bind(publish_wiki_lock_object(key))
        .execute(&mut *holder)
        .await
        .expect("take the publish lock");
    holder
}

/// Sessions waiting on an advisory lock right now.
async fn advisory_waiters(pool: &PgPool) -> i64 {
    sqlx::query_scalar(
        "SELECT count(*) FROM pg_stat_activity \
         WHERE wait_event_type = 'Lock' AND wait_event = 'advisory' \
           AND datname = current_database()",
    )
    .fetch_one(pool)
    .await
    .expect("waiters")
}

/// The orphan sweep re-checks the project IMMEDIATELY before each wiki's
/// deletion, under that wiki's lock: a wiki published after the sweep began
/// (while it waited on another wiki's publish) stops it, and survives.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_wiki_published_mid_sweep_survives_and_the_project_is_reported_skipped() {
    let Some(pool) = common::fresh_database("sweep_midway").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "sweeper");
    for wiki in ["w-1", "w-2", "w-3"] {
        publish(&space, &common::key_in(2, wiki), "alpha", 2).await;
    }
    sqlx::query(
        "UPDATE wikis SET created_at = now() - interval '2 hours', \
         updated_at = now() - interval '2 hours'",
    )
    .execute(&pool)
    .await
    .expect("age");
    let existing: HashSet<i32> = [1].into_iter().collect();
    let at = listed_at(&pool, 1).await;

    // The sweep deletes w-1, then queues behind a "publish" of w-2.
    let holder = hold_publish_lock(&pool, &common::key_in(2, "w-2")).await;
    let sweep = {
        let pool = pool.clone();
        let existing = existing.clone();
        let at = at.clone();
        tokio::spawn(async move {
            delete::sweep_orphans(
                &pool,
                &existing,
                &SweepOptions {
                    listed_at: Some(&at),
                    delete: true,
                    allow_stale_list: false,
                },
                &PublishSettings::default(),
                &ProjectLimits {
                    stale_after: STALE_AFTER,
                    batch: 1,
                    max_rounds: 10,
                },
            )
            .await
        })
    };
    for _ in 0..100 {
        if advisory_waiters(&pool).await > 0 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    assert_eq!(advisory_waiters(&pool).await, 1, "the sweep waits on w-2");

    // A new wiki of the project is published meanwhile, then w-2's publish ends.
    publish(&space, &common::key_in(2, "w-fresh"), "beta", 2).await;
    holder.rollback().await.expect("release");

    let done = sweep.await.expect("join").expect("sweep");
    assert!(done.failed.is_empty(), "{:?}", done.failed);
    assert!(done.deleted.is_empty(), "{:?}", done.deleted);
    assert_eq!(done.skipped.len(), 1, "{:?}", done.skipped);
    assert_eq!(done.skipped[0].0.project_id, 2);
    assert!(done.skipped[0].1.contains("w-fresh"), "{:?}", done.skipped);
    // w-1 went before the newer wiki appeared; everything else survives.
    assert_eq!(rows(&pool, 2, "w-1").await, vec![0; TABLES.len()]);
    for wiki in ["w-2", "w-3", "w-fresh"] {
        assert!(populated(&rows(&pool, 2, wiki).await), "{wiki} survives: {:?}", rows(&pool, 2, wiki).await);
    }
}

/// The wait for a publish of the same wiki is bounded: past it the deletion
/// answers "being published; retry" and deletes nothing.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_deletion_gives_up_waiting_for_a_publish_and_deletes_nothing() {
    let Some(pool) = common::fresh_database("delete_lock_wait").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "publisher");
    let key = common::key("w-busy");
    publish(&space, &key, "alpha", 2).await;
    let holder = hold_publish_lock(&pool, &key).await;
    let settings = PublishSettings {
        delete_lock_wait: Duration::from_millis(400),
        ..PublishSettings::default()
    };
    let started = std::time::Instant::now();
    let error = delete::delete_wiki(&pool, &key, &settings)
        .await
        .expect_err("the lock is held");
    assert!(matches!(error, StorageError::Busy(_)), "{error:?}");
    assert!(error.to_string().contains("being published"), "{error}");
    assert!(error.to_string().contains("retry"), "{error}");
    assert!(
        started.elapsed() < Duration::from_secs(10),
        "waited {:?}",
        started.elapsed()
    );
    assert!(populated(&rows(&pool, 1, "w-busy").await), "nothing deleted");

    // As a tool: a runtime error a caller can read and retry.
    let refused = maintenance::run(
        "delete_wiki_index",
        &arguments(1, &json!({"wiki_id": "w-busy"})),
        &pool,
        &settings,
        STALE_AFTER,
        &StopSignal::default(),
    )
    .await
    .expect_err("busy");
    assert!(refused.to_string().contains("retry"), "{refused}");

    // Released, the same deletion goes through.
    holder.rollback().await.expect("release");
    let deleted = delete::delete_wiki(&pool, &key, &settings)
        .await
        .expect("delete");
    assert!(deleted.existed);
}

/// Dropping the deletion (the invocation was cancelled) cancels its wait in
/// the database: no session is left queued on the lock.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_cancelled_deletion_stops_waiting_in_the_database() {
    let Some(pool) = common::fresh_database("delete_cancel").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "publisher");
    let key = common::key("w-cancel");
    publish(&space, &key, "alpha", 2).await;
    let holder = hold_publish_lock(&pool, &key).await;

    // Directly: the future is aborted while it waits.
    let attempt = {
        let pool = pool.clone();
        let key = key.clone();
        tokio::spawn(async move {
            delete::delete_wiki(&pool, &key, &PublishSettings::default()).await
        })
    };
    for _ in 0..100 {
        if advisory_waiters(&pool).await > 0 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    assert_eq!(advisory_waiters(&pool).await, 1);
    attempt.abort();
    let _ = attempt.await;
    for _ in 0..100 {
        if advisory_waiters(&pool).await == 0 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    assert_eq!(advisory_waiters(&pool).await, 0, "the waiter was cancelled");

    // Through the tool: a stop request ends the wait with the stop line.
    let stop = StopSignal::default();
    let tool = {
        let pool = pool.clone();
        let stop = stop.clone();
        tokio::spawn(async move {
            maintenance::run(
                "delete_wiki_index",
                &arguments(1, &json!({"wiki_id": "w-cancel"})),
                &pool,
                &PublishSettings::default(),
                STALE_AFTER,
                &stop,
            )
            .await
        })
    };
    for _ in 0..100 {
        if advisory_waiters(&pool).await > 0 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    assert_eq!(advisory_waiters(&pool).await, 1);
    stop.request();
    let stopped = tokio::time::timeout(Duration::from_secs(10), tool)
        .await
        .expect("the stop ends the tool")
        .expect("join");
    assert!(stopped.is_err());
    for _ in 0..100 {
        if advisory_waiters(&pool).await == 0 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    assert_eq!(advisory_waiters(&pool).await, 0);

    // Nothing was deleted, and once the publish ends a deletion works.
    holder.rollback().await.expect("release");
    assert!(populated(&rows(&pool, 1, "w-cancel").await), "{:?}", rows(&pool, 1, "w-cancel").await);
    let deleted = delete::delete_wiki(&pool, &key, &PublishSettings::default())
        .await
        .expect("delete");
    assert!(deleted.existed);
}

/// The question's embedding model is chosen from one read of the wiki's row;
/// the dense search re-checks it INSIDE its own snapshot. A wiki republished
/// with another model between the two is refused, not ranked by a distance
/// between vectors of different spaces.
#[tokio::test(flavor = "multi_thread")]
async fn a_wiki_republished_with_another_model_between_steps_is_refused() {
    use elitea_deepwiki_engine::ask::stored_embedding_model;
    use elitea_deepwiki_engine::storage::adapter::{Scope, UnifiedDb};
    use elitea_deepwiki_engine::storage::search::Hybrid;

    let Some(pool) = common::fresh_database("model_between").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "recorder");
    let key = common::key_in(1, "w-a");
    let publish_with = |model: &'static str| {
        let space = space.clone();
        let key = key.clone();
        async move {
            stage(&space, &key, "alpha", 3)
                .await
                .publish(&WikiRecord {
                    embedding_model: Some(model.into()),
                    embedding_dim: Some(3),
                    ..WikiRecord::default()
                })
                .await
                .expect("publish");
        }
    };
    let search = |db: UnifiedDb| async move {
        db.search_hybrid(
            "alpha",
            Some(&[1.0, 1.0, 1.0]),
            &Scope::default(),
            &Hybrid::default(),
        )
        .await
    };

    publish_with("model-a").await;
    // Step 1: the model is chosen from the row.
    let chosen = stored_embedding_model(&pool, common::project(1), "w-a")
        .await
        .expect("read")
        .expect("recorded");
    assert_eq!(chosen, "model-a");
    // Not republished yet: the search is served.
    let db = UnifiedDb::new(pool.clone(), key.clone()).expecting_embedding_model(chosen.clone());
    assert!(!search(db).await.expect("same model").is_empty());

    // The wiki is republished with another model BETWEEN the steps.
    publish_with("model-b").await;
    let db = UnifiedDb::new(pool.clone(), key.clone()).expecting_embedding_model(chosen);
    let refused = search(db).await.expect_err("the vectors are model-b's now");
    assert!(
        matches!(&refused, StorageError::EmbeddingModelChanged { stored, expected, .. }
            if stored == "model-b" && expected == "model-a"),
        "{refused:?}"
    );
    assert!(refused.to_string().contains("model-b"), "{refused}");

    // A caller that chose the current model is served; one that did not
    // record a model (the plain reader) is unaffected.
    let db = UnifiedDb::new(pool.clone(), key.clone()).expecting_embedding_model("model-b");
    assert!(!search(db).await.expect("current model").is_empty());
    let unchecked = UnifiedDb::new(pool.clone(), key.clone());
    assert!(!search(unchecked).await.expect("unchecked").is_empty());

    // A wiki that records no model has nothing to refuse.
    publish(&space, &key, "alpha", 3).await;
    let db = UnifiedDb::new(pool.clone(), key).expecting_embedding_model("anything");
    assert!(!search(db).await.expect("no model recorded").is_empty());
}
