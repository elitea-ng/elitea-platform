//! Task worktrees (#1235, ADR-0029 decision 4: "parallel agents get their
//! own git worktree"): the host creates a linked worktree of the person's
//! repository in its own data directory, gives it a branch, and later
//! removes it. Each one is a folder of its own for a session of its own
//! (own checkpoints under `refs/elitea/checkpoints/<session>/` in the shared
//! object store, own turn claim), so several agents work on one repository
//! at once without touching each other or the person's folder.
//!
//! Every git call goes through [`crate::git`]'s four layers. The worktree
//! is accepted by layer 1 only as the host registered it
//! ([`RegisteredWorktree`]); nothing here reads the repository's own idea of
//! its worktrees.
//!
//! * **Create** ([`create`]): `worktree add --no-checkout -b elitea/<id>`
//!   at a folder the host made, then `read-tree` and `checkout-index` in
//!   the new worktree (after its attribute check, as a checkpoint restore
//!   does), so no hook, filter or checkout config of the repository runs.
//!   Host git may write only the new folder, `.git/worktrees/` and
//!   `refs/heads/elitea/`.
//! * **Status** ([`status`]): commits on the branch since the base, paths
//!   changed and not committed.
//! * **Diff** ([`diff`]): the worktree as a checkpoint would take it now
//!   against the base, without `path_deny` matches and the session's deny
//!   list (as `git_diff`).
//! * **Remove** ([`remove`]): the host deletes the folder and the admin
//!   directory it created (only those: other worktrees of the repository,
//!   stale or not, are the person's), and the branch when nothing was
//!   committed on it.

use std::path::{Path, PathBuf};

use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::git::{RegisteredWorktree, Repo, capped, register_worktree, unregister_worktree};
use crate::session::LocalSession;

/// The branch prefix task worktrees get (`elitea/<first 12 of the thread>`).
pub const BRANCH_PREFIX: &str = "elitea/";

/// A worktree [`create`] made.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Created {
    pub entry: RegisteredWorktree,
    pub branch: String,
    /// The commit it started from.
    pub base_sha: String,
}

/// Where a worktree stands.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct Status {
    /// Commits on its branch since the base.
    pub ahead: u64,
    /// Paths changed and not committed (untracked ones included).
    pub dirty: u64,
}

/// How many letters or digits of the thread's id name its branch: enough
/// that two threads of one repository practically never share one.
const BRANCH_ID_LEN: usize = 12;

/// The most `-<n>` suffixes [`create`] tries when the branch is taken.
const MAX_BRANCH_SUFFIX: u32 = 99;

/// The branch of a thread's worktree: [`BRANCH_PREFIX`] and the first 12
/// letters or digits of the thread's id ([`create`] adds `-<n>` when a
/// branch of that name exists already, one a removed worktree kept).
#[must_use]
pub fn branch_name(thread: &str) -> String {
    let short: String = thread
        .chars()
        .filter(char::is_ascii_alphanumeric)
        .take(BRANCH_ID_LEN)
        .collect();
    format!("{BRANCH_PREFIX}{}", short.to_ascii_lowercase())
}

fn exists(error: impl Into<String>) -> ToolError {
    ToolError::new(ErrorCode::InvalidArgument, error)
}

/// `branch` (`elitea/<short>`), or the first `elitea/<short>-<n>` that
/// `repo` has no branch of.
fn free_branch(repo: &Repo, branch: &str) -> ToolResult<String> {
    let short = branch
        .strip_prefix(BRANCH_PREFIX)
        .filter(|rest| !rest.is_empty() && crate::git::valid_worktree_name(rest))
        .ok_or_else(|| ToolError::invalid("not a task worktree branch"))?;
    let taken = |name: &str| {
        repo.git(repo.top())
            .run(&[
                "rev-parse",
                "--verify",
                "--quiet",
                &format!("refs/heads/{name}"),
            ])
            .is_ok()
    };
    std::iter::once(branch.to_owned())
        .chain((2..=MAX_BRANCH_SUFFIX).map(|n| format!("{BRANCH_PREFIX}{short}-{n}")))
        .find(|candidate| !taken(candidate))
        .ok_or_else(|| {
            exists(format!(
                "the branch {branch} and its suffixes already exist"
            ))
        })
}

