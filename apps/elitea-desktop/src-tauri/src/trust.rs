//! Trusted folders (ADR-0061 decision 5i): the person's record that a
//! folder's commands run on the host, outside the sandbox.
//!
//! | File | Holds |
//! |---|---|
//! | `<data>/workspaces/<id>/trust.json` (0600) | the folder's trust: the SHA-256 of its canonical path, when and under which confirmation it was granted, "ask for every command", and a suspension (by the policy or by tampering) |
//! | `<data>/workspaces/trust-ledger.jsonl` (0600, append-only) | every grant, revoke and suspension, each line chained to the previous one by its SHA-256 and naming the SHA-256 of the `trust.json` it wrote; paths appear only as digests |
//!
//! Trust is read through the ledger every time: a `trust.json` whose bytes
//! are not the ones the ledger's last entry for that folder names, a
//! ledger whose chain is broken, or a record with no ledger at all
//! suspends **every** trusted folder (`suspended_by: "tamper"`), keeps the
//! damaged ledger beside the new one, and says so (`trust://event`
//! `{kind: "tamper"}`). A policy that stops allowing trust
//! ([`LocalWorkPolicy::folder_trust_allowed`]) suspends it the next time it
//! is read (`suspended_by: "policy"`); either way the person grants it
//! again, under the policy of the day. A folder moved or replaced (its
//! canonical path's digest differs) is untrusted. A task worktree is never
//! trusted on its own: it inherits its folder's trust (#3015).

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use elitea_local_tools::policy::{LocalWorkPolicy, TrustBlock};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::workspaces::{Workspace, WorkspaceStore};

/// The confirmation text's version the dialog shows; a grant made under
/// another is refused (`confirmation_outdated`).
pub const CONFIRMATION_VERSION: u32 = 1;
/// The app event trust changes are sent on.
pub const EVENT_NAME: &str = "trust://event";
const RECORD: &str = "trust.json";
/// The ledger's key for the person's rules file.
const USER_RULES: &str = "user";
/// A ledger line that records a rules file's digest.
const RULES_EVENT: &str = "rules";
const LEDGER: &str = "trust-ledger.jsonl";

/// Why trust was suspended.
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum SuspendedBy {
    Policy,
    Tamper,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
struct Suspension {
    by: SuspendedBy,
    at: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    file: Option<String>,
}

/// `trust.json`.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
struct Record {
    version: u32,
    workspace_id: String,
    path_sha256: String,
    granted_at: String,
    confirmation_version: u32,
    ask_every_command: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    suspended: Option<Suspension>,
}

/// One ledger line.
#[derive(Clone, Debug, Deserialize, Serialize)]
struct Entry {
    at: String,
    event: String,
    workspace_id: String,
    path_sha256: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    by: Option<SuspendedBy>,
    /// The SHA-256 of the `trust.json` written with it; `None`: removed.
    #[serde(default)]
    record_sha256: Option<String>,
    /// The SHA-256 of the previous line ("" for the first).
    prev: String,
}

/// What the trust state of a folder is.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum TrustState {
    Untrusted,
    Trusted,
    Suspended,
}

/// IPC `trust_status` (`apps/elitea-desktop/IPC.md`, "Trusted folders").
#[derive(Clone, Debug, Serialize)]
pub struct TrustStatus {
    pub workspace_id: String,
    pub state: TrustState,
    pub suspended_by: Option<SuspendedBy>,
    pub tampered_file: Option<String>,
    pub allowed_by_policy: bool,
    pub policy_blocks: Vec<TrustBlock>,
    pub inherited_from: Option<String>,
    pub grantable: bool,
    pub ask_every_command: bool,
    pub env_fallback: bool,
    pub platform: &'static str,
    pub sandbox_available: bool,
    pub granted_at: Option<String>,
    pub last_host_command_at: Option<String>,
    pub confirmation_version: u32,
}

impl TrustStatus {
    /// The folder's trust as the rules engine takes it (`None`: not
    /// trusted now).
    #[must_use]
    pub fn folder_trust(&self) -> Option<elitea_local_tools::approvals::FolderTrust> {
        (self.state == TrustState::Trusted).then_some(elitea_local_tools::approvals::FolderTrust {
            ask_every_command: self.ask_every_command,
        })
    }
}

