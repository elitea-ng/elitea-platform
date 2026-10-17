//! The snapshot of a send (ADR-0034 decisions 5 and 6).
//!
//! **The transfer set** is the checkpoint set: tracked files plus untracked
//! files that are not ignored, built in a temporary index (`GIT_INDEX_FILE`:
//! `read-tree HEAD`, `add --all`, `write-tree`), so the person's index,
//! `HEAD`, branches, stash and working tree are never touched. Then:
//!
//! 1. every candidate path is matched against the exclusions
//!    ([`super::exclude`]); excluded and oversize paths are left out of the
//!    `add`, and excluded *tracked* paths are removed from the temporary
//!    index (`update-index --force-remove`), so they are not in the tree;
//! 2. the tree is listed back and checked again: the exclusions hold on
//!    what is really sent, not on a guess;
//! 3. every file of the tree is read from the object database and scanned
//!    ([`super::scan`]), and the caps checked ([`super::caps`]);
//! 4. history mode `none`: the orphan base `B` (tree of `HEAD`, trailer
//!    `Elitea-Base: <HEAD>`), the orphan upstream `U` when `HEAD`'s branch
//!    has one (the upstream commit's tree, trailer `Elitea-Upstream: <sha>
//!    <remote url> <branch>`, the URL without credentials), the snapshot
//!    `H_1` (parent `B`, the note as message) and its twin `H'_1` (parent
//!    the real `HEAD`), all as objects only.
//!
//! [`keep_refs`] then records `H_1` and `H'_1` on private refs
//! (`refs/elitea/handoff-local/<conversation>/{sent,twin}`), and
//! [`bundle`] writes the `COMPACT` segment of `B`, `U` and `H_1`.

use std::collections::BTreeSet;
use std::ffi::OsStr;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use elitea_workspace_store::bundle::{self as segment, Limits, Written};
use elitea_workspace_store::objects::{self as store_objects, EMPTY_TREE, ObjectDb};
use elitea_workspace_store::refs as store_refs;
use serde::{Deserialize, Serialize};

use super::caps::{self, CapReport, Caps, SizedPath};
use super::exclude::{Excluded, Exclusions, Patterns, Reason, Rules};
use super::scan::{self, Finding};
use super::{LOCAL_REF_PREFIX, SendRecord, TempDir, check_conversation, shadow};
use crate::deny::DenyList;
use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::git::Repo;
use crate::sandbox::SandboxConfig;
use crate::workspace::Workspace;

/// The largest handoff note (contract §2.1).
pub const MAX_NOTE_BYTES: usize = 8 * 1024;

const IDENTITY: &str = "Elitea handoff";
const EMAIL: &str = "handoff@elitea.invalid";

/// Where a send reads from.
#[derive(Clone, Debug)]
pub struct SendConfig {
    /// The workspace folder (the repository's top level, or any folder).
    pub root: PathBuf,
    /// The host's data directory: the shadow repository lives there.
    pub data_dir: PathBuf,
    pub sandbox: SandboxConfig,
    /// What host git may not read (the session's deny list).
    pub deny: Arc<DenyList>,
    pub path_deny: Vec<String>,
    pub leave_on_laptop: Vec<String>,
    /// The exclusion patterns by source (platform, project, the person's).
    pub patterns: Patterns,
    /// Explicit includes; empty unless the policy allows including.
    pub include: Vec<String>,
    pub caps: Caps,
    pub conversation_uuid: String,
    pub note: String,
    /// Tests: read this file as the person's global git config.
    pub global_config: Option<PathBuf>,
}

impl SendConfig {
    /// The exclusion rules of this send.
    #[must_use]
    pub fn rules(&self) -> Rules {
        Rules {
            patterns: self.patterns.clone(),
            path_deny: self.path_deny.clone(),
            leave_on_laptop: self.leave_on_laptop.clone(),
            include: self.include.clone(),
        }
    }
}

/// The repository a send reads: the folder's own, or its shadow.
#[derive(Clone, Debug)]
pub struct Source {
    pub(crate) repo: Repo,
    pub shadow: bool,
}

