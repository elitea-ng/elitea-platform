//! The one apply engine (#1235 stage 4, #1187 WP6): bring a tree made
//! elsewhere into a folder as uncommitted changes, after a dry-run preview.
//! Two callers share it:
//!
//! * the worktree merge-back ([`preflight`], [`apply`]): a task worktree's
//!   work ([`crate::worktree`]) merged into its parent folder;
//! * the handoff take-back (`handoff::apply`): the cloud's turns merged into
//!   the folder they were sent from.
//!
//! Both pass a `(base, ours, theirs)` tree triple ([`Trees`]): `base` where
//! the other side started, `ours` the folder as it is now (snapshotted as a
//! checkpoint would take it: tracked and untracked files, a temporary index;
//! the folder, its index and `HEAD` stay as they are), `theirs` the work to
//! bring in. Both get the same [`ApplyOutcome`]:
//!
//! * `fast_path`: the folder did not change since `base`; the result is
//!   `theirs`;
//! * `clean_merge`: both changed, on different lines or files, and
//!   `merge-tree --write-tree` merged them (when `theirs` changed nothing,
//!   or equals `ours`, the result is `ours` and nothing is written);
//! * `conflicts`: `merge-tree` reported conflicted paths. Nothing is written
//!   unless the caller asks for conflict markers: then text files get
//!   `merge-tree`'s markers, and a binary conflict is never written (it is
//!   reported as `binary_conflict`, the folder keeps its version).
//!
//! What applying would write or delete is `changed_paths`; what it leaves
//! alone is `skipped_paths`, each with its reason (the caller's rules:
//! `path_deny`, the session's deny list, the handoff's exclusions; and the
//! engine's own: symlinks and submodules on either side and anything under a
//! `.git` are `not_a_file`, files over [`MAX_FILE_BYTES`] are `too_large`).
//!
//! **Apply** re-snapshots the folder and refuses with [`FOLDER_CHANGED`]
//! ([`ErrorCode::StaleRead`]) when it moved since the preview; the caller
//! takes a checkpoint (restoring it undoes the apply); the result's files
//! are written through the confined
//! [`Workspace`] writer. `HEAD`, the index and
//! every branch of the folder stay as they were.
//!
//! merge-tree runs under [`crate::git`]'s four layers, with attributes read
//! from the empty tree (`--attr-source`) and `merge.default=text`: no merge
//! driver, filter or text conversion of anyone's config can run. The three
//! trees are wrapped in commits of their own first (fixed identity and
//! date), so `merge-tree` needs no git that merges bare trees.

use std::collections::{BTreeMap, BTreeSet};
use std::ffi::{OsStr, OsString};
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::git::{Git, Repo};
use crate::session::LocalSession;
use crate::workspace::{EntryKind, Intent, Workspace, WsPath, is_protected_name};

/// The machine code of a folder that changed after its preview.
pub const FOLDER_CHANGED: &str = "folder_changed_since_preflight";

/// The largest file an apply writes; larger ones are skipped (`too_large`).
pub const MAX_FILE_BYTES: u64 = 64 * 1024 * 1024;

/// A conflicted file larger than this is treated as binary (no markers).
const MAX_MARKER_FILE_BYTES: u64 = 8 * 1024 * 1024;

/// git's own binary sniff: a NUL in the first 8000 bytes.
const BINARY_SNIFF_BYTES: usize = 8000;

/// One `cat-file --batch` read holds at most this many bytes of blobs.
const BATCH_BYTES: u64 = 16 * 1024 * 1024;

/// What a preview found.
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Outcome {
    FastPath,
    CleanMerge,
    Conflicts,
    /// Reserved for a caller that reports a failed preview in the outcome
    /// shape; the engine itself returns an error instead.
    Error,
}

/// How one path changes.
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Change {
    Added,
    Modified,
    Deleted,
    Renamed,
}

/// One path applying writes or deletes.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct ChangedPath {
    pub path: String,
    pub change: Change,
    /// The old path of a rename.
    pub from: Option<String>,
}

/// Why applying leaves a path alone.
#[derive(Clone, Copy, Debug, Deserialize, Eq, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum SkipReason {
    /// The handoff's exclusions (left on the laptop, the platform's,
    /// project's or person's patterns, `.eliteaignore`).
    Excluded,
    /// The policy's `path_deny` or the session's deny list.
    PathDeny,
    /// Over [`MAX_FILE_BYTES`].
    TooLarge,
    /// Both sides changed a binary file: never written with markers.
    BinaryConflict,
    /// A symlink, a submodule, anything under a `.git`.
    NotAFile,
    /// Outside the folder (a folder that is a subdirectory of its
    /// repository).
    OutsideFolder,
}

/// One path applying leaves alone.
#[derive(Clone, Debug, Deserialize, Eq, Ord, PartialEq, PartialOrd, Serialize)]
pub struct SkippedPath {
    pub path: String,
    pub reason: SkipReason,
}

/// The answer of a preview and of an apply, for both callers (the client
/// contract of #1187: `applied` false for a preview).
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct ApplyOutcome {
    pub outcome: Outcome,
    pub applied: bool,
    /// What applying writes or deletes (for `conflicts`: what applying with
    /// markers would).
    pub changed_paths: Vec<ChangedPath>,
    /// Paths both sides changed that `merge-tree` could not merge.
    pub conflict_paths: Vec<String>,
    pub skipped_paths: Vec<SkippedPath>,
    /// What the caller named as the base (a commit when it has one).
    pub base: String,
    /// The tree the folder becomes; `None` for `conflicts`.
    pub result_tree: Option<String>,
}

impl ApplyOutcome {
    /// Every path applying writes or deletes, both ends of a rename.
    #[must_use]
    pub fn touched(&self) -> Vec<String> {
        let mut out: Vec<String> = self
            .changed_paths
            .iter()
            .flat_map(|change| std::iter::once(change.path.clone()).chain(change.from.clone()))
            .collect();
        out.sort();
        out
    }

    /// The skipped paths, without their reasons.
    #[must_use]
    pub fn skipped(&self) -> Vec<String> {
        self.skipped_paths.iter().map(|s| s.path.clone()).collect()
    }
}

