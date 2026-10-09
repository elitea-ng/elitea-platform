//! Elitea desktop host (ADR-0029 decision 9).
//!
//! A Tauri 2 shell around the elitea-web `desktop` build, which it loads from
//! its BUNDLED assets. This crate owns what a webview must not: the OS
//! stored sign-in (an owner-only credentials file), the loopback sign-in listener, the token endpoint. Remote
//! content is never loaded into the privileged webview (see `window.rs`).

mod app_events;
mod attention;
mod auth;
mod commands;
mod credentials_file;
mod d0;
mod discovery;
mod error;
mod history;
mod http_scope;
mod local_commands;
mod logging;
mod loopback;
mod menu;
mod pkce;
mod platform;
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

use crate::attention::Attention;
use crate::auth::{AuthConfig, AuthService, BrowserOpener};
use crate::commands::AppState;
use crate::credentials_file::CredentialsFile;
use crate::d0::remote_tools::RetryPolicy;
use crate::d0::turn::{AgentHost, HostDeps};
use crate::error::HostError;
use crate::history::HistoryStore;
use crate::local_commands::{AuthCredentials, LocalState, MainWindowEvents, StoredPolicy};
use crate::settings::SettingsFiles;
use crate::tokens::TokenEndpoint;
use crate::workspaces::WorkspaceStore;

/// The credentials file's slots (src/credentials_file.rs).
const SESSION_SLOT: &str = "device-session";
/// Sign-outs whose server revoke did not get through, retried at launch.
const PENDING_REVOKE_SLOT: &str = "pending-revoke";

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
    // Logging first, so every later plugin and the setup below can report.
    let builder = tauri::Builder::default().plugin(logging::plugin());
    // First, so a second launch exits before it can touch the credentials file: two
    // processes would race the rotating refresh token.
    #[cfg(any(target_os = "macos", target_os = "windows", target_os = "linux"))]
    let builder = builder
        .plugin(tauri_plugin_single_instance::init(|app, _args, _cwd| {
            app_events::show_main(app);
        }))
        .plugin(
            tauri_plugin_window_state::Builder::new()
                .with_state_flags(window::persisted_state())
                .build(),
        );
    let built = builder
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_http::init())
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_notification::init())
        .manage(menu::Zoom::default())
        .manage(app_events::PendingOpens::default())
        .manage(app_events::CommandQueue::default())
        // A (re)loading page is not listening to app://command yet: hold
        // commands until it calls `app_ready`.
        .on_page_load(|webview, payload| {
            if webview.label() == "main"
                && payload.event() == tauri::webview::PageLoadEvent::Started
                && let Some(queue) = webview.try_state::<app_events::CommandQueue>()
            {
                queue.not_ready();
            }
        })
        .menu(menu::build)
        .on_menu_event(|app, event| menu::on_event(app, &event))
        .setup(|app| {
            log::info!(
                "Elitea {} starting (logs: {})",
                env!("CARGO_PKG_VERSION"),
                app.path()
                    .app_log_dir()
                    .map(|d| d.display().to_string())
                    .unwrap_or_default()
            );
            let config_dir = app.path().app_config_dir()?;
            let data_dir = app.path().app_data_dir()?;
            let tokens = TokenEndpoint::new(env!("CARGO_PKG_VERSION"))?;
            // Read once, on first use, then served from memory. Deliberately NO
            // migration from the old keychain item (`ai.elitea.desktop` /
            // `device-session`): touching the keychain is what prompted for the
            // login password on every build (owner decision). People sign in once
            // more; the old device session is revoked from Settings > Devices on
            // the web, or idles out server-side (README, "Upgrading from a
            // keychain build").
            let credentials = CredentialsFile::new(config_dir.clone());
            let auth = AuthService::new(AuthConfig {
                store: Arc::new(credentials.slot(SESSION_SLOT)),
                pending_revokes: Arc::new(credentials.slot(PENDING_REVOKE_SLOT)),
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
            // The thread history; the app runs without it when it is refused.
            let history = match HistoryStore::open(&data_dir) {
                Ok(store) => {
                    log::info!("thread history: {}", store.path().display());
                    Some(Arc::new(store))
                }
                Err(error) => {
                    log::warn!("running without the thread history: {error}");
                    None
                }
            };
            let workspaces = Arc::new(WorkspaceStore::new(data_dir));
            let agents = AgentHost::new(HostDeps {
                credentials: Arc::new(AuthCredentials(auth.clone())),
                client_version: env!("CARGO_PKG_VERSION").to_owned(),
                policy: Arc::new(StoredPolicy(SettingsFiles::new(config_dir))),
                workspaces: workspaces.clone(),
                emitter: Arc::new(MainWindowEvents(app.handle().clone())),
                retry: RetryPolicy::default(),
                history: history.clone(),
            })
            .map_err(|error| error.message)?;
            app.manage(LocalState {
                workspaces,
                history,
                agents: Arc::new(agents),
            });
            app.manage(AppState { auth });
            app.manage(Arc::new(Attention::new(app.handle().clone())));
            #[cfg(target_os = "macos")]
            window::quiet_input_source_indicator();
            window::create_main_window(app.handle())?;
            app_events::drain_pending(app.handle());
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
            local_commands::workspace_files,
            local_commands::agent_turn_start,
            local_commands::agent_turn_cancel,
            local_commands::agent_turn_status,
            local_commands::approval_respond,
            local_commands::turn_changes,
            local_commands::thread_history,
            local_commands::thread_history_delete,
            local_commands::checkpoint_restore,
            local_commands::reveal_path,
            local_commands::open_path,
            platform::app_platform,
            app_events::app_ready,
        ])
        .build(tauri::generate_context!());
    let app = match built {
        Ok(app) => app,
        Err(error) => {
            log::error!("failed to start: {error}");
            eprintln!("elitea-desktop failed to start: {error}");
            std::process::exit(1);
        }
    };
    app.run(on_run_event);
}

#[allow(clippy::needless_pass_by_value)] // the signature `App::run` takes
fn on_run_event(app: &tauri::AppHandle, event: tauri::RunEvent) {
    // The history writer commits what it still holds before the process ends.
    if matches!(event, tauri::RunEvent::Exit)
        && let Some(local) = app.try_state::<LocalState>()
        && let Some(history) = &local.history
    {
        history.flush();
    }
    #[cfg(target_os = "macos")]
    match event {
        // A folder dropped on the dock icon (or opened with the app): the
        // same path as a drop on the window. Info.plist declares folders.
        tauri::RunEvent::Opened { urls } => {
            app_events::open_paths(app, &app_events::file_urls_to_paths(&urls));
        }
        // The dock icon clicked with the window closed (hidden).
        tauri::RunEvent::Reopen {
            has_visible_windows: false,
            ..
        } => app_events::show_main(app),
        _ => {}
    }
    // Linux and Windows: a drop on the window is the only native open.
    #[cfg(not(target_os = "macos"))]
    let _ = (app, event);
}
