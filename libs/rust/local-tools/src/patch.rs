//! Unified diffs: parse one (`diff -u`, `git diff`) and apply it exactly.
//!
//! A hunk applies where its context and removed lines match the file
//! exactly (line endings aside), searching outward from the line numbers
//! the header states, so a hunk whose numbers drifted still lands. There is
//! no fuzz: a hunk whose context does not match anywhere fails the whole
//! patch, and nothing is written.
//!
//! Every line keeps its own line ending: context lines stay as they are in
//! the file, and an added line takes the ending of the original line next
//! to it (the file's usual ending when the hunk has none).
//!
//! git's extended headers are read too: a section with `rename from` /
//! `rename to` renames (with or without hunks), and `new file mode` /
//! `deleted file mode` without hunks create or delete an empty file. A
//! binary section, a copy or a mode change cannot be applied, so the whole
//! patch is refused ([`ErrorCode::Unsupported`]) before anything is
//! written; it is never applied with that section left out.

use crate::error::{ErrorCode, ToolError, ToolResult};

/// One line of a hunk.
#[derive(Clone, Debug, Eq, PartialEq)]
enum Line {
    Context(String),
    Remove(String),
    Add(String),
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct Hunk {
    old_start: usize,
    old_len: usize,
    lines: Vec<Line>,
    /// `\ No newline at end of file` after the last old-side line.
    old_no_newline: bool,
    /// The same after the last new-side line.
    new_no_newline: bool,
}

/// The change to one file.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct FilePatch {
    /// `None` for a file the patch creates.
    pub old_path: Option<String>,
    /// `None` for a file the patch deletes.
    pub new_path: Option<String>,
    /// git's `rename from` / `rename to`: `old_path` becomes `new_path`.
    rename: bool,
    /// Permission bits of a created file (git's `new file mode 100755`);
    /// `None`: the default.
    created_mode: Option<u32>,
    hunks: Vec<Hunk>,
}

impl FilePatch {
    /// The path this patch changes (the new one, or the old for a delete).
    #[must_use]
    pub fn target(&self) -> &str {
        self.new_path
            .as_deref()
            .or(self.old_path.as_deref())
            .unwrap_or_default()
    }

    /// Whether the patch renames `old_path` to `new_path` (git's `rename
    /// from` / `rename to`), applying its hunks, if any, on the way.
    #[must_use]
    pub fn is_rename(&self) -> bool {
        self.rename
    }

    /// The permission bits a created file gets, when the patch names them
    /// (`new file mode 100755`).
    #[must_use]
    pub fn created_mode(&self) -> Option<u32> {
        self.created_mode
    }

    #[must_use]
    pub fn is_create(&self) -> bool {
        self.old_path.is_none()
    }

    #[must_use]
    pub fn is_delete(&self) -> bool {
        self.new_path.is_none()
    }
}

fn malformed(message: impl Into<String>) -> ToolError {
    ToolError::invalid(format!("malformed patch: {}", message.into()))
}

fn unsupported(message: impl Into<String>) -> ToolError {
    ToolError::new(
        ErrorCode::Unsupported,
        format!("{}; nothing was applied", message.into()),
    )
}

fn header_path(rest: &str, prefix: &str) -> Option<String> {
    // `--- a/x\t2024-01-01 …`: the timestamp follows a tab.
    let path = rest.split('\t').next().unwrap_or_default().trim_end();
    if path == "/dev/null" {
        return None;
    }
    let path = path.strip_prefix(prefix).unwrap_or(path);
    Some(path.to_owned())
}

fn parse_range(text: &str) -> Option<(usize, usize)> {
    let (start, len) = match text.split_once(',') {
        Some((start, len)) => (start, len),
        None => (text, "1"),
    };
    Some((start.parse().ok()?, len.parse().ok()?))
}

fn parse_hunk_header(line: &str) -> Option<(usize, usize, usize)> {
    let body = line.strip_prefix("@@ -")?;
    let (ranges, _) = body.split_once(" @@")?;
    let (old, new) = ranges.split_once(" +")?;
    let (old_start, old_len) = parse_range(old)?;
    let (_, new_len) = parse_range(new)?;
    Some((old_start, old_len, new_len))
}

