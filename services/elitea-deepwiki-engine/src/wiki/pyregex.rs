//! Python `re` on `fancy-regex`, for the transcribed Mermaid sanitizer.
//!
//! The sanitizer is ~150 Python regular expressions, several with
//! look-around (`(?<![A-Za-z0-9_])`, `"(?!\])`, `(?=-{1,2}>>)`), which the
//! `regex` crate does not have. `fancy-regex` (already in the tree through
//! `tiktoken-rs`) is a backtracking engine like `sre`: leftmost-first, the
//! same alternative preference and the same capture spans. What still
//! differs is rewritten here:
//!
//! * `\w`, `\s`, the brace and class syntax: [`crate::graph::pyre::translate`];
//! * `\b`: Python's boundary is between a `str.isalnum()`-or-`_` character
//!   and anything else. It becomes the equivalent look-around pair over
//!   `[\p{L}\p{N}_]`, so a combining mark is not a word character (it is
//!   in Rust);
//! * `$` without `MULTILINE` matches at the end AND before a final `\n` in
//!   Python; it becomes `(?=\n?\z)`. `\Z` becomes `\z`;
//! * `re.sub` replaces an empty match that directly follows a non-empty
//!   one (Python 3.7+); the `regex` iterator skips it. [`PyRe::sub_with`]
//!   follows Python;
//! * replacement templates (`\1`, `\g<name>`, `\\`) are parsed as
//!   `re._parser.parse_template` does — `r"'\\1'"` is the literal text
//!   `'\1'`, not group 1 (the sanitizer has that quirk, kept);
//! * a group that did not take part substitutes `""`;
//! * under `IGNORECASE`, Python's `i` also matches `İ` (U+0130, whose
//!   simple lower case is `i`) and `ı` (U+0131, an `sre` case
//!   equivalence); Unicode simple case folding (Rust) has neither. A
//!   literal `i` / `I` becomes `[iI\u{130}\u{131}]` and a class that
//!   holds `i` or `I` gains the two characters. Every other ASCII letter
//!   folds the same in both (`k` / U+212A and `s` / U+017F included;
//!   checked over all code points with Python 3.12).
//!
//! The one `sre` behaviour not reproduced: after an EMPTY match, `sre`
//! retries the same position for a non-empty alternative; this skips to
//! the next character. No sanitizer pattern can match both empty and
//! non-empty at one position with the empty match preferred.

use fancy_regex::{Captures, Regex, RegexBuilder, RegexInput};
use std::cell::Cell;
use std::fmt::{self, Write as _};
use std::time::Instant;

/// `re.MULTILINE`, `re.DOTALL`, `re.IGNORECASE`.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Flags {
    pub multiline: bool,
    pub dotall: bool,
    pub ignorecase: bool,
}

impl Flags {
    pub const NONE: Self = Self {
        multiline: false,
        dotall: false,
        ignorecase: false,
    };
    pub const M: Self = Self {
        multiline: true,
        dotall: false,
        ignorecase: false,
    };
    pub const I: Self = Self {
        multiline: false,
        dotall: false,
        ignorecase: true,
    };
}

/// A pattern that did not compile or a match that hit the backtrack cap.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ReError(pub String);

impl fmt::Display for ReError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for ReError {}

/// The backtracking cap of ONE search. `sre` has none; a diagram is at
/// most 8,000 characters (`max_diagram_chars`), and every search is
/// anchored or linear in the line, so a search that reaches this is
/// pathological and the page keeps its unsanitised text (Python's
/// fail-soft path). [`with_deadline`] bounds the sum of all searches.
const BACKTRACK_LIMIT: usize = 1_000_000;

thread_local! {
    /// The wall-clock end of the current [`with_deadline`] scope.
    static DEADLINE: Cell<Option<Instant>> = const { Cell::new(None) };
}

/// Run `work` with a wall-clock deadline: every search that starts after
/// `deadline` fails with [`ReError`]. The deadline is per thread and the
/// previous one is restored after `work`.
pub fn with_deadline<T>(deadline: Instant, work: impl FnOnce() -> T) -> T {
    let previous = DEADLINE.with(|cell| cell.replace(Some(deadline)));
    let result = work();
    DEADLINE.with(|cell| cell.set(previous));
    result
}

/// The message of a search that started after the deadline.
pub const DEADLINE_EXCEEDED: &str = "regular-expression time budget exceeded";

