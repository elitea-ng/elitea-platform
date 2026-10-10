//! The JSON chunker: `LangChain`'s `RecursiveJsonSplitter` (`convert_lists`
//! on), with Python's `json.dumps` for sizes and output.
//!
//! The splitter fills a chunk key by key while `len(json.dumps(chunk))`
//! stays under `max_chunk_size`; a value too big for the chunk's room starts
//! a new chunk (once the current one is at least `max - 200`, floor 50
//! characters) and is split below its own path. Sizes are in characters of
//! Python's default dump (`", "` and `": "` separators, `ensure_ascii`).
//!
//! Key order is the document's: this crate has its own ordered value type so
//! it does not need `serde_json`'s `preserve_order` feature.

use serde::de::{Deserialize, Deserializer, MapAccess, SeqAccess, Visitor};
use std::fmt::{self, Write as _};

/// SDK default (`json_chunker`): `max_chunk_size`.
pub(crate) const MAX_CHUNK_SIZE: usize = 512;

/// A JSON value with ordered object keys.
#[derive(Debug, Clone, PartialEq)]
pub(crate) enum Json {
    Null,
    Bool(bool),
    Number(serde_json::Number),
    String(String),
    Array(Vec<Json>),
    Object(Vec<(String, Json)>),
}

impl<'de> Deserialize<'de> for Json {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        struct V;
        impl<'de> Visitor<'de> for V {
            type Value = Json;
            fn expecting(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                f.write_str("any JSON value")
            }
            fn visit_bool<E>(self, v: bool) -> Result<Json, E> {
                Ok(Json::Bool(v))
            }
            fn visit_i64<E>(self, v: i64) -> Result<Json, E> {
                Ok(Json::Number(v.into()))
            }
            fn visit_u64<E>(self, v: u64) -> Result<Json, E> {
                Ok(Json::Number(v.into()))
            }
            fn visit_f64<E>(self, v: f64) -> Result<Json, E> {
                Ok(serde_json::Number::from_f64(v).map_or(Json::Null, Json::Number))
            }
            fn visit_str<E>(self, v: &str) -> Result<Json, E> {
                Ok(Json::String(v.to_owned()))
            }
            fn visit_string<E>(self, v: String) -> Result<Json, E> {
                Ok(Json::String(v))
            }
            fn visit_unit<E>(self) -> Result<Json, E> {
                Ok(Json::Null)
            }
            fn visit_none<E>(self) -> Result<Json, E> {
                Ok(Json::Null)
            }
            fn visit_seq<A: SeqAccess<'de>>(self, mut seq: A) -> Result<Json, A::Error> {
                let mut items = Vec::new();
                while let Some(item) = seq.next_element()? {
                    items.push(item);
                }
                Ok(Json::Array(items))
            }
            fn visit_map<A: MapAccess<'de>>(self, mut map: A) -> Result<Json, A::Error> {
                let mut pairs: Vec<(String, Json)> = Vec::new();
                while let Some((key, value)) = map.next_entry::<String, Json>()? {
                    // Python's dict: a repeated key keeps its first
                    // position and takes the last value.
                    match pairs.iter_mut().find(|(k, _)| *k == key) {
                        Some((_, slot)) => *slot = value,
                        None => pairs.push((key, value)),
                    }
                }
                Ok(Json::Object(pairs))
            }
        }
        deserializer.deserialize_any(V)
    }
}

/// `json.dumps(value)`.
fn dumps(value: &Json, out: &mut String) {
    match value {
        Json::Null => out.push_str("null"),
        Json::Bool(true) => out.push_str("true"),
        Json::Bool(false) => out.push_str("false"),
        Json::Number(n) => {
            let _ = write!(out, "{n}");
        }
        Json::String(s) => dump_str(s, out),
        Json::Array(items) => {
            out.push('[');
            for (i, item) in items.iter().enumerate() {
                if i > 0 {
                    out.push_str(", ");
                }
                dumps(item, out);
            }
            out.push(']');
        }
        Json::Object(pairs) => {
            out.push('{');
            for (i, (key, item)) in pairs.iter().enumerate() {
                if i > 0 {
                    out.push_str(", ");
                }
                dump_str(key, out);
                out.push_str(": ");
                dumps(item, out);
            }
            out.push('}');
        }
    }
}

