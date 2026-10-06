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
    /// `LOWER(source_text)` of every node, in node order, built once:
    /// SQLite's `LOWER` folds ASCII only.
    lowered: Vec<String>,
}

impl SubstringSearch {
    #[must_use]
    pub fn new(index: Arc<PageIndex>) -> Self {
        let lowered = index
            .nodes()
            .iter()
            .map(|n| n.text().to_ascii_lowercase())
            .collect();
        Self { index, lowered }
    }
}

/// A compiled SQLite `LIKE '%pattern%'` (ASCII case-insensitive, `_` one
/// character, `%` any run, no escape character) for texts that are
/// already ASCII-lowercased.
enum Like {
    /// No wildcard: a plain substring search.
    Literal(memchr::memmem::Finder<'static>),
    /// The pattern's characters, wrapped in `%…%`, and its longest
    /// literal run (it must occur in any matching text).
    Wildcard {
        pattern: Vec<char>,
        required: memchr::memmem::Finder<'static>,
    },
}

impl Like {
    fn new(pattern: &str) -> Self {
        let pattern: Vec<char> = pattern.chars().map(|c| c.to_ascii_lowercase()).collect();
        if !pattern.iter().any(|&c| c == '%' || c == '_') {
            let needle: String = pattern.into_iter().collect();
            return Self::Literal(memchr::memmem::Finder::new(needle.as_bytes()).into_owned());
        }
        let required: String = pattern
            .split(|&c| c == '%' || c == '_')
            .max_by_key(|run| run.len())
            .unwrap_or_default()
            .iter()
            .collect();
        let mut wrapped = Vec::with_capacity(pattern.len() + 2);
        wrapped.push('%');
        wrapped.extend(pattern);
        wrapped.push('%');
        Self::Wildcard {
            pattern: wrapped,
            required: memchr::memmem::Finder::new(required.as_bytes()).into_owned(),
        }
    }

