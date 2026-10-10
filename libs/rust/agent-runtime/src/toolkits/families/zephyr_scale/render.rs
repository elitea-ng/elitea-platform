//! The SDK's text renderings of provider objects.
//!
//! The Zephyr Scale wrapper builds its results with `str(...)`, f-strings
//! and `json.dumps(..., indent=2)`. These functions reproduce those forms for
//! JSON values. Object members are written in sorted key order, whatever
//! `serde_json` features a build unifies (see `crate::canonical`): the
//! provider's own member order is the one difference from the SDK's text.

use std::fmt::Write as _;

use serde_json::{Map, Value};

use crate::toolkits::families::zephyr_rest::client::ZephyrReply;

/// Python's `repr(value)` for a decoded JSON value.
#[must_use]
pub(in crate::toolkits) fn py_repr(value: &Value) -> String {
    let mut out = String::new();
    write_repr(&mut out, value);
    out
}

/// Python's `str(value)` (and `f"{value}"`): a string is itself, anything
/// else its `repr`.
#[must_use]
pub(in crate::toolkits) fn py_str(value: &Value) -> String {
    match value {
        Value::String(text) => text.clone(),
        other => py_repr(other),
    }
}

/// `f"{value}"` of a member read with `.get()`: a missing member is `None`.
#[must_use]
pub(in crate::toolkits) fn py_str_opt(value: Option<&Value>) -> String {
    value.map_or_else(|| "None".to_owned(), py_str)
}

/// `str(response)` of a library call: the JSON body's `str`, or `""`.
#[must_use]
pub(in crate::toolkits) fn reply_str(reply: &ZephyrReply) -> String {
    match reply {
        ZephyrReply::Json(value) => py_str(value),
        ZephyrReply::Text(text) => text.clone(),
        ZephyrReply::Empty => String::new(),
    }
}

/// `str(list_of_strings)`.
#[must_use]
pub(in crate::toolkits) fn py_repr_str_list(items: &[String]) -> String {
    let mut out = String::from("[");
    for (index, item) in items.iter().enumerate() {
        if index > 0 {
            out.push_str(", ");
        }
        write_repr_str(&mut out, item);
    }
    out.push(']');
    out
}

fn write_repr(out: &mut String, value: &Value) {
    match value {
        Value::Null => out.push_str("None"),
        Value::Bool(true) => out.push_str("True"),
        Value::Bool(false) => out.push_str("False"),
        Value::Number(number) => {
            let _ = write!(out, "{number}");
        }
        Value::String(text) => write_repr_str(out, text),
        Value::Array(items) => {
            out.push('[');
            for (index, item) in items.iter().enumerate() {
                if index > 0 {
                    out.push_str(", ");
                }
                write_repr(out, item);
            }
            out.push(']');
        }
        Value::Object(object) => {
            out.push('{');
            for (index, (key, item)) in sorted(object).into_iter().enumerate() {
                if index > 0 {
                    out.push_str(", ");
                }
                write_repr_str(out, key);
                out.push_str(": ");
                write_repr(out, item);
            }
            out.push('}');
        }
    }
}

/// Python's `repr(str)`: single quotes unless the text has a single quote and
/// no double quote, with Python's escapes for the quote, backslash and
/// non-printable characters.
fn write_repr_str(out: &mut String, text: &str) {
    let quote = if text.contains('\'') && !text.contains('"') {
        '"'
    } else {
        '\''
    };
    out.push(quote);
    for character in text.chars() {
        match character {
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            character if character == quote => {
                out.push('\\');
                out.push(character);
            }
            character
                if u32::from(character) < 0x20 || (0x7f..=0xa0).contains(&u32::from(character)) =>
            {
                let _ = write!(out, "\\x{:02x}", u32::from(character));
            }
            character => out.push(character),
        }
    }
    out.push(quote);
}

fn sorted(object: &Map<String, Value>) -> Vec<(&String, &Value)> {
    let mut members = object.iter().collect::<Vec<_>>();
    members.sort_by(|left, right| left.0.cmp(right.0));
    members
}

/// One object whose members keep the caller's order, as a Python `dict`
/// built field by field does.
pub(in crate::toolkits) type OrderedObject = Vec<(String, Value)>;

/// `json.dumps(list_of_dicts, indent=2)` with Python's defaults
/// (`ensure_ascii=True`, `", "`/`": "` separators under an indent).
#[must_use]
pub(in crate::toolkits) fn json_dumps_indent(objects: &[OrderedObject]) -> String {
    let mut out = String::new();
    if objects.is_empty() {
        out.push_str("[]");
        return out;
    }
    out.push('[');
    for (index, object) in objects.iter().enumerate() {
        out.push_str(if index == 0 { "\n" } else { ",\n" });
        indent(&mut out, 1);
        if object.is_empty() {
            out.push_str("{}");
            continue;
        }
        out.push('{');
        for (member, (key, value)) in object.iter().enumerate() {
            out.push_str(if member == 0 { "\n" } else { ",\n" });
            indent(&mut out, 2);
            write_json_str(&mut out, key);
            out.push_str(": ");
            write_json(&mut out, value, 2);
        }
        out.push('\n');
        indent(&mut out, 1);
        out.push('}');
    }
    out.push_str("\n]");
    out
}

fn indent(out: &mut String, level: usize) {
    for _ in 0..level {
        out.push_str("  ");
    }
}

fn write_json(out: &mut String, value: &Value, level: usize) {
    match value {
        Value::Null => out.push_str("null"),
        Value::Bool(flag) => out.push_str(if *flag { "true" } else { "false" }),
        Value::Number(number) => {
            let _ = write!(out, "{number}");
        }
        Value::String(text) => write_json_str(out, text),
        Value::Array(items) if items.is_empty() => out.push_str("[]"),
        Value::Array(items) => {
            out.push('[');
            for (index, item) in items.iter().enumerate() {
                out.push_str(if index == 0 { "\n" } else { ",\n" });
                indent(out, level + 1);
                write_json(out, item, level + 1);
            }
            out.push('\n');
            indent(out, level);
            out.push(']');
        }
        Value::Object(object) if object.is_empty() => out.push_str("{}"),
        Value::Object(object) => {
            out.push('{');
            for (index, (key, item)) in sorted(object).into_iter().enumerate() {
                out.push_str(if index == 0 { "\n" } else { ",\n" });
                indent(out, level + 1);
                write_json_str(out, key);
                out.push_str(": ");
                write_json(out, item, level + 1);
            }
            out.push('\n');
            indent(out, level);
            out.push('}');
        }
    }
}

/// A JSON string with `ensure_ascii`: every non-ASCII code point becomes
/// `\uXXXX` (a surrogate pair above the BMP), as Python writes it.
fn write_json_str(out: &mut String, text: &str) {
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
            character if character.is_ascii() && !character.is_ascii_control() => {
                out.push(character);
            }
            character => {
                let mut units = [0_u16; 2];
                for unit in character.encode_utf16(&mut units) {
                    let _ = write!(out, "\\u{unit:04x}");
                }
            }
        }
    }
    out.push('"');
}