/// The tree triple of a merge, as tree ids, and how conflict markers name
/// the two sides (`<<<<<<< your folder`, `>>>>>>> cloud`).
#[derive(Clone, Copy, Debug)]
pub struct Trees<'a> {
    pub base: &'a str,
    pub ours: &'a str,
    pub theirs: &'a str,
    pub ours_label: &'a str,
    pub theirs_label: &'a str,
}

/// How conflict markers name the merge base (diff3 styles only).
const BASE_LABEL: &str = "base";

/// Where the engine's git processes find objects and write new ones: the
/// repository itself, or a scratch object directory over it and other
/// object directories (the handoff's quarantine), so a preview never writes
/// into anything but its own scratch.
#[derive(Clone, Debug)]
pub(crate) struct Objects {
    repo: Repo,
    scratch: Option<(PathBuf, OsString)>,
}

impl Objects {
    /// The repository's own object database.
    pub(crate) fn of(repo: &Repo) -> Self {
        Self {
            repo: repo.clone(),
            scratch: None,
        }
    }

    /// New objects in `dir`; `alternates` (and the repository's objects)
    /// read through it.
    #[cfg(feature = "handoff")]
    pub(crate) fn scratch(repo: &Repo, dir: &Path, alternates: &[PathBuf]) -> Self {
        let mut list: Vec<PathBuf> = vec![repo.common_dir().join("objects")];
        list.extend(alternates.iter().cloned());
        let joined = list
            .iter()
            .map(|path| quote_alternate(&path.to_string_lossy()))
            .collect::<Vec<_>>()
            .join(":");
        Self {
            repo: repo.clone(),
            scratch: Some((dir.to_path_buf(), joined.into())),
        }
    }

    pub(crate) fn written_dir(&self) -> PathBuf {
        match &self.scratch {
            Some((dir, _)) => dir.clone(),
            None => self.repo.common_dir().join("objects"),
        }
    }

    pub(crate) fn git(&self) -> Git<'_> {
        let git = self.repo.git(self.repo.top());
        match &self.scratch {
            Some((dir, alternates)) => git
                .env("GIT_OBJECT_DIRECTORY", dir.as_os_str())
                .env("GIT_ALTERNATE_OBJECT_DIRECTORIES", alternates.as_os_str()),
            None => git,
        }
    }
}

/// An entry of `GIT_ALTERNATE_OBJECT_DIRECTORIES`: C-quoted when it holds
/// the separator or a quote.
#[cfg(feature = "handoff")]
fn quote_alternate(path: &str) -> String {
    if !path.contains([':', '"', '\\']) {
        return path.to_owned();
    }
    let mut out = String::from("\"");
    for c in path.chars() {
        if matches!(c, '"' | '\\') {
            out.push('\\');
        }
        out.push(c);
    }
    out.push('"');
    out
}

/// One entry of a tree: its blob and git mode.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct Entry {
    pub(crate) oid: String,
    pub(crate) mode: String,
}

impl Entry {
    fn special(&self) -> bool {
        matches!(self.mode.as_str(), "120000" | "160000")
    }

    fn file_mode(&self) -> u32 {
        if self.mode == "100755" { 0o755 } else { 0o644 }
    }
}

/// One path the apply writes (`new`) or deletes (`None`).
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct Planned {
    /// Folder-relative.
    pub(crate) path: String,
    pub(crate) old: Option<Entry>,
    pub(crate) new: Option<Entry>,
}

/// A preview: the outcome, and what applying it writes.
#[derive(Clone, Debug)]
pub(crate) struct Plan {
    pub(crate) outcome: ApplyOutcome,
    pub(crate) writes: Vec<Planned>,
}

/// What [`write_plan`] did.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub(crate) struct Written {
    pub(crate) written: Vec<String>,
    pub(crate) deleted: Vec<String>,
}

/// The caller's verdict on one changed path (repository-relative): where it
/// lands in the folder, or why it is left alone. `new` is the result's
/// entry (`None`: a deletion), `old` the folder's.
pub(crate) type Decide<'a> =
    dyn Fn(&str, Option<&Entry>, Option<&Entry>) -> Result<String, SkipReason> + 'a;

fn check_id(text: &str) -> ToolResult<()> {
    if text.len() >= 4 && text.len() <= 64 && text.bytes().all(|b| b.is_ascii_hexdigit()) {
        Ok(())
    } else {
        Err(ToolError::invalid(format!("`{text}` is not an object id")))
    }
}

