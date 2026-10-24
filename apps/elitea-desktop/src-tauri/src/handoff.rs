//! Desktop ⇄ cloud handoff (ADR-0034, Phase 1 wire contract §2 and §5).
//!
//! The IPC surface (IPC.md, "Cloud handoff"): `handoff_preview`,
//! `handoff_send`, `handoff_status`, `handoff_reclaim`, `handoff_preflight`,
//! `handoff_apply`, `handoff_undo` and `handoff_erase`, with progress on
//! `handoff://event`. The folder work is `elitea-local-tools`' handoff
//! module (snapshot, exclusions, scan, caps, take-back; every git call
//! hardened); the bytes move through `elitea-workspace-store`'s REST
//! transport on the app's one pooled client with the native token.
//!
//! * **Policy.** A send needs the device's `local_work.allowed` and
//!   `local_work.cloud_handoff` (`cloud_handoff_disabled`); the server
//!   checks the platform ceiling and the project on begin.
//! * **One executor.** While a thread is not `LOCAL` the host refuses its
//!   local turns (`thread_executor_is_cloud`, before the server would) and
//!   `handoff_status` tells the UI to show it read-only.
//! * **Records.** What take-back needs (the send's record, the epoch, the
//!   project) is kept in the app data (`handoff/threads/<uuid>.json`,
//!   owner-only), never in the folder. A release the server did not
//!   acknowledge is kept as pending and sent again by `handoff_status`.
//! * **History.** On release the cloud turns (`executed_by:
//!   cloud_executor`, within the handoff's epochs) are imported into
//!   `threads.sqlite` with `origin: cloud`, their changed files from the
//!   cloud commits' diffs.

use std::collections::{BTreeMap, HashMap};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use async_trait::async_trait;
use elitea_local_tools::handoff::apply::{self as take_back, ApplyOutcome, Mode, Preflight};
use elitea_local_tools::handoff::caps::{self, Caps};
use elitea_local_tools::handoff::exclude::{Excluded, Patterns, Reason};
use elitea_local_tools::handoff::scan::Finding;
use elitea_local_tools::handoff::snapshot::Included;
use elitea_local_tools::handoff::snapshot::{self, SendConfig, Snapshot, Source};
use elitea_local_tools::handoff::{SendRecord, check_conversation};
use elitea_local_tools::policy::LocalWorkPolicy;
use elitea_local_tools::sandbox::SandboxConfig;
use elitea_workspace_store::StoreError;
use elitea_workspace_store::bundle::Limits;
use elitea_workspace_store::refs::{self as store_refs, Allowlist, Writer};
use elitea_workspace_store::rest::{RestStore, TokenSource};
use elitea_workspace_store::sync::{self, Backoff};
use elitea_workspace_store::transport::{PublishRequest, RefUpdate};
use reqwest::Method;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use crate::d0::api::{Answer, ApiError, Credentials, PlatformApi};
use crate::d0::recorder::FileChange;
use crate::d0::turn::{AgentHost, PolicySource, TurnError};
use crate::handoff_settings::{self as settings, EffectivePolicy, FolderSettings, SettingsStore};
use crate::history::{HistoryStore, ImportedTurn};
use crate::workspaces::WorkspaceStore;

/// How long a sidebar status is reused (`handoff_status_many`).
const STATUS_CACHE: Duration = Duration::from_secs(15);

/// The diff text a preflight returns at most, and the largest file diffed.
const PREFLIGHT_DIFF_BYTES: usize = 256 * 1024;
const PREFLIGHT_DIFF_FILE_BYTES: u64 = 1024 * 1024;

/// The event the UI listens to for a send's progress.
pub const EVENT_NAME: &str = "handoff://event";

/// `executed_by` of a cloud turn (contract §1).
pub const EXECUTED_BY_CLOUD: &str = "cloud_executor";

/// One progress event (contract §5).
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct HandoffEvent {
    pub conversation_uuid: String,
    /// `snapshot`, `scan`, `bundle`, `upload`, `publish`, `complete`, `error`.
    pub phase: &'static str,
    pub done: u64,
    pub total: u64,
    pub message: Option<String>,
}

/// Where progress goes (the main window; a list in tests).
pub trait HandoffEvents: Send + Sync {
    fn emit(&self, event: HandoffEvent);
}

/// A refused or failed handoff command.
#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct HandoffError {
    pub code: String,
    pub message: String,
    /// Secret findings, cap violations, the largest paths: what the send
    /// dialog shows (`null` otherwise).
    pub details: Value,
}

impl HandoffError {
    pub(crate) fn new(code: &str, message: impl Into<String>) -> Self {
        Self {
            code: code.to_owned(),
            message: message.into(),
            details: Value::Null,
        }
    }
}

impl From<TurnError> for HandoffError {
    fn from(error: TurnError) -> Self {
        Self::new(&error.code, error.message)
    }
}

impl From<ApiError> for HandoffError {
    fn from(error: ApiError) -> Self {
        Self::new(&error.code, error.message)
    }
}

impl From<StoreError> for HandoffError {
    fn from(error: StoreError) -> Self {
        let message = match &error {
            StoreError::Http { message, .. } => message.clone(),
            other => other.to_string(),
        };
        Self::new(error.code(), message)
    }
}

fn tool_error(error: &elitea_local_tools::error::ToolError) -> HandoffError {
    use elitea_local_tools::error::ErrorCode;
    let code = match error.code() {
        ErrorCode::StaleRead => take_back::FOLDER_CHANGED,
        ErrorCode::UnsafeRepository => "unsafe_repository",
        ErrorCode::Unsupported if error.message().contains("sha256") => "unsupported_object_format",
        ErrorCode::Unsupported => "handoff_unsupported",
        ErrorCode::Conflict if error.message().contains("integrity") => "integrity_failed",
        ErrorCode::Conflict => "handoff_conflict",
        ErrorCode::InvalidArgument => "invalid_request",
        _ => "handoff_failed",
    };
    HandoffError::new(code, error.message())
}

fn joined<T>(result: Result<T, tokio::task::JoinError>) -> Result<T, HandoffError> {
    result.map_err(|_| HandoffError::new("internal", "the handoff work stopped unexpectedly"))
}

/// The epoch-tracked record of a thread that left this desktop.
#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
struct ThreadRecord {
    conversation_uuid: String,
    workspace_id: String,
    project_id: i64,
    handoff_id: String,
    /// The epoch the server answered last.
    epoch: u64,
    /// The epoch the handoff became `CLOUD_IDLE` at: cloud turns are later.
    cloud_epoch: u64,
    state: String,
    send: SendRecord,
    sent_at: i64,
    /// A release the server did not acknowledge yet.
    #[serde(default)]
    pending_release: Option<Value>,
}

/// The records, one owner-only JSON file per thread.
struct Records {
    dir: PathBuf,
    lock: Mutex<()>,
}

impl Records {
    fn path(&self, uuid: &str) -> PathBuf {
        self.dir.join(format!("{uuid}.json"))
    }

    fn get(&self, uuid: &str) -> Result<Option<ThreadRecord>, HandoffError> {
        let _guard = self
            .lock
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        match std::fs::read(self.path(uuid)) {
            Ok(bytes) => serde_json::from_slice(&bytes)
                .map(Some)
                .map_err(|_| HandoffError::new("storage", "the handoff record is unreadable")),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(error) => Err(HandoffError::new(
                "storage",
                format!("cannot read the handoff record: {error}"),
            )),
        }
    }

    fn put(&self, record: &ThreadRecord) -> Result<(), HandoffError> {
        let _guard = self
            .lock
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let storage = |e: &dyn std::fmt::Display| {
            HandoffError::new("storage", format!("cannot store the handoff record: {e}"))
        };
        crate::credentials_file::create_private_dir(&self.dir).map_err(|e| storage(&e))?;
        let bytes = serde_json::to_vec(record).map_err(|e| storage(&e))?;
        let temp = self.dir.join(format!(
            ".{}.{}.tmp",
            record.conversation_uuid,
            std::process::id()
        ));
        let _ = std::fs::remove_file(&temp);
        {
            use std::io::Write as _;
            let mut file =
                crate::credentials_file::create_private_file(&temp).map_err(|e| storage(&e))?;
            file.write_all(&bytes).map_err(|e| storage(&e))?;
            file.sync_all().map_err(|e| storage(&e))?;
        }
        std::fs::rename(&temp, self.path(&record.conversation_uuid)).map_err(|e| storage(&e))
    }

    fn remove(&self, uuid: &str) {
        let _guard = self
            .lock
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let _ = std::fs::remove_file(self.path(uuid));
    }
}

/// What the host needs.
pub struct HandoffDeps {
    pub agents: Arc<AgentHost>,
    pub http: crate::net::SharedHttp,
    pub credentials: Arc<dyn Credentials>,
    pub client_version: String,
    pub workspaces: Arc<WorkspaceStore>,
    pub policy: Arc<dyn PolicySource>,
    pub history: Option<Arc<HistoryStore>>,
    /// The app data directory: records, shadows.
    pub data_dir: PathBuf,
    pub events: Arc<dyn HandoffEvents>,
    /// The native client's device id (the signed-in session's).
    pub device_id: Arc<dyn Fn() -> Option<String> + Send + Sync>,
    pub backoff: Backoff,
    /// Tests: the global git config.
    pub git_global_config: Option<PathBuf>,
    /// The variables the desktop's `run_command` passes to commands: what
    /// the env harvest offers (values read again at every send, never
    /// stored).
    pub env_source: Arc<dyn settings::EnvSource>,
}

/// A running send's cancel switch.
#[derive(Default)]
struct CancelSwitch {
    cancelled: AtomicBool,
    notify: tokio::sync::Notify,
}

impl CancelSwitch {
    fn cancel(&self) {
        self.cancelled.store(true, Ordering::SeqCst);
        self.notify.notify_one();
    }

    fn is_cancelled(&self) -> bool {
        self.cancelled.load(Ordering::SeqCst)
    }

