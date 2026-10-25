//! Merge-back through `git merge-tree` (#1235 stage 4): bring a commit made
//! elsewhere (a task worktree's work, [`crate::worktree`]) into a folder as
//! uncommitted changes, with a dry-run preview first. The one merge-tree
//! apply implementation; the handoff take-back's 3-way mode (#1187 WP6)
//! builds on it.
//!
//! **Preflight** ([`preflight`], writes only objects, nothing anyone sees):
//!
//! 1. the target folder is snapshotted as a checkpoint would take it
//!    (tracked and untracked files, a temporary index; the folder, its index
//!    and `HEAD` stay as they are): `ours`;
//! 2. the outcome against `base` (where the source started):
//!    * `nothing`: the source changed nothing since `base`;
//!    * `fast_forward`: the folder did not change since `base`, so the
//!      result is the source's tree;
//!    * `merge`: both changed, `merge-tree --write-tree` merged them cleanly;
//!    * `conflicts`: `merge-tree` reported conflicted paths; nothing can be
//!      applied (the work stays where it is, on its branch);
//! 3. what applying would write or delete (`applied_paths`), and what it
//!    leaves alone (`skipped_paths`: `path_deny` and the session's deny
//!    list, `.git`, symlinks and submodules on either side, paths outside
//!    the folder when it is a subdirectory of its repository).
//!
//! **Apply** ([`apply`]) snapshots the folder again and refuses with
//! [`FOLDER_CHANGED`] ([`ErrorCode::StaleRead`]) when it moved since the
//! preview, takes a checkpoint (restoring it undoes the merge), then writes
//! the result's files through the session's confined
//! [`Workspace`](crate::workspace::Workspace) writer.
//! `HEAD`, the index and every branch of the folder stay as they were.
//!
//! merge-tree runs under [`crate::git`]'s four layers, with attributes read
//! from the empty tree (`--attr-source`) and `merge.default=text`: no merge
//! driver, filter or text conversion of anyone's config can run.

use std::path::Path;

use serde::Serialize;

use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::git::Repo;
use crate::session::LocalSession;
use crate::workspace::{Intent, WsPath};

/// The machine code of a folder that changed after its preview.
pub const FOLDER_CHANGED: &str = "folder_changed_since_preflight";

/// The largest file a merge-back writes; larger ones are skipped.
pub const MAX_FILE_BYTES: u64 = 64 * 1024 * 1024;

/// What a preflight found.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Mode {
    Nothing,
    FastForward,
    Merge,
    Conflicts,
}

/// One path applying writes: its new blob and mode, or `None` to delete it.
#[derive(Clone, Debug, Eq, PartialEq)]
struct Change {
    /// Workspace-relative.
    path: String,
    new: Option<(String, u32)>,
}

/// A preview, held until the person confirms (the `ApplyOutcome` shape:
/// what is applied, what conflicts, what is skipped).
#[derive(Clone, Debug)]
pub struct Preflight {
    pub mode: Mode,
    /// Workspace-relative paths applying writes or deletes (empty for
    /// [`Mode::Conflicts`] and [`Mode::Nothing`]).
    pub applied_paths: Vec<String>,
    /// Paths both sides changed that `merge-tree` could not merge.
    pub conflict_paths: Vec<String>,
    /// Paths the result changes that applying leaves alone.
    pub skipped_paths: Vec<String>,
    /// The folder's snapshot (a commit) the preview was made against.
    ours: String,
    changes: Vec<Change>,
}

