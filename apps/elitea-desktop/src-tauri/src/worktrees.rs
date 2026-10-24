//! Task worktrees (#1235, ADR-0029 decision 4: "parallel agents get their
//! own git worktree"): IPC `worktree_*` (IPC.md, "Parallel agents").
//!
//! A task worktree is a linked git worktree of a workspace's repository that
//! the host makes in the workspace's own data,
//! `<app data>/workspaces/<id>/worktrees/<thread>`, on a branch
//! `elitea/<first 12 of the thread>` (`elitea_local_tools::worktree`). It is
//! a workspace of its own (`Workspace::parent_id`: its own session,
//! checkpoints and turn claim, the parent's project), so turns run in
//! several worktrees of one repository, and in the folder, at once.
//!
//! The host records each one in `worktrees.json` in the parent's data
//! (owner-only, `0600` created before it is written, never through a
//! symlink, another user's or a group- or world-readable file refused) and
//! registers it with host git at creation and at start-up
//! ([`elitea_local_tools::git::register_worktree`]): host git accepts a
//! linked worktree only as that record names it, never as the repository
//! declares one.
//!
//! Merging back is `elitea_local_tools::apply`: a preview first (nothing
//! written), then the folder checkpointed and the result written through
//! the confined writer, undoable with `worktree_merge_undo`.

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use adk_core::{ReadonlyContext, Tool, Toolset};
use async_trait::async_trait;
use elitea_agent_runtime::host::{
    HostError as RuntimeError, HostErrorCode, ToolProvider, ToolsetRequest,
};
use elitea_agent_runtime::subagents::ChildIdentity;
use elitea_local_tools::apply::{self, ApplyOutcome, Outcome};
use elitea_local_tools::deny::DenyList;
use elitea_local_tools::error::{ErrorCode, ToolError};
use elitea_local_tools::git::{RegisteredWorktree, Repo, register_worktree};
use elitea_local_tools::provider::LocalToolProvider;
use elitea_local_tools::sandbox::SandboxConfig;
use elitea_local_tools::worktree;
use serde::{Deserialize, Serialize};

use crate::d0::approvals::TurnBinding;
use crate::d0::subagents::ChildCheckouts;
use crate::d0::turn::{AgentHost, ChildHold, TurnError};
use crate::workspaces::{Workspace, WorkspaceStore};

/// The registry file in a workspace's data.
const REGISTRY_FILE: &str = "worktrees.json";
/// The checkpoints merges into a workspace took (`worktree_merge_undo`
/// restores only those), in its data beside the registry.
const MERGES_FILE: &str = "worktree-merges.json";
/// The most merge checkpoints remembered per workspace (the oldest go first).
const MAX_MERGES: usize = 64;
/// The folder in a workspace's data that holds its task worktrees.
const DIR: &str = "worktrees";
/// How long a merge preview is held for its apply.
const PREFLIGHT_TTL: Duration = Duration::from_secs(15 * 60);
/// The most merge previews held at once (the oldest go first).
const MAX_PREFLIGHTS: usize = 32;

/// The folder holding `parent_id`'s task worktrees.
#[must_use]
pub fn worktrees_dir(workspaces: &WorkspaceStore, parent_id: &str) -> PathBuf {
    workspaces.data_dir(parent_id).join(DIR)
}

/// One task worktree as the registry keeps it.
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub struct Record {
    pub thread_uuid: String,
    /// The worktree's own workspace.
    pub workspace_id: String,
    pub path: String,
    /// Its admin directory under the repository's `.git/worktrees/`.
    pub name: String,
    pub admin_dir: String,
    pub branch: String,
    pub base_sha: String,
    pub created_at: u64,
}

/// `worktree_create` and `worktree_list`'s entries.
#[derive(Clone, Debug, Serialize, PartialEq, Eq)]
pub struct Entry {
    pub workspace_id: String,
    pub thread_uuid: String,
    pub path: String,
    pub branch: String,
    pub base_sha: String,
    pub created_at: u64,
    pub ahead: u64,
    pub dirty: u64,
    pub last_turn_at: Option<u64>,
    pub missing: bool,
}

/// `worktree_diff`'s answer.
#[derive(Clone, Debug, Serialize, PartialEq, Eq)]
pub struct Diff {
    pub diff: String,
    pub truncated: bool,
}

/// What a merge-back preview found, as `worktree_merge_preflight` has
/// always answered it (the shared engine's outcome, mapped).
#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum Mode {
    Nothing,
    FastForward,
    Merge,
    Conflicts,
}

