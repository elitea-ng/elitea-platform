//! Python's text rules the parser output depends on: string literal
//! decoding, `repr`, `inspect.cleandoc`, `str.isupper`, number formatting.
//!
//! WHY by hand: docstrings are `ast.get_docstring` values (decoded literals,
//! then `cleandoc`), signatures and return types hold `ast.unparse` text
//! (which writes constants with `repr`), and constant symbol types use
//! `str.isupper`. Each is Python 3.12's behaviour; known gaps are named on
//! the function.

use std::fmt::Write as _;

/// The pieces of one string literal token: prefix flags and the body
/// between the quotes.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
#[allow(clippy::struct_excessive_bools)] // one flag per prefix letter
pub struct Prefix {
    pub raw: bool,
    pub bytes: bool,
    pub format: bool,
    pub unicode: bool,
}

impl Prefix {
    /// Read the prefix letters of a `string_start` token (`rb"`, `f'''` …).
    #[must_use]
    pub fn parse(start: &str) -> Self {
        let mut prefix = Self::default();
        for c in start.chars() {
            match c.to_ascii_lowercase() {
                'r' => prefix.raw = true,
                'b' => prefix.bytes = true,
                'f' => prefix.format = true,
                'u' => prefix.unicode = true,
                _ => {}
            }
        }
        prefix
    }
}

/// Decode the body of a non-raw `str` literal part, as the tokenizer does.
///
/// Known gap: `\N{NAME}` needs the Unicode name table; it is kept verbatim.
#[must_use]
pub fn decode_str(body: &str, raw: bool) -> String {
    if raw || !body.contains('\\') {
        return body.to_owned();
    }
    let mut out = String::with_capacity(body.len());
    let mut chars = body.chars().peekable();
    while let Some(c) = chars.next() {
        if c != '\\' {
            out.push(c);
            continue;
        }
        let Some(next) = chars.next() else {
            out.push('\\');
            break;
        };
        match next {
            '\n' => {}
            '\\' => out.push('\\'),
            '\'' => out.push('\''),
            '"' => out.push('"'),
            'a' => out.push('\u{7}'),
            'b' => out.push('\u{8}'),
            'f' => out.push('\u{c}'),
            'n' => out.push('\n'),
            'r' => out.push('\r'),
            't' => out.push('\t'),
            'v' => out.push('\u{b}'),
            '0'..='7' => {
                let mut value = next.to_digit(8).unwrap_or(0);
                for _ in 0..2 {
                    match chars.peek().and_then(|d| d.to_digit(8)) {
                        Some(digit) => {
                            value = value * 8 + digit;
                            chars.next();
                        }
                        None => break,
                    }
                }
                out.push(char::from_u32(value).unwrap_or('\u{fffd}'));
            }
            'x' | 'u' | 'U' => {
                let width = match next {
                    'x' => 2,
                    'u' => 4,
                    _ => 8,
                };
                let digits: String = chars.clone().take(width).collect();
                if let Some(decoded) = (digits.len() == width)
                    .then(|| u32::from_str_radix(&digits, 16).ok())
                    .flatten()
                    .and_then(char::from_u32)
                {
                    out.push(decoded);
                    for _ in 0..width {
                        chars.next();
                    }
                } else {
                    out.push('\\');
                    out.push(next);
                }
            }
            other => {
                out.push('\\');
                out.push(other);
            }
        }
    }
    out
}

/// Decode the body of a `bytes` literal (ASCII only, no `\u`/`\N`).
#[must_use]
pub fn decode_bytes(body: &str, raw: bool) -> Vec<u8> {
    if raw {
        return body.as_bytes().to_vec();
    }
    let bytes = body.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        let c = bytes[i];
        if c != b'\\' || i + 1 >= bytes.len() {
            out.push(c);
            i += 1;
            continue;
        }
        let next = bytes[i + 1];
        i += 2;
        match next {
            b'\n' => {}
            b'\\' | b'\'' | b'"' => out.push(next),
            b'a' => out.push(7),
            b'b' => out.push(8),
            b'f' => out.push(12),
            b'n' => out.push(b'\n'),
            b'r' => out.push(b'\r'),
            b't' => out.push(b'\t'),
            b'v' => out.push(11),
            b'0'..=b'7' => {
                let mut value = u32::from(next - b'0');
                for _ in 0..2 {
                    match bytes.get(i) {
                        Some(d @ b'0'..=b'7') => {
                            value = value * 8 + u32::from(d - b'0');
                            i += 1;
                        }
                        _ => break,
                    }
                }
                out.push(u8::try_from(value & 0xff).unwrap_or(0));
            }
            b'x' => {
                match body
                    .get(i..i + 2)
                    .and_then(|h| u8::from_str_radix(h, 16).ok())
                {
                    Some(value) => {
                        out.push(value);
                        i += 2;
                    }
                    None => out.extend_from_slice(b"\\x"),
                }
            }
            other => {
                out.push(b'\\');
                out.push(other);
            }
        }
    }
    out
}

