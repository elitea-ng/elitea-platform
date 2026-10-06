//! Deep research's in-memory file system: `deepagents` 0.7.13's
//! `StateBackend` and the `FilesystemMiddleware` tools over it (`ls`,
//! `read_file`, `write_file`, `edit_file`, `delete`, `glob`, `grep`, and the
//! `execute` the backend cannot serve), plus the large-result eviction.
//!
//! The files live in the agent's state, which `LangGraph` updates once per
//! step: the tools of one model turn read the state the turn started
//! with, and their writes apply after the turn, in call order. [`Vfs`] does
//! the same ([`Vfs::apply`]). The bounds are checked against [`Bounds`]:
//! the sizes as they will be after the writes of the turn so far, so the
//! (up to 25) calls of one turn cannot together pass them.
//!
//! Paths are validated as `validate_path` does (no `..` component, no
//! leading `~`, no Windows drive; normalised under `/`). Nothing here
//! touches a real file system.
//!
//! Deliberate differences: the state holds text only (a `.png` the model
//! writes reads back as text, where `deepagents` answered an image block);
//! the file system is bounded ([`MAX_FILES`], [`MAX_BYTES`]); `glob` uses
//! `globset` for `wcmatch`'s `BRACE | GLOBSTAR` (a malformed pattern
//! matches nothing in both).

use super::pyfmt;
use crate::graph::pystr;
use indexmap::IndexMap;
use regex::Regex;
use serde_json::Value;
use std::collections::HashMap;
use std::sync::LazyLock;

/// Files the state may hold.
pub const MAX_FILES: usize = 1_000;
/// Bytes the state may hold in all.
pub const MAX_BYTES: usize = 16 * 1024 * 1024;

/// `NUM_CHARS_PER_TOKEN * tool_token_limit_before_evict`.
pub const EVICT_CHARS: usize = 4 * 20_000;

/// Follows a too-large result that could not be saved because the research
/// file system is full (not in Python, which had no file-system bound).
const FULL_FILESYSTEM_NOTE: &str = "... [result truncated: the research file system is full, so the whole result could not be saved; delete files you no longer need or ask for less]";
const MAX_LINE_LENGTH: usize = 5_000;
const TRUNCATION_GUIDANCE: &str =
    "... [results truncated, try being more specific with your parameters]";
const EMPTY_CONTENT_WARNING: &str = "System reminder: File exists but has empty contents";
const GREP_TRUNCATION_NOTE: &str = "Note: the search stopped early (it hit its time limit or the maximum match count). The matches above are valid but incomplete. Narrow the search (a more specific pattern or a narrower path), or raise max_count, to see the rest.";
const REGEX_HINT: &str = "Note: grep matches literal text, not regex, so characters like `|`, `.*`, and `\\.` are searched verbatim. Search for the literal text you need instead; for `|` alternation, run a separate search per alternative.";
const GREP_MAX_COUNT: usize = 1_000;

/// The tools that are never evicted (`TOOLS_EXCLUDED_FROM_EVICTION`).
pub const NOT_EVICTED: &[&str] = &[
    "ls",
    "glob",
    "grep",
    "read_file",
    "edit_file",
    "write_file",
    "delete",
];

/// One file.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct File {
    pub content: String,
    /// The write sequence number (`modified_at`).
    pub modified: u64,
}

/// The file state.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Vfs {
    files: IndexMap<String, File>,
    clock: u64,
}

/// A pending change: a write, or a deletion (`None`).
pub type Update = (String, Option<String>);

impl Vfs {
    #[must_use]
    pub fn files(&self) -> &IndexMap<String, File> {
        &self.files
    }

    /// Apply a turn's updates in order (the `files` reducer: a write
    /// replaces in place, a new file is appended, `None` removes).
    pub fn apply(&mut self, updates: Vec<Update>) {
        for (path, content) in updates {
            match content {
                None => {
                    self.files.shift_remove(&path);
                }
                Some(content) => {
                    self.clock += 1;
                    let modified = self.clock;
                    self.files.insert(path, File { content, modified });
                }
            }
        }
    }

