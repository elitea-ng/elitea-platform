//! Shared bounded core of the `split_out` and `aggregate` pipeline nodes.
//!
//! The binding contract is `docs/split-out-aggregate-contract.md`. Every walk
//! here is iterative, so adversarial nesting never grows the call stack, and no
//! error or log text carries data values, pointer text or state values.
// Consumed by split_out.rs and aggregate.rs (Track A2/A3).
#![cfg_attr(not(test), allow(dead_code))]

use std::collections::BTreeSet;
use std::fmt::Write as _;
use std::io;

use adk_rust::graph::GraphError;
use ring::digest;
use serde::de::{DeserializeOwned, Error as _};
use serde::{Deserialize, Deserializer};
use serde_json::{Map, Number, Value};
use thiserror::Error;

pub(super) const MAX_NODE_YAML_BYTES: usize = 64 * 1024;
pub(super) const MAX_POINTER_BYTES: usize = 512;
pub(super) const MAX_POINTER_TOKENS: usize = 32;
pub(super) const MAX_NAME_BYTES: usize = 256;
pub(super) const MAX_SELECTIONS: usize = 64;

const LIMIT_CEILINGS: [u64; 6] = [10_000, 10_000, 1_000, 524_288, 32, 32_768];
// An i64 or u64 magnitude has at most 20 decimal digits; 10^20 fits u128.
const MAX_INTEGER_DIGITS: u64 = 20;

/// Configuration failure, surfaced as `graph.pipeline.invalid_configuration`.
#[derive(Debug, Error)]
pub(super) enum ShapingConfigurationError {
    #[error("the data shaping node exceeds its resource bound")]
    ResourceExhausted,
    #[error("the data shaping YAML is malformed")]
    MalformedYaml {
        #[source]
        source: serde_yaml_ng::Error,
    },
    #[error("the data shaping node is invalid: {0}")]
    Invalid(&'static str),
}

/// Parses one bounded node YAML document into its strict raw form.
pub(super) fn parse_node_yaml<T: DeserializeOwned>(
    yaml: &str,
) -> Result<T, ShapingConfigurationError> {
    if yaml.is_empty() || yaml.len() > MAX_NODE_YAML_BYTES {
        return Err(ShapingConfigurationError::ResourceExhausted);
    }
    serde_yaml_ng::from_str(yaml)
        .map_err(|source| ShapingConfigurationError::MalformedYaml { source })
}

/// A literal object key: non-empty, bounded, without control characters.
pub(super) fn valid_name(value: &str) -> bool {
    !value.is_empty() && value.len() <= MAX_NAME_BYTES && !value.chars().any(char::is_control)
}

// ---------------------------------------------------------------- pointers

/// RFC 6901 JSON pointer, validated when the pipeline compiles.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) struct Pointer {
    text: String,
    tokens: Vec<String>,
}

impl Pointer {
    pub(super) fn parse(text: &str) -> Result<Self, ShapingConfigurationError> {
        const INVALID: ShapingConfigurationError =
            ShapingConfigurationError::Invalid("a JSON pointer is malformed");
        let Some(body) = text.strip_prefix('/') else {
            return Err(INVALID);
        };
        if text.len() > MAX_POINTER_BYTES {
            return Err(INVALID);
        }
        let mut tokens = Vec::new();
        for raw in body.split('/') {
            if tokens.len() == MAX_POINTER_TOKENS {
                return Err(INVALID);
            }
            let mut token = String::with_capacity(raw.len());
            let mut chars = raw.chars();
            while let Some(character) = chars.next() {
                let decoded = if character == '~' {
                    match chars.next() {
                        Some('0') => '~',
                        Some('1') => '/',
                        _ => return Err(INVALID),
                    }
                } else {
                    character
                };
                token.push(decoded);
            }
            tokens.push(token);
        }
        Ok(Self {
            text: text.to_owned(),
            tokens,
        })
    }

    /// The authored text, for config digests only (never for messages).
    pub(super) fn as_str(&self) -> &str {
        &self.text
    }

    pub(super) fn first_token(&self) -> &str {
        self.tokens.first().map_or("", String::as_str)
    }

    pub(super) fn token_count(&self) -> usize {
        self.tokens.len()
    }

