//! The SDK is Python, and some of its outputs are text a model or a user
//! reads: `json.dumps(docs, indent=4)`, an f-string of a score, `str()` of a
//! list of dicts inside a prompt. This module prints [`serde_json::Value`]s
//! the way `CPython` 3 does, so those strings stay the same.

use std::fmt::Write as _;

use serde_json::Value;

/// `repr(float)`: the shortest digits that round-trip, fixed notation for
/// decimal exponents in `-4..16`, scientific (`1e-05`, `1.5e+16`) outside.
#[must_use]
pub fn float_repr(value: f64) -> String {
    if value.is_nan() {
        return "nan".to_owned();
    }
    if value.is_infinite() {
        return if value < 0.0 { "-inf" } else { "inf" }.to_owned();
    }
    let scientific = format!("{value:e}");
    let (mantissa, exponent) = scientific.split_once('e').unwrap_or((&scientific, "0"));
    let exponent: i32 = exponent.parse().unwrap_or(0);
    let negative = mantissa.starts_with('-');
    let digits: String = mantissa.chars().filter(char::is_ascii_digit).collect();
    let sign = if negative { "-" } else { "" };
    if (-4..16).contains(&exponent) {
        if exponent >= 0 {
            let integer_len = usize::try_from(exponent).unwrap_or(0) + 1;
            let padded = format!("{digits:0<integer_len$}");
            let (integer, fraction) = padded.split_at(integer_len);
            let fraction = if fraction.is_empty() { "0" } else { fraction };
            format!("{sign}{integer}.{fraction}")
        } else {
            let zeros = "0".repeat(usize::try_from(-exponent - 1).unwrap_or(0));
            format!("{sign}0.{zeros}{digits}")
        }
    } else {
        let (head, tail) = digits.split_at(1);
        let fraction = if tail.is_empty() {
            String::new()
        } else {
            format!(".{tail}")
        };
        let exponent_sign = if exponent < 0 { '-' } else { '+' };
        format!(
            "{sign}{head}{fraction}e{exponent_sign}{:02}",
            exponent.abs()
        )
    }
}

fn number_repr(number: &serde_json::Number) -> String {
    if let Some(integer) = number.as_i64() {
        integer.to_string()
    } else if let Some(integer) = number.as_u64() {
        integer.to_string()
    } else {
        float_repr(number.as_f64().unwrap_or(f64::NAN))
    }
}

/// `repr(str)`: single quotes unless the text holds a `'` and no `"`.
fn string_repr(text: &str) -> String {
    let quote = if text.contains('\'') && !text.contains('"') {
        '"'
    } else {
        '\''
    };
    let mut out = String::with_capacity(text.len() + 2);
    out.push(quote);
    for character in text.chars() {
        match character {
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            other if other == quote => {
                out.push('\\');
                out.push(other);
            }
            other if (other as u32) < 0x20 || (0x7f..=0x9f).contains(&(other as u32)) => {
                let _ = write!(out, "\\x{:02x}", other as u32);
            }
            other => out.push(other),
        }
    }
    out.push(quote);
    out
}

/// `repr(value)` of the Python object `json.loads` would have made.
#[must_use]
pub fn repr(value: &Value) -> String {
    match value {
        Value::Null => "None".to_owned(),
        Value::Bool(true) => "True".to_owned(),
        Value::Bool(false) => "False".to_owned(),
        Value::Number(number) => number_repr(number),
        Value::String(text) => string_repr(text),
        Value::Array(items) => {
            let inner: Vec<String> = items.iter().map(repr).collect();
            format!("[{}]", inner.join(", "))
        }
        Value::Object(map) => {
            let inner: Vec<String> = map
                .iter()
                .map(|(key, item)| format!("{}: {}", string_repr(key), repr(item)))
                .collect();
            format!("{{{}}}", inner.join(", "))
        }
    }
}

/// `str(value)`: a string prints as itself, anything else as its `repr`.
#[must_use]
pub fn display(value: &Value) -> String {
    match value {
        Value::String(text) => text.clone(),
        other => repr(other),
    }
}

