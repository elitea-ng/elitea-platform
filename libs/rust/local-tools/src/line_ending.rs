//! The one rule every edit path (`edit_file`/`multi_edit`, unified diffs,
//! the Codex envelope) uses for the ending a new line takes when nothing
//! nearer decides it: the file's majority ending, CRLF only when more
//! lines end in `\r\n` than in a bare `\n` (a tie, or a file without
//! newlines, is LF). Only `\n` and `\r\n` end a line; a lone `\r` is
//! content.

/// The ending most of `text`'s lines use (`\n` on a tie or with none).
#[must_use]
pub(crate) fn usual_ending(text: &str) -> &'static str {
    let crlf = text.matches("\r\n").count();
    let lf = text.matches('\n').count() - crlf;
    if crlf > lf { "\r\n" } else { "\n" }
}

#[cfg(test)]
mod tests {
    use super::usual_ending;

    #[test]
    fn majority_wins_and_ties_are_lf() {
        assert_eq!(usual_ending(""), "\n");
        assert_eq!(usual_ending("no newline"), "\n");
        assert_eq!(usual_ending("a\r\nb\r\nc\n"), "\r\n");
        assert_eq!(usual_ending("a\r\nb\n"), "\n");
        assert_eq!(usual_ending("a\nb\nc\r\n"), "\n");
        // A lone CR is content, not an ending.
        assert_eq!(usual_ending("a\rb\r\n"), "\r\n");
    }
}