impl Source {
    /// The git directory (the shadow's for a folder that is not a
    /// repository).
    #[must_use]
    pub fn git_dir(&self) -> &Path {
        self.repo.git_dir()
    }

    /// The folder.
    #[must_use]
    pub fn top(&self) -> &Path {
        self.repo.top()
    }

    /// Where objects and shared refs live (a linked worktree's repository).
    #[must_use]
    pub fn common_dir(&self) -> &Path {
        self.repo.common_dir()
    }

    pub(crate) fn objects(&self) -> PathBuf {
        self.repo.common_dir().join("objects")
    }

    pub(crate) fn refs_dir(&self) -> PathBuf {
        self.repo.common_dir().join("refs/elitea")
    }

    /// `HEAD`'s commit, when there is one.
    #[must_use]
    pub fn head(&self) -> Option<String> {
        self.repo
            .git(self.top())
            .text(&["rev-parse", "--verify", "--quiet", "HEAD^{commit}"])
            .ok()
            .filter(|head| !head.is_empty())
    }

    /// The read-only object database (workspace-store).
    ///
    /// # Errors
    ///
    /// A sha256 repository, or one that cannot be opened.
    pub fn object_db(&self) -> ToolResult<ObjectDb> {
        ObjectDb::open_git_dir(self.common_dir()).map_err(|e| super::store_error(&e))
    }
}

/// Open the folder's repository, or its shadow when it is not one.
///
/// # Errors
///
/// A folder inside a repository but not its top level (Phase 1 hands off
/// whole repositories), an unsafe repository, a sha256 repository.
pub fn open_source(
    root: &Path,
    data_dir: &Path,
    sandbox: &SandboxConfig,
    deny: &Arc<DenyList>,
    global_config: Option<&Path>,
) -> ToolResult<Source> {
    let root = std::fs::canonicalize(root)
        .map_err(|error| ToolError::io("cannot open the folder", &error))?;
    let found = Repo::discover(&root, sandbox, deny.clone())?;
    let (repo, is_shadow) = if let Some(repo) = found {
        if repo.top() != root {
            return Err(ToolError::new(
                ErrorCode::Unsupported,
                "this folder is inside a git repository but is not its top level; open the repository's top folder to send it to the cloud",
            ));
        }
        (repo, false)
    } else {
        let git_dir = shadow::dir_for(data_dir, &root);
        shadow::ensure(&git_dir)?;
        let carved = shadow::carve(deny, &git_dir);
        (Repo::shadow(&git_dir, &root, sandbox, carved)?, true)
    };
    let repo = match global_config {
        Some(path) => repo.with_global_config(path.to_path_buf()),
        None => repo,
    };
    let format =
        store_objects::object_format(repo.common_dir()).map_err(|e| super::store_error(&e))?;
    if format != "sha1" {
        return Err(ToolError::new(
            ErrorCode::Unsupported,
            format!(
                "the repository uses {format} object ids; only sha1 repositories can be sent to the cloud (unsupported_object_format)"
            ),
        ));
    }
    Ok(Source {
        repo,
        shadow: is_shadow,
    })
}

/// The upstream of `HEAD`'s branch, as the send records it.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct Upstream {
    /// `U`: the orphan commit with the upstream commit's tree.
    pub commit: String,
    /// The upstream commit.
    pub sha: String,
    /// The remote's URL without credentials.
    pub url: String,
    /// The upstream branch name (`main`).
    pub branch: String,
}

/// A snapshot, before anything is sent.
#[derive(Clone, Debug, Serialize)]
pub struct Snapshot {
    pub shadow: bool,
    pub head: Option<String>,
    pub tree: String,
    pub base: String,
    pub upstream: Option<Upstream>,
    pub handoff: String,
    pub twin: String,
    /// Files in the tree and their bytes.
    pub files: usize,
    pub unpacked_bytes: u64,
    pub excluded: Vec<Excluded>,
    /// Files the exclusions match that the person included (sent; listed
    /// as `included_excluded` and in the `Elitea-Included` trailers).
    pub included: Vec<Included>,
    /// Findings outside the included files: they block the send.
    pub findings: Vec<Finding>,
    /// Findings in included files: warnings the person confirms per send.
    pub include_findings: Vec<Finding>,
    pub caps: CapReport,
    /// The patterns the send used (kept for take-back).
    pub patterns: Patterns,
}