fn check_deadline() -> Result<(), ReError> {
    match DEADLINE.with(Cell::get) {
        Some(deadline) if Instant::now() >= deadline => Err(ReError(DEADLINE_EXCEEDED.to_owned())),
        _ => Ok(()),
    }
}

const WORD: &str = r"[\p{L}\p{N}_]";

/// A compiled Python pattern.
#[derive(Debug)]
pub struct PyRe {
    regex: Regex,
}

/// One match: the group spans (byte offsets) and the group names.
#[derive(Debug, Clone)]
pub struct Match<'t> {
    text: &'t str,
    spans: Vec<Option<(usize, usize)>>,
    names: Vec<Option<String>>,
}

impl<'t> Match<'t> {
    fn from_captures(text: &'t str, caps: &Captures<'t, str>, regex: &Regex) -> Self {
        let spans = (0..caps.len())
            .map(|i| caps.get(i).map(|m| (m.start(), m.end())))
            .collect();
        let names = regex
            .capture_names()
            .map(|n| n.map(str::to_owned))
            .collect();
        Self { text, spans, names }
    }

    /// `m.start()`.
    #[must_use]
    pub fn start(&self) -> usize {
        self.spans.first().copied().flatten().map_or(0, |s| s.0)
    }

    /// `m.end()`.
    #[must_use]
    pub fn end(&self) -> usize {
        self.spans.first().copied().flatten().map_or(0, |s| s.1)
    }

    /// `m.group(0)`.
    #[must_use]
    pub fn whole(&self) -> &'t str {
        &self.text[self.start()..self.end()]
    }

    /// `m.group(i)`, `""` when the group did not take part.
    #[must_use]
    pub fn group(&self, index: usize) -> &'t str {
        self.get(index).unwrap_or("")
    }

    /// `m.group(i)`, `None` when the group did not take part.
    #[must_use]
    pub fn get(&self, index: usize) -> Option<&'t str> {
        self.spans
            .get(index)
            .copied()
            .flatten()
            .map(|(s, e)| &self.text[s..e])
    }

    /// `m.group(name)`, `""` when the group did not take part.
    #[must_use]
    pub fn name(&self, name: &str) -> &'t str {
        self.names
            .iter()
            .position(|n| n.as_deref() == Some(name))
            .map_or("", |i| self.group(i))
    }

    fn len(&self) -> usize {
        self.spans.len()
    }
}

/// Rewrite the Python-only syntax (see the module comment) after
/// [`crate::graph::pyre::translate`].
fn rewrite(pattern: &str, multiline: bool, ignorecase: bool) -> String {
    let chars: Vec<char> = pattern.chars().collect();
    let mut out = String::with_capacity(pattern.len() + 16);
    let mut in_class = false;
    let mut class_start = false;
    let mut class_body = 0;
    let mut i = 0;
    while i < chars.len() {
        let c = chars[i];
        if c == '\\' {
            let next = chars.get(i + 1).copied();
            // `\p{L}`, `\x{1C}`: the braces are part of the escape.
            if matches!(next, Some('p' | 'P' | 'x' | 'u')) && chars.get(i + 2) == Some(&'{') {
                let close = chars[i..]
                    .iter()
                    .position(|&ch| ch == '}')
                    .map_or(chars.len(), |at| i + at + 1);
                out.extend(&chars[i..close]);
                i = close;
                class_start = false;
                continue;
            }
            match next {
                Some('b') if !in_class => {
                    let _ = write!(out, "(?:(?<={WORD})(?!{WORD})|(?<!{WORD})(?={WORD}))");
                }
                Some('b') => out.push_str(r"\x08"),
                Some('Z') if !in_class => out.push_str(r"\z"),
                // `\"` and `\'` are literal quotes in Python; spell them
                // plainly rather than rely on the engine's escape rules.
                Some(q @ ('"' | '\'')) => out.push(q),
                Some(n) => {
                    out.push('\\');
                    out.push(n);
                }
                None => out.push('\\'),
            }
            i += 2;
            class_start = false;
            continue;
        }
        if in_class {
            if c == ']' && !class_start {
                in_class = false;
                if ignorecase && class_holds_i(&chars[class_body..i]) {
                    out.push_str(DOTTED_DOTLESS_I);
                }
            }
            class_start = false;
            out.push(c);
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
                class_body = i + 1;
            }
            // A group header: its name and flags are not literals.
            '(' if chars.get(i + 1) == Some(&'?') => {
                let header = group_header_len(&chars[i..]);
                out.extend(&chars[i..i + header]);
                i += header;
                continue;
            }
            'i' | 'I' if ignorecase => {
                out.push_str("[iI");
                out.push_str(DOTTED_DOTLESS_I);
                out.push(']');
            }
            '$' if !multiline => out.push_str(r"(?=\n?\z)"),
            _ => out.push(c),
        }
        i += 1;
    }
    out
}

