//! String-replacement edits (`edit_file`, `multi_edit`) and the snippet
//! every edit result carries.
//!
//! Each `old_string` is matched against the file as it was before the
//! call: exactly, else whitespace-normalised (CRLF read as LF, trailing
//! whitespace ignored, one uniform indentation shift), which is accepted
//! only where it finds a single place (or every place, with `replace_all`)
//! and is reported. Replacements keep the file's line endings: a
//! `new_string` takes the ending of the lines it replaces (the file's
//! usual ending, `crate::line_ending`, when they have none).
//!
//! An indentation shift re-indents `new_string` by the same amount. When
//! the pattern is indented more than the file and a `new_string` line is
//! indented less than that difference, the edit is refused
//! ([`Unplaced::Unshiftable`]) rather than changing the relative
//! indentation the model wrote.

use serde::Deserialize;

use crate::line_ending::usual_ending;

/// The most edits one `multi_edit` call takes.
pub const MAX_EDITS: usize = elitea_agent_runtime::edit_profile::MULTI_EDIT_MAX_EDITS;
/// The most snippet text one call returns.
pub const MAX_SNIPPET_BYTES: usize = 4 * 1024;
/// Lines of context around each change in a snippet.
const SNIPPET_CONTEXT: usize = 3;

/// One replacement.
#[derive(Clone, Debug, Deserialize)]
pub struct EditSpec {
    pub old_string: String,
    pub new_string: String,
    #[serde(default)]
    pub replace_all: bool,
}

/// How an edit's `old_string` was found.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum EditMatch {
    Exact,
    /// Line endings, trailing whitespace or a uniform indentation shift
    /// told apart.
    Normalised,
}

impl EditMatch {
    #[must_use]
    pub fn as_str(self) -> &'static str {
        match self {
            EditMatch::Exact => "exact",
            EditMatch::Normalised => "normalised",
        }
    }
}

/// A byte range of the original and what replaces it.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Span {
    pub start: usize,
    pub end: usize,
    pub new: String,
}

/// Why an edit has no place.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Unplaced {
    Absent,
    /// Found this many times, and `replace_all` is off.
    Ambiguous(usize, EditMatch),
    /// Found only with the pattern's indentation reduced, and some
    /// `new_string` line is indented less than that reduction.
    Unshiftable,
}

/// `text` with every line ending made `ending`.
fn with_endings(text: &str, ending: &str) -> String {
    let lf = text.replace("\r\n", "\n");
    if ending == "\n" {
        lf
    } else {
        lf.replace('\n', ending)
    }
}

/// The ending a replacement takes: the replaced text's, else the file's
/// usual one (computed once per call by the caller).
fn ending_for(replaced: &str, usual: &'static str) -> &'static str {
    if replaced.contains("\r\n") {
        "\r\n"
    } else if replaced.contains('\n') {
        "\n"
    } else {
        usual
    }
}

/// The places `edit` applies in `text`.
///
/// # Errors
///
/// [`Unplaced`] when it is nowhere, or in more than one place without
/// `replace_all`, or matched only by a shift `new_string` cannot take.
pub fn locate(text: &str, edit: &EditSpec) -> Result<(Vec<Span>, EditMatch), Unplaced> {
    let usual = usual_ending(text);
    let exact: Vec<Span> = text
        .match_indices(edit.old_string.as_str())
        .map(|(start, found)| Span {
            start,
            end: start + found.len(),
            new: with_endings(&edit.new_string, ending_for(found, usual)),
        })
        .collect();
    let (spans, how) = if exact.is_empty() {
        (normalised(text, edit)?, EditMatch::Normalised)
    } else {
        (exact, EditMatch::Exact)
    };
    match spans.len() {
        0 => Err(Unplaced::Absent),
        1 => Ok((spans, how)),
        count if !edit.replace_all => Err(Unplaced::Ambiguous(count, how)),
        _ => Ok((spans, how)),
    }
}

/// An indentation difference between the file and the pattern.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Shift<'a> {
    /// The file's lines are indented by this much more.
    Add(&'a str),
    /// The pattern's lines are indented by this much more.
    Remove(&'a str),
}

fn leading(line: &str) -> &str {
    &line[..line.len() - line.trim_start().len()]
}

/// The shift that makes `file` equal `pattern` (both with trailing
/// whitespace removed), if one does.
fn shift_of<'a>(file: &'a str, pattern: &'a str) -> Option<Shift<'a>> {
    if file.trim_start() != pattern.trim_start() {
        return None;
    }
    let (have, want) = (leading(file), leading(pattern));
    if let Some(extra) = have.strip_prefix(want) {
        Some(Shift::Add(extra))
    } else {
        want.strip_prefix(have).map(Shift::Remove)
    }
}

/// One line of the file: its text without the ending, and its byte range
/// with and without the ending.
struct FileLine<'a> {
    text: &'a str,
    start: usize,
    content_end: usize,
    end: usize,
}

fn file_lines(text: &str) -> Vec<FileLine<'_>> {
    let mut out = Vec::new();
    let mut start = 0;
    for line in text.split_inclusive('\n') {
        let content = line
            .strip_suffix("\r\n")
            .or_else(|| line.strip_suffix('\n'))
            .unwrap_or(line);
        out.push(FileLine {
            text: content,
            start,
            content_end: start + content.len(),
            end: start + line.len(),
        });
        start += line.len();
    }
    out
}