/// The engine's one entry point: preview bringing `trees.theirs` into the
/// folder whose tree is `trees.ours`. Writes nothing but objects (into the
/// scratch directory when `objects` has one).
///
/// # Errors
///
/// A malformed id, git failures.
pub(crate) fn plan(
    objects: &Objects,
    trees: Trees<'_>,
    base_label: &str,
    decide: &Decide<'_>,
) -> ToolResult<Plan> {
    for id in [trees.base, trees.ours, trees.theirs] {
        check_id(id)?;
    }
    let mut conflicts: BTreeMap<String, Vec<Entry>> = BTreeMap::new();
    let mut merged_sides = None;
    let (outcome, result) = if trees.ours == trees.base {
        (Outcome::FastPath, trees.theirs.to_owned())
    } else if trees.theirs == trees.base || trees.theirs == trees.ours {
        (Outcome::CleanMerge, trees.ours.to_owned())
    } else {
        let (tree, conflicted, commits) = merge_tree(objects, trees)?;
        conflicts = conflicted;
        merged_sides = Some(commits);
        let outcome = if conflicts.is_empty() {
            Outcome::CleanMerge
        } else {
            Outcome::Conflicts
        };
        (outcome, tree)
    };
    let binary = binary_conflicts(objects, &conflicts)?;
    let mut raw = diff_trees(objects, trees.ours, &result)?;
    if let Some(commits) = &merged_sides {
        let labels = [
            (commits.ours.as_str(), trees.ours_label),
            (commits.theirs.as_str(), trees.theirs_label),
            (commits.base.as_str(), BASE_LABEL),
        ];
        relabel_markers(objects, &mut raw, &conflicts, &binary, &labels)?;
    }
    let sizes = sizes(
        objects,
        raw.iter()
            .flat_map(|change| change.new.iter().map(|entry| entry.oid.as_str())),
    )?;
    let mut writes = Vec::new();
    let mut skipped = BTreeSet::new();
    // Where each side of a rename landed (both written: one `renamed`).
    let mut landed: BTreeMap<usize, String> = BTreeMap::new();
    let mut verdicts = Vec::with_capacity(raw.len());
    for change in &raw {
        verdicts.push(if binary.contains(&change.path) {
            Err(SkipReason::BinaryConflict)
        } else if change.path.split('/').any(is_protected_name)
            || change.old.as_ref().is_some_and(Entry::special)
            || change.new.as_ref().is_some_and(Entry::special)
        {
            Err(SkipReason::NotAFile)
        } else if change.new.as_ref().is_some_and(|entry| {
            sizes.get(&entry.oid).copied().unwrap_or(u64::MAX) > MAX_FILE_BYTES
        }) {
            Err(SkipReason::TooLarge)
        } else {
            decide(&change.path, change.new.as_ref(), change.old.as_ref())
        });
    }
    block_kept_paths(&raw, &mut verdicts);
    for (index, (change, verdict)) in raw.iter().zip(verdicts).enumerate() {
        match verdict {
            Ok(path) => {
                landed.insert(index, path.clone());
                writes.push(Planned {
                    path,
                    old: change.old.clone(),
                    new: change.new.clone(),
                });
            }
            Err(reason) => {
                skipped.insert(SkippedPath {
                    path: change.path.clone(),
                    reason,
                });
            }
        }
    }
    for path in &binary {
        skipped.insert(SkippedPath {
            path: path.clone(),
            reason: SkipReason::BinaryConflict,
        });
    }
    let changed_paths = changed_paths(&raw, &landed);
    let conflict_paths: Vec<String> = conflicts.keys().cloned().collect();
    Ok(Plan {
        outcome: ApplyOutcome {
            outcome,
            applied: false,
            changed_paths,
            conflict_paths,
            skipped_paths: skipped.into_iter().collect(),
            base: base_label.to_owned(),
            result_tree: (outcome != Outcome::Conflicts).then_some(result),
        },
        writes,
    })
}

/// The changed paths of a plan, a rename as one entry when both of its
/// sides are written.
fn changed_paths(raw: &[RawChange], landed: &BTreeMap<usize, String>) -> Vec<ChangedPath> {
    let mut out = Vec::new();
    let mut paired = BTreeSet::new();
    for (index, change) in raw.iter().enumerate() {
        let Some(path) = landed.get(&index) else {
            continue;
        };
        if paired.contains(&index) {
            continue;
        }
        if let Some(other) = change.renamed_with
            && let Some(other_path) = landed.get(&other)
        {
            paired.insert(other);
            let (from, to) = if change.new.is_some() {
                (other_path.clone(), path.clone())
            } else {
                (path.clone(), other_path.clone())
            };
            out.push(ChangedPath {
                path: to,
                change: Change::Renamed,
                from: Some(from),
            });
            continue;
        }
        let kind = match (&change.old, &change.new) {
            (None, _) => Change::Added,
            (_, None) => Change::Deleted,
            _ => Change::Modified,
        };
        out.push(ChangedPath {
            path: path.clone(),
            change: kind,
            from: None,
        });
    }
    out.sort_by(|a, b| a.path.cmp(&b.path));
    out
}

/// `path` is strictly inside the directory `dir`.
fn is_under(path: &str, dir: &str) -> bool {
    path.len() > dir.len() && path.starts_with(dir) && path.as_bytes()[dir.len()] == b'/'
}

/// A path the folder keeps (its change skipped) stays where it is, so the
/// result cannot put a file over its directory or a directory over it: a
/// file whose path holds a kept file, or a file under a kept file, is left
/// alone too, for the kept path's reason.
fn block_kept_paths(raw: &[RawChange], verdicts: &mut [Result<String, SkipReason>]) {
    let kept: Vec<(String, SkipReason)> = raw
        .iter()
        .zip(verdicts.iter())
        .filter_map(|(change, verdict)| match verdict {
            Err(reason) if change.old.is_some() => Some((change.path.clone(), *reason)),
            _ => None,
        })
        .collect();
    if kept.is_empty() {
        return;
    }
    for (change, verdict) in raw.iter().zip(verdicts.iter_mut()) {
        if verdict.is_err() || change.new.is_none() {
            continue;
        }
        if let Some((_, reason)) = kept
            .iter()
            .find(|(path, _)| is_under(path, &change.path) || is_under(&change.path, path))
        {
            *verdict = Err(*reason);
        }
    }
}

/// Give the conflict markers of the conflicted text files the sides'
/// names instead of the throwaway commits' ids (`merge-tree` labels each
/// side with the revision it was given): the relabelled blob is written
/// (into the scratch directory when there is one) and becomes the change's
/// new entry.
fn relabel_markers(
    objects: &Objects,
    raw: &mut [RawChange],
    conflicts: &BTreeMap<String, Vec<Entry>>,
    binary: &BTreeSet<String>,
    labels: &[(&str, &str)],
) -> ToolResult<()> {
    let marked: Vec<usize> = raw
        .iter()
        .enumerate()
        .filter(|(_, change)| {
            conflicts.contains_key(&change.path)
                && !binary.contains(&change.path)
                && change.new.as_ref().is_some_and(|entry| !entry.special())
        })
        .map(|(index, _)| index)
        .collect();
    let oids: Vec<&str> = marked
        .iter()
        .filter_map(|index| raw[*index].new.as_ref().map(|entry| entry.oid.as_str()))
        .collect();
    let blobs = read_blobs(objects, &oids, MAX_MARKER_FILE_BYTES)?;
    for index in marked {
        let Some(entry) = raw[index].new.as_mut() else {
            continue;
        };
        let Some(bytes) = blobs.get(&entry.oid) else {
            continue;
        };
        let relabelled = relabel(bytes, labels);
        if relabelled == *bytes {
            continue;
        }
        entry.oid = objects
            .git()
            .writes(&[objects.written_dir()])
            .stdin(&relabelled)
            .text(&["hash-object", "-w", "--no-filters", "--stdin"])?;
    }
    Ok(())
}