/// `İ` and `ı`, which Python's `IGNORECASE` lets an `i` match.
const DOTTED_DOTLESS_I: &str = "\u{130}\u{131}";

/// Whether a class body (between `[` / `[^` and `]`) holds `i` or `I`,
/// as a literal or inside a range.
fn class_holds_i(body: &[char]) -> bool {
    let mut j = 0;
    while j < body.len() {
        if body[j] == '\\' {
            j += 2;
            continue;
        }
        let low = body[j];
        if body.get(j + 1) == Some(&'-')
            && let Some(&high) = body.get(j + 2)
            && high != '\\'
        {
            if (low..=high).contains(&'i') || (low..=high).contains(&'I') {
                return true;
            }
            j += 3;
            continue;
        }
        if matches!(low, 'i' | 'I') {
            return true;
        }
        j += 1;
    }
    false
}

/// The length of the group header at the start of `chars` (`(?:`, `(?=`,
/// `(?<!`, `(?P<name>`, `(?P=name)`, `(?<name>`, `(?i)`, `(?s:` …): the
/// part that holds names and flags, not pattern text.
fn group_header_len(chars: &[char]) -> usize {
    let through = |stop: char| {
        chars
            .iter()
            .position(|&c| c == stop)
            .map_or(chars.len(), |at| at + 1)
    };
    match (chars.get(2).copied(), chars.get(3).copied()) {
        (Some('P'), Some('<')) => through('>'),
        (Some('<'), Some(c)) if c != '=' && c != '!' => through('>'),
        (Some('P'), Some('=')) => through(')'),
        (Some(c), _) if c.is_ascii_alphabetic() || c == '-' => {
            2 + chars[2..]
                .iter()
                .take_while(|c| c.is_ascii_alphabetic() || **c == '-')
                .count()
        }
        _ => 2,
    }
}

impl PyRe {
    /// Compile a Python pattern.
    ///
    /// # Errors
    ///
    /// When the translated pattern does not compile (a transcription bug;
    /// the tests compile every sanitizer pattern).
    pub fn new(pattern: &str, flags: Flags) -> Result<Self, ReError> {
        // A leading inline `(?m)` is MULTILINE for the `$` rewrite too.
        let (pattern, flags) = match pattern.strip_prefix("(?m)") {
            Some(rest) => (
                rest,
                Flags {
                    multiline: true,
                    ..flags
                },
            ),
            None => (pattern, flags),
        };
        let mut source = String::new();
        if flags.multiline {
            source.push_str("(?m)");
        }
        if flags.dotall {
            source.push_str("(?s)");
        }
        if flags.ignorecase {
            source.push_str("(?i)");
        }
        // `translate` first: it would read the `\p{L}` this module writes
        // for `\b` as a literal brace.
        source.push_str(&rewrite(
            &crate::graph::pyre::translate(pattern),
            flags.multiline,
            flags.ignorecase,
        ));
        let regex = RegexBuilder::new(&source)
            .backtrack_limit(BACKTRACK_LIMIT)
            .build()
            .map_err(|e| ReError(format!("pattern {pattern:?}: {e}")))?;
        Ok(Self { regex })
    }

