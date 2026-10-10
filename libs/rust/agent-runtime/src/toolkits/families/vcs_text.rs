//! Text behaviour shared by the repository-hosting families (`gitlab`,
//! `bitbucket`): the SDK's line slicing, read guard, `grep_file` search, the
//! capped batch read's skip notice, and Python `repr` of a JSON value.
//!
//! Each function ports one SDK helper of the pinned revision
//! (`elitea_sdk/tools/utils/text_operations.py`, `utils/file_metadata.py`
//! and `BaseCodeToolApiWrapper.search_file`) so a family's output keeps the
//! shape a saved agent or pipeline was written against. The GitHub family
//! carries its own copies of the same algorithms; these are kept equal to it.

use std::fmt::Write as _;

use regex::{Regex, RegexBuilder};
use serde_json::{Map, Value, json};

/// The SDK's `DEFAULT_MAX_OUTPUT_CHARS`.
pub(in crate::toolkits) const MAX_OUTPUT_CHARS: usize = 200_000;
/// The most files one `read_multiple_files` call reads.
pub(in crate::toolkits) const MAX_BATCH_FILES: usize = 32;
pub(in crate::toolkits) const MAX_PATTERN_BYTES: usize = 4 * 1_024;
pub(in crate::toolkits) const MAX_CONTEXT_LINES: usize = 32;
const MAX_GREP_MATCHES: usize = 2_000;
const REGEX_SIZE_LIMIT: usize = 2 * 1_024 * 1_024;
const REGEX_DFA_SIZE_LIMIT: usize = 2 * 1_024 * 1_024;
const MAX_REPR_DEPTH: usize = 64;

/// The search output would exceed [`MAX_OUTPUT_CHARS`] or the match bound.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(in crate::toolkits) struct OutputExhausted;

/// Python's `str.splitlines(keepends=True)` boundaries as byte ranges.
pub(in crate::toolkits) fn python_line_ranges(content: &str) -> Vec<(usize, usize)> {
    let mut ranges = Vec::new();
    let mut start = 0_usize;
    let mut characters = content.char_indices().peekable();
    while let Some((index, character)) = characters.next() {
        let end = if character == '\r' {
            if let Some((next_index, '\n')) = characters.peek().copied() {
                let _ = characters.next();
                Some(next_index.saturating_add('\n'.len_utf8()))
            } else {
                Some(index.saturating_add(character.len_utf8()))
            }
        } else if is_python_line_break(character) {
            Some(index.saturating_add(character.len_utf8()))
        } else {
            None
        };
        if let Some(end) = end {
            ranges.push((start, end));
            start = end;
        }
    }
    if start < content.len() {
        ranges.push((start, content.len()));
    }
    ranges
}

const fn is_python_line_break(character: char) -> bool {
    matches!(
        character,
        '\n' | '\r'
            | '\u{000B}'
            | '\u{000C}'
            | '\u{001C}'
            | '\u{001D}'
            | '\u{001E}'
            | '\u{0085}'
            | '\u{2028}'
            | '\u{2029}'
    )
}

/// `apply_line_slice(content, offset=start, limit=end-start+1)`: 1-indexed,
/// inclusive, and an out-of-range request is an empty slice, not an error.
pub(in crate::toolkits) fn slice_lines(
    content: &str,
    start_line: Option<usize>,
    end_line: Option<usize>,
) -> &str {
    if start_line.is_none() && end_line.is_none() {
        return content;
    }
    let ranges = python_line_ranges(content);
    let first = start_line.unwrap_or(1).saturating_sub(1);
    if first >= ranges.len() {
        return "";
    }
    let last_exclusive = end_line.unwrap_or(ranges.len()).min(ranges.len());
    if first >= last_exclusive {
        return "";
    }
    &content[ranges[first].0..ranges[last_exclusive - 1].1]
}

