//! Shared plumbing of the regex-transcribed parsers ([`crate::kotlin`],
//! [`crate::swift`]).
//!
//! Those Python parsers are not tree-sitter visitors: they run `re`
//! patterns over the whole file text and build symbols from the matches.
//! This module keeps the parts of that machinery both share, with the
//! Python semantics that move positions:
//!
//! * **Reading.** `open(path, encoding='utf-8', errors='ignore')`: invalid
//!   UTF-8 is DROPPED (not replaced), a BOM is kept as a character, and
//!   universal newlines turn `\r\n` and a lone `\r` into `\n`.
//! * **Positions.** `_make_range`: the line is one more than the newlines
//!   before the offset, and the column counts CHARACTERS (Python string
//!   indexes) since the last newline, not bytes. Every end column is 0.
//! * **Truthiness.** An optional regex group is `None` when it did not take
//!   part; the parsers test it with `if group:`, so an empty capture counts
//!   as absent as well ([`group`]).
//! * **Whitespace.** Python's `str.strip()` / `str.split()` are
//!   [`str::trim`] / [`str::split_whitespace`]. They differ only on the
//!   ASCII separators `\x1c`–`\x1f`, which Python counts as whitespace.

use crate::limits;
use crate::model::{ParseResult, Range, Relationship, RelationshipType, Scope, Symbol, SymbolType};
use regex::{Captures, Regex};
use serde_json::{Map, Value};

/// Compile one transcribed pattern. Every pattern is a literal checked by
/// the modules' tests, so `None` never happens in practice; the parsers
/// fail the file instead of panicking if it ever did.
pub(crate) fn compile(pattern: &str) -> Option<Regex> {
    Regex::new(pattern).ok()
}

/// The error of a file parsed while a pattern did not compile.
pub(crate) const PATTERN_ERROR: &str = "internal error: a parser pattern did not compile";

/// Read a file as the Python parsers' `open(..., errors='ignore')` does.
pub(crate) fn read_text(path: &str) -> Result<String, String> {
    let bytes = std::fs::read(path).map_err(|error| python_os_error(&error, path))?;
    let mut text = String::with_capacity(bytes.len());
    for chunk in bytes.utf8_chunks() {
        text.push_str(chunk.valid());
    }
    if text.contains('\r') {
        text = text.replace("\r\n", "\n").replace('\r', "\n");
    }
    Ok(text)
}

/// `str(OSError)`: `[Errno 2] No such file or directory: 'path'`.
fn python_os_error(error: &std::io::Error, path: &str) -> String {
    let Some(code) = error.raw_os_error() else {
        return error.to_string();
    };
    let full = error.to_string();
    let suffix = format!(" (os error {code})");
    let message = full.strip_suffix(&suffix).unwrap_or(&full);
    // `repr(str)`: single quotes unless the text holds one and no double.
    let quoted = if path.contains('\'') && !path.contains('"') {
        format!("\"{path}\"")
    } else {
        format!("'{}'", path.replace('\\', "\\\\").replace('\'', "\\'"))
    };
    format!("[Errno {code}] {message}: {quoted}")
}

/// Parse one file: read it, run `parse` under the output budget, and turn
/// a read error or an exhausted budget into a result with `errors` set.
pub(crate) fn parse_path(
    path: &str,
    language: &str,
    parse: impl FnOnce(&str, &str) -> ParseResult,
) -> ParseResult {
    let failed = |error: String| {
        let mut result = ParseResult::new(path, language);
        result.errors.push(error);
        result
    };
    match read_text(path) {
        Ok(text) => limits::with_output_budget(|| parse(path, &text))
            .unwrap_or_else(|error| failed(error.to_owned())),
        Err(error) => failed(error),
    }
}

/// A failed result for a pattern that did not compile.
pub(crate) fn pattern_failure(path: &str, language: &str) -> ParseResult {
    let mut result = ParseResult::new(path, language);
    result.errors.push(PATTERN_ERROR.to_owned());
    result
}

/// `Path(file_path).stem`.
pub(crate) fn file_stem(path: &str) -> String {
    std::path::Path::new(path)
        .file_stem()
        .map(|s| s.to_string_lossy().into_owned())
        .unwrap_or_default()
}

/// A regex group as the parsers test it: `None` when it did not take part
/// or captured nothing.
pub(crate) fn group<'h>(captures: &Captures<'h>, index: usize) -> Option<&'h str> {
    captures
        .get(index)
        .map(|m| m.as_str())
        .filter(|s| !s.is_empty())
}

/// The whole match's start and end byte offsets.
pub(crate) fn span(captures: &Captures<'_>) -> (usize, usize) {
    captures.get(0).map_or((0, 0), |m| (m.start(), m.end()))
}

/// `str.split()` as a JSON list.
pub(crate) fn word_list(text: &str) -> Value {
    Value::Array(
        text.split_whitespace()
            .map(|w| Value::String(w.to_owned()))
            .collect(),
    )
}

