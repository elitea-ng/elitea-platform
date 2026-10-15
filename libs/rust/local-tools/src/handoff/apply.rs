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

use super::exclude::{Exclusions, Reason};
use super::snapshot::{Source, TreeLine, list_tree, snapshot_tree};
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
    /// `worktree`: the folder's excluded files copied into it.
    pub copied: Vec<String>,
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

/// How take-back treats one path the cloud changed.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Write {
    /// Written (or deleted) like any change.
    Always,
    /// An excluded path the cloud created: written only where no local
    /// file exists (a local excluded file is never overwritten).
    IfAbsent,
    /// Never written or deleted.
    Never,
}

/// How take-back may write `path` (`entry` the new entry, if any; `added`:
/// the cloud created it).
fn writable(path: &str, entry: Option<&FileEntry>, added: bool, exclusions: &Exclusions) -> Write {
    if path.split('/').any(is_protected_name)
        || entry.is_some_and(|entry| entry.is_link() || entry.is_gitlink())
    {
        return Write::Never;
    }
    match exclusions.reason(path) {
        None => Write::Always,
        Some(Reason::PathDeny | Reason::NotAFile) => Write::Never,
        Some(_) if added && entry.is_some() => Write::IfAbsent,
        Some(_) => Write::Never,
    }
}