    async fn wait(&self) {
        while !self.is_cancelled() {
            self.notify.notified().await;
        }
    }
}

fn cancelled() -> HandoffError {
    HandoffError::new(
        "handoff_cancelled",
        "Sending was cancelled; the thread stays on this computer.",
    )
}

/// A held preflight.
struct Held {
    conversation_uuid: String,
    workspace_id: String,
    root: PathBuf,
    path_deny: Vec<String>,
    preflight: Preflight,
}

/// The handoff host.
pub struct HandoffHost {
    deps: HandoffDeps,
    api: PlatformApi,
    records: Records,
    /// Used when the session names no device id: one per install.
    install_id: String,
    preflights: Mutex<HashMap<String, Held>>,
    settings: SettingsStore,
    /// Running sends by conversation.
    sends: Mutex<HashMap<String, Arc<CancelSwitch>>>,
    /// `handoff_status_many`: the last answer per conversation.
    status_cache: Mutex<HashMap<String, (Instant, Value)>>,
}

/// A running send's registration (removed when the send ends).
struct SendGuard<'a> {
    host: &'a HandoffHost,
    uuid: String,
    switch: Arc<CancelSwitch>,
}

impl Drop for SendGuard<'_> {
    fn drop(&mut self) {
        let mut sends = self
            .host
            .sends
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if sends
            .get(&self.uuid)
            .is_some_and(|switch| Arc::ptr_eq(switch, &self.switch))
        {
            sends.remove(&self.uuid);
        }
    }
}

/// One excluded path as the send dialog shows it.
#[derive(Clone, Debug, Serialize)]
pub struct ExcludedView {
    pub path: String,
    pub reason: Reason,
    pub tracked: bool,
    /// "Include anyway" is offered.
    pub includable: bool,
}

/// `handoff_preview`'s answer.
#[derive(Clone, Debug, Serialize)]
pub struct Preview {
    pub files: usize,
    pub bytes: u64,
    pub unpacked_bytes: u64,
    pub excluded: Vec<ExcludedView>,
    pub included: Vec<Included>,
    /// They block the send.
    pub findings: Vec<Finding>,
    /// In included files: warnings the send needs confirmed.
    pub include_findings: Vec<Finding>,
    pub has_upstream: bool,
    /// From the policy route (`None`: the server has none).
    pub project_workspace_bytes_left: Option<u64>,
    pub caps: caps::CapReport,
    /// The folder is not a repository: its shadow is used.
    pub shadow: bool,
    /// `HandoffPolicy` (IPC.md).
    pub policy: Value,
    /// `null` unless the policy harvests env variables; names and value
    /// lengths only.
    pub env: Value,
}

/// `handoff_send`'s answer.
#[derive(Clone, Debug, Serialize)]
pub struct Sent {
    pub handoff_id: String,
    pub state: String,
    pub epoch: u64,
    pub files: usize,
    pub excluded: usize,
    pub included: usize,
    pub env_vars: usize,
}

/// `handoff_preflight`'s answer.
#[derive(Clone, Debug, Serialize)]
pub struct PreflightAnswer {
    pub preflight_id: String,
    /// The shared apply engine's outcome (`outcome`, `applied`,
    /// `changed_paths`, `conflict_paths`, `skipped_paths`, `base`,
    /// `result_tree`), flattened into the answer.
    #[serde(flatten)]
    pub report: ApplyOutcome,
    pub cloud_turns: usize,
    /// The turns this preflight takes back (`1..=cloud_turns`).
    pub up_to_turn: usize,
    /// What `handoff_apply` accepts: `apply` for `fast_path` and
    /// `clean_merge`, `apply_with_markers` for `conflicts`; `worktree` and
    /// `discard` always.
    pub modes: Vec<Mode>,
    /// What the take-back writes, per file, with its diff (bounded).
    pub files: Vec<FileChange>,
    pub diffs_truncated: bool,
}

/// `handoff_apply`'s answer.
#[derive(Clone, Debug, Serialize)]
pub struct ApplyAnswer {
    pub applied: bool,
    /// What was done: `applied`, `applied_with_markers`, `worktree`,
    /// `discarded`.
    pub outcome: &'static str,
    /// The engine's outcome of the apply (`applied` set when files were
    /// written).
    pub report: ApplyOutcome,
    /// `handoff_undo` restores it (`null` when the folder was not written).
    pub checkpoint_id: Option<String>,
    pub worktree_path: Option<String>,
    pub branch: Option<String>,
    pub imported_turns: usize,
    /// The release is acknowledged (`false`: kept and retried).
    pub released: bool,
    /// The server refused the release for good (`handoff_erased`: the cloud
    /// copy was erased meanwhile; `stale_epoch`, `handoff_not_found`): it is
    /// not retried, and the thread follows the server's state.
    pub release_refused: Option<String>,
    /// `worktree`: the folder's excluded files copied into it.
    pub copied: Vec<String>,
}

/// A release refused with one of these codes can never be acknowledged:
/// the cloud copy was erased or expired (`handoff_erased`), the thread is
/// not this owner's any more (`handoff_not_found`), or it moved on
/// (`stale_epoch`). Retrying it would keep the thread read-only forever.
const RELEASE_REFUSED_FOR_GOOD: [&str; 3] = ["handoff_erased", "handoff_not_found", "stale_epoch"];

/// What a release came to.
enum Released {
    /// Acknowledged: the new epoch.
    Acknowledged(u64),
    /// Refused for good (the code); the record follows the server's state.
    Refused(String),
}

/// The bearer through the pinned host credentials.
struct Tokens(Arc<dyn Credentials>);

#[async_trait]
impl TokenSource for Tokens {
    async fn bearer(&self) -> Result<String, StoreError> {
        self.0
            .bearer()
            .await
            .map(|bearer| bearer.token)
            .map_err(|e| StoreError::http(401, &e.code, e.message))
    }

    async fn refreshed(&self) -> Result<String, StoreError> {
        self.0
            .refreshed()
            .await
            .map(|bearer| bearer.token)
            .map_err(|e| StoreError::http(401, &e.code, e.message))
    }
}

/// The error of a non-2xx answer: the flat desktop envelope or the nested
/// form.
fn answer_error(answer: &Answer) -> HandoffError {
    let nested = answer.body.get("error").filter(|e| e.is_object());
    let text = |value: Option<&Value>| value.and_then(Value::as_str).map(str::to_owned);
    let code = text(nested.and_then(|e| e.get("code")))
        .or_else(|| text(answer.body.get("error")))
        .unwrap_or_else(|| format!("http_{}", answer.status));
    let message = text(nested.and_then(|e| e.get("message")))
        .or_else(|| text(answer.body.get("message")))
        .unwrap_or_else(|| format!("the platform answered HTTP {}", answer.status));
    HandoffError {
        code,
        message,
        details: nested
            .and_then(|e| e.get("details"))
            .or_else(|| answer.body.get("details"))
            .cloned()
            .unwrap_or(Value::Null),
    }
}

/// The device's own refusal: `local_work.allowed` or
/// `local_work.cloud_handoff` is off.
const DEVICE_OFF: &str = "device_off";

/// `cloud_handoff_disabled` with the reason the UI picks its text by
/// (`details.reason`, as the server's 403 carries it), or no details when
/// the server named none.
fn disabled(reason: Option<&str>, message: &str) -> HandoffError {
    let mut error = HandoffError::new("cloud_handoff_disabled", message);
    if let Some(reason) = reason {
        error.details = json!({ "reason": reason });
    }
    error
}

fn ok_body(answer: Answer) -> Result<Value, HandoffError> {
    if (200..300).contains(&answer.status) {
        Ok(answer.body)
    } else {
        Err(answer_error(&answer))
    }
}

fn canonical_uuid(raw: &str) -> Result<String, HandoffError> {
    let uuid = uuid::Uuid::parse_str(raw)
        .map(|u| u.hyphenated().to_string())
        .map_err(|_| {
            HandoffError::new(
                "invalid_request",
                "conversation_uuid must be the conversation's UUID",
            )
        })?;
    check_conversation(&uuid).map_err(|e| tool_error(&e))?;
    Ok(uuid)
}

fn u64_of(body: &Value, key: &str) -> u64 {
    body.get(key).and_then(Value::as_u64).unwrap_or_default()
}

fn str_of(body: &Value, key: &str) -> Option<String> {
    body.get(key).and_then(Value::as_str).map(str::to_owned)
}

/// `executor` as the UI shows it, from a server state.
fn executor_of(state: &str) -> &'static str {
    match state {
        "LOCAL" => "local",
        "RECLAIMING" => "returning",
        _ => "cloud",
    }
}

impl HandoffHost {
    /// # Errors
    ///
    /// The device id cannot be stored.
    pub fn new(deps: HandoffDeps) -> Result<Self, HandoffError> {
        let api = PlatformApi::shared(
            deps.http.clone(),
            deps.credentials.clone(),
            &deps.client_version,
        );
        let dir = deps.data_dir.join("handoff");
        crate::credentials_file::create_private_dir(&dir).map_err(|e| {
            HandoffError::new("storage", format!("cannot prepare the handoff data: {e}"))
        })?;
        let device_path = dir.join("install-id");
        let install_id = match std::fs::read_to_string(&device_path) {
            Ok(id) if uuid::Uuid::parse_str(id.trim()).is_ok() => id.trim().to_owned(),
            _ => {
                let id = uuid::Uuid::new_v4().to_string();
                std::fs::write(&device_path, &id).map_err(|e| {
                    HandoffError::new("storage", format!("cannot store the device id: {e}"))
                })?;
                id
            }
        };
        Ok(Self {
            records: Records {
                dir: dir.join("threads"),
                lock: Mutex::new(()),
            },
            settings: SettingsStore::new(dir.join("settings")),
            api,
            install_id,
            preflights: Mutex::new(HashMap::new()),
            sends: Mutex::new(HashMap::new()),
            status_cache: Mutex::new(HashMap::new()),
            deps,
        })
    }

