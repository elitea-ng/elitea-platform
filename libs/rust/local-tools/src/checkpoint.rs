//! Checkpoints: before a turn that writes, the host snapshots the
//! workspace, so the person can undo the turn (or one file), also after a
//! restart (ADR-0029 decision 4).
//!
//! **In a git work tree** a checkpoint is a commit on a private ref,
//! `refs/elitea/checkpoints/<session>/<n>`, built through a temporary index
//! (`GIT_INDEX_FILE`): `read-tree HEAD`, `add -A` in the workspace (tracked
//! files, deletions, and untracked files that are not ignored),
//! `write-tree`, `commit-tree` with HEAD as parent, `update-ref`. The
//! person's index, HEAD, branches and stash are never touched, and the ref
//! keeps the objects alive across `gc` and restarts (up to
//! [`CheckpointLimits::keep`] per session and [`CheckpointLimits::max_age`];
//! older ones are pruned). Files over [`CheckpointLimits::max_file_bytes`]
//! and special files (FIFOs, sockets, devices) are left out, the large ones
//! recorded in the commit so a restore leaves them alone; too many
//! untracked bytes and the turn runs without a checkpoint. Restore diffs the
//! checkpoint's tree against a fresh snapshot of now: files that differ or
//! disappeared are written back with `checkout-index` (from another
//! temporary index), files created since are deleted. A file is only
//! deleted when the checkpoint's own ignore rules (its `.gitignore` files)
//! did not ignore it: one ignored then and un-ignored during the turn was
//! not snapshotted, so it may well be older than the turn, and is kept.
//!
//! **Elsewhere** a checkpoint is a manifest (path, SHA-256, mode) in the
//! host's data directory plus content-addressed copies of the files
//! (`objects/<sha256>`, shared between checkpoints), walking the folder with
//! `.gitignore` rules honoured. Bounded: [`MAX_COPY_FILES`],
//! [`MAX_COPY_BYTES`]; files over [`MAX_COPY_FILE_BYTES`], and files that
//! could not be read, are recorded as skipped and left alone by a restore.
//! Deletions follow the checkpoint's ignore rules as in git. A restore
//! never writes through a symlink: one the turn put where the checkpoint
//! had a file or a directory is removed and the file or directory
//! recreated. Sessions of one folder share `objects/`: creating, pruning
//! and restoring hold an exclusive lock on the folder's store, and every
//! file there is written to a temporary name and renamed (a manifest is
//! synced to disk first).
//!
//! Restores write through a [`Workspace`] without `path_deny` (it is the
//! person's undo, not the agent's write), so they are still confined to the
//! folder and never follow a symlink out of it.
//!
//! Every git call goes through [`crate::git`]'s hardening: a repository
//! that could run its own code in the host's git is refused
//! ([`ErrorCode::UnsafeRepository`]) and checkpointed by copy instead
//! ([`Checkpoints::git_refusal`] says why), and each git process runs in a
//! sandbox that may write only the temporary index, `objects/` and
//! `refs/elitea/` (the workspace too, for a restore).

use std::collections::{BTreeMap, BTreeSet};
use std::ffi::OsStr;
use std::fs::File;
use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use ignore::gitignore::{Gitignore, GitignoreBuilder};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::clone_file;
use crate::deny::DenyList;
use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::folder_scan::{self, FileStat, PlaceholderTest, ScanRules};
use crate::git::Repo;
use crate::sandbox::SandboxConfig;
use crate::workspace::{EntryKind, Workspace, WsPath, is_protected_name};

/// The ref namespace checkpoints live under.
pub const REF_PREFIX: &str = "refs/elitea/checkpoints";

/// The most files a copy checkpoint takes.
pub const MAX_COPY_FILES: usize = 50_000;
/// The most bytes a copy checkpoint takes.
pub const MAX_COPY_BYTES: u64 = 1024 * 1024 * 1024;
/// Files larger than this are skipped by copy checkpoints.
pub const MAX_COPY_FILE_BYTES: u64 = 64 * 1024 * 1024;

/// What one session's checkpoints may hold, and for how long.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CheckpointLimits {
    /// Files larger than this are left out of a checkpoint (recorded as
    /// skipped, and left alone by a restore).
    pub max_file_bytes: u64,
    /// A git checkpoint stores untracked files as new objects: more than
    /// this many bytes of them and the turn runs without a checkpoint.
    pub max_untracked_bytes: u64,
    /// The same, in files.
    pub max_untracked_files: usize,
    /// A copy checkpoint takes at most this many files ([`MAX_COPY_FILES`]):
    /// more and the folder cannot be checkpointed.
    pub max_copy_files: usize,
    /// The same, in bytes ([`MAX_COPY_BYTES`]).
    pub max_copy_bytes: u64,
    /// Checkpoints kept per session (the oldest are pruned).
    pub keep: usize,
    /// Checkpoints older than this are pruned (the newest is always kept).
    pub max_age: Duration,
}

impl Default for CheckpointLimits {
    fn default() -> Self {
        Self {
            max_file_bytes: MAX_COPY_FILE_BYTES,
            max_untracked_bytes: 512 * 1024 * 1024,
            max_untracked_files: MAX_COPY_FILES,
            max_copy_files: MAX_COPY_FILES,
            max_copy_bytes: MAX_COPY_BYTES,
            keep: 50,
            max_age: Duration::from_hours(30 * 24),
        }
    }
}

/// How old a `<ref>.lock` must be before pruning treats it as left by a
/// crashed writer and takes it over (#2087). A live git `update-ref` holds
/// its lock for milliseconds; ten minutes is far beyond any writer.
const STALE_LOCK_AGE: Duration = Duration::from_mins(10);

/// The commit-message trailer listing the paths a git checkpoint skipped.
const SKIPPED_TRAILER: &str = "Elitea-Skipped: ";

static TEMP_COUNTER: AtomicU64 = AtomicU64::new(0);

/// One checkpoint.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize)]
pub struct CheckpointInfo {
    pub seq: u64,
    pub label: String,
    pub created_unix: u64,
    /// The commit id (git) or the manifest name (copy).
    pub id: String,
    /// Files left out because they are over the size limit
    /// ([`CheckpointLimits::max_file_bytes`]). A restore leaves them alone,
    /// so a turn's change to one cannot be undone. At most
    /// [`MAX_REPORTED`] are named; [`Self::too_large_count`] counts all.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub too_large: Vec<String>,
    #[serde(skip_serializing_if = "is_zero")]
    pub too_large_count: usize,
    /// Cloud placeholders left out without being opened (opening one
    /// downloads it, see [`folder_scan::is_placeholder`]). A restore leaves
    /// them alone. At most [`MAX_REPORTED`] named.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub placeholders: Vec<String>,
    #[serde(skip_serializing_if = "is_zero")]
    pub placeholder_count: usize,
}

/// The most paths a [`CheckpointInfo`] names per list.
pub const MAX_REPORTED: usize = 50;

#[allow(clippy::trivially_copy_pass_by_ref)] // serde's skip_serializing_if takes a reference
fn is_zero(count: &usize) -> bool {
    *count == 0
}

impl CheckpointInfo {
    /// Name what was left out: the first [`MAX_REPORTED`] of each list.
    fn with_left_out<'a>(
        mut self,
        too_large: impl IntoIterator<Item = &'a String>,
        placeholders: impl IntoIterator<Item = &'a String>,
    ) -> Self {
        for path in too_large {
            self.too_large_count += 1;
            if self.too_large.len() < MAX_REPORTED {
                self.too_large.push(path.clone());
            }
        }
        for path in placeholders {
            self.placeholder_count += 1;
            if self.placeholders.len() < MAX_REPORTED {
                self.placeholders.push(path.clone());
            }
        }
        self
    }
}

/// The file in a folder's copy store that remembers each stored file's
/// stat and digest, so an unchanged file is not read or hashed again.
const STAT_CACHE: &str = "stat-cache.json";

/// A file modified this recently is hashed again even when its stat is
/// unchanged: a write in the same tick as the stat leaves the stat equal.
const RACY_WINDOW_NS: i128 = 2_000_000_000;

/// `bytes` for a person: `1.2 GiB`, `340 MiB`, `12 KiB`.
#[must_use]
pub fn human_bytes(bytes: u64) -> String {
    const UNITS: [&str; 4] = ["KiB", "MiB", "GiB", "TiB"];
    if bytes < 1024 {
        return format!("{bytes} bytes");
    }
    #[allow(clippy::cast_precision_loss)] // a rounded size for a message
    let mut value = bytes as f64 / 1024.0;
    let mut unit = 0;
    while value >= 1024.0 && unit + 1 < UNITS.len() {
        value /= 1024.0;
        unit += 1;
    }
    if value >= 10.0 {
        format!("{value:.0} {}", UNITS[unit])
    } else {
        format!("{value:.1} {}", UNITS[unit])
    }
}

#[cfg(test)]
thread_local! {
    /// The files copy checkpoints on this thread read into the store
    /// (tests only: an unchanged file must not be among them).
    static STORED: std::cell::RefCell<Vec<String>> = const { std::cell::RefCell::new(Vec::new()) };
}

/// Whether `dir` is inside a git work tree (a `.git` in it or above).
fn in_git_tree(dir: &Path) -> bool {
    dir.ancestors().any(|dir| dir.join(".git").exists())
}

/// What a restore changed.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize)]
pub struct RestoreReport {
    /// Files written back.
    pub restored: Vec<String>,
    /// Files deleted (created after the checkpoint).
    pub deleted: Vec<String>,
}

fn now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |elapsed| elapsed.as_secs())
}

fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    bytes.iter().fold(String::new(), |mut out, byte| {
        let _ = write!(out, "{byte:02x}");
        out
    })
}

/// Whether `session` is usable in a ref and a directory name.
fn check_session(session: &str) -> ToolResult<()> {
    let valid = !session.is_empty()
        && session.len() <= 64
        && session
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_')
        && !session.starts_with('-');
    if valid {
        Ok(())
    } else {
        Err(ToolError::invalid(
            "a session id is 1-64 letters, digits, `-` or `_`",
        ))
    }
}

fn single_line(label: &str) -> String {
    let line: String = label
        .chars()
        .map(|c| if c.is_control() { ' ' } else { c })
        .take(200)
        .collect();
    line.trim().to_owned()
}

/// Reads one ignore file of a checkpoint by its `/`-joined path.
type LoadIgnoreFile<'a> = Box<dyn FnMut(&str) -> Option<Vec<u8>> + 'a>;

/// The ignore rules a checkpoint was taken under, read from the ignore
/// files it holds (`load` returns one by its `/`-joined path, or `None`).
struct CheckpointIgnores<'a> {
    /// The ignore file names honoured in each directory, lowest precedence
    /// first.
    names: &'static [&'static str],
    load: LoadIgnoreFile<'a>,
    /// One matcher per directory (`""`: the root), `None` without rules.
    matchers: BTreeMap<String, Option<Gitignore>>,
}

impl<'a> CheckpointIgnores<'a> {
    fn new(names: &'static [&'static str], load: impl FnMut(&str) -> Option<Vec<u8>> + 'a) -> Self {
        Self {
            names,
            load: Box::new(load),
            matchers: BTreeMap::new(),
        }
    }

    fn matcher(&mut self, dir: &str) -> Option<&Gitignore> {
        if !self.matchers.contains_key(dir) {
            let mut builder = GitignoreBuilder::new(Path::new("/").join(dir));
            let mut any = false;
            for name in self.names {
                let file = if dir.is_empty() {
                    (*name).to_owned()
                } else {
                    format!("{dir}/{name}")
                };
                if let Some(bytes) = (self.load)(&file) {
                    any = true;
                    for line in String::from_utf8_lossy(&bytes).lines() {
                        // A line that is not a valid glob is skipped, as git does.
                        let _ = builder.add_line(None, line);
                    }
                }
            }
            let matcher = any.then(|| builder.build().ok()).flatten();
            self.matchers.insert(dir.to_owned(), matcher);
        }
        self.matchers.get(dir).and_then(Option::as_ref)
    }

    /// Whether the checkpoint's rules ignored the file `path`: the deepest
    /// directory whose rules decide (ignore or re-include) wins, as in git.
    fn ignores(&mut self, path: &str) -> bool {
        let components: Vec<&str> = path.split('/').collect();
        let full = Path::new("/").join(path);
        for depth in (0..components.len()).rev() {
            let dir = components[..depth].join("/");
            let Some(matcher) = self.matcher(&dir) else {
                continue;
            };
            let decided = matcher.matched_path_or_any_parents(&full, false);
            if decided.is_ignore() {
                return true;
            }
            if decided.is_whitelist() {
                return false;
            }
        }
        false
    }
}

/// Write `bytes` to `target` all at once: a temporary file beside it
/// (synced to disk when `sync`), then renamed over it.
fn write_atomically(target: &Path, bytes: &[u8], sync: bool) -> std::io::Result<()> {
    let mut name = target.file_name().unwrap_or_default().to_os_string();
    name.push(format!(
        ".{}-{}.tmp",
        std::process::id(),
        TEMP_COUNTER.fetch_add(1, Ordering::Relaxed)
    ));
    let temp = target.with_file_name(name);
    let written = File::create(&temp)
        .and_then(|mut file| {
            file.write_all(bytes)?;
            if sync { file.sync_all() } else { Ok(()) }
        })
        .and_then(|()| std::fs::rename(&temp, target));
    if written.is_err() {
        let _ = std::fs::remove_file(&temp);
    }
    written
}

/// The checkpoints of one session in one workspace.
#[derive(Debug)]
#[allow(clippy::large_enum_variant)] // One value per session, never held in bulk; boxing buys nothing.
pub enum Checkpoints {
    Git(GitCheckpoints),
    Copy(CopyCheckpoints),
}

