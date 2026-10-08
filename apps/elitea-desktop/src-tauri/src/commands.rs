//! The IPC surface: seven commands, nothing else.
//!
//! Each one is named in `build.rs` (so Tauri generates an `allow-*` permission
//! for it) and in `capabilities/default.json` (so only the bundled window may
//! call it). The webview can learn the connection state, start the flows, and
//! read a short-lived access token. It can never read the refresh token: no
//! command returns it.

use std::sync::Arc;

use tauri::State;

use crate::auth::{AccessToken, AuthService, DeploymentInfo, HostState, RefreshResult};
use crate::error::HostError;

pub struct AppState {
    pub auth: Arc<AuthService>,
}

#[tauri::command]
pub fn host_state(state: State<'_, AppState>) -> Result<HostState, HostError> {
    state.auth.state()
}

/// Step one of connecting: validate the address and show whose deployment it is.
#[tauri::command]
pub async fn host_connect(
    state: State<'_, AppState>,
    url: String,
) -> Result<DeploymentInfo, HostError> {
    state.auth.connect(&url).await
}

/// Step two: sign in through the system browser. Resolves when the flow ends.
#[tauri::command]
pub async fn host_sign_in(state: State<'_, AppState>) -> Result<HostState, HostError> {
    state.auth.sign_in().await
}

/// A usable access token, or `null` when there is no session.
#[tauri::command]
pub async fn host_access_token(
    state: State<'_, AppState>,
) -> Result<Option<AccessToken>, HostError> {
    state.auth.access_token().await
}

/// The webview saw a 401: exchange the refresh token now.
#[tauri::command]
pub async fn host_refresh(state: State<'_, AppState>) -> Result<RefreshResult, HostError> {
    state.auth.refresh().await
}

/// Revoke the device session on the server, then forget it.
#[tauri::command]
pub async fn host_sign_out(state: State<'_, AppState>) -> Result<(), HostError> {
    state.auth.sign_out().await
}

/// Forget the session and local data without contacting the server
/// (`device_revoked`, or a refresh token the server no longer honours).
#[tauri::command]
pub async fn host_wipe(state: State<'_, AppState>) -> Result<(), HostError> {
    state.auth.wipe().await
}
