//! The parallel agents panel's commands (IPC.md, "Parallel agents",
//! #1235): task worktrees of a workspace's repository, each a workspace of
//! its own, and merging one back into the folder.

use std::sync::Arc;

use serde::Serialize;
use tauri::State;

use crate::local_commands::IpcError;
use crate::worktrees::{Diff, Entry, MergeApplied, MergePreflight, WorktreeHost};

/// `worktree_create`: a task worktree of `workspace_id` for `thread_uuid`.
#[tauri::command(rename_all = "snake_case")]
pub async fn worktree_create(
    host: State<'_, Arc<WorktreeHost>>,
    workspace_id: String,
    thread_uuid: String,
    base: Option<String>,
) -> Result<Entry, IpcError> {
    Ok(host
        .create(&workspace_id, &thread_uuid, base.as_deref())
        .await?)
}

/// `worktree_list`: the workspace's task worktrees, newest first.
#[tauri::command(rename_all = "snake_case")]
pub async fn worktree_list(
    host: State<'_, Arc<WorktreeHost>>,
    workspace_id: String,
) -> Result<Vec<Entry>, IpcError> {
    Ok(host.list(&workspace_id).await?)
}

/// `worktree_diff`: a task worktree against its base.
#[tauri::command(rename_all = "snake_case")]
pub async fn worktree_diff(
    host: State<'_, Arc<WorktreeHost>>,
    workspace_id: String,
    thread_uuid: String,
) -> Result<Diff, IpcError> {
    Ok(host.diff(&workspace_id, &thread_uuid).await?)
}

/// `worktree_remove`: a task worktree and what the app keeps for it.
#[tauri::command(rename_all = "snake_case")]
pub async fn worktree_remove(
    host: State<'_, Arc<WorktreeHost>>,
    workspace_id: String,
    thread_uuid: String,
    force: Option<bool>,
) -> Result<(), IpcError> {
    Ok(host
        .remove(&workspace_id, &thread_uuid, force.unwrap_or(false))
        .await?)
}

/// `worktree_merge_preflight`: what merging a task worktree back would do.
#[tauri::command(rename_all = "snake_case")]
pub async fn worktree_merge_preflight(
    host: State<'_, Arc<WorktreeHost>>,
    workspace_id: String,
    thread_uuid: String,
) -> Result<MergePreflight, IpcError> {
    Ok(host.merge_preflight(&workspace_id, &thread_uuid).await?)
}

/// `worktree_merge_apply`: apply a preview to the folder.
#[tauri::command(rename_all = "snake_case")]
pub async fn worktree_merge_apply(
    host: State<'_, Arc<WorktreeHost>>,
    preflight_id: String,
) -> Result<MergeApplied, IpcError> {
    Ok(host.merge_apply(&preflight_id).await?)
}

/// What `worktree_merge_undo` restored.
#[derive(Serialize)]
pub struct Restored {
    pub restored: Vec<String>,
}

/// `worktree_merge_undo`: restore the checkpoint a merge took
/// (`confirm_older` when the folder was checkpointed since).
#[tauri::command(rename_all = "snake_case")]
pub async fn worktree_merge_undo(
    host: State<'_, Arc<WorktreeHost>>,
    checkpoint_id: String,
    confirm_older: Option<bool>,
) -> Result<Restored, IpcError> {
    Ok(Restored {
        restored: host
            .merge_undo(&checkpoint_id, confirm_older.unwrap_or(false))
            .await?,
    })
}
