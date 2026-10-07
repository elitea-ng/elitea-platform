//! The RECORDED store: Phase 2 answered from a recording of the Python
//! run, for the parity gate; and the stand-in embedding both sides use.
//!
//! `parity/python_reference.py --through phase2` runs Python's `run_phase2`
//! over its own `.wiki.db` through a recording wrapper and writes
//! `recording.jsonl`: one line per DISTINCT read, `{"call", "args",
//! "result"}`, with the result projected to the fields Phase 2 reads. This
//! store answers a call by its identity — the call name and its arguments,
//! serialised the same way on both sides — so the Rust phase sees exactly
//! the index Python saw. A call the recording does not hold is an error
//! (the port asked something Python did not); a recorded call Rust never
//! makes is allowed (Rust batches and caches rows Python re-read).
//!
//! Writes are kept for the dump: the hub list, the edge rows, the meta.

use super::digest::sha256;
use super::store::{SearchHit, StoreError, StoredNode, TextEmbedder, TopologyStore};
use crate::graph::EdgeRow;
use crate::pyjson;
use serde_json::{Value, json};
use std::collections::{HashMap, HashSet};

/// Dimension of the stand-in embedding.
pub const STANDIN_DIM: usize = 16;

/// The deterministic stand-in for the embedding model
/// (`python_reference.standin_embedding`): SHA-256 of the UTF-8 text;
/// component `i` is the big-endian 16-bit word `i` of the digest scaled to
/// `[-0.5, 0.5]`; L2-normalised with a left-to-right sum of squares.
#[must_use]
pub fn standin_embedding(text: &str) -> Vec<f64> {
    let digest = sha256(text.as_bytes());
    let vector: Vec<f64> = digest
        .chunks_exact(2)
        .take(STANDIN_DIM)
        .map(|word| f64::from(u16::from_be_bytes([word[0], word[1]])) / 65535.0 - 0.5)
        .collect();
    let mut squares = 0.0;
    for x in &vector {
        squares += x * x;
    }
    let norm = squares.sqrt();
    if norm == 0.0 {
        return vector;
    }
    vector.into_iter().map(|x| x / norm).collect()
}

/// [`standin_embedding`] as a [`TextEmbedder`].
#[derive(Debug, Clone, Copy, Default)]
pub struct StandinEmbedder;

impl TextEmbedder for StandinEmbedder {
    fn embed(&mut self, text: &str) -> Result<Vec<f64>, StoreError> {
        Ok(standin_embedding(text))
    }
}

/// The identity of a call: its name and its JSON arguments.
fn call_key(call: &str, args: &Value) -> String {
    format!("{call}\u{0}{}", pyjson::dumps(args))
}

/// A [`TopologyStore`] that answers from a recording.
#[derive(Debug, Default)]
pub struct ReplayStore {
    calls: HashMap<String, Value>,
    used: HashSet<String>,
    /// The hubs Phase 2 flagged.
    pub hubs: Vec<String>,
    /// The edge rows Phase 2 persisted, in graph order.
    pub edges: Vec<EdgeRow>,
    /// The meta entries Phase 2 wrote.
    pub meta: Vec<(String, Value)>,
}

impl ReplayStore {
    /// Load `recording.jsonl`.
    ///
    /// # Errors
    ///
    /// A line that is not a call record, a recorded failure, or the same
    /// call with two results.
    pub fn from_jsonl(text: &str) -> Result<Self, StoreError> {
        let mut store = Self::default();
        for (number, line) in text.lines().enumerate() {
            if line.trim().is_empty() {
                continue;
            }
            let entry: Value = serde_json::from_str(line)
                .map_err(|e| StoreError::new(format!("recording line {}: {e}", number + 1)))?;
            let call = entry["call"].as_str().unwrap_or_default();
            if let Some(error) = entry.get("error") {
                return Err(StoreError::new(format!(
                    "recording line {}: {call} raised in Python: {error}",
                    number + 1
                )));
            }
            let key = call_key(call, &entry["args"]);
            let result = entry.get("result").cloned().unwrap_or(Value::Null);
            if let Some(known) = store.calls.insert(key, result.clone())
                && known != result
            {
                return Err(StoreError::new(format!(
                    "recording line {}: {call} recorded twice with different results",
                    number + 1
                )));
            }
        }
        Ok(store)
    }

    /// How many recorded calls Rust never made.
    #[must_use]
    pub fn unused_calls(&self) -> usize {
        self.calls.len().saturating_sub(self.used.len())
    }

    fn answer(&mut self, call: &str, args: &Value) -> Result<&Value, StoreError> {
        let key = call_key(call, args);
        if !self.calls.contains_key(&key) {
            return Err(StoreError::new(format!(
                "not in the recording: {call}{}",
                pyjson::dumps(args)
            )));
        }
        self.used.insert(key.clone());
        self.calls
            .get(&key)
            .ok_or_else(|| StoreError::new(format!("not in the recording: {call}")))
    }
}

fn text(value: &Value, key: &str) -> String {
    value
        .get(key)
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_owned()
}

fn optional_text(value: &Value, key: &str) -> Option<String> {
    value.get(key).and_then(Value::as_str).map(str::to_owned)
}