impl Mode {
    fn of(report: &ApplyOutcome) -> Self {
        match report.outcome {
            Outcome::Conflicts | Outcome::Error => Self::Conflicts,
            _ if report.changed_paths.is_empty() && report.skipped_paths.is_empty() => {
                Self::Nothing
            }
            Outcome::FastPath => Self::FastForward,
            Outcome::CleanMerge => Self::Merge,
        }
    }
}

/// `worktree_merge_preflight`'s answer.
#[derive(Clone, Debug, Serialize, PartialEq, Eq)]
pub struct MergePreflight {
    pub preflight_id: String,
    pub mode: Mode,
    pub applied_paths: Vec<String>,
    pub conflict_paths: Vec<String>,
    pub skipped_paths: Vec<String>,
    pub branch: String,
}

/// `worktree_merge_apply`'s answer.
#[derive(Clone, Debug, Serialize, PartialEq, Eq)]
pub struct MergeApplied {
    pub written: Vec<String>,
    pub deleted: Vec<String>,
    pub skipped_paths: Vec<String>,
    /// `<workspace id>:<checkpoint>`, for `worktree_merge_undo`.
    pub checkpoint_id: Option<String>,
}

struct Held {
    workspace_id: String,
    branch: String,
    preflight: apply::Preflight,
    at: Instant,
}

/// The task worktrees of every workspace.
pub struct WorktreeHost {
    pub(crate) agents: Arc<AgentHost>,
    workspaces: Arc<WorkspaceStore>,
    preflights: Mutex<HashMap<String, Held>>,
}

fn now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |elapsed| elapsed.as_secs())
}

/// Serialises every read-modify-write of the registry files (both
/// [`REGISTRY_FILE`] and [`MERGES_FILE`], every workspace): the host's calls
/// and the Doctor's prune share it, and with it the one temporary name.
static REGISTRY_LOCK: Mutex<()> = Mutex::new(());

/// `file` in `parent_id`'s data (the default when there is none): owner-only,
/// never through a symlink; another user's or a group- or world-readable
/// file is refused.
fn read_private<T: serde::de::DeserializeOwned + Default>(
    workspaces: &WorkspaceStore,
    parent_id: &str,
    file: &str,
) -> Result<T, TurnError> {
    let path = workspaces.data_dir(parent_id).join(file);
    let opened = match crate::credentials_file::open_no_follow(&path) {
        Ok(opened) => opened,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(T::default()),
        Err(e) => return Err(storage(e)),
    };
    {
        use std::os::unix::fs::MetadataExt as _;
        let meta = opened.metadata().map_err(storage)?;
        if !meta.is_file()
            || meta.uid() != rustix::process::geteuid().as_raw()
            || meta.mode() & 0o077 != 0
        {
            return Err(TurnError::new(
                "storage",
                "the workspace's worktree registry is not private to you; it was not used",
            ));
        }
    }
    serde_json::from_reader(opened)
        .map_err(|_| TurnError::new("storage", "the workspace's worktree registry is damaged"))
}

/// Write `value` to `file` in `parent_id`'s data: a `0600` temporary file
/// created before it is written, then renamed over. The caller holds
/// [`REGISTRY_LOCK`].
fn write_private<T: Serialize>(
    workspaces: &WorkspaceStore,
    parent_id: &str,
    file: &str,
    value: &T,
) -> Result<(), TurnError> {
    let dir = workspaces.data_dir(parent_id);
    crate::credentials_file::create_private_dir(&dir).map_err(storage)?;
    let bytes = serde_json::to_vec_pretty(value).map_err(storage)?;
    let temp = dir.join(format!(".{file}.{}.tmp", std::process::id()));
    let _ = std::fs::remove_file(&temp);
    {
        use std::io::Write as _;
        let mut out = crate::credentials_file::create_private_file(&temp).map_err(storage)?;
        out.write_all(&bytes)
            .and_then(|()| out.sync_all())
            .map_err(storage)?;
    }
    std::fs::rename(&temp, dir.join(file)).map_err(storage)
}

/// Read, change and write `file` in `parent_id`'s data under [`REGISTRY_LOCK`].
fn update_private<T: serde::de::DeserializeOwned + Serialize + Default, R>(
    workspaces: &WorkspaceStore,
    parent_id: &str,
    file: &str,
    change: impl FnOnce(&mut T) -> R,
) -> Result<R, TurnError> {
    let _guard = REGISTRY_LOCK
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    let mut value: T = read_private(workspaces, parent_id, file)?;
    let answer = change(&mut value);
    write_private(workspaces, parent_id, file, &value)?;
    Ok(answer)
}

fn storage(error: impl std::fmt::Display) -> TurnError {
    TurnError::new("storage", error.to_string())
}