impl Checkpoints {
    /// Git checkpoints when the workspace is in a git work tree, copies (under
    /// `data_dir`) otherwise; host git runs without a Linux sandbox helper.
    ///
    /// # Errors
    ///
    /// An invalid session id.
    pub fn open(workspace: &Workspace, session: &str, data_dir: &Path) -> ToolResult<Self> {
        Self::open_with(workspace, session, data_dir, &SandboxConfig::default())
    }

    /// As [`Self::open`], running host git under `sandbox`'s settings. A
    /// repository refused as unsafe gets copy checkpoints, and
    /// [`Self::git_refusal`] keeps the reason.
    ///
    /// # Errors
    ///
    /// An invalid session id.
    pub fn open_with(
        workspace: &Workspace,
        session: &str,
        data_dir: &Path,
        sandbox: &SandboxConfig,
    ) -> ToolResult<Self> {
        check_session(session)?;
        let restorer = Workspace::open(workspace.root(), &[])?;
        // Host git cannot read the session's deny list; a host that gave
        // none gets the person's credentials under HOME.
        let deny = workspace
            .deny_list()
            .cloned()
            .unwrap_or_else(|| Arc::new(DenyList::env_home_credentials()));
        let refused =
            match Repo::discover(workspace.root(), sandbox, deny).and_then(|repo| match repo {
                Some(repo) => repo.check_attributes(&[]).map(|()| Some(repo)),
                None => Ok(None),
            }) {
                Ok(Some(repo)) => {
                    if let Ok(prefix) = workspace.root().strip_prefix(repo.top()) {
                        let prefix = WsPath::from_relative(prefix)?;
                        return Ok(Self::Git(GitCheckpoints {
                            repo,
                            prefix,
                            session: session.to_owned(),
                            restorer,
                            limits: CheckpointLimits::default(),
                        }));
                    }
                    None
                }
                Ok(None) => None,
                Err(error) => {
                    tracing::warn!(
                        reason = error.message(),
                        "unsafe git repository: checkpoints by copy, git tools refused"
                    );
                    Some(error)
                }
            };
        let mut key = Sha256::new();
        key.update(workspace.root().as_os_str().as_encoded_bytes());
        let key = hex(&key.finalize()[..8]);
        let base = data_dir.join("checkpoints").join(key);
        // A repository refused as unsafe is still a git work tree: its
        // `.gitignore` decides, as before. A plain folder's does not.
        let rules = if refused.is_some() || in_git_tree(workspace.root()) {
            ScanRules::Git
        } else {
            ScanRules::Folder
        };
        Ok(Self::Copy(CopyCheckpoints {
            lock: base.join("lock"),
            objects: base.join("objects"),
            cache: base.join(STAT_CACHE),
            manifests: base.join("sessions").join(session),
            restorer,
            refused,
            limits: CheckpointLimits::default(),
            rules,
            placeholder: folder_scan::is_placeholder,
            clone: true,
        }))
    }

    /// Read `path` as the global git config (hosts that keep it elsewhere,
    /// tests).
    #[must_use]
    pub fn with_git_global_config(mut self, path: Option<PathBuf>) -> Self {
        if let (Self::Git(git), Some(path)) = (&mut self, path) {
            git.repo = git.repo.clone().with_global_config(path);
        }
        self
    }

    /// Tell cloud placeholders with `test` (tests simulate one).
    #[cfg(test)]
    #[must_use]
    pub fn with_placeholder_test(mut self, test: PlaceholderTest) -> Self {
        if let Self::Copy(copy) = &mut self {
            copy.placeholder = test;
        }
        self
    }

    /// Never clone into the store (tests of the plain copy).
    #[cfg(test)]
    #[must_use]
    pub fn without_clones(mut self) -> Self {
        if let Self::Copy(copy) = &mut self {
            copy.clone = false;
        }
        self
    }

    /// Use `limits` instead of the defaults.
    #[must_use]
    pub fn with_limits(mut self, limits: CheckpointLimits) -> Self {
        self.set_limits(limits);
        self
    }

    /// Use `limits` from now on.
    pub fn set_limits(&mut self, limits: CheckpointLimits) {
        match self {
            Self::Git(git) => git.limits = limits,
            Self::Copy(copy) => copy.limits = limits,
        }
    }

    /// Whether a checkpoint of the folder as it is now fits the limits.
    /// The answer comes from a walk that reads no file (stat only), so the
    /// host can ask before a turn changes anything: `Err` with
    /// [`ErrorCode::TooLarge`] and the reason when it does not fit.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::TooLarge`], or git failures.
    pub fn check_fits(&self) -> ToolResult<()> {
        match self {
            Self::Git(git) => git.plan().map(|_| ()),
            Self::Copy(copy) => copy.plan().map(|_| ()),
        }
    }

    /// What file `path` held at checkpoint `seq`: `Ok(None)` when it did
    /// not exist then, or was left out (too large, a placeholder).
    ///
    /// # Errors
    ///
    /// An unknown checkpoint, [`ErrorCode::TooLarge`] past `max_bytes`, or
    /// git failures.
    pub fn file_at(&self, seq: u64, path: &WsPath, max_bytes: u64) -> ToolResult<Option<Vec<u8>>> {
        match self {
            Self::Git(git) => git.file_at(seq, path, max_bytes),
            Self::Copy(copy) => copy.file_at(seq, path, max_bytes),
        }
    }

    /// The scan rules that match what this folder's checkpoints cover:
    /// the host scans with them to list what a command changed.
    #[must_use]
    pub fn scan_rules(&self) -> ScanRules {
        match self {
            Self::Git(_) => ScanRules::Git,
            Self::Copy(copy) => copy.rules,
        }
    }

    /// Why the folder's git repository is not used, when it was refused.
    #[must_use]
    pub fn git_refusal(&self) -> Option<&ToolError> {
        match self {
            Self::Git(_) => None,
            Self::Copy(copy) => copy.refused.as_ref(),
        }
    }

    /// The checked repository, for git checkpoints.
    #[must_use]
    pub fn repo(&self) -> Option<&Repo> {
        match self {
            Self::Git(git) => Some(&git.repo),
            Self::Copy(_) => None,
        }
    }

    /// `git` or `copy`.
    #[must_use]
    pub fn kind(&self) -> &'static str {
        match self {
            Self::Git(_) => "git",
            Self::Copy(_) => "copy",
        }
    }

    /// A commit of the folder as a checkpoint would take it now (`HEAD` its
    /// parent), stored as objects only: no ref names it, and the folder,
    /// the index and `HEAD` are untouched (the merge-back's preview, which
    /// writes nothing anyone sees). Git checkpoints only.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::Unsupported`] for copy checkpoints; git failures, or too
    /// many untracked bytes.
    pub fn snapshot_commit(&self) -> ToolResult<String> {
        match self {
            Self::Git(git) => {
                let (tree, _) = git.snapshot_tree()?;
                git.commit_tree(&tree, "elitea snapshot")
            }
            Self::Copy(_) => Err(ToolError::new(
                ErrorCode::Unsupported,
                "the folder is not in a git repository the host can use",
            )),
        }
    }

    /// Take a checkpoint now.
    ///
    /// # Errors
    ///
    /// git failures, or a folder too large to copy.
    pub fn create(&self, label: &str) -> ToolResult<CheckpointInfo> {
        match self {
            Self::Git(git) => git.create(label),
            Self::Copy(copy) => copy.create(label),
        }
    }

    /// Every checkpoint of the session, oldest first.
    ///
    /// # Errors
    ///
    /// git failures or unreadable manifests.
    pub fn list(&self) -> ToolResult<Vec<CheckpointInfo>> {
        match self {
            Self::Git(git) => git.list(),
            Self::Copy(copy) => copy.list(),
        }
    }

    /// Make the workspace what it was at checkpoint `seq`.
    ///
    /// # Errors
    ///
    /// An unknown checkpoint, git failures, or I/O.
    pub fn restore(&self, seq: u64) -> ToolResult<RestoreReport> {
        match self {
            Self::Git(git) => git.restore(seq, None, false),
            Self::Copy(copy) => copy.restore(seq, None, false),
        }
    }

    /// What [`Self::restore`] would do now, without doing it: the files it
    /// would write back and those it would delete. The workspace is not
    /// touched (a git checkpoint store may gain the objects of a snapshot
    /// of now, as a restore would).
    ///
    /// # Errors
    ///
    /// As [`Self::restore`].
    pub fn preview(&self, seq: u64) -> ToolResult<RestoreReport> {
        match self {
            Self::Git(git) => git.restore(seq, None, true),
            Self::Copy(copy) => copy.restore(seq, None, true),
        }
    }

    /// Make one file what it was at checkpoint `seq` (deleting it if it did
    /// not exist then).
    ///
    /// # Errors
    ///
    /// As [`Self::restore`].
    pub fn restore_file(&self, seq: u64, path: &WsPath) -> ToolResult<RestoreReport> {
        match self {
            Self::Git(git) => git.restore(seq, Some(path), false),
            Self::Copy(copy) => copy.restore(seq, Some(path), false),
        }
    }
}

/// A temporary index in a private directory of its own (the sandbox lets
/// git write that directory: the index and its lock), removed on drop.
struct TempIndex {
    dir: PathBuf,
    index: PathBuf,
}

impl TempIndex {
    fn new() -> ToolResult<Self> {
        let dir = std::env::temp_dir().join(format!(
            "elitea-checkpoint-{}-{}",
            std::process::id(),
            TEMP_COUNTER.fetch_add(1, Ordering::Relaxed)
        ));
        std::fs::create_dir(&dir)
            .map_err(|error| ToolError::io("cannot create a temporary index", &error))?;
        let dir = crate::sys::canonicalize(&dir)
            .map_err(|error| ToolError::io("cannot create a temporary index", &error))?;
        Ok(Self {
            index: dir.join("index"),
            dir,
        })
    }

    fn os(&self) -> &OsStr {
        self.index.as_os_str()
    }
}

impl Drop for TempIndex {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.dir);
    }
}

/// Checkpoints as commits on private refs.
#[derive(Debug)]
pub struct GitCheckpoints {
    repo: Repo,
    /// The workspace relative to the top level.
    prefix: WsPath,
    session: String,
    restorer: Workspace,
    limits: CheckpointLimits,
}

/// Remove the empty directories above `file`, up to but not including
/// `stop`. A directory that is not empty (another session's refs) stays:
/// `remove_dir` refuses it.
fn remove_empty_parents(file: &Path, stop: &Path) {
    let mut dir = file.parent();
    while let Some(current) = dir {
        if current == stop || !current.starts_with(stop) || std::fs::remove_dir(current).is_err() {
            return;
        }
        dir = current.parent();
    }
}

/// A held `<ref>.lock`, removed when dropped.
struct RefLock(PathBuf);

impl Drop for RefLock {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.0);
    }
}

/// What [`GitCheckpoints::prune_loose`] did with one ref.
enum Pruned {
    /// Removed, reflog included.
    Done,
    /// Another writer holds the ref's lock: leave it for next time.
    Busy,
    /// Not a loose ref this checkpoint owns (packed, or changed): ask git.
    NotLoose,
}

/// What a git checkpoint leaves out.
#[derive(Default)]
struct Plan {
    /// Workspace-relative paths not added: too large, or not regular files
    /// (FIFOs, sockets, devices).
    excluded: Vec<String>,
    /// Top-relative paths a restore must leave alone.
    skipped: Vec<String>,
    /// Workspace-relative paths over the size limit (reported).
    too_large: Vec<String>,
    /// Workspace-relative cloud placeholders (reported, never read).
    placeholders: Vec<String>,
}

impl GitCheckpoints {
    fn top(&self) -> &Path {
        self.repo.top()
    }

    fn workspace_dir(&self) -> PathBuf {
        let mut dir = self.top().to_path_buf();
        for component in self.prefix.components() {
            dir.push(component);
        }
        dir
    }

    fn objects(&self) -> PathBuf {
        self.repo.common_dir().join("objects")
    }

    fn refs(&self) -> PathBuf {
        self.repo.common_dir().join("refs/elitea")
    }

    fn head(&self) -> Option<String> {
        self.repo
            .git(self.top())
            .text(&["rev-parse", "--verify", "--quiet", "HEAD^{commit}"])
            .ok()
            .filter(|head| !head.is_empty())
    }

    /// Size the snapshot before `add` stores anything: files over the cap
    /// and special files are left out, and too many untracked bytes refuse
    /// the checkpoint ([`ErrorCode::TooLarge`]: the turn runs without one).
    fn plan(&self) -> ToolResult<Plan> {
        let workspace_dir = self.workspace_dir();
        let tracked: BTreeSet<String> = self
            .repo
            .git(&workspace_dir)
            .run(&["ls-files", "-z", "--cached"])?
            .split(|byte| *byte == 0)
            .filter(|path| !path.is_empty())
            .map(|path| String::from_utf8_lossy(path).into_owned())
            .collect();
        let walker = ignore::WalkBuilder::new(&workspace_dir)
            .hidden(false)
            .require_git(false)
            .follow_links(false)
            .filter_entry(|entry| !entry.file_name().to_str().is_some_and(is_protected_name))
            .build();
        let mut plan = Plan::default();
        let (mut untracked_bytes, mut untracked_files) = (0u64, 0usize);
        for entry in walker.flatten() {
            let Ok(relative) = entry.path().strip_prefix(&workspace_dir) else {
                continue;
            };
            let Some(relative) = relative.to_str().filter(|text| !text.is_empty()) else {
                continue;
            };
            let Ok(meta) = entry.path().symlink_metadata() else {
                continue;
            };
            let kind = meta.file_type();
            if kind.is_dir() || kind.is_symlink() {
                continue;
            }
            let ws_path = WsPath::from_relative(Path::new(relative))?;
            if !kind.is_file() {
                plan.excluded.push(relative.to_owned());
                continue;
            }
            if meta.len() > self.limits.max_file_bytes {
                plan.excluded.push(relative.to_owned());
                plan.skipped.push(self.top_relative(&ws_path));
                plan.too_large.push(relative.to_owned());
                continue;
            }
            // `git add` would read a cloud placeholder and download it.
            if folder_scan::is_placeholder(entry.path(), &meta) {
                plan.excluded.push(relative.to_owned());
                plan.skipped.push(self.top_relative(&ws_path));
                plan.placeholders.push(relative.to_owned());
                continue;
            }
            if !tracked.contains(relative) {
                untracked_bytes += meta.len();
                untracked_files += 1;
            }
        }
        if untracked_bytes > self.limits.max_untracked_bytes
            || untracked_files > self.limits.max_untracked_files
        {
            return Err(ToolError::new(
                ErrorCode::TooLarge,
                format!(
                    "{untracked_files} untracked files ({untracked_bytes} bytes) are too many to checkpoint"
                ),
            ));
        }
        Ok(plan)
    }

