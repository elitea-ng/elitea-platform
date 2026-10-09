//! A Java file's text as the Python parser sees it, and its range quirk.
//!
//! The Python parser reads a file with `open(path, 'r', encoding='utf-8')`:
//! STRICT decoding — one invalid byte makes the whole file a parse error
//! with Python's `UnicodeDecodeError` text — and text mode's universal
//! newlines (`\r\n` and a lone `\r` become `\n`). It parses
//! `bytes(content, 'utf8')`, so tree-sitter's byte offsets are offsets into
//! that normalised text, and node text is correct UTF-8.
//!
//! Its `_byte_to_line_col` however counts newlines in `content[:offset]` —
//! the decoded `str`, indexed by CODE POINT — with a BYTE offset, and the
//! column is `offset - (index of the last newline before it + 1)`. For an
//! ASCII file both agree; after any multi-byte character the line can run
//! ahead and the column is a byte/code-point mix. [`Source::position`]
//! reproduces that exactly.
//!
//! The C# parser (`csharp_visitor_parser.py`) reads and indexes its files
//! the same way, so it shares this type.

use crate::model::{Position, Range};
use tree_sitter::Node;

/// The decoded, newline-normalised text and its newline index.
pub(crate) struct Source {
    text: String,
    /// Code-point index of every `\n`, ascending.
    newlines: Vec<usize>,
    /// The number of code points.
    chars: usize,
}

impl Source {
    /// Decode `bytes` as Python's strict text-mode read does; the error is
    /// `str(UnicodeDecodeError)`.
    pub(crate) fn decode(bytes: &[u8]) -> Result<Self, String> {
        match std::str::from_utf8(bytes) {
            Ok(text) => Ok(Self::from_text(&normalise_newlines(text))),
            Err(error) => Err(decode_error(bytes, &error)),
        }
    }

    /// Wrap text that is already decoded and newline-normalised.
    pub(crate) fn from_text(text: &str) -> Self {
        let mut newlines = Vec::new();
        let mut chars = 0;
        for (index, c) in text.chars().enumerate() {
            if c == '\n' {
                newlines.push(index);
            }
            chars = index + 1;
        }
        Self {
            text: text.to_owned(),
            newlines,
            chars,
        }
    }

    /// The bytes tree-sitter parses.
    pub(crate) fn bytes(&self) -> &[u8] {
        self.text.as_bytes()
    }

    /// `node.text.decode('utf-8')`.
    pub(crate) fn text<'s>(&'s self, node: Node<'_>) -> &'s str {
        self.text
            .get(node.start_byte()..node.end_byte())
            .unwrap_or("")
    }

    /// `_byte_to_line_col(offset)`, quirk included.
    pub(crate) fn position(&self, offset: usize) -> Position {
        let end = offset.min(self.chars);
        let before = self.newlines.partition_point(|&n| n < end);
        let line_start = if before == 0 {
            0
        } else {
            self.newlines[before - 1] + 1
        };
        Position {
            line: u32::try_from(before + 1).unwrap_or(u32::MAX),
            column: u32::try_from(offset.saturating_sub(line_start)).unwrap_or(u32::MAX),
        }
    }

    /// `_create_range(node)`.
    pub(crate) fn range(&self, node: Node<'_>) -> Range {
        Range {
            start: self.position(node.start_byte()),
            end: self.position(node.end_byte()),
        }
    }
}

/// Universal newlines: `\r\n` and a lone `\r` both become `\n`.
fn normalise_newlines(text: &str) -> String {
    if text.contains('\r') {
        text.replace("\r\n", "\n").replace('\r', "\n")
    } else {
        text.to_owned()
    }
}

/// Python's `UnicodeDecodeError` text for the first invalid sequence.
///
/// Rust's `error_len` is the maximal invalid subpart, the same unit Python
/// reports; a lone byte that can never start a sequence is an "invalid
/// start byte", any other a lead byte whose continuation is wrong, and a
/// truncated sequence at the end "unexpected end of data".
fn decode_error(bytes: &[u8], error: &std::str::Utf8Error) -> String {
    let start = error.valid_up_to();
    let (end, reason) = match error.error_len() {
        None => (bytes.len(), "unexpected end of data"),
        Some(1) if matches!(bytes[start], 0x80..=0xC1 | 0xF5..=0xFF) => {
            (start + 1, "invalid start byte")
        }
        Some(len) => (start + len, "invalid continuation byte"),
    };
    if end - start == 1 {
        format!(
            "'utf-8' codec can't decode byte 0x{:02x} in position {start}: {reason}",
            bytes[start]
        )
    } else {
        format!(
            "'utf-8' codec can't decode bytes in position {start}-{}: {reason}",
            end - 1
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn err(bytes: &[u8]) -> String {
        Source::decode(bytes).err().unwrap_or_default()
    }

    #[test]
    fn decode_errors_read_as_cpython_writes_them() {
        assert_eq!(
            err(b"ab\xff"),
            "'utf-8' codec can't decode byte 0xff in position 2: invalid start byte"
        );
        assert_eq!(
            err(b"\xc0\x80"),
            "'utf-8' codec can't decode byte 0xc0 in position 0: invalid start byte"
        );
        assert_eq!(
            err(b"\xe4\xb8x"),
            "'utf-8' codec can't decode bytes in position 0-1: invalid continuation byte"
        );
        assert_eq!(
            err(b"\xed\xa0\x80"),
            "'utf-8' codec can't decode byte 0xed in position 0: invalid continuation byte"
        );
        assert_eq!(
            err(b"abc\xe4\xb8"),
            "'utf-8' codec can't decode bytes in position 3-4: unexpected end of data"
        );
    }

    #[test]
    fn newlines_are_normalised() {
        let source = Source::decode(b"a\r\nb\rc").unwrap_or_else(|_| Source::from_text(""));
        assert_eq!(source.bytes(), b"a\nb\nc");
    }

    #[test]
    fn positions_mix_byte_offsets_with_code_point_indexes() {
        // "é" is 2 bytes. Byte 4 is the newline ending line 1; Python
        // slices 4 CODE POINTS ("éé\nx"[..4] = "éé\nx"), counts one newline
        // and reports line 2, column 4 - 3 = 1.
        let source = Source::from_text("éé\nx");
        assert_eq!(source.position(4), Position { line: 2, column: 1 });
        assert_eq!(source.position(1), Position { line: 1, column: 1 });
        let ascii = Source::from_text("ab\ncd");
        assert_eq!(ascii.position(4), Position { line: 2, column: 1 });
        // Past the end: the slice clamps, the column does not.
        assert_eq!(ascii.position(9), Position { line: 2, column: 6 });
    }
}