    fn emit(
        &self,
        uuid: &str,
        phase: &'static str,
        done: u64,
        total: u64,
        message: Option<String>,
    ) {
        self.deps.events.emit(HandoffEvent {
            conversation_uuid: uuid.to_owned(),
            phase,
            done,
            total,
            message,
        });
    }

    fn device_id(&self) -> String {
        (self.deps.device_id)().unwrap_or_else(|| self.install_id.clone())
    }

    /// The device side of the policy: Local work and the handoff switch.
    fn policy(&self) -> Result<LocalWorkPolicy, HandoffError> {
        let policy: LocalWorkPolicy = self
            .deps
            .policy
            .local_work()
            .and_then(|section| serde_json::from_value(section).ok())
            .unwrap_or_default();
        if !policy.allowed || !policy.cloud_handoff {
            return Err(disabled(
                Some(DEVICE_OFF),
                "Sending threads to the cloud is turned off by your organisation's policy.",
            ));
        }
        Ok(policy)
    }

    /// The workspace's folder and project.
    fn workspace(&self, workspace_id: &str) -> Result<(PathBuf, i64), HandoffError> {
        let workspace = self
            .deps
            .workspaces
            .get(workspace_id)
            .map_err(|e| HandoffError::new("storage", e.to_string()))?
            .ok_or_else(|| HandoffError::new("workspace_unknown", "That workspace is not open."))?;
        let project = workspace.project_id.ok_or_else(|| {
            HandoffError::new(
                "workspace_unbound",
                "Bind a project to this workspace first.",
            )
        })?;
        Ok((PathBuf::from(workspace.path), project))
    }

    fn config(
        &self,
        root: &Path,
        uuid: &str,
        policy: &LocalWorkPolicy,
        leave: Vec<String>,
        caps: Caps,
    ) -> SendConfig {
        SendConfig {
            root: root.to_path_buf(),
            data_dir: self.deps.data_dir.clone(),
            sandbox: SandboxConfig::default(),
            deny: self.deps.agents.host_deny(),
            path_deny: policy.path_deny.clone(),
            leave_on_laptop: leave,
            patterns: Patterns::default(),
            include: Vec::new(),
            caps,
            conversation_uuid: uuid.to_owned(),
            note: String::new(),
            global_config: self.deps.git_global_config.clone(),
        }
    }

    fn open(&self, config: &SendConfig) -> Result<Source, HandoffError> {
        snapshot::open_source(
            &config.root,
            &config.data_dir,
            &config.sandbox,
            &config.deny,
            config.global_config.as_deref(),
        )
        .map_err(|e| tool_error(&e))
    }

    async fn store(&self, project_id: i64, uuid: &str) -> Result<RestStore, HandoffError> {
        let bearer = self.deps.credentials.bearer().await?;
        let client = self.deps.http.client().await.clone();
        Ok(RestStore::new(
            client,
            &bearer.origin,
            project_id,
            uuid,
            Arc::new(Tokens(self.deps.credentials.clone())),
        )
        .with_header(
            crate::tokens::CLIENT_VERSION_HEADER,
            &self.deps.client_version,
        ))
    }

    async fn call(
        &self,
        method: Method,
        path: &str,
        body: Option<&Value>,
    ) -> Result<Value, HandoffError> {
        ok_body(self.api.send(method, path, body, &[]).await?)
    }

    /// Whether a thread is away from this desktop (sent and not released):
    /// its local turns are refused.
    #[must_use]
    pub fn is_away(&self, conversation: &str) -> bool {
        uuid::Uuid::parse_str(conversation)
            .ok()
            .map(|u| u.hyphenated().to_string())
            .and_then(|uuid| self.records.get(&uuid).ok().flatten())
            .is_some_and(|record| record.state != "LOCAL")
    }

    /// The effective handoff policy of `project_id` before begin (contract
    /// §6.1); the ADR's defaults when the server has no such route (404).
    ///
    /// # Errors
    ///
    /// The platform refused or did not answer; `cloud_handoff_disabled`
    /// when the policy is off.
    async fn effective_policy(&self, project_id: i64) -> Result<EffectivePolicy, HandoffError> {
        let answer = self
            .api
            .send(
                Method::GET,
                &format!("/api/v2/elitea_core/workspace_handoff_policy/prompt_lib/{project_id}"),
                None,
                &[],
            )
            .await?;
        let policy = if answer.status == 404 {
            EffectivePolicy::defaults()
        } else {
            EffectivePolicy::from_server(&ok_body(answer)?)
        };
        if !policy.enabled {
            return Err(disabled(
                policy.disabled_reason.as_deref(),
                "Sending threads to the cloud is turned off for this project.",
            ));
        }
        Ok(policy)
    }

    /// The device's and the server's `path_deny`, together.
    fn path_deny(device: &LocalWorkPolicy, effective: &EffectivePolicy) -> Vec<String> {
        let mut deny = device.path_deny.clone();
        for pattern in &effective.path_deny {
            if !deny.contains(pattern) {
                deny.push(pattern.clone());
            }
        }
        deny
    }

    /// A send's configuration under the effective policy and the folder's
    /// settings.
    fn send_config(
        &self,
        root: &Path,
        uuid: &str,
        device: &LocalWorkPolicy,
        effective: &EffectivePolicy,
        folder: &FolderSettings,
        leave: Vec<String>,
    ) -> SendConfig {
        let mut config = self.config(root, uuid, device, leave, effective.limits);
        config.path_deny = Self::path_deny(device, effective);
        config.patterns = Patterns {
            platform: effective.patterns.platform.clone(),
            project: effective.patterns.project.clone(),
            user: folder.exclude_patterns.clone(),
        };
        config.include = if effective.allow_user_include {
            folder.include.clone()
        } else {
            Vec::new()
        };
        config
    }

    /// The variables the harvest may offer now (read once), and a notice
    /// when the login shell could not be asked.
    async fn offered_env(
        &self,
        effective: &EffectivePolicy,
    ) -> (Vec<(String, String)>, Option<String>) {
        let environment = self.deps.env_source.environment().await;
        (
            settings::offered(environment.vars, effective),
            environment.notice,
        )
    }

    /// The env variables the folder's selection would send now.
    async fn harvest(
        &self,
        effective: &EffectivePolicy,
        folder: &FolderSettings,
    ) -> settings::Harvest {
        if !effective.env_harvest {
            return settings::Harvest::default();
        }
        let (offered, _) = self.offered_env(effective).await;
        settings::harvest(&folder.env_names, &offered, effective.env_limits)
    }

    /// `handoff_preview`: what a send would transfer and leave out, under
    /// the effective policy and the folder's settings, the findings and the
    /// caps. Sends nothing.
    ///
    /// # Errors
    ///
    /// `cloud_handoff_disabled`, `workspace_*`, `workspace_busy`, the
    /// folder's refusals, the policy route's failures.
    pub async fn preview(
        &self,
        workspace_id: &str,
        conversation_uuid: &str,
        leave_on_laptop: Vec<String>,
    ) -> Result<Preview, HandoffError> {
        let uuid = canonical_uuid(conversation_uuid)?;
        let device = self.policy()?;
        let (root, project_id) = self.workspace(workspace_id)?;
        let effective = self.effective_policy(project_id).await?;
        let folder = self.settings.get(workspace_id)?;
        let claim = self
            .deps
            .agents
            .claim_workspace(workspace_id, "Wait for the running turn to end.")?;
        let config = self.send_config(&root, &uuid, &device, &effective, &folder, leave_on_laptop);
        let source = self.open(&config)?;
        let limits = config.caps;
        let path_deny = config.path_deny.clone();
        let (snap, bytes) = joined(
            tokio::task::spawn_blocking(move || {
                let snap = snapshot::take(&source, &config)?;
                let bytes = snapshot::bundle(&source, &snap, Limits::default())
                    .map(|written| written.bytes.len() as u64)
                    .unwrap_or_default();
                Ok::<_, elitea_local_tools::error::ToolError>((snap, bytes))
            })
            .await,
        )?
        .map_err(|e| tool_error(&e))?;
        drop(claim);
        let mut caps_report = snap.caps.clone();
        caps::check_segment(&mut caps_report, bytes, &limits);
        let env = if effective.env_harvest {
            let (offered, notice) = self.offered_env(&effective).await;
            let harvest = settings::harvest(&folder.env_names, &offered, effective.env_limits);
            json!({
                "notice": notice,
                "offered": settings::offered_wire(&offered),
                "selected": harvest.env.keys().collect::<Vec<_>>(),
                "missing": harvest.missing,
                "vars": harvest.env.len(),
                "bytes": harvest.bytes,
                "over_limit": harvest.over_limit,
            })
        } else {
            Value::Null
        };
        let includable = |entry: &Excluded| {
            effective.allow_user_include
                && !matches!(
                    entry.reason,
                    Reason::PathDeny | Reason::LeaveOnLaptop | Reason::NotAFile
                )
        };
        Ok(Preview {
            files: snap.files,
            bytes,
            unpacked_bytes: snap.unpacked_bytes,
            has_upstream: snap.upstream.is_some(),
            excluded: snap
                .excluded
                .iter()
                .map(|entry| ExcludedView {
                    path: entry.path.clone(),
                    reason: entry.reason,
                    tracked: entry.tracked,
                    includable: includable(entry),
                })
                .collect(),
            included: snap.included,
            findings: snap.findings,
            include_findings: snap.include_findings,
            project_workspace_bytes_left: limits.project_workspace_bytes_left,
            caps: caps_report,
            shadow: snap.shadow,
            policy: effective.wire(&folder.exclude_patterns, &path_deny),
            env,
        })
    }

