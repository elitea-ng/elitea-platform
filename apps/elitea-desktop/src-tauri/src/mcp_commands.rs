//! The MCP settings pane's commands (IPC.md, "Local MCP servers", #1230):
//! the person's servers in both settings files, their status in a
//! workspace's session, Restart, and the answer to a repository proposal.
//!
//! These are the person's own controls: saving a server starts nothing
//! (the next turn that may offer its tools starts it, under the sandbox and
//! the approval rules); Restart starts it through the same path.

use std::sync::Arc;

use elitea_local_tools::error::{ErrorCode, ToolError};
use elitea_local_tools::mcp::ServerStatus;
use elitea_local_tools::mcp::config::{Scope, ServerConfig};
use elitea_local_tools::mcp::proposal::Proposal;
use serde::Serialize;
use serde_json::json;
use tauri::{AppHandle, State};

use crate::d0::turn::AgentHost;
use crate::local_commands::{IpcError, LocalState};
use crate::mcp::EVENT_NAME;

fn settings_error(error: &ToolError) -> IpcError {
    let code = match error.code() {
        ErrorCode::InvalidArgument => "invalid_request",
        ErrorCode::Denied => "mcp_settings_refused",
        ErrorCode::NotFound => "mcp_server_unknown",
        ErrorCode::Conflict => "conflict",
        _ => "storage",
    };
    IpcError::new(code, error.message())
}

fn known(state: &LocalState, workspace_id: &str) -> Result<(), IpcError> {
    if state.workspaces.get(workspace_id)?.is_none() {
        return Err(IpcError::new(
            "workspace_unknown",
            "That workspace is not known to this app.",
        ));
    }
    Ok(())
}

async fn blocking<T: Send + 'static>(
    work: impl FnOnce() -> Result<T, IpcError> + Send + 'static,
) -> Result<T, IpcError> {
    tokio::task::spawn_blocking(work)
        .await
        .map_err(|_| IpcError::new("internal", "the MCP settings call stopped unexpectedly"))?
}