/// Something (a file, a link, a directory) is at `path` under `top`.
fn present(top: &Path, path: &str) -> bool {
    std::fs::symlink_metadata(top.join(path)).is_ok()
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
        let added = change.kind == ChangeKind::Added;
        match writable(&change.path, change.new.as_ref(), added, &exclusions) {
            Write::Always => changed_paths.push(change.path.clone()),
            Write::IfAbsent if !present(source.top(), &change.path) => {
                changed_paths.push(change.path.clone());
            }
            Write::IfAbsent | Write::Never => skipped_paths.push(change.path.clone()),
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

/// `tree` with every excluded path as `HEAD` had it: the entry of `HEAD`
/// where it has one, nothing where it has none. The branch's commits then
/// never delete an excluded tracked file (the send left it out of the
/// snapshot) and never carry an excluded file the cloud wrote.
fn graft_excluded(
    source: &Source,
    tree: &str,
    head_excluded: &[TreeLine],
    exclusions: &Exclusions,
) -> ToolResult<String> {
    let lines = list_tree(source, tree)?;
    let remove: Vec<&TreeLine> = lines
        .iter()
        .filter(|line| exclusions.reason(&line.path).is_some())
        .filter(|line| {
            !head_excluded.iter().any(|head| {
                head.path == line.path && head.oid == line.oid && head.mode == line.mode
            })
        })
        .collect();
    let add: Vec<&TreeLine> = head_excluded
        .iter()
        .filter(|head| {
            !lines.iter().any(|line| {
                line.path == head.path && line.oid == head.oid && line.mode == head.mode
            })
        })
        .collect();
    if remove.is_empty() && add.is_empty() {
        return Ok(tree.to_owned());
    }
    let index = TempDir::new("graft")?;
    let index_file = index.path().join("index");
    let index_os: &OsStr = index_file.as_os_str();
    let index_dir = [index.path().to_path_buf()];
    let with_objects = [index.path().to_path_buf(), source.objects()];
    let git = || {
        source
            .repo
            .git(source.top())
            .env("GIT_INDEX_FILE", index_os)
    };
    git().writes(&index_dir).run(&["read-tree", tree])?;
    let mut info = Vec::new();
    for line in &remove {
        // Mode 0 removes the entry.
        info.extend_from_slice(
            format!("0 0000000000000000000000000000000000000000\t{}", line.path).as_bytes(),
        );
        info.push(0);
    }
    for line in &add {
        info.extend_from_slice(format!("{} {}\t{}", line.mode, line.oid, line.path).as_bytes());
        info.push(0);
    }
    git()
        .writes(&index_dir)
        .stdin(&info)
        .run(&["update-index", "-z", "--index-info"])?;
    git().writes(&with_objects).text(&["write-tree"])
}

/// The excluded entries of `HEAD` at the send.
fn head_excluded(
    source: &Source,
    record: &SendRecord,
    exclusions: &Exclusions,
) -> ToolResult<Vec<TreeLine>> {
    let Some(head) = &record.head else {
        return Ok(Vec::new());
    };
    Ok(list_tree(source, &format!("{head}^{{tree}}"))?
        .into_iter()
        .filter(|line| exclusions.reason(&line.path).is_some())
        .collect())
}

/// Import the segment's objects and replay the cloud chain onto `H'_1`
/// (each tree with the excluded paths as `HEAD` had them,
/// [`graft_excluded`]); the branch name and the replayed commits.
fn import_and_replay(
    source: &Source,
    preflight: &Preflight,
    exclusions: &Exclusions,
) -> ToolResult<(String, Vec<String>)> {
    let objects = source.objects();
    source
        .repo
        .git(source.top())
        .writes(std::slice::from_ref(&objects))
        .stdin(&preflight.pack)
        .run(&["index-pack", "--stdin"])?;
    let name = OsStr::new("Elitea cloud executor");
    let email = OsStr::new("cloud@elitea.invalid");
    let head_excluded = head_excluded(source, &preflight.record, exclusions)?;
    let mut previous = preflight.record.twin.clone();
    if !head_excluded.is_empty() {
        // The twin, with HEAD's excluded entries back.
        let tree = graft_excluded(source, &preflight.record.tree, &head_excluded, exclusions)?;
        if tree != preflight.record.tree
            && let Some(head) = &preflight.record.head
        {
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
                    head,
                    "-m",
                    &format!(
                        "Elitea handoff (local twin)\n\nElitea-Handoff-Twin: {}",
                        preflight.record.handoff
                    ),
                ])?;
        }
    }
    let mut replayed = Vec::new();
    for commit in &preflight.chain {
        let tree = source
            .repo
            .git(source.top())
            .text(&["rev-parse", &format!("{}^{{tree}}", commit.sha)])?;
        let tree = graft_excluded(source, &tree, &head_excluded, exclusions)?;
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
    let heads = source.common_dir().join("refs/heads/elitea/cloud");
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

/// Write `files` (a cloud tree's entries, each with whether the cloud
/// created it) into `workspace`.
fn write_files<'a>(
    workspace: &Workspace,
    db: &ObjectDb,
    files: impl Iterator<Item = (&'a String, &'a FileEntry, bool)>,
    exclusions: &Exclusions,
    applied: &mut Applied,
) -> ToolResult<()> {
    for (path, entry, added) in files {
        match writable(path, Some(entry), added, exclusions) {
            Write::Always => {}
            Write::IfAbsent if !present(workspace.root(), path) => {}
            Write::IfAbsent | Write::Never => continue,
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
    let (branch, replayed) = import_and_replay(source, preflight, exclusions)?;
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
            .map(|(path, _)| (path, entry, change.kind == ChangeKind::Added))
    });
    write_files(
        &workspace,
        &preflight.bundle.db,
        added_or_modified,
        exclusions,
        &mut applied,
    )?;
    for change in &preflight.changes {
        if change.kind == ChangeKind::Deleted
            && writable(&change.path, None, false, exclusions) == Write::Always
        {
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
    let (branch, replayed) = import_and_replay(source, preflight, exclusions)?;
    if !source.shadow {
        let admin = source.common_dir().join("worktrees");
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
    // The person's own excluded files first (local → local), so the
    // worktree builds like the folder; a cloud file never overwrites one.
    applied.copied = copy_local_excluded(source, &workspace, exclusions)?;
    let cloud = flatten_commit(&preflight.bundle.db, &preflight.cloud_tip)?;
    let sent = flatten_commit(&preflight.bundle.db, &preflight.record.handoff)?;
    write_files(
        &workspace,
        &preflight.bundle.db,
        cloud
            .iter()
            .map(|(path, entry)| (path, entry, !sent.contains_key(path))),
        exclusions,
        &mut applied,
    )?;
    Ok(applied)
}

/// The most local excluded files copied into a worktree, and their bytes.
const COPY_MAX_FILES: usize = 1000;
const COPY_MAX_BYTES: u64 = 64 * 1024 * 1024;
const COPY_MAX_FILE_BYTES: u64 = 25 * 1024 * 1024;

/// Copy the folder's excluded files (tracked, untracked and ignored ones:
/// `.env`, keys, `.eliteaignore`d files) into `worktree`, through the
/// confined reader and writer: regular files only, never through a
/// symlink, bounded. The paths copied.
fn copy_local_excluded(
    source: &Source,
    worktree: &Workspace,
    exclusions: &Exclusions,
) -> ToolResult<Vec<String>> {
    let git = || source.repo.git(source.top()).worktree();
    let mut listed = git().run(&[
        "ls-files",
        "-z",
        "--cached",
        "--others",
        "--exclude-standard",
    ])?;
    listed.push(0);
    listed.extend(git().run(&[
        "ls-files",
        "-z",
        "--others",
        "--ignored",
        "--exclude-standard",
        "--directory",
    ])?);
    let mut paths: Vec<String> = listed
        .split(|byte| *byte == 0)
        .filter(|path| !path.is_empty() && !path.ends_with(b"/"))
        .map(|path| String::from_utf8_lossy(path).into_owned())
        .filter(|path| {
            !matches!(exclusions.reason(path), None | Some(Reason::NotAFile))
                && !path.split('/').any(is_protected_name)
        })
        .collect();
    paths.sort();
    paths.dedup();
    let folder = Workspace::open(source.top(), &[])?;
    let (mut copied, mut total) = (Vec::new(), 0u64);
    for path in paths {
        if copied.len() >= COPY_MAX_FILES {
            break;
        }
        let Ok(meta) = std::fs::symlink_metadata(source.top().join(&path)) else {
            continue;
        };
        if !meta.file_type().is_file()
            || meta.len() > COPY_MAX_FILE_BYTES
            || total + meta.len() > COPY_MAX_BYTES
        {
            continue;
        }
        let ws_path = WsPath::from_relative(Path::new(&path))?;
        let Ok(file) = folder.read(&ws_path, COPY_MAX_FILE_BYTES) else {
            continue;
        };
        if file.path != ws_path {
            // Reached through a symlink: not copied.
            continue;
        }
        total += file.bytes.len() as u64;
        worktree.write(&ws_path, &file.bytes, Some(file.mode & 0o777))?;
        copied.push(path);
    }
    Ok(copied)
}
