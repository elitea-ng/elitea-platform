//! A small JSON object attribute of a node or an edge (`annotations`,
//! `extra`).
//!
//! A JSON object in insertion order, as a `serde_json::Map` is, but stored as
//! a vector of entries with no spare capacity and no hash table. An edge
//! has one to three annotations and rarely any extra attribute, and the
//! graph holds hundreds of thousands of edges: a `Map` (a hash table, room
//! for more entries than it holds, and 72 bytes inline) was the largest part
//! of an edge. Lookups are a linear search over those few entries.
//!
//! The keys the builders and parsers use are borrowed from a fixed list
//! ([`Label`]), so they are not allocated once per edge.

use super::Label;
use serde_json::{Map, Value};

/// The keys stored without an allocation: every annotation key a parser or
/// a graph pass writes, and the common `extra` keys.
const KNOWN_KEYS: &[&str] = &[
    "provenance",
    "api_surface",
    "source_context",
    "target_context",
    "via",
    "is_method_call",
    "cross_file",
    "member_type",
    "confidence",
    "is_package_call",
    "implicit",
    "obj_kind",
    "method_level",
    "is_static",
    "reference_type",
    "impl_kind",
    "matcher",
    "field_name",
    "field_type",
    "is_abstract",
    "is_readonly",
    "is_classmethod",
    "is_property",
    "decorators",
    "creation_context",
    "detection_method",
    "is_dot",
    "is_blank",
    "alias",
    "container_type",
    "table_name",
];

fn key_label(key: String) -> Label {
    match KNOWN_KEYS.iter().find(|known| **known == key) {
        Some(known) => Label::Borrowed(known),
        None => Label::Owned(key),
    }
}

/// A JSON object in insertion order, compact (see the module docs).
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Attributes(Vec<(Label, Value)>);

impl Attributes {
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    #[must_use]
    pub fn len(&self) -> usize {
        self.0.len()
    }

    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.0.is_empty()
    }

    fn position(&self, key: &str) -> Option<usize> {
        self.0.iter().position(|(k, _)| k == key)
    }

    #[must_use]
    pub fn get(&self, key: &str) -> Option<&Value> {
        self.0
            .iter()
            .find(|(k, _)| k == key)
            .map(|(_, value)| value)
    }

    pub fn get_mut(&mut self, key: &str) -> Option<&mut Value> {
        self.0
            .iter_mut()
            .find(|(k, _)| k == key)
            .map(|(_, value)| value)
    }

    #[must_use]
    pub fn contains_key(&self, key: &str) -> bool {
        self.position(key).is_some()
    }

    /// Set `key`, as `Map::insert`: an existing key keeps its position and
    /// gets the new value (the old one is returned); a new key goes last.
    pub fn insert(&mut self, key: impl Into<String>, value: Value) -> Option<Value> {
        let key = key.into();
        if let Some(index) = self.position(&key) {
            return self
                .0
                .get_mut(index)
                .map(|(_, old)| std::mem::replace(old, value));
        }
        self.0.reserve_exact(1);
        self.0.push((key_label(key), value));
        None
    }

    /// Insert every entry of `other` in its order (`Map::extend`).
    pub fn extend(&mut self, other: Attributes) {
        for (key, value) in other.0 {
            if let Some(index) = self.position(&key) {
                if let Some((_, old)) = self.0.get_mut(index) {
                    *old = value;
                }
            } else {
                self.0.push((key, value));
            }
        }
    }

    /// The entries in insertion order.
    pub fn iter(&self) -> impl Iterator<Item = (&str, &Value)> {
        self.0.iter().map(|(key, value)| (&**key, value))
    }

    /// The annotations as a JSON object, in insertion order.
    #[must_use]
    pub fn to_map(&self) -> Map<String, Value> {
        let mut map = Map::with_capacity(self.0.len());
        for (key, value) in &self.0 {
            map.insert(key.to_string(), value.clone());
        }
        map
    }
}

impl From<Map<String, Value>> for Attributes {
    fn from(map: Map<String, Value>) -> Self {
        let mut entries = Vec::with_capacity(map.len());
        entries.extend(map.into_iter().map(|(key, value)| (key_label(key), value)));
        Self(entries)
    }
}

impl From<&Map<String, Value>> for Attributes {
    /// A copy with no spare capacity.
    fn from(map: &Map<String, Value>) -> Self {
        let mut entries = Vec::with_capacity(map.len());
        entries.extend(
            map.iter()
                .map(|(key, value)| (key_label(key.clone()), value.clone())),
        );
        Self(entries)
    }
}

impl std::ops::Index<&str> for Attributes {
    type Output = Value;

    /// The value of `key`; `Value::Null` when missing, as indexing a
    /// `serde_json::Value` does.
    fn index(&self, key: &str) -> &Value {
        static NULL: Value = Value::Null;
        self.get(key).unwrap_or(&NULL)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn keeps_insertion_order_and_replaces_in_place() {
        let mut annotations = Attributes::new();
        annotations.insert("b", json!(1));
        annotations.insert("via", json!([]));
        annotations.insert("custom_key", json!("x"));
        assert_eq!(annotations.insert("b", json!(2)), Some(json!(1)));
        let keys: Vec<&str> = annotations.iter().map(|(k, _)| k).collect();
        assert_eq!(keys, ["b", "via", "custom_key"]);
        assert_eq!(annotations["b"], json!(2));
        assert_eq!(annotations["missing"], Value::Null);
        assert_eq!(
            Value::Object(annotations.to_map()).to_string(),
            r#"{"b":2,"via":[],"custom_key":"x"}"#
        );
        let map = annotations.to_map();
        assert_eq!(Attributes::from(&map), annotations);
        assert_eq!(Attributes::from(map), annotations);
    }
}
