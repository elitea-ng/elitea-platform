//! Per-call token budgets for tool output, and the session's spill files.
//!
//! Every model step resends every earlier tool result, so one oversized
//! result slows the rest of the turn. Each tool keeps its answer within a
//! budget of estimated tokens (`ceil(bytes / 4)`, the runtime's shared
//! [`elitea_agent_runtime::context_budget::estimate_tokens`]):
//!
//! | Tool | Default ([`Budgets`]) | Over budget |
//! |---|---|---|
//! | `read_file`, `read_output` | 12k tokens | stops at the last whole line; `truncated`, `next_offset` (and `next_column` inside a line too long for one page) |
//! | `run_command` | 6k tokens, stdout and stderr together | head and tail kept; the whole output spilled (`output_id`) |
//! | `search_files`, `find_files` | 8k tokens | stops early; `truncated` |
//! | `git_diff`, `git_log` | 8k tokens | the head kept; the whole output spilled |
//! | `read_document` | 12k tokens | the head kept; the whole text spilled |
//!
//! A spilled output is a file under the host's data directory,
//! `<data_dir>/sessions/<session id>/out/<conversation>/<output_id>.txt`
//! (with a line index beside it, `<output_id>.idx`), never one the agent
//! can read through its file tools: the data directory is on the session's
//! deny list, so a workspace that contains it (a home folder) still spills.
//! The directories are `0700`, each file created `0600` with
//! `O_CREAT | O_EXCL | O_NOFOLLOW`; a directory or file that is a symlink,
//! another user's, or open to group or others is refused.
//!
//! Outputs belong to the conversation whose turn made them
//! ([`SpillStore::set_conversation`]): `read_output` pages only the
//! calling conversation's ids, by opaque id (it takes no path). At most
//! [`MAX_SPILL_BYTES`] per conversation and [`MAX_SPILL_BYTES_TOTAL`] per
//! session are kept, the oldest dropped first (a conversation over its own
//! cap drops its own oldest; the session cap drops the oldest of any).
//! Outputs outlive a session reopen (a policy change) and an app restart
//! for [`SPILL_RETENTION`]; then they go, as do a conversation's outputs
//! when its local history is deleted ([`forget_conversation_outputs`]).
//! A gone id answers [`ErrorCode::OutputExpired`]: re-run the tool.

use std::collections::VecDeque;
use std::fmt::Write as _;
use std::fs::File;
use std::io::{BufRead, BufReader, Read as _, Seek as _, SeekFrom, Write as _};
use std::os::fd::OwnedFd;
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, SystemTime};

use rustix::fs::{AtFlags, Mode, OFlags};
use serde::Deserialize;
use serde_json::{Value, json};
use sha2::{Digest, Sha256};

use crate::error::{ErrorCode, ToolError, ToolResult};

/// Bytes per estimated token (the runtime's shared heuristic).
pub const BYTES_PER_TOKEN: usize = elitea_agent_runtime::context_budget::BYTES_PER_TOKEN;

/// The most bytes one conversation keeps in spill files.
pub const MAX_SPILL_BYTES: u64 = 64 * 1024 * 1024;

/// The most bytes one session (a workspace's conversations together)
/// keeps in spill files.
pub const MAX_SPILL_BYTES_TOTAL: u64 = 256 * 1024 * 1024;

/// How long a spilled output is kept.
pub const SPILL_RETENTION: Duration = Duration::from_hours(24);

/// Lines `read_output` returns by default (as `read_file`).
const DEFAULT_PAGE_LINES: usize = 2000;

/// Room kept for the omission markers inside a budget.
const MARKER_ROOM: usize = 256;

/// The spill index records the byte offset of every this many lines.
const INDEX_STRIDE: usize = 256;

/// The scope of outputs made outside any conversation.
const DEFAULT_SCOPE: &str = "default";

