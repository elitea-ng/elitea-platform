//! Take-back (ADR-0034 decision 13): a preflight with no side effects,
//! then apply (the fast path or a clean merge), apply with conflict
//! markers, a worktree, or discard. The merge itself is the shared apply
//! engine ([`crate::apply`]), the one the worktree merge-back uses.
//!
//! **Preflight** ([`preflight`], [`preflight_up_to`], [`preflight_chain`]):
//!
//! 1. the fetched chain (a `COMPACT`, then the `PUSH` segments after it) is
//!    verified into a quarantine of its own (content addresses, every
//!    `PUSH`'s prerequisites present before its pack is indexed, packs,
//!    connectivity from every ref; the refs must be the plan's), before
//!    anything else is read from it;
//! 2. the cloud chain `H_1..C` is read (first parents from `C` back to the
//!    `H_1` this desktop sent; anything else is refused); the person may
//!    take back only up to turn `k` (`C_k`; the latest by default);
//! 3. the folder is snapshotted as `L` with the send's exclusions, in a
//!    temporary index (objects only: the folder, `HEAD`, the index and every
//!    ref stay byte-identical);
//! 4. the engine merges `(tree(H_1), L, tree(C_k))` in a scratch object
//!    directory over the repository and the quarantine: `fast_path` when
//!    `L` is the sent tree, `clean_merge` when both sides changed other
//!    lines or files, `conflicts` otherwise (with the conflicting paths).
//!    Everything is tree-based: a `HEAD` that moved since the send (the
//!    person committed) is the same algorithm.
//!
//! **The remaining turns** ([`Take::Rest`]): after a take-back up to turn
//! `k`, the turns `k+1..N` are taken back the same way, merging
//! `(tree(C_k), L, tree(C_N))`: the folder already holds `C_k`'s work, so
//! only what the later turns changed is applied. Their commits are replayed
//! onto `C'_k` (the first take-back's replayed `C_k`) and kept on
//! `elitea/cloud/<thread>-turns-<k+1>-<N>`.
//!
//! **Apply** ([`apply`]) first takes `L` (and `HEAD`) again and refuses
//! with `folder_changed_since_preflight` ([`ErrorCode::StaleRead`]) if it
//! moved. Then, by mode:
//!
//! * `apply` (`fast_path`, `clean_merge`) and `apply_with_markers`
//!   (`conflicts`): a checkpoint of the folder (so restoring it undoes the
//!   take-back), the segment's objects imported into the repository
//!   (`index-pack`), the cloud commits up to `C_k` replayed onto `H'_1` and
//!   kept on `elitea/cloud/<thread>`, and the merge's result written
//!   through the confined workspace writer: with markers, conflicted text
//!   files carry `merge-tree`'s conflict markers, and a binary conflict
//!   keeps the folder's file (`binary_conflict`). `HEAD` and the index are
//!   untouched: the cloud's work lands as uncommitted changes.
//! * `worktree`: the same branch, registered as a worktree beside the folder
//!   (`worktree add --no-checkout`, its index read from the branch, its
//!   files written by the confined writer: no checkout filter runs); the
//!   folder is untouched. A folder without a repository gets a plain copy.
//! * `discard`: nothing is written.
//!
//! Excluded paths (the send's `excluded[]`, the exclusion rules,
//! `path_deny`) are never written or deleted, nor are symlinks, submodule
//! entries or anything under a `.git`: all reported in `skipped_paths`
//! with their reason.

use std::collections::BTreeMap;
use std::ffi::OsStr;
use std::path::{Path, PathBuf};

use elitea_workspace_store::bundle::{self as segment, Limits, VerifiedBundle};
use elitea_workspace_store::chain::{self as store_chain, ChainSegment};
use elitea_workspace_store::objects::{self as store_objects, ChangeKind, FileEntry, ObjectDb};
use elitea_workspace_store::refs as store_refs;
use serde::{Deserialize, Serialize};

