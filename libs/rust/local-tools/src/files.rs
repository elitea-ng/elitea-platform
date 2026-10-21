//! The file tools' bodies: read, write, edit, patch, search, tree and
//! document text. Each takes the session's [`Workspace`] and
//! [`ReadLedger`]; approval and checkpoints happen around them in
//! [`crate::session`].

use std::path::Path;

use grep_regex::{RegexMatcher, RegexMatcherBuilder};
use grep_searcher::{BinaryDetection, Searcher, SearcherBuilder, Sink, SinkContext, SinkMatch};
use serde::Deserialize;
use serde_json::{Value, json};

use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::ledger::ReadLedger;
use crate::patch;
use crate::workspace::{EntryKind, Intent, ReadFile, Workspace, WsPath, is_protected_name};

/// The largest file the text tools read or write.
pub const MAX_FILE_BYTES: u64 = 16 * 1024 * 1024;
/// The most text one read returns.
pub const MAX_READ_OUTPUT: usize = 256 * 1024;
/// Lines one read returns by default.
pub const DEFAULT_READ_LINES: usize = 2000;
/// The largest document `read_document` opens.
pub const MAX_DOCUMENT_BYTES: u64 = 64 * 1024 * 1024;
/// The most text `read_document` returns.
pub const MAX_DOCUMENT_OUTPUT: usize = 512 * 1024;
/// The longest line a search result shows.
const MAX_MATCH_LINE: usize = 400;

/// Whether `bytes` are text the tools can show and edit: UTF-8 with no NUL
/// in the first 8 KiB.
#[must_use]
pub fn is_text(bytes: &[u8]) -> bool {
    !bytes[..bytes.len().min(8192)].contains(&0) && std::str::from_utf8(bytes).is_ok()
}

fn text_of(read: &ReadFile) -> ToolResult<&str> {
    if !is_text(&read.bytes) {
        return Err(ToolError::new(
            ErrorCode::Binary,
            format!(
                "`{}` is not UTF-8 text; use read_document for documents",
                read.path
            ),
        ));
    }
    std::str::from_utf8(&read.bytes).map_err(|_| ToolError::new(ErrorCode::Binary, "not UTF-8"))
}

fn parse<T: for<'de> Deserialize<'de>>(args: Value) -> ToolResult<T> {
    serde_json::from_value(args)
        .map_err(|error| ToolError::invalid(format!("bad arguments: {error}")))
}

/// Read the current state of `path` for a write: `None` when absent.
fn current(workspace: &Workspace, path: &WsPath) -> ToolResult<Option<ReadFile>> {
    match workspace.read(path, MAX_FILE_BYTES) {
        Ok(read) => Ok(Some(read)),
        Err(error) if error.code() == ErrorCode::NotFound => Ok(None),
        Err(error) => Err(error),
    }
}

#[derive(Deserialize)]
pub(crate) struct ReadArgs {
    pub path: String,
    /// First line, 1-based.
    #[serde(default)]
    pub offset: Option<usize>,
    #[serde(default)]
    pub limit: Option<usize>,
}

/// `read_file`.
pub(crate) fn read(workspace: &Workspace, ledger: &ReadLedger, args: Value) -> ToolResult<Value> {
    let args: ReadArgs = parse(args)?;
    let path = workspace.resolve(&args.path, Intent::Read)?;
    let file = workspace.read(&path, MAX_FILE_BYTES)?;
    // Read as ingestion indexes it (a legacy encoding is decoded), so a file
    // the index holds can be read back. Editing stays UTF-8 only: writing a
    // decoded legacy file back would change its encoding.
    let has_nul = file.bytes[..file.bytes.len().min(8192)].contains(&0);
    let text = if has_nul || is_text(&file.bytes) {
        // UTF-8, or a NUL byte: binary, as before.
        text_of(&file)?.to_owned()
    } else {
        elitea_doc_extract::decode_text(&file.bytes).map_err(|reason| {
            ToolError::new(
                ErrorCode::Binary,
                format!(
                    "`{}` is not text ({reason}); use read_document for documents",
                    file.path
                ),
            )
        })?
    };
    ledger.record(&file.path, file.stamp.clone());
    let lines: Vec<&str> = text.lines().collect();
    let start = args.offset.unwrap_or(1).max(1);
    let limit = args.limit.unwrap_or(DEFAULT_READ_LINES).max(1);
    let mut content = String::new();
    let mut end = start.saturating_sub(1);
    for (index, line) in lines.iter().enumerate().skip(start - 1).take(limit) {
        let numbered = format!("{:>6}\t{line}\n", index + 1);
        if content.len() + numbered.len() > MAX_READ_OUTPUT {
            break;
        }
        content.push_str(&numbered);
        end = index + 1;
    }
    let truncated = end < lines.len();
    Ok(json!({
        "path": file.path.display_string(),
        "content": content,
        "start_line": start,
        "end_line": end,
        "total_lines": lines.len(),
        "truncated": truncated,
        "next_offset": truncated.then_some(end + 1),
    }))
}

#[derive(Deserialize)]
pub(crate) struct WriteArgs {
    pub path: String,
    pub content: String,
}

/// Check that a write of `path` may replace what is there now.
pub(crate) fn check_fresh(
    workspace: &Workspace,
    ledger: &ReadLedger,
    path: &WsPath,
) -> ToolResult<Option<ReadFile>> {
    let now = current(workspace, path)?;
    let key = now
        .as_ref()
        .map_or_else(|| path.clone(), |read| read.path.clone());
    ledger.check_fresh(&key, now.as_ref().map(|read| &read.stamp))?;
    Ok(now)
}

fn write_and_record(
    workspace: &Workspace,
    ledger: &ReadLedger,
    path: &WsPath,
    bytes: &[u8],
) -> ToolResult<WsPath> {
    let written = workspace.write(path, bytes, None)?;
    let read = workspace.read(&written, MAX_FILE_BYTES)?;
    ledger.record(&read.path, read.stamp);
    Ok(written)
}

/// `write_file`, after approval.
pub(crate) fn write(
    workspace: &Workspace,
    ledger: &ReadLedger,
    args: &WriteArgs,
) -> ToolResult<Value> {
    if args.content.len() as u64 > MAX_FILE_BYTES {
        return Err(ToolError::new(
            ErrorCode::TooLarge,
            "the content is too large",
        ));
    }
    let path = workspace.resolve(&args.path, Intent::Write)?;
    let existed = check_fresh(workspace, ledger, &path)?.is_some();
    let written = write_and_record(workspace, ledger, &path, args.content.as_bytes())?;
    Ok(json!({
        "path": written.display_string(),
        "created": !existed,
        "bytes": args.content.len(),
    }))
}

#[derive(Deserialize)]
pub(crate) struct EditArgs {
    pub path: String,
    pub old_string: String,
    pub new_string: String,
    #[serde(default)]
    pub replace_all: bool,
}

/// `edit_file`, after approval: exact string replacement.
pub(crate) fn edit(
    workspace: &Workspace,
    ledger: &ReadLedger,
    args: &EditArgs,
) -> ToolResult<Value> {
    if args.old_string.is_empty() {
        return Err(ToolError::invalid(
            "old_string is empty; use write_file to create a file",
        ));
    }
    if args.old_string == args.new_string {
        return Err(ToolError::invalid("old_string and new_string are the same"));
    }
    let path = workspace.resolve(&args.path, Intent::Write)?;
    let now = check_fresh(workspace, ledger, &path)?
        .ok_or_else(|| ToolError::new(ErrorCode::NotFound, format!("`{path}` does not exist")))?;
    let text = text_of(&now)?;
    let count = text.matches(args.old_string.as_str()).count();
    if count == 0 {
        return Err(ToolError::new(
            ErrorCode::Conflict,
            format!("old_string does not occur in `{path}`"),
        ));
    }
    if count > 1 && !args.replace_all {
        return Err(ToolError::new(
            ErrorCode::Conflict,
            format!(
                "old_string occurs {count} times in `{path}`; add context to make it unique, or set replace_all"
            ),
        ));
    }
    let updated = text.replace(args.old_string.as_str(), &args.new_string);
    let written = write_and_record(workspace, ledger, &now.path, updated.as_bytes())?;
    Ok(json!({ "path": written.display_string(), "replacements": count }))
}

/// One file of a checked patch, ready to write.
pub(crate) struct PlannedChange {
    /// Where the change lands (in-workspace symlinks followed).
    pub path: WsPath,
    /// The path as the patch names it.
    pub typed: WsPath,
    /// `None`: delete.
    pub content: Option<String>,
    /// Permission bits to write with (a renamed file keeps its own, a
    /// created file may name them); `None`: keep the existing or default.
    pub mode: Option<u32>,
}

