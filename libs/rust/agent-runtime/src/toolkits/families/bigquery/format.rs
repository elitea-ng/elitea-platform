//! The SDK's result text: `json.dumps(rows, default=str)` over the rows the
//! Python `BigQuery` client decodes.
//!
//! Two properties of that text are kept on purpose. Columns appear in the
//! table's schema order (a Python `dict(row)` preserves it; a serde map would
//! sort them), and the encoding is `CPython`'s default `json.dumps`: `", "` and
//! `": "` separators, `ensure_ascii`, `repr`-style floats and the
//! `NaN`/`Infinity` literals. `default=str` renders the values the Python
//! client converts to `datetime`, `date`, `time` and `Decimal` as their
//! `str()` forms, which [`super::client`] reproduces when it decodes a cell.

use std::fmt::Write as _;

use serde_json::Value;

/// One decoded `BigQuery` value, in the shape the Python client produces.
#[derive(Clone, Debug, PartialEq)]
pub(in crate::toolkits) enum Cell {
    Null,
    Bool(bool),
    /// An INT64 value; its decimal digits are emitted verbatim.
    Int(i64),
    Float(f64),
    Text(String),
    /// A JSON column, already parsed (`json.loads` in the Python client).
    Json(Value),
    List(Vec<Cell>),
    /// A row or a STRUCT, in schema field order.
    Record(Vec<(String, Cell)>),
}

impl Cell {
    /// Insert or replace one field of a record, keeping an existing field's
    /// position as a Python `dict` update does.
    pub(in crate::toolkits) fn set_field(&mut self, name: &str, value: Self) {
        if let Self::Record(fields) = self {
            if let Some(slot) = fields.iter_mut().find(|(field, _)| field == name) {
                slot.1 = value;
            } else {
                fields.push((name.to_owned(), value));
            }
        }
    }
}

/// `json.dumps(value, default=str)` with `CPython`'s default settings.
#[must_use]
pub(in crate::toolkits) fn python_dumps(value: &Cell) -> String {
    let mut out = String::new();
    write_cell(&mut out, value);
    out
}

fn write_cell(out: &mut String, value: &Cell) {
    match value {
        Cell::Null => out.push_str("null"),
        Cell::Bool(true) => out.push_str("true"),
        Cell::Bool(false) => out.push_str("false"),
        Cell::Int(value) => {
            let _ = write!(out, "{value}");
        }
        Cell::Float(value) => out.push_str(&python_float(*value)),
        Cell::Text(value) => write_string(out, value),
        Cell::Json(value) => write_value(out, value),
        Cell::List(values) => {
            out.push('[');
            for (index, value) in values.iter().enumerate() {
                if index != 0 {
                    out.push_str(", ");
                }
                write_cell(out, value);
            }
            out.push(']');
        }
        Cell::Record(fields) => {
            out.push('{');
            for (index, (name, value)) in fields.iter().enumerate() {
                if index != 0 {
                    out.push_str(", ");
                }
                write_string(out, name);
                out.push_str(": ");
                write_cell(out, value);
            }
            out.push('}');
        }
    }
}

fn write_value(out: &mut String, value: &Value) {
    match value {
        Value::Null => out.push_str("null"),
        Value::Bool(true) => out.push_str("true"),
        Value::Bool(false) => out.push_str("false"),
        Value::Number(number) => {
            if let Some(integer) = number.as_i64() {
                let _ = write!(out, "{integer}");
            } else if let Some(integer) = number.as_u64() {
                let _ = write!(out, "{integer}");
            } else if let Some(float) = number.as_f64() {
                out.push_str(&python_float(float));
            } else {
                let _ = write!(out, "{number}");
            }
        }
        Value::String(text) => write_string(out, text),
        Value::Array(values) => {
            out.push('[');
            for (index, value) in values.iter().enumerate() {
                if index != 0 {
                    out.push_str(", ");
                }
                write_value(out, value);
            }
            out.push(']');
        }
        Value::Object(fields) => {
            out.push('{');
            for (index, (name, value)) in fields.iter().enumerate() {
                if index != 0 {
                    out.push_str(", ");
                }
                write_string(out, name);
                out.push_str(": ");
                write_value(out, value);
            }
            out.push('}');
        }
    }
}