/// `bytes` with each `from` id in a conflict-marker line replaced by its
/// label.
fn relabel(bytes: &[u8], labels: &[(&str, &str)]) -> Vec<u8> {
    const MARKERS: [&[u8]; 3] = [b"<<<<<<< ", b">>>>>>> ", b"||||||| "];
    let mut out = Vec::with_capacity(bytes.len());
    for line in bytes.split_inclusive(|byte| *byte == b'\n') {
        if !MARKERS.iter().any(|marker| line.starts_with(marker)) {
            out.extend_from_slice(line);
            continue;
        }
        let mut line = line.to_vec();
        for (from, to) in labels {
            line = replace_bytes(&line, from.as_bytes(), to.as_bytes());
        }
        out.extend_from_slice(&line);
    }
    out
}

fn replace_bytes(haystack: &[u8], from: &[u8], to: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(haystack.len());
    let mut rest = haystack;
    while let Some(at) = rest.windows(from.len()).position(|window| window == from) {
        out.extend_from_slice(&rest[..at]);
        out.extend_from_slice(to);
        rest = &rest[at + from.len()..];
    }
    out.extend_from_slice(rest);
    out
}

/// The error of a plan that cannot land: something in the folder (an
/// ignored file the snapshot does not hold) is in the way.
fn in_the_way(path: &str, what: &str) -> ToolError {
    ToolError::new(
        ErrorCode::Conflict,
        format!(
            "path_in_the_way: the folder has {what} at `{path}` where the result needs the other; move it and preview again"
        ),
    )
}

/// Every file under the directory `dir` (folder-relative) is in `deleting`.
fn emptied_by(workspace: &Workspace, dir: &str, deleting: &BTreeSet<&str>) -> ToolResult<bool> {
    let mut stack = vec![dir.to_owned()];
    while let Some(next) = stack.pop() {
        let absolute = workspace.absolute(&WsPath::from_relative(Path::new(&next))?);
        let entries = std::fs::read_dir(&absolute)
            .map_err(|error| ToolError::io("cannot list the folder", &error))?;
        for entry in entries {
            let entry = entry.map_err(|error| ToolError::io("cannot list the folder", &error))?;
            let name = entry.file_name().to_string_lossy().into_owned();
            let child = format!("{next}/{name}");
            let is_dir = entry
                .file_type()
                .map_err(|error| ToolError::io("cannot list the folder", &error))?
                .is_dir();
            if is_dir {
                stack.push(child);
            } else if !deleting.contains(child.as_str()) {
                return Ok(false);
            }
        }
    }
    Ok(true)
}

/// Check, before anything is written, that every file of `plan` can land
/// in `workspace`: no file (kept) where a written file needs a directory,
/// no directory holding more than the plan deletes where it writes a
/// file. Callers run it before their checkpoint; [`write_plan`] runs it
/// again.
///
/// # Errors
///
/// [`ErrorCode::Conflict`] (`path_in_the_way`), unreadable folder.
pub(crate) fn check_plan(workspace: &Workspace, plan: &Plan) -> ToolResult<()> {
    let deleting: BTreeSet<&str> = plan
        .writes
        .iter()
        .filter(|write| write.new.is_none())
        .map(|write| write.path.as_str())
        .collect();
    for write in plan.writes.iter().filter(|write| write.new.is_some()) {
        let parts: Vec<&str> = write.path.split('/').collect();
        // Whether the path can exist now (no ancestor missing or a file).
        let mut reachable = true;
        for end in 1..parts.len() {
            let ancestor = parts[..end].join("/");
            let kind = workspace.stat(&WsPath::from_relative(Path::new(&ancestor))?)?;
            match kind {
                Some(EntryKind::Dir) => continue,
                Some(_) if !deleting.contains(ancestor.as_str()) => {
                    return Err(in_the_way(&ancestor, "a file"));
                }
                // Missing, or a file the plan deletes first.
                _ => {}
            }
            reachable = false;
            break;
        }
        let ws_path = WsPath::from_relative(Path::new(&write.path))?;
        if reachable
            && workspace.stat(&ws_path)? == Some(EntryKind::Dir)
            && !emptied_by(workspace, &write.path, &deleting)?
        {
            return Err(in_the_way(&write.path, "a directory"));
        }
    }
    Ok(())
}

/// Write a plan into `workspace` (the caller checked the folder and took
/// its checkpoint). It is checked first ([`check_plan`]), so it fails
/// before writing anything, not halfway; then every deletion goes first
/// (and the directories they empty), so a directory the result turns into
/// a file, or a file it turns into a directory, is out of the way.
///
/// # Errors
///
/// A path in the way, an unreadable object, write failures.
pub(crate) fn write_plan(
    objects: &Objects,
    workspace: &Workspace,
    plan: &Plan,
) -> ToolResult<Written> {
    check_plan(workspace, plan)?;
    let mut done = Written::default();
    let new: Vec<&str> = plan
        .writes
        .iter()
        .filter_map(|write| write.new.as_ref().map(|entry| entry.oid.as_str()))
        .collect();
    let blobs = read_blobs(objects, &new, MAX_FILE_BYTES)?;
    for write in plan.writes.iter().filter(|write| write.new.is_none()) {
        let ws_path = WsPath::from_relative(Path::new(&write.path))?;
        if workspace.remove_file(&ws_path)? {
            done.deleted.push(write.path.clone());
        }
        // The directories this deletion emptied go too (git keeps none).
        let parts: Vec<&str> = write.path.split('/').collect();
        for end in (1..parts.len()).rev() {
            let dir = WsPath::from_relative(Path::new(&parts[..end].join("/")))?;
            if !workspace.remove_empty_dir(&dir)? {
                break;
            }
        }
    }
    for write in &plan.writes {
        let Some(entry) = &write.new else {
            continue;
        };
        let ws_path = WsPath::from_relative(Path::new(&write.path))?;
        let bytes = blobs.get(&entry.oid).ok_or_else(|| {
            ToolError::new(ErrorCode::Git, format!("object {} is missing", entry.oid))
        })?;
        workspace.write(&ws_path, bytes, Some(entry.file_mode()))?;
        done.written.push(write.path.clone());
    }
    Ok(done)
}

