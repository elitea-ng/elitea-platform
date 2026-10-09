//! The local-work IPC surface (IPC.md): workspaces and the D0 agent turn.
//!
//! Like `commands.rs`, every command is named in `build.rs` and allowed in
//! `capabilities/default.json` for the main window only. Argument keys are
//! snake_case, exactly as IPC.md lists them. Nothing here returns a token.
//!
//! A failed command here rejects with [`IpcError`], `{code, message}`: the
//! UI branches on the machine code and shows the message.

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
use crate::d0::turn::{AgentHost, PolicySource, TurnError, TurnRequest, TurnStarted, TurnStatus};
use crate::error::HostError;
use crate::settings::SettingsFiles;
use crate::workspaces::{Workspace, WorkspaceStore};

pub struct LocalState {
    pub workspaces: Arc<WorkspaceStore>,
    pub agents: Arc<AgentHost>,
}

/// A local-work command's failure as the webview receives it.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct IpcError {
    pub code: String,
    pub message: String,
}

impl IpcError {
    fn new(code: &str, message: impl Into<String>) -> Self {
        Self {
            code: code.to_owned(),
            message: message.into(),
        }
    }
}

impl From<TurnError> for IpcError {
    fn from(error: TurnError) -> Self {
        Self {
            code: error.code,
            message: error.message,
        }
    }
}

impl From<HostError> for IpcError {
    fn from(error: HostError) -> Self {
        let code = match error {
            HostError::Storage(_) => "storage",
            _ => "internal",
        };
        Self::new(code, error.to_string())
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
) -> Result<Option<Workspace>, IpcError> {
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
        .map_err(|_| IpcError::new("storage", "the dialog did not return a local folder"))?;
    Ok(state.workspaces.add(&path).map(Some)?)
}

#[tauri::command(rename_all = "snake_case")]
pub fn workspace_list(state: State<'_, LocalState>) -> Result<Vec<Workspace>, IpcError> {
    Ok(state.workspaces.list()?)
}

/// Refused (`workspace_busy`) while a turn runs in the workspace; also
/// drops the agent host's session and turns of it.
#[tauri::command(rename_all = "snake_case")]
pub fn workspace_remove(state: State<'_, LocalState>, id: String) -> Result<(), IpcError> {
    Ok(state.agents.remove_workspace(&id)?)
}

/// Refused (`workspace_busy`) while a turn runs in the workspace.
#[tauri::command(rename_all = "snake_case")]
pub fn workspace_bind_project(
    state: State<'_, LocalState>,
    id: String,
    project_id: i64,
) -> Result<Workspace, IpcError> {
    Ok(state.agents.bind_project(&id, project_id)?)
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
) -> Result<TurnStarted, IpcError> {
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
pub fn agent_turn_cancel(state: State<'_, LocalState>, turn_id: String) -> Result<(), IpcError> {
    Ok(state.agents.cancel(&turn_id)?)
}

/// Where a turn is (`running`, `committing`, `done` with the `done`
/// payload): the UI re-syncs with it when it may have missed an event.
#[tauri::command(rename_all = "snake_case")]
pub fn agent_turn_status(
    state: State<'_, LocalState>,
    turn_id: String,
) -> Result<TurnStatus, IpcError> {
    Ok(state.agents.status(&turn_id)?)
}

#[tauri::command(rename_all = "snake_case")]
pub fn approval_respond(
    state: State<'_, LocalState>,
    request_id: String,
    decision: String,
) -> Result<(), IpcError> {
    let decision = UiDecision::parse(&decision).ok_or_else(|| {
        IpcError::new(
            "invalid_request",
            "decision must be allow_once, allow_always or deny",
        )
    })?;
    if state.agents.respond(&request_id, decision) {
        Ok(())
    } else {
        Err(IpcError::new(
            "approval_closed",
            "That question is no longer open (the turn ended or it was answered).",
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
) -> Result<TurnChanges, IpcError> {
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
) -> Result<Restored, IpcError> {
    Ok(Restored {
        restored: state.agents.restore(&turn_id, path.as_deref())?,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_refusal_reaches_the_webview_with_its_code() {
        let error: IpcError = TurnError {
            code: "workspace_busy".into(),
            message: "A turn is already running in this workspace.".into(),
        }
        .into();
        assert_eq!(
            serde_json::to_value(&error).unwrap(),
            serde_json::json!({
                "code": "workspace_busy",
                "message": "A turn is already running in this workspace."
            })
        );
        let storage: IpcError = HostError::Storage("disk full".into()).into();
        assert_eq!(storage.code, "storage");
        assert!(storage.message.contains("disk full"));
    }
}
