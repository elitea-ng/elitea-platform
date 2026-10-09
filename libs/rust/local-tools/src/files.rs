//! The file tools' bodies: read, write, edit, patch, search, tree and
//! document text. Each takes the session's [`Workspace`] and
//! [`ReadLedger`]; approval and checkpoints happen around them in
//! [`crate::session`].

use std::path::Path;

use grep_regex::RegexMatcherBuilder;
use grep_searcher::sinks::UTF8;
use grep_searcher::{BinaryDetection, SearcherBuilder};
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
    let text = text_of(&file)?;
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
    pub path: WsPath,
    /// `None`: delete.
    pub content: Option<String>,
}

/// Parse a patch, resolve its paths and compute every result, before
/// anything is written; also the paths the approval names.
pub(crate) fn plan_patch(
    workspace: &Workspace,
    ledger: &ReadLedger,
    text: &str,
) -> ToolResult<Vec<PlannedChange>> {
    let mut planned = Vec::new();
    for file in patch::parse(text)? {
        let path = workspace.resolve(file.target(), Intent::Write)?;
        if let (Some(old), Some(new)) = (&file.old_path, &file.new_path)
            && old != new
        {
            return Err(ToolError::new(
                ErrorCode::Unsupported,
                "patches that rename files are not supported",
            ));
        }
        let now = check_fresh(workspace, ledger, &path)?;
        let original = match (&now, file.is_create()) {
            (Some(_), true) => {
                return Err(ToolError::new(
                    ErrorCode::Conflict,
                    format!("`{path}` already exists"),
                ));
            }
            (None, false) => {
                return Err(ToolError::new(
                    ErrorCode::NotFound,
                    format!("`{path}` does not exist"),
                ));
            }
            (Some(read), false) => text_of(read)?.to_owned(),
            (None, true) => String::new(),
        };
        let target = now
            .as_ref()
            .map_or_else(|| path.clone(), |read| read.path.clone());
        planned.push(PlannedChange {
            path: target,
            content: patch::apply(&original, &file)?,
        });
    }
    Ok(planned)
}

/// `apply_patch`, after approval.
pub(crate) fn apply_patch(
    workspace: &Workspace,
    ledger: &ReadLedger,
    planned: Vec<PlannedChange>,
) -> ToolResult<Value> {
    // Re-check: the person may have edited a file while the approval was
    // pending.
    for change in &planned {
        check_fresh(workspace, ledger, &change.path)?;
    }
    let mut changed = Vec::new();
    for change in planned {
        if let Some(content) = change.content {
            write_and_record(workspace, ledger, &change.path, content.as_bytes())?;
        } else {
            workspace.remove_file(&change.path)?;
            ledger.forget(&change.path);
        }
        changed.push(change.path.display_string());
    }
    Ok(json!({ "changed": changed }))
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
}

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

fn walker(
    workspace: &Workspace,
    base: &WsPath,
    glob: Option<&str>,
    depth: Option<usize>,
) -> ToolResult<ignore::Walk> {
    let mut builder = ignore::WalkBuilder::new(workspace.absolute(base));
    builder
        .hidden(false)
        .require_git(false)
        .follow_links(false)
        .max_depth(depth)
        .sort_by_file_name(Ord::cmp)
        .filter_entry(|entry| !entry.file_name().to_str().is_some_and(is_protected_name));
    if let Some(glob) = glob {
        let mut overrides = ignore::overrides::OverrideBuilder::new(workspace.root());
        overrides
            .add(glob)
            .map_err(|_| ToolError::invalid(format!("`{glob}` is not a glob")))?;
        builder.overrides(
            overrides
                .build()
                .map_err(|_| ToolError::invalid(format!("`{glob}` is not a glob")))?,
        );
    }
    Ok(builder.build())
}

fn relative(workspace: &Workspace, path: &Path) -> Option<WsPath> {
    WsPath::from_relative(path.strip_prefix(workspace.root()).ok()?).ok()
}

/// `search_files`: a regex (or literal) over the workspace's text files,
/// `.gitignore` honoured, denied paths skipped.
pub(crate) fn search(workspace: &Workspace, args: Value) -> ToolResult<Value> {
    let args: SearchArgs = parse(args)?;
    let max = args.max_results.unwrap_or(200).clamp(1, 1000);
    let matcher = RegexMatcherBuilder::new()
        .case_insensitive(args.case_insensitive)
        .fixed_strings(args.fixed_strings)
        .build(&args.pattern)
        .map_err(|error| ToolError::invalid(format!("bad pattern: {error}")))?;
    let mut searcher = SearcherBuilder::new()
        .line_number(true)
        .binary_detection(BinaryDetection::quit(0))
        .build();
    let base = base_dir(workspace, args.path.as_deref())?;
    let mut found = Vec::new();
    let mut files = 0usize;
    for entry in walker(workspace, &base, args.glob.as_deref(), None)?.flatten() {
        if found.len() >= max {
            break;
        }
        if !entry.file_type().is_some_and(|kind| kind.is_file()) {
            continue;
        }
        let Some(path) = relative(workspace, entry.path()) else {
            continue;
        };
        if workspace.denied_by(&path, Intent::Read).is_some() {
            continue;
        }
        let Ok((file, _)) = workspace.open_file(&path) else {
            continue;
        };
        files += 1;
        let shown = path.display_string();
        let _ = searcher.search_file(
            &matcher,
            &file,
            UTF8(|line_number, line| {
                let line = line.trim_end_matches(['\n', '\r']);
                let line: String = line.chars().take(MAX_MATCH_LINE).collect();
                found.push(json!({ "path": shown, "line": line_number, "text": line }));
                Ok(found.len() < max)
            }),
        );
    }
    Ok(json!({
        "matches": found,
        "files_searched": files,
        "truncated": found.len() >= max,
    }))
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
        EditArgs, WriteArgs, apply_patch, document, edit, plan_patch, read, search, tree, write,
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
        let result = apply_patch(&ws, &ledger, planned).expect("apply");
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
