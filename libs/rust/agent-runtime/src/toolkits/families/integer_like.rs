//! The one reading of an integer argument for the SDK-parity families, as
//! the SDK's pydantic (lax mode) reads one: a JSON integer, a float with an
//! integer value (`5.0`), or a string of digits with an optional sign (`"5"`,
//! `" -5 "`). Anything else (`5.5`, `"5x"`, `""`, `true`, a list) is not an
//! integer, and the caller says "must be a number".

use serde_json::Value;

/// Floats beyond this lose integer precision (2^53).
const MAX_EXACT_FLOAT: f64 = 9_007_199_254_740_992.0;

/// The integer `value` stands for, if it stands for one.
pub(in crate::toolkits) fn integer_like(value: &Value) -> Option<i64> {
    match value {
        Value::Number(number) => number.as_i64().or_else(|| {
            let float = number.as_f64()?;
            (float.fract() == 0.0 && float.abs() < MAX_EXACT_FLOAT)
                .then(|| format!("{float:.0}").parse().ok())
                .flatten()
        }),
        Value::String(text) => {
            let text = text.trim();
            let digits = text.strip_prefix(['+', '-']).unwrap_or(text);
            (!digits.is_empty() && digits.bytes().all(|byte| byte.is_ascii_digit()))
                .then(|| text.parse().ok())
                .flatten()
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
        ] {
            assert_eq!(integer_like(&value), None, "{value}");
        }
        assert_eq!(unsigned_like(&json!("-1")), None);
        assert_eq!(unsigned_like(&json!("12")), Some(12));
    }
}
