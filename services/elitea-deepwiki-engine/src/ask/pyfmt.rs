//! Python text formatting the tool results and events carry: `repr()` of
//! a parsed JSON value (`str(tool_call["args"])`), `str()` of one, and
//! code-point slicing (`text[:n]`).

use crate::parsers::python::text::{repr_float, repr_str};
use serde_json::{Number, Value};

/// `repr(value)` of the Python object `json.loads` would give: a `dict`
/// as `{'k': v}`, a `list` as `[a, b]`, `True`, `False`, `None`, an `int`
/// in decimal, a `float` as `repr(float)`.
#[must_use]
pub fn repr(value: &Value) -> String {
    let mut out = String::new();
    write_repr(&mut out, value);
    out
}

fn write_repr(out: &mut String, value: &Value) {
    match value {
        Value::Null => out.push_str("None"),
        Value::Bool(true) => out.push_str("True"),
        Value::Bool(false) => out.push_str("False"),
        Value::Number(number) => out.push_str(&number_repr(number)),
        Value::String(text) => out.push_str(&repr_str(text)),
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
        Value::Object(map) => {
            out.push('{');
            for (index, (key, item)) in map.iter().enumerate() {
                if index > 0 {
                    out.push_str(", ");
                }
                out.push_str(&repr_str(key));
                out.push_str(": ");
                write_repr(out, item);
            }
            out.push('}');
        }
    }
}

fn number_repr(number: &Number) -> String {
    if number.is_i64() || number.is_u64() {
        return number.to_string();
    }
    number
        .as_f64()
        .map_or_else(|| number.to_string(), repr_float)
}

/// `str(value)`: a string is itself, anything else its `repr`.
#[must_use]
pub fn str_of(value: &Value) -> String {
    match value {
        Value::String(text) => text.clone(),
        other => repr(other),
    }
}

/// Python truthiness of a parsed JSON value.
#[must_use]
pub fn truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(flag) => *flag,
        Value::Number(number) => number.as_f64().is_some_and(|v| v != 0.0),
        Value::String(text) => !text.is_empty(),
        Value::Array(items) => !items.is_empty(),
        Value::Object(map) => !map.is_empty(),
    }
}

/// `text[:count]` (code points).
#[must_use]
pub fn head(text: &str, count: usize) -> &str {
    crate::graph::pystr::prefix_chars(text, count)
}

/// `len(text)` (code points).
#[must_use]
pub fn len(text: &str) -> usize {
    text.chars().count()
}

/// `text[:count] + "..." if len(text) > count else text`.
#[must_use]
pub fn preview(text: &str, count: usize) -> String {
    if len(text) > count {
        format!("{}...", head(text, count))
    } else {
        text.to_owned()
    }
}

/// `str.lower()` for ASCII only: `SQLite`'s `LOWER()`.
#[must_use]
pub fn sqlite_lower(text: &str) -> String {
    text.to_ascii_lowercase()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn repr_is_python_repr() {
        assert_eq!(
            repr(&json!({"query": "it's", "k": 5, "x": 1.0, "f": [true, null]})),
            "{'query': \"it's\", 'k': 5, 'x': 1.0, 'f': [True, None]}"
        );
        assert_eq!(repr(&json!({})), "{}");
        assert_eq!(str_of(&json!("a")), "a");
        assert_eq!(str_of(&json!(3)), "3");
    }

    #[test]
    fn slicing_counts_code_points() {
        assert_eq!(head("héllo", 2), "hé");
        assert_eq!(preview("abcdef", 3), "abc...");
        assert_eq!(preview("abc", 3), "abc");
        assert_eq!(len("é"), 1);
    }
}