use super::exclude::{Exclusions, Reason, Rules};
use super::snapshot::{Source, TreeLine, list_tree, snapshot_tree};
use super::{SendRecord, TempDir, check_conversation, short_thread};
use crate::apply::{self as engine, Entry, Objects, Plan, Trees};
pub use crate::apply::{ApplyOutcome, Change, ChangedPath, Outcome, SkipReason, SkippedPath};
use crate::checkpoint::Checkpoints;
use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::workspace::{Workspace, WsPath, is_protected_name};

/// The most cloud commits one take-back replays.
pub const MAX_CLOUD_TURNS: usize = 1000;

/// The machine code of a folder that changed after its preflight.
pub const FOLDER_CHANGED: &str = "folder_changed_since_preflight";

/// How a take-back is applied.
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Mode {
    /// Write the result into the folder (`fast_path`, `clean_merge`).
    Apply,
    /// Write the result with conflict markers (`conflicts`).
    ApplyWithMarkers,
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

/// What a preflight takes back.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Take {
    /// The cloud turns up to `k` (1-based; `None`: all of them).
    UpTo(Option<usize>),
    /// The turns after `after_turn`: an earlier take-back took back up to
    /// it, and `replayed` is that take-back's replayed `C'_k` (the tip of
    /// its `elitea/cloud/<thread>` branch).
    Rest { after_turn: usize, replayed: String },
}

/// A preflight, held until the person picks a mode.
pub struct Preflight {
    /// The engine's outcome (`applied` false): what applying writes, what
    /// conflicts, what is left alone and why. `base` is `H'_1`.
    pub report: ApplyOutcome,
    /// Every cloud commit, oldest first (the person may take back fewer).
    pub chain: Vec<CloudCommit>,
    /// `k`: the turns taken back (`chain[from_turn..k]`; `chain.len()` by
    /// default).
    pub up_to_turn: usize,
    /// The turns an earlier take-back already took back (0, or the `k` of
    /// [`Take::Rest`]).
    pub from_turn: usize,
    /// `C`, the cloud tip (`H_1` when no cloud turn changed anything).
    pub cloud_tip: String,
    /// `C_k`, the commit whose tree is taken back.
    pub target: String,
    /// `L` and `HEAD` as the preflight saw them.
    local_tree: String,
    head: Option<String>,
    /// The commit whose tree is the merge's base: `H_1`, or `C_k` for the
    /// remaining turns.
    base_commit: String,
    /// `C'_k`, the replay's start for the remaining turns.
    replay_onto: Option<String>,
    plan: Plan,
    objects: Objects,
    /// The verified chain. Its packs stay indexed in the quarantine (one
    /// `.pack` per segment), which the import reads them from: the preflight
    /// holds no copy of the fetched bytes.
    bundle: VerifiedBundle,
    record: SendRecord,
    _scratch: TempDir,
    _quarantine: TempDir,
}

impl std::fmt::Debug for Preflight {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Preflight")
            .field("report", &self.report)
            .field("up_to_turn", &self.up_to_turn)
            .field("from_turn", &self.from_turn)
            .field("cloud_tip", &self.cloud_tip)
            .field("target", &self.target)
            .finish_non_exhaustive()
    }
}

impl Preflight {
    /// Cloud turns (commits after `H_1`).
    #[must_use]
    pub fn cloud_turns(&self) -> usize {
        self.chain.len()
    }

    /// The modes [`apply`] accepts for this preflight: `apply` for
    /// `fast_path` and `clean_merge`, `apply_with_markers` for `conflicts`;
    /// `worktree` and `discard` always.
    #[must_use]
    pub fn modes(&self) -> Vec<Mode> {
        match self.report.outcome {
            Outcome::FastPath | Outcome::CleanMerge => {
                vec![Mode::Apply, Mode::Worktree, Mode::Discard]
            }
            Outcome::Conflicts => vec![Mode::ApplyWithMarkers, Mode::Worktree, Mode::Discard],
            Outcome::Error => vec![Mode::Discard],
        }
    }

