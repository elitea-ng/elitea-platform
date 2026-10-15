//! Take-back, Phase 1 (ADR-0034 decision 13): a preflight with no side
//! effects, then the fast path, a worktree, or discard.
//!
//! **Preflight** ([`preflight`]):
//!
//! 1. the fetched segment is verified into a quarantine of its own
//!    (content address, pack, connectivity from every ref; the refs must be
//!    the plan's), before anything else is read from it;
//! 2. the cloud chain `H_1..C` is read (first parents from `C` back to the
//!    `H_1` this desktop sent; anything else is refused);
//! 3. the folder is snapshotted as `L` with the send's exclusions, in a
//!    temporary index (objects only: the folder, `HEAD`, the index and every
//!    ref stay byte-identical);
//! 4. the outcome: `fast_path` when `HEAD` is the `HEAD` of the send and
//!    `tree(L) == tree(H'_1)`; otherwise `conflicts` (Phase 1 offers a
//!    worktree or discard), with the paths both sides changed.
//!
//! **Apply** ([`apply`]) first takes `L` again and refuses with
//! `folder_changed_since_preflight` ([`ErrorCode::StaleRead`]) if it moved.
//! Then, by mode:
//!
//! * `apply` (fast path only): a checkpoint of the folder (so restoring it
//!   undoes the take-back), the segment's objects imported into the
//!   repository (`index-pack`), the cloud commits replayed onto `H'_1` and
//!   kept on `elitea/cloud/<thread>`, and the changed paths written through
//!   the confined workspace writer. `HEAD` and the index are untouched: the
//!   cloud's work lands as uncommitted changes.
//! * `worktree`: the same branch, registered as a worktree beside the folder
//!   (`worktree add --no-checkout`, its index read from the branch, its
//!   files written by the confined writer: no checkout filter runs); the
//!   folder is untouched. A folder without a repository gets a plain copy.
//! * `discard`: nothing is written.
//!
//! Excluded paths are never written or deleted, nor are symlinks, submodule
//! entries or anything under a `.git` (reported as skipped).

use std::collections::{BTreeMap, BTreeSet};
use std::ffi::OsStr;
use std::path::{Path, PathBuf};

use elitea_workspace_store::bundle::{self as segment, Expect, Limits, VerifiedBundle};
use elitea_workspace_store::objects::{
    self as store_objects, Change, ChangeKind, FileEntry, ObjectDb,
};
use elitea_workspace_store::refs as store_refs;
use serde::{Deserialize, Serialize};

use super::exclude::Exclusions;
use super::snapshot::{Source, snapshot_tree};
use super::{SendRecord, TempDir, check_conversation, short_thread};
use crate::checkpoint::Checkpoints;
use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::workspace::{Workspace, WsPath, is_protected_name};

/// The most cloud commits one take-back replays.
pub const MAX_CLOUD_TURNS: usize = 1000;

/// The machine code of a folder that changed after its preflight.
pub const FOLDER_CHANGED: &str = "folder_changed_since_preflight";

/// What a preflight found.
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Outcome {
    FastPath,
    Conflicts,
    Error,
}

/// How a take-back is applied.
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Mode {
    Apply,
    Worktree,
    Discard,
}

/// One cloud commit, oldest first.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct CloudCommit {
    pub sha: String,
    pub parent: String,
    pub message: String,
}

/// One file a cloud commit changed, with its contents before and after
/// (for the history's changed-files records).
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct FileDiff {
    pub path: String,
    pub kind: ChangeKind,
    pub before: Option<Vec<u8>>,
    pub after: Option<Vec<u8>>,
}

/// A preflight, held until the person picks a mode.
pub struct Preflight {
    pub outcome: Outcome,
    /// Paths the cloud changed that take-back writes.
    pub changed_paths: Vec<String>,
    /// Paths both the cloud and the folder changed since the send.
    pub conflict_paths: Vec<String>,
    /// Paths the cloud changed that take-back leaves alone.
    pub skipped_paths: Vec<String>,
    pub chain: Vec<CloudCommit>,
    /// `C`, the cloud tip (`H_1` when no cloud turn changed anything).
    pub cloud_tip: String,
    /// `L` and `HEAD` as the preflight saw them.
    local_tree: String,
    head: Option<String>,
    changes: Vec<Change>,
    bundle: VerifiedBundle,
    pack: Vec<u8>,
    record: SendRecord,
    _quarantine: TempDir,
}

