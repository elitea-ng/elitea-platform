//! The pull-request diff of a cloud turn (ADR-0034 decision 10), computed by
//! the worker from its own unpacked repository, outside the sandbox: from
//! the desktop's upstream commit U (`refs/elitea/upstream`, read from the
//! turn's verified quarantine) to the workspace as it is now (moved out of
//! the sandbox and redacted, as the end-of-turn commit would be).
//!
//! Main reads no git pack, so it no longer computes this. The limits are the
//! GitHub family's patch bounds and are checked before anything is
//! rendered: text only (a NUL in a changed file's first 8000 bytes, a
//! changed symlink or submodule is binary), at most 1 MiB of diff and 100
//! files; no change at all is refused too. The rendering is git's unified
//! format (Myers diff, 3 lines of context), the shape the family's patch
//! reader applies; renames are a delete plus a create, and a mode-only
//! change is not part of it (the patch path writes contents, not modes).

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;
use std::path::Path;

use elitea_agent_runtime::toolkits::workspace_pull_request::{DiffUnavailable, WorkspaceDiff};
use elitea_workspace_store::objects::{FileEntry, ObjectDb, oid};

pub(crate) const MAX_DIFF_BYTES: usize = 1 << 20;
pub(crate) const MAX_DIFF_FILES: usize = 100;
const CONTEXT_LINES: usize = 3;
const BINARY_SNIFF_BYTES: usize = 8000;
const MODE_SYMLINK: u16 = 0o120_000;
const MODE_GITLINK: u16 = 0o160_000;

/// The upstream the desktop recorded on U (`Elitea-Upstream: <sha> <url>
/// <branch>`); the last such trailer wins, as git trailers do.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct Upstream {
    pub(crate) sha: String,
    pub(crate) remote_url: String,
    pub(crate) branch: String,
}

#[must_use]
pub(crate) fn parse_upstream_trailer(message: &str) -> Option<Upstream> {
    let mut found = None;
    for line in message.lines() {
        let Some(value) = line.trim_end_matches('\r').strip_prefix("Elitea-Upstream:") else {
            continue;
        };
        let fields: Vec<&str> = value.split_whitespace().collect();
        if let [sha, url, branch] = fields.as_slice()
            && super::context::sha1_hex(sha)
            && !branch.is_empty()
            && branch.len() <= 255
        {
            found = Some(Upstream {
                sha: (*sha).to_owned(),
                remote_url: strip_url_credentials(url),
                branch: (*branch).to_owned(),
            });
        }
    }
    found
}

/// A remote URL without its user-info part, so no credential the desktop's
/// remote carried reaches a pull request or a log.
#[must_use]
pub(crate) fn strip_url_credentials(remote: &str) -> String {
    if let Some((scheme, rest)) = remote.split_once("://") {
        let (authority, path) = rest.split_once('/').map_or((rest, ""), |(a, p)| (a, p));
        let host = authority
            .rsplit_once('@')
            .map_or(authority, |(_, host)| host);
        return if path.is_empty() && !rest.contains('/') {
            format!("{scheme}://{host}")
        } else {
            format!("{scheme}://{host}/{path}")
        };
    }
    // scp-like `git@github.com:org/repo.git`.
    match remote.split_once('@') {
        Some((user, host)) if !user.contains('/') && host.contains(':') => host.to_owned(),
        _ => remote.to_owned(),
    }
}

/// One changed file: its contents on each side (`None`: absent).
struct Change {
    path: String,
    old: Option<Vec<u8>>,
    new: Option<Vec<u8>>,
}

fn text_content(bytes: Vec<u8>) -> Result<Vec<u8>, DiffUnavailable> {
    if bytes.len() > MAX_DIFF_BYTES {
        return Err(DiffUnavailable::TooLarge);
    }
    if bytes[..bytes.len().min(BINARY_SNIFF_BYTES)].contains(&0) {
        return Err(DiffUnavailable::Binary);
    }
    Ok(bytes)
}

fn blob_id(length: u64, mut reader: impl std::io::Read) -> Option<gix_hash::ObjectId> {
    let mut hasher = gix_hash::hasher(gix_hash::Kind::Sha1);
    hasher.update(format!("blob {length}\0").as_bytes());
    let mut buffer = vec![0_u8; 64 * 1024];
    loop {
        let count = reader.read(&mut buffer).ok()?;
        if count == 0 {
            break;
        }
        hasher.update(&buffer[..count]);
    }
    hasher.try_finalize().ok()
}

