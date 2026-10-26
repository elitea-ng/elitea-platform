//! Elitea desktop host (ADR-0029 decision 9).
//!
//! A Tauri 2 shell around the elitea-web `desktop` build, which it loads from
//! its BUNDLED assets. This crate owns what a webview must not: the OS
//! stored sign-in (an owner-only credentials file), the loopback sign-in listener, the token endpoint. Remote
//! content is never loaded into the privileged webview (see `window.rs`).

mod acp;
mod activity;
mod app_events;
mod attention;
mod auth;
mod commands;
mod context_menu;
mod credentials_file;
mod d0;
mod discovery;
mod doctor;
mod documents;
mod embeddings;
mod error;
mod handoff;
mod handoff_settings;
mod history;
mod hooks;
mod hooks_commands;
mod index;
mod local_commands;
mod logging;
mod loopback;
mod lsp_commands;
mod mcp;
mod mcp_commands;
mod menu;
mod models;
mod net;
mod pkce;
mod platform;
mod process_commands;
mod rules;
mod seed;
mod sessions;
mod settings;
mod store;
mod tokens;
mod window;
mod workspaces;
mod worktree_commands;
mod worktrees;

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
use crate::index::{IndexRegistry, MainWindowIndexEvents};
use crate::local_commands::{
    AppDoctorHooks, AuthCredentials, LocalState, MainWindowEvents, StoredPolicy,
};
use crate::sessions::DesktopStateStore;
use crate::settings::SettingsFiles;
use crate::tokens::TokenEndpoint;
use crate::workspaces::WorkspaceStore;
use elitea_local_index::embedding::{EmbedConfig, EmbedStatus, Embedding};

/// The credentials file's slots (src/credentials_file.rs).
pub(crate) const SESSION_SLOT: &str = "device-session";
/// Sign-outs whose server revoke did not get through, retried at launch.
pub(crate) const PENDING_REVOKE_SLOT: &str = "pending-revoke";

/// Baked in at build time when a deployment's own build registers a different
/// client id (`ELITEA_DESKTOP_CLIENT_ID=... tauri build`); see the README.
pub(crate) const BUILD_CLIENT_ID: Option<&str> = option_env!("ELITEA_DESKTOP_CLIENT_ID");

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

