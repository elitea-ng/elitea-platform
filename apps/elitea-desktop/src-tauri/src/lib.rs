//! Elitea desktop host (ADR-0029 decision 9).
//!
//! A Tauri 2 shell around the elitea-web `desktop` build, which it loads from
//! its BUNDLED assets. This crate owns what a webview must not: the OS
//! keychain, the loopback sign-in listener, the token endpoint. Remote
//! content is never loaded into the privileged webview (see `window.rs`).

mod auth;
mod commands;
mod discovery;
mod error;
mod loopback;
mod pkce;
mod settings;
mod store;
mod tokens;
mod window;

#[cfg(test)]
mod testutil;

use std::sync::Arc;

use tauri::Manager as _;
use tauri_plugin_opener::OpenerExt as _;

use crate::auth::{AuthConfig, AuthService, BrowserOpener};
use crate::commands::AppState;
use crate::error::HostError;
use crate::settings::SettingsFiles;
use crate::store::KeyringStore;
use crate::tokens::TokenEndpoint;

/// Keychain item identity. The service matches the bundle identifier.
const KEYCHAIN_SERVICE: &str = "ai.elitea.desktop";
const KEYCHAIN_ACCOUNT: &str = "device-session";

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
    let result = tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_http::init())
        .setup(|app| {
            let config_dir = app.path().app_config_dir()?;
            let tokens = TokenEndpoint::new(env!("CARGO_PKG_VERSION"))?;
            let auth = AuthService::new(AuthConfig {
                store: Arc::new(KeyringStore::new(KEYCHAIN_SERVICE, KEYCHAIN_ACCOUNT)?),
                files: SettingsFiles::new(config_dir),
                tokens,
                opener: Arc::new(SystemBrowser(app.handle().clone())),
                build_client_id: BUILD_CLIENT_ID,
                runtime_client_id: std::env::var("ELITEA_DESKTOP_CLIENT_ID").ok(),
            });
            app.manage(AppState {
                auth: Arc::new(auth),
            });
            window::create_main_window(app.handle())?;
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            commands::host_state,
            commands::host_connect,
            commands::host_sign_in,
            commands::host_access_token,
            commands::host_refresh,
            commands::host_sign_out,
            commands::host_wipe,
        ])
        .run(tauri::generate_context!());
    if let Err(error) = result {
        eprintln!("elitea-desktop failed to start: {error}");
        std::process::exit(1);
    }
}