    /// Resolves the pointer; `None` is missing, `Some(Null)` is null.
    pub(super) fn resolve<'a>(&self, root: &'a Value) -> Option<&'a Value> {
        let mut current = root;
        for token in &self.tokens {
            current = match current {
                Value::Object(map) => map.get(token)?,
                Value::Array(items) => items.get(array_index(token)?)?,
                _ => return None,
            };
        }
        Some(current)
    }

    /// Removes the target from its containing object or array slot.
    pub(super) fn remove_from(&self, root: &mut Value) -> bool {
        let Some((last, prefix)) = self.tokens.split_last() else {
            return false;
        };
        let mut current = root;
        for token in prefix {
            let next = match current {
                Value::Object(map) => map.get_mut(token),
                Value::Array(items) => array_index(token).and_then(|index| items.get_mut(index)),
                _ => None,
            };
            let Some(next) = next else {
                return false;
            };
            current = next;
        }
        match current {
            Value::Object(map) => map.remove(last).is_some(),
            Value::Array(items) => match array_index(last) {
                Some(index) if index < items.len() => {
                    items.remove(index);
                    true
                }
                _ => false,
            },
            _ => false,
        }
    }
}

/// Canonical decimal array index: `0` or `[1-9][0-9]*`, never `-`.
fn array_index(token: &str) -> Option<usize> {
    let canonical = token == "0"
        || (!token.starts_with('0')
            && !token.is_empty()
            && token.bytes().all(|byte| byte.is_ascii_digit()));
    if canonical { token.parse().ok() } else { None }
}

// ---------------------------------------------------------------- selection

/// What a missing pointer does. YAML `null` (bare or quoted) selects `Null`.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub(super) enum MissingPolicy {
    #[default]
    Error,
    Null,
    Skip,
}

impl MissingPolicy {
    pub(super) const fn tag(self) -> u8 {
        match self {
            Self::Error => 0,
            Self::Null => 1,
            Self::Skip => 2,
        }
    }
}

impl<'de> Deserialize<'de> for MissingPolicy {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        match Option::<String>::deserialize(deserializer)?.as_deref() {
            Some("error") => Ok(Self::Error),
            None | Some("null") => Ok(Self::Null),
            Some("skip") => Ok(Self::Skip),
            Some(_) => Err(D::Error::custom(
                "unknown missing policy, expected error, null or skip",
            )),
        }
    }
}

/// What a JSON null does after the missing policy ran.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
pub(super) enum NullPolicy {
    #[default]
    Keep,
    Error,
    Skip,
}

impl NullPolicy {
    pub(super) const fn tag(self) -> u8 {
        match self {
            Self::Keep => 0,
            Self::Error => 1,
            Self::Skip => 2,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) struct FieldSelection {
    pub(super) pointer: Pointer,
    pub(super) missing: MissingPolicy,
    pub(super) null: NullPolicy,
}

/// Outcome of one selection. `Value` never holds JSON null.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum Selected<'a> {
    Value(&'a Value),
    Null,
    Skip,
}

impl FieldSelection {
    /// Resolve, then the missing policy, then the null policy.
    pub(super) fn select<'a>(&self, row: &'a Value) -> Result<Selected<'a>, ShapingCode> {
        match self.pointer.resolve(row) {
            Some(Value::Null) => self.on_null(),
            Some(value) => Ok(Selected::Value(value)),
            None => match self.missing {
                MissingPolicy::Error => Err(ShapingCode::MissingField),
                MissingPolicy::Skip => Ok(Selected::Skip),
                MissingPolicy::Null => self.on_null(),
            },
        }
    }

    fn on_null<'a>(&self) -> Result<Selected<'a>, ShapingCode> {
        match self.null {
            NullPolicy::Keep => Ok(Selected::Null),
            NullPolicy::Error => Err(ShapingCode::NullValue),
            NullPolicy::Skip => Ok(Selected::Skip),
        }
    }
}

// ---------------------------------------------------------------- retain

#[derive(Clone, Copy, Debug, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
pub(super) enum RetainMode {
    None,
    All,
    Only,
    Except,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub(super) struct RawRetain {
    mode: RetainMode,
    #[serde(default)]
    fields: Option<Vec<RawRetainEntry>>,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(untagged)]
enum RawRetainEntry {
    Name(String),
    Selection(RawRetainedField),
}

#[derive(Clone, Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawRetainedField {
    path: String,
    output: String,
    #[serde(default)]
    missing: MissingPolicy,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) struct RetainedField {
    pointer: Pointer,
    output: String,
    missing: MissingPolicy,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) enum RetainSpec {
    None,
    All,
    Only(Vec<RetainedField>),
    Except(Vec<String>),
}

