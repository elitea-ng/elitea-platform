//! `lsp_status` (IPC.md, "Language servers", #1236): the composer chip's
//! state when it mounts. The `lsp://event` stream only carries changes, so
//! a chip mounted after a server started would not know of it.

use elitea_local_tools::lsp::LspStatus;
use serde::Serialize;
use tauri::State;

use crate::local_commands::{IpcError, LocalState};

/// `lsp_status`'s answer.
#[derive(Serialize)]
pub struct LspStatusView {
    /// What `statuses` reflects: every `lsp://event` with a smaller `seq`.
    pub seq: u64,
    pub statuses: Vec<LspStatus>,
}

/// `lsp_status`: the language servers of the workspace's session (none
/// before its first turn).
#[tauri::command(rename_all = "snake_case")]
pub async fn lsp_status(
    state: State<'_, LocalState>,
    workspace_id: String,
) -> Result<LspStatusView, IpcError> {
    if state.workspaces.get(&workspace_id)?.is_none() {
        return Err(IpcError::new(
            "workspace_unknown",
            "That workspace is not known to this app.",
        ));
    }
    // Read before the statuses: an event numbered below it changed them
    // before it was sent, so they already show it.
    let seq = crate::d0::lsp::next_seq();
    let statuses = state
        .agents
        .process_session(&workspace_id)
        .map(|(session, _)| session.lsp().status())
        .unwrap_or_default();
    Ok(LspStatusView { seq, statuses })
}