/// `str.isprintable` for one character. Known gap: unassigned code points
/// (category `Cn`) count as printable.
fn is_printable(c: char) -> bool {
    let u = u32::from(c);
    if c == ' ' {
        return true;
    }
    if c.is_control() || c.is_whitespace() {
        return false;
    }
    !matches!(u,
        0xAD | 0x600..=0x605 | 0x61C | 0x6DD | 0x70F | 0x890..=0x891 | 0x8E2 | 0x180E
        | 0x200B..=0x200F | 0x202A..=0x202E | 0x2060..=0x2064 | 0x2066..=0x206F
        | 0xFEFF | 0xFFF9..=0xFFFB | 0x110BD | 0x110CD | 0x13430..=0x1343F
        | 0x1BCA0..=0x1BCA3 | 0x1D173..=0x1D17A | 0xE0001 | 0xE0020..=0xE007F
        | 0xD800..=0xDFFF | 0xE000..=0xF8FF | 0xF0000..=0xFFFFD | 0x10_0000..=0x10_FFFD)
}

/// `repr(str)`.
#[must_use]
pub fn repr_str(value: &str) -> String {
    let quote = if value.contains('\'') && !value.contains('"') {
        '"'
    } else {
        '\''
    };
    let mut out = String::with_capacity(value.len() + 2);
    out.push(quote);
    for c in value.chars() {
        match c {
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if c == quote => {
                out.push('\\');
                out.push(c);
            }
            c if is_printable(c) => out.push(c),
            c => {
                let u = u32::from(c);
                let _ = if u < 0x100 {
                    write!(out, "\\x{u:02x}")
                } else if u < 0x10000 {
                    write!(out, "\\u{u:04x}")
                } else {
                    write!(out, "\\U{u:08x}")
                };
            }
        }
    }
    out.push(quote);
    out
}

/// `repr(bytes)`.
#[must_use]
pub fn repr_bytes(value: &[u8]) -> String {
    let quote = if value.contains(&b'\'') && !value.contains(&b'"') {
        b'"'
    } else {
        b'\''
    };
    let mut out = String::from("b");
    out.push(char::from(quote));
    for &b in value {
        match b {
            b'\\' => out.push_str("\\\\"),
            b'\n' => out.push_str("\\n"),
            b'\r' => out.push_str("\\r"),
            b'\t' => out.push_str("\\t"),
            b if b == quote => {
                out.push('\\');
                out.push(char::from(b));
            }
            0x20..=0x7e => out.push(char::from(b)),
            b => {
                let _ = write!(out, "\\x{b:02x}");
            }
        }
    }
    out.push(char::from(quote));
    out
}

/// `repr(float)`: the shortest round-trip digits, fixed notation for
/// decimal exponents in `-4..16`, else `1e+16` style.
#[must_use]
pub fn repr_float(value: f64) -> String {
    if value.is_nan() {
        return "nan".to_owned();
    }
    if value.is_infinite() {
        return if value > 0.0 { "inf" } else { "-inf" }.to_owned();
    }
    if value == 0.0 {
        return if value.is_sign_negative() {
            "-0.0"
        } else {
            "0.0"
        }
        .to_owned();
    }
    let sci = format!("{value:e}");
    let (mantissa, exponent) = sci.split_once('e').unwrap_or((&sci, "0"));
    let exponent: i32 = exponent.parse().unwrap_or(0);
    let negative = mantissa.starts_with('-');
    let digits: String = mantissa.chars().filter(char::is_ascii_digit).collect();
    let sign = if negative { "-" } else { "" };
    if (-4..16).contains(&exponent) {
        let point = exponent + 1;
        let body = if point <= 0 {
            let zeros = usize::try_from(-point).unwrap_or(0);
            format!("0.{}{digits}", "0".repeat(zeros))
        } else {
            let point = usize::try_from(point).unwrap_or(0);
            if digits.len() <= point {
                format!("{digits}{}.0", "0".repeat(point - digits.len()))
            } else {
                format!("{}.{}", &digits[..point], &digits[point..])
            }
        };
        format!("{sign}{body}")
    } else {
        let rest = &digits[1..];
        let mantissa = if rest.is_empty() {
            digits[..1].to_owned()
        } else {
            format!("{}.{rest}", &digits[..1])
        };
        let exp_sign = if exponent < 0 { '-' } else { '+' };
        format!("{sign}{mantissa}e{exp_sign}{:02}", exponent.abs())
    }
}

/// `repr(complex)` for a pure imaginary literal: `2j`, `1.5j`, `1e+20j`.
#[must_use]
pub fn repr_imaginary(value: f64) -> String {
    let text = repr_float(value);
    let text = text.strip_suffix(".0").unwrap_or(&text);
    format!("{text}j")
}