/// A projection failure; `field` is the `retain.fields` index when one applies.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) struct RetainFailure {
    pub(super) code: ShapingCode,
    pub(super) field: Option<usize>,
}

impl RetainSpec {
    /// Validates the shared rules. Callers refuse modes their node forbids.
    pub(super) fn from_raw(raw: RawRetain) -> Result<Self, ShapingConfigurationError> {
        const MALFORMED: ShapingConfigurationError =
            ShapingConfigurationError::Invalid("a retain field is malformed");
        let entries = match (raw.mode, raw.fields) {
            (RetainMode::None, None) => return Ok(Self::None),
            (RetainMode::All, None) => return Ok(Self::All),
            (RetainMode::None | RetainMode::All, Some(_)) => {
                return Err(ShapingConfigurationError::Invalid(
                    "retain fields are only allowed for only and except",
                ));
            }
            (_, entries) => entries.unwrap_or_default(),
        };
        if entries.len() > MAX_SELECTIONS {
            return Err(ShapingConfigurationError::ResourceExhausted);
        }
        let mut seen = BTreeSet::new();
        if raw.mode == RetainMode::Except {
            let mut names = Vec::with_capacity(entries.len());
            for entry in entries {
                let RawRetainEntry::Name(name) = entry else {
                    return Err(MALFORMED);
                };
                if !valid_name(&name) || !seen.insert(name.clone()) {
                    return Err(MALFORMED);
                }
                names.push(name);
            }
            return Ok(Self::Except(names));
        }
        let mut fields = Vec::with_capacity(entries.len());
        for entry in entries {
            let RawRetainEntry::Selection(field) = entry else {
                return Err(MALFORMED);
            };
            if !valid_name(&field.output) || !seen.insert(field.output.clone()) {
                return Err(MALFORMED);
            }
            fields.push(RetainedField {
                pointer: Pointer::parse(&field.path)?,
                output: field.output,
                missing: field.missing,
            });
        }
        Ok(Self::Only(fields))
    }

    pub(super) const fn mode(&self) -> RetainMode {
        match self {
            Self::None => RetainMode::None,
            Self::All => RetainMode::All,
            Self::Only(_) => RetainMode::Only,
            Self::Except(_) => RetainMode::Except,
        }
    }

    /// Output names of `only` selections (empty for the other modes).
    pub(super) fn outputs(&self) -> impl Iterator<Item = &str> {
        let fields = match self {
            Self::Only(fields) => fields.as_slice(),
            _ => &[],
        };
        fields.iter().map(|field| field.output.as_str())
    }

    pub(super) fn project(&self, parent: &Value) -> Result<Map<String, Value>, RetainFailure> {
        let invalid_row = RetainFailure {
            code: ShapingCode::InvalidRow,
            field: None,
        };
        match self {
            Self::None => Ok(Map::new()),
            Self::All => parent.as_object().cloned().ok_or(invalid_row),
            Self::Except(names) => {
                let mut map = parent.as_object().cloned().ok_or(invalid_row)?;
                for name in names {
                    map.remove(name);
                }
                Ok(map)
            }
            Self::Only(fields) => {
                let mut map = Map::new();
                for (index, field) in fields.iter().enumerate() {
                    let value = match (field.pointer.resolve(parent), field.missing) {
                        (Some(value), _) => value.clone(),
                        (None, MissingPolicy::Null) => Value::Null,
                        (None, MissingPolicy::Skip) => continue,
                        (None, MissingPolicy::Error) => {
                            return Err(RetainFailure {
                                code: ShapingCode::MissingField,
                                field: Some(index),
                            });
                        }
                    };
                    map.insert(field.output.clone(), value);
                }
                Ok(map)
            }
        }
    }

