//! Per-call token budgets for tool output, and the session's spill files.
//!
//! Every model step resends every earlier tool result, so one oversized
//! result slows the rest of the turn. Each tool keeps its answer within a
//! budget of estimated tokens (`ceil(bytes / 4)`, [`estimate_tokens`]):
//!
//! | Tool | Default ([`Budgets`]) | Over budget |
//! |---|---|---|
//! | `read_file`, `read_output` | 12k tokens | stops at the last whole line; `truncated`, `next_offset` |
//! | `run_command` | 6k tokens, stdout and stderr together | head and tail kept; the whole output spilled (`output_id`) |
//! | `search_files`, `find_files` | 8k tokens | stops early; `truncated` |
//! | `git_diff`, `git_log` | 8k tokens | the head kept; the whole output spilled |
//! | `read_document` | 12k tokens | the head kept; the whole text spilled |
//!
//! A spilled output is a file under the host's data directory,
//! `<data_dir>/sessions/<session id>/out/<output_id>.txt`, never in the
//! workspace: the directories `0700`, each file created `0600` with
//! `O_CREAT | O_EXCL | O_NOFOLLOW`, a directory or file that is a symlink,
//! another user's, or open to group or others refused. At most
//! [`MAX_SPILL_BYTES`] per session, the oldest dropped first. The session
//! empties the directory when it opens (a policy change reopens it) and
//! removes its files when it ends. `read_output` pages one by its opaque
//! id; it takes no path, and only this session's ids.

use std::collections::VecDeque;
use std::fmt::Write as _;
use std::fs::File;
use std::io::{Read as _, Write as _};
use std::os::fd::OwnedFd;
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::sync::atomic::{AtomicU64, Ordering};

use rustix::fs::{AtFlags, Mode, OFlags};
use serde::Deserialize;
use serde_json::{Value, json};
use sha2::{Digest, Sha256};

use crate::error::{ErrorCode, ToolError, ToolResult};

/// Bytes per estimated token.
pub const BYTES_PER_TOKEN: usize = 4;

/// The most bytes one session keeps in spill files.
pub const MAX_SPILL_BYTES: u64 = 64 * 1024 * 1024;

/// Lines `read_output` returns by default (as `read_file`).
const DEFAULT_PAGE_LINES: usize = 2000;

/// Room kept for the omission markers inside a budget.
const MARKER_ROOM: usize = 256;

/// Estimated tokens of `bytes` bytes of text.
#[must_use]
pub const fn estimate_tokens(bytes: usize) -> usize {
    bytes.div_ceil(BYTES_PER_TOKEN)
}

/// Each tool's budget, in estimated tokens. The host may lower or raise
/// them ([`crate::session::SessionConfig::budgets`]).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Budgets {
    /// `read_file` and `read_output`, per call.
    pub read_file: usize,
    /// `run_command`: stdout and stderr together.
    pub run_command: usize,
    /// `search_files` and `find_files`.
    pub search: usize,
    /// `git_diff` and `git_log`.
    pub git: usize,
    /// `read_document`.
    pub document: usize,
}

impl Default for Budgets {
    fn default() -> Self {
        Self {
            read_file: 12_000,
            run_command: 6_000,
            search: 8_000,
            git: 8_000,
            document: 12_000,
        }
    }
}

/// The bytes `tokens` estimated tokens allow (at least one marker's room).
#[must_use]
pub const fn bytes(tokens: usize) -> usize {
    let bytes = tokens.saturating_mul(BYTES_PER_TOKEN);
    if bytes < 2 * MARKER_ROOM {
        2 * MARKER_ROOM
    } else {
        bytes
    }
}

/// The largest index `<= at` on a char boundary of `text`.
fn floor_boundary(text: &str, at: usize) -> usize {
    let mut at = at.min(text.len());
    while !text.is_char_boundary(at) {
        at -= 1;
    }
    at
}

/// The smallest index `>= at` on a char boundary of `text`.
fn ceil_boundary(text: &str, at: usize) -> usize {
    let mut at = at.min(text.len());
    while !text.is_char_boundary(at) {
        at += 1;
    }
    at
}

/// The start of `text` in at most `max` bytes, cut after the last whole
/// line when one fits in the second half.
#[must_use]
pub fn head(text: &str, max: usize) -> &str {
    if text.len() <= max {
        return text;
    }
    let end = floor_boundary(text, max);
    match text[..end].rfind('\n') {
        Some(newline) if newline >= end / 2 => &text[..=newline],
        _ => &text[..end],
    }
}