/// Create a task worktree of `repo` (the person's folder, not itself a
/// linked worktree) at `path` (absent; its parent is the host's own,
/// private directory), named `name` under `.git/worktrees/`, on a new
/// `branch` at `base` (`HEAD`, a commit id, or a checkpoint's ref); when a
/// branch of that name exists (a removed worktree's, kept for its
/// commits), the first free `branch-<n>` ([`Created::branch`]). It is
/// registered ([`register_worktree`]) before anything is checked out into
/// it; a failure removes what was made.
///
/// # Errors
///
/// [`ErrorCode::InvalidArgument`] when the path or the name is taken, the
/// branch and its suffixes are, or one is malformed;
/// [`ErrorCode::UnsafeRepository`]; git failures.
pub fn create(
    repo: &Repo,
    path: &Path,
    name: &str,
    branch: &str,
    base: &str,
) -> ToolResult<Created> {
    if repo.is_linked_worktree() {
        return Err(ToolError::invalid(
            "a task worktree is made from the repository's own folder",
        ));
    }
    if !crate::git::valid_worktree_name(name) {
        return Err(ToolError::invalid("not a worktree name"));
    }
    if base.is_empty() || base.starts_with('-') || base.contains(['\0', '\n', ':', ' ']) {
        return Err(ToolError::invalid("not a base revision"));
    }
    let common = repo.common_dir().to_path_buf();
    let top = repo.top().to_path_buf();
    let git = || repo.git(&top);
    let base_sha = git()
        .text(&[
            "rev-parse",
            "--verify",
            "--quiet",
            "--end-of-options",
            &format!("{base}^{{commit}}"),
        ])
        .map_err(|_| ToolError::invalid(format!("`{base}` is not a commit of this repository")))?;
    let branch = free_branch(repo, branch)?;
    let admin = common.join("worktrees").join(name);
    if std::fs::symlink_metadata(&admin).is_ok() {
        return Err(exists("the repository already has a worktree of that name"));
    }
    if std::fs::symlink_metadata(path).is_ok() {
        return Err(exists("the worktree's folder already exists"));
    }
    {
        use std::os::unix::fs::DirBuilderExt as _;
        std::fs::DirBuilder::new()
            .mode(0o700)
            .create(path)
            .map_err(|error| ToolError::io("cannot create the worktree's folder", &error))?;
    }
    let path = std::fs::canonicalize(path)
        .map_err(|error| ToolError::io("cannot resolve the worktree's folder", &error))?;
    let entry = RegisteredWorktree {
        worktree: path.clone(),
        top: top.clone(),
        name: name.to_owned(),
    };
    let made = (|| {
        let heads = common.join("refs/heads/elitea");
        for dir in [common.join("worktrees"), heads.clone()] {
            std::fs::create_dir_all(&dir)
                .map_err(|error| ToolError::io("cannot prepare the repository", &error))?;
        }
        // `--no-checkout`: no file is written by `worktree add` itself (no
        // checkout filter, no post-checkout hook even if one slipped past
        // `core.hooksPath`).
        git()
            .writes(&[common.join("worktrees"), heads, path.clone()])
            .run(&[
                "worktree",
                "add",
                "--no-checkout",
                "-b",
                &branch,
                &path.to_string_lossy(),
                &base_sha,
            ])?;
        register_worktree(&entry);
        let worktree = repo.registered_worktree_repo(&entry)?;
        let admin = worktree.git_dir().to_path_buf();
        worktree
            .git(&path)
            .writes(std::slice::from_ref(&admin))
            .run(&["read-tree", &base_sha])?;
        worktree
            .git(&path)
            .writes(&[path.clone(), admin])
            .worktree()
            .run(&["checkout-index", "--all", "--force", "-u"])?;
        Ok(())
    })();
    if let Err(error) = made {
        let _ = remove(repo, &entry, false);
        return Err(error);
    }
    Ok(Created {
        entry,
        branch,
        base_sha,
    })
}

/// Commits on the worktree's branch since `base_sha`, and its changed paths.
///
/// # Errors
///
/// [`ErrorCode::UnsafeRepository`], git failures.
pub fn status(worktree: &Repo, base_sha: &str) -> ToolResult<Status> {
    let top = worktree.top();
    let ahead = worktree
        .git(top)
        .text(&["rev-list", "--count", &format!("{base_sha}..HEAD")])?
        .parse()
        .unwrap_or(0);
    let listed = worktree.git(top).worktree().run(&[
        "status",
        "--porcelain=v1",
        "-z",
        "--no-renames",
        "--untracked-files=all",
        "--ignore-submodules=all",
    ])?;
    let dirty = listed
        .split(|byte| *byte == 0)
        .filter(|entry| !entry.is_empty())
        .count() as u64;
    Ok(Status { ahead, dirty })
}

/// The worktree's session's folder as a checkpoint would take it now
/// (tracked and untracked files, ignored ones left out) against
/// `base_sha`, as a unified diff without the paths `path_deny` and the
/// session's deny list cover; whether it was cut at [`crate::git::OUTPUT_CAP`].
///
/// # Errors
///
/// The folder's repository is refused or git fails.
pub fn diff(session: &LocalSession, base_sha: &str) -> ToolResult<(String, bool)> {
    let checkpoints = session.checkpoints();
    let repo = checkpoints
        .repo()
        .ok_or_else(|| ToolError::new(ErrorCode::Git, "the worktree is not in git"))?;
    let snapshot = checkpoints.snapshot_commit()?;
    let mut words: Vec<String> = [
        "diff",
        "--no-ext-diff",
        "--no-textconv",
        "--no-color",
        "--ignore-submodules=all",
        base_sha,
        snapshot.as_str(),
        "--",
        ":(literal).",
    ]
    .iter()
    .map(|word| (*word).to_owned())
    .collect();
    words.extend(crate::session::diff_exclusions(
        session.workspace(),
        session.deny_list().paths(),
    ));
    let words: Vec<&str> = words.iter().map(String::as_str).collect();
    let output = repo
        .git(session.workspace().root())
        .literal(false)
        .run(&words)?;
    Ok(capped(&output))
}