impl std::fmt::Debug for Preflight {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Preflight")
            .field("outcome", &self.outcome)
            .field("changed_paths", &self.changed_paths)
            .field("conflict_paths", &self.conflict_paths)
            .field("skipped_paths", &self.skipped_paths)
            .field("cloud_tip", &self.cloud_tip)
            .finish_non_exhaustive()
    }
}

impl Preflight {
    /// Cloud turns (commits after `H_1`).
    #[must_use]
    pub fn cloud_turns(&self) -> usize {
        self.chain.len()
    }

    /// What cloud commit `sha` changed, read from the quarantine.
    ///
    /// # Errors
    ///
    /// An unknown commit or an unreadable object.
    pub fn commit_changes(&self, sha: &str) -> ToolResult<Vec<FileDiff>> {
        let commit = self
            .chain
            .iter()
            .find(|commit| commit.sha == sha)
            .ok_or_else(|| {
                ToolError::new(ErrorCode::NotFound, "not a cloud commit of this handoff")
            })?;
        let db = &self.bundle.db;
        let before = flatten_commit(db, &commit.parent)?;
        let after = flatten_commit(db, &commit.sha)?;
        let read = |entry: Option<&FileEntry>| -> ToolResult<Option<Vec<u8>>> {
            entry
                .filter(|entry| !entry.is_gitlink())
                .map(|entry| db.blob(&entry.oid).map_err(|e| super::store_error(&e)))
                .transpose()
        };
        store_objects::diff(&before, &after)
            .into_iter()
            .map(|change| {
                Ok(FileDiff {
                    before: read(change.old.as_ref())?,
                    after: read(change.new.as_ref())?,
                    path: change.path,
                    kind: change.kind,
                })
            })
            .collect()
    }
}

/// What [`apply`] did.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize)]
pub struct Applied {
    /// Files were written to the folder (or the worktree).
    pub applied: bool,
    /// The checkpoint taken before the folder was written (`apply` only).
    pub checkpoint: Option<u64>,
    pub worktree_path: Option<PathBuf>,
    /// `elitea/cloud/<thread>`, the replayed cloud commits.
    pub branch: Option<String>,
    /// The replayed commits, oldest first (`H'_1` descendants).
    pub replayed: Vec<String>,
    pub written: Vec<String>,
    pub deleted: Vec<String>,
}

fn flatten_commit(db: &ObjectDb, sha: &str) -> ToolResult<BTreeMap<String, FileEntry>> {
    let id = store_objects::oid(sha).map_err(|e| super::store_error(&e))?;
    let commit = db.commit(&id).map_err(|e| super::store_error(&e))?;
    db.flatten(&commit.tree).map_err(|e| super::store_error(&e))
}

fn flatten_tree(db: &ObjectDb, tree: &str) -> ToolResult<BTreeMap<String, FileEntry>> {
    let id = store_objects::oid(tree).map_err(|e| super::store_error(&e))?;
    db.flatten(&id).map_err(|e| super::store_error(&e))
}

/// The send's exclusions again: the policy's and the folder's rules, and
/// every path the send left out.
fn exclusions_of(
    source: &Source,
    record: &SendRecord,
    path_deny: &[String],
) -> ToolResult<Exclusions> {
    let workspace = Workspace::open(source.top(), &[])?;
    let leave: Vec<String> = record.excluded.iter().map(|e| e.path.clone()).collect();
    Exclusions::new(&workspace, path_deny, &leave)
}

/// `L`: the folder's tree under the send's exclusions.
fn local_tree(source: &Source, exclusions: &Exclusions) -> ToolResult<String> {
    // No cap here: the folder is only compared, nothing is sent.
    snapshot_tree(source, exclusions, u64::MAX).map(|(tree, _, _)| tree)
}

/// Whether take-back may write `path` (`kind` the new entry, if any).
fn writable(path: &str, entry: Option<&FileEntry>, exclusions: &Exclusions) -> bool {
    exclusions.reason(path).is_none()
        && !path.split('/').any(is_protected_name)
        && entry.is_none_or(|entry| !entry.is_link() && !entry.is_gitlink())
}