    /// What file `path` held at checkpoint `seq` (see
    /// [`Checkpoints::file_at`]).
    fn file_at(&self, seq: u64, path: &WsPath, max_bytes: u64) -> ToolResult<Option<Vec<u8>>> {
        let commit = self
            .list()?
            .into_iter()
            .find(|info| info.seq == seq)
            .ok_or_else(|| ToolError::new(ErrorCode::NotFound, format!("no checkpoint {seq}")))?
            .id;
        let object = format!("{commit}:{}", self.top_relative(path));
        // Absent from the checkpoint's tree: not an error.
        let Ok(size) = self.repo.git(self.top()).text(&["cat-file", "-s", &object]) else {
            return Ok(None);
        };
        let size: u64 = size.trim().parse().unwrap_or(u64::MAX);
        if size > max_bytes {
            return Err(ToolError::new(
                ErrorCode::TooLarge,
                format!("`{path}` was {size} bytes; the limit is {max_bytes}"),
            ));
        }
        self.repo
            .git(self.top())
            .run(&["cat-file", "blob", &object])
            .map(Some)
    }

    /// The tree of the workspace as it is now (outside the workspace: as
    /// in HEAD), and the paths it skipped.
    fn snapshot_tree(&self) -> ToolResult<(String, Vec<String>)> {
        let (tree, plan) = self.snapshot_tree_planned()?;
        Ok((tree, plan.skipped))
    }

    /// [`Self::snapshot_tree`] with the whole plan.
    fn snapshot_tree_planned(&self) -> ToolResult<(String, Plan)> {
        let plan = self.plan()?;
        let index = TempIndex::new()?;
        let index_dir = [index.dir.clone()];
        let with_objects = [index.dir.clone(), self.objects()];
        if let Some(head) = self.head() {
            self.repo
                .git(self.top())
                .env("GIT_INDEX_FILE", index.os())
                .writes(&index_dir)
                .run(&["read-tree", &head])?;
        }
        let mut pathspecs = b".\0".to_vec();
        for path in &plan.excluded {
            pathspecs.extend_from_slice(format!(":(exclude,literal){path}").as_bytes());
            pathspecs.push(0);
        }
        self.repo
            .git(&self.workspace_dir())
            .env("GIT_INDEX_FILE", index.os())
            .writes(&with_objects)
            .literal(false)
            .stdin(&pathspecs)
            .worktree()
            .run(&[
                "add",
                "--all",
                "--pathspec-from-file=-",
                "--pathspec-file-nul",
            ])?;
        let tree = self
            .repo
            .git(self.top())
            .env("GIT_INDEX_FILE", index.os())
            .writes(&with_objects)
            .text(&["write-tree"])?;
        Ok((tree, plan))
    }

    /// The paths checkpoint `commit` skipped.
    fn skipped_in(&self, commit: &str) -> Vec<String> {
        let body = self
            .repo
            .git(self.top())
            .text(&["cat-file", "commit", commit])
            .unwrap_or_default();
        body.lines()
            .filter_map(|line| line.strip_prefix(SKIPPED_TRAILER))
            .filter_map(|list| serde_json::from_str::<Vec<String>>(list).ok())
            .flatten()
            .collect()
    }

    /// Drop the session's checkpoints beyond [`CheckpointLimits::keep`] and
    /// older than [`CheckpointLimits::max_age`] (never the newest). Best
    /// effort: a failure leaves them for the next time.
    fn prune(&self) {
        let Ok(all) = self.list() else { return };
        let cutoff = now_unix().saturating_sub(self.limits.max_age.as_secs());
        let excess = all.len().saturating_sub(self.limits.keep.max(1));
        for (index, info) in all.iter().enumerate() {
            let newest = index + 1 == all.len();
            if newest || (index >= excess && info.created_unix >= cutoff) {
                continue;
            }
            let name = self.ref_name(info.seq);
            let packed = match self.prune_loose(info) {
                Pruned::Busy => continue,
                // A packed copy under the same name would bring the ref
                // back: it goes too, whatever it names.
                Pruned::Done => self.prune_packed(&name, None),
                // Changed under us: leave it.
                Pruned::NotLoose if self.repo.common_dir().join(&name).exists() => continue,
                // Packed (after `gc`): only while it still names this
                // checkpoint.
                Pruned::NotLoose => self.prune_packed(&name, Some(&info.id)),
            };
            if !matches!(packed, Pruned::Done) {
                tracing::warn!(
                    checkpoint = info.seq,
                    "a packed checkpoint ref could not be pruned"
                );
            }
        }
    }

    /// Remove the packed line of checkpoint ref `name` the way git does:
    /// under `packed-refs.lock`, the rewritten file renamed over
    /// `packed-refs`. It runs in this process, not as a sandboxed
    /// `update-ref -d`: that needs `packed-refs.lock` writable, and the
    /// Linux sandbox cannot bind a file that does not exist yet.
    /// `expected` is the id the line must still name (`None`: any).
    /// No `packed-refs`, or no line for `name`, is [`Pruned::Done`].
    fn prune_packed(&self, name: &str, expected: Option<&str>) -> Pruned {
        use std::io::ErrorKind;
        let common = self.repo.common_dir();
        let packed = common.join("packed-refs");
        let lock = common.join("packed-refs.lock");
        let mut file = match std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&lock)
        {
            Ok(file) => file,
            Err(error) if error.kind() == ErrorKind::AlreadyExists => return Pruned::Busy,
            Err(_) => return Pruned::NotLoose,
        };
        let release = RefLock(lock.clone());
        let text = match std::fs::read_to_string(&packed) {
            Ok(text) => text,
            Err(error) if error.kind() == ErrorKind::NotFound => return Pruned::Done,
            Err(_) => return Pruned::NotLoose,
        };
        let mut kept = String::with_capacity(text.len());
        let mut found = false;
        let mut skip_peeled = false;
        for line in text.split_inclusive('\n') {
            if skip_peeled && line.starts_with('^') {
                continue;
            }
            skip_peeled = false;
            let entry = line.trim_end_matches(['\n', '\r']);
            if let Some((id, refname)) = entry.split_once(' ')
                && refname == name
                && !entry.starts_with('#')
            {
                if expected.is_some_and(|expected| expected != id) {
                    return Pruned::NotLoose;
                }
                found = true;
                skip_peeled = true;
                continue;
            }
            kept.push_str(line);
        }
        if !found {
            return Pruned::Done;
        }
        // The reflog first, as for a loose ref.
        if let Err(error) = std::fs::remove_file(common.join("logs").join(name))
            && error.kind() != ErrorKind::NotFound
        {
            tracing::warn!(%error, "a pruned checkpoint's reflog stays");
            return Pruned::Busy;
        }
        if file
            .write_all(kept.as_bytes())
            .and_then(|()| file.sync_all())
            .is_err()
        {
            return Pruned::NotLoose;
        }
        drop(file);
        if std::fs::rename(&lock, &packed).is_err() {
            return Pruned::NotLoose;
        }
        // The lock became `packed-refs`: dropping the guard now could
        // delete another writer's new lock.
        std::mem::forget(release);
        let prefix = Path::new(REF_PREFIX);
        remove_empty_parents(
            &common.join("logs").join(name),
            &common.join("logs").join(prefix),
        );
        Pruned::Done
    }

    /// Remove a loose checkpoint ref the way git does, under its own
    /// `<ref>.lock`, so a concurrent writer of the same ref and this
    /// removal exclude each other; the ref's reflog goes with it.
    /// `update-ref -d` is not used for this: it also locks `packed-refs`,
    /// which a fresh repository lacks and the sandbox cannot bind writable.
    /// A ref that is not loose (after `gc`) is left to [`Self::prune_packed`].
    fn prune_loose(&self, info: &CheckpointInfo) -> Pruned {
        use std::io::ErrorKind;
        let name = self.ref_name(info.seq);
        let common = self.repo.common_dir();
        let loose = common.join(&name);
        let mut lock_name = loose.clone().into_os_string();
        lock_name.push(".lock");
        let lock = PathBuf::from(lock_name);
        let take = || {
            std::fs::OpenOptions::new()
                .write(true)
                .create_new(true)
                .open(&lock)
        };
        match take() {
            Ok(file) => drop(file),
            Err(error) if error.kind() == ErrorKind::AlreadyExists => {
                // A lock a crash left behind would keep this checkpoint
                // unpruned forever: one older than STALE_LOCK_AGE is taken
                // over (removed, then locked afresh, so two pruners racing
                // for it still exclude each other).
                // The takeover moves the stale lock aside under a unique name
                // and judges THAT file, so a racing pruner that already
                // replaced it cannot have a fresh lock deleted: a moved file
                // that turns out fresh is put back and the ref left alone.
                let aged = |path: &Path| {
                    std::fs::metadata(path)
                        .and_then(|meta| meta.modified())
                        .ok()
                        .and_then(|modified| modified.elapsed().ok())
                        .is_some_and(|age| age >= STALE_LOCK_AGE)
                };
                let mut aside = lock.clone().into_os_string();
                aside.push(format!(
                    ".stale-{}-{}",
                    std::process::id(),
                    TEMP_COUNTER.fetch_add(1, Ordering::Relaxed)
                ));
                let aside = PathBuf::from(aside);
                if !aged(&lock) || std::fs::rename(&lock, &aside).is_err() {
                    tracing::warn!(lock = %lock.display(), "a checkpoint ref is locked, not pruned");
                    return Pruned::Busy;
                }
                if !aged(&aside) {
                    let _ = std::fs::rename(&aside, &lock);
                    tracing::warn!(lock = %lock.display(), "a checkpoint ref is locked, not pruned");
                    return Pruned::Busy;
                }
                let _ = std::fs::remove_file(&aside);
                if take().is_err() {
                    tracing::warn!(lock = %lock.display(), "a checkpoint ref is locked, not pruned");
                    return Pruned::Busy;
                }
                tracing::warn!(lock = %lock.display(), "took over a stale checkpoint ref lock");
            }
            Err(_) => return Pruned::NotLoose,
        }
        // Released on every exit, a panic included.
        let release = RefLock(lock);
        // Under the lock: the ref still names this checkpoint, or leave it.
        if !std::fs::read_to_string(&loose).is_ok_and(|text| text.trim() == info.id) {
            return Pruned::NotLoose;
        }
        // The reflog first: a reflog left behind would keep the commit
        // reachable, and one that cannot go keeps the ref for the next try.
        if let Err(error) = std::fs::remove_file(common.join("logs").join(&name))
            && error.kind() != ErrorKind::NotFound
        {
            tracing::warn!(%error, "a pruned checkpoint's reflog stays");
            return Pruned::Busy;
        }
        if std::fs::remove_file(&loose).is_ok() {
            // The session's now-empty `refs/...` and `logs/...` directories
            // go too (not the shared prefix, and never a non-empty one:
            // `remove_dir` refuses those).
            // The lock file sits in the same directory: release it first.
            drop(release);
            let prefix = Path::new(REF_PREFIX);
            remove_empty_parents(&loose, &common.join(prefix));
            remove_empty_parents(
                &common.join("logs").join(&name),
                &common.join("logs").join(prefix),
            );
            Pruned::Done
        } else {
            Pruned::NotLoose
        }
    }

    /// A commit of `tree` with `HEAD` as parent, objects only.
    fn commit_tree(&self, tree: &str, message: &str) -> ToolResult<String> {
        let mut args = vec!["commit-tree", "--no-gpg-sign", tree, "-m", message];
        let head = self.head();
        if let Some(head) = &head {
            args.extend(["-p", head.as_str()]);
        }
        let identity = OsStr::new("Elitea checkpoints");
        let email = OsStr::new("checkpoints@elitea.invalid");
        self.repo
            .git(self.top())
            .env("GIT_AUTHOR_NAME", identity)
            .env("GIT_AUTHOR_EMAIL", email)
            .env("GIT_COMMITTER_NAME", identity)
            .env("GIT_COMMITTER_EMAIL", email)
            .writes(&[self.objects()])
            .text(&args)
    }

    fn ref_name(&self, seq: u64) -> String {
        format!("{REF_PREFIX}/{}/{seq}", self.session)
    }

    fn create(&self, label: &str) -> ToolResult<CheckpointInfo> {
        let (tree, plan) = self.snapshot_tree_planned()?;
        let skipped = &plan.skipped;
        let seq = self.list()?.last().map_or(1, |last| last.seq + 1);
        let label = single_line(label);
        let mut message = format!("elitea checkpoint {seq}: {label}");
        if !skipped.is_empty() {
            let list = serde_json::to_string(skipped)
                .map_err(|_| ToolError::new(ErrorCode::Io, "cannot encode the checkpoint"))?;
            message = format!("{message}\n\n{SKIPPED_TRAILER}{list}");
        }
        let commit = self.commit_tree(&tree, &message)?;
        std::fs::create_dir_all(self.refs())
            .map_err(|error| ToolError::io("cannot store the checkpoint", &error))?;
        self.repo.git(self.top()).writes(&[self.refs()]).run(&[
            "update-ref",
            &self.ref_name(seq),
            &commit,
        ])?;
        self.prune();
        Ok(CheckpointInfo {
            seq,
            label,
            created_unix: now_unix(),
            id: commit,
            ..CheckpointInfo::default()
        }
        .with_left_out(&plan.too_large, &plan.placeholders))
    }

    fn list(&self) -> ToolResult<Vec<CheckpointInfo>> {
        let prefix = format!("{REF_PREFIX}/{}/", self.session);
        let text = self.repo.git(self.top()).text(&[
            "for-each-ref",
            "--format=%(refname)%09%(objectname)%09%(committerdate:unix)%09%(subject)",
            &prefix,
        ])?;
        let mut out: Vec<CheckpointInfo> = text
            .lines()
            .filter_map(|line| {
                let mut fields = line.splitn(4, '\t');
                let seq = fields.next()?.strip_prefix(&prefix)?.parse().ok()?;
                let id = fields.next()?.to_owned();
                let created_unix = fields.next()?.parse().ok()?;
                let subject = fields.next().unwrap_or_default();
                let label = subject
                    .split_once(": ")
                    .map_or("", |(_, label)| label)
                    .to_owned();
                Some(CheckpointInfo {
                    seq,
                    label,
                    created_unix,
                    id,
                    ..CheckpointInfo::default()
                })
            })
            .collect();
        out.sort_by_key(|info| info.seq);
        Ok(out)
    }

    /// A top-level path as a workspace path, if it is in the workspace.
    fn in_workspace(&self, top_relative: &str) -> Option<WsPath> {
        let path = WsPath::from_relative(Path::new(top_relative)).ok()?;
        let prefix = self.prefix.components();
        path.components()
            .starts_with(prefix)
            .then(|| {
                WsPath::from_relative(Path::new(&path.components()[prefix.len()..].join("/"))).ok()
            })
            .flatten()
    }

    fn top_relative(&self, path: &WsPath) -> String {
        self.prefix
            .components()
            .iter()
            .chain(path.components())
            .cloned()
            .collect::<Vec<_>>()
            .join("/")
    }

    /// What restoring checkpoint `seq` (within `only`) changes: the
    /// checkpoint's commit, the top-relative paths to write back and those
    /// to delete (the ones its ignore rules ignored left out).
    fn restore_plan(
        &self,
        seq: u64,
        only: Option<&WsPath>,
    ) -> ToolResult<(String, Vec<String>, Vec<String>)> {
        let commit = self
            .repo
            .git(self.top())
            .text(&[
                "rev-parse",
                "--verify",
                "--quiet",
                &format!("{}^{{commit}}", self.ref_name(seq)),
            ])
            .map_err(|_| ToolError::new(ErrorCode::NotFound, format!("no checkpoint {seq}")))?;
        let (current, _) = self.snapshot_tree()?;
        let skipped: BTreeSet<String> = self.skipped_in(&commit).into_iter().collect();
        let scope = match only {
            Some(path) => self.top_relative(path),
            None if self.prefix.is_root() => ".".to_owned(),
            None => self.top_relative(&WsPath::root()),
        };
        let changes = self.repo.git(self.top()).run(&[
            "diff-tree",
            "-r",
            "-z",
            "--no-renames",
            "--no-ext-diff",
            "--no-textconv",
            "--ignore-submodules=all",
            "--name-status",
            &commit,
            &current,
            "--",
            &scope,
        ])?;
        let mut fields = changes
            .split(|byte| *byte == 0)
            .filter(|field| !field.is_empty())
            .map(|field| String::from_utf8_lossy(field).into_owned());
        let mut write_back = Vec::new();
        let mut delete = Vec::new();
        while let (Some(status), Some(path)) = (fields.next(), fields.next()) {
            if skipped.contains(&path) {
                continue;
            }
            if status == "A" {
                delete.push(path);
            } else {
                write_back.push(path);
            }
        }
        // Not in the checkpoint, but ignored by its rules: it was not
        // snapshotted, so it may be older than the checkpoint. Kept.
        let mut ignores = CheckpointIgnores::new(&[".gitignore"], |file| {
            self.repo
                .git(self.top())
                .run(&["cat-file", "blob", &format!("{commit}:{file}")])
                .ok()
        });
        delete.retain(|path| !ignores.ignores(path));
        drop(ignores);
        Ok((commit, write_back, delete))
    }

    fn restore(&self, seq: u64, only: Option<&WsPath>, dry_run: bool) -> ToolResult<RestoreReport> {
        let (commit, write_back, delete) = self.restore_plan(seq, only)?;
        let mut report = RestoreReport::default();
        if dry_run {
            for path in delete {
                if let Some(ws_path) = self.in_workspace(&path)
                    && self.restorer.stat(&ws_path)?.is_some()
                {
                    report.deleted.push(ws_path.display_string());
                }
            }
            report.restored = write_back
                .iter()
                .filter_map(|path| self.in_workspace(path))
                .map(|path| path.display_string())
                .collect();
            return Ok(report);
        }
        for path in delete {
            if let Some(ws_path) = self.in_workspace(&path)
                && self.restorer.remove_file(&ws_path)?
            {
                report.deleted.push(ws_path.display_string());
            }
        }
        if !write_back.is_empty() {
            // Files written back get the attributes of now: none may select
            // a smudge filter.
            self.repo.check_attributes(&write_back)?;
            let index = TempIndex::new()?;
            self.repo
                .git(self.top())
                .env("GIT_INDEX_FILE", index.os())
                .writes(std::slice::from_ref(&index.dir))
                .run(&["read-tree", &commit])?;
            let mut stdin = Vec::new();
            for path in &write_back {
                stdin.extend_from_slice(path.as_bytes());
                stdin.push(0);
            }
            self.repo
                .git(self.top())
                .env("GIT_INDEX_FILE", index.os())
                .stdin(&stdin)
                .writes(&[index.dir.clone(), self.workspace_dir()])
                .worktree()
                .run(&["checkout-index", "--force", "-z", "--stdin"])?;
            report.restored = write_back
                .iter()
                .filter_map(|path| self.in_workspace(path))
                .map(|path| path.display_string())
                .collect();
        }
        Ok(report)
    }
}