/// Estimated tokens of `bytes` bytes of text.
#[must_use]
pub fn estimate_tokens(bytes: usize) -> usize {
    usize::try_from(elitea_agent_runtime::context_budget::estimate_tokens(bytes))
        .unwrap_or(usize::MAX)
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

/// Which page to show: `read_file`'s and `read_output`'s arguments.
#[derive(Clone, Copy, Debug, Default)]
pub struct PageRequest {
    /// First line, 1-based.
    pub offset: Option<usize>,
    /// Byte offset inside the first line to resume from (the
    /// `next_column` a cut line gave); past the line's end it shows empty.
    pub column: Option<usize>,
    /// Lines at most (a cut line counts as one).
    pub limit: Option<usize>,
}

/// One page of the lines `reader` yields (the first numbered
/// `first_line`, `total_lines` in all), numbered as `read_file` numbers
/// them, in at most `max_bytes`: it stops at the last whole line. A line
/// that alone does not fit is cut, and `next_offset` with `next_column`
/// resume inside it, so every byte of every line is reachable.
fn page_lines<R: BufRead>(
    reader: &mut R,
    first_line: usize,
    total_lines: usize,
    request: PageRequest,
    max_bytes: usize,
) -> std::io::Result<Value> {
    let start = request.offset.unwrap_or(1).max(1);
    let limit = request.limit.unwrap_or(DEFAULT_PAGE_LINES).max(1);
    let mut line_no = first_line.max(1);
    while line_no < start {
        if reader.skip_until(b'\n')? == 0 {
            break;
        }
        line_no += 1;
    }
    let column = request.column.unwrap_or(0);
    let mut content = String::new();
    let mut end = start - 1;
    // Where the next page starts: a line, and a byte inside it.
    let mut next: Option<(usize, usize)> = None;
    let mut shown = 0usize;
    let mut buffer = Vec::new();
    let mut start_column = 0usize;
    while shown < limit && line_no >= start {
        buffer.clear();
        if reader.read_until(b'\n', &mut buffer)? == 0 {
            break;
        }
        if buffer.last() == Some(&b'\n') {
            buffer.pop();
            if buffer.last() == Some(&b'\r') {
                buffer.pop();
            }
        }
        let line = String::from_utf8_lossy(&buffer);
        let from = if shown == 0 {
            ceil_boundary(&line, column)
        } else {
            0
        };
        if shown == 0 {
            start_column = from;
        }
        let rest = &line[from..];
        let prefix = format!("{line_no:>6}\t");
        if content.len() + prefix.len() + rest.len() + 1 > max_bytes {
            if content.is_empty() {
                let room = max_bytes.saturating_sub(MARKER_ROOM + prefix.len());
                let mut kept = floor_boundary(rest, room);
                if kept == 0 {
                    // Always advance, by one character at least.
                    kept = ceil_boundary(rest, 1);
                }
                let resume = from + kept;
                let _ = writeln!(
                    content,
                    "{prefix}{}… [line cut: {} more bytes; next_column {resume}]",
                    &rest[..kept],
                    rest.len() - kept
                );
                end = line_no;
                if kept < rest.len() {
                    next = Some((line_no, resume));
                }
            } else {
                next = Some((line_no, 0));
            }
            break;
        }
        content.push_str(&prefix);
        content.push_str(rest);
        content.push('\n');
        end = line_no;
        shown += 1;
        line_no += 1;
    }
    if next.is_none() && end < total_lines {
        next = Some((end + 1, 0));
    }
    let mut page = json!({
        "content": content,
        "start_line": start,
        "end_line": end,
        "total_lines": total_lines,
        "truncated": next.is_some(),
        "next_offset": next.map(|(line, _)| line),
    });
    if let Value::Object(map) = &mut page {
        if start_column > 0 {
            map.insert("start_column".to_owned(), json!(start_column));
        }
        if let Some((_, column)) = next.filter(|(_, column)| *column > 0) {
            map.insert("next_column".to_owned(), json!(column));
        }
    }
    Ok(page)
}

/// One page of `text` by lines, `read_file`'s page: whole lines in at most
/// `max_bytes`; a line longer than a page is cut and continues at
/// `next_offset` with `next_column`.
#[must_use]
pub fn page(text: &str, request: PageRequest, max_bytes: usize) -> Value {
    let total = text.lines().count();
    page_lines(
        &mut std::io::Cursor::new(text.as_bytes()),
        1,
        total,
        request,
        max_bytes,
    )
    .unwrap_or_else(|_| json!({ "content": "", "truncated": false }))
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

/// Whether `name` is a scope directory [`scope_of`] makes.
fn valid_scope(name: &str) -> bool {
    name == DEFAULT_SCOPE
        || (name.len() == 18
            && name.starts_with("c-")
            && name[2..]
                .bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte)))
}

fn hex16(bytes: &[u8]) -> String {
    bytes[..8]
        .iter()
        .fold(String::with_capacity(16), |mut hex, byte| {
            let _ = write!(hex, "{byte:02x}");
            hex
        })
}

/// The directory name of a conversation's outputs: a digest of its id,
/// so any id the host uses is a plain name.
fn scope_of(conversation: Option<&str>) -> String {
    conversation.map_or_else(
        || DEFAULT_SCOPE.to_owned(),
        |conversation| {
            let digest = Sha256::digest(conversation.as_bytes());
            format!("c-{}", hex16(&digest))
        },
    )
}

fn spill_error(what: &str) -> ToolError {
    ToolError::new(
        ErrorCode::Io,
        format!("the session's output store is unavailable: {what}"),
    )
}

