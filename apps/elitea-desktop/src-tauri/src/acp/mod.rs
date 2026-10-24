//! ACP agent mode (#1241): `elitea-desktop --acp` speaks the Agent Client
//! Protocol v1 on stdio, so an editor (Zed, JetBrains, …) drives the local
//! Elitea agent. The editor owns the UI and asks the person; everything
//! else is the desktop's own: the same agent host (`d0::turn::AgentHost`),
//! tools, sandbox, deny list, approval rules, checkpoints and platform
//! calls as a workspace turn in the app.
//!
//! * [`types`] — the ACP messages, written by hand from the schema (the
//!   official crates turn on serde_json `preserve_order`);
//! * [`rpc`] — JSON-RPC 2.0 framing: one bounded line per message;
//! * [`agent`] — sessions, prompts and permission requests.
//!
//! Headless: no window, no webview, no Tauri event loop. It starts only
//! with a signed-in device session (`credentials.json`, read like the app
//! reads it, refreshed on the existing path) and a cached policy whose
//! `local_work.allowed` is on. stdout is the protocol: logs go to their own
//! file in the app's log directory (`elitea-acp.log`), and a start refusal
//! to stderr.

pub mod agent;
pub mod rpc;
pub mod types;

#[cfg(test)]
mod tests;

use std::path::PathBuf;
use std::sync::Arc;

use crate::auth::{AuthConfig, AuthService, BrowserOpener};
use crate::credentials_file::CredentialsFile;
use crate::d0::api::PlatformApi;
use crate::d0::remote_tools::RetryPolicy;
use crate::d0::turn::{AgentHost, HostDeps, PolicySource};
use crate::error::HostError;
use crate::history::HistoryStore;
use crate::index::IndexRegistry;
use crate::local_commands::{AuthCredentials, StoredPolicy};
use crate::sessions::DesktopStateStore;
use crate::settings::SettingsFiles;
use crate::tokens::TokenEndpoint;
use crate::workspaces::WorkspaceStore;

/// The app's bundle identifier (`tauri.conf.json`): the directories below
/// are named after it, as Tauri names them.
pub const APP_IDENTIFIER: &str = "ai.elitea.desktop";

/// The app's directories, resolved the way Tauri 2's path resolver does
/// (`app_config_dir`, `app_data_dir`, `app_log_dir`, `app_cache_dir`).
#[derive(Clone, Debug)]
pub struct AppDirs {
    pub config: PathBuf,
    pub data: PathBuf,
    pub log: Option<PathBuf>,
    pub cache: Option<PathBuf>,
}

impl AppDirs {
    /// # Errors
    ///
    /// The OS names no config or data directory (no home).
    pub fn resolve() -> Result<Self, String> {
        let config = dirs::config_dir()
            .ok_or("no config directory")?
            .join(APP_IDENTIFIER);
        let data = dirs::data_dir()
            .ok_or("no data directory")?
            .join(APP_IDENTIFIER);
        #[cfg(target_os = "macos")]
        let log = dirs::home_dir().map(|home| home.join("Library/Logs").join(APP_IDENTIFIER));
        #[cfg(not(target_os = "macos"))]
        let log = dirs::data_local_dir().map(|dir| dir.join(APP_IDENTIFIER).join("logs"));
        let cache = dirs::cache_dir().map(|dir| dir.join(APP_IDENTIFIER));
        Ok(Self {
            config,
            data,
            log,
            cache,
        })
    }
}

/// No browser in ACP mode: sign-in happens in the app.
struct NoBrowser;

impl BrowserOpener for NoBrowser {
    fn open(&self, _url: &str) -> Result<(), HostError> {
        Err(HostError::Browser)
    }
}

/// Run the ACP agent until the client closes stdin. The process's exit
/// code: 0, or 1 when it could not start.
#[must_use]
pub fn run() -> i32 {
    let runtime = match tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
    {
        Ok(runtime) => runtime,
        Err(error) => {
            eprintln!("elitea-desktop --acp: no async runtime: {error}");
            return 1;
        }
    };
    // Library code that spawns on Tauri's runtime gets this one.
    tauri::async_runtime::set(runtime.handle().clone());
    let dirs = match AppDirs::resolve() {
        Ok(dirs) => dirs,
        Err(error) => {
            eprintln!("elitea-desktop --acp: {error}");
            return 1;
        }
    };
    crate::logging::init_acp(dirs.log.as_deref());
    log::info!("Elitea {} starting in ACP mode", env!("CARGO_PKG_VERSION"));
    match runtime.block_on(start(dirs)) {
        Ok(()) => {
            log::info!("ACP client disconnected");
            0
        }
        Err(error) => {
            log::error!("ACP mode did not start: {error}");
            eprintln!("elitea-desktop --acp: {error}");
            1
        }
    }
}