/// Record a `\ No newline at end of file` marker that follows `previous`.
fn mark_no_newline(hunk: &mut Hunk, previous: Option<&str>) {
    match previous.and_then(|line| line.chars().next()) {
        Some('-') => hunk.old_no_newline = true,
        Some('+') => hunk.new_no_newline = true,
        _ => {
            hunk.old_no_newline = true;
            hunk.new_no_newline = true;
        }
    }
}

/// What a `diff --git` section's extended header lines say.
#[derive(Default)]
struct GitHeader {
    /// The path both sides name, when they name the same one.
    path: Option<String>,
    rename_from: Option<String>,
    rename_to: Option<String>,
    /// `new file mode`'s value.
    new_file: Option<String>,
    /// `deleted file mode`'s value.
    deleted_file: Option<String>,
    old_mode: Option<String>,
    new_mode: Option<String>,
    copy: bool,
    binary: bool,
}

impl GitHeader {
    /// `diff --git a/<p> b/<p>`: the path, when both sides are the same
    /// (a rename names its paths in `rename from` / `rename to`).
    fn parse(rest: &str) -> Self {
        let path = rest
            .len()
            .checked_sub(1)
            .map(|both| both / 2)
            .filter(|half| rest.is_char_boundary(*half) && rest.get(*half..=*half) == Some(" "))
            .and_then(|half| {
                let old = rest[..half].strip_prefix("a/")?;
                let new = rest[half + 1..].strip_prefix("b/")?;
                (old == new).then(|| old.to_owned())
            });
        Self {
            path,
            ..Self::default()
        }
    }

    fn read(&mut self, line: &str) {
        if let Some(path) = line.strip_prefix("rename from ") {
            self.rename_from = Some(path.to_owned());
        } else if let Some(path) = line.strip_prefix("rename to ") {
            self.rename_to = Some(path.to_owned());
        } else if let Some(mode) = line.strip_prefix("new file mode ") {
            self.new_file = Some(mode.trim().to_owned());
        } else if let Some(mode) = line.strip_prefix("deleted file mode ") {
            self.deleted_file = Some(mode.trim().to_owned());
        } else if let Some(mode) = line.strip_prefix("old mode ") {
            self.old_mode = Some(mode.trim().to_owned());
        } else if let Some(mode) = line.strip_prefix("new mode ") {
            self.new_mode = Some(mode.trim().to_owned());
        } else if line.starts_with("copy from ") || line.starts_with("copy to ") {
            self.copy = true;
        } else if line.starts_with("Binary files ") || line.starts_with("GIT binary patch") {
            self.binary = true;
        }
    }

    fn name(&self) -> &str {
        self.path
            .as_deref()
            .or(self.rename_to.as_deref())
            .unwrap_or("a file")
    }

    /// Refuse what cannot be applied.
    fn check(&self) -> ToolResult<()> {
        if self.binary {
            return Err(unsupported(format!(
                "`{}` is a binary change, which apply_patch cannot apply",
                self.name()
            )));
        }
        if self.copy {
            return Err(unsupported(format!(
                "`{}` is a copy, which apply_patch cannot apply",
                self.name()
            )));
        }
        if self.old_mode.is_some() || self.new_mode.is_some() {
            return Err(unsupported(format!(
                "`{}` changes the file mode, which apply_patch cannot apply",
                self.name()
            )));
        }
        for mode in [&self.new_file, &self.deleted_file].into_iter().flatten() {
            if !matches!(mode.as_str(), "100644" | "100755") {
                return Err(unsupported(format!(
                    "`{}` is not a regular file (mode {mode}), which apply_patch cannot apply",
                    self.name()
                )));
            }
        }
        if self.rename_from.is_some() != self.rename_to.is_some() {
            return Err(malformed(format!(
                "`{}` has only one of `rename from` / `rename to`",
                self.name()
            )));
        }
        Ok(())
    }

    /// The permission bits `new file mode` names, when not the default.
    fn created_mode(&self) -> Option<u32> {
        (self.new_file.as_deref() == Some("100755")).then_some(0o755)
    }