/// Wrap `tree` in a commit of its own (no parent, fixed identity and date),
/// for a `merge-tree` that merges commits only.
fn commit_of(objects: &Objects, tree: &str) -> ToolResult<String> {
    let name = OsStr::new("Elitea merge");
    let email = OsStr::new("merge@elitea.invalid");
    let date = OsStr::new("@0 +0000");
    objects
        .git()
        .env("GIT_AUTHOR_NAME", name)
        .env("GIT_AUTHOR_EMAIL", email)
        .env("GIT_AUTHOR_DATE", date)
        .env("GIT_COMMITTER_NAME", name)
        .env("GIT_COMMITTER_EMAIL", email)
        .env("GIT_COMMITTER_DATE", date)
        .writes(&[objects.written_dir()])
        .text(&["commit-tree", "--no-gpg-sign", tree, "-m", "merge side"])
}

/// `merge-tree --write-tree` of the triple: the result tree (with conflict
/// markers in conflicted text files) and the conflicted paths with their
/// stage entries (none: a clean merge).
/// The throwaway commits `merge-tree` merged (the ids its markers carry).
struct SideCommits {
    base: String,
    ours: String,
    theirs: String,
}

fn merge_tree(
    objects: &Objects,
    trees: Trees<'_>,
) -> ToolResult<(String, BTreeMap<String, Vec<Entry>>, SideCommits)> {
    let base = commit_of(objects, trees.base)?;
    let ours = commit_of(objects, trees.ours)?;
    let theirs = commit_of(objects, trees.theirs)?;
    let empty_tree = objects
        .git()
        .writes(&[objects.written_dir()])
        .stdin(b"")
        .text(&["hash-object", "-w", "-t", "tree", "--stdin"])?;
    let attr_source = format!("--attr-source={empty_tree}");
    let merge_base = format!("--merge-base={base}");
    let output = objects
        .git()
        .writes(&[objects.written_dir()])
        .exit_one_ok()
        .run(&[
            "-c",
            "merge.default=text",
            &attr_source,
            "merge-tree",
            "--write-tree",
            "-z",
            "--no-messages",
            &merge_base,
            &ours,
            &theirs,
        ])?;
    let mut fields = output.split(|byte| *byte == 0);
    let tree = fields
        .next()
        .map(|tree| String::from_utf8_lossy(tree).trim().to_owned())
        .filter(|tree| check_id(tree).is_ok())
        .ok_or_else(|| ToolError::new(ErrorCode::Git, "git merge-tree gave no tree"))?;
    let mut conflicts: BTreeMap<String, Vec<Entry>> = BTreeMap::new();
    for field in fields.filter(|field| !field.is_empty()) {
        // `<mode> <object> <stage>\t<path>`
        let text = String::from_utf8_lossy(field);
        let Some((meta, path)) = text.split_once('\t') else {
            continue;
        };
        let parts: Vec<&str> = meta.split(' ').collect();
        let [mode, oid, _stage] = parts.as_slice() else {
            continue;
        };
        conflicts.entry(path.to_owned()).or_default().push(Entry {
            oid: (*oid).to_owned(),
            mode: (*mode).to_owned(),
        });
    }
    Ok((tree, conflicts, SideCommits { base, ours, theirs }))
}

/// The conflicted paths whose sides are binary (a NUL in the first 8000
/// bytes, as git sniffs it, or too large to merge as text).
fn binary_conflicts(
    objects: &Objects,
    conflicts: &BTreeMap<String, Vec<Entry>>,
) -> ToolResult<BTreeSet<String>> {
    let oids: Vec<&str> = conflicts
        .values()
        .flatten()
        .filter(|entry| !entry.special())
        .map(|entry| entry.oid.as_str())
        .collect();
    let blobs = read_blobs(objects, &oids, MAX_MARKER_FILE_BYTES)?;
    Ok(conflicts
        .iter()
        .filter(|(_, entries)| {
            entries
                .iter()
                .filter(|entry| !entry.special())
                .any(|entry| {
                    blobs.get(&entry.oid).is_none_or(|bytes| {
                        bytes[..bytes.len().min(BINARY_SNIFF_BYTES)].contains(&0)
                    })
                })
        })
        .map(|(path, _)| path.clone())
        .collect())
}

/// One path `diff-tree` reported; a rename is two (its deletion and its
/// addition), each pointing at the other.
#[derive(Clone, Debug)]
struct RawChange {
    path: String,
    old: Option<Entry>,
    new: Option<Entry>,
    renamed_with: Option<usize>,
}

const NULL_OID: &str = "0000000000000000000000000000000000000000";

/// What turning tree `from` into tree `to` changes, by path (renames
/// detected for the report, written as a deletion and an addition).
fn diff_trees(objects: &Objects, from: &str, to: &str) -> ToolResult<Vec<RawChange>> {
    if from == to {
        return Ok(Vec::new());
    }
    let raw = objects
        .git()
        .run(&["diff-tree", "-r", "-z", "--raw", "-M", from, to])?;
    let mut fields = raw.split(|byte| *byte == 0).filter(|f| !f.is_empty());
    let entry = |mode: &str, oid: &str| {
        (mode != "000000" && oid != NULL_OID).then(|| Entry {
            oid: oid.to_owned(),
            mode: mode.to_owned(),
        })
    };
    let mut out = Vec::new();
    while let Some(meta) = fields.next() {
        let meta = String::from_utf8_lossy(meta);
        // `:<old mode> <new mode> <old id> <new id> <status>`
        let parts: Vec<&str> = meta.trim_start_matches(':').split(' ').collect();
        let [old_mode, new_mode, old_id, new_id, status] = parts.as_slice() else {
            continue;
        };
        let Some(path) = fields.next() else { break };
        let path = String::from_utf8_lossy(path).into_owned();
        if status.starts_with('R') {
            let Some(to_path) = fields.next() else { break };
            let to_path = String::from_utf8_lossy(to_path).into_owned();
            let at = out.len();
            out.push(RawChange {
                path,
                old: entry(old_mode, old_id),
                new: None,
                renamed_with: Some(at + 1),
            });
            out.push(RawChange {
                path: to_path,
                old: None,
                new: entry(new_mode, new_id),
                renamed_with: Some(at),
            });
            continue;
        }
        out.push(RawChange {
            path,
            old: entry(old_mode, old_id),
            new: if status.starts_with('D') {
                None
            } else {
                entry(new_mode, new_id)
            },
            renamed_with: None,
        });
    }
    Ok(out)
}