/// `elitea-desktop --acp`: the local agent as an Agent Client Protocol
/// agent on stdio, without a window (src/acp). Returns the exit code.
#[must_use]
pub fn run_acp() -> i32 {
    acp::run()
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
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_notification::init())
        .manage(menu::Zoom::default())
        .manage(context_menu::Pending::default())
        .manage(app_events::PendingOpens::default())
        .manage(app_events::CommandQueue::default())
        // A (re)loading page is not listening to app://command yet: hold
        // commands until it calls `app_ready`.
        .on_page_load(|webview, payload| {
            if webview.label() == "main"
                && payload.event() == tauri::webview::PageLoadEvent::Started
            {
                if let Some(queue) = webview.try_state::<app_events::CommandQueue>() {
                    queue.not_ready();
                }
                // Nobody will read the old page's bodies or streams.
                if let Some(fetches) = webview.try_state::<net::WebFetch>() {
                    fetches.table.clear();
                }
            }
        })
        .menu(menu::build)
        .on_menu_event(|app, event| menu::on_event(app, &event))
        .setup(|app| {
            // Each app directory resolved once. Config and data are
            // required; logs and cache are optional, but one that cannot be
            // resolved is said, not dropped silently (it then cannot be
            // kept out of local commands by its resolved path).
            let log_dir = optional_app_dir("log", app.path().app_log_dir());
            let cache_dir = optional_app_dir("cache", app.path().app_cache_dir());
            log::info!(
                "Elitea {} starting (logs: {})",
                env!("CARGO_PKG_VERSION"),
                log_dir
                    .as_ref()
                    .map(|d| d.display().to_string())
                    .unwrap_or_default()
            );
            let config_dir = app.path().app_config_dir()?;
            let data_dir = app.path().app_data_dir()?;
            // What no local tool reads (commands, host git, file tools, the
            // index): every directory the app keeps data in, as resolved
            // here (XDG overrides and Windows included), and its identifier.
            let sandbox_deny = d0::turn::app_dirs_for_sandbox([
                Some(config_dir.clone()),
                Some(data_dir.clone()),
                log_dir.clone(),
                cache_dir.clone(),
            ]);
            let app_id = sandbox_app_id(app.config());
            let host_deny = Arc::new(elitea_local_tools::deny::DenyList::for_session(
                std::env::var_os("HOME")
                    .filter(|home| !home.is_empty())
                    .map(std::path::PathBuf::from)
                    .as_deref(),
                app_id.as_deref(),
                true,
                sandbox_deny.iter().cloned(),
            ));
            // The one HTTP client (src/net.rs). Its TLS roots load on a
            // background thread, so they never hold the window back.
            let http = net::SharedHttp::new(env!("CARGO_PKG_VERSION"));
            http.prewarm();
            let tokens = TokenEndpoint::shared(http.clone(), env!("CARGO_PKG_VERSION"));
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
            let stored = auth.state().ok();
            // The webview's fetch reaches the stored deployment, and no other.
            let scope = Arc::new(net::HttpScope::default());
            scope.grant_stored(stored.as_ref().and_then(|s| s.origin.as_deref()));
            app.manage(net::WebFetch {
                http: http.clone(),
                scope,
                table: net::FetchTable::default(),
            });
            // Signed in: start the launch's token refresh now, while the
            // window and the page load, instead of when the page first asks.
            // The page's own request joins it (refresh_inner re-checks the
            // cache under the gate), so this never rotates twice.
            if stored.as_ref().is_some_and(|s| s.signed_in) {
                let auth = auth.clone();
                tauri::async_runtime::spawn(async move {
                    let started = std::time::Instant::now();
                    match auth.access_token().await {
                        Ok(_) => log::debug!("launch token ready in {:?}", started.elapsed()),
                        Err(error) => log::debug!("launch token not ready: {error}"),
                    }
                });
            }
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
            // The agent's session memory; turns run without it (seeded from
            // the server every time) when the file is refused.
            let sessions = match DesktopStateStore::open(&data_dir) {
                Ok(store) => {
                    log::info!("session store: {}", store.store().path().display());
                    Some(Arc::new(store))
                }
                Err(error) => {
                    log::warn!("running without the session store: {error}");
                    None
                }
            };
            let workspaces = Arc::new(WorkspaceStore::new(data_dir.clone()));
            // A linked worktree (a take-back's, ADR-0034) is used by host
            // git only when its repository is a workspace the person opened.
            handoff::trust_open_workspaces(workspaces.clone());
            let policy = Arc::new(StoredPolicy(SettingsFiles::new(config_dir.clone())));
            let handoff_policy = Arc::new(StoredPolicy(SettingsFiles::new(config_dir.clone())));
            // The local workspace indexes (ADR-0029 decision 7), opened on
            // first use; kept across sign-out and wipe (src/index.rs).
            // Their embeddings, off unless a workspace turns them on
            // (src/embeddings.rs): one background thread, one model in memory.
            let index = IndexRegistry::new(
                workspaces.clone(),
                policy.clone(),
                Arc::new(MainWindowIndexEvents(app.handle().clone())),
            )
            .with_deny_list(host_deny);
            let remote_embeddings = match tauri::async_runtime::handle() {
                tauri::async_runtime::RuntimeHandle::Tokio(runtime) => {
                    Some(embeddings::RemoteDeps {
                        http: http.clone(),
                        credentials: Arc::new(AuthCredentials(auth.clone())),
                        workspaces: workspaces.clone(),
                        runtime,
                    })
                }
            };
            let index_events: models::Emit = {
                let handle = app.handle().clone();
                Arc::new(move |payload: serde_json::Value| {
                    use tauri::Emitter as _;
                    if let Err(error) = handle.emit_to("main", index::EVENT_NAME, &payload) {
                        log::warn!("could not deliver an index event: {error}");
                    }
                })
            };
            app.manage(Arc::new(models::ModelHost::new(
                &data_dir,
                policy.clone(),
                remote_embeddings.clone(),
                index_events.clone(),
            )));
            let index = match Embedding::start(
                Arc::new(embeddings::Embedders {
                    local: embeddings::LocalModels::new(&data_dir),
                    remote: remote_embeddings,
                    policy: policy.clone(),
                }),
                Arc::new(embeddings::Power::default()),
                EmbedConfig::default(),
                Arc::new(move |workspace_id: &str, status: &EmbedStatus| {
                    index_events(serde_json::json!({
                        "kind": "embeddings",
                        "workspace_id": workspace_id,
                        "status": status,
                    }));
                }),
            ) {
                Ok(embedding) => index.with_embedding(Arc::new(embedding)),
                Err(error) => {
                    log::warn!("the index embedder did not start: {error}");
                    index
                }
            }
            .shared();
            // An embedding pass the app quit in the middle of resumes once
            // start-up settles (only the chunks still missing a vector).
            {
                let index = index.clone();
                tauri::async_runtime::spawn(async move {
                    tokio::time::sleep(index::RESUME_EMBEDDINGS_AFTER).await;
                    let resumed = index.resume_embeddings().await;
                    if resumed > 0 {
                        log::info!("resumed embedding {resumed} workspace index(es)");
                    }
                });
            }
            let agents = Arc::new(
                AgentHost::new(HostDeps {
                    http: http.clone(),
                    credentials: Arc::new(AuthCredentials(auth.clone())),
                    client_version: env!("CARGO_PKG_VERSION").to_owned(),
                    policy,
                    workspaces: workspaces.clone(),
                    emitter: Arc::new(MainWindowEvents(app.handle().clone())),
                    retry: RetryPolicy::default(),
                    history: history.clone(),
                    sessions: sessions.clone(),
                    index: Some(index.clone()),
                    // The stored sign-in, the history, other workspaces'
                    // checkpoints and indexes, and the logs stay out of
                    // every local tool's reach.
                    sandbox_deny,
                    app_id,
                    user_rules_dir: Some(config_dir.clone()),
                    user_commands_dir: Some(config_dir.join("commands")),
                    cache_dir: cache_dir.clone(),
                })
                .map_err(|error| error.message)?,
            );
            // The desktop ⇄ cloud handoff (ADR-0034).
            let handoff = handoff::HandoffHost::new(handoff::HandoffDeps {
                agents: agents.clone(),
                http: http.clone(),
                credentials: Arc::new(AuthCredentials(auth.clone())),
                client_version: env!("CARGO_PKG_VERSION").to_owned(),
                workspaces: workspaces.clone(),
                policy: handoff_policy,
                history: history.clone(),
                data_dir: data_dir.clone(),
                events: Arc::new(handoff::MainWindowHandoffEvents(app.handle().clone())),
                device_id: {
                    let auth = auth.clone();
                    Arc::new(move || auth.device_id())
                },
                backoff: elitea_workspace_store::sync::Backoff::default(),
                git_global_config: None,
                // The env harvest offers the person's login-shell environment.
                env_source: Arc::new(handoff_settings::LoginShellEnv {
                    shell: None,
                    timeout: handoff_settings::LOGIN_SHELL_TIMEOUT,
                }),
            })
            .map_err(|error| error.message)?;
            let handoff = Arc::new(handoff);
            app.manage(handoff::HandoffState(handoff.clone()));
            let doctor_files = SettingsFiles::new(config_dir.clone());
            let history_retention_days = match doctor_files.settings() {
                Ok(settings) => crate::settings::history_retention_days(&settings),
                Err(_) => crate::settings::DEFAULT_HISTORY_RETENTION_DAYS,
            };
            app.manage(Arc::new(doctor::Doctor {
                local: doctor::LocalDoctor {
                    hooks: Arc::new(AppDoctorHooks {
                        agents: agents.clone(),
                        app: app.handle().clone(),
                    }),
                    config_dir,
                    data_dir,
                    log_dir,
                    credentials: credentials.clone(),
                    workspaces: workspaces.clone(),
                    history: history.clone(),
                    sessions: sessions.clone(),
                    cache_dir,
                    history_retention_days,
                },
                auth: auth.clone(),
            }));
            let worktrees = Arc::new(worktrees::WorktreeHost::new(
                agents.clone(),
                workspaces.clone(),
            ));
            agents.set_worktrees(&worktrees);
            worktrees.register_all();
            app.manage(worktrees);
            app.manage(LocalState {
                workspaces,
                history,
                agents,
                index,
                handoff: Some(handoff),
            });
            app.manage(AppState { auth });
            app.manage(Arc::new(Attention::new(app.handle().clone())));
            app.manage(Arc::new(activity::Activity::new(app.handle().clone())));
            // The menu-bar item: a compact state and the turns that run or wait.
            if let Err(error) = activity::create_tray(app.handle()) {
                log::warn!("no menu-bar item: {error}");
            }
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
            worktree_commands::worktree_create,
            worktree_commands::worktree_list,
            worktree_commands::worktree_diff,
            worktree_commands::worktree_remove,
            worktree_commands::worktree_merge_preflight,
            worktree_commands::worktree_merge_apply,
            worktree_commands::worktree_merge_undo,
            local_commands::workspace_bind_project,
            local_commands::workspace_agent_settings_get,
            local_commands::workspace_agent_settings_set,
            local_commands::workspace_files,
            local_commands::commands_list,
            local_commands::rules_get,
            local_commands::rules_put,
            local_commands::rules_explain,
            local_commands::rules_import_proposed,
            local_commands::agent_turn_start,
            local_commands::attachment_add,
            local_commands::attachment_add_bytes,
            local_commands::attachment_remove,
            local_commands::agent_prewarm,
            local_commands::turn_commit_retry,
            local_commands::agent_turn_cancel,
            local_commands::agent_turn_status,
            local_commands::agent_turn_steer,
            local_commands::agent_turn_steer_update,
            local_commands::approval_respond,
            local_commands::turn_changes,
            local_commands::thread_history,
            local_commands::thread_history_delete,
            local_commands::thread_history_fork,
            local_commands::session_info,
            local_commands::checkpoint_restore,
            local_commands::checkpoint_preview,
            local_commands::checkpoint_after,
            local_commands::reveal_path,
            local_commands::open_path,
            process_commands::process_list,
            process_commands::process_read,
            process_commands::process_write,
            process_commands::process_resize,
            process_commands::process_kill,
            process_commands::process_pin,
            mcp_commands::mcp_list,
            mcp_commands::mcp_save,
            mcp_commands::mcp_remove,
            mcp_commands::mcp_restart,
            mcp_commands::mcp_status,
            mcp_commands::mcp_proposal_answer,
            hooks_commands::hooks_list,
            hooks_commands::hooks_save,
            hooks_commands::hooks_set_enabled,
            hooks_commands::hooks_proposal_answer,
            lsp_commands::lsp_status,
            index::index_status,
            index::index_open,
            index::index_enable,
            index::index_disable,
            index::index_refresh,
            index::index_cancel,
            index::index_embeddings_get,
            models::index_embeddings_set,
            models::index_embeddings_status,
            models::index_embeddings_rebuild,
            models::index_estimate,
            models::index_embeddings_caps_set,
            models::index_models,
            models::index_model_download,
            models::index_model_cancel,
            models::index_model_import,
            models::index_model_remove,
            models::index_remote_models,
            index::index_remove,
            platform::app_platform,
            context_menu::context_menu_show,
            activity::activity_cloud_run,
            net::http_fetch,
            net::http_read_body,
            net::http_cancel,
            app_events::app_ready,
            doctor::doctor_run,
            doctor::doctor_fix,
            handoff::handoff_preview,
            handoff::handoff_send,
            handoff::handoff_status,
            handoff::handoff_reclaim,
            handoff::handoff_preflight,
            handoff::handoff_apply,
            handoff::handoff_undo,
            handoff::handoff_erase,
            handoff::handoff_cancel,
            handoff::handoff_status_many,
            handoff::handoff_settings_get,
            handoff::handoff_settings_set,
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

/// How long the app's exit waits for the local turns' commits still queued
/// or running, over every conversation.
const EXIT_COMMIT_WAIT: std::time::Duration = std::time::Duration::from_secs(10);

#[allow(clippy::needless_pass_by_value)] // the signature `App::run` takes
fn on_run_event(app: &tauri::AppHandle, event: tauri::RunEvent) {
    // The history writer commits what it still holds before the process ends.
    if matches!(event, tauri::RunEvent::Exit)
        && let Some(local) = app.try_state::<LocalState>()
    {
        if let Some(history) = &local.history {
            history.flush();
        }
        // Background processes the agent started end with the app (#1223):
        // `SIGTERM`, then `SIGKILL` to what still runs 2 s later.
        local
            .agents
            .stop_all_processes_blocking(elitea_local_tools::process::TERM_GRACE);
        // A turn's commit runs after its `done`: the exit waits (bounded)
        // for those queued or running; one still pending after that, or kept
        // after it failed, is not saved; its execution expires on the server.
        if local.agents.unsaved_commits().0 > 0 {
            let agents = local.agents.clone();
            let settled = tauri::async_runtime::block_on(async move {
                tokio::time::timeout(EXIT_COMMIT_WAIT, agents.commits_idle()).await
            });
            if settled.is_err() {
                log::warn!(
                    "the exit stopped waiting for local turn commits after {}s",
                    EXIT_COMMIT_WAIT.as_secs()
                );
            }
        }
        let (pending, failed) = local.agents.unsaved_commits();
        if pending + failed > 0 {
            log::warn!(
                "quitting with {pending} local turn commit(s) still running and {failed} failed; \
                 those turns are not in their conversations"
            );
        }
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

/// An optional app directory, or `None` with a warning saying which one
/// could not be resolved.
fn optional_app_dir(
    which: &str,
    resolved: tauri::Result<std::path::PathBuf>,
) -> Option<std::path::PathBuf> {
    resolved
        .map_err(|error| {
            log::warn!(
                "the app {which} directory cannot be resolved ({error}); local tools are kept \
                 out of it only by its default location"
            );
        })
        .ok()
}

/// The identifier local tools deny the app's default directories under:
/// the bundle identifier in the Tauri config (`tauri.conf.json`).
pub(crate) fn sandbox_app_id(config: &tauri::Config) -> Option<String> {
    let identifier = config.identifier.trim();
    (!identifier.is_empty()).then(|| identifier.to_owned())
}