/// A file sent although the exclusions match it.
#[derive(Clone, Debug, Deserialize, Eq, Ord, PartialEq, PartialOrd, Serialize)]
pub struct Included {
    pub path: String,
    /// The exclusion it overrode.
    pub reason: Reason,
}

impl Snapshot {
    /// The send is refused: a secret finding or a cap.
    #[must_use]
    pub fn blocked(&self) -> bool {
        !self.findings.is_empty() || !self.caps.ok()
    }

    /// What take-back needs later.
    #[must_use]
    pub fn record(&self, conversation_uuid: &str) -> SendRecord {
        SendRecord {
            conversation_uuid: conversation_uuid.to_owned(),
            shadow: self.shadow,
            head: self.head.clone(),
            base: self.base.clone(),
            upstream: self.upstream.as_ref().map(|u| u.commit.clone()),
            handoff: self.handoff.clone(),
            twin: self.twin.clone(),
            tree: self.tree.clone(),
            excluded: self.excluded.clone(),
            included: self.included.iter().map(|i| i.path.clone()).collect(),
            patterns: Some(self.patterns.clone()),
        }
    }
}

/// One entry of `ls-tree -r -l`.
#[derive(Clone, Debug)]
pub(crate) struct TreeLine {
    pub mode: String,
    pub kind: String,
    pub oid: String,
    pub size: u64,
    pub path: String,
}

fn nul_fields(bytes: &[u8]) -> impl Iterator<Item = String> + '_ {
    bytes
        .split(|byte| *byte == 0)
        .filter(|field| !field.is_empty())
        .map(|field| String::from_utf8_lossy(field).into_owned())
}

/// The files of `tree`, recursively, with sizes.
pub(crate) fn list_tree(source: &Source, tree: &str) -> ToolResult<Vec<TreeLine>> {
    let out =
        source
            .repo
            .git(source.top())
            .run(&["ls-tree", "-r", "-z", "-l", "--full-tree", tree])?;
    let mut lines = Vec::new();
    for field in nul_fields(&out) {
        let Some((meta, path)) = field.split_once('\t') else {
            continue;
        };
        let mut parts = meta.split_whitespace();
        let (Some(mode), Some(kind), Some(oid)) = (parts.next(), parts.next(), parts.next()) else {
            continue;
        };
        let size = parts.next().and_then(|size| size.parse().ok()).unwrap_or(0);
        lines.push(TreeLine {
            mode: mode.to_owned(),
            kind: kind.to_owned(),
            oid: oid.to_owned(),
            size,
            path: path.to_owned(),
        });
    }
    Ok(lines)
}

/// Candidates sorted into excluded, oversize and the rest.
#[derive(Default)]
struct Classified {
    excluded: Vec<Excluded>,
    oversize: Vec<SizedPath>,
    /// Paths the `add` leaves out (excluded, oversize, not files).
    left_out: Vec<String>,
}

fn classify(
    top: &Path,
    candidates: &BTreeSet<String>,
    tracked: &BTreeSet<String>,
    exclusions: &Exclusions,
    max_file_bytes: u64,
) -> Classified {
    let mut out = Classified::default();
    for path in candidates {
        let clean = path.trim_end_matches('/');
        let is_tracked = tracked.contains(clean);
        if let Some(reason) = exclusions.reason(clean) {
            out.excluded.push(Excluded {
                path: clean.to_owned(),
                reason,
                tracked: is_tracked,
            });
            out.left_out.push(clean.to_owned());
            continue;
        }
        if path.ends_with('/') {
            // An untracked nested repository: not a file a bundle carries.
            out.excluded.push(Excluded {
                path: clean.to_owned(),
                reason: Reason::NotAFile,
                tracked: false,
            });
            out.left_out.push(clean.to_owned());
            continue;
        }
        let Ok(meta) = std::fs::symlink_metadata(top.join(clean)) else {
            // Deleted since HEAD: `add --all` records the deletion.
            continue;
        };
        let kind = meta.file_type();
        if kind.is_symlink() {
            continue;
        }
        if kind.is_dir() {
            // A tracked submodule's directory: its gitlink stays as in HEAD.
            continue;
        }
        if !kind.is_file() {
            out.excluded.push(Excluded {
                path: clean.to_owned(),
                reason: Reason::NotAFile,
                tracked: is_tracked,
            });
            out.left_out.push(clean.to_owned());
            continue;
        }
        if meta.len() > max_file_bytes {
            out.oversize.push(SizedPath {
                path: clean.to_owned(),
                bytes: meta.len(),
            });
            out.left_out.push(clean.to_owned());
        }
    }
    out
}