    /// The sizes of the files, to check a turn's writes against.
    #[must_use]
    pub fn bounds(&self) -> Bounds {
        let sizes: HashMap<String, usize> = self
            .files
            .iter()
            .map(|(path, file)| (path.clone(), file.content.len()))
            .collect();
        let bytes = sizes.values().sum();
        Bounds { sizes, bytes }
    }
}

/// The file sizes the state will have once the pending writes apply.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Bounds {
    sizes: HashMap<String, usize>,
    bytes: usize,
}

impl Bounds {
    /// Whether a write of `content` to `path` fits the bounds.
    #[must_use]
    pub fn fits(&self, path: &str, content: &str) -> bool {
        let existing = self.sizes.get(path).copied().unwrap_or(0);
        let count = self.sizes.len() + usize::from(!self.sizes.contains_key(path));
        count <= MAX_FILES && self.bytes - existing + content.len() <= MAX_BYTES
    }

    /// Count `updates` as applied (in order, as [`Vfs::apply`] will).
    pub fn record(&mut self, updates: &[Update]) {
        for (path, content) in updates {
            let size = content.as_ref().map(String::len);
            let before = match size {
                Some(size) => self.sizes.insert(path.clone(), size),
                None => self.sizes.remove(path),
            };
            self.bytes = self.bytes - before.unwrap_or(0) + size.unwrap_or(0);
        }
    }
}

/// `validate_path`.
///
/// # Errors
///
/// The `ValueError` text.
pub fn validate_path(path: &str) -> Result<String, String> {
    let posix = path.replace('\\', "/");
    if posix.split('/').any(|part| part == "..") || path.starts_with('~') {
        return Err(format!("Path traversal not allowed: {path}"));
    }
    let bytes = path.as_bytes();
    if bytes.len() >= 2 && bytes[0].is_ascii_alphabetic() && bytes[1] == b':' {
        return Err(format!(
            "Windows absolute paths are not supported: {path}. Please use virtual paths starting with / (e.g., /workspace/file.txt)"
        ));
    }
    let mut normalized = pystr::normpath(path).replace('\\', "/");
    if !normalized.starts_with('/') {
        normalized.insert(0, '/');
    }
    if normalized.split('/').any(|part| part == "..") {
        return Err(format!(
            "Path traversal detected after normalization: {path} -> {normalized}"
        ));
    }
    Ok(normalized)
}

/// `_normalize_path` (glob and grep roots).
fn normalize_root(path: Option<&str>) -> Option<String> {
    let path = path.filter(|p| !p.is_empty()).unwrap_or("/");
    if pystr::strip(path).is_empty() {
        return None;
    }
    let mut normalized = if path.starts_with('/') {
        path.to_owned()
    } else {
        format!("/{path}")
    };
    if normalized != "/" {
        normalized = normalized.trim_end_matches('/').to_owned();
    }
    Some(normalized)
}

fn filter_by_root<'f>(
    files: &'f IndexMap<String, File>,
    root: &str,
) -> Vec<(&'f String, &'f File)> {
    if let Some((path, file)) = files.get_key_value(root) {
        return vec![(path, file)];
    }
    let prefix = if root == "/" {
        "/".to_owned()
    } else {
        format!("{root}/")
    };
    files
        .iter()
        .filter(|(p, _)| p.starts_with(&prefix))
        .collect()
}

fn relative_to_root(file: &str, root: &str) -> String {
    if root == "/" {
        return file[1..].to_owned();
    }
    if file == root {
        return file.rsplit('/').next().unwrap_or(file).to_owned();
    }
    file.get(root.len() + 1..).unwrap_or_default().to_owned()
}