    /// The change a section without `---`/`+++` makes.
    fn header_only(&self) -> ToolResult<FilePatch> {
        if let (Some(from), Some(to)) = (&self.rename_from, &self.rename_to) {
            return Ok(FilePatch {
                old_path: Some(from.clone()),
                new_path: Some(to.clone()),
                rename: true,
                created_mode: None,
                hunks: Vec::new(),
            });
        }
        let path = self
            .path
            .clone()
            .ok_or_else(|| malformed("a `diff --git` line without a path"))?;
        let (old_path, new_path) = match (&self.new_file, &self.deleted_file) {
            (Some(_), None) => (None, Some(path)),
            (None, Some(_)) => (Some(path), None),
            _ => return Err(malformed(format!("no change for `{path}`"))),
        };
        Ok(FilePatch {
            old_path,
            new_path,
            rename: false,
            created_mode: self.created_mode(),
            hunks: Vec::new(),
        })
    }
}

/// Parse a `---`/`+++` pair and its hunks starting at `lines[*index]`.
fn parse_section(lines: &[&str], index: &mut usize) -> ToolResult<FilePatch> {
    let old = lines[*index].strip_prefix("--- ").unwrap_or_default();
    let new = lines
        .get(*index + 1)
        .and_then(|line| line.strip_prefix("+++ "))
        .ok_or_else(|| malformed("`---` without `+++`"))?;
    let git_style = old.starts_with("a/") || new.starts_with("b/");
    let (old_prefix, new_prefix) = if git_style { ("a/", "b/") } else { ("", "") };
    let mut patch = FilePatch {
        old_path: header_path(old, old_prefix),
        new_path: header_path(new, new_prefix),
        rename: false,
        created_mode: None,
        hunks: Vec::new(),
    };
    if patch.old_path.is_none() && patch.new_path.is_none() {
        return Err(malformed("both sides are /dev/null"));
    }
    *index += 2;
    while let Some(header) = lines.get(*index).filter(|line| line.starts_with("@@")) {
        let (old_start, old_len, new_len) = parse_hunk_header(header)
            .ok_or_else(|| malformed(format!("bad hunk header `{header}`")))?;
        *index += 1;
        let mut hunk = Hunk {
            old_start,
            old_len,
            lines: Vec::new(),
            old_no_newline: false,
            new_no_newline: false,
        };
        let (mut old_seen, mut new_seen) = (0usize, 0usize);
        while old_seen < old_len || new_seen < new_len {
            let line = lines
                .get(*index)
                .ok_or_else(|| malformed("a hunk ends before its stated length"))?;
            *index += 1;
            let parsed = match line.chars().next() {
                Some(' ') => Line::Context(line[1..].to_owned()),
                // Some tools strip the space of an empty context line.
                None => Line::Context(String::new()),
                Some('-') => Line::Remove(line[1..].to_owned()),
                Some('+') => Line::Add(line[1..].to_owned()),
                Some('\\') => {
                    let previous = index.checked_sub(2).and_then(|at| lines.get(at));
                    mark_no_newline(&mut hunk, previous.copied());
                    continue;
                }
                Some(_) => return Err(malformed(format!("unexpected hunk line `{line}`"))),
            };
            match parsed {
                Line::Context(_) => {
                    old_seen += 1;
                    new_seen += 1;
                }
                Line::Remove(_) => old_seen += 1,
                Line::Add(_) => new_seen += 1,
            }
            hunk.lines.push(parsed);
        }
        if old_seen != old_len || new_seen != new_len {
            return Err(malformed("a hunk's lines do not match its header"));
        }
        // Trailing `\ No newline at end of file` markers name the line
        // before them.
        while lines.get(*index).is_some_and(|line| line.starts_with('\\')) {
            mark_no_newline(&mut hunk, lines.get(*index - 1).copied());
            *index += 1;
        }
        patch.hunks.push(hunk);
    }
    if patch.hunks.is_empty() && !(patch.is_create() || patch.is_delete()) {
        return Err(malformed(format!("no hunks for `{}`", patch.target())));
    }
    Ok(patch)
}