fn tool_error(error: &ToolError) -> TurnError {
    let code = match error.code() {
        ErrorCode::UnsafeRepository => "unsafe_repository",
        ErrorCode::InvalidArgument if error.message().starts_with("merge_conflicts") => {
            "merge_conflicts"
        }
        ErrorCode::InvalidArgument => "invalid_request",
        ErrorCode::StaleRead => apply::FOLDER_CHANGED,
        ErrorCode::Unsupported => "not_a_git_repository",
        _ => "worktree_failed",
    };
    TurnError::new(code, error.message())
}

async fn blocking<T: Send + 'static>(
    work: impl FnOnce() -> Result<T, TurnError> + Send + 'static,
) -> Result<T, TurnError> {
    tokio::task::spawn_blocking(work)
        .await
        .map_err(|_| TurnError::new("internal", "the worktree call stopped unexpectedly"))?
}

/// A thread id as the UI sends it, canonical; its simple form names the
/// admin directory and the folder.
fn thread_id(thread_uuid: &str) -> Result<(String, String), TurnError> {
    let uuid = uuid::Uuid::parse_str(thread_uuid)
        .map_err(|_| TurnError::new("invalid_request", "thread_uuid is not a UUID"))?;
    Ok((uuid.hyphenated().to_string(), uuid.simple().to_string()))
}

impl WorktreeHost {
    #[must_use]
    pub fn new(agents: Arc<AgentHost>, workspaces: Arc<WorkspaceStore>) -> Self {
        Self {
            agents,
            workspaces,
            preflights: Mutex::new(HashMap::new()),
        }
    }

    /// The registry of `parent_id` (empty when there is none).
    fn read(&self, parent_id: &str) -> Result<Vec<Record>, TurnError> {
        read_private(&self.workspaces, parent_id, REGISTRY_FILE)
    }

    fn update(
        &self,
        parent_id: &str,
        change: impl FnOnce(&mut Vec<Record>),
    ) -> Result<(), TurnError> {
        update_private(&self.workspaces, parent_id, REGISTRY_FILE, change)
    }

    /// The workspace `parent_id` (not itself a task worktree's).
    fn parent(&self, parent_id: &str) -> Result<Workspace, TurnError> {
        match self.workspaces.get(parent_id).map_err(storage)? {
            Some(workspace) if workspace.parent_id.is_none() => Ok(workspace),
            Some(_) => Err(TurnError::new(
                "invalid_request",
                "A task worktree has no task worktrees of its own.",
            )),
            None => Err(TurnError::new(
                "workspace_unknown",
                "That workspace is not open.",
            )),
        }
    }

    fn record(&self, parent_id: &str, thread_uuid: &str) -> Result<Record, TurnError> {
        let (thread, _) = thread_id(thread_uuid)?;
        self.read(parent_id)?
            .into_iter()
            .find(|record| record.thread_uuid == thread)
            .ok_or_else(|| TurnError::new("worktree_unknown", "No such task worktree."))
    }

    fn entry_of(parent: &Workspace, record: &Record) -> RegisteredWorktree {
        RegisteredWorktree {
            worktree: PathBuf::from(&record.path),
            top: PathBuf::from(&parent.path),
            name: record.name.clone(),
        }
    }

    /// The repository of `parent`'s folder, which must be its top.
    fn main_repo(&self, parent: &Workspace) -> Result<Repo, TurnError> {
        let root = PathBuf::from(&parent.path);
        let repo = Repo::discover(&root, &SandboxConfig::default(), self.agents.host_deny())
            .map_err(|e| tool_error(&e))?
            .ok_or_else(|| {
                TurnError::new(
                    "not_a_git_repository",
                    "The folder is not in a git repository.",
                )
            })?;
        if repo.top() != root {
            return Err(TurnError::new(
                "not_a_git_repository",
                "Task worktrees are made for a folder that is its repository's top.",
            ));
        }
        Ok(repo)
    }

    /// Register every recorded task worktree with host git (start-up).
    pub fn register_all(&self) {
        let Ok(all) = self.workspaces.all() else {
            return;
        };
        for parent in all.iter().filter(|w| w.parent_id.is_none()) {
            match self.read(&parent.id) {
                Ok(records) => {
                    for record in &records {
                        register_worktree(&Self::entry_of(parent, record));
                    }
                }
                Err(error) => log::warn!("a worktree registry was not used: {}", error.message),
            }
        }
    }

    /// Whether a turn of `workspace_id` may give a sub-agent a task
    /// worktree: a workspace of its own (not a task worktree's) in git.
    #[must_use]
    pub fn can_isolate(&self, workspace_id: &str) -> bool {
        self.workspaces
            .get(workspace_id)
            .ok()
            .flatten()
            .is_some_and(|w| w.parent_id.is_none() && Path::new(&w.path).join(".git").is_dir())
    }