/// IPC `trust_list`'s rows.
#[derive(Clone, Debug, Serialize)]
pub struct TrustEntry {
    #[serde(flatten)]
    pub status: TrustStatus,
    pub name: String,
    pub path: String,
}

/// A refused grant: its IPC code and message.
#[derive(Debug)]
pub struct Refusal {
    pub code: &'static str,
    pub message: String,
}

impl Refusal {
    fn new(code: &'static str, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
        }
    }
}

type Listener = Box<dyn Fn(TrustChange) + Send + Sync>;

/// What changed: one folder's trust, every folder's (`None`), or the
/// ledger was found tampered with.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum TrustChange {
    Changed(Option<String>),
    Tamper(String),
}

impl TrustChange {
    /// Its `trust://event` payload.
    #[must_use]
    pub fn payload(&self) -> serde_json::Value {
        match self {
            Self::Changed(id) => serde_json::json!({ "kind": "changed", "workspace_id": id }),
            Self::Tamper(file) => serde_json::json!({ "kind": "tamper", "file": file }),
        }
    }
}

/// The trust records of every workspace, and their ledger.
pub struct TrustStore {
    workspaces: Arc<WorkspaceStore>,
    /// The person's rules file (`<config>/rules.json`), watched with the
    /// folders' own while any folder is trusted.
    user_rules: Option<PathBuf>,
    /// Whether the policy in hand is one the server sent (not the defaults
    /// of a missing or unreadable one): only a received policy suspends a
    /// folder for good; an unknown one leaves trust inert for the read.
    policy_known: std::sync::atomic::AtomicBool,
    /// Rules files the app is writing right now (not tampering).
    writing: Mutex<std::collections::HashSet<PathBuf>>,
    /// The app's own directories: never trusted, nor anything inside them.
    refused: Vec<PathBuf>,
    /// Serializes every read-check-write of the files.
    lock: Mutex<()>,
    /// When a host command last ran in each folder (this app session).
    last_host_command: Mutex<HashMap<String, String>>,
    /// Whether the host's environment capture failed (host commands then
    /// get the fallback environment).
    env_fallback: std::sync::atomic::AtomicBool,
    listeners: Mutex<Vec<Listener>>,
}

fn now() -> String {
    chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Secs, true)
}

fn sha256_hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    Sha256::digest(bytes)
        .iter()
        .fold(String::with_capacity(64), |mut out, byte| {
            let _ = write!(out, "{byte:02x}");
            out
        })
}

/// The digest a folder's trust is bound to: its canonical path's.
fn path_digest(path: &Path) -> Option<String> {
    let canonical = std::fs::canonicalize(path).ok()?;
    Some(sha256_hex(canonical.as_os_str().as_encoded_bytes()))
}

const fn platform() -> &'static str {
    if cfg!(target_os = "macos") {
        "macos"
    } else if cfg!(windows) {
        "windows"
    } else {
        "linux"
    }
}

impl TrustStore {
    #[must_use]
    pub fn new(
        workspaces: Arc<WorkspaceStore>,
        user_rules: Option<PathBuf>,
        refused: Vec<PathBuf>,
    ) -> Arc<Self> {
        Arc::new(Self {
            workspaces,
            user_rules,
            policy_known: std::sync::atomic::AtomicBool::new(false),
            writing: Mutex::new(std::collections::HashSet::new()),
            refused,
            lock: Mutex::new(()),
            last_host_command: Mutex::new(HashMap::new()),
            env_fallback: std::sync::atomic::AtomicBool::new(false),
            listeners: Mutex::new(Vec::new()),
        })
    }

    /// Whether the policy passed in next came from the server.
    pub fn set_policy_known(&self, known: bool) {
        self.policy_known
            .store(known, std::sync::atomic::Ordering::Relaxed);
    }

    fn policy_known(&self) -> bool {
        self.policy_known.load(std::sync::atomic::Ordering::Relaxed)
    }

    /// The rules file `key` names (`"user"`, or a workspace id).
    fn rules_path(&self, key: &str) -> Option<PathBuf> {
        if key == USER_RULES {
            self.user_rules.clone()
        } else {
            Some(self.workspaces.data_dir(key).join(crate::rules::RULES_FILE))
        }
    }

