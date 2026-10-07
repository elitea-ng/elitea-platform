//! Reading a model's JSON answer: `RobustJsonOutputParser`.
//!
//! The Python extractors parsed every answer with `LangChain`'s
//! `JsonOutputParser` (the whole text, else a fenced block, with control
//! characters allowed inside strings and a truncated answer completed —
//! `parse_partial_json`), and on failure with `_repair_json_text`
//! (single-quoted values, then Python literals: `True`, `None`, `'…'`).
//! The same fallbacks run here, in the same order.

use serde_json::Value;
use std::fmt::Write as _;

/// Escape raw control characters inside double-quoted strings (Python's
/// `json.loads(strict=False)` accepts them; serde does not).
fn escape_controls(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let (mut in_string, mut escaped) = (false, false);
    for c in text.chars() {
        if in_string {
            if escaped {
                escaped = false;
                out.push(c);
                continue;
            }
            match c {
                '\\' => escaped = true,
                '"' => in_string = false,
                '\n' => {
                    out.push_str("\\n");
                    continue;
                }
                '\r' => {
                    out.push_str("\\r");
                    continue;
                }
                '\t' => {
                    out.push_str("\\t");
                    continue;
                }
                c if (c as u32) < 0x20 => {
                    let _ = write!(out, "\\u{:04x}", c as u32);
                    continue;
                }
                _ => {}
            }
        } else if c == '"' {
            in_string = true;
        }
        out.push(c);
    }
    out
}

fn parse(text: &str) -> Option<Value> {
    serde_json::from_str(&escape_controls(text)).ok()
}

/// `parse_partial_json`: close what a truncated answer left open (a
/// string, then every bracket), dropping a dangling `,` or `:`.
fn parse_partial(text: &str) -> Option<Value> {
    if let Some(value) = parse(text) {
        return Some(value);
    }
    let mut stack = Vec::new();
    let (mut in_string, mut escaped) = (false, false);
    let mut completed = String::with_capacity(text.len() + 8);
    for c in text.chars() {
        completed.push(c);
        if in_string {
            if escaped {
                escaped = false;
            } else if c == '\\' {
                escaped = true;
            } else if c == '"' {
                in_string = false;
            }
            continue;
        }
        match c {
            '"' => in_string = true,
            '{' => stack.push('}'),
            '[' => stack.push(']'),
            '}' | ']' if stack.pop() != Some(c) => {
                return None;
            }
            _ => {}
        }
    }
    if in_string {
        completed.push('"');
    }
    // Drop trailing commas and colons that a cut-off answer leaves.
    loop {
        let trimmed = completed.trim_end();
        if trimmed.ends_with(',') || trimmed.ends_with(':') {
            let cut = trimmed.len() - 1;
            completed.truncate(cut);
        } else {
            break;
        }
    }
    while let Some(closer) = stack.pop() {
        completed.push(closer);
    }
    parse(&completed)
}

/// `LangChain`'s `parse_json_markdown`: the text, else what follows the first
/// fence.
fn langchain(text: &str) -> Option<Value> {
    let stripped = text.trim();
    if let Some(value) = parse(stripped) {
        return Some(value);
    }
    if let Some(start) = stripped.find("```") {
        let mut body = &stripped[start + 3..];
        body = body.strip_prefix("json").unwrap_or(body);
        let body = body.split("```").next().unwrap_or(body).trim();
        return parse_partial(body);
    }
    parse_partial(stripped)
}

/// `_FENCE_RE` of the repair: the first fenced block, else the stripped
/// text.
fn fenced(text: &str) -> &str {
    if let Some(start) = text.find("```") {
        let mut body = &text[start + 3..];
        body = body.strip_prefix("json").unwrap_or(body);
        if let Some(end) = body.find("```") {
            return body[..end].trim_matches(|c: char| c.is_whitespace());
        }
    }
    text.trim()
}