    /// `worktree_create`: a task worktree of `parent_id` for `thread_uuid`
    /// at `base` (`head`, the default, or `checkpoint:<n>`: the folder as
    /// its checkpoint `n` holds it, #1234).
    ///
    /// # Errors
    ///
    /// `workspace_unknown`, `invalid_request`, `not_a_git_repository`,
    /// `unsafe_repository`, `worktree_exists`, `worktree_failed`, `storage`.
    pub async fn create(
        self: &Arc<Self>,
        parent_id: &str,
        thread_uuid: &str,
        base: Option<&str>,
    ) -> Result<Entry, TurnError> {
        let (thread, name) = thread_id(thread_uuid)?;
        let parent = self.parent(parent_id)?;
        let base = match base.unwrap_or("head") {
            "head" => "HEAD".to_owned(),
            other => {
                let seq: u64 = other
                    .strip_prefix("checkpoint:")
                    .and_then(|n| n.parse().ok())
                    .ok_or_else(|| {
                        TurnError::new("invalid_request", "base is `head` or `checkpoint:<n>`")
                    })?;
                format!(
                    "{}/{}/{seq}",
                    elitea_local_tools::checkpoint::REF_PREFIX,
                    parent.id
                )
            }
        };
        if self
            .read(&parent.id)?
            .iter()
            .any(|r| r.thread_uuid == thread)
        {
            return Err(TurnError::new(
                "worktree_exists",
                "This thread already has a task worktree.",
            ));
        }
        let this = Arc::clone(self);
        blocking(move || {
            let repo = this.main_repo(&parent)?;
            let dir = worktrees_dir(&this.workspaces, &parent.id);
            crate::credentials_file::create_private_dir(&dir).map_err(storage)?;
            let branch = worktree::branch_name(&name);
            let created = worktree::create(&repo, &dir.join(&name), &name, &branch, &base)
                .map_err(|e| match e.code() {
                    ErrorCode::InvalidArgument if e.message().contains("exists") => {
                        TurnError::new("worktree_exists", e.message())
                    }
                    _ => tool_error(&e),
                })?;
            let path = created.entry.worktree.clone();
            let recorded = this
                .workspaces
                .add_worktree(&parent.id, &path, &thread, &created.branch)
                .map_err(storage)
                .and_then(|workspace| {
                    let record = Record {
                        thread_uuid: thread.clone(),
                        workspace_id: workspace.id,
                        path: path.to_string_lossy().into_owned(),
                        name: name.clone(),
                        admin_dir: format!(".git/worktrees/{name}"),
                        branch: created.branch.clone(),
                        base_sha: created.base_sha.clone(),
                        created_at: now_unix(),
                    };
                    this.update(&parent.id, |records| records.push(record.clone()))?;
                    Ok(record)
                });
            match recorded {
                Ok(record) => Ok(Entry {
                    workspace_id: record.workspace_id,
                    thread_uuid: record.thread_uuid,
                    path: record.path,
                    branch: record.branch,
                    base_sha: record.base_sha,
                    created_at: record.created_at,
                    ahead: 0,
                    dirty: 0,
                    last_turn_at: None,
                    missing: false,
                }),
                Err(error) => {
                    let _ = worktree::remove(&repo, &created.entry, false);
                    Err(error)
                }
            }
        })
        .await
    }

    /// `worktree_list`: `parent_id`'s task worktrees, newest first, each with
    /// its commits ahead of its base and its uncommitted paths.
    ///
    /// # Errors
    ///
    /// `workspace_unknown`, `storage`.
    pub async fn list(self: &Arc<Self>, parent_id: &str) -> Result<Vec<Entry>, TurnError> {
        let parent = self.parent(parent_id)?;
        let records = self.read(&parent.id)?;
        let deny = self.agents.host_deny();
        blocking(move || {
            let mut out: Vec<Entry> = records
                .iter()
                .map(|record| {
                    let entry = Self::entry_of(&parent, record);
                    let missing = !Path::new(&record.path).is_dir();
                    let repo = (!missing)
                        .then(|| {
                            Repo::open_registered_worktree(
                                &entry,
                                &SandboxConfig::default(),
                                deny.clone(),
                            )
                            .ok()
                        })
                        .flatten();
                    let status = repo
                        .as_ref()
                        .and_then(|repo| worktree::status(repo, &record.base_sha).ok())
                        .unwrap_or_default();
                    Entry {
                        workspace_id: record.workspace_id.clone(),
                        thread_uuid: record.thread_uuid.clone(),
                        path: record.path.clone(),
                        branch: record.branch.clone(),
                        base_sha: record.base_sha.clone(),
                        created_at: record.created_at,
                        ahead: status.ahead,
                        dirty: status.dirty,
                        last_turn_at: repo.as_ref().and_then(|repo| {
                            worktree::last_checkpoint_at(repo, &record.workspace_id)
                        }),
                        missing,
                    }
                })
                .collect();
            out.sort_by_key(|record| std::cmp::Reverse(record.created_at));
            Ok(out)
        })
        .await
    }