/// The whitespace-normalised places of `edit`: whole lines that equal the
/// pattern's once CRLF, trailing whitespace and one uniform indentation
/// shift are set aside. `new_string` is re-indented by the same shift and
/// takes the matched lines' ending.
fn normalised(text: &str, edit: &EditSpec) -> Result<Vec<Span>, Unplaced> {
    let pattern = edit.old_string.replace("\r\n", "\n");
    let ends_with_newline = pattern.ends_with('\n');
    let body = pattern.strip_suffix('\n').unwrap_or(&pattern);
    let wanted: Vec<&str> = body.split('\n').map(str::trim_end).collect();
    if wanted.iter().all(|line| line.is_empty()) {
        return Ok(Vec::new());
    }
    let lines = file_lines(text);
    let mut spans: Vec<Span> = Vec::new();
    let mut at = 0;
    while at + wanted.len() <= lines.len() {
        let window = &lines[at..at + wanted.len()];
        let Some(shift) = window_shift(window, &wanted) else {
            at += 1;
            continue;
        };
        let last = &window[wanted.len() - 1];
        let end = if ends_with_newline {
            last.end
        } else {
            last.content_end
        };
        let ending = if text[window[0].content_end..window[0].end].starts_with("\r\n") {
            "\r\n"
        } else {
            "\n"
        };
        spans.push(Span {
            start: window[0].start,
            end,
            new: reindent(&edit.new_string, shift, ending).ok_or(Unplaced::Unshiftable)?,
        });
        at += wanted.len();
    }
    Ok(spans)
}

/// The one shift under which `window` matches `wanted` line for line.
fn window_shift<'a>(window: &[FileLine<'a>], wanted: &[&'a str]) -> Option<Shift<'a>> {
    let mut shift = None;
    for (line, want) in window.iter().zip(wanted) {
        let have = line.text.trim_end();
        if want.is_empty() || have.is_empty() {
            if want.is_empty() && have.is_empty() {
                continue;
            }
            return None;
        }
        let this = shift_of(have, want)?;
        match shift {
            None => shift = Some(this),
            Some(seen) if seen == this => {}
            Some(_) => return None,
        }
    }
    shift
}

/// `new` shifted like the match, with `ending` between its lines; `None`
/// when a non-blank line does not start with a [`Shift::Remove`]'s whole
/// prefix (stripping less would change its indentation relative to the
/// other lines).
fn reindent(new: &str, shift: Shift<'_>, ending: &str) -> Option<String> {
    let lf = new.replace("\r\n", "\n");
    let lines: Option<Vec<String>> = lf
        .split('\n')
        .map(|line| {
            if line.trim().is_empty() {
                return Some(line.to_owned());
            }
            match shift {
                Shift::Add(extra) => Some(format!("{extra}{line}")),
                Shift::Remove(extra) => line.strip_prefix(extra).map(str::to_owned),
            }
        })
        .collect();
    Some(lines?.join(ending))
}

/// The 0-based `[start, end)` lines of `text` a byte range covers (both
/// ends on character boundaries).
#[must_use]
pub fn line_range(text: &str, start: usize, end: usize) -> (usize, usize) {
    let first = text[..start].matches('\n').count();
    if end <= start {
        return (first, first);
    }
    let upto = &text[..end];
    let lines = upto.matches('\n').count();
    (
        first,
        if upto.ends_with('\n') {
            lines
        } else {
            lines + 1
        },
    )
}

/// A bounded view of the new text: `SNIPPET_CONTEXT` (3) lines around each
/// changed range, numbered like `read_file`, gaps marked `...`. Stops at
/// the last whole line within `budget` bytes, which it lowers; the flag
/// says whether something was left out.
#[must_use]
pub fn snippet(text: &str, ranges: &[(usize, usize)], budget: &mut usize) -> (String, bool) {
    let lines: Vec<&str> = text.lines().collect();
    let mut windows: Vec<(usize, usize)> = Vec::new();
    let mut sorted = ranges.to_vec();
    sorted.sort_unstable();
    for (start, end) in sorted {
        let from = start.saturating_sub(SNIPPET_CONTEXT);
        let to = (end.max(start + 1) + SNIPPET_CONTEXT).min(lines.len());
        if from >= to {
            continue;
        }
        match windows.last_mut() {
            Some(last) if from <= last.1 => last.1 = last.1.max(to),
            _ => windows.push((from, to)),
        }
    }
    let mut out = String::new();
    let mut cut = false;
    'windows: for (index, (from, to)) in windows.iter().enumerate() {
        if index > 0 {
            if *budget < 4 {
                cut = true;
                break;
            }
            out.push_str("...\n");
            *budget -= 4;
        }
        for (number, line) in lines.iter().enumerate().take(*to).skip(*from) {
            let shown = format!("{:>6}\t{line}\n", number + 1);
            if shown.len() > *budget {
                cut = true;
                break 'windows;
            }
            *budget -= shown.len();
            out.push_str(&shown);
        }
    }
    (out, cut)
}