/// The end of `text` in at most `max` bytes, from the start of a whole
/// line when one starts in the first half.
fn tail(text: &str, max: usize) -> &str {
    if text.len() <= max {
        return text;
    }
    let start = ceil_boundary(text, text.len() - max);
    match text[start..].find('\n') {
        Some(newline) if newline < max / 2 => &text[start + newline + 1..],
        _ => &text[start..],
    }
}

/// `text` in at most `max` bytes (plus a marker): its head and tail, the
/// middle replaced by a note naming `output_id` when the whole is spilled.
#[must_use]
pub fn head_and_tail(text: &str, max: usize, output_id: Option<&str>) -> String {
    if text.len() <= max {
        return text.to_owned();
    }
    let head = head(text, max / 2);
    let tail = tail(&text[head.len()..], max - head.len());
    let omitted = text.len() - head.len() - tail.len();
    let paging = output_id.map_or_else(String::new, |id| {
        format!("; read_output {{\"output_id\": \"{id}\"}} pages the whole output")
    });
    format!("{head}\n… [{omitted} bytes omitted{paging}] …\n{tail}")
}

/// One page of `text` by lines, numbered as `read_file` numbers them, in
/// at most `max_bytes`: it stops at the last whole line; a single line
/// longer than that is cut (so a page always advances).
#[must_use]
pub fn page(text: &str, offset: Option<usize>, limit: Option<usize>, max_bytes: usize) -> Value {
    let lines: Vec<&str> = text.lines().collect();
    let start = offset.unwrap_or(1).max(1);
    let limit = limit.unwrap_or(DEFAULT_PAGE_LINES).max(1);
    let mut content = String::new();
    let mut end = start.saturating_sub(1);
    for (index, line) in lines.iter().enumerate().skip(start - 1).take(limit) {
        let numbered = format!("{:>6}\t{line}\n", index + 1);
        if content.len() + numbered.len() > max_bytes {
            if content.is_empty() {
                let kept = floor_boundary(line, max_bytes.saturating_sub(MARKER_ROOM));
                content = format!(
                    "{:>6}\t{}… [line cut: {} more bytes]\n",
                    index + 1,
                    &line[..kept],
                    line.len() - kept
                );
                end = index + 1;
            }
            break;
        }
        content.push_str(&numbered);
        end = index + 1;
    }
    let truncated = end < lines.len();
    json!({
        "content": content,
        "start_line": start,
        "end_line": end,
        "total_lines": lines.len(),
        "truncated": truncated,
        "next_offset": truncated.then_some(end + 1),
    })
}