/// The tree of the folder as a send sees it: the transfer set minus
/// `exclusions` in a temporary index. Returns the tree, the excluded paths
/// and the files left out for size.
pub(crate) fn snapshot_tree(
    source: &Source,
    exclusions: &Exclusions,
    max_file_bytes: u64,
) -> ToolResult<(String, Vec<Excluded>, Vec<SizedPath>)> {
    let index = TempDir::new("index")?;
    let index_file = index.path().join("index");
    let index_os: &OsStr = index_file.as_os_str();
    let index_dir = [index.path().to_path_buf()];
    let with_objects = [index.path().to_path_buf(), source.objects()];
    let top = source.top();
    let head = source.head();
    if let Some(head) = &head {
        source
            .repo
            .git(top)
            .env("GIT_INDEX_FILE", index_os)
            .writes(&index_dir)
            .run(&["read-tree", head])?;
    }
    let tracked: BTreeSet<String> = nul_fields(
        &source
            .repo
            .git(top)
            .env("GIT_INDEX_FILE", index_os)
            .run(&["ls-files", "-z", "--cached"])?,
    )
    .collect();
    let candidates: BTreeSet<String> = nul_fields(
        &source
            .repo
            .git(top)
            .env("GIT_INDEX_FILE", index_os)
            .worktree()
            .run(&[
                "ls-files",
                "-z",
                "--cached",
                "--others",
                "--exclude-standard",
            ])?,
    )
    .collect();
    let Classified {
        mut excluded,
        oversize,
        left_out,
    } = classify(top, &candidates, &tracked, exclusions, max_file_bytes);
    let mut pathspecs = b".\0".to_vec();
    for path in &left_out {
        pathspecs.extend_from_slice(format!(":(exclude,literal){path}").as_bytes());
        pathspecs.push(0);
    }
    source
        .repo
        .git(top)
        .env("GIT_INDEX_FILE", index_os)
        .writes(&with_objects)
        .literal(false)
        .stdin(&pathspecs)
        .worktree()
        .run(&[
            "add",
            "--all",
            "--pathspec-from-file=-",
            "--pathspec-file-nul",
        ])?;
    // Left out but tracked: still in the index as HEAD had them. Removed.
    let mut remove = Vec::new();
    for path in &left_out {
        if tracked.contains(path) {
            remove.extend_from_slice(path.as_bytes());
            remove.push(0);
        }
    }
    if !remove.is_empty() {
        source
            .repo
            .git(top)
            .env("GIT_INDEX_FILE", index_os)
            .writes(&index_dir)
            .stdin(&remove)
            .run(&["update-index", "-z", "--force-remove", "--stdin"])?;
    }
    let tree = source
        .repo
        .git(top)
        .env("GIT_INDEX_FILE", index_os)
        .writes(&with_objects)
        .text(&["write-tree"])?;
    excluded.sort();
    Ok((tree, excluded, oversize))
}

/// The empty tree, written to the object database.
fn empty_tree(source: &Source) -> ToolResult<String> {
    let tree = source
        .repo
        .git(source.top())
        .writes(&[source.objects()])
        .stdin(b"")
        .text(&["mktree"])?;
    if tree != EMPTY_TREE {
        return Err(ToolError::new(
            ErrorCode::Git,
            "git wrote an unexpected empty tree",
        ));
    }
    Ok(tree)
}

