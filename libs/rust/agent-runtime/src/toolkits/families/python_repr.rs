//! Python `repr()` of JSON data, for families whose SDK tools return
//! f-strings such as `f"Extracted data:\n{data}"`.
//!
//! The model reads the SDK's output as Python literal text (single-quoted
//! strings, `True`, `None`), so a port that wants the same text renders the
//! same literal. This is the one implementation of `CPython`'s `repr` rules in
//! the toolkit families: string quoting and escaping (`unicode_repr`, with
//! `str.isprintable`), float `repr`, and `None`/`True`/`False`.
//!
//! Object members of a [`Value`] render in sorted key order whatever
//! `serde_json` features a build unifies, the rule `crate::canonical` sets:
//! without `preserve_order` (the worker) a `Map` iterates sorted, with it
//! (the desktop, through feature unification) it would iterate in insertion
//! order, and the same tool would answer different text on the two hosts.
//! Callers that need the SDK's insertion order pass ordered pairs to
//! [`repr_pairs`] or build a [`PyValue`].

use std::fmt::Write as _;

use serde_json::{Map, Value};
use unicode_general_category::{GeneralCategory, get_general_category};

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
        Value::Object(object) => write_pairs(output, sorted_members(object)),
    }
}

/// A map's members in key-byte order, as `crate::canonical` writes them.
fn sorted_members(object: &Map<String, Value>) -> Vec<(&str, &Value)> {
    let mut members = object
        .iter()
        .map(|(key, value)| (key.as_str(), value))
        .collect::<Vec<_>>();
    members.sort_unstable_by(|left, right| left.0.as_bytes().cmp(right.0.as_bytes()));
    members
}

/// `repr(list_of_strings)`.
pub(in crate::toolkits) fn repr_str_list<S: AsRef<str>>(items: &[S]) -> String {
    let mut output = String::from("[");
    for (index, item) in items.iter().enumerate() {
        if index > 0 {
            output.push_str(", ");
        }
        write_str(&mut output, item.as_ref());
    }
    output.push(']');
    output
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
///
/// Under `arbitrary_precision` (the worker) an integer wider than 64 bits
/// keeps its digits, as `json.loads` makes it a Python `int`.
pub(in crate::toolkits) fn repr_number(number: &serde_json::Number) -> String {
    if number.is_i64() || number.is_u64() {
        return number.to_string();
    }
    let text = number.to_string();
    if !text.is_empty()
        && text
            .strip_prefix('-')
            .unwrap_or(&text)
            .bytes()
            .all(|byte| byte.is_ascii_digit())
    {
        return text;
    }
    number.as_f64().map_or(text, repr_float)
}

/// `CPython`'s `repr(float)`: the shortest round-tripping digits, positional
/// for decimal exponents in `-4..16`, otherwise `d.ddde±XX`; `nan`, `inf`.
pub(in crate::toolkits) fn repr_float(float: f64) -> String {
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

/// `CPython`'s `unicode_repr`: the quote, backslash, `\t`, `\n` and `\r`
/// escaped; other characters below U+0020 and U+007F as `\xhh`; any other
/// character `str.isprintable` refuses as `\xhh`, `\uhhhh` or `\Uhhhhhhhh`
/// by its width; everything else as itself.
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
            character if character < ' ' || character == '\u{7f}' => {
                let _ = write!(output, "\\x{:02x}", u32::from(character));
            }
            character if character.is_ascii() || is_printable(character) => output.push(character),
            character => {
                let code = u32::from(character);
                if code <= 0xff {
                    let _ = write!(output, "\\x{code:02x}");
                } else if code <= 0xffff {
                    let _ = write!(output, "\\u{code:04x}");
                } else {
                    let _ = write!(output, "\\U{code:08x}");
                }
            }
        }
    }
    output.push(quote);
}

/// `str.isprintable` for one non-ASCII character: false for the categories
/// `CPython` treats as non-printable (Cc, Cf, Cs, Co, Cn, Zl, Zp, Zs; the ASCII
/// space is handled before this is asked).
fn is_printable(character: char) -> bool {
    !matches!(
        get_general_category(character),
        GeneralCategory::Control
            | GeneralCategory::Format
            | GeneralCategory::Surrogate
            | GeneralCategory::PrivateUse
            | GeneralCategory::Unassigned
            | GeneralCategory::LineSeparator
            | GeneralCategory::ParagraphSeparator
            | GeneralCategory::SpaceSeparator
    )
}

/// A Python value built in the SDK's insertion order: a JSON leaf, or a
/// list/dict whose members keep the order the SDK code wrote them in.
#[derive(Clone, Debug, PartialEq)]
pub(in crate::toolkits) enum PyValue {
    Json(Value),
    List(Vec<PyValue>),
    Dict(Vec<(String, PyValue)>),
}

impl PyValue {
    /// A string leaf.
    pub(in crate::toolkits) fn text(value: impl Into<String>) -> Self {
        Self::Json(Value::String(value.into()))
    }

    /// `repr(value)` in insertion order.
    pub(in crate::toolkits) fn repr(&self) -> String {
        let mut output = String::new();
        self.write(&mut output);
        output
    }