/// The cloud commits from `handoff` (excluded) to `tip`, oldest first.
fn read_chain(
    bundle: &VerifiedBundle,
    tip: &store_objects::ObjectId,
    handoff: &store_objects::ObjectId,
) -> ToolResult<Vec<CloudCommit>> {
    let chain_ids = bundle
        .db
        .first_parent_chain(tip, handoff, MAX_CLOUD_TURNS)
        .map_err(|e| super::store_error(&e))?
        .ok_or_else(|| {
            ToolError::new(
                ErrorCode::Conflict,
                "the cloud tip does not descend from the handoff this desktop sent",
            )
        })?;
    let mut chain = Vec::new();
    for id in chain_ids.iter().rev() {
        let commit = bundle.db.commit(id).map_err(|e| super::store_error(&e))?;
        chain.push(CloudCommit {
            sha: id.to_string(),
            parent: commit
                .parents
                .first()
                .map(ToString::to_string)
                .unwrap_or_default(),
            message: commit.message,
        });
    }
    Ok(chain)
}

/// Verify a fetched segment, read the cloud chain, snapshot the folder,
/// and decide. Writes nothing to the folder, `HEAD`, the index or any ref.
///
/// # Errors
///
/// A segment that fails verification, a cloud tip that does not descend
/// from the `H_1` this desktop sent, git failures.
pub fn preflight(
    source: &Source,
    record: &SendRecord,
    bytes: &[u8],
    plan_refs: &BTreeMap<String, String>,
    path_deny: &[String],
) -> ToolResult<Preflight> {
    check_conversation(&record.conversation_uuid)?;
    let quarantine = TempDir::new("quarantine")?;
    let git_dir = quarantine.path().join("git");
    segment::init_quarantine(&git_dir).map_err(|e| super::store_error(&e))?;
    let bundle = segment::verify_into(
        bytes,
        &git_dir,
        &Expect {
            refs: Some(plan_refs),
            ..Expect::default()
        },
        Limits::default(),
    )
    .map_err(|e| super::store_error(&e))?;
    let sent = bundle
        .ref_id(&store_refs::handoff(1))
        .map(|id| id.to_string());
    if sent.as_deref() != Some(record.handoff.as_str()) {
        return Err(ToolError::new(
            ErrorCode::Conflict,
            "the cloud copy is not of the handoff this desktop sent",
        ));
    }
    let handoff = store_objects::oid(&record.handoff).map_err(|e| super::store_error(&e))?;
    // No cloud turn published: the tip is H_1 itself.
    let tip = bundle.ref_id(store_refs::CLOUD).unwrap_or(handoff);
    let chain = read_chain(&bundle, &tip, &handoff)?;
    let sent_files = flatten_commit(&bundle.db, &record.handoff)?;
    let sent_tree = bundle
        .db
        .commit(&handoff)
        .map_err(|e| super::store_error(&e))?
        .tree
        .to_string();
    if sent_tree != record.tree {
        return Err(ToolError::new(
            ErrorCode::Conflict,
            "the handoff in the cloud copy has another tree than was sent",
        ));
    }
    let cloud_files = flatten_commit(&bundle.db, &tip.to_string())?;
    let changes = store_objects::diff(&sent_files, &cloud_files);
    let exclusions = exclusions_of(source, record, path_deny)?;
    let local = local_tree(source, &exclusions)?;
    let head = source.head();
    let fast = head == record.head && local == record.tree;
    let mut changed_paths = Vec::new();
    let mut skipped_paths = Vec::new();
    for change in &changes {
        if writable(&change.path, change.new.as_ref(), &exclusions) {
            changed_paths.push(change.path.clone());
        } else {
            skipped_paths.push(change.path.clone());
        }
    }
    let conflict_paths = if fast {
        Vec::new()
    } else {
        let db = source.object_db()?;
        let local_files = flatten_tree(&db, &local)?;
        let twin_files = flatten_tree(&db, &record.tree)?;
        let local_changed: BTreeSet<String> = store_objects::diff(&twin_files, &local_files)
            .into_iter()
            .map(|change| change.path)
            .collect();
        changes
            .iter()
            .filter(|change| local_changed.contains(&change.path))
            .map(|change| change.path.clone())
            .collect()
    };
    let pack = bytes[bundle.pack_offset..].to_vec();
    Ok(Preflight {
        outcome: if fast {
            Outcome::FastPath
        } else {
            Outcome::Conflicts
        },
        changed_paths,
        conflict_paths,
        skipped_paths,
        chain,
        cloud_tip: tip.to_string(),
        local_tree: local,
        head,
        changes,
        bundle,
        pack,
        record: record.clone(),
        _quarantine: quarantine,
    })
}