/// `_SINGLE_QUOTE_VALUE_RE` substitution: `: 'value'` becomes `: "value"`
/// with the value's double quotes escaped.
fn single_quoted_values(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut rest = text;
    while let Some(index) = rest.find(':') {
        out.push_str(&rest[..=index]);
        rest = &rest[index + 1..];
        let whitespace = rest.len() - rest.trim_start().len();
        let after = &rest[whitespace..];
        if whitespace == 0 || !after.starts_with('\'') {
            continue;
        }
        let body = &after[1..];
        let mut end = None;
        let mut chars = body.char_indices();
        while let Some((i, c)) = chars.next() {
            if c == '\\' {
                chars.next();
            } else if c == '\'' {
                end = Some(i);
                break;
            }
        }
        let Some(end) = end else {
            continue;
        };
        out.push_str(&rest[..whitespace]);
        out.push('"');
        out.push_str(&body[..end].replace('"', "\\\""));
        out.push('"');
        rest = &body[end + 1..];
    }
    out.push_str(rest);
    out
}

/// `ast.literal_eval` for the dict/list literals models write: single- or
/// double-quoted strings, `True`/`False`/`None`.
fn python_literal(text: &str) -> Option<Value> {
    let mut out = String::with_capacity(text.len());
    let chars: Vec<char> = text.chars().collect();
    let mut i = 0;
    while i < chars.len() {
        let c = chars[i];
        if c == '\'' || c == '"' {
            let quote = c;
            let mut value = String::new();
            i += 1;
            let mut closed = false;
            while i < chars.len() {
                let d = chars[i];
                if d == '\\' && i + 1 < chars.len() {
                    let next = chars[i + 1];
                    match next {
                        'n' => value.push('\n'),
                        't' => value.push('\t'),
                        'r' => value.push('\r'),
                        other => value.push(other),
                    }
                    i += 2;
                    continue;
                }
                if d == quote {
                    closed = true;
                    i += 1;
                    break;
                }
                value.push(d);
                i += 1;
            }
            if !closed {
                return None;
            }
            out.push_str(&serde_json::to_string(&value).ok()?);
            continue;
        }
        if c.is_ascii_alphabetic() {
            let start = i;
            while i < chars.len() && (chars[i].is_ascii_alphanumeric() || chars[i] == '_') {
                i += 1;
            }
            let word: String = chars[start..i].iter().collect();
            out.push_str(match word.as_str() {
                "True" => "true",
                "False" => "false",
                "None" => "null",
                _ => return None,
            });
            continue;
        }
        out.push(c);
        i += 1;
    }
    let value = parse(&out)?;
    matches!(value, Value::Array(_) | Value::Object(_)).then_some(value)
}

/// The JSON a model answered, or `None` when nothing repairs it.
#[must_use]
pub fn parse_model_json(text: &str) -> Option<Value> {
    if let Some(value) = langchain(text) {
        return Some(value);
    }
    let raw = fenced(text);
    parse(raw)
        .or_else(|| parse(&single_quoted_values(raw)))
        .or_else(|| python_literal(raw))
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn answers_parse_as_the_python_parser_read_them() {
        for (text, expected) in [
            ("[{\"a\": 1}]", json!([{"a": 1}])),
            ("  ```json\n[{\"a\": 1}]\n```  ", json!([{"a": 1}])),
            (
                "Here you go:\n```\n{\"a\": [1, 2]}\n```\nDone.",
                json!({"a": [1, 2]}),
            ),
            (
                "[{\"a\": \"line one\nline two\"}]",
                json!([{"a": "line one\nline two"}]),
            ),
            (
                "[{\"a\": 1}, {\"b\": \"cut",
                json!([{"a": 1}, {"b": "cut"}]),
            ),
            ("[{\"a\": 1},", json!([{"a": 1}])),
            (
                "[{\"selector\": '[role=\"dialog\"]', \"n\": 1}]",
                json!([{"selector": "[role=\"dialog\"]", "n": 1}]),
            ),
            (
                "[{'name': 'X', 'ok': True, 'none': None}]",
                json!([{"name": "X", "ok": true, "none": null}]),
            ),
        ] {
            assert_eq!(parse_model_json(text), Some(expected), "{text:?}");
        }
        assert_eq!(parse_model_json("I could not find anything."), None);
        assert_eq!(parse_model_json("{'a': undefined}"), None);
    }
}
