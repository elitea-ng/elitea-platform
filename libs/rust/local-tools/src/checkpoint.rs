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
//! keeps the objects alive across `gc` and restarts. Restore diffs the
//! checkpoint's tree against a fresh snapshot of now: files that differ or
//! disappeared are written back with `checkout-index` (from another
//! temporary index), files created since are deleted.
//!
//! **Elsewhere** a checkpoint is a manifest (path, SHA-256, mode) in the
//! host's data directory plus content-addressed copies of the files
//! (`objects/<sha256>`, shared between checkpoints), walking the folder with
//! `.gitignore` rules honoured. Bounded: [`MAX_COPY_FILES`],
//! [`MAX_COPY_BYTES`]; files over [`MAX_COPY_FILE_BYTES`] are recorded as
//! skipped and left alone by a restore.
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
use std::time::{SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::git::Repo;
use crate::sandbox::SandboxConfig;
use crate::workspace::{Workspace, WsPath, is_protected_name};

/// The ref namespace checkpoints live under.
pub const REF_PREFIX: &str = "refs/elitea/checkpoints";

/// The most files a copy checkpoint takes.
pub const MAX_COPY_FILES: usize = 50_000;
/// The most bytes a copy checkpoint takes.
pub const MAX_COPY_BYTES: u64 = 1024 * 1024 * 1024;
/// Files larger than this are skipped by copy checkpoints.
pub const MAX_COPY_FILE_BYTES: u64 = 64 * 1024 * 1024;

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
        }))
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

    /// The tree of the workspace as it is now (outside the workspace: as
    /// in HEAD).
    fn snapshot_tree(&self) -> ToolResult<String> {
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
        let workspace_dir = self.workspace_dir();
        self.repo
            .git(&workspace_dir)
            .env("GIT_INDEX_FILE", index.os())
            .writes(&with_objects)
            .worktree()
            .run(&["add", "--all", "--ignore-submodules=all", "--", "."])
            .or_else(|_| {
                // Older gits lack the flag on add; submodules are refused
                // before this point anyway.
                self.repo
                    .git(&workspace_dir)
                    .env("GIT_INDEX_FILE", index.os())
                    .writes(&with_objects)
                    .worktree()
                    .run(&["add", "--all", "--", "."])
            })?;
        self.repo
            .git(self.top())
            .env("GIT_INDEX_FILE", index.os())
            .writes(&with_objects)
            .text(&["write-tree"])
    }

    fn ref_name(&self, seq: u64) -> String {
        format!("{REF_PREFIX}/{}/{seq}", self.session)
    }

    fn create(&self, label: &str) -> ToolResult<CheckpointInfo> {
        let tree = self.snapshot_tree()?;
        let seq = self.list()?.last().map_or(1, |last| last.seq + 1);
        let label = single_line(label);
        let message = format!("elitea checkpoint {seq}: {label}");
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
        self.repo.git(self.top()).writes(&[self.refs()]).run(&[
            "update-ref",
            &self.ref_name(seq),
            &commit,
        ])?;
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
        let current = self.snapshot_tree()?;
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
            if status == "A" {
                delete.push(path);
            } else {
                write_back.push(path);
            }
        }
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
            let read = match self.restorer.read(&path, MAX_COPY_FILE_BYTES) {
                Ok(read) => read,
                Err(error) if error.code() == ErrorCode::TooLarge => {
                    manifest.skipped.insert(path.display_string());
                    continue;
                }
                // Vanished or unreadable since the walk: not part of it.
                Err(_) => continue,
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
        Ok(CheckpointInfo {
            seq: manifest.seq,
            label: manifest.label,
            created_unix: manifest.created_unix,
            id: format!("{:08}", manifest.seq),
        })
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

    fn restore(&self, seq: u64, only: Option<&WsPath>) -> ToolResult<RestoreReport> {
        let manifest = self.read_manifest(seq)?;
        let in_scope = |path: &str| only.is_none_or(|only| only.display_string() == path);
        let mut report = RestoreReport::default();
        for path in walk_files(self.restorer.root()) {
            let name = path.display_string();
            if in_scope(&name)
                && !manifest.files.contains_key(&name)
                && !manifest.skipped.contains(&name)
                && self.restorer.remove_file(&path)?
            {
                report.deleted.push(name);
            }
        }
        for (name, file) in &manifest.files {
            if !in_scope(name) {
                continue;
            }
            let path = WsPath::from_relative(Path::new(name))?;
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

    use super::{Checkpoints, REF_PREFIX};
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
}
