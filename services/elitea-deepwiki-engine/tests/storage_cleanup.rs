//! The background removal of the dead `'bm25'` statistics branch
//! (ADR-0031 phase D0; migration 0007 is a no-op).

mod storage_common;

use elitea_deepwiki_engine::storage::cleanup::{self, Pacing};
use sqlx::postgres::PgPool;
use std::time::Duration;
use storage_common as common;

const TABLES: [&str; 4] = [
    "wiki_bm25_meta",
    "wiki_bm25_docs",
    "wiki_bm25_terms",
    "wiki_bm25_postings",
];

fn quick(batch_rows: i64) -> Pacing {
    Pacing {
        batch_rows,
        batch_pause: Duration::ZERO,
        pass_interval: Duration::from_millis(20),
    }
}

/// `docs` documents of one wiki on `branch`, each with one term and posting.
async fn seed(pool: &PgPool, wiki: &str, branch: &str, docs: i32) {
    sqlx::query(
        "INSERT INTO wiki_bm25_meta (project_id, wiki_id, branch, doc_count, avgdl, k1, b) \
         VALUES (1, $1, $2, $3, 1.0, 1.2, 0.75) ON CONFLICT DO NOTHING",
    )
    .bind(wiki)
    .bind(branch)
    .bind(docs)
    .execute(pool)
    .await
    .expect("meta");
    sqlx::query(
        "INSERT INTO wiki_bm25_docs (project_id, wiki_id, branch, doc_idx, node_id, length) \
         SELECT 1, $1, $2, i, 'n' || i, 1 FROM generate_series(0, $3 - 1) AS i",
    )
    .bind(wiki)
    .bind(branch)
    .bind(docs)
    .execute(pool)
    .await
    .expect("docs");
    sqlx::query(
        "INSERT INTO wiki_bm25_terms (project_id, wiki_id, branch, term, df) \
         SELECT 1, $1, $2, 't' || i, 1 FROM generate_series(0, $3 - 1) AS i",
    )
    .bind(wiki)
    .bind(branch)
    .bind(docs)
    .execute(pool)
    .await
    .expect("terms");
    sqlx::query(
        "INSERT INTO wiki_bm25_postings (project_id, wiki_id, branch, term, doc_idx, tf) \
         SELECT 1, $1, $2, 't' || i, i, 1 FROM generate_series(0, $3 - 1) AS i",
    )
    .bind(wiki)
    .bind(branch)
    .bind(docs)
    .execute(pool)
    .await
    .expect("postings");
}

async fn count(pool: &PgPool, table: &str, branch: &str) -> i64 {
    sqlx::query_scalar(&format!("SELECT count(*) FROM {table} WHERE branch = $1"))
        .bind(branch)
        .fetch_one(pool)
        .await
        .expect("count")
}

