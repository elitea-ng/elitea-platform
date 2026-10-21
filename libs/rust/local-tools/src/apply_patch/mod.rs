// Portions adapted from OpenAI Codex (https://github.com/openai/codex), codex-rs/apply-patch/src/file_update.rs and codex-rs/apply-patch/src/text_file.rs @ be9060f. Apache License 2.0. Copyright 2025 OpenAI.
// Changes: pure functions over text (no file system: this crate's
// Workspace reads and writes); each chunk reports the seek pass that
// located it; a fuzzy location must be unique; the new text's changed
// line ranges are returned for the result's snippet.

//! The Codex `*** Begin Patch` envelope, as GPT-5 and codex-family models
//! write it: [`parser`] reads it, [`seek_sequence`] locates each chunk with
//! an ordered fuzzy ladder, and [`derive_new_contents`] builds the new
//! text. `crate::files::plan_patch` turns the hunks into the same atomic,
//! ledger-checked plan a unified diff becomes.

pub mod parser;
pub mod seek_sequence;

use crate::error::{ErrorCode, ToolError, ToolResult};
use parser::UpdateFileChunk;
pub use parser::{Hunk, is_envelope, parse_patch};
pub use seek_sequence::Pass;
use seek_sequence::{SeekError, seek_sequence};

/// `(start, old_len, new_lines)` in the original's lines.
type Replacement = (usize, usize, Vec<String>);

#[derive(Clone, Copy)]
enum LineEnding {
    Lf,
    CrLf,
    Cr,
}

impl LineEnding {
    fn as_str(self) -> &'static str {
        match self {
            Self::Lf => "\n",
            Self::CrLf => "\r\n",
            Self::Cr => "\r",
        }
    }
}

struct SourceLine {
    text: String,
    ending: Option<LineEnding>,
}

/// A file as lines that keep their own endings.
struct SourceFile {
    lines: Vec<SourceLine>,
    preferred_ending: LineEnding,
}

impl SourceFile {
    /// Split into lines, keeping each ending. The first ending is the one
    /// inserted lines take; a file without one gets LF.
    fn parse(contents: &str) -> Self {
        let bytes = contents.as_bytes();
        let mut lines = Vec::new();
        let mut preferred_ending = None;
        let mut line_start = 0;
        let mut cursor = 0;
        while cursor < bytes.len() {
            let (ending, ending_len) = match bytes[cursor] {
                b'\r' if bytes.get(cursor + 1) == Some(&b'\n') => (LineEnding::CrLf, 2),
                b'\r' => (LineEnding::Cr, 1),
                b'\n' => (LineEnding::Lf, 1),
                _ => {
                    cursor += 1;
                    continue;
                }
            };
            preferred_ending.get_or_insert(ending);
            lines.push(SourceLine {
                text: contents[line_start..cursor].to_owned(),
                ending: Some(ending),
            });
            cursor += ending_len;
            line_start = cursor;
        }
        if line_start < contents.len() {
            lines.push(SourceLine {
                text: contents[line_start..].to_owned(),
                ending: None,
            });
        }
        Self {
            lines,
            preferred_ending: preferred_ending.unwrap_or(LineEnding::Lf),
        }
    }

    fn line_texts(&self) -> Vec<String> {
        self.lines.iter().map(|line| line.text.clone()).collect()
    }

    /// Apply source-ordered, non-overlapping replacements. Unchanged lines
    /// keep their endings; inserted lines take the preferred one; every
    /// line ends with one (as Codex's updates always have).
    fn apply_replacements(&mut self, replacements: &[Replacement]) {
        let mut source_lines = std::mem::take(&mut self.lines).into_iter();
        let mut new_lines = Vec::new();
        let mut source_index = 0;
        for (start, old_len, segment) in replacements {
            new_lines.extend(
                source_lines
                    .by_ref()
                    .take(start.saturating_sub(source_index)),
            );
            source_lines.by_ref().take(*old_len).for_each(drop);
            new_lines.extend(segment.iter().map(|text| SourceLine {
                text: text.clone(),
                ending: Some(self.preferred_ending),
            }));
            source_index = start + old_len;
        }
        new_lines.extend(source_lines);
        for line in &mut new_lines {
            line.ending.get_or_insert(self.preferred_ending);
        }
        self.lines = new_lines;
    }

    fn into_contents(self) -> String {
        let mut contents = String::new();
        for line in self.lines {
            contents.push_str(&line.text);
            if let Some(ending) = line.ending {
                contents.push_str(ending.as_str());
            }
        }
        contents
    }
}

/// An update's result.
#[derive(Debug, PartialEq, Eq)]
pub struct Derived {
    pub content: String,
    /// How each chunk's lines were located (an anchor-only or pure
    /// addition chunk reports how its anchor matched, or `Exact`).
    pub matches: Vec<Pass>,
    /// The new text's changed lines: 0-based `[start, end)` ranges (empty
    /// for a deletion, at the line after it).
    pub changed: Vec<(usize, usize)>,
}

fn located(path: &str, what: &str, error: SeekError) -> ToolError {
    match error {
        SeekError::NotFound => ToolError::new(
            ErrorCode::Conflict,
            format!("failed to find {what} in `{path}`"),
        ),
        SeekError::Ambiguous { pass, candidates } => ToolError::new(
            ErrorCode::Conflict,
            format!(
                "{what} in `{path}` matches only after normalisation ({}), at lines {}; add context to make it unique",
                pass.as_str(),
                candidates
                    .iter()
                    .map(|at| (at + 1).to_string())
                    .collect::<Vec<_>>()
                    .join(", ")
            ),
        ),
    }
}