    /// The worktree's session, under its claim (`message` when it is busy).
    fn worktree_session(
        &self,
        record: &Record,
        message: &str,
    ) -> Result<
        (
            crate::d0::turn::WorkspaceClaim,
            Arc<elitea_local_tools::session::LocalSession>,
        ),
        TurnError,
    > {
        let claim = self.agents.claim_workspace(&record.workspace_id, message)?;
        let session = self
            .agents
            .local_session(&record.workspace_id, PathBuf::from(&record.path))?;
        Ok((claim, session))
    }

    /// `worktree_diff`: the worktree against its base, denied paths left out.
    ///
    /// # Errors
    ///
    /// `worktree_unknown`, `workspace_busy` while a turn runs in it, git
    /// failures.
    pub async fn diff(
        self: &Arc<Self>,
        parent_id: &str,
        thread_uuid: &str,
    ) -> Result<Diff, TurnError> {
        let record = self.record(parent_id, thread_uuid)?;
        let this = Arc::clone(self);
        blocking(move || {
            let (_claim, session) =
                this.worktree_session(&record, "Wait for the task's turn to end.")?;
            let (diff, truncated) =
                worktree::diff(&session, &record.base_sha).map_err(|e| tool_error(&e))?;
            Ok(Diff { diff, truncated })
        })
        .await
    }

    /// `worktree_remove`: its workspace (session, turns, history, data), its
    /// folder and admin directory, and its branch unless commits are on it
    /// (or it cannot be told: a worktree that does not open keeps it).
    /// Refused (`worktree_dirty`) with uncommitted work unless `force`.
    ///
    /// # Errors
    ///
    /// `worktree_unknown`, `worktree_dirty`, `workspace_busy`, `storage`.
    pub async fn remove(
        self: &Arc<Self>,
        parent_id: &str,
        thread_uuid: &str,
        force: bool,
    ) -> Result<(), TurnError> {
        let parent = self.parent(parent_id)?;
        let record = self.record(&parent.id, thread_uuid)?;
        let entry = Self::entry_of(&parent, &record);
        let deny = self.agents.host_deny();
        let status = {
            let entry = entry.clone();
            let base = record.base_sha.clone();
            blocking(move || {
                Ok(
                    Repo::open_registered_worktree(&entry, &SandboxConfig::default(), deny)
                        .ok()
                        .and_then(|repo| worktree::status(&repo, &base).ok()),
                )
            })
            .await?
        };
        if status.is_some_and(|status| status.dirty > 0) && !force {
            return Err(TurnError::new(
                "worktree_dirty",
                "The task worktree has changes that are not committed; remove it anyway?",
            ));
        }
        // First, under its claim (refused while a turn runs there): its
        // folder is not deleted under a running turn. A workspace already
        // gone (an earlier remove that failed later) removes as nothing, so
        // a retry finishes the folder and the record.
        self.agents.remove_workspace(&record.workspace_id).await?;
        let this = Arc::clone(self);
        // A worktree that could not be opened (missing, refused) may still
        // have commits on its branch: kept, as the Doctor's prune keeps it.
        let keep_branch = status.is_none_or(|status| status.ahead > 0);
        blocking(move || {
            match this.main_repo(&parent) {
                Ok(repo) => {
                    worktree::remove(&repo, &entry, keep_branch).map_err(|e| tool_error(&e))?
                }
                // The repository is gone or refused: only what the host owns.
                Err(_) => {
                    elitea_local_tools::git::unregister_worktree(&entry.worktree);
                    if Path::new(&record.path).exists() {
                        std::fs::remove_dir_all(&record.path).map_err(storage)?;
                    }
                }
            }
            this.update(&parent.id, |records| {
                records.retain(|r| r.thread_uuid != record.thread_uuid);
            })
        })
        .await
    }