/// One file in a copy manifest.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
struct ManifestFile {
    sha256: String,
    mode: u32,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
struct Manifest {
    seq: u64,
    label: String,
    created_unix: u64,
    files: BTreeMap<String, ManifestFile>,
    /// Left alone by a restore: too large, unreadable, a placeholder.
    skipped: BTreeSet<String>,
    /// The rules the checkpoint walked by. `None`: a checkpoint from before
    /// they were recorded, which always honoured `.gitignore`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    rules: Option<ScanRules>,
    /// The part of `skipped` over the size limit.
    #[serde(default, skip_serializing_if = "BTreeSet::is_empty")]
    too_large: BTreeSet<String>,
    /// The part of `skipped` that are cloud placeholders.
    #[serde(default, skip_serializing_if = "BTreeSet::is_empty")]
    placeholders: BTreeSet<String>,
}

impl Manifest {
    /// What this checkpoint walked by (legacy ones: `.gitignore`).
    fn rules(&self) -> ScanRules {
        self.rules.unwrap_or(ScanRules::Git)
    }
}

/// A stored file's stat and digest, as [`STAT_CACHE`] keeps them.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
struct CachedFile {
    stat: FileStat,
    sha256: String,
}

/// What a copy checkpoint takes, from a stat-only walk.
#[derive(Debug, Default)]
struct CopyPlan {
    /// The files to store and their stat.
    store: BTreeMap<String, FileStat>,
    too_large: BTreeSet<String>,
    placeholders: BTreeSet<String>,
}

/// Checkpoints as copies in the host's data directory.
#[derive(Debug)]
pub struct CopyCheckpoints {
    /// The folder's store lock (shared by its sessions).
    lock: PathBuf,
    objects: PathBuf,
    /// The folder's [`STAT_CACHE`].
    cache: PathBuf,
    manifests: PathBuf,
    restorer: Workspace,
    /// Why the folder's git repository was not used.
    refused: Option<ToolError>,
    limits: CheckpointLimits,
    /// What the walk honours: a plain folder's `.gitignore` is not.
    rules: ScanRules,
    placeholder: PlaceholderTest,
    /// Store copy-on-write clones where the file system can.
    clone: bool,
}

/// The regular files of the workspace a copy checkpoint walked under
/// `rules`: not under `.git`, symlinks not followed (placeholders
/// included, see [`folder_scan`]).
fn walk_files(root: &Path, rules: ScanRules) -> Vec<WsPath> {
    let found = folder_scan::scan(root, rules, usize::MAX);
    found
        .files
        .into_keys()
        .chain(found.placeholders)
        .filter_map(|name| WsPath::from_relative(Path::new(&name)).ok())
        .collect()
}

impl CopyCheckpoints {
    /// The walk, the limits, and what to leave out: nothing is read.
    fn plan(&self) -> ToolResult<CopyPlan> {
        let max_files = self.limits.max_copy_files;
        let found =
            folder_scan::scan_with(self.restorer.root(), self.rules, max_files, self.placeholder);
        if found.truncated {
            return Err(ToolError::new(
                ErrorCode::TooLarge,
                format!("the folder has more than {max_files} files to checkpoint"),
            ));
        }
        let max_file = self.limits.max_file_bytes.min(MAX_COPY_FILE_BYTES);
        let mut plan = CopyPlan {
            placeholders: found.placeholders,
            ..CopyPlan::default()
        };
        let mut total = 0u64;
        for (name, stat) in found.files {
            if stat.len > max_file {
                plan.too_large.insert(name);
                continue;
            }
            total += stat.len;
            plan.store.insert(name, stat);
        }
        if total > self.limits.max_copy_bytes {
            return Err(ToolError::new(
                ErrorCode::TooLarge,
                format!(
                    "the folder holds {} to checkpoint; the limit is {}",
                    human_bytes(total),
                    human_bytes(self.limits.max_copy_bytes)
                ),
            ));
        }
        Ok(plan)
    }

    fn read_cache(&self) -> BTreeMap<String, CachedFile> {
        std::fs::read(&self.cache)
            .ok()
            .and_then(|bytes| serde_json::from_slice(&bytes).ok())
            .unwrap_or_default()
    }

    /// Store one file (opened confined, never through a link): a clone
    /// where the file system can, a copy otherwise, named by the digest of
    /// what was stored. `None`: unreadable, or grown past the limit.
    fn store(&self, path: &WsPath, max_bytes: u64) -> Option<(String, u32, Option<FileStat>)> {
        #[cfg(test)]
        STORED.with(|stored| stored.borrow_mut().push(path.display_string()));
        let (mut file, _) = self.restorer.open_file(path).ok()?;
        let before = FileStat::of(&file.metadata().ok()?);
        let temp = self.objects.join(format!(
            "{}-{}.tmp",
            std::process::id(),
            TEMP_COUNTER.fetch_add(1, Ordering::Relaxed)
        ));
        let copied = clone_file::copy_into(&mut file, &temp, max_bytes, self.clone)
            .ok()
            .flatten()?;
        let sha = hex(&copied.sha256);
        let object = self.objects.join(&sha);
        let placed = if object.exists() {
            std::fs::remove_file(&temp)
        } else {
            std::fs::rename(&temp, &object)
        };
        if placed.is_err() {
            let _ = std::fs::remove_file(&temp);
            return None;
        }
        // Changed while it was stored: the digest names what was stored,
        // but this stat does not, so it is not cached.
        let after = file.metadata().ok().map(|meta| FileStat::of(&meta));
        let cacheable = (after == Some(before) && copied.len == before.len).then_some(before);
        Some((sha, before.mode, cacheable))
    }

    /// What file `path` held at checkpoint `seq`.
    fn file_at(&self, seq: u64, path: &WsPath, max_bytes: u64) -> ToolResult<Option<Vec<u8>>> {
        let manifest = self.read_manifest(seq)?;
        let Some(file) = manifest.files.get(&path.display_string()) else {
            return Ok(None);
        };
        let object = self.objects.join(&file.sha256);
        let len = std::fs::metadata(&object)
            .map_err(|error| ToolError::io("cannot read the checkpoint", &error))?
            .len();
        if len > max_bytes {
            return Err(ToolError::new(
                ErrorCode::TooLarge,
                format!("`{path}` was {len} bytes; the limit is {max_bytes}"),
            ));
        }
        std::fs::read(object)
            .map(Some)
            .map_err(|error| ToolError::io("cannot read the checkpoint", &error))
    }
    fn manifest_path(&self, seq: u64) -> PathBuf {
        self.manifests.join(format!("{seq:08}.json"))
    }

    fn read_manifest(&self, seq: u64) -> ToolResult<Manifest> {
        let bytes = std::fs::read(self.manifest_path(seq))
            .map_err(|_| ToolError::new(ErrorCode::NotFound, format!("no checkpoint {seq}")))?;
        serde_json::from_slice(&bytes)
            .map_err(|_| ToolError::new(ErrorCode::Io, format!("checkpoint {seq} is unreadable")))
    }

    /// Hold the folder's store exclusively: no other session's prune runs
    /// while this one stores contents its manifest does not name yet.
    fn lock(&self) -> ToolResult<File> {
        let io = |error: std::io::Error| ToolError::io("cannot lock the checkpoint store", &error);
        if let Some(base) = self.lock.parent() {
            std::fs::create_dir_all(base).map_err(io)?;
        }
        let file = File::options()
            .create(true)
            .truncate(false)
            .write(true)
            .open(&self.lock)
            .map_err(io)?;
        file.lock().map_err(io)?;
        Ok(file)
    }