/// Parse a patch, resolve its paths and compute every result, before
/// anything is written; also the paths the approval names. A rename is two
/// changes: the new path created with the (patched) content and mode of the
/// old one, and the old path deleted.
pub(crate) fn plan_patch(
    workspace: &Workspace,
    ledger: &ReadLedger,
    text: &str,
) -> ToolResult<Vec<PlannedChange>> {
    let mut planned: Vec<PlannedChange> = Vec::new();
    let push = |planned: &mut Vec<PlannedChange>, change: PlannedChange| {
        if planned.iter().any(|done| done.path == change.path) {
            return Err(ToolError::invalid(format!(
                "the patch changes `{}` more than once; put all of its hunks in one section",
                change.path
            )));
        }
        planned.push(change);
        Ok(())
    };
    for file in patch::parse(text)? {
        if file.is_rename() {
            let (Some(old), Some(new)) = (&file.old_path, &file.new_path) else {
                return Err(ToolError::invalid("a rename names both paths"));
            };
            let (old_typed, old_target, original) = existing(workspace, ledger, old)?;
            let (new_typed, new_target) = absent(workspace, ledger, new)?;
            let content = patch::apply(text_of(&original)?, &file)?.ok_or_else(|| {
                ToolError::invalid(format!("the rename of `{old_typed}` deletes it"))
            })?;
            push(
                &mut planned,
                PlannedChange {
                    path: new_target,
                    typed: new_typed,
                    content: Some(content),
                    mode: Some(original.mode),
                },
            )?;
            push(
                &mut planned,
                PlannedChange {
                    path: old_target,
                    typed: old_typed,
                    content: None,
                    mode: None,
                },
            )?;
            continue;
        }
        if let (Some(old), Some(new)) = (&file.old_path, &file.new_path)
            && old != new
        {
            return Err(ToolError::new(
                ErrorCode::Unsupported,
                "a section names two different paths; a rename needs git's `rename from` / `rename to` lines",
            ));
        }
        let (typed, target, content) = if file.is_create() {
            let (typed, target) = absent(workspace, ledger, file.target())?;
            (typed, target, patch::apply("", &file)?)
        } else {
            let (typed, target, read) = existing(workspace, ledger, file.target())?;
            (typed, target, patch::apply(text_of(&read)?, &file)?)
        };
        push(
            &mut planned,
            PlannedChange {
                path: target,
                typed,
                content,
                mode: file.created_mode(),
            },
        )?;
    }
    Ok(planned)
}

/// A file a patch changes: its typed path, where it lands, and what it
/// holds now (read-before-write guard checked).
fn existing(
    workspace: &Workspace,
    ledger: &ReadLedger,
    argument: &str,
) -> ToolResult<(WsPath, WsPath, ReadFile)> {
    let path = workspace.resolve(argument, Intent::Write)?;
    let read = check_fresh(workspace, ledger, &path)?
        .ok_or_else(|| ToolError::new(ErrorCode::NotFound, format!("`{path}` does not exist")))?;
    let target = read.path.clone();
    workspace.check(&target, Intent::Write)?;
    Ok((path, target, read))
}

/// A file a patch creates: its typed path and where it lands; it must not
/// exist.
fn absent(
    workspace: &Workspace,
    ledger: &ReadLedger,
    argument: &str,
) -> ToolResult<(WsPath, WsPath)> {
    let path = workspace.resolve(argument, Intent::Write)?;
    if current(workspace, &path)?.is_some() || check_fresh(workspace, ledger, &path)?.is_some() {
        return Err(ToolError::new(
            ErrorCode::Conflict,
            format!("`{path}` already exists"),
        ));
    }
    let target = workspace.final_target(&path)?;
    workspace.check(&target, Intent::Write)?;
    Ok((path, target))
}

/// `apply_patch`, after approval: every file or none. Each file is
/// replaced atomically; when one cannot be written, the files already
/// changed are put back as they were.
pub(crate) fn apply_patch(
    workspace: &Workspace,
    ledger: &ReadLedger,
    planned: &[PlannedChange],
) -> ToolResult<Value> {
    // Re-check: the person may have edited a file while the approval was
    // pending. What is there now is what a rollback restores.
    let mut originals = Vec::new();
    for change in planned {
        let now = check_fresh(workspace, ledger, &change.path)?;
        originals.push(now.map(|read| (read.bytes, read.mode)));
    }
    let mut changed = Vec::new();
    for (index, change) in planned.iter().enumerate() {
        let applied = match &change.content {
            Some(content) => workspace
                .write(&change.path, content.as_bytes(), change.mode)
                .and_then(|written| workspace.read(&written, MAX_FILE_BYTES))
                .map(|read| ledger.record(&read.path, read.stamp)),
            None => workspace.remove_file(&change.path).map(|_| {
                ledger.forget(&change.path);
            }),
        };
        if let Err(error) = applied {
            for (done, original) in planned[..index].iter().zip(&originals).rev() {
                let restored = match original {
                    Some((bytes, mode)) => {
                        workspace.write(&done.path, bytes, Some(*mode)).map(|_| ())
                    }
                    None => workspace.remove_file(&done.path).map(|_| ()),
                };
                if let Err(rollback) = restored {
                    tracing::warn!(
                        path = %done.path,
                        reason = rollback.message(),
                        "a patch could not be rolled back"
                    );
                }
                ledger.forget(&done.path);
            }
            return Err(ToolError::new(
                error.code(),
                format!(
                    "the patch was not applied (`{}`: {}); nothing changed",
                    change.path,
                    error.message()
                ),
            ));
        }
        changed.push(change.path.display_string());
    }
    Ok(json!({ "changed": changed }))
}

/// What `search_files` answers with.
#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
pub(crate) enum OutputMode {
    /// Matching lines (and context lines).
    #[default]
    Content,
    /// The paths of matching files only.
    Files,
    /// Matches per file.
    Count,
}

#[derive(Deserialize)]
pub(crate) struct SearchArgs {
    pub pattern: String,
    #[serde(default)]
    pub path: Option<String>,
    /// Only files matching this glob (`*.rs`, `src/**/*.ts`).
    #[serde(default)]
    pub glob: Option<String>,
    #[serde(default)]
    pub fixed_strings: bool,
    #[serde(default)]
    pub case_insensitive: bool,
    #[serde(default)]
    pub max_results: Option<usize>,
    #[serde(default)]
    pub output_mode: OutputMode,
    /// Lines before each match (`-B`).
    #[serde(default)]
    pub context_before: Option<usize>,
    /// Lines after each match (`-A`).
    #[serde(default)]
    pub context_after: Option<usize>,
    /// Lines before and after (`-C`); the two above win over it.
    #[serde(default)]
    pub context: Option<usize>,
    /// Let the pattern span lines (`rg -U`).
    #[serde(default)]
    pub multiline: bool,
    /// The most matches one file contributes (`-m`).
    #[serde(default)]
    pub max_per_file: Option<usize>,
}

/// The most context lines on either side of a match.
const MAX_CONTEXT: usize = 20;
/// Matches `search_files` answers by default.
const DEFAULT_SEARCH_RESULTS: usize = 200;

fn base_dir(workspace: &Workspace, path: Option<&str>) -> ToolResult<WsPath> {
    let base = match path {
        Some(path) => workspace.resolve(path, Intent::Read)?,
        None => WsPath::root(),
    };
    match workspace.stat(&base)? {
        Some(EntryKind::Dir) => Ok(base),
        Some(_) => Err(ToolError::invalid(format!("`{base}` is not a directory"))),
        None => Err(ToolError::new(
            ErrorCode::NotFound,
            format!("`{base}` does not exist"),
        )),
    }
}

/// A glob over workspace files, as `search_files` and `find_files` take it:
/// one with a `/` matches the path from the workspace root
/// (`src/**/*.rs`), one without matches the file name at any depth
/// (`*.rs`); a leading `./` or `/` is dropped.
fn file_glob(glob: &str) -> ToolResult<(globset::GlobMatcher, bool)> {
    let trimmed = glob.trim();
    let trimmed = trimmed.strip_prefix("./").unwrap_or(trimmed);
    let trimmed = trimmed.trim_start_matches('/');
    if trimmed.is_empty() {
        return Err(ToolError::invalid("the glob is empty"));
    }
    let matcher = globset::GlobBuilder::new(trimmed)
        .literal_separator(true)
        .build()
        .map_err(|_| ToolError::invalid(format!("`{glob}` is not a glob")))?
        .compile_matcher();
    Ok((matcher, trimmed.contains('/')))
}

