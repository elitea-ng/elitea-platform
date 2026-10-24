//! The local-work IPC surface (IPC.md): workspaces and the D0 agent turn.
//!
//! Like `commands.rs`, every command is named in `build.rs` and allowed in
//! `capabilities/default.json` for the main window only. Argument keys are
//! snake_case, exactly as IPC.md lists them. Nothing here returns a token.
//!
//! A failed command here rejects with [`IpcError`], `{code, message}`: the
//! UI branches on the machine code and shows the message.

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use async_trait::async_trait;
use serde::Serialize;
use serde_json::Value;
use tauri::{AppHandle, Emitter as _, Manager as _, State};

use crate::attention::Attention;
use crate::d0::mentions::Located;

use crate::auth::{AuthService, RefreshResult};
use crate::d0::api::{ApiError, Bearer, Credentials};
use crate::d0::approvals::UiDecision;
use crate::d0::attachments::{Added, ImagePick};
use crate::d0::definition::ModelChoice;
use crate::d0::events::{AgentEvent, EVENT_NAME, EventEmitter};
use crate::d0::recorder::FileChange;
use crate::d0::skills::SkillPick;
use crate::d0::slash_commands::{CommandPick, ListedCommand};
use crate::d0::turn::{
    AgentHost, PolicySource, PrewarmRequest, Prewarmed, RestorePreview, TurnError, TurnRequest,
    TurnStarted, TurnStatus,
};
use crate::error::HostError;
use crate::history::StoredTurn;
use crate::settings::SettingsFiles;
use crate::workspaces::{AgentSettings, STEP_LIMIT_RANGE, Workspace, WorkspaceStore};
use elitea_local_tools::find::FoundPath;

pub struct LocalState {
    pub workspaces: Arc<WorkspaceStore>,
    pub agents: Arc<AgentHost>,
    /// The workspaces' local indexes (`src/index.rs`).
    pub index: Arc<crate::index::IndexRegistry>,
    /// The thread history, flushed at exit (`None` runs without one).
    pub history: Option<Arc<crate::history::HistoryStore>>,
    /// The cloud handoff (`src/handoff.rs`): a thread in the cloud runs no
    /// local turn.
    pub handoff: Option<Arc<crate::handoff::HandoffHost>>,
}

/// The Doctor's repairs through the app's own paths: the agent host's (see
/// its `DoctorHooks`), and for a local sign-out also what `host_sign_out`
/// does to the webview — its stored data cleared, and `signed_out` sent so
/// the page drops the session it shows.
pub struct AppDoctorHooks {
    pub agents: Arc<AgentHost>,
    pub app: AppHandle,
}

impl crate::doctor::DoctorHooks for AppDoctorHooks {
    fn remove_workspace(&self, workspace_id: &str) -> Result<(), String> {
        crate::doctor::DoctorHooks::remove_workspace(self.agents.as_ref(), workspace_id)
    }

    fn signed_out(&self) {
        crate::doctor::DoctorHooks::signed_out(self.agents.as_ref());
        if let Some(window) = self.app.get_webview_window("main")
            && let Err(error) = window.clear_all_browsing_data()
        {
            log::warn!("could not clear the webview's data: {error}");
        }
        crate::app_events::emit_live(&self.app, "signed_out");
    }

    fn with_index_closed(&self, workspace_id: &str, work: &mut (dyn FnMut() + Send)) {
        crate::doctor::DoctorHooks::with_index_closed(self.agents.as_ref(), workspace_id, work);
    }

    fn rebuild_index(&self, workspace_id: &str) -> Result<(), String> {
        crate::doctor::DoctorHooks::rebuild_index(self.agents.as_ref(), workspace_id)
    }

    fn index_watching(&self, workspace_id: &str) -> bool {
        crate::doctor::DoctorHooks::index_watching(self.agents.as_ref(), workspace_id)
    }

    fn refresh_index(&self, workspace_id: &str) -> Result<(), String> {
        crate::doctor::DoctorHooks::refresh_index(self.agents.as_ref(), workspace_id)
    }

