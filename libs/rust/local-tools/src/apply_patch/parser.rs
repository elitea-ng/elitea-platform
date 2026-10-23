// Portions adapted from OpenAI Codex (https://github.com/openai/codex), codex-rs/apply-patch/src/parser.rs @ be9060f (and the line rules of codex-rs/apply-patch/src/streaming_parser.rs). Apache License 2.0. Copyright 2025 OpenAI.
// Changes: one non-streaming pass over whole lines; paths are kept as the
// patch spells them (resolved by the confined Workspace, never joined to a
// cwd); errors are this crate's ToolError; `*** Environment ID:` is read and
// ignored.

//! Parse the Codex `*** Begin Patch` envelope into hunks. It does not look
//! at the file system.
//!
//! The grammar (Lark, as Codex states it):
//!
//! ```text
//! start: begin_patch environment_id? hunk+ end_patch
//! begin_patch: "*** Begin Patch" LF
//! environment_id: "*** Environment ID: " filename LF
//! end_patch: "*** End Patch" LF?
//!
//! hunk: add_hunk | delete_hunk | update_hunk
//! add_hunk: "*** Add File: " filename LF add_line+
//! delete_hunk: "*** Delete File: " filename LF
//! update_hunk: "*** Update File: " filename LF change_move? change?
//! filename: /(.+)/
//! add_line: "+" /(.+)/ LF -> line
//!
//! change_move: "*** Move to: " filename LF
//! change: (change_context | change_line)+ eof_line?
//! change_context: ("@@" | "@@ " /(.+)/) LF
//! change_line: ("+" | "-" | " ") /(.+)/ LF
//! eof_line: "*** End of File" LF
//! ```
//!
//! The parser is a little more lenient than the grammar: whitespace around
//! the markers is allowed, and the envelope may be wrapped in a heredoc
//! (`<<EOF` … `EOF`, quoted or not), as some models send it.

use crate::error::{ToolError, ToolResult};

pub const BEGIN_PATCH_MARKER: &str = "*** Begin Patch";
pub const END_PATCH_MARKER: &str = "*** End Patch";
pub const ADD_FILE_MARKER: &str = "*** Add File: ";
pub const DELETE_FILE_MARKER: &str = "*** Delete File: ";
pub const UPDATE_FILE_MARKER: &str = "*** Update File: ";
pub const MOVE_TO_MARKER: &str = "*** Move to: ";
pub const EOF_MARKER: &str = "*** End of File";
pub const CHANGE_CONTEXT_MARKER: &str = "@@ ";
pub const EMPTY_CHANGE_CONTEXT_MARKER: &str = "@@";
const ENVIRONMENT_ID_MARKER: &str = "*** Environment ID:";

/// One file's change.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Hunk {
    AddFile {
        path: String,
        contents: String,
    },
    DeleteFile {
        path: String,
    },
    UpdateFile {
        path: String,
        move_path: Option<String>,
        /// In file order: each chunk's context follows the previous chunk.
        chunks: Vec<UpdateFileChunk>,
    },
}

impl Hunk {
    /// The path the hunk names first (an update's source).
    #[must_use]
    pub fn path(&self) -> &str {
        match self {
            Hunk::AddFile { path, .. }
            | Hunk::DeleteFile { path }
            | Hunk::UpdateFile { path, .. } => path,
        }
    }
}

/// One `@@` chunk of an update.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct UpdateFileChunk {
    /// The `@@ <line>` anchor: a line (usually a class or function header)
    /// the chunk's lines follow.
    pub change_context: Option<String>,
    /// The lines replaced, context included.
    pub old_lines: Vec<String>,
    /// What replaces them.
    pub new_lines: Vec<String>,
    /// `(old, new)` indices of the lines parsed as context, so they stay
    /// as the file has them.
    pub context_line_indices: Vec<(usize, usize)>,
    /// `*** End of File`: `old_lines` end the file.
    pub is_end_of_file: bool,
}