    /// `handoff_send`: snapshot, scan, bundle, begin, upload, publish,
    /// complete; progress on `handoff://event`. A failure after begin (the
    /// begin answer's caps, the upload, a cancel) cancels the handoff on
    /// the server, so the thread is `LOCAL` again.
    ///
    /// # Errors
    ///
    /// `secret_found`, `handoff_too_large` and `workspace_quota_exceeded` (with `details`),
    /// `include_confirm_required`, `env_too_large`, `handoff_cancelled`, the
    /// server's refusals (`cloud_handoff_disabled`,
    /// `local_turn_in_progress`, `handoff_in_progress`,
    /// `workspace_quota_exceeded`, …), store errors.
    pub async fn send(
        &self,
        workspace_id: &str,
        conversation_uuid: &str,
        leave_on_laptop: Vec<String>,
        confirm_includes: bool,
    ) -> Result<Sent, HandoffError> {
        let uuid = canonical_uuid(conversation_uuid)?;
        let switch = Arc::new(CancelSwitch::default());
        {
            let mut sends = self
                .sends
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            if sends.contains_key(&uuid) {
                return Err(HandoffError::new(
                    "handoff_in_progress",
                    "This thread is being sent already.",
                ));
            }
            sends.insert(uuid.clone(), switch.clone());
        }
        let _guard = SendGuard {
            host: self,
            uuid: uuid.clone(),
            switch: switch.clone(),
        };
        let result = self
            .send_inner(
                workspace_id,
                &uuid,
                leave_on_laptop,
                confirm_includes,
                &switch,
            )
            .await;
        match &result {
            Err(error) if error.code == "handoff_cancelled" => {
                self.emit(&uuid, "cancelled", 0, 0, None);
            }
            Err(error) => self.emit(
                &uuid,
                "error",
                0,
                0,
                Some(format!("{}: {}", error.code, error.message)),
            ),
            Ok(_) => {}
        }
        result
    }

    /// `handoff_cancel`: stop a running send of `conversation_uuid`.
    ///
    /// # Errors
    ///
    /// `invalid_request` for a malformed UUID.
    pub fn cancel(&self, conversation_uuid: &str) -> Result<bool, HandoffError> {
        let uuid = canonical_uuid(conversation_uuid)?;
        let switch = self
            .sends
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .get(&uuid)
            .cloned();
        Ok(switch.is_some_and(|switch| {
            switch.cancel();
            true
        }))
    }

    /// Cancel a begun handoff on the server (`DELETE` in `HANDING_OFF`,
    /// contract §6.3). Best effort: the server's 15-minute expiry is the
    /// fallback. The state is read first and only a `HANDING_OFF` thread is
    /// deleted: a handoff that already completed (a lost `complete` answer)
    /// holds the fresh cloud copy, which a delete would erase. A 404 or 409
    /// is fine (nothing to cancel).
    async fn abandon(&self, project_id: i64, uuid: &str) {
        let path = format!("/api/v2/elitea_core/workspace_handoff/prompt_lib/{project_id}/{uuid}");
        // An unreadable state falls through to the delete: the server only
        // accepts it in HANDING_OFF.
        if let Ok(answer) = self.api.send(Method::GET, &path, None, &[]).await
            && (200..300).contains(&answer.status)
            && str_of(&answer.body, "state").is_some_and(|state| state != "HANDING_OFF")
        {
            return;
        }
        match self.api.send(Method::DELETE, &path, None, &[]).await {
            Ok(answer)
                if (200..300).contains(&answer.status)
                    || answer.status == 404
                    || answer.status == 409 => {}
            Ok(answer) => log::warn!(
                "a begun handoff could not be cancelled (HTTP {}); the server expires it",
                answer.status
            ),
            Err(error) => log::warn!(
                "a begun handoff could not be cancelled ({}); the server expires it",
                error.code
            ),
        }
    }

    #[allow(clippy::too_many_lines)]
    async fn send_inner(
        &self,
        workspace_id: &str,
        uuid: &str,
        leave: Vec<String>,
        confirm_includes: bool,
        switch: &CancelSwitch,
    ) -> Result<Sent, HandoffError> {
        let device = self.policy()?;
        let (root, project_id) = self.workspace(workspace_id)?;
        if self.is_away(uuid) {
            return Err(HandoffError::new(
                "thread_executor_is_cloud",
                "This thread is already in the cloud.",
            ));
        }
        let effective = self.effective_policy(project_id).await?;
        let folder = self.settings.get(workspace_id)?;
        let _claim = self.deps.agents.claim_workspace(
            workspace_id,
            "Wait for the running turn to end before sending this thread.",
        )?;
        // The thread leaves this device: nothing the agent started here
        // keeps running (or writing) while the snapshot is taken (#1223).
        self.deps.agents.kill_processes(workspace_id).await;
        self.emit(uuid, "snapshot", 0, 1, None);
        let config = self.send_config(&root, uuid, &device, &effective, &folder, leave);
        let source = self.open(&config)?;
        let (source, snap) = {
            let config = config.clone();
            joined(
                tokio::task::spawn_blocking(move || {
                    snapshot::take(&source, &config).map(|snap| (source, snap))
                })
                .await,
            )?
            .map_err(|e| tool_error(&e))?
        };
        if switch.is_cancelled() {
            return Err(cancelled());
        }
        self.emit(
            uuid,
            "scan",
            1,
            1,
            Some(format!(
                "{} files, {} findings",
                snap.files,
                snap.findings.len()
            )),
        );
        refuse_blocked(&snap)?;
        if !snap.include_findings.is_empty() && !confirm_includes {
            return Err(HandoffError {
                code: "include_confirm_required".into(),
                message: "Some files you included look like they hold credentials. Confirm sending them for this send.".into(),
                details: json!({ "findings": snap.include_findings }),
            });
        }
        // The env selection, before begin (values only in the begin body).
        let harvest = self.harvest(&effective, &folder).await;
        if harvest.over_limit {
            return Err(HandoffError {
                code: "env_too_large".into(),
                message: "The selected environment variables are more than a handoff may carry."
                    .into(),
                details: json!({
                    "vars": harvest.env.len(), "bytes": harvest.bytes,
                    "max_vars": effective.env_limits.max_vars, "max_bytes": effective.env_limits.max_bytes,
                }),
            });
        }
        self.emit(uuid, "bundle", 0, 1, None);
        let (source, snap, written) = joined(
            tokio::task::spawn_blocking(move || {
                snapshot::bundle(&source, &snap, Limits::default())
                    .map(|written| (source, snap, written))
            })
            .await,
        )?
        .map_err(|e| tool_error(&e))?;
        self.emit(
            uuid,
            "bundle",
            1,
            1,
            Some(format!("{} bytes", written.bytes.len())),
        );
        // The policy's caps, before begin.
        let mut before = caps::CapReport::default();
        caps::check_segment(&mut before, written.bytes.len() as u64, &effective.limits);
        if !before.ok() {
            before.largest.clone_from(&snap.caps.largest);
            return Err(quota_or_too_large(&before));
        }
        if switch.is_cancelled() {
            return Err(cancelled());
        }
        // Begin.
        self.emit(uuid, "begin", 0, 1, None);
        let mut body = json!({
            "device_id": self.device_id(),
            "history_mode": "none",
            "stats": {
                "files": snap.files,
                "bytes": written.bytes.len(),
                "unpacked_bytes": snap.unpacked_bytes,
                "excluded": snap.excluded.iter().map(|e| format!("./{}", e.path)).collect::<Vec<_>>(),
                "scan_findings": snap.include_findings.len(),
            },
            "has_upstream": snap.upstream.is_some(),
            "note": handoff_note(
                &snap,
                !self.deps.agents.mcp_files().merged(workspace_id).0.is_empty(),
                !self.deps.agents.hook_files().merged(workspace_id).0.is_empty()
            ),
        });
        if !snap.included.is_empty() {
            body["included_excluded"] = json!(
                snap.included
                    .iter()
                    .map(|file| format!("./{}", file.path))
                    .collect::<Vec<_>>()
            );
        }
        let env_vars = harvest.env.len();
        if effective.env_harvest {
            body["env"] = json!(harvest.env);
        }
        drop(harvest);
        let begin = self
            .api
            .send(
                Method::POST,
                &format!("/api/v2/elitea_core/workspace_handoff/prompt_lib/{project_id}/{uuid}"),
                Some(&body),
                &[],
            )
            .await;
        // The env values leave memory with the request.
        drop(body);
        let begin = match begin {
            Ok(answer) if (200..300).contains(&answer.status) => answer.body,
            // The server may have committed a begin whose answer was lost
            // (a timeout, a dropped connection, a 5xx): cancel it, or the
            // thread would stay in HANDING_OFF until the server's expiry.
            Ok(answer) if answer.status >= 500 => {
                self.abandon(project_id, uuid).await;
                return Err(answer_error(&answer));
            }
            Ok(answer) => return Err(answer_error(&answer)),
            Err(error) => {
                self.abandon(project_id, uuid).await;
                return Err(error.into());
            }
        };
        self.emit(uuid, "begin", 1, 1, None);
        let completing = AtomicBool::new(false);
        let mut after_begin = std::pin::pin!(self.after_begin(
            project_id,
            workspace_id,
            uuid,
            &begin,
            &snap,
            &written,
            &completing
        ));
        let outcome = tokio::select! {
            outcome = &mut after_begin => outcome,
            () = switch.wait() => {
                // Once `complete` is on its way the cloud holds the copy: a
                // cancel no longer abandons, the send finishes.
                if completing.load(Ordering::SeqCst) {
                    after_begin.await
                } else {
                    Err(cancelled())
                }
            }
        };
        let (handoff_id, state, cloud_epoch) = match outcome {
            Ok(done) => done,
            Err(error) => {
                self.abandon(project_id, uuid).await;
                return Err(error);
            }
        };
        {
            let snap = snap.clone();
            let uuid = uuid.to_owned();
            let kept = joined(
                tokio::task::spawn_blocking(move || snapshot::keep_refs(&source, &snap, &uuid))
                    .await,
            )?;
            if let Err(error) = kept {
                log::warn!(
                    "the handoff's private refs could not be kept: {}",
                    error.message()
                );
            }
        }
        self.emit(uuid, "complete", 1, 1, Some(state.clone()));
        Ok(Sent {
            handoff_id,
            state,
            epoch: cloud_epoch,
            files: snap.files,
            excluded: snap.excluded.len(),
            included: snap.included.len(),
            env_vars,
        })
    }