/// Whether `id` has the shape [`SpillStore::spill`] gives: `out-` and 16
/// lowercase hex digits. Nothing else (no path) is ever opened.
fn valid_id(id: &str) -> bool {
    id.len() == 20
        && id.starts_with("out-")
        && id[4..]
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

fn spill_error(what: &str) -> ToolError {
    ToolError::new(
        ErrorCode::Io,
        format!("the session's output store is unavailable: {what}"),
    )
}

/// Refuse a directory or file that is not this user's, or that group or
/// others may use.
fn check_private(fd: &OwnedFd, directory: bool) -> ToolResult<()> {
    let stat = rustix::fs::fstat(fd)?;
    let kind = rustix::fs::FileType::from_raw_mode(stat.st_mode);
    let expected = if directory {
        rustix::fs::FileType::Directory
    } else {
        rustix::fs::FileType::RegularFile
    };
    if kind != expected {
        return Err(spill_error("not a directory or regular file"));
    }
    if stat.st_uid != rustix::process::geteuid().as_raw() {
        return Err(spill_error("owned by another user"));
    }
    if u32::from(stat.st_mode) & 0o077 != 0 {
        return Err(spill_error("open to group or others"));
    }
    Ok(())
}

/// Create (or open) the owner-only directory `name` under `parent`,
/// without following a symlink.
fn private_dir(parent: &OwnedFd, name: &str) -> ToolResult<OwnedFd> {
    match rustix::fs::mkdirat(parent, name, Mode::RWXU) {
        Ok(()) | Err(rustix::io::Errno::EXIST) => {}
        Err(errno) => return Err(errno.into()),
    }
    let fd = rustix::fs::openat(
        parent,
        name,
        OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .map_err(|_| spill_error("a directory is a symlink or cannot be opened"))?;
    check_private(&fd, true)?;
    Ok(fd)
}

#[derive(Default)]
struct Spilled {
    /// This session's files, oldest first, with their sizes.
    files: VecDeque<(String, u64)>,
    total: u64,
}

/// One session's spill files (see the module docs).
pub struct SpillStore {
    data_dir: PathBuf,
    session_id: String,
    /// The workspace root: a data directory inside it gets no spill files.
    workspace_root: PathBuf,
    cap: u64,
    counter: AtomicU64,
    spilled: Mutex<Spilled>,
}

impl std::fmt::Debug for SpillStore {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("SpillStore")
            .field("dir", &self.dir())
            .finish_non_exhaustive()
    }
}

impl SpillStore {
    /// The store of session `session_id` under `data_dir`; nothing is
    /// created until the first spill.
    #[must_use]
    pub fn new(data_dir: &Path, session_id: &str, workspace_root: &Path) -> Self {
        Self {
            data_dir: data_dir.to_path_buf(),
            session_id: session_id.to_owned(),
            workspace_root: workspace_root.to_path_buf(),
            cap: MAX_SPILL_BYTES,
            counter: AtomicU64::new(0),
            spilled: Mutex::new(Spilled::default()),
        }
    }

    /// The same store with another cap (tests).
    #[must_use]
    pub fn with_cap(mut self, cap: u64) -> Self {
        self.cap = cap;
        self
    }

    /// Where the files live.
    #[must_use]
    pub fn dir(&self) -> PathBuf {
        self.data_dir
            .join("sessions")
            .join(&self.session_id)
            .join("out")
    }

    /// Open (creating) `sessions/<id>/out` under the data directory, each
    /// level owner-only and not a symlink.
    fn open_dir(&self) -> ToolResult<OwnedFd> {
        if self.session_id.is_empty()
            || !self
                .session_id
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-' || byte == b'_')
        {
            return Err(spill_error("the session id is not a plain name"));
        }
        std::fs::create_dir_all(&self.data_dir)
            .map_err(|error| ToolError::io("cannot create the data directory", &error))?;
        let data = std::fs::canonicalize(&self.data_dir)
            .map_err(|error| ToolError::io("cannot resolve the data directory", &error))?;
        let root = std::fs::canonicalize(&self.workspace_root)
            .unwrap_or_else(|_| self.workspace_root.clone());
        if data.starts_with(&root) {
            return Err(spill_error("the data directory is inside the workspace"));
        }
        let base = rustix::fs::open(
            &data,
            OFlags::RDONLY | OFlags::DIRECTORY | OFlags::CLOEXEC,
            Mode::empty(),
        )?;
        let sessions = private_dir(&base, "sessions")?;
        let session = private_dir(&sessions, &self.session_id)?;
        private_dir(&session, "out")
    }

    /// Keep `text` (at most the cap) as a new file; its opaque id.
    ///
    /// # Errors
    ///
    /// The directory cannot be made or opened safely, or the file cannot
    /// be written.
    pub fn spill(&self, text: &str) -> ToolResult<String> {
        let dir = self.open_dir()?;
        let kept = head(text, usize::try_from(self.cap).unwrap_or(usize::MAX));
        let size = kept.len() as u64;
        let mut spilled = self
            .spilled
            .lock()
            .map_err(|_| spill_error("the store is poisoned"))?;
        while spilled.total + size > self.cap {
            let Some((old, old_size)) = spilled.files.pop_front() else {
                break;
            };
            let _ = rustix::fs::unlinkat(&dir, format!("{old}.txt"), AtFlags::empty());
            spilled.total -= old_size;
        }
        let id = self.next_id();
        let fd = rustix::fs::openat(
            &dir,
            format!("{id}.txt"),
            OFlags::WRONLY | OFlags::CREATE | OFlags::EXCL | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::RUSR | Mode::WUSR,
        )?;
        let mut file = File::from(fd);
        if let Err(error) = file.write_all(kept.as_bytes()) {
            let _ = rustix::fs::unlinkat(&dir, format!("{id}.txt"), AtFlags::empty());
            return Err(ToolError::io("cannot write the output", &error));
        }
        spilled.files.push_back((id.clone(), size));
        spilled.total += size;
        Ok(id)
    }

    fn next_id(&self) -> String {
        let mut hasher = Sha256::new();
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |since| since.as_nanos());
        hasher.update(now.to_le_bytes());
        hasher.update(std::process::id().to_le_bytes());
        hasher.update(self.counter.fetch_add(1, Ordering::Relaxed).to_le_bytes());
        hasher.update(self.dir().as_os_str().as_encoded_bytes());
        let digest = hasher.finalize();
        let hex = digest[..8]
            .iter()
            .fold(String::with_capacity(16), |mut hex, byte| {
                let _ = write!(hex, "{byte:02x}");
                hex
            });
        format!("out-{hex}")
    }

    /// The text of this session's output `id`.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::InvalidArgument`] for anything but an id this session
    /// gave (a path included), [`ErrorCode::NotFound`] for one dropped
    /// since.
    pub fn read(&self, id: &str) -> ToolResult<String> {
        if !valid_id(id) {
            return Err(ToolError::invalid(
                "output_id is not an output id (read_output takes the id a tool result gave, never a path)",
            ));
        }
        let known = self
            .spilled
            .lock()
            .map_err(|_| spill_error("the store is poisoned"))?
            .files
            .iter()
            .any(|(spilled, _)| spilled == id);
        if !known {
            return Err(ToolError::new(
                ErrorCode::NotFound,
                format!("`{id}` is not an output of this session (or it was dropped)"),
            ));
        }
        let dir = self.open_dir()?;
        let fd = rustix::fs::openat(
            &dir,
            format!("{id}.txt"),
            OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::empty(),
        )
        .map_err(|_| ToolError::new(ErrorCode::NotFound, format!("`{id}` is gone")))?;
        check_private(&fd, false)?;
        let mut text = String::new();
        File::from(fd)
            .take(self.cap)
            .read_to_string(&mut text)
            .map_err(|error| ToolError::io("cannot read the output", &error))?;
        Ok(text)
    }

    /// Remove every file in the directory, this session's or left behind
    /// (a crash, the session before a policy change).
    pub fn clear(&self) {
        if let Ok(mut spilled) = self.spilled.lock() {
            spilled.files.clear();
            spilled.total = 0;
        }
        let dir = self.dir();
        let Ok(entries) = std::fs::read_dir(&dir) else {
            return;
        };
        let Ok(fd) = self.open_dir() else {
            return;
        };
        for entry in entries.flatten() {
            let _ = rustix::fs::unlinkat(&fd, entry.file_name(), AtFlags::empty());
        }
    }

    /// The ids this session holds, oldest first.
    #[must_use]
    pub fn ids(&self) -> Vec<String> {
        self.spilled.lock().map_or_else(
            |_| Vec::new(),
            |spilled| spilled.files.iter().map(|(id, _)| id.clone()).collect(),
        )
    }
}