fn expired(id: &str) -> ToolError {
    ToolError::new(
        ErrorCode::OutputExpired,
        format!(
            "`{id}` is no longer kept (outputs are kept {} hours and at most {} MiB per conversation, and only for the conversation that made them): re-run the tool that produced it",
            SPILL_RETENTION.as_secs() / 3600,
            MAX_SPILL_BYTES / (1024 * 1024)
        ),
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
    if crate::workspace::mode_bits(stat.st_mode) & 0o077 != 0 {
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
    open_private_dir(parent, name)
}

/// Open the owner-only directory `name` under `parent` (never creating
/// it), without following a symlink.
fn open_private_dir(parent: &OwnedFd, name: &str) -> ToolResult<OwnedFd> {
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

/// A private regular file `name` under `dir`, opened for reading.
fn open_private_file(dir: &OwnedFd, name: &str) -> Option<File> {
    let fd = rustix::fs::openat(
        dir,
        name,
        OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .ok()?;
    check_private(&fd, false).ok()?;
    Some(File::from(fd))
}

/// Write `bytes` to the new private file `name` under `dir`.
fn create_private_file(dir: &OwnedFd, name: &str, bytes: &[u8]) -> ToolResult<()> {
    let fd = rustix::fs::openat(
        dir,
        name,
        OFlags::WRONLY | OFlags::CREATE | OFlags::EXCL | OFlags::NOFOLLOW | OFlags::CLOEXEC,
        Mode::RUSR | Mode::WUSR,
    )?;
    if let Err(error) = File::from(fd).write_all(bytes) {
        let _ = rustix::fs::unlinkat(dir, name, AtFlags::empty());
        return Err(ToolError::io("cannot write the output", &error));
    }
    Ok(())
}

/// The line index of `text`: its line count, then the byte offset of
/// line `1 + k * INDEX_STRIDE` for every `k`, each a little-endian `u64`.
fn line_index(text: &str) -> Vec<u8> {
    let total = text.lines().count();
    let mut offsets = vec![0u64];
    let mut line = 1usize;
    for (at, byte) in text.bytes().enumerate() {
        if byte == b'\n' {
            line += 1;
            if (line - 1).is_multiple_of(INDEX_STRIDE) && at + 1 < text.len() {
                offsets.push(at as u64 + 1);
            }
        }
    }
    let mut out = Vec::with_capacity(8 * (offsets.len() + 1));
    out.extend_from_slice(&(total as u64).to_le_bytes());
    for offset in offsets {
        out.extend_from_slice(&offset.to_le_bytes());
    }
    out
}

/// A spill file's index: its line count and the stride offsets.
fn read_index(dir: &OwnedFd, id: &str, text: &mut File) -> ToolResult<(usize, Vec<u64>)> {
    let parsed = open_private_file(dir, &format!("{id}.idx")).and_then(|file| {
        let mut bytes = Vec::new();
        file.take(64 * 1024 * 1024).read_to_end(&mut bytes).ok()?;
        let mut words = bytes
            .chunks_exact(8)
            .map(|chunk| u64::from_le_bytes(chunk.try_into().unwrap_or([0; 8])));
        let total = usize::try_from(words.next()?).ok()?;
        let offsets: Vec<u64> = words.collect();
        (!offsets.is_empty()).then_some((total, offsets))
    });
    if let Some(index) = parsed {
        return Ok(index);
    }
    // No index (or a broken one): count the lines once.
    let mut reader = BufReader::new(&mut *text);
    let mut total = 0usize;
    let mut buffer = Vec::new();
    loop {
        buffer.clear();
        let read = reader
            .read_until(b'\n', &mut buffer)
            .map_err(|error| ToolError::io("cannot read the output", &error))?;
        if read == 0 {
            break;
        }
        total += 1;
    }
    Ok((total, vec![0]))
}

struct Entry {
    scope: String,
    id: String,
    bytes: u64,
    at: SystemTime,
}

/// One session's spill files (see the module docs).
pub struct SpillStore {
    data_dir: PathBuf,
    session_id: String,
    /// The conversation whose turn runs now: its outputs are the ones
    /// spilled and read.
    scope: Mutex<String>,
    cap: u64,
    total_cap: u64,
    retention: Duration,
    counter: AtomicU64,
    /// Every kept output of the session, oldest first; `None` until read
    /// from the disk (the first use).
    entries: Mutex<Option<VecDeque<Entry>>>,
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
    /// created until the first spill. Its outputs are the default scope's
    /// until [`Self::set_conversation`] names a conversation.
    #[must_use]
    pub fn new(data_dir: &Path, session_id: &str) -> Self {
        Self {
            data_dir: data_dir.to_path_buf(),
            session_id: session_id.to_owned(),
            scope: Mutex::new(DEFAULT_SCOPE.to_owned()),
            cap: MAX_SPILL_BYTES,
            total_cap: MAX_SPILL_BYTES_TOTAL,
            retention: SPILL_RETENTION,
            counter: AtomicU64::new(0),
            entries: Mutex::new(None),
        }
    }

    /// The same store with other caps (tests).
    #[must_use]
    pub const fn with_caps(mut self, per_conversation: u64, total: u64) -> Self {
        self.cap = per_conversation;
        self.total_cap = total;
        self
    }

    /// The same store with another cap per conversation (tests).
    #[must_use]
    pub const fn with_cap(self, cap: u64) -> Self {
        let total = self.total_cap;
        self.with_caps(cap, total)
    }

    /// The same store with another retention (tests).
    #[must_use]
    pub const fn with_retention(mut self, retention: Duration) -> Self {
        self.retention = retention;
        self
    }

    /// The conversation whose outputs the next calls spill and read
    /// (`None`: the default scope, a host without conversations). The host
    /// sets it as a turn starts; only one turn of a session runs at once.
    pub fn set_conversation(&self, conversation: Option<&str>) {
        if let Ok(mut scope) = self.scope.lock() {
            *scope = scope_of(conversation);
        }
    }

    fn scope(&self) -> String {
        self.scope
            .lock()
            .map_or_else(|_| DEFAULT_SCOPE.to_owned(), |scope| scope.clone())
    }

    /// Where the session's files live.
    #[must_use]
    pub fn dir(&self) -> PathBuf {
        self.data_dir
            .join("sessions")
            .join(&self.session_id)
            .join("out")
    }

    /// Where the current conversation's files live.
    #[must_use]
    pub fn conversation_dir(&self) -> PathBuf {
        self.dir().join(self.scope())
    }

    fn check_session_id(&self) -> ToolResult<()> {
        if self.session_id.is_empty()
            || !self
                .session_id
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-' || byte == b'_')
        {
            return Err(spill_error("the session id is not a plain name"));
        }
        Ok(())
    }

    /// Open `sessions/<id>/out` under the data directory, each level
    /// owner-only and not a symlink; created when `create`.
    fn open_out(&self, create: bool) -> ToolResult<OwnedFd> {
        self.check_session_id()?;
        if create {
            std::fs::create_dir_all(&self.data_dir)
                .map_err(|error| ToolError::io("cannot create the data directory", &error))?;
        }
        let data = std::fs::canonicalize(&self.data_dir)
            .map_err(|error| ToolError::io("cannot resolve the data directory", &error))?;
        let base = rustix::fs::open(
            &data,
            OFlags::RDONLY | OFlags::DIRECTORY | OFlags::CLOEXEC,
            Mode::empty(),
        )?;
        if create {
            let sessions = private_dir(&base, "sessions")?;
            let session = private_dir(&sessions, &self.session_id)?;
            private_dir(&session, "out")
        } else {
            let sessions = open_private_dir(&base, "sessions")?;
            let session = open_private_dir(&sessions, &self.session_id)?;
            open_private_dir(&session, "out")
        }
    }

    /// Open a conversation's directory (`scope`), created when `create`.
    fn open_scope(&self, scope: &str, create: bool) -> ToolResult<OwnedFd> {
        let out = self.open_out(create)?;
        if create {
            private_dir(&out, scope)
        } else {
            open_private_dir(&out, scope)
        }
    }

    /// What the disk holds, oldest first; expired files and anything the
    /// store does not make (an older layout's files) are removed.
    fn scan(&self) -> VecDeque<Entry> {
        let mut found = Vec::new();
        let Ok(out) = self.open_out(false) else {
            return VecDeque::new();
        };
        let Ok(scopes) = std::fs::read_dir(self.dir()) else {
            return VecDeque::new();
        };
        let now = SystemTime::now();
        for scope in scopes.flatten() {
            let name = scope.file_name();
            let Some(name) = name.to_str() else {
                continue;
            };
            let is_dir = scope.file_type().is_ok_and(|kind| kind.is_dir());
            if !is_dir || !valid_scope(name) {
                // An older layout's file directly under `out/`.
                if !is_dir {
                    let _ = rustix::fs::unlinkat(&out, name, AtFlags::empty());
                }
                continue;
            }
            let Ok(dir) = open_private_dir(&out, name) else {
                continue;
            };
            let Ok(files) = std::fs::read_dir(scope.path()) else {
                continue;
            };
            for file in files.flatten() {
                let file_name = file.file_name();
                let Some(file_name) = file_name.to_str() else {
                    continue;
                };
                let Some(id) = file_name.strip_suffix(".txt").filter(|id| valid_id(id)) else {
                    continue;
                };
                let Ok(meta) = file.metadata() else {
                    continue;
                };
                let at = meta.modified().unwrap_or(std::time::UNIX_EPOCH);
                if now.duration_since(at).unwrap_or_default() > self.retention {
                    remove_output(&dir, id);
                    continue;
                }
                found.push(Entry {
                    scope: name.to_owned(),
                    id: id.to_owned(),
                    bytes: meta.len(),
                    at,
                });
            }
            // Indexes without their output (a crash between the writes).
            if let Ok(files) = std::fs::read_dir(scope.path()) {
                for file in files.flatten() {
                    let file_name = file.file_name();
                    if let Some(id) = file_name
                        .to_str()
                        .and_then(|file_name| file_name.strip_suffix(".idx"))
                        && !found
                            .iter()
                            .any(|entry| entry.scope == name && entry.id == id)
                    {
                        let _ = rustix::fs::unlinkat(&dir, file_name, AtFlags::empty());
                    }
                }
            }
            let _ = rustix::fs::unlinkat(&out, name, AtFlags::REMOVEDIR);
        }
        found.sort_by(|a, b| a.at.cmp(&b.at).then_with(|| a.id.cmp(&b.id)));
        found.into()
    }

    /// Run `work` on the entries, read from the disk first when they have
    /// not been (outside the lock).
    fn with_entries<T>(&self, work: impl FnOnce(&mut VecDeque<Entry>) -> T) -> ToolResult<T> {
        let loaded = self
            .entries
            .lock()
            .map_err(|_| spill_error("the store is poisoned"))?
            .is_some();
        let scanned = (!loaded).then(|| self.scan());
        let mut entries = self
            .entries
            .lock()
            .map_err(|_| spill_error("the store is poisoned"))?;
        if entries.is_none() {
            *entries = Some(scanned.unwrap_or_default());
        }
        Ok(work(entries.get_or_insert_with(VecDeque::new)))
    }

    /// Read the disk now, removing what is past its retention: the host
    /// calls it when a session opens, so old outputs go without waiting
    /// for the next spill.
    pub fn sweep(&self) {
        let now = SystemTime::now();
        let retention = self.retention;
        let gone = self
            .with_entries(|entries| {
                let mut gone = Vec::new();
                entries.retain(|entry| {
                    let keep = now.duration_since(entry.at).unwrap_or_default() <= retention;
                    if !keep {
                        gone.push((entry.scope.clone(), entry.id.clone()));
                    }
                    keep
                });
                gone
            })
            .unwrap_or_default();
        self.remove(gone);
    }

    /// Remove outputs (outside the lock).
    fn remove(&self, gone: Vec<(String, String)>) {
        for (scope, id) in gone {
            if let Ok(dir) = self.open_scope(&scope, false) {
                remove_output(&dir, &id);
            }
        }
    }

    /// Keep `text` (at most the per-conversation cap) as a new output of
    /// the current conversation; its opaque id.
    ///
    /// # Errors
    ///
    /// The directory cannot be made or opened safely, or the file cannot
    /// be written.
    pub fn spill(&self, text: &str) -> ToolResult<String> {
        let scope = self.scope();
        let kept = head(text, usize::try_from(self.cap).unwrap_or(usize::MAX));
        let index = line_index(kept);
        // What the disk held is read first, so the new file is not counted
        // twice; the file IO happens outside the lock.
        self.with_entries(|_| ())?;
        let dir = self.open_scope(&scope, true)?;
        let id = self.next_id(&scope);
        create_private_file(&dir, &format!("{id}.txt"), kept.as_bytes())?;
        if let Err(error) = create_private_file(&dir, &format!("{id}.idx"), &index) {
            let _ = rustix::fs::unlinkat(&dir, format!("{id}.txt"), AtFlags::empty());
            return Err(error);
        }
        let size = (kept.len() + index.len()) as u64;
        let (cap, total_cap, retention) = (self.cap, self.total_cap, self.retention);
        let now = SystemTime::now();
        let gone = self.with_entries(|entries| {
            entries.push_back(Entry {
                scope: scope.clone(),
                id: id.clone(),
                bytes: size,
                at: now,
            });
            evict(entries, &scope, &id, now, (cap, total_cap, retention))
        })?;
        self.remove(gone);
        Ok(id)
    }

    fn next_id(&self, scope: &str) -> String {
        let mut hasher = Sha256::new();
        let now = SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |since| since.as_nanos());
        hasher.update(now.to_le_bytes());
        hasher.update(std::process::id().to_le_bytes());
        hasher.update(self.counter.fetch_add(1, Ordering::Relaxed).to_le_bytes());
        hasher.update(self.dir().join(scope).as_os_str().as_encoded_bytes());
        format!("out-{}", hex16(&hasher.finalize()))
    }

    /// The current conversation's output `id`, opened, if it is kept.
    fn open_output(&self, id: &str) -> ToolResult<(OwnedFd, File)> {
        if !valid_id(id) {
            return Err(ToolError::invalid(
                "output_id is not an output id (read_output takes the id a tool result gave, never a path)",
            ));
        }
        let scope = self.scope();
        let now = SystemTime::now();
        let retention = self.retention;
        let (known, stale) = self.with_entries(|entries| {
            let Some(at) = entries
                .iter()
                .position(|entry| entry.scope == scope && entry.id == id)
            else {
                return (false, None);
            };
            if now.duration_since(entries[at].at).unwrap_or_default() > retention {
                let entry = entries.remove(at);
                return (false, entry.map(|entry| (entry.scope, entry.id)));
            }
            (true, None)
        })?;
        if let Some(stale) = stale {
            self.remove(vec![stale]);
        }
        if !known {
            return Err(expired(id));
        }
        let dir = self.open_scope(&scope, false).map_err(|_| expired(id))?;
        let file = open_private_file(&dir, &format!("{id}.txt")).ok_or_else(|| expired(id))?;
        Ok((dir, file))
    }

    /// The whole text of the current conversation's output `id`.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::InvalidArgument`] for anything but an output id (a
    /// path included), [`ErrorCode::OutputExpired`] for one this
    /// conversation does not keep (dropped, expired, or another's).
    pub fn read(&self, id: &str) -> ToolResult<String> {
        let (_, file) = self.open_output(id)?;
        let mut text = String::new();
        file.take(self.cap)
            .read_to_string(&mut text)
            .map_err(|error| ToolError::io("cannot read the output", &error))?;
        Ok(text)
    }

    /// One page of the current conversation's output `id`: only the page
    /// is read (from the nearest indexed line).
    ///
    /// # Errors
    ///
    /// As [`Self::read`].
    pub fn page(&self, id: &str, request: PageRequest, max_bytes: usize) -> ToolResult<Value> {
        let (dir, mut file) = self.open_output(id)?;
        let (total, offsets) = read_index(&dir, id, &mut file)?;
        let start = request.offset.unwrap_or(1).max(1);
        let slot = ((start - 1) / INDEX_STRIDE).min(offsets.len() - 1);
        file.seek(SeekFrom::Start(offsets[slot]))
            .map_err(|error| ToolError::io("cannot read the output", &error))?;
        let mut reader = BufReader::new(file.take(self.cap));
        page_lines(
            &mut reader,
            slot * INDEX_STRIDE + 1,
            total,
            request,
            max_bytes,
        )
        .map_err(|error| ToolError::io("cannot read the output", &error))
    }

    /// Remove every output of the session, every conversation's.
    pub fn clear(&self) {
        if let Ok(mut entries) = self.entries.lock() {
            *entries = Some(VecDeque::new());
        }
        let _ = std::fs::remove_dir_all(self.dir());
    }

    /// Remove one conversation's outputs (its local history was deleted).
    pub fn forget_conversation(&self, conversation: &str) {
        let scope = scope_of(Some(conversation));
        if let Ok(mut entries) = self.entries.lock()
            && let Some(entries) = entries.as_mut()
        {
            entries.retain(|entry| entry.scope != scope);
        }
        if self.check_session_id().is_ok() {
            let _ = std::fs::remove_dir_all(self.dir().join(scope));
        }
    }

    /// The ids the current conversation holds, oldest first.
    #[must_use]
    pub fn ids(&self) -> Vec<String> {
        let scope = self.scope();
        self.with_entries(|entries| {
            entries
                .iter()
                .filter(|entry| entry.scope == scope)
                .map(|entry| entry.id.clone())
                .collect()
        })
        .unwrap_or_default()
    }
}

/// Remove one output and its index.
fn remove_output(dir: &OwnedFd, id: &str) {
    let _ = rustix::fs::unlinkat(dir, format!("{id}.txt"), AtFlags::empty());
    let _ = rustix::fs::unlinkat(dir, format!("{id}.idx"), AtFlags::empty());
}

/// What a spill of `id` into `scope` evicts: anything past the retention,
/// then the scope's oldest while it is over its cap, then the session's
/// oldest while it is over the total cap; never `id` itself.
fn evict(
    entries: &mut VecDeque<Entry>,
    scope: &str,
    id: &str,
    now: SystemTime,
    (cap, total_cap, retention): (u64, u64, Duration),
) -> Vec<(String, String)> {
    let mut gone = Vec::new();
    let mut take = |entries: &mut VecDeque<Entry>, at: usize| {
        if let Some(entry) = entries.remove(at) {
            gone.push((entry.scope, entry.id));
        }
    };
    while let Some(at) = entries
        .iter()
        .position(|entry| now.duration_since(entry.at).unwrap_or_default() > retention)
    {
        take(entries, at);
    }
    let scope_total = |entries: &VecDeque<Entry>| -> u64 {
        entries
            .iter()
            .filter(|entry| entry.scope == scope)
            .map(|entry| entry.bytes)
            .sum()
    };
    while scope_total(entries) > cap {
        let Some(at) = entries
            .iter()
            .position(|entry| entry.scope == scope && entry.id != id)
        else {
            break;
        };
        take(entries, at);
    }
    while entries.iter().map(|entry| entry.bytes).sum::<u64>() > total_cap {
        let Some(at) = entries.iter().position(|entry| entry.id != id) else {
            break;
        };
        take(entries, at);
    }
    gone
}

/// Remove conversation `conversation`'s outputs of session `session_id`
/// under `data_dir`, with or without an open session (its local history
/// was deleted).
pub fn forget_conversation_outputs(data_dir: &Path, session_id: &str, conversation: &str) {
    SpillStore::new(data_dir, session_id).forget_conversation(conversation);
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
    column: Option<usize>,
    #[serde(default)]
    limit: Option<usize>,
}

/// `read_output`: a page of one of the current conversation's spilled
/// outputs, by id.
pub(crate) fn read_output(spill: &SpillStore, args: Value, max_bytes: usize) -> ToolResult<Value> {
    let args: ReadOutputArgs = serde_json::from_value(args)
        .map_err(|error| ToolError::invalid(format!("bad arguments: {error}")))?;
    let mut page = spill.page(
        &args.output_id,
        PageRequest {
            offset: args.offset,
            column: args.column,
            limit: args.limit,
        },
        max_bytes,
    )?;
    if let Value::Object(map) = &mut page {
        map.insert("output_id".to_owned(), Value::String(args.output_id));
    }
    Ok(page)
}

#[cfg(test)]
mod tests {
    use std::os::unix::fs::PermissionsExt;
    use std::time::{Duration, SystemTime};

    use serde_json::json;

    use super::{
        Budgets, PageRequest, SpillStore, bytes, estimate_tokens, forget_conversation_outputs,
        head_and_tail, page, read_output,
    };
    use crate::error::ErrorCode;
    use crate::ledger::ReadLedger;
    use crate::workspace::Workspace;

    fn store(dir: &tempfile::TempDir) -> SpillStore {
        SpillStore::new(&dir.path().join("data"), "ws-1")
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
        assert!(read.get("next_column").is_none(), "{read}");
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
    }

    /// The text of one page's lines, without the numbers and markers.
    fn page_text(page: &serde_json::Value) -> String {
        let content = page["content"].as_str().expect("content");
        content
            .lines()
            .map(|line| {
                let text = line.split_once('\t').map_or("", |(_, text)| text);
                text.split_once("… [line cut:")
                    .map_or(text, |(kept, _)| kept)
            })
            .collect::<Vec<_>>()
            .join("\n")
    }

    /// Page `next` until the end, joining what each page showed.
    fn page_all(
        mut next: impl FnMut(Option<u64>, Option<u64>) -> serde_json::Value,
        budget: usize,
    ) -> (String, usize) {
        let (mut offset, mut column) = (None, None);
        let mut whole = String::new();
        let mut pages = 0;
        loop {
            let page = next(offset, column);
            pages += 1;
            assert!(page["content"].as_str().expect("content").len() <= budget);
            let text = page_text(&page);
            // A new line (not a continuation) starts after a newline.
            if !whole.is_empty() && page.get("start_column").is_none() {
                whole.push('\n');
            }
            whole.push_str(&text);
            if page["truncated"] != true {
                return (whole, pages);
            }
            offset = page["next_offset"].as_u64();
            column = page["next_column"].as_u64();
            assert!(pages < 10_000, "paging advances");
        }
    }

    /// Security/regression: a 500 KB single line used to be cut once and
    /// skipped; every byte of it pages now, through `read_file` and
    /// `read_output`, with multi-byte characters never split.
    #[test]
    fn a_long_single_line_pages_fully() {
        let dir = tempfile::tempdir().expect("dir");
        let line: String = "abc é 漢字 ".repeat(40_000);
        assert!(line.len() > 500_000);
        let text = format!("first\n{line}\nlast\n");
        std::fs::create_dir_all(dir.path().join("ws")).expect("ws");
        std::fs::write(dir.path().join("ws/one.txt"), &text).expect("write");
        let ws = Workspace::open(&dir.path().join("ws"), &[]).expect("ws");
        let budget = bytes(Budgets::default().read_file);
        let (read, pages) = page_all(
            |offset, column| {
                crate::files::read(
                    &ws,
                    &ReadLedger::new(),
                    json!({ "path": "one.txt", "offset": offset, "column": column }),
                    budget,
                )
                .expect("read")
            },
            budget,
        );
        assert!(pages > 10, "{pages}");
        assert_eq!(read, text.trim_end_matches('\n'));
        let spill = store(&dir);
        let id = spill.spill(&text).expect("spill");
        let (from_store, _) = page_all(
            |offset, column| {
                read_output(
                    &spill,
                    json!({ "output_id": id, "offset": offset, "column": column }),
                    budget,
                )
                .expect("page")
            },
            budget,
        );
        assert_eq!(from_store, text.trim_end_matches('\n'));
    }

    /// `read_output` reads from the nearest indexed line: every page equals
    /// the in-memory page of the same text, across index strides.
    #[test]
    fn indexed_pages_equal_in_memory_pages() {
        let dir = tempfile::tempdir().expect("dir");
        let text: String = (1..=1_000)
            .map(|n| "line ".to_owned() + &n.to_string() + "\n")
            .collect();
        let spill = store(&dir);
        let id = spill.spill(&text).expect("spill");
        for (offset, limit) in [
            (1, 10),
            (255, 3),
            (256, 1),
            (257, 2),
            (512, 5),
            (513, 5),
            (999, 10),
            (1_000, 1),
            (1_001, 1),
        ] {
            let request = PageRequest {
                offset: Some(offset),
                column: None,
                limit: Some(limit),
            };
            let mut stored = spill.page(&id, request, 4096).expect("page");
            let memory = page(&text, request, 4096);
            if let serde_json::Value::Object(map) = &mut stored {
                map.remove("output_id");
            }
            assert_eq!(stored, memory, "{offset}");
        }
    }

    #[test]
    fn read_output_refuses_paths_and_foreign_ids() {
        let dir = tempfile::tempdir().expect("dir");
        let ours = store(&dir);
        ours.set_conversation(Some("42"));
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
            "sessions/ws-1/out/x",
            "",
        ] {
            let refused = read_output(&ours, json!({ "output_id": path }), 4096)
                .expect_err("a path is refused");
            assert_eq!(refused.code(), ErrorCode::InvalidArgument, "{path}");
        }
        // Another conversation's id, in the same session, is not ours.
        ours.set_conversation(Some("43"));
        let theirs = ours.spill("theirs\n").expect("spill");
        assert_eq!(
            read_output(&ours, json!({ "output_id": id }), 4096)
                .expect_err("foreign")
                .code(),
            ErrorCode::OutputExpired
        );
        ours.set_conversation(Some("42"));
        assert_eq!(
            read_output(&ours, json!({ "output_id": theirs }), 4096)
                .expect_err("foreign")
                .code(),
            ErrorCode::OutputExpired
        );
        let unknown = read_output(&ours, json!({ "output_id": "out-0123456789abcdef" }), 4096)
            .expect_err("unknown");
        assert_eq!(unknown.code(), ErrorCode::OutputExpired);
        assert!(
            unknown.message().contains("re-run"),
            "{}",
            unknown.message()
        );
        // A symlink planted in place of our file is not followed.
        let file = ours.conversation_dir().join(format!("{id}.txt"));
        std::fs::remove_file(&file).expect("rm");
        std::os::unix::fs::symlink("/etc/hosts", &file).expect("link");
        assert!(read_output(&ours, json!({ "output_id": id }), 4096).is_err());
    }

    fn mode(path: &std::path::Path) -> u32 {
        std::fs::metadata(path).expect("meta").permissions().mode() & 0o777
    }

    /// Regression: a session reopened (a policy change, an app restart)
    /// keeps its outputs, so the ids in the conversation still page; past
    /// the retention they go and answer `output_expired`.
    #[test]
    fn spills_are_private_and_outlive_a_reopen_within_the_retention() {
        let dir = tempfile::tempdir().expect("dir");
        let first = store(&dir);
        first.set_conversation(Some("7"));
        let id = first.spill("secret output\n").expect("spill");
        let file = first.conversation_dir().join(format!("{id}.txt"));
        assert_eq!(mode(&file), 0o600);
        assert_eq!(mode(&first.conversation_dir()), 0o700);
        assert_eq!(mode(&first.dir()), 0o700);
        assert_eq!(mode(&dir.path().join("data/sessions")), 0o700);
        drop(first);
        assert!(file.exists(), "a session ending keeps its outputs");
        let second = store(&dir);
        second.sweep();
        second.set_conversation(Some("7"));
        assert_eq!(second.read(&id).expect("kept"), "secret output\n");
        assert_eq!(second.ids(), std::slice::from_ref(&id));
        // Past the retention: removed at the next open.
        let old = SystemTime::now() - Duration::from_hours(25);
        std::fs::File::options()
            .write(true)
            .open(&file)
            .expect("open")
            .set_modified(old)
            .expect("mtime");
        let third = store(&dir);
        third.sweep();
        third.set_conversation(Some("7"));
        assert!(!file.exists(), "expired outputs go");
        assert_eq!(
            third.read(&id).expect_err("expired").code(),
            ErrorCode::OutputExpired
        );
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
        // Each spill of four bytes keeps 4 + 16 index bytes.
        let small = store(&dir).with_cap(50);
        let a = small.spill("aaaa").expect("a");
        let b = small.spill("bbbb").expect("b");
        let c = small.spill("cccc").expect("c");
        assert_eq!(small.ids(), [b.clone(), c]);
        assert_eq!(
            small.read(&a).expect_err("dropped").code(),
            ErrorCode::OutputExpired
        );
        assert_eq!(small.read(&b).expect("b"), "bbbb");
    }

    /// Regression: outputs are per conversation. One conversation over its
    /// cap drops only its own oldest; the session cap drops the oldest of
    /// any; forgetting a conversation removes only its outputs.
    #[test]
    fn conversations_have_their_own_outputs_and_caps() {
        let dir = tempfile::tempdir().expect("dir");
        let spill = store(&dir).with_caps(50, 70);
        spill.set_conversation(Some("a"));
        let a1 = spill.spill("aaaa").expect("a1");
        spill.set_conversation(Some("b"));
        let b1 = spill.spill("bbbb").expect("b1");
        let b2 = spill.spill("bbbb").expect("b2");
        let b3 = spill.spill("bbbb").expect("b3");
        // b over its 50: its own oldest went, a's stayed.
        assert_eq!(spill.ids(), [b2.clone(), b3.clone()]);
        assert!(spill.read(&b1).is_err());
        spill.set_conversation(Some("a"));
        assert_eq!(spill.ids(), std::slice::from_ref(&a1));
        // 80 in all is over the session's 70: its oldest (a1) goes.
        let a2 = spill.spill("aaaa").expect("a2");
        assert_eq!(spill.ids(), std::slice::from_ref(&a2));
        assert!(spill.read(&a1).is_err());
        forget_conversation_outputs(&dir.path().join("data"), "ws-1", "a");
        spill.forget_conversation("a");
        assert!(spill.ids().is_empty());
        assert!(!spill.conversation_dir().exists());
        assert_eq!(
            spill.read(&a2).expect_err("forgotten").code(),
            ErrorCode::OutputExpired
        );
        spill.set_conversation(Some("b"));
        assert_eq!(spill.ids(), [b2, b3.clone()]);
        assert_eq!(spill.read(&b3).expect("b kept"), "bbbb");
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
