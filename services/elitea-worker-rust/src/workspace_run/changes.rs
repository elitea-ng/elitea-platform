//! The files a cloud turn changed (ADR-0034 Phase 2: the web "Files
//! changed" view), computed by the worker from its own repository right
//! after the publish, outside the sandbox, and recorded with Main over the
//! claim-bound `WorkspaceRecordChanges` stream. Main reads no git pack.
//!
//! Two scopes: the TURN (the parent's tree to the commit's tree) and the
//! CUMULATIVE set (the handoff snapshot's tree to the commit's tree). Both
//! come from committed trees, so what the end-of-turn redaction removed is
//! never in a diff.
//!
//! Bounds, per scope (every one keeps the work and the wire bounded; none
//! fails the record):
//! * at most [`MAX_FILES`] files listed, sorted by path; the rest, and any
//!   path over [`MAX_PATH_BYTES`], are counted as omitted;
//! * a blob over [`MAX_BLOB_BYTES`], a trimmed middle over
//!   [`MAX_MIDDLE_LINES`] lines (Myers' trace is O(D·(N+M))), or a file past
//!   [`MAX_DIFFED_BYTES`] read in the scope is `too_large`: no counts;
//! * a diff is kept when it is at most [`MAX_FILE_DIFF_BYTES`] and the
//!   scope's kept diffs stay within [`MAX_SCOPE_DIFF_BYTES`]; otherwise the
//!   counts stay and the diff is empty.
//!
//! A symlink on either side, or a NUL in either side's first 8000 bytes, is
//! `binary`. Submodule commits are not listed. A rename is exact: a deleted
//! and an added file with the same blob, paired in path order.

use std::collections::BTreeMap;

use elitea_workspace_store::StoreError;
use elitea_workspace_store::objects::{ChangeKind, FileEntry, ObjectDb, diff, oid};

use crate::protocol::elitea::runtime::v1::{
    WorkspaceChangeScopeV1, WorkspaceChangeStatusV1, WorkspaceChangedFileV1,
    WorkspaceChangesFrameV1, WorkspaceChangesHeaderV1, WorkspaceClaimV1,
    WorkspaceRecordChangesResponseV1, workspace_changes_frame_v1,
};

use super::pr_diff::{Kind, Op, diff_lines, split_lines, write_file_header, write_hunks_ops};
use super::store::StoreCallError;

pub(crate) const MAX_FILES: usize = 1_000;
pub(crate) const MAX_PATH_BYTES: usize = 4_096;
pub(crate) const MAX_BLOB_BYTES: usize = 1 << 20;
pub(crate) const MAX_MIDDLE_LINES: usize = 3_000;
pub(crate) const MAX_DIFFED_BYTES: u64 = 8 << 20;
pub(crate) const MAX_FILE_DIFF_BYTES: usize = 48 * 1_024;
pub(crate) const MAX_SCOPE_DIFF_BYTES: usize = 256 * 1_024;
const BINARY_SNIFF_BYTES: usize = 8_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ChangeStatus {
    Added,
    Modified,
    Deleted,
    Renamed,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ChangeScope {
    Turn,
    Cumulative,
}

/// One changed file of one scope.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct ChangedFile {
    pub(crate) path: String,
    /// `Renamed` only.
    pub(crate) from_path: Option<String>,
    pub(crate) status: ChangeStatus,
    pub(crate) additions: Option<u32>,
    pub(crate) deletions: Option<u32>,
    pub(crate) binary: bool,
    pub(crate) too_large: bool,
    /// Empty when none is kept.
    pub(crate) diff: String,
}

/// What a turn's published commit changed.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub(crate) struct ChangeSet {
    pub(crate) commit: String,
    pub(crate) parent: String,
    pub(crate) base: String,
    pub(crate) turn: Vec<ChangedFile>,
    pub(crate) turn_omitted: u32,
    pub(crate) cumulative: Vec<ChangedFile>,
    pub(crate) cumulative_omitted: u32,
}

