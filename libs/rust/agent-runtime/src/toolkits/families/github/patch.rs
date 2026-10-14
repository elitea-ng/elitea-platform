//! The unified-diff reader and applier behind `apply_git_patch`.
//!
//! It follows the pinned SDK's parser (`_parse_git_patch`, `_parse_file_change`,
//! `_parse_hunk` in `elitea_sdk/tools/github/github_client.py`): changes start
//! at `diff --git a/OLD b/NEW`, the operation is `new file mode` (create),
//! `deleted file mode` (delete), `rename from` (rename) or modify, and a
//! hunk starts at `@@ -a,b +c,d @@`. Four places differ, each because the
//! SDK's reading of a valid diff corrupts the file:
//!
//! * the SDK applies a hunk without comparing it to the file, so a patch made
//!   for other content deletes and inserts at the wrong lines. Here every
//!   context and removed line must equal the file's line, or the file is
//!   refused;
//! * the SDK ignores a blank line inside a hunk (an editor stripped the space
//!   of an empty context line), which shifts every later line. Here it is
//!   an empty context line, as `git apply` reads it;
//! * `\ No newline at end of file` ends the SDK's hunk and the lines after it
//!   are dropped. Here it is a marker on the line before it;
//! * a created file ends with a newline unless the diff says it does not;
//!   the SDK joins the added lines without one.

/// What a change does to a file.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum Operation {
    Create,
    Delete,
    Modify,
    Rename,
}

/// One hunk of a modification.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(super) struct Hunk {
    old_start: usize,
    old_count: usize,
    lines: Vec<String>,
    /// The new side of the hunk ends without a newline.
    new_has_no_newline: bool,
}

/// One file of a patch.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(super) struct FileChange {
    pub(super) old_path: String,
    pub(super) path: String,
    pub(super) operation: Operation,
    pub(super) hunks: Vec<Hunk>,
}

/// The files a patch changes, in patch order.
pub(super) fn parse_patch(patch: &str) -> Vec<FileChange> {
    // `split('\n')`, as the SDK does: a CR stays on the line, so a CRLF file
    // is compared and created with its CRs.
    #[allow(
        clippy::str_split_at_newline,
        reason = "`lines()` would drop the CR of a CRLF patch"
    )]
    let lines: Vec<&str> = patch.trim().split('\n').collect();
    let mut changes = Vec::new();
    let mut index = 0;
    while index < lines.len() {
        if lines[index].starts_with("diff --git")
            && let Some((change, next)) = parse_file_change(&lines, index)
        {
            changes.push(change);
            index = next.max(index + 1);
            continue;
        }
        index += 1;
    }
    changes
}

fn parse_file_change(lines: &[&str], start: usize) -> Option<(FileChange, usize)> {
    let rest = lines[start].strip_prefix("diff --git a/")?;
    // The SDK's greedy `(.+) b/(.+)`: the old path runs to the LAST ` b/`.
    let split = rest.rfind(" b/")?;
    let old_path = &rest[..split];
    let path = &rest[split + 3..];
    if old_path.is_empty() || path.is_empty() {
        return None;
    }
    let mut operation = None;
    let mut hunks = Vec::new();
    let mut index = start + 1;
    while index < lines.len() {
        let line = lines[index];
        if line.starts_with("new file mode") {
            operation = Some(Operation::Create);
        } else if line.starts_with("deleted file mode") {
            operation = Some(Operation::Delete);
        } else if line.starts_with("rename from") {
            operation = Some(Operation::Rename);
        } else if line.starts_with("@@") {
            let (hunk, next) = parse_hunk(lines, index);
            if let Some(hunk) = hunk {
                hunks.push(hunk);
            }
            index = next;
            continue;
        } else if line.starts_with("diff --git") || line.starts_with("Binary files") {
            break;
        }
        index += 1;
    }
    let operation = operation.unwrap_or(if old_path == path {
        Operation::Modify
    } else {
        Operation::Rename
    });
    Some((
        FileChange {
            old_path: old_path.to_owned(),
            path: path.to_owned(),
            operation,
            hunks,
        },
        index,
    ))
}