/// Import the segment's objects and replay the cloud chain onto `H'_1`;
/// the branch name and the replayed commits.
fn import_and_replay(source: &Source, preflight: &Preflight) -> ToolResult<(String, Vec<String>)> {
    let objects = source.git_dir().join("objects");
    source
        .repo
        .git(source.top())
        .writes(std::slice::from_ref(&objects))
        .stdin(&preflight.pack)
        .run(&["index-pack", "--stdin"])?;
    let name = OsStr::new("Elitea cloud executor");
    let email = OsStr::new("cloud@elitea.invalid");
    let mut previous = preflight.record.twin.clone();
    let mut replayed = Vec::new();
    for commit in &preflight.chain {
        let tree = source
            .repo
            .git(source.top())
            .text(&["rev-parse", &format!("{}^{{tree}}", commit.sha)])?;
        let message = if commit.message.trim().is_empty() {
            "Elitea cloud turn".to_owned()
        } else {
            commit.message.clone()
        };
        previous = source
            .repo
            .git(source.top())
            .env("GIT_AUTHOR_NAME", name)
            .env("GIT_AUTHOR_EMAIL", email)
            .env("GIT_COMMITTER_NAME", name)
            .env("GIT_COMMITTER_EMAIL", email)
            .writes(std::slice::from_ref(&objects))
            .text(&[
                "commit-tree",
                "--no-gpg-sign",
                &tree,
                "-p",
                &previous,
                "-m",
                &message,
            ])?;
        replayed.push(previous.clone());
    }
    let short = short_thread(&preflight.record.conversation_uuid);
    let heads = source.git_dir().join("refs/heads/elitea/cloud");
    std::fs::create_dir_all(&heads)
        .map_err(|error| ToolError::io("cannot create the cloud branch", &error))?;
    let mut branch = format!("elitea/cloud/{short}");
    let mut n = 2;
    while source
        .repo
        .git(source.top())
        .text(&[
            "rev-parse",
            "--verify",
            "--quiet",
            &format!("refs/heads/{branch}"),
        ])
        .is_ok_and(|sha| !sha.is_empty() && sha != previous)
    {
        branch = format!("elitea/cloud/{short}-{n}");
        n += 1;
    }
    source
        .repo
        .git(source.top())
        .writes(std::slice::from_ref(&heads))
        .run(&["update-ref", &format!("refs/heads/{branch}"), &previous])?;
    Ok((branch, replayed))
}

/// Write `files` (a cloud tree's entries) into `workspace`.
fn write_files<'a>(
    workspace: &Workspace,
    db: &ObjectDb,
    files: impl Iterator<Item = (&'a String, &'a FileEntry)>,
    exclusions: &Exclusions,
    applied: &mut Applied,
) -> ToolResult<()> {
    for (path, entry) in files {
        if !writable(path, Some(entry), exclusions) {
            continue;
        }
        let bytes = db.blob(&entry.oid).map_err(|e| super::store_error(&e))?;
        let ws_path = WsPath::from_relative(Path::new(path))?;
        let mode = if entry.is_executable() { 0o755 } else { 0o644 };
        workspace.write(&ws_path, &bytes, Some(mode))?;
        applied.written.push(path.clone());
    }
    Ok(())
}

/// A path beside the folder that does not exist yet.
fn worktree_path(top: &Path, short: &str) -> ToolResult<PathBuf> {
    let parent = top
        .parent()
        .ok_or_else(|| ToolError::invalid("the folder has no parent for a worktree"))?;
    let name = top
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    for n in 1..100 {
        let suffix = if n == 1 {
            String::new()
        } else {
            format!("-{n}")
        };
        let candidate = parent.join(format!("{name}-elitea-cloud-{short}{suffix}"));
        if std::fs::symlink_metadata(&candidate).is_err() {
            return Ok(candidate);
        }
    }
    Err(ToolError::invalid("no free name for the worktree"))
}

