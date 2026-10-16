//! The one reading of an integer argument for the SDK-parity families, as
//! the SDK's pydantic (lax mode) reads one: a JSON integer, a float with an
//! integer value (`5.0`, `1e2`), or a string of either (`"5"`, `" -5 "`,
//! `"10.0"`, `"1e2"`), read exactly. Anything else (`5.5`, `"5x"`, `""`, `true`, a list) is not an
//! integer, and the caller says "must be a number".

use serde_json::Value;

use crate::exact_number::exact_i64;

/// The integer `value` stands for, if it stands for one. Read exactly, from
/// the number's text, never through `f64`: `5.0`, `"10.0"` and `"1e2"` are
/// integers (5, 10, 100), `5.5`, `"1e-1"` and anything outside `i64` are not,
/// and a large integer-valued float or string keeps every digit.
pub(in crate::toolkits) fn integer_like(value: &Value) -> Option<i64> {
    match value {
        Value::Number(number) => number
            .as_i64()
            .or_else(|| exact_i64(&number.to_string()).ok()),
        Value::String(text) => {
            let text = text.trim();
            // `exact_i64` takes a sign of `-` only; pydantic also takes `+`.
            let text = match text.strip_prefix('+') {
                Some(rest) if rest.starts_with(['+', '-']) => return None,
                Some(rest) => rest,
                None => text,
            };
            exact_i64(text).ok()
        }
        _ => None,
    }
}

/// [`integer_like`], for a count that cannot be negative.
pub(in crate::toolkits) fn unsigned_like(value: &Value) -> Option<u64> {
    integer_like(value).and_then(|value| u64::try_from(value).ok())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn integers_are_read_the_way_pydantic_reads_them() {
        for (value, expected) in [
            (json!(5), 5),
            (json!(-5), -5),
            (json!(5.0), 5),
            (json!("5"), 5),
            (json!(" 42 "), 42),
            (json!("+7"), 7),
            (json!("-3"), -3),
            (json!("007"), 7),
            // Integer-valued spellings, read exactly.
            (json!("10.0"), 10),
            (json!(" 5.0 "), 5),
            (json!("1e2"), 100),
            (json!("1E2"), 100),
            (json!(1e2), 100),
            (json!("-2.50e1"), -25),
            (json!("9007199254740993"), 9_007_199_254_740_993),
            (json!("9007199254740993.0"), 9_007_199_254_740_993),
            (json!("9223372036854775807"), i64::MAX),
        ] {
            assert_eq!(integer_like(&value), Some(expected), "{value}");
        }
        for value in [
            json!(5.5),
            json!("5.5"),
            json!("5x"),
            json!(""),
            json!("-"),
            json!(" "),
            json!(true),
            json!(null),
            json!([5]),
            json!({"n": 5}),
            json!(1.0e300),
            json!("99999999999999999999"),
            json!("1e30"),
            json!("1e19"),
            json!("9223372036854775808"),
            json!("1e-1"),
            json!("10.5"),
            json!("1_0"),
            json!("+-5"),
            json!("0x10"),
            json!("1e"),
            json!(".5"),
        ] {
            assert_eq!(integer_like(&value), None, "{value}");
        }
        assert_eq!(unsigned_like(&json!("-1")), None);
        assert_eq!(unsigned_like(&json!("12")), Some(12));
    }
}
