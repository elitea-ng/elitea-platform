//! Deleting an index: one wiki, every wiki of a project, and the listing of
//! wikis whose project is gone (ADR-0031 phase D0, issue #1243).
//!
//! Before this module nothing deleted a wiki's rows. The host's
//! `delete_wiki` removed the artifact objects, and the index stayed in this
//! database for ever: node text, symbols, edges, embeddings and the
//! `wiki_bm25_*` statistics.
//!
//! # One wiki
//!
//! [`delete_wiki`] removes the `wikis` row and every row of `wiki_nodes`,
//! `wiki_edges`, `wiki_node_embeddings` and `wiki_bm25_*` for one
//! `(project_id, wiki_id)`, in ONE transaction. The transaction first takes
//! the publish's per-wiki advisory lock ([`PUBLISH_WIKI_LOCK`], the same
//! object [`publish_wiki_lock_object`] names), so it queues behind a publish
//! of that wiki and a publish queues behind it. Whichever commits last wins
//! whole: the wiki is either the published index or absent, never a mix. The
//! `wiki_bm25_*` tables have no foreign key (migration 0001), so they are
//! deleted explicitly, as the publish does; the other three would also go
//! with the `wikis` row (`ON DELETE CASCADE`), and are deleted explicitly
//! first so the result can report what was removed.
//!
//! A build that is still in progress is left alone: its row is locked by a
//! publish, and deleting it here would wait on that publish while the
//! publish waits on this deletion's advisory lock. A generation that was
//! already running when the wiki was deleted can still publish and bring the
//! wiki back; the host's `delete_wiki` removes the artifacts first, and the
//! stop of a running generation is the caller's.
//!
//! # One project
//!
//! [`delete_project`] deletes the project's wikis one transaction at a time
//! (a project can hold thousands, and one transaction would hold as many
//! advisory locks). It lists a batch, deletes it, and lists again until no
//! wiki remains, so a wiki published while it ran is deleted too. It makes
//! at most [`ProjectLimits::max_rounds`] rounds; when the project still holds
//! wikis after that (something keeps publishing into it) it returns an error
//! that names how many remain, and the caller must treat the deletion as NOT
//! done. It is idempotent and safe to repeat after a failure.
//!
//! ## Builds in progress are left to their run
//!
//! A build whose heartbeat is LIVE (younger than
//! [`ProjectLimits::stale_after`], the sweep's threshold) belongs to a
//! generation that is still running: [`delete_project`] does not delete its
//! staged rows, because deleting them under the run only turns its publish
//! into a confusing "build no longer exists" failure. Only builds with a
//! stale heartbeat (a replica that went away; the periodic sweep would remove
//! them anyway) are deleted. The count of builds left is reported in
//! [`ProjectDeletion::live_builds`].
//!
//! What the run's publish does afterwards: a publish is an upsert of the
//! `wikis` row and a replacement of the wiki's rows, and the engine cannot
//! know the project was deleted (the product database is not its own). So a
//! generation that was running when the project was deleted and then
//! publishes RECREATES that wiki's index, for a project that no longer
//! exists. This is the existing publish behaviour and is accepted here;
//! the caller stops the project's generations before it deletes the project,
//! and the `orphans` listing finds and removes any index that comes back.
//!
//! # Orphans
//!
//! The index lives in the `deepwiki` database and the projects in the
//! product database, so the engine cannot tell by itself which projects
//! still exist. [`orphans`] takes the set of existing project ids from the
//! caller and lists the projects in this database that are not in it. It
//! reads only; [`delete_project`] is the separate, explicit step.

use crate::storage::build::{
    LiveRows, PUBLISH_WIKI_LOCK, PublishSettings, apply_settings, delete_live,
    publish_wiki_lock_object,
};
use crate::storage::{ProjectScope, Result, StorageError, WikiKey};
use sqlx::postgres::PgPool;
use sqlx::{Connection, Row};
use std::collections::HashSet;
use std::time::Duration;