/// A JSON string with `CPython`'s `ensure_ascii=True` escaping: everything
/// outside printable ASCII becomes `\uXXXX` (surrogate pairs above U+FFFF).
fn write_string(out: &mut String, value: &str) {
    out.push('"');
    for character in value.chars() {
        match character {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{8}' => out.push_str("\\b"),
            '\u{c}' => out.push_str("\\f"),
            ' '..='~' => out.push(character),
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

/// `CPython`'s `repr(float)`, which `json.dumps` uses: the shortest
/// round-tripping digits, positional for decimal exponents in `-4..16`,
/// otherwise `d.ddde±XX`.
#[must_use]
pub(in crate::toolkits) fn python_float(value: f64) -> String {
    if value.is_nan() {
        return "NaN".to_owned();
    }
    if value.is_infinite() {
        return if value.is_sign_positive() {
            "Infinity".to_owned()
        } else {
            "-Infinity".to_owned()
        };
    }
    if value == 0.0 {
        return if value.is_sign_negative() {
            "-0.0".to_owned()
        } else {
            "0.0".to_owned()
        };
    }
    // `{:e}` is Rust's shortest round-trip form, e.g. `-1.2345e-5`.
    let scientific = format!("{value:e}");
    let (mantissa, exponent) = scientific
        .split_once('e')
        .unwrap_or((scientific.as_str(), "0"));
    let exponent: i32 = exponent.parse().unwrap_or(0);
    let (sign, mantissa) = mantissa
        .strip_prefix('-')
        .map_or(("", mantissa), |rest| ("-", rest));
    let digits: String = mantissa.chars().filter(char::is_ascii_digit).collect();
    if (-4..16).contains(&exponent) {
        let point = exponent + 1;
        let mut text = String::from(sign);
        if point <= 0 {
            text.push_str("0.");
            for _ in 0..(-point) {
                text.push('0');
            }
            text.push_str(&digits);
        } else {
            let point = usize::try_from(point).unwrap_or(0);
            if digits.len() <= point {
                text.push_str(&digits);
                for _ in digits.len()..point {
                    text.push('0');
                }
                text.push_str(".0");
            } else {
                text.push_str(&digits[..point]);
                text.push('.');
                text.push_str(&digits[point..]);
            }
        }
        text
    } else {
        let mut text = String::from(sign);
        text.push_str(&digits[..1]);
        if digits.len() > 1 {
            text.push('.');
            text.push_str(&digits[1..]);
        }
        let _ = write!(
            text,
            "e{}{:02}",
            if exponent < 0 { '-' } else { '+' },
            exponent.unsigned_abs()
        );
        text
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn floats_follow_cpython_repr() {
        for (value, expected) in [
            (1.0, "1.0"),
            (0.5, "0.5"),
            (-2.25, "-2.25"),
            (123_456.789, "123456.789"),
            (1e16, "1e+16"),
            (1.5e16, "1.5e+16"),
            (1e15, "1000000000000000.0"),
            (0.0001, "0.0001"),
            (0.00001, "1e-05"),
            (-1.25e-7, "-1.25e-07"),
            (0.1, "0.1"),
            (f64::NAN, "NaN"),
            (f64::NEG_INFINITY, "-Infinity"),
        ] {
            assert_eq!(python_float(value), expected, "{value}");
        }
    }

    #[test]
    fn records_keep_column_order_and_python_separators() {
        let row = Cell::Record(vec![
            ("zeta".to_owned(), Cell::Int(1)),
            (
                "alpha".to_owned(),
                Cell::Text("caf\u{e9} \u{1f600}\"".to_owned()),
            ),
            (
                "tags".to_owned(),
                Cell::List(vec![Cell::Bool(true), Cell::Null]),
            ),
        ]);
        assert_eq!(
            python_dumps(&Cell::List(vec![row])),
            r#"[{"zeta": 1, "alpha": "caf\u00e9 \ud83d\ude00\"", "tags": [true, null]}]"#
        );
    }
}