/// The walk every listing tool shares (see [`walker`]); the glob is a
/// filter on files, so `.gitignore` still applies under it.
fn walk_builder(
    workspace: &Workspace,
    base: &WsPath,
    glob: Option<&str>,
    depth: Option<usize>,
) -> ToolResult<ignore::WalkBuilder> {
    let mut builder = ignore::WalkBuilder::new(workspace.absolute(base));
    let denied = workspace.read_denied_filter();
    let glob = glob.map(file_glob).transpose()?;
    let root = workspace.root().to_path_buf();
    builder
        .hidden(false)
        .require_git(false)
        .follow_links(false)
        .max_depth(depth)
        // `.git`, `path_deny` and the session's deny list are pruned here,
        // never filtered after: a denied tree is never descended into, so
        // it cannot use up a walk's limit (nor a parallel walker's time).
        .filter_entry(move |entry| {
            if entry.file_name().to_str().is_some_and(is_protected_name) || denied(entry.path()) {
                return false;
            }
            let Some((glob, whole_path)) = &glob else {
                return true;
            };
            if entry.file_type().is_some_and(|kind| kind.is_dir()) {
                return true;
            }
            if *whole_path {
                entry
                    .path()
                    .strip_prefix(&root)
                    .is_ok_and(|relative| glob.is_match(relative))
            } else {
                glob.is_match(entry.file_name())
            }
        });
    Ok(builder)
}

/// A walk of the workspace from `base` the way every listing tool walks it:
/// `.gitignore` honoured (with or without a git repository), hidden files
/// included, `.git` and `path_deny` matches pruned (never descended into),
/// symlinks yielded as entries but never followed, names in byte order.
/// `glob` narrows it to matching files (`.gitignore` still applies);
/// `depth` limits it.
///
/// Public for the desktop's local index (ADR-0029 decision 7), which lists
/// a workspace exactly as the agent's tools see it. A caller that wants
/// files keeps only the entries whose `file_type()` is a regular file.
///
/// # Errors
///
/// `glob` is not a glob.
pub fn walker(
    workspace: &Workspace,
    base: &WsPath,
    glob: Option<&str>,
    depth: Option<usize>,
) -> ToolResult<ignore::Walk> {
    let mut builder = walk_builder(workspace, base, glob, depth)?;
    builder.sort_by_file_name(Ord::cmp);
    Ok(builder.build())
}

/// A path [`walker`] yielded, as a workspace path; `None` for one outside
/// the root or not UTF-8.
#[must_use]
pub fn relative(workspace: &Workspace, path: &Path) -> Option<WsPath> {
    WsPath::from_relative(path.strip_prefix(workspace.root()).ok()?).ok()
}

/// [`walker`] for many threads: the same pruning (`.git`, `path_deny`
/// and the session's deny list in `filter_entry`, never filtered after),
/// the same glob, symlinks never followed; entries arrive in no particular
/// order, so a caller sorts what it collects.
///
/// # Errors
///
/// `glob` is not a glob.
pub fn walker_parallel(
    workspace: &Workspace,
    base: &WsPath,
    glob: Option<&str>,
    depth: Option<usize>,
) -> ToolResult<ignore::WalkParallel> {
    let threads = std::thread::available_parallelism().map_or(1, |n| n.get().min(8));
    let mut builder = walk_builder(workspace, base, glob, depth)?;
    builder.threads(threads);
    Ok(builder.build_parallel())
}

/// One line a search shows.
struct Hit {
    line: u64,
    text: String,
    matched: bool,
}

/// One searched file: its matches, and the lines to show when they are kept.
struct FileHits {
    path: WsPath,
    matches: u64,
    hits: Vec<Hit>,
}

fn shown_line(bytes: &[u8]) -> String {
    let line = String::from_utf8_lossy(bytes);
    line.trim_end_matches(['\n', '\r'])
        .chars()
        .take(MAX_MATCH_LINE)
        .collect()
}

/// A sink keeping one file's matches (each line of a multiline match) and
/// context lines, in file order.
struct Collect {
    keep_lines: bool,
    matches: u64,
    hits: Vec<Hit>,
}

impl Sink for Collect {
    type Error = std::io::Error;

    fn matched(&mut self, _: &Searcher, found: &SinkMatch<'_>) -> Result<bool, std::io::Error> {
        self.matches += 1;
        if self.keep_lines {
            let first = found.line_number().unwrap_or(0);
            for (line, text) in (first..).zip(found.lines()) {
                self.hits.push(Hit {
                    line,
                    text: shown_line(text),
                    matched: true,
                });
            }
        }
        Ok(true)
    }

    fn context(&mut self, _: &Searcher, context: &SinkContext<'_>) -> Result<bool, std::io::Error> {
        if self.keep_lines {
            self.hits.push(Hit {
                line: context.line_number().unwrap_or(0),
                text: shown_line(context.bytes()),
                matched: false,
            });
        }
        Ok(true)
    }
}

/// What one search runs with, shared by every walker thread.
struct SearchPlan {
    matcher: RegexMatcher,
    /// Each thread builds its own searcher (a searcher is not `Sync`).
    searcher: SearcherBuilder,
    keep_lines: bool,
    /// The walk stops once this many results are collected.
    stop_after: u64,
    mode: OutputMode,
}

impl SearchPlan {
    fn new(args: &SearchArgs, max: usize) -> ToolResult<Self> {
        let matcher = RegexMatcherBuilder::new()
            .case_insensitive(args.case_insensitive)
            .fixed_strings(args.fixed_strings)
            .multi_line(args.multiline)
            .build(&args.pattern)
            .map_err(|error| ToolError::invalid(format!("bad pattern: {error}")))?;
        let keep_lines = args.output_mode == OutputMode::Content;
        let context = |side: Option<usize>| {
            if keep_lines {
                side.or(args.context).unwrap_or(0).min(MAX_CONTEXT)
            } else {
                0
            }
        };
        let per_file = match args.output_mode {
            OutputMode::Files => Some(1),
            _ => args.max_per_file.map(|max| max.clamp(1, 1000) as u64),
        };
        let mut searcher = SearcherBuilder::new();
        searcher
            .line_number(true)
            .binary_detection(BinaryDetection::quit(0))
            .multi_line(args.multiline)
            .before_context(context(args.context_before))
            .after_context(context(args.context_after))
            .max_matches(per_file);
        Ok(Self {
            matcher,
            searcher,
            keep_lines,
            stop_after: max as u64,
            mode: args.output_mode,
        })
    }

    /// What one file adds toward [`Self::stop_after`].
    fn weight(&self, file: &FileHits) -> u64 {
        match self.mode {
            OutputMode::Content => file.hits.iter().filter(|hit| hit.matched).count() as u64,
            OutputMode::Files | OutputMode::Count => u64::from(file.matches > 0),
        }
    }

    /// Search one walked file: `None` when it is not a regular file, is
    /// denied or cannot be opened (confined, `O_NOFOLLOW`).
    fn search_file(
        &self,
        workspace: &Workspace,
        searcher: &mut Searcher,
        entry: &ignore::DirEntry,
    ) -> Option<FileHits> {
        if !entry.file_type().is_some_and(|kind| kind.is_file()) {
            return None;
        }
        let path = relative(workspace, entry.path())?;
        if workspace.denied_by(&path, Intent::Read).is_some() {
            return None;
        }
        let (file, _) = workspace.open_file(&path).ok()?;
        let mut sink = Collect {
            keep_lines: self.keep_lines,
            matches: 0,
            hits: Vec::new(),
        };
        let _ = searcher.search_file(&self.matcher, &file, &mut sink);
        Some(FileHits {
            path,
            matches: sink.matches,
            hits: sink.hits,
        })
    }
}

/// What a walk collected: the files with matches, how many were searched,
/// and whether it stopped early.
struct Collected {
    files: Vec<FileHits>,
    searched: usize,
    stopped: bool,
}

/// Search with the parallel walker: workers send each searched file down a
/// bounded channel; once the plan's limit is collected they stop.
fn collect_parallel(
    workspace: &Workspace,
    base: &WsPath,
    glob: Option<&str>,
    plan: &SearchPlan,
) -> ToolResult<Collected> {
    use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
    let walk = walker_parallel(workspace, base, glob, None)?;
    let collected = AtomicU64::new(0);
    let stopped = AtomicBool::new(false);
    let (sender, receiver) = std::sync::mpsc::sync_channel::<FileHits>(256);
    let (files, searched) = std::thread::scope(|scope| {
        let gather = scope.spawn(move || {
            let mut files = Vec::new();
            let mut searched = 0usize;
            for file in receiver {
                searched += 1;
                if file.matches > 0 {
                    files.push(file);
                }
            }
            (files, searched)
        });
        walk.run(|| {
            let sender = sender.clone();
            let mut searcher = plan.searcher.build();
            let (collected, stopped) = (&collected, &stopped);
            Box::new(move |entry| {
                if collected.load(Ordering::Relaxed) >= plan.stop_after {
                    stopped.store(true, Ordering::Relaxed);
                    return ignore::WalkState::Quit;
                }
                let Ok(entry) = entry else {
                    return ignore::WalkState::Continue;
                };
                let Some(file) = plan.search_file(workspace, &mut searcher, &entry) else {
                    return ignore::WalkState::Continue;
                };
                collected.fetch_add(plan.weight(&file), Ordering::Relaxed);
                if sender.send(file).is_err() {
                    return ignore::WalkState::Quit;
                }
                ignore::WalkState::Continue
            })
        });
        drop(sender);
        gather.join()
    })
    .map_err(|_| ToolError::new(ErrorCode::Io, "the search stopped unexpectedly"))?;
    Ok(Collected {
        files,
        searched,
        stopped: stopped.load(Ordering::Relaxed),
    })
}

