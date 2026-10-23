// Portions adapted from OpenAI Codex (https://github.com/openai/codex), codex-rs/apply-patch/src/seek_sequence.rs @ be9060f. Apache License 2.0. Copyright 2025 OpenAI.
// Changes: the passes are a named ladder (`Pass`) that is reported; every
// fuzzy pass must find one position (several fail with the candidates);
// the end-of-file try falls back to `start`; shared with unified diffs.

//! Locate a run of lines in a file with decreasing strictness: exact, then
//! ignoring trailing whitespace, then ignoring leading and trailing
//! whitespace, then with typographic punctuation and odd spaces normalised
//! to ASCII. The pass that matched is reported; a fuzzy pass never guesses
//! between several positions.

/// How a run of lines matched.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Pass {
    Exact,
    /// Trailing whitespace ignored.
    TrimEnd,
    /// Leading and trailing whitespace ignored.
    Trim,
    /// As `Trim`, with typographic dashes, quotes and spaces read as ASCII.
    Unicode,
}

impl Pass {
    /// The ladder, strictest first.
    pub const LADDER: [Pass; 4] = [Pass::Exact, Pass::TrimEnd, Pass::Trim, Pass::Unicode];

    /// The name a result reports.
    #[must_use]
    pub fn as_str(self) -> &'static str {
        match self {
            Pass::Exact => "exact",
            Pass::TrimEnd => "trim_end",
            Pass::Trim => "trim",
            Pass::Unicode => "unicode",
        }
    }

    /// Whether `have` (a file line, its ending removed) matches `want` (a
    /// patch line) under this pass.
    #[must_use]
    pub fn line_matches(self, have: &str, want: &str) -> bool {
        match self {
            Pass::Exact => have == want,
            Pass::TrimEnd => have.trim_end() == want.trim_end(),
            Pass::Trim => have.trim() == want.trim(),
            Pass::Unicode => normalise(have) == normalise(want),
        }
    }
}

/// `s` trimmed, with common Unicode punctuation read as its ASCII
/// equivalent, so a patch written in plain ASCII still finds a line with
/// typographic dashes or quotes (as `git apply` ignores minor byte-level
/// differences when locating context).
fn normalise(s: &str) -> String {
    s.trim()
        .chars()
        .map(|c| match c {
            // Dashes and hyphens.
            '\u{2010}' | '\u{2011}' | '\u{2012}' | '\u{2013}' | '\u{2014}' | '\u{2015}'
            | '\u{2212}' => '-',
            // Single quotes.
            '\u{2018}' | '\u{2019}' | '\u{201A}' | '\u{201B}' => '\'',
            // Double quotes.
            '\u{201C}' | '\u{201D}' | '\u{201E}' | '\u{201F}' => '"',
            // Non-breaking and other odd spaces.
            '\u{00A0}' | '\u{2002}' | '\u{2003}' | '\u{2004}' | '\u{2005}' | '\u{2006}'
            | '\u{2007}' | '\u{2008}' | '\u{2009}' | '\u{200A}' | '\u{202F}' | '\u{205F}'
            | '\u{3000}' => ' ',
            other => other,
        })
        .collect()
}

/// Every position at or after `start` where `pattern` matches `lines` under
/// `pass`.
pub fn positions<L: AsRef<str>, P: AsRef<str>>(
    lines: &[L],
    pattern: &[P],
    start: usize,
    pass: Pass,
) -> Vec<usize> {
    if pattern.is_empty() || pattern.len() > lines.len() {
        return Vec::new();
    }
    (start..=lines.len() - pattern.len())
        .filter(|at| {
            pattern
                .iter()
                .zip(&lines[*at..*at + pattern.len()])
                .all(|(want, have)| pass.line_matches(have.as_ref(), want.as_ref()))
        })
        .collect()
}

/// Why a run of lines was not located.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum SeekError {
    /// Nowhere, under any pass.
    NotFound,
    /// A fuzzy pass matched more than once: the 0-based candidates.
    Ambiguous { pass: Pass, candidates: Vec<usize> },
}

/// One pass of the ladder from `start`: the exact pass takes the first
/// position (patches apply in order), a fuzzy pass only a unique one.
fn seek_pass<L: AsRef<str>, P: AsRef<str>>(
    lines: &[L],
    pattern: &[P],
    start: usize,
    pass: Pass,
) -> Result<Option<usize>, SeekError> {
    let found = positions(lines, pattern, start, pass);
    match (pass, found.as_slice()) {
        (_, []) => Ok(None),
        (Pass::Exact, [first, ..]) | (_, [first]) => Ok(Some(*first)),
        (_, _) => Err(SeekError::Ambiguous {
            pass,
            candidates: found,
        }),
    }
}

/// Find `pattern` in `lines` at or after `start`, strictest pass first.
/// With `eof`, the position that ends at the end of the file is tried
/// first (a chunk meant for the file's end lands there), then from
/// `start`. An empty pattern matches at `start`.
///
/// # Errors
///
/// [`SeekError::NotFound`], or [`SeekError::Ambiguous`] when the first
/// pass that matches is a fuzzy one and matches more than once.
pub fn seek_sequence<L: AsRef<str>, P: AsRef<str>>(
    lines: &[L],
    pattern: &[P],
    start: usize,
    eof: bool,
) -> Result<(usize, Pass), SeekError> {
    if pattern.is_empty() {
        return Ok((start, Pass::Exact));
    }
    // A pattern longer than the input cannot match (and must not slice
    // past the end).
    if pattern.len() > lines.len() {
        return Err(SeekError::NotFound);
    }
    if eof {
        let at = (lines.len() - pattern.len()).max(start);
        for pass in Pass::LADDER {
            if let Some(found) = seek_pass(lines, pattern, at, pass)? {
                return Ok((found, pass));
            }
        }
    }
    for pass in Pass::LADDER {
        if let Some(found) = seek_pass(lines, pattern, start, pass)? {
            return Ok((found, pass));
        }
    }
    Err(SeekError::NotFound)
}