/// `compile_grep_include_glob`: a matcher over root-relative paths.
fn compile_glob(pattern: &str) -> Result<Option<GlobMatcher>, String> {
    if pattern
        .replace('\\', "/")
        .split('/')
        .any(|part| part == "..")
    {
        return Err(format!(
            "Path traversal not allowed in glob pattern {}",
            crate::parsers::python::text::repr_str(pattern)
        ));
    }
    let anchored = pattern.contains('/');
    let stripped = pattern.trim_start_matches('/');
    let Ok(glob) = globset::GlobBuilder::new(stripped)
        .literal_separator(true)
        .backslash_escape(true)
        .build()
    else {
        return Ok(None);
    };
    let dot_segments: Vec<bool> = stripped.split('/').map(|s| s.starts_with('.')).collect();
    Ok(Some(GlobMatcher {
        matcher: glob.compile_matcher(),
        anchored,
        dots_allowed: dot_segments.iter().any(|d| *d),
    }))
}

struct GlobMatcher {
    matcher: globset::GlobMatcher,
    anchored: bool,
    dots_allowed: bool,
}

impl GlobMatcher {
    fn matches(&self, relative: &str) -> bool {
        let subject = if self.anchored {
            relative
        } else {
            pystr::file_name(relative)
        };
        // No DOTMATCH: a leading-dot name only matches a dot pattern.
        if !self.dots_allowed && subject.split('/').any(|s| s.starts_with('.')) {
            return false;
        }
        self.matcher.is_match(subject)
    }
}

/// `str(truncate_if_too_long(paths))`, or `No files found`.
fn format_paths(paths: &[String]) -> String {
    if paths.is_empty() {
        return "No files found".to_owned();
    }
    let total: usize = paths.iter().map(|p| pyfmt::len(p)).sum();
    let mut items: Vec<Value> = paths.iter().map(|p| Value::from(p.as_str())).collect();
    if total > EVICT_CHARS {
        items.truncate(paths.len() * EVICT_CHARS / total);
        items.push(Value::from(TRUNCATION_GUIDANCE));
    }
    pyfmt::repr(&Value::Array(items))
}

fn truncate_text(text: String) -> String {
    if pyfmt::len(&text) > EVICT_CHARS {
        format!("{}\n{TRUNCATION_GUIDANCE}", pyfmt::head(&text, EVICT_CHARS))
    } else {
        text
    }
}

/// `format_content_with_line_numbers`.
#[must_use]
pub fn number_lines(content: &[&str], start_line: usize) -> String {
    let mut rows: Vec<(String, String)> = Vec::new();
    let mut width = 0;
    for (i, line) in content.iter().enumerate() {
        let number = i + start_line;
        let chars: Vec<char> = line.chars().collect();
        let chunks: Vec<String> = if chars.is_empty() {
            vec![String::new()]
        } else {
            chars
                .chunks(MAX_LINE_LENGTH)
                .map(|c| c.iter().collect())
                .collect()
        };
        for (index, chunk) in chunks.into_iter().enumerate() {
            let marker = if index == 0 {
                number.to_string()
            } else {
                format!("{number}.{index}")
            };
            width = width.max(marker.len());
            rows.push((marker, chunk));
        }
    }
    rows.iter()
        .map(|(marker, line)| format!("{marker:>width$}  {line}"))
        .collect::<Vec<_>>()
        .join("\n")
}

fn split_content(content: &str) -> Vec<&str> {
    let mut lines: Vec<&str> = content.split('\n').collect();
    if lines.last() == Some(&"") {
        lines.pop();
    }
    lines
}

/// The read window's pagination (`ReadResult`).
#[derive(Clone, Copy)]
struct Window {
    total: usize,
    start: usize,
    end: usize,
    next: Option<usize>,
}

fn remaining_notice(window: Window) -> String {
    let Some(next) = window.next else {
        return String::new();
    };
    if window.end >= window.total {
        return String::new();
    }
    let read = window.end + 1 - window.start;
    let remaining = window.total - window.end;
    format!(
        "\n\n[Read {read} {} (lines {}-{} of {} total). {remaining} {} remaining from offset {next}.]",
        if read == 1 { "line" } else { "lines" },
        window.start,
        window.end,
        window.total,
        if remaining == 1 { "line" } else { "lines" },
    )
}