impl UpdateFileChunk {
    fn push_context_line(&mut self, line: String) {
        self.context_line_indices
            .push((self.old_lines.len(), self.new_lines.len()));
        self.old_lines.push(line.clone());
        self.new_lines.push(line);
    }

    fn is_empty(&self) -> bool {
        self.old_lines.is_empty() && self.new_lines.is_empty()
    }
}

fn invalid_patch(message: &str) -> ToolError {
    ToolError::invalid(format!("invalid patch: {message}"))
}

fn invalid_hunk(line_number: usize, message: &str) -> ToolError {
    ToolError::invalid(format!("invalid hunk at line {line_number}: {message}"))
}

const BAD_HEADER: &str = "is not a valid hunk header. Valid hunk headers: '*** Add File: {path}', '*** Delete File: {path}', '*** Update File: {path}'";
const BAD_UPDATE_LINE: &str =
    "Every line should start with ' ' (context line), '+' (added line), or '-' (removed line)";

/// Whether `text` is a Codex envelope (possibly in a heredoc), not a
/// unified diff.
#[must_use]
pub fn is_envelope(text: &str) -> bool {
    let mut lines = text.trim().lines().map(str::trim);
    match lines.next() {
        Some(BEGIN_PATCH_MARKER) => true,
        Some("<<EOF" | "<<'EOF'" | "<<\"EOF\"") => lines.next() == Some(BEGIN_PATCH_MARKER),
        _ => false,
    }
}

/// The lines between the markers: checks the first and last line, and
/// unwraps a heredoc (`<<EOF`, `<<'EOF'`, `<<"EOF"` … `EOF`).
fn patch_lines<'a>(lines: &'a [&'a str]) -> ToolResult<&'a [&'a str]> {
    let strict = |lines: &'a [&'a str]| -> ToolResult<&'a [&'a str]> {
        let first = lines.first().map(|line| line.trim());
        let last = lines.last().map(|line| line.trim());
        match (first, last) {
            (Some(BEGIN_PATCH_MARKER), Some(END_PATCH_MARKER)) if lines.len() >= 2 => Ok(lines),
            (Some(first), _) if first != BEGIN_PATCH_MARKER => Err(invalid_patch(
                "The first line of the patch must be '*** Begin Patch'",
            )),
            _ => Err(invalid_patch(
                "The last line of the patch must be '*** End Patch'",
            )),
        }
    };
    let error = match strict(lines) {
        Ok(lines) => return Ok(lines),
        Err(error) => error,
    };
    match lines {
        [first, .., last]
            if matches!(first.trim(), "<<EOF" | "<<'EOF'" | "<<\"EOF\"")
                && last.trim_end().ends_with("EOF")
                && lines.len() >= 4 =>
        {
            strict(&lines[1..lines.len() - 1])
        }
        _ => Err(error),
    }
}

