//! Python `re` patterns on the `regex` crate.
//!
//! The Phase 1c passes are regex matchers transcribed from Python. Both
//! engines pick the same match (leftmost-first, the alternative a
//! backtracker tries first, the same capture spans), so a pattern carries
//! over unchanged except for what the two syntaxes and character classes
//! disagree on. [`translate`] rewrites those parts:
//!
//! * `\w` is `str.isalnum()` or `_` in Python: letters and every numeric
//!   character (`²` included), but NOT combining marks. Rust's `\w` is the
//!   reverse on both. It becomes `[\p{L}\p{N}_]`.
//! * `\s` is `str.isspace()` in Python, which adds U+001C..U+001F to
//!   Rust's `White_Space`.
//! * A `{` that does not open a valid quantifier is a literal in Python
//!   and an error in Rust; `{,n}` is a quantifier in Python only.
//! * Inside a class, `[`, `&&`, `--` and `~~` are literals in Python and
//!   class syntax in Rust.
//!
//! `\b` cannot be rewritten (no look-around in the `regex` crate); it keeps
//! Rust's word definition, so a boundary next to a combining mark or a
//! `No` digit can differ. Back-references and look-ahead are not
//! supported at all: the two Python patterns that use them are matched by
//! hand in `api_surface`.

use regex::{Regex, RegexBuilder};

const WORD_ITEMS: &str = r"\p{L}\p{N}_";
const SPACE_ITEMS: &str = r"\s\x{1C}-\x{1F}";

/// Rewrite a Python pattern into the `regex` crate's syntax with Python's
/// character classes.
#[must_use]
pub fn translate(pattern: &str) -> String {
    let chars: Vec<char> = pattern.chars().collect();
    let mut out = String::with_capacity(pattern.len() * 2);
    let mut in_class = false;
    // True right after `[` or `[^`, where `]` is a literal.
    let mut class_start = false;
    let mut i = 0;
    while i < chars.len() {
        let c = chars[i];
        if c == '\\' {
            match chars.get(i + 1) {
                Some('w') if in_class => out.push_str(WORD_ITEMS),
                Some('w') => {
                    out.push('[');
                    out.push_str(WORD_ITEMS);
                    out.push(']');
                }
                Some('s') if in_class => out.push_str(SPACE_ITEMS),
                Some('s') => {
                    out.push('[');
                    out.push_str(SPACE_ITEMS);
                    out.push(']');
                }
                Some(&next) => {
                    out.push('\\');
                    out.push(next);
                }
                None => out.push_str(r"\\"),
            }
            i += 2;
            class_start = false;
            continue;
        }
        if in_class {
            match c {
                ']' if !class_start => {
                    in_class = false;
                    out.push(']');
                }
                '[' | '&' | '~' => {
                    out.push('\\');
                    out.push(c);
                }
                '-' if chars.get(i + 1) == Some(&'-') => out.push_str(r"\-"),
                _ => out.push(c),
            }
            class_start = false;
            i += 1;
            continue;
        }
        match c {
            '[' => {
                in_class = true;
                class_start = true;
                out.push('[');
                if chars.get(i + 1) == Some(&'^') {
                    out.push('^');
                    i += 1;
                }
            }
            '{' => match quantifier(&chars[i..]) {
                Some((text, length)) => {
                    out.push_str(&text);
                    i += length;
                    continue;
                }
                None => out.push_str(r"\{"),
            },
            '}' => out.push_str(r"\}"),
            _ => out.push(c),
        }
        i += 1;
    }
    out
}

/// A Python `{m,n}` quantifier at the start of `chars`, as Rust text and
/// its length in `chars`; `None` when the brace is a literal.
fn quantifier(chars: &[char]) -> Option<(String, usize)> {
    let mut i = 1;
    let mut low = String::new();
    while let Some(c) = chars.get(i).filter(|c| c.is_ascii_digit()) {
        low.push(*c);
        i += 1;
    }
    let mut high: Option<String> = None;
    if chars.get(i) == Some(&',') {
        i += 1;
        let mut digits = String::new();
        while let Some(c) = chars.get(i).filter(|c| c.is_ascii_digit()) {
            digits.push(*c);
            i += 1;
        }
        high = Some(digits);
    }
    if chars.get(i) != Some(&'}') || (low.is_empty() && high.is_none()) {
        return None;
    }
    let low = if low.is_empty() { "0".to_owned() } else { low };
    let text = match high {
        None => format!("{{{low}}}"),
        Some(high) => format!("{{{low},{high}}}"),
    };
    Some((text, i + 1))
}

/// Compile a Python pattern; `flags` holds the letters of `re.I`, `re.M`
/// and `re.S` that the Python pattern was compiled with.
///
/// # Panics
///
/// Never for the engine's constant patterns (each is compiled by a unit
/// test); a pattern that does not compile is a programming error.
#[must_use]
pub fn compile(pattern: &str, flags: &str) -> Regex {
    RegexBuilder::new(&translate(pattern))
        .case_insensitive(flags.contains('i'))
        .multi_line(flags.contains('m'))
        .dot_matches_new_line(flags.contains('s'))
        .size_limit(64 << 20)
        .build()
        .unwrap_or_else(|error| unreachable!("constant pattern {pattern:?}: {error}"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn classes_follow_python() {
        let word = compile(r"^\w+$", "");
        assert!(word.is_match("a²_1"));
        // A combining mark is not a Python word character.
        assert!(!word.is_match("e\u{301}"));
        assert!(compile(r"^\s$", "").is_match("\u{1c}"));
        assert!(compile(r"^[\w<>]+$", "").is_match("List<a²>"));
    }

    #[test]
    fn literal_braces_and_class_operators() {
        assert_eq!(translate(r"\s*{"), r"[\s\x{1C}-\x{1F}]*\{");
        assert!(compile(r"a{0,2}?b", "").is_match("aab"));
        assert!(compile(r"^x{,2}$", "").is_match("xx"));
        assert!(compile(r"^a{}$", "").is_match("a{}"));
        assert!(compile(r"^[^{}]+$", "").is_match("abc"));
        assert!(compile(r"^[a&&b]+$", "").is_match("&a&"));
        assert!(compile(r"^[[]+$", "").is_match("[["));
        assert!(compile(r"^[]a]+$", "").is_match("]a"));
    }
}