fn commit(source: &Source, tree: &str, parent: Option<&str>, message: &str) -> ToolResult<String> {
    let mut args = vec!["commit-tree", "--no-gpg-sign", tree, "-m", message];
    if let Some(parent) = parent {
        args.extend(["-p", parent]);
    }
    let name = OsStr::new(IDENTITY);
    let email = OsStr::new(EMAIL);
    source
        .repo
        .git(source.top())
        .env("GIT_AUTHOR_NAME", name)
        .env("GIT_AUTHOR_EMAIL", email)
        .env("GIT_COMMITTER_NAME", name)
        .env("GIT_COMMITTER_EMAIL", email)
        .writes(&[source.objects()])
        .text(&args)
}

/// A note as it may go into a commit message: no control characters but
/// line breaks, at most [`MAX_NOTE_BYTES`].
#[must_use]
pub fn clean_note(note: &str) -> String {
    let mut out: String = note
        .chars()
        .filter(|c| *c == '\n' || !c.is_control())
        .collect();
    if out.len() > MAX_NOTE_BYTES {
        let mut end = MAX_NOTE_BYTES;
        while !out.is_char_boundary(end) {
            end -= 1;
        }
        out.truncate(end);
    }
    out.trim().to_owned()
}

/// `HEAD`'s branch's upstream: `(sha, url, branch)`.
fn upstream_of(source: &Source) -> Option<(String, String, String)> {
    let git = || source.repo.git(source.top());
    let branch = git().text(&["symbolic-ref", "-q", "HEAD"]).ok()?;
    let branch = branch.strip_prefix("refs/heads/")?.to_owned();
    let line = git()
        .text(&[
            "for-each-ref",
            "--format=%(upstream)%00%(upstream:remotename)%00%(upstream:remoteref)",
            &format!("refs/heads/{branch}"),
        ])
        .ok()?;
    let mut fields = line.split('\0');
    let (upstream, remote, remote_ref) = (fields.next()?, fields.next()?, fields.next()?);
    if upstream.is_empty() || remote.is_empty() {
        return None;
    }
    let sha = git()
        .text(&[
            "rev-parse",
            "--verify",
            "--quiet",
            &format!("{upstream}^{{commit}}"),
        ])
        .ok()
        .filter(|sha| !sha.is_empty())?;
    let url = git()
        .text(&["config", "--get", &format!("remote.{remote}.url")])
        .ok()
        .filter(|url| !url.is_empty())
        .map_or_else(|| remote.to_owned(), |url| super::redact_url(&url));
    let branch = remote_ref
        .strip_prefix("refs/heads/")
        .unwrap_or(remote_ref)
        .to_owned();
    Some((sha, url, branch))
}

/// The commits of a send (objects only).
struct Commits {
    head: Option<String>,
    base: String,
    upstream: Option<Upstream>,
    handoff: String,
    twin: String,
}

fn commits(source: &Source, tree: &str, note: &str, included: &[Included]) -> ToolResult<Commits> {
    let head = source.head();
    let head_tree = match &head {
        Some(head) => source
            .repo
            .git(source.top())
            .text(&["rev-parse", &format!("{head}^{{tree}}")])?,
        None => empty_tree(source)?,
    };
    let base = commit(
        source,
        &head_tree,
        None,
        &format!(
            "Elitea base\n\nElitea-Base: {}",
            head.as_deref().unwrap_or("none")
        ),
    )?;
    let upstream = if source.shadow || head.is_none() {
        None
    } else {
        match upstream_of(source) {
            Some((sha, url, branch)) => {
                let upstream_tree = source
                    .repo
                    .git(source.top())
                    .text(&["rev-parse", &format!("{sha}^{{tree}}")])?;
                let commit = commit(
                    source,
                    &upstream_tree,
                    None,
                    &format!("Elitea upstream\n\nElitea-Upstream: {sha} {url} {branch}"),
                )?;
                Some(Upstream {
                    commit,
                    sha,
                    url,
                    branch,
                })
            }
            None => None,
        }
    };
    let note = clean_note(note);
    let mut trailers = "Elitea-Handoff: 1".to_owned();
    for file in included {
        // One trailer per path (contract §6.4); `included()` never lets a
        // path with a control character through.
        trailers.push_str("\nElitea-Included: ./");
        trailers.push_str(&file.path);
    }
    let message = if note.is_empty() {
        format!("Elitea handoff\n\n{trailers}")
    } else {
        format!("Elitea handoff\n\n{note}\n\n{trailers}")
    };
    let handoff = commit(source, tree, Some(&base), &message)?;
    let twin = commit(
        source,
        tree,
        head.as_deref(),
        &format!("Elitea handoff (local twin)\n\nElitea-Handoff-Twin: {handoff}"),
    )?;
    Ok(Commits {
        head,
        base,
        upstream,
        handoff,
        twin,
    })
}