fn walk(
    root: &Path,
    directory: &Path,
    out: &mut BTreeMap<String, std::path::PathBuf>,
) -> std::io::Result<()> {
    for entry in std::fs::read_dir(directory)? {
        let entry = entry?;
        if entry.file_name().eq_ignore_ascii_case(".git") {
            continue;
        }
        let path = entry.path();
        if entry.file_type()?.is_dir() {
            walk(root, &path, out)?;
        } else if let Ok(relative) = path.strip_prefix(root) {
            out.insert(relative.to_string_lossy().replace('\\', "/"), path);
        }
    }
    Ok(())
}

fn failed() -> DiffUnavailable {
    DiffUnavailable::Failed("The workspace could not be read; no pull request was opened.")
}

/// The diff from U's tree (in `db`) to the files under `worktree`.
///
/// # Errors
///
/// [`DiffUnavailable::Empty`], `TooLarge`, `Binary`, or a read failure.
pub(crate) fn workspace_diff(
    db: &ObjectDb,
    upstream_commit: &str,
    worktree: &Path,
) -> Result<(String, usize), DiffUnavailable> {
    let id = oid(upstream_commit).map_err(|_| DiffUnavailable::NoUpstream)?;
    let commit = db.commit(&id).map_err(|_| DiffUnavailable::NoUpstream)?;
    let before: BTreeMap<String, FileEntry> = db.flatten(&commit.tree).map_err(|_| failed())?;
    let mut after = BTreeMap::new();
    walk(worktree, worktree, &mut after).map_err(|_| failed())?;
    let mut changes: Vec<Change> = Vec::new();
    let mut paths: BTreeSet<&String> = before.keys().collect();
    paths.extend(after.keys());
    for path in paths {
        let old = before.get(path);
        // Submodule commits have no content in the segment and are never
        // checked out: they are not part of a pull request.
        if old.is_some_and(|entry| entry.mode == MODE_GITLINK) {
            continue;
        }
        let new = after.get(path);
        let new_meta = match new {
            Some(file) => Some(std::fs::symlink_metadata(file).map_err(|_| failed())?),
            None => None,
        };
        let old_is_link = old.is_some_and(|entry| entry.mode == MODE_SYMLINK);
        let new_is_link = new_meta
            .as_ref()
            .is_some_and(|meta| meta.file_type().is_symlink());
        match (old, new, &new_meta) {
            (Some(entry), Some(file), Some(meta)) if !old_is_link && !new_is_link => {
                let same = std::fs::File::open(file)
                    .ok()
                    .and_then(|handle| blob_id(meta.len(), handle))
                    .is_some_and(|id| id == entry.oid);
                if same {
                    continue;
                }
            }
            (Some(entry), Some(file), _) if old_is_link && new_is_link => {
                let target = std::fs::read_link(file).map_err(|_| failed())?;
                let old_target = db.blob(&entry.oid).map_err(|_| failed())?;
                if target.to_string_lossy().as_bytes() == old_target.as_slice() {
                    continue;
                }
                return Err(DiffUnavailable::Binary);
            }
            _ => {}
        }
        if old_is_link || new_is_link {
            return Err(DiffUnavailable::Binary);
        }
        changes.push(Change {
            path: path.clone(),
            old: None,
            new: None,
        });
        if changes.len() > MAX_DIFF_FILES {
            return Err(DiffUnavailable::TooLarge);
        }
    }
    if changes.is_empty() {
        return Err(DiffUnavailable::Empty);
    }
    for change in &mut changes {
        if let Some(entry) = before.get(&change.path) {
            change.old = Some(text_content(db.blob(&entry.oid).map_err(|_| failed())?)?);
        }
        if let Some(file) = after.get(&change.path) {
            let length = std::fs::metadata(file).map_err(|_| failed())?.len();
            if length > MAX_DIFF_BYTES as u64 {
                return Err(DiffUnavailable::TooLarge);
            }
            change.new = Some(text_content(std::fs::read(file).map_err(|_| failed())?)?);
        }
    }
    render(&changes)
}

