//! The two full-text lookups of cluster expansion.
//!
//! Python ran both against SQLite FTS5 (`porter unicode61`, BM25 rank):
//!
//! * `search_fts5(term, cluster_id, limit)` — framework-reference
//!   discovery for an "orphan" seed: code that names the orphan in a
//!   string (`_search_framework_fts`);
//! * `_fts_fallback(name, macro_id)` — a seed name with no exact
//!   `symbol_name` match (`symbol_name:"name"`, three rows).
//!
//! There is no FTS5 here (no SQLite, ADR-0026). [`PageSearch`] is the seam:
//!
//! * [`ReplaySearch`] answers from the recording
//!   `parity/python_pages_dump.py` writes — the parity gate, as Phase 2's
//!   `ReplayStore`;
//! * [`SubstringSearch`] is the production lookup. DELIBERATE DIFFERENCE:
//!   the framework search is Python's own non-FTS fallback in
//!   `_search_framework_fts` (`LOWER(source_text) LIKE '%term%' [AND
//!   macro_cluster = ?] ORDER BY rel_path, start_line LIMIT ?`), not BM25;
//!   the symbol fallback matches the name as a token phrase of
//!   `symbol_name`, in row order. Both are deterministic and need no
//!   index; the callers filter the hits exactly as Python did.

use super::index::{IndexNode, PageIndex};
use crate::errors::{EngineError, ErrorType};
use serde::Deserialize;
use std::collections::HashMap;
use std::sync::Arc;

/// The lookups cluster expansion needs. Results are node ids, in rank
/// order.
pub trait PageSearch: Send + Sync {
    /// `search_fts5(query=term, cluster_id=…, limit=…)`.
    ///
    /// # Errors
    ///
    /// A replay without a recording for the call.
    fn search(
        &self,
        term: &str,
        cluster_id: Option<i64>,
        limit: usize,
    ) -> Result<Vec<String>, EngineError>;

    /// `_fts_fallback(name, macro_id)`: up to three architectural nodes.
    ///
    /// # Errors
    ///
    /// A replay without a recording for the call.
    fn symbol(&self, name: &str, macro_id: Option<i64>) -> Result<Vec<String>, EngineError>;
}

/// One recorded call (`fts.jsonl`).
#[derive(Debug, Clone, Deserialize)]
struct Recorded {
    kind: String,
    query: String,
    cluster_id: Option<i64>,
    limit: usize,
    node_ids: Vec<Option<String>>,
}

type Key = (String, String, Option<i64>, usize);

/// Answers from Python's recording; a call it has no answer for is an
/// error (the parity run then fails loudly).
#[derive(Debug, Clone, Default)]
pub struct ReplaySearch {
    answers: HashMap<Key, Vec<String>>,
}

impl ReplaySearch {
    /// Parse `fts.jsonl`.
    ///
    /// # Errors
    ///
    /// A line that is not a recorded call.
    pub fn from_jsonl(text: &str) -> Result<Self, EngineError> {
        let mut answers = HashMap::new();
        for line in text.lines().filter(|l| !l.trim().is_empty()) {
            let call: Recorded = serde_json::from_str(line)
                .map_err(|e| EngineError::new(ErrorType::Value, format!("fts recording: {e}")))?;
            answers.insert(
                (call.kind, call.query, call.cluster_id, call.limit),
                call.node_ids.into_iter().flatten().collect(),
            );
        }
        Ok(Self { answers })
    }

    fn answer(
        &self,
        kind: &str,
        query: &str,
        cluster_id: Option<i64>,
        limit: usize,
    ) -> Result<Vec<String>, EngineError> {
        self.answers
            .get(&(kind.to_owned(), query.to_owned(), cluster_id, limit))
            .cloned()
            .ok_or_else(|| {
                EngineError::new(
                    ErrorType::Runtime,
                    format!("no recorded {kind} search for {query:?} (cluster {cluster_id:?}, limit {limit})"),
                )
            })
    }
}

impl PageSearch for ReplaySearch {
    fn search(
        &self,
        term: &str,
        cluster_id: Option<i64>,
        limit: usize,
    ) -> Result<Vec<String>, EngineError> {
        self.answer("search", term, cluster_id, limit)
    }