/// [`collect_parallel`] on the serial [`walker`] (the reference the
/// parallel walk is checked against).
#[cfg(test)]
fn collect_serial(
    workspace: &Workspace,
    base: &WsPath,
    glob: Option<&str>,
    plan: &SearchPlan,
) -> ToolResult<Collected> {
    let mut searcher = plan.searcher.build();
    let mut collected = Collected {
        files: Vec::new(),
        searched: 0,
        stopped: false,
    };
    let mut weight = 0;
    for entry in walker(workspace, base, glob, None)?.flatten() {
        if weight >= plan.stop_after {
            collected.stopped = true;
            break;
        }
        if let Some(file) = plan.search_file(workspace, &mut searcher, &entry) {
            collected.searched += 1;
            weight += plan.weight(&file);
            if file.matches > 0 {
                collected.files.push(file);
            }
        }
    }
    Ok(collected)
}

/// The answer, from what a walk collected: files in path order (the
/// serial walker's order), then lines in file order, at most `max`.
fn search_answer(mut collected: Collected, mode: OutputMode, max: usize) -> Value {
    collected
        .files
        .sort_by(|a, b| a.path.components().cmp(b.path.components()));
    let mut truncated = collected.stopped;
    let files = collected.files;
    let mut answer = match mode {
        OutputMode::Content => {
            let mut matches = Vec::new();
            let mut shown = 0usize;
            'files: for file in &files {
                let path = file.path.display_string();
                for hit in &file.hits {
                    if hit.matched {
                        if shown == max {
                            truncated = true;
                            break 'files;
                        }
                        shown += 1;
                    }
                    matches.push(json!({
                        "path": path,
                        "line": hit.line,
                        "text": hit.text,
                        "kind": if hit.matched { "match" } else { "context" },
                    }));
                }
            }
            json!({ "matches": matches })
        }
        OutputMode::Files => {
            truncated |= files.len() > max;
            let paths: Vec<String> = files
                .iter()
                .take(max)
                .map(|file| file.path.display_string())
                .collect();
            json!({ "files": paths })
        }
        OutputMode::Count => {
            truncated |= files.len() > max;
            let total: u64 = files.iter().map(|file| file.matches).sum();
            let counts: Vec<Value> = files
                .iter()
                .take(max)
                .map(|file| json!({ "path": file.path.display_string(), "count": file.matches }))
                .collect();
            json!({ "counts": counts, "total": total })
        }
    };
    if let Value::Object(map) = &mut answer {
        map.insert("files_searched".to_owned(), json!(collected.searched));
        map.insert("truncated".to_owned(), json!(truncated));
    }
    answer
}

/// `search_files`: a regex (or literal) over the workspace's text files,
/// `.gitignore` honoured, denied paths pruned; matching lines with
/// optional context, the matching files, or counts per file. The walk runs
/// on several threads; the answer is sorted, so it does not depend on them.
pub(crate) fn search(workspace: &Workspace, args: Value) -> ToolResult<Value> {
    let args: SearchArgs = parse(args)?;
    let max = args
        .max_results
        .unwrap_or(DEFAULT_SEARCH_RESULTS)
        .clamp(1, 1000);
    let plan = SearchPlan::new(&args, max)?;
    let base = base_dir(workspace, args.path.as_deref())?;
    let collected = collect_parallel(workspace, &base, args.glob.as_deref(), &plan)?;
    Ok(search_answer(collected, plan.mode, max))
}

#[derive(Deserialize)]
pub(crate) struct TreeArgs {
    #[serde(default)]
    pub path: Option<String>,
    #[serde(default)]
    pub depth: Option<usize>,
    #[serde(default)]
    pub max_entries: Option<usize>,
}

/// `list_tree`: directories (with `/`) and files with sizes, ignored files
/// and denied paths left out.
pub(crate) fn tree(workspace: &Workspace, args: Value) -> ToolResult<Value> {
    let args: TreeArgs = parse(args)?;
    let depth = args.depth.unwrap_or(3).clamp(1, 12);
    let max = args.max_entries.unwrap_or(500).clamp(1, 5000);
    let base = base_dir(workspace, args.path.as_deref())?;
    let mut entries = Vec::new();
    let mut truncated = false;
    for entry in walker(workspace, &base, None, Some(depth))?.flatten() {
        let Some(path) = relative(workspace, entry.path()) else {
            continue;
        };
        if path == base || workspace.denied_by(&path, Intent::Read).is_some() {
            continue;
        }
        if entries.len() >= max {
            truncated = true;
            break;
        }
        let kind = entry.file_type();
        let shown = path.display_string();
        entries.push(if kind.is_some_and(|kind| kind.is_dir()) {
            json!({ "path": format!("{shown}/") })
        } else if kind.is_some_and(|kind| kind.is_symlink()) {
            json!({ "path": shown, "symlink": true })
        } else {
            json!({ "path": shown, "size": entry.metadata().map_or(0, |meta| meta.len()) })
        });
    }
    Ok(json!({ "root": base.display_string(), "entries": entries, "truncated": truncated }))
}

#[derive(Deserialize)]
pub(crate) struct FindArgs {
    pub pattern: String,
    #[serde(default)]
    pub path: Option<String>,
    #[serde(default)]
    pub fuzzy: bool,
    #[serde(default)]
    pub max_results: Option<usize>,
}

/// Files `find_files` answers by default.
const DEFAULT_FIND_RESULTS: usize = 100;
/// The most files one `find_files` call looks at: a huge folder answers
/// from its first part rather than stalling the turn.
pub const MAX_FIND_VISITED: usize = 200_000;

/// The regular files [`walker`] yields under `base` (narrowed by `glob`),
/// as workspace paths, with their modification time; at most `limit`
/// looked at. `true` when the limit cut the walk short.
fn walked_files(
    workspace: &Workspace,
    base: &WsPath,
    glob: Option<&str>,
    limit: usize,
) -> ToolResult<(Vec<(WsPath, std::time::SystemTime)>, bool)> {
    let mut files = Vec::new();
    let mut visited = 0usize;
    for entry in walker(workspace, base, glob, None)?.flatten() {
        visited += 1;
        if visited > limit {
            return Ok((files, true));
        }
        // Regular files only: a symlink is neither listed nor followed.
        if !entry.file_type().is_some_and(|kind| kind.is_file()) {
            continue;
        }
        let Some(path) = relative(workspace, entry.path()) else {
            continue;
        };
        if workspace.denied_by(&path, Intent::Read).is_some() {
            continue;
        }
        let modified = entry
            .metadata()
            .ok()
            .and_then(|meta| meta.modified().ok())
            .unwrap_or(std::time::UNIX_EPOCH);
        files.push((path, modified));
    }
    Ok((files, false))
}

/// `find_files`: workspace files by glob, newest first, or by a fuzzy name
/// fragment, best first. The same view as `list_tree` (`.gitignore`
/// honoured, `.git`, `path_deny` and the session's deny list pruned,
/// symlinks never followed); names only, nothing is opened.
pub(crate) fn find_files(workspace: &Workspace, args: Value) -> ToolResult<Value> {
    let args: FindArgs = parse(args)?;
    let pattern = args.pattern.trim();
    if pattern.is_empty() {
        return Err(ToolError::invalid("pattern is empty"));
    }
    let max = args
        .max_results
        .unwrap_or(DEFAULT_FIND_RESULTS)
        .clamp(1, 1000);
    let base = base_dir(workspace, args.path.as_deref())?;
    if args.fuzzy {
        let (files, cut) = walked_files(workspace, &base, None, MAX_FIND_VISITED)?;
        let ranked = fuzzy_rank(pattern, files.into_iter().map(|(path, _)| path));
        let total = ranked.len();
        let shown: Vec<Value> = ranked
            .into_iter()
            .take(max)
            .map(|(path, score)| json!({ "path": path.display_string(), "score": score }))
            .collect();
        return Ok(json!({
            "files": shown,
            "matched": total,
            "truncated": total > max || cut,
        }));
    }
    let (mut files, cut) = walked_files(workspace, &base, Some(pattern), MAX_FIND_VISITED)?;
    // Newest first; equal times in path order, so the answer is stable.
    files.sort_by(|a, b| {
        b.1.cmp(&a.1)
            .then_with(|| a.0.components().cmp(b.0.components()))
    });
    let total = files.len();
    let shown: Vec<String> = files
        .into_iter()
        .take(max)
        .map(|(path, _)| path.display_string())
        .collect();
    Ok(json!({
        "files": shown,
        "matched": total,
        "truncated": total > max || cut,
    }))
}

