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

/// Python's `str.splitlines(keepends=True)`, over the shared boundaries in
/// `vcs_text`.
pub(crate) fn python_lines(content: &str) -> Vec<&str> {
    crate::toolkits::families::vcs_text::python_line_ranges(content)
        .into_iter()
        .map(|(start, end)| &content[start..end])
        .collect()
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