    fn matches(&self, lowered: &str) -> bool {
        match self {
            Self::Literal(finder) => finder.find(lowered.as_bytes()).is_some(),
            Self::Wildcard { pattern, required } => {
                required.find(lowered.as_bytes()).is_some() && glob(lowered, pattern)
            }
        }
    }
}

/// `LIKE` matching of the whole `text` against `pattern` (`%` any run,
/// `_` one character): the iterative two-pointer match that backtracks
/// to the last `%` only, O(len(text) · len(pattern)).
fn glob(text: &str, pattern: &[char]) -> bool {
    let next = |at: usize| at + text[at..].chars().next().map_or(1, char::len_utf8);
    let (mut ti, mut pi) = (0, 0);
    // The pattern position after the last `%`, and the text position it
    // was tried at.
    let mut star: Option<(usize, usize)> = None;
    loop {
        if let Some(&c) = pattern.get(pi) {
            match c {
                '%' => {
                    pi += 1;
                    star = Some((pi, ti));
                    continue;
                }
                '_' if ti < text.len() => {
                    ti = next(ti);
                    pi += 1;
                    continue;
                }
                '_' => {}
                c if text[ti..].starts_with(c) => {
                    ti += c.len_utf8();
                    pi += 1;
                    continue;
                }
                _ => {}
            }
        } else if ti == text.len() {
            return true;
        }
        match star {
            Some((after, tried)) if tried < text.len() => {
                let retry = next(tried);
                star = Some((after, retry));
                pi = after;
                ti = retry;
            }
            _ => return false,
        }
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
        let like = Like::new(&term.to_lowercase());
        let mut rows: Vec<&IndexNode> = self
            .index
            .nodes()
            .iter()
            .zip(&self.lowered)
            .filter(|(n, _)| cluster_id.is_none() || n.macro_cluster == cluster_id)
            .filter(|(_, lowered)| like.matches(lowered))
            .map(|(n, _)| n)
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

    /// `LIKE '%pattern%'` on `LOWER(text)` as the first port wrote it:
    /// a recursive match at every start (exponential in `%`).
    fn like_contains_reference(text: &str, pattern: &str) -> bool {
        fn here(text: &[char], pattern: &[char]) -> bool {
            match pattern.first() {
                None => true,
                Some('%') => (0..=text.len()).any(|skip| here(&text[skip..], &pattern[1..])),
                Some('_') => !text.is_empty() && here(&text[1..], &pattern[1..]),
                Some(&c) => text.first() == Some(&c) && here(&text[1..], &pattern[1..]),
            }
        }
        let text: Vec<char> = text.chars().map(|c| c.to_ascii_lowercase()).collect();
        let pattern: Vec<char> = pattern.chars().map(|c| c.to_ascii_lowercase()).collect();
        (0..=text.len()).any(|start| here(&text[start..], &pattern))
    }

    fn like_contains(text: &str, pattern: &str) -> bool {
        Like::new(&pattern.to_lowercase()).matches(&text.to_ascii_lowercase())
    }

    #[test]
    fn like_is_ascii_case_insensitive_with_underscore_wildcard() {
        assert!(like_contains("Call get_Owner here", "get_owner"));
        assert!(like_contains("getXowner", "get_owner"));
        assert!(!like_contains("getowner", "get_owner"));
        assert!(!like_contains("ÉCOLE", "école"));
        assert!(like_contains("a\u{e9}b", "a_b"));
        assert!(like_contains("anything", ""));
    }

    /// The compiled matcher against the recursive one on random texts and
    /// patterns (ASCII case, a multi-byte letter, `%`, `_`).
    #[test]
    fn like_matches_the_recursive_reference() {
        let text_chars: Vec<char> = "aAbB_%\u{e9}\u{c9}x ".chars().collect();
        let pattern_chars: Vec<char> = "aAbB_%%__\u{e9}x".chars().collect();
        let mut state = 0x853c_49e6_748f_ea9b_u64;
        let mut next = move || {
            state ^= state << 13;
            state ^= state >> 7;
            state ^= state << 17;
            usize::try_from(state % 1_000_003).unwrap()
        };
        for _ in 0..20_000 {
            let text: String = (0..next() % 12)
                .map(|_| text_chars[next() % text_chars.len()])
                .collect();
            let pattern: String = (0..next() % 6)
                .map(|_| pattern_chars[next() % pattern_chars.len()])
                .collect();
            assert_eq!(
                like_contains(&text, &pattern),
                like_contains_reference(&text, &pattern.to_lowercase()),
                "{text:?} LIKE %{pattern:?}%"
            );
        }
    }

    /// Many `%` made the recursive matcher exponential; this is linear in
    /// the pattern and quadratic at worst.
    #[test]
    fn many_percent_signs_stay_fast() {
        let text = "a".repeat(4000);
        let pattern = format!("{}b", "a%".repeat(40));
        let started = std::time::Instant::now();
        assert!(!like_contains(&text, &pattern));
        let pattern = format!("{}_", "%a".repeat(40));
        assert!(like_contains(&text, &pattern));
        assert!(started.elapsed() < std::time::Duration::from_secs(1));
    }

    #[test]
    fn search_keeps_row_order_filters_and_limit() {
        let node = |id: &str, path: &str, line: i64, text: &str, cluster: Option<i64>| IndexNode {
            node_id: id.to_owned(),
            rel_path: path.to_owned(),
            start_line: Some(line),
            source_text: Some(text.to_owned()),
            macro_cluster: cluster,
            ..IndexNode::default()
        };
        let index = Arc::new(PageIndex::new(
            vec![
                node("n1", "b.py", 1, "call Get_Owner()", Some(1)),
                node("n2", "a.py", 9, "getXowner", Some(1)),
                node("n3", "a.py", 2, "GET_OWNER", Some(2)),
                node("n4", "a.py", 1, "nothing", Some(1)),
            ],
            Vec::new(),
            super::super::index::GraphFacts::default(),
        ));
        let search = SubstringSearch::new(index);
        assert_eq!(
            search.search("get_owner", None, 9).unwrap(),
            ["n3", "n2", "n1"]
        );
        assert_eq!(search.search("get_owner", Some(1), 1).unwrap(), ["n2"]);
        assert_eq!(search.search("Get_Owner()", None, 9).unwrap(), ["n1"]);
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