/// Take the snapshot: tree, exclusions, scan, caps and the commits (as
/// objects only; no ref is written).
///
/// # Errors
///
/// git failures, an unsafe repository, an unreadable object.
pub fn take(source: &Source, config: &SendConfig) -> ToolResult<Snapshot> {
    check_conversation(&config.conversation_uuid)?;
    let workspace = Workspace::open(source.top(), &[])?;
    let exclusions = Exclusions::new(&workspace, &config.rules())?;
    let (tree, excluded, oversize) =
        snapshot_tree(source, &exclusions, config.caps.max_file_bytes)?;
    // What is really in the tree, checked again.
    let lines = list_tree(source, &tree)?;
    let mut files = Vec::new();
    let mut included = Vec::new();
    for line in &lines {
        if let Some(reason) = exclusions.included_reason(&line.path) {
            included.push(Included {
                path: line.path.clone(),
                reason,
            });
        }
        if exclusions.reason(&line.path).is_some() {
            return Err(ToolError::new(
                ErrorCode::Denied,
                format!(
                    "`{}` is excluded but reached the snapshot; nothing was sent",
                    line.path
                ),
            ));
        }
        if line.kind == "blob" {
            files.push(SizedPath {
                path: line.path.clone(),
                bytes: line.size,
            });
        }
    }
    let report = caps::check(&files, &oversize, &config.caps);
    let db = source.object_db()?;
    let mut findings = Vec::new();
    let mut include_findings = Vec::new();
    for line in &lines {
        // Symlinks travel as their target text; gitlinks as an id.
        if line.kind != "blob" || line.mode == "120000" {
            continue;
        }
        let id = store_objects::oid(&line.oid).map_err(|e| super::store_error(&e))?;
        let bytes = db.blob(&id).map_err(|e| super::store_error(&e))?;
        let found = scan::scan(&line.path, &bytes)?;
        if included.iter().any(|file| file.path == line.path) {
            include_findings.extend(found);
        } else {
            findings.extend(found);
        }
    }
    findings.sort();
    include_findings.sort();
    included.sort();
    let Commits {
        head,
        base,
        upstream,
        handoff,
        twin,
    } = commits(source, &tree, &config.note, &included)?;
    Ok(Snapshot {
        shadow: source.shadow,
        head,
        tree,
        base,
        upstream,
        handoff,
        twin,
        files: files.len(),
        unpacked_bytes: files.iter().map(|file| file.bytes).sum(),
        excluded,
        included,
        findings,
        include_findings,
        caps: report,
        patterns: config.patterns.clone(),
    })
}

/// The refs a send publishes, in order: `base`, `upstream` (when there is
/// one), `handoff/1`.
#[must_use]
pub fn published_refs(snapshot: &Snapshot) -> Vec<(String, String)> {
    let mut refs = vec![(store_refs::BASE.to_owned(), snapshot.base.clone())];
    if let Some(upstream) = &snapshot.upstream {
        refs.push((store_refs::UPSTREAM.to_owned(), upstream.commit.clone()));
    }
    refs.push((store_refs::handoff(1), snapshot.handoff.clone()));
    refs
}

/// How long the pack of a send may take (a 2 GB tree's deltas).
const PACK_TIMEOUT: std::time::Duration = std::time::Duration::from_mins(15);