    /// Everything a send does after begin, up to the stored record:
    /// `(handoff id, state, epoch)`. Any failure is answered by cancelling
    /// the handoff on the server.
    #[allow(clippy::too_many_lines, clippy::too_many_arguments)]
    async fn after_begin(
        &self,
        project_id: i64,
        workspace_id: &str,
        uuid: &str,
        begin: &Value,
        snap: &Snapshot,
        written: &elitea_workspace_store::bundle::Written,
        completing: &AtomicBool,
    ) -> Result<(String, String, u64), HandoffError> {
        let handoff_id = str_of(begin, "handoff_id").ok_or_else(|| {
            HandoffError::new("invalid_response", "the begin answer has no handoff id")
        })?;
        let epoch = u64_of(begin, "epoch");
        let server_caps: Caps = begin
            .get("limits")
            .and_then(|limits| serde_json::from_value(limits.clone()).ok())
            .unwrap_or_default();
        // The server's caps, before a byte is uploaded.
        let mut report = caps::CapReport::default();
        if snap.files as u64 > server_caps.max_files {
            report.violations.push(caps::Violation {
                cap: "max_files".into(),
                limit: server_caps.max_files,
                actual: snap.files as u64,
                path: None,
            });
        }
        if snap.unpacked_bytes > server_caps.max_unpacked_bytes {
            report.violations.push(caps::Violation {
                cap: "max_unpacked_bytes".into(),
                limit: server_caps.max_unpacked_bytes,
                actual: snap.unpacked_bytes,
                path: None,
            });
        }
        if let Some(largest) = snap.caps.largest.first()
            && largest.bytes > server_caps.max_file_bytes
        {
            report.violations.push(caps::Violation {
                cap: "max_file_bytes".into(),
                limit: server_caps.max_file_bytes,
                actual: largest.bytes,
                path: Some(largest.path.clone()),
            });
        }
        caps::check_segment(&mut report, written.bytes.len() as u64, &server_caps);
        if !report.ok() {
            report.largest.clone_from(&snap.caps.largest);
            return Err(quota_or_too_large(&report));
        }
        // The refs, under both allowlists.
        let allowed: Vec<String> = begin
            .get("allowed_refs")
            .and_then(Value::as_array)
            .map(|refs| {
                refs.iter()
                    .filter_map(Value::as_str)
                    .map(str::to_owned)
                    .collect()
            })
            .unwrap_or_default();
        let expected_old = begin.get("expected_old").cloned().unwrap_or(Value::Null);
        let published = snapshot::published_refs(snap);
        let names: Vec<&str> = published.iter().map(|(name, _)| name.as_str()).collect();
        store_refs::check(Writer::Desktop, Some(&Allowlist::new(allowed)), &names)?;
        let request = PublishRequest {
            epoch,
            kind: elitea_workspace_store::KIND_COMPACT.into(),
            segment_sha256: written.sha256.clone(),
            bytes: written.bytes.len() as u64,
            prerequisites: Vec::new(),
            refs: published
                .iter()
                .map(|(name, new)| RefUpdate {
                    name: name.clone(),
                    old: expected_old
                        .get(name)
                        .and_then(Value::as_str)
                        .map(str::to_owned),
                    new: new.clone(),
                })
                .collect(),
            object_format: elitea_workspace_store::OBJECT_FORMAT_SHA1.into(),
        };
        self.emit(uuid, "upload", 0, written.bytes.len() as u64, None);
        let store = self.store(project_id, uuid).await?;
        sync::upload(&store, epoch, &written.bytes, self.deps.backoff).await?;
        self.emit(
            uuid,
            "upload",
            written.bytes.len() as u64,
            written.bytes.len() as u64,
            None,
        );
        self.emit(uuid, "publish", 0, 1, None);
        sync::publish(&store, &request, self.deps.backoff).await?;
        // From here a cancel cannot take the handoff back.
        completing.store(true, Ordering::SeqCst);
        self.emit(uuid, "publish", 1, 1, None);
        let complete = self
            .call(
                Method::POST,
                &format!("/api/v2/elitea_core/workspace_handoff_complete/prompt_lib/{project_id}/{handoff_id}"),
                Some(&json!({"epoch": epoch, "handoff_ref": store_refs::handoff(1), "sha": snap.handoff})),
            )
            .await?;
        let state = str_of(&complete, "state").unwrap_or_else(|| "CLOUD_IDLE".into());
        let cloud_epoch = u64_of(&complete, "epoch");
        let record = ThreadRecord {
            conversation_uuid: uuid.to_owned(),
            workspace_id: workspace_id.to_owned(),
            project_id,
            handoff_id: handoff_id.clone(),
            epoch: cloud_epoch,
            cloud_epoch,
            state: state.clone(),
            send: snap.record(uuid),
            sent_at: chrono::Utc::now().timestamp_millis(),
            pending_release: None,
        };
        self.records.put(&record)?;
        Ok((handoff_id, state, cloud_epoch))
    }

    /// `handoff_settings_get`: the folder's settings, the effective policy
    /// and the env variables it may offer (names and lengths).
    ///
    /// # Errors
    ///
    /// `workspace_*`, the policy route's failures.
    pub async fn settings_get(&self, workspace_id: &str) -> Result<Value, HandoffError> {
        let device = self.policy()?;
        let (_, project_id) = self.workspace(workspace_id)?;
        let effective = self.effective_policy(project_id).await?;
        let folder = self.settings.get(workspace_id)?;
        let (offered, notice) = if effective.env_harvest {
            self.offered_env(&effective).await
        } else {
            (Vec::new(), None)
        };
        Ok(json!({
            "env_notice": notice,
            "settings": folder,
            "policy": effective.wire(&folder.exclude_patterns, &Self::path_deny(&device, &effective)),
            "env_offered": settings::offered_wire(&offered),
        }))
    }

    /// `handoff_settings_set`: store the folder's settings (validated).
    ///
    /// # Errors
    ///
    /// `invalid_request`, `workspace_*`, storage.
    pub async fn settings_set(
        &self,
        workspace_id: &str,
        folder: FolderSettings,
    ) -> Result<Value, HandoffError> {
        self.workspace(workspace_id)?;
        let folder = folder.validated()?;
        self.settings.put(workspace_id, &folder)?;
        self.settings_get(workspace_id).await
    }

    /// The server's state of a thread (and a pending release sent again).
    async fn server_state(&self, project_id: i64, uuid: &str) -> Result<Value, HandoffError> {
        self.call(
            Method::GET,
            &format!("/api/v2/elitea_core/workspace_handoff/prompt_lib/{project_id}/{uuid}"),
            None,
        )
        .await
    }

    /// `handoff_status`: the server's banner state, and what the UI needs:
    /// `executor` (`local`, `cloud`, `returning`) and `read_only`.
    ///
    /// # Errors
    ///
    /// The platform refused or did not answer.
    pub async fn status(
        &self,
        conversation_uuid: &str,
        project_id: Option<i64>,
    ) -> Result<Value, HandoffError> {
        let uuid = canonical_uuid(conversation_uuid)?;
        let record = self.records.get(&uuid)?;
        let Some(project_id) = record.as_ref().map(|r| r.project_id).or(project_id) else {
            return Ok(
                json!({"state": "LOCAL", "epoch": 0, "executor": "local", "read_only": false, "sent_from_here": false}),
            );
        };
        if let Some(record) = &record
            && record.pending_release.is_some()
        {
            self.retry_release(record.clone()).await;
        }
        let mut state = self.server_state(project_id, &uuid).await?;
        let name = str_of(&state, "state").unwrap_or_else(|| "LOCAL".into());
        if let Some(mut record) = self.records.get(&uuid)? {
            if name == "LOCAL" && record.pending_release.is_none() {
                // Erased or expired elsewhere: the folder stays as it is.
                self.records.remove(&uuid);
            } else if record.state != name {
                record.state.clone_from(&name);
                record.epoch = u64_of(&state, "epoch");
                self.records.put(&record)?;
            }
        }
        if let Some(object) = state.as_object_mut() {
            object.insert("executor".into(), json!(executor_of(&name)));
            object.insert("read_only".into(), json!(name != "LOCAL"));
            object.insert("sent_from_here".into(), json!(record.is_some()));
        }
        Ok(state)
    }

    /// `handoff_status_many`: the sidebar's badges. A thread this desktop
    /// holds no record of is `LOCAL` with no request; the others answer from
    /// a 15-second cache, and at most one server call per project refreshes
    /// the stalest of them (the rest answer from their records until their
    /// turn).
    ///
    /// # Errors
    ///
    /// Storage.
    pub async fn status_many(&self, conversation_uuids: &[String]) -> Result<Value, HandoffError> {
        let mut out = serde_json::Map::new();
        let mut by_project: BTreeMap<i64, Vec<(String, String)>> = BTreeMap::new();
        for raw in conversation_uuids.iter().take(500) {
            let Ok(uuid) = canonical_uuid(raw) else {
                continue;
            };
            match self.records.get(&uuid)? {
                None => {
                    out.insert(
                        raw.clone(),
                        json!({"state": "LOCAL", "epoch": 0, "executor": "local", "read_only": false, "sent_from_here": false}),
                    );
                }
                Some(record) => by_project
                    .entry(record.project_id)
                    .or_default()
                    .push((raw.clone(), uuid)),
            }
        }
        for (project_id, threads) in by_project {
            let now = Instant::now();
            let stalest = {
                let cache = self
                    .status_cache
                    .lock()
                    .unwrap_or_else(std::sync::PoisonError::into_inner);
                threads
                    .iter()
                    .filter_map(|(_, uuid)| match cache.get(uuid) {
                        Some((at, _)) if now.duration_since(*at) < STATUS_CACHE => None,
                        Some((at, _)) => Some((Some(*at), uuid.clone())),
                        None => Some((None, uuid.clone())),
                    })
                    .min()
                    .map(|(_, uuid)| uuid)
            };
            if let Some(uuid) = stalest {
                match self.status(&uuid, Some(project_id)).await {
                    Ok(state) => {
                        self.status_cache
                            .lock()
                            .unwrap_or_else(std::sync::PoisonError::into_inner)
                            .insert(uuid, (Instant::now(), state));
                    }
                    Err(error) => log::warn!("a handoff status was not refreshed: {}", error.code),
                }
            }
            let cache = self
                .status_cache
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            for (raw, uuid) in threads {
                let answer = cache
                    .get(&uuid)
                    .map(|(_, state)| state.clone())
                    .or_else(|| {
                        self.records.get(&uuid).ok().flatten().map(|record| {
                            json!({
                                "state": record.state, "epoch": record.epoch,
                                "executor": executor_of(&record.state),
                                "read_only": record.state != "LOCAL",
                                "sent_from_here": true,
                            })
                        })
                    });
                out.insert(
                    raw,
                    answer.unwrap_or_else(|| json!({"state": "LOCAL", "epoch": 0, "executor": "local", "read_only": false, "sent_from_here": false})),
                );
            }
        }
        Ok(json!({ "statuses": out }))
    }