    /// `re.search` from byte offset `pos` (look-behind sees the text
    /// before it, `^` matches only at 0, as `pattern.search(text, pos)`).
    ///
    /// # Errors
    ///
    /// The backtrack cap or the deadline.
    pub fn search_from<'t>(&self, text: &'t str, pos: usize) -> Result<Option<Match<'t>>, ReError> {
        if pos > text.len() {
            return Ok(None);
        }
        check_deadline()?;
        self.regex
            .captures_from_pos(text, pos)
            .map(|c| c.map(|caps| Match::from_captures(text, &caps, &self.regex)))
            .map_err(|e| ReError(format!("{e} ({})", self.regex.as_str())))
    }

    /// `re.search(pattern, text)`.
    ///
    /// # Errors
    ///
    /// The backtrack cap or the deadline.
    pub fn search<'t>(&self, text: &'t str) -> Result<Option<Match<'t>>, ReError> {
        self.search_from(text, 0)
    }

    /// `bool(re.search(pattern, text))`.
    ///
    /// # Errors
    ///
    /// The backtrack cap or the deadline.
    pub fn is_match(&self, text: &str) -> Result<bool, ReError> {
        Ok(self.search(text)?.is_some())
    }

    /// `pattern.match(text, pos)`: a match that starts at `pos`. The
    /// search is ANCHORED at `pos` (one attempt, not a scan of the rest of
    /// the text), on the whole text: look-behind still sees the text before
    /// `pos` and `^` still matches only at 0 (or after a newline under
    /// `MULTILINE`), as in Python.
    ///
    /// # Errors
    ///
    /// The backtrack cap or the deadline.
    pub fn match_at<'t>(&self, text: &'t str, pos: usize) -> Result<Option<Match<'t>>, ReError> {
        if pos > text.len() {
            return Ok(None);
        }
        check_deadline()?;
        self.regex
            .captures_input(RegexInput::new(text).from_pos(pos).anchored(true))
            .map(|c| c.map(|caps| Match::from_captures(text, &caps, &self.regex)))
            .map_err(|e| ReError(format!("{e} ({})", self.regex.as_str())))
    }

    /// `re.match(pattern, text)`.
    ///
    /// # Errors
    ///
    /// The backtrack cap or the deadline.
    pub fn match_start<'t>(&self, text: &'t str) -> Result<Option<Match<'t>>, ReError> {
        self.match_at(text, 0)
    }

    /// `re.finditer`, with Python's empty-match rule.
    ///
    /// # Errors
    ///
    /// The backtrack cap or the deadline.
    pub fn find_all<'t>(&self, text: &'t str) -> Result<Vec<Match<'t>>, ReError> {
        let mut found = Vec::new();
        let mut pos = 0;
        let mut last_empty_at: Option<usize> = None;
        while pos <= text.len() {
            let Some(m) = self.search_from(text, pos)? else {
                break;
            };
            if m.start() == m.end() && last_empty_at == Some(m.start()) {
                // The same empty match again: step one character.
                pos = next_char(text, m.start());
                last_empty_at = None;
                continue;
            }
            let (start, end) = (m.start(), m.end());
            found.push(m);
            if start == end {
                last_empty_at = Some(end);
                pos = end;
            } else {
                last_empty_at = None;
                pos = end;
            }
        }
        Ok(found)
    }

    /// `re.sub(pattern, repl, text)` with a function replacement.
    ///
    /// # Errors
    ///
    /// The backtrack cap, the deadline, or an error from `repl`.
    pub fn sub_with<'t>(
        &self,
        text: &'t str,
        mut repl: impl FnMut(&Match<'t>) -> Result<String, ReError>,
    ) -> Result<String, ReError> {
        let matches = self.find_all(text)?;
        if matches.is_empty() {
            return Ok(text.to_owned());
        }
        let mut out = String::with_capacity(text.len());
        let mut last = 0;
        for m in &matches {
            out.push_str(&text[last..m.start()]);
            out.push_str(&repl(m)?);
            last = m.end();
        }
        out.push_str(&text[last..]);
        Ok(out)
    }

    /// `re.sub(pattern, template, text)`.
    ///
    /// # Errors
    ///
    /// The backtrack cap, the deadline, or a template that names a missing group.
    pub fn sub(&self, text: &str, template: &str) -> Result<String, ReError> {
        let parts = parse_template(template)?;
        self.sub_with(text, |m| expand(&parts, m))
    }
}

fn next_char(text: &str, at: usize) -> usize {
    text[at..]
        .chars()
        .next()
        .map_or(text.len() + 1, |c| at + c.len_utf8())
}

/// One piece of a parsed replacement template.
#[derive(Debug, Clone, PartialEq, Eq)]
enum Piece {
    Literal(String),
    Group(usize),
    Named(String),
}

