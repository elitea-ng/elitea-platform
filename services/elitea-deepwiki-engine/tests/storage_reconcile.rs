//! Reconciliation of abandoned builds (ADR-0026 decision 5): keyed by
//! owner at startup, by heartbeat age in the sweep. Two owners share one
//! database, as two replicas do.

mod storage_common;

use elitea_deepwiki_engine::storage::StorageError;
use elitea_deepwiki_engine::storage::build::{BuildSpace, WikiRecord, sweep_interval};
use elitea_deepwiki_engine::storage::rows::IndexNode;
use sqlx::postgres::PgPool;
use std::time::Duration;
use storage_common as common;

fn nodes(tag: &str) -> Vec<IndexNode> {
    (0..3)
        .map(|i| IndexNode {
            node_id: format!("{tag}::{i}"),
            symbol_name: format!("{tag}{i}"),
            source_text: format!("{tag} body {i}"),
            ..IndexNode::default()
        })
        .collect()
}

async fn staged_rows(pool: &PgPool, build_id: &str) -> i64 {
    sqlx::query_scalar(
        "SELECT (SELECT count(*) FROM deepwiki_build.wiki_nodes WHERE build_id = $1) \
              + (SELECT count(*) FROM deepwiki_build.bm25_docs WHERE build_id = $1) \
              + (SELECT count(*) FROM deepwiki_build.bm25_postings WHERE build_id = $1)",
    )
    .bind(build_id)
    .fetch_one(pool)
    .await
    .expect("count")
}

#[tokio::test(flavor = "multi_thread")]
async fn startup_reconciliation_removes_only_this_owners_builds() {
    let Some(pool) = common::fresh_database("reconcile_owner").await else {
        return;
    };
    let replica_a = BuildSpace::new(pool.clone(), "replica-a");
    let replica_b = BuildSpace::new(pool.clone(), "replica-b");

    // Replica A had two builds in flight when it died; replica B has one.
    let mut a1 = replica_a.begin("acme--one--main").await.expect("begin");
    a1.stage_nodes(nodes("a1")).await.expect("stage");
    let mut a2 = replica_a.begin("acme--two--main").await.expect("begin");
    a2.stage_nodes(nodes("a2")).await.expect("stage");
    let mut b1 = replica_b.begin("acme--one--main").await.expect("begin");
    b1.stage_nodes(nodes("b1")).await.expect("stage");
    let (a1_id, a2_id) = (a1.build_id().to_owned(), a2.build_id().to_owned());
    assert!(staged_rows(&pool, &a1_id).await > 0);
    drop((a1, a2));

    // Replica A restarts under the same owner identity.
    let restarted_a = BuildSpace::new(pool.clone(), "replica-a");
    assert_eq!(restarted_a.reconcile_owner().await.expect("reconcile"), 2);
    assert_eq!(
        staged_rows(&pool, &a1_id).await,
        0,
        "the cascade removed the staged rows"
    );
    assert_eq!(staged_rows(&pool, &a2_id).await, 0);

    // Replica B's build is untouched and still publishes.
    assert!(staged_rows(&pool, b1.build_id()).await > 0);
    b1.heartbeat().await.expect("B is alive");
    let counts = b1.publish(&WikiRecord::default()).await.expect("publish");
    assert_eq!(counts.nodes, 3);
    // Nothing left to reconcile.
    assert_eq!(restarted_a.reconcile_owner().await.expect("reconcile"), 0);
}

#[tokio::test(flavor = "multi_thread")]
async fn the_sweep_removes_stale_builds_of_any_owner() {
    let Some(pool) = common::fresh_database("reconcile_sweep").await else {
        return;
    };
    let replica_a = BuildSpace::new(pool.clone(), "replica-a");
    let replica_b = BuildSpace::new(pool.clone(), "replica-b");
    let mut stale = replica_a.begin("acme--one--main").await.expect("begin");
    stale.stage_nodes(nodes("stale")).await.expect("stage");
    let mut fresh = replica_b.begin("acme--two--main").await.expect("begin");
    fresh.stage_nodes(nodes("fresh")).await.expect("stage");
    // Replica A went away three hours ago.
    sqlx::query(
        "UPDATE deepwiki_build.builds SET heartbeat_at = now() - interval '3 hours' \
         WHERE build_id = $1",
    )
    .bind(stale.build_id())
    .execute(&pool)
    .await
    .expect("age the build");

    // Any replica sweeps; only the stale build goes.
    assert_eq!(
        replica_b
            .sweep(Duration::from_hours(2))
            .await
            .expect("sweep"),
        1
    );
    assert_eq!(staged_rows(&pool, stale.build_id()).await, 0);
    assert!(staged_rows(&pool, fresh.build_id()).await > 0);

    // The swept build learns it at its next heartbeat or stage call, and
    // cannot publish.
    let beat = stale.heartbeat().await;
    assert!(
        matches!(&beat, Err(StorageError::Publish(m)) if m.contains("no longer exists")),
        "{beat:?}"
    );
    let staged = stale.stage_nodes(nodes("late")).await;
    assert!(
        matches!(staged, Err(StorageError::Publish(_))),
        "{staged:?}"
    );
    let published = stale.publish(&WikiRecord::default()).await;
    assert!(
        matches!(&published, Err(StorageError::Publish(m)) if m.contains("no longer exists")),
        "{published:?}"
    );
    let live: i64 = sqlx::query_scalar("SELECT count(*) FROM wiki_nodes")
        .fetch_one(&pool)
        .await
        .expect("count");
    assert_eq!(live, 0, "a swept build published nothing");

    // A heartbeat keeps a long build out of the sweep.
    fresh.heartbeat().await.expect("beat");
    assert_eq!(
        replica_a
            .sweep(Duration::from_mins(1))
            .await
            .expect("sweep"),
        0
    );
    fresh
        .publish(&WikiRecord::default())
        .await
        .expect("publish");
}

#[test]
fn the_sweep_interval_is_bounded() {
    assert_eq!(
        sweep_interval(Duration::from_hours(2)),
        Duration::from_mins(10)
    );
    assert_eq!(
        sweep_interval(Duration::from_mins(4)),
        Duration::from_mins(1)
    );
    assert_eq!(
        sweep_interval(Duration::from_secs(1)),
        Duration::from_secs(10)
    );
}
