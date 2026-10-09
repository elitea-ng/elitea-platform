//! Per-source ingestion state (`migrations/0002_sources.sql`): the status
//! `sources_status.json` held, the file hashes the checkpoint held, and the
//! lease that keeps two ingestions of one graph apart.

use super::{GraphKey, Result, write_graph};
use crate::graph::Graph;
use elitea_content_source::Acl;
use serde_json::{Map, Value, json};
use sqlx::Row;
use sqlx::pool::PoolConnection;
use sqlx::postgres::{PgPool, Postgres};
use sqlx::types::Json;
use std::collections::BTreeMap;

/// What one source's status row says.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct SourceStatus {
    /// The status key: the source toolkit's id, as text.
    pub toolkit_id: String,
    pub toolkit_name: String,
    pub toolkit_type: String,
    pub branch: Option<String>,
}

/// The counts a finished run records.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct RunCounts {
    pub entities: i64,
    pub relations: i64,
    pub documents: i64,
}

/// `start_ingestion`: the source is `in_progress` from now; its previous
/// counts stay until the run finishes.
///
/// # Errors
///
/// [`StoreError::Database`](super::StoreError::Database).
pub async fn start(pool: &PgPool, key: GraphKey, source: &SourceStatus) -> Result<()> {
    sqlx::query(
        "INSERT INTO inventory_graph.sources
             (project_id, application_id, toolkit_id, toolkit_name, toolkit_type, status,
              started_at, last_updated, documents_processed, error_message, progress_message, branch)
         VALUES ($1, $2, $3, $4, $5, 'in_progress', clock_timestamp(), clock_timestamp(), 0, NULL,
                 'Starting ingestion...', $6)
         ON CONFLICT (project_id, application_id, toolkit_id) DO UPDATE SET
             toolkit_name = EXCLUDED.toolkit_name,
             toolkit_type = EXCLUDED.toolkit_type,
             status = 'in_progress',
             started_at = EXCLUDED.started_at,
             last_updated = EXCLUDED.last_updated,
             documents_processed = 0,
             error_message = NULL,
             progress_message = EXCLUDED.progress_message,
             branch = EXCLUDED.branch",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .bind(&source.toolkit_id)
    .bind(&source.toolkit_name)
    .bind(&source.toolkit_type)
    .bind(&source.branch)
    .execute(pool)
    .await?;
    Ok(())
}

/// The progress line a running source shows.
///
/// # Errors
///
/// [`StoreError::Database`](super::StoreError::Database).
pub async fn progress(pool: &PgPool, key: GraphKey, toolkit_id: &str, message: &str) -> Result<()> {
    sqlx::query(
        "UPDATE inventory_graph.sources
            SET progress_message = $4, last_updated = clock_timestamp()
          WHERE project_id = $1 AND application_id = $2 AND toolkit_id = $3",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .bind(toolkit_id)
    .bind(message)
    .execute(pool)
    .await?;
    Ok(())
}

/// `fail_ingestion`: the run ended in `error`, saying why.
///
/// # Errors
///
/// [`StoreError::Database`](super::StoreError::Database).
pub async fn fail(pool: &PgPool, key: GraphKey, toolkit_id: &str, error: &str) -> Result<()> {
    sqlx::query(
        "UPDATE inventory_graph.sources
            SET status = 'error', error_message = $4, progress_message = NULL,
                last_updated = clock_timestamp()
          WHERE project_id = $1 AND application_id = $2 AND toolkit_id = $3",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .bind(toolkit_id)
    .bind(error)
    .execute(pool)
    .await?;
    Ok(())
}

/// What the store keeps of one document of a source (ADR-0028): its
/// version, media type and readers.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DocumentState {
    pub version: String,
    pub mime: String,
    pub acl: Acl,
}

/// The version of every document the source's last completed run read,
/// by key.
///
/// # Errors
///
/// [`StoreError::Database`](super::StoreError::Database).
pub async fn document_versions(
    pool: &PgPool,
    key: GraphKey,
    source_name: &str,
) -> Result<BTreeMap<String, String>> {
    let rows = sqlx::query(
        "SELECT document_key, version FROM inventory_graph.documents
          WHERE project_id = $1 AND application_id = $2 AND source_name = $3",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .bind(source_name)
    .fetch_all(pool)
    .await?;
    rows.into_iter()
        .map(|row| Ok((row.try_get("document_key")?, row.try_get("version")?)))
        .collect()
}

/// Every restricted document of a graph: `(source name, key, acl)`. A read
/// filters what a caller sees by these; a graph with none needs no filter.
///
/// # Errors
///
/// [`StoreError::Database`](super::StoreError::Database).
pub async fn restricted_documents(
    pool: &PgPool,
    key: GraphKey,
) -> Result<Vec<(String, String, Acl)>> {
    let rows = sqlx::query(
        "SELECT source_name, document_key, acl FROM inventory_graph.documents
          WHERE project_id = $1 AND application_id = $2 AND acl ->> 'scope' = 'restricted'",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .fetch_all(pool)
    .await?;
    rows.into_iter()
        .map(|row| {
            let acl: Json<Acl> = row.try_get("acl")?;
            Ok((
                row.try_get("source_name")?,
                row.try_get("document_key")?,
                acl.0,
            ))
        })
        .collect()
}

/// What a completed run commits.
#[derive(Debug, Clone)]
pub struct Completion<'a> {
    pub toolkit_id: &'a str,
    pub source_name: &'a str,
    /// Every document the source now has.
    pub documents: &'a BTreeMap<String, DocumentState>,
    pub counts: RunCounts,
    pub commit_sha: Option<&'a str>,
}

/// Commit a completed run in ONE transaction: the graph, the source's file
/// hashes, and its `completed` status. A run that fails before this leaves
/// the previous graph and hashes in place, so the next run re-reads exactly
/// what this one did not commit. Returns the graph's revision.
///
/// # Errors
///
/// See [`super::save`].
pub async fn complete(
    pool: &PgPool,
    key: GraphKey,
    graph: &Graph,
    completion: &Completion<'_>,
) -> Result<i64> {
    let mut transaction = pool.begin().await?;
    let revision = write_graph(&mut transaction, key, graph).await?;
    sqlx::query(
        "DELETE FROM inventory_graph.documents
          WHERE project_id = $1 AND application_id = $2 AND source_name = $3",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .bind(completion.source_name)
    .execute(&mut *transaction)
    .await?;
    let mut keys = Vec::with_capacity(completion.documents.len());
    let mut versions = Vec::with_capacity(completion.documents.len());
    let mut mimes = Vec::with_capacity(completion.documents.len());
    let mut acls = Vec::with_capacity(completion.documents.len());
    for (document, state) in completion.documents {
        keys.push(document.as_str());
        versions.push(state.version.as_str());
        mimes.push(state.mime.as_str());
        acls.push(Json(&state.acl));
    }
    sqlx::query(
        "INSERT INTO inventory_graph.documents
             (project_id, application_id, source_name, document_key, version, mime, acl)
         SELECT $1, $2, $3, k, v, m, a
           FROM unnest($4::text[], $5::text[], $6::text[], $7::jsonb[]) AS d(k, v, m, a)",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .bind(completion.source_name)
    .bind(&keys)
    .bind(&versions)
    .bind(&mimes)
    .bind(&acls)
    .execute(&mut *transaction)
    .await?;
    sqlx::query(
        "UPDATE inventory_graph.sources
            SET status = 'completed', entities_count = $4, relations_count = $5,
                documents_processed = $6, error_message = NULL, progress_message = NULL,
                commit_sha = $7, last_updated = clock_timestamp()
          WHERE project_id = $1 AND application_id = $2 AND toolkit_id = $3",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .bind(completion.toolkit_id)
    .bind(completion.counts.entities)
    .bind(completion.counts.relations)
    .bind(completion.counts.documents)
    .bind(completion.commit_sha)
    .execute(&mut *transaction)
    .await?;
    transaction.commit().await?;
    Ok(revision)
}

/// Commit a source's removal in ONE transaction: the graph without it, and
/// its status row and file hashes gone. Returns the graph's revision.
///
/// # Errors
///
/// See [`super::save`].
pub async fn remove(
    pool: &PgPool,
    key: GraphKey,
    graph: &Graph,
    toolkit_id: &str,
    source_name: &str,
) -> Result<i64> {
    let mut transaction = pool.begin().await?;
    let revision = write_graph(&mut transaction, key, graph).await?;
    sqlx::query(
        "DELETE FROM inventory_graph.documents
          WHERE project_id = $1 AND application_id = $2 AND source_name = $3",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .bind(source_name)
    .execute(&mut *transaction)
    .await?;
    sqlx::query(
        "DELETE FROM inventory_graph.sources
          WHERE project_id = $1 AND application_id = $2 AND toolkit_id = $3",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .bind(toolkit_id)
    .execute(&mut *transaction)
    .await?;
    transaction.commit().await?;
    Ok(revision)
}

/// The `sources_status.json` document of a graph:
/// `{sources: {toolkit_id: {...}}, last_modified}`, timestamps ISO 8601.
///
/// # Errors
///
/// [`StoreError::Database`](super::StoreError::Database).
pub async fn status_document(pool: &PgPool, key: GraphKey) -> Result<Value> {
    let rows = sqlx::query(
        "SELECT toolkit_id, toolkit_name, toolkit_type, status,
                to_char(started_at AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US') AS started_at,
                to_char(last_updated AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US') AS last_updated,
                entities_count, relations_count, documents_processed,
                error_message, progress_message, branch
           FROM inventory_graph.sources
          WHERE project_id = $1 AND application_id = $2
          ORDER BY toolkit_id",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .fetch_all(pool)
    .await?;
    let mut sources = Map::new();
    let mut last_modified: Option<String> = None;
    for row in rows {
        let toolkit_id: String = row.try_get("toolkit_id")?;
        let updated: String = row.try_get("last_updated")?;
        if last_modified.as_ref().is_none_or(|known| *known < updated) {
            last_modified = Some(updated.clone());
        }
        sources.insert(
            toolkit_id.clone(),
            json!({
                "toolkit_id": toolkit_id,
                "toolkit_name": row.try_get::<String, _>("toolkit_name")?,
                "toolkit_type": row.try_get::<String, _>("toolkit_type")?,
                "status": row.try_get::<String, _>("status")?,
                "started_at": row.try_get::<Option<String>, _>("started_at")?,
                "last_updated": updated,
                "entities_count": row.try_get::<i64, _>("entities_count")?,
                "relations_count": row.try_get::<i64, _>("relations_count")?,
                "documents_processed": row.try_get::<i64, _>("documents_processed")?,
                "error_message": row.try_get::<Option<String>, _>("error_message")?,
                "progress_message": row.try_get::<Option<String>, _>("progress_message")?,
                "branch": row.try_get::<Option<String>, _>("branch")?,
            }),
        );
    }
    Ok(json!({"sources": sources, "last_modified": last_modified}))
}

/// The right to ingest into one graph: a session advisory lock held on its
/// own connection for the whole run. Two runs on one toolkit would each
/// replace the graph with their own copy, the later erasing the earlier.
///
/// Dropping the lease closes its connection, which releases the lock even
/// when the run ends by a panic or a cancelled task.
#[derive(Debug)]
pub struct Lease {
    connection: Option<PoolConnection<Postgres>>,
}

impl Drop for Lease {
    fn drop(&mut self) {
        if let Some(connection) = self.connection.take() {
            // Detached, the connection is closed rather than returned to
            // the pool, where it would keep the lock.
            drop(connection.detach());
        }
    }
}

/// Take the ingestion lease of `key`, or `None` when another run holds it.
///
/// # Errors
///
/// [`StoreError::Database`](super::StoreError::Database).
pub async fn lease(pool: &PgPool, key: GraphKey) -> Result<Option<Lease>> {
    let mut connection = pool.acquire().await?;
    let taken: bool = sqlx::query_scalar(
        "SELECT pg_try_advisory_lock(hashtext('elitea_inventory.ingest'), hashtext($1 || '/' || $2))",
    )
    .bind(key.project_id.to_string())
    .bind(key.application_id.to_string())
    .fetch_one(&mut *connection)
    .await?;
    if taken {
        Ok(Some(Lease {
            connection: Some(connection),
        }))
    } else {
        Ok(None)
    }
}