    pub(super) fn digest_into(&self, context: &mut digest::Context) {
        let mode = match self {
            Self::None => 0_u8,
            Self::All => 1,
            Self::Only(_) => 2,
            Self::Except(_) => 3,
        };
        context.update(&[mode]);
        match self {
            Self::None | Self::All => {}
            Self::Only(fields) => {
                context.update(&(fields.len() as u64).to_be_bytes());
                for field in fields {
                    digest_field(context, field.pointer.as_str().as_bytes());
                    digest_field(context, field.output.as_bytes());
                    context.update(&[field.missing.tag()]);
                }
            }
            Self::Except(names) => {
                context.update(&(names.len() as u64).to_be_bytes());
                for name in names {
                    digest_field(context, name.as_bytes());
                }
            }
        }
    }
}

// ---------------------------------------------------------------- envelope

/// A strict `{parent_index, position, data}` row emitted by `split_out`.
#[derive(Clone, Copy, Debug)]
pub(super) struct Envelope<'a> {
    pub(super) parent_index: u64,
    pub(super) position: u64,
    pub(super) data: &'a Map<String, Value>,
}

pub(super) fn parse_envelope(row: &Value) -> Option<Envelope<'_>> {
    let object = row.as_object()?;
    if object.len() != 3 {
        return None;
    }
    let index = |key: &str| exact_u64(object.get(key)?.as_number()?).ok();
    Some(Envelope {
        parent_index: index("parent_index")?,
        position: index("position")?,
        data: object.get("data")?.as_object()?,
    })
}

pub(super) fn envelope_row(parent_index: u64, position: u64, data: Map<String, Value>) -> Value {
    let mut row = Map::new();
    row.insert("data".to_owned(), Value::Object(data));
    row.insert("parent_index".to_owned(), Value::from(parent_index));
    row.insert("position".to_owned(), Value::from(position));
    Value::Object(row)
}

// ---------------------------------------------------------------- numbers

/// Exact decimal `(-1)^negative * digits * 10^exponent`. `digits` has no
/// leading or trailing zeros; zero is `(false, "", 0)`, so the form is unique.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) struct Decimal {
    pub(super) negative: bool,
    pub(super) digits: String,
    pub(super) exponent: i64,
}