/// `_truncate_paginated_read` with the 20 000-token limit.
fn truncate_read(content: &str, file_path: &str, window: Window) -> String {
    let notice = remaining_notice(window);
    if pyfmt::len(content) + pyfmt::len(&notice) < EVICT_CHARS {
        return format!("{content}{notice}");
    }
    let message = format!(
        "\n\n[Output was truncated due to size limits. The file content is very large. Consider reformatting the file to make it easier to navigate. For example, if this is JSON, use execute(command='jq . {file_path}') to pretty-print it with line breaks. For other formats, you can use appropriate formatting tools to split long lines.]"
    );
    let rows: Vec<&str> = content.split('\n').collect();
    let marker = |row: &str| -> usize {
        row.trim_start()
            .split("  ")
            .next()
            .unwrap_or_default()
            .split('.')
            .next()
            .unwrap_or_default()
            .parse()
            .unwrap_or(0)
    };
    // (char position after the row, source line) for the last row of
    // each source line.
    let mut boundaries: Vec<(usize, usize)> = Vec::new();
    let mut position = 0;
    for (index, row) in rows.iter().enumerate() {
        position += pyfmt::len(row);
        let line = marker(row);
        if line > window.end {
            break;
        }
        let next = rows.get(index + 1).map(|r| marker(r));
        if next != Some(line) {
            boundaries.push((position, line));
        }
        position += 1;
    }
    for &(boundary, end_line) in boundaries.iter().rev() {
        let adjusted = remaining_notice(Window {
            total: window.total,
            start: window.start,
            end: end_line,
            next: Some(end_line),
        });
        if boundary + pyfmt::len(&message) + pyfmt::len(&adjusted) <= EVICT_CHARS {
            return format!("{}{message}{adjusted}", pyfmt::head(content, boundary));
        }
    }
    let keep = EVICT_CHARS.saturating_sub(pyfmt::len(&message));
    format!("{}{message}", pyfmt::head(content, keep))
}

/// `str.splitlines(keepends=True)`.
fn lines_keepends(text: &str) -> Vec<&str> {
    let spans = pystr::splitline_spans(text);
    let mut out = Vec::with_capacity(spans.len());
    for (index, (start, _)) in spans.iter().enumerate() {
        let end = spans.get(index + 1).map_or(text.len(), |s| s.0);
        out.push(&text[*start..end]);
    }
    out
}

fn read_file(vfs: &Vfs, path: &str, offset: i64, limit: i64) -> String {
    let Some(file) = vfs.files.get(path) else {
        return format!("Error: File '{path}' not found");
    };
    let content = &file.content;
    if pystr::strip(content).is_empty() {
        return EMPTY_CONTENT_WARNING.to_owned();
    }
    let start = usize::try_from(offset.max(0)).unwrap_or(usize::MAX);
    let count = usize::try_from(limit.max(0)).unwrap_or(usize::MAX);
    if count == 0 {
        return format!(
            "System reminder: no lines were read because `limit` was {limit}. The file was not inspected and may have contents; retry with `limit` >= 1 to read it."
        );
    }
    let lines = lines_keepends(content);
    let total = lines.len();
    if start >= total {
        return format!("Error: Line offset {start} exceeds file length ({total} lines)");
    }
    let end = start.saturating_add(count).min(total);
    let sliced = lines[start..end]
        .concat()
        .replace("\r\n", "\n")
        .replace('\r', "\n");
    let window = Window {
        total,
        start: start + 1,
        end,
        next: (end < total).then_some(end),
    };
    let numbered = number_lines(&split_content(&sliced), start + 1);
    let mut out = truncate_read(&numbered, path, window);
    if offset < 0 {
        out = format!(
            "{out}\n\n[Requested offset {offset} is before the start of the file; read from line 1 instead.]"
        );
    }
    out
}