/// The replacements that turn `original_lines` into the new lines, and how
/// each chunk matched.
fn compute_replacements(
    original_lines: &[String],
    path: &str,
    chunks: &[UpdateFileChunk],
) -> ToolResult<(Vec<Replacement>, Vec<Pass>)> {
    let mut replacements: Vec<Replacement> = Vec::new();
    let mut matches = Vec::new();
    let mut line_index = 0usize;
    for chunk in chunks {
        let mut chunk_pass = Pass::Exact;
        // The `@@` anchor narrows where the chunk's lines are looked for.
        if let Some(context) = &chunk.change_context {
            let (at, pass) = seek_sequence(
                original_lines,
                std::slice::from_ref(context),
                line_index,
                false,
            )
            .map_err(|error| located(path, &format!("context '{context}'"), error))?;
            line_index = at + 1;
            chunk_pass = pass;
        }
        if chunk.old_lines.is_empty() {
            // Codex's rule: a chunk with no old lines appends at the end.
            replacements.push((original_lines.len(), 0, chunk.new_lines.clone()));
            matches.push(chunk_pass);
            continue;
        }
        // A trailing empty old line stands for the file's final newline,
        // which the split lines do not hold: retry without it.
        let mut pattern: &[String] = &chunk.old_lines;
        let mut new_slice: &[String] = &chunk.new_lines;
        let mut found = seek_sequence(original_lines, pattern, line_index, chunk.is_end_of_file);
        if matches!(found, Err(SeekError::NotFound)) && pattern.last().is_some_and(String::is_empty)
        {
            pattern = &pattern[..pattern.len() - 1];
            if new_slice.last().is_some_and(String::is_empty) {
                new_slice = &new_slice[..new_slice.len() - 1];
            }
            found = seek_sequence(original_lines, pattern, line_index, chunk.is_end_of_file);
        }
        let (start, pass) = found.map_err(|error| {
            located(
                path,
                &format!("the expected lines\n{}\n", chunk.old_lines.join("\n")),
                error,
            )
        })?;
        // Context lines keep the original's text and ending: replace only
        // the runs between them.
        let (mut old_start, mut new_start) = (0, 0);
        for &(old_context, new_context) in &chunk.context_line_indices {
            if old_context >= pattern.len() || new_context >= new_slice.len() {
                break;
            }
            if old_start != old_context || new_start != new_context {
                replacements.push((
                    start + old_start,
                    old_context - old_start,
                    new_slice[new_start..new_context].to_vec(),
                ));
            }
            old_start = old_context + 1;
            new_start = new_context + 1;
        }
        if old_start != pattern.len() || new_start != new_slice.len() {
            replacements.push((
                start + old_start,
                pattern.len() - old_start,
                new_slice[new_start..].to_vec(),
            ));
        }
        line_index = start + pattern.len();
        matches.push(if pass == Pass::Exact {
            chunk_pass
        } else {
            pass
        });
    }
    replacements.sort_by_key(|(index, _, _)| *index);
    Ok((replacements, matches))
}

/// The new text of `original` after an update's chunks.
///
/// # Errors
///
/// [`ErrorCode::Conflict`] when a chunk's anchor or lines are not in the
/// file, or a fuzzy pass finds them more than once.
pub fn derive_new_contents(
    original: &str,
    path: &str,
    chunks: &[UpdateFileChunk],
) -> ToolResult<Derived> {
    let mut source = SourceFile::parse(original);
    let lines = source.line_texts();
    let (replacements, matches) = compute_replacements(&lines, path, chunks)?;
    for pair in replacements.windows(2) {
        if pair[0].0 + pair[0].1 > pair[1].0 {
            return Err(ToolError::new(
                ErrorCode::Conflict,
                format!("two chunks of `{path}` change the same lines"),
            ));
        }
    }
    let mut changed = Vec::new();
    let mut shift: isize = 0;
    for (start, old_len, segment) in &replacements {
        let at = start.saturating_add_signed(shift);
        changed.push((at, at + segment.len()));
        shift += signed(segment.len()) - signed(*old_len);
    }
    source.apply_replacements(&replacements);
    Ok(Derived {
        content: source.into_contents(),
        matches,
        changed,
    })
}

fn signed(value: usize) -> isize {
    isize::try_from(value).unwrap_or(isize::MAX)
}

/// The paths an envelope names, and each `(from, to)` move.
pub type NamedPaths = (Vec<String>, Vec<(String, String)>);

/// The workspace paths an envelope names: every path, and each `(from, to)`
/// move. `None` when `text` is not a well-formed envelope.
#[must_use]
pub fn named_paths(text: &str) -> Option<NamedPaths> {
    let hunks = parse_patch(text).ok()?;
    let mut paths = Vec::new();
    let mut moves = Vec::new();
    for hunk in hunks {
        match hunk {
            Hunk::AddFile { path, .. } | Hunk::DeleteFile { path } => paths.push(path),
            Hunk::UpdateFile {
                path, move_path, ..
            } => {
                paths.push(path.clone());
                if let Some(to) = move_path {
                    paths.push(to.clone());
                    moves.push((path, to));
                }
            }
        }
    }
    Some((paths, moves))
}

#[cfg(test)]
mod tests;
