//! `search_workspace`: one ranked list of places in the workspace for a
//! question, from every signal the index has.
//!
//! * **lexical** — BM25 over the chunks (`chunks_fts`: porter-stemmed
//!   text, plus the identifier column that splits `camelCase` and
//!   `snake_case` and holds the path's parts);
//! * **graph** — the Inventory search over the code graph (names, types,
//!   descriptions), each entity mapped to the chunk its citation falls in;
//! * **neighbour** — the chunks of the entities one hop from the best graph
//!   matches (callers, callees, containers, imports): a smaller boost;
//! * **semantic** — the chunk vectors nearest the query, when embeddings
//!   are turned on ([`Dense`]).
//!
//! The lists are fused by weighted reciprocal rank (`k = 60`):
//! `score = Σ weight / (60 + rank)`. Without vectors the text lists weigh
//! 1 (neighbours ½); with them, the text side weighs 0.3 and the dense
//! side 0.7, the cloud vector index's fusion (`vector-index`).

use crate::chunks::split_identifier;
use crate::chunks::{ChunkKind, query_terms};
use crate::sqlite_store::{SqliteGraphStore, StoreError, StoredChunk};
use elitea_inventory_core::retrieval::query::citations_of;
use elitea_inventory_core::retrieval::view::GraphView;
use serde::Serialize;
use serde_json::{Map, Value};
use std::collections::HashMap;

/// The RRF constant.
const RRF_K: f64 = 60.0;
/// The text side's weight when vectors take part, and the dense side's.
pub const TEXT_WEIGHT: f64 = 0.3;
pub const DENSE_WEIGHT: f64 = 0.7;
/// How many candidates each list contributes.
const CANDIDATES: usize = 100;
/// The most lines and characters of a snippet.
const SNIPPET_LINES: usize = 12;
const SNIPPET_CHARS: usize = 1200;
/// The most results.
pub const MAX_TOP_K: usize = 50;

/// Which signals a search uses.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Mode {
    /// Every signal the index has.
    Auto,
    /// The text and the graph only (no vectors).
    Lexical,
    /// The vectors only (falls back to lexical, saying so, without them).
    Semantic,
}

/// One `search_workspace` call.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Request {
    pub query: String,
    pub top_k: usize,
    pub path_glob: Option<String>,
    pub kind: Option<ChunkKind>,
    pub mode: Mode,
    /// The agent turn the search runs for (never an argument: the host
    /// sets it); a remote query embedding is attributed to it.
    pub execution_id: Option<String>,
}

impl Request {
    /// The request of the tool's arguments.
    ///
    /// # Errors
    ///
    /// A missing query, or a `kind` / `mode` it does not know.
    pub fn from_args(args: &Map<String, Value>) -> Result<Self, String> {
        let query = args
            .get("query")
            .and_then(Value::as_str)
            .map(str::trim)
            .filter(|q| !q.is_empty())
            .ok_or("query is required")?
            .to_owned();
        let top_k = args
            .get("top_k")
            .and_then(Value::as_u64)
            .map_or(10, |k| usize::try_from(k).unwrap_or(MAX_TOP_K))
            .clamp(1, MAX_TOP_K);
        let path_glob = args
            .get("path_glob")
            .and_then(Value::as_str)
            .map(str::trim)
            .filter(|g| !g.is_empty())
            .map(str::to_owned);
        if let Some(glob) = &path_glob
            && PathGlob::new(glob).is_none()
        {
            return Err(format!("path_glob {glob:?} is not a glob"));
        }
        let kind = match args.get("kind").and_then(Value::as_str).unwrap_or("any") {
            "any" => None,
            other => Some(
                ChunkKind::parse(other)
                    .ok_or_else(|| format!("kind must be code, docs or any, not {other}"))?,
            ),
        };
        let mode = match args.get("mode").and_then(Value::as_str).unwrap_or("auto") {
            "auto" => Mode::Auto,
            "lexical" => Mode::Lexical,
            "semantic" => Mode::Semantic,
            other => {
                return Err(format!(
                    "mode must be auto, lexical or semantic, not {other}"
                ));
            }
        };
        Ok(Self {
            query,
            top_k,
            path_glob,
            kind,
            mode,
            execution_id: None,
        })
    }
}

/// The nearest chunks of a query by their vectors.
pub trait Dense: Send + Sync {
    /// `(chunk id, cosine)`, best first, at most `limit`.
    ///
    /// # Errors
    ///
    /// The query could not be embedded (the model failed, the service
    /// refused): the search goes on without the dense side, saying why.
    fn nearest(
        &self,
        query: &str,
        limit: usize,
        execution_id: Option<&str>,
    ) -> Result<Vec<(i64, f32)>, String>;
}