/// The SDK's `requested` label: `start_line=…, end_line=…` or `full file read`.
pub(in crate::toolkits) fn requested_label(
    start_line: Option<usize>,
    end_line: Option<usize>,
) -> String {
    if start_line.is_none() && end_line.is_none() {
        return "full file read".to_owned();
    }
    let label = |value: Option<usize>| value.map_or_else(|| "None".to_owned(), |v| v.to_string());
    format!(
        "start_line={}, end_line={}",
        label(start_line),
        label(end_line)
    )
}

/// `guard_text_read`: the content itself, or the `content_too_large`
/// guidance object when it exceeds [`MAX_OUTPUT_CHARS`].
pub(in crate::toolkits) fn guard_text_read(
    content: &str,
    file_path: &str,
    requested: &str,
    full_content: &str,
) -> Value {
    let actual_chars = content.chars().count();
    if actual_chars <= MAX_OUTPUT_CHARS {
        return Value::String(content.to_owned());
    }
    let total_lines = python_line_ranges(full_content).len();
    let extension = file_extension(file_path);
    let (first_class_params, notes, full_read_allowed) = if total_lines <= 1 {
        (
            Map::new(),
            format!(
                "This file has no usable line breaks ({actual_chars} characters on a single line) and exceeds the {MAX_OUTPUT_CHARS}-character read limit. Line slicing would return the whole file, so a bounded read is not possible — the full read is refused."
            ),
            false,
        )
    } else {
        let mut params = Map::new();
        params.insert(
            "start_line".to_owned(),
            Value::String(format!(
                "integer (1-indexed, inclusive) — first line to read. Valid range 1..{total_lines}. Omit to read from the beginning."
            )),
        );
        params.insert(
            "end_line".to_owned(),
            Value::String(format!(
                "integer (1-indexed, inclusive) — last line to read. Valid range 1..{total_lines}. Omit to read to the end."
            )),
        );
        (
            params,
            "Use start_line/end_line together to read a bounded slice of a large file and keep tokens bounded."
                .to_owned(),
            full_content.chars().count() <= MAX_OUTPUT_CHARS,
        )
    };
    json!({
        "__result_status__": "content_too_large",
        "context": {
            "actual_chars": actual_chars,
            "limit_chars": MAX_OUTPUT_CHARS,
            "requested": requested
        },
        "extension": extension,
        "filename": file_path,
        "instruction_for_readFile": {
            "extra_params": {},
            "first_class_params": first_class_params,
            "notes": notes
        },
        "read_limits": {
            "full_read_allowed": full_read_allowed,
            "max_output_chars": MAX_OUTPUT_CHARS
        },
        "schema_version": "1.0",
        "total_lines": total_lines,
        "type": mime_type(extension),
        "unit": "lines"
    })
}

fn file_extension(file_path: &str) -> &str {
    let filename = file_path.rsplit('/').next().unwrap_or(file_path);
    filename
        .rfind('.')
        .filter(|index| *index > 0)
        .map_or("", |index| &filename[index..])
}

fn mime_type(extension: &str) -> &'static str {
    match extension.to_ascii_lowercase().as_str() {
        ".py" => "text/x-python",
        ".rs" => "text/x-rust",
        ".js" => "text/javascript",
        ".ts" => "text/typescript",
        ".json" => "application/json",
        ".md" => "text/markdown",
        ".yaml" | ".yml" => "application/yaml",
        ".html" => "text/html",
        ".css" => "text/css",
        ".csv" => "text/csv",
        ".txt" | ".log" => "text/plain",
        _ => "application/octet-stream",
    }
}

/// `measure_result_chars`: a string's length, else its JSON rendering's.
pub(in crate::toolkits) fn measure_result_chars(value: &Value) -> usize {
    match value {
        Value::String(value) => value.chars().count(),
        _ => serde_json::to_string(value).map_or(MAX_OUTPUT_CHARS, |value| value.chars().count()),
    }
}

