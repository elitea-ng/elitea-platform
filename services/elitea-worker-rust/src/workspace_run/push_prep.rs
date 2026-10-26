//! The commits a cloud `git_push` sends (ADR-0034 decision 10, amendment
//! 2026-10-26), built by the worker from the turn's own repository, outside
//! the sandbox.
//!
//! The thread's store history sits on an orphan base (history mode `none`):
//! B (the desktop's HEAD tree), the handoff snapshot H, then one commit per
//! cloud turn. None of those ids exist on the user's remote, so the history
//! is re-parented onto the recorded upstream commit R (`Elitea-Upstream` on
//! U): every commit whose tree differs from its predecessor's becomes a new
//! commit with R (then the previous new commit) as its parent, keeping its
//! tree, author and message (without the `Elitea-*` store trailers). The
//! workspace as it is now (pulled and redacted like an end-of-turn commit)
//! is one more commit when it differs. The pack carries what the remote does
//! not hold through R: U's tree is R's, so U's closure stands for R's.
//!
//! **What the handoff left out stays as the remote has it.** A tracked file
//! the send excluded (a secret, a pattern) or left out for size is in B
//! (HEAD's tree) but not in H, and so in no cloud workspace. Pushed as is,
//! H would delete it on the remote, and B would publish HEAD's version of
//! it. So every pushed tree carries, at each such path the cloud did not
//! write itself, B's entry when the remote already holds that object (it is
//! in R's tree, so U's closure has it) and otherwise R's own entry, or no
//! entry when R has none: nothing is deleted, and no left-out content the
//! remote lacks is uploaded. A path the cloud replaced by a directory (or
//! whose parent it made a file) cannot be kept, and the push is refused.

use std::collections::{BTreeMap, HashMap, HashSet};
use std::path::Path;

use elitea_agent_runtime::toolkits::workspace_pull_request::{
    PreparedPush, PushPack, PushUnavailable,
};
use elitea_workspace_store::bundle::Limits;
use elitea_workspace_store::connectivity::{WalkError, reachable_bounded};
use elitea_workspace_store::objects::{CommitInfo, FileEntry, ObjectDb, TreeEntry, oid};
use gix_hash::ObjectId;
use gix_object::Write as _;

use super::gix_codec::write_tree;
use super::pr_diff::parse_upstream_trailer;

/// The most store commits walked back from the thread's head.
pub(crate) const MAX_PUSH_COMMITS: usize = 1_000;
const WIP_MESSAGE: &str = "Work in progress from the Elitea cloud thread\n";
const DEFAULT_MESSAGE: &str = "Elitea cloud turn\n";
/// The deepest path a left-out entry is restored at.
const MAX_DEPTH: usize = 256;

fn failed() -> PushUnavailable {
    PushUnavailable::Failed("The workspace could not be read; nothing was pushed.")
}

/// A store object that should be there is missing or corrupt.
const fn integrity() -> PushUnavailable {
    PushUnavailable::StoreIntegrity
}

fn walk_error(error: &WalkError) -> PushUnavailable {
    match error {
        WalkError::OverLimit { .. } => PushUnavailable::TooLarge,
        WalkError::Store(_) => integrity(),
    }
}

/// The `author` and `committer` header lines of a raw commit.
fn people(raw: &[u8]) -> Option<(String, String)> {
    let text = std::str::from_utf8(raw).ok()?;
    let header = text.split("\n\n").next()?;
    let line = |prefix: &str| {
        header
            .lines()
            .find(|line| line.starts_with(prefix))
            .map(str::to_owned)
    };
    Some((line("author ")?, line("committer ")?))
}

/// The message without the store's `Elitea-*` trailers.
fn public_message(message: &str) -> String {
    let kept: Vec<&str> = message
        .lines()
        .filter(|line| !line.starts_with("Elitea-"))
        .collect();
    let text = kept.join("\n");
    let text = text.trim_end();
    if text.trim().is_empty() {
        DEFAULT_MESSAGE.to_owned()
    } else {
        format!("{text}\n")
    }
}

fn has_trailer(info: &CommitInfo, trailer: &str) -> bool {
    info.message.lines().any(|line| line.starts_with(trailer))
}

