//! A C++ file's text as the Python parser sees it, and its slicing quirk.
//!
//! The Python parser reads a file with `open(path, 'r', encoding='utf-8',
//! errors='ignore')`: LENIENT decoding — every invalid byte sequence is
//! dropped, never an error — and text mode's universal newlines (`\r\n` and
//! a lone `\r` become `\n`). A BOM is kept as U+FEFF (the codec is `utf-8`,
//! not `utf-8-sig`). It parses `bytes(content, 'utf8')`, so tree-sitter's
//! byte offsets and points are those of that normalised text, and ranges
//! (`start_point` / `end_point`: row and BYTE column) are exact.
//!
//! Its `_get_node_text` however slices the decoded `str` — indexed by CODE
//! POINT — with the node's BYTE offsets. For an ASCII file the two agree;
//! after a multi-byte character every node's text is shifted right (and a
//! slice past the end is clamped, possibly to `""`). [`Source::text`]
//! reproduces that. The few places that use `node.text.decode()` instead
//! (the module-level `_collect_qualified_parts_from_node`) get the exact
//! text from [`Source::raw`].

use crate::model::{Position, Range};
use elitea_engine_core::pystr::{Errors, decode_text};
use tree_sitter::Node;

/// The decoded, newline-normalised text and its code-point index.
pub(super) struct Source {
    text: String,
    /// Byte offset of every code point, when the text is not ASCII.
    char_starts: Option<Vec<usize>>,
}

impl Source {
    /// Decode `bytes` as Python's lenient text-mode read does.
    pub(super) fn decode(bytes: &[u8]) -> Self {
        Self::from_text(decode_text(bytes, Errors::Ignore))
    }

    /// Wrap text that is already decoded and newline-normalised.
    pub(super) fn from_text(text: impl Into<String>) -> Self {
        let text = text.into();
        let char_starts = (!text.is_ascii()).then(|| text.char_indices().map(|(i, _)| i).collect());
        Self { text, char_starts }
    }

    /// The bytes tree-sitter parses.
    pub(super) fn bytes(&self) -> &[u8] {
        self.text.as_bytes()
    }

    /// `_get_node_text(node)`: `content[start_byte:end_byte]`, the quirk
    /// included.
    pub(super) fn text<'s>(&'s self, node: Node<'_>) -> &'s str {
        self.slice(node.start_byte(), node.end_byte())
    }

    /// `node.text.decode()`: the node's exact text.
    pub(super) fn raw<'s>(&'s self, node: Node<'_>) -> &'s str {
        self.text
            .get(node.start_byte()..node.end_byte())
            .unwrap_or("")
    }

    /// Python's `str[start:end]` with code-point indexes.
    fn slice(&self, start: usize, end: usize) -> &str {
        match &self.char_starts {
            None => {
                let len = self.text.len();
                let (start, end) = (start.min(len), end.min(len));
                if start >= end {
                    ""
                } else {
                    &self.text[start..end]
                }
            }
            Some(starts) => {
                let count = starts.len();
                let (start, end) = (start.min(count), end.min(count));
                if start >= end {
                    return "";
                }
                let byte_end = starts.get(end).copied().unwrap_or(self.text.len());
                self.text.get(starts[start]..byte_end).unwrap_or("")
            }
        }
    }

    /// `_node_to_range(node)`: 1-based rows, byte columns.
    pub(super) fn range(node: Node<'_>) -> Range {
        let (start, end) = (node.start_position(), node.end_position());
        Range {
            start: Position {
                line: u32::try_from(start.row + 1).unwrap_or(u32::MAX),
                column: u32::try_from(start.column).unwrap_or(u32::MAX),
            },
            end: Position {
                line: u32::try_from(end.row + 1).unwrap_or(u32::MAX),
                column: u32::try_from(end.column).unwrap_or(u32::MAX),
            },
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn invalid_bytes_are_dropped_and_newlines_normalised() {
        let source = Source::decode(b"a\xffb\r\nc\rd\xe4\xb8");
        assert_eq!(source.bytes(), b"ab\nc\nd");
        let bom = Source::decode(b"\xef\xbb\xbfint x;");
        assert_eq!(bom.bytes(), "\u{feff}int x;".as_bytes());
    }

    #[test]
    fn slices_index_code_points_with_byte_offsets() {
        // "é" is 2 bytes: bytes 3..6 of "é x yz" are "x y" in the
        // encoding, but Python slices CODE POINTS 3..6, " yz".
        let source = Source::from_text("é x yz");
        assert_eq!(source.slice(3, 6), " yz");
        let source = Source::from_text("éa x yz");
        assert_eq!(source.slice(3, 6), "x y");
        assert_eq!(source.slice(4, 99), " yz");
        assert_eq!(source.slice(40, 99), "");
        let ascii = Source::from_text("abc");
        assert_eq!(ascii.slice(1, 9), "bc");
    }
}