/// Parse a unified diff of one or more files.
///
/// # Errors
///
/// [`ErrorCode::InvalidArgument`] when it is not a well-formed unified diff;
/// [`ErrorCode::Unsupported`] when a section is binary, a copy or a mode
/// change (the whole patch is refused, not that section skipped).
pub fn parse(text: &str) -> ToolResult<Vec<FilePatch>> {
    let lines: Vec<&str> = text.lines().collect();
    let mut patches = Vec::new();
    let mut index = 0;
    while index < lines.len() {
        if let Some(rest) = lines[index].strip_prefix("diff --git ") {
            index += 1;
            let mut header = GitHeader::parse(rest);
            while let Some(line) = lines.get(index).filter(|line| {
                !line.starts_with("diff --git ")
                    && !line.starts_with("--- ")
                    && !line.starts_with("@@")
            }) {
                header.read(line);
                index += 1;
            }
            header.check()?;
            match lines.get(index) {
                Some(line) if line.starts_with("--- ") => {
                    let mut patch = parse_section(&lines, &mut index)?;
                    if let (Some(from), Some(to)) = (&header.rename_from, &header.rename_to) {
                        patch.old_path = Some(from.clone());
                        patch.new_path = Some(to.clone());
                        patch.rename = true;
                    }
                    if patch.is_create() {
                        patch.created_mode = header.created_mode();
                    }
                    patches.push(patch);
                }
                Some(line) if line.starts_with("@@") => {
                    return Err(malformed(format!(
                        "hunks for `{}` without `---`/`+++` lines",
                        header.name()
                    )));
                }
                _ => patches.push(header.header_only()?),
            }
            continue;
        }
        if lines[index].starts_with("--- ") {
            patches.push(parse_section(&lines, &mut index)?);
        } else {
            index += 1;
        }
    }
    if patches.is_empty() {
        return Err(malformed("no `---`/`+++` file headers"));
    }
    Ok(patches)
}

fn signed(value: usize) -> isize {
    isize::try_from(value).unwrap_or(isize::MAX)
}

/// A line's ending: `\r\n`, `\n`, or empty (the last line of a file
/// without a final newline).
fn ending_of(line: &str) -> &str {
    if line.ends_with("\r\n") {
        "\r\n"
    } else if line.ends_with('\n') {
        "\n"
    } else {
        ""
    }
}

fn strip_ending(line: &str) -> &str {
    &line[..line.len() - ending_of(line).len()]
}

/// The ending most of `lines` use (`\n` on a tie or with none).
fn usual_ending(lines: &[String]) -> &'static str {
    let crlf = lines.iter().filter(|line| line.ends_with("\r\n")).count();
    let lf = lines.iter().filter(|line| ending_of(line) == "\n").count();
    if crlf > lf { "\r\n" } else { "\n" }
}

/// The new lines for a hunk matched at `matched` (the original lines its
/// context and removed lines matched): context lines as they were, added
/// lines with the ending of the nearest original line in the hunk (before
/// it, else after it, else `usual`). Every line ends with a newline; the
/// caller trims the last one when the new side has none.
fn replacement(hunk: &Hunk, matched: &[String], usual: &str) -> Vec<String> {
    let mut originals = matched.iter();
    // The ending of each hunk line's original (`None` for added lines).
    let endings: Vec<Option<&str>> = hunk
        .lines
        .iter()
        .map(|line| match line {
            Line::Context(_) | Line::Remove(_) => originals
                .next()
                .map(|original| ending_of(original))
                .filter(|ending| !ending.is_empty()),
            Line::Add(_) => None,
        })
        .collect();
    let nearest = |at: usize| {
        endings[..at]
            .iter()
            .rev()
            .flatten()
            .chain(endings[at..].iter().flatten())
            .next()
            .copied()
            .unwrap_or(usual)
    };
    let mut originals = matched.iter();
    let mut out = Vec::new();
    for (at, line) in hunk.lines.iter().enumerate() {
        match line {
            Line::Context(_) => {
                let original = originals.next().map_or("", String::as_str);
                let ending = ending_of(original);
                let ending = if ending.is_empty() {
                    nearest(at)
                } else {
                    ending
                };
                out.push(format!("{}{ending}", strip_ending(original)));
            }
            Line::Remove(_) => {
                originals.next();
            }
            Line::Add(text) => out.push(format!("{text}{}", nearest(at))),
        }
    }
    out
}