/// Remove a task worktree of `repo`: its folder, its admin directory, and
/// its branch unless `keep_branch` (the caller keeps one with commits). It
/// is unregistered first, so nothing uses it while it goes. Only what
/// `entry` names is deleted, and only when it is where the host puts it.
///
/// # Errors
///
/// [`ErrorCode::InvalidArgument`] when `entry` is not a worktree of `repo`;
/// I/O failures.
pub fn remove(repo: &Repo, entry: &RegisteredWorktree, keep_branch: bool) -> ToolResult<()> {
    if !crate::git::valid_worktree_name(&entry.name) {
        return Err(ToolError::invalid("not a worktree name"));
    }
    let entry_top = std::fs::canonicalize(&entry.top).unwrap_or_else(|_| entry.top.clone());
    if entry_top != repo.top() {
        return Err(ToolError::invalid(
            "the worktree belongs to another repository",
        ));
    }
    unregister_worktree(&entry.worktree);
    let branch = admin_branch(repo, &entry.name);
    let admin = repo.common_dir().join("worktrees").join(&entry.name);
    remove_tree(&entry.worktree, "the worktree's folder")?;
    remove_tree(&admin, "the worktree's git directory")?;
    if let Some(branch) = branch.filter(|_| !keep_branch) {
        let common = repo.common_dir().to_path_buf();
        let top = repo.top().to_path_buf();
        // Best effort: a branch left behind is the person's to delete.
        let _ = repo
            .git(&top)
            .writes(&[
                common.join("refs/heads/elitea"),
                common.join("packed-refs"),
                common.join("packed-refs.lock"),
            ])
            .run(&["update-ref", "-d", &format!("refs/heads/{branch}")]);
    }
    Ok(())
}

/// The `elitea/…` branch the admin directory `name` has checked out.
fn admin_branch(repo: &Repo, name: &str) -> Option<String> {
    let head = std::fs::read_to_string(repo.common_dir().join("worktrees").join(name).join("HEAD"))
        .ok()?;
    head.trim()
        .strip_prefix("ref: refs/heads/")
        .filter(|branch| branch.starts_with(BRANCH_PREFIX))
        .map(str::to_owned)
}

/// Delete `path` and what is under it, never following a symlink (a
/// symlink at `path` itself is unlinked). Absent is fine.
fn remove_tree(path: &Path, what: &str) -> ToolResult<()> {
    match std::fs::symlink_metadata(path) {
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(ToolError::io(&format!("cannot remove {what}"), &error)),
        Ok(meta) if meta.is_dir() => std::fs::remove_dir_all(path)
            .map_err(|error| ToolError::io(&format!("cannot remove {what}"), &error)),
        Ok(_) => std::fs::remove_file(path)
            .map_err(|error| ToolError::io(&format!("cannot remove {what}"), &error)),
    }
}

/// The admin directories of `repo` whose `gitdir` points into `dir` (the
/// host's folder for one workspace's task worktrees): `(name, worktree
/// folder)`. What the Doctor compares with the host's registry.
#[must_use]
pub fn host_created(repo: &Repo, dir: &Path) -> Vec<(String, PathBuf)> {
    let dir = std::fs::canonicalize(dir).unwrap_or_else(|_| dir.to_path_buf());
    let Ok(entries) = std::fs::read_dir(repo.common_dir().join("worktrees")) else {
        return Vec::new();
    };
    let mut out = Vec::new();
    for entry in entries.flatten() {
        let name = entry.file_name().to_string_lossy().into_owned();
        if !crate::git::valid_worktree_name(&name)
            || !entry.file_type().is_ok_and(|kind| kind.is_dir())
        {
            continue;
        }
        let Ok(text) = std::fs::read_to_string(entry.path().join("gitdir")) else {
            continue;
        };
        let pointer = PathBuf::from(text.trim());
        let Some(folder) = pointer.parent() else {
            continue;
        };
        if folder.parent() == Some(dir.as_path()) {
            out.push((name, folder.to_path_buf()));
        }
    }
    out.sort();
    out
}

/// When the newest checkpoint of `session` was taken (unix seconds): the
/// last turn that wrote in a task worktree whose session that is.
#[must_use]
pub fn last_checkpoint_at(repo: &Repo, session: &str) -> Option<u64> {
    if !crate::git::valid_worktree_name(session) {
        return None;
    }
    repo.git(repo.top())
        .text(&[
            "for-each-ref",
            "--format=%(creatordate:unix)",
            &format!("{}/{session}/", crate::checkpoint::REF_PREFIX),
        ])
        .ok()?
        .lines()
        .filter_map(|line| line.trim().parse::<u64>().ok())
        .max()
}