/// The recorded upstream of the thread: U's `Elitea-Upstream` trailer.
///
/// # Errors
///
/// No upstream commit, or no trailer on it.
pub(crate) fn read_upstream(
    db: &ObjectDb,
    upstream_commit: &str,
) -> Result<elitea_agent_runtime::toolkits::workspace_pull_request::Upstream, PushUnavailable> {
    let u = oid(upstream_commit).map_err(|_| PushUnavailable::NoUpstream)?;
    let u_commit = db.commit(&u).map_err(|_| PushUnavailable::NoUpstream)?;
    let upstream = parse_upstream_trailer(&u_commit.message).ok_or(PushUnavailable::NoUpstream)?;
    Ok(
        elitea_agent_runtime::toolkits::workspace_pull_request::Upstream {
            sha: upstream.sha,
            branch: upstream.branch,
            remote_url: upstream.remote_url,
        },
    )
}

/// Where a path stands in a tree.
enum Found {
    Absent,
    File,
    /// A directory is there, or a file stands where a parent directory
    /// would be: the left-out entry cannot be put back.
    Blocked,
}

/// The tracked paths the handoff left out, and what each pushed tree keeps
/// at them (`None`: no entry, as on the remote).
struct LeftOut {
    keep: BTreeMap<String, Option<FileEntry>>,
}

impl LeftOut {
    /// The paths in B's tree that H's tree lacks (the send removed them
    /// from its index: excluded or over the size cap; a file deleted before
    /// the send looks the same and keeps the remote's state, which loses
    /// nothing), each with the entry the remote already holds.
    fn find(
        db: &ObjectDb,
        chain: &[(ObjectId, CommitInfo)],
        upstream_tree: &ObjectId,
        known: &HashSet<ObjectId>,
    ) -> Result<Self, PushUnavailable> {
        let mut keep = BTreeMap::new();
        let (Some((_, base)), Some((_, handoff))) = (chain.first(), chain.get(1)) else {
            return Ok(Self { keep });
        };
        if !has_trailer(base, "Elitea-Base:") || !has_trailer(handoff, "Elitea-Handoff:") {
            return Ok(Self { keep });
        }
        let base_files = db.flatten(&base.tree).map_err(|_| integrity())?;
        let handoff_files = db.flatten(&handoff.tree).map_err(|_| integrity())?;
        let upstream_files = db.flatten(upstream_tree).map_err(|_| integrity())?;
        for (path, entry) in &base_files {
            if handoff_files.contains_key(path) {
                continue;
            }
            let on_remote = !entry.is_gitlink() && known.contains(&entry.oid);
            let kept = if on_remote {
                Some(*entry)
            } else {
                upstream_files.get(path).copied()
            };
            keep.insert(path.clone(), kept);
        }
        Ok(Self { keep })
    }

    fn is_empty(&self) -> bool {
        self.keep.is_empty()
    }
}

/// `path`'s place in `tree`.
fn find(db: &ObjectDb, tree: &ObjectId, path: &str) -> Result<Found, PushUnavailable> {
    let mut current = *tree;
    let mut parts = path.split('/').peekable();
    while let Some(part) = parts.next() {
        let entries = db.tree(&current).map_err(|_| integrity())?;
        let Some(entry) = entries.iter().find(|entry| entry.name == part.as_bytes()) else {
            return Ok(Found::Absent);
        };
        let last = parts.peek().is_none();
        match (last, entry.is_tree()) {
            (true, false) => return Ok(Found::File),
            (false, true) => current = entry.oid,
            _ => return Ok(Found::Blocked),
        }
    }
    Ok(Found::Absent)
}

/// git's tree order: a directory sorts as if its name ended in `/`.
fn git_key(entry: &TreeEntry) -> Vec<u8> {
    let mut name = entry.name.clone();
    if entry.is_tree() {
        name.push(b'/');
    }
    name
}

/// One edit of a tree: a path below it, split, and the entry to put there
/// (`None`: remove it).
type Edit<'a> = (Vec<&'a str>, Option<FileEntry>);

