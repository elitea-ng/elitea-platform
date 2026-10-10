//! Removing the dead `'bm25'` statistics branch, in the background
//! (ADR-0031 phase D0).
//!
//! The standalone `'bm25'` branch of the `wiki_bm25_*` tables is no longer
//! written or read; only `'fts'` is. Existing wikis still carry millions of
//! `'bm25'` postings. A migration cannot remove them: its `DELETE` would be
//! one statement over every row, holding locks and WAL for as long as the
//! largest index takes, in a Job that blocks the rollout. So migration 0007
//! is a no-op, and this module does the work after the engine is up, in
//! bounded statements.
//!
//! # The pass
//!
//! [`run_pass`] deletes `'bm25'` rows from the four tables, postings first
//! (the large one), [`BATCH_ROWS`] rows per statement with a short pause
//! between statements, and stops when no `'bm25'` row remains. A statement
//! skips rows another transaction holds locked (`FOR UPDATE SKIP LOCKED`), so
//! it never queues behind a publish. A pass is idempotent: a crash loses
//! nothing and the next pass resumes.
//!
//! # The schedule
//!
//! [`run`] makes a pass when the engine starts, then one every
//! [`PASS_INTERVAL`], until [`QUIET_PASSES`] passes IN A ROW found nothing,
//! then stops. A replica of the previous release that is still running
//! during a rolling deploy keeps writing `'bm25'` rows; they are removed by
//! the pass after it is gone, and the two quiet passes in a row are what
//! tell this task the old replicas are gone. A replica started later does
//! the same, so the cleanup runs on every start and is a few cheap scans
//! once the rows are gone.
//!
//! The delete finds its rows by a scan (the tables' indexes lead with
//! `wiki_id`, not `branch`); with few rows left, a pass is a few sequential
//! scans, which is why the task stops rather than polls for ever.

use crate::storage::Result;
use sqlx::postgres::PgPool;
use std::time::Duration;

/// The statistics branch that is no longer written or read.
pub const DEAD_BRANCH: &str = "bm25";

/// Rows deleted per statement.
pub const BATCH_ROWS: i64 = 10_000;

/// The pause between two statements of a pass, so a large backlog does not
/// starve the engine's own queries or the replication stream.
pub const BATCH_PAUSE: Duration = Duration::from_millis(100);

/// The time between two passes.
pub const PASS_INTERVAL: Duration = Duration::from_hours(1);

/// The passes in a row that must find nothing before the task stops.
pub const QUIET_PASSES: u32 = 2;

/// Log a progress line every this many statements of one table.
const LOG_EVERY_BATCHES: u64 = 100;

/// The four tables, postings (the large one) first. A table name from this
/// list is the only thing formatted into a statement.
const TABLES: [&str; 4] = [
    "wiki_bm25_postings",
    "wiki_bm25_terms",
    "wiki_bm25_docs",
    "wiki_bm25_meta",
];

/// How a cleanup paces itself.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Pacing {
    /// Rows deleted per statement.
    pub batch_rows: i64,
    /// The pause between statements.
    pub batch_pause: Duration,
    /// The time between passes.
    pub pass_interval: Duration,
}

impl Default for Pacing {
    fn default() -> Self {
        Self {
            batch_rows: BATCH_ROWS,
            batch_pause: BATCH_PAUSE,
            pass_interval: PASS_INTERVAL,
        }
    }
}

/// Delete at most `limit` `'bm25'` rows of `table`; the rows removed.
async fn delete_batch(pool: &PgPool, table: &str, limit: i64) -> Result<u64> {
    let statement = format!(
        "DELETE FROM {table} WHERE ctid IN ( \
             SELECT ctid FROM {table} WHERE branch = $1 LIMIT $2 FOR UPDATE SKIP LOCKED)"
    );
    Ok(sqlx::query(&statement)
        .bind(DEAD_BRANCH)
        .bind(limit.max(1))
        .execute(pool)
        .await?
        .rows_affected())
}

/// One pass: delete every `'bm25'` row, in batches. Returns the rows
/// removed (0 when there was nothing to do).
///
/// # Errors
///
/// [`crate::storage::StorageError::Database`]; the rows deleted before the
/// failure stay deleted.
pub async fn run_pass(pool: &PgPool, pacing: &Pacing) -> Result<u64> {
    let mut total = 0;
    for table in TABLES {
        let mut table_total = 0_u64;
        let mut batches = 0_u64;
        loop {
            let removed = delete_batch(pool, table, pacing.batch_rows).await?;
            if removed == 0 {
                break;
            }
            table_total += removed;
            batches += 1;
            if batches.is_multiple_of(LOG_EVERY_BATCHES) {
                tracing::info!(
                    table,
                    removed = table_total,
                    "removing the dead bm25 branch"
                );
            }
            if !pacing.batch_pause.is_zero() {
                tokio::time::sleep(pacing.batch_pause).await;
            }
        }
        if table_total > 0 {
            tracing::info!(
                table,
                removed = table_total,
                "removed the dead bm25 branch rows"
            );
        }
        total += table_total;
    }
    Ok(total)
}

/// The scheduled cleanup, to be spawned: a pass now, then one per
/// `pacing.pass_interval`, until [`QUIET_PASSES`] passes in a row found
/// nothing. A failed pass is logged and counts as not quiet; it is tried
/// again next interval. Returns the passes made.
pub async fn run(pool: PgPool, pacing: Pacing) -> u32 {
    let mut passes = 0;
    let mut quiet = 0;
    loop {
        passes += 1;
        match run_pass(&pool, &pacing).await {
            Ok(0) => {
                quiet += 1;
                tracing::info!(pass = passes, quiet, "no dead bm25 branch rows");
            }
            Ok(removed) => {
                quiet = 0;
                tracing::info!(pass = passes, removed, "removed dead bm25 branch rows");
            }
            Err(error) => {
                quiet = 0;
                tracing::warn!(%error, pass = passes, "removing the dead bm25 branch failed; trying again next pass");
            }
        }
        if quiet >= QUIET_PASSES {
            tracing::info!(passes, "the dead bm25 branch is gone; the cleanup stops");
            return passes;
        }
        tokio::time::sleep(pacing.pass_interval).await;
    }
}