    /// The key of the rules file at `path`, when it is one this store
    /// watches.
    fn rules_key(&self, path: &Path) -> Option<String> {
        if self.user_rules.as_deref() == Some(path) {
            return Some(USER_RULES.to_owned());
        }
        let dir = path.parent()?;
        let id = dir.file_name()?.to_str()?;
        (self.workspaces.data_dir(id) == dir).then(|| id.to_owned())
    }

    /// The app is about to write the rules file at `path` (a merge of what
    /// is on disk). The file is checked first, so an outside edit is never
    /// laundered into the ledger by the app's next write: it suspends every
    /// trusted folder.
    pub fn rules_writing(&self, path: &Path) {
        let mut changes = Vec::new();
        if let Ok(_guard) = self.lock.lock() {
            if let Err(file) = self.verify() {
                self.suspend_all(&file);
                changes.push(TrustChange::Tamper(file));
                changes.push(TrustChange::Changed(None));
            }
            if let Ok(mut writing) = self.writing.lock() {
                writing.insert(path.to_path_buf());
            }
        }
        finish(self, changes, None);
    }

    /// The app wrote `bytes` to the rules file at `path`: their digest goes
    /// into the ledger (not a re-read of the file), so only an outside write
    /// is tampering.
    pub fn rules_written(&self, path: &Path, bytes: &[u8]) {
        if let Ok(_guard) = self.lock.lock() {
            if let Some(key) = self.rules_key(path) {
                let _ = self.record_rules(&key, Some(bytes));
            }
            if let Ok(mut writing) = self.writing.lock() {
                writing.remove(path);
            }
        }
    }

    /// The app's write to `path` failed: the file is as it was.
    pub fn rules_written_failed(&self, path: &Path) {
        if let Ok(mut writing) = self.writing.lock() {
            writing.remove(path);
        }
    }

    /// Append the digest of `key`'s rules file: of `written`, or of what is
    /// on disk (the caller holds the lock).
    fn record_rules(&self, key: &str, written: Option<&[u8]>) -> Result<(), Refusal> {
        let digest = match written {
            Some(bytes) => Some(sha256_hex(bytes)),
            None => self
                .rules_path(key)
                .and_then(|path| std::fs::read(path).ok())
                .map(|bytes| sha256_hex(&bytes)),
        };
        self.append(&Entry {
            at: now(),
            event: RULES_EVENT.to_owned(),
            workspace_id: key.to_owned(),
            path_sha256: String::new(),
            by: None,
            record_sha256: digest,
            prev: String::new(),
        })
    }

    /// Call `listener` on every change (the agent host: a session's trust
    /// and its host processes; the app: `trust://event`).
    pub fn on_change(&self, listener: impl Fn(TrustChange) + Send + Sync + 'static) {
        if let Ok(mut listeners) = self.listeners.lock() {
            listeners.push(Box::new(listener));
        }
    }

    fn notify(&self, change: &TrustChange) {
        if let Ok(listeners) = self.listeners.lock() {
            for listener in listeners.iter() {
                listener(change.clone());
            }
        }
    }

    /// Record that the person's login-shell environment could not be read.
    pub fn set_env_fallback(&self, fallback: bool) {
        self.env_fallback
            .store(fallback, std::sync::atomic::Ordering::Relaxed);
    }

    /// Record that a host command ran in `workspace_id` now.
    pub fn host_command_ran(&self, workspace_id: &str) {
        // A task worktree's commands count for the folder it came from.
        let owner = self
            .workspace(workspace_id)
            .ok()
            .and_then(|workspace| self.owner(&workspace))
            .map_or_else(|| workspace_id.to_owned(), |owner| owner.id);
        if let Ok(mut last) = self.last_host_command.lock() {
            last.insert(owner, now());
        }
    }

    fn ledger_path(&self) -> PathBuf {
        self.workspaces
            .data_dir("x")
            .parent()
            .map_or_else(|| PathBuf::from(LEDGER), |dir| dir.join(LEDGER))
    }

    fn record_path(&self, workspace_id: &str) -> PathBuf {
        self.workspaces.data_dir(workspace_id).join(RECORD)
    }