/// What [`delete_wiki`] removed.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct WikiDeletion {
    /// There was a `wikis` row. `false` for a wiki that is not indexed (never
    /// published, or already deleted), which is not an error.
    pub existed: bool,
    pub rows: LiveRows,
}

impl WikiDeletion {
    /// Rows removed from every table, the `wikis` row included.
    #[must_use]
    pub fn total_rows(&self) -> u64 {
        self.rows.nodes
            + self.rows.edges
            + self.rows.embeddings
            + self.rows.statistics
            + u64::from(self.existed)
    }

    /// Whether anything was removed. Rows can exist with no `wikis` row (the
    /// `wiki_bm25_*` tables have no foreign key, so a failed delete or a
    /// crashed publish can leave them), and removing them is a deletion
    /// even though [`Self::existed`] is `false`.
    #[must_use]
    pub fn deleted(&self) -> bool {
        self.total_rows() > 0
    }
}

/// What [`delete_project`] removed.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ProjectDeletion {
    /// The wiki ids that were deleted, in deletion order.
    pub wikis: Vec<String>,
    pub rows: LiveRows,
    /// Stale builds that were removed: staged rows of generations whose
    /// heartbeat stopped.
    pub builds: u64,
    /// Builds left alone because their heartbeat is live: a generation is
    /// still running and its publish will find the wiki deleted (and
    /// recreate it; see the module comment).
    pub live_builds: u64,
}

/// How [`delete_project`] pages and when it gives up.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ProjectLimits {
    /// A build whose heartbeat is older than this is stale and is deleted
    /// (the build sweep's threshold, `ELITEA_DEEPWIKI_BUILD_STALE_AFTER`).
    pub stale_after: Duration,
    /// Wikis listed and deleted per round.
    pub batch: i64,
    /// The rounds made at most, so a project whose generations keep
    /// publishing cannot hold the deletion for ever.
    pub max_rounds: usize,
}

impl ProjectLimits {
    /// The defaults for a deployment whose sweep threshold is `stale_after`.
    #[must_use]
    pub fn new(stale_after: Duration) -> Self {
        Self {
            stale_after,
            batch: PROJECT_ROUND,
            max_rounds: PROJECT_ROUNDS,
        }
    }
}

/// Delete one wiki's index. See the module comment.
///
/// `settings` supplies the statement and lock timeouts (the publish's: both
/// move a whole index). The wait for a publish of the same wiki has none.
///
/// # Errors
///
/// [`crate::storage::StorageError::Database`], also for a statement or lock
/// timeout; nothing is deleted then.
pub async fn delete_wiki(
    pool: &PgPool,
    key: &WikiKey,
    settings: &PublishSettings,
) -> Result<WikiDeletion> {
    let mut connection = pool.acquire().await?;
    let mut tx = connection.begin().await?;
    // The queue: no timeout while a publish of this wiki finishes.
    apply_settings(
        &mut tx,
        &[
            ("lock_timeout", "0".to_owned()),
            ("statement_timeout", "0".to_owned()),
        ],
    )
    .await?;
    sqlx::query("SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))")
        .bind(PUBLISH_WIKI_LOCK)
        .bind(publish_wiki_lock_object(key))
        .execute(&mut *tx)
        .await?;
    apply_settings(
        &mut tx,
        &[
            (
                "statement_timeout",
                format!("{}ms", settings.statement_timeout.as_millis().max(1)),
            ),
            (
                "lock_timeout",
                format!("{}ms", settings.lock_timeout.as_millis().max(1)),
            ),
        ],
    )
    .await?;
    let rows = delete_live(&mut tx, key).await?;
    let existed = sqlx::query("DELETE FROM wikis WHERE project_id = $1 AND wiki_id = $2")
        .bind(key.project_id())
        .bind(key.wiki_id())
        .execute(&mut *tx)
        .await?
        .rows_affected()
        > 0;
    tx.commit().await?;
    Ok(WikiDeletion { existed, rows })
}

