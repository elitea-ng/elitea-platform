//! The local-work IPC surface (IPC.md): workspaces and the D0 agent turn.
//!
//! Like `commands.rs`, every command is named in `build.rs` and allowed in
//! `capabilities/default.json` for the main window only. Argument keys are
//! snake_case, exactly as IPC.md lists them. Nothing here returns a token.

use std::sync::Arc;

use async_trait::async_trait;
use serde::Serialize;
use serde_json::Value;
use tauri::{AppHandle, Emitter as _, State};

use crate::auth::{AuthService, RefreshResult};
use crate::d0::api::{ApiError, Bearer, Credentials};
use crate::d0::approvals::UiDecision;
use crate::d0::events::{AgentEvent, EVENT_NAME, EventEmitter};
use crate::d0::recorder::FileChange;
use crate::d0::turn::{AgentHost, PolicySource, TurnError, TurnRequest, TurnStarted};
use crate::error::HostError;
use crate::settings::SettingsFiles;
use crate::workspaces::{Workspace, WorkspaceStore};

pub struct LocalState {
    pub workspaces: Arc<WorkspaceStore>,
    pub agents: Arc<AgentHost>,
}

impl From<TurnError> for HostError {
    fn from(error: TurnError) -> Self {
        Self::Agent(error.message)
    }
}

/// The signed-in session's bearer, for the connected origin only.
pub struct AuthCredentials(pub Arc<AuthService>);

fn not_signed_in() -> ApiError {
    ApiError::local("not_signed_in", "Sign in to the deployment first.")
}

impl AuthCredentials {
    async fn current(&self) -> Result<Bearer, ApiError> {
        let origin = self
            .0
            .state()
            .ok()
            .and_then(|state| state.origin)
            .ok_or_else(not_signed_in)?;
        let token = self
            .0
            .access_token()
            .await
            .map_err(|e| ApiError::local("not_signed_in", e.to_string()))?
            .ok_or_else(not_signed_in)?;
        Ok(Bearer {
            origin: origin.trim_end_matches('/').to_owned(),
            token: token.token,
        })
    }
}

#[async_trait]
impl Credentials for AuthCredentials {
    async fn bearer(&self) -> Result<Bearer, ApiError> {
        self.current().await
    }

    async fn refreshed(&self) -> Result<Bearer, ApiError> {
        match self.0.refresh().await {
            Ok(RefreshResult::Refreshed) => self.current().await,
            _ => Err(not_signed_in()),
        }
    }
}

/// The stored client policy's `local_work` section.
pub struct StoredPolicy(pub SettingsFiles);

impl PolicySource for StoredPolicy {
    fn local_work(&self) -> Option<Value> {
        self.0.policy().ok().flatten()?.get("local_work").cloned()
    }
}

/// Events to the main window only.
pub struct MainWindowEvents(pub AppHandle);

impl EventEmitter for MainWindowEvents {
    fn emit(&self, event: AgentEvent) {
        if let Err(error) = self.0.emit_to("main", EVENT_NAME, &event) {
            eprintln!("elitea-desktop: could not deliver an agent event: {error}");
        }
    }
}

/// Pick a folder with the native dialog and open it as a workspace;
/// `null` when the person cancelled.
#[tauri::command(rename_all = "snake_case")]
pub async fn workspace_open(
    app: AppHandle,
    state: State<'_, LocalState>,
) -> Result<Option<Workspace>, HostError> {
    use tauri_plugin_dialog::DialogExt as _;
    let (sender, receiver) = tokio::sync::oneshot::channel();
    app.dialog().file().pick_folder(move |picked| {
        let _ = sender.send(picked);
    });
    let Some(picked) = receiver.await.ok().flatten() else {
        return Ok(None);
    };
    let path = picked
        .into_path()
        .map_err(|_| HostError::Storage("the dialog did not return a local folder".into()))?;
    state.workspaces.add(&path).map(Some)
}

#[tauri::command(rename_all = "snake_case")]
pub fn workspace_list(state: State<'_, LocalState>) -> Result<Vec<Workspace>, HostError> {
    state.workspaces.list()
}

#[tauri::command(rename_all = "snake_case")]
pub fn workspace_remove(state: State<'_, LocalState>, id: String) -> Result<(), HostError> {
    state.workspaces.remove(&id)
}

#[tauri::command(rename_all = "snake_case")]
pub fn workspace_bind_project(
    state: State<'_, LocalState>,
    id: String,
    project_id: i64,
) -> Result<Workspace, HostError> {
    state.workspaces.bind_project(&id, project_id)
}

#[allow(clippy::too_many_arguments)] // the IPC contract's argument list
#[tauri::command(rename_all = "snake_case")]
pub async fn agent_turn_start(
    state: State<'_, LocalState>,
    workspace_id: String,
    project_id: i64,
    conversation_id: Value,
    application_id: i64,
    version_id: i64,
    prompt: String,
    plan_mode: bool,
) -> Result<TurnStarted, HostError> {
    Ok(state
        .agents
        .start(TurnRequest {
            workspace_id,
            project_id,
            conversation_id,
            application_id,
            version_id,
            prompt,
            plan_mode,
        })
        .await?)
}

#[tauri::command(rename_all = "snake_case")]
pub fn agent_turn_cancel(state: State<'_, LocalState>, turn_id: String) -> Result<(), HostError> {
    if state.agents.cancel(&turn_id) {
        Ok(())
    } else {
        Err(HostError::Agent(
            "That turn is not known to this app.".into(),
        ))
    }
}

#[tauri::command(rename_all = "snake_case")]
pub fn approval_respond(
    state: State<'_, LocalState>,
    request_id: String,
    decision: String,
) -> Result<(), HostError> {
    let decision = UiDecision::parse(&decision).ok_or_else(|| {
        HostError::Agent("decision must be allow_once, allow_always or deny".into())
    })?;
    if state.agents.respond(&request_id, decision) {
        Ok(())
    } else {
        Err(HostError::Agent(
            "That question is no longer open (the turn ended or it was answered).".into(),
        ))
    }
}

#[derive(Serialize)]
pub struct TurnChanges {
    files: Vec<FileChange>,
}

#[tauri::command(rename_all = "snake_case")]
pub fn turn_changes(
    state: State<'_, LocalState>,
    turn_id: String,
) -> Result<TurnChanges, HostError> {
    Ok(TurnChanges {
        files: state.agents.changes(&turn_id)?,
    })
}

#[derive(Serialize)]
pub struct Restored {
    restored: Vec<String>,
}

#[tauri::command(rename_all = "snake_case")]
pub fn checkpoint_restore(
    state: State<'_, LocalState>,
    turn_id: String,
    path: Option<String>,
) -> Result<Restored, HostError> {
    Ok(Restored {
        restored: state.agents.restore(&turn_id, path.as_deref())?,
    })
}