/// Apply a preflight.
///
/// # Errors
///
/// [`ErrorCode::StaleRead`] (`folder_changed_since_preflight`) when the
/// folder or `HEAD` moved since the preflight; [`ErrorCode::InvalidArgument`]
/// for `apply` without the fast path; git or I/O failures.
pub fn apply(
    source: &Source,
    preflight: &Preflight,
    mode: Mode,
    path_deny: &[String],
    checkpoints: Option<&Checkpoints>,
) -> ToolResult<Applied> {
    let exclusions = exclusions_of(source, &preflight.record, path_deny)?;
    if local_tree(source, &exclusions)? != preflight.local_tree || source.head() != preflight.head {
        return Err(ToolError::new(
            ErrorCode::StaleRead,
            format!(
                "{FOLDER_CHANGED}: the folder changed after the preview; preview the take-back again"
            ),
        ));
    }
    match mode {
        Mode::Discard => Ok(Applied::default()),
        Mode::Apply => apply_fast(source, preflight, &exclusions, checkpoints),
        Mode::Worktree => apply_worktree(source, preflight, &exclusions),
    }
}

/// `apply`: the fast path into the folder.
fn apply_fast(
    source: &Source,
    preflight: &Preflight,
    exclusions: &Exclusions,
    checkpoints: Option<&Checkpoints>,
) -> ToolResult<Applied> {
    if preflight.outcome != Outcome::FastPath {
        return Err(ToolError::invalid(
            "the folder changed since it was sent: open the cloud work as a worktree or discard it",
        ));
    }
    let checkpoint = match checkpoints {
        Some(checkpoints) => Some(
            checkpoints
                .create("before taking the thread back from the cloud")?
                .seq,
        ),
        None => None,
    };
    let (branch, replayed) = import_and_replay(source, preflight)?;
    let workspace = Workspace::open(source.top(), &[])?;
    let mut applied = Applied {
        applied: true,
        checkpoint,
        branch: Some(branch),
        replayed,
        ..Applied::default()
    };
    let cloud = flatten_commit(&preflight.bundle.db, &preflight.cloud_tip)?;
    let added_or_modified = preflight.changes.iter().filter_map(|change| {
        let entry = change.new.as_ref()?;
        cloud
            .get_key_value(&change.path)
            .map(|(path, _)| (path, entry))
    });
    write_files(
        &workspace,
        &preflight.bundle.db,
        added_or_modified,
        exclusions,
        &mut applied,
    )?;
    for change in &preflight.changes {
        if change.kind == ChangeKind::Deleted && writable(&change.path, None, exclusions) {
            let path = WsPath::from_relative(Path::new(&change.path))?;
            if workspace.remove_file(&path)? {
                applied.deleted.push(change.path.clone());
            }
        }
    }
    Ok(applied)
}

/// `worktree`: the cloud tip beside the folder.
fn apply_worktree(
    source: &Source,
    preflight: &Preflight,
    exclusions: &Exclusions,
) -> ToolResult<Applied> {
    let short = short_thread(&preflight.record.conversation_uuid).to_owned();
    let path = worktree_path(source.top(), &short)?;
    std::fs::create_dir(&path)
        .map_err(|error| ToolError::io("cannot create the worktree", &error))?;
    let path = std::fs::canonicalize(&path)
        .map_err(|error| ToolError::io("cannot create the worktree", &error))?;
    let (branch, replayed) = import_and_replay(source, preflight)?;
    if !source.shadow {
        let admin = source.git_dir().join("worktrees");
        std::fs::create_dir_all(&admin)
            .map_err(|error| ToolError::io("cannot create the worktree", &error))?;
        source
            .repo
            .git(source.top())
            .writes(&[admin, path.clone()])
            .run(&[
                "worktree",
                "add",
                "--no-checkout",
                &path.to_string_lossy(),
                &branch,
            ])?;
        let pointer = std::fs::read_to_string(path.join(".git"))
            .map_err(|error| ToolError::io("cannot read the new worktree", &error))?;
        let admin_dir = PathBuf::from(pointer.trim().trim_start_matches("gitdir:").trim());
        let index = admin_dir.join("index");
        source
            .repo
            .git(source.top())
            .env("GIT_INDEX_FILE", index.as_os_str())
            .writes(std::slice::from_ref(&admin_dir))
            .run(&["read-tree", &branch])?;
    }
    let workspace = Workspace::open(&path, &[])?;
    let mut applied = Applied {
        applied: true,
        worktree_path: Some(path),
        branch: Some(branch),
        replayed,
        ..Applied::default()
    };
    let cloud = flatten_commit(&preflight.bundle.db, &preflight.cloud_tip)?;
    write_files(
        &workspace,
        &preflight.bundle.db,
        cloud.iter(),
        exclusions,
        &mut applied,
    )?;
    Ok(applied)
}