/// What [`apply`] did.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize)]
pub struct Applied {
    pub written: Vec<String>,
    pub deleted: Vec<String>,
    pub skipped_paths: Vec<String>,
    /// The checkpoint taken first (restore it to undo the merge); `None`
    /// when nothing was written.
    pub checkpoint: Option<u64>,
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

fn check_commit(text: &str) -> ToolResult<()> {
    if text.len() >= 4 && text.len() <= 64 && text.bytes().all(|b| b.is_ascii_hexdigit()) {
        Ok(())
    } else {
        Err(ToolError::invalid(format!("`{text}` is not a commit id")))
    }
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
    check_commit(source)?;
    check_commit(base)?;
    let repo = repo_of(target)?;
    let ours = target.checkpoints().snapshot_commit()?;
    let (ours_tree, theirs_tree, base_tree) = (
        tree_of(repo, &ours)?,
        tree_of(repo, source)?,
        tree_of(repo, base)?,
    );
    let mut conflict_paths = Vec::new();
    let (mode, result_tree) = if theirs_tree == base_tree || theirs_tree == ours_tree {
        (Mode::Nothing, ours_tree.clone())
    } else if ours_tree == base_tree {
        (Mode::FastForward, theirs_tree)
    } else {
        let (tree, conflicts) = merge_tree(repo, &ours, source, base)?;
        conflict_paths = conflicts;
        let mode = if conflict_paths.is_empty() {
            Mode::Merge
        } else {
            Mode::Conflicts
        };
        (mode, tree)
    };
    let (changes, skipped_paths) = changes(target, repo, &ours_tree, &result_tree)?;
    let (applied_paths, changes) = if matches!(mode, Mode::Conflicts | Mode::Nothing) {
        (Vec::new(), Vec::new())
    } else {
        (changes.iter().map(|c| c.path.clone()).collect(), changes)
    };
    Ok(Preflight {
        mode,
        applied_paths,
        conflict_paths,
        skipped_paths,
        ours,
        changes,
    })
}

/// `merge-tree --write-tree` of `ours` and `theirs` over `base`: the result
/// tree and the conflicted paths (none: a clean merge).
fn merge_tree(
    repo: &Repo,
    ours: &str,
    theirs: &str,
    base: &str,
) -> ToolResult<(String, Vec<String>)> {
    let top = repo.top();
    let empty_tree = repo
        .git(top)
        .stdin(b"")
        .text(&["hash-object", "-t", "tree", "--stdin"])?;
    let attr_source = format!("--attr-source={empty_tree}");
    let merge_base = format!("--merge-base={base}");
    let objects = [repo.common_dir().join("objects")];
    let output = repo.git(top).writes(&objects).exit_one_ok().run(&[
        "-c",
        "merge.default=text",
        &attr_source,
        "merge-tree",
        "--write-tree",
        "-z",
        "--name-only",
        "--no-messages",
        &merge_base,
        ours,
        theirs,
    ])?;
    let mut fields = output.split(|byte| *byte == 0);
    let tree = fields
        .next()
        .map(|tree| String::from_utf8_lossy(tree).trim().to_owned())
        .filter(|tree| check_commit(tree).is_ok())
        .ok_or_else(|| ToolError::new(ErrorCode::Git, "git merge-tree gave no tree"))?;
    let mut conflicts: Vec<String> = fields
        .filter(|path| !path.is_empty())
        .map(|path| String::from_utf8_lossy(path).into_owned())
        .collect();
    conflicts.sort();
    conflicts.dedup();
    Ok((tree, conflicts))
}

/// What turning `from` into `to` changes in the folder, and the paths it
/// leaves alone.
fn changes(
    target: &LocalSession,
    repo: &Repo,
    from: &str,
    to: &str,
) -> ToolResult<(Vec<Change>, Vec<String>)> {
    let workspace = target.workspace();
    let prefix = workspace
        .root()
        .strip_prefix(repo.top())
        .map(Path::to_path_buf)
        .unwrap_or_default();
    let raw =
        repo.git(repo.top())
            .run(&["diff-tree", "-r", "-z", "--no-renames", "--raw", from, to])?;
    let mut fields = raw.split(|byte| *byte == 0);
    let (mut out, mut skipped) = (Vec::new(), Vec::new());
    while let (Some(meta), Some(path)) = (fields.next(), fields.next()) {
        let meta = String::from_utf8_lossy(meta);
        let path = String::from_utf8_lossy(path).into_owned();
        // `:<old mode> <new mode> <old id> <new id> <status>`
        let parts: Vec<&str> = meta.trim_start_matches(':').split(' ').collect();
        let [old_mode, new_mode, _, new_id, status] = parts.as_slice() else {
            continue;
        };
        let special = |mode: &str| matches!(mode, "120000" | "160000");
        let relative = Path::new(&path).strip_prefix(&prefix).ok();
        let ws_path = relative.and_then(|relative| WsPath::from_relative(relative).ok());
        if !ws_path.is_some_and(|ws_path| {
            !special(old_mode)
                && !special(new_mode)
                && workspace.denied_by(&ws_path, Intent::Write).is_none()
        }) {
            skipped.push(path);
            continue;
        }
        let relative = relative
            .map(|relative| relative.to_string_lossy().into_owned())
            .unwrap_or_default();
        let new = if status.starts_with('D') {
            None
        } else {
            let mode = if *new_mode == "100755" { 0o755 } else { 0o644 };
            Some(((*new_id).to_owned(), mode))
        };
        out.push(Change {
            path: relative,
            new,
        });
    }
    skipped.sort();
    Ok((out, skipped))
}

/// Apply a preview to `target`'s folder (see the module documentation);
/// `label` names the checkpoint taken first.
///
/// # Errors
///
/// [`ErrorCode::StaleRead`] ([`FOLDER_CHANGED`]) when the folder changed
/// since the preview; [`ErrorCode::InvalidArgument`] for a preview with
/// conflicts; checkpoint, git or write failures (a failure after the
/// checkpoint leaves it to restore).
pub fn apply(target: &LocalSession, preflight: &Preflight, label: &str) -> ToolResult<Applied> {
    let repo = repo_of(target)?;
    if preflight.mode == Mode::Conflicts {
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
        skipped_paths: preflight.skipped_paths.clone(),
        ..Applied::default()
    };
    if preflight.changes.is_empty() {
        return Ok(applied);
    }
    applied.checkpoint = Some(target.checkpoints().create(label)?.seq);
    let workspace = target.workspace();
    for change in &preflight.changes {
        let ws_path = WsPath::from_relative(Path::new(&change.path))?;
        match &change.new {
            None => {
                if workspace.remove_file(&ws_path)? {
                    applied.deleted.push(change.path.clone());
                }
            }
            Some((id, mode)) => {
                let size: u64 = repo
                    .git(repo.top())
                    .text(&["cat-file", "-s", id])?
                    .parse()
                    .unwrap_or(u64::MAX);
                if size > MAX_FILE_BYTES {
                    applied.skipped_paths.push(change.path.clone());
                    continue;
                }
                let bytes = repo.git(repo.top()).run(&["cat-file", "blob", id])?;
                workspace.write(&ws_path, &bytes, Some(*mode))?;
                applied.written.push(change.path.clone());
            }
        }
    }
    applied.skipped_paths.sort();
    Ok(applied)
}

#[cfg(test)]
mod tests {
    use std::path::{Path, PathBuf};
    use std::process::Command;
    use std::sync::Arc;