/// `re._parser.parse_template`: `\N` / `\NN` and `\g<…>` are groups,
/// `\\` a backslash, the ASCII escapes `\n \t \r \f \v \a \b` their
/// characters; any other escaped ASCII letter is an error; anything else
/// after a backslash keeps the backslash.
fn parse_template(template: &str) -> Result<Vec<Piece>, ReError> {
    let chars: Vec<char> = template.chars().collect();
    let mut pieces = Vec::new();
    let mut literal = String::new();
    let mut i = 0;
    while i < chars.len() {
        let c = chars[i];
        if c != '\\' {
            literal.push(c);
            i += 1;
            continue;
        }
        let Some(&next) = chars.get(i + 1) else {
            return Err(ReError("bad escape (end of pattern)".to_owned()));
        };
        match next {
            'g' => {
                let close = chars[i..].iter().position(|&ch| ch == '>');
                let (Some(close), Some('<')) = (close, chars.get(i + 2).copied()) else {
                    return Err(ReError("missing group name".to_owned()));
                };
                let name: String = chars[i + 3..i + close].iter().collect();
                if !literal.is_empty() {
                    pieces.push(Piece::Literal(std::mem::take(&mut literal)));
                }
                match name.parse::<usize>() {
                    Ok(index) => pieces.push(Piece::Group(index)),
                    Err(_) => pieces.push(Piece::Named(name)),
                }
                i += close + 1;
            }
            '0' => {
                return Err(ReError("octal escapes are not used here".to_owned()));
            }
            '1'..='9' => {
                // `\N`, `\NN`, or a three-digit octal escape.
                let mut digits = String::from(next);
                let mut used = 2;
                if let Some(&d2) = chars.get(i + 2).filter(|d| d.is_ascii_digit()) {
                    digits.push(d2);
                    used = 3;
                    let octal = |c: char| ('0'..='7').contains(&c);
                    if let Some(&d3) = chars.get(i + 3).filter(|d| octal(**d))
                        && octal(next)
                        && octal(d2)
                    {
                        digits.push(d3);
                        let code =
                            u32::from_str_radix(&digits, 8).map_err(|e| ReError(e.to_string()))?;
                        if code > 0o377 {
                            return Err(ReError(format!(
                                "octal escape value \\{digits} outside of range 0-0o377"
                            )));
                        }
                        literal.push(char::from_u32(code).unwrap_or('\u{fffd}'));
                        i += 4;
                        continue;
                    }
                }
                if !literal.is_empty() {
                    pieces.push(Piece::Literal(std::mem::take(&mut literal)));
                }
                let index = digits
                    .parse::<usize>()
                    .map_err(|e| ReError(e.to_string()))?;
                pieces.push(Piece::Group(index));
                i += used;
            }
            '\\' => {
                literal.push('\\');
                i += 2;
            }
            'n' | 't' | 'r' | 'f' | 'v' | 'a' | 'b' => {
                literal.push(match next {
                    'n' => '\n',
                    't' => '\t',
                    'r' => '\r',
                    'f' => '\u{0c}',
                    'v' => '\u{0b}',
                    'a' => '\u{07}',
                    _ => '\u{08}',
                });
                i += 2;
            }
            letter if letter.is_ascii_alphabetic() => {
                return Err(ReError(format!("bad escape \\{letter}")));
            }
            other => {
                literal.push('\\');
                literal.push(other);
                i += 2;
            }
        }
    }
    if !literal.is_empty() {
        pieces.push(Piece::Literal(literal));
    }
    Ok(pieces)
}

fn expand(pieces: &[Piece], m: &Match<'_>) -> Result<String, ReError> {
    let mut out = String::new();
    for piece in pieces {
        match piece {
            Piece::Literal(text) => out.push_str(text),
            Piece::Group(index) => {
                if *index >= m.len() {
                    return Err(ReError(format!("invalid group reference {index}")));
                }
                out.push_str(m.group(*index));
            }
            Piece::Named(name) => out.push_str(m.name(name)),
        }
    }
    Ok(out)
}

