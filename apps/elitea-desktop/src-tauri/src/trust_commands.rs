//! IPC for trusted folders (ADR-0061 decision 5i; `IPC.md`, "Trusted
//! folders"): `trust_status`, `trust_grant`, `trust_revoke`, `trust_list`.
//! Changes are announced on `trust://event`.

use tauri::State;

use crate::local_commands::{IpcError, LocalState};
use crate::trust::{Refusal, TrustEntry, TrustStatus, TrustStore};

impl From<Refusal> for IpcError {
    fn from(refusal: Refusal) -> Self {
        Self::new(refusal.code, refusal.message)
    }
}

fn store(state: &LocalState) -> Result<std::sync::Arc<TrustStore>, IpcError> {
    state
        .agents
        .trust_store()
        .cloned()
        .ok_or_else(|| IpcError::new("internal", "trusted folders are not available"))
}

/// Run `work` with the store and the policy off the async runtime (the
/// store reads and writes files).
async fn blocking<T: Send + 'static>(
    state: &LocalState,
    work: impl FnOnce(&TrustStore, &elitea_local_tools::policy::LocalWorkPolicy) -> Result<T, Refusal>
    + Send
    + 'static,
) -> Result<T, IpcError> {
    let store = store(state)?;
    let policy = state.agents.local_work_policy();
    tokio::task::spawn_blocking(move || work(&store, &policy).map_err(IpcError::from))
        .await
        .map_err(|_| IpcError::new("internal", "the trust store stopped unexpectedly"))?
}

/// `trust_status`: a folder's trust, and whether it can be granted.
#[tauri::command(rename_all = "snake_case")]
pub async fn trust_status(
    state: State<'_, LocalState>,
    workspace_id: String,
) -> Result<TrustStatus, IpcError> {
    blocking(&state, move |store, policy| {
        store.status(&workspace_id, policy)
    })
    .await
}

/// `trust_grant`: trust a folder, after the person confirmed the text of
/// `confirmation_version`.
#[tauri::command(rename_all = "snake_case")]
pub async fn trust_grant(
    state: State<'_, LocalState>,
    workspace_id: String,
    confirmation_version: u32,
    ask_every_command: bool,
) -> Result<TrustStatus, IpcError> {
    blocking(&state, move |store, policy| {
        store.grant(
            &workspace_id,
            policy,
            confirmation_version,
            ask_every_command,
        )
    })
    .await
}

/// `trust_revoke`: stop trusting a folder (its host processes stop).
#[tauri::command(rename_all = "snake_case")]
pub async fn trust_revoke(
    state: State<'_, LocalState>,
    workspace_id: String,
) -> Result<TrustStatus, IpcError> {
    blocking(&state, move |store, policy| {
        store.revoke(&workspace_id, policy)
    })
    .await
}

/// `trust_list`: the trusted and suspended folders (Settings › Trusted
/// folders).
#[tauri::command(rename_all = "snake_case")]
pub async fn trust_list(state: State<'_, LocalState>) -> Result<Vec<TrustEntry>, IpcError> {
    blocking(&state, |store, policy| store.list(policy)).await
}
