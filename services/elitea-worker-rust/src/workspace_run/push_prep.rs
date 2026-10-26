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

use std::collections::HashSet;
use std::path::Path;

use elitea_agent_runtime::toolkits::workspace_pull_request::{PreparedPush, PushUnavailable};
use elitea_workspace_store::bundle::Limits;
use elitea_workspace_store::objects::{ObjectDb, oid};
use gix_object::Write as _;

use super::gix_codec::write_tree;
use super::pr_diff::parse_upstream_trailer;

/// The most store commits walked back from the thread's head.
pub(crate) const MAX_PUSH_COMMITS: usize = 1_000;
const WIP_MESSAGE: &str = "Work in progress from the Elitea cloud thread\n";
const DEFAULT_MESSAGE: &str = "Elitea cloud turn\n";

fn failed() -> PushUnavailable {
    PushUnavailable::Failed("The workspace could not be read; nothing was pushed.")
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

/// Re-parent the history at `head` (and the files under `worktree`) onto
/// the upstream recorded on `upstream_commit`, writing the new commits into
/// `git_dir`'s objects, and pack what the remote lacks.
///
/// # Errors
///
/// No upstream, no change, a pack over `max_pack_bytes`, or a read or write
/// failure.
#[allow(
    clippy::too_many_lines,
    reason = "one ordered flow: walk, re-parent, snapshot, pack"
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
    let u = oid(upstream_commit).map_err(|_| PushUnavailable::NoUpstream)?;
    let u_commit = db.commit(&u).map_err(|_| PushUnavailable::NoUpstream)?;
    let upstream = parse_upstream_trailer(&u_commit.message).ok_or(PushUnavailable::NoUpstream)?;
    let r = oid(&upstream.sha).map_err(|_| PushUnavailable::NoUpstream)?;

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
        let info = db.commit(&current).map_err(|_| failed())?;
        let is_base = info
            .message
            .lines()
            .any(|line| line.starts_with("Elitea-Base:"));
        let parent = info.parents.first().copied();
        chain.push((current, info));
        match parent {
            Some(parent) if !is_base => current = parent,
            _ => break,
        }
    }
    chain.reverse();

    let loose = gix_odb::loose::Store::at(&objects, gix_hash::Kind::Sha1);
    let write_commit = |tree: &gix_hash::ObjectId,
                        parent: &gix_hash::ObjectId,
                        author: &str,
                        committer: &str,
                        message: &str| {
        let raw = format!("tree {tree}\nparent {parent}\n{author}\n{committer}\n\n{message}");
        loose
            .write_buf(gix_object::Kind::Commit, raw.as_bytes())
            .map_err(|_| failed())
    };
    let mut parent = r;
    let mut parent_tree = u_commit.tree;
    let mut commits: u32 = 0;
    for (id, info) in chain {
        if info.tree == parent_tree {
            continue;
        }
        let raw = db.find(&id).map_err(|_| failed())?.ok_or_else(failed)?.1;
        let (author, committer) = people(&raw).ok_or_else(failed)?;
        parent = write_commit(
            &info.tree,
            &parent,
            &author,
            &committer,
            &public_message(&info.message),
        )?;
        parent_tree = info.tree;
        commits += 1;
    }
    let wip_tree = match write_tree(&loose, worktree, 0).map_err(|_| failed())? {
        Some(tree) => tree,
        None => loose
            .write_buf(gix_object::Kind::Tree, &[])
            .map_err(|_| failed())?,
    };
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
    let before = db.flatten(&u_commit.tree).map_err(|_| failed())?;
    let after = db.flatten(&parent_tree).map_err(|_| failed())?;
    let files = u32::try_from(elitea_workspace_store::objects::diff(&before, &after).len())
        .unwrap_or(u32::MAX);
    let limits = Limits::default();
    let upstream_closure =
        elitea_workspace_store::connectivity::reachable(&db, &[u], limits.max_objects)
            .map_err(|_| failed())?;
    let mut known: HashSet<gix_hash::ObjectId> = upstream_closure.objects.into_iter().collect();
    known.insert(r);
    let walk = elitea_workspace_store::connectivity::reachable_excluding(
        &db,
        &[tip],
        &known,
        limits.max_objects,
    )
    .map_err(|_| PushUnavailable::TooLarge)?;
    if walk.blob_bytes > max_pack_bytes {
        return Err(PushUnavailable::TooLarge);
    }
    let mut pack = Vec::new();
    elitea_workspace_store::pack::write(&db, &walk.objects, &mut pack).map_err(|_| failed())?;
    if u64::try_from(pack.len()).unwrap_or(u64::MAX) > max_pack_bytes {
        return Err(PushUnavailable::TooLarge);
    }
    Ok(PreparedPush {
        upstream: elitea_agent_runtime::toolkits::workspace_pull_request::Upstream {
            sha: upstream.sha,
            branch: upstream.branch,
            remote_url: upstream.remote_url,
        },
        tip: tip.to_string(),
        commits,
        files,
        pack,
    })
}

#[cfg(test)]
#[path = "push_prep_tests.rs"]
mod tests;