    /// `handoff_reclaim`: take the thread back (`CLOUD_IDLE` → `RECLAIMING`,
    /// or `reclaim_pending` while a cloud turn runs).
    ///
    /// # Errors
    ///
    /// `handoff_unknown` (not sent from this desktop), the server's
    /// refusals.
    pub async fn reclaim(&self, conversation_uuid: &str) -> Result<Value, HandoffError> {
        let uuid = canonical_uuid(conversation_uuid)?;
        let mut record = self.record(&uuid)?;
        let state = self.server_state(record.project_id, &uuid).await?;
        let epoch = u64_of(&state, "epoch");
        if str_of(&state, "state").as_deref() == Some("RECLAIMING") {
            record.state = "RECLAIMING".into();
            record.epoch = epoch;
            self.records.put(&record)?;
            return Ok(state);
        }
        let answer = self
            .call(
                Method::POST,
                &format!(
                    "/api/v2/elitea_core/workspace_handoff_reclaim/prompt_lib/{}/{uuid}",
                    record.project_id
                ),
                Some(&json!({"epoch": epoch, "device_id": self.device_id()})),
            )
            .await?;
        record.state = str_of(&answer, "state").unwrap_or_else(|| "RECLAIMING".into());
        record.epoch = u64_of(&answer, "epoch");
        self.records.put(&record)?;
        Ok(answer)
    }

    fn record(&self, uuid: &str) -> Result<ThreadRecord, HandoffError> {
        self.records.get(uuid)?.ok_or_else(|| {
            HandoffError::new(
                "handoff_unknown",
                "This thread was not sent to the cloud from this desktop.",
            )
        })
    }

    /// `handoff_preflight`: fetch, verify, replay, compare. Writes nothing
    /// to the folder.
    ///
    /// # Errors
    ///
    /// `handoff_not_reclaimed` before the state is `RECLAIMING`,
    /// `integrity_failed` for a segment that fails verification, store
    /// errors.
    pub async fn preflight(
        &self,
        conversation_uuid: &str,
        up_to_turn: Option<usize>,
    ) -> Result<PreflightAnswer, HandoffError> {
        let uuid = canonical_uuid(conversation_uuid)?;
        let mut record = self.record(&uuid)?;
        let state = self.server_state(record.project_id, &uuid).await?;
        if str_of(&state, "state").as_deref() != Some("RECLAIMING") {
            return Err(HandoffError::new(
                "handoff_not_reclaimed",
                "Take the thread back first; the cloud may still be working on it.",
            ));
        }
        // The release belongs to this epoch. A take-back asked while a cloud
        // turn ran (202 `reclaim_pending`) became RECLAIMING one epoch later,
        // when the turn ended; the record still holds the earlier one.
        let epoch = u64_of(&state, "epoch");
        if record.state != "RECLAIMING" || record.epoch != epoch {
            record.state = "RECLAIMING".into();
            record.epoch = epoch;
            self.records.put(&record)?;
        }
        let policy = self.policy_or_default();
        let (root, _) = self.workspace(&record.workspace_id)?;
        let store = self.store(record.project_id, &uuid).await?;
        let (_plan, segment, bytes) = sync::fetch_latest(&store, 0, self.deps.backoff).await?;
        let config = self.config(&root, &uuid, &policy, Vec::new(), Caps::default());
        let source = self.open(&config)?;
        let send = record.send.clone();
        let path_deny = policy.path_deny.clone();
        let (preflight, files, diffs_truncated) = joined(
            tokio::task::spawn_blocking(move || {
                let preflight = take_back::preflight_up_to(
                    &source,
                    &send,
                    &bytes,
                    &segment.refs,
                    &path_deny,
                    up_to_turn,
                )?;
                let (files, truncated) = preflight_files(&preflight)?;
                Ok::<_, elitea_local_tools::error::ToolError>((preflight, files, truncated))
            })
            .await,
        )?
        .map_err(|e| tool_error(&e))?;
        let id = uuid::Uuid::new_v4().to_string();
        let answer = PreflightAnswer {
            preflight_id: id.clone(),
            report: preflight.report.clone(),
            cloud_turns: preflight.cloud_turns(),
            up_to_turn: preflight.up_to_turn,
            modes: preflight.modes(),
            files,
            diffs_truncated,
        };
        let mut held = self
            .preflights
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        held.retain(|_, other| other.conversation_uuid != uuid);
        held.insert(
            id,
            Held {
                conversation_uuid: uuid,
                workspace_id: record.workspace_id,
                root,
                path_deny: policy.path_deny,
                preflight,
            },
        );
        Ok(answer)
    }

    /// Take-back does not need the send switch (a thread already in the
    /// cloud can always come back), only the folder rules.
    fn policy_or_default(&self) -> LocalWorkPolicy {
        self.deps
            .policy
            .local_work()
            .and_then(|section| serde_json::from_value(section).ok())
            .unwrap_or_default()
    }

    /// `handoff_apply`: apply a preflight (`apply`, `apply_with_markers`,
    /// `worktree`, `discard`), release the thread, import its cloud turns.
    /// `up_to_turn`, when given, must be the preflight's.
    ///
    /// # Errors
    ///
    /// `preflight_unknown`, `folder_changed_since_preflight`,
    /// `workspace_busy`, `invalid_request` (a mode the outcome does not
    /// offer, another turn than the preflight's), git or I/O failures.
    pub async fn apply(
        &self,
        preflight_id: &str,
        mode: Mode,
        up_to_turn: Option<usize>,
    ) -> Result<ApplyAnswer, HandoffError> {
        let held = {
            let mut preflights = self
                .preflights
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            let previewed = preflights
                .get(preflight_id)
                .ok_or_else(|| {
                    HandoffError::new("preflight_unknown", "Preview the take-back again.")
                })?
                .preflight
                .up_to_turn;
            if let Some(turn) = up_to_turn
                && turn != previewed
            {
                return Err(HandoffError::new(
                    "invalid_request",
                    format!(
                        "this preview takes back up to turn {previewed}, not {turn}; preview turn {turn} first"
                    ),
                ));
            }
            preflights.remove(preflight_id).ok_or_else(|| {
                HandoffError::new("preflight_unknown", "Preview the take-back again.")
            })?
        };
        let mut record = self.record(&held.conversation_uuid)?;
        let claim = self.deps.agents.claim_workspace(
            &held.workspace_id,
            "Wait for the running turn to end before taking the thread back.",
        )?;
        let session = if matches!(mode, Mode::Apply | Mode::ApplyWithMarkers) {
            Some(
                self.deps
                    .agents
                    .local_session(&held.workspace_id, held.root.clone())?,
            )
        } else {
            None
        };
        let policy = self.policy_or_default();
        let config = self.config(
            &held.root,
            &held.conversation_uuid,
            &policy,
            Vec::new(),
            Caps::default(),
        );
        let source = self.open(&config)?;
        let (held, applied) = joined(
            tokio::task::spawn_blocking(move || {
                let checkpoints = session.as_ref().map(|session| session.checkpoints());
                let applied =
                    take_back::apply(&source, &held.preflight, mode, &held.path_deny, checkpoints);
                (held, applied)
            })
            .await,
        )?;
        drop(claim);
        let applied = applied.map_err(|e| tool_error(&e))?;
        let outcome = match mode {
            Mode::Apply => "applied",
            Mode::ApplyWithMarkers => "applied_with_markers",
            Mode::Worktree => "worktree",
            Mode::Discard => "discarded",
        };
        let applied_sha = applied.replayed.last().cloned();
        record.pending_release =
            Some(json!({"epoch": record.epoch, "outcome": outcome, "applied_sha": applied_sha}));
        self.records.put(&record)?;
        let released = self.release(&record).await;
        let (workspace_id, conversation_uuid) =
            (held.workspace_id.clone(), held.conversation_uuid.clone());
        let (imported, acknowledged, refused) = match released {
            Ok(Released::Acknowledged(release_epoch)) => {
                let imported = self.import(&record, held, release_epoch).await;
                self.records.remove(&conversation_uuid);
                (imported, true, None)
            }
            Ok(Released::Refused(code)) => {
                // The thread is wherever the server says; LOCAL (an erase
                // won) keeps the folder as applied and imports the turns.
                let local = self.settle_refused_release(&record).await;
                let imported = if local {
                    self.import(&record, held, record.epoch).await
                } else {
                    0
                };
                (imported, local, Some(code))
            }
            Err(_) => (0, false, None),
        };
        // The take-back's release was acknowledged: the cloud's turns are in
        // the conversation now, so the agent's session re-seeds from the
        // server before the next local turn.
        if acknowledged {
            self.deps
                .agents
                .mark_session_stale(&conversation_uuid)
                .await;
        }
        Ok(ApplyAnswer {
            applied: applied.applied,
            outcome,
            report: applied.report,
            checkpoint_id: applied
                .checkpoint
                .map(|seq| format!("{workspace_id}:{seq}")),
            worktree_path: applied
                .worktree_path
                .map(|p| p.to_string_lossy().into_owned()),
            branch: applied.branch,
            imported_turns: imported,
            released: acknowledged,
            release_refused: refused,
            copied: applied.copied,
        })
    }

