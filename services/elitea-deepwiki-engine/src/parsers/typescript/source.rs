//! A file's text as the Python parser sees it, and its node-text quirk.
//!
//! The Python parser reads a file with `open(path, encoding='utf-8',
//! errors='ignore')`: invalid UTF-8 is dropped (not replaced), and text
//! mode's universal newlines turn `\r\n` and a lone `\r` into `\n`. It then
//! parses `bytes(content, 'utf8')`, so tree-sitter's byte offsets are offsets
//! into that normalised text.
//!
//! Its `_get_node_text` slices the decoded `str` — indexed by code point —
//! with those BYTE offsets. For an ASCII file the two agree. For a file with
//! any multi-byte character every node after the first such character gets
//! text shifted to the right (and names such as `ubLocationAssign(s`). The
//! graph is built from those names, so [`Source::slice`] reproduces the shift
//! exactly rather than returning the correct text.

/// The decoded text and, for a non-ASCII file, where each code point starts.
pub(super) struct Source {
    text: String,
    /// `starts[i]` is the byte offset of code point `i`; one extra entry holds
    /// the text length. `None` when the text is ASCII (offsets agree).
    starts: Option<Vec<usize>>,
}

impl Source {
    /// Decode `bytes` as Python's text-mode `open(..., errors='ignore')` does.
    pub(super) fn decode(bytes: &[u8]) -> Self {
        let mut decoded = String::with_capacity(bytes.len());
        // `utf8_chunks` splits at maximal invalid subparts, the same units
        // CPython's UTF-8 decoder drops with `errors='ignore'`.
        for chunk in bytes.utf8_chunks() {
            decoded.push_str(chunk.valid());
        }
        Self::from_text(&normalise_newlines(&decoded))
    }

    /// Wrap text that is already decoded and newline-normalised.
    pub(super) fn from_text(text: &str) -> Self {
        let starts = (!text.is_ascii()).then(|| {
            let mut starts: Vec<usize> = text.char_indices().map(|(i, _)| i).collect();
            starts.push(text.len());
            starts
        });
        Self {
            text: text.to_owned(),
            starts,
        }
    }

    /// The bytes tree-sitter parses.
    pub(super) fn bytes(&self) -> &[u8] {
        self.text.as_bytes()
    }

    /// Python's `content[start_byte:end_byte]` on the decoded `str`.
    pub(super) fn slice(&self, start_byte: usize, end_byte: usize) -> &str {
        match &self.starts {
            None => {
                let len = self.text.len();
                let (start, end) = (start_byte.min(len), end_byte.min(len));
                if start >= end {
                    ""
                } else {
                    &self.text[start..end]
                }
            }
            Some(starts) => {
                let chars = starts.len() - 1;
                let (start, end) = (start_byte.min(chars), end_byte.min(chars));
                if start >= end {
                    ""
                } else {
                    &self.text[starts[start]..starts[end]]
                }
            }
        }
    }
}

/// Universal newlines: `\r\n` and a lone `\r` both become `\n`.
fn normalise_newlines(text: &str) -> String {
    if !text.contains('\r') {
        return text.to_owned();
    }
    let mut out = String::with_capacity(text.len());
    let mut chars = text.chars().peekable();
    while let Some(c) = chars.next() {
        if c == '\r' {
            if chars.peek() == Some(&'\n') {
                chars.next();
            }
            out.push('\n');
        } else {
            out.push(c);
        }
    }
    out
}

/// Python's `str.isspace()` for one character: Rust's `White_Space` plus the
/// four information separators (U+001C..U+001F) Python also counts.
pub(super) fn is_py_space(c: char) -> bool {
    c.is_whitespace() || ('\u{1c}'..='\u{1f}').contains(&c)
}

/// Python's `str.strip()` with no argument.
pub(super) fn py_strip(text: &str) -> &str {
    text.trim_matches(is_py_space)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn invalid_utf8_is_dropped_and_newlines_are_normalised() {
        let source = Source::decode(b"a\xffb\r\nc\rd");
        assert_eq!(source.bytes(), b"ab\nc\nd");
    }

    #[test]
    fn slicing_uses_byte_offsets_as_code_point_indices() {
        // "é" is two bytes: the node "bcd" at bytes 3..6 reads the code
        // points 3..6 instead — shifted one to the right, cut at the end.
        let source = Source::from_text("éabcd");
        assert_eq!(source.slice(3, 6), "cd");
        assert_eq!(source.slice(0, 2), "éa");
        assert_eq!(source.slice(4, 100), "d");
        let ascii = Source::from_text("abcd");
        assert_eq!(ascii.slice(1, 3), "bc");
        assert_eq!(ascii.slice(3, 1), "");
    }

    #[test]
    fn strip_matches_python() {
        assert_eq!(py_strip("\u{1c} a b\t"), "a b");
    }
}