/// `perform_string_replacement`.
fn replace(
    content: &str,
    old: &str,
    new: &str,
    replace_all: bool,
) -> Result<(String, usize), String> {
    let occurrences = content.matches(old).count();
    if occurrences == 0 {
        if old.len() > 1 && old.ends_with('\n') {
            let stripped = &old[..old.len() - 1];
            if content.ends_with(stripped) {
                let count = content.matches(stripped).count();
                if count == 1 {
                    return Err("Error: old_string ends with a newline, but the file does not end with a newline. Retry with the trailing newline removed from old_string (and from new_string if it also ends with a newline).".to_owned());
                }
                return Err(format!(
                    "Error: old_string ends with a newline, but the file does not end with a newline. With the trailing newline removed, old_string would appear {count} times in the file. Retry with the trailing newline removed and add surrounding context so the match is unique."
                ));
            }
        }
        return Err(format!("Error: String not found in file: '{old}'"));
    }
    if occurrences > 1 && !replace_all {
        return Err(format!(
            "Error: String '{old}' appears {occurrences} times in file. Use replace_all=True to replace all instances, or provide a more specific string with surrounding context."
        ));
    }
    // The size of the result before it is built: a short `old_string`
    // replaced everywhere must not allocate past the file-system bound.
    let size = content.len() - occurrences * old.len() + occurrences.saturating_mul(new.len());
    if size > MAX_BYTES {
        return Err(format!(
            "Error: the file system is full ({MAX_FILES} files, {MAX_BYTES} bytes); delete files you no longer need"
        ));
    }
    Ok((content.replace(old, new), occurrences))
}

static REGEX_SIGNAL: LazyLock<Option<Regex>> =
    LazyLock::new(|| Regex::new(r"\||\.\*|\.\+|\\[.wWdDsSbB(){}\[\]|+*?^$]").ok());

fn glob_tool(vfs: &Vfs, pattern: &str, path: Option<&str>) -> String {
    let root_for_check = path.unwrap_or("/");
    let validated = match validate_path(root_for_check) {
        Ok(v) => v,
        Err(e) => return format!("Error: {e}"),
    };
    let base = path.map(|_| validated);
    let Some(root) = normalize_root(base.as_deref()) else {
        return "No files found".to_owned();
    };
    let compiled = match compile_glob(pattern) {
        Ok(m) => m,
        Err(e) => return format!("Error: {e}"),
    };
    let mut found: Vec<(&String, u64)> = filter_by_root(&vfs.files, &root)
        .into_iter()
        .filter(|(p, _)| {
            compiled
                .as_ref()
                .is_some_and(|m| m.matches(&relative_to_root(p, &root)))
        })
        .map(|(p, f)| (p, f.modified))
        .collect();
    found.sort_by_key(|f| std::cmp::Reverse(f.1));
    let paths: Vec<String> = found.into_iter().map(|(p, _)| p.clone()).collect();
    format_paths(&paths)
}