/// `re.escape` for the identifiers the sanitizer interpolates.
#[must_use]
pub fn escape(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    for c in text.chars() {
        if c.is_ascii_alphanumeric() || c == '_' || !c.is_ascii() {
            out.push(c);
        } else {
            out.push('\\');
            out.push(c);
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn re(pattern: &str, flags: Flags) -> PyRe {
        PyRe::new(pattern, flags).unwrap()
    }

    #[test]
    fn dollar_matches_before_a_final_newline() {
        // python3 -c 'import re;print(re.sub(r"a$","X","a\n"), re.sub(r"a$","X","a\nb"))'
        assert_eq!(re("a$", Flags::NONE).sub("a\n", "X").unwrap(), "X\n");
        assert_eq!(re("a$", Flags::NONE).sub("a\nb", "X").unwrap(), "a\nb");
        assert_eq!(re("a$", Flags::M).sub("a\nb", "X").unwrap(), "X\nb");
    }

    #[test]
    fn empty_match_after_a_match_is_replaced() {
        // python3 -c 'import re;print(re.sub("x*","-","abxd"))'  →  -a-b--d-
        assert_eq!(re("x*", Flags::NONE).sub("abxd", "-").unwrap(), "-a-b--d-");
    }

    #[test]
    fn template_backslash_backslash_is_literal() {
        // python3 -c 'import re;print(re.sub(r"(a)", r"[\\1]", "a"))'  →  [\1]
        assert_eq!(re("(a)", Flags::NONE).sub("a", r"[\\1]").unwrap(), r"[\1]");
        assert_eq!(
            re("(a)(b)?", Flags::NONE).sub("a", r"<\1\2>").unwrap(),
            "<a>"
        );
        assert_eq!(
            re("(?P<x>a)", Flags::NONE).sub("a", r"\g<x>\g<1>").unwrap(),
            "aa"
        );
    }

    #[test]
    fn word_boundary_is_python_s() {
        // A combining mark is not a word character in Python.
        // python3 -c 'import re;print(re.findall(r"\bx", "éx"))'  →  ['x']
        assert_eq!(
            re(r"\bx", Flags::NONE).find_all("e\u{301}x").unwrap().len(),
            1
        );
        assert_eq!(
            re(r"\bend\b", Flags::NONE)
                .find_all("end; bend endx")
                .unwrap()
                .len(),
            1
        );
    }

    #[test]
    fn lookaround_works() {
        assert_eq!(
            re(r#"(?<![A-Za-z0-9_])end\[""#, Flags::NONE)
                .sub(r#"end["a"] xend["b"]"#, r#"end_[""#)
                .unwrap(),
            r#"end_["a"] xend["b"]"#
        );
    }

    /// `re.IGNORECASE` lets `i` match `İ` and `ı` (python3.12: `[c for c
    /// in range(0x110000) if re.fullmatch("i", chr(c), re.I)]` is `i I İ ı`;
    /// for every other ASCII letter Python and Rust agree).
    #[test]
    fn ignorecase_i_matches_the_dotted_and_dotless_i() {
        let p = re("mermaid", Flags::I);
        for text in ["mermaid", "MERMAID", "merma\u{130}d", "MERMA\u{131}D"] {
            assert!(p.is_match(text).unwrap(), "{text}");
        }
        assert!(
            !re("mermaid", Flags::NONE)
                .is_match("merma\u{130}d")
                .unwrap()
        );
        assert!(re("[h-j]", Flags::I).is_match("\u{131}").unwrap());
        assert!(!re("[^i]", Flags::I).is_match("\u{130}").unwrap());
        assert!(!re("[a-h]", Flags::I).is_match("\u{131}").unwrap());
        // Names and flags are not pattern text.
        let named = re(r"(?P<inner>x)(?P=inner)", Flags::I);
        assert_eq!(named.search("XX").unwrap().unwrap().name("inner"), "X");
        // python3 -c 'import re;print(bool(re.search(r"\bin\b", "x İn y", re.I)),
        //   bool(re.search(r"\bin\b", "xİn y", re.I)))'  →  True False
        assert!(re(r"\bin\b", Flags::I).is_match("x \u{130}n y").unwrap());
        assert!(!re(r"\bin\b", Flags::I).is_match("x\u{130}n y").unwrap());
        // Python and Rust both fold the Kelvin sign and the long s.
        assert!(re("k", Flags::I).is_match("\u{212a}").unwrap());
        assert!(re("s", Flags::I).is_match("\u{17f}").unwrap());
    }

    #[test]
    fn match_at_respects_the_position() {
        let p = re("b+", Flags::NONE);
        assert!(p.match_at("abb", 1).unwrap().is_some());
        assert!(p.match_at("abb", 0).unwrap().is_none());
    }
}
