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
/// database failure (its text stays in the log: it can quote a statement).
pub async fn run(
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