    fn write(&self, output: &mut String) {
        match self {
            Self::Json(value) => write_value(output, value),
            Self::List(items) => {
                output.push('[');
                for (index, item) in items.iter().enumerate() {
                    if index > 0 {
                        output.push_str(", ");
                    }
                    item.write(output);
                }
                output.push(']');
            }
            Self::Dict(members) => {
                output.push('{');
                for (index, (key, value)) in members.iter().enumerate() {
                    if index > 0 {
                        output.push_str(", ");
                    }
                    write_str(output, key);
                    output.push_str(": ");
                    value.write(output);
                }
                output.push('}');
            }
        }
    }

    /// The JSON `json.dumps` of this value describes (member order is the
    /// map's own).
    pub(in crate::toolkits) fn to_json(&self) -> Value {
        match self {
            Self::Json(value) => value.clone(),
            Self::List(items) => Value::Array(items.iter().map(Self::to_json).collect()),
            Self::Dict(members) => Value::Object(
                members
                    .iter()
                    .map(|(key, value)| (key.clone(), value.to_json()))
                    .collect(),
            ),
        }
    }

    /// `dict.get(key)` on a dict built in order.
    pub(in crate::toolkits) fn get(&self, key: &str) -> Option<&Self> {
        match self {
            Self::Dict(members) => members
                .iter()
                .find(|(name, _)| name == key)
                .map(|(_, value)| value),
            _ => None,
        }
    }

    /// `dict[key] = value`, keeping an existing key's position.
    pub(in crate::toolkits) fn set(&mut self, key: &str, value: Self) {
        if let Self::Dict(members) = self {
            if let Some(member) = members.iter_mut().find(|(name, _)| name == key) {
                member.1 = value;
            } else {
                members.push((key.to_owned(), value));
            }
        }
    }

    /// A JSON value as an ordered value, objects in sorted key order (the
    /// same order [`repr`] uses, whatever the `serde_json` features).
    pub(in crate::toolkits) fn from_json(value: &Value) -> Self {
        match value {
            Value::Array(items) => Self::List(items.iter().map(Self::from_json).collect()),
            Value::Object(object) => Self::Dict(
                sorted_members(object)
                    .into_iter()
                    .map(|(key, value)| (key.to_owned(), Self::from_json(value)))
                    .collect(),
            ),
            other => Self::Json(other.clone()),
        }
    }
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

    /// Expected strings are what `CPython`'s `repr` prints for the same value.
    #[test]
    fn strings_escape_like_cpython() {
        // C0 controls and DEL as \xhh; C1 controls, NBSP and the soft hyphen
        // (Cc, Zs, Cf) as \xhh too; printable Latin-1 as itself.
        assert_eq!(repr_str("\u{0}\u{1f}\u{7f}"), "'\\x00\\x1f\\x7f'");
        assert_eq!(
            repr_str("\u{80}\u{9f}\u{a0}\u{ad}"),
            "'\\x80\\x9f\\xa0\\xad'"
        );
        assert_eq!(repr_str("\u{a1}é\u{ff}"), "'\u{a1}é\u{ff}'");
        // Line/paragraph separators, ideographic space, zero-width space and
        // BOM as \uhhhh; private use and unassigned planes as \Uhhhhhhhh.
        assert_eq!(
            repr_str("\u{2028}\u{2029}\u{3000}\u{200b}\u{feff}"),
            "'\\u2028\\u2029\\u3000\\u200b\\ufeff'"
        );
        assert_eq!(repr_str("\u{f0000}\u{e0001}"), "'\\U000f0000\\U000e0001'");
        // Printable non-Latin text and emoji stay as themselves.
        assert_eq!(repr_str("Привет 日本 😀"), "'Привет 日本 😀'");
        assert_eq!(repr_str("both ' and \""), "'both \\' and \"'");
    }

    #[test]
    fn numbers_follow_cpython() {
        assert_eq!(repr(&json!(0.0001)), "0.0001");
        assert_eq!(repr(&json!(9_999_999_999_999_998.0)), "9999999999999998.0");
        assert_eq!(repr(&json!(1e22)), "1e+22");
        assert_eq!(repr(&json!(1.5e-7)), "1.5e-07");
        assert_eq!(repr(&json!(-0.0)), "-0.0");
        assert_eq!(repr(&json!(100.0)), "100.0");
        assert_eq!(repr(&json!(u64::MAX)), "18446744073709551615");
    }

    /// Passes with and without `--features test-preserve-order`: the members
    /// were inserted out of order, and repr sorts them like `crate::canonical`.
    #[test]
    fn object_members_render_sorted_whatever_the_serde_json_features() {
        let value: serde_json::Value = serde_json::from_str(
            r#"{"zeta": {"b": 1, "a": [{"y": 2, "x": 1}]}, "Beta": true, "alpha": null}"#,
        )
        .expect("fixture");
        let expected = "{'Beta': True, 'alpha': None, 'zeta': {'a': [{'x': 1, 'y': 2}], 'b': 1}}";
        assert_eq!(repr(&value), expected);
        assert_eq!(super::PyValue::from_json(&value).repr(), expected);
        assert_eq!(
            crate::canonical::to_string(&value).expect("canonical"),
            r#"{"Beta":true,"alpha":null,"zeta":{"a":[{"x":1,"y":2}],"b":1}}"#
        );
    }
}