    /// Take a checkpoint: a stat-only walk first (the limits are checked
    /// before anything is read), then each file whose stat is unchanged
    /// since the folder's last checkpoint reuses its stored digest
    /// ([`STAT_CACHE`]: size, modification time, inode and mode), and the
    /// rest are cloned or copied into the store and hashed.
    fn create(&self, label: &str) -> ToolResult<CheckpointInfo> {
        let io = |error: std::io::Error| ToolError::io("cannot store the checkpoint", &error);
        let plan = self.plan()?;
        let _lock = self.lock()?;
        std::fs::create_dir_all(&self.objects).map_err(io)?;
        std::fs::create_dir_all(&self.manifests).map_err(io)?;
        let mut manifest = Manifest {
            seq: self.list()?.last().map_or(1, |last| last.seq + 1),
            label: single_line(label),
            created_unix: now_unix(),
            files: BTreeMap::new(),
            skipped: plan
                .too_large
                .iter()
                .chain(&plan.placeholders)
                .cloned()
                .collect(),
            rules: Some(self.rules),
            too_large: plan.too_large,
            placeholders: plan.placeholders,
        };
        let cache = self.read_cache();
        let mut next_cache = BTreeMap::new();
        let max_file = self.limits.max_file_bytes.min(MAX_COPY_FILE_BYTES);
        let racy_after = i128::from(now_unix()) * 1_000_000_000 - RACY_WINDOW_NS;
        for (name, stat) in plan.store {
            let cached = cache.get(&name).filter(|cached| {
                cached.stat == stat
                    && stat.mtime_ns < racy_after
                    && self.objects.join(&cached.sha256).exists()
            });
            let (sha, mode, cacheable) = if let Some(cached) = cached {
                (cached.sha256.clone(), stat.mode, Some(stat))
            } else {
                let Ok(path) = WsPath::from_relative(Path::new(&name)) else {
                    manifest.skipped.insert(name);
                    continue;
                };
                // Unreadable, grown past the limit, or gone since the
                // walk: not stored, and left alone by a restore (never
                // deleted as if the turn had created it).
                let Some(stored) = self.store(&path, max_file) else {
                    manifest.skipped.insert(name);
                    continue;
                };
                stored
            };
            if let Some(stat) = cacheable.filter(|stat| stat.mtime_ns < racy_after) {
                next_cache.insert(
                    name.clone(),
                    CachedFile {
                        stat,
                        sha256: sha.clone(),
                    },
                );
            }
            manifest.files.insert(name, ManifestFile { sha256: sha, mode });
        }
        let bytes = serde_json::to_vec(&manifest)
            .map_err(|_| ToolError::new(ErrorCode::Io, "cannot encode the checkpoint"))?;
        write_atomically(&self.manifest_path(manifest.seq), &bytes, true).map_err(io)?;
        if let Ok(cache) = serde_json::to_vec(&next_cache) {
            // Only a cache: a failure costs the next checkpoint some hashing.
            let _ = write_atomically(&self.cache, &cache, false);
        }
        self.prune();
        Ok(CheckpointInfo {
            seq: manifest.seq,
            label: manifest.label.clone(),
            created_unix: manifest.created_unix,
            id: format!("{:08}", manifest.seq),
            ..CheckpointInfo::default()
        }
        .with_left_out(&manifest.too_large, &manifest.placeholders))
    }

    /// As [`GitCheckpoints::prune`], then remove stored contents no
    /// manifest of any session of this workspace refers to, and temporary
    /// files a crash left. Called with the store locked.
    fn prune(&self) {
        let Ok(all) = self.list() else { return };
        let cutoff = now_unix().saturating_sub(self.limits.max_age.as_secs());
        let excess = all.len().saturating_sub(self.limits.keep.max(1));
        for (index, info) in all.iter().enumerate() {
            let newest = index + 1 == all.len();
            if !newest && (index < excess || info.created_unix < cutoff) {
                let _ = std::fs::remove_file(self.manifest_path(info.seq));
            }
        }
        let Some(sessions) = self.manifests.parent() else {
            return;
        };
        let mut referenced = BTreeSet::new();
        for session in std::fs::read_dir(sessions).into_iter().flatten().flatten() {
            for manifest in std::fs::read_dir(session.path())
                .into_iter()
                .flatten()
                .flatten()
            {
                let Ok(bytes) = std::fs::read(manifest.path()) else {
                    // Unreadable: keep every object rather than lose one.
                    return;
                };
                let Ok(manifest) = serde_json::from_slice::<Manifest>(&bytes) else {
                    if manifest.path().extension().is_some_and(|ext| ext == "json") {
                        return;
                    }
                    continue;
                };
                referenced.extend(manifest.files.into_values().map(|file| file.sha256));
            }
        }
        for object in std::fs::read_dir(&self.objects)
            .into_iter()
            .flatten()
            .flatten()
        {
            let name = object.file_name().to_string_lossy().into_owned();
            let temporary = Path::new(&name)
                .extension()
                .is_some_and(|extension| extension == "tmp");
            if (name.len() == 64 && !referenced.contains(&name)) || temporary {
                let _ = std::fs::remove_file(object.path());
            }
        }
    }

    fn list(&self) -> ToolResult<Vec<CheckpointInfo>> {
        let entries = match std::fs::read_dir(&self.manifests) {
            Ok(entries) => entries,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(error) => return Err(ToolError::io("cannot list checkpoints", &error)),
        };
        let mut out = Vec::new();
        for entry in entries.flatten() {
            let name = entry.file_name();
            let Some(seq) = name
                .to_str()
                .and_then(|name| name.strip_suffix(".json"))
                .and_then(|seq| seq.parse::<u64>().ok())
            else {
                continue;
            };
            let manifest = self.read_manifest(seq)?;
            out.push(
                CheckpointInfo {
                    seq,
                    label: manifest.label.clone(),
                    created_unix: manifest.created_unix,
                    id: format!("{seq:08}"),
                    ..CheckpointInfo::default()
                }
                .with_left_out(&manifest.too_large, &manifest.placeholders),
            );
        }
        out.sort_by_key(|info| info.seq);
        Ok(out)
    }

    /// Remove the symlinks on the way to `path` and at it (the checkpoint
    /// had directories and a file there: copies never record links), so a
    /// write recreates them instead of writing through a link.
    fn unlink_links_on(&self, path: &WsPath, report: &mut RestoreReport) -> ToolResult<()> {
        let components = path.components();
        for end in 1..=components.len() {
            let prefix = WsPath::from_relative(Path::new(&components[..end].join("/")))?;
            match self.restorer.stat(&prefix)? {
                Some(EntryKind::Symlink) => {
                    if self.restorer.remove_file(&prefix)? {
                        report.deleted.push(prefix.display_string());
                    }
                    return Ok(());
                }
                Some(EntryKind::Dir) => {}
                _ => return Ok(()),
            }
        }
        Ok(())
    }

    fn restore(&self, seq: u64, only: Option<&WsPath>, dry_run: bool) -> ToolResult<RestoreReport> {
        let _lock = self.lock()?;
        let manifest = self.read_manifest(seq)?;
        let in_scope = |path: &str| only.is_none_or(|only| only.display_string() == path);
        // Not in the checkpoint, but ignored by its rules: it was not
        // copied, so it may be older than the checkpoint. Kept. A plain
        // folder's checkpoint honoured no ignore file: everything it walks
        // was copied (or skipped and named).
        let rules = manifest.rules();
        let mut ignores = CheckpointIgnores::new(&[".gitignore", ".ignore"], |file| {
            let stored = manifest.files.get(file)?;
            std::fs::read(self.objects.join(&stored.sha256)).ok()
        });
        let mut report = RestoreReport::default();
        for path in walk_files(self.restorer.root(), rules) {
            let name = path.display_string();
            if in_scope(&name)
                && !manifest.files.contains_key(&name)
                && !manifest.skipped.contains(&name)
                && (rules == ScanRules::Folder || !ignores.ignores(&name))
                && (dry_run || self.restorer.remove_file(&path)?)
            {
                report.deleted.push(name);
            }
        }
        drop(ignores);
        for (name, file) in &manifest.files {
            if !in_scope(name) {
                continue;
            }
            let path = WsPath::from_relative(Path::new(name))?;
            if !dry_run {
                self.unlink_links_on(&path, &mut report)?;
            }
            let unchanged = self
                .restorer
                .read(&path, MAX_COPY_FILE_BYTES)
                .is_ok_and(|read| read.mode == file.mode && hex(&read.stamp.sha256) == file.sha256);
            if unchanged {
                continue;
            }
            if dry_run {
                report.restored.push(name.clone());
                continue;
            }
            let bytes = std::fs::read(self.objects.join(&file.sha256)).map_err(|_| {
                ToolError::new(
                    ErrorCode::NotFound,
                    format!("checkpoint {seq} is missing the content of `{name}`"),
                )
            })?;
            self.restorer.write(&path, &bytes, Some(file.mode))?;
            report.restored.push(name.clone());
        }
        Ok(report)
    }
}

#[cfg(all(test, unix))]
mod tests {
    use std::path::Path;
    use std::process::Command;
    use std::time::Duration;

    use super::{
        CheckpointLimits, Checkpoints, GitCheckpoints, Pruned, REF_PREFIX, STALE_LOCK_AGE,
    };
    use crate::error::ErrorCode;
    use crate::workspace::{Intent, Workspace};

    fn git(dir: &Path, args: &[&str]) -> String {
        let output = Command::new("git")
            .current_dir(dir)
            .args([
                "-c",
                "user.name=T",
                "-c",
                "user.email=t@example.com",
                "-c",
                "commit.gpgsign=false",
                "-c",
                "init.defaultBranch=main",
            ])
            .args(args)
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .env("HOME", dir)
            .output()
            .expect("git");
        assert!(
            output.status.success(),
            "git {args:?}: {}",
            String::from_utf8_lossy(&output.stderr)
        );
        String::from_utf8_lossy(&output.stdout).trim().to_owned()
    }

    fn read(dir: &Path, name: &str) -> Option<String> {
        std::fs::read_to_string(dir.join(name)).ok()
    }

    fn repo() -> tempfile::TempDir {
        let dir = tempfile::tempdir().expect("dir");
        git(dir.path(), &["init", "-q"]);
        std::fs::write(dir.path().join(".gitignore"), "target/\n").expect("ignore");
        std::fs::write(dir.path().join("tracked.txt"), "v1\n").expect("tracked");
        std::fs::write(dir.path().join("doomed.txt"), "keep me\n").expect("doomed");
        git(dir.path(), &["add", "-A"]);
        git(dir.path(), &["commit", "-q", "-m", "init"]);
        dir
    }

    #[test]
    fn git_checkpoints_snapshot_without_touching_index_head_or_branch() {
        let dir = repo();
        let root = dir.path();
        std::fs::write(root.join("tracked.txt"), "v2 uncommitted\n").expect("edit");
        std::fs::write(root.join("untracked.txt"), "new\n").expect("untracked");
        std::fs::create_dir(root.join("target")).expect("target");
        std::fs::write(root.join("target/build.o"), "ignored").expect("ignored");
        git(root, &["add", "tracked.txt"]);
        let index_before = git(root, &["diff", "--cached", "--name-only"]);
        let head_before = git(root, &["rev-parse", "HEAD"]);
        let status_before = git(root, &["status", "--porcelain"]);

        let workspace = Workspace::open(root, &[]).expect("workspace");
        let data = tempfile::tempdir().expect("data");
        let checkpoints = Checkpoints::open(&workspace, "s1", data.path()).expect("open");
        assert_eq!(checkpoints.kind(), "git");
        let first = checkpoints.create("before turn 1").expect("create");
        assert_eq!(first.seq, 1);

        assert_eq!(
            git(root, &["diff", "--cached", "--name-only"]),
            index_before
        );
        assert_eq!(git(root, &["rev-parse", "HEAD"]), head_before);
        assert_eq!(git(root, &["status", "--porcelain"]), status_before);
        assert_eq!(git(root, &["stash", "list"]), "");
        let files = git(
            root,
            &[
                "ls-tree",
                "-r",
                "--name-only",
                &format!("{REF_PREFIX}/s1/1"),
            ],
        );
        assert!(files.contains("untracked.txt"));
        assert!(
            !files.contains("build.o"),
            "ignored files are not snapshotted"
        );
        assert_eq!(
            git(root, &["show", &format!("{REF_PREFIX}/s1/1:tracked.txt")]),
            "v2 uncommitted"
        );
        assert_eq!(
            git(root, &["rev-parse", &format!("{REF_PREFIX}/s1/1^")]),
            head_before,
            "HEAD is the parent"
        );

        // The turn: edit, create, delete.
        std::fs::write(root.join("tracked.txt"), "agent edit\n").expect("edit");
        std::fs::write(root.join("created.txt"), "by agent\n").expect("create");
        std::fs::remove_file(root.join("doomed.txt")).expect("delete");
        std::fs::remove_file(root.join("untracked.txt")).expect("delete");
        let second = checkpoints.create("before turn 2").expect("create");
        assert_eq!(second.seq, 2);

        let report = checkpoints.restore(1).expect("restore");
        assert_eq!(
            read(root, "tracked.txt").as_deref(),
            Some("v2 uncommitted\n")
        );
        assert_eq!(read(root, "doomed.txt").as_deref(), Some("keep me\n"));
        assert_eq!(read(root, "untracked.txt").as_deref(), Some("new\n"));
        assert_eq!(read(root, "created.txt"), None);
        assert_eq!(
            read(root, "target/build.o").as_deref(),
            Some("ignored"),
            "ignored files are left alone"
        );
        assert_eq!(report.deleted, ["created.txt"]);
        assert_eq!(
            git(root, &["diff", "--cached", "--name-only"]),
            index_before,
            "restore leaves the index alone"
        );
        assert_eq!(git(root, &["rev-parse", "HEAD"]), head_before);

        // Survives a restart: a new handle sees both.
        let reopened = Checkpoints::open(&workspace, "s1", data.path()).expect("reopen");
        let listed = reopened.list().expect("list");
        assert_eq!(
            listed.iter().map(|info| info.seq).collect::<Vec<_>>(),
            [1, 2]
        );
        assert_eq!(listed[1].label, "before turn 2");
        // One file back to checkpoint 2.
        let path = workspace
            .resolve("tracked.txt", Intent::Read)
            .expect("path");
        let one = reopened.restore_file(2, &path).expect("restore file");
        assert_eq!(one.restored, ["tracked.txt"]);
        assert_eq!(read(root, "tracked.txt").as_deref(), Some("agent edit\n"));
        assert_eq!(
            read(root, "doomed.txt").as_deref(),
            Some("keep me\n"),
            "other files untouched"
        );
        // Other sessions are separate.
        let other = Checkpoints::open(&workspace, "s2", data.path()).expect("other");
        assert_eq!(other.list().expect("list"), []);
        assert!(other.restore(1).is_err());
    }

