//! Single-pass settings decoding that refuses duplicate members.
//!
//! `serde_json::Value` keeps the last of two equal keys, but the Python worker
//! refuses the document (`parse_settings_json`'s `_unique_object`), so the
//! same refusal is made here while the value is built: one parse yields the
//! `Value` and the duplicate check together.

use std::collections::BTreeSet;
use std::fmt;

use serde::de::{self, Deserializer, MapAccess, SeqAccess, Visitor};
use serde_json::{Map, Number, Value};

/// `serde_json`'s private single-member map key that carries a number's literal
/// text under `arbitrary_precision`.
const NUMBER_MARKER: &str = "$serde_json::private::Number";

/// `Err(())` for a duplicated member anywhere in the document or for any JSON
/// syntax error; the caller treats both as malformed input.
#[allow(clippy::result_unit_err)]
pub(super) fn parse_without_duplicates(raw: &[u8]) -> Result<Value, ()> {
    serde_json::from_slice::<StrictValue>(raw)
        .map(|StrictValue(value)| value)
        .map_err(|_| ())
}

struct StrictValue(Value);

impl<'de> de::Deserialize<'de> for StrictValue {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        deserializer.deserialize_any(StrictValueVisitor)
    }
}

struct StrictValueVisitor;

impl<'de> Visitor<'de> for StrictValueVisitor {
    type Value = StrictValue;

    fn expecting(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("a JSON value without duplicate members")
    }

    fn visit_bool<E: de::Error>(self, value: bool) -> Result<Self::Value, E> {
        Ok(StrictValue(Value::Bool(value)))
    }

    fn visit_i64<E: de::Error>(self, value: i64) -> Result<Self::Value, E> {
        Ok(StrictValue(Value::Number(value.into())))
    }

    fn visit_u64<E: de::Error>(self, value: u64) -> Result<Self::Value, E> {
        Ok(StrictValue(Value::Number(value.into())))
    }

    fn visit_f64<E: de::Error>(self, value: f64) -> Result<Self::Value, E> {
        Number::from_f64(value)
            .map(|number| StrictValue(Value::Number(number)))
            .ok_or_else(|| E::custom("non-finite number"))
    }

    fn visit_str<E: de::Error>(self, value: &str) -> Result<Self::Value, E> {
        Ok(StrictValue(Value::String(value.to_owned())))
    }

    fn visit_string<E: de::Error>(self, value: String) -> Result<Self::Value, E> {
        Ok(StrictValue(Value::String(value)))
    }

    fn visit_unit<E: de::Error>(self) -> Result<Self::Value, E> {
        Ok(StrictValue(Value::Null))
    }

    fn visit_seq<A: SeqAccess<'de>>(self, mut seq: A) -> Result<Self::Value, A::Error> {
        let mut items = Vec::new();
        while let Some(StrictValue(item)) = seq.next_element()? {
            items.push(item);
        }
        Ok(StrictValue(Value::Array(items)))
    }

    fn visit_map<A: MapAccess<'de>>(self, mut map: A) -> Result<Self::Value, A::Error> {
        let Some(first) = map.next_key::<String>()? else {
            return Ok(StrictValue(Value::Object(Map::new())));
        };
        // With `arbitrary_precision` every number arrives as a one-member map
        // under serde_json's private marker key; that is a number, never an
        // object, and never a duplicate.
        if first == NUMBER_MARKER {
            let literal: String = map.next_value()?;
            let number = serde_json::from_str::<Number>(&literal)
                .map_err(|_| de::Error::custom("malformed number"))?;
            return Ok(StrictValue(Value::Number(number)));
        }
        let mut seen = BTreeSet::new();
        let mut members = Map::new();
        let mut key = Some(first);
        while let Some(current) = key {
            if !seen.insert(current.clone()) {
                return Err(de::Error::custom("duplicate member"));
            }
            let StrictValue(value) = map.next_value()?;
            members.insert(current, value);
            key = map.next_key::<String>()?;
        }
        Ok(StrictValue(Value::Object(members)))
    }
}