    /// Send the pending release: acknowledged (the new epoch) or refused
    /// for good; `Err` when it should be sent again.
    async fn release(&self, record: &ThreadRecord) -> Result<Released, HandoffError> {
        let body = record
            .pending_release
            .clone()
            .ok_or_else(|| HandoffError::new("internal", "no release to send"))?;
        let answer = self
            .call(
                Method::POST,
                &format!(
                    "/api/v2/elitea_core/workspace_handoff_release/prompt_lib/{}/{}",
                    record.project_id, record.conversation_uuid
                ),
                Some(&body),
            )
            .await;
        match answer {
            Ok(answer) => Ok(Released::Acknowledged(u64_of(&answer, "epoch"))),
            Err(error) if RELEASE_REFUSED_FOR_GOOD.contains(&error.code.as_str()) => {
                log::warn!(
                    "the handoff release was refused ({}); it is not sent again",
                    error.code
                );
                Ok(Released::Refused(error.code))
            }
            Err(error) => {
                log::warn!(
                    "the handoff release was not acknowledged ({}); it is kept and sent again",
                    error.code
                );
                Err(error)
            }
        }
    }

    async fn retry_release(&self, record: ThreadRecord) {
        match self.release(&record).await {
            Ok(Released::Acknowledged(_)) => {
                self.records.remove(&record.conversation_uuid);
                self.deps
                    .agents
                    .mark_session_stale(&record.conversation_uuid)
                    .await;
            }
            Ok(Released::Refused(_)) => {
                // A take-back that settled to LOCAL imports the cloud's turns,
                // so the session re-seeds before the next local turn.
                if self.settle_refused_release(&record).await {
                    self.deps
                        .agents
                        .mark_session_stale(&record.conversation_uuid)
                        .await;
                }
            }
            Err(_) => {}
        }
    }

    /// After a release refused for good: drop the pending release and follow
    /// the server's state (the record goes when the thread is `LOCAL`).
    /// Whether the thread is `LOCAL` (also when the state cannot be read:
    /// the release will never be acknowledged, so nothing is kept away).
    async fn settle_refused_release(&self, record: &ThreadRecord) -> bool {
        let state = self
            .server_state(record.project_id, &record.conversation_uuid)
            .await
            .ok();
        let name = state
            .as_ref()
            .and_then(|state| str_of(state, "state"))
            .unwrap_or_else(|| "LOCAL".into());
        if name == "LOCAL" {
            self.records.remove(&record.conversation_uuid);
            return true;
        }
        let mut kept = record.clone();
        kept.pending_release = None;
        kept.state = name;
        kept.epoch = state
            .as_ref()
            .map_or(record.epoch, |state| u64_of(state, "epoch"));
        if let Err(error) = self.records.put(&kept) {
            log::warn!("the handoff record was not updated: {}", error.code);
        }
        false
    }

    /// Import the cloud turns into the thread history; how many were added.
    /// `held` is owned, never borrowed across an await (a preflight's
    /// object database is not `Sync`).
    async fn import(&self, record: &ThreadRecord, held: Held, release_epoch: u64) -> usize {
        let Some(history) = self.deps.history.clone() else {
            return 0;
        };
        let owner = match self.deps.agents.history_owner().await {
            Ok(owner) => owner,
            Err(error) => {
                log::warn!("cloud turns not imported: {}", error.code);
                return 0;
            }
        };
        let path = format!(
            "/api/v2/elitea_core/conversation/prompt_lib/{}/{}?messages_limit=1000&sort_order=asc",
            record.project_id, record.conversation_uuid
        );
        let detail = match self.call(Method::GET, &path, None).await {
            Ok(detail) => detail,
            Err(error) => {
                log::warn!("cloud turns not imported: {}", error.code);
                return 0;
            }
        };
        let to = release_epoch.max(record.epoch);
        let from = record.cloud_epoch;
        match tokio::task::spawn_blocking(move || {
            let turns = cloud_turns(&detail, from, to, &held.preflight);
            history.import_turns(&owner, &held.workspace_id, &held.conversation_uuid, &turns)
        })
        .await
        {
            Ok(Ok(added)) => added,
            Ok(Err(error)) => {
                log::warn!("cloud turns not imported: {error}");
                0
            }
            Err(_) => 0,
        }
    }

    /// `handoff_undo`: restore the checkpoint a take-back took.
    ///
    /// # Errors
    ///
    /// `invalid_request` for a malformed id, `workspace_busy`, a failed
    /// restore.
    pub async fn undo(&self, checkpoint_id: &str) -> Result<Vec<String>, HandoffError> {
        let (workspace_id, seq) = checkpoint_id
            .split_once(':')
            .and_then(|(ws, seq)| Some((ws.to_owned(), seq.parse::<u64>().ok()?)))
            .ok_or_else(|| HandoffError::new("invalid_request", "not a take-back checkpoint"))?;
        let (root, _) = self.workspace(&workspace_id)?;
        let claim = self
            .deps
            .agents
            .claim_workspace(&workspace_id, "Wait for the running turn to end.")?;
        let session = self.deps.agents.local_session(&workspace_id, root)?;
        let report =
            joined(tokio::task::spawn_blocking(move || session.checkpoints().restore(seq)).await)?
                .map_err(|e| HandoffError::new("restore_failed", e.message()))?;
        drop(claim);
        let mut paths = report.restored;
        paths.extend(report.deleted);
        Ok(paths)
    }

    /// `handoff_erase`: erase the cloud copy (the folder is kept).
    ///
    /// # Errors
    ///
    /// The platform refused or did not answer.
    pub async fn erase(
        &self,
        conversation_uuid: &str,
        project_id: Option<i64>,
    ) -> Result<Value, HandoffError> {
        let uuid = canonical_uuid(conversation_uuid)?;
        let record = self.records.get(&uuid)?;
        let project_id = record
            .as_ref()
            .map(|r| r.project_id)
            .or(project_id)
            .ok_or_else(|| {
                HandoffError::new(
                    "invalid_request",
                    "project_id is needed for a thread not sent from here",
                )
            })?;
        let answer = self
            .call(
                Method::DELETE,
                &format!("/api/v2/elitea_core/workspace_handoff/prompt_lib/{project_id}/{uuid}"),
                None,
            )
            .await?;
        self.records.remove(&uuid);
        self.preflights
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .retain(|_, held| held.conversation_uuid != uuid);
        Ok(answer)
    }
}

/// The per-file diffs a take-back writes: text only, at most
/// [`PREFLIGHT_DIFF_BYTES`] of diff text (later files keep their status and
/// counts, without the text); whether anything was cut.
fn preflight_files(
    preflight: &Preflight,
) -> elitea_local_tools::error::ToolResult<(Vec<FileChange>, bool)> {
    let mut budget = PREFLIGHT_DIFF_BYTES;
    let mut truncated = false;
    let mut files = Vec::new();
    for (diff, too_large) in preflight.changed_files(PREFLIGHT_DIFF_FILE_BYTES)? {
        let mut change = if too_large {
            let status = match diff.kind {
                elitea_workspace_store::objects::ChangeKind::Added => "added",
                elitea_workspace_store::objects::ChangeKind::Deleted => "deleted",
                _ => "modified",
            };
            truncated = true;
            FileChange {
                path: diff.path,
                status,
                added: 0,
                removed: 0,
                diff: String::new(),
            }
        } else {
            crate::d0::recorder::diff(&diff.path, diff.before.as_deref(), diff.after.as_deref())
        };
        if change.diff.len() > budget {
            change.diff.clear();
            truncated = true;
        } else {
            budget -= change.diff.len();
        }
        files.push(change);
    }
    Ok((files, truncated))
}

/// The refusal for a failed cap check. When the project's remaining cloud
/// storage is the only cap exceeded, the server's own code for it
/// (`workspace_quota_exceeded`, with the figures); any per-segment, file or
/// unpacked cap keeps `handoff_too_large` with the whole breakdown.
fn quota_or_too_large(report: &caps::CapReport) -> HandoffError {
    if let [quota] = report.violations.as_slice()
        && quota.cap == "project_workspace_bytes_left"
    {
        let mut details = serde_json::to_value(report).unwrap_or(Value::Null);
        if let Some(map) = details.as_object_mut() {
            map.insert("bytes_left".into(), json!(quota.limit));
            map.insert("bytes_needed".into(), json!(quota.actual));
        }
        return HandoffError {
            code: "workspace_quota_exceeded".into(),
            message: "This project's cloud storage does not have room for this folder.".into(),
            details,
        };
    }
    HandoffError {
        code: "handoff_too_large".into(),
        message: "The folder is larger than this project allows to send.".into(),
        details: serde_json::to_value(report).unwrap_or(Value::Null),
    }
}

fn refuse_blocked(snap: &Snapshot) -> Result<(), HandoffError> {
    if !snap.findings.is_empty() {
        return Err(HandoffError {
            code: "secret_found".into(),
            message: "Some files look like they hold credentials. Leave them on this computer or edit them, then send again.".into(),
            details: json!({ "findings": snap.findings }),
        });
    }
    if !snap.caps.ok() {
        return Err(HandoffError {
            code: "handoff_too_large".into(),
            message: "The folder is larger than a handoff may be.".into(),
            details: serde_json::to_value(&snap.caps).unwrap_or(Value::Null),
        });
    }
    Ok(())
}