fn parse_hunk(lines: &[&str], start: usize) -> (Option<Hunk>, usize) {
    let Some((old_start, old_count)) = parse_hunk_header(lines[start]) else {
        return (None, start + 1);
    };
    let mut body: Vec<String> = Vec::new();
    let mut new_has_no_newline = false;
    let mut index = start + 1;
    while index < lines.len() {
        let line = lines[index];
        if line.starts_with("@@")
            || line.starts_with("diff --git")
            || line.starts_with("Binary files")
        {
            break;
        }
        if line.starts_with('\\') {
            // `\ No newline at end of file` marks the line before it.
            if body
                .last()
                .is_some_and(|last| last.starts_with('+') || last.starts_with(' '))
            {
                new_has_no_newline = true;
            }
            index += 1;
            continue;
        }
        if line.starts_with(' ') || line.starts_with('+') || line.starts_with('-') {
            body.push(line.to_owned());
        } else if line.trim().is_empty() {
            // An empty context line whose space an editor stripped.
            body.push(" ".to_owned());
        } else {
            break;
        }
        index += 1;
    }
    (
        Some(Hunk {
            old_start,
            old_count,
            lines: body,
            new_has_no_newline,
        }),
        index,
    )
}

/// `@@ -a[,b] +c[,d] @@`: the old start and old count.
fn parse_hunk_header(line: &str) -> Option<(usize, usize)> {
    let rest = line.strip_prefix("@@ -")?;
    let (old, rest) = rest.split_once(" +")?;
    let (_, _) = rest.split_once(" @@")?;
    let (start, count) = old
        .split_once(',')
        .map_or((old, None), |(start, count)| (start, Some(count)));
    let start: usize = start.parse().ok()?;
    let count = match count {
        Some(count) => count.parse().ok()?,
        None => 1,
    };
    Some((start, count))
}

/// The content a `new file mode` change creates: its added lines.
pub(super) fn created_content(change: &FileChange) -> String {
    let mut lines: Vec<&str> = Vec::new();
    let mut no_newline = false;
    for hunk in &change.hunks {
        no_newline = hunk.new_has_no_newline;
        for line in &hunk.lines {
            if let Some(text) = line.strip_prefix('+') {
                lines.push(text);
            }
        }
    }
    if lines.is_empty() {
        return String::new();
    }
    let mut content = lines.join("\n");
    if !no_newline {
        content.push('\n');
    }
    content
}

/// Why a change cannot be applied to the content it was made for.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(super) enum ApplyError {
    /// A context or removed line is not the file's line.
    Mismatch { line: usize },
    /// A hunk starts inside an earlier one.
    Overlap { line: usize },
}

impl std::fmt::Display for ApplyError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Mismatch { line } => write!(
                formatter,
                "the patch does not match the file at line {line}; the file has changed or the patch was made for other content"
            ),
            Self::Overlap { line } => {
                write!(formatter, "two hunks overlap at line {line}")
            }
        }
    }
}

/// `_apply_patch_to_content`, with every context and removed line compared to
/// the file's.
pub(super) fn apply_hunks(original: &str, hunks: &[Hunk]) -> Result<String, ApplyError> {
    if hunks.is_empty() {
        return Ok(original.to_owned());
    }
    let lines: Vec<&str> = original.split('\n').collect();
    let mut result: Vec<&str> = Vec::with_capacity(lines.len());
    let mut sorted: Vec<&Hunk> = hunks.iter().collect();
    sorted.sort_by_key(|hunk| hunk.old_start);
    let mut current = 0_usize;
    for hunk in sorted {
        let copy_until = if hunk.old_count == 0 {
            hunk.old_start
        } else {
            hunk.old_start.saturating_sub(1)
        };
        if copy_until < current {
            return Err(ApplyError::Overlap {
                line: hunk.old_start,
            });
        }
        while current < copy_until {
            if let Some(line) = lines.get(current) {
                result.push(line);
            }
            current += 1;
        }
        for patch_line in &hunk.lines {
            if let Some(added) = patch_line.strip_prefix('+') {
                result.push(added);
                continue;
            }
            let expected = &patch_line[1..];
            let Some(actual) = lines.get(current) else {
                return Err(ApplyError::Mismatch { line: current + 1 });
            };
            if actual.trim_end_matches('\r') != expected.trim_end_matches('\r') {
                return Err(ApplyError::Mismatch { line: current + 1 });
            }
            if patch_line.starts_with(' ') {
                result.push(actual);
            }
            current += 1;
        }
    }
    while current < lines.len() {
        result.push(lines[current]);
        current += 1;
    }
    Ok(result.join("\n"))
}

#[cfg(test)]
mod tests {
    use super::*;

    const MODIFY: &str = "diff --git a/src/a.txt b/src/a.txt\nindex 1..2 100644\n--- a/src/a.txt\n+++ b/src/a.txt\n@@ -1,3 +1,3 @@\n one\n-two\n+TWO\n three\n";

