//! Python's view of a JSON value: `str()`, truthiness and `repr()`.
//!
//! The engines answer with the strings and branches the Python engines
//! produced, so where those read a JSON value through Python semantics, the
//! Rust ports read it through these.

use serde_json::Value;
use std::fmt::Write as _;

/// Python's `str(value)` for the JSON values a repository field can hold.
#[must_use]
pub fn py_str(value: &Value) -> String {
    match value {
        Value::String(text) => text.clone(),
        Value::Null => "None".to_owned(),
        Value::Bool(true) => "True".to_owned(),
        Value::Bool(false) => "False".to_owned(),
        Value::Number(number) => number.to_string(),
        other => other.to_string(),
    }
}

/// Python truthiness of a JSON value.
#[must_use]
pub fn py_truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(flag) => *flag,
        Value::Number(number) => number.as_f64().is_some_and(|n| n != 0.0),
        Value::String(text) => !text.is_empty(),
        Value::Array(items) => !items.is_empty(),
        Value::Object(map) => !map.is_empty(),
    }
}

/// Python's `repr()` of a plain string, close enough for messages: single
/// quotes unless the text holds one and no double quote.
#[must_use]
pub fn py_repr(text: &str) -> String {
    let quote = if text.contains('\'') && !text.contains('"') {
        '"'
    } else {
        '\''
    };
    let mut out = String::from(quote);
    for character in text.chars() {
        match character {
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if c == quote => {
                out.push('\\');
                out.push(c);
            }
            // Python escapes the other C0/C1 control characters as `\xNN`.
            c if matches!(u32::from(c), 0..=0x1f | 0x7f..=0x9f) => {
                let _ = write!(out, "\\x{:02x}", u32::from(c));
            }
            c => out.push(c),
        }
    }
    out.push(quote);
    out
}

/// Python's `repr()` of a string, exactly: the quote rule of [`py_repr`],
/// printable characters as they are, and every other character escaped as
/// `\xNN`, `\uNNNN` or `\UNNNNNNNN` (`str.isprintable`).
#[must_use]
pub fn repr_text(text: &str) -> String {
    let quote = if text.contains('\'') && !text.contains('"') {
        '"'
    } else {
        '\''
    };
    let mut out = String::with_capacity(text.len() + 2);
    out.push(quote);
    for c in text.chars() {
        match c {
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if c == quote => {
                out.push('\\');
                out.push(c);
            }
            c if crate::pystr::is_printable(c) => out.push(c),
            c => {
                let code = u32::from(c);
                let _ = if code < 0x100 {
                    write!(out, "\\x{code:02x}")
                } else if code < 0x1_0000 {
                    write!(out, "\\u{code:04x}")
                } else {
                    write!(out, "\\U{code:08x}")
                };
            }
        }
    }
    out.push(quote);
    out
}

/// Python's `repr()` of the object `json.loads` gives for `value`: a dict
/// as `{'k': v}`, a list as `[a, b]`, `True`, `False`, `None`, an int in
/// decimal, a float as `repr(float)`.
#[must_use]
pub fn repr_value(value: &Value) -> String {
    let mut out = String::new();
    write_repr(&mut out, value);
    out
}

fn write_repr(out: &mut String, value: &Value) {
    match value {
        Value::Null => out.push_str("None"),
        Value::Bool(true) => out.push_str("True"),
        Value::Bool(false) => out.push_str("False"),
        Value::Number(number) if number.is_i64() || number.is_u64() => {
            out.push_str(&number.to_string());
        }
        Value::Number(number) => match number.as_f64() {
            Some(float) => out.push_str(&crate::pyjson::float_repr(float)),
            None => out.push_str(&number.to_string()),
        },
        Value::String(text) => out.push_str(&repr_text(text)),
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
                out.push_str(&repr_text(key));
                out.push_str(": ");
                write_repr(out, item);
            }
            out.push('}');
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn repr_spells_python_objects() {
        assert_eq!(
            repr_value(&json!({"a": [1, 2.5, null, true], "b'": "x\u{200b}\n"})),
            "{'a': [1, 2.5, None, True], \"b'\": 'x\\u200b\\n'}"
        );
        assert_eq!(repr_value(&json!(1e-7)), "1e-07");
        assert_eq!(repr_text("é\u{7f}"), "'é\\x7f'");
    }

    #[test]
    fn str_spells_python_scalars() {
        assert_eq!(py_str(&json!(null)), "None");
        assert_eq!(py_str(&json!(true)), "True");
        assert_eq!(py_str(&json!("a")), "a");
        assert_eq!(py_str(&json!(3)), "3");
    }

    #[test]
    fn truthiness_follows_python() {
        for falsy in [
            json!(null),
            json!(false),
            json!(0),
            json!(0.0),
            json!(""),
            json!([]),
            json!({}),
        ] {
            assert!(!py_truthy(&falsy), "{falsy} should be falsy");
        }
        for truthy in [
            json!(true),
            json!(1),
            json!("x"),
            json!([0]),
            json!({"k": 0}),
        ] {
            assert!(py_truthy(&truthy), "{truthy} should be truthy");
        }
    }

    #[test]
    fn repr_picks_quotes_and_escapes_controls() {
        assert_eq!(py_repr("a"), "'a'");
        assert_eq!(py_repr("it's"), "\"it's\"");
        assert_eq!(py_repr("a'b\""), "'a\\'b\"'");
        assert_eq!(py_repr("\u{1}\n"), "'\\x01\\n'");
    }
}