async fn start(dirs: AppDirs) -> Result<(), String> {
    let defaults = agent::Defaults::from_env(|name| std::env::var(name).ok())?;
    let version = env!("CARGO_PKG_VERSION");
    let app_dirs = crate::d0::turn::app_dirs_for_sandbox([
        Some(dirs.config.clone()),
        Some(dirs.data.clone()),
        dirs.log.clone(),
        dirs.cache.clone(),
    ]);
    let http = crate::net::SharedHttp::new(version);
    http.prewarm();
    // Read again at every use: the app may rotate the refresh token while
    // this process runs, and a stale copy would spend a used one.
    let credentials = CredentialsFile::rereading(dirs.config.clone());
    let auth = Arc::new(AuthService::new(AuthConfig {
        store: Arc::new(credentials.slot(crate::SESSION_SLOT)),
        pending_revokes: Arc::new(credentials.slot(crate::PENDING_REVOKE_SLOT)),
        files: SettingsFiles::new(dirs.config.clone()),
        tokens: TokenEndpoint::shared(http.clone(), version),
        opener: Arc::new(NoBrowser),
        build_client_id: crate::BUILD_CLIENT_ID,
        runtime_client_id: std::env::var("ELITEA_DESKTOP_CLIENT_ID").ok(),
    }));
    let state = auth.state().map_err(|error| error.to_string())?;
    if !state.signed_in {
        return Err("not signed in: open the Elitea app and sign in first".to_owned());
    }
    let policy = Arc::new(StoredPolicy(SettingsFiles::new(dirs.config.clone())));
    let allowed = policy
        .local_work()
        .and_then(|section| {
            serde_json::from_value::<elitea_local_tools::policy::LocalWorkPolicy>(section).ok()
        })
        .is_some_and(|local| local.allowed);
    if !allowed {
        return Err("local work is turned off by your organisation's policy".to_owned());
    }
    let history = HistoryStore::open(&dirs.data)
        .map_err(|error| log::warn!("running without the thread history: {error}"))
        .ok()
        .map(Arc::new);
    let sessions = DesktopStateStore::open(&dirs.data)
        .map_err(|error| log::warn!("running without the session store: {error}"))
        .ok()
        .map(Arc::new);
    let workspaces = Arc::new(WorkspaceStore::new(dirs.data.clone()));
    let host_deny = Arc::new(elitea_local_tools::deny::DenyList::for_session(
        std::env::var_os("HOME")
            .filter(|home| !home.is_empty())
            .map(PathBuf::from)
            .as_deref(),
        Some(APP_IDENTIFIER),
        true,
        app_dirs.iter().cloned(),
    ));
    let index = IndexRegistry::new(
        workspaces.clone(),
        policy.clone(),
        Arc::new(elitea_local_index::service::NoEvents),
    )
    .with_deny_list(host_deny)
    .shared();
    let router = Arc::new(agent::EventRouter::default());
    let credentials: Arc<dyn crate::d0::api::Credentials> = Arc::new(AuthCredentials(auth.clone()));
    let agents = Arc::new(
        AgentHost::new(HostDeps {
            http: http.clone(),
            credentials: credentials.clone(),
            client_version: version.to_owned(),
            policy,
            workspaces: workspaces.clone(),
            emitter: router.clone(),
            retry: RetryPolicy::default(),
            history: history.clone(),
            sessions,
            index: Some(index),
            sandbox_deny: app_dirs.clone(),
            app_id: Some(APP_IDENTIFIER.to_owned()),
            user_rules_dir: Some(dirs.config.clone()),
            user_commands_dir: Some(dirs.config.join("commands")),
        })
        .map_err(|error| error.message)?,
    );
    let api = Arc::new(PlatformApi::shared(http, credentials, version));
    agent::serve(
        agent::AcpConfig {
            agents: agents.clone(),
            workspaces,
            api,
            router,
            defaults,
            app_dirs,
            client_version: version.to_owned(),
        },
        tokio::io::stdin(),
        tokio::io::stdout(),
    )
    .await;
    // As the app's exit does: the history's writer commits what it holds,
    // and the agent's background processes end with the process.
    let _ = tokio::task::spawn_blocking(move || {
        if let Some(history) = &history {
            history.flush();
        }
        agents.stop_all_processes_blocking(elitea_local_tools::process::TERM_GRACE);
    })
    .await;
    Ok(())
}