/// The change set of `commit` in `db`: the turn from `parent`, the
/// cumulative set from `base` (the handoff snapshot).
///
/// # Errors
///
/// A commit, tree or blob is missing or broken.
pub(crate) fn compute(
    db: &ObjectDb,
    base: &str,
    parent: &str,
    commit: &str,
) -> Result<ChangeSet, StoreError> {
    let tree_of = |sha: &str| -> Result<BTreeMap<String, FileEntry>, StoreError> {
        db.flatten(&db.commit(&oid(sha)?)?.tree)
    };
    let after = tree_of(commit)?;
    let (turn, turn_omitted) = scope(db, &tree_of(parent)?, &after)?;
    let (cumulative, cumulative_omitted) = if base == parent {
        (turn.clone(), turn_omitted)
    } else {
        scope(db, &tree_of(base)?, &after)?
    };
    Ok(ChangeSet {
        commit: commit.to_owned(),
        parent: parent.to_owned(),
        base: base.to_owned(),
        turn,
        turn_omitted,
        cumulative,
        cumulative_omitted,
    })
}

/// A listed change before its content is read.
struct Listed {
    path: String,
    from_path: Option<String>,
    status: ChangeStatus,
    old: Option<FileEntry>,
    new: Option<FileEntry>,
}

/// The listed changes from `before` to `after` (renames paired, sorted by
/// path), then each one's content, under the scope's bounds.
fn scope(
    db: &ObjectDb,
    before: &BTreeMap<String, FileEntry>,
    after: &BTreeMap<String, FileEntry>,
) -> Result<(Vec<ChangedFile>, u32), StoreError> {
    let mut listed = pair_renames(diff(before, after));
    listed.sort_by(|a, b| a.path.cmp(&b.path));
    let mut omitted = 0_u32;
    let mut budget = Budget::default();
    let mut out = Vec::new();
    for change in listed {
        let too_long = change.path.len() > MAX_PATH_BYTES
            || change
                .from_path
                .as_ref()
                .is_some_and(|from| from.len() > MAX_PATH_BYTES);
        if too_long || out.len() >= MAX_FILES {
            omitted = omitted.saturating_add(1);
            continue;
        }
        out.push(content(db, &change, &mut budget)?);
    }
    Ok((out, omitted))
}

fn pair_renames(changes: Vec<elitea_workspace_store::objects::Change>) -> Vec<Listed> {
    let mut listed = Vec::new();
    // Deleted regular files by blob, each blob's paths in path order.
    let mut deleted: BTreeMap<gix_hash::ObjectId, Vec<String>> = BTreeMap::new();
    let mut added = Vec::new();
    for change in changes {
        let gitlink = change.old.is_some_and(|entry| entry.is_gitlink())
            || change.new.is_some_and(|entry| entry.is_gitlink());
        if gitlink {
            continue;
        }
        match (change.kind, change.old, change.new) {
            (ChangeKind::Deleted, Some(old), _) if !old.is_link() => {
                deleted.entry(old.oid).or_default().push(change.path);
            }
            (ChangeKind::Added, _, Some(new)) if !new.is_link() => added.push((change.path, new)),
            (kind, old, new) => listed.push(Listed {
                path: change.path,
                from_path: None,
                status: match kind {
                    ChangeKind::Added => ChangeStatus::Added,
                    ChangeKind::Modified => ChangeStatus::Modified,
                    ChangeKind::Deleted => ChangeStatus::Deleted,
                },
                old,
                new,
            }),
        }
    }
    for paths in deleted.values_mut() {
        paths.sort();
        paths.reverse();
    }
    added.sort_by(|a, b| a.0.cmp(&b.0));
    for (path, new) in added {
        if let Some(from) = deleted.get_mut(&new.oid).and_then(Vec::pop) {
            listed.push(Listed {
                path,
                from_path: Some(from),
                status: ChangeStatus::Renamed,
                old: None,
                new: Some(new),
            });
        } else {
            listed.push(Listed {
                path,
                from_path: None,
                status: ChangeStatus::Added,
                old: None,
                new: Some(new),
            });
        }
    }
    for (oid, paths) in deleted {
        for path in paths {
            listed.push(Listed {
                path,
                from_path: None,
                status: ChangeStatus::Deleted,
                old: Some(FileEntry {
                    oid,
                    mode: 0o100_644,
                }),
                new: None,
            });
        }
    }
    listed
}

#[derive(Default)]
struct Budget {
    diffed: u64,
    kept: usize,
}

fn bare(change: &Listed) -> ChangedFile {
    ChangedFile {
        path: change.path.clone(),
        from_path: change.from_path.clone(),
        status: change.status,
        additions: None,
        deletions: None,
        binary: false,
        too_large: false,
        diff: String::new(),
    }
}

