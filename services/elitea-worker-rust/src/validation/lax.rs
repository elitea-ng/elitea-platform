//! pydantic v2 "lax mode" coercions for the two non-string scalar kinds the
//! SDK configuration models use (`Optional[int]`, `Optional[bool]`).
//!
//! Each rule below was read off recorded pydantic 2.12 behaviour, not off its
//! documentation; `tests.rs` replays those recordings.

use serde_json::Value;

/// `int`: booleans, JSON integers of any size, integral floats inside the i64
/// range, and text that is an optionally signed decimal integer (single
/// underscores between digits allowed) or an integer with an all-zero fraction.
#[allow(clippy::float_cmp)] // exact integrality is the rule, not an approximation
pub(super) fn accepts_integer(value: &Value) -> bool {
    match value {
        Value::Bool(_) => true,
        Value::Number(number) => {
            if is_integer_literal(&number.to_string()) {
                return true;
            }
            number.as_f64().is_some_and(|float| {
                float.is_finite()
                    && float.fract() == 0.0
                    && (-9_223_372_036_854_775_808.0..9_223_372_036_854_775_808.0).contains(&float)
            })
        }
        Value::String(text) => integer_text(text.trim()),
        _ => false,
    }
}

/// `bool`: booleans, the integers 0 and 1, the floats 0.0 and 1.0, and exactly
/// the spellings pydantic lists (case-insensitive, no surrounding space).
#[allow(clippy::float_cmp)] // only exactly 0.0 and 1.0 are booleans
pub(super) fn accepts_boolean(value: &Value) -> bool {
    match value {
        Value::Bool(_) => true,
        Value::Number(number) => {
            let text = number.to_string();
            if is_integer_literal(&text) {
                matches!(text.as_str(), "0" | "1" | "-0")
            } else {
                number
                    .as_f64()
                    .is_some_and(|float| float == 0.0 || float == 1.0)
            }
        }
        Value::String(text) => matches!(
            text.to_ascii_lowercase().as_str(),
            "0" | "off" | "f" | "false" | "n" | "no" | "1" | "on" | "t" | "true" | "y" | "yes"
        ),
        _ => false,
    }
}

/// A JSON number written without a fraction or exponent.
fn is_integer_literal(text: &str) -> bool {
    !text.contains(['.', 'e', 'E'])
}

fn integer_text(text: &str) -> bool {
    let unsigned = text.strip_prefix(['+', '-']).unwrap_or(text);
    let (whole, fraction) = match unsigned.split_once('.') {
        Some((whole, fraction)) => (whole, Some(fraction)),
        None => (unsigned, None),
    };
    underscored_digits(whole)
        && fraction.is_none_or(|digits| !digits.is_empty() && digits.bytes().all(|b| b == b'0'))
}

/// ASCII digits with at most single underscores strictly between them.
fn underscored_digits(text: &str) -> bool {
    let bytes = text.as_bytes();
    !bytes.is_empty()
        && bytes.first().is_some_and(u8::is_ascii_digit)
        && bytes.last().is_some_and(u8::is_ascii_digit)
        && bytes.windows(2).all(|pair| pair != b"__")
        && bytes.iter().all(|b| b.is_ascii_digit() || *b == b'_')
}