/// Wikis deleted per listing round of [`delete_project`].
const PROJECT_ROUND: i64 = 100;

/// The rounds [`delete_project`] makes at most by default.
const PROJECT_ROUNDS: usize = 1_000;

/// The wiki ids a project's index holds, in id order.
///
/// # Errors
///
/// [`crate::storage::StorageError::Database`].
pub async fn project_wikis(pool: &PgPool, project: ProjectScope) -> Result<Vec<String>> {
    Ok(
        sqlx::query_scalar("SELECT wiki_id FROM wikis WHERE project_id = $1 ORDER BY wiki_id")
            .bind(project.id())
            .fetch_all(pool)
            .await?,
    )
}

/// Delete every wiki of one project, and the project's stale builds. See the
/// module comment.
///
/// # Errors
///
/// [`StorageError::Database`]; or [`StorageError::Delete`] when the project
/// still holds wikis after `limits.max_rounds` rounds (the message names how
/// many). The wikis deleted before the failure stay deleted; call it again
/// to finish.
pub async fn delete_project(
    pool: &PgPool,
    project: ProjectScope,
    settings: &PublishSettings,
    limits: &ProjectLimits,
) -> Result<ProjectDeletion> {
    let mut outcome = ProjectDeletion::default();
    let mut finished = false;
    for _ in 0..limits.max_rounds {
        let batch: Vec<String> = sqlx::query_scalar(
            "SELECT wiki_id FROM wikis WHERE project_id = $1 ORDER BY wiki_id LIMIT $2",
        )
        .bind(project.id())
        .bind(limits.batch.max(1))
        .fetch_all(pool)
        .await?;
        if batch.is_empty() {
            finished = true;
            break;
        }
        for wiki in batch {
            let key = WikiKey::new(project, wiki.clone());
            let deleted = delete_wiki(pool, &key, settings).await?;
            outcome.rows.nodes += deleted.rows.nodes;
            outcome.rows.edges += deleted.rows.edges;
            outcome.rows.embeddings += deleted.rows.embeddings;
            outcome.rows.statistics += deleted.rows.statistics;
            if deleted.existed {
                outcome.wikis.push(wiki);
            }
        }
    }
    if !finished {
        // The cap was hit with the last batch deleted: look once more.
        let remaining: i64 = sqlx::query_scalar("SELECT count(*) FROM wikis WHERE project_id = $1")
            .bind(project.id())
            .fetch_one(pool)
            .await?;
        if remaining > 0 {
            return Err(StorageError::Delete(format!(
                "project {} still holds {remaining} wiki(s) after {} round(s) of {} (deleted {} wiki(s) meanwhile); something keeps publishing into it. Stop its generations and run the deletion again",
                project.id(),
                limits.max_rounds,
                limits.batch.max(1),
                outcome.wikis.len()
            )));
        }
    }
    // Staged rows of generations that never published, and only those whose
    // heartbeat stopped: a live build belongs to a running generation (see
    // the module comment). A build a publish holds locked is skipped too
    // (its own sweep removes it).
    outcome.builds = sqlx::query(
        "DELETE FROM deepwiki_build.builds WHERE build_id IN ( \
             SELECT build_id FROM deepwiki_build.builds \
             WHERE project_id = $1 \
               AND heartbeat_at < now() - make_interval(secs => $2) \
             FOR UPDATE SKIP LOCKED)",
    )
    .bind(project.id())
    .bind(limits.stale_after.as_secs_f64())
    .execute(pool)
    .await?
    .rows_affected();
    let live: i64 =
        sqlx::query_scalar("SELECT count(*) FROM deepwiki_build.builds WHERE project_id = $1")
            .bind(project.id())
            .fetch_one(pool)
            .await?;
    outcome.live_builds = u64::try_from(live).unwrap_or(0);
    Ok(outcome)
}

/// One project that has indexed wikis.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct IndexedProject {
    pub project_id: i32,
    pub wikis: i64,
    /// The newest `updated_at` among the project's wikis, RFC 3339.
    pub last_updated: String,
}