/// Apply non-overlapping spans (sorted by start) to `text`; also the new
/// text's changed line ranges (as [`line_range`] gives them), counted as
/// the text is built so many spans stay linear.
#[must_use]
pub fn splice(text: &str, spans: &[Span]) -> (String, Vec<(usize, usize)>) {
    let mut out = String::with_capacity(text.len());
    let mut lines = Vec::with_capacity(spans.len());
    // Newlines in `out` so far.
    let mut newlines = 0;
    let mut at = 0;
    for span in spans {
        let kept = &text[at..span.start];
        newlines += kept.matches('\n').count();
        out.push_str(kept);
        let first = newlines;
        newlines += span.new.matches('\n').count();
        out.push_str(&span.new);
        lines.push(if span.new.is_empty() {
            (first, first)
        } else if span.new.ends_with('\n') {
            (first, newlines)
        } else {
            (first, newlines + 1)
        });
        at = span.end;
    }
    out.push_str(&text[at..]);
    (out, lines)
}

#[cfg(test)]
mod tests {
    use super::{EditMatch, EditSpec, Span, Unplaced, line_range, locate, snippet, splice};

    fn spec(old: &str, new: &str) -> EditSpec {
        EditSpec {
            old_string: old.to_owned(),
            new_string: new.to_owned(),
            replace_all: false,
        }
    }

    fn apply(text: &str, old: &str, new: &str) -> Result<(String, EditMatch), Unplaced> {
        let (spans, how) = locate(text, &spec(old, new))?;
        Ok((splice(text, &spans).0, how))
    }

    #[test]
    fn exact_first_then_normalised() {
        assert_eq!(
            apply("a\nb\n", "b", "B"),
            Ok(("a\nB\n".to_owned(), EditMatch::Exact))
        );
        // CRLF file, LF old_string: normalised, and the new text keeps CRLF.
        assert_eq!(
            apply(
                "x\r\n  if a {\r\n    b();\r\n  }\r\n",
                "if a {\n  b();\n}\n",
                "if a {\n  c();\n}\n"
            ),
            Ok((
                "x\r\n  if a {\r\n    c();\r\n  }\r\n".to_owned(),
                EditMatch::Normalised
            ))
        );
        // Trailing whitespace in the file.
        assert_eq!(
            apply("f() {   \n  g();\t\n}\n", "f() {\n  g();", "f() {\n  h();"),
            Ok(("f() {\n  h();\n}\n".to_owned(), EditMatch::Normalised))
        );
        // The pattern indented more than the file.
        assert_eq!(
            apply(
                "def f():\n  return 1\n",
                "    def f():\n      return 1\n",
                "    def f():\n      return 2\n"
            ),
            Ok(("def f():\n  return 2\n".to_owned(), EditMatch::Normalised))
        );
        // A shift that differs between lines is no match.
        assert_eq!(apply("  a\n    a\n", "a\na\n", "x"), Err(Unplaced::Absent));
        // Two normalised places without replace_all: ambiguous.
        assert_eq!(
            apply("a \nb\na\t\nb\n", "a\nb", "c"),
            Err(Unplaced::Ambiguous(2, EditMatch::Normalised))
        );
    }

