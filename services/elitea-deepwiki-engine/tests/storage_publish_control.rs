//! How a publish behaves under contention: a failed publish can be retried
//! or abandoned, the timeouts bound it, it queues for a publish slot, the
//! live tables are `ANALYZE`d after the commit (best effort), and two
//! publishes at once (of one wiki, of two wikis) both land whole.

mod storage_common;

use elitea_deepwiki_engine::storage::StorageError;
use elitea_deepwiki_engine::storage::build::{
    Build, BuildSpace, PUBLISH_SLOT_LOCK, PublishSettings, WikiRecord, is_lock_timeout,
};
use elitea_deepwiki_engine::storage::rows::IndexNode;
use sqlx::postgres::PgPool;
use std::time::{Duration, Instant};
use storage_common as common;

fn nodes(tag: &str, count: usize) -> Vec<IndexNode> {
    (0..count)
        .map(|i| IndexNode {
            node_id: format!("{tag}::{i}"),
            symbol_name: format!("{tag}{i}"),
            source_text: format!("def {tag}{i}(): return common"),
            ..IndexNode::default()
        })
        .collect()
}

async fn staged(space: &BuildSpace, wiki: &str, tag: &str, count: usize) -> Build {
    let mut build = space.begin(wiki).await.expect("begin");
    build.stage_nodes(nodes(tag, count)).await.expect("stage");
    build
}

async fn live_nodes(pool: &PgPool, wiki: &str) -> Vec<String> {
    sqlx::query_scalar("SELECT node_id FROM wiki_nodes WHERE wiki_id = $1 ORDER BY node_id")
        .bind(wiki)
        .fetch_all(pool)
        .await
        .expect("live nodes")
}

async fn build_exists(pool: &PgPool, build: &str) -> bool {
    sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM deepwiki_build.builds WHERE build_id = $1)")
        .bind(build)
        .fetch_one(pool)
        .await
        .expect("build row")
}

fn quick(settings: PublishSettings) -> PublishSettings {
    PublishSettings {
        lock_timeout: Duration::from_millis(300),
        ..settings
    }
}

/// `Build::publish` keeps the build on an error: retry after the cause
/// is gone, stage more and retry, or abandon.
#[tokio::test(flavor = "multi_thread")]
async fn a_failed_publish_can_be_retried_or_abandoned() {
    let Some(pool) = common::fresh_database("control_retry").await else {
        return;
    };
    let wiki = "acme--retry--main";
    let space = BuildSpace::new(pool.clone(), "publisher")
        .with_publish_settings(quick(PublishSettings::default()));

    // Refused as empty, then staged and published by the same build.
    let mut build = space.begin(wiki).await.expect("begin");
    let refused = build.publish(&WikiRecord::default()).await;
    assert!(
        matches!(&refused, Err(StorageError::Publish(m)) if m.contains("staged no nodes")),
        "{refused:?}"
    );
    build
        .stage_nodes(nodes("one", 4))
        .await
        .expect("stage after the refusal");
    let counts = build.publish(&WikiRecord::default()).await.expect("retry");
    assert_eq!(counts.nodes, 4);
    let again = build.publish(&WikiRecord::default()).await;
    assert!(
        matches!(&again, Err(StorageError::Publish(m)) if m.contains("already published")),
        "{again:?}"
    );

    // A lock timeout: another transaction holds the wiki's row.
    let mut holder = pool.begin().await.expect("holder");
    sqlx::query("SELECT 1 FROM wikis WHERE wiki_id = $1 FOR UPDATE")
        .bind(wiki)
        .execute(&mut *holder)
        .await
        .expect("lock the wikis row");
    let mut build = staged(&space, wiki, "two", 6).await;
    let timed_out = build.publish(&WikiRecord::default()).await;
    assert!(
        matches!(&timed_out, Err(StorageError::Database(e)) if is_lock_timeout(e)),
        "{timed_out:?}"
    );
    assert!(
        build_exists(&pool, build.build_id()).await,
        "the build is kept"
    );
    assert_eq!(
        live_nodes(&pool, wiki).await.len(),
        4,
        "the live index is untouched"
    );
    build.heartbeat().await.expect("the build is alive");
    holder.rollback().await.expect("release");
    let counts = build.publish(&WikiRecord::default()).await.expect("retry");
    assert_eq!(counts.nodes, 6);
    assert!(!build_exists(&pool, build.build_id()).await);

    // The same failure, then the documented cleanup.
    let mut holder = pool.begin().await.expect("holder");
    sqlx::query("SELECT 1 FROM wikis WHERE wiki_id = $1 FOR UPDATE")
        .bind(wiki)
        .execute(&mut *holder)
        .await
        .expect("lock the wikis row");
    let mut build = staged(&space, wiki, "three", 2).await;
    assert!(build.publish(&WikiRecord::default()).await.is_err());
    let id = build.build_id().to_owned();
    build
        .abandon()
        .await
        .expect("abandon after a failed publish");
    holder.rollback().await.expect("release");
    assert!(!build_exists(&pool, &id).await);
    assert_eq!(live_nodes(&pool, wiki).await.len(), 6);
}

