//! The hooks settings pane's commands (IPC.md, "Hooks", #1237): the
//! person's hooks in both files, their last runs in a workspace's session,
//! on/off, and the answer to a repository proposal. Saving starts nothing:
//! the next turn takes the files as they are then.

use std::sync::Arc;

use elitea_local_tools::error::{ErrorCode, ToolError};
use elitea_local_tools::hooks::HookRun;
use elitea_local_tools::hooks::config::{Hook, HooksConfig, Scope};
use elitea_local_tools::hooks::proposal::Proposal;
use serde::Serialize;
use serde_json::json;
use tauri::{AppHandle, State};

use crate::d0::turn::AgentHost;
use crate::hooks::EVENT_NAME;
use crate::local_commands::{IpcError, LocalState};

fn settings_error(error: &ToolError) -> IpcError {
    let code = match error.code() {
        ErrorCode::InvalidArgument => "invalid_request",
        ErrorCode::Denied => "hooks_settings_refused",
        ErrorCode::NotFound => "hook_unknown",
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
        .map_err(|_| IpcError::new("internal", "the hooks settings call stopped unexpectedly"))?
}

/// One hooks file as the pane shows it.
#[derive(Serialize)]
pub struct HooksFileView {
    pub path: String,
    pub hooks: Vec<Hook>,
    /// Why the file is not used (refused or broken): it adds no hook.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

/// `hooks_list`'s answer.
#[derive(Serialize)]
pub struct HooksList {
    /// Whether the policy lets hooks run (local work and the shell on).
    pub allowed: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub user: Option<HooksFileView>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub workspace: Option<HooksFileView>,
    /// Each hook's last run in the workspace's session (empty before its
    /// first turn).
    pub last_runs: Vec<HookRun>,
    /// What the folder proposes that the person has not answered.
    pub proposals: Vec<Proposal>,
}

fn view(agents: &AgentHost, scope: Scope, workspace_id: Option<&str>) -> Option<HooksFileView> {
    let files = agents.hook_files();
    let path = match scope {
        Scope::User => files.user_path()?,
        Scope::Workspace => files.workspace_path(workspace_id?),
    };
    let (hooks, error) = match files.load(scope, workspace_id) {
        Ok(file) => (file.hooks, None),
        Err(error) => (Vec::new(), Some(error.message().to_owned())),
    };
    Some(HooksFileView {
        path: path.display().to_string(),
        hooks,
        error,
    })
}

/// `hooks_list`: the person's hooks (every workspace's and, with
/// `workspace_id`, that workspace's), their last runs and the folder's
/// unanswered proposals.
#[tauri::command(rename_all = "snake_case")]
pub async fn hooks_list(
    state: State<'_, LocalState>,
    workspace_id: Option<String>,
) -> Result<HooksList, IpcError> {
    if let Some(id) = &workspace_id {
        known(&state, id)?;
    }
    let agents = state.agents.clone();
    blocking(move || {
        let id = workspace_id.as_deref();
        let proposals = id
            .and_then(|id| {
                agents.rules_folder(id).ok().map(|(root, path_deny)| {
                    agents.hook_files().pending_proposals(id, &root, &path_deny)
                })
            })
            .unwrap_or_default();
        Ok(HooksList {
            allowed: agents.hooks_allowed(),
            user: view(&agents, Scope::User, None),
            workspace: id.and_then(|id| view(&agents, Scope::Workspace, Some(id))),
            last_runs: id
                .and_then(|id| agents.process_session(id))
                .map(|(session, _)| session.hooks().last_runs())
                .unwrap_or_default(),
            proposals,
        })
    })
    .await
}

/// `hooks_save`: replace the `scope`'s hooks (`workspace` needs
/// `workspace_id`) with `hooks`, schema-checked; the next turn runs them.
#[tauri::command(rename_all = "snake_case")]
pub async fn hooks_save(
    state: State<'_, LocalState>,
    scope: Scope,
    hooks: Vec<Hook>,
    workspace_id: Option<String>,
) -> Result<(), IpcError> {
    if let Some(id) = &workspace_id {
        known(&state, id)?;
    }
    // Checked here too, so a bad list is refused before the file is touched.
    HooksConfig {
        hooks: hooks.clone(),
        ..HooksConfig::default()
    }
    .validate()
    .map_err(|error| settings_error(&error))?;
    let agents = state.agents.clone();
    blocking(move || {
        agents
            .hook_files()
            .save_hooks(scope, workspace_id.as_deref(), hooks)
            .map_err(|error| settings_error(&error))
    })
    .await
}

/// `hooks_set_enabled`: turn the hook `id` of the `scope`'s file on or
/// off; whether it was there.
#[tauri::command(rename_all = "snake_case")]
pub async fn hooks_set_enabled(
    state: State<'_, LocalState>,
    scope: Scope,
    id: String,
    enabled: bool,
    workspace_id: Option<String>,
) -> Result<bool, IpcError> {
    if let Some(id) = &workspace_id {
        known(&state, id)?;
    }
    let agents = state.agents.clone();
    blocking(move || {
        agents
            .hook_files()
            .set_enabled(scope, workspace_id.as_deref(), &id, enabled)
            .map_err(|error| settings_error(&error))
    })
    .await
}

/// `hooks_proposal_answer`'s answer.
#[derive(Serialize)]
pub struct HooksProposalAnswered {
    /// The hooks copied into the workspace's file.
    pub accepted: Vec<String>,
}

/// `hooks_proposal_answer`: copy the hooks named in `accept` (none:
/// declined) from the folder's proposal whose file hashes to `file_sha256`
/// into the workspace's file; the proposal is not shown again until the
/// file changes. Nothing runs until the next turn.
#[tauri::command(rename_all = "snake_case")]
pub async fn hooks_proposal_answer(
    state: State<'_, LocalState>,
    workspace_id: String,
    file_sha256: String,
    accept: Vec<String>,
) -> Result<HooksProposalAnswered, IpcError> {
    known(&state, &workspace_id)?;
    let agents = state.agents.clone();
    blocking(move || {
        let (root, path_deny) = agents.rules_folder(&workspace_id)?;
        let accepted = agents
            .hook_files()
            .answer(&workspace_id, &root, &path_deny, &file_sha256, &accept)
            .map_err(|error| settings_error(&error))?;
        Ok(HooksProposalAnswered { accepted })
    })
    .await
}

/// A workspace was opened: its folder's unanswered hook proposals go to the
/// window as `hooks_proposal` events on `hooks://event`. Nothing runs from
/// them.
pub fn propose_on_open(app: &AppHandle, agents: &Arc<AgentHost>, workspace_id: &str) {
    let Ok((root, path_deny)) = agents.rules_folder(workspace_id) else {
        return;
    };
    for proposal in agents
        .hook_files()
        .pending_proposals(workspace_id, &root, &path_deny)
    {
        use tauri::Emitter as _;
        let payload = json!({
            "kind": "hooks_proposal",
            "workspace_id": workspace_id,
            "file": proposal.file,
            "file_sha256": proposal.file_sha256,
            "hooks": proposal.hooks,
        });
        if let Err(error) = app.emit_to("main", EVENT_NAME, &payload) {
            log::warn!("could not deliver a hooks proposal: {error}");
        }
    }
}