/// `tree` with `edits` applied, written as loose objects; `None` for a tree
/// left empty.
fn edit_tree(
    db: &ObjectDb,
    loose: &gix_odb::loose::Store,
    tree: Option<&ObjectId>,
    edits: &[Edit<'_>],
    depth: usize,
) -> Result<Option<ObjectId>, PushUnavailable> {
    if depth > MAX_DEPTH {
        return Err(PushUnavailable::ExcludedTracked);
    }
    let mut entries = match tree {
        Some(tree) => db.tree(tree).map_err(|_| integrity())?,
        None => Vec::new(),
    };
    let mut below: BTreeMap<&str, Vec<Edit<'_>>> = BTreeMap::new();
    for (parts, entry) in edits {
        let Some((first, rest)) = parts.split_first() else {
            continue;
        };
        if rest.is_empty() {
            entries.retain(|existing| existing.name != first.as_bytes());
            if let Some(entry) = entry {
                entries.push(TreeEntry {
                    name: first.as_bytes().to_vec(),
                    mode: entry.mode,
                    oid: entry.oid,
                });
            }
        } else {
            below
                .entry(first)
                .or_default()
                .push((rest.to_vec(), *entry));
        }
    }
    for (name, edits) in below {
        let at = entries
            .iter()
            .position(|entry| entry.name == name.as_bytes());
        let existing = at.map(|index| entries[index].clone());
        if existing.as_ref().is_some_and(|entry| !entry.is_tree()) {
            // A file where the directory would be: checked by the caller.
            return Err(PushUnavailable::ExcludedTracked);
        }
        let edited = edit_tree(
            db,
            loose,
            existing.as_ref().map(|entry| &entry.oid),
            &edits,
            depth + 1,
        )?;
        if let Some(index) = at {
            entries.remove(index);
        }
        if let Some(id) = edited {
            entries.push(TreeEntry {
                name: name.as_bytes().to_vec(),
                mode: 0o040_000,
                oid: id,
            });
        }
    }
    if entries.is_empty() {
        return Ok(None);
    }
    entries.sort_by_key(git_key);
    let mut raw = Vec::new();
    for entry in &entries {
        raw.extend_from_slice(format!("{:o} ", entry.mode).as_bytes());
        raw.extend_from_slice(&entry.name);
        raw.push(0);
        raw.extend_from_slice(entry.oid.as_bytes());
    }
    loose
        .write_buf(gix_object::Kind::Tree, &raw)
        .map(Some)
        .map_err(|_| failed())
}

/// Rewrites each pushed tree so the left-out paths keep the remote's state.
struct Keeper<'a> {
    left_out: &'a LeftOut,
    loose: &'a gix_odb::loose::Store,
    empty_tree: ObjectId,
    done: HashMap<ObjectId, ObjectId>,
}

impl Keeper<'_> {
    /// `tree` as pushed. `replace`: B's own tree, whose left-out paths all
    /// hold HEAD's content and are set to what the remote holds; any other
    /// tree only gets back what it lacks (what the cloud wrote at such a
    /// path stays).
    fn tree(
        &mut self,
        db: &ObjectDb,
        tree: ObjectId,
        replace: bool,
    ) -> Result<ObjectId, PushUnavailable> {
        if self.left_out.is_empty() {
            return Ok(tree);
        }
        if !replace && let Some(done) = self.done.get(&tree) {
            return Ok(*done);
        }
        let mut edits = Vec::new();
        for (path, kept) in &self.left_out.keep {
            if replace {
                edits.push((path.split('/').collect::<Vec<_>>(), *kept));
                continue;
            }
            match find(db, &tree, path)? {
                Found::File => {}
                Found::Absent => {
                    if kept.is_some() {
                        edits.push((path.split('/').collect(), *kept));
                    }
                }
                Found::Blocked => {
                    if kept.is_some() {
                        return Err(PushUnavailable::ExcludedTracked);
                    }
                }
            }
        }
        let edited = if edits.is_empty() {
            tree
        } else {
            edit_tree(db, self.loose, Some(&tree), &edits, 0)?.unwrap_or(self.empty_tree)
        };
        if !replace {
            self.done.insert(tree, edited);
        }
        Ok(edited)
    }
}