/// A string as `ensure_ascii` writes it: everything outside space..`~` is
/// escaped (astral characters as surrogate pairs).
fn dump_str(s: &str, out: &mut String) {
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{8}' => out.push_str("\\b"),
            '\u{c}' => out.push_str("\\f"),
            ' '..='~' => out.push(c),
            _ => {
                let mut units = [0u16; 2];
                for unit in c.encode_utf16(&mut units) {
                    let _ = write!(out, "\\u{unit:04x}");
                }
            }
        }
    }
    out.push('"');
}

/// `len(json.dumps(value))`.
fn size(value: &Json) -> usize {
    let mut out = String::new();
    dumps(value, &mut out);
    out.len()
}

/// Python text of one chunk.
fn text(value: &Json) -> String {
    let mut out = String::new();
    dumps(value, &mut out);
    out
}

/// `_list_to_dict_preprocessing`: a list becomes `{"0": …, "1": …}`.
fn lists_to_objects(value: Json) -> Json {
    match value {
        Json::Object(pairs) => Json::Object(
            pairs
                .into_iter()
                .map(|(k, v)| (k, lists_to_objects(v)))
                .collect(),
        ),
        Json::Array(items) => Json::Object(
            items
                .into_iter()
                .enumerate()
                .map(|(i, v)| (i.to_string(), lists_to_objects(v)))
                .collect(),
        ),
        other => other,
    }
}

/// `_set_nested_dict`.
fn set_nested(target: &mut Json, path: &[String], value: Json) {
    let Some((last, parents)) = path.split_last() else {
        return;
    };
    let mut cursor = target;
    for key in parents {
        if !matches!(cursor, Json::Object(_)) {
            *cursor = Json::Object(Vec::new());
        }
        let Json::Object(pairs) = cursor else {
            return;
        };
        let at = if let Some(at) = pairs.iter().position(|(k, _)| k == key) {
            at
        } else {
            pairs.push((key.clone(), Json::Object(Vec::new())));
            pairs.len() - 1
        };
        cursor = &mut pairs[at].1;
    }
    if !matches!(cursor, Json::Object(_)) {
        *cursor = Json::Object(Vec::new());
    }
    if let Json::Object(pairs) = cursor {
        match pairs.iter_mut().find(|(k, _)| k == last) {
            Some((_, slot)) => *slot = value,
            None => pairs.push((last.clone(), value)),
        }
    }
}

/// `RecursiveJsonSplitter._json_split`.
fn split_into(
    data: &Json,
    path: &[String],
    chunks: &mut Vec<Json>,
    max_size: usize,
    min_size: usize,
) {
    if let Json::Object(pairs) = data {
        for (key, value) in pairs {
            let mut new_path = path.to_vec();
            new_path.push(key.clone());
            let current = chunks.last().map_or(2, size);
            let wanted = size(&Json::Object(vec![(key.clone(), value.clone())]));
            let room = max_size.saturating_sub(current);
            if wanted < room {
                if let Some(last) = chunks.last_mut() {
                    set_nested(last, &new_path, value.clone());
                }
            } else {
                if current >= min_size {
                    chunks.push(Json::Object(Vec::new()));
                }
                split_into(value, &new_path, chunks, max_size, min_size);
            }
        }
    } else if let Some(last) = chunks.last_mut() {
        set_nested(last, path, data.clone());
    }
}

/// What the chunker made of a document.
pub(crate) enum Split {
    /// The whole document fits one chunk: the SDK yields it as it came.
    Whole,
    /// The chunks' Python-`json.dumps` texts (none for `{}`).
    Chunks(Vec<String>),
    /// Not a JSON object (invalid, a scalar, a list of lines): the caller
    /// chunks it as text.
    NotAnObject,
}