    fn git_deny(&self) -> Arc<elitea_local_tools::deny::DenyList> {
        crate::doctor::DoctorHooks::git_deny(self.agents.as_ref())
    }
}

/// A local-work command's failure as the webview receives it.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct IpcError {
    pub code: String,
    pub message: String,
}

impl IpcError {
    pub(crate) fn new(code: &str, message: impl Into<String>) -> Self {
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

    /// The connected origin and the sign-in session (`session_epoch`).
    fn identity(&self) -> Option<String> {
        let state = self.0.state().ok()?;
        if !state.signed_in {
            return None;
        }
        Some(format!(
            "{}#{}",
            state.origin?.trim_end_matches('/'),
            self.0.session_epoch()
        ))
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

/// Events to the main window only; the host's notifications and dock
/// badge watch the same stream (`attention.rs`).
pub struct MainWindowEvents(pub AppHandle);

impl EventEmitter for MainWindowEvents {
    fn emit(&self, event: AgentEvent) {
        if let Err(error) = self.0.emit_to("main", EVENT_NAME, &event) {
            log::warn!("could not deliver an agent event: {error}");
        }
        if let Some(attention) = self.0.try_state::<Arc<Attention>>() {
            attention.observe(&event);
        }
        if let Some(activity) = self.0.try_state::<Arc<crate::activity::Activity>>() {
            activity.agent(&event);
        }
    }

    fn emit_process(&self, event: crate::d0::processes::ProcessEvent) {
        if let Err(error) = self
            .0
            .emit_to("main", crate::d0::processes::EVENT_NAME, &event)
        {
            log::warn!("could not deliver a process event: {error}");
        }
    }
}

/// The native folder dialog; `None` when the person cancelled (or the
/// dialog handed back something that is not a local folder).
pub async fn pick_folder(app: &AppHandle) -> Option<PathBuf> {
    use tauri_plugin_dialog::DialogExt as _;
    let (sender, receiver) = tokio::sync::oneshot::channel();
    app.dialog().file().pick_folder(move |picked| {
        let _ = sender.send(picked);
    });
    receiver.await.ok().flatten()?.into_path().ok()
}

/// Pick a folder with the native dialog and open it as a workspace;
/// `null` when the person cancelled.
#[tauri::command(rename_all = "snake_case")]
pub async fn workspace_open(
    app: AppHandle,
    state: State<'_, LocalState>,
) -> Result<Option<Workspace>, IpcError> {
    let Some(path) = pick_folder(&app).await else {
        return Ok(None);
    };
    let workspace = state.workspaces.add(&path)?;
    // The folder's MCP files are proposals only (#1230): shown, never run.
    let agents = state.agents.clone();
    let id = workspace.id.clone();
    tauri::async_runtime::spawn_blocking(move || {
        crate::mcp_commands::propose_on_open(&app, &agents, &id);
    });
    Ok(Some(workspace))
}

/// Files whose "open with the default app" would RUN something (an app
/// bundle, a script, an installer): `open_path` refuses them, Reveal in
/// Finder still works.
const LAUNCHABLE_EXTENSIONS: &[&str] = &[
    "app",
    "command",
    "tool",
    "terminal",
    "workflow",
    "action",
    "scpt",
    "scptd",
    "applescript",
    "pkg",
    "mpkg",
    "dmg",
    "prefpane",
    "kext",
    "webloc",
    "inetloc",
    "fileloc",
    "exe",
    "com",
    "bat",
    "cmd",
    "ps1",
    "psm1",
    "vbs",
    "vbe",
    "js",
    "jse",
    "wsf",
    "wsh",
    "msi",
    "msp",
    "scr",
    "lnk",
    "url",
    "pif",
    "hta",
    "cpl",
    "reg",
    "jar",
    "desktop",
    "appimage",
    "sh",
    "bash",
    "zsh",
    "fish",
    "csh",
    "py",
    "pl",
    "rb",
    // Windows, beyond the above: PATHEXT's defaults and Outlook's blocked
    // attachment types (file types Windows runs, installs, or hands to a
    // script host or shell handler rather than showing).
    "ade",
    "adp",
    "application",
    "appref-ms",
    "appx",
    "appxbundle",
    "bas",
    "bgi",
    "cab",
    "cer",
    "chm",
    "cnt",
    "crt",
    "der",
    "diagcab",
    "fxp",
    "gadget",
    "grp",
    "hlp",
    "hpj",
    "htc",
    "inf",
    "ins",
    "isp",
    "its",
    "jnlp",
    "ksh",
    "library-ms",
    "mad",
    "maf",
    "mag",
    "mam",
    "maq",
    "mar",
    "mas",
    "mat",
    "mau",
    "mav",
    "maw",
    "mcf",
    "mda",
    "mdb",
    "mde",
    "mdt",
    "mdw",
    "mdz",
    "msc",
    "msh",
    "msh1",
    "msh2",
    "mshxml",
    "msh1xml",
    "msh2xml",
    "msix",
    "msixbundle",
    "mst",
    "msu",
    "ops",
    "osd",
    "pcd",
    "plg",
    "prf",
    "prg",
    "printerexport",
    "ps1xml",
    "ps2",
    "ps2xml",
    "psc1",
    "psc2",
    "psd1",
    "psdm1",
    "pyc",
    "pyo",
    "pyw",
    "pyz",
    "pyzw",
    "scf",
    "sct",
    "search-ms",
    "searchconnector-ms",
    "settingcontent-ms",
    "shb",
    "shs",
    "theme",
    "vb",
    "vbp",
    "vhd",
    "vhdx",
    "vsmacros",
    "vsw",
    "webpnp",
    "website",
    "ws",
    "wsb",
    "wsc",
    "xbap",
    "xll",
    "xnk",
];

/// True for a path the OS would execute rather than show.
#[must_use]
pub fn is_launchable(path: &Path, is_dir: bool) -> bool {
    let extension = path
        .extension()
        .map(|e| e.to_string_lossy().to_ascii_lowercase());
    if extension
        .as_deref()
        .is_some_and(|e| LAUNCHABLE_EXTENSIONS.contains(&e))
    {
        return true;
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        if !is_dir
            && std::fs::metadata(path).is_ok_and(|meta| meta.permissions().mode() & 0o111 != 0)
        {
            return true;
        }
    }
    let _ = is_dir;
    false
}

async fn located(
    state: &State<'_, LocalState>,
    workspace_id: String,
    path: String,
) -> Result<Located, IpcError> {
    let agents = state.agents.clone();
    tokio::task::spawn_blocking(move || agents.locate(&workspace_id, &path))
        .await
        .map_err(|_| IpcError::new("internal", "the path lookup stopped unexpectedly"))?
        .map_err(IpcError::from)
}

/// Reveal a workspace file or folder in Finder / the file manager.
/// `path` is workspace-relative (`""` is the workspace itself).
#[tauri::command(rename_all = "snake_case")]
pub async fn reveal_path(
    app: AppHandle,
    state: State<'_, LocalState>,
    workspace_id: String,
    path: String,
) -> Result<(), IpcError> {
    use tauri_plugin_opener::OpenerExt as _;
    let found = located(&state, workspace_id, path).await?;
    app.opener()
        .reveal_item_in_dir(&found.absolute)
        .map_err(|e| IpcError::new("os_refused", format!("could not reveal it: {e}")))
}

/// Open a workspace file (or folder) with its default app. Refuses
/// (`open_refused`) anything the OS would run instead of show.
#[tauri::command(rename_all = "snake_case")]
pub async fn open_path(
    app: AppHandle,
    state: State<'_, LocalState>,
    workspace_id: String,
    path: String,
) -> Result<(), IpcError> {
    use tauri_plugin_opener::OpenerExt as _;
    let found = located(&state, workspace_id, path).await?;
    if is_launchable(&found.absolute, found.is_dir) {
        return Err(IpcError::new(
            "open_refused",
            "This file would run a program, so it is not opened from here. Reveal it in the file manager instead.",
        ));
    }
    app.opener()
        .open_path(found.absolute.to_string_lossy(), None::<&str>)
        .map_err(|e| IpcError::new("os_refused", format!("could not open it: {e}")))
}

#[tauri::command(rename_all = "snake_case")]
pub fn workspace_list(state: State<'_, LocalState>) -> Result<Vec<Workspace>, IpcError> {
    Ok(state.workspaces.list()?)
}

/// Refused (`workspace_busy`) while a turn runs in the workspace; also
/// drops the agent host's session and turns of it.
#[tauri::command(rename_all = "snake_case")]
pub async fn workspace_remove(state: State<'_, LocalState>, id: String) -> Result<(), IpcError> {
    let path = state.workspaces.get(&id)?.map(|workspace| workspace.path);
    state.agents.remove_workspace(&id).await?;
    // Its linked worktrees (a take-back's) are no longer trusted by host git.
    if let Some(path) = path {
        elitea_local_tools::git::forget_worktree_parent(std::path::Path::new(&path));
    }
    Ok(())
}

/// The workspace's agent settings (`step_limit`: its turns' steps per run,
/// over the agent's own; `null` for the agent's value, else the default).
#[tauri::command(rename_all = "snake_case")]
pub fn workspace_agent_settings_get(
    state: State<'_, LocalState>,
    id: String,
) -> Result<AgentSettings, IpcError> {
    Ok(state.workspaces.agent_settings(&id)?)
}

/// Store the workspace's agent settings; they apply from the next turn.
/// `invalid_request` for a step limit outside 10 to 200.
#[tauri::command(rename_all = "snake_case")]
pub fn workspace_agent_settings_set(
    state: State<'_, LocalState>,
    id: String,
    settings: AgentSettings,
) -> Result<AgentSettings, IpcError> {
    if settings
        .step_limit
        .is_some_and(|limit| !STEP_LIMIT_RANGE.contains(&limit))
    {
        return Err(IpcError::new(
            "invalid_request",
            format!(
                "The step limit must be {} to {}.",
                STEP_LIMIT_RANGE.start(),
                STEP_LIMIT_RANGE.end()
            ),
        ));
    }
    Ok(state.workspaces.set_agent_settings(&id, settings)?)
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

/// `agent_turn_start`'s `model` (#1228): the model the person chose for
/// the turn, and its reasoning effort (`none`, `low`, `medium`, `high`).
#[derive(Clone, Debug, serde::Deserialize)]
pub struct TurnModel {
    pub name: String,
    #[serde(default)]
    pub reasoning_effort: Option<String>,
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
    mentions: Option<Vec<String>>,
    skills: Option<Vec<SkillPick>>,
    compact: Option<bool>,
    model: Option<TurnModel>,
    command: Option<CommandPick>,
    images: Option<Vec<ImagePick>>,
) -> Result<TurnStarted, IpcError> {
    // A thread in the cloud runs no local turn (ADR-0034 decision 7; the
    // platform refuses it too).
    if let (Some(handoff), Some(conversation)) = (&state.handoff, conversation_id.as_str())
        && handoff.is_away(conversation)
    {
        return Err(IpcError::new(
            "thread_executor_is_cloud",
            "This thread is running in the cloud. Take it back to continue here.",
        ));
    }
    let (skills, project_skills) = crate::d0::skills::split(skills.unwrap_or_default());
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
            mentions: mentions.unwrap_or_default(),
            skills,
            project_skills,
            compact: compact.unwrap_or(false),
            model: model
                .map(|model| {
                    ModelChoice::parse(&model.name, model.reasoning_effort.as_deref())
                        .map_err(|refusal| IpcError::new(refusal.code, refusal.message))
                })
                .transpose()?,
            command,
            images: images.unwrap_or_default(),
        })
        .await?)
}

/// Attach the image file at `path` (absolute: a dropped file's path from
/// `files_dropped`, or a picked one) to the next turn of the workspace
/// (#1229). The host reads it confined (credentials, the app's data and
/// `path_deny` are refused with `path_denied`), checks and shrinks it, and
/// keeps it in memory; the answer's token goes in `agent_turn_start.images`.
#[tauri::command(rename_all = "snake_case")]
pub async fn attachment_add(
    state: State<'_, LocalState>,
    workspace_id: String,
    path: String,
) -> Result<Added, IpcError> {
    let agents = state.agents.clone();
    tokio::task::spawn_blocking(move || agents.attachment_add(&workspace_id, &path))
        .await
        .map_err(|_| IpcError::new("internal", "the image lookup stopped unexpectedly"))?
        .map_err(IpcError::from)
}

/// Attach a pasted image (`data`: base64, at most 10 MiB decoded). `mime`
/// is what the clipboard said; the host goes by the bytes.
#[tauri::command(rename_all = "snake_case")]
pub async fn attachment_add_bytes(
    state: State<'_, LocalState>,
    workspace_id: String,
    mime: String,
    data: String,
    name: Option<String>,
) -> Result<Added, IpcError> {
    let _ = mime;
    let agents = state.agents.clone();
    tokio::task::spawn_blocking(move || {
        agents.attachment_add_bytes(&workspace_id, name.as_deref(), &data)
    })
    .await
    .map_err(|_| IpcError::new("internal", "the image lookup stopped unexpectedly"))?
    .map_err(IpcError::from)
}

/// Take an attached image off the composer before it is sent.
#[tauri::command(rename_all = "snake_case")]
pub fn attachment_remove(state: State<'_, LocalState>, workspace_id: String, token: String) {
    state.agents.attachment_remove(&workspace_id, &token);
}

/// Read ahead what a turn to this agent version (and conversation) reads
/// first, while the person is still typing: the turn then makes one serial
/// request before the model, its start. Starts nothing.
#[tauri::command(rename_all = "snake_case")]
pub async fn agent_prewarm(
    state: State<'_, LocalState>,
    project_id: i64,
    application_id: i64,
    version_id: i64,
    conversation_id: Option<Value>,
) -> Result<Prewarmed, IpcError> {
    Ok(state
        .agents
        .prewarm(PrewarmRequest {
            project_id,
            application_id,
            version_id,
            conversation_id,
        })
        .await?)
}

/// Send a turn's commit again after `commit_failed` (same execution, same
/// body, in its conversation's order); resolves once it settled.
#[tauri::command(rename_all = "snake_case")]
pub async fn turn_commit_retry(
    state: State<'_, LocalState>,
    execution_id: String,
) -> Result<(), IpcError> {
    Ok(state.agents.retry_commit(&execution_id).await?)
}

/// The "@" picker: files and folders of the workspace matching `query`
/// (`.gitignore`, `.git` and `path_deny` left out, symlinks never listed or
/// followed), best first, at most `limit` (default 50, at most 200). Walks
/// off the main thread.
#[tauri::command(rename_all = "snake_case")]
pub async fn workspace_files(
    state: State<'_, LocalState>,
    workspace_id: String,
    query: Option<String>,
    limit: Option<usize>,
) -> Result<Vec<FoundPath>, IpcError> {
    let agents = state.agents.clone();
    tokio::task::spawn_blocking(move || {
        agents.workspace_files(&workspace_id, query.as_deref().unwrap_or_default(), limit)
    })
    .await
    .map_err(|_| IpcError::new("internal", "the file lookup stopped unexpectedly"))?
    .map_err(IpcError::from)
}

/// The "/" menu's custom commands: the workspace's `.elitea/commands/` and
/// `.claude/commands/` (confined reads, `path_deny` and the session's deny
/// list honoured, symlinks never followed) and the person's own folder,
/// by name. Reads off the main thread.
#[tauri::command(rename_all = "snake_case")]
pub async fn commands_list(
    state: State<'_, LocalState>,
    workspace_id: String,
) -> Result<Vec<ListedCommand>, IpcError> {
    let agents = state.agents.clone();
    tokio::task::spawn_blocking(move || agents.commands_list(&workspace_id))
        .await
        .map_err(|_| IpcError::new("internal", "the command lookup stopped unexpectedly"))?
        .map_err(IpcError::from)
}

/// `rules_get`: the workspace's and the person's approval rules (each with
/// the error that keeps it from loading, if any), and the rules the folder
/// proposes in `.elitea/rules.json` (never loaded until imported).
#[tauri::command(rename_all = "snake_case")]
pub async fn rules_get(
    state: State<'_, LocalState>,
    workspace_id: String,
) -> Result<crate::rules::RulesView, IpcError> {
    let agents = state.agents.clone();
    tokio::task::spawn_blocking(move || {
        let (root, path_deny) = agents.rules_folder(&workspace_id)?;
        Ok(crate::rules::RulesView {
            scopes: agents.rule_files().states(&workspace_id),
            proposed: crate::rules::RuleFiles::proposed(&root, &path_deny),
        })
    })
    .await
    .map_err(|_| IpcError::new("internal", "reading the rules stopped unexpectedly"))?
}

/// `rules_put`: validate `file` and store it as the `scope`'s rules, when
/// the file is still at `version` (from `rules_get`). A file that does not
/// validate is not written: `{saved: false, error}`; one changed since:
/// `{saved: false, conflict: true}`.
#[tauri::command(rename_all = "snake_case")]
pub async fn rules_put(
    state: State<'_, LocalState>,
    workspace_id: String,
    scope: elitea_local_tools::execpolicy::RuleScope,
    file: Value,
    version: String,
) -> Result<crate::rules::PutOutcome, IpcError> {
    let agents = state.agents.clone();
    tokio::task::spawn_blocking(move || {
        agents.rules_folder(&workspace_id)?;
        Ok(crate::rules::PutOutcome::of(agents.rule_files().put(
            scope,
            &workspace_id,
            &file,
            &version,
        ))?)
    })
    .await
    .map_err(|_| IpcError::new("internal", "saving the rules stopped unexpectedly"))?
}

/// `rules_import_proposed`: copy the folder's proposed rules, as
/// `rules_get` showed them (`digest`), into the workspace's rules file
/// (the person's explicit import). A proposal changed since:
/// `{saved: false, conflict: true}`.
#[tauri::command(rename_all = "snake_case")]
pub async fn rules_import_proposed(
    state: State<'_, LocalState>,
    workspace_id: String,
    digest: String,
) -> Result<crate::rules::PutOutcome, IpcError> {
    let agents = state.agents.clone();
    tokio::task::spawn_blocking(move || {
        let (root, path_deny) = agents.rules_folder(&workspace_id)?;
        Ok(crate::rules::PutOutcome::of(
            agents
                .rule_files()
                .import_proposed(&workspace_id, &root, &path_deny, &digest),
        )?)
    })
    .await
    .map_err(|_| IpcError::new("internal", "importing the rules stopped unexpectedly"))?
}

/// `rules_explain`'s answer: the verdict, the layer that decided it, and
/// the rule (with its justification) when a rule did.
#[derive(Serialize)]
pub struct Explained {
    verdict: elitea_local_tools::approvals::Verdict,
    source: elitea_local_tools::approvals::Source,
    reason: String,
    rule: Option<elitea_local_tools::approvals::RuleMatch>,
    justification: Option<String>,
}

/// `rules_explain`: which layer and rule would decide `command` here now.
#[tauri::command(rename_all = "snake_case")]
pub async fn rules_explain(
    state: State<'_, LocalState>,
    workspace_id: String,
    command: String,
) -> Result<Explained, IpcError> {
    if command.trim().is_empty() || command.len() > 16 * 1024 {
        return Err(IpcError::new("invalid_request", "Type a command to test."));
    }
    let agents = state.agents.clone();
    let decision =
        tokio::task::spawn_blocking(move || agents.explain_command(&workspace_id, &command))
            .await
            .map_err(|_| IpcError::new("internal", "the explanation stopped unexpectedly"))??;
    Ok(Explained {
        justification: decision
            .rule
            .as_ref()
            .and_then(|rule| rule.justification.clone()),
        verdict: decision.verdict,
        source: decision.source,
        reason: decision.reason,
        rule: decision.rule,
    })
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
    app: AppHandle,
    state: State<'_, LocalState>,
    request_id: String,
    decision: String,
    feedback: Option<String>,
) -> Result<(), IpcError> {
    let decision = UiDecision::parse(&decision, feedback.as_deref()).ok_or_else(|| {
        IpcError::new(
            "invalid_request",
            "decision must be allow_once, allow_always, deny, continue, cancel, accept or decline",
        )
    })?;
    if state.agents.respond(&request_id, decision) {
        if let Some(attention) = app.try_state::<Arc<Attention>>() {
            attention.answered(&request_id);
        }
        if let Some(activity) = app.try_state::<Arc<crate::activity::Activity>>() {
            activity.answered(&request_id);
        }
        Ok(())
    } else {
        Err(IpcError::new(
            "approval_closed",
            "That question is no longer open (the turn ended, it was answered, or nobody answered it in time).",
        ))
    }
}

#[derive(Serialize)]
pub struct ThreadHistory {
    turns: Vec<StoredTurn>,
}

/// The stored turns of one thread of the signed-in account, oldest first,
/// each with its `agent://event` stream (`history.rs`). Empty when nothing
/// is stored (the thread ran elsewhere, or before this version).
#[tauri::command(rename_all = "snake_case")]
pub async fn thread_history(
    state: State<'_, LocalState>,
    workspace_id: String,
    conversation_id: String,
) -> Result<ThreadHistory, IpcError> {
    Ok(ThreadHistory {
        turns: state
            .agents
            .thread_history(&workspace_id, &conversation_id)
            .await?,
    })
}

/// The size and state of a conversation's agent session, or `null` without
/// one (`sessions.rs`): for the Doctor and the context meter.
#[tauri::command(rename_all = "snake_case")]
pub async fn session_info(
    state: State<'_, LocalState>,
    conversation_uuid: String,
) -> Result<Option<crate::sessions::SessionInfo>, IpcError> {
    Ok(state.agents.session_info(&conversation_uuid).await?)
}

#[derive(Serialize)]
pub struct HistoryDeleted {
    deleted: usize,
}

/// Forget one thread's stored turns (the signed-in account's only).
#[tauri::command(rename_all = "snake_case")]
pub async fn thread_history_delete(
    state: State<'_, LocalState>,
    workspace_id: String,
    conversation_id: String,
) -> Result<HistoryDeleted, IpcError> {
    Ok(HistoryDeleted {
        deleted: state
            .agents
            .delete_thread_history(&workspace_id, &conversation_id)
            .await?,
    })
}

#[derive(Serialize)]
pub struct HistoryForked {
    copied: usize,
}

/// Copy one thread's stored turns into the conversation a server fork
/// made (`forkConversation`), in the same workspace: up to and including
/// `up_to_turn_id`, or all of them. The signed-in account's rows only.
/// `message_ids` is the fork's `forked_messages` (source message UUID to
/// the copy's): the copied turns name the new conversation's messages.
#[tauri::command(rename_all = "snake_case")]
pub async fn thread_history_fork(
    state: State<'_, LocalState>,
    workspace_id: String,
    source_conversation_id: String,
    target_conversation_id: String,
    target_conversation_uuid: String,
    up_to_turn_id: Option<String>,
    message_ids: Option<HashMap<String, String>>,
) -> Result<HistoryForked, IpcError> {
    Ok(HistoryForked {
        copied: state
            .agents
            .fork_thread_history(
                &workspace_id,
                &source_conversation_id,
                (&target_conversation_id, &target_conversation_uuid),
                up_to_turn_id.as_deref(),
                message_ids.unwrap_or_default(),
            )
            .await?,
    })
}

#[derive(Serialize)]
pub struct TurnChanges {
    files: Vec<FileChange>,
    /// The newest turn of its workspace whose changes stand: "Undo" is
    /// offered for it; an older one offers "Restore folder to before this
    /// turn" (`checkpoint_preview`, then `confirm_older`).
    latest: bool,
    /// Its changes were undone (no undo is offered).
    undone: bool,
}

#[tauri::command(rename_all = "snake_case")]
pub fn turn_changes(
    state: State<'_, LocalState>,
    turn_id: String,
) -> Result<TurnChanges, IpcError> {
    let offer = state.agents.undo_offer(&turn_id)?;
    Ok(TurnChanges {
        files: state.agents.changes(&turn_id)?,
        latest: offer.latest,
        undone: offer.undone,
    })
}

#[derive(Serialize)]
pub struct Restored {
    restored: Vec<String>,
}

/// Undo a turn, one file of it, or (`confirm_older`) restore the folder
/// to before an older turn. Writes files: off the main thread.
#[tauri::command(rename_all = "snake_case")]
pub async fn checkpoint_restore(
    state: State<'_, LocalState>,
    turn_id: String,
    path: Option<String>,
    confirm_older: Option<bool>,
) -> Result<Restored, IpcError> {
    let agents = state.agents.clone();
    let restored = tokio::task::spawn_blocking(move || {
        agents.restore(&turn_id, path.as_deref(), confirm_older.unwrap_or(false))
    })
    .await
    .map_err(|e| IpcError::new("internal", e.to_string()))??;
    Ok(Restored { restored })
}

#[derive(Serialize)]
pub struct RestorePoint {
    turn_id: Option<String>,
}

/// The turn whose checkpoint is the folder as `turn_id` left it (the
/// first later turn of its workspace that changed files and stands), or
/// `null` when no later turn changed the folder.
#[tauri::command(rename_all = "snake_case")]
pub fn checkpoint_after(
    state: State<'_, LocalState>,
    turn_id: String,
) -> Result<RestorePoint, IpcError> {
    Ok(RestorePoint {
        turn_id: state.agents.restore_point_after(&turn_id)?,
    })
}

/// What restoring the folder to before a turn would write back and
/// delete now (later turns' changes and the person's own edits included).
#[tauri::command(rename_all = "snake_case")]
pub async fn checkpoint_preview(
    state: State<'_, LocalState>,
    turn_id: String,
) -> Result<RestorePreview, IpcError> {
    let agents = state.agents.clone();
    Ok(
        tokio::task::spawn_blocking(move || agents.restore_preview(&turn_id))
            .await
            .map_err(|e| IpcError::new("internal", e.to_string()))??,
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_refusal_reaches_the_webview_with_its_code() {
        let error: IpcError = TurnError {
            code: "workspace_busy".into(),
            message: "A turn is already running in this workspace.".into(),
            status: None,
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

    #[test]
    fn windows_launch_types_are_refused_whatever_their_case() {
        let dir = tempfile::tempdir().unwrap();
        for name in [
            "tool.pyw",
            "console.MSC",
            "app.appref-ms",
            "deploy.Application",
            "x.ws",
            "w.gadget",
            "s.SettingContent-ms",
            "l.library-ms",
            "q.search-ms",
            "c.searchConnector-ms",
            "macro.xll",
            "help.chm",
            "box.wsb",
            "pkg.msix",
            "disk.vhdx",
            "cfg.inf",
            "x.ps1xml",
            "y.psd1",
            "z.cpl",
            "a.MSI",
            "b.Bat",
        ] {
            assert!(
                is_launchable(&dir.path().join(name), false),
                "{name} would run"
            );
        }
        for name in ["notes.txt", "photo.png", "report.pdf", "data.json"] {
            assert!(!is_launchable(&dir.path().join(name), false), "{name}");
        }
    }

    #[test]
    fn programs_and_scripts_are_never_opened_with_the_default_app() {
        let dir = tempfile::tempdir().unwrap();
        let plain = dir.path().join("README.md");
        std::fs::write(&plain, "x").unwrap();
        assert!(!is_launchable(&plain, false));
        assert!(!is_launchable(dir.path(), true));
        for name in [
            "Tool.app",
            "run.command",
            "setup.EXE",
            "x.sh",
            "a.webloc",
            "i.pkg",
        ] {
            assert!(
                is_launchable(&dir.path().join(name), name.ends_with(".app")),
                "{name}"
            );
        }
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let script = dir.path().join("build");
            std::fs::write(&script, "#!/bin/sh").unwrap();
            std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755)).unwrap();
            assert!(is_launchable(&script, false));
        }
    }
}