    /// A shift that removes indentation must remove all of it from every
    /// `new_string` line; one indented less than the shift refuses the
    /// edit instead of flattening its relative indentation.
    #[test]
    fn an_under_indented_new_line_refuses_the_shift() {
        let file = "def f():\n  if x:\n    y()\n";
        // The pattern is indented 4 more than the file; new_string's
        // second line has only 2 of those 4.
        assert_eq!(
            apply(file, "      if x:\n        y()\n", "      if x:\n  z()\n"),
            Err(Unplaced::Unshiftable)
        );
        // Every line has the whole shift: re-indented, relative indentation
        // kept.
        assert_eq!(
            apply(
                file,
                "      if x:\n        y()\n",
                "      if x:\n        z()\n          w()\n"
            ),
            Ok((
                "def f():\n  if x:\n    z()\n      w()\n".to_owned(),
                EditMatch::Normalised
            ))
        );
        // Blank lines need no shift.
        assert_eq!(
            apply(
                file,
                "      if x:\n        y()\n",
                "      if x:\n\n        z()\n"
            ),
            Ok((
                "def f():\n  if x:\n\n    z()\n".to_owned(),
                EditMatch::Normalised
            ))
        );
    }

    /// `splice`'s line ranges equal [`line_range`]'s, span by span.
    #[test]
    fn splice_line_ranges_match_line_range() {
        let text = "a\nb\nc\nd\ne";
        let spans = vec![
            Span {
                start: 0,
                end: 2,
                new: "A\nA2\n".to_owned(),
            },
            Span {
                start: 4,
                end: 6,
                new: String::new(),
            },
            Span {
                start: 7,
                end: 9,
                new: "D".to_owned(),
            },
        ];
        let (out, ranges) = splice(text, &spans);
        assert_eq!(out, "A\nA2\nb\ndD");
        assert_eq!(ranges, [(0, 2), (3, 3), (3, 4)]);
        // The same ranges from the reference.
        let mut at = 0;
        let mut built = String::new();
        let mut expected = Vec::new();
        for span in &spans {
            built.push_str(&text[at..span.start]);
            let start = built.len();
            built.push_str(&span.new);
            expected.push((start, built.len()));
            at = span.end;
        }
        built.push_str(&text[at..]);
        let expected: Vec<_> = expected
            .into_iter()
            .map(|(start, end)| line_range(&built, start, end))
            .collect();
        assert_eq!(ranges, expected);
    }

    /// Perf guard: `replace_all` over 200k matches in a 3 MB file is
    /// linear (the file's usual ending is computed once per call, line
    /// ranges are counted while splicing). Either quadratic path took
    /// minutes here; the bound is generous for slow CI runners.
    #[test]
    fn replace_all_over_many_matches_is_linear() {
        const LINES: usize = 200_000;
        let text = "let value_x = 1;\n".repeat(LINES);
        let started = std::time::Instant::now();
        let (spans, how) = locate(
            &text,
            &EditSpec {
                old_string: "value_x".to_owned(),
                new_string: "value_y".to_owned(),
                replace_all: true,
            },
        )
        .expect("located");
        assert_eq!((spans.len(), how), (LINES, EditMatch::Exact));
        let (out, ranges) = splice(&text, &spans);
        let elapsed = started.elapsed();
        assert_eq!(out.len(), text.len());
        assert_eq!(ranges.last(), Some(&(LINES - 1, LINES)));
        assert!(
            elapsed < std::time::Duration::from_secs(10),
            "took {elapsed:?}"
        );
    }

    #[test]
    fn snippets_number_lines_merge_windows_and_respect_the_budget() {
        let text = (1..=20).fold(String::new(), |text, n| text + &format!("line {n}\n"));
        let mut budget = 4096;
        let (shown, cut) = snippet(&text, &[(9, 10)], &mut budget);
        assert!(!cut);
        assert!(shown.starts_with("     7\tline 7\n"), "{shown}");
        assert!(shown.ends_with("    13\tline 13\n"), "{shown}");
        let mut budget = 4096;
        let (shown, _) = snippet(&text, &[(1, 2), (3, 4), (17, 18)], &mut budget);
        assert_eq!(shown.matches("...").count(), 1, "{shown}");
        let mut budget = 30;
        let (shown, cut) = snippet(&text, &[(9, 10)], &mut budget);
        assert!(cut);
        assert_eq!(shown.lines().count(), 2, "{shown}");
    }
}