    /// Every file under `root` (but `.git`) with its content.
    fn snapshot(root: &Path) -> Vec<(String, String)> {
        let mut out = Vec::new();
        let mut stack = vec![root.to_path_buf()];
        while let Some(dir) = stack.pop() {
            for entry in std::fs::read_dir(&dir).expect("read dir") {
                let path = entry.expect("entry").path();
                if path.file_name().is_some_and(|name| name == ".git") {
                    continue;
                }
                if path.is_dir() {
                    stack.push(path);
                } else {
                    let name = path.strip_prefix(root).expect("inside");
                    let text = std::fs::read_to_string(&path).unwrap_or_default();
                    out.push((name.to_string_lossy().into_owned(), text));
                }
            }
        }
        out.sort();
        out
    }

    #[test]
    fn a_preview_names_what_a_restore_would_change_and_changes_nothing() {
        for in_git in [true, false] {
            let dir = tempfile::tempdir().expect("dir");
            let root = dir.path().join("folder");
            std::fs::create_dir_all(&root).expect("folder");
            if in_git {
                git(&root, &["init", "-q"]);
            }
            std::fs::write(root.join("kept.txt"), "k\n").expect("kept");
            std::fs::write(root.join("edited.txt"), "before\n").expect("edited");
            std::fs::write(root.join("removed.txt"), "r\n").expect("removed");
            let workspace = Workspace::open(&root, &[]).expect("workspace");
            let data = tempfile::tempdir().expect("data");
            let checkpoints = Checkpoints::open(&workspace, "s", data.path()).expect("open");
            assert_eq!(checkpoints.kind(), if in_git { "git" } else { "copy" });
            checkpoints.create("before").expect("create");
            std::fs::write(root.join("edited.txt"), "after\n").expect("edit");
            std::fs::remove_file(root.join("removed.txt")).expect("remove");
            std::fs::write(root.join("created.txt"), "c\n").expect("create");

            let before = snapshot(&root);
            let mut preview = checkpoints.preview(1).expect("preview");
            assert_eq!(snapshot(&root), before, "a preview writes nothing");
            preview.restored.sort();
            assert_eq!(
                preview.restored,
                ["edited.txt", "removed.txt"],
                "git: {in_git}"
            );
            assert_eq!(preview.deleted, ["created.txt"], "git: {in_git}");
            let mut done = checkpoints.restore(1).expect("restore");
            done.restored.sort();
            assert_eq!(done, preview, "the restore did what it said, git: {in_git}");
        }
    }

    #[test]
    fn git_checkpoints_work_in_a_subdirectory_and_an_unborn_repository() {
        let dir = tempfile::tempdir().expect("dir");
        let root = dir.path();
        git(root, &["init", "-q"]);
        std::fs::create_dir(root.join("app")).expect("app");
        std::fs::write(root.join("app/a.txt"), "a\n").expect("a");
        std::fs::write(root.join("outside.txt"), "o\n").expect("o");
        let workspace = Workspace::open(&root.join("app"), &[]).expect("workspace");
        let data = tempfile::tempdir().expect("data");
        let checkpoints = Checkpoints::open(&workspace, "s", data.path()).expect("open");
        checkpoints.create("first").expect("create");
        std::fs::write(root.join("app/a.txt"), "changed\n").expect("edit");
        std::fs::write(root.join("app/b.txt"), "b\n").expect("b");
        std::fs::write(root.join("outside.txt"), "changed outside\n").expect("edit outside");
        let report = checkpoints.restore(1).expect("restore");
        assert_eq!(read(root, "app/a.txt").as_deref(), Some("a\n"));
        assert_eq!(read(root, "app/b.txt"), None);
        assert_eq!(
            read(root, "outside.txt").as_deref(),
            Some("changed outside\n"),
            "outside the workspace is not restored"
        );
        assert_eq!(report.restored, ["a.txt"]);
        assert_eq!(report.deleted, ["b.txt"]);
    }

    #[test]
    fn copy_checkpoints_restore_turns_and_files_in_a_plain_folder() {
        use std::os::unix::fs::PermissionsExt;
        let dir = tempfile::tempdir().expect("dir");
        let root = dir.path().join("folder");
        std::fs::create_dir_all(root.join("sub")).expect("folder");
        std::fs::write(root.join(".gitignore"), "cache/\n").expect("ignore");
        std::fs::write(root.join("a.txt"), "a1\n").expect("a");
        std::fs::write(root.join("sub/run.sh"), "#!/bin/sh\n").expect("script");
        std::fs::set_permissions(
            root.join("sub/run.sh"),
            std::fs::Permissions::from_mode(0o755),
        )
        .expect("chmod");
        std::fs::create_dir(root.join("cache")).expect("cache");
        std::fs::write(root.join("cache/blob"), "ignored").expect("cache");
        let workspace = Workspace::open(&root, &[]).expect("workspace");
        let data = tempfile::tempdir().expect("data");
        let checkpoints = Checkpoints::open(&workspace, "s1", data.path()).expect("open");
        assert_eq!(checkpoints.kind(), "copy");
        checkpoints.create("before").expect("create");

        std::fs::write(root.join("a.txt"), "a2\n").expect("edit");
        std::fs::remove_file(root.join("sub/run.sh")).expect("delete");
        std::fs::write(root.join("new.txt"), "n\n").expect("new");
        std::fs::write(root.join("cache/blob"), "ignored changed").expect("cache");
        checkpoints.create("after").expect("create");

        let path = workspace.resolve("a.txt", Intent::Read).expect("path");
        let one = checkpoints.restore_file(1, &path).expect("restore file");
        assert_eq!(one.restored, ["a.txt"]);
        assert_eq!(
            read(&root, "new.txt").as_deref(),
            Some("n\n"),
            "a file restore touches one file"
        );

        let report = checkpoints.restore(1).expect("restore");
        assert_eq!(read(&root, "a.txt").as_deref(), Some("a1\n"));
        assert_eq!(read(&root, "sub/run.sh").as_deref(), Some("#!/bin/sh\n"));
        let mode = std::fs::metadata(root.join("sub/run.sh"))
            .expect("meta")
            .permissions()
            .mode();
        assert_eq!(mode & 0o777, 0o755);
        assert_eq!(read(&root, "new.txt"), None);
        // A plain folder's `.gitignore` does not decide what undo covers:
        // the ignored file was saved and is put back too.
        assert_eq!(read(&root, "cache/blob").as_deref(), Some("ignored"));
        assert_eq!(report.deleted, ["new.txt"]);

        let reopened = Checkpoints::open(&workspace, "s1", data.path()).expect("reopen");
        assert_eq!(
            reopened.list().expect("list").len(),
            2,
            "copies survive a restart"
        );
        assert!(Checkpoints::open(&workspace, "../evil", data.path()).is_err());
    }

    fn small_limits() -> CheckpointLimits {
        CheckpointLimits {
            max_file_bytes: 1024,
            ..CheckpointLimits::default()
        }
    }

    /// M5 / L2: large files and special files stay out of a git
    /// checkpoint, and a restore leaves what it skipped alone.
    #[test]
    fn git_checkpoints_skip_large_and_special_files_and_restore_leaves_them() {
        let dir = repo();
        let root = dir.path();
        std::fs::write(root.join("big.bin"), vec![b'x'; 4096]).expect("big");
        std::fs::write(root.join("grows.bin"), vec![b'y'; 4096]).expect("grows");
        let fifo = Command::new("mkfifo")
            .arg(root.join("pipe"))
            .status()
            .expect("mkfifo");
        assert!(fifo.success());
        let workspace = Workspace::open(root, &[]).expect("workspace");
        let data = tempfile::tempdir().expect("data");
        let checkpoints = Checkpoints::open(&workspace, "s", data.path())
            .expect("open")
            .with_limits(small_limits());
        assert_eq!(checkpoints.kind(), "git");
        checkpoints
            .create("turn")
            .expect("a FIFO does not fail the checkpoint");
        let files = git(
            root,
            &["ls-tree", "-r", "--name-only", &format!("{REF_PREFIX}/s/1")],
        );
        assert!(!files.contains("big.bin"), "{files}");
        assert!(!files.contains("pipe"), "{files}");
        assert!(files.contains("tracked.txt"));

        std::fs::write(root.join("tracked.txt"), "agent\n").expect("edit");
        std::fs::write(root.join("big.bin"), vec![b'z'; 8192]).expect("big edit");
        std::fs::write(root.join("grows.bin"), "now small\n").expect("shrink");
        let report = checkpoints.restore(1).expect("restore");
        assert_eq!(read(root, "tracked.txt").as_deref(), Some("v1\n"));
        assert_eq!(
            std::fs::read(root.join("big.bin")).expect("big").len(),
            8192,
            "a skipped file is left as it is"
        );
        assert_eq!(
            read(root, "grows.bin").as_deref(),
            Some("now small\n"),
            "a file skipped then is not deleted now"
        );
        assert!(!report.deleted.iter().any(|path| path.contains(".bin")));
    }

    #[test]
    fn too_many_untracked_bytes_skip_the_checkpoint() {
        let dir = repo();
        std::fs::write(dir.path().join("dump.json"), vec![b'1'; 900]).expect("dump");
        let workspace = Workspace::open(dir.path(), &[]).expect("workspace");
        let data = tempfile::tempdir().expect("data");
        let checkpoints = Checkpoints::open(&workspace, "s", data.path())
            .expect("open")
            .with_limits(CheckpointLimits {
                max_untracked_bytes: 500,
                ..CheckpointLimits::default()
            });
        assert_eq!(
            checkpoints.create("turn").expect_err("too large").code(),
            ErrorCode::TooLarge
        );
        assert_eq!(checkpoints.list().expect("list"), []);
    }

    #[test]
    fn old_checkpoints_are_pruned_in_git_and_copies() {
        let limits = CheckpointLimits {
            keep: 2,
            ..CheckpointLimits::default()
        };
        let dir = repo();
        let plain = tempfile::tempdir().expect("plain");
        for root in [dir.path(), plain.path()] {
            let workspace = Workspace::open(root, &[]).expect("workspace");
            let data = tempfile::tempdir().expect("data");
            let checkpoints = Checkpoints::open(&workspace, "s", data.path())
                .expect("open")
                .with_limits(limits);
            for turn in 0..4 {
                std::fs::write(root.join("f.txt"), format!("turn {turn}\n")).expect("edit");
                checkpoints.create(&format!("turn {turn}")).expect("create");
            }
            let seqs: Vec<u64> = checkpoints
                .list()
                .expect("list")
                .iter()
                .map(|info| info.seq)
                .collect();
            assert_eq!(seqs, [3, 4], "{}", checkpoints.kind());
            if checkpoints.kind() == "copy" {
                let objects = walk_count(&data.path().join("checkpoints"), "objects");
                assert_eq!(objects, 2, "contents only the kept checkpoints use");
            }
        }
    }

    /// A pruned loose ref goes under git's own `<ref>.lock` protocol: a ref
    /// whose lock another writer holds stays, and one removed takes its
    /// reflog with it, so nothing keeps the commit reachable.
    #[test]
    fn pruning_respects_the_ref_lock_and_removes_the_reflog() {
        let dir = repo();
        let root = dir.path();
        let workspace = Workspace::open(root, &[]).expect("workspace");
        let data = tempfile::tempdir().expect("data");
        let checkpoints = Checkpoints::open(&workspace, "s", data.path())
            .expect("open")
            .with_limits(CheckpointLimits {
                keep: 2,
                ..CheckpointLimits::default()
            });
        let seqs = || -> Vec<u64> {
            checkpoints
                .list()
                .expect("list")
                .iter()
                .map(|info| info.seq)
                .collect()
        };
        for turn in 0..2 {
            std::fs::write(root.join("f.txt"), format!("turn {turn}\n")).expect("edit");
            checkpoints.create(&format!("turn {turn}")).expect("create");
        }
        let git_dir = root.join(".git");
        let reflog = git_dir.join("logs").join(REF_PREFIX).join("s/1");
        std::fs::create_dir_all(reflog.parent().expect("parent")).expect("logs");
        std::fs::write(&reflog, "reflog\n").expect("reflog");
        let lock = git_dir.join(REF_PREFIX).join("s/1.lock");
        std::fs::write(&lock, "").expect("lock");

        std::fs::write(root.join("f.txt"), "turn 2\n").expect("edit");
        checkpoints.create("turn 2").expect("create");
        assert_eq!(seqs(), [1, 2, 3], "a locked ref is not dropped");
        assert!(reflog.exists());

        std::fs::remove_file(&lock).expect("unlock");
        std::fs::write(root.join("f.txt"), "turn 3\n").expect("edit");
        checkpoints.create("turn 3").expect("create");
        assert_eq!(seqs(), [3, 4]);
        assert!(!reflog.exists(), "the reflog goes with the ref");
        assert!(
            !git_dir.join(REF_PREFIX).join("s/1.lock").exists(),
            "the lock is released"
        );
    }

    /// A session with two checkpoints under `keep: 2`: creating a third
    /// prunes the first.
    fn pruning_fixture() -> (tempfile::TempDir, tempfile::TempDir, Checkpoints) {
        let dir = repo();
        let workspace = Workspace::open(dir.path(), &[]).expect("workspace");
        let data = tempfile::tempdir().expect("data");
        let checkpoints = Checkpoints::open(&workspace, "s", data.path())
            .expect("open")
            .with_limits(CheckpointLimits {
                keep: 2,
                ..CheckpointLimits::default()
            });
        for turn in 0..2 {
            std::fs::write(dir.path().join("f.txt"), format!("turn {turn}\n")).expect("edit");
            checkpoints.create(&format!("turn {turn}")).expect("create");
        }
        (dir, data, checkpoints)
    }