    /// `worktree_merge_preflight`: what merging the worktree (as it is now,
    /// uncommitted work included) into the folder would do. Writes nothing.
    ///
    /// # Errors
    ///
    /// `worktree_unknown`, `workspace_busy` while a turn runs in the folder
    /// or the worktree, git failures.
    pub async fn merge_preflight(
        self: &Arc<Self>,
        parent_id: &str,
        thread_uuid: &str,
    ) -> Result<MergePreflight, TurnError> {
        let parent = self.parent(parent_id)?;
        let record = self.record(&parent.id, thread_uuid)?;
        let parent_id = parent.id.clone();
        let this = Arc::clone(self);
        let (preflight, branch) = blocking(move || {
            let theirs = {
                let (_claim, session) =
                    this.worktree_session(&record, "Wait for the task's turn to end.")?;
                session
                    .checkpoints()
                    .snapshot_commit()
                    .map_err(|e| tool_error(&e))?
            };
            let _claim = this
                .agents
                .claim_workspace(&parent.id, "Wait for the running turn to end.")?;
            let session = this
                .agents
                .local_session(&parent.id, PathBuf::from(&parent.path))?;
            let preflight = apply::preflight(&session, &theirs, &record.base_sha)
                .map_err(|e| tool_error(&e))?;
            Ok((preflight, record.branch))
        })
        .await?;
        let id = uuid::Uuid::new_v4().simple().to_string();
        let answer = MergePreflight {
            preflight_id: id.clone(),
            mode: Mode::of(&preflight.report),
            // Conflicts are never applied from here (no markers).
            applied_paths: if preflight.report.outcome == Outcome::Conflicts {
                Vec::new()
            } else {
                preflight.report.touched()
            },
            conflict_paths: preflight.report.conflict_paths.clone(),
            skipped_paths: preflight.report.skipped(),
            branch: branch.clone(),
        };
        let mut held = self
            .preflights
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        held.retain(|_, held| held.at.elapsed() < PREFLIGHT_TTL);
        while held.len() >= MAX_PREFLIGHTS {
            let Some(oldest) = held
                .iter()
                .min_by_key(|(_, held)| held.at)
                .map(|(id, _)| id.clone())
            else {
                break;
            };
            held.remove(&oldest);
        }
        held.insert(
            id,
            Held {
                workspace_id: parent_id,
                branch,
                preflight,
                at: Instant::now(),
            },
        );
        Ok(answer)
    }

    /// `worktree_merge_apply`: apply a preview to the folder, after a
    /// checkpoint of it (`worktree_merge_undo` restores it).
    ///
    /// # Errors
    ///
    /// `preflight_expired`, `merge_conflicts`, `folder_changed_since_preflight`,
    /// `workspace_busy`, write failures.
    pub async fn merge_apply(
        self: &Arc<Self>,
        preflight_id: &str,
    ) -> Result<MergeApplied, TurnError> {
        let held = self
            .preflights
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .remove(preflight_id)
            .filter(|held| held.at.elapsed() < PREFLIGHT_TTL)
            .ok_or_else(|| TurnError::new("preflight_expired", "Preview the merge again."))?;
        let parent = self.parent(&held.workspace_id)?;
        let this = Arc::clone(self);
        blocking(move || {
            let _claim = this
                .agents
                .claim_workspace(&parent.id, "Wait for the running turn to end.")?;
            let session = this
                .agents
                .local_session(&parent.id, PathBuf::from(&parent.path))?;
            let applied = apply::apply(
                &session,
                &held.preflight,
                &format!("before merging {}", held.branch),
            )
            .map_err(|e| tool_error(&e))?;
            if let Some(seq) = applied.checkpoint {
                // What `worktree_merge_undo` accepts: this merge's checkpoint.
                update_private(
                    &this.workspaces,
                    &parent.id,
                    MERGES_FILE,
                    |merges: &mut Vec<u64>| {
                        merges.push(seq);
                        let over = merges.len().saturating_sub(MAX_MERGES);
                        merges.drain(..over);
                    },
                )?;
            }
            Ok(MergeApplied {
                written: applied.written,
                deleted: applied.deleted,
                skipped_paths: applied.report.skipped(),
                checkpoint_id: applied.checkpoint.map(|seq| format!("{}:{seq}", parent.id)),
            })
        })
        .await
    }