    fn workspace(&self, workspace_id: &str) -> Result<Workspace, Refusal> {
        self.workspaces
            .get(workspace_id)
            .map_err(|e| Refusal::new("storage", e.to_string()))?
            .ok_or_else(|| Refusal::new("workspace_unknown", "That workspace is not open."))
    }

    /// The folder whose trust `workspace` has: its own, or a task
    /// worktree's root folder's.
    fn owner(&self, workspace: &Workspace) -> Option<Workspace> {
        let mut current = workspace.clone();
        for _ in 0..8 {
            let Some(parent) = current.parent_id.clone() else {
                return Some(current);
            };
            current = self.workspaces.get(&parent).ok().flatten()?;
        }
        None
    }

    /// `workspace_id`'s trust under `policy` (suspending it when the
    /// policy no longer allows it, or when the files were tampered with).
    ///
    /// # Errors
    ///
    /// `workspace_unknown`, `storage`.
    pub fn status(
        &self,
        workspace_id: &str,
        policy: &LocalWorkPolicy,
    ) -> Result<TrustStatus, Refusal> {
        let workspace = self.workspace(workspace_id)?;
        let owner = self.owner(&workspace);
        let inherited_from = owner
            .as_ref()
            .filter(|owner| owner.id != workspace.id)
            .map(|owner| owner.id.clone());
        let record = match &owner {
            Some(owner) => self.checked_record(owner, policy),
            None => None,
        };
        let allowed = policy.folder_trust_allowed();
        // An unknown policy (none received yet, or unreadable) suspends
        // nothing on disk: trust is inert for this read only.
        let suspension = record
            .as_ref()
            .and_then(|r| r.suspended.clone())
            .or_else(|| {
                (record.is_some() && !allowed).then(|| Suspension {
                    by: SuspendedBy::Policy,
                    at: now(),
                    file: None,
                })
            });
        let state = match (&record, &suspension) {
            (None, _) => TrustState::Untrusted,
            (Some(_), Some(_)) => TrustState::Suspended,
            (Some(_), None) => TrustState::Trusted,
        };
        let last = self.last_host_command.lock().ok().and_then(|last| {
            last.get(owner.as_ref().map_or(workspace_id, |o| &o.id))
                .cloned()
        });
        Ok(TrustStatus {
            workspace_id: workspace_id.to_owned(),
            state,
            suspended_by: suspension.as_ref().map(|s| s.by),
            tampered_file: suspension.and_then(|s| s.file),
            allowed_by_policy: allowed,
            policy_blocks: policy.folder_trust_blocks(),
            grantable: allowed && inherited_from.is_none() && self.refusal(&workspace).is_none(),
            inherited_from,
            ask_every_command: record.as_ref().is_some_and(|r| r.ask_every_command),
            env_fallback: self.env_fallback.load(std::sync::atomic::Ordering::Relaxed),
            platform: platform(),
            // Windows has no sandbox (ADR-0061 decision 5j).
            sandbox_available: !cfg!(windows),
            granted_at: record.as_ref().map(|r| r.granted_at.clone()),
            last_host_command_at: last,
            confirmation_version: CONFIRMATION_VERSION,
        })
    }

    /// Every open workspace's trust (IPC `trust_list`): the trusted and
    /// suspended ones.
    ///
    /// # Errors
    ///
    /// `storage`.
    pub fn list(&self, policy: &LocalWorkPolicy) -> Result<Vec<TrustEntry>, Refusal> {
        let all = self
            .workspaces
            .list()
            .map_err(|e| Refusal::new("storage", e.to_string()))?;
        Ok(all
            .into_iter()
            .filter(|workspace| workspace.parent_id.is_none())
            .filter_map(|workspace| {
                let status = self.status(&workspace.id, policy).ok()?;
                (status.state != TrustState::Untrusted).then(|| TrustEntry {
                    status,
                    name: workspace.name.clone(),
                    path: workspace.path.clone(),
                })
            })
            .collect())
    }