/// Re-parent the history at `head` (and the files under `worktree`) onto
/// the upstream recorded on `upstream_commit`, writing the new commits into
/// `git_dir`'s objects, and pack what the remote lacks.
///
/// # Errors
///
/// No upstream, no change, a pack over `max_pack_bytes`, a missing or
/// corrupt store object, a left-out tracked file the push would lose, or a
/// read or write failure.
#[allow(
    clippy::too_many_lines,
    reason = "one ordered flow: walk, keep the left-out paths, re-parent, snapshot, pack"
)]
pub(crate) fn prepare_push(
    git_dir: &Path,
    upstream_commit: &str,
    head: &str,
    worktree: &Path,
    max_pack_bytes: u64,
    signature: &str,
) -> Result<PreparedPush, PushUnavailable> {
    let objects = git_dir.join("objects");
    let db = ObjectDb::open_objects(&objects).map_err(|_| failed())?;
    let upstream = read_upstream(&db, upstream_commit)?;
    let u = oid(upstream_commit).map_err(|_| PushUnavailable::NoUpstream)?;
    let u_commit = db.commit(&u).map_err(|_| PushUnavailable::NoUpstream)?;
    let r = oid(&upstream.sha).map_err(|_| PushUnavailable::NoUpstream)?;

    // What the remote holds through R.
    let limits = Limits::default();
    let upstream_closure = reachable_bounded(&db, &[u], &HashSet::new(), limits.max_objects)
        .map_err(|e| walk_error(&e))?;
    let mut known: HashSet<ObjectId> = upstream_closure.objects.into_iter().collect();
    known.insert(r);

    // The thread's first-parent chain, newest first, down to its base.
    let mut chain = Vec::new();
    let mut current = oid(head).map_err(|_| failed())?;
    loop {
        if current == r {
            // A history that reaches the upstream commit itself.
            break;
        }
        if chain.len() >= MAX_PUSH_COMMITS {
            return Err(PushUnavailable::TooLarge);
        }
        let info = db.commit(&current).map_err(|_| integrity())?;
        let is_base = has_trailer(&info, "Elitea-Base:");
        let parent = info.parents.first().copied();
        chain.push((current, info));
        match parent {
            Some(parent) if !is_base => current = parent,
            _ => break,
        }
    }
    chain.reverse();
    let left_out = LeftOut::find(&db, &chain, &u_commit.tree, &known)?;

    let loose = gix_odb::loose::Store::at(&objects, gix_hash::Kind::Sha1);
    let empty_tree = loose
        .write_buf(gix_object::Kind::Tree, &[])
        .map_err(|_| failed())?;
    let mut keeper = Keeper {
        left_out: &left_out,
        loose: &loose,
        empty_tree,
        done: HashMap::new(),
    };
    let write_commit =
        |tree: &ObjectId, parent: &ObjectId, author: &str, committer: &str, message: &str| {
            let raw = format!("tree {tree}\nparent {parent}\n{author}\n{committer}\n\n{message}");
            loose
                .write_buf(gix_object::Kind::Commit, raw.as_bytes())
                .map_err(|_| failed())
        };
    let mut parent = r;
    let mut parent_tree = u_commit.tree;
    let mut commits: u32 = 0;
    for (id, info) in chain {
        let is_base = has_trailer(&info, "Elitea-Base:");
        let tree = keeper.tree(&db, info.tree, is_base)?;
        if tree == parent_tree {
            continue;
        }
        let raw = db
            .find(&id)
            .map_err(|_| integrity())?
            .ok_or_else(integrity)?
            .1;
        let (author, committer) = people(&raw).ok_or_else(integrity)?;
        parent = write_commit(
            &tree,
            &parent,
            &author,
            &committer,
            &public_message(&info.message),
        )?;
        parent_tree = tree;
        commits += 1;
    }
    let wip_tree = match write_tree(&loose, worktree, 0).map_err(|_| failed())? {
        Some(tree) => tree,
        None => empty_tree,
    };
    // Read again: the workspace's tree is loose objects written just now.
    let db = ObjectDb::open_objects(&objects).map_err(|_| failed())?;
    let wip_tree = keeper.tree(&db, wip_tree, false)?;
    if wip_tree != parent_tree {
        let author = format!("author {signature}");
        let committer = format!("committer {signature}");
        parent = write_commit(&wip_tree, &parent, &author, &committer, WIP_MESSAGE)?;
        parent_tree = wip_tree;
        commits += 1;
    }
    if commits == 0 || parent_tree == u_commit.tree {
        return Err(PushUnavailable::Empty);
    }
    let tip = parent;

    // Read again: the new loose objects are on disk now.
    let db = ObjectDb::open_objects(&objects).map_err(|_| failed())?;
    let before = db.flatten(&u_commit.tree).map_err(|_| integrity())?;
    let after = db.flatten(&parent_tree).map_err(|_| integrity())?;
    let files = u32::try_from(elitea_workspace_store::objects::diff(&before, &after).len())
        .unwrap_or(u32::MAX);
    let walk =
        reachable_bounded(&db, &[tip], &known, limits.max_objects).map_err(|e| walk_error(&e))?;
    if walk.blob_bytes > max_pack_bytes {
        return Err(PushUnavailable::TooLarge);
    }
    let mut pack = PushPack::new();
    pack.append(|buffer| elitea_workspace_store::pack::write(&db, &walk.objects, buffer))
        .map_err(|error| match error {
            elitea_workspace_store::StoreError::Invalid(_) => PushUnavailable::TooLarge,
            _ => integrity(),
        })?;
    if u64::try_from(pack.len()).unwrap_or(u64::MAX) > max_pack_bytes {
        return Err(PushUnavailable::TooLarge);
    }
    Ok(PreparedPush {
        upstream,
        tip: tip.to_string(),
        commits,
        files,
        pack,
    })
}

#[cfg(test)]
#[path = "push_prep_tests.rs"]
mod tests;