/// The handoff note: what is sent and left behind (the server adds it to
/// the event message; it carries no file contents). The person's local MCP
/// servers (#1230) and hooks (#1237) are among what stays: the cloud runs
/// none of them, nor any language server (`diagnostics`, #1236).
fn handoff_note(snap: &Snapshot, mcp_servers: bool, hooks: bool) -> String {
    let mut note = format!(
        "{} files sent, {} left on the laptop.",
        snap.files,
        snap.excluded.len()
    );
    if let Some(upstream) = &snap.upstream {
        note.push_str(&format!(
            " Upstream: {} at {}.",
            upstream.branch,
            &upstream.sha[..12.min(upstream.sha.len())]
        ));
    }
    if mcp_servers {
        note.push_str(
            " Local MCP servers stay on the laptop: their tools are not available in the cloud.",
        );
    }
    if hooks {
        note.push_str(" Your hooks stay on the laptop: the cloud runs none of them.");
    }
    // The cloud runs no language server (#1236).
    note.push_str(" The diagnostics tool (language servers) is not available in the cloud.");
    snapshot::clean_note(&note)
}

/// The text of a message group's text items.
fn group_text(group: &Value) -> String {
    group
        .get("message_items")
        .and_then(Value::as_array)
        .map(|items| {
            items
                .iter()
                .filter(|item| {
                    item.get("item_type")
                        .and_then(Value::as_str)
                        .is_none_or(|t| t == "text_message")
                })
                .filter_map(|item| {
                    item.get("item_details")
                        .and_then(|d| d.get("content"))
                        .or_else(|| item.get("content"))
                        .and_then(Value::as_str)
                })
                .collect::<Vec<_>>()
                .join("\n")
        })
        .unwrap_or_default()
}

fn parse_time(value: Option<&Value>) -> Option<i64> {
    let text = value?.as_str()?;
    chrono::NaiveDateTime::parse_from_str(text, "%Y-%m-%d %H:%M:%S%.f")
        .ok()
        .map(|t| t.and_utc().timestamp_millis())
        .or_else(|| {
            chrono::DateTime::parse_from_rfc3339(text)
                .ok()
                .map(|t| t.timestamp_millis())
        })
}

/// The cloud turns of a conversation detail (`message_groups`, oldest
/// first): answers whose meta says `executed_by: cloud_executor` and whose
/// `workspace_epoch` lies in `[from, to]`, each with its question (the group
/// it replies to, else the group before it) and its commit's changes.
fn cloud_turns(detail: &Value, from: u64, to: u64, preflight: &Preflight) -> Vec<ImportedTurn> {
    let groups: Vec<&Value> = detail
        .get("message_groups")
        .and_then(Value::as_array)
        .map(|groups| groups.iter().collect())
        .unwrap_or_default();
    let by_id: BTreeMap<i64, &Value> = groups
        .iter()
        .filter_map(|group| Some((group.get("id")?.as_i64()?, *group)))
        .collect();
    let mut out = Vec::new();
    for (index, group) in groups.iter().enumerate() {
        let meta = group.get("meta").cloned().unwrap_or(Value::Null);
        if meta.get("executed_by").and_then(Value::as_str) != Some(EXECUTED_BY_CLOUD) {
            continue;
        }
        let epoch = meta
            .get("workspace_epoch")
            .and_then(Value::as_u64)
            .unwrap_or_default();
        if epoch < from || epoch > to {
            continue;
        }
        let question = group
            .get("reply_to_id")
            .and_then(Value::as_i64)
            .and_then(|id| by_id.get(&id).copied())
            .or_else(|| index.checked_sub(1).and_then(|i| groups.get(i).copied()));
        let answer_uuid = group
            .get("uuid")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned();
        if answer_uuid.is_empty() {
            continue;
        }
        let changes = meta
            .get("workspace_commit")
            .and_then(Value::as_str)
            .and_then(|sha| preflight.commit_changes(sha).ok())
            .unwrap_or_default()
            .into_iter()
            .map(|diff| {
                crate::d0::recorder::diff(&diff.path, diff.before.as_deref(), diff.after.as_deref())
            })
            .collect();
        let finished = parse_time(group.get("created_at")).unwrap_or_default();
        out.push(ImportedTurn {
            turn_id: format!("cloud:{answer_uuid}"),
            prompt: question.map(group_text).unwrap_or_default(),
            answer: group_text(group),
            is_error: meta
                .get("is_error")
                .and_then(Value::as_bool)
                .unwrap_or(false),
            started_at: question
                .and_then(|q| parse_time(q.get("created_at")))
                .unwrap_or(finished),
            finished_at: finished,
            message_ids: question
                .and_then(|q| q.get("uuid").and_then(Value::as_str))
                .map(str::to_owned)
                .into_iter()
                .chain([answer_uuid])
                .collect(),
            changes,
        });
    }
    out
}

/// Host git uses a linked worktree (a take-back's, opened as a workspace)
/// only when its repository is a workspace the person opened (ADR-0034
/// decision 13; `elitea_local_tools::git::set_worktree_trust`).
pub fn trust_open_workspaces(workspaces: Arc<WorkspaceStore>) {
    elitea_local_tools::git::set_worktree_trust(move |top| {
        workspaces.list().is_ok_and(|all| {
            all.iter()
                .any(|workspace| Path::new(&workspace.path) == top)
        })
    });
}

/// The host, as Tauri manages it.
pub struct HandoffState(pub Arc<HandoffHost>);

/// Progress to the main window only.
pub struct MainWindowHandoffEvents(pub tauri::AppHandle);

impl HandoffEvents for MainWindowHandoffEvents {
    fn emit(&self, event: HandoffEvent) {
        use tauri::{Emitter as _, Manager as _};
        if let Err(error) = self.0.emit_to("main", EVENT_NAME, &event) {
            log::warn!("could not deliver a handoff event: {error}");
        }
        if let Some(activity) = self
            .0
            .try_state::<std::sync::Arc<crate::activity::Activity>>()
        {
            activity.send(
                &event.conversation_uuid,
                event.phase,
                event.done,
                event.total,
            );
        }
    }
}

#[tauri::command(rename_all = "snake_case")]
pub async fn handoff_preview(
    state: tauri::State<'_, HandoffState>,
    workspace_id: String,
    conversation_uuid: String,
    leave_on_laptop: Option<Vec<String>>,
) -> Result<Preview, HandoffError> {
    state
        .0
        .preview(
            &workspace_id,
            &conversation_uuid,
            leave_on_laptop.unwrap_or_default(),
        )
        .await
}

#[tauri::command(rename_all = "snake_case")]
pub async fn handoff_send(
    state: tauri::State<'_, HandoffState>,
    workspace_id: String,
    conversation_uuid: String,
    leave_on_laptop: Option<Vec<String>>,
    confirm_includes: Option<bool>,
) -> Result<Sent, HandoffError> {
    state
        .0
        .send(
            &workspace_id,
            &conversation_uuid,
            leave_on_laptop.unwrap_or_default(),
            confirm_includes.unwrap_or(false),
        )
        .await
}

#[tauri::command(rename_all = "snake_case")]
pub fn handoff_cancel(
    state: tauri::State<'_, HandoffState>,
    conversation_uuid: String,
) -> Result<Value, HandoffError> {
    Ok(json!({ "cancelled": state.0.cancel(&conversation_uuid)? }))
}

#[tauri::command(rename_all = "snake_case")]
pub async fn handoff_status_many(
    state: tauri::State<'_, HandoffState>,
    conversation_uuids: Vec<String>,
) -> Result<Value, HandoffError> {
    state.0.status_many(&conversation_uuids).await
}

#[tauri::command(rename_all = "snake_case")]
pub async fn handoff_settings_get(
    state: tauri::State<'_, HandoffState>,
    workspace_id: String,
) -> Result<Value, HandoffError> {
    state.0.settings_get(&workspace_id).await
}

#[tauri::command(rename_all = "snake_case")]
pub async fn handoff_settings_set(
    state: tauri::State<'_, HandoffState>,
    workspace_id: String,
    settings: FolderSettings,
) -> Result<Value, HandoffError> {
    state.0.settings_set(&workspace_id, settings).await
}

#[tauri::command(rename_all = "snake_case")]
pub async fn handoff_status(
    state: tauri::State<'_, HandoffState>,
    conversation_uuid: String,
    project_id: Option<i64>,
) -> Result<Value, HandoffError> {
    state.0.status(&conversation_uuid, project_id).await
}

#[tauri::command(rename_all = "snake_case")]
pub async fn handoff_reclaim(
    state: tauri::State<'_, HandoffState>,
    conversation_uuid: String,
) -> Result<Value, HandoffError> {
    state.0.reclaim(&conversation_uuid).await
}

#[tauri::command(rename_all = "snake_case")]
pub async fn handoff_preflight(
    state: tauri::State<'_, HandoffState>,
    conversation_uuid: String,
    up_to_turn: Option<usize>,
) -> Result<PreflightAnswer, HandoffError> {
    state.0.preflight(&conversation_uuid, up_to_turn).await
}

#[tauri::command(rename_all = "snake_case")]
pub async fn handoff_apply(
    state: tauri::State<'_, HandoffState>,
    preflight_id: String,
    mode: Mode,
    up_to_turn: Option<usize>,
) -> Result<ApplyAnswer, HandoffError> {
    state.0.apply(&preflight_id, mode, up_to_turn).await
}

#[tauri::command(rename_all = "snake_case")]
pub async fn handoff_undo(
    state: tauri::State<'_, HandoffState>,
    checkpoint_id: String,
) -> Result<Value, HandoffError> {
    let restored = state.0.undo(&checkpoint_id).await?;
    Ok(json!({ "restored": restored }))
}

#[tauri::command(rename_all = "snake_case")]
pub async fn handoff_erase(
    state: tauri::State<'_, HandoffState>,
    conversation_uuid: String,
    project_id: Option<i64>,
) -> Result<Value, HandoffError> {
    state.0.erase(&conversation_uuid, project_id).await
}

#[cfg(test)]
#[path = "handoff_tests.rs"]
mod tests;