    /// Why `workspace` can never be trusted, or `None`.
    fn refusal(&self, workspace: &Workspace) -> Option<Refusal> {
        if workspace.parent_id.is_some() {
            return Some(Refusal::new(
                "trust_refused",
                "A task worktree has its folder's trust; trust the folder instead.",
            ));
        }
        let path = Path::new(&workspace.path);
        let Ok(canonical) = std::fs::canonicalize(path) else {
            return Some(Refusal::new("trust_refused", "The folder cannot be found."));
        };
        if canonical.parent().is_none()
            || std::env::home_dir()
                .is_some_and(|home| std::fs::canonicalize(home).is_ok_and(|home| home == canonical))
        {
            return Some(Refusal::new(
                "trust_refused",
                "The filesystem root and your home folder cannot be trusted; open a project folder.",
            ));
        }
        if self.refused.iter().any(|dir| {
            let dir = std::fs::canonicalize(dir).unwrap_or_else(|_| dir.clone());
            canonical.starts_with(&dir) || dir.starts_with(&canonical)
        }) {
            return Some(Refusal::new(
                "trust_refused",
                "A folder holding the app's own data cannot be trusted.",
            ));
        }
        // A symlink to another open workspace would carry that folder's
        // trust under another name.
        if path
            .symlink_metadata()
            .is_ok_and(|meta| meta.file_type().is_symlink())
            && self.workspaces.list().is_ok_and(|all| {
                all.iter().any(|other| {
                    other.id != workspace.id
                        && std::fs::canonicalize(&other.path).is_ok_and(|o| o == canonical)
                })
            })
        {
            return Some(Refusal::new(
                "trust_refused",
                "This folder is a link to another open folder; trust that one instead.",
            ));
        }
        None
    }

    /// Grant trust to `workspace_id` (IPC `trust_grant`).
    ///
    /// # Errors
    ///
    /// `trust_not_allowed` (the policy), `trust_refused` (the folder),
    /// `confirmation_outdated`, `workspace_unknown`, `storage`.
    pub fn grant(
        &self,
        workspace_id: &str,
        policy: &LocalWorkPolicy,
        confirmation_version: u32,
        ask_every_command: bool,
    ) -> Result<TrustStatus, Refusal> {
        if !policy.folder_trust_allowed() {
            return Err(Refusal::new(
                "trust_not_allowed",
                "Your organisation's policy does not allow running commands outside the sandbox.",
            ));
        }
        if confirmation_version != CONFIRMATION_VERSION {
            return Err(Refusal::new(
                "confirmation_outdated",
                "The confirmation you saw is out of date; read it again.",
            ));
        }
        let workspace = self.workspace(workspace_id)?;
        if let Some(refusal) = self.refusal(&workspace) {
            return Err(refusal);
        }
        let digest = path_digest(Path::new(&workspace.path))
            .ok_or_else(|| Refusal::new("trust_refused", "The folder cannot be found."))?;
        {
            let _guard = self.lock.lock().map_err(|_| poisoned())?;
            let record = Record {
                version: 1,
                workspace_id: workspace_id.to_owned(),
                path_sha256: digest,
                granted_at: now(),
                confirmation_version,
                ask_every_command,
                suspended: None,
            };
            self.write(&record, "granted", None)?;
            // The rules it is granted with: from now on only the app
            // changes them.
            self.record_rules(workspace_id, None)?;
            self.record_rules(USER_RULES, None)?;
        }
        self.notify(&TrustChange::Changed(Some(workspace_id.to_owned())));
        self.status(workspace_id, policy)
    }

    /// Revoke `workspace_id`'s trust (IPC `trust_revoke`); revoking an
    /// untrusted folder changes nothing.
    ///
    /// # Errors
    ///
    /// `workspace_unknown`, `storage`.
    pub fn revoke(
        &self,
        workspace_id: &str,
        policy: &LocalWorkPolicy,
    ) -> Result<TrustStatus, Refusal> {
        let workspace = self.workspace(workspace_id)?;
        let path = self.record_path(workspace_id);
        {
            let _guard = self.lock.lock().map_err(|_| poisoned())?;
            if path.exists() {
                let digest = path_digest(Path::new(&workspace.path)).unwrap_or_default();
                std::fs::remove_file(&path).map_err(|e| Refusal::new("storage", e.to_string()))?;
                self.append(&Entry {
                    at: now(),
                    event: "revoked".to_owned(),
                    workspace_id: workspace_id.to_owned(),
                    path_sha256: digest,
                    by: None,
                    record_sha256: None,
                    prev: String::new(),
                })?;
            }
        }
        self.notify(&TrustChange::Changed(Some(workspace_id.to_owned())));
        self.status(workspace_id, policy)
    }