    /// `worktree_merge_undo`: restore the checkpoint a merge took (only a
    /// merge's, as `worktree_merge_apply` recorded it). When the folder was
    /// checkpointed since (a later turn or merge), restoring reverts that
    /// too: only with `confirm_older`, as `checkpoint_restore` asks.
    ///
    /// # Errors
    ///
    /// `invalid_request` for a malformed id or one no merge took,
    /// `undo_not_latest` (newer checkpoints, no `confirm_older`),
    /// `workspace_busy`, a failed restore.
    pub async fn merge_undo(
        self: &Arc<Self>,
        checkpoint_id: &str,
        confirm_older: bool,
    ) -> Result<Vec<String>, TurnError> {
        let (workspace_id, seq) = checkpoint_id
            .split_once(':')
            .and_then(|(ws, seq)| Some((ws.to_owned(), seq.parse::<u64>().ok()?)))
            .ok_or_else(|| TurnError::new("invalid_request", "not a merge checkpoint"))?;
        let parent = self.parent(&workspace_id)?;
        let this = Arc::clone(self);
        blocking(move || {
            let _claim = this
                .agents
                .claim_workspace(&parent.id, "Wait for the running turn to end.")?;
            let merges: Vec<u64> = read_private(&this.workspaces, &parent.id, MERGES_FILE)?;
            if !merges.contains(&seq) {
                return Err(TurnError::new("invalid_request", "not a merge checkpoint"));
            }
            let session = this
                .agents
                .local_session(&parent.id, PathBuf::from(&parent.path))?;
            if !confirm_older {
                let newer = session
                    .checkpoints()
                    .list()
                    .map_err(|e| tool_error(&e))?
                    .iter()
                    .any(|checkpoint| checkpoint.seq > seq);
                if newer {
                    return Err(TurnError::new(
                        "undo_not_latest",
                        "The folder changed after this merge (a later turn or merge). Undoing \
                         the merge also reverts those changes; ask for that explicitly.",
                    ));
                }
            }
            let report = session
                .restore_checkpoint(seq)
                .map_err(|e| TurnError::new("restore_failed", e.message()))?;
            update_private(
                &this.workspaces,
                &parent.id,
                MERGES_FILE,
                |merges: &mut Vec<u64>| merges.retain(|&taken| taken != seq),
            )?;
            let mut paths = report.restored;
            paths.extend(report.deleted);
            Ok(paths)
        })
        .await
    }
}

/// What the Doctor found wrong with the task worktrees: registry entries
/// whose folder is gone, and admin directories the host made (their
/// `gitdir` points into a workspace's worktrees folder) that no registry
/// names. `(workspace name, what)` for a person.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Stale {
    pub missing: Vec<(String, Record)>,
    pub orphans: Vec<(String, PathBuf, String)>,
}

impl Stale {
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.missing.is_empty() && self.orphans.is_empty()
    }
}

/// Look for stale task worktrees (the Doctor's check; reads only).
#[must_use]
pub fn find_stale(workspaces: &WorkspaceStore, deny: &Arc<DenyList>) -> Stale {
    let mut stale = Stale::default();
    let Ok(all) = workspaces.all() else {
        return stale;
    };
    let host = RegistryReader { workspaces };
    for parent in all.iter().filter(|w| w.parent_id.is_none()) {
        let records = host.read(&parent.id).unwrap_or_default();
        for record in &records {
            if !Path::new(&record.path).is_dir() {
                stale.missing.push((parent.id.clone(), record.clone()));
            }
        }
        let common = Path::new(&parent.path).join(".git");
        if !common.is_dir() {
            continue;
        }
        let Ok(Some(repo)) = Repo::discover(
            Path::new(&parent.path),
            &SandboxConfig::default(),
            deny.clone(),
        ) else {
            continue;
        };
        for (name, folder) in worktree::host_created(&repo, &worktrees_dir(workspaces, &parent.id))
        {
            if !records.iter().any(|record| record.name == name) {
                stale.orphans.push((parent.id.clone(), folder, name));
            }
        }
    }
    stale
}

/// Reads registries without a host (the Doctor's view).
struct RegistryReader<'a> {
    workspaces: &'a WorkspaceStore,
}

impl RegistryReader<'_> {
    fn read(&self, parent_id: &str) -> Option<Vec<Record>> {
        let path = self.workspaces.data_dir(parent_id).join(REGISTRY_FILE);
        let file = crate::credentials_file::open_no_follow(&path).ok()?;
        serde_json::from_reader(file).ok()
    }
}