/// Split a JSON document at `max_chunk_size` characters per chunk.
pub(crate) fn split(document: &str, max_chunk_size: usize) -> Split {
    let Ok(parsed) = serde_json::from_str::<Json>(document) else {
        return Split::NotAnObject;
    };
    if !matches!(parsed, Json::Object(_) | Json::Array(_)) {
        return Split::NotAnObject;
    }
    let data = lists_to_objects(parsed);
    let min_size = max_chunk_size.saturating_sub(200).max(50);
    let mut chunks = vec![Json::Object(Vec::new())];
    split_into(&data, &[], &mut chunks, max_chunk_size, min_size);
    if chunks.last() == Some(&Json::Object(Vec::new())) {
        chunks.pop();
    }
    if chunks.len() == 1 {
        return Split::Whole;
    }
    Split::Chunks(chunks.iter().map(text).collect())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn pieces(document: &str, max: usize) -> Vec<String> {
        match split(document, max) {
            Split::Chunks(chunks) => chunks,
            Split::Whole => vec![document.to_owned()],
            Split::NotAnObject => panic!("not an object"),
        }
    }

    #[test]
    fn python_dumps_spacing_and_ascii_escapes() {
        let parsed: Json = serde_json::from_str(r#"{"b": [1, 2.5, null], "a": "é😀\n"}"#)
            .unwrap_or_else(|e| panic!("{e}"));
        assert_eq!(
            text(&parsed),
            "{\"b\": [1, 2.5, null], \"a\": \"\\u00e9\\ud83d\\ude00\\n\"}"
        );
    }

    #[test]
    fn a_small_document_is_whole_and_key_order_is_kept() {
        assert!(matches!(split(r#"{"z": 1, "a": 2}"#, 512), Split::Whole));
        let parsed: Json =
            serde_json::from_str(r#"{"z": 1, "a": 2, "z": 3}"#).unwrap_or_else(|e| panic!("{e}"));
        assert_eq!(text(&parsed), r#"{"z": 3, "a": 2}"#);
        assert!(matches!(split("not json", 512), Split::NotAnObject));
        assert!(matches!(split("42", 512), Split::NotAnObject));
        assert!(matches!(split("{}", 512), Split::Chunks(c) if c.is_empty()));
    }

    #[test]
    fn keys_fill_chunks_up_to_the_size_and_each_chunk_is_json() {
        let entries: Vec<String> = (0..30)
            .map(|n| format!(r#""key{n:02}": "{}""#, "v".repeat(20)))
            .collect();
        let document = format!("{{{}}}", entries.join(", "));
        let chunks = pieces(&document, 200);
        assert!(chunks.len() > 3, "{}", chunks.len());
        let mut keys = Vec::new();
        for chunk in &chunks {
            assert!(chunk.len() <= 200, "{} > 200", chunk.len());
            let parsed: serde_json::Value =
                serde_json::from_str(chunk).unwrap_or_else(|e| panic!("{e}: {chunk}"));
            keys.extend(
                parsed
                    .as_object()
                    .into_iter()
                    .flat_map(|o| o.keys().cloned()),
            );
        }
        // Every key lands in exactly one chunk, in order.
        let expected: Vec<String> = (0..30).map(|n| format!("key{n:02}")).collect();
        keys.sort();
        assert_eq!(keys, expected);
    }

    #[test]
    fn a_nested_value_too_big_for_the_room_is_split_below_its_path() {
        let items: Vec<String> = (0..40).map(|n| format!(r#""item number {n}""#)).collect();
        let document = format!(r#"{{"meta": {{"id": 1}}, "list": [{}]}}"#, items.join(", "));
        let chunks = pieces(&document, 150);
        assert!(chunks.len() > 2);
        // The list became an object keyed by index, under its own path.
        assert!(
            chunks.iter().any(|c| c.starts_with(r#"{"list": {"#)),
            "{chunks:?}"
        );
        assert!(chunks.iter().all(|c| c.len() <= 150), "{chunks:?}");
    }
}