/// One result.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct Hit {
    pub path: String,
    pub line_start: u32,
    pub line_end: u32,
    pub snippet: String,
    pub score: f64,
    /// `lexical`, `graph`, `neighbour`, `semantic`: the lists it was in.
    pub matched_by: Vec<&'static str>,
    /// The entity it holds, as `get_entity_details` takes it.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub entity_ref: Option<String>,
}

/// What a search answers.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct Answer {
    pub hits: Vec<Hit>,
    /// Why a signal was left out (semantic asked for without vectors, the
    /// query embedding failed).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub note: Option<String>,
}

/// A `path_glob`, compiled once per search (globset, linear in the path
/// whatever the pattern): `**` any run of path segments, `*` any run
/// within one segment, `?` one character. A pattern without a `/` matches
/// the file name or the whole path.
#[derive(Debug, Clone)]
pub struct PathGlob {
    matcher: globset::GlobMatcher,
    /// No `/` in the pattern: the file name may match too.
    name_too: bool,
}

impl PathGlob {
    /// `None` when the pattern does not parse.
    #[must_use]
    pub fn new(pattern: &str) -> Option<Self> {
        let pattern = pattern.trim_start_matches("./");
        let matcher = globset::GlobBuilder::new(pattern)
            .literal_separator(true)
            .backslash_escape(true)
            .build()
            .ok()?
            .compile_matcher();
        Some(Self {
            matcher,
            name_too: !pattern.contains('/'),
        })
    }

    #[must_use]
    pub fn is_match(&self, path: &str) -> bool {
        self.matcher.is_match(path)
            || (self.name_too
                && self
                    .matcher
                    .is_match(path.rsplit('/').next().unwrap_or(path)))
    }
}

/// Whether `path` matches the glob ([`PathGlob`]); a pattern that does not
/// parse matches nothing.
#[must_use]
pub fn glob_match(pattern: &str, path: &str) -> bool {
    PathGlob::new(pattern).is_some_and(|glob| glob.is_match(path))
}

/// The FTS5 expression of a query: its terms (compound identifiers split
/// too), quoted, any of them. `None` when nothing is left to look for.
#[must_use]
pub fn fts_expression(query: &str) -> Option<String> {
    let terms: Vec<String> = query_terms(query)
        .into_iter()
        .map(|term| format!("\"{}\"", term.replace('"', "\"\"")))
        .collect();
    (!terms.is_empty()).then(|| terms.join(" OR "))
}

/// Chunks read once per search.
struct Chunks<'a> {
    store: &'a SqliteGraphStore,
    by_id: HashMap<i64, StoredChunk>,
    covering: HashMap<(String, i64, i64), Option<i64>>,
}

impl<'a> Chunks<'a> {
    fn new(store: &'a SqliteGraphStore) -> Self {
        Self {
            store,
            by_id: HashMap::new(),
            covering: HashMap::new(),
        }
    }

    fn load(&mut self, ids: &[i64]) -> Result<(), StoreError> {
        let missing: Vec<i64> = ids
            .iter()
            .copied()
            .filter(|id| !self.by_id.contains_key(id))
            .collect();
        for chunk in self.store.chunks_by_id(&missing)? {
            self.by_id.insert(chunk.id, chunk);
        }
        Ok(())
    }

    /// The chunk of `path` that best covers `start..=end`.
    fn covering(&mut self, path: &str, start: i64, end: i64) -> Result<Option<i64>, StoreError> {
        let key = (path.to_owned(), start, end);
        if let Some(known) = self.covering.get(&key) {
            return Ok(*known);
        }
        let found = self.store.chunk_covering(path, start, end)?;
        self.covering.insert(key, found);
        Ok(found)
    }

    fn admits(&self, id: i64, request: &Request, glob: Option<&Option<PathGlob>>) -> bool {
        self.by_id.get(&id).is_some_and(|chunk| {
            request.kind.is_none_or(|kind| chunk.kind == kind)
                && glob.is_none_or(|glob| glob.as_ref().is_some_and(|g| g.is_match(&chunk.path)))
        })
    }
}

/// The graph's entities by the words of their names: what the graph signal
/// matches a query against. Built once per build (a walk of the nodes);
/// a query is a few lookups, where the Inventory search scores every node
/// (160 ms per query on this repository's 223,000 entities).
#[derive(Debug, Default)]
pub struct NameIndex {
    /// Entity ids, in node order.
    ids: Vec<String>,
    /// How many words each entity's name has.
    words: Vec<u16>,
    /// Lower-case word → the entities whose name has it.
    by_word: HashMap<String, Vec<u32>>,
    /// The revision it was built from.
    pub revision: i64,
}