/// The decimal value of an integer literal (`0x1F`, `1_000`, `0o7`, `0b1`).
/// A literal too large for 128 bits keeps its source text.
#[must_use]
pub fn int_literal(text: &str) -> String {
    let clean: String = text.chars().filter(|&c| c != '_').collect();
    let lower = clean.to_ascii_lowercase();
    let parsed = if let Some(hex) = lower.strip_prefix("0x") {
        u128::from_str_radix(hex, 16).ok()
    } else if let Some(oct) = lower.strip_prefix("0o") {
        u128::from_str_radix(oct, 8).ok()
    } else if let Some(bin) = lower.strip_prefix("0b") {
        u128::from_str_radix(bin, 2).ok()
    } else {
        let trimmed = clean.trim_start_matches('0');
        return if trimmed.is_empty() {
            "0".to_owned()
        } else {
            trimmed.to_owned()
        };
    };
    parsed.map_or(clean, |v| v.to_string())
}

/// `str.expandtabs()` with the default tab size 8.
fn expand_tabs(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut column = 0usize;
    for c in text.chars() {
        match c {
            '\t' => {
                let spaces = 8 - column % 8;
                out.extend(std::iter::repeat_n(' ', spaces));
                column += spaces;
            }
            '\n' | '\r' => {
                out.push(c);
                column = 0;
            }
            _ => {
                out.push(c);
                column += 1;
            }
        }
    }
    out
}

/// `inspect.cleandoc` (3.12): expand tabs, strip the first line's leading
/// whitespace, remove the common indentation of the others (counted in
/// characters, whitespace as `str.isspace`), drop leading and trailing
/// EMPTY lines. Python quirk: a whitespace-only line longer than the margin
/// keeps its excess and is then not "empty".
#[must_use]
pub fn cleandoc(doc: &str) -> String {
    let expanded = expand_tabs(doc);
    let mut lines: Vec<String> = expanded.split('\n').map(str::to_owned).collect();
    let mut margin = usize::MAX;
    for line in lines.iter().skip(1) {
        let content = line.trim_start_matches(char::is_whitespace).chars().count();
        if content > 0 {
            margin = margin.min(line.chars().count() - content);
        }
    }
    if let Some(first) = lines.first_mut() {
        *first = first.trim_start_matches(char::is_whitespace).to_owned();
    }
    if margin < usize::MAX {
        for line in lines.iter_mut().skip(1) {
            *line = line.chars().skip(margin).collect();
        }
    }
    while lines.last().is_some_and(String::is_empty) {
        lines.pop();
    }
    let leading = lines.iter().take_while(|l| l.is_empty()).count();
    lines.drain(..leading);
    lines.join("\n")
}

/// `str.isupper`: at least one cased character and no lower-case one.
/// Known gap: title-case letters (`ǅ`) count as upper here.
#[must_use]
pub fn is_upper(text: &str) -> bool {
    let mut cased = false;
    for c in text.chars() {
        if c.is_lowercase() {
            return false;
        }
        if c.is_uppercase() {
            cased = true;
        }
    }
    cased
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn literals_decode_as_the_tokenizer_does() {
        assert_eq!(decode_str(r"a\nb\x41é\101\q", false), "a\nbA\u{e9}A\\q");
        assert_eq!(decode_str("a\\\nb", false), "ab");
        assert_eq!(decode_str(r"a\nb", true), r"a\nb");
        assert_eq!(decode_bytes(r"\x00a\n", false), b"\x00a\n");
    }

    #[test]
    fn repr_picks_quotes_and_escapes_like_python() {
        assert_eq!(repr_str("it's"), "\"it's\"");
        assert_eq!(repr_str("a'\"b"), "'a\\'\"b'");
        assert_eq!(repr_str("tab\there\u{a0}"), "'tab\\there\\xa0'");
        assert_eq!(repr_str("é"), "'é'");
        assert_eq!(repr_bytes(b"a\x00'"), "b\"a\\x00'\"");
    }

    #[test]
    fn floats_print_as_python_repr() {
        assert_eq!(repr_float(1e16), "1e+16");
        assert_eq!(repr_float(1e15), "1000000000000000.0");
        assert_eq!(repr_float(0.0001), "0.0001");
        assert_eq!(repr_float(0.00001), "1e-05");
        assert_eq!(repr_float(1.5), "1.5");
        assert_eq!(repr_float(123.456), "123.456");
        assert_eq!(repr_imaginary(2.0), "2j");
        assert_eq!(int_literal("0x1F"), "31");
        assert_eq!(int_literal("1_000"), "1000");
        assert_eq!(int_literal("000"), "0");
    }

    #[test]
    fn cleandoc_matches_inspect() {
        assert_eq!(
            cleandoc("  First.\n\n    body\n      more\n  "),
            "First.\n\nbody\n  more"
        );
        assert_eq!(cleandoc("\n    a\n\tb\n"), "a\n    b");
        assert_eq!(cleandoc("x\n      \n    y"), "x\n  \ny");
        assert!(is_upper("MAX_2"));
        assert!(!is_upper("__all__"));
        assert!(!is_upper("_"));
    }
}