/// The sizes of `oids` (one `cat-file --batch-check`); a missing object is
/// left out.
fn sizes<'a>(
    objects: &Objects,
    oids: impl Iterator<Item = &'a str>,
) -> ToolResult<BTreeMap<String, u64>> {
    let unique: BTreeSet<&str> = oids.collect();
    if unique.is_empty() {
        return Ok(BTreeMap::new());
    }
    let mut input = Vec::new();
    for oid in &unique {
        input.extend_from_slice(oid.as_bytes());
        input.push(b'\n');
    }
    let output = objects.git().stdin(&input).run(&[
        "cat-file",
        "--batch-check=%(objectname) %(objecttype) %(objectsize)",
    ])?;
    let mut out = BTreeMap::new();
    for line in String::from_utf8_lossy(&output).lines() {
        let parts: Vec<&str> = line.split(' ').collect();
        if let [oid, "blob", size] = parts.as_slice()
            && let Ok(size) = size.parse::<u64>()
        {
            out.insert((*oid).to_owned(), size);
        }
    }
    Ok(out)
}

/// The blobs `oids` of at most `max` bytes each, read in bounded batches;
/// a larger or missing one is left out.
pub(crate) fn read_blobs(
    objects: &Objects,
    oids: &[&str],
    max: u64,
) -> ToolResult<BTreeMap<String, Vec<u8>>> {
    let sizes = sizes(objects, oids.iter().copied())?;
    let mut out = BTreeMap::new();
    let mut batch: Vec<&str> = Vec::new();
    let mut batch_bytes = 0u64;
    let wanted: Vec<(&String, u64)> = sizes
        .iter()
        .filter(|(_, size)| **size <= max)
        .map(|(oid, size)| (oid, *size))
        .collect();
    for (index, (oid, size)) in wanted.iter().enumerate() {
        batch.push(oid.as_str());
        batch_bytes += size;
        let last = index + 1 == wanted.len();
        if last || batch_bytes >= BATCH_BYTES {
            read_batch(objects, &batch, &mut out)?;
            batch.clear();
            batch_bytes = 0;
        }
    }
    Ok(out)
}

fn read_batch(
    objects: &Objects,
    oids: &[&str],
    out: &mut BTreeMap<String, Vec<u8>>,
) -> ToolResult<()> {
    if oids.is_empty() {
        return Ok(());
    }
    let mut input = Vec::new();
    for oid in oids {
        input.extend_from_slice(oid.as_bytes());
        input.push(b'\n');
    }
    let output = objects.git().stdin(&input).run(&["cat-file", "--batch"])?;
    let mut rest: &[u8] = &output;
    while !rest.is_empty() {
        let Some(newline) = rest.iter().position(|b| *b == b'\n') else {
            break;
        };
        let header = String::from_utf8_lossy(&rest[..newline]).into_owned();
        rest = &rest[newline + 1..];
        let parts: Vec<&str> = header.split(' ').collect();
        let [oid, _kind, size] = parts.as_slice() else {
            // `<oid> missing`
            continue;
        };
        let size: usize = size
            .parse()
            .map_err(|_| ToolError::new(ErrorCode::Git, "git cat-file gave a bad size"))?;
        if rest.len() < size {
            return Err(ToolError::new(
                ErrorCode::Git,
                "git cat-file output was cut",
            ));
        }
        out.insert((*oid).to_owned(), rest[..size].to_vec());
        rest = rest.get(size + 1..).unwrap_or_default();
    }
    Ok(())
}

// ---- The worktree merge-back (#1235) ----

/// A worktree merge-back preview, held until the person confirms.
#[derive(Clone, Debug)]
pub struct Preflight {
    pub report: ApplyOutcome,
    /// The folder's snapshot (a commit) the preview was made against.
    ours: String,
    plan: Plan,
}

/// What [`apply`] did.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct Applied {
    pub written: Vec<String>,
    pub deleted: Vec<String>,
    /// The checkpoint taken first (restore it to undo the merge); `None`
    /// when nothing was written.
    pub checkpoint: Option<u64>,
    /// The preview's outcome, `applied` set.
    pub report: ApplyOutcome,
}

fn repo_of(session: &LocalSession) -> ToolResult<&Repo> {
    match (
        session.checkpoints().repo(),
        session.checkpoints().git_refusal(),
    ) {
        (Some(repo), _) => Ok(repo),
        (None, Some(refusal)) => Err(refusal.clone()),
        (None, None) => Err(ToolError::new(
            ErrorCode::Git,
            "the folder is not in a git repository",
        )),
    }
}

fn tree_of(repo: &Repo, commit: &str) -> ToolResult<String> {
    repo.git(repo.top())
        .text(&["rev-parse", "--verify", &format!("{commit}^{{tree}}")])
}

