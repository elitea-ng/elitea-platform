//! Result shapes the `azure-devops` Python client gives the SDK.
//!
//! The SDK returns most provider objects through msrest `Model.as_dict()`,
//! which renames every attribute to its `snake_case` Python name, and embeds a
//! few lists in strings with Python's `str()`. These helpers reproduce both
//! over the raw REST JSON.

use std::fmt::Write as _;

use serde_json::{Map, Value};

/// Members whose VALUE is a free-form map in the msrest models (`{object}`
/// attributes): work item `fields`, relation `attributes`, `_links`, and the
/// test plan `workItemFields` list. Their own keys are field reference names
/// such as `System.Title` and are kept verbatim.
const VERBATIM_MEMBERS: &[&str] = &["fields", "attributes", "_links", "workItemFields"];

/// `Model.as_dict()` over raw REST JSON: object keys become `snake_case`,
/// recursively, except inside the free-form members above.
pub(crate) fn as_dict(value: &Value) -> Value {
    match value {
        Value::Object(object) => {
            let mut converted = Map::with_capacity(object.len());
            for (key, member) in object {
                let member = if VERBATIM_MEMBERS.contains(&key.as_str()) {
                    member.clone()
                } else {
                    as_dict(member)
                };
                converted.insert(snake_case(key), member);
            }
            Value::Object(converted)
        }
        Value::Array(values) => Value::Array(values.iter().map(as_dict).collect()),
        other => other.clone(),
    }
}

/// msrest's attribute name for a REST member: `pullRequestId` →
/// `pull_request_id`, `_links` stays `_links`.
pub(crate) fn snake_case(key: &str) -> String {
    let characters = key.chars().collect::<Vec<_>>();
    let mut output = String::with_capacity(key.len() + 4);
    for (index, character) in characters.iter().enumerate() {
        if character.is_ascii_uppercase() {
            let previous = index.checked_sub(1).map(|at| characters[at]);
            let next = characters.get(index + 1);
            let boundary = previous.is_some_and(|previous| {
                previous.is_ascii_lowercase()
                    || previous.is_ascii_digit()
                    || (previous.is_ascii_uppercase() && next.is_some_and(char::is_ascii_lowercase))
            });
            if boundary {
                output.push('_');
            }
            output.push(character.to_ascii_lowercase());
        } else {
            output.push(*character);
        }
    }
    output
}

/// The REST member name for a Python attribute: `area_path` → `areaPath`.
pub(crate) fn camel_case(key: &str) -> String {
    let mut output = String::with_capacity(key.len());
    let mut upper = false;
    for (index, character) in key.chars().enumerate() {
        if character == '_' && index > 0 {
            upper = true;
        } else if upper {
            output.push(character.to_ascii_uppercase());
            upper = false;
        } else {
            output.push(character);
        }
    }
    output
}

/// Python `repr()` of a JSON value as the SDK's `str(list_of_dicts)` prints
/// it: `None`, `True`, single-quoted strings, `{'k': v}`.
pub(crate) fn python_repr(value: &Value) -> String {
    let mut output = String::new();
    write_python(value, &mut output);
    output
}

fn write_python(value: &Value, output: &mut String) {
    match value {
        Value::Null => output.push_str("None"),
        Value::Bool(true) => output.push_str("True"),
        Value::Bool(false) => output.push_str("False"),
        Value::Number(number) => output.push_str(&number.to_string()),
        Value::String(text) => output.push_str(&python_str_repr(text)),
        Value::Array(values) => {
            output.push('[');
            for (index, value) in values.iter().enumerate() {
                if index > 0 {
                    output.push_str(", ");
                }
                write_python(value, output);
            }
            output.push(']');
        }
        Value::Object(object) => {
            output.push('{');
            for (index, (key, value)) in object.iter().enumerate() {
                if index > 0 {
                    output.push_str(", ");
                }
                output.push_str(&python_str_repr(key));
                output.push_str(": ");
                write_python(value, output);
            }
            output.push('}');
        }
    }
}

/// Python `repr(str)`: single quotes unless the text holds a single quote
/// and no double quote.
pub(crate) fn python_str_repr(text: &str) -> String {
    let quote = if text.contains('\'') && !text.contains('"') {
        '"'
    } else {
        '\''
    };
    let mut output = String::with_capacity(text.len() + 2);
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
            character if character.is_control() || matches!(character, '\u{2028}' | '\u{2029}') => {
                let code = u32::from(character);
                if code < 0x100 {
                    let _ = write!(output, "\\x{code:02x}");
                } else {
                    let _ = write!(output, "\\u{code:04x}");
                }
            }
            character => output.push(character),
        }
    }
    output.push(quote);
    output
}

/// Python's `str.splitlines(keepends=True)` boundaries.
pub(crate) fn python_lines(content: &str) -> Vec<&str> {
    let mut lines = Vec::new();
    let mut start = 0usize;
    let mut characters = content.char_indices().peekable();
    while let Some((index, character)) = characters.next() {
        let end = match character {
            '\r' => {
                if characters.peek().is_some_and(|(_, next)| *next == '\n') {
                    let (next_index, _) = characters.next().unwrap_or((index, '\n'));
                    next_index + 1
                } else {
                    index + 1
                }
            }
            '\n' | '\u{000B}' | '\u{000C}' | '\u{001C}' | '\u{001D}' | '\u{001E}' | '\u{0085}'
            | '\u{2028}' | '\u{2029}' => index + character.len_utf8(),
            _ => continue,
        };
        lines.push(&content[start..end]);
        start = end;
    }
    if start < content.len() {
        lines.push(&content[start..]);
    }
    lines
}

/// `json.dumps(str)`: ASCII-only, as Python's default `ensure_ascii=True`.
pub(crate) fn python_json_string(value: &str) -> String {
    let mut output = String::with_capacity(value.len() + 2);
    output.push('"');
    for character in value.chars() {
        match character {
            '"' => output.push_str("\\\""),
            '\\' => output.push_str("\\\\"),
            '\n' => output.push_str("\\n"),
            '\r' => output.push_str("\\r"),
            '\t' => output.push_str("\\t"),
            '\u{08}' => output.push_str("\\b"),
            '\u{0C}' => output.push_str("\\f"),
            character if character.is_ascii() && !character.is_ascii_control() => {
                output.push(character);
            }
            character => {
                let mut units = [0u16; 2];
                for unit in character.encode_utf16(&mut units) {
                    let _ = write!(output, "\\u{unit:04x}");
                }
            }
        }
    }
    output.push('"');
    output
}
