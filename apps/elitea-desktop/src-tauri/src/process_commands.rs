//! The Processes pane's commands (IPC.md, "Background processes", #1223):
//! list a workspace's background processes, read their output, type into
//! them, resize a terminal, stop or pin one.
//!
//! These are the person's own controls: none of them asks for an approval
//! (the agent's `process_*` tools do), and none of them starts anything.

use std::sync::Arc;

use elitea_local_tools::error::{ErrorCode, ToolError};
use elitea_local_tools::process::{Process, ProcessInfo, complete_prefix};
use elitea_local_tools::session::LocalSession;
use serde::Serialize;
use tauri::State;

use crate::local_commands::{IpcError, LocalState};

/// `process_read`'s cap on one answer.
const MAX_READ_BYTES: usize = 256 * 1024;
/// `process_write`'s cap on one write.
const MAX_WRITE_BYTES: usize = 64 * 1024;

fn process_error(error: &ToolError) -> IpcError {
    let code = match error.code() {
        ErrorCode::NotFound => "process_unknown",
        ErrorCode::Conflict => "process_ended",
        ErrorCode::Timeout => "process_not_reading",
        ErrorCode::InvalidArgument => "invalid_request",
        _ => "internal",
    };
    IpcError::new(code, error.message())
}

fn unknown(process_id: &str) -> IpcError {
    IpcError::new(
        "process_unknown",
        format!("process {process_id} is not known to this workspace"),
    )
}

/// The workspace's session, refused for a workspace the app does not know.
fn session(
    state: &LocalState,
    workspace_id: &str,
) -> Result<Option<(Arc<LocalSession>, bool)>, IpcError> {
    if state.workspaces.get(workspace_id)?.is_none() {
        return Err(IpcError::new(
            "workspace_unknown",
            "That workspace is not known to this app.",
        ));
    }
    Ok(state.agents.process_session(workspace_id))
}

fn process(
    state: &LocalState,
    workspace_id: &str,
    process_id: &str,
) -> Result<(Arc<LocalSession>, Arc<Process>, bool), IpcError> {
    let (session, turn_running) =
        session(state, workspace_id)?.ok_or_else(|| unknown(process_id))?;
    let process = session
        .process(process_id)
        .map_err(|error| process_error(&error))?;
    Ok((session, process, turn_running))
}

/// The workspace's background processes, oldest first (none before a turn
/// opened its session).
#[tauri::command(rename_all = "snake_case")]
pub fn process_list(
    state: State<'_, LocalState>,
    workspace_id: String,
) -> Result<Vec<ProcessInfo>, IpcError> {
    Ok(session(&state, &workspace_id)?
        .map(|(session, _)| session.processes())
        .unwrap_or_default())
}

/// What `process_read` answers.
#[derive(Debug, Serialize)]
pub struct ProcessOutput {
    pub data: String,
    /// The byte offset of `data`'s first byte.
    pub offset: u64,
    /// Where the next read continues.
    pub next_offset: u64,
    /// Bytes between `since` and `offset` the buffer no longer holds.
    pub dropped_bytes: u64,
    /// A terminal process's rendered screen, rows joined by `\n`.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub screen: Option<String>,
}

/// The output from byte `since`, or without it the latest 256 KiB the
/// buffer holds (a pane opened late catches up with it).
#[tauri::command(rename_all = "snake_case")]
pub fn process_read(
    state: State<'_, LocalState>,
    workspace_id: String,
    process_id: String,
    since: Option<u64>,
) -> Result<ProcessOutput, IpcError> {
    let (_, process, _) = process(&state, &workspace_id, &process_id)?;
    let since = since.unwrap_or_else(|| {
        process
            .end()
            .saturating_sub(u64::try_from(MAX_READ_BYTES).unwrap_or(u64::MAX))
    });
    let chunk = process.read(since, MAX_READ_BYTES);
    let keep = complete_prefix(&chunk.data);
    Ok(ProcessOutput {
        data: String::from_utf8_lossy(&chunk.data[..keep]).into_owned(),
        offset: chunk.start,
        next_offset: chunk.start + u64::try_from(keep).unwrap_or(u64::MAX),
        dropped_bytes: chunk.dropped,
        screen: process.screen(),
    })
}

/// Type into a process: the keystrokes as the terminal produced them
/// (nothing is added). Async: the write runs on the process's writer
/// thread, never the main one, so a process that does not read its input
/// answers `process_not_reading` after a few seconds instead of freezing
/// the window.
#[tauri::command(rename_all = "snake_case")]
pub async fn process_write(
    state: State<'_, LocalState>,
    workspace_id: String,
    process_id: String,
    data: String,
) -> Result<(), IpcError> {
    if data.is_empty() || data.len() > MAX_WRITE_BYTES {
        return Err(IpcError::new(
            "invalid_request",
            "`data` is 1 byte to 64 KiB",
        ));
    }
    let (_, process, _) = process(&state, &workspace_id, &process_id)?;
    process
        .write(data.as_bytes())
        .await
        .map_err(|error| process_error(&error))
}

/// A terminal process's size, in rows and columns (1 to 1000 each).
#[tauri::command(rename_all = "snake_case")]
pub fn process_resize(
    state: State<'_, LocalState>,
    workspace_id: String,
    process_id: String,
    rows: u16,
    cols: u16,
) -> Result<(), IpcError> {
    let (_, process, _) = process(&state, &workspace_id, &process_id)?;
    process
        .resize(rows, cols)
        .map_err(|error| process_error(&error))
}

/// Stop a process: `SIGTERM` to its group, `SIGKILL` 2 s later.
#[tauri::command(rename_all = "snake_case")]
pub async fn process_kill(
    state: State<'_, LocalState>,
    workspace_id: String,
    process_id: String,
) -> Result<(), IpcError> {
    let (session, _, _) = process(&state, &workspace_id, &process_id)?;
    session
        .kill_process(&process_id)
        .await
        .map_err(|error| process_error(&error))
}

/// Pin a process past its turn's end, or unpin it (an unpinned process
/// of an ended turn stops now).
#[tauri::command(rename_all = "snake_case")]
pub async fn process_pin(
    state: State<'_, LocalState>,
    workspace_id: String,
    process_id: String,
    pinned: bool,
) -> Result<(), IpcError> {
    let (session, _, turn_running) = process(&state, &workspace_id, &process_id)?;
    session
        .pin_process(&process_id, pinned, turn_running)
        .await
        .map_err(|error| process_error(&error))
}