/// `paths` that fuzzily match `pattern` (nucleo's path scoring, smart
/// case), best first; equal scores shorter, then in path order.
fn fuzzy_rank(pattern: &str, paths: impl Iterator<Item = WsPath>) -> Vec<(WsPath, u32)> {
    use nucleo_matcher::pattern::{AtomKind, CaseMatching, Normalization, Pattern};
    use nucleo_matcher::{Config, Matcher, Utf32Str};
    let mut matcher = Matcher::new(Config::DEFAULT.match_paths());
    let pattern = Pattern::new(
        pattern,
        CaseMatching::Smart,
        Normalization::Smart,
        AtomKind::Fuzzy,
    );
    let mut buffer = Vec::new();
    let mut ranked: Vec<(WsPath, String, u32)> = paths
        .filter_map(|path| {
            let shown = path.display_string();
            let score = pattern.score(Utf32Str::new(&shown, &mut buffer), &mut matcher)?;
            Some((path, shown, score))
        })
        .collect();
    ranked.sort_by(|a, b| {
        b.2.cmp(&a.2)
            .then_with(|| a.1.len().cmp(&b.1.len()))
            .then_with(|| a.0.components().cmp(b.0.components()))
    });
    ranked
        .into_iter()
        .map(|(path, _, score)| (path, score))
        .collect()
}

#[derive(Deserialize)]
pub(crate) struct DocumentArgs {
    pub path: String,
}