/// `capped_read_multiple_files`' notice once the batch budget is spent.
pub(in crate::toolkits) fn batch_skip_notice() -> String {
    format!(
        "Skipped: the batch's cumulative {MAX_OUTPUT_CHARS}-character read limit was already reached by earlier files in this call. Read this file individually with read_file."
    )
}

/// `search_in_content`'s pattern: case-insensitive, escaped when literal.
/// `None` is an invalid regular expression, which the SDK answers with "no
/// matches" rather than an error.
pub(in crate::toolkits) fn compile_pattern(pattern: &str, is_regex: bool) -> Option<Regex> {
    let source = if is_regex {
        pattern.to_owned()
    } else {
        regex::escape(pattern)
    };
    RegexBuilder::new(&source)
        .case_insensitive(true)
        .size_limit(REGEX_SIZE_LIMIT)
        .dfa_size_limit(REGEX_DFA_SIZE_LIMIT)
        .build()
        .ok()
}

/// `BaseCodeToolApiWrapper.search_file`'s formatted result.
pub(in crate::toolkits) fn grep_content(
    content: &str,
    file_path: &str,
    pattern: &str,
    expression: Option<&Regex>,
    context_lines: usize,
) -> Result<String, OutputExhausted> {
    let ranges = python_line_ranges(content);
    let lines = ranges
        .iter()
        .map(|(start, end)| content[*start..*end].trim_end_matches(is_python_line_break))
        .collect::<Vec<_>>();
    let mut matches = Vec::new();
    if let Some(expression) = expression {
        for (index, line) in lines.iter().enumerate() {
            if expression.is_match(line) {
                if matches.len() >= MAX_GREP_MATCHES {
                    return Err(OutputExhausted);
                }
                matches.push(index);
            }
        }
    }
    if matches.is_empty() {
        return Ok(format!(
            "No matches found for pattern '{pattern}' in {file_path}"
        ));
    }
    let mut output = format!(
        "Found {} match(es) for pattern '{pattern}' in {file_path}:\n",
        matches.len()
    );
    let mut output_chars = output.chars().count();
    if output_chars > MAX_OUTPUT_CHARS {
        return Err(OutputExhausted);
    }
    for (match_number, line_index) in matches.into_iter().enumerate() {
        push_bounded(
            &mut output,
            &mut output_chars,
            &format!(
                "\n\n--- Match {} at line {} ---",
                match_number + 1,
                line_index + 1
            ),
        )?;
        let context_start = line_index.saturating_sub(context_lines);
        for line in &lines[context_start..line_index] {
            push_bounded(&mut output, &mut output_chars, "\n  ")?;
            push_bounded(&mut output, &mut output_chars, line)?;
        }
        push_bounded(&mut output, &mut output_chars, "\n> ")?;
        push_bounded(&mut output, &mut output_chars, lines[line_index])?;
        let context_end = line_index
            .saturating_add(context_lines)
            .saturating_add(1)
            .min(lines.len());
        for line in &lines[line_index.saturating_add(1)..context_end] {
            push_bounded(&mut output, &mut output_chars, "\n  ")?;
            push_bounded(&mut output, &mut output_chars, line)?;
        }
    }
    Ok(output)
}

fn push_bounded(
    output: &mut String,
    output_chars: &mut usize,
    value: &str,
) -> Result<(), OutputExhausted> {
    let next = output_chars
        .checked_add(value.chars().count())
        .ok_or(OutputExhausted)?;
    if next > MAX_OUTPUT_CHARS {
        return Err(OutputExhausted);
    }
    output.push_str(value);
    *output_chars = next;
    Ok(())
}

/// Python's `repr` of the value `json.loads` would produce, as the SDK's
/// `str(dict)` / `str(list)` renders a provider response inside a message.
pub(in crate::toolkits) fn python_repr(value: &Value) -> String {
    let mut output = String::new();
    push_repr(&mut output, value, 0);
    output
}