/// `statement_timeout` applies to the publish statements, after the queue.
#[tokio::test(flavor = "multi_thread")]
async fn a_statement_timeout_bounds_the_publish() {
    let Some(pool) = common::fresh_database("control_statement").await else {
        return;
    };
    let wiki = "acme--slow--main";
    let space = BuildSpace::new(pool.clone(), "publisher");
    staged(&space, wiki, "one", 3)
        .await
        .publish(&WikiRecord::default())
        .await
        .expect("first publish");

    let slow = space.clone().with_publish_settings(PublishSettings {
        statement_timeout: Duration::from_millis(500),
        lock_timeout: Duration::from_mins(1),
        ..PublishSettings::default()
    });
    let mut holder = pool.begin().await.expect("holder");
    sqlx::query("SELECT 1 FROM wikis WHERE wiki_id = $1 FOR UPDATE")
        .bind(wiki)
        .execute(&mut *holder)
        .await
        .expect("lock the wikis row");
    let mut build = staged(&slow, wiki, "two", 3).await;
    let started = Instant::now();
    let cancelled = build.publish(&WikiRecord::default()).await;
    let code = match &cancelled {
        Err(StorageError::Database(e)) => e
            .as_database_error()
            .and_then(sqlx::error::DatabaseError::code)
            .map(std::borrow::Cow::into_owned),
        _ => None,
    };
    assert_eq!(code.as_deref(), Some("57014"), "{cancelled:?}");
    assert!(
        started.elapsed() < Duration::from_secs(30),
        "{:?}",
        started.elapsed()
    );
    holder.rollback().await.expect("release");
    build.abandon().await.expect("abandon");
    assert_eq!(live_nodes(&pool, wiki).await.len(), 3);
}

/// With every publish slot taken, a publish waits (past its own
/// `lock_timeout`) and runs once a slot frees.
#[tokio::test(flavor = "multi_thread")]
async fn a_publish_queues_for_a_slot() {
    let Some(pool) = common::fresh_database("control_slots").await else {
        return;
    };
    let wiki = "acme--queue--main";
    let space = BuildSpace::new(pool.clone(), "publisher").with_publish_settings(PublishSettings {
        slots: 1,
        lock_timeout: Duration::from_millis(200),
        ..PublishSettings::default()
    });
    let mut holder = pool.begin().await.expect("holder");
    sqlx::query("SELECT pg_advisory_xact_lock(hashtext($1), 0)")
        .bind(PUBLISH_SLOT_LOCK)
        .execute(&mut *holder)
        .await
        .expect("take the only slot");

    let mut build = staged(&space, wiki, "queued", 5).await;
    let publish = tokio::spawn(async move {
        let outcome = build.publish(&WikiRecord::default()).await;
        outcome.map(|counts| counts.nodes)
    });
    tokio::time::sleep(Duration::from_millis(1500)).await;
    assert!(!publish.is_finished(), "the publish waits for the slot");
    assert!(live_nodes(&pool, wiki).await.is_empty());
    holder.commit().await.expect("free the slot");
    let published = tokio::time::timeout(Duration::from_mins(1), publish)
        .await
        .expect("the publish ran once the slot freed")
        .expect("task");
    assert_eq!(published.ok(), Some(5));
}