fn json_string(text: &str, out: &mut String) {
    out.push('"');
    for character in text.chars() {
        match character {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{8}' => out.push_str("\\b"),
            '\u{c}' => out.push_str("\\f"),
            other if (other as u32) < 0x20 => {
                let _ = write!(out, "\\u{:04x}", other as u32);
            }
            other if other.is_ascii() => out.push(other),
            other => {
                let mut units = [0_u16; 2];
                for unit in other.encode_utf16(&mut units) {
                    let _ = write!(out, "\\u{unit:04x}");
                }
            }
        }
    }
    out.push('"');
}

fn dump(value: &Value, level: usize, indent: usize, out: &mut String) {
    let pad = |depth: usize, out: &mut String| out.push_str(&" ".repeat(depth * indent));
    match value {
        Value::Null => out.push_str("null"),
        Value::Bool(flag) => out.push_str(if *flag { "true" } else { "false" }),
        Value::Number(number) => out.push_str(&number_repr(number)),
        Value::String(text) => json_string(text, out),
        Value::Array(items) if items.is_empty() => out.push_str("[]"),
        Value::Array(items) => {
            out.push_str("[\n");
            for (position, item) in items.iter().enumerate() {
                if position > 0 {
                    out.push_str(",\n");
                }
                pad(level + 1, out);
                dump(item, level + 1, indent, out);
            }
            out.push('\n');
            pad(level, out);
            out.push(']');
        }
        Value::Object(map) if map.is_empty() => out.push_str("{}"),
        Value::Object(map) => {
            out.push_str("{\n");
            for (position, (key, item)) in map.iter().enumerate() {
                if position > 0 {
                    out.push_str(",\n");
                }
                pad(level + 1, out);
                json_string(key, out);
                out.push_str(": ");
                dump(item, level + 1, indent, out);
            }
            out.push('\n');
            pad(level, out);
            out.push('}');
        }
    }
}

/// `json.dumps(value, indent=indent)`: `ensure_ascii` on, key order kept.
#[must_use]
pub fn json_dumps(value: &Value, indent: usize) -> String {
    let mut out = String::new();
    dump(value, 0, indent, &mut out);
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn floats_print_like_python() {
        let cases = [
            (0.5, "0.5"),
            (1.0, "1.0"),
            (0.0, "0.0"),
            (-2.25, "-2.25"),
            (100.0, "100.0"),
            (0.0001, "0.0001"),
            (0.00001, "1e-05"),
            (1.5e-7, "1.5e-07"),
            (1e16, "1e+16"),
            (1.5e16, "1.5e+16"),
            (123_456_789_012_345.0, "123456789012345.0"),
            (0.871_234_512_329_101_7, "0.8712345123291017"),
        ];
        for (value, expected) in cases {
            assert_eq!(float_repr(value), expected, "{value}");
        }
    }

    #[test]
    fn repr_matches_python_containers() {
        let value =
            json!({"a": [1, 2.5, null, true], "it's": "x", "q": "say \"hi\" it's", "n": "a\nb"});
        assert_eq!(
            repr(&value),
            r#"{'a': [1, 2.5, None, True], "it's": 'x', 'q': 'say "hi" it\'s', 'n': 'a\nb'}"#
        );
        assert_eq!(display(&json!("plain")), "plain");
        assert_eq!(display(&json!([])), "[]");
    }

    #[test]
    fn json_dumps_matches_python_indent_and_ascii() {
        let value = json!({"page_content": "caf\u{e9} \u{1f600}", "score": 0.5, "empty": {}, "list": [], "n": [1, "a"]});
        let expected = "{\n    \"page_content\": \"caf\\u00e9 \\ud83d\\ude00\",\n    \"score\": 0.5,\n    \"empty\": {},\n    \"list\": [],\n    \"n\": [\n        1,\n        \"a\"\n    ]\n}";
        assert_eq!(json_dumps(&value, 4), expected);
    }
}