/// Preview merging `source` (a commit of the folder's repository) into
/// `target`'s folder, `base` being the commit `source`'s work started
/// from. Writes nothing but objects.
///
/// # Errors
///
/// The folder is not in git or is refused, a commit id is malformed or
/// unknown, too many untracked bytes to snapshot, git failures.
pub fn preflight(target: &LocalSession, source: &str, base: &str) -> ToolResult<Preflight> {
    check_id(source)?;
    check_id(base)?;
    let repo = repo_of(target)?;
    let ours = target.checkpoints().snapshot_commit()?;
    let (ours_tree, theirs_tree, base_tree) = (
        tree_of(repo, &ours)?,
        tree_of(repo, source)?,
        tree_of(repo, base)?,
    );
    let workspace = target.workspace();
    let prefix = workspace
        .root()
        .strip_prefix(repo.top())
        .map(Path::to_path_buf)
        .unwrap_or_default();
    let decide = |path: &str, _new: Option<&Entry>, _old: Option<&Entry>| {
        let relative = Path::new(path)
            .strip_prefix(&prefix)
            .map_err(|_| SkipReason::OutsideFolder)?;
        let ws_path = WsPath::from_relative(relative).map_err(|_| SkipReason::OutsideFolder)?;
        if workspace.denied_by(&ws_path, Intent::Write).is_some() {
            return Err(SkipReason::PathDeny);
        }
        Ok(relative.to_string_lossy().into_owned())
    };
    let plan = plan(
        &Objects::of(repo),
        Trees {
            base: &base_tree,
            ours: &ours_tree,
            theirs: &theirs_tree,
            ours_label: "your folder",
            theirs_label: "task worktree",
        },
        base,
        &decide,
    )?;
    Ok(Preflight {
        report: plan.outcome.clone(),
        ours,
        plan,
    })
}

/// Apply a preview to `target`'s folder (see the module documentation);
/// `label` names the checkpoint taken first. A preview with conflicts is
/// refused ([`apply_with_markers`] writes it).
///
/// # Errors
///
/// [`ErrorCode::StaleRead`] ([`FOLDER_CHANGED`]) when the folder changed
/// since the preview; [`ErrorCode::InvalidArgument`] for a preview with
/// conflicts; checkpoint, git or write failures (a failure after the
/// checkpoint leaves it to restore).
pub fn apply(target: &LocalSession, preflight: &Preflight, label: &str) -> ToolResult<Applied> {
    apply_mode(target, preflight, label, false)
}

/// [`apply`], conflicts included: conflicted text files get conflict
/// markers, a binary conflict keeps the folder's version (`binary_conflict`).
///
/// # Errors
///
/// As [`apply`], without the refusal of conflicts.
pub fn apply_with_markers(
    target: &LocalSession,
    preflight: &Preflight,
    label: &str,
) -> ToolResult<Applied> {
    apply_mode(target, preflight, label, true)
}

fn apply_mode(
    target: &LocalSession,
    preflight: &Preflight,
    label: &str,
    markers: bool,
) -> ToolResult<Applied> {
    let repo = repo_of(target)?;
    if preflight.report.outcome == Outcome::Conflicts && !markers {
        return Err(ToolError::invalid(
            "merge_conflicts: both sides changed the same lines; the work stays on its branch",
        ));
    }
    let now = target.checkpoints().snapshot_commit()?;
    if tree_of(repo, &now)? != tree_of(repo, &preflight.ours)? {
        return Err(ToolError::new(
            ErrorCode::StaleRead,
            format!(
                "{FOLDER_CHANGED}: the folder changed after the preview; preview the merge again"
            ),
        ));
    }
    let mut applied = Applied {
        written: Vec::new(),
        deleted: Vec::new(),
        checkpoint: None,
        report: ApplyOutcome {
            applied: true,
            ..preflight.report.clone()
        },
    };
    if preflight.plan.writes.is_empty() {
        return Ok(applied);
    }
    check_plan(target.workspace(), &preflight.plan)?;
    applied.checkpoint = Some(target.checkpoints().create(label)?.seq);
    let done = write_plan(&Objects::of(repo), target.workspace(), &preflight.plan)?;
    applied.written = done.written;
    applied.deleted = done.deleted;
    Ok(applied)
}

#[cfg(test)]
mod tests {
    use std::path::{Path, PathBuf};
    use std::process::Command;
    use std::sync::Arc;