impl NameIndex {
    /// The index of `view`'s names (files and directories left out: the
    /// chunks' path column finds those).
    #[must_use]
    pub fn build(view: &GraphView) -> Self {
        let mut index = Self {
            revision: view.revision,
            ..Self::default()
        };
        for (id, node) in view.graph.nodes() {
            let Some(name) = node.get("name").and_then(Value::as_str) else {
                continue;
            };
            if matches!(
                node.get("type").and_then(Value::as_str),
                Some("file" | "source_file" | "directory" | "folder")
            ) {
                continue;
            }
            let mut words = Vec::new();
            split_identifier(name, &mut words);
            words.sort();
            words.dedup();
            if words.is_empty() || words.len() > 12 {
                continue;
            }
            let at = u32::try_from(index.ids.len()).unwrap_or(u32::MAX);
            index.ids.push(id.to_owned());
            index
                .words
                .push(u16::try_from(words.len()).unwrap_or(u16::MAX));
            for word in words {
                index.by_word.entry(word).or_default().push(at);
            }
        }
        index
    }

    /// The entities whose names share the most words with `query`, best
    /// first: more query words matched, then the larger share of the
    /// name matched (an exact name first), then node order.
    #[must_use]
    pub fn matches(&self, query: &str, limit: usize) -> Vec<&str> {
        let terms = query_terms(query);
        let mut matched: HashMap<u32, u16> = HashMap::new();
        for term in &terms {
            // A very common word (`new`, `get`) alone says little: it
            // counts only for entities another word matched too.
            if let Some(entities) = self.by_word.get(term) {
                for entity in entities {
                    *matched.entry(*entity).or_default() += 1;
                }
            }
        }
        let mut ranked: Vec<(u32, u16, f64)> = matched
            .into_iter()
            .map(|(entity, count)| {
                let words = f64::from(self.words[entity as usize]);
                (entity, count, f64::from(count) / words)
            })
            .filter(|(entity, count, _)| {
                *count > 1 || self.by_word_len_of(*entity) || terms.len() == 1
            })
            .collect();
        ranked.sort_by(|a, b| b.1.cmp(&a.1).then(b.2.total_cmp(&a.2)).then(a.0.cmp(&b.0)));
        ranked
            .into_iter()
            .take(limit)
            .map(|(entity, _, _)| self.ids[entity as usize].as_str())
            .collect()
    }

    /// Whether the entity's name is one word (a one-word name matched by
    /// one query word is a full match).
    fn by_word_len_of(&self, entity: u32) -> bool {
        self.words[entity as usize] == 1
    }
}

/// An entity as `get_entity_details` takes it.
fn entity_ref(view: &GraphView, id: &str) -> Option<String> {
    let node = view.node(id)?;
    let name = node.get("name").and_then(Value::as_str)?;
    Some(match node.get("type").and_then(Value::as_str) {
        Some(kind) => format!("{name} ({kind})"),
        None => name.to_owned(),
    })
}

/// The file and lines an entity is cited at.
fn cited_at(view: &GraphView, id: &str) -> Option<(String, i64, i64)> {
    let node = view.node(id)?;
    let citation = citations_of(node).into_iter().next()?;
    let path = citation.get("file_path")?.as_str()?.to_owned();
    let line = |key: &str| citation.get(key).and_then(Value::as_i64).unwrap_or(0);
    (!path.is_empty()).then(|| (path, line("line_start"), line("line_end")))
}

/// The lines of `text` around its best run of query terms.
fn snippet(text: &str, terms: &[String]) -> String {
    let lines: Vec<&str> = text.lines().collect();
    let hits: Vec<usize> = lines
        .iter()
        .map(|line| {
            let lower = line.to_lowercase();
            terms
                .iter()
                .filter(|term| lower.contains(term.as_str()))
                .count()
        })
        .collect();
    let window = SNIPPET_LINES.min(lines.len());
    let start = (0..=lines.len() - window)
        .max_by_key(|&start| {
            // The most matches; ties to the earliest window.
            (
                hits[start..start + window].iter().sum::<usize>(),
                usize::MAX - start,
            )
        })
        .unwrap_or(0);
    let mut out = String::new();
    for line in &lines[start..start + window] {
        if out.len() + line.len() > SNIPPET_CHARS {
            break;
        }
        out.push_str(line);
        out.push('\n');
    }
    out.truncate(out.trim_end().len());
    out
}