    use super::{Mode, apply, preflight};
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
            spill_dir: None, // under data_dir
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
        assert_eq!(preview.mode, Mode::Conflicts);
        assert_eq!(preview.conflict_paths, ["a.txt"]);
        assert!(preview.applied_paths.is_empty(), "{preview:?}");
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
        assert_eq!(preview.mode, Mode::Merge);
        assert_eq!(preview.applied_paths, ["a.txt", "b.txt"]);
        assert!(preview.conflict_paths.is_empty(), "{preview:?}");
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
        assert_eq!(preview.mode, Mode::FastForward);
        assert_eq!(preview.applied_paths, ["a.txt", "b.txt", "doomed.txt"]);
        assert_eq!(preview.skipped_paths, ["secret.txt"]);

        // The folder moved since the preview: refused, nothing written.
        std::fs::write(f.root.join("late.txt"), "late\n").expect("late");
        let stale = apply(&main, &preview, "merge").expect_err("stale");
        assert_eq!(stale.code(), ErrorCode::StaleRead);
        assert!(read(&f.root, "b.txt").is_none());
        std::fs::remove_file(f.root.join("late.txt")).expect("unlate");

        let applied = apply(&main, &preview, "merge").expect("apply");
        assert_eq!(applied.written, ["a.txt", "b.txt"]);
        assert_eq!(applied.deleted, ["doomed.txt"]);
        assert_eq!(applied.skipped_paths, ["secret.txt"]);
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