/// Normalizes the lexical JSON number text without floating point.
pub(super) fn decimal(number: &Number) -> Result<Decimal, ShapingCode> {
    const UNSUPPORTED: ShapingCode = ShapingCode::UnsupportedNumber;
    let text = number.as_str();
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

/// The exact integer magnitude; fractional is `type_mismatch`, more than 20
/// digits is `integer_overflow`.
fn exact_integer(number: &Number) -> Result<(bool, u128), ShapingCode> {
    let decimal = decimal(number)?;
    if decimal.exponent < 0 {
        return Err(ShapingCode::TypeMismatch);
    }
    let exponent = decimal.exponent.unsigned_abs();
    let length = u64::try_from(decimal.digits.len()).map_err(|_| ShapingCode::IntegerOverflow)?;
    if length
        .checked_add(exponent)
        .is_none_or(|total| total > MAX_INTEGER_DIGITS)
    {
        return Err(ShapingCode::IntegerOverflow);
    }
    let mut magnitude = 0_u128;
    for byte in decimal.digits.bytes() {
        magnitude = magnitude
            .checked_mul(10)
            .and_then(|value| value.checked_add(u128::from(byte - b'0')))
            .ok_or(ShapingCode::IntegerOverflow)?;
    }
    for _ in 0..exponent {
        magnitude = magnitude
            .checked_mul(10)
            .ok_or(ShapingCode::IntegerOverflow)?;
    }
    Ok((decimal.negative, magnitude))
}

pub(super) fn exact_i64(number: &Number) -> Result<i64, ShapingCode> {
    let (negative, magnitude) = exact_integer(number)?;
    let signed = i128::try_from(magnitude).map_err(|_| ShapingCode::IntegerOverflow)?;
    i64::try_from(if negative { -signed } else { signed }).map_err(|_| ShapingCode::IntegerOverflow)
}

pub(super) fn exact_u64(number: &Number) -> Result<u64, ShapingCode> {
    match exact_integer(number)? {
        (true, _) => Err(ShapingCode::TypeMismatch),
        (false, magnitude) => u64::try_from(magnitude).map_err(|_| ShapingCode::IntegerOverflow),
    }
}

// ---------------------------------------------------------------- canonical keys

/// Injective, type-tagged, length-prefixed encoding under canonical equality.
/// Each encoded value is self-delimiting, so a pushed tuple is injective too.
#[derive(Clone, Debug, Default, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub(super) struct CanonicalKey(Vec<u8>);

enum KeyItem<'a> {
    Value(&'a Value, usize),
    Key(&'a str),
}

impl CanonicalKey {
    /// Appends one component; values deeper than `max_depth` (root = 1) fail.
    pub(super) fn push(&mut self, value: &Value, max_depth: usize) -> Result<(), ShapingCode> {
        let buffer = &mut self.0;
        let mut stack = vec![KeyItem::Value(value, 1)];
        while let Some(item) = stack.pop() {
            let (value, depth) = match item {
                KeyItem::Key(key) => {
                    push_bytes(buffer, key.as_bytes());
                    continue;
                }
                KeyItem::Value(value, depth) => (value, depth),
            };
            if depth > max_depth {
                return Err(ShapingCode::LimitExceeded);
            }
            match value {
                Value::Null => buffer.push(0),
                Value::Bool(false) => buffer.push(1),
                Value::Bool(true) => buffer.push(2),
                Value::Number(number) => {
                    let decimal = decimal(number)?;
                    buffer.push(3);
                    buffer.push(u8::from(decimal.negative));
                    push_bytes(buffer, decimal.digits.as_bytes());
                    buffer.extend_from_slice(&decimal.exponent.to_be_bytes());
                }
                Value::String(text) => {
                    buffer.push(4);
                    push_bytes(buffer, text.as_bytes());
                }
                Value::Array(items) => {
                    buffer.push(5);
                    buffer.extend_from_slice(&(items.len() as u64).to_be_bytes());
                    stack.extend(
                        items
                            .iter()
                            .rev()
                            .map(|item| KeyItem::Value(item, depth + 1)),
                    );
                }
                Value::Object(map) => {
                    buffer.push(6);
                    buffer.extend_from_slice(&(map.len() as u64).to_be_bytes());
                    for (key, item) in map.iter().rev() {
                        stack.push(KeyItem::Value(item, depth + 1));
                        stack.push(KeyItem::Key(key));
                    }
                }
            }
        }
        Ok(())
    }
}

pub(super) fn canonical_key(value: &Value, max_depth: usize) -> Result<CanonicalKey, ShapingCode> {
    let mut key = CanonicalKey::default();
    key.push(value, max_depth)?;
    Ok(key)
}

fn push_bytes(buffer: &mut Vec<u8>, bytes: &[u8]) {
    buffer.extend_from_slice(&(bytes.len() as u64).to_be_bytes());
    buffer.extend_from_slice(bytes);
}

// ---------------------------------------------------------------- limits

#[derive(Clone, Copy, Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub(super) struct RawLimits {
    pub(super) input_items: Option<u64>,
    pub(super) output_items: Option<u64>,
    pub(super) groups: Option<u64>,
    pub(super) bytes: Option<u64>,
    pub(super) depth: Option<u64>,
    pub(super) values: Option<u64>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum ShapingLimitKind {
    InputItems,
    OutputItems,
    Groups,
    Bytes,
    Depth,
    Values,
}

impl ShapingLimitKind {
    pub(super) const fn field(self) -> &'static str {
        match self {
            Self::InputItems => "limits.input_items",
            Self::OutputItems => "limits.output_items",
            Self::Groups => "limits.groups",
            Self::Bytes => "limits.bytes",
            Self::Depth => "limits.depth",
            Self::Values => "limits.values",
        }
    }
}

/// Effective limits; `groups` is `None` for `split_out`.
#[derive(Clone, Copy, Debug)]
pub(super) struct ShapingLimits {
    pub(super) input_items: usize,
    pub(super) output_items: usize,
    pub(super) groups: Option<usize>,
    pub(super) bytes: usize,
    pub(super) depth: usize,
    pub(super) values: usize,
    authored: [Option<u64>; 6],
}

impl ShapingLimits {
    pub(super) fn from_raw(
        raw: Option<&RawLimits>,
        allow_groups: bool,
    ) -> Result<Self, ShapingConfigurationError> {
        let raw = raw.copied().unwrap_or_default();
        if raw.groups.is_some() && !allow_groups {
            return Err(ShapingConfigurationError::Invalid(
                "limits.groups only applies to aggregate",
            ));
        }
        let authored = [
            raw.input_items,
            raw.output_items,
            raw.groups,
            raw.bytes,
            raw.depth,
            raw.values,
        ];
        let mut effective = [0_usize; 6];
        for ((slot, value), ceiling) in effective.iter_mut().zip(authored).zip(LIMIT_CEILINGS) {
            let value = value.unwrap_or(ceiling);
            if !(1..=ceiling).contains(&value) {
                return Err(ShapingConfigurationError::Invalid(
                    "a shaping limit must be between 1 and its ceiling",
                ));
            }
            *slot =
                usize::try_from(value).map_err(|_| ShapingConfigurationError::ResourceExhausted)?;
        }
        let [input_items, output_items, groups, bytes, depth, values] = effective;
        Ok(Self {
            input_items,
            output_items,
            groups: allow_groups.then_some(groups),
            bytes,
            depth,
            values,
            authored,
        })
    }

    /// Digests the authored values with a presence tag, in a fixed order.
    pub(super) fn digest_into(&self, context: &mut digest::Context) {
        for value in self.authored {
            match value {
                Some(value) => {
                    context.update(&[1]);
                    context.update(&value.to_be_bytes());
                }
                None => context.update(&[0]),
            }
        }
    }

    /// Checked before any work.
    pub(super) const fn check_input(&self, len: usize) -> Result<(), ShapingLimitKind> {
        if len > self.input_items {
            return Err(ShapingLimitKind::InputItems);
        }
        Ok(())
    }

    /// Checked on insertion of a group (or a parent with `regroup: parent`).
    pub(super) fn check_groups(&self, count: usize) -> Result<(), ShapingLimitKind> {
        match self.groups {
            Some(groups) if count > groups => Err(ShapingLimitKind::Groups),
            _ => Ok(()),
        }
    }
}

// ---------------------------------------------------------------- budget

/// Running charge of the emitted list. It starts with the list itself: `[]`
/// (2 bytes), 1 value at depth 1.
///
/// Use one `Budget` for exact rows (`charge_row`). Aggregate uses a separate
/// `Budget` for its lower-bound accumulation (`charge_value`), charging only
/// values that will certainly appear in the output once each: every `collect`
/// value (with `merge_lists`, each element, not the list) and every
/// `collect_rows` projection — never `first`/`last` candidates.
#[derive(Clone, Debug)]
pub(super) struct Budget {
    limits: ShapingLimits,
    items: usize,
    bytes: usize,
    values: usize,
}

impl Budget {
    pub(super) fn new(limits: &ShapingLimits) -> Result<Self, ShapingLimitKind> {
        if limits.bytes < 2 {
            return Err(ShapingLimitKind::Bytes);
        }
        Ok(Self {
            limits: *limits,
            items: 0,
            bytes: 2,
            values: 1,
        })
    }

    pub(super) const fn bytes_used(&self) -> usize {
        self.bytes
    }

    pub(super) const fn values_used(&self) -> usize {
        self.values
    }

    /// Charges one emitted row exactly: item, depth, values, then bytes
    /// (including its `,` separator). Nothing is committed on failure.
    pub(super) fn charge_row(&mut self, row: &Value) -> Result<(), ShapingLimitKind> {
        if self.items >= self.limits.output_items {
            return Err(ShapingLimitKind::OutputItems);
        }
        let values = self.count_values(row, 2)?;
        let separator = usize::from(self.items > 0);
        let bytes = self.serialized_len(row, separator)?;
        self.items += 1;
        self.values = values;
        self.bytes = bytes;
        Ok(())
    }

    /// Lower-bound charge of a value that will appear somewhere in the output
    /// (depth at least 2): its bytes and values, never separators. It never
    /// exceeds the exact charge of the final rows holding the same values.
    pub(super) fn charge_value(&mut self, value: &Value) -> Result<(), ShapingLimitKind> {
        let values = self.count_values(value, 2)?;
        let bytes = self.serialized_len(value, 0)?;
        self.values = values;
        self.bytes = bytes;
        Ok(())
    }

    /// Returns the new value total. Iterative, stops at the first violation.
    fn count_values(&self, root: &Value, root_depth: usize) -> Result<usize, ShapingLimitKind> {
        let mut total = self.values;
        let mut stack = vec![(root, root_depth)];
        while let Some((value, depth)) = stack.pop() {
            if depth > self.limits.depth {
                return Err(ShapingLimitKind::Depth);
            }
            total += 1;
            if total > self.limits.values {
                return Err(ShapingLimitKind::Values);
            }
            match value {
                Value::Array(items) => stack.extend(items.iter().map(|item| (item, depth + 1))),
                Value::Object(map) => stack.extend(map.values().map(|item| (item, depth + 1))),
                _ => {}
            }
        }
        Ok(total)
    }

    /// Returns the new byte total. Runs only after the depth walk passed, so
    /// serialization recursion is bounded by `limits.depth`.
    fn serialized_len(&self, value: &Value, separator: usize) -> Result<usize, ShapingLimitKind> {
        let used = self.bytes + separator;
        let mut writer = CappedWriter {
            remaining: self.limits.bytes.saturating_sub(used),
        };
        let available = writer.remaining;
        if used > self.limits.bytes || serde_json::to_writer(&mut writer, value).is_err() {
            return Err(ShapingLimitKind::Bytes);
        }
        Ok(used + (available - writer.remaining))
    }
}

struct CappedWriter {
    remaining: usize,
}

impl io::Write for CappedWriter {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        self.remaining = self
            .remaining
            .checked_sub(bytes.len())
            .ok_or_else(|| io::Error::other("shaping byte limit"))?;
        Ok(bytes.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

// ---------------------------------------------------------------- errors

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum ShapingCode {
    InvalidSource,
    InvalidRow,
    InvalidEnvelope,
    MissingField,
    NullValue,
    TypeMismatch,
    IntegerOverflow,
    UnsupportedNumber,
    FieldCollision,
    RegroupConflict,
    RegroupInconsistent,
    LimitExceeded,
}

impl ShapingCode {
    pub(super) const fn as_str(self) -> &'static str {
        match self {
            Self::InvalidSource => "invalid_source",
            Self::InvalidRow => "invalid_row",
            Self::InvalidEnvelope => "invalid_envelope",
            Self::MissingField => "missing_field",
            Self::NullValue => "null_value",
            Self::TypeMismatch => "type_mismatch",
            Self::IntegerOverflow => "integer_overflow",
            Self::UnsupportedNumber => "unsupported_number",
            Self::FieldCollision => "field_collision",
            Self::RegroupConflict => "regroup_conflict",
            Self::RegroupInconsistent => "regroup_inconsistent",
            Self::LimitExceeded => "limit_exceeded",
        }
    }
}

/// Runtime failure naming only a config field path and row indices.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) struct ShapingError {
    code: ShapingCode,
    field: Box<str>,
    item: Option<u64>,
    position: Option<u64>,
}

impl ShapingError {
    pub(super) fn new(code: ShapingCode, field: impl Into<Box<str>>) -> Self {
        Self {
            code,
            field: field.into(),
            item: None,
            position: None,
        }
    }

    pub(super) fn limit(kind: ShapingLimitKind) -> Self {
        Self::new(ShapingCode::LimitExceeded, kind.field())
    }

    #[must_use]
    pub(super) fn at_item(mut self, item: u64) -> Self {
        self.item = Some(item);
        self
    }

    #[must_use]
    pub(super) fn at_position(mut self, position: u64) -> Self {
        self.position = Some(position);
        self
    }

    pub(super) const fn code(&self) -> ShapingCode {
        self.code
    }

    pub(super) fn message(&self) -> String {
        let mut message = format!("graph.shaping.{}: {}", self.code.as_str(), self.field);
        if let Some(item) = self.item {
            let _ = write!(message, " at item {item}");
        }
        if let Some(position) = self.position {
            let _ = write!(message, " position {position}");
        }
        message
    }

    pub(super) fn into_graph_error(self, node: &str) -> GraphError {
        tracing::warn!(
            node_id = node,
            error_code = self.code.as_str(),
            config_field = &*self.field,
            item = self.item,
            position = self.position,
            "Data shaping node failed; no output was written"
        );
        GraphError::NodeExecutionFailed {
            node: node.to_owned(),
            message: self.message(),
        }
    }
}

// ---------------------------------------------------------------- digests

pub(super) fn digest_field(context: &mut digest::Context, value: &[u8]) {
    context.update(&(value.len() as u64).to_be_bytes());
    context.update(value);
}

pub(super) fn copy_digest(value: &[u8]) -> [u8; 32] {
    let mut output = [0_u8; 32];
    for (slot, byte) in output.iter_mut().zip(value) {
        *slot = *byte;
    }
    output
}