fn stored_node(value: &Value) -> Option<StoredNode> {
    if value.is_null() {
        return None;
    }
    Some(StoredNode {
        symbol_name: text(value, "symbol_name"),
        rel_path: text(value, "rel_path"),
        symbol_type: text(value, "symbol_type"),
        source_text: optional_text(value, "source_text"),
        docstring: optional_text(value, "docstring"),
        is_doc: value.get("is_doc").and_then(Value::as_i64).unwrap_or(0),
        language: text(value, "language"),
    })
}

fn hits(value: &Value) -> Vec<SearchHit> {
    value
        .as_array()
        .into_iter()
        .flatten()
        .map(|hit| SearchHit {
            node_id: text(hit, "node_id"),
            rel_path: text(hit, "rel_path"),
            symbol_type: text(hit, "symbol_type"),
            fts_rank: hit.get("fts_rank").and_then(Value::as_f64),
            score_norm: hit.get("score_norm").and_then(Value::as_f64),
            vec_distance: hit.get("vec_distance").and_then(Value::as_f64),
        })
        .collect()
}

fn count(value: &Value) -> u64 {
    value.as_u64().unwrap_or(0)
}

impl TopologyStore for ReplayStore {
    fn get_nodes(&mut self, ids: &[&str]) -> Result<Vec<Option<StoredNode>>, StoreError> {
        ids.iter()
            .map(|id| self.answer("get_node", &json!([id])).map(stored_node))
            .collect()
    }

    fn node_count(&mut self) -> Result<u64, StoreError> {
        self.answer("node_count", &json!([])).map(count)
    }

    fn count_phrase_matches(&mut self, query: &str) -> Result<u64, StoreError> {
        self.answer("count_fts_matches", &json!([query, true]))
            .map(count)
    }

    fn search_lexical(
        &mut self,
        query: &str,
        path_prefix: Option<&str>,
        limit: usize,
    ) -> Result<Vec<SearchHit>, StoreError> {
        self.answer("search_fts", &json!([query, path_prefix, limit]))
            .map(hits)
    }

    fn get_embeddings(&mut self, ids: &[&str]) -> Result<Vec<Option<Vec<f64>>>, StoreError> {
        ids.iter()
            .map(|id| {
                self.answer("get_embedding", &json!([id])).map(|v| {
                    v.as_array()
                        .map(|xs| xs.iter().filter_map(Value::as_f64).collect())
                })
            })
            .collect()
    }

    fn search_dense(
        &mut self,
        embedding: &[f64],
        k: usize,
        path_prefix: Option<&str>,
    ) -> Result<Vec<SearchHit>, StoreError> {
        self.answer("search_dense", &json!([embedding, k, path_prefix]))
            .map(hits)
    }

    fn set_hubs(&mut self, hubs: &[&str]) -> Result<(), StoreError> {
        self.hubs.extend(hubs.iter().map(|h| (*h).to_owned()));
        Ok(())
    }

    fn replace_edges(
        &mut self,
        rows: &mut dyn Iterator<Item = EdgeRow>,
    ) -> Result<u64, StoreError> {
        self.edges = rows.collect();
        Ok(self.edges.len() as u64)
    }

    fn set_meta(&mut self, key: &str, value: &Value) -> Result<(), StoreError> {
        self.meta.push((key.to_owned(), value.clone()));
        Ok(())
    }
}

#[cfg(test)]
#[allow(clippy::float_cmp)] // bit-exact parity is the point
mod tests {
    use super::*;

    #[test]
    fn the_standin_embedding_is_unit_length_and_python_equal() {
        let v = standin_embedding("def f(): pass");
        assert_eq!(v.len(), STANDIN_DIM);
        let norm: f64 = v.iter().map(|x| x * x).sum::<f64>().sqrt();
        assert!((norm - 1.0).abs() < 1e-12);
        // python_reference.standin_embedding("abc")[:2]
        let abc = standin_embedding("abc");
        assert_eq!(abc[0], 0.199_383_162_566_557_41);
        assert_eq!(abc[1], -0.358_905_676_839_670_45);
    }

    #[test]
    fn calls_are_answered_by_identity() {
        let recording = concat!(
            r#"{"call": "node_count", "args": [], "result": 7}"#,
            "\n",
            r#"{"call": "search_fts", "args": ["Foo", null, 16], "result": [{"node_id": "a", "rel_path": "x/a.py", "symbol_type": "class", "fts_rank": -1.5, "score_norm": 0.8}]}"#,
            "\n",
            r#"{"call": "get_node", "args": ["a"], "result": null}"#,
        );
        let mut store = ReplayStore::from_jsonl(recording).unwrap();
        assert_eq!(store.node_count().unwrap(), 7);
        let found = store.search_lexical("Foo", None, 16).unwrap();
        assert_eq!(found[0].fts_rank, Some(-1.5));
        assert_eq!(store.get_nodes(&["a"]).unwrap(), [None]);
        assert!(store.search_lexical("Foo", Some("x"), 16).is_err());
        assert_eq!(store.unused_calls(), 0);
    }

    #[test]
    fn a_recorded_failure_or_conflict_is_refused() {
        assert!(
            ReplayStore::from_jsonl(r#"{"call": "node_count", "args": [], "error": "boom"}"#)
                .is_err()
        );
        let twice = "{\"call\": \"node_count\", \"args\": [], \"result\": 1}\n\
                     {\"call\": \"node_count\", \"args\": [], \"result\": 2}";
        assert!(ReplayStore::from_jsonl(twice).is_err());
    }
}