    fn seqs_of(checkpoints: &Checkpoints) -> Vec<u64> {
        checkpoints
            .list()
            .expect("list")
            .iter()
            .map(|info| info.seq)
            .collect()
    }

    fn git_side(checkpoints: &Checkpoints) -> &GitCheckpoints {
        match checkpoints {
            Checkpoints::Git(side) => side,
            Checkpoints::Copy(_) => panic!("a git work tree"),
        }
    }

    /// A `<ref>.lock` a crash left behind (older than the stale age) is
    /// taken over, so its checkpoint is pruned and the lock does not stay.
    #[test]
    fn a_stale_ref_lock_is_taken_over_by_pruning() {
        let (dir, _data, checkpoints) = pruning_fixture();
        let lock = dir.path().join(".git").join(REF_PREFIX).join("s/1.lock");
        let file = std::fs::File::create(&lock).expect("lock");
        file.set_modified(std::time::SystemTime::now() - STALE_LOCK_AGE - Duration::from_secs(60))
            .expect("age the lock");
        drop(file);

        std::fs::write(dir.path().join("f.txt"), "turn 2\n").expect("edit");
        checkpoints.create("turn 2").expect("create");
        assert_eq!(
            seqs_of(&checkpoints),
            [2, 3],
            "the stale lock no longer pins checkpoint 1"
        );
        assert!(!lock.exists(), "the taken-over lock is released");
    }

    /// A session whose refs are all pruned leaves no empty directory, and
    /// another session's directory stays.
    #[test]
    fn prune_loose_removes_empty_session_directories() {
        let (dir, _data, checkpoints) = pruning_fixture();
        let repo_dir = dir.path().join(".git");
        let session_refs = repo_dir.join(REF_PREFIX).join("s");
        let session_logs = repo_dir.join("logs").join(REF_PREFIX).join("s");
        std::fs::create_dir_all(&session_logs).expect("logs");
        let other = repo_dir.join(REF_PREFIX).join("other");
        std::fs::create_dir_all(&other).expect("other");
        std::fs::write(other.join("keep"), "x").expect("other ref");
        for info in &checkpoints.list().expect("list") {
            assert!(matches!(
                git_side(&checkpoints).prune_loose(info),
                Pruned::Done
            ));
        }
        assert!(
            !session_refs.exists(),
            "the emptied session directory is removed"
        );
        assert!(!session_logs.exists(), "and its empty logs directory");
        assert!(
            other.join("keep").exists(),
            "another session's directory stays"
        );
        assert!(
            repo_dir.join(REF_PREFIX).exists(),
            "the shared prefix stays"
        );
    }

    /// A ref whose content is not the checkpoint's id is not removed (it
    /// changed under us), and a packed ref (no loose file) is removed from
    /// `packed-refs` by the host, which the Linux sandbox could not do.
    #[test]
    fn prune_loose_leaves_a_changed_ref_and_packed_refs_fall_through() {
        let (dir, _data, checkpoints) = pruning_fixture();
        let repo_dir = dir.path().join(".git");
        let all = checkpoints.list().expect("list");
        let first = &all[0];
        let loose = repo_dir.join(REF_PREFIX).join("s/1");
        let original = std::fs::read_to_string(&loose).expect("ref");
        std::fs::write(&loose, "0000000000000000000000000000000000000000\n").expect("change");
        assert!(matches!(
            git_side(&checkpoints).prune_loose(first),
            Pruned::NotLoose
        ));
        assert!(loose.exists(), "a ref that changed is left alone");
        assert!(
            !repo_dir.join(REF_PREFIX).join("s/1.lock").exists(),
            "and its lock is released"
        );
        std::fs::write(&loose, original).expect("restore");

        git(dir.path(), &["pack-refs", "--all"]);
        assert!(!loose.exists(), "packed");
        assert!(matches!(
            git_side(&checkpoints).prune_loose(first),
            Pruned::NotLoose
        ));
        std::fs::write(dir.path().join("f.txt"), "turn 2\n").expect("edit");
        checkpoints.create("turn 2").expect("create");
        assert_eq!(
            seqs_of(&checkpoints),
            [2, 3],
            "the packed ref is pruned from packed-refs"
        );
        let packed = std::fs::read_to_string(repo_dir.join("packed-refs")).expect("packed");
        assert!(!packed.contains(&format!("{REF_PREFIX}/s/1\n")), "{packed}");
        assert!(packed.contains(&format!("{REF_PREFIX}/s/2\n")), "{packed}");
        assert!(
            !repo_dir.join("packed-refs.lock").exists(),
            "the lock became packed-refs"
        );
        // git still reads the rewritten file.
        let listed = git(
            dir.path(),
            &["for-each-ref", "--format=%(refname)", REF_PREFIX],
        );
        assert!(!listed.contains("/s/1\n"), "{listed}");
    }

    /// A packed ref that names another commit is left alone; a held
    /// `packed-refs.lock` is another writer's, so nothing is pruned; a loose
    /// ref with a stale packed copy loses both, or the copy would bring it
    /// back.
    #[test]
    fn prune_packed_respects_a_changed_line_the_lock_and_a_stale_copy() {
        let (dir, _data, checkpoints) = pruning_fixture();
        let repo_dir = dir.path().join(".git");
        let all = checkpoints.list().expect("list");
        let first = &all[0];
        let name = format!("{REF_PREFIX}/s/1");
        git(dir.path(), &["pack-refs", "--all"]);
        let other = "1".repeat(first.id.len());
        assert!(matches!(
            git_side(&checkpoints).prune_packed(&name, Some(&other)),
            Pruned::NotLoose
        ));
        assert!(
            !repo_dir.join("packed-refs.lock").exists(),
            "the lock is released"
        );
        std::fs::write(repo_dir.join("packed-refs.lock"), "").expect("held");
        assert!(matches!(
            git_side(&checkpoints).prune_packed(&name, None),
            Pruned::Busy
        ));
        assert!(
            repo_dir.join("packed-refs.lock").exists(),
            "another writer's lock stays"
        );
        std::fs::remove_file(repo_dir.join("packed-refs.lock")).expect("release");

        // A loose copy over the packed one: both go.
        std::fs::create_dir_all(repo_dir.join(&name).parent().expect("parent")).expect("dir");
        std::fs::write(repo_dir.join(&name), format!("{}\n", first.id)).expect("loose copy");
        assert!(repo_dir.join(&name).exists(), "loose again");
        std::fs::write(dir.path().join("f.txt"), "turn 2\n").expect("edit");
        checkpoints.create("turn 2").expect("create");
        assert_eq!(
            seqs_of(&checkpoints),
            [2, 3],
            "the stale packed copy went too"
        );
        let packed = std::fs::read_to_string(repo_dir.join("packed-refs")).expect("packed");
        assert!(!packed.contains(&format!("{name}\n")), "{packed}");
    }

    /// A file ignored when the checkpoint was taken and un-ignored during
    /// the turn was not created by the turn: a restore keeps it (whole or
    /// one file), in git and in copies.
    #[test]
    fn restore_keeps_files_that_were_ignored_at_checkpoint_time() {
        let dir = repo();
        let plain = tempfile::tempdir().expect("plain");
        for root in [dir.path(), plain.path()] {
            std::fs::write(root.join(".gitignore"), "target/\n.env\n").expect("ignore");
            std::fs::write(root.join(".env"), "SECRET=1\n").expect("env");
            std::fs::create_dir_all(root.join("target")).expect("target");
            std::fs::write(root.join("target/out.o"), "built").expect("built");
            let workspace = Workspace::open(root, &[]).expect("workspace");
            let data = tempfile::tempdir().expect("data");
            let checkpoints = Checkpoints::open(&workspace, "s", data.path()).expect("open");
            let kind = checkpoints.kind();
            checkpoints.create("turn").expect("create");

            // The turn un-ignores both and creates one file of its own.
            std::fs::write(root.join(".gitignore"), "# nothing ignored\n").expect("unignore");
            std::fs::write(root.join("created.txt"), "by the turn\n").expect("created");
            let report = checkpoints.restore(1).expect("restore");
            assert_eq!(
                read(root, ".gitignore").as_deref(),
                Some("target/\n.env\n"),
                "{kind}"
            );
            assert_eq!(read(root, ".env").as_deref(), Some("SECRET=1\n"), "{kind}");
            assert_eq!(
                read(root, "target/out.o").as_deref(),
                Some("built"),
                "{kind}"
            );
            assert_eq!(read(root, "created.txt"), None, "{kind}");
            assert_eq!(report.deleted, ["created.txt"], "{kind}");

            std::fs::write(root.join(".gitignore"), "\n").expect("unignore");
            let path = workspace.resolve(".env", Intent::Read).expect("path");
            let one = checkpoints.restore_file(1, &path).expect("restore file");
            assert!(one.deleted.is_empty(), "{kind}: {one:?}");
            assert_eq!(read(root, ".env").as_deref(), Some("SECRET=1\n"), "{kind}");
        }
    }

    /// A file a copy checkpoint could not read is recorded as skipped, so a
    /// restore leaves it alone instead of deleting it as new.
    #[test]
    fn copy_checkpoints_keep_files_they_could_not_read() {
        use std::os::unix::fs::PermissionsExt;
        let dir = tempfile::tempdir().expect("dir");
        let root = dir.path();
        std::fs::write(root.join("a.txt"), "a\n").expect("a");
        std::fs::write(root.join("locked.txt"), "private\n").expect("locked");
        std::fs::set_permissions(
            root.join("locked.txt"),
            std::fs::Permissions::from_mode(0o000),
        )
        .expect("chmod");
        let workspace = Workspace::open(root, &[]).expect("workspace");
        let data = tempfile::tempdir().expect("data");
        let checkpoints = Checkpoints::open(&workspace, "s", data.path()).expect("open");
        assert_eq!(checkpoints.kind(), "copy");
        checkpoints.create("turn").expect("create");
        std::fs::write(root.join("a.txt"), "changed\n").expect("edit");
        let report = checkpoints.restore(1).expect("restore");
        std::fs::set_permissions(
            root.join("locked.txt"),
            std::fs::Permissions::from_mode(0o644),
        )
        .expect("chmod back");
        assert!(report.deleted.is_empty(), "{report:?}");
        assert_eq!(read(root, "locked.txt").as_deref(), Some("private\n"));
        assert_eq!(read(root, "a.txt").as_deref(), Some("a\n"));
    }

    /// The turn replaced a directory with a symlink to another one: the
    /// restore recreates the directory instead of writing through the link.
    #[test]
    fn copy_restore_does_not_write_through_a_symlink_the_turn_made() {
        let dir = tempfile::tempdir().expect("dir");
        let root = dir.path();
        std::fs::create_dir_all(root.join("src")).expect("src");
        std::fs::create_dir_all(root.join("lib")).expect("lib");
        std::fs::write(root.join("src/a.txt"), "src a\n").expect("src a");
        std::fs::write(root.join("lib/a.txt"), "lib a\n").expect("lib a");
        std::fs::write(root.join("b.txt"), "b\n").expect("b");
        std::fs::write(root.join("lib/b.txt"), "lib b\n").expect("lib b");
        let workspace = Workspace::open(root, &[]).expect("workspace");
        let data = tempfile::tempdir().expect("data");
        let checkpoints = Checkpoints::open(&workspace, "s", data.path()).expect("open");
        checkpoints.create("turn").expect("create");

        std::fs::remove_dir_all(root.join("src")).expect("rm src");
        crate::sys::os::symlink("lib", root.join("src")).expect("link dir");
        std::fs::remove_file(root.join("b.txt")).expect("rm b");
        crate::sys::os::symlink("lib/b.txt", root.join("b.txt")).expect("link file");
        checkpoints.restore(1).expect("restore");

        assert!(
            !std::fs::symlink_metadata(root.join("src"))
                .expect("src")
                .file_type()
                .is_symlink(),
            "src is a directory again"
        );
        assert_eq!(read(root, "src/a.txt").as_deref(), Some("src a\n"));
        assert_eq!(read(root, "lib/a.txt").as_deref(), Some("lib a\n"));
        assert!(
            !std::fs::symlink_metadata(root.join("b.txt"))
                .expect("b")
                .file_type()
                .is_symlink()
        );
        assert_eq!(read(root, "b.txt").as_deref(), Some("b\n"));
        assert_eq!(read(root, "lib/b.txt").as_deref(), Some("lib b\n"));
    }

    /// Sessions of one folder share `objects/`: one's prune never removes
    /// what another's create is storing.
    #[test]
    fn concurrent_copy_checkpoints_never_lose_contents() {
        let dir = tempfile::tempdir().expect("dir");
        let root = dir.path().to_path_buf();
        for file in 0..40 {
            std::fs::write(root.join(format!("f{file}.txt")), "seed\n").expect("seed");
        }
        let data = tempfile::tempdir().expect("data");
        let limits = CheckpointLimits {
            keep: 1,
            ..CheckpointLimits::default()
        };
        let threads: Vec<_> = ["a", "b", "c"]
            .into_iter()
            .map(|session| {
                let (root, data) = (root.clone(), data.path().to_path_buf());
                std::thread::spawn(move || {
                    let workspace = Workspace::open(&root, &[]).expect("workspace");
                    let checkpoints = Checkpoints::open(&workspace, session, &data)
                        .expect("open")
                        .with_limits(limits);
                    for turn in 0..40 {
                        for file in 0..40 {
                            let _ = std::fs::write(
                                root.join(format!("f{file}.txt")),
                                format!("{session} {turn} {file}\n"),
                            );
                        }
                        let info = checkpoints.create("turn").expect("create");
                        let Checkpoints::Copy(copy) = &checkpoints else {
                            panic!("a plain folder has copy checkpoints");
                        };
                        let manifest = copy.read_manifest(info.seq).expect("manifest");
                        let missing = manifest
                            .files
                            .values()
                            .filter(|file| !copy.objects.join(&file.sha256).exists())
                            .count();
                        assert_eq!(missing, 0, "{session} turn {turn}: contents pruned");
                    }
                    checkpoints
                })
            })
            .collect();
        for thread in threads {
            let checkpoints = thread.join().expect("thread");
            let last = checkpoints.list().expect("list").last().expect("one").seq;
            checkpoints
                .restore(last)
                .expect("every content of the kept checkpoint is still stored");
        }
    }