/// Parse a Codex envelope.
///
/// # Errors
///
/// [`crate::error::ErrorCode::InvalidArgument`] naming the line that is
/// not well formed.
pub fn parse_patch(text: &str) -> ToolResult<Vec<Hunk>> {
    let all: Vec<&str> = text.trim().lines().collect();
    let lines = patch_lines(&all)?;
    let mut parser = Parser::default();
    // The first line is `*** Begin Patch`, the last `*** End Patch`.
    for (index, line) in lines[1..lines.len() - 1].iter().enumerate() {
        let line = line.strip_suffix('\r').unwrap_or(line);
        parser.line(index + 2, line)?;
    }
    parser.end(lines.len())?;
    Ok(parser.hunks)
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
enum Mode {
    #[default]
    Started,
    AddFile,
    DeleteFile,
    UpdateFile {
        header_line: usize,
    },
}

#[derive(Default)]
struct Parser {
    mode: Mode,
    hunks: Vec<Hunk>,
    environment_id: bool,
}

impl Parser {
    /// An update hunk that ends must have a chunk with lines.
    fn check_update_not_empty(&self, line_number: usize, line: &str) -> ToolResult<()> {
        let Some(Hunk::UpdateFile { path, chunks, .. }) = self.hunks.last() else {
            return Ok(());
        };
        if let (true, Mode::UpdateFile { header_line }) = (chunks.is_empty(), self.mode) {
            return Err(invalid_hunk(
                header_line,
                &format!("Update file hunk for path '{path}' is empty"),
            ));
        }
        if chunks.last().is_some_and(UpdateFileChunk::is_empty) {
            if line == END_PATCH_MARKER {
                return Err(invalid_hunk(
                    line_number,
                    "Update hunk does not contain any lines",
                ));
            }
            return Err(invalid_hunk(
                line_number,
                &format!("Unexpected line found in update hunk: '{line}'. {BAD_UPDATE_LINE}"),
            ));
        }
        Ok(())
    }

    /// A file header (or an environment id): `true` when `trimmed` was one.
    fn header(&mut self, line_number: usize, trimmed: &str) -> ToolResult<bool> {
        if self.mode == Mode::Started
            && self.hunks.is_empty()
            && let Some(id) = trimmed.strip_prefix(ENVIRONMENT_ID_MARKER)
        {
            if self.environment_id {
                return Err(invalid_patch(
                    "apply_patch environment_id cannot be specified more than once",
                ));
            }
            if id.trim().is_empty() {
                return Err(invalid_patch("apply_patch environment_id cannot be empty"));
            }
            self.environment_id = true;
            return Ok(true);
        }
        let (hunk, mode) = if let Some(path) = trimmed.strip_prefix(ADD_FILE_MARKER) {
            (
                Hunk::AddFile {
                    path: path.to_owned(),
                    contents: String::new(),
                },
                Mode::AddFile,
            )
        } else if let Some(path) = trimmed.strip_prefix(DELETE_FILE_MARKER) {
            (
                Hunk::DeleteFile {
                    path: path.to_owned(),
                },
                Mode::DeleteFile,
            )
        } else if let Some(path) = trimmed.strip_prefix(UPDATE_FILE_MARKER) {
            (
                Hunk::UpdateFile {
                    path: path.to_owned(),
                    move_path: None,
                    chunks: Vec::new(),
                },
                Mode::UpdateFile {
                    header_line: line_number,
                },
            )
        } else {
            return Ok(false);
        };
        self.check_update_not_empty(line_number, trimmed)?;
        self.hunks.push(hunk);
        self.mode = mode;
        Ok(true)
    }

    fn line(&mut self, line_number: usize, line: &str) -> ToolResult<()> {
        let trimmed = line.trim();
        match self.mode {
            Mode::Started | Mode::DeleteFile => {
                if self.header(line_number, trimmed)? {
                    return Ok(());
                }
                Err(invalid_hunk(
                    line_number,
                    &format!("'{trimmed}' {BAD_HEADER}"),
                ))
            }
            Mode::AddFile => {
                if self.header(line_number, trimmed)? {
                    return Ok(());
                }
                if let (Some(added), Some(Hunk::AddFile { contents, .. })) =
                    (line.strip_prefix('+'), self.hunks.last_mut())
                {
                    contents.push_str(added);
                    contents.push('\n');
                    return Ok(());
                }
                Err(invalid_hunk(
                    line_number,
                    &format!("'{trimmed}' {BAD_HEADER}"),
                ))
            }
            Mode::UpdateFile { .. } => self.update_line(line_number, line),
        }
    }

    fn update_line(&mut self, line_number: usize, line: &str) -> ToolResult<()> {
        let update_line = line.trim_end();
        if self.header(line_number, update_line)? {
            return Ok(());
        }
        let Some(Hunk::UpdateFile {
            move_path, chunks, ..
        }) = self.hunks.last_mut()
        else {
            return Err(invalid_hunk(
                line_number,
                "an update line outside an update",
            ));
        };
        let is_context_marker = update_line == EMPTY_CHANGE_CONTEXT_MARKER
            || update_line.starts_with(CHANGE_CONTEXT_MARKER);
        if chunks.last().is_some_and(|chunk| chunk.is_end_of_file) {
            if update_line.is_empty() {
                return Ok(());
            }
            if !is_context_marker {
                return Err(invalid_hunk(
                    line_number,
                    &format!(
                        "Expected update hunk to start with a @@ context marker, got: '{line}'"
                    ),
                ));
            }
        }
        if chunks.is_empty()
            && move_path.is_none()
            && let Some(to) = update_line.strip_prefix(MOVE_TO_MARKER)
        {
            *move_path = Some(to.to_owned());
            return Ok(());
        }
        if is_context_marker && chunks.last().is_some_and(UpdateFileChunk::is_empty) {
            return Err(invalid_hunk(
                line_number,
                &format!("Unexpected line found in update hunk: '{line}'. {BAD_UPDATE_LINE}"),
            ));
        }
        if update_line == EMPTY_CHANGE_CONTEXT_MARKER {
            chunks.push(UpdateFileChunk::default());
            return Ok(());
        }
        if let Some(context) = update_line.strip_prefix(CHANGE_CONTEXT_MARKER) {
            chunks.push(UpdateFileChunk {
                change_context: Some(context.to_owned()),
                ..UpdateFileChunk::default()
            });
            return Ok(());
        }
        if update_line == EOF_MARKER {
            match chunks.last_mut() {
                Some(chunk) if !chunk.is_empty() => {
                    chunk.is_end_of_file = true;
                    return Ok(());
                }
                _ => {
                    return Err(invalid_hunk(
                        line_number,
                        "Update hunk does not contain any lines",
                    ));
                }
            }
        }
        let (kind, rest) = match line.chars().next() {
            // A bare empty line is an empty context line.
            None => (' ', ""),
            Some(kind @ (' ' | '+' | '-')) => (kind, &line[1..]),
            Some(_) => {
                let message = if chunks.last().is_some_and(|chunk| !chunk.is_empty()) {
                    format!("Expected update hunk to start with a @@ context marker, got: '{line}'")
                } else {
                    format!("Unexpected line found in update hunk: '{line}'. {BAD_UPDATE_LINE}")
                };
                return Err(invalid_hunk(line_number, &message));
            }
        };
        if chunks.is_empty() {
            chunks.push(UpdateFileChunk::default());
        }
        let Some(chunk) = chunks.last_mut() else {
            return Err(invalid_hunk(line_number, "no chunk"));
        };
        match kind {
            ' ' => chunk.push_context_line(rest.to_owned()),
            '+' => chunk.new_lines.push(rest.to_owned()),
            _ => chunk.old_lines.push(rest.to_owned()),
        }
        Ok(())
    }

    fn end(&self, line_number: usize) -> ToolResult<()> {
        self.check_update_not_empty(line_number, END_PATCH_MARKER)
    }
}

#[cfg(test)]
mod tests {
    use super::{Hunk, UpdateFileChunk, is_envelope, parse_patch};

    fn message(text: &str) -> String {
        parse_patch(text).expect_err(text).message().to_owned()
    }

    #[test]
    fn boundaries_are_checked() {
        assert!(message("bad").contains("The first line of the patch must be '*** Begin Patch'"));
        assert!(
            message("*** Begin Patch\nbad")
                .contains("The last line of the patch must be '*** End Patch'")
        );
        assert_eq!(
            parse_patch("*** Begin Patch \n*** Add File: foo\n+hi\n *** End Patch").expect("ok"),
            vec![Hunk::AddFile {
                path: "foo".into(),
                contents: "hi\n".into()
            }]
        );
        assert!(
            message("*** Begin Patch\n*** Update File: test.py\n*** End Patch")
                .contains("invalid hunk at line 2: Update file hunk for path 'test.py' is empty")
        );
        assert_eq!(
            parse_patch("*** Begin Patch\n*** End Patch").expect("empty"),
            Vec::new()
        );
    }

    #[test]
    fn add_delete_update_and_move_parse() {
        let text = "*** Begin Patch\n*** Add File: path/add.py\n+abc\n+def\n*** Delete File: path/delete.py\n*** Update File: path/update.py\n*** Move to: path/update2.py\n@@ def f():\n-    pass\n+    return 123\n*** End Patch";
        assert_eq!(
            parse_patch(text).expect("parse"),
            vec![
                Hunk::AddFile {
                    path: "path/add.py".into(),
                    contents: "abc\ndef\n".into()
                },
                Hunk::DeleteFile {
                    path: "path/delete.py".into()
                },
                Hunk::UpdateFile {
                    path: "path/update.py".into(),
                    move_path: Some("path/update2.py".into()),
                    chunks: vec![UpdateFileChunk {
                        change_context: Some("def f():".into()),
                        old_lines: vec!["    pass".into()],
                        new_lines: vec!["    return 123".into()],
                        context_line_indices: vec![],
                        is_end_of_file: false
                    }]
                }
            ]
        );
        // An update followed by another hunk; a first chunk without `@@`.
        let text = "*** Begin Patch\n*** Update File: file.py\n@@\n+line\n*** Add File: other.py\n+content\n*** Update File: file2.py\n import foo\n+bar\n*** End Patch";
        let hunks = parse_patch(text).expect("parse");
        assert_eq!(hunks.len(), 3);
        assert_eq!(
            hunks[2],
            Hunk::UpdateFile {
                path: "file2.py".into(),
                move_path: None,
                chunks: vec![UpdateFileChunk {
                    change_context: None,
                    old_lines: vec!["import foo".into()],
                    new_lines: vec!["import foo".into(), "bar".into()],
                    context_line_indices: vec![(0, 0)],
                    is_end_of_file: false,
                }],
            }
        );
    }

    #[test]
    fn end_of_file_marker_and_heredoc() {
        let text = "*** Begin Patch\n*** Update File: file.txt\n@@\n+quux\n*** End of File\n\n*** End Patch";
        let Hunk::UpdateFile { chunks, .. } = &parse_patch(text).expect("parse")[0] else {
            panic!("update");
        };
        assert!(chunks[0].is_end_of_file);
        let body = "*** Begin Patch\n*** Update File: file2.py\n import foo\n+bar\n*** End Patch";
        for wrapped in [
            format!("<<EOF\n{body}\nEOF\n"),
            format!("<<'EOF'\n{body}\nEOF\n"),
            format!("<<\"EOF\"\n{body}\nEOF\n"),
        ] {
            assert!(is_envelope(&wrapped));
            assert_eq!(parse_patch(&wrapped).expect("heredoc").len(), 1);
        }
        assert!(parse_patch(&format!("<<\"EOF'\n{body}\nEOF\n")).is_err());
        assert!(
            message("<<EOF\n*** Begin Patch\n*** Update File: file2.py\nEOF\n")
                .contains("The last line of the patch must be '*** End Patch'")
        );
        assert!(!is_envelope("--- a/x\n+++ b/x\n"));
        assert!(is_envelope("\n  *** Begin Patch\n*** End Patch"));
    }

    #[test]
    fn environment_id_is_read_once() {
        assert_eq!(
            parse_patch(
                "*** Begin Patch\n*** Environment ID: remote\n*** Add File: hello.txt\n+hello\n*** End Patch"
            )
            .expect("parse")
            .len(),
            1
        );
        assert!(
            message("*** Begin Patch\n*** Environment ID:   \n*** Add File: hello.txt\n+hello\n*** End Patch")
                .contains("environment_id cannot be empty")
        );
    }

    #[test]
    fn bad_update_lines_are_refused() {
        assert!(
            message("*** Begin Patch\n*** Update File: a\n@@\n*** End Patch")
                .contains("Update hunk does not contain any lines")
        );
        assert!(
            message("*** Begin Patch\n*** Update File: a\n-x\n+y\nzzz\n*** End Patch")
                .contains("Expected update hunk to start with a @@ context marker")
        );
        assert!(
            message("*** Begin Patch\n*** Frobnicate: a\n*** End Patch")
                .contains("is not a valid hunk header")
        );
    }
}