/// Apply one file's hunks to `original` (empty for a created file).
/// Returns the new content, or `None` when the patch deletes the file.
///
/// # Errors
///
/// [`ErrorCode::Conflict`] when a hunk's context is not in the file.
pub fn apply(original: &str, patch: &FilePatch) -> ToolResult<Option<String>> {
    let mut lines: Vec<String> = original.split_inclusive('\n').map(str::to_owned).collect();
    let usual = usual_ending(&lines);
    // `cursor`: hunks apply in order and never overlap the previous one.
    let mut cursor = 0usize;
    let mut delta: isize = 0;
    for (number, hunk) in patch.hunks.iter().enumerate() {
        let old: Vec<&str> = hunk
            .lines
            .iter()
            .filter_map(|line| match line {
                Line::Context(text) | Line::Remove(text) => Some(text.as_str()),
                Line::Add(_) => None,
            })
            .collect();
        let stated = if hunk.old_len == 0 {
            hunk.old_start
        } else {
            hunk.old_start.saturating_sub(1)
        };
        let expected = stated.saturating_add_signed(delta).max(cursor);
        let matches_at = |at: usize| {
            at + old.len() <= lines.len()
                && old
                    .iter()
                    .zip(&lines[at..at + old.len()])
                    .all(|(want, have)| *want == strip_ending(have))
        };
        let position = (0..=lines.len())
            .flat_map(|distance| {
                [
                    expected.checked_add(distance),
                    expected.checked_sub(distance),
                ]
            })
            .flatten()
            .filter(|at| *at >= cursor && *at <= lines.len())
            .take(2 * (lines.len() + 1))
            .find(|at| matches_at(*at))
            .ok_or_else(|| {
                ToolError::new(
                    ErrorCode::Conflict,
                    format!(
                        "hunk {} of `{}` does not match the file (stated at line {})",
                        number + 1,
                        patch.target(),
                        hunk.old_start
                    ),
                )
            })?;
        let replacement = replacement(hunk, &lines[position..position + old.len()], usual);
        let replaced_tail = position + old.len() == lines.len();
        if hunk.old_no_newline && !replaced_tail {
            return Err(ToolError::new(
                ErrorCode::Conflict,
                format!(
                    "hunk {} of `{}` expects the end of the file",
                    number + 1,
                    patch.target()
                ),
            ));
        }
        let added = replacement.len();
        lines.splice(position..position + old.len(), replacement);
        if replaced_tail
            && hunk.new_no_newline
            && added > 0
            && let Some(last) = lines.last_mut()
        {
            let trimmed = strip_ending(last).to_owned();
            *last = trimmed;
        }
        cursor = position + added;
        // Where the original's lines now sit relative to their numbers.
        delta = signed(position) - signed(stated) + signed(added) - signed(old.len());
    }
    if patch.is_delete() {
        if lines.is_empty() {
            return Ok(None);
        }
        return Err(ToolError::new(
            ErrorCode::Conflict,
            format!(
                "the patch deletes `{}` but does not remove all of its content",
                patch.target()
            ),
        ));
    }
    Ok(Some(lines.concat()))
}

#[cfg(test)]
mod tests {
    use super::{apply, parse};
    use crate::error::ErrorCode;

    fn apply_one(original: &str, diff: &str) -> crate::error::ToolResult<Option<String>> {
        let patches = parse(diff)?;
        assert_eq!(patches.len(), 1);
        apply(original, &patches[0])
    }

    #[test]
    fn a_git_diff_applies() {
        let original = "one\ntwo\nthree\nfour\n";
        let diff = "diff --git a/f.txt b/f.txt\nindex 1..2 100644\n--- a/f.txt\n+++ b/f.txt\n@@ -1,4 +1,4 @@\n one\n-two\n+TWO\n three\n four\n";
        let patches = parse(diff).expect("parse");
        assert_eq!(patches[0].target(), "f.txt");
        assert_eq!(
            apply(original, &patches[0]).expect("apply").as_deref(),
            Some("one\nTWO\nthree\nfour\n")
        );
    }

    #[test]
    fn hunks_with_drifted_line_numbers_still_land_and_apply_in_order() {
        let original = "h1\nh2\na\nb\nc\nx\ny\nz\n";
        let diff =
            "--- f\n+++ f\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n@@ -4,3 +4,4 @@\n x\n y\n+y2\n z\n";
        assert_eq!(
            apply_one(original, diff).expect("apply").as_deref(),
            Some("h1\nh2\na\nB\nc\nx\ny\ny2\nz\n")
        );
    }

    #[test]
    fn a_mismatched_context_is_a_conflict() {
        let error = apply_one("a\nb\nc\n", "--- f\n+++ f\n@@ -1,2 +1,2 @@\n a\n-q\n+r\n")
            .expect_err("conflict");
        assert_eq!(error.code(), ErrorCode::Conflict);
    }

