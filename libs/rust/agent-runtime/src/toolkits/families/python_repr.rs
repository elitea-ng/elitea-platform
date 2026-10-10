//! Python `repr()` of JSON data, for families whose SDK tools return
//! f-strings such as `f"Extracted data:\n{data}"`.
//!
//! The model reads the SDK's output as Python literal text (single-quoted
//! strings, `True`, `None`), so a port that wants the same text renders the
//! same literal. Object members render in the order the [`Value`] holds them;
//! callers that need the SDK's insertion order pass ordered pairs to
//! [`repr_pairs`].

use std::fmt::Write as _;

use serde_json::Value;

/// `repr(value)` for a JSON value.
pub(in crate::toolkits) fn repr(value: &Value) -> String {
    let mut output = String::new();
    write_value(&mut output, value);
    output
}

/// `str(value)`, as an f-string interpolates it: text as-is, anything else
/// as its `repr`.
pub(in crate::toolkits) fn str_of(value: &Value) -> String {
    match value {
        Value::String(text) => text.clone(),
        other => repr(other),
    }
}

/// `repr(dict)` for members in the given order.
pub(in crate::toolkits) fn repr_pairs<'a>(
    pairs: impl IntoIterator<Item = (&'a str, &'a Value)>,
) -> String {
    let mut output = String::new();
    write_pairs(&mut output, pairs);
    output
}

/// `repr(str)`: single quotes unless the text holds a single quote and no
/// double quote, with Python's escapes for the chosen quote, backslash and
/// non-printable characters.
pub(in crate::toolkits) fn repr_str(value: &str) -> String {
    let mut output = String::with_capacity(value.len() + 2);
    write_str(&mut output, value);
    output
}

fn write_value(output: &mut String, value: &Value) {
    match value {
        Value::Null => output.push_str("None"),
        Value::Bool(true) => output.push_str("True"),
        Value::Bool(false) => output.push_str("False"),
        Value::Number(number) => output.push_str(&repr_number(number)),
        Value::String(text) => write_str(output, text),
        Value::Array(items) => {
            output.push('[');
            for (index, item) in items.iter().enumerate() {
                if index > 0 {
                    output.push_str(", ");
                }
                write_value(output, item);
            }
            output.push(']');
        }
        Value::Object(object) => {
            write_pairs(
                output,
                object.iter().map(|(key, value)| (key.as_str(), value)),
            );
        }
    }
}

fn write_pairs<'a>(output: &mut String, pairs: impl IntoIterator<Item = (&'a str, &'a Value)>) {
    output.push('{');
    for (index, (key, value)) in pairs.into_iter().enumerate() {
        if index > 0 {
            output.push_str(", ");
        }
        write_str(output, key);
        output.push_str(": ");
        write_value(output, value);
    }
    output.push('}');
}

/// An integer as digits; a float the way Python prints one (`1.0`, `1e+16`).
pub(in crate::toolkits) fn repr_number(number: &serde_json::Number) -> String {
    if number.is_i64() || number.is_u64() {
        return number.to_string();
    }
    let Some(float) = number.as_f64() else {
        return number.to_string();
    };
    if float.is_nan() {
        return "nan".to_owned();
    }
    if float.is_infinite() {
        return if float > 0.0 { "inf" } else { "-inf" }.to_owned();
    }
    let magnitude = float.abs();
    if magnitude != 0.0 && !(1e-4..1e16).contains(&magnitude) {
        // Rust's `{:e}` is the shortest round-trip mantissa, like Python;
        // Python spells the exponent with a sign and at least two digits.
        let rendered = format!("{float:e}");
        let (mantissa, exponent) = rendered.split_once('e').unwrap_or((&rendered, "0"));
        let (sign, digits) = exponent
            .strip_prefix('-')
            .map_or(("+", exponent), |digits| ("-", digits));
        return format!("{mantissa}e{sign}{digits:0>2}");
    }
    let rendered = format!("{float}");
    if rendered.contains('.') {
        rendered
    } else {
        format!("{rendered}.0")
    }
}

fn write_str(output: &mut String, value: &str) {
    let quote = if value.contains('\'') && !value.contains('"') {
        '"'
    } else {
        '\''
    };
    output.push(quote);
    for character in value.chars() {
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
                let code = u32::from(character);
                if code <= 0xff {
                    let _ = write!(output, "\\x{code:02x}");
                } else {
                    let _ = write!(output, "\\u{code:04x}");
                }
            }
            character => output.push(character),
        }
    }
    output.push(quote);
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::{repr, repr_pairs, repr_str};

    #[test]
    fn renders_python_literals() {
        assert_eq!(
            repr(&json!([{"a": null, "b": true, "c": [1, 2.5, 1.0]}, "x"])),
            "[{'a': None, 'b': True, 'c': [1, 2.5, 1.0]}, 'x']"
        );
        assert_eq!(repr_str("it's"), "\"it's\"");
        assert_eq!(repr_str("say \"hi\" it's"), "'say \"hi\" it\\'s'");
        assert_eq!(repr_str("a\\b\nc\u{1}"), "'a\\\\b\\nc\\x01'");
        assert_eq!(repr(&json!(1e16)), "1e+16");
        assert_eq!(repr(&json!(0.00001)), "1e-05");
        assert_eq!(repr(&json!(-0.5)), "-0.5");
        let (one, two) = (json!(1), json!("x"));
        assert_eq!(repr_pairs([("z", &one), ("a", &two)]), "{'z': 1, 'a': 'x'}");
    }
}
