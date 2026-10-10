//! The JSON chunker: `LangChain`'s `RecursiveJsonSplitter` (`convert_lists`
//! on), with Python's `json.dumps` for sizes and output.
//!
//! The splitter fills a chunk key by key while `len(json.dumps(chunk))`
//! stays under `max_chunk_size`; a value too big for the chunk's room starts
//! a new chunk (once the current one is at least `max - 200`, floor 50
//! characters) and is split below its own path. Sizes are in characters of
//! Python's default dump (`", "` and `": "` separators, `ensure_ascii`).
//!
//! Every chunk stays within `max_chunk_size`: a key is added only when the
//! chunk, wrapper braces and parent keys included, still fits; a scalar
//! (in practice a long string) too big for a chunk even on its own is cut
//! with the text splitter into pieces within the budget, each carrying the
//! JSON path it came from. The split is linear in the document: the running
//! chunk size is tracked as keys are added, and a subtree's size is counted
//! by reference.
//!
//! Key order is the document's: this crate has its own ordered value type so
//! it does not need `serde_json`'s `preserve_order` feature.

use serde::de::{Deserialize, Deserializer, MapAccess, SeqAccess, Visitor};
use std::collections::HashMap;
use std::fmt::{self, Write as _};
use text_splitter::{ChunkConfig, TextSplitter};

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
                let mut index: HashMap<String, usize> = HashMap::new();
                while let Some((key, value)) = map.next_entry::<String, Json>()? {
                    // Python's dict: a repeated key keeps its first
                    // position and takes the last value.
                    match index.get(&key) {
                        Some(at) => pairs[*at].1 = value,
                        None => {
                            index.insert(key.clone(), pairs.len());
                            pairs.push((key, value));
                        }
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

/// `len(json.dumps(value))`, counted without building the text.
fn size(value: &Json) -> usize {
    match value {
        Json::Null => 4,
        Json::Bool(true) => 4,
        Json::Bool(false) => 5,
        Json::Number(n) => {
            let mut counter = Counter(0);
            let _ = write!(counter, "{n}");
            counter.0
        }
        Json::String(s) => str_size(s),
        Json::Array(items) => {
            2 + items.iter().map(size).sum::<usize>() + 2 * items.len().saturating_sub(1)
        }
        Json::Object(pairs) => {
            2 + pairs
                .iter()
                .map(|(k, v)| str_size(k) + 2 + size(v))
                .sum::<usize>()
                + 2 * pairs.len().saturating_sub(1)
        }
    }
}

/// A `fmt::Write` that only counts.
struct Counter(usize);

impl fmt::Write for Counter {
    fn write_str(&mut self, s: &str) -> fmt::Result {
        self.0 += s.len();
        Ok(())
    }
}

/// The length of a string as `dump_str` writes it.
fn str_size(s: &str) -> usize {
    2 + s
        .chars()
        .map(|c| match c {
            '"' | '\\' | '\n' | '\r' | '\t' | '\u{8}' | '\u{c}' => 2,
            ' '..='~' => 1,
            _ => 6 * c.len_utf16(),
        })
        .sum::<usize>()
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

/// A chunk under construction and its `json.dumps` length, kept up to date
/// as keys are added.
struct Open {
    value: Json,
    size: usize,
}

impl Open {
    fn new() -> Self {
        Self {
            value: Json::Object(Vec::new()),
            size: 2,
        }
    }

    fn is_empty(&self) -> bool {
        matches!(&self.value, Json::Object(pairs) if pairs.is_empty())
    }
}

/// What the split has so far, in document order.
enum Part {
    Open(Open),
    /// A piece of a scalar too big for any chunk, with its path.
    Piece {
        text: String,
        path: Vec<String>,
    },
}

/// How much longer a chunk's text gets when `value` (of `value_size`) is
/// set at `path` in it: the missing parents' braces and keys, the value, a
/// separator where the parent already holds keys. A key already there is
/// replaced, so its old value's size comes off.
fn added_size(target: &Json, path: &[String], value_size: usize) -> isize {
    let mut cursor = target;
    for (depth, key) in path.iter().enumerate() {
        let Json::Object(pairs) = cursor else {
            // A non-object parent is replaced by an object holding the rest.
            return nested_size(&path[depth..], value_size) as isize - size(cursor) as isize;
        };
        match pairs.iter().find(|(k, _)| k == key) {
            Some((_, next)) if depth + 1 == path.len() => {
                return value_size as isize - size(next) as isize;
            }
            Some((_, next)) => cursor = next,
            None => {
                let separator = if pairs.is_empty() { 0 } else { 2 };
                return (separator + str_size(key) + 2 + nested_size(&path[depth + 1..], value_size))
                    as isize;
            }
        }
    }
    0
}

/// `{"k1": {"k2": … value}}` for `path`.
fn nested_size(path: &[String], value_size: usize) -> usize {
    match path.split_first() {
        None => value_size,
        Some((key, rest)) => 2 + str_size(key) + 2 + nested_size(rest, value_size),
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

/// The splitter's state: the parts so far and the budget.
struct Splitter {
    parts: Vec<Part>,
    max_size: usize,
    min_size: usize,
}

impl Splitter {
    fn open(&mut self) -> &mut Open {
        if !matches!(self.parts.last(), Some(Part::Open(_))) {
            self.parts.push(Part::Open(Open::new()));
        }
        match self.parts.last_mut() {
            Some(Part::Open(open)) => open,
            _ => unreachable!("an open chunk was just pushed"),
        }
    }

    fn current_size(&self) -> usize {
        match self.parts.last() {
            Some(Part::Open(open)) => open.size,
            _ => 2,
        }
    }

    /// Set `value` at `path` in the open chunk and track the new size.
    fn insert(&mut self, path: &[String], value: &Json, value_size: usize) {
        let open = self.open();
        let delta = added_size(&open.value, path, value_size);
        set_nested(&mut open.value, path, value.clone());
        open.size = open.size.saturating_add_signed(delta);
    }

    /// `RecursiveJsonSplitter._json_split`.
    fn walk(&mut self, data: &Json, path: &[String]) {
        if let Json::Object(pairs) = data {
            for (key, value) in pairs {
                let mut new_path = path.to_vec();
                new_path.push(key.clone());
                let value_size = size(value);
                let current = self.current_size();
                // The key alone, as the SDK weighs it (`{key: value}`).
                let wanted = 2 + str_size(key) + 2 + value_size;
                let room = self.max_size.saturating_sub(current);
                let fits = match self.parts.last() {
                    Some(Part::Open(open)) => {
                        added_size(&open.value, &new_path, value_size) + current as isize
                            <= self.max_size as isize
                    }
                    _ => nested_size(&new_path, value_size) <= self.max_size,
                };
                if wanted < room && fits {
                    self.insert(&new_path, value, value_size);
                } else {
                    if current >= self.min_size {
                        self.parts.push(Part::Open(Open::new()));
                    }
                    self.walk(value, &new_path);
                }
            }
        } else {
            self.leaf(data, path);
        }
    }

    /// A scalar: into the open chunk if it fits there, else into a fresh
    /// one, else (too big for any chunk) cut into pieces.
    fn leaf(&mut self, data: &Json, path: &[String]) {
        let leaf_size = size(data);
        if nested_size(path, leaf_size) > self.max_size {
            self.pieces(data, path);
            return;
        }
        let fits_here = match self.parts.last() {
            Some(Part::Open(open)) => {
                added_size(&open.value, path, leaf_size) + open.size as isize
                    <= self.max_size as isize
            }
            _ => true,
        };
        if !fits_here {
            self.parts.push(Part::Open(Open::new()));
        }
        self.insert(path, data, leaf_size);
    }

    /// A scalar cut by the text splitter into pieces of at most `max_size`
    /// characters.
    fn pieces(&mut self, data: &Json, path: &[String]) {
        let raw = match data {
            Json::String(s) => s.clone(),
            other => text(other),
        };
        let splitter = TextSplitter::new(ChunkConfig::new(self.max_size.max(1)));
        for piece in splitter.chunks(&raw) {
            self.parts.push(Part::Piece {
                text: piece.to_owned(),
                path: path.to_vec(),
            });
        }
    }
}

/// A JSON Pointer (`/list/3/name`) for a path.
fn pointer(path: &[String]) -> String {
    path.iter().fold(String::new(), |mut out, key| {
        out.push('/');
        out.push_str(&key.replace('~', "~0").replace('/', "~1"));
        out
    })
}

/// One chunk of a split document.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct JsonChunk {
    pub text: String,
    /// The JSON Pointer of the scalar a piece was cut from; `None` for a
    /// chunk of keys (its path is in its text).
    pub path: Option<String>,
}

/// What the chunker made of a document.
pub(crate) enum Split {
    /// The whole document fits one chunk: the SDK yields it as it came.
    Whole,
    /// The chunks (none for `{}`): the Python-`json.dumps` texts of the keys
    /// that fit together, and the pieces of a scalar too big for a chunk.
    Chunks(Vec<JsonChunk>),
    /// Not a JSON object (invalid, a scalar, a list of lines): the caller
    /// chunks it as text.
    NotAnObject,
}

/// Split a JSON document at `max_chunk_size` characters per chunk. No chunk
/// is longer, and `Whole` is only the document itself when it is within the
/// size.
pub(crate) fn split(document: &str, max_chunk_size: usize) -> Split {
    let Ok(parsed) = serde_json::from_str::<Json>(document) else {
        return Split::NotAnObject;
    };
    if !matches!(parsed, Json::Object(_) | Json::Array(_)) {
        return Split::NotAnObject;
    }
    let data = lists_to_objects(parsed);
    let mut splitter = Splitter {
        parts: vec![Part::Open(Open::new())],
        max_size: max_chunk_size,
        min_size: max_chunk_size.saturating_sub(200).max(50),
    };
    splitter.walk(&data, &[]);
    let parts: Vec<Part> = splitter
        .parts
        .into_iter()
        .filter(|part| !matches!(part, Part::Open(open) if open.is_empty()))
        .collect();
    if let [Part::Open(only)] = parts.as_slice()
        && document.chars().count() <= max_chunk_size
    {
        debug_assert_eq!(only.size, size(&only.value));
        return Split::Whole;
    }
    Split::Chunks(
        parts
            .iter()
            .map(|part| match part {
                Part::Open(open) => {
                    debug_assert_eq!(open.size, size(&open.value));
                    JsonChunk {
                        text: text(&open.value),
                        path: None,
                    }
                }
                Part::Piece { text, path } => JsonChunk {
                    text: text.clone(),
                    path: Some(pointer(path)),
                },
            })
            .collect(),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn pieces(document: &str, max: usize) -> Vec<String> {
        match split(document, max) {
            Split::Chunks(chunks) => chunks.into_iter().map(|c| c.text).collect(),
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

    #[test]
    fn a_long_string_leaf_is_cut_into_pieces_within_the_budget_with_its_path() {
        let words: String = (0..40_000).map(|n| format!("w{n} ")).collect();
        assert!(words.len() > 200_000);
        let document = format!(r#"{{"id": 1, "body": {{"text": "{words}"}}, "tail": 2}}"#);
        let Split::Chunks(chunks) = split(&document, 512) else {
            panic!("expected chunks");
        };
        assert!(chunks.iter().all(|c| c.text.chars().count() <= 512));
        let pieces: Vec<&JsonChunk> = chunks.iter().filter(|c| c.path.is_some()).collect();
        assert!(pieces.len() > 300, "{}", pieces.len());
        assert!(
            pieces
                .iter()
                .all(|c| c.path.as_deref() == Some("/body/text"))
        );
        // The text survives: the pieces, rejoined, hold every word.
        let joined: String = pieces
            .iter()
            .map(|c| c.text.as_str())
            .collect::<Vec<_>>()
            .join(" ");
        assert!(joined.contains("w39999") && joined.contains("w20000"));
        // Keys before and after the leaf are still chunks, in order.
        assert!(
            chunks
                .first()
                .is_some_and(|c| c.text.contains(r#""id": 1"#))
        );
        assert!(
            chunks
                .last()
                .is_some_and(|c| c.text.contains(r#""tail": 2"#))
        );
    }

    #[test]
    fn a_whole_document_is_never_over_the_budget() {
        // Few keys, but pretty-printed past the budget: it is a chunk of its
        // own dump, not the document as written.
        let padded = format!("{{\n{}\"a\": 1\n}}", " ".repeat(600));
        let Split::Chunks(chunks) = split(&padded, 512) else {
            panic!("expected one chunk");
        };
        assert_eq!(chunks.len(), 1);
        assert_eq!(chunks[0].text, r#"{"a": 1}"#);
        assert!(matches!(split(r#"{"a": 1}"#, 512), Split::Whole));
    }

    #[test]
    fn a_20_mb_nested_document_splits_in_bounded_time_within_the_budget() {
        let mut document = String::from(r#"{"records": ["#);
        let mut n = 0;
        while document.len() < 20 * 1024 * 1024 {
            if n > 0 {
                document.push_str(", ");
            }
            document.push_str(&format!(
                r#"{{"id": {n}, "name": "record number {n}", "tags": ["a", "b", "c"], "meta": {{"owner": "someone", "note": "{}"}}}}"#,
                "x".repeat(40)
            ));
            n += 1;
        }
        document.push_str("]}");
        let started = std::time::Instant::now();
        let Split::Chunks(chunks) = split(&document, 512) else {
            panic!("expected chunks");
        };
        let elapsed = started.elapsed();
        assert!(chunks.len() > 50_000, "{}", chunks.len());
        assert!(
            chunks.iter().all(|c| c.text.chars().count() <= 512),
            "a chunk is over the budget"
        );
        assert!(elapsed.as_secs() < 60, "took {elapsed:?}");
    }

    #[test]
    fn a_wide_object_does_not_make_parsing_quadratic() {
        let entries: Vec<String> = (0..200_000).map(|n| format!(r#""k{n}": {n}"#)).collect();
        let document = format!("{{{}}}", entries.join(", "));
        let started = std::time::Instant::now();
        let Split::Chunks(chunks) = split(&document, 512) else {
            panic!("expected chunks");
        };
        assert!(chunks.iter().all(|c| c.text.len() <= 512));
        assert!(started.elapsed().as_secs() < 60, "{:?}", started.elapsed());
    }
}