    use super::{Outcome, SkipReason, SkippedPath, apply, preflight};
    use crate::approvals::{DeferringPrompt, MemoryChoices, WorkspaceSettings};
    use crate::error::ErrorCode;
    use crate::git::Repo;
    use crate::policy::LocalWorkPolicy;
    use crate::session::{LocalSession, SessionConfig};

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
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .output()
            .expect("git");
        assert!(
            output.status.success(),
            "git {args:?}: {}",
            String::from_utf8_lossy(&output.stderr)
        );
        String::from_utf8_lossy(&output.stdout).trim().to_owned()
    }

    struct Fixture {
        _dir: tempfile::TempDir,
        root: PathBuf,
        data: PathBuf,
        tree: PathBuf,
        base: String,
    }

    /// A repository with a task worktree of its `HEAD`.
    fn fixture() -> Fixture {
        let dir = tempfile::tempdir().expect("dir");
        let base = std::fs::canonicalize(dir.path()).expect("canonical");
        let root = base.join("repo");
        let data = base.join("data");
        std::fs::create_dir_all(&root).expect("repo");
        std::fs::create_dir_all(data.join("worktrees")).expect("data");
        git(&root, &["init", "-q"]);
        std::fs::write(root.join("a.txt"), "1\n2\n3\n4\n5\n6\n7\n").expect("a");
        std::fs::write(root.join("doomed.txt"), "doomed\n").expect("doomed");
        git(&root, &["add", "-A"]);
        git(&root, &["commit", "-q", "-m", "init"]);
        let main = Repo::discover(
            &root,
            &crate::sandbox::SandboxConfig::default(),
            Arc::new(crate::deny::DenyList::new([])),
        )
        .expect("discover")
        .expect("repo");
        let created = crate::worktree::create(
            &main,
            &data.join("worktrees/abcd1234"),
            "abcd1234",
            "elitea/abcd1234",
            "HEAD",
        )
        .expect("create");
        Fixture {
            _dir: dir,
            tree: created.entry.worktree,
            base: created.base_sha,
            root,
            data,
        }
    }

    fn session(root: &Path, data: &Path, id: &str, path_deny: &[&str]) -> Arc<LocalSession> {
        LocalSession::open(SessionConfig {
            root: root.to_path_buf(),
            session_id: id.to_owned(),
            policy: LocalWorkPolicy {
                allowed: true,
                path_deny: path_deny.iter().map(|p| (*p).to_owned()).collect(),
                ..LocalWorkPolicy::default()
            },
            settings: WorkspaceSettings::default(),
            choices: Arc::new(MemoryChoices::default()),
            prompt: Arc::new(DeferringPrompt),
            data_dir: data.join(id),
            shell: None,
            deny_read: Vec::new(),
            app_id: None,
            budgets: crate::budget::Budgets::default(),
            documents: crate::document::Documents::default(),
        })
        .expect("session")
    }

    fn read(dir: &Path, name: &str) -> Option<String> {
        std::fs::read_to_string(dir.join(name)).ok()
    }

    #[test]
    fn preflight_reports_conflicts_without_writing() {
        let f = fixture();
        std::fs::write(f.tree.join("a.txt"), "1\ntwo\n3\n4\n5\n6\n7\n").expect("theirs");
        std::fs::write(f.tree.join("b.txt"), "new\n").expect("b");
        let theirs = session(&f.tree, &f.data, "wt", &[])
            .checkpoints()
            .snapshot_commit()
            .expect("theirs");
        std::fs::write(f.root.join("a.txt"), "1\nTWO\n3\n4\n5\n6\n7\n").expect("ours");
        let head = git(&f.root, &["rev-parse", "HEAD"]);
        let status = git(&f.root, &["status", "--porcelain"]);
        let main = session(&f.root, &f.data, "main", &[]);

        let preview = preflight(&main, &theirs, &f.base).expect("preflight");
        assert_eq!(preview.report.outcome, Outcome::Conflicts);
        assert_eq!(preview.report.conflict_paths, ["a.txt"]);
        assert_eq!(preview.report.result_tree, None);
        // What applying with markers would write.
        assert_eq!(preview.report.touched(), ["a.txt", "b.txt"]);
        // Nothing the person sees moved: the folder, HEAD, the index, refs.
        assert_eq!(
            read(&f.root, "a.txt").as_deref(),
            Some("1\nTWO\n3\n4\n5\n6\n7\n")
        );
        assert!(read(&f.root, "b.txt").is_none());
        assert_eq!(git(&f.root, &["rev-parse", "HEAD"]), head);
        assert_eq!(git(&f.root, &["status", "--porcelain"]), status);
        assert_eq!(git(&f.root, &["for-each-ref", "refs/elitea"]), "");
        let refused = apply(&main, &preview, "merge").expect_err("conflicts");
        assert_eq!(refused.code(), ErrorCode::InvalidArgument);
        assert!(read(&f.root, "b.txt").is_none());

        // Different lines merge cleanly, 3-way.
        std::fs::write(f.root.join("a.txt"), "1\n2\n3\n4\n5\n6\nSEVEN\n").expect("ours");
        let preview = preflight(&main, &theirs, &f.base).expect("preflight");
        assert_eq!(preview.report.outcome, Outcome::CleanMerge);
        assert_eq!(preview.report.touched(), ["a.txt", "b.txt"]);
        assert!(preview.report.conflict_paths.is_empty(), "{preview:?}");
        assert!(preview.report.result_tree.is_some());
        assert_eq!(
            read(&f.root, "a.txt").as_deref(),
            Some("1\n2\n3\n4\n5\n6\nSEVEN\n")
        );
        let applied = apply(&main, &preview, "merge").expect("apply");
        assert_eq!(applied.written, ["a.txt", "b.txt"]);
        assert_eq!(
            read(&f.root, "a.txt").as_deref(),
            Some("1\ntwo\n3\n4\n5\n6\nSEVEN\n")
        );
    }

    #[test]
    fn apply_checkpoints_first_and_is_undoable() {
        let f = fixture();
        std::fs::write(f.tree.join("a.txt"), "1\ntwo\n3\n4\n5\n6\n7\n").expect("edit");
        std::fs::write(f.tree.join("b.txt"), "new\n").expect("add");
        std::fs::write(f.tree.join("secret.txt"), "token\n").expect("denied");
        std::fs::remove_file(f.tree.join("doomed.txt")).expect("delete");
        let theirs = session(&f.tree, &f.data, "wt", &[])
            .checkpoints()
            .snapshot_commit()
            .expect("theirs");
        let head = git(&f.root, &["rev-parse", "HEAD"]);
        let main = session(&f.root, &f.data, "main", &["secret.txt"]);

        let preview = preflight(&main, &theirs, &f.base).expect("preflight");
        assert_eq!(preview.report.outcome, Outcome::FastPath);
        assert_eq!(preview.report.touched(), ["a.txt", "b.txt", "doomed.txt"]);
        let denied = [SkippedPath {
            path: "secret.txt".into(),
            reason: SkipReason::PathDeny,
        }];
        assert_eq!(preview.report.skipped_paths, denied);

        // The folder moved since the preview: refused, nothing written.
        std::fs::write(f.root.join("late.txt"), "late\n").expect("late");
        let stale = apply(&main, &preview, "merge").expect_err("stale");
        assert_eq!(stale.code(), ErrorCode::StaleRead);
        assert!(read(&f.root, "b.txt").is_none());
        std::fs::remove_file(f.root.join("late.txt")).expect("unlate");

        let applied = apply(&main, &preview, "merge").expect("apply");
        assert_eq!(applied.written, ["a.txt", "b.txt"]);
        assert_eq!(applied.deleted, ["doomed.txt"]);
        assert_eq!(applied.report.skipped_paths, denied);
        assert!(applied.report.applied);
        let seq = applied.checkpoint.expect("a checkpoint first");
        assert_eq!(
            read(&f.root, "a.txt").as_deref(),
            Some("1\ntwo\n3\n4\n5\n6\n7\n")
        );
        assert!(read(&f.root, "secret.txt").is_none());
        // Uncommitted changes: HEAD stays.
        assert_eq!(git(&f.root, &["rev-parse", "HEAD"]), head);

        main.restore_checkpoint(seq).expect("undo");
        assert_eq!(
            read(&f.root, "a.txt").as_deref(),
            Some("1\n2\n3\n4\n5\n6\n7\n")
        );
        assert_eq!(read(&f.root, "doomed.txt").as_deref(), Some("doomed\n"));
        assert!(read(&f.root, "b.txt").is_none());
    }
}
