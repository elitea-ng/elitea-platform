//! Single-pass settings decoding with the Python worker's `json.loads` rules.
//!
//! The Python worker decodes settings with `json.loads(raw.decode("utf-8"),
//! object_pairs_hook=_unique_object, parse_constant=_reject_constant)` and
//! then walks the result for the nesting bound (`_check_json_limits`). This
//! parser reproduces that in one pass and does not go through `serde_json`'s
//! `Deserializer`, for three reasons that each mattered in review:
//!
//! - **Duplicate members.** `serde_json::Value` keeps the last of two equal
//!   keys; Python refuses the document. The duplicate is refused here while
//!   the object is built.
//! - **The private number marker.** Under `arbitrary_precision` `serde_json`
//!   hands every number to a visitor as a one-member map keyed
//!   `$serde_json::private::Number`, so a *user* object with that exact key
//!   is indistinguishable from a number inside any serde visitor. Here a
//!   number is lexed as a number and an object as an object; the marker is
//!   never interpreted.
//! - **Nesting.** `serde_json` stops at 128 levels with a syntax error, but the
//!   Python worker reports a document deeper than [`MAX_JSON_DEPTH`] as
//!   resource exhaustion. The depth is tracked while parsing: beyond
//!   `MAX_JSON_DEPTH` the verdict is recorded and parsing continues (a syntax
//!   error or duplicate later in the document still wins, as it does in
//!   Python, where `json.loads` runs before the limit walk); beyond
//!   [`HARD_MAX_DEPTH`] parsing stops so recursion stays bounded.
//!
//! Numbers keep their literal text (`serde_json::Number` under
//! `arbitrary_precision`); an integer literal past Python's 4300-digit `int`
//! bound is refused here, where `json.loads` refuses it.

use serde_json::{Map, Number, Value};

use super::{MAX_JSON_DEPTH, SettingsRefusal};

/// Recursion ceiling. A document this deep is already far past
/// `MAX_JSON_DEPTH`; the ceiling exists only to bound stack use.
pub(super) const HARD_MAX_DEPTH: usize = 1024;

/// The decoded document and whether any value sat deeper than
/// `MAX_JSON_DEPTH` (the root is depth 0, as in `_check_json_limits`).
pub(super) struct Parsed {
    pub(super) value: Value,
    pub(super) too_deep: bool,
}

/// `InvalidInput` for anything `json.loads` would refuse (syntax, a stray
/// constant, invalid UTF-8, a duplicate member); `ResourceExhausted` only when
/// nesting passes the hard ceiling.
pub(super) fn parse(raw: &[u8]) -> Result<Parsed, SettingsRefusal> {
    let text = std::str::from_utf8(raw).map_err(|_| SettingsRefusal::InvalidInput)?;
    let mut parser = Parser {
        text,
        pos: 0,
        too_deep: false,
    };
    let value = parser.value(0)?;
    parser.skip_whitespace();
    if parser.pos != text.len() {
        return Err(SettingsRefusal::InvalidInput);
    }
    Ok(Parsed {
        value,
        too_deep: parser.too_deep,
    })
}

struct Parser<'a> {
    text: &'a str,
    pos: usize,
    too_deep: bool,
}

const fn malformed() -> SettingsRefusal {
    SettingsRefusal::InvalidInput
}

