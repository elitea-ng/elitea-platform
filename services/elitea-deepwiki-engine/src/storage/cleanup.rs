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
//! [`PASS_INTERVAL`], until [`QUIET_PASSES`] passes IN A ROW were quiet (removed
//! nothing and found no row left), then stops. A replica of the previous release that is still running
//! during a rolling deploy keeps writing `'bm25'` rows; they are removed by
//! the pass after it is gone, and the two quiet passes in a row are what
//! tell this task the old replicas are gone. A replica started later does
//! the same, so the cleanup runs on every start and is a few cheap scans
//! once the rows are gone.
//!
//! The tables' primary keys lead with `(project_id, wiki_id, branch)`, so
//! the pass works wiki by wiki: it lists the wikis that have `'bm25'`
//! statistics ONCE (`wiki_bm25_meta`, a table with one row per wiki and
//! branch), then deletes each wiki's rows through the primary-key index in
//! bounded batches. No batch scans the whole table. A table with rows but no
//! `meta` row (a crashed publish, a half-finished earlier pass) is found by a
//! `LIMIT 1` probe once the listed wikis are done.
//!
//! # Quiet means none exist
//!
//! `SKIP LOCKED` leaves a row another transaction holds, and a pass that
//! skipped rows removed nothing from them. That is NOT quiet: a pass is
//! quiet only when it removed nothing AND an `EXISTS` (which ignores row
//! locks) finds no `'bm25'` row left in any of the four tables.

use crate::storage::Result;
use sqlx::postgres::PgPool;
use std::collections::HashSet;
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

/// Delete at most `limit` `'bm25'` rows of `table` that belong to one wiki.
/// The `WHERE` names the primary key's leading columns, so the statement
/// walks the index range of that wiki and nothing else.
async fn delete_batch(
    pool: &PgPool,
    table: &str,
    wiki: &(i32, String),
    limit: i64,
) -> Result<u64> {
    let statement = format!(
        "DELETE FROM {table} WHERE ctid IN ( \
             SELECT ctid FROM {table} \
             WHERE project_id = $1 AND wiki_id = $2 AND branch = $3 \
             LIMIT $4 FOR UPDATE SKIP LOCKED)"
    );
    Ok(sqlx::query(&statement)
        .bind(wiki.0)
        .bind(&wiki.1)
        .bind(DEAD_BRANCH)
        .bind(limit.max(1))
        .execute(pool)
        .await?
        .rows_affected())
}

/// The wikis that have `'bm25'` statistics, from `wiki_bm25_meta`: one small
/// listing per pass.
async fn listed_wikis(pool: &PgPool) -> Result<Vec<(i32, String)>> {
    Ok(sqlx::query_as(
        "SELECT DISTINCT project_id, wiki_id FROM wiki_bm25_meta \
         WHERE branch = $1 ORDER BY project_id, wiki_id",
    )
    .bind(DEAD_BRANCH)
    .fetch_all(pool)
    .await?)
}

/// One wiki with `'bm25'` rows in a table other than `meta` (the probe for
/// strays), skipping the wikis this pass already handled.
async fn stray_wiki(pool: &PgPool, handled: &HashSet<(i32, String)>) -> Result<Option<(i32, String)>> {
    for table in &TABLES[..3] {
        let statement = format!(
            "SELECT DISTINCT project_id, wiki_id FROM {table} WHERE branch = $1 LIMIT {}",
            handled.len() + 1
        );
        let found: Vec<(i32, String)> = sqlx::query_as(&statement)
            .bind(DEAD_BRANCH)
            .fetch_all(pool)
            .await?;
        if let Some(wiki) = found.into_iter().find(|wiki| !handled.contains(wiki)) {
            return Ok(Some(wiki));
        }
    }
    Ok(None)
}

/// Whether any `'bm25'` row exists in any of the four tables. A plain
/// `EXISTS`: it reads the committed rows and ignores row locks, so rows a
/// publish holds locked still count.
///
/// # Errors
///
/// [`crate::storage::StorageError::Database`].
pub async fn dead_rows_exist(pool: &PgPool) -> Result<bool> {
    Ok(sqlx::query_scalar(
        "SELECT EXISTS (SELECT 1 FROM wiki_bm25_meta WHERE branch = $1) \
             OR EXISTS (SELECT 1 FROM wiki_bm25_docs WHERE branch = $1) \
             OR EXISTS (SELECT 1 FROM wiki_bm25_terms WHERE branch = $1) \
             OR EXISTS (SELECT 1 FROM wiki_bm25_postings WHERE branch = $1)",
    )
    .bind(DEAD_BRANCH)
    .fetch_one(pool)
    .await?)
}