    fn symbol(&self, name: &str, macro_id: Option<i64>) -> Result<Vec<String>, EngineError> {
        self.answer("symbol", name, macro_id, 3)
    }
}

/// The production lookups over the in-memory index (see the module
/// comment for how they differ from FTS5).
#[derive(Debug, Clone)]
pub struct SubstringSearch {
    index: Arc<PageIndex>,
}

impl SubstringSearch {
    #[must_use]
    pub fn new(index: Arc<PageIndex>) -> Self {
        Self { index }
    }
}

/// SQLite `LIKE '%pattern%'` on `LOWER(text)`: ASCII case-insensitive,
/// `_` one character, `%` any run (no escape character).
fn like_contains(text: &str, pattern: &str) -> bool {
    let text: Vec<char> = text.chars().map(|c| c.to_ascii_lowercase()).collect();
    let pattern: Vec<char> = pattern.chars().map(|c| c.to_ascii_lowercase()).collect();
    like_at(&text, &pattern)
}

/// `%pattern%`: try the pattern at every start.
fn like_at(text: &[char], pattern: &[char]) -> bool {
    (0..=text.len()).any(|start| like_here(&text[start..], pattern))
}

fn like_here(text: &[char], pattern: &[char]) -> bool {
    match pattern.first() {
        None => true,
        Some('%') => (0..=text.len()).any(|skip| like_here(&text[skip..], &pattern[1..])),
        Some('_') => !text.is_empty() && like_here(&text[1..], &pattern[1..]),
        Some(&c) => text.first() == Some(&c) && like_here(&text[1..], &pattern[1..]),
    }
}

/// The `unicode61` tokens of a text: runs of letters and digits, folded
/// to lower case.
fn tokens(text: &str) -> Vec<String> {
    text.split(|c: char| !c.is_alphanumeric())
        .filter(|t| !t.is_empty())
        .map(str::to_lowercase)
        .collect()
}

impl PageSearch for SubstringSearch {
    fn search(
        &self,
        term: &str,
        cluster_id: Option<i64>,
        limit: usize,
    ) -> Result<Vec<String>, EngineError> {
        let pattern = term.to_lowercase();
        let mut rows: Vec<&IndexNode> = self
            .index
            .nodes()
            .iter()
            .filter(|n| cluster_id.is_none() || n.macro_cluster == cluster_id)
            .filter(|n| like_contains(n.text(), &pattern))
            .collect();
        rows.sort_by(|a, b| {
            a.rel_path
                .cmp(&b.rel_path)
                .then(a.start_line.cmp(&b.start_line))
        });
        Ok(rows
            .into_iter()
            .take(limit)
            .map(|n| n.node_id.clone())
            .collect())
    }

    fn symbol(&self, name: &str, macro_id: Option<i64>) -> Result<Vec<String>, EngineError> {
        let phrase = tokens(name);
        if phrase.is_empty() {
            return Ok(Vec::new());
        }
        Ok(self
            .index
            .nodes()
            .iter()
            .filter(|n| n.is_architectural && (macro_id.is_none() || n.macro_cluster == macro_id))
            .filter(|n| {
                tokens(&n.symbol_name)
                    .windows(phrase.len())
                    .any(|w| w == phrase.as_slice())
            })
            .take(3)
            .map(|n| n.node_id.clone())
            .collect())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn like_is_ascii_case_insensitive_with_underscore_wildcard() {
        assert!(like_contains("Call get_Owner here", "get_owner"));
        assert!(like_contains("getXowner", "get_owner"));
        assert!(!like_contains("getowner", "get_owner"));
        assert!(!like_contains("ÉCOLE", "école"));
    }

    #[test]
    fn replay_answers_only_recorded_calls() {
        let replay = ReplaySearch::from_jsonl(
            r#"{"kind": "search", "query": "Foo", "cluster_id": 1, "limit": 9, "node_ids": ["a", "b"]}"#,
        )
        .unwrap();
        assert_eq!(replay.search("Foo", Some(1), 9).unwrap(), ["a", "b"]);
        assert!(replay.search("Foo", None, 9).is_err());
    }
}