/// Insert `key: value` into a metadata map.
pub(crate) fn put(map: &mut Map<String, Value>, key: &str, value: impl Into<Value>) {
    map.insert(key.to_owned(), value.into());
}

/// One file's text with its newline offsets, for Python's positions.
pub(crate) struct Source<'a> {
    pub(crate) text: &'a str,
    newlines: Vec<usize>,
}

fn to_u32(value: usize) -> u32 {
    u32::try_from(value).unwrap_or(u32::MAX)
}

impl<'a> Source<'a> {
    pub(crate) fn new(text: &'a str) -> Self {
        Self {
            text,
            newlines: text.match_indices('\n').map(|(i, _)| i).collect(),
        }
    }

    /// Newlines before byte `offset`.
    fn newlines_before(&self, offset: usize) -> usize {
        self.newlines.partition_point(|&p| p < offset)
    }

    /// `content[:offset].count('\n') + 1`.
    pub(crate) fn line(&self, offset: usize) -> u32 {
        to_u32(self.newlines_before(offset) + 1)
    }

    /// Characters since the last newline before `offset`.
    pub(crate) fn column(&self, offset: usize) -> u32 {
        let before = self.newlines_before(offset);
        let line_start = before
            .checked_sub(1)
            .and_then(|i| self.newlines.get(i))
            .map_or(0, |&p| p + 1);
        let column = self
            .text
            .get(line_start..offset)
            .map_or(0, |s| s.chars().count());
        to_u32(column)
    }

    /// `_make_range(content, offset, end_line)`.
    pub(crate) fn range(&self, offset: usize, end_line: u32) -> Range {
        Range::new(self.line(offset), self.column(offset), end_line, 0)
    }

    /// `_find_block_end`: the line of the `}` that closes the first `{` at
    /// or after `start` (a `}` before any `{` counts down too, as in
    /// Python); the line of `start` when none closes.
    pub(crate) fn block_end(&self, start: usize) -> u32 {
        let mut depth: i64 = 0;
        let mut in_block = false;
        let tail = self.text.as_bytes().get(start..).unwrap_or_default();
        for (index, byte) in tail.iter().enumerate() {
            match byte {
                b'{' => {
                    depth += 1;
                    in_block = true;
                }
                b'}' => {
                    depth -= 1;
                    if in_block && depth == 0 {
                        return self.line(start + index);
                    }
                }
                _ => {}
            }
        }
        self.line(start)
    }

    /// A global-scope symbol starting at `offset`, its metadata `metadata`
    /// plus `is_exported` (always `false`: neither Python parser sets it).
    pub(crate) fn symbol(
        &self,
        file_path: &str,
        name: &str,
        symbol_type: SymbolType,
        offset: usize,
        end_line: u32,
        mut metadata: Map<String, Value>,
    ) -> Symbol {
        let mut symbol = Symbol::new(
            name,
            symbol_type,
            Scope::Global,
            self.range(offset, end_line),
            file_path,
        );
        put(&mut metadata, "is_exported", false);
        symbol.metadata = metadata;
        symbol
    }

    /// `_make_relationship`: the range is the offset's line, end column 0;
    /// the Python `metadata` is the annotations.
    pub(crate) fn relationship(
        &self,
        file_path: &str,
        source: &str,
        target: &str,
        relationship_type: RelationshipType,
        offset: usize,
        annotations: Map<String, Value>,
    ) -> Relationship {
        let mut relationship = Relationship::new(source, target, relationship_type, file_path);
        relationship.source_range = Some(self.range(offset, self.line(offset)));
        relationship.annotations = annotations;
        relationship
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn positions_count_characters_and_lines_as_python() {
        let source = Source::new("ab\nкод x\n");
        let offset = "ab\nкод ".len();
        assert_eq!(source.line(offset), 2);
        assert_eq!(source.column(offset), 4);
        assert_eq!(source.line(0), 1);
        assert_eq!(source.column(1), 1);
    }

    #[test]
    fn block_end_follows_python_brace_counting() {
        let source = Source::new("a {\n b {\n }\n}\nz");
        assert_eq!(source.block_end(0), 4);
        let unclosed = Source::new("x\n{ {\n}");
        assert_eq!(unclosed.block_end(2), 2);
    }

    #[test]
    fn reading_drops_invalid_bytes_and_translates_newlines() {
        let dir = std::env::temp_dir().join(format!("regex-support-{}", std::process::id()));
        let _ = std::fs::create_dir_all(&dir);
        let path = dir.join("a.kt");
        let _ = std::fs::write(&path, b"a\xff\r\nb\rc");
        let path = path.to_string_lossy().into_owned();
        assert_eq!(read_text(&path).ok().as_deref(), Some("a\nb\nc"));
        let _ = std::fs::remove_dir_all(&dir);
        let missing = read_text("/nonexistent/it's.kt");
        assert_eq!(
            missing.err().as_deref(),
            Some("[Errno 2] No such file or directory: \"/nonexistent/it's.kt\"")
        );
    }
}