#[tokio::test(flavor = "multi_thread")]
async fn a_pass_removes_the_dead_branch_in_batches_and_only_that() {
    let Some(pool) = common::fresh_database("cleanup_pass").await else {
        return;
    };
    seed(&pool, "w-a", "bm25", 25).await;
    seed(&pool, "w-b", "bm25", 7).await;
    seed(&pool, "w-a", "fts", 25).await;

    // Batches of 4 rows: many statements, one pass, the same end state.
    let removed = cleanup::run_pass(&pool, &quick(4)).await.expect("pass");
    // meta 2 + docs 32 + terms 32 + postings 32.
    assert_eq!(removed, 2 + 32 * 3);
    for table in TABLES {
        assert_eq!(count(&pool, table, "bm25").await, 0, "{table}");
    }
    assert_eq!(count(&pool, "wiki_bm25_postings", "fts").await, 25);
    assert_eq!(count(&pool, "wiki_bm25_meta", "fts").await, 1);

    // Idempotent: nothing left, nothing removed.
    assert_eq!(cleanup::run_pass(&pool, &quick(4)).await.expect("again"), 0);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_pass_leaves_rows_another_transaction_holds_and_the_next_pass_takes_them() {
    let Some(pool) = common::fresh_database("cleanup_locked").await else {
        return;
    };
    seed(&pool, "w-a", "bm25", 5).await;
    let mut holder = pool.begin().await.expect("tx");
    sqlx::query("SELECT 1 FROM wiki_bm25_postings WHERE branch = 'bm25' FOR UPDATE")
        .execute(&mut *holder)
        .await
        .expect("lock the postings");
    // Not blocked by the lock: the postings are skipped, the rest go.
    let removed = tokio::time::timeout(
        Duration::from_secs(20),
        cleanup::run_pass(&pool, &quick(100)),
    )
    .await
    .expect("the pass does not wait on a publish")
    .expect("pass");
    assert_eq!(removed, 1 + 5 + 5);
    assert_eq!(count(&pool, "wiki_bm25_postings", "bm25").await, 5);
    holder.rollback().await.expect("release");
    assert_eq!(
        cleanup::run_pass(&pool, &quick(100)).await.expect("pass"),
        5
    );
}

/// The task runs until two passes in a row find nothing, then stops: an
/// old replica's late rows reset the count.
#[tokio::test(flavor = "multi_thread")]
async fn the_task_stops_after_two_quiet_passes_in_a_row() {
    let Some(pool) = common::fresh_database("cleanup_task").await else {
        return;
    };
    seed(&pool, "w-a", "bm25", 3).await;
    let task = tokio::spawn(cleanup::run(pool.clone(), quick(10)));
    // Pass 1 removes the backlog; pass 2 and 3 find nothing.
    let passes = tokio::time::timeout(Duration::from_secs(30), task)
        .await
        .expect("the task stops")
        .expect("join");
    assert_eq!(passes, 3);

    // A late write by an old replica during the quiet window: the count
    // starts over (found by a pass, then two more quiet ones).
    seed(&pool, "w-late", "bm25", 2).await;
    let passes = tokio::time::timeout(
        Duration::from_secs(30),
        cleanup::run(pool.clone(), quick(10)),
    )
    .await
    .expect("stops");
    assert_eq!(passes, 3);
    for table in TABLES {
        assert_eq!(count(&pool, table, "bm25").await, 0, "{table}");
    }
}

/// A mix of `'fts'` and `'bm25'` rows over several wikis and projects: the
/// pass removes the `'bm25'` rows of every wiki and none of the `'fts'` ones.
#[tokio::test(flavor = "multi_thread")]
async fn a_pass_over_a_mix_of_branches_and_wikis_removes_exactly_the_dead_branch() {
    let Some(pool) = common::fresh_database("cleanup_mix").await else {
        return;
    };
    for wiki in ["w-a", "w-b", "w-c"] {
        seed(&pool, wiki, "fts", 9).await;
        seed(&pool, wiki, "bm25", 11).await;
    }
    // A wiki of another project (seed() writes project 1).
    sqlx::query(
        "INSERT INTO wiki_bm25_meta (project_id, wiki_id, branch, doc_count, avgdl, k1, b) \
         VALUES (2, 'w-a', 'bm25', 1, 1.0, 1.2, 0.75), (2, 'w-a', 'fts', 1, 1.0, 1.2, 0.75)",
    )
    .execute(&pool)
    .await
    .expect("project 2");

    let report = cleanup::run_pass_report(&pool, &quick(5))
        .await
        .expect("pass");
    // Per wiki: meta 1 + docs 11 + terms 11 + postings 11; plus project 2's meta.
    assert_eq!(report.removed, 3 * (1 + 33) + 1);
    assert!(!report.remaining);
    assert!(!report.quiet(), "a pass that removed rows is not quiet");
    for table in TABLES {
        assert_eq!(count(&pool, table, "bm25").await, 0, "{table}");
    }
    assert_eq!(count(&pool, "wiki_bm25_postings", "fts").await, 27);
    assert_eq!(count(&pool, "wiki_bm25_docs", "fts").await, 27);
    assert_eq!(count(&pool, "wiki_bm25_meta", "fts").await, 4);

    let again = cleanup::run_pass_report(&pool, &quick(5))
        .await
        .expect("again");
    assert!(again.quiet());
}

/// Rows with no `meta` row (a crashed publish) are found and removed too.
#[tokio::test(flavor = "multi_thread")]
async fn rows_without_a_meta_row_are_removed() {
    let Some(pool) = common::fresh_database("cleanup_stray").await else {
        return;
    };
    seed(&pool, "w-a", "bm25", 4).await;
    sqlx::query("DELETE FROM wiki_bm25_meta")
        .execute(&pool)
        .await
        .expect("drop the meta row");
    let report = cleanup::run_pass_report(&pool, &quick(3))
        .await
        .expect("pass");
    assert_eq!(report.removed, 12);
    assert!(!report.remaining);
    for table in TABLES {
        assert_eq!(count(&pool, table, "bm25").await, 0, "{table}");
    }
}

/// Rows a publish holds locked are skipped, and that is NOT quiet: the pass
/// says rows remain, and the scheduled task does not stop on it.
#[tokio::test(flavor = "multi_thread")]
async fn locked_rows_do_not_count_as_quiet() {
    let Some(pool) = common::fresh_database("cleanup_not_quiet").await else {
        return;
    };
    seed(&pool, "w-a", "bm25", 3).await;
    let mut holder = pool.begin().await.expect("tx");
    sqlx::query("SELECT 1 FROM wiki_bm25_postings WHERE branch = 'bm25' FOR UPDATE")
        .execute(&mut *holder)
        .await
        .expect("lock the postings");

    // The first pass removes the rest and reports the locked postings.
    let first = cleanup::run_pass_report(&pool, &quick(100))
        .await
        .expect("pass");
    assert!(first.remaining && !first.quiet());
    // The second removes nothing (everything left is locked) and is STILL
    // not quiet: a delete that returned 0 is not the same as nothing there.
    let second = cleanup::run_pass_report(&pool, &quick(100))
        .await
        .expect("pass");
    assert_eq!(second.removed, 0);
    assert!(second.remaining && !second.quiet());

    // The task does not give up while the rows exist.
    let task = tokio::spawn(cleanup::run(pool.clone(), quick(100)));
    tokio::time::sleep(Duration::from_millis(400)).await;
    assert!(!task.is_finished(), "the task stopped with rows still there");
    holder.rollback().await.expect("release");
    let passes = tokio::time::timeout(Duration::from_secs(30), task)
        .await
        .expect("the task stops once the rows are gone")
        .expect("join");
    assert!(passes >= 3, "removal pass plus two quiet ones, got {passes}");
    assert_eq!(count(&pool, "wiki_bm25_postings", "bm25").await, 0);
}