    #[test]
    fn creates_and_deletes() {
        let created = parse("--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1,2 @@\n+hello\n+world\n")
            .expect("parse");
        assert!(created[0].is_create());
        assert_eq!(
            apply("", &created[0]).expect("apply").as_deref(),
            Some("hello\nworld\n")
        );
        let deleted = parse("--- a/old.txt\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-hello\n-world\n")
            .expect("parse");
        assert!(deleted[0].is_delete());
        assert_eq!(apply("hello\nworld\n", &deleted[0]).expect("apply"), None);
        assert_eq!(
            apply("hello\nworld\nmore\n", &deleted[0])
                .expect_err("partial")
                .code(),
            ErrorCode::Conflict
        );
    }

    #[test]
    fn no_newline_markers_and_crlf_are_kept() {
        let diff = "--- f\n+++ f\n@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+c\n\\ No newline at end of file\n";
        assert_eq!(
            apply_one("a\nb", diff).expect("apply").as_deref(),
            Some("a\nc")
        );
        let crlf = "--- f\n+++ f\n@@ -1,2 +1,2 @@\n a\n-b\n+c\n";
        assert_eq!(
            apply_one("a\r\nb\r\n", crlf).expect("apply").as_deref(),
            Some("a\r\nc\r\n")
        );
    }

    #[test]
    fn several_files_parse_and_malformed_input_is_refused() {
        let diff = "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-1\n+2\n--- a/y\n+++ b/y\n@@ -1 +1 @@\n-3\n+4\n";
        let patches = parse(diff).expect("parse");
        assert_eq!(
            patches
                .iter()
                .map(super::FilePatch::target)
                .collect::<Vec<_>>(),
            ["x", "y"]
        );
        for bad in [
            "just text",
            "--- a\nno plus",
            "--- a\n+++ a\n@@ -1,3 +1,1 @@\n x\n",
            "--- a\n+++ a\n@@ nonsense @@\n",
        ] {
            assert_eq!(
                parse(bad).expect_err(bad).code(),
                ErrorCode::InvalidArgument,
                "{bad}"
            );
        }
    }

    /// git sections without `---`/`+++`: a pure rename and empty files
    /// created or deleted are changes, not silently dropped.
    #[test]
    fn header_only_git_sections_parse() {
        let diff = "diff --git a/old.txt b/new.txt\nsimilarity index 100%\nrename from old.txt\nrename to new.txt\ndiff --git a/empty.txt b/empty.txt\nnew file mode 100644\nindex 0000000..e69de29\ndiff --git a/gone.txt b/gone.txt\ndeleted file mode 100644\nindex e69de29..0000000\ndiff --git a/f.txt b/f.txt\nindex 1..2 100644\n--- a/f.txt\n+++ b/f.txt\n@@ -1 +1 @@\n-a\n+b\n";
        let patches = parse(diff).expect("parse");
        assert_eq!(patches.len(), 4);
        assert!(patches[0].is_rename());
        assert_eq!(patches[0].old_path.as_deref(), Some("old.txt"));
        assert_eq!(patches[0].new_path.as_deref(), Some("new.txt"));
        assert_eq!(
            apply("keep\r\nme\n", &patches[0])
                .expect("apply")
                .as_deref(),
            Some("keep\r\nme\n"),
            "a pure rename keeps the content byte for byte"
        );
        assert!(patches[1].is_create());
        assert_eq!(patches[1].target(), "empty.txt");
        assert_eq!(apply("", &patches[1]).expect("apply").as_deref(), Some(""));
        assert!(patches[2].is_delete());
        assert_eq!(patches[2].target(), "gone.txt");
        assert_eq!(apply("", &patches[2]).expect("apply"), None);
        assert_eq!(
            apply("not empty\n", &patches[2])
                .expect_err("content")
                .code(),
            ErrorCode::Conflict
        );
        assert_eq!(patches[3].target(), "f.txt");
        let executable = parse(
            "diff --git a/run.sh b/run.sh\nnew file mode 100755\n--- /dev/null\n+++ b/run.sh\n@@ -0,0 +1 @@\n+#!/bin/sh\n",
        )
        .expect("parse");
        assert_eq!(executable[0].created_mode(), Some(0o755));
        assert_eq!(patches[1].created_mode(), None);
        let edited_rename = "diff --git a/a.txt b/b.txt\nsimilarity index 80%\nrename from a.txt\nrename to b.txt\n--- a/a.txt\n+++ b/b.txt\n@@ -1 +1 @@\n-x\n+y\n";
        let patches = parse(edited_rename).expect("parse");
        assert!(patches[0].is_rename());
        assert_eq!(
            apply("x\n", &patches[0]).expect("apply").as_deref(),
            Some("y\n")
        );
    }