/// `read_document`: text from PDF, Office, e-mail and HTML (with the
/// `documents` feature), or text files.
pub(crate) fn document(workspace: &Workspace, args: Value) -> ToolResult<Value> {
    let args: DocumentArgs = parse(args)?;
    let path = workspace.resolve(&args.path, Intent::Read)?;
    let file = workspace.read(&path, MAX_DOCUMENT_BYTES)?;
    let name = file.path.file_name().unwrap_or_default();
    let mime = elitea_content_source::mime_of(name);
    match elitea_doc_extract::extract(mime, &file.bytes) {
        elitea_doc_extract::Extracted::Text { text, extractor } => {
            let truncated = text.len() > MAX_DOCUMENT_OUTPUT;
            let mut end = text.len().min(MAX_DOCUMENT_OUTPUT);
            while !text.is_char_boundary(end) {
                end -= 1;
            }
            Ok(json!({
                "path": file.path.display_string(),
                "mime_type": mime,
                "extractor": extractor,
                "text": &text[..end],
                "truncated": truncated,
            }))
        }
        elitea_doc_extract::Extracted::Unsupported => Err(ToolError::new(
            ErrorCode::Unsupported,
            format!("this build cannot extract text from {mime}"),
        )),
        elitea_doc_extract::Extracted::Unreadable(reason) => Err(ToolError::new(
            ErrorCode::Binary,
            format!("`{path}` is unreadable as {mime}: {reason}"),
        )),
    }
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::{
        EditArgs, WriteArgs, apply_patch, document, edit, find_files, plan_patch, read, search,
        tree, write,
    };
    use crate::error::ErrorCode;
    use crate::ledger::ReadLedger;
    use crate::workspace::Workspace;

    fn setup(deny: &[&str]) -> (tempfile::TempDir, Workspace, ReadLedger) {
        let dir = tempfile::tempdir().expect("dir");
        let deny: Vec<String> = deny.iter().map(|s| (*s).to_owned()).collect();
        let workspace = Workspace::open(dir.path(), &deny).expect("workspace");
        (dir, workspace, ReadLedger::new())
    }

    fn seed(dir: &tempfile::TempDir, name: &str, content: &str) {
        let path = dir.path().join(name);
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent).expect("dirs");
        }
        std::fs::write(path, content).expect("seed");
    }

    fn write_args(path: &str, content: &str) -> WriteArgs {
        WriteArgs {
            path: path.to_owned(),
            content: content.to_owned(),
        }
    }

    fn edit_args(path: &str, old: &str, new: &str) -> EditArgs {
        EditArgs {
            path: path.to_owned(),
            old_string: old.to_owned(),
            new_string: new.to_owned(),
            replace_all: false,
        }
    }

    #[test]
    fn reads_number_lines_page_and_refuse_binaries() {
        let (dir, ws, ledger) = setup(&[]);
        seed(&dir, "a.txt", "one\ntwo\nthree\n");
        let all = read(&ws, &ledger, json!({ "path": "a.txt" })).expect("read");
        assert_eq!(all["content"], "     1\tone\n     2\ttwo\n     3\tthree\n");
        assert_eq!(all["truncated"], false);
        let page = read(
            &ws,
            &ledger,
            json!({ "path": "a.txt", "offset": 2, "limit": 1 }),
        )
        .expect("page");
        assert_eq!(page["content"], "     2\ttwo\n");
        assert_eq!(page["next_offset"], 3);
        std::fs::write(dir.path().join("bin"), [0u8, 1, 2, 3]).expect("bin");
        let binary = read(&ws, &ledger, json!({ "path": "bin" })).expect_err("binary");
        assert_eq!(binary.code(), ErrorCode::Binary);
        assert_eq!(
            read(&ws, &ledger, json!({ "path": "../x" }))
                .expect_err("escape")
                .code(),
            ErrorCode::OutsideWorkspace
        );
        assert_eq!(
            read(&ws, &ledger, json!({ "nope": 1 }))
                .expect_err("args")
                .code(),
            ErrorCode::InvalidArgument
        );
    }

    #[test]
    fn read_decodes_a_legacy_encoding_and_refuses_binary_without_a_nul() {
        let (dir, ws, ledger) = setup(&[]);
        let (sjis, _, _) = encoding_rs::SHIFT_JIS
            .encode("これは日本語の文章です。\n返金は三十日以内に受け付けます。\n");
        std::fs::write(dir.path().join("jp.txt"), &*sjis).expect("write");
        let shown = read(&ws, &ledger, json!({ "path": "jp.txt" })).expect("read");
        assert!(
            shown["content"].as_str().unwrap_or("").contains("返金"),
            "{shown}"
        );
        // Binary with no NUL byte is still refused.
        let noise: Vec<u8> = (0..2000_usize)
            .map(|i| [1_u8, 2, 3, 0x81, 4][i % 5])
            .collect();
        std::fs::write(dir.path().join("noise"), noise).expect("noise");
        let refused = read(&ws, &ledger, json!({ "path": "noise" })).expect_err("noise");
        assert_eq!(refused.code(), ErrorCode::Binary);
    }

    #[test]
    fn read_before_write_guards_writes_and_edits() {
        let (dir, ws, ledger) = setup(&[]);
        seed(&dir, "a.txt", "alpha beta\n");
        assert_eq!(
            write(&ws, &ledger, &write_args("a.txt", "x"))
                .expect_err("unread")
                .code(),
            ErrorCode::StaleRead
        );
        assert_eq!(
            edit(&ws, &ledger, &edit_args("a.txt", "alpha", "ALPHA"))
                .expect_err("unread")
                .code(),
            ErrorCode::StaleRead
        );
        read(&ws, &ledger, json!({ "path": "a.txt" })).expect("read");
        edit(&ws, &ledger, &edit_args("a.txt", "alpha", "ALPHA")).expect("edit");
        // Its own edit keeps the ledger current: a second edit needs no re-read.
        edit(&ws, &ledger, &edit_args("a.txt", "beta", "BETA")).expect("second edit");
        assert_eq!(
            std::fs::read_to_string(dir.path().join("a.txt")).expect("content"),
            "ALPHA BETA\n"
        );
        // Someone else changes it: refused until read again.
        std::fs::write(dir.path().join("a.txt"), "changed by the person\n").expect("external");
        assert_eq!(
            edit(&ws, &ledger, &edit_args("a.txt", "person", "agent"))
                .expect_err("stale")
                .code(),
            ErrorCode::StaleRead
        );
        read(&ws, &ledger, json!({ "path": "a.txt" })).expect("re-read");
        edit(&ws, &ledger, &edit_args("a.txt", "person", "agent")).expect("edit after re-read");
        // New files need no read.
        let created = write(&ws, &ledger, &write_args("new/file.txt", "hi")).expect("create");
        assert_eq!(created["created"], true);
    }

    #[test]
    fn edits_must_match_exactly_once_unless_replace_all() {
        let (dir, ws, ledger) = setup(&[]);
        seed(&dir, "a.txt", "x x x\n");
        read(&ws, &ledger, json!({ "path": "a.txt" })).expect("read");
        assert_eq!(
            edit(&ws, &ledger, &edit_args("a.txt", "x", "y"))
                .expect_err("ambiguous")
                .code(),
            ErrorCode::Conflict
        );
        assert_eq!(
            edit(&ws, &ledger, &edit_args("a.txt", "z", "y"))
                .expect_err("absent")
                .code(),
            ErrorCode::Conflict
        );
        let all = EditArgs {
            replace_all: true,
            ..edit_args("a.txt", "x", "y")
        };
        assert_eq!(edit(&ws, &ledger, &all).expect("all")["replacements"], 3);
        assert_eq!(
            edit(&ws, &ledger, &edit_args("a.txt", "", "y"))
                .expect_err("empty")
                .code(),
            ErrorCode::InvalidArgument
        );
    }

    #[test]
    fn patches_apply_atomically_with_the_read_guard() {
        let (dir, ws, ledger) = setup(&[]);
        seed(&dir, "a.txt", "1\n2\n3\n");
        seed(&dir, "gone.txt", "bye\n");
        let diff = "--- a/a.txt\n+++ b/a.txt\n@@ -1,3 +1,3 @@\n 1\n-2\n+two\n 3\n--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1 @@\n+fresh\n--- a/gone.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye\n";
        assert_eq!(
            plan_patch(&ws, &ledger, diff)
                .err()
                .map(|error| error.code()),
            Some(ErrorCode::StaleRead)
        );
        read(&ws, &ledger, json!({ "path": "a.txt" })).expect("read");
        read(&ws, &ledger, json!({ "path": "gone.txt" })).expect("read");
        let planned = plan_patch(&ws, &ledger, diff).expect("plan");
        let result = apply_patch(&ws, &ledger, &planned).expect("apply");
        assert_eq!(result["changed"], json!(["a.txt", "new.txt", "gone.txt"]));
        assert_eq!(
            std::fs::read_to_string(dir.path().join("a.txt")).expect("a"),
            "1\ntwo\n3\n"
        );
        assert_eq!(
            std::fs::read_to_string(dir.path().join("new.txt")).expect("new"),
            "fresh\n"
        );
        assert!(!dir.path().join("gone.txt").exists());
        // A conflicting hunk writes nothing.
        let bad = "--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-1\n+one\n--- a/new.txt\n+++ b/new.txt\n@@ -1 +1 @@\n-nope\n+x\n";
        assert_eq!(
            plan_patch(&ws, &ledger, bad)
                .err()
                .map(|error| error.code()),
            Some(ErrorCode::Conflict)
        );
        assert_eq!(
            std::fs::read_to_string(dir.path().join("a.txt")).expect("a"),
            "1\ntwo\n3\n"
        );
        let escape = "--- a/../x\n+++ b/../x\n@@ -1 +1 @@\n-a\n+b\n";
        assert_eq!(
            plan_patch(&ws, &ledger, escape)
                .err()
                .map(|error| error.code()),
            Some(ErrorCode::OutsideWorkspace)
        );
    }

    /// git sections without `---`/`+++` apply: a pure rename keeps the
    /// content and mode, empty files are created and deleted; an
    /// executable new file is created executable.
    #[test]
    fn renames_and_empty_files_apply() {
        use std::os::unix::fs::PermissionsExt;
        let (dir, ws, ledger) = setup(&[]);
        seed(&dir, "old.sh", "#!/bin/sh\r\necho\n");
        std::fs::set_permissions(
            dir.path().join("old.sh"),
            std::fs::Permissions::from_mode(0o755),
        )
        .expect("chmod");
        seed(&dir, "empty.txt", "");
        seed(&dir, "taken.txt", "t\n");
        read(&ws, &ledger, json!({ "path": "old.sh" })).expect("read");
        read(&ws, &ledger, json!({ "path": "empty.txt" })).expect("read");
        let onto_existing = "diff --git a/old.sh b/taken.txt\nsimilarity index 100%\nrename from old.sh\nrename to taken.txt\n";
        assert_eq!(
            plan_patch(&ws, &ledger, onto_existing)
                .err()
                .map(|error| error.code()),
            Some(ErrorCode::Conflict)
        );
        let diff = "diff --git a/old.sh b/bin/new.sh\nsimilarity index 100%\nrename from old.sh\nrename to bin/new.sh\ndiff --git a/blank.txt b/blank.txt\nnew file mode 100644\nindex 0000000..e69de29\ndiff --git a/empty.txt b/empty.txt\ndeleted file mode 100644\nindex e69de29..0000000\ndiff --git a/run.sh b/run.sh\nnew file mode 100755\n--- /dev/null\n+++ b/run.sh\n@@ -0,0 +1 @@\n+#!/bin/sh\n";
        let planned = plan_patch(&ws, &ledger, diff).expect("plan");
        let result = apply_patch(&ws, &ledger, &planned).expect("apply");
        assert_eq!(
            result["changed"],
            json!(["bin/new.sh", "old.sh", "blank.txt", "empty.txt", "run.sh"])
        );
        assert!(!dir.path().join("old.sh").exists());
        assert_eq!(
            std::fs::read_to_string(dir.path().join("bin/new.sh")).expect("renamed"),
            "#!/bin/sh\r\necho\n"
        );
        let mode = |name: &str| {
            std::fs::metadata(dir.path().join(name))
                .expect("meta")
                .permissions()
                .mode()
                & 0o777
        };
        assert_eq!(mode("bin/new.sh"), 0o755);
        assert_eq!(mode("run.sh"), 0o755);
        assert_eq!(
            std::fs::read_to_string(dir.path().join("blank.txt")).expect("blank"),
            ""
        );
        assert!(!dir.path().join("empty.txt").exists());
        // A binary section refuses the whole patch: the text section next
        // to it is not applied either.
        seed(&dir, "t.txt", "a\n");
        read(&ws, &ledger, json!({ "path": "t.txt" })).expect("read");
        let mixed = "--- a/t.txt\n+++ b/t.txt\n@@ -1 +1 @@\n-a\n+b\ndiff --git a/x.png b/x.png\nBinary files a/x.png and b/x.png differ\n";
        assert_eq!(
            plan_patch(&ws, &ledger, mixed)
                .err()
                .map(|error| error.code()),
            Some(ErrorCode::Unsupported)
        );
        assert_eq!(
            std::fs::read_to_string(dir.path().join("t.txt")).expect("t"),
            "a\n"
        );
    }

    /// L2: one path twice in a patch is refused (the second section would
    /// silently overwrite the first), also when a link names it again.
    #[test]
    fn a_path_repeated_in_one_patch_is_refused() {
        let (dir, ws, ledger) = setup(&[]);
        seed(&dir, "a.txt", "1\n2\n3\n");
        std::os::unix::fs::symlink("a.txt", dir.path().join("alias.txt")).expect("link");
        read(&ws, &ledger, json!({ "path": "a.txt" })).expect("read");
        for second in ["a.txt", "alias.txt"] {
            let diff = format!(
                "--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-1\n+one\n--- a/{second}\n+++ b/{second}\n@@ -3 +3 @@\n-3\n+three\n"
            );
            assert_eq!(
                plan_patch(&ws, &ledger, &diff)
                    .err()
                    .map(|error| error.code()),
                Some(ErrorCode::InvalidArgument),
                "{second}"
            );
        }
    }

    /// L2: when one file of a patch cannot be written, the files already
    /// written are put back: the patch applies whole or not at all.
    #[test]
    fn a_failed_write_rolls_the_patch_back() {
        use std::os::unix::fs::PermissionsExt;
        let (dir, ws, ledger) = setup(&[]);
        seed(&dir, "a.txt", "1\n2\n3\n");
        seed(&dir, "gone.txt", "bye\n");
        seed(&dir, "locked/keep.txt", "k\n");
        read(&ws, &ledger, json!({ "path": "a.txt" })).expect("read");
        read(&ws, &ledger, json!({ "path": "gone.txt" })).expect("read");
        let diff = "--- a/a.txt\n+++ b/a.txt\n@@ -2 +2 @@\n-2\n+two\n--- a/gone.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye\n--- /dev/null\n+++ b/fresh.txt\n@@ -0,0 +1 @@\n+fresh\n--- /dev/null\n+++ b/locked/new.txt\n@@ -0,0 +1 @@\n+x\n";
        let planned = plan_patch(&ws, &ledger, diff).expect("plan");
        let locked = dir.path().join("locked");
        std::fs::set_permissions(&locked, std::fs::Permissions::from_mode(0o555)).expect("lock");
        let result = apply_patch(&ws, &ledger, &planned);
        std::fs::set_permissions(&locked, std::fs::Permissions::from_mode(0o755)).expect("unlock");
        assert!(result.is_err(), "{result:?}");
        assert_eq!(
            std::fs::read_to_string(dir.path().join("a.txt")).expect("a"),
            "1\n2\n3\n",
            "the written file is restored"
        );
        assert_eq!(
            std::fs::read_to_string(dir.path().join("gone.txt")).expect("gone"),
            "bye\n",
            "the deleted file is back"
        );
        assert!(
            !dir.path().join("fresh.txt").exists(),
            "the created file is removed"
        );
        assert!(!locked.join("new.txt").exists());
        // The ledger still lets the agent retry after reading again.
        read(&ws, &ledger, json!({ "path": "a.txt" })).expect("read");
    }

    #[test]
    fn search_honours_gitignore_globs_and_deny_rules() {
        let (dir, ws, _ledger) = setup(&["secret/**"]);
        seed(&dir, ".gitignore", "target/\n");
        seed(&dir, "src/lib.rs", "fn needle() {}\n");
        seed(&dir, "src/other.ts", "const needle = 1;\n");
        seed(&dir, "target/out.rs", "needle\n");
        seed(&dir, "secret/key.rs", "needle\n");
        seed(&dir, ".git/config", "needle\n");
        let found = search(&ws, json!({ "pattern": "needle" })).expect("search");
        let paths: Vec<&str> = found["matches"]
            .as_array()
            .expect("array")
            .iter()
            .map(|m| m["path"].as_str().expect("path"))
            .collect();
        assert_eq!(paths, ["src/lib.rs", "src/other.ts"]);
        let rust_only = search(
            &ws,
            json!({ "pattern": "NEEDLE", "case_insensitive": true, "glob": "*.rs" }),
        )
        .expect("search");
        assert_eq!(rust_only["matches"].as_array().expect("array").len(), 1);
        assert_eq!(rust_only["matches"][0]["line"], 1);
        let literal =
            search(&ws, json!({ "pattern": "needle()", "fixed_strings": true })).expect("search");
        assert_eq!(literal["matches"].as_array().expect("array").len(), 1);
        assert_eq!(
            search(&ws, json!({ "pattern": "(" }))
                .expect_err("bad")
                .code(),
            ErrorCode::InvalidArgument
        );
    }

    fn found(result: &serde_json::Value) -> Vec<String> {
        result["files"]
            .as_array()
            .expect("files")
            .iter()
            .map(|file| {
                file.as_str()
                    .or_else(|| file["path"].as_str())
                    .expect("path")
                    .to_owned()
            })
            .collect()
    }

    fn touch(dir: &tempfile::TempDir, name: &str, seconds_ago: u64) {
        let time = std::time::SystemTime::now() - std::time::Duration::from_secs(seconds_ago);
        std::fs::File::options()
            .write(true)
            .open(dir.path().join(name))
            .expect("open")
            .set_modified(time)
            .expect("mtime");
    }

    #[test]
    fn find_files_glob_newest_first() {
        let (dir, ws, _ledger) = setup(&[]);
        seed(&dir, "src/old.rs", "");
        seed(&dir, "src/new.rs", "");
        seed(&dir, "src/deep/mid.rs", "");
        seed(&dir, "src/notes.md", "");
        seed(&dir, "other.rs", "");
        touch(&dir, "src/old.rs", 3000);
        touch(&dir, "src/deep/mid.rs", 2000);
        touch(&dir, "src/new.rs", 1000);
        let result = find_files(&ws, json!({ "pattern": "src/**/*.rs" })).expect("find");
        assert_eq!(
            found(&result),
            ["src/new.rs", "src/deep/mid.rs", "src/old.rs"]
        );
        assert_eq!(result["truncated"], false);
        // A name glob matches at any depth; max_results caps.
        let capped = find_files(&ws, json!({ "pattern": "*.rs", "max_results": 2 })).expect("find");
        assert_eq!(found(&capped).len(), 2);
        assert_eq!(capped["matched"], 4);
        assert_eq!(capped["truncated"], true);
        // Under a directory only.
        let sub = find_files(&ws, json!({ "pattern": "*.rs", "path": "src/deep" })).expect("find");
        assert_eq!(found(&sub), ["src/deep/mid.rs"]);
        assert_eq!(
            find_files(&ws, json!({ "pattern": "[" }))
                .expect_err("bad glob")
                .code(),
            ErrorCode::InvalidArgument
        );
    }

    #[test]
    fn find_files_fuzzy_ranks_best_first() {
        let (dir, ws, _ledger) = setup(&[]);
        for name in [
            "app/user_controller.rs",
            "app/user_service.rs",
            "app/controller.rs",
            "app/session_ctrl.rs",
            "README.md",
        ] {
            seed(&dir, name, "");
        }
        let result = find_files(&ws, json!({ "pattern": "usrctrl", "fuzzy": true })).expect("find");
        let files = found(&result);
        assert_eq!(
            files.first().map(String::as_str),
            Some("app/user_controller.rs")
        );
        assert!(!files.contains(&"README.md".to_owned()), "{files:?}");
        assert!(
            result["files"][0]["score"]
                .as_u64()
                .is_some_and(|score| score > 0)
        );
    }

    #[test]
    fn find_files_prunes_denied_and_git() {
        let (dir, ws, _ledger) = setup(&["secrets/**", "*.pem"]);
        seed(&dir, ".gitignore", "target/\n");
        seed(&dir, ".git/config.rs", "");
        seed(&dir, "target/gen.rs", "");
        seed(&dir, "secrets/key.rs", "");
        seed(&dir, "src/key.pem", "");
        seed(&dir, "src/lib.rs", "");
        for args in [
            json!({ "pattern": "**/*" }),
            json!({ "pattern": "key", "fuzzy": true }),
            json!({ "pattern": "rs", "fuzzy": true }),
        ] {
            let files = found(&find_files(&ws, args).expect("find"));
            assert!(
                files
                    .iter()
                    .all(|path| path == "src/lib.rs" || path == ".gitignore"),
                "{files:?}"
            );
        }
    }

    #[test]
    fn find_files_does_not_follow_symlinks() {
        let outside = tempfile::tempdir().expect("outside");
        seed(&outside, "passwords.rs", "");
        let (dir, ws, _ledger) = setup(&[]);
        seed(&dir, "src/lib.rs", "");
        std::os::unix::fs::symlink(outside.path(), dir.path().join("escape")).expect("link");
        std::os::unix::fs::symlink(
            outside.path().join("passwords.rs"),
            dir.path().join("src/link.rs"),
        )
        .expect("link");
        let glob = found(&find_files(&ws, json!({ "pattern": "**/*.rs" })).expect("find"));
        assert_eq!(glob, ["src/lib.rs"]);
        let fuzzy =
            found(&find_files(&ws, json!({ "pattern": "pass", "fuzzy": true })).expect("find"));
        assert!(fuzzy.is_empty(), "{fuzzy:?}");
    }

    /// Security regression: a workspace bound at a home directory holds
    /// `~/.ssh` and the app's own credentials; the session's deny list
    /// keeps both out of every search and listing.
    #[test]
    fn search_never_reads_deny_list_paths() {
        let dir = tempfile::tempdir().expect("dir");
        let home = std::fs::canonicalize(dir.path()).expect("canonical");
        let plant = |path: &str, content: &str| {
            let path = home.join(path);
            std::fs::create_dir_all(path.parent().expect("parent")).expect("dirs");
            std::fs::write(path, content).expect("plant");
        };
        plant(".ssh/id_rsa", "-----BEGIN OPENSSH PRIVATE KEY-----\n");
        plant(".ssh/id_rsa.pub", "ssh-rsa AAAA PRIVATE KEY comment\n");
        plant(
            "Library/Application Support/ai.elitea.desktop/credentials.json",
            "{\"refresh\": \"PRIVATE KEY\"}\n",
        );
        plant("notes/id_card.txt", "nothing secret\n");
        let list = crate::deny::DenyList::for_session(
            Some(&home),
            Some("ai.elitea.desktop"),
            true,
            Vec::new(),
        );
        let ws = Workspace::open(&home, &[])
            .expect("workspace")
            .with_deny_list(std::sync::Arc::new(list));
        let searched = search(
            &ws,
            json!({ "pattern": "PRIVATE KEY", "fixed_strings": true }),
        )
        .expect("search");
        assert_eq!(searched["matches"], json!([]), "{searched}");
        let ids = found(&find_files(&ws, json!({ "pattern": "**/id_*" })).expect("find"));
        assert_eq!(ids, ["notes/id_card.txt"]);
        let creds = found(
            &find_files(&ws, json!({ "pattern": "credentials", "fuzzy": true })).expect("find"),
        );
        assert!(creds.is_empty(), "{creds:?}");
    }

    fn lines(result: &serde_json::Value) -> Vec<(String, u64, String)> {
        result["matches"]
            .as_array()
            .expect("matches")
            .iter()
            .map(|hit| {
                (
                    hit["kind"].as_str().expect("kind").to_owned(),
                    hit["line"].as_u64().expect("line"),
                    hit["text"].as_str().expect("text").to_owned(),
                )
            })
            .collect()
    }

    #[test]
    fn search_context_lines() {
        let (dir, ws, _ledger) = setup(&[]);
        seed(&dir, "a.txt", "1\n2\n3\nneedle\n5\n6\n7\n8\nneedle\n10\n");
        let found = search(&ws, json!({ "pattern": "needle", "context": 2 })).expect("search");
        let got: Vec<(String, u64)> = lines(&found)
            .into_iter()
            .map(|(kind, line, _)| (kind, line))
            .collect();
        let want: Vec<(String, u64)> = [
            ("context", 2),
            ("context", 3),
            ("match", 4),
            ("context", 5),
            ("context", 6),
            ("context", 7),
            ("context", 8),
            ("match", 9),
            ("context", 10),
        ]
        .iter()
        .map(|(kind, line)| ((*kind).to_owned(), *line))
        .collect();
        // Adjacent groups merge: line 6 to 8 appear once.
        assert_eq!(got, want);
        // -B / -A win over -C.
        let after = search(
            &ws,
            json!({ "pattern": "needle", "context": 3, "context_before": 0, "context_after": 1 }),
        )
        .expect("search");
        let got: Vec<u64> = lines(&after).iter().map(|(_, line, _)| *line).collect();
        assert_eq!(got, [4, 5, 9, 10]);
        // Old calls keep their shape: path, line, text.
        let plain = search(&ws, json!({ "pattern": "needle" })).expect("search");
        assert_eq!(plain["matches"][0]["path"], "a.txt");
        assert_eq!(plain["matches"][0]["text"], "needle");
        assert_eq!(plain["matches"].as_array().expect("array").len(), 2);
    }

    #[test]
    fn search_files_and_count_modes() {
        let (dir, ws, _ledger) = setup(&[]);
        seed(&dir, "b.rs", "x\nx\nx\n");
        seed(&dir, "a/one.rs", "x\n");
        seed(&dir, "c.rs", "none\n");
        let files = search(&ws, json!({ "pattern": "x", "output_mode": "files" })).expect("files");
        assert_eq!(files["files"], json!(["a/one.rs", "b.rs"]));
        assert_eq!(files["files_searched"], 3);
        let counts = search(&ws, json!({ "pattern": "x", "output_mode": "count" })).expect("count");
        assert_eq!(
            counts["counts"],
            json!([{ "path": "a/one.rs", "count": 1 }, { "path": "b.rs", "count": 3 }])
        );
        assert_eq!(counts["total"], 4);
        // max_per_file caps every mode.
        let capped = search(
            &ws,
            json!({ "pattern": "x", "output_mode": "count", "max_per_file": 2 }),
        )
        .expect("count");
        assert_eq!(capped["total"], 3);
        let content = search(&ws, json!({ "pattern": "x", "max_per_file": 1 })).expect("content");
        assert_eq!(content["matches"].as_array().expect("array").len(), 2);
        // max_results counts files in files mode.
        let one = search(
            &ws,
            json!({ "pattern": "x", "output_mode": "files", "max_results": 1 }),
        )
        .expect("files");
        assert_eq!(one["files"].as_array().expect("array").len(), 1);
        assert_eq!(one["truncated"], true);
        assert_eq!(
            search(&ws, json!({ "pattern": "x", "output_mode": "lines" }))
                .expect_err("bad mode")
                .code(),
            ErrorCode::InvalidArgument
        );
    }

    #[test]
    fn search_multiline() {
        let (dir, ws, _ledger) = setup(&[]);
        seed(&dir, "a.rs", "fn main() {\n    run();\n}\nfn other() {}\n");
        let single = search(&ws, json!({ "pattern": r"\{\n\s+run" })).expect("search");
        assert_eq!(single["matches"], json!([]));
        let multi =
            search(&ws, json!({ "pattern": r"\{\n\s+run", "multiline": true })).expect("search");
        assert_eq!(
            lines(&multi),
            [
                ("match".to_owned(), 1, "fn main() {".to_owned()),
                ("match".to_owned(), 2, "    run();".to_owned()),
            ]
        );
        let counted = search(
            &ws,
            json!({ "pattern": r"\{\n\s+run", "multiline": true, "output_mode": "count" }),
        )
        .expect("search");
        assert_eq!(counted["total"], 1);
    }

    #[test]
    fn search_parallel_matches_serial() {
        let (dir, ws, _ledger) = setup(&["deny/**"]);
        seed(&dir, ".gitignore", "ignored/\n");
        for d in ["a", "a/b", "a/b/c", "a.d", "z", "ignored", "deny"] {
            for f in 0..12 {
                let body: String = (0..30)
                    .map(|line| {
                        if (line + f) % 7 == 0 {
                            format!("hit {d} {f} {line}\n")
                        } else {
                            format!("plain {line}\n")
                        }
                    })
                    .collect();
                seed(&dir, &format!("{d}/f{f:02}.txt"), &body);
            }
        }
        seed(&dir, "a.txt", "hit top\n");
        for args in [
            json!({ "pattern": "hit", "context": 1, "max_results": 1000 }),
            json!({ "pattern": "hit", "output_mode": "files" }),
            json!({ "pattern": "hit", "output_mode": "count", "max_per_file": 3 }),
            json!({ "pattern": "hit", "glob": "a/**", "max_results": 1000 }),
        ] {
            let parsed: super::SearchArgs = serde_json::from_value(args.clone()).expect("args");
            let max = parsed.max_results.unwrap_or(200);
            let plan = super::SearchPlan::new(&parsed, max).expect("plan");
            let base = crate::workspace::WsPath::root();
            let glob = parsed.glob.as_deref();
            let serial = super::search_answer(
                super::collect_serial(&ws, &base, glob, &plan).expect("serial"),
                plan.mode,
                max,
            );
            let serial_order: Vec<String> = serial
                .get("matches")
                .or_else(|| serial.get("files"))
                .or_else(|| serial.get("counts"))
                .and_then(serde_json::Value::as_array)
                .expect("list")
                .iter()
                .map(|v| v.get("path").unwrap_or(v).to_string())
                .collect();
            // The serial walker's own order is the sorted order.
            assert!(!serial_order.is_empty(), "{args}");
            for _ in 0..5 {
                assert_eq!(
                    search(&ws, args.clone()).expect("parallel"),
                    serial,
                    "{args}"
                );
            }
            assert!(!serial.to_string().contains("ignored/"), "{serial}");
            assert!(!serial.to_string().contains("deny/"), "{serial}");
        }
    }

    #[test]
    fn tree_lists_with_depth_and_skips_ignored_and_denied() {
        let (dir, ws, _ledger) = setup(&["*.pem"]);
        seed(&dir, ".gitignore", "build/\n");
        seed(&dir, "a/b/c/deep.txt", "x");
        seed(&dir, "a/top.txt", "xyz");
        seed(&dir, "build/out", "x");
        seed(&dir, "key.pem", "x");
        let listed = tree(&ws, json!({ "depth": 2 })).expect("tree");
        let paths: Vec<&str> = listed["entries"]
            .as_array()
            .expect("array")
            .iter()
            .map(|e| e["path"].as_str().expect("path"))
            .collect();
        assert_eq!(paths, [".gitignore", "a/", "a/b/", "a/top.txt"]);
        let sub = tree(&ws, json!({ "path": "a/b", "depth": 5 })).expect("tree");
        assert_eq!(sub["entries"].as_array().expect("array").len(), 2);
    }

    #[test]
    fn documents_extract_text_files_and_report_unsupported_formats() {
        let (dir, ws, _ledger) = setup(&[]);
        seed(&dir, "notes.md", "# Title\n");
        let text = document(&ws, json!({ "path": "notes.md" })).expect("document");
        assert_eq!(text["text"], "# Title\n");
        std::fs::write(dir.path().join("x.pdf"), b"%PDF-not-really").expect("pdf");
        let pdf = document(&ws, json!({ "path": "x.pdf" }));
        if cfg!(feature = "documents") {
            if let Err(error) = pdf {
                assert_eq!(error.code(), ErrorCode::Binary);
            }
        } else {
            assert_eq!(pdf.expect_err("unsupported").code(), ErrorCode::Unsupported);
        }
    }
}