/// The live tables are `ANALYZE`d after the commit, not inside the
/// transaction: a table another session holds does not hold up or fail the
/// publish, its `ANALYZE` is skipped and reported.
#[tokio::test(flavor = "multi_thread")]
async fn analyze_runs_after_the_commit_and_yields_to_a_lock() {
    let Some(pool) = common::fresh_database("control_analyze").await else {
        return;
    };
    let wiki = "acme--analyze--main";
    let space = BuildSpace::new(pool.clone(), "publisher").with_publish_settings(PublishSettings {
        lock_timeout: Duration::from_secs(2),
        analyze_lock_timeout: Duration::from_millis(200),
        ..PublishSettings::default()
    });
    // The lock ANALYZE (and autovacuum) take; inserts and deletes do not
    // conflict with it.
    let mut holder = pool.begin().await.expect("holder");
    sqlx::raw_sql("LOCK TABLE wiki_nodes IN SHARE UPDATE EXCLUSIVE MODE")
        .execute(&mut *holder)
        .await
        .expect("hold wiki_nodes");
    let counts = staged(&space, wiki, "one", 4)
        .await
        .publish(&WikiRecord::default())
        .await
        .expect("the publish does not wait for ANALYZE's lock");
    assert_eq!(counts.nodes, 4);
    assert!(
        !counts.statistics_refreshed,
        "the ANALYZE of wiki_nodes was skipped"
    );
    assert_eq!(live_nodes(&pool, wiki).await.len(), 4, "committed");
    holder.rollback().await.expect("release");

    let counts = staged(&space, wiki, "two", 5)
        .await
        .publish(&WikiRecord::default())
        .await
        .expect("publish");
    assert!(counts.statistics_refreshed);
}

/// Two publishes of one wiki at once: both succeed, one after the other,
/// and the live index is one of the two whole versions.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn concurrent_publishes_of_one_wiki_serialise() {
    let Some(pool) = common::fresh_database("control_same_wiki").await else {
        return;
    };
    let wiki = "acme--same--main";
    let space = BuildSpace::new(pool.clone(), "publisher");
    let mut first = staged(&space, wiki, "alpha", 300).await;
    let mut second = staged(&space, wiki, "beta", 500).await;
    let record = WikiRecord::default();
    let (a, b) = tokio::join!(first.publish(&record), second.publish(&record));
    assert_eq!(a.expect("first").nodes, 300);
    assert_eq!(b.expect("second").nodes, 500);
    let live = live_nodes(&pool, wiki).await;
    let whole_alpha = live.len() == 300 && live.iter().all(|id| id.starts_with("alpha::"));
    let whole_beta = live.len() == 500 && live.iter().all(|id| id.starts_with("beta::"));
    assert!(whole_alpha || whole_beta, "{} live nodes", live.len());
    let (docs, meta): (i64, i64) = sqlx::query_as(
        "SELECT (SELECT count(*) FROM wiki_bm25_docs WHERE wiki_id = $1 AND branch = 'bm25'), \
                (SELECT count(*) FROM wiki_bm25_meta WHERE wiki_id = $1)",
    )
    .bind(wiki)
    .fetch_one(&pool)
    .await
    .expect("statistics");
    assert_eq!(docs, i64::try_from(live.len()).expect("small"));
    assert_eq!(meta, 2);
}

/// Two publishes of different wikis at once, with one slot: both land.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn concurrent_publishes_of_two_wikis_both_land() {
    let Some(pool) = common::fresh_database("control_two_wikis").await else {
        return;
    };
    let space = BuildSpace::new(pool.clone(), "publisher").with_publish_settings(PublishSettings {
        slots: 1,
        ..PublishSettings::default()
    });
    let mut first = staged(&space, "acme--one--main", "one", 200).await;
    let mut second = staged(&space, "acme--two--main", "two", 300).await;
    let record = WikiRecord::default();
    let (a, b) = tokio::join!(first.publish(&record), second.publish(&record));
    assert_eq!(a.expect("first").nodes, 200);
    assert_eq!(b.expect("second").nodes, 300);
    assert_eq!(live_nodes(&pool, "acme--one--main").await.len(), 200);
    assert_eq!(live_nodes(&pool, "acme--two--main").await.len(), 300);
    let builds: i64 = sqlx::query_scalar("SELECT count(*) FROM deepwiki_build.builds")
        .fetch_one(&pool)
        .await
        .expect("builds");
    assert_eq!(builds, 0);
}