fn render(changes: &[Change]) -> Result<(String, usize), DiffUnavailable> {
    let mut out = String::new();
    let mut files = 0;
    for change in changes {
        if let (Some(old), Some(new)) = (&change.old, &change.new)
            && old == new
        {
            // Mode-only: nothing the patch path writes.
            continue;
        }
        let as_text = |bytes: &Option<Vec<u8>>| {
            bytes
                .as_deref()
                .map(|bytes| String::from_utf8_lossy(bytes).into_owned())
        };
        write_file_diff(
            &mut out,
            &change.path,
            as_text(&change.old).as_deref(),
            as_text(&change.new).as_deref(),
        );
        files += 1;
        if out.len() > MAX_DIFF_BYTES {
            return Err(DiffUnavailable::TooLarge);
        }
    }
    if files == 0 {
        return Err(DiffUnavailable::Empty);
    }
    Ok((out, files))
}

fn write_file_diff(out: &mut String, path: &str, old: Option<&str>, new: Option<&str>) {
    let _ = writeln!(out, "diff --git a/{path} b/{path}");
    match (&old, &new) {
        (None, _) => {
            out.push_str("new file mode 100644\n");
            let _ = write!(out, "--- /dev/null\n+++ b/{path}\n");
        }
        (_, None) => {
            out.push_str("deleted file mode 100644\n");
            let _ = write!(out, "--- a/{path}\n+++ /dev/null\n");
        }
        _ => {
            let _ = write!(out, "--- a/{path}\n+++ b/{path}\n");
        }
    }
    write_hunks(
        out,
        &split_lines(old.unwrap_or("")),
        &split_lines(new.unwrap_or("")),
    );
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct Line<'a> {
    text: &'a str,
    no_newline: bool,
}

fn split_lines(content: &str) -> Vec<Line<'_>> {
    if content.is_empty() {
        return Vec::new();
    }
    let (text, trailing) = match content.strip_suffix('\n') {
        Some(text) => (text, true),
        None => (content, false),
    };
    let mut lines: Vec<Line<'_>> = text
        .split('\n')
        .map(|text| Line {
            text,
            no_newline: false,
        })
        .collect();
    if !trailing && let Some(last) = lines.last_mut() {
        last.no_newline = true;
    }
    lines
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Kind {
    Equal,
    Delete,
    Insert,
}

#[derive(Clone, Copy, Debug)]
struct Op {
    kind: Kind,
    old: Option<usize>,
    new: Option<usize>,
}

/// Myers' O(ND) diff.
#[allow(
    clippy::many_single_char_names,
    clippy::cast_possible_wrap,
    clippy::cast_sign_loss,
    clippy::cast_possible_truncation,
    reason = "Myers' names; indexes bounded by the 1 MiB diff cap"
)]
fn diff_lines(a: &[Line<'_>], b: &[Line<'_>]) -> Vec<Op> {
    let (n, m) = (a.len() as isize, b.len() as isize);
    let max = n + m;
    let offset = max + 1;
    let mut v = vec![0_isize; (2 * max + 2) as usize];
    let mut trace: Vec<Vec<isize>> = Vec::new();
    for d in 0..=max {
        trace.push(v.clone());
        let mut k = -d;
        while k <= d {
            let mut x = if k == -d
                || (k != d && v[(offset + k - 1) as usize] < v[(offset + k + 1) as usize])
            {
                v[(offset + k + 1) as usize]
            } else {
                v[(offset + k - 1) as usize] + 1
            };
            let mut y = x - k;
            while x < n && y < m && a[x as usize] == b[y as usize] {
                x += 1;
                y += 1;
            }
            v[(offset + k) as usize] = x;
            if x >= n && y >= m {
                return backtrack(&trace, a, b, offset, d);
            }
            k += 2;
        }
    }
    Vec::new()
}

