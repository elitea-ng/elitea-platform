//! The index-deletion tools (ADR-0031 phase D0, issue #1243).
//!
//! * `delete_wiki_index` deletes one wiki's index: arguments `wiki_id` and
//!   the host's reserved [`crate::storage::PROJECT_ARG`].
//! * `delete_project_wikis` deletes every wiki of the project the host
//!   named, and nothing else.
//!
//! Both read the project from the reserved argument the host stamps from the
//! authenticated hop, exactly as `ask` does; a caller cannot name another
//! project. Neither streams: the work is one or a few database transactions.
//! The storage side is [`crate::storage::delete`].

use crate::errors::{EngineError, ErrorType};
use crate::runner::StopSignal;
use crate::storage::build::PublishSettings;
use crate::storage::delete::{self, ProjectLimits, WikiDeletion};
use crate::storage::{ProjectScope, StorageError, WikiKey};
use serde_json::{Map, Value, json};
use sqlx::postgres::PgPool;
use std::time::Duration;

/// The tools [`run`] serves.
pub const MAINTENANCE_TOOLS: [&str; 2] = ["delete_wiki_index", "delete_project_wikis"];

fn storage_failure(action: &str, error: &StorageError) -> EngineError {
    tracing::error!(%error, "{action} failed");
    if let StorageError::Busy(message) = error {
        // A publish of the wiki holds it: retryable, and the caller must be
        // able to tell it from a failure of the database.
        return EngineError::new(ErrorType::Runtime, format!("{action} failed: {message}"));
    }
    if let StorageError::Delete(message) = error {
        // Ours, and it quotes no statement: the caller needs to read it (a
        // project that still holds wikis after the rounds a deletion makes).
        return EngineError::new(ErrorType::Runtime, format!("{action} failed: {message}"));
    }
    EngineError::new(
        ErrorType::Runtime,
        format!("{action} failed: the index database refused it"),
    )
}

fn rows_json(deleted: &WikiDeletion) -> Value {
    json!({
        "nodes": deleted.rows.nodes,
        "edges": deleted.rows.edges,
        "embeddings": deleted.rows.embeddings,
        "statistics": deleted.rows.statistics,
        // The `wikis` row itself: 1 when there was one.
        "wikis": u64::from(deleted.existed),
    })
}

/// Run one maintenance tool. `build_stale_after` is the build sweep's
/// threshold: `delete_project_wikis` deletes only builds older than it.
///
/// # Errors
///
/// A `ValueError` for a missing project or wiki id; a `RuntimeError` for a
/// database failure (its text stays in the log: it can quote a statement)
/// or for a publish that held the wiki past the bounded wait (retry); the
/// stop line after a stop.
pub async fn run(
    tool: &str,
    arguments: &Map<String, Value>,
    pool: &PgPool,
    settings: &PublishSettings,
    build_stale_after: Duration,
    stop: &StopSignal,
) -> Result<Value, EngineError> {
    // A stop (the invocation was cancelled, or its reader went away) ends the
    // deletion at once: the work future is dropped, which cancels the
    // transaction in the database (`delete::CancelOnDrop`) instead of letting
    // it wait on a publish's lock for nobody.
    tokio::select! {
        result = run_tool(tool, arguments, pool, settings, build_stale_after) => result,
        () = stop.stopped() => Err(EngineError::cancelled()),
    }
}

async fn run_tool(
    tool: &str,
    arguments: &Map<String, Value>,
    pool: &PgPool,
    settings: &PublishSettings,
    build_stale_after: Duration,
) -> Result<Value, EngineError> {
    let project = ProjectScope::from_arguments(arguments)?;
    match tool {
        "delete_wiki_index" => {
            let wiki_id = arguments
                .get("wiki_id")
                .and_then(Value::as_str)
                .map(str::trim)
                .filter(|id| !id.is_empty())
                .ok_or_else(|| EngineError::new(ErrorType::Value, "wiki_id is required"))?;
            let key = WikiKey::new(project, wiki_id.to_owned());
            let deleted = delete::delete_wiki(pool, &key, settings)
                .await
                .map_err(|e| storage_failure("Deleting the wiki's index", &e))?;
            Ok(json!({
                "success": true,
                "wiki_id": wiki_id,
                // Anything removed counts, a `wikis` row or not: stray
                // statistics rows are a deletion too.
                "deleted": deleted.deleted(),
                "rows": rows_json(&deleted),
            }))
        }
        "delete_project_wikis" => {
            let limits = ProjectLimits::new(build_stale_after);
            let deleted = delete::delete_project(pool, project, settings, &limits)
                .await
                .map_err(|e| storage_failure("Deleting the project's wiki indexes", &e))?;
            Ok(json!({
                "success": true,
                "project_id": project.id(),
                "wikis": deleted.wikis,
                "rows": {
                    "nodes": deleted.rows.nodes,
                    "edges": deleted.rows.edges,
                    "embeddings": deleted.rows.embeddings,
                    "statistics": deleted.rows.statistics,
                },
                // What each deleted wiki lost, in the order of `wikis`.
                "per_wiki": deleted
                    .wikis
                    .iter()
                    .zip(&deleted.per_wiki)
                    .map(|(wiki, rows)| json!({
                        "wiki_id": wiki,
                        "nodes": rows.nodes,
                        "edges": rows.edges,
                        "embeddings": rows.embeddings,
                        "statistics": rows.statistics,
                    }))
                    .collect::<Vec<_>>(),
                "builds": deleted.builds,
                "live_builds": deleted.live_builds,
            }))
        }
        other => Err(EngineError::new(
            ErrorType::Key,
            format!("Unknown tool: {other}"),
        )),
    }
}