fn content(db: &ObjectDb, change: &Listed, budget: &mut Budget) -> Result<ChangedFile, StoreError> {
    let mut file = bare(change);
    if change.status == ChangeStatus::Renamed {
        // The same blob moved: nothing to count.
        file.additions = Some(0);
        file.deletions = Some(0);
        return Ok(file);
    }
    if change.old.is_some_and(|entry| entry.is_link()) || change.new.is_some_and(|e| e.is_link()) {
        file.binary = true;
        return Ok(file);
    }
    if budget.diffed >= MAX_DIFFED_BYTES {
        file.too_large = true;
        return Ok(file);
    }
    let mut read = |entry: Option<FileEntry>| -> Result<Vec<u8>, StoreError> {
        let bytes = match entry {
            Some(entry) => db.blob(&entry.oid)?,
            None => Vec::new(),
        };
        budget.diffed = budget.diffed.saturating_add(bytes.len() as u64);
        Ok(bytes)
    };
    let old = read(change.old)?;
    let new = read(change.new)?;
    if old.len() > MAX_BLOB_BYTES || new.len() > MAX_BLOB_BYTES {
        file.too_large = true;
        return Ok(file);
    }
    let sniff = |bytes: &[u8]| bytes[..bytes.len().min(BINARY_SNIFF_BYTES)].contains(&0);
    if sniff(&old) || sniff(&new) {
        file.binary = true;
        return Ok(file);
    }
    let (old, new) = (String::from_utf8_lossy(&old), String::from_utf8_lossy(&new));
    let Some(rendered) = render(
        &change.path,
        change.old.is_none(),
        change.new.is_none(),
        &old,
        &new,
    ) else {
        file.too_large = true;
        return Ok(file);
    };
    file.additions = Some(rendered.additions);
    file.deletions = Some(rendered.deletions);
    // A NUL past the binary sniff cannot be stored as text: no diff, the
    // counts stay.
    if !rendered.diff.is_empty()
        && !rendered.diff.contains('\0')
        && rendered.diff.len() <= MAX_FILE_DIFF_BYTES
        && budget.kept + rendered.diff.len() <= MAX_SCOPE_DIFF_BYTES
    {
        budget.kept += rendered.diff.len();
        file.diff = rendered.diff;
    }
    Ok(file)
}

struct Rendered {
    additions: u32,
    deletions: u32,
    /// Empty for a change with no line changed (a mode-only change).
    diff: String,
}

/// The line diff of one text file; `None` when its trimmed middle is over
/// [`MAX_MIDDLE_LINES`].
fn render(path: &str, added: bool, deleted: bool, old: &str, new: &str) -> Option<Rendered> {
    let (a, b) = (split_lines(old), split_lines(new));
    let ops = trimmed_ops(&a, &b)?;
    let count = |kind: Kind| {
        u32::try_from(ops.iter().filter(|op| op.kind == kind).count()).unwrap_or(u32::MAX)
    };
    let (additions, deletions) = (count(Kind::Insert), count(Kind::Delete));
    let mut diff = String::new();
    if additions + deletions > 0 || added || deleted {
        write_file_header(&mut diff, path, added, deleted);
        write_hunks_ops(&mut diff, &ops, &a, &b);
    }
    Some(Rendered {
        additions,
        deletions,
        diff,
    })
}

