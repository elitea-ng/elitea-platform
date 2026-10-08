//! Reading a model's JSON answer.
//!
//! The Python extractors parsed every answer with `LangChain`'s
//! `JsonOutputParser` (the whole text, else a fenced block, a truncated
//! answer completed) and on failure with a hand-written repair (single-quoted
//! values, Python literals). Here the answer's fenced block, else the whole
//! answer, is parsed as it is and otherwise repaired by `jsonrepair` (a port
//! of the JavaScript `jsonrepair`): raw control characters in strings, a
//! cut-off answer, single quotes, `True`/`None`, trailing commas, comments,
//! prose around the JSON.
//!
//! One rule of the Python repair is kept in front of it: a single-quoted
//! VALUE holding double quotes (`'[role="dialog"]'`, a CSS selector) is
//! re-quoted first, which `jsonrepair` would otherwise split at the inner
//! quote into a differently shaped document. (`llm_json`, the port of
//! Python's `json_repair`, was tried and rejected: it leaves raw control
//! characters, and unwraps a one-element array into its object.)

use serde_json::Value;

/// The first fenced block (` ``` ` or ` ```json `), else the trimmed text. An
/// unclosed fence (a cut-off answer) runs to the end.
fn fenced(text: &str) -> &str {
    let Some(start) = text.find("```") else {
        return text.trim();
    };
    let body = &text[start + 3..];
    let body = body.strip_prefix("json").unwrap_or(body);
    body.split("```").next().unwrap_or(body).trim()
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

/// The JSON a model answered — an array, or an object with something in
/// it — or `None` when nothing repairs it into one.
#[must_use]
pub fn parse_model_json(text: &str) -> Option<Value> {
    let raw = fenced(text);
    let value = serde_json::from_str(raw).ok().or_else(|| {
        jsonrepair::repair_to_value(&single_quoted_values(raw), &jsonrepair::Options::default())
            .ok()
    })?;
    match &value {
        Value::Array(_) => Some(value),
        Value::Object(fields) if !fields.is_empty() => Some(value),
        _ => None,
    }
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
            ("```json\n[{\"a\": 1}", json!([{"a": 1}])),
            (
                "[{\"selector\": '[role=\"dialog\"]', \"n\": 1}]",
                json!([{"selector": "[role=\"dialog\"]", "n": 1}]),
            ),
            (
                "[{'name': 'X', 'ok': True, 'none': None}]",
                json!([{"name": "X", "ok": true, "none": null}]),
            ),
            ("Sure! [{\"a\": 1}] Hope it helps.", json!([{"a": 1}])),
        ] {
            assert_eq!(parse_model_json(text), Some(expected), "{text:?}");
        }
        assert_eq!(parse_model_json("I could not find anything."), None);
        assert_eq!(parse_model_json(""), None);
        assert_eq!(
            parse_model_json("[{\"note\": \"x: 'y'\"}]"),
            Some(json!([{"note": "x: 'y'"}])),
            "valid JSON is never re-quoted"
        );
    }
}