/// The graph's matches and their one-hop neighbours, as chunk ids, best
/// first; each chunk's entity noted in `entity_of`.
fn graph_lists(
    view: &GraphView,
    names: &NameIndex,
    chunks: &mut Chunks<'_>,
    request: &Request,
    entity_of: &mut HashMap<i64, String>,
) -> Result<(Vec<i64>, Vec<i64>), StoreError> {
    let hits = names.matches(&request.query, 50);
    let mut place = |id: &str, list: &mut Vec<i64>| -> Result<(), StoreError> {
        if let Some((path, start, end)) = cited_at(view, id)
            && let Some(chunk) = chunks.covering(&path, start, end)?
            && !list.contains(&chunk)
        {
            list.push(chunk);
            if let Some(reference) = entity_ref(view, id) {
                entity_of.entry(chunk).or_insert(reference);
            }
        }
        Ok(())
    };
    let mut graph = Vec::new();
    for hit in &hits {
        place(hit, &mut graph)?;
    }
    let mut neighbours = Vec::new();
    for hit in hits.iter().take(5) {
        let next: Vec<&str> = view
            .out_edges(hit)
            .map(|(target, _)| target)
            .chain(view.in_edges(hit).map(|(source, _)| source))
            .take(8)
            .collect();
        for neighbour in next {
            place(neighbour, &mut neighbours)?;
        }
    }
    Ok((graph, neighbours))
}