fn grep_tool(vfs: &Vfs, args: &super::args::Args) -> String {
    let pattern = args.str("pattern");
    let mut path = args.opt_str("path").map(str::to_owned);
    if let Some(raw) = &path {
        match validate_path(raw) {
            Ok(v) => path = Some(v),
            Err(e) => return format!("Error: {e}"),
        }
    }
    let max_count = args
        .opt_int("max_count")
        .map_or(GREP_MAX_COUNT, |n| usize::try_from(n).unwrap_or(usize::MAX));
    let Some(root) = normalize_root(path.as_deref()) else {
        return "No matches found".to_owned();
    };
    let mut candidates = filter_by_root(&vfs.files, &root);
    if let Some(glob) = args.opt_str("glob").filter(|g| !g.is_empty()) {
        match compile_glob(glob) {
            Err(e) => return e,
            Ok(matcher) => candidates.retain(|(p, _)| {
                matcher
                    .as_ref()
                    .is_some_and(|m| m.matches(&relative_to_root(p, &root)))
            }),
        }
    }
    let mut matches: Vec<(&str, usize, &str)> = Vec::new();
    let mut truncated = false;
    'files: for (file_path, file) in candidates {
        for (index, line) in file.content.split('\n').enumerate() {
            if line.contains(pattern) {
                if matches.len() >= max_count {
                    truncated = true;
                    break 'files;
                }
                matches.push((file_path, index + 1, line));
            }
        }
    }
    let formatted = if matches.is_empty() {
        "No matches found".to_owned()
    } else {
        let mut grouped: IndexMap<&str, Vec<(usize, &str)>> = IndexMap::new();
        for (p, n, l) in &matches {
            grouped.entry(p).or_default().push((*n, l));
        }
        grouped.sort_keys();
        let lines: Vec<String> = match args.str("output_mode") {
            "count" => grouped
                .iter()
                .map(|(p, m)| format!("{p}: {}", m.len()))
                .collect(),
            "content" => grouped
                .iter()
                .flat_map(|(p, m)| {
                    std::iter::once(format!("{p}:"))
                        .chain(m.iter().map(|(n, l)| format!("  {n}: {l}")))
                })
                .collect(),
            _ => grouped.keys().map(|p| (*p).to_owned()).collect(),
        };
        truncate_text(lines.join("\n"))
    };
    let mut notes = Vec::new();
    if truncated {
        notes.push(GREP_TRUNCATION_NOTE);
    }
    if !truncated
        && matches.is_empty()
        && REGEX_SIGNAL.as_ref().is_some_and(|r| r.is_match(pattern))
    {
        notes.push(REGEX_HINT);
    }
    if notes.is_empty() {
        formatted
    } else {
        format!("{formatted}\n\n{}", notes.join("\n\n"))
    }
}

/// Run one file-system tool on the turn's snapshot, its writes checked
/// against `bounds` (the turn's earlier writes counted). Returns the result
/// text and the writes to apply after the turn; `None` for another tool.
#[must_use]
pub fn run(
    vfs: &Vfs,
    bounds: &Bounds,
    tool: &str,
    args: &super::args::Args,
) -> Option<(String, Vec<Update>)> {
    let checked = |raw: &str| validate_path(raw).map_err(|e| format!("Error: {e}"));
    let mut updates = Vec::new();
    let text = match tool {
        "ls" => match checked(args.str("path")) {
            Err(e) => e,
            Ok(path) => {
                let prefix = if path.ends_with('/') {
                    path
                } else {
                    format!("{path}/")
                };
                let mut entries: Vec<String> = Vec::new();
                let mut dirs: Vec<String> = Vec::new();
                for key in vfs.files.keys() {
                    let Some(relative) = key.strip_prefix(&prefix) else {
                        continue;
                    };
                    match relative.split_once('/') {
                        Some((dir, _)) => {
                            let dir = format!("{prefix}{dir}/");
                            if !dirs.contains(&dir) {
                                dirs.push(dir);
                            }
                        }
                        None => entries.push(key.clone()),
                    }
                }
                entries.extend(dirs);
                entries.sort();
                format_paths(&entries)
            }
        },
        "read_file" => match checked(args.str("file_path")) {
            Err(e) => e,
            Ok(path) => read_file(vfs, &path, args.int("offset"), args.int("limit")),
        },
        "write_file" => match checked(args.str("file_path")) {
            Err(e) => e,
            Ok(path) => {
                let content = args.str("content");
                if bounds.fits(&path, content) {
                    updates.push((path.clone(), Some(content.to_owned())));
                    format!("Updated file {path}")
                } else {
                    format!(
                        "Error: the file system is full ({MAX_FILES} files, {MAX_BYTES} bytes); delete files you no longer need"
                    )
                }
            }
        },
        "edit_file" => match checked(args.str("file_path")) {
            Err(e) => e,
            Ok(path) => match vfs.files.get(&path) {
                None => format!("Error: File '{path}' not found"),
                Some(file) => match replace(
                    &file.content,
                    args.str("old_string"),
                    args.str("new_string"),
                    args.bool("replace_all"),
                ) {
                    Err(e) => e,
                    Ok((content, _)) if !bounds.fits(&path, &content) => format!(
                        "Error: the file system is full ({MAX_FILES} files, {MAX_BYTES} bytes); delete files you no longer need"
                    ),
                    Ok((content, count)) => {
                        updates.push((path.clone(), Some(content)));
                        format!("Successfully replaced {count} instance(s) of the string in '{path}'")
                    }
                },
            },
        },
        "delete" => match checked(args.str("file_path")) {
            Err(e) => e,
            Ok(path) => {
                let base = path.trim_end_matches('/');
                let prefix = format!("{base}/");
                let doomed: Vec<String> = vfs
                    .files
                    .keys()
                    .filter(|k| k.as_str() == base || k.starts_with(&prefix))
                    .cloned()
                    .collect();
                if doomed.is_empty() {
                    format!("Error: File '{path}' not found")
                } else {
                    updates.extend(doomed.into_iter().map(|k| (k, None)));
                    format!("Deleted {path}")
                }
            }
        },
        "glob" => glob_tool(vfs, args.str("pattern"), args.opt_str("path")),
        "grep" => grep_tool(vfs, args),
        "execute" => match args.opt_int("timeout") {
            Some(t) if t < 0 => format!("Error: timeout must be non-negative, got {t}."),
            _ => "Error: Execution not available. This agent's backend does not support command execution (SandboxBackendProtocol). To use the execute tool, provide a backend that implements SandboxBackendProtocol.".to_owned(),
        },
        _ => return None,
    };
    Some((text, updates))
}

