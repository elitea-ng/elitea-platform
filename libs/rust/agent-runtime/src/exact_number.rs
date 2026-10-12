//! Exact reading of JSON number text, without floating point.
//!
//! The functions take the number's text, never a parsed `f64`. Under the
//! worker's `serde_json` `arbitrary_precision` that text is the lexical input
//! (`Number::as_str`); without it, `Number`'s `Display` is the exact text of
//! the value `serde_json` holds. Either way `2.0` and `1e2` are the integers 2
//! and 100, `2.5` is not an integer, and nothing is coerced through `f64`.
//! Nothing here depends on either `serde_json` feature (see `canonical`).

/// An i64 or u64 magnitude has at most 20 decimal digits; `10^20` fits `u128`.
pub const MAX_INTEGER_DIGITS: u64 = 20;

/// Exact decimal `(-1)^negative * digits * 10^exponent`. `digits` has no
/// leading or trailing zeros; zero is `(false, "", 0)`, so the form is unique.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Decimal {
    pub negative: bool,
    pub digits: String,
    pub exponent: i64,
}

/// Why number text is not the exact integer a caller asked for.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum NumberFault {
    /// Not JSON number text, or an exponent no `i64` holds.
    Unsupported,
    /// A fraction, or a negative value where a `u64` is required.
    NotInteger,
    /// More than [`MAX_INTEGER_DIGITS`] digits, or outside the target type.
    Overflow,
}

/// Normalizes JSON number text without floating point.
///
/// # Errors
///
/// [`NumberFault::Unsupported`] for text that is not a JSON number or whose
/// exponent no `i64` holds.
pub fn decimal(text: &str) -> Result<Decimal, NumberFault> {
    const UNSUPPORTED: NumberFault = NumberFault::Unsupported;
    let (negative, unsigned) = match text.strip_prefix('-') {
        Some(rest) => (true, rest),
        None => (false, text),
    };
    let (mantissa, exponent) = match unsigned.find(['e', 'E']) {
        Some(at) => (
            unsigned.get(..at).ok_or(UNSUPPORTED)?,
            unsigned.get(at + 1..).ok_or(UNSUPPORTED)?,
        ),
        None => (unsigned, "0"),
    };
    let (integer, fraction) = mantissa.split_once('.').unwrap_or((mantissa, ""));
    let is_digits = |part: &str| part.bytes().all(|byte| byte.is_ascii_digit());
    let exponent_digits = exponent.strip_prefix(['+', '-']).unwrap_or(exponent);
    if integer.is_empty()
        || !is_digits(integer)
        || !is_digits(fraction)
        || exponent_digits.is_empty()
        || !is_digits(exponent_digits)
    {
        return Err(UNSUPPORTED);
    }
    let exponent = exponent.parse::<i64>().map_err(|_| UNSUPPORTED)?;
    let all = integer.chars().chain(fraction.chars()).collect::<String>();
    let significant = all.trim_start_matches('0');
    let digits = significant.trim_end_matches('0');
    if digits.is_empty() {
        return Ok(Decimal {
            negative: false,
            digits: String::new(),
            exponent: 0,
        });
    }
    let fraction_len = i64::try_from(fraction.len()).map_err(|_| UNSUPPORTED)?;
    let trailing = i64::try_from(significant.len() - digits.len()).map_err(|_| UNSUPPORTED)?;
    let exponent = exponent
        .checked_sub(fraction_len)
        .and_then(|value| value.checked_add(trailing))
        .ok_or(UNSUPPORTED)?;
    Ok(Decimal {
        negative,
        digits: digits.to_owned(),
        exponent,
    })
}

/// The exact integer `(negative, magnitude)` of number text.
fn exact_integer(text: &str) -> Result<(bool, u128), NumberFault> {
    let decimal = decimal(text)?;
    if decimal.exponent < 0 {
        return Err(NumberFault::NotInteger);
    }
    let exponent = decimal.exponent.unsigned_abs();
    let length = u64::try_from(decimal.digits.len()).map_err(|_| NumberFault::Overflow)?;
    if length
        .checked_add(exponent)
        .is_none_or(|total| total > MAX_INTEGER_DIGITS)
    {
        return Err(NumberFault::Overflow);
    }
    let mut magnitude = 0_u128;
    for byte in decimal.digits.bytes() {
        magnitude = magnitude
            .checked_mul(10)
            .and_then(|value| value.checked_add(u128::from(byte - b'0')))
            .ok_or(NumberFault::Overflow)?;
    }
    for _ in 0..exponent {
        magnitude = magnitude.checked_mul(10).ok_or(NumberFault::Overflow)?;
    }
    Ok((decimal.negative, magnitude))
}

/// The exact `i64` of number text: `2.0` and `1e2` qualify, `2.5` does not.
///
/// # Errors
///
/// [`NumberFault::NotInteger`] for a fraction, [`NumberFault::Overflow`]
/// outside `i64`, [`NumberFault::Unsupported`] as for [`decimal`].
pub fn exact_i64(text: &str) -> Result<i64, NumberFault> {
    let (negative, magnitude) = exact_integer(text)?;
    let signed = i128::try_from(magnitude).map_err(|_| NumberFault::Overflow)?;
    i64::try_from(if negative { -signed } else { signed }).map_err(|_| NumberFault::Overflow)
}

/// The exact `u64` of number text.
///
/// # Errors
///
/// [`NumberFault::NotInteger`] for a fraction or a negative value,
/// [`NumberFault::Overflow`] outside `u64`, [`NumberFault::Unsupported`] as
/// for [`decimal`].
pub fn exact_u64(text: &str) -> Result<u64, NumberFault> {
    match exact_integer(text)? {
        (true, _) => Err(NumberFault::NotInteger),
        (false, magnitude) => u64::try_from(magnitude).map_err(|_| NumberFault::Overflow),
    }
}

#[cfg(test)]
#[path = "exact_number_tests.rs"]
mod tests;