/// Myers over what lies between the common prefix and suffix only, with
/// every op's index into the whole files.
fn trimmed_ops(a: &[super::pr_diff::Line<'_>], b: &[super::pr_diff::Line<'_>]) -> Option<Vec<Op>> {
    let prefix = a.iter().zip(b).take_while(|(x, y)| x == y).count();
    let suffix = a[prefix..]
        .iter()
        .rev()
        .zip(b[prefix..].iter().rev())
        .take_while(|(x, y)| x == y)
        .count();
    let (a_mid, b_mid) = (&a[prefix..a.len() - suffix], &b[prefix..b.len() - suffix]);
    if a_mid.len() + b_mid.len() > MAX_MIDDLE_LINES {
        return None;
    }
    let equal = |index: usize, offset_b: usize| Op {
        kind: Kind::Equal,
        old: Some(index),
        new: Some(offset_b),
    };
    let mut ops: Vec<Op> = (0..prefix).map(|index| equal(index, index)).collect();
    let middle = if a_mid.is_empty() || b_mid.is_empty() {
        // One side is empty: no search.
        let deletes = (0..a_mid.len()).map(|index| Op {
            kind: Kind::Delete,
            old: Some(index),
            new: None,
        });
        let inserts = (0..b_mid.len()).map(|index| Op {
            kind: Kind::Insert,
            old: None,
            new: Some(index),
        });
        deletes.chain(inserts).collect()
    } else {
        diff_lines(a_mid, b_mid)
    };
    ops.extend(middle.into_iter().map(|op| Op {
        kind: op.kind,
        old: op.old.map(|index| index + prefix),
        new: op.new.map(|index| index + prefix),
    }));
    let (a_end, b_end) = (a.len() - suffix, b.len() - suffix);
    ops.extend((0..suffix).map(|index| equal(a_end + index, b_end + index)));
    Some(ops)
}

/// The record stream's frames: the header, then the turn's files, then the
/// cumulative ones.
#[must_use]
pub(crate) fn frames(claim: &WorkspaceClaimV1, set: &ChangeSet) -> Vec<WorkspaceChangesFrameV1> {
    use workspace_changes_frame_v1::Frame;
    let mut frames = vec![WorkspaceChangesFrameV1 {
        frame: Some(Frame::Header(WorkspaceChangesHeaderV1 {
            claim: Some(claim.clone()),
            commit: set.commit.clone(),
            parent: set.parent.clone(),
            base: set.base.clone(),
            turn_omitted: set.turn_omitted,
            cumulative_omitted: set.cumulative_omitted,
        })),
    }];
    let scoped = set.turn.iter().map(|file| (ChangeScope::Turn, file)).chain(
        set.cumulative
            .iter()
            .map(|file| (ChangeScope::Cumulative, file)),
    );
    for (scope, file) in scoped {
        frames.push(WorkspaceChangesFrameV1 {
            frame: Some(Frame::File(file_wire(scope, file))),
        });
    }
    frames
}

fn file_wire(scope: ChangeScope, file: &ChangedFile) -> WorkspaceChangedFileV1 {
    WorkspaceChangedFileV1 {
        scope: match scope {
            ChangeScope::Turn => WorkspaceChangeScopeV1::Turn,
            ChangeScope::Cumulative => WorkspaceChangeScopeV1::Cumulative,
        } as i32,
        path: file.path.clone(),
        from_path: file.from_path.clone().unwrap_or_default(),
        status: match file.status {
            ChangeStatus::Added => WorkspaceChangeStatusV1::Added,
            ChangeStatus::Modified => WorkspaceChangeStatusV1::Modified,
            ChangeStatus::Deleted => WorkspaceChangeStatusV1::Deleted,
            ChangeStatus::Renamed => WorkspaceChangeStatusV1::Renamed,
        } as i32,
        additions: file.additions,
        deletions: file.deletions,
        binary: file.binary,
        too_large: file.too_large,
        diff: file.diff.clone(),
    }
}

/// Read a record-changes response: recorded, or exactly one refusal or
/// rejection.
///
/// # Errors
///
/// The refusal or rejection, or an ambiguous response.
pub(crate) fn read_record_changes(
    response: &WorkspaceRecordChangesResponseV1,
) -> Result<(), StoreCallError> {
    match (&response.refusal, &response.rejection, response.recorded) {
        (Some(refusal), None, false) => Err(super::store::refused(refusal)),
        (None, Some(rejection), false) => Err(super::store::rejected(rejection)),
        (None, None, true) => Ok(()),
        _ => Err(StoreCallError::Malformed(
            "the record-changes response is ambiguous",
        )),
    }
}

/// The handoff snapshot of `refs`: its single `refs/elitea/handoff/*` ref.
#[must_use]
pub(crate) fn handoff_snapshot(refs: &BTreeMap<String, String>) -> Option<String> {
    let mut snapshots = refs
        .iter()
        .filter(|(name, _)| name.starts_with(super::store::REF_HANDOFF_PREFIX));
    match (snapshots.next(), snapshots.next()) {
        (Some((_, sha)), None) => Some(sha.clone()),
        _ => None,
    }
}

#[cfg(test)]
#[path = "changes_tests.rs"]
mod tests;