/// Every project that has wikis in the index, with its wiki count.
///
/// # Errors
///
/// [`crate::storage::StorageError::Database`].
pub async fn indexed_projects(pool: &PgPool) -> Result<Vec<IndexedProject>> {
    let rows = sqlx::query(
        "SELECT project_id, count(*) AS wikis, \
                to_char(max(updated_at) AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"') AS updated \
         FROM wikis GROUP BY project_id ORDER BY project_id",
    )
    .fetch_all(pool)
    .await?;
    let mut projects = Vec::with_capacity(rows.len());
    for row in rows {
        projects.push(IndexedProject {
            project_id: row.try_get("project_id")?,
            wikis: row.try_get("wikis")?,
            last_updated: row.try_get("updated")?,
        });
    }
    Ok(projects)
}

/// The indexed projects that are not in `existing`: their project was
/// deleted and the index was not. Reads only.
///
/// # Errors
///
/// [`crate::storage::StorageError::Database`].
pub async fn orphans<S: std::hash::BuildHasher>(
    pool: &PgPool,
    existing: &HashSet<i32, S>,
) -> Result<Vec<IndexedProject>> {
    Ok(indexed_projects(pool)
        .await?
        .into_iter()
        .filter(|project| !existing.contains(&project.project_id))
        .collect())
}

/// The oldest project listing `orphans --delete` accepts, without
/// `--allow-stale-list`.
pub const MAX_LISTING_AGE: Duration = Duration::from_mins(10);

/// How far in the future a listing time may be (clock skew between the host
/// that took the list and the database).
const MAX_LISTING_SKEW: Duration = Duration::from_mins(1);

/// Check that a project listing taken at `listed_at` (RFC 3339, already
/// validated by the caller) is fresh enough to delete by.
///
/// A project created after the list was taken is not in it, so it looks like
/// an orphan; the older the list, the more such projects there are. A list
/// older than [`MAX_LISTING_AGE`] is refused unless `allow_stale`; one from
/// the future is always refused (the time is wrong).
///
/// # Errors
///
/// [`StorageError::Delete`] for a stale or future listing, a timestamp
/// PostgreSQL cannot read; [`StorageError::Database`].
pub async fn check_listing_fresh(pool: &PgPool, listed_at: &str, allow_stale: bool) -> Result<()> {
    let age: f64 = sqlx::query_scalar("SELECT extract(epoch FROM now() - $1::timestamptz)::float8")
        .bind(listed_at)
        .fetch_one(pool)
        .await
        .map_err(|error| match error {
            sqlx::Error::Database(_) => {
                StorageError::Delete(format!("--listed-at {listed_at:?} is not a valid time"))
            }
            other => StorageError::Database(other),
        })?;
    if age < -MAX_LISTING_SKEW.as_secs_f64() {
        return Err(StorageError::Delete(format!(
            "--listed-at {listed_at} is in the future; the list cannot have been taken then"
        )));
    }
    if age > MAX_LISTING_AGE.as_secs_f64() && !allow_stale {
        return Err(StorageError::Delete(format!(
            "the project list was taken {:.0} minute(s) ago (--listed-at {listed_at}); a project created since is not in it and would look orphaned. Take a fresh list, or pass --allow-stale-list to delete by this one",
            age / 60.0
        )));
    }
    Ok(())
}