/// What one pass did.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct PassReport {
    /// Rows removed.
    pub removed: u64,
    /// `'bm25'` rows still exist after the pass (they were locked, or a
    /// replica wrote them meanwhile). A pass with `remaining` is not quiet.
    pub remaining: bool,
}

impl PassReport {
    /// A pass is quiet when it removed nothing and nothing is left.
    #[must_use]
    pub fn quiet(&self) -> bool {
        self.removed == 0 && !self.remaining
    }
}

/// One wiki's rows, table after table (postings first), in bounded batches.
async fn clean_wiki(pool: &PgPool, wiki: &(i32, String), pacing: &Pacing) -> Result<u64> {
    let mut total = 0;
    for table in TABLES {
        let mut table_total = 0_u64;
        let mut batches = 0_u64;
        loop {
            let removed = delete_batch(pool, table, wiki, pacing.batch_rows).await?;
            if removed == 0 {
                break;
            }
            table_total += removed;
            batches += 1;
            if batches.is_multiple_of(LOG_EVERY_BATCHES) {
                tracing::info!(
                    table,
                    project_id = wiki.0,
                    wiki_id = %wiki.1,
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
                project_id = wiki.0,
                wiki_id = %wiki.1,
                removed = table_total,
                "removed the dead bm25 branch rows"
            );
        }
        total += table_total;
    }
    Ok(total)
}

/// One pass: delete every `'bm25'` row, wiki by wiki, in batches, and say
/// whether any remain.
///
/// # Errors
///
/// [`crate::storage::StorageError::Database`]; the rows deleted before the
/// failure stay deleted.
pub async fn run_pass_report(pool: &PgPool, pacing: &Pacing) -> Result<PassReport> {
    let mut removed = 0;
    let mut handled: HashSet<(i32, String)> = HashSet::new();
    for wiki in listed_wikis(pool).await? {
        removed += clean_wiki(pool, &wiki, pacing).await?;
        handled.insert(wiki);
    }
    // Rows with no `meta` row. Each wiki is handled once per pass, so rows
    // that stay locked cannot make this loop spin.
    let remaining = loop {
        if !dead_rows_exist(pool).await? {
            break false;
        }
        let Some(wiki) = stray_wiki(pool, &handled).await? else {
            // Rows exist and every wiki that has them was handled: they were
            // locked (or written meanwhile). The pass is not quiet.
            break true;
        };
        removed += clean_wiki(pool, &wiki, pacing).await?;
        handled.insert(wiki);
    };
    Ok(PassReport { removed, remaining })
}

/// One pass, as the rows removed (0 when there was nothing to do, or when
/// everything was locked: see [`run_pass_report`] for the difference).
///
/// # Errors
///
/// As [`run_pass_report`].
pub async fn run_pass(pool: &PgPool, pacing: &Pacing) -> Result<u64> {
    Ok(run_pass_report(pool, pacing).await?.removed)
}

/// The scheduled cleanup, to be spawned: a pass now, then one per
/// `pacing.pass_interval`, until [`QUIET_PASSES`] passes in a row were quiet
/// (see [`PassReport::quiet`]). A failed pass is logged and counts as not
/// quiet; it is tried again next interval. Returns the passes made.
pub async fn run(pool: PgPool, pacing: Pacing) -> u32 {
    let mut passes = 0;
    let mut quiet = 0;
    loop {
        passes += 1;
        match run_pass_report(&pool, &pacing).await {
            Ok(report) if report.quiet() => {
                quiet += 1;
                tracing::info!(pass = passes, quiet, "no dead bm25 branch rows");
            }
            Ok(report) => {
                quiet = 0;
                tracing::info!(
                    pass = passes,
                    removed = report.removed,
                    remaining = report.remaining,
                    "removed dead bm25 branch rows"
                );
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
