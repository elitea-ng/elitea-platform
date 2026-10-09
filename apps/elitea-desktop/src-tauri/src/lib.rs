//! Elitea desktop host (ADR-0029 decision 9).
//!
//! A Tauri 2 shell around the elitea-web `desktop` build, which it loads from
//! its BUNDLED assets. This crate owns what a webview must not: the OS
//! keychain, the loopback sign-in listener, the token endpoint. Remote
//! content is never loaded into the privileged webview (see `window.rs`).

mod auth;
mod commands;
mod d0;
mod discovery;
mod error;
mod http_scope;
mod local_commands;
mod loopback;
mod pkce;
mod settings;
mod store;
mod tokens;
mod window;
mod workspaces;

#[cfg(test)]
mod testutil;

#[cfg(test)]
mod config_tests;

use std::sync::Arc;

use tauri::Manager as _;
use tauri_plugin_opener::OpenerExt as _;

use crate::auth::{AuthConfig, AuthService, BrowserOpener};
use crate::commands::AppState;
use crate::d0::remote_tools::RetryPolicy;
use crate::d0::turn::{AgentHost, HostDeps};
use crate::error::HostError;
use crate::local_commands::{AuthCredentials, LocalState, MainWindowEvents, StoredPolicy};
use crate::settings::SettingsFiles;
use crate::store::KeyringStore;
use crate::tokens::TokenEndpoint;
use crate::workspaces::WorkspaceStore;

/// Keychain item identity. The service matches the bundle identifier.
const KEYCHAIN_SERVICE: &str = "ai.elitea.desktop";
const KEYCHAIN_ACCOUNT: &str = "device-session";
/// Sign-outs whose server revoke did not get through, retried at launch.
const KEYCHAIN_PENDING_REVOKE_ACCOUNT: &str = "pending-revoke";

/// Baked in at build time when a deployment's own build registers a different
/// client id (`ELITEA_DESKTOP_CLIENT_ID=... tauri build`); see the README.
const BUILD_CLIENT_ID: Option<&str> = option_env!("ELITEA_DESKTOP_CLIENT_ID");

struct SystemBrowser(tauri::AppHandle);

impl BrowserOpener for SystemBrowser {
    fn open(&self, url: &str) -> Result<(), HostError> {
        // The sign-in URL is always https (or loopback http for a local stack)
        // by the time it gets here: discovery validated it against the origin.
        self.0
            .opener()
            .open_url(url, None::<&str>)
            .map_err(|_| HostError::Browser)
    }
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    let builder = tauri::Builder::default();
    // First, so a second launch exits before it can touch the keychain: two
    // processes would race the rotating refresh token.
    #[cfg(any(target_os = "macos", target_os = "windows", target_os = "linux"))]
    let builder = builder.plugin(tauri_plugin_single_instance::init(|app, _args, _cwd| {
        if let Some(window) = app.get_webview_window("main") {
            let _ = window.unminimize();
            let _ = window.show();
            let _ = window.set_focus();
        }
    }));
    let result = builder
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_http::init())
        .plugin(tauri_plugin_dialog::init())
        .setup(|app| {
            let config_dir = app.path().app_config_dir()?;
            let data_dir = app.path().app_data_dir()?;
            let tokens = TokenEndpoint::new(env!("CARGO_PKG_VERSION"))?;
            let auth = AuthService::new(AuthConfig {
                store: Arc::new(KeyringStore::new(KEYCHAIN_SERVICE, KEYCHAIN_ACCOUNT)?),
                pending_revokes: Arc::new(KeyringStore::new(
                    KEYCHAIN_SERVICE,
                    KEYCHAIN_PENDING_REVOKE_ACCOUNT,
                )?),
                files: SettingsFiles::new(config_dir.clone()),
                tokens,
                opener: Arc::new(SystemBrowser(app.handle().clone())),
                build_client_id: BUILD_CLIENT_ID,
                runtime_client_id: std::env::var("ELITEA_DESKTOP_CLIENT_ID").ok(),
            });
            let auth = Arc::new(auth);
            {
                let auth = auth.clone();
                tauri::async_runtime::spawn(async move {
                    auth.retry_pending_revokes().await;
                });
            }
            let stored_origin = auth.state().ok().and_then(|s| s.origin);
            http_scope::grant_stored(app.handle(), stored_origin.as_deref());
            // Local work (ADR-0029 D0): workspaces and the local agent turn.
            let workspaces = Arc::new(WorkspaceStore::new(data_dir));
            let agents = AgentHost::new(HostDeps {
                credentials: Arc::new(AuthCredentials(auth.clone())),
                client_version: env!("CARGO_PKG_VERSION").to_owned(),
                policy: Arc::new(StoredPolicy(SettingsFiles::new(config_dir))),
                workspaces: workspaces.clone(),
                emitter: Arc::new(MainWindowEvents(app.handle().clone())),
                retry: RetryPolicy::default(),
            })
            .map_err(|error| error.message)?;
            app.manage(LocalState {
                workspaces,
                agents: Arc::new(agents),
            });
            app.manage(AppState { auth });
            window::create_main_window(app.handle())?;
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            commands::host_state,
            commands::host_connect,
            commands::host_sign_in,
            commands::host_sign_in_cancel,
            commands::host_access_token,
            commands::host_refresh,
            commands::host_sign_out,
            commands::host_wipe,
            local_commands::workspace_open,
            local_commands::workspace_list,
            local_commands::workspace_remove,
            local_commands::workspace_bind_project,
            local_commands::agent_turn_start,
            local_commands::agent_turn_cancel,
            local_commands::approval_respond,
            local_commands::turn_changes,
            local_commands::checkpoint_restore,
        ])
        .run(tauri::generate_context!());
    if let Err(error) = result {
        eprintln!("elitea-desktop failed to start: {error}");
        std::process::exit(1);
    }
}
