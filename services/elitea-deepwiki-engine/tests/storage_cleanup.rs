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