    /// What take-back writes, file by file: the folder's and the result's
    /// content of every path written or deleted (`None` where a side has no
    /// file, or the file is larger than `max_file_bytes`, then `too_large`
    /// is set). For `conflicts`, the result is what applying with markers
    /// writes.
    ///
    /// # Errors
    ///
    /// An unreadable object.
    pub fn changed_files(&self, max_file_bytes: u64) -> ToolResult<Vec<(FileDiff, bool)>> {
        let oids: Vec<&str> = self
            .plan
            .writes
            .iter()
            .flat_map(|write| write.old.iter().chain(write.new.iter()))
            .map(|entry| entry.oid.as_str())
            .collect();
        let blobs = engine::read_blobs(&self.objects, &oids, max_file_bytes)?;
        let mut out = Vec::new();
        for write in &self.plan.writes {
            let mut too_large = false;
            let mut read = |entry: Option<&Entry>| {
                let entry = entry?;
                let found = blobs.get(&entry.oid).cloned();
                too_large |= found.is_none();
                found
            };
            let before = read(write.old.as_ref());
            let after = read(write.new.as_ref());
            let kind = match (&write.old, &write.new) {
                (None, _) => ChangeKind::Added,
                (_, None) => ChangeKind::Deleted,
                _ => ChangeKind::Modified,
            };
            out.push((
                FileDiff {
                    path: write.path.clone(),
                    kind,
                    before,
                    after,
                },
                too_large,
            ));
        }
        Ok(out)
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
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct Applied {
    /// The preflight's outcome, `applied` set when the folder (or the
    /// worktree) was written.
    pub report: ApplyOutcome,
    /// Files were written to the folder (or the worktree).
    pub applied: bool,
    /// The checkpoint taken before the folder was written (`apply` only).
    pub checkpoint: Option<u64>,
    pub worktree_path: Option<PathBuf>,
    /// `elitea/cloud/<thread>`, the replayed cloud commits.
    pub branch: Option<String>,
    /// `elitea/cloud/<thread>-after-turn-<k>`: the cloud turns after `k`
    /// when the person took back fewer than all (kept as commits only; they
    /// are not part of the take-back).
    pub rest_branch: Option<String>,
    /// The replayed commits, oldest first (`H'_1` descendants).
    pub replayed: Vec<String>,
    pub written: Vec<String>,
    pub deleted: Vec<String>,
    /// `worktree`: the folder's excluded files copied into it.
    pub copied: Vec<String>,
}

impl Applied {
    fn nothing(report: &ApplyOutcome) -> Self {
        Self {
            report: report.clone(),
            applied: false,
            checkpoint: None,
            worktree_path: None,
            branch: None,
            rest_branch: None,
            replayed: Vec::new(),
            written: Vec::new(),
            deleted: Vec::new(),
            copied: Vec::new(),
        }
    }
}

fn flatten_commit(db: &ObjectDb, sha: &str) -> ToolResult<BTreeMap<String, FileEntry>> {
    let id = store_objects::oid(sha).map_err(|e| super::store_error(&e))?;
    let commit = db.commit(&id).map_err(|e| super::store_error(&e))?;
    db.flatten(&commit.tree).map_err(|e| super::store_error(&e))
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
    Exclusions::new(
        &workspace,
        &Rules {
            patterns: record.patterns.clone().unwrap_or_default(),
            path_deny: path_deny.to_vec(),
            leave_on_laptop: leave,
            // What the send included is part of the snapshot, as sent.
            include: record.included.clone(),
        },
    )
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

/// The engine's verdict for one path the merge changes in the folder: the
/// [`writable`] rules, as skip reasons.
fn decide(
    top: &Path,
    exclusions: &Exclusions,
    path: &str,
    new: Option<&Entry>,
    added: bool,
) -> Result<String, SkipReason> {
    match exclusions.reason(path) {
        None => Ok(path.to_owned()),
        Some(Reason::PathDeny) => Err(SkipReason::PathDeny),
        Some(Reason::NotAFile) => Err(SkipReason::NotAFile),
        // An excluded file the cloud created: only where none is local.
        Some(_) if added && new.is_some() && !present(top, path) => Ok(path.to_owned()),
        Some(_) => Err(SkipReason::Excluded),
    }
}

fn skip_reason(reason: Reason) -> SkipReason {
    match reason {
        Reason::PathDeny => SkipReason::PathDeny,
        Reason::NotAFile => SkipReason::NotAFile,
        _ => SkipReason::Excluded,
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

/// `k` (1-based; `None`: the latest) and `C_k` (`H_1` when there is no
/// cloud turn).
fn pick_turn(
    chain: &[CloudCommit],
    up_to_turn: Option<usize>,
    handoff: &str,
) -> ToolResult<(usize, String)> {
    let turns = chain.len();
    let k = match up_to_turn {
        None => turns,
        Some(k) if k <= turns && (k >= 1 || turns == 0) => k,
        Some(k) => {
            return Err(ToolError::invalid(format!(
                "turn_out_of_range: this thread has {turns} cloud turns; take back up to a turn from 1 to {turns}, not {k}"
            )));
        }
    };
    let target = match k.checked_sub(1) {
        Some(last) => chain[last].sha.clone(),
        None => handoff.to_owned(),
    };
    Ok((k, target))
}

/// What the send left on the laptop, and what `path_deny` blocks now that
/// the cloud changed, are never written or deleted here: listed with the
/// skipped paths (one entry per path, the merge's reason first).
fn add_record_exclusions(report: &mut ApplyOutcome, record: &SendRecord, denied: &[String]) {
    let mut skipped: BTreeMap<String, SkipReason> = report
        .skipped_paths
        .iter()
        .map(|skip| (skip.path.clone(), skip.reason))
        .collect();
    for excluded in &record.excluded {
        skipped
            .entry(excluded.path.clone())
            .or_insert_with(|| skip_reason(excluded.reason));
    }
    for path in denied {
        skipped.entry(path.clone()).or_insert(SkipReason::PathDeny);
    }
    report.skipped_paths = skipped
        .into_iter()
        .map(|(path, reason)| SkippedPath { path, reason })
        .collect();
}

/// `tree` without the paths `path_deny` blocks now (read and written in the
/// preflight's scratch objects); the tree, and the dropped entries by path
/// (`<mode> <type> <id>`). The folder's snapshot `L` leaves them out, so
/// the base and the cloud's tree must too: a path denied since the send
/// would otherwise read as deleted locally and conflict with the cloud's
/// change.
fn without_denied(
    objects: &Objects,
    tree: &str,
    exclusions: &Exclusions,
) -> ToolResult<(String, BTreeMap<String, String>)> {
    let listed = objects
        .git()
        .run(&["ls-tree", "-r", "-z", "--full-tree", tree])?;
    let mut dropped = BTreeMap::new();
    for field in listed.split(|byte| *byte == 0).filter(|f| !f.is_empty()) {
        let text = String::from_utf8_lossy(field);
        let Some((meta, path)) = text.split_once('\t') else {
            continue;
        };
        if matches!(exclusions.reason(path), Some(Reason::PathDeny)) {
            dropped.insert(path.to_owned(), meta.to_owned());
        }
    }
    if dropped.is_empty() {
        return Ok((tree.to_owned(), dropped));
    }
    let index = TempDir::new("deny")?;
    let index_file = index.path().join("index");
    let dirs = [index.path().to_path_buf(), objects.written_dir()];
    let git = || objects.git().env("GIT_INDEX_FILE", index_file.as_os_str());
    git().writes(&dirs).run(&["read-tree", tree])?;
    let mut info = Vec::new();
    for path in dropped.keys() {
        // Mode 0 removes the entry.
        info.extend_from_slice(
            format!("0 0000000000000000000000000000000000000000\t{path}").as_bytes(),
        );
        info.push(0);
    }
    git()
        .writes(&dirs)
        .stdin(&info)
        .run(&["update-index", "-z", "--index-info"])?;
    let filtered = git().writes(&dirs).text(&["write-tree"])?;
    Ok((filtered, dropped))
}

/// Verify a fetched segment, read the cloud chain, snapshot the folder,
/// and merge, taking back every cloud turn. Writes nothing to the folder,
/// `HEAD`, the index or any ref.
///
/// # Errors
///
/// As [`preflight_up_to`].
pub fn preflight(
    source: &Source,
    record: &SendRecord,
    bytes: &[u8],
    plan_refs: &BTreeMap<String, String>,
    path_deny: &[String],
) -> ToolResult<Preflight> {
    preflight_up_to(source, record, bytes, plan_refs, path_deny, None)
}

/// [`preflight`] taking back the cloud turns up to `up_to_turn` (1-based;
/// `None`: all of them), from one `COMPACT` segment.
///
/// # Errors
///
/// As [`preflight_chain`].
pub fn preflight_up_to(
    source: &Source,
    record: &SendRecord,
    bytes: &[u8],
    plan_refs: &BTreeMap<String, String>,
    path_deny: &[String],
    up_to_turn: Option<usize>,
) -> ToolResult<Preflight> {
    let sha256 = elitea_workspace_store::digest::sha256_hex(bytes);
    let segments = [ChainSegment {
        seq: 1,
        kind: elitea_workspace_store::KIND_COMPACT,
        sha256: &sha256,
        prerequisites: &[],
        refs: plan_refs,
        bytes,
    }];
    preflight_chain(
        source,
        record,
        &segments,
        path_deny,
        &Take::UpTo(up_to_turn),
    )
}

/// The preflight over a fetched chain (a `COMPACT`, then `PUSH` segments,
/// as [`store_chain::plan_segments`] pairs a plan with its bodies), taking
/// back what `take` says.
///
/// # Errors
///
/// A chain that fails verification, a cloud tip that does not descend from
/// the `H_1` this desktop sent, [`ErrorCode::InvalidArgument`]
/// (`turn_out_of_range`) for a turn the chain does not have (or, for the
/// remaining turns, no turn after `k`), git failures.
#[allow(
    clippy::too_many_lines,
    reason = "the preflight in order: verify, read the chain, snapshot, merge"
)]
pub fn preflight_chain(
    source: &Source,
    record: &SendRecord,
    segments: &[ChainSegment<'_>],
    path_deny: &[String],
    take: &Take,
) -> ToolResult<Preflight> {
    check_conversation(&record.conversation_uuid)?;
    let quarantine = TempDir::new("quarantine")?;
    let git_dir = quarantine.path().join("git");
    segment::init_quarantine(&git_dir).map_err(|e| super::store_error(&e))?;
    let bundle = store_chain::verify_chain_into(segments, &git_dir, Limits::default())
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
    let (from_turn, up_to_turn, target, replay_onto) = match take {
        Take::UpTo(k) => {
            let (k, target) = pick_turn(&chain, *k, &record.handoff)?;
            (0, k, target, None)
        }
        Take::Rest {
            after_turn,
            replayed,
        } => {
            if *after_turn == 0 || *after_turn >= chain.len() {
                return Err(ToolError::invalid(format!(
                    "turn_out_of_range: this thread has {} cloud turns; there are none after turn {after_turn} to take back",
                    chain.len()
                )));
            }
            store_objects::oid(replayed).map_err(|e| super::store_error(&e))?;
            let (k, target) = pick_turn(&chain, None, &record.handoff)?;
            (*after_turn, k, target, Some(replayed.clone()))
        }
    };
    let base_commit = match from_turn.checked_sub(1) {
        Some(last) => chain[last].sha.clone(),
        None => record.handoff.clone(),
    };
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
    let base_tree = bundle
        .db
        .commit(&store_objects::oid(&base_commit).map_err(|e| super::store_error(&e))?)
        .map_err(|e| super::store_error(&e))?
        .tree
        .to_string();
    let target_tree = bundle
        .db
        .commit(&store_objects::oid(&target).map_err(|e| super::store_error(&e))?)
        .map_err(|e| super::store_error(&e))?
        .tree
        .to_string();
    let exclusions = exclusions_of(source, record, path_deny)?;
    let local = local_tree(source, &exclusions)?;
    let head = source.head();
    // The merge's objects go to a scratch directory, never the repository.
    let scratch = TempDir::new("merge")?;
    let objects = Objects::scratch(&source.repo, scratch.path(), &[git_dir.join("objects")]);
    let top = source.top();
    let verdict = |path: &str, new: Option<&Entry>, old: Option<&Entry>| {
        decide(top, &exclusions, path, new, old.is_none())
    };
    let (base_tree, base_denied) = without_denied(&objects, &base_tree, &exclusions)?;
    let (theirs_tree, theirs_denied) = without_denied(&objects, &target_tree, &exclusions)?;
    // A denied path the cloud changed is a skip, not a change or a conflict.
    let denied: Vec<String> = base_denied
        .keys()
        .chain(theirs_denied.keys())
        .filter(|path| base_denied.get(*path) != theirs_denied.get(*path))
        .cloned()
        .collect();
    let mut plan = engine::plan(
        &objects,
        Trees {
            base: &base_tree,
            ours: &local,
            theirs: &theirs_tree,
            ours_label: "your folder",
            theirs_label: "cloud",
        },
        replay_onto.as_deref().unwrap_or(&record.twin),
        &verdict,
    )?;
    add_record_exclusions(&mut plan.outcome, record, &denied);
    Ok(Preflight {
        report: plan.outcome.clone(),
        chain,
        up_to_turn,
        from_turn,
        cloud_tip: tip.to_string(),
        target,
        local_tree: local,
        head,
        base_commit,
        replay_onto,
        plan,
        objects,
        bundle,
        record: record.clone(),
        _scratch: scratch,
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

/// The `.pack` files the chain's verification indexed into `quarantine`
/// (one per segment), in a stable order. None of them is thin: each holds
/// its own deltas' bases, so the order they are imported in is free.
fn quarantine_packs(quarantine: &Path) -> ToolResult<Vec<PathBuf>> {
    let mut packs: Vec<PathBuf> = std::fs::read_dir(quarantine.join("objects/pack"))
        .map_err(|error| ToolError::io("cannot read the verified packs", &error))?
        .filter_map(Result::ok)
        .map(|entry| entry.path())
        .filter(|path| path.extension().is_some_and(|ext| ext == "pack"))
        .collect();
    packs.sort();
    Ok(packs)
}

/// Import the segment's objects and replay the cloud chain onto `H'_1`
/// (each tree with the excluded paths as `HEAD` had them,
/// [`graft_excluded`]); the branch name and the replayed commits.
fn import_and_replay(
    source: &Source,
    preflight: &Preflight,
    exclusions: &Exclusions,
) -> ToolResult<Replayed> {
    let objects = source.objects();
    for path in quarantine_packs(&preflight.bundle.quarantine)? {
        // One segment's pack at a time, as verified into the quarantine.
        let pack = std::fs::read(&path)
            .map_err(|error| ToolError::io("cannot read a verified pack", &error))?;
        source
            .repo
            .git(source.top())
            .writes(std::slice::from_ref(&objects))
            .stdin(&pack)
            .run(&["index-pack", "--stdin"])?;
    }
    let head_excluded = head_excluded(source, &preflight.record, exclusions)?;
    let mut previous = preflight.record.twin.clone();
    if !head_excluded.is_empty() {
        // The twin, with HEAD's excluded entries back.
        let tree = graft_excluded(source, &preflight.record.tree, &head_excluded, exclusions)?;
        if tree != preflight.record.tree
            && let Some(head) = &preflight.record.head
        {
            previous = commit_as_cloud(
                source,
                &tree,
                head,
                &format!(
                    "Elitea handoff (local twin)\n\nElitea-Handoff-Twin: {}",
                    preflight.record.handoff
                ),
            )?;
        }
    }
    // The remaining turns go onto `C'_k` while the repository still has
    // it; otherwise every turn is replayed again from the twin.
    let mut skip = 0;
    if let Some(onto) = &preflight.replay_onto
        && source
            .repo
            .git(source.top())
            .text(&[
                "rev-parse",
                "--verify",
                "--quiet",
                &format!("{onto}^{{commit}}"),
            ])
            .is_ok_and(|sha| sha == *onto)
    {
        previous.clone_from(onto);
        skip = preflight.from_turn;
    }
    let mut replayed = Vec::new();
    // Every turn is replayed: those after `k` go on a branch of their own,
    // so taking back fewer turns never loses the others' commits.
    let mut tip = previous.clone();
    for commit in preflight.chain.iter().skip(skip) {
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
        previous = commit_as_cloud(source, &tree, &previous, &message)?;
        replayed.push(previous.clone());
        if skip + replayed.len() == preflight.up_to_turn {
            tip.clone_from(&previous);
        }
    }
    let short = short_thread(&preflight.record.conversation_uuid);
    if preflight.from_turn > 0 {
        // The remaining turns: only theirs are reported and branched.
        let commits = replayed.split_off(preflight.from_turn - skip);
        let branch = keep_branch(
            source,
            &format!(
                "elitea/cloud/{short}-turns-{}-{}",
                preflight.from_turn + 1,
                preflight.up_to_turn
            ),
            &tip,
        )?;
        return Ok(Replayed {
            branch,
            commits,
            rest_branch: None,
        });
    }
    let rest = replayed.split_off(preflight.up_to_turn);
    let branch = keep_branch(source, &format!("elitea/cloud/{short}"), &tip)?;
    let rest_branch = match rest.last() {
        Some(last) => Some(keep_branch(
            source,
            &format!("elitea/cloud/{short}-after-turn-{}", preflight.up_to_turn),
            last,
        )?),
        None => None,
    };
    Ok(Replayed {
        branch,
        commits: replayed,
        rest_branch,
    })
}

/// A commit of `tree` on `parent` in the repository, by the cloud executor.
fn commit_as_cloud(source: &Source, tree: &str, parent: &str, message: &str) -> ToolResult<String> {
    let name = OsStr::new("Elitea cloud executor");
    let email = OsStr::new("cloud@elitea.invalid");
    source
        .repo
        .git(source.top())
        .env("GIT_AUTHOR_NAME", name)
        .env("GIT_AUTHOR_EMAIL", email)
        .env("GIT_COMMITTER_NAME", name)
        .env("GIT_COMMITTER_EMAIL", email)
        .writes(&[source.objects()])
        .text(&[
            "commit-tree",
            "--no-gpg-sign",
            tree,
            "-p",
            parent,
            "-m",
            message,
        ])
}

/// What [`import_and_replay`] made.
struct Replayed {
    /// `elitea/cloud/<thread>`: the turns taken back.
    branch: String,
    /// Their replayed commits, oldest first.
    commits: Vec<String>,
    /// The turns after `k`, when the person took back fewer than all.
    rest_branch: Option<String>,
}

/// Point a free branch named `name` (or `name-2`, `name-3`, …; one already
/// at `tip` is reused) at `tip`; its name.
fn keep_branch(source: &Source, name: &str, tip: &str) -> ToolResult<String> {
    let heads = source.common_dir().join("refs/heads/elitea/cloud");
    std::fs::create_dir_all(&heads)
        .map_err(|error| ToolError::io("cannot create the cloud branch", &error))?;
    let mut branch = name.to_owned();
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
        .is_ok_and(|sha| !sha.is_empty() && sha != tip)
    {
        branch = format!("{name}-{n}");
        n += 1;
    }
    source
        .repo
        .git(source.top())
        .writes(std::slice::from_ref(&heads))
        .run(&["update-ref", &format!("refs/heads/{branch}"), tip])?;
    Ok(branch)
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
/// for a mode the outcome does not offer ([`Preflight::modes`]: `apply`
/// with conflicts, `apply_with_markers` without); git or I/O failures.
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
    if !preflight.modes().contains(&mode) {
        return Err(ToolError::invalid(match mode {
            Mode::Apply => {
                "merge_conflicts: the cloud and the folder changed the same lines; open the cloud work as a worktree, apply it with conflict markers, or discard it"
            }
            _ => "nothing conflicts: apply the take-back without conflict markers",
        }));
    }
    match mode {
        Mode::Discard => Ok(Applied::nothing(&preflight.report)),
        Mode::Apply | Mode::ApplyWithMarkers => {
            apply_folder(source, preflight, &exclusions, checkpoints)
        }
        Mode::Worktree => apply_worktree(source, preflight, &exclusions),
    }
}

/// `apply`, `apply_with_markers`: the merge's result into the folder.
fn apply_folder(
    source: &Source,
    preflight: &Preflight,
    exclusions: &Exclusions,
    checkpoints: Option<&Checkpoints>,
) -> ToolResult<Applied> {
    let workspace = Workspace::open(source.top(), &[])?;
    // An excluded file the cloud created is written only where none is
    // local: one that appeared since the preview (`L` leaves excluded files
    // out, so the check above does not see it) is never overwritten.
    if let Some(write) = preflight.plan.writes.iter().find(|write| {
        write.new.is_some()
            && exclusions.reason(&write.path).is_some()
            && present(source.top(), &write.path)
    }) {
        return Err(ToolError::new(
            ErrorCode::StaleRead,
            format!(
                "{FOLDER_CHANGED}: `{}` is a local file the take-back would overwrite; preview the take-back again",
                write.path
            ),
        ));
    }
    engine::check_plan(&workspace, &preflight.plan)?;
    let checkpoint = match checkpoints {
        Some(checkpoints) => Some(
            checkpoints
                .create("before taking the thread back from the cloud")?
                .seq,
        ),
        None => None,
    };
    let Replayed {
        branch,
        commits: replayed,
        rest_branch,
    } = import_and_replay(source, preflight, exclusions)?;
    let done = engine::write_plan(&preflight.objects, &workspace, &preflight.plan)?;
    Ok(Applied {
        report: ApplyOutcome {
            applied: true,
            ..preflight.report.clone()
        },
        applied: true,
        checkpoint,
        branch: Some(branch),
        rest_branch,
        replayed,
        written: done.written,
        deleted: done.deleted,
        ..Applied::nothing(&preflight.report)
    })
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
    let Replayed {
        branch,
        commits: replayed,
        rest_branch,
    } = import_and_replay(source, preflight, exclusions)?;
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
        report: ApplyOutcome {
            applied: true,
            ..preflight.report.clone()
        },
        applied: true,
        worktree_path: Some(path),
        branch: Some(branch),
        rest_branch,
        replayed,
        ..Applied::nothing(&preflight.report)
    };
    // The person's own excluded files first (local → local), so the
    // worktree builds like the folder; a cloud file never overwrites one.
    applied.copied = copy_local_excluded(source, &workspace, exclusions)?;
    let cloud = flatten_commit(&preflight.bundle.db, &preflight.target)?;
    let sent = flatten_commit(&preflight.bundle.db, &preflight.base_commit)?;
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
