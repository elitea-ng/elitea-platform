//! `json.dumps(value, indent=2)` with Python's defaults, byte for byte.
//!
//! Artifact bodies the engine returns (the manifest, the structure JSON) are
//! uploaded as the bytes they are. The Python engine wrote them with
//! `json.dumps(..., indent=2)`, whose output differs from `serde_json`'s
//! pretty printer in one way that matters: `ensure_ascii=True` escapes every
//! non-ASCII character as `\uXXXX` (surrogate pairs above the BMP). Keys keep
//! insertion order (`serde_json` is built with `preserve_order`).

use serde_json::Value;
use std::fmt::Write as _;

/// Serialise `value` as Python's `json.dumps(value, indent=2)` does.
#[must_use]
pub fn dumps_indent2(value: &Value) -> String {
    let mut out = String::new();
    write_value(&mut out, value, Some(0));
    out
}

/// Serialise `value` as Python's `json.dumps(value)` does: one line, with
/// `", "` and `": "` separators.
#[must_use]
pub fn dumps(value: &Value) -> String {
    let mut out = String::new();
    write_value(&mut out, value, None);
    out
}

/// `depth` is `None` for the one-line form, else the indent level.
fn write_value(out: &mut String, value: &Value, depth: Option<usize>) {
    match value {
        Value::Null => out.push_str("null"),
        Value::Bool(true) => out.push_str("true"),
        Value::Bool(false) => out.push_str("false"),
        Value::Number(number) => out.push_str(&number.to_string()),
        Value::String(text) => write_string(out, text),
        Value::Array(items) => {
            if items.is_empty() {
                out.push_str("[]");
                return;
            }
            out.push('[');
            for (index, item) in items.iter().enumerate() {
                separate(out, index, depth);
                write_value(out, item, depth.map(|d| d + 1));
            }
            close(out, depth);
            out.push(']');
        }
        Value::Object(map) => {
            if map.is_empty() {
                out.push_str("{}");
                return;
            }
            out.push('{');
            for (index, (key, item)) in map.iter().enumerate() {
                separate(out, index, depth);
                write_string(out, key);
                out.push_str(": ");
                write_value(out, item, depth.map(|d| d + 1));
            }
            close(out, depth);
            out.push('}');
        }
    }
}

/// What goes before the item at `index` inside a container.
fn separate(out: &mut String, index: usize, depth: Option<usize>) {
    if index > 0 {
        out.push(',');
    }
    match depth {
        Some(depth) => newline(out, depth + 1),
        None if index > 0 => out.push(' '),
        None => {}
    }
}

/// What goes before a container's closing bracket.
fn close(out: &mut String, depth: Option<usize>) {
    if let Some(depth) = depth {
        newline(out, depth);
    }
}

fn newline(out: &mut String, depth: usize) {
    out.push('\n');
    for _ in 0..depth {
        out.push_str("  ");
    }
}

/// Python's `ensure_ascii` string encoder.
fn write_string(out: &mut String, text: &str) {
    out.push('"');
    for character in text.chars() {
        match character {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{08}' => out.push_str("\\b"),
            '\u{0c}' => out.push_str("\\f"),
            ' '..='~' => out.push(character),
            _ => {
                let mut units = [0u16; 2];
                for unit in character.encode_utf16(&mut units) {
                    let _ = write!(out, "\\u{unit:04x}");
                }
            }
        }
    }
    out.push('"');
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn matches_python_indent_two() {
        // python3 -c 'import json;print(json.dumps({"a":[1,{"b":[]}],"c":{},"d":"x"},indent=2))'
        let expected = "{\n  \"a\": [\n    1,\n    {\n      \"b\": []\n    }\n  ],\n  \"c\": {},\n  \"d\": \"x\"\n}";
        assert_eq!(
            dumps_indent2(&json!({"a": [1, {"b": []}], "c": {}, "d": "x"})),
            expected
        );
    }

    #[test]
    fn matches_python_one_line() {
        // python3 -c 'import json;print(json.dumps({"a":[1,{"b":[]}],"c":{},"d":"x"}))'
        assert_eq!(
            dumps(&json!({"a": [1, {"b": []}], "c": {}, "d": "x"})),
            "{\"a\": [1, {\"b\": []}], \"c\": {}, \"d\": \"x\"}"
        );
    }

    #[test]
    fn escapes_like_ensure_ascii() {
        // python3 -c 'import json;print(json.dumps("é😀\u0001\"\\\n/"))'
        assert_eq!(
            dumps_indent2(&json!("é😀\u{1}\"\\\n/")),
            "\"\\u00e9\\ud83d\\ude00\\u0001\\\"\\\\\\n/\""
        );
    }
}
