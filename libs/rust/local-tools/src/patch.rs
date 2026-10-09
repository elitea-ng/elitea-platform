//! Unified diffs: parse one (`diff -u`, `git diff`) and apply it exactly.
//!
//! A hunk applies where its context and removed lines match the file
//! exactly (line endings aside), searching outward from the line numbers
//! the header states, so a hunk whose numbers drifted still lands. There is
//! no fuzz: a hunk whose context does not match anywhere fails the whole
//! patch, and nothing is written.

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

/// Parse a unified diff of one or more files.
///
/// # Errors
///
/// [`ErrorCode::InvalidArgument`] when it is not a well-formed unified diff.
pub fn parse(text: &str) -> ToolResult<Vec<FilePatch>> {
    let lines: Vec<&str> = text.lines().collect();
    let mut patches = Vec::new();
    let mut index = 0;
    while index < lines.len() {
        let Some(old) = lines[index].strip_prefix("--- ") else {
            index += 1;
            continue;
        };
        let new = lines
            .get(index + 1)
            .and_then(|line| line.strip_prefix("+++ "))
            .ok_or_else(|| malformed("`---` without `+++`"))?;
        let git_style = old.starts_with("a/") || new.starts_with("b/");
        let (old_prefix, new_prefix) = if git_style { ("a/", "b/") } else { ("", "") };
        let mut patch = FilePatch {
            old_path: header_path(old, old_prefix),
            new_path: header_path(new, new_prefix),
            hunks: Vec::new(),
        };
        if patch.old_path.is_none() && patch.new_path.is_none() {
            return Err(malformed("both sides are /dev/null"));
        }
        index += 2;
        while let Some(header) = lines.get(index).filter(|line| line.starts_with("@@")) {
            let (old_start, old_len, new_len) = parse_hunk_header(header)
                .ok_or_else(|| malformed(format!("bad hunk header `{header}`")))?;
            index += 1;
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
                    .get(index)
                    .ok_or_else(|| malformed("a hunk ends before its stated length"))?;
                index += 1;
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
            while lines.get(index).is_some_and(|line| line.starts_with('\\')) {
                mark_no_newline(&mut hunk, lines.get(index - 1).copied());
                index += 1;
            }
            patch.hunks.push(hunk);
        }
        if patch.hunks.is_empty() && !(patch.is_create() || patch.is_delete()) {
            return Err(malformed(format!("no hunks for `{}`", patch.target())));
        }
        patches.push(patch);
    }
    if patches.is_empty() {
        return Err(malformed("no `---`/`+++` file headers"));
    }
    Ok(patches)
}

fn signed(value: usize) -> isize {
    isize::try_from(value).unwrap_or(isize::MAX)
}

fn strip_ending(line: &str) -> &str {
    line.strip_suffix('\n')
        .map_or(line, |line| line.strip_suffix('\r').unwrap_or(line))
}

/// Apply one file's hunks to `original` (empty for a created file).
/// Returns the new content, or `None` when the patch deletes the file.
///
/// # Errors
///
/// [`ErrorCode::Conflict`] when a hunk's context is not in the file.
pub fn apply(original: &str, patch: &FilePatch) -> ToolResult<Option<String>> {
    let ending = if original.contains("\r\n") {
        "\r\n"
    } else {
        "\n"
    };
    let mut lines: Vec<String> = original.split_inclusive('\n').map(str::to_owned).collect();
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
        let replacement: Vec<String> = hunk
            .lines
            .iter()
            .filter_map(|line| match line {
                Line::Context(text) | Line::Add(text) => Some(format!("{text}{ending}")),
                Line::Remove(_) => None,
            })
            .collect();
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

    #[test]
    fn insertion_into_an_empty_position() {
        let diff = "--- f\n+++ f\n@@ -2,0 +3,1 @@\n+inserted\n";
        assert_eq!(
            apply_one("a\nb\nc\n", diff).expect("apply").as_deref(),
            Some("a\nb\ninserted\nc\n")
        );
    }
}