/// The `COMPACT` segment of the snapshot, deltified: hardened host git
/// (`pack-objects --revs --delta-base-offset`, sandboxed, writing only into
/// a private temporary directory) packs every object reachable from the
/// published refs, `elitea-workspace-store` wraps the pack in the bundle
/// header, and the result is verified with the store's own reader (content
/// address, pack, connectivity from every ref, the refs) before anyone may
/// upload it. The pack is about the size `git bundle create` writes for the
/// same refs.
///
/// # Errors
///
/// A missing object, git failures, a pack that does not verify, or the
/// limits.
pub fn bundle(source: &Source, snapshot: &Snapshot, limits: Limits) -> ToolResult<Written> {
    let db = source.object_db()?;
    let mut refs = Vec::new();
    for (name, sha) in published_refs(snapshot) {
        refs.push((
            name,
            store_objects::oid(&sha).map_err(|e| super::store_error(&e))?,
        ));
    }
    // git writes its temporary pack into the object directory: a private
    // one, which borrows the repository's objects (read-only) as an
    // alternate, so nothing is written to the repository.
    let dir = TempDir::new("pack")?;
    let objects = dir.path().join("objects");
    std::fs::create_dir_all(objects.join("pack"))
        .map_err(|error| ToolError::io("cannot prepare the pack", &error))?;
    let base = dir.path().join("segment");
    let mut tips = Vec::new();
    for (_, id) in &refs {
        tips.extend_from_slice(format!("{id}\n").as_bytes());
    }
    let repo_objects = source.objects();
    let hash = source
        .repo
        .git(source.top())
        .env("GIT_OBJECT_DIRECTORY", objects.as_os_str())
        .env("GIT_ALTERNATE_OBJECT_DIRECTORIES", repo_objects.as_os_str())
        .writes(&[dir.path().to_path_buf()])
        .timeout(PACK_TIMEOUT)
        .stdin(&tips)
        .text(&[
            "pack-objects",
            "--revs",
            "--delta-base-offset",
            "--quiet",
            &base.to_string_lossy(),
        ])?;
    if hash.len() != 40 || !hash.bytes().all(|b| b.is_ascii_hexdigit()) {
        return Err(ToolError::new(
            ErrorCode::Git,
            "git pack-objects named no pack",
        ));
    }
    let pack = std::fs::read(dir.path().join(format!("segment-{hash}.pack")))
        .map_err(|error| ToolError::io("cannot read the pack", &error))?;
    let written = segment::write_compact_with_pack(&db, &refs, &pack, limits)
        .map_err(|e| super::store_error(&e))?;
    drop(pack);
    let expected: std::collections::BTreeMap<String, String> = refs
        .iter()
        .map(|(name, id)| (name.clone(), id.to_string()))
        .collect();
    let quarantine = TempDir::new("verify")?;
    let git_dir = quarantine.path().join("git");
    segment::init_quarantine(&git_dir).map_err(|e| super::store_error(&e))?;
    segment::verify_into(
        &written.bytes,
        &git_dir,
        &segment::Expect {
            sha256: Some(&written.sha256),
            bytes: Some(written.bytes.len() as u64),
            refs: Some(&expected),
        },
        limits,
    )
    .map_err(|e| super::store_error(&e))?;
    Ok(written)
}

/// Record `H_1` and `H'_1` on the private refs of the thread.
///
/// # Errors
///
/// git failures.
pub fn keep_refs(source: &Source, snapshot: &Snapshot, conversation_uuid: &str) -> ToolResult<()> {
    check_conversation(conversation_uuid)?;
    let dir = source
        .refs_dir()
        .join("handoff-local")
        .join(conversation_uuid);
    std::fs::create_dir_all(&dir)
        .map_err(|error| ToolError::io("cannot record the handoff", &error))?;
    for (name, sha) in [("sent", &snapshot.handoff), ("twin", &snapshot.twin)] {
        source
            .repo
            .git(source.top())
            .writes(std::slice::from_ref(&dir))
            .run(&[
                "update-ref",
                &format!("{LOCAL_REF_PREFIX}/{conversation_uuid}/{name}"),
                sha,
            ])?;
    }
    Ok(())
}