    /// The policy changed: suspend every trusted folder it no longer
    /// allows (called after the app stores a new policy).
    pub fn policy_changed(&self, policy: &LocalWorkPolicy) {
        if policy.folder_trust_allowed() {
            return;
        }
        let Ok(all) = self.workspaces.list() else {
            return;
        };
        for workspace in all.iter().filter(|w| w.parent_id.is_none()) {
            let _ = self.checked_record(workspace, policy);
        }
    }

    /// `owner`'s record as the ledger vouches for it, suspended first when
    /// the policy no longer allows it; `None`: not trusted (or moved).
    fn checked_record(&self, owner: &Workspace, policy: &LocalWorkPolicy) -> Option<Record> {
        let mut changes = Vec::new();
        let record = 'checked: {
            let Ok(_guard) = self.lock.lock() else {
                break 'checked None;
            };
            if let Err(file) = self.verify() {
                self.suspend_all(&file);
                changes.push(TrustChange::Tamper(file));
                changes.push(TrustChange::Changed(None));
            }
            let Ok(Some(bytes)) = crate::rules::read_private(&self.record_path(&owner.id)) else {
                break 'checked None;
            };
            let Ok(mut record) = serde_json::from_slice::<Record>(&bytes) else {
                break 'checked None;
            };
            if record.workspace_id != owner.id
                || path_digest(Path::new(&owner.path)).as_deref() != Some(&record.path_sha256)
            {
                break 'checked None;
            }
            if record.suspended.is_none() && !policy.folder_trust_allowed() && self.policy_known() {
                record.suspended = Some(Suspension {
                    by: SuspendedBy::Policy,
                    at: now(),
                    file: None,
                });
                if self
                    .write(&record, "suspended", Some(SuspendedBy::Policy))
                    .is_ok()
                {
                    changes.push(TrustChange::Changed(Some(owner.id.clone())));
                }
            }
            Some(record)
        };
        finish(self, changes, record)
    }

    /// Check the ledger's chain and that every record is the one it names.
    /// `Err`: the file found tampered with.
    fn verify(&self) -> Result<(), String> {
        let ledger = self.ledger_path();
        let text = match crate::rules::read_private(&ledger) {
            Ok(Some(bytes)) => String::from_utf8(bytes).map_err(|_| ledger_name(&ledger))?,
            Ok(None) => String::new(),
            Err(_) => return Err(ledger_name(&ledger)),
        };
        let mut prev = String::new();
        let mut latest: HashMap<String, Option<String>> = HashMap::new();
        let mut rules: HashMap<String, Option<String>> = HashMap::new();
        for line in text.lines() {
            let entry: Entry = serde_json::from_str(line).map_err(|_| ledger_name(&ledger))?;
            if entry.prev != prev {
                return Err(ledger_name(&ledger));
            }
            prev = sha256_hex(line.as_bytes());
            if entry.event == RULES_EVENT {
                rules.insert(entry.workspace_id, entry.record_sha256);
            } else {
                latest.insert(entry.workspace_id.clone(), entry.record_sha256);
            }
        }
        let all = self.workspaces.list().map_err(|_| ledger_name(&ledger))?;
        let mut trusted = Vec::new();
        for workspace in all {
            let path = self.record_path(&workspace.id);
            let named = latest.get(&workspace.id).cloned().flatten();
            match crate::rules::read_private(&path) {
                Ok(None) => {}
                Ok(Some(bytes)) if named.as_deref() == Some(&sha256_hex(&bytes)) => {
                    if serde_json::from_slice::<Record>(&bytes)
                        .is_ok_and(|record| record.suspended.is_none())
                    {
                        trusted.push(workspace.id.clone());
                    }
                }
                _ => return Err(format!("workspaces/{}/{RECORD}", workspace.id)),
            }
        }
        // While a folder is trusted, its rules and the person's change only
        // through the app.
        if !trusted.is_empty() {
            trusted.push(USER_RULES.to_owned());
        }
        let writing = self.writing.lock().map(|w| w.clone()).unwrap_or_default();
        for key in trusted {
            let Some(path) = self.rules_path(&key) else {
                continue;
            };
            if writing.contains(&path) {
                continue;
            }
            let digest = std::fs::read(&path).ok().map(|bytes| sha256_hex(&bytes));
            if rules.get(&key).cloned().flatten() != digest {
                return Err(if key == USER_RULES {
                    crate::rules::RULES_FILE.to_owned()
                } else {
                    format!("workspaces/{key}/{}", crate::rules::RULES_FILE)
                });
            }
        }
        Ok(())
    }

    /// Tampering found in `file`: keep the damaged ledger aside, and write
    /// every record still there as suspended in a new ledger.
    fn suspend_all(&self, file: &str) {
        log::warn!(
            "the trust ledger or a trust record was changed outside the app ({file}); every trusted folder is suspended"
        );
        let ledger = self.ledger_path();
        if ledger.exists() {
            let aside =
                ledger.with_extension(format!("tampered-{}.jsonl", chrono::Utc::now().timestamp()));
            let _ = std::fs::rename(&ledger, aside);
        }
        let Ok(all) = self.workspaces.list() else {
            return;
        };
        for workspace in all {
            let path = self.record_path(&workspace.id);
            let Ok(Some(bytes)) = crate::rules::read_private(&path).or_else(|_| {
                // A record that cannot be read safely is removed.
                let _ = std::fs::remove_file(&path);
                Ok::<_, String>(None)
            }) else {
                continue;
            };
            let Ok(mut record) = serde_json::from_slice::<Record>(&bytes) else {
                let _ = std::fs::remove_file(&path);
                continue;
            };
            record.workspace_id.clone_from(&workspace.id);
            record.suspended = Some(Suspension {
                by: SuspendedBy::Tamper,
                at: now(),
                file: Some(file.to_owned()),
            });
            if self
                .write(&record, "suspended", Some(SuspendedBy::Tamper))
                .is_err()
            {
                let _ = std::fs::remove_file(&path);
            }
        }
    }

    /// Write `record` and its ledger line (the caller holds the lock).
    fn write(&self, record: &Record, event: &str, by: Option<SuspendedBy>) -> Result<(), Refusal> {
        let bytes = serde_json::to_vec_pretty(record)
            .map_err(|e| Refusal::new("storage", e.to_string()))?;
        crate::rules::write_private(&self.record_path(&record.workspace_id), &bytes)
            .map_err(|e| Refusal::new("storage", e.to_string()))?;
        self.append(&Entry {
            at: now(),
            event: event.to_owned(),
            workspace_id: record.workspace_id.clone(),
            path_sha256: record.path_sha256.clone(),
            by,
            record_sha256: Some(sha256_hex(&bytes)),
            prev: String::new(),
        })
    }

    /// Append `entry`, chained to the ledger's last line.
    fn append(&self, entry: &Entry) -> Result<(), Refusal> {
        let ledger = self.ledger_path();
        let mut text = match crate::rules::read_private(&ledger) {
            Ok(Some(bytes)) => String::from_utf8(bytes).unwrap_or_default(),
            _ => String::new(),
        };
        let prev = text
            .lines()
            .last()
            .map(|line| sha256_hex(line.as_bytes()))
            .unwrap_or_default();
        let line = serde_json::to_string(&Entry {
            prev,
            ..entry.clone()
        })
        .map_err(|e| Refusal::new("storage", e.to_string()))?;
        text.push_str(&line);
        text.push('\n');
        crate::rules::write_private(&ledger, text.as_bytes())
            .map_err(|e| Refusal::new("storage", e.to_string()))
    }
}

fn finish(store: &TrustStore, changes: Vec<TrustChange>, record: Option<Record>) -> Option<Record> {
    for change in &changes {
        store.notify(change);
    }
    record
}

fn ledger_name(_path: &Path) -> String {
    format!("workspaces/{LEDGER}")
}

fn poisoned() -> Refusal {
    Refusal::new("internal", "the trust store is poisoned")
}

#[cfg(test)]
mod tests;