    /// Binary and mode-only sections cannot be applied: the whole patch is
    /// refused (nothing applies), not the section skipped.
    #[test]
    fn binary_mode_and_copy_sections_refuse_the_whole_patch() {
        let ok = "--- a/f.txt\n+++ b/f.txt\n@@ -1 +1 @@\n-a\n+b\n";
        for section in [
            "diff --git a/x.png b/x.png\nindex 1..2 100644\nBinary files a/x.png and b/x.png differ\n",
            "diff --git a/x.png b/x.png\nindex 1..2 100644\nGIT binary patch\nliteral 3\nKcmZ?wbN~PV\n\n",
            "diff --git a/run.sh b/run.sh\nold mode 100644\nnew mode 100755\n",
            "diff --git a/a b/c\nsimilarity index 100%\ncopy from a\ncopy to c\n",
            "diff --git a/l b/l\nnew file mode 120000\n--- /dev/null\n+++ b/l\n@@ -0,0 +1 @@\n+target\n\\ No newline at end of file\n",
        ] {
            for diff in [format!("{ok}{section}"), format!("{section}{ok}")] {
                let error = parse(&diff).expect_err(section);
                assert_eq!(error.code(), ErrorCode::Unsupported, "{section}");
            }
        }
        let mode_and_content = "diff --git a/run.sh b/run.sh\nold mode 100644\nnew mode 100755\n--- a/run.sh\n+++ b/run.sh\n@@ -1 +1 @@\n-a\n+b\n";
        assert_eq!(
            parse(mode_and_content).expect_err("mode").code(),
            ErrorCode::Unsupported
        );
        assert_eq!(
            parse("diff --git a/f b/f\nindex 1..2 100644\n")
                .expect_err("no change")
                .code(),
            ErrorCode::InvalidArgument
        );
    }

    /// Context lines keep their own line endings; an added line takes the
    /// ending of the original line next to it.
    #[test]
    fn mixed_line_endings_are_preserved() {
        let original = "one\r\ntwo\nthree\r\nfour\n";
        let diff = "--- f\n+++ f\n@@ -1,4 +1,5 @@\n one\n two\n-three\n+THREE\n+inserted\n four\n";
        assert_eq!(
            apply_one(original, diff).expect("apply").as_deref(),
            Some("one\r\ntwo\nTHREE\r\ninserted\r\nfour\n")
        );
        let lf_with_one_crlf = "a\nb\r\nc\nd\n";
        let edit = "--- f\n+++ f\n@@ -2,3 +2,3 @@\n b\n-c\n+C\n d\n";
        assert_eq!(
            apply_one(lf_with_one_crlf, edit).expect("apply").as_deref(),
            Some("a\nb\r\nC\nd\n"),
            "one CRLF line does not turn the file's other lines into CRLF"
        );
        let pure_insert = "--- f\n+++ f\n@@ -1,2 +1,3 @@\n a\n+new\n b\n";
        assert_eq!(
            apply_one("a\r\nb\n", pure_insert)
                .expect("apply")
                .as_deref(),
            Some("a\r\nnew\r\nb\n")
        );
        let at_end_without_newline =
            "--- f\n+++ f\n@@ -1,2 +1,3 @@\n a\n-b\n\\ No newline at end of file\n+b\n+c\n";
        assert_eq!(
            apply_one("a\r\nb", at_end_without_newline)
                .expect("apply")
                .as_deref(),
            Some("a\r\nb\r\nc\r\n")
        );
    }

    #[test]
    fn insertion_into_an_empty_position() {
        let diff = "--- f\n+++ f\n@@ -2,0 +3,1 @@\n+inserted\n";
        assert_eq!(
            apply_one("a\nb\nc\n", diff).expect("apply").as_deref(),
            Some("a\nb\ninserted\nc\n")
        );
    }
}