/// Why a project must not be treated as an orphan by a list taken at
/// `listed_at`: a wiki of it was created or published, or a build of it
/// started, after that time. `None` when the project has no such activity.
///
/// # Errors
///
/// [`StorageError::Database`].
pub async fn activity_since(
    pool: &PgPool,
    project_id: i32,
    listed_at: &str,
) -> Result<Option<String>> {
    let wiki: Option<String> = sqlx::query_scalar(
        "SELECT wiki_id FROM wikis \
         WHERE project_id = $1 AND (created_at > $2::timestamptz OR updated_at > $2::timestamptz) \
         ORDER BY updated_at DESC LIMIT 1",
    )
    .bind(project_id)
    .bind(listed_at)
    .fetch_optional(pool)
    .await?;
    if let Some(wiki) = wiki {
        return Ok(Some(format!(
            "wiki {wiki} was created or published after {listed_at}"
        )));
    }
    let build: Option<String> = sqlx::query_scalar(
        "SELECT wiki_id FROM deepwiki_build.builds \
         WHERE project_id = $1 AND (started_at > $2::timestamptz OR heartbeat_at > $2::timestamptz) \
         ORDER BY started_at DESC LIMIT 1",
    )
    .bind(project_id)
    .bind(listed_at)
    .fetch_optional(pool)
    .await?;
    Ok(build.map(|wiki| format!("a build of wiki {wiki} started or is running after {listed_at}")))
}

/// What [`sweep_orphans`] is asked to do.
#[derive(Debug, Clone, Copy)]
pub struct SweepOptions<'a> {
    /// When the list of existing projects was taken (RFC 3339, validated).
    /// `None` is a dry run's choice: nothing is skipped by activity.
    pub listed_at: Option<&'a str>,
    /// Delete, rather than only report.
    pub delete: bool,
    /// Accept a list older than [`MAX_LISTING_AGE`].
    pub allow_stale_list: bool,
}

/// What [`sweep_orphans`] did, project by project.
#[derive(Debug, Default)]
pub struct SweepOutcome {
    /// Orphans deleted (`delete`), with what was removed.
    pub deleted: Vec<(IndexedProject, ProjectDeletion)>,
    /// Orphans that would be deleted (a dry run).
    pub would_delete: Vec<IndexedProject>,
    /// Projects left alone because a wiki or build of theirs is newer than
    /// the list, with the reason.
    pub skipped: Vec<(IndexedProject, String)>,
    /// Projects whose deletion failed, with the error. Non-empty means the
    /// sweep did not finish and the command must exit non-zero.
    pub failed: Vec<(IndexedProject, String)>,
}

/// List the orphans of `existing` and, with `options.delete`, delete them.
///
/// A project created after the list was taken is not in `existing` and
/// would look orphaned. The guard is two-fold: with `delete`, a list older
/// than [`MAX_LISTING_AGE`] is refused ([`check_listing_fresh`]); and every
/// project with a wiki or build created or published after `listed_at` is
/// skipped, the check made again just before that project's deletion.
///
/// # Errors
///
/// [`StorageError::Delete`] for a missing, stale or invalid `listed_at` with
/// `delete`; [`StorageError::Database`]. A failed project deletion is in
/// [`SweepOutcome::failed`], not here, so one project does not stop the rest.
pub async fn sweep_orphans<S: std::hash::BuildHasher>(
    pool: &PgPool,
    existing: &HashSet<i32, S>,
    options: &SweepOptions<'_>,
    settings: &PublishSettings,
    limits: &ProjectLimits,
) -> Result<SweepOutcome> {
    if options.delete {
        let Some(listed_at) = options.listed_at else {
            return Err(StorageError::Delete(
                "deleting orphans needs the time the project list was taken (--listed-at)".into(),
            ));
        };
        check_listing_fresh(pool, listed_at, options.allow_stale_list).await?;
    }
    let mut outcome = SweepOutcome::default();
    for project in orphans(pool, existing).await? {
        if let Some(listed_at) = options.listed_at
            && let Some(reason) = activity_since(pool, project.project_id, listed_at).await?
        {
            outcome.skipped.push((project, reason));
            continue;
        }
        if !options.delete {
            outcome.would_delete.push(project);
            continue;
        }
        let Some(scope) = ProjectScope::new(project.project_id) else {
            continue;
        };
        match delete_project(pool, scope, settings, limits).await {
            Ok(done) => outcome.deleted.push((project, done)),
            Err(error) => outcome.failed.push((project, error.to_string())),
        }
    }
    Ok(outcome)
}