fn push_repr(output: &mut String, value: &Value, depth: usize) {
    if depth > MAX_REPR_DEPTH {
        output.push_str("...");
        return;
    }
    match value {
        Value::Null => output.push_str("None"),
        Value::Bool(true) => output.push_str("True"),
        Value::Bool(false) => output.push_str("False"),
        Value::Number(number) => output.push_str(&number.to_string()),
        Value::String(text) => push_quoted(output, text),
        Value::Array(values) => {
            output.push('[');
            for (index, value) in values.iter().enumerate() {
                if index > 0 {
                    output.push_str(", ");
                }
                push_repr(output, value, depth + 1);
            }
            output.push(']');
        }
        Value::Object(values) => {
            output.push('{');
            for (index, (key, value)) in values.iter().enumerate() {
                if index > 0 {
                    output.push_str(", ");
                }
                push_quoted(output, key);
                output.push_str(": ");
                push_repr(output, value, depth + 1);
            }
            output.push('}');
        }
    }
}

/// Python's string `repr`: single quotes unless the text holds a single
/// quote and no double quote.
fn push_quoted(output: &mut String, text: &str) {
    let quote = if text.contains('\'') && !text.contains('"') {
        '"'
    } else {
        '\''
    };
    output.push(quote);
    for character in text.chars() {
        match character {
            '\\' => output.push_str("\\\\"),
            '\n' => output.push_str("\\n"),
            '\r' => output.push_str("\\r"),
            '\t' => output.push_str("\\t"),
            character if character == quote => {
                output.push('\\');
                output.push(character);
            }
            character if character.is_control() => {
                let _ = write!(output, "\\x{:02x}", u32::from(character));
            }
            character => output.push(character),
        }
    }
    output.push(quote);
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn slices_follow_apply_line_slice() {
        let text = "a\nb\r\nc\rd";
        assert_eq!(slice_lines(text, Some(2), Some(3)), "b\r\nc\r");
        assert_eq!(slice_lines(text, Some(9), None), "");
        assert_eq!(slice_lines(text, Some(3), Some(2)), "");
        assert_eq!(
            requested_label(None, Some(4)),
            "start_line=None, end_line=4"
        );
        assert_eq!(requested_label(None, None), "full file read");
    }

    #[test]
    fn grep_matches_the_sdk_layout_and_tolerates_bad_regex() {
        let content = "alpha\nbeta\ngamma\nBETA two";
        let expression = compile_pattern("beta", true);
        let found = grep_content(content, "f.txt", "beta", expression.as_ref(), 1).expect("grep");
        assert_eq!(
            found,
            "Found 2 match(es) for pattern 'beta' in f.txt:\n\n\n--- Match 1 at line 2 ---\n  alpha\n> beta\n  gamma\n\n--- Match 2 at line 4 ---\n  gamma\n> BETA two"
        );
        let invalid = compile_pattern("(", true);
        assert!(invalid.is_none());
        assert_eq!(
            grep_content(content, "f.txt", "(", invalid.as_ref(), 2).expect("no match"),
            "No matches found for pattern '(' in f.txt"
        );
        assert!(compile_pattern("(", false).is_some());
    }

    #[test]
    fn repr_matches_python() {
        let value = json!({"a": [1, true, null, "it's"], "b": "x\ny", "c": {"d": false}});
        assert_eq!(
            python_repr(&value),
            "{'a': [1, True, None, \"it's\"], 'b': 'x\\ny', 'c': {'d': False}}"
        );
    }

    #[test]
    fn oversized_reads_return_guidance() {
        let big = "x\n".repeat(MAX_OUTPUT_CHARS);
        let guarded = guard_text_read(&big, "src/a.py", "full file read", &big);
        assert_eq!(guarded["__result_status__"], "content_too_large");
        assert_eq!(guarded["type"], "text/x-python");
        assert_eq!(guarded["total_lines"], MAX_OUTPUT_CHARS);
        assert_eq!(
            guard_text_read("small", "a", "full file read", "small"),
            Value::String("small".to_owned())
        );
    }
}
