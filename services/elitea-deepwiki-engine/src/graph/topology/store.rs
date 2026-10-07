//! What Phase 2 asks of the index: the [`TopologyStore`] trait.
//!
//! Python's Phase 2 reads and writes its `UnifiedWikiDB` (the `SQLite`
//! `.wiki.db`) directly. Here every such access is a method of this trait,
//! so the algorithm does not depend on the storage. Two implementations:
//!
//! * the RECORDED one ([`super::replay::ReplayStore`]) answers from a
//!   recording of the Python run, for the parity gate;
//! * the PRODUCTION one runs against the `PostgreSQL` build space (ADR-0026
//!   decision 5: `deepwiki_build.wiki_nodes` with its folded `tsvector`,
//!   `deepwiki_build.wiki_node_embeddings` with pgvector). ADR-0026 accepts
//!   that its lexical ranking is not FTS5's `bm25()`; the difference is
//!   measured, not hidden.
//!
//! The trait is synchronous: Phase 2 is a sequential pass that issues one
//! small query per orphan. An implementation over an async pool runs the
//! phase on a blocking thread (`tokio::task::spawn_blocking`) and blocks on
//! each query (`Handle::block_on`).
//!
//! # What each method must mean
//!
//! The method docs state the Python call each one replaces and the result
//! Phase 2 reads. A production implementation keeps the CONTRACT (which
//! rows match, which fields come back, the order); the ranking inside the
//! contract is the store's.

use serde_json::Value;

/// A storage failure. Python logged most of them at debug level and went
/// on with a poorer graph; this engine fails the phase instead, so a
/// broken index is not published as if it were whole.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("{0}")]
pub struct StoreError(pub String);

impl StoreError {
    pub fn new(message: impl Into<String>) -> Self {
        Self(message.into())
    }
}

/// The columns of one stored node (`repo_nodes` / `wiki_nodes`) that
/// Phase 2 reads: `UnifiedWikiDB.get_node`'s row, projected.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct StoredNode {
    pub symbol_name: String,
    pub rel_path: String,
    /// Lower-cased by the index writer.
    pub symbol_type: String,
    /// `NULL` when the parser symbol had no source text.
    pub source_text: Option<String>,
    pub docstring: Option<String>,
    /// `is_doc` as stored (0 or 1).
    pub is_doc: i64,
    pub language: String,
}

/// One search result: the fields of the joined node row Phase 2 reads,
/// and the score of the branch that found it.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct SearchHit {
    pub node_id: String,
    pub rel_path: String,
    pub symbol_type: String,
    /// Lexical rank, lower is better (FTS5's `rank`; negated BM25 in
    /// `PostgreSQL`). Lexical hits only.
    pub fts_rank: Option<f64>,
    /// `1 / (1 + exp(fts_rank))`. Lexical hits only.
    pub score_norm: Option<f64>,
    /// L2 distance. Dense hits only.
    pub vec_distance: Option<f64>,
}

/// The index operations Phase 2 needs.
pub trait TopologyStore {
    /// `get_node(id)` for each of `ids`, in order; `None` for an id the
    /// index does not hold. Batched: Python asked one id at a time, and
    /// the rows do not change during the phase.
    ///
    /// # Errors
    ///
    /// A [`StoreError`] when the store cannot answer.
    fn get_nodes(&mut self, ids: &[&str]) -> Result<Vec<Option<StoredNode>>, StoreError>;

    /// `node_count()`: the number of nodes in the index.
    ///
    /// # Errors
    ///
    /// A [`StoreError`] when the store cannot answer.
    fn node_count(&mut self) -> Result<u64, StoreError>;

    /// `count_fts_matches(query, exact_match=True)`: how many nodes the
    /// PHRASE `query` matches (the IDF gate of the tiered lexical pass).
    /// FTS5 matched `"<query>"`; `PostgreSQL` matches
    /// `phraseto_tsquery('deepwiki_porter', <folded query>)`.
    ///
    /// # Errors
    ///
    /// A [`StoreError`] when the store cannot answer.
    fn count_phrase_matches(&mut self, query: &str) -> Result<u64, StoreError>;

    /// `search_fts5(query, limit=…)` / `search_fts_with_path(query,
    /// path_prefix=…, limit=…)`: at most `limit` lexical hits, best first,
    /// each with `fts_rank` and `score_norm`. With a prefix, only nodes
    /// whose `rel_path` is under `<prefix>/` (FTS5: `rel_path GLOB
    /// '<prefix>/*'`). The query is the symbol name as Python passed it,
    /// not escaped: FTS5 read it as a query expression (and returned
    /// nothing for one it could not parse), `PostgreSQL` folds it through
    /// `plainto_tsquery`.
    ///
    /// # Errors
    ///
    /// A [`StoreError`] when the store cannot answer.
    fn search_lexical(
        &mut self,
        query: &str,
        path_prefix: Option<&str>,
        limit: usize,
    ) -> Result<Vec<SearchHit>, StoreError>;

    /// `get_embedding_by_id(id)` for each of `ids`, in order: the stored
    /// vector, or `None`. Batched, as [`TopologyStore::get_nodes`].
    ///
    /// # Errors
    ///
    /// A [`StoreError`] when the store cannot answer.
    fn get_embeddings(&mut self, ids: &[&str]) -> Result<Vec<Option<Vec<f64>>>, StoreError>;

    /// `search_vec(embedding, k=…, path_prefix=…)`: the nearest stored
    /// vectors by L2 distance, nearest first, each with `vec_distance`.
    /// sqlite-vec took the `k` nearest of ALL vectors and then dropped
    /// those outside the prefix (so fewer than `k` could come back);
    /// pgvector filters first. That is part of the measured difference.
    ///
    /// # Errors
    ///
    /// A [`StoreError`] when the store cannot answer.
    fn search_dense(
        &mut self,
        embedding: &[f64],
        k: usize,
        path_prefix: Option<&str>,
    ) -> Result<Vec<SearchHit>, StoreError>;

    /// `set_hub(id, is_hub=True)` for each hub: `is_hub = 1`,
    /// `hub_assignment = NULL`.
    ///
    /// # Errors
    ///
    /// A [`StoreError`] when the store cannot write.
    fn set_hubs(&mut self, hubs: &[&str]) -> Result<(), StoreError>;

    /// `persist_weights_to_db`: delete every stored edge and write `rows`
    /// (the whole graph, in graph order, produced one at a time so the
    /// rows are never all in memory). Returns the stored edge count
    /// afterwards (`edge_count()`), which Phase 2 reports.
    ///
    /// # Errors
    ///
    /// A [`StoreError`] when the store cannot write.
    fn replace_edges(
        &mut self,
        rows: &mut dyn Iterator<Item = crate::graph::EdgeRow>,
    ) -> Result<u64, StoreError>;

    /// `set_meta(key, value)`.
    ///
    /// # Errors
    ///
    /// A [`StoreError`] when the store cannot write.
    fn set_meta(&mut self, key: &str, value: &Value) -> Result<(), StoreError>;
}

/// The `embedding_fn` Phase 2 receives: text → vector, for an orphan whose
/// vector is not stored. `None` from Python's `run_phase2` default means
/// no fallback; here that is `Option::None` for the whole embedder.
pub trait TextEmbedder {
    /// Embed one text.
    ///
    /// # Errors
    ///
    /// A [`StoreError`] when the model cannot answer. Phase 2 then treats
    /// the orphan as having no vector, logs a warning and goes on, as
    /// Python did; only index failures fail the phase.
    fn embed(&mut self, text: &str) -> Result<Vec<f64>, StoreError>;
}