impl Drop for SpillStore {
    /// The session ended: its files go, and the directories when empty.
    fn drop(&mut self) {
        let ids = self.ids();
        if ids.is_empty() && !self.dir().exists() {
            return;
        }
        if let Ok(fd) = self.open_dir() {
            for id in ids {
                let _ = rustix::fs::unlinkat(&fd, format!("{id}.txt"), AtFlags::empty());
            }
        }
        let out = self.dir();
        let _ = std::fs::remove_dir(&out);
        if let Some(session) = out.parent() {
            let _ = std::fs::remove_dir(session);
        }
    }
}

/// `text` within `max_bytes`: unchanged when it fits; else its head, the
/// whole spilled. The shown text, the output id, and whether it was cut.
pub(crate) fn spill_over(
    spill: &SpillStore,
    text: &str,
    max_bytes: usize,
) -> (String, Option<String>, bool) {
    if text.len() <= max_bytes {
        return (text.to_owned(), None, false);
    }
    let id = spill.spill(text).ok();
    let shown = head(text, max_bytes.saturating_sub(MARKER_ROOM));
    let note = id.as_ref().map_or_else(
        || format!("\n… [{} more bytes not shown] …\n", text.len() - shown.len()),
        |id| {
            format!(
                "\n… [{} more bytes; read_output {{\"output_id\": \"{id}\", \"offset\": n}} pages the whole output] …\n",
                text.len() - shown.len()
            )
        },
    );
    (format!("{shown}{note}"), id, true)
}