    #[test]
    fn a_modification_is_parsed_and_applied() {
        let changes = parse_patch(MODIFY);
        assert_eq!(changes.len(), 1);
        assert_eq!(changes[0].operation, Operation::Modify);
        assert_eq!(changes[0].path, "src/a.txt");
        let updated = apply_hunks("one\ntwo\nthree\n", &changes[0].hunks).expect("applies");
        assert_eq!(updated, "one\nTWO\nthree\n");
    }

    #[test]
    fn a_hunk_made_for_other_content_is_refused_not_applied() {
        let changes = parse_patch(MODIFY);
        let error = apply_hunks("one\nOTHER\nthree\n", &changes[0].hunks).expect_err("mismatch");
        assert_eq!(error, ApplyError::Mismatch { line: 2 });
        let error = apply_hunks("one\n", &changes[0].hunks).expect_err("short file");
        assert!(matches!(error, ApplyError::Mismatch { .. }));
    }

    #[test]
    fn created_deleted_and_renamed_files_are_told_apart() {
        let patch = "diff --git a/new.txt b/new.txt\nnew file mode 100644\n--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1,2 @@\n+hello\n+world\ndiff --git a/gone.txt b/gone.txt\ndeleted file mode 100644\n--- a/gone.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye\ndiff --git a/old.txt b/moved.txt\nsimilarity index 100%\nrename from old.txt\nrename to moved.txt\n";
        let changes = parse_patch(patch);
        let operations: Vec<_> = changes.iter().map(|change| change.operation).collect();
        assert_eq!(
            operations,
            [Operation::Create, Operation::Delete, Operation::Rename]
        );
        assert_eq!(created_content(&changes[0]), "hello\nworld\n");
        assert_eq!(changes[2].old_path, "old.txt");
        assert_eq!(changes[2].path, "moved.txt");
    }

    #[test]
    fn a_created_file_keeps_a_missing_final_newline() {
        let patch = "diff --git a/n.txt b/n.txt\nnew file mode 100644\n--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1 @@\n+only\n\\ No newline at end of file\n";
        let changes = parse_patch(patch);
        assert_eq!(created_content(&changes[0]), "only");
    }

    #[test]
    fn lines_after_a_no_newline_marker_are_kept() {
        let patch = "diff --git a/f.txt b/f.txt\n--- a/f.txt\n+++ b/f.txt\n@@ -1,2 +1,2 @@\n keep\n-old\n\\ No newline at end of file\n+new\n\\ No newline at end of file\n";
        let changes = parse_patch(patch);
        let updated = apply_hunks("keep\nold", &changes[0].hunks).expect("applies");
        assert_eq!(updated, "keep\nnew");
    }

    #[test]
    fn a_blank_line_in_a_hunk_is_an_empty_context_line() {
        let patch =
            "diff --git a/f.txt b/f.txt\n--- a/f.txt\n+++ b/f.txt\n@@ -1,3 +1,3 @@\n a\n\n-b\n+B\n";
        let changes = parse_patch(patch);
        let updated = apply_hunks("a\n\nb\n", &changes[0].hunks).expect("applies");
        assert_eq!(updated, "a\n\nB\n");
    }

    #[test]
    fn an_addition_after_a_line_and_two_hunks_apply_in_order() {
        let patch = "diff --git a/f.txt b/f.txt\n--- a/f.txt\n+++ b/f.txt\n@@ -0,0 +1 @@\n+top\n@@ -3,1 +4,1 @@\n-c\n+C\n";
        let changes = parse_patch(patch);
        // old_count 0 inserts after line `old_start` (0 = the top).
        let updated = apply_hunks("a\nb\nc\n", &changes[0].hunks).expect("applies");
        assert_eq!(updated, "top\na\nb\nC\n");
    }

    #[test]
    fn overlapping_hunks_are_refused() {
        let hunks = vec![
            Hunk {
                old_start: 1,
                old_count: 2,
                lines: vec![" a".into(), " b".into()],
                new_has_no_newline: false,
            },
            Hunk {
                old_start: 2,
                old_count: 1,
                lines: vec![" b".into()],
                new_has_no_newline: false,
            },
        ];
        assert_eq!(
            apply_hunks("a\nb\n", &hunks),
            Err(ApplyError::Overlap { line: 2 })
        );
    }

    #[test]
    fn nothing_is_found_in_text_that_is_not_a_patch() {
        assert!(parse_patch("hello world").is_empty());
        assert!(parse_patch("diff --git broken").is_empty());
    }
}