    fn walk_count(base: &Path, name: &str) -> usize {
        std::fs::read_dir(base)
            .into_iter()
            .flatten()
            .flatten()
            .map(|entry| entry.path().join(name))
            .filter_map(|objects| std::fs::read_dir(objects).ok())
            .map(Iterator::count)
            .sum()
    }

    /// A task worktree (#1235): its checkpoints live in the repository's
    /// shared object store and refs, under the worktree session's own
    /// namespace, through a temporary index: neither the repository's
    /// index nor the worktree's own is touched.
    #[test]
    fn checkpoint_in_worktree_uses_shared_objects_and_own_index() {
        let dir = repo();
        let root = crate::sys::canonicalize(dir.path()).expect("canonical");
        let data = tempfile::tempdir().expect("data");
        let data_root = crate::sys::canonicalize(data.path()).expect("canonical");
        let main = crate::git::Repo::discover(
            &root,
            &crate::sandbox::SandboxConfig::default(),
            std::sync::Arc::new(crate::deny::DenyList::new([])),
        )
        .expect("discover")
        .expect("repo");
        std::fs::create_dir_all(data_root.join("worktrees")).expect("dir");
        let created = crate::worktree::create(
            &main,
            &data_root.join("worktrees/0123abcd"),
            "0123abcd",
            "elitea/0123abcd",
            "HEAD",
        )
        .expect("create");
        let tree = created.entry.worktree.clone();
        let main_index = std::fs::read(root.join(".git/index")).expect("main index");
        let own_index = root.join(".git/worktrees/0123abcd/index");
        let own_before = std::fs::read(&own_index).expect("own index");
        std::fs::write(tree.join("tracked.txt"), "worktree edit\n").expect("edit");

        let workspace = Workspace::open(&tree, &[]).expect("workspace");
        let checkpoints = Checkpoints::open(&workspace, "wt1", &data_root).expect("open");
        assert_eq!(checkpoints.kind(), "git");
        let repo = checkpoints.repo().expect("repo");
        assert!(repo.is_linked_worktree());
        assert_eq!(repo.common_dir(), root.join(".git").as_path());
        let info = checkpoints.create("turn").expect("checkpoint");
        // The ref is the session's, in the repository's refs; the commit is
        // in the shared object store.
        assert_eq!(
            git(
                &root,
                &["rev-parse", &format!("{REF_PREFIX}/wt1/{}", info.seq)]
            ),
            info.id
        );
        assert_eq!(git(&root, &["cat-file", "-t", &info.id]), "commit");
        assert_eq!(
            std::fs::read(root.join(".git/index")).expect("main"),
            main_index
        );
        assert_eq!(std::fs::read(&own_index).expect("own"), own_before);
        assert_eq!(git(&tree, &["branch", "--show-current"]), "elitea/0123abcd");

        std::fs::write(tree.join("tracked.txt"), "turn wrote this\n").expect("turn");
        checkpoints.restore(info.seq).expect("restore");
        assert_eq!(
            read(&tree, "tracked.txt").as_deref(),
            Some("worktree edit\n")
        );
        // The person's folder never moved.
        assert_eq!(read(&root, "tracked.txt").as_deref(), Some("v1\n"));
        crate::worktree::remove(&main, &created.entry, false).expect("remove");
        assert!(!tree.exists());
        assert!(!root.join(".git/worktrees/0123abcd").exists());
    }

    /// Back-date `path` past the racy window, as a file the person saved
    /// a while ago.
    fn age(path: &Path) {
        let then = std::time::SystemTime::now() - Duration::from_secs(3600);
        std::fs::File::options()
            .write(true)
            .open(path)
            .expect("open")
            .set_modified(then)
            .expect("set mtime");
    }

    fn stored() -> Vec<String> {
        super::STORED.with(|stored| std::mem::take(&mut *stored.borrow_mut()))
    }

    fn plain_folder() -> (tempfile::TempDir, Workspace, tempfile::TempDir) {
        let dir = tempfile::tempdir().expect("dir");
        let workspace = Workspace::open(dir.path(), &[]).expect("workspace");
        let data = tempfile::tempdir().expect("data");
        (dir, workspace, data)
    }

    #[test]
    fn copy_checkpoints_do_not_read_files_unchanged_since_the_last_one() {
        let (dir, workspace, data) = plain_folder();
        for name in ["a.txt", "b.txt", "c.txt"] {
            std::fs::write(dir.path().join(name), name).expect("write");
            age(&dir.path().join(name));
        }
        let checkpoints = Checkpoints::open(&workspace, "s", data.path()).expect("open");
        stored();
        checkpoints.create("one").expect("one");
        assert_eq!(stored(), ["a.txt", "b.txt", "c.txt"]);
        // Nothing changed: nothing is read again, and the checkpoint still
        // restores every file.
        checkpoints.create("two").expect("two");
        assert_eq!(stored(), Vec::<String>::new());
        std::fs::write(dir.path().join("b.txt"), "changed").expect("change");
        age(&dir.path().join("b.txt"));
        checkpoints.create("three").expect("three");
        assert_eq!(stored(), ["b.txt"]);
        std::fs::write(dir.path().join("a.txt"), "lost").expect("lose");
        std::fs::write(dir.path().join("b.txt"), "lost").expect("lose");
        checkpoints.restore(2).expect("restore");
        assert_eq!(read(dir.path(), "a.txt").as_deref(), Some("a.txt"));
        assert_eq!(read(dir.path(), "b.txt").as_deref(), Some("b.txt"));
        stored();
        // Another session of the folder shares the cache: only the two
        // files the restore rewrote (new stats) are read.
        let other = Checkpoints::open(&workspace, "t", data.path()).expect("open");
        other.create("other").expect("other");
        assert_eq!(stored(), ["a.txt", "b.txt"]);
    }

    #[test]
    fn a_file_saved_just_now_is_read_again_even_with_its_stat_unchanged() {
        let (dir, workspace, data) = plain_folder();
        std::fs::write(dir.path().join("fresh.txt"), "x").expect("write");
        let checkpoints = Checkpoints::open(&workspace, "s", data.path()).expect("open");
        stored();
        checkpoints.create("one").expect("one");
        checkpoints.create("two").expect("two");
        // Within the racy window the stat proves nothing: read both times.
        assert_eq!(stored(), ["fresh.txt", "fresh.txt"]);
    }

    #[test]
    fn copy_checkpoints_store_plain_copies_when_cloning_is_off() {
        let (dir, workspace, data) = plain_folder();
        std::fs::write(dir.path().join("doc.md"), "# notes\n").expect("write");
        let checkpoints = Checkpoints::open(&workspace, "s", data.path())
            .expect("open")
            .without_clones();
        checkpoints.create("one").expect("one");
        std::fs::write(dir.path().join("doc.md"), "gone").expect("edit");
        checkpoints.restore(1).expect("restore");
        assert_eq!(read(dir.path(), "doc.md").as_deref(), Some("# notes\n"));
    }

    #[test]
    fn cloud_placeholders_are_never_read_and_are_named_in_the_checkpoint() {
        fn simulated(path: &Path, metadata: &std::fs::Metadata) -> bool {
            path.ends_with("cloud.pdf") || crate::folder_scan::is_placeholder(path, metadata)
        }
        let (dir, workspace, data) = plain_folder();
        std::fs::write(dir.path().join("here.docx"), "local").expect("write");
        std::fs::write(dir.path().join("cloud.pdf"), "").expect("write");
        let checkpoints = Checkpoints::open(&workspace, "s", data.path())
            .expect("open")
            .with_placeholder_test(simulated);
        stored();
        let info = checkpoints.create("one").expect("one");
        assert_eq!(stored(), ["here.docx"], "a placeholder is never opened");
        assert_eq!(info.placeholders, ["cloud.pdf"]);
        assert_eq!(info.placeholder_count, 1);
        assert_eq!(
            checkpoints.list().expect("list")[0].placeholders,
            ["cloud.pdf"]
        );
        // A restore leaves it alone (never deleted as if the turn made it).
        let report = checkpoints.restore(1).expect("restore");
        assert!(report.deleted.is_empty());
        assert!(dir.path().join("cloud.pdf").exists());
    }

    #[test]
    fn files_over_the_size_limit_are_reported_not_dropped_silently() {
        let (dir, workspace, data) = plain_folder();
        std::fs::write(dir.path().join("small.txt"), "s").expect("write");
        std::fs::write(dir.path().join("video.mov"), vec![0u8; 2048]).expect("write");
        let checkpoints = Checkpoints::open(&workspace, "s", data.path())
            .expect("open")
            .with_limits(small_limits());
        let info = checkpoints.create("one").expect("one");
        assert_eq!(info.too_large, ["video.mov"]);
        assert_eq!(info.too_large_count, 1);
    }

    #[test]
    fn a_folder_over_the_copy_limits_is_refused_before_anything_is_read() {
        let (dir, workspace, data) = plain_folder();
        for index in 0..4 {
            std::fs::write(dir.path().join(format!("{index}.txt")), "0123456789").expect("write");
        }
        let checkpoints = Checkpoints::open(&workspace, "s", data.path())
            .expect("open")
            .with_limits(CheckpointLimits {
                max_copy_files: 3,
                ..CheckpointLimits::default()
            });
        stored();
        let error = checkpoints.check_fits().expect_err("too many files");
        assert_eq!(error.code(), ErrorCode::TooLarge);
        assert_eq!(
            checkpoints.create("one").expect_err("too many").code(),
            ErrorCode::TooLarge
        );
        let by_bytes = Checkpoints::open(&workspace, "s", data.path())
            .expect("open")
            .with_limits(CheckpointLimits {
                max_copy_bytes: 30,
                ..CheckpointLimits::default()
            });
        let error = by_bytes.check_fits().expect_err("too many bytes");
        assert!(error.message().contains("40 bytes"), "{}", error.message());
        assert_eq!(
            by_bytes.create("one").expect_err("too large").code(),
            ErrorCode::TooLarge
        );
        assert_eq!(stored(), Vec::<String>::new(), "nothing was read");
        assert!(
            Checkpoints::open(&workspace, "s", data.path())
                .expect("open")
                .check_fits()
                .is_ok()
        );
    }

    #[test]
    fn a_checkpoint_from_before_the_rules_were_recorded_still_keeps_ignored_files() {
        let (dir, workspace, data) = plain_folder();
        std::fs::write(dir.path().join(".gitignore"), "cache/\n").expect("ignore");
        std::fs::write(dir.path().join("a.txt"), "a").expect("a");
        let checkpoints = Checkpoints::open(&workspace, "s", data.path()).expect("open");
        checkpoints.create("one").expect("one");
        // The manifest as an older host wrote it: no rules recorded.
        let manifest = walk_paths(data.path())
            .into_iter()
            .find(|path| {
                path.extension().is_some_and(|ext| ext == "json")
                    && path.to_string_lossy().contains("sessions")
            })
            .expect("manifest");
        let mut json: serde_json::Value =
            serde_json::from_slice(&std::fs::read(&manifest).expect("read")).expect("json");
        json.as_object_mut().expect("object").remove("rules");
        std::fs::write(&manifest, serde_json::to_vec(&json).expect("encode")).expect("write");
        std::fs::create_dir(dir.path().join("cache")).expect("cache");
        std::fs::write(dir.path().join("cache/old"), "kept").expect("old");
        std::fs::write(dir.path().join("made.txt"), "new").expect("new");
        let report = checkpoints.restore(1).expect("restore");
        assert_eq!(report.deleted, ["made.txt"]);
        assert!(dir.path().join("cache/old").exists());
    }

    #[test]
    fn file_at_reads_a_file_as_the_checkpoint_holds_it() {
        let (dir, workspace, data) = plain_folder();
        std::fs::write(dir.path().join("a.txt"), "then").expect("a");
        let copy = Checkpoints::open(&workspace, "s", data.path()).expect("open");
        copy.create("one").expect("one");
        std::fs::write(dir.path().join("a.txt"), "now").expect("a");
        let a = workspace.resolve("a.txt", Intent::Read).expect("a");
        let b = workspace.resolve("b.txt", Intent::Read).expect("b");
        assert_eq!(
            copy.file_at(1, &a, 100).expect("a").as_deref(),
            Some(&b"then"[..])
        );
        assert_eq!(copy.file_at(1, &b, 100).expect("b"), None);
        assert_eq!(
            copy.file_at(1, &a, 2).expect_err("large").code(),
            ErrorCode::TooLarge
        );

        let repo = repo();
        std::fs::write(repo.path().join("g.txt"), "then").expect("g");
        let workspace = Workspace::open(repo.path(), &[]).expect("workspace");
        let tracked = Checkpoints::open(&workspace, "s", data.path()).expect("open");
        assert_eq!(tracked.kind(), "git");
        tracked.create("one").expect("one");
        std::fs::write(repo.path().join("g.txt"), "now").expect("g");
        let g = workspace.resolve("g.txt", Intent::Read).expect("g");
        let none = workspace.resolve("none.txt", Intent::Read).expect("none");
        assert_eq!(
            tracked.file_at(1, &g, 100).expect("g").as_deref(),
            Some(&b"then"[..])
        );
        assert_eq!(tracked.file_at(1, &none, 100).expect("none"), None);
    }

    fn walk_paths(root: &Path) -> Vec<std::path::PathBuf> {
        let mut out = Vec::new();
        let mut stack = vec![root.to_path_buf()];
        while let Some(dir) = stack.pop() {
            for entry in std::fs::read_dir(&dir).into_iter().flatten().flatten() {
                let path = entry.path();
                if path.is_dir() {
                    stack.push(path);
                } else {
                    out.push(path);
                }
            }
        }
        out
    }
}