/// `_create_content_preview`.
fn content_preview(content: &str) -> String {
    let lines = pystr::splitlines(content);
    let clip = |line: &&str| pyfmt::head(line, 1000).to_owned();
    if lines.len() <= 10 {
        let shown: Vec<String> = lines.iter().map(clip).collect();
        let refs: Vec<&str> = shown.iter().map(String::as_str).collect();
        return number_lines(&refs, 1);
    }
    let head: Vec<String> = lines[..5].iter().map(clip).collect();
    let tail: Vec<String> = lines[lines.len() - 5..].iter().map(clip).collect();
    let head_refs: Vec<&str> = head.iter().map(String::as_str).collect();
    let tail_refs: Vec<&str> = tail.iter().map(String::as_str).collect();
    format!(
        "{}\n... [{} lines truncated] ...\n{}",
        number_lines(&head_refs, 1),
        lines.len() - 10,
        number_lines(&tail_refs, lines.len() - 4)
    )
}

/// The large-result eviction: a result of another tool longer than
/// [`EVICT_CHARS`] is written to `/large_tool_results/{id}` and replaced by
/// `TOO_LARGE_TOOL_MSG`. Returns the text the model sees and the write.
#[must_use]
pub fn evict(
    bounds: &Bounds,
    tool: &str,
    tool_call_id: &str,
    content: String,
) -> (String, Option<Update>) {
    if NOT_EVICTED.contains(&tool) || pyfmt::len(&content) <= EVICT_CHARS {
        return (content, None);
    }
    let sanitized = if tool_call_id.is_empty() {
        "unknown".to_owned()
    } else {
        tool_call_id.replace(['.', '/', '\\'], "_")
    };
    let path = format!("/large_tool_results/{sanitized}");
    if !bounds.fits(&path, &content) {
        // The file system is full: the model gets the head of the result,
        // never all of it, so one large result cannot fill its context.
        let text = format!(
            "{}\n{FULL_FILESYSTEM_NOTE}",
            pyfmt::head(&content, EVICT_CHARS)
        );
        return (text, None);
    }
    let text = format!(
        "Tool result too large, the result of this tool call {tool_call_id} was saved in the filesystem at this path: {path}\n\nYou can read the result from the filesystem by using the read_file tool, but make sure to only read part of the result at a time.\n\nYou can do this by specifying an offset and limit in the read_file tool call. For example, to read the first 100 lines, you can use the read_file tool with offset=0 and limit=100.\n\nHere is a preview showing the head and tail of the result (lines of the form `... [N lines truncated] ...` indicate omitted lines in the middle of the content):\n\n{}\n",
        content_preview(&content)
    );
    (text, Some((path, Some(content))))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_replace_that_would_pass_the_bound_is_refused_before_it_is_built() {
        let content = "a".repeat(1_000_000);
        let new = "b".repeat(1_000);
        let refused = replace(&content, "a", &new, true);
        assert!(
            refused
                .as_ref()
                .is_err_and(|e| e.contains("file system is full")),
            "{refused:?}"
        );
        assert_eq!(replace("aXa", "a", "bb", true), Ok(("bbXbb".to_owned(), 2)));
    }

    #[test]
    fn a_large_result_is_cut_when_the_file_system_is_full() {
        let content = "x".repeat(EVICT_CHARS * 3);
        let full = Bounds {
            sizes: HashMap::new(),
            bytes: MAX_BYTES,
        };
        let (text, update) = evict(&full, "search_codebase", "c1", content);
        assert!(update.is_none());
        assert!(text.ends_with(FULL_FILESYSTEM_NOTE), "{text}");
        assert!(pyfmt::len(&text) <= EVICT_CHARS + 1 + pyfmt::len(FULL_FILESYSTEM_NOTE));
    }

    #[test]
    fn paths_are_contained() {
        assert_eq!(validate_path("a.md").as_deref(), Ok("/a.md"));
        assert_eq!(validate_path("/./x//y/").as_deref(), Ok("/x/y"));
        assert!(validate_path("/../etc/passwd").is_err());
        assert!(validate_path("/findings/../a.md").is_err());
        assert!(validate_path("~/a").is_err());
        assert!(validate_path("C:/x").is_err());
        assert!(validate_path("..\\x").is_err());
    }

    #[test]
    fn line_numbers_pad_to_the_widest_marker() {
        assert_eq!(number_lines(&["x", "y"], 1), "1  x\n2  y");
        assert_eq!(number_lines(&["x"; 10], 1).lines().next(), Some(" 1  x"));
        let long = "a".repeat(5001);
        assert_eq!(
            number_lines(&[long.as_str()], 1).lines().nth(1),
            Some("1.1  a")
        );
    }

    #[test]
    fn the_state_is_bounded() {
        let mut vfs = Vfs::default();
        assert!(!vfs.bounds().fits("/a", &"x".repeat(MAX_BYTES + 1)));
        vfs.apply(vec![("/a".into(), Some("x".into()))]);
        assert!(vfs.bounds().fits("/a", "y"));
    }

    #[test]
    fn the_bounds_count_the_pending_writes() {
        let vfs = Vfs::default();
        let mut bounds = vfs.bounds();
        let half = "x".repeat(MAX_BYTES / 2);
        assert!(bounds.fits("/a", &half));
        bounds.record(&[("/a".into(), Some(half.clone()))]);
        assert!(bounds.fits("/b", &half));
        bounds.record(&[("/b".into(), Some(half.clone()))]);
        // Full: one more byte does not fit, a rewrite of the same size does.
        assert!(!bounds.fits("/c", "y"));
        assert!(bounds.fits("/a", &half));
        // A pending delete frees its bytes.
        bounds.record(&[("/a".into(), None)]);
        assert!(bounds.fits("/c", "y"));
        // The file count too.
        let mut bounds = vfs.bounds();
        let many: Vec<Update> = (0..MAX_FILES)
            .map(|i| (format!("/f{i}"), Some(String::new())))
            .collect();
        bounds.record(&many);
        assert!(!bounds.fits("/one-more", ""));
        assert!(bounds.fits("/f0", "z"));
        // The snapshot itself is unchanged.
        assert_eq!(vfs, Vfs::default());
    }
}
