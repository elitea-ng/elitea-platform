//! The IPC surface: seven commands, nothing else.
//!
//! Each one is named in `build.rs` (so Tauri generates an `allow-*` permission
//! for it) and in `capabilities/default.json` (so only the bundled window may
//! call it). The webview can learn the connection state, start the flows, and
//! read a short-lived access token. It can never read the refresh token: no
//! command returns it.

use std::sync::Arc;

use tauri::{AppHandle, State, WebviewWindow};

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
    app: AppHandle,
    state: State<'_, AppState>,
    url: String,
) -> Result<DeploymentInfo, HostError> {
    let info = state.auth.connect(&url).await?;
    // From here the webview may reach this deployment through the HTTP plugin, and no other.
    crate::http_scope::grant(&app, &info.origin).map_err(HostError::Internal)?;
    Ok(info)
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
pub async fn host_sign_out(
    window: WebviewWindow,
    state: State<'_, AppState>,
) -> Result<(), HostError> {
    let result = state.auth.sign_out().await;
    clear_webview_data(&window);
    result
}

/// Forget the session and local data without contacting the server
/// (`device_revoked`, or a refresh token the server no longer honours).
#[tauri::command]
pub async fn host_wipe(window: WebviewWindow, state: State<'_, AppState>) -> Result<(), HostError> {
    let result = state.auth.wipe().await;
    clear_webview_data(&window);
    result
}

/// Drop everything the webview stored (localStorage, sessionStorage, IndexedDB,
/// caches, cookies). Done here, not left to the page: the host is the
/// authority on what a wiped install holds, and the page may already be gone.
/// The page runs its own logout sweep first as well; this is the backstop.
fn clear_webview_data(window: &WebviewWindow) {
    if let Err(error) = window.clear_all_browsing_data() {
        eprintln!("elitea-desktop: could not clear the webview's data: {error}");
    }
}