/// Prune what [`find_stale`] found: a missing worktree's record, admin
/// directory and workspace (through `remove_workspace`, the app's own
/// path); an orphan admin directory and its folder. What was done, for a
/// person.
///
/// # Errors
///
/// A registry cannot be written.
pub fn prune_stale(
    workspaces: &WorkspaceStore,
    deny: &Arc<DenyList>,
    remove_workspace: &dyn Fn(&str) -> Result<(), String>,
) -> Result<String, crate::error::HostError> {
    let stale = find_stale(workspaces, deny);
    let (mut pruned, mut kept) = (0usize, Vec::new());
    for (parent_id, record) in &stale.missing {
        if let Err(why) = remove_workspace(&record.workspace_id) {
            kept.push(format!("{} ({why})", record.branch));
            continue;
        }
        elitea_local_tools::git::unregister_worktree(Path::new(&record.path));
        if let Some(parent) = workspaces.get(parent_id).ok().flatten()
            && let Ok(Some(repo)) = Repo::discover(
                Path::new(&parent.path),
                &SandboxConfig::default(),
                deny.clone(),
            )
        {
            let entry = RegisteredWorktree {
                worktree: PathBuf::from(&record.path),
                top: PathBuf::from(&parent.path),
                name: record.name.clone(),
            };
            let _ = worktree::remove(&repo, &entry, true);
        }
        update_private(
            workspaces,
            parent_id,
            REGISTRY_FILE,
            |records: &mut Vec<Record>| records.retain(|r| r.thread_uuid != record.thread_uuid),
        )
        .map_err(|e| crate::error::HostError::Storage(e.message))?;
        pruned += 1;
    }
    for (parent_id, folder, name) in &stale.orphans {
        let Some(parent) = workspaces.get(parent_id).ok().flatten() else {
            continue;
        };
        if let Ok(Some(repo)) = Repo::discover(
            Path::new(&parent.path),
            &SandboxConfig::default(),
            deny.clone(),
        ) {
            let entry = RegisteredWorktree {
                worktree: folder.clone(),
                top: PathBuf::from(&parent.path),
                name: name.clone(),
            };
            if worktree::remove(&repo, &entry, true).is_ok() {
                pruned += 1;
            }
        }
    }
    log::info!(
        "diagnostics: pruned {pruned} stale task worktree(s), {} refused",
        kept.len()
    );
    let mut message = format!("Pruned {pruned} stale task worktree(s).");
    if !kept.is_empty() {
        message.push_str(&format!(" Not pruned: {}.", kept.join(", ")));
    }
    Ok(message)
}

/// The task worktrees an isolated sub-agent of one turn gets (#1235): each
/// child its own, in the turn's workspace, its tools those of a session
/// there whose approvals are asked in the parent turn.
pub struct WorktreeCheckouts {
    host: Arc<WorktreeHost>,
    workspace_id: String,
    binding: TurnBinding,
    /// The parent turn is in bypass mode: so are its children.
    bypass: bool,
}

impl WorktreeCheckouts {
    #[must_use]
    pub fn new(
        host: Arc<WorktreeHost>,
        workspace_id: &str,
        binding: TurnBinding,
        bypass: bool,
    ) -> Self {
        Self {
            host,
            workspace_id: workspace_id.to_owned(),
            binding,
            bypass,
        }
    }
}

fn runtime_error(error: &TurnError) -> RuntimeError {
    log::warn!(
        "a sub-agent's task worktree: {}: {}",
        error.code,
        error.message
    );
    RuntimeError::new(
        HostErrorCode::DependencyUnavailable,
        "the sub-agent's task worktree could not be made",
    )
    .with_reason("subagent_checkout_unavailable")
}

#[async_trait]
impl ChildCheckouts for WorktreeCheckouts {
    async fn toolsets(
        &self,
        _child: &ChildIdentity,
    ) -> Result<Vec<Arc<dyn Toolset>>, RuntimeError> {
        let thread = uuid::Uuid::new_v4().hyphenated().to_string();
        let entry = self
            .host
            .create(&self.workspace_id, &thread, None)
            .await
            .map_err(|e| runtime_error(&e))?;
        let (session, hold) = self
            .host
            .agents
            .child_session(
                &entry.workspace_id,
                PathBuf::from(&entry.path),
                self.binding.clone(),
                self.bypass,
            )
            .map_err(|e| runtime_error(&e))?;
        let hold = Arc::new(hold);
        let toolsets = LocalToolProvider::new(session)
            .toolsets(&ToolsetRequest {
                toolkits: Vec::new(),
                context_label: "agent".to_owned(),
            })
            .await?;
        Ok(toolsets
            .into_iter()
            .map(|inner| {
                Arc::new(HeldToolset {
                    inner,
                    _hold: hold.clone(),
                }) as Arc<dyn Toolset>
            })
            .collect())
    }
}

/// A toolset that keeps its child's worktree session held while it lives.
struct HeldToolset {
    inner: Arc<dyn Toolset>,
    _hold: Arc<ChildHold>,
}

#[async_trait]
impl Toolset for HeldToolset {
    fn name(&self) -> &str {
        self.inner.name()
    }

    async fn tools(&self, ctx: Arc<dyn ReadonlyContext>) -> adk_core::Result<Vec<Arc<dyn Tool>>> {
        self.inner.tools(ctx).await
    }
}