/// Weighted reciprocal-rank fusion of `lists` over the chunks `request`
/// admits: `(chunk id, score, lists it was in)`, best first.
fn fuse(
    lists: &[(&'static str, f64, Vec<i64>)],
    chunks: &Chunks<'_>,
    request: &Request,
) -> Vec<(i64, f64, Vec<&'static str>)> {
    // Compiled once (`Some(None)`: it does not parse, nothing is admitted).
    let glob = request.path_glob.as_deref().map(PathGlob::new);
    let mut fused: HashMap<i64, (f64, Vec<&'static str>)> = HashMap::new();
    for (name, weight, ids) in lists {
        let mut rank = 0_u32;
        for id in ids {
            if !chunks.admits(*id, request, glob.as_ref()) {
                continue;
            }
            rank += 1;
            if usize::try_from(rank).unwrap_or(usize::MAX) > CANDIDATES {
                break;
            }
            let entry = fused.entry(*id).or_insert((0.0, Vec::new()));
            entry.0 += weight / (RRF_K + f64::from(rank));
            if !entry.1.contains(name) {
                entry.1.push(name);
            }
        }
    }
    let mut ranked: Vec<(i64, f64, Vec<&'static str>)> = fused
        .into_iter()
        .map(|(id, (score, by))| (id, score, by))
        .collect();
    ranked.sort_by(|a, b| b.1.total_cmp(&a.1).then(a.0.cmp(&b.0)));
    ranked.truncate(request.top_k);
    ranked
}

/// Search the workspace.
///
/// # Errors
///
/// The store failed.
pub fn search_workspace(
    store: &SqliteGraphStore,
    view: &GraphView,
    names: &NameIndex,
    request: &Request,
    dense: Option<&dyn Dense>,
) -> Result<Answer, StoreError> {
    let mut chunks = Chunks::new(store);
    let mut note = None;
    let terms = query_terms(&request.query);
    // (name, weight, ranked chunk ids)
    let mut lists: Vec<(&'static str, f64, Vec<i64>)> = Vec::new();
    let mut entity_of: HashMap<i64, String> = HashMap::new();

    let mut dense_ids = Vec::new();
    let use_dense = request.mode != Mode::Lexical;
    if use_dense {
        match dense {
            Some(dense) => match dense.nearest(
                &request.query,
                CANDIDATES * 2,
                request.execution_id.as_deref(),
            ) {
                Ok(ranked) => dense_ids = ranked.into_iter().map(|(id, _)| id).collect(),
                Err(why) => note = Some(format!("semantic search was skipped: {why}")),
            },
            None if request.mode == Mode::Semantic => {
                note = Some(
                    "semantic search needs embeddings, which are off for this workspace; these are lexical results"
                        .to_owned(),
                );
            }
            None => {}
        }
    }
    let semantic_only = request.mode == Mode::Semantic && !dense_ids.is_empty();
    let text_weight = if dense_ids.is_empty() {
        1.0
    } else {
        TEXT_WEIGHT
    };

    if !semantic_only {
        // Lexical: more candidates when a filter will drop some.
        let filtered = request.kind.is_some() || request.path_glob.is_some();
        let limit = if filtered { CANDIDATES * 5 } else { CANDIDATES };
        let lexical: Vec<i64> = fts_expression(&request.query)
            .map(|expression| store.search_fts(&expression, limit))
            .transpose()?
            .unwrap_or_default()
            .into_iter()
            .map(|(id, _)| id)
            .collect();
        chunks.load(&lexical)?;
        lists.push(("lexical", text_weight, lexical));
        let (graph, neighbours) = graph_lists(view, names, &mut chunks, request, &mut entity_of)?;
        // Their chunks are often not among the full-text hits: loaded too,
        // or the filters would see nothing to admit.
        chunks.load(&graph)?;
        chunks.load(&neighbours)?;
        lists.push(("graph", text_weight, graph));
        lists.push(("neighbour", text_weight / 2.0, neighbours));
    }
    if !dense_ids.is_empty() {
        chunks.load(&dense_ids)?;
        let weight = if semantic_only { 1.0 } else { DENSE_WEIGHT };
        lists.push(("semantic", weight, dense_ids));
    }

    let ranked = fuse(&lists, &chunks, request);
    let hits = ranked
        .into_iter()
        .filter_map(|(id, score, matched_by)| {
            let chunk = chunks.by_id.get(&id)?;
            Some(Hit {
                path: chunk.path.clone(),
                line_start: chunk.line_start,
                line_end: chunk.line_end,
                snippet: snippet(&chunk.text, &terms),
                score: (score * 10_000.0).round() / 10_000.0,
                matched_by,
                entity_ref: entity_of.get(&id).cloned(),
            })
        })
        .collect();
    Ok(Answer { hits, note })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn globs_match_segments_and_file_names() {
        assert!(glob_match("*.rs", "src/index.rs"));
        assert!(glob_match("index.rs", "apps/x/src/index.rs"));
        assert!(glob_match("src/**/*.rs", "src/a/b/c.rs"));
        assert!(glob_match("src/**/*.rs", "src/c.rs"));
        assert!(glob_match("**/tests/*", "libs/rust/x/tests/a.rs"));
        assert!(!glob_match("src/*.rs", "src/a/b.rs"));
        assert!(!glob_match("*.go", "src/index.rs"));
        assert!(glob_match("docs/?.md", "docs/a.md"));
        assert!(glob_match("./src/*.rs", "src/a.rs"));
        assert!(!glob_match("[", "src/a.rs"), "does not parse: nothing");
    }

    #[test]
    fn a_pathological_glob_answers_at_once() {
        let pattern = "**a".repeat(40) + "**b";
        let path = "a".repeat(400);
        let started = std::time::Instant::now();
        for _ in 0..100 {
            assert!(!glob_match(&pattern, &path));
        }
        assert!(
            started.elapsed() < std::time::Duration::from_secs(2),
            "{:?}",
            started.elapsed()
        );
    }

    #[test]
    fn queries_become_quoted_alternatives() {
        assert_eq!(
            fts_expression("where is IndexRegistry \"x\"").as_deref(),
            Some("\"index\" OR \"registry\" OR \"indexregistry\" OR \"x\"")
        );
        assert_eq!(fts_expression("how is the"), None);
    }

    #[test]
    fn arguments_are_checked() {
        let args = |v: Value| v.as_object().unwrap().clone();
        let request =
            Request::from_args(&args(serde_json::json!({"query": " q ", "top_k": 500}))).unwrap();
        assert_eq!(
            (request.query.as_str(), request.top_k, request.mode),
            ("q", MAX_TOP_K, Mode::Auto)
        );
        assert!(Request::from_args(&args(serde_json::json!({"query": ""}))).is_err());
        assert!(Request::from_args(&args(serde_json::json!({"query": "q", "kind": "x"}))).is_err());
        assert!(Request::from_args(&args(serde_json::json!({"query": "q", "mode": "x"}))).is_err());
        let docs = Request::from_args(&args(
            serde_json::json!({"query": "q", "kind": "docs", "mode": "lexical"}),
        ))
        .unwrap();
        assert_eq!(
            (docs.kind, docs.mode),
            (Some(ChunkKind::Docs), Mode::Lexical)
        );
    }

    #[test]
    fn a_snippet_is_the_window_with_the_most_terms() {
        let text = (1..=40)
            .map(|n| {
                if n == 30 {
                    "fn refresh_index() {}".to_owned()
                } else {
                    format!("line {n}")
                }
            })
            .collect::<Vec<_>>()
            .join("\n");
        let snippet = snippet(&text, &["refresh".to_owned()]);
        assert!(snippet.contains("refresh_index"), "{snippet}");
        assert_eq!(snippet.lines().count(), SNIPPET_LINES);
    }
}
