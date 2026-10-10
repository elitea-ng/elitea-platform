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
//! advisory locks), repeating until none is left, then removes the project's
//! unlocked builds. It is idempotent and safe to repeat after a failure.
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
use crate::storage::{ProjectScope, Result, WikiKey};
use sqlx::postgres::PgPool;
use sqlx::{Connection, Row};
use std::collections::HashSet;

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
}

/// What [`delete_project`] removed.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ProjectDeletion {
    /// The wiki ids that were deleted, in deletion order.
    pub wikis: Vec<String>,
    pub rows: LiveRows,
    /// Builds in progress that were removed (those not locked by a publish).
    pub builds: u64,
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

/// The rounds [`delete_project`] makes at most, so a project whose
/// generations keep publishing cannot hold it for ever.
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

/// Delete every wiki of one project, and the project's unlocked builds. See
/// the module comment.
///
/// # Errors
///
/// [`crate::storage::StorageError::Database`]. The wikis deleted before the
/// failure stay deleted; call it again to finish.
pub async fn delete_project(
    pool: &PgPool,
    project: ProjectScope,
    settings: &PublishSettings,
) -> Result<ProjectDeletion> {
    let mut outcome = ProjectDeletion::default();
    for _ in 0..PROJECT_ROUNDS {
        let batch: Vec<String> = sqlx::query_scalar(
            "SELECT wiki_id FROM wikis WHERE project_id = $1 ORDER BY wiki_id LIMIT $2",
        )
        .bind(project.id())
        .bind(PROJECT_ROUND)
        .fetch_all(pool)
        .await?;
        if batch.is_empty() {
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
    // Staged rows of generations that never published. A build a publish
    // holds locked is skipped (its own sweep removes it).
    outcome.builds = sqlx::query(
        "DELETE FROM deepwiki_build.builds WHERE build_id IN ( \
             SELECT build_id FROM deepwiki_build.builds WHERE project_id = $1 \
             FOR UPDATE SKIP LOCKED)",
    )
    .bind(project.id())
    .execute(pool)
    .await?
    .rows_affected();
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