/// Bound a command's stdout and stderr together within `max_bytes`: when
/// they do not fit, the whole output is spilled and each stream keeps its
/// head and tail (a short stream whole, the longer one the rest).
pub(crate) fn shape_command(
    output: &mut crate::shell::CommandOutput,
    max_bytes: usize,
    spill: &SpillStore,
) {
    let total = output.stdout.len() + output.stderr.len();
    if total <= max_bytes {
        return;
    }
    let whole = if output.stderr.is_empty() {
        output.stdout.clone()
    } else if output.stdout.is_empty() {
        output.stderr.clone()
    } else {
        format!("[stdout]\n{}\n[stderr]\n{}", output.stdout, output.stderr)
    };
    let id = spill.spill(&whole).ok();
    let room = max_bytes.saturating_sub(2 * MARKER_ROOM);
    let half = room / 2;
    let out_share = output
        .stdout
        .len()
        .min(half.max(room.saturating_sub(output.stderr.len())));
    let err_share = output.stderr.len().min(room - out_share);
    if output.stdout.len() > out_share {
        output.stdout = head_and_tail(&output.stdout, out_share, id.as_deref());
        output.stdout_truncated = true;
    }
    if output.stderr.len() > err_share {
        output.stderr = head_and_tail(&output.stderr, err_share, id.as_deref());
        output.stderr_truncated = true;
    }
    output.output_id = id;
}

#[derive(Deserialize)]
struct ReadOutputArgs {
    output_id: String,
    #[serde(default)]
    offset: Option<usize>,
    #[serde(default)]
    limit: Option<usize>,
}

/// `read_output`: a page of one of this session's spilled outputs, by id.
pub(crate) fn read_output(spill: &SpillStore, args: Value, max_bytes: usize) -> ToolResult<Value> {
    let args: ReadOutputArgs = serde_json::from_value(args)
        .map_err(|error| ToolError::invalid(format!("bad arguments: {error}")))?;
    let text = spill.read(&args.output_id)?;
    let mut page = page(&text, args.offset, args.limit, max_bytes);
    if let Value::Object(map) = &mut page {
        map.insert("output_id".to_owned(), Value::String(args.output_id));
    }
    Ok(page)
}

#[cfg(test)]
mod tests {
    use std::os::unix::fs::PermissionsExt;

    use serde_json::json;

    use super::{Budgets, SpillStore, bytes, estimate_tokens, head_and_tail, read_output};
    use crate::error::ErrorCode;
    use crate::ledger::ReadLedger;
    use crate::workspace::Workspace;

    fn store(dir: &tempfile::TempDir) -> SpillStore {
        std::fs::create_dir_all(dir.path().join("ws")).expect("ws");
        SpillStore::new(&dir.path().join("data"), "conv-1", &dir.path().join("ws"))
    }

    #[test]
    fn read_file_is_token_capped() {
        let dir = tempfile::tempdir().expect("dir");
        let line = "x".repeat(99);
        let text: String = std::iter::repeat_n(format!("{line}\n"), 20_000).collect();
        assert!(text.len() >= 2_000_000);
        std::fs::write(dir.path().join("big.txt"), &text).expect("write");
        let ws = Workspace::open(dir.path(), &[]).expect("ws");
        let budget = bytes(Budgets::default().read_file);
        let read = crate::files::read(
            &ws,
            &ReadLedger::new(),
            json!({ "path": "big.txt" }),
            budget,
        )
        .expect("read");
        let content = read["content"].as_str().expect("content");
        assert!(
            estimate_tokens(content.len()) <= 12_000,
            "{}",
            content.len()
        );
        assert!(content.ends_with('\n'), "whole lines only");
        assert_eq!(read["truncated"], true);
        let next = read["next_offset"].as_u64().expect("next_offset");
        assert_eq!(read["end_line"].as_u64(), Some(next - 1));
        let again = crate::files::read(
            &ws,
            &ReadLedger::new(),
            json!({ "path": "big.txt", "offset": next }),
            budget,
        )
        .expect("page");
        assert_eq!(again["start_line"].as_u64(), Some(next));
        // One line longer than the budget is cut, and the page advances.
        std::fs::write(dir.path().join("one.txt"), "y".repeat(100_000)).expect("write");
        let one = crate::files::read(
            &ws,
            &ReadLedger::new(),
            json!({ "path": "one.txt" }),
            budget,
        )
        .expect("read");
        assert!(one["content"].as_str().expect("content").len() <= budget);
        assert_eq!(one["end_line"], 1);
    }