/// One settings file as the pane shows it.
#[derive(Serialize)]
pub struct McpFileView {
    pub path: String,
    pub servers: std::collections::BTreeMap<String, ServerConfig>,
    /// Why the file is not used (refused or broken): it adds no server.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

/// `mcp_list`'s answer.
#[derive(Serialize)]
pub struct McpList {
    /// Whether the organisation's policy lets local MCP servers run.
    pub allowed: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub user: Option<McpFileView>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub workspace: Option<McpFileView>,
    /// The workspace's servers as its session has them (empty before its
    /// first turn).
    pub status: Vec<ServerStatus>,
    /// What the folder proposes that the person has not answered.
    pub proposals: Vec<Proposal>,
}

fn view(agents: &AgentHost, scope: Scope, workspace_id: Option<&str>) -> Option<McpFileView> {
    let files = agents.mcp_files();
    let path = match scope {
        Scope::User => files.user_path()?,
        Scope::Workspace => files.workspace_path(workspace_id?),
    };
    let (servers, error) = match files.load(scope, workspace_id) {
        Ok(file) => (file.servers, None),
        Err(error) => (Default::default(), Some(error.message().to_owned())),
    };
    Some(McpFileView {
        path: path.display().to_string(),
        servers,
        error,
    })
}

/// `mcp_list`: the person's servers (every workspace's and, with
/// `workspace_id`, that workspace's), their status in its session and the
/// folder's unanswered proposals.
#[tauri::command(rename_all = "snake_case")]
pub async fn mcp_list(
    state: State<'_, LocalState>,
    workspace_id: Option<String>,
) -> Result<McpList, IpcError> {
    if let Some(id) = &workspace_id {
        known(&state, id)?;
    }
    let agents = state.agents.clone();
    blocking(move || {
        let id = workspace_id.as_deref();
        let allowed = agents.local_mcp_allowed();
        let proposals = match id {
            Some(id) if allowed => agents
                .rules_folder(id)
                .map(|(root, path_deny)| {
                    agents.mcp_files().pending_proposals(id, &root, &path_deny)
                })
                .unwrap_or_default(),
            _ => Vec::new(),
        };
        Ok(McpList {
            allowed,
            user: view(&agents, Scope::User, None),
            workspace: id.and_then(|id| view(&agents, Scope::Workspace, Some(id))),
            status: id
                .and_then(|id| agents.process_session(id))
                .map(|(session, _)| session.mcp().status())
                .unwrap_or_default(),
            proposals,
        })
    })
    .await
}

/// `mcp_save`: add or replace `name` in the `scope`'s file (`workspace`
/// needs `workspace_id`). Open sessions take it now: a changed server
/// stops, and starts again with the next turn.
#[tauri::command(rename_all = "snake_case")]
pub async fn mcp_save(
    state: State<'_, LocalState>,
    scope: Scope,
    name: String,
    server: ServerConfig,
    workspace_id: Option<String>,
) -> Result<(), IpcError> {
    if let Some(id) = &workspace_id {
        known(&state, id)?;
    }
    let agents = state.agents.clone();
    blocking(move || {
        agents
            .mcp_files()
            .save_server(scope, workspace_id.as_deref(), &name, server)
            .map_err(|error| settings_error(&error))?;
        agents.mcp_reconfigure(only(scope, workspace_id.as_deref()));
        Ok(())
    })
    .await
}

/// A workspace file's change reaches that workspace's session; a user
/// file's, every session.
fn only(scope: Scope, workspace_id: Option<&str>) -> Option<&str> {
    match scope {
        Scope::User => None,
        Scope::Workspace => workspace_id,
    }
}

/// `mcp_remove`: remove `name` from the `scope`'s file; whether it was
/// there. A running server of that name stops.
#[tauri::command(rename_all = "snake_case")]
pub async fn mcp_remove(
    state: State<'_, LocalState>,
    scope: Scope,
    name: String,
    workspace_id: Option<String>,
) -> Result<bool, IpcError> {
    if let Some(id) = &workspace_id {
        known(&state, id)?;
    }
    let agents = state.agents.clone();
    blocking(move || {
        let removed = agents
            .mcp_files()
            .remove_server(scope, workspace_id.as_deref(), &name)
            .map_err(|error| settings_error(&error))?;
        agents.mcp_reconfigure(only(scope, workspace_id.as_deref()));
        Ok(removed)
    })
    .await
}

/// `mcp_restart`: stop `name` in the workspace's session and start it
/// again, under the sandbox and the approval rules (a start that would ask
/// is refused outside a turn: nobody is there to answer the card).
#[tauri::command(rename_all = "snake_case")]
pub async fn mcp_restart(
    state: State<'_, LocalState>,
    workspace_id: String,
    name: String,
) -> Result<ServerStatus, IpcError> {
    known(&state, &workspace_id)?;
    if !state.agents.local_mcp_allowed() {
        return Err(IpcError::new(
            "local_mcp_disabled",
            "Local MCP servers are turned off by your organisation's policy.",
        ));
    }
    let (session, _) = state.agents.process_session(&workspace_id).ok_or_else(|| {
        IpcError::new(
            "mcp_not_started",
            "This workspace's MCP servers start with its next turn.",
        )
    })?;
    session
        .mcp()
        .restart(&session, &name)
        .await
        .map_err(|error| settings_error(&error))
}

/// One server's status in one workspace (`mcp_status`).
#[derive(Serialize)]
pub struct WorkspaceServerStatus {
    pub workspace_id: String,
    #[serde(flatten)]
    pub status: ServerStatus,
}

/// `mcp_status`: the servers of the workspace's session (or, without
/// `workspace_id`, of every open session).
#[tauri::command(rename_all = "snake_case")]
pub async fn mcp_status(
    state: State<'_, LocalState>,
    workspace_id: Option<String>,
) -> Result<Vec<WorkspaceServerStatus>, IpcError> {
    if let Some(id) = &workspace_id {
        known(&state, id)?;
    }
    let ids: Vec<String> = match workspace_id {
        Some(id) => vec![id],
        None => state.workspaces.list()?.into_iter().map(|w| w.id).collect(),
    };
    Ok(ids
        .into_iter()
        .filter_map(|id| {
            let (session, _) = state.agents.process_session(&id)?;
            Some((id, session.mcp().status()))
        })
        .flat_map(|(id, statuses)| {
            statuses
                .into_iter()
                .map(move |status| WorkspaceServerStatus {
                    workspace_id: id.clone(),
                    status,
                })
        })
        .collect())
}

/// `mcp_proposal_answer`'s answer.
#[derive(Serialize)]
pub struct ProposalAnswered {
    /// The servers copied into the workspace's settings.
    pub accepted: Vec<String>,
}

/// `mcp_proposal_answer`: copy the servers named in `accept` (none:
/// declined) from the folder's proposal whose file hashes to `file_sha256`
/// into the workspace's settings; the proposal is not shown again. Nothing
/// starts until the next turn.
#[tauri::command(rename_all = "snake_case")]
pub async fn mcp_proposal_answer(
    state: State<'_, LocalState>,
    workspace_id: String,
    file_sha256: String,
    accept: Vec<String>,
) -> Result<ProposalAnswered, IpcError> {
    known(&state, &workspace_id)?;
    let agents = state.agents.clone();
    blocking(move || {
        if !agents.local_mcp_allowed() {
            return Err(IpcError::new(
                "local_mcp_disabled",
                "Local MCP servers are turned off by your organisation's policy.",
            ));
        }
        let (root, path_deny) = agents.rules_folder(&workspace_id)?;
        let accepted = agents
            .mcp_files()
            .answer(&workspace_id, &root, &path_deny, &file_sha256, &accept)
            .map_err(|error| settings_error(&error))?;
        agents.mcp_reconfigure(Some(&workspace_id));
        Ok(ProposalAnswered { accepted })
    })
    .await
}

/// A workspace was opened: its folder's unanswered proposals go to the
/// window as `mcp_proposal` events on `mcp://event` (when the policy lets MCP servers run).
/// Nothing starts from them.
pub fn propose_on_open(app: &AppHandle, agents: &Arc<AgentHost>, workspace_id: &str) {
    if !agents.local_mcp_allowed() {
        return;
    }
    let Ok((root, path_deny)) = agents.rules_folder(workspace_id) else {
        return;
    };
    for proposal in agents
        .mcp_files()
        .pending_proposals(workspace_id, &root, &path_deny)
    {
        use tauri::Emitter as _;
        let payload = json!({
            "kind": "mcp_proposal",
            "workspace_id": workspace_id,
            "file": proposal.file,
            "file_sha256": proposal.file_sha256,
            "servers": proposal.servers,
        });
        if let Err(error) = app.emit_to("main", EVENT_NAME, &payload) {
            log::warn!("could not deliver an MCP proposal: {error}");
        }
    }
}
