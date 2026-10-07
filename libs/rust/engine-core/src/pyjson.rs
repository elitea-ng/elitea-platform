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
    dumps_with(value, Some(2), true)
}

/// Serialise `value` as Python's `json.dumps(value)` does: one line, with
/// `", "` and `": "` separators.
#[must_use]
pub fn dumps(value: &Value) -> String {
    dumps_with(value, None, true)
}

/// `json.dumps(value, indent=indent, ensure_ascii=ensure_ascii)`.
///
/// The wiki artifacts need both other combinations: the structure JSON is
/// `indent=4` with the default `ensure_ascii=True`, the manifest
/// `indent=2, ensure_ascii=False`.
#[must_use]
pub fn dumps_with(value: &Value, indent: Option<usize>, ensure_ascii: bool) -> String {
    let mut out = String::new();
    let style = Style {
        indent,
        ensure_ascii,
    };
    write_value(&mut out, value, style, indent.map(|_| 0));
    out
}

#[derive(Clone, Copy)]
struct Style {
    indent: Option<usize>,
    ensure_ascii: bool,
}

/// `depth` is `None` for the one-line form, else the indent level.
fn write_value(out: &mut String, value: &Value, style: Style, depth: Option<usize>) {
    match value {
        Value::Null => out.push_str("null"),
        Value::Bool(true) => out.push_str("true"),
        Value::Bool(false) => out.push_str("false"),
        Value::Number(number) => out.push_str(&number.to_string()),
        Value::String(text) => write_string(out, text, style.ensure_ascii),
        Value::Array(items) => {
            if items.is_empty() {
                out.push_str("[]");
                return;
            }
            out.push('[');
            for (index, item) in items.iter().enumerate() {
                separate(out, index, style, depth);
                write_value(out, item, style, depth.map(|d| d + 1));
            }
            close(out, style, depth);
            out.push(']');
        }
        Value::Object(map) => {
            if map.is_empty() {
                out.push_str("{}");
                return;
            }
            out.push('{');
            for (index, (key, item)) in map.iter().enumerate() {
                separate(out, index, style, depth);
                write_string(out, key, style.ensure_ascii);
                out.push_str(": ");
                write_value(out, item, style, depth.map(|d| d + 1));
            }
            close(out, style, depth);
            out.push('}');
        }
    }
}

/// What goes before the item at `index` inside a container.
fn separate(out: &mut String, index: usize, style: Style, depth: Option<usize>) {
    if index > 0 {
        out.push(',');
    }
    match depth {
        Some(depth) => newline(out, style, depth + 1),
        None if index > 0 => out.push(' '),
        None => {}
    }
}

/// What goes before a container's closing bracket.
fn close(out: &mut String, style: Style, depth: Option<usize>) {
    if let Some(depth) = depth {
        newline(out, style, depth);
    }
}

fn newline(out: &mut String, style: Style, depth: usize) {
    out.push('\n');
    let width = style.indent.unwrap_or(0);
    for _ in 0..depth * width {
        out.push(' ');
    }
}

/// Python's string encoder: `ensure_ascii` escapes every non-ASCII
/// character (surrogate pairs above the BMP); without it only the control
/// characters below U+0020 are escaped.
fn write_string(out: &mut String, text: &str, ensure_ascii: bool) {
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
            _ if !ensure_ascii && u32::from(character) >= 0x20 => out.push(character),
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
    fn matches_python_indent_four_and_no_ensure_ascii() {
        // python3 -c 'import json;print(json.dumps({"a":["é",{}]},indent=4))'
        assert_eq!(
            dumps_with(&json!({"a": ["é", {}]}), Some(4), true),
            "{\n    \"a\": [\n        \"\\u00e9\",\n        {}\n    ]\n}"
        );
        // python3 -c 'import json;print(json.dumps({"a":"é😀\u0001\x7f"},indent=2,ensure_ascii=False))'
        assert_eq!(
            dumps_with(&json!({"a": "é😀\u{1}\u{7f}"}), Some(2), false),
            "{\n  \"a\": \"é😀\\u0001\u{7f}\"\n}"
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
