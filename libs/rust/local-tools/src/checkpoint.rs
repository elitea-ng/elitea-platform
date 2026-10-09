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
//! recreated.
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
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use ignore::gitignore::{Gitignore, GitignoreBuilder};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::error::{ErrorCode, ToolError, ToolResult};
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
            keep: 50,
            max_age: Duration::from_hours(30 * 24),
        }
    }
}

/// The commit-message trailer listing the paths a git checkpoint skipped.
const SKIPPED_TRAILER: &str = "Elitea-Skipped: ";

static TEMP_COUNTER: AtomicU64 = AtomicU64::new(0);

/// One checkpoint.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct CheckpointInfo {
    pub seq: u64,
    pub label: String,
    pub created_unix: u64,
    /// The commit id (git) or the manifest name (copy).
    pub id: String,
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

/// The checkpoints of one session in one workspace.
#[derive(Debug)]
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
        let refused = match Repo::discover(workspace.root(), sandbox).and_then(|repo| match repo {
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
        Ok(Self::Copy(CopyCheckpoints {
            objects: base.join("objects"),
            manifests: base.join("sessions").join(session),
            restorer,
            refused,
            limits: CheckpointLimits::default(),
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

    /// Use `limits` instead of the defaults.
    #[must_use]
    pub fn with_limits(mut self, limits: CheckpointLimits) -> Self {
        match &mut self {
            Self::Git(git) => git.limits = limits,
            Self::Copy(copy) => copy.limits = limits,
        }
        self
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
            Self::Git(git) => git.restore(seq, None),
            Self::Copy(copy) => copy.restore(seq, None),
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
            Self::Git(git) => git.restore(seq, Some(path)),
            Self::Copy(copy) => copy.restore(seq, Some(path)),
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
        let dir = std::fs::canonicalize(&dir)
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

/// What a git checkpoint leaves out.
#[derive(Default)]
struct Plan {
    /// Workspace-relative paths not added: too large, or not regular files
    /// (FIFOs, sockets, devices).
    excluded: Vec<String>,
    /// Top-relative paths a restore must leave alone.
    skipped: Vec<String>,
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
        self.repo.git_dir().join("objects")
    }

    fn refs(&self) -> PathBuf {
        self.repo.git_dir().join("refs/elitea")
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

    /// The tree of the workspace as it is now (outside the workspace: as
    /// in HEAD), and the paths it skipped.
    fn snapshot_tree(&self) -> ToolResult<(String, Vec<String>)> {
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
        Ok((tree, plan.skipped))
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
        let writes = [
            self.refs(),
            self.repo.git_dir().join("packed-refs"),
            self.repo.git_dir().join("packed-refs.lock"),
        ];
        for (index, info) in all.iter().enumerate() {
            let newest = index + 1 == all.len();
            if newest || (index >= excess && info.created_unix >= cutoff) {
                continue;
            }
            let deleted = self.repo.git(self.top()).writes(&writes).run(&[
                "update-ref",
                "-d",
                &self.ref_name(info.seq),
                &info.id,
            ]);
            if let Err(error) = deleted {
                tracing::warn!(reason = error.message(), "a checkpoint could not be pruned");
            }
        }
    }

    fn ref_name(&self, seq: u64) -> String {
        format!("{REF_PREFIX}/{}/{seq}", self.session)
    }

    fn create(&self, label: &str) -> ToolResult<CheckpointInfo> {
        let (tree, skipped) = self.snapshot_tree()?;
        let seq = self.list()?.last().map_or(1, |last| last.seq + 1);
        let label = single_line(label);
        let mut message = format!("elitea checkpoint {seq}: {label}");
        if !skipped.is_empty() {
            let list = serde_json::to_string(&skipped)
                .map_err(|_| ToolError::new(ErrorCode::Io, "cannot encode the checkpoint"))?;
            message = format!("{message}\n\n{SKIPPED_TRAILER}{list}");
        }
        let mut args = vec![
            "commit-tree",
            "--no-gpg-sign",
            tree.as_str(),
            "-m",
            message.as_str(),
        ];
        let head = self.head();
        if let Some(head) = &head {
            args.extend(["-p", head.as_str()]);
        }
        let identity = OsStr::new("Elitea checkpoints");
        let email = OsStr::new("checkpoints@elitea.invalid");
        let commit = self
            .repo
            .git(self.top())
            .env("GIT_AUTHOR_NAME", identity)
            .env("GIT_AUTHOR_EMAIL", email)
            .env("GIT_COMMITTER_NAME", identity)
            .env("GIT_COMMITTER_EMAIL", email)
            .writes(&[self.objects()])
            .text(&args)?;
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
        })
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

    fn restore(&self, seq: u64, only: Option<&WsPath>) -> ToolResult<RestoreReport> {
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
        let mut report = RestoreReport::default();
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
    skipped: BTreeSet<String>,
}

/// Checkpoints as copies in the host's data directory.
#[derive(Debug)]
pub struct CopyCheckpoints {
    objects: PathBuf,
    manifests: PathBuf,
    restorer: Workspace,
    /// Why the folder's git repository was not used.
    refused: Option<ToolError>,
    limits: CheckpointLimits,
}

/// The regular files of the workspace a copy checkpoint covers: not
/// ignored, not under `.git`, symlinks not followed.
fn walk_files(root: &Path) -> Vec<WsPath> {
    let mut out = Vec::new();
    let walker = ignore::WalkBuilder::new(root)
        .hidden(false)
        .require_git(false)
        .follow_links(false)
        .filter_entry(|entry| !entry.file_name().to_str().is_some_and(is_protected_name))
        .build();
    for entry in walker.flatten() {
        if !entry.file_type().is_some_and(|kind| kind.is_file()) {
            continue;
        }
        if let Ok(relative) = entry.path().strip_prefix(root)
            && let Ok(path) = WsPath::from_relative(relative)
        {
            out.push(path);
        }
    }
    out.sort();
    out
}

impl CopyCheckpoints {
    fn manifest_path(&self, seq: u64) -> PathBuf {
        self.manifests.join(format!("{seq:08}.json"))
    }

    fn read_manifest(&self, seq: u64) -> ToolResult<Manifest> {
        let bytes = std::fs::read(self.manifest_path(seq))
            .map_err(|_| ToolError::new(ErrorCode::NotFound, format!("no checkpoint {seq}")))?;
        serde_json::from_slice(&bytes)
            .map_err(|_| ToolError::new(ErrorCode::Io, format!("checkpoint {seq} is unreadable")))
    }

    fn create(&self, label: &str) -> ToolResult<CheckpointInfo> {
        let io = |error: std::io::Error| ToolError::io("cannot store the checkpoint", &error);
        std::fs::create_dir_all(&self.objects).map_err(io)?;
        std::fs::create_dir_all(&self.manifests).map_err(io)?;
        let files = walk_files(self.restorer.root());
        if files.len() > MAX_COPY_FILES {
            return Err(ToolError::new(
                ErrorCode::TooLarge,
                format!("the folder has more than {MAX_COPY_FILES} files to checkpoint"),
            ));
        }
        let mut manifest = Manifest {
            seq: self.list()?.last().map_or(1, |last| last.seq + 1),
            label: single_line(label),
            created_unix: now_unix(),
            files: BTreeMap::new(),
            skipped: BTreeSet::new(),
        };
        let mut total = 0u64;
        for path in files {
            // Too large, unreadable, or gone since the walk: not stored,
            // and left alone by a restore (never deleted as if the turn had
            // created it).
            let Ok(read) = self
                .restorer
                .read(&path, self.limits.max_file_bytes.min(MAX_COPY_FILE_BYTES))
            else {
                manifest.skipped.insert(path.display_string());
                continue;
            };
            total += read.stamp.len;
            if total > MAX_COPY_BYTES {
                return Err(ToolError::new(
                    ErrorCode::TooLarge,
                    format!("the folder holds more than {MAX_COPY_BYTES} bytes to checkpoint"),
                ));
            }
            let sha = hex(&read.stamp.sha256);
            let object = self.objects.join(&sha);
            if !object.exists() {
                let temp = self
                    .objects
                    .join(format!("{sha}.tmp{}", std::process::id()));
                std::fs::write(&temp, &read.bytes)
                    .and_then(|()| std::fs::rename(&temp, &object))
                    .map_err(io)?;
            }
            manifest.files.insert(
                path.display_string(),
                ManifestFile {
                    sha256: sha,
                    mode: read.mode,
                },
            );
        }
        let bytes = serde_json::to_vec(&manifest)
            .map_err(|_| ToolError::new(ErrorCode::Io, "cannot encode the checkpoint"))?;
        let target = self.manifest_path(manifest.seq);
        let temp = target.with_extension("json.tmp");
        std::fs::write(&temp, bytes)
            .and_then(|()| std::fs::rename(&temp, &target))
            .map_err(io)?;
        self.prune();
        Ok(CheckpointInfo {
            seq: manifest.seq,
            label: manifest.label,
            created_unix: manifest.created_unix,
            id: format!("{:08}", manifest.seq),
        })
    }

    /// As [`GitCheckpoints::prune`], then remove stored contents no
    /// manifest of any session of this workspace refers to.
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
            if name.len() == 64 && !referenced.contains(&name) {
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
            out.push(CheckpointInfo {
                seq,
                label: manifest.label,
                created_unix: manifest.created_unix,
                id: format!("{seq:08}"),
            });
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

    fn restore(&self, seq: u64, only: Option<&WsPath>) -> ToolResult<RestoreReport> {
        let manifest = self.read_manifest(seq)?;
        let in_scope = |path: &str| only.is_none_or(|only| only.display_string() == path);
        // Not in the checkpoint, but ignored by its rules: it was not
        // copied, so it may be older than the checkpoint. Kept.
        let mut ignores = CheckpointIgnores::new(&[".gitignore", ".ignore"], |file| {
            let stored = manifest.files.get(file)?;
            std::fs::read(self.objects.join(&stored.sha256)).ok()
        });
        let mut report = RestoreReport::default();
        for path in walk_files(self.restorer.root()) {
            let name = path.display_string();
            if in_scope(&name)
                && !manifest.files.contains_key(&name)
                && !manifest.skipped.contains(&name)
                && !ignores.ignores(&name)
                && self.restorer.remove_file(&path)?
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
            self.unlink_links_on(&path, &mut report)?;
            let unchanged = self
                .restorer
                .read(&path, MAX_COPY_FILE_BYTES)
                .ok()
                .is_some_and(|read| {
                    read.mode == file.mode && hex(&read.stamp.sha256) == file.sha256
                });
            if unchanged {
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

#[cfg(test)]
mod tests {
    use std::path::Path;
    use std::process::Command;

    use super::{CheckpointLimits, Checkpoints, REF_PREFIX};
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
        assert!(other.list().expect("list").is_empty());
        assert!(other.restore(1).is_err());
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
        assert_eq!(
            read(&root, "cache/blob").as_deref(),
            Some("ignored changed")
        );
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
        assert!(checkpoints.list().expect("list").is_empty());
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
        std::os::unix::fs::symlink("lib", root.join("src")).expect("link dir");
        std::fs::remove_file(root.join("b.txt")).expect("rm b");
        std::os::unix::fs::symlink("lib/b.txt", root.join("b.txt")).expect("link file");
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
}