impl Parser<'_> {
    fn bytes(&self) -> &[u8] {
        self.text.as_bytes()
    }

    fn peek(&self) -> Option<u8> {
        self.bytes().get(self.pos).copied()
    }

    /// JSON whitespace, which is also exactly what Python's decoder skips.
    fn skip_whitespace(&mut self) {
        while matches!(self.peek(), Some(b' ' | b'\t' | b'\n' | b'\r')) {
            self.pos += 1;
        }
    }

    fn expect(&mut self, literal: &str) -> Result<(), SettingsRefusal> {
        if self.text[self.pos..].starts_with(literal) {
            self.pos += literal.len();
            Ok(())
        } else {
            Err(malformed())
        }
    }

    fn value(&mut self, depth: usize) -> Result<Value, SettingsRefusal> {
        if depth > HARD_MAX_DEPTH {
            return Err(SettingsRefusal::ResourceExhausted);
        }
        if depth > MAX_JSON_DEPTH {
            self.too_deep = true;
        }
        self.skip_whitespace();
        match self.peek().ok_or_else(malformed)? {
            b'{' => self.object(depth),
            b'[' => self.array(depth),
            b'"' => self.string().map(Value::String),
            b't' => self.expect("true").map(|()| Value::Bool(true)),
            b'f' => self.expect("false").map(|()| Value::Bool(false)),
            b'n' => self.expect("null").map(|()| Value::Null),
            b'-' | b'0'..=b'9' => self.number(),
            // `NaN`, `Infinity` and `-Infinity` are refused by Python's
            // `parse_constant` hook and fall out here as a syntax error.
            _ => Err(malformed()),
        }
    }

    fn object(&mut self, depth: usize) -> Result<Value, SettingsRefusal> {
        self.pos += 1;
        let mut members = Map::new();
        self.skip_whitespace();
        if self.peek() == Some(b'}') {
            self.pos += 1;
            return Ok(Value::Object(members));
        }
        loop {
            self.skip_whitespace();
            if self.peek() != Some(b'"') {
                return Err(malformed());
            }
            let key = self.string()?;
            self.skip_whitespace();
            self.expect(":")?;
            let value = self.value(depth + 1)?;
            if members.insert(key, value).is_some() {
                // `_unique_object` raises for a repeated key.
                return Err(malformed());
            }
            self.skip_whitespace();
            match self.peek() {
                Some(b',') => self.pos += 1,
                Some(b'}') => {
                    self.pos += 1;
                    return Ok(Value::Object(members));
                }
                _ => return Err(malformed()),
            }
        }
    }

    fn array(&mut self, depth: usize) -> Result<Value, SettingsRefusal> {
        self.pos += 1;
        let mut items = Vec::new();
        self.skip_whitespace();
        if self.peek() == Some(b']') {
            self.pos += 1;
            return Ok(Value::Array(items));
        }
        loop {
            items.push(self.value(depth + 1)?);
            self.skip_whitespace();
            match self.peek() {
                Some(b',') => self.pos += 1,
                Some(b']') => {
                    self.pos += 1;
                    return Ok(Value::Array(items));
                }
                _ => return Err(malformed()),
            }
        }
    }

    /// A JSON string (the opening quote is at `pos`). Python's strict decoder
    /// refuses raw control characters and unknown escapes; a lone surrogate
    /// escape, which Python admits into a `str`, has no `String` form and is
    /// refused here.
    fn string(&mut self) -> Result<String, SettingsRefusal> {
        self.pos += 1;
        let mut out = String::new();
        let mut run_start = self.pos;
        loop {
            let byte = self.peek().ok_or_else(malformed)?;
            match byte {
                b'"' => {
                    out.push_str(&self.text[run_start..self.pos]);
                    self.pos += 1;
                    return Ok(out);
                }
                b'\\' => {
                    out.push_str(&self.text[run_start..self.pos]);
                    self.pos += 1;
                    let escaped = self.peek().ok_or_else(malformed)?;
                    self.pos += 1;
                    match escaped {
                        b'"' => out.push('"'),
                        b'\\' => out.push('\\'),
                        b'/' => out.push('/'),
                        b'b' => out.push('\u{8}'),
                        b'f' => out.push('\u{c}'),
                        b'n' => out.push('\n'),
                        b'r' => out.push('\r'),
                        b't' => out.push('\t'),
                        b'u' => out.push(self.unicode_escape()?),
                        _ => return Err(malformed()),
                    }
                    run_start = self.pos;
                }
                0x00..=0x1f => return Err(malformed()),
                _ => self.pos += 1,
            }
        }
    }

    /// The four hex digits after `\u`, joining a surrogate pair when the
    /// escape that follows completes one.
    fn unicode_escape(&mut self) -> Result<char, SettingsRefusal> {
        let first = self.hex4()?;
        let code = match first {
            0xD800..=0xDBFF => {
                self.expect("\\u")?;
                let second = self.hex4()?;
                if !(0xDC00..=0xDFFF).contains(&second) {
                    return Err(malformed());
                }
                0x1_0000 + ((first - 0xD800) << 10) + (second - 0xDC00)
            }
            other => other,
        };
        char::from_u32(code).ok_or_else(malformed)
    }

    fn hex4(&mut self) -> Result<u32, SettingsRefusal> {
        let digits = self
            .text
            .get(self.pos..self.pos + 4)
            .filter(|digits| digits.bytes().all(|b| b.is_ascii_hexdigit()))
            .ok_or_else(malformed)?;
        self.pos += 4;
        u32::from_str_radix(digits, 16).map_err(|_| malformed())
    }

    /// `-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?`, kept as literal text.
    fn number(&mut self) -> Result<Value, SettingsRefusal> {
        let start = self.pos;
        if self.peek() == Some(b'-') {
            self.pos += 1;
        }
        match self.peek() {
            Some(b'0') => self.pos += 1,
            Some(b'1'..=b'9') => self.digits()?,
            _ => return Err(malformed()),
        }
        if self.peek() == Some(b'.') {
            self.pos += 1;
            self.digits()?;
        }
        if matches!(self.peek(), Some(b'e' | b'E')) {
            self.pos += 1;
            if matches!(self.peek(), Some(b'+' | b'-')) {
                self.pos += 1;
            }
            self.digits()?;
        }
        let literal = &self.text[start..self.pos];
        if !literal.contains(['.', 'e', 'E'])
            && literal.trim_start_matches('-').len() > super::MAX_INTEGER_LITERAL_DIGITS
        {
            // `json.loads` converts the literal with `int()`, which raises
            // `ValueError` past the digit limit: malformed input, decided
            // while decoding and so before any limit walk.
            return Err(malformed());
        }
        serde_json::from_str::<Number>(literal)
            .map(Value::Number)
            .map_err(|_| malformed())
    }

    /// One or more ASCII digits.
    fn digits(&mut self) -> Result<(), SettingsRefusal> {
        let start = self.pos;
        while self.peek().is_some_and(|b| b.is_ascii_digit()) {
            self.pos += 1;
        }
        if self.pos == start {
            return Err(malformed());
        }
        Ok(())
    }
}