#[allow(
    clippy::cast_possible_wrap,
    clippy::cast_sign_loss,
    clippy::many_single_char_names,
    reason = "Myers' names; indexes bounded by the 1 MiB diff cap"
)]
fn backtrack(
    trace: &[Vec<isize>],
    a: &[Line<'_>],
    b: &[Line<'_>],
    offset: isize,
    depth: isize,
) -> Vec<Op> {
    let (mut x, mut y) = (a.len() as isize, b.len() as isize);
    let mut ops = Vec::new();
    let mut d = depth;
    while d > 0 {
        let v = &trace[d as usize];
        let k = x - y;
        let previous_k =
            if k == -d || (k != d && v[(offset + k - 1) as usize] < v[(offset + k + 1) as usize]) {
                k + 1
            } else {
                k - 1
            };
        let previous_x = v[(offset + previous_k) as usize];
        let previous_y = previous_x - previous_k;
        while x > previous_x && y > previous_y {
            x -= 1;
            y -= 1;
            ops.push(Op {
                kind: Kind::Equal,
                old: Some(x as usize),
                new: Some(y as usize),
            });
        }
        if x == previous_x {
            y -= 1;
            ops.push(Op {
                kind: Kind::Insert,
                old: None,
                new: Some(y as usize),
            });
        } else {
            x -= 1;
            ops.push(Op {
                kind: Kind::Delete,
                old: Some(x as usize),
                new: None,
            });
        }
        d -= 1;
    }
    while x > 0 && y > 0 {
        x -= 1;
        y -= 1;
        ops.push(Op {
            kind: Kind::Equal,
            old: Some(x as usize),
            new: Some(y as usize),
        });
    }
    ops.reverse();
    ops
}

fn write_hunks(out: &mut String, a: &[Line<'_>], b: &[Line<'_>]) {
    let ops = diff_lines(a, b);
    let mut index = 0;
    while index < ops.len() {
        while index < ops.len() && ops[index].kind == Kind::Equal {
            index += 1;
        }
        if index >= ops.len() {
            return;
        }
        let start = index.saturating_sub(CONTEXT_LINES);
        let mut end = index;
        while end < ops.len() {
            if ops[end].kind != Kind::Equal {
                end += 1;
                continue;
            }
            let mut run = end;
            while run < ops.len() && ops[run].kind == Kind::Equal {
                run += 1;
            }
            if run >= ops.len() || run - end > 2 * CONTEXT_LINES {
                end += CONTEXT_LINES.min(run - end);
                break;
            }
            end = run;
        }
        write_hunk(out, &ops[start..end], a, b);
        index = end;
    }
}

fn write_hunk(out: &mut String, ops: &[Op], a: &[Line<'_>], b: &[Line<'_>]) {
    let (mut old_start, mut new_start, mut old_count, mut new_count) = (None, None, 0, 0);
    for op in ops {
        if op.kind != Kind::Insert {
            old_start = old_start.or(op.old);
            old_count += 1;
        }
        if op.kind != Kind::Delete {
            new_start = new_start.or(op.new);
            new_count += 1;
        }
    }
    let _ = writeln!(
        out,
        "@@ -{} +{} @@",
        hunk_range(old_start, old_count, ops, true),
        hunk_range(new_start, new_count, ops, false)
    );
    for op in ops {
        let (sign, line) = match op.kind {
            Kind::Insert => ('+', &b[op.new.unwrap_or(0)]),
            Kind::Delete => ('-', &a[op.old.unwrap_or(0)]),
            Kind::Equal => (' ', &a[op.old.unwrap_or(0)]),
        };
        out.push(sign);
        out.push_str(line.text);
        out.push('\n');
        if line.no_newline {
            out.push_str("\\ No newline at end of file\n");
        }
    }
}

/// git's conventions: a zero count names the line before the hunk, and a
/// count of one is written alone.
fn hunk_range(start: Option<usize>, count: usize, ops: &[Op], old: bool) -> String {
    if count == 0 {
        let position = ops
            .iter()
            .find_map(|op| if old { op.old } else { op.new })
            .unwrap_or(0);
        return format!("{position},0");
    }
    let start = start.unwrap_or(0) + 1;
    if count == 1 {
        format!("{start}")
    } else {
        format!("{start},{count}")
    }
}

/// The diff and what it is opened against.
///
/// # Errors
///
/// As [`workspace_diff`]; `NoUpstream` when U has no `Elitea-Upstream`.
pub(crate) fn pull_request_diff(
    db: &ObjectDb,
    upstream_commit: &str,
    worktree: &Path,
) -> Result<WorkspaceDiff, DiffUnavailable> {
    let id = oid(upstream_commit).map_err(|_| DiffUnavailable::NoUpstream)?;
    let commit = db.commit(&id).map_err(|_| DiffUnavailable::NoUpstream)?;
    let upstream = parse_upstream_trailer(&commit.message).ok_or(DiffUnavailable::NoUpstream)?;
    let (text, _) = workspace_diff(db, upstream_commit, worktree)?;
    Ok(WorkspaceDiff {
        upstream_sha: upstream.sha,
        upstream_branch: upstream.branch,
        remote_url: upstream.remote_url,
        text,
    })
}

#[cfg(test)]
#[path = "pr_diff_tests.rs"]
mod tests;