    #[test]
    fn read_output_refuses_paths_and_foreign_ids() {
        let dir = tempfile::tempdir().expect("dir");
        let ours = store(&dir);
        let id = ours.spill("one\ntwo\nthree\n").expect("spill");
        let page = read_output(
            &ours,
            json!({ "output_id": id, "offset": 2, "limit": 1 }),
            4096,
        )
        .expect("page");
        assert_eq!(page["content"], "     2\ttwo\n");
        assert_eq!(page["next_offset"], 3);
        for path in [
            "../../../etc/passwd",
            "/etc/passwd",
            "out-../../x",
            "sessions/conv-1/out/x",
            "",
        ] {
            let refused = read_output(&ours, json!({ "output_id": path }), 4096)
                .expect_err("a path is refused");
            assert_eq!(refused.code(), ErrorCode::InvalidArgument, "{path}");
        }
        // Another session's id, even in the same directory, is not ours.
        let other = SpillStore::new(&dir.path().join("data"), "conv-1", &dir.path().join("ws"));
        let foreign = other.spill("theirs\n").expect("spill");
        assert_eq!(
            read_output(&ours, json!({ "output_id": foreign }), 4096)
                .expect_err("foreign")
                .code(),
            ErrorCode::NotFound
        );
        assert_eq!(
            read_output(&ours, json!({ "output_id": "out-0123456789abcdef" }), 4096)
                .expect_err("unknown")
                .code(),
            ErrorCode::NotFound
        );
        // A symlink planted in place of our file is not followed.
        let file = ours.dir().join(format!("{id}.txt"));
        std::fs::remove_file(&file).expect("rm");
        std::os::unix::fs::symlink("/etc/hosts", &file).expect("link");
        assert!(read_output(&ours, json!({ "output_id": id }), 4096).is_err());
    }

    #[test]
    fn spill_files_are_private_outside_the_workspace_and_wiped() {
        let dir = tempfile::tempdir().expect("dir");
        let first = store(&dir);
        let id = first.spill("secret output\n").expect("spill");
        let file = first.dir().join(format!("{id}.txt"));
        let mode = |path: &std::path::Path| {
            std::fs::metadata(path).expect("meta").permissions().mode() & 0o777
        };
        assert_eq!(mode(&file), 0o600);
        assert_eq!(mode(&first.dir()), 0o700);
        assert_eq!(mode(&dir.path().join("data/sessions")), 0o700);
        assert!(!file.starts_with(dir.path().join("ws")));
        // A policy change reopens the session: the new one empties the
        // directory before it spills anything.
        let second = store(&dir);
        second.clear();
        assert!(!file.exists());
        let kept = second.spill("new\n").expect("spill");
        // The session ends: its files and directories go.
        drop(first);
        let kept_file = second.dir().join(format!("{kept}.txt"));
        assert!(kept_file.exists(), "another live store's file stays");
        let out = second.dir();
        drop(second);
        assert!(!kept_file.exists());
        assert!(!out.exists());
        // A data directory inside the workspace gets no spill files.
        let inside = SpillStore::new(&dir.path().join("ws/data"), "s", &dir.path().join("ws"));
        assert!(inside.spill("x").is_err());
        // A directory open to others is refused.
        let open = store(&dir);
        open.spill("x").expect("spill");
        std::fs::set_permissions(open.dir(), std::fs::Permissions::from_mode(0o755))
            .expect("chmod");
        assert!(open.spill("y").is_err());
        std::fs::set_permissions(open.dir(), std::fs::Permissions::from_mode(0o700))
            .expect("chmod");
    }

    #[test]
    fn the_cap_drops_the_oldest_first() {
        let dir = tempfile::tempdir().expect("dir");
        let small = store(&dir).with_cap(10);
        let a = small.spill("aaaa").expect("a");
        let b = small.spill("bbbb").expect("b");
        let c = small.spill("cccc").expect("c");
        assert_eq!(small.ids(), [b.clone(), c]);
        assert_eq!(
            small.read(&a).expect_err("dropped").code(),
            ErrorCode::NotFound
        );
        assert_eq!(small.read(&b).expect("b"), "bbbb");
    }

    #[test]
    fn head_and_tail_keep_both_ends_within_the_budget() {
        let text: String = (1..=1000).map(|n| n.to_string() + "\n").collect();
        let shown = head_and_tail(&text, 200, Some("out-0123456789abcdef"));
        assert!(shown.starts_with("1\n2\n"));
        assert!(shown.ends_with("1000\n"));
        assert!(shown.contains("out-0123456789abcdef"));
        assert!(shown.len() <= 200 + 256);
    }
}
