//! Phase 2's index on PostgreSQL: [`PgTopologyStore`], the production
//! [`TopologyStore`] over one build's staged rows (ADR-0026 decision 5).
//!
//! Python's Phase 2 read its `.wiki.db` (FTS5 and sqlite-vec). Here every
//! read goes to the `deepwiki_build` tables the build staged before the
//! phase, with the semantics the parity tool's `PostgresSearchDB`
//! (`parity/python_reference.py --search-dsn`) measured against Python:
//!
//! * **lexical** (`search_lexical`): the folded `plainto_tsquery` over the
//!   staged `fts` column (the generated expression 0001 and 0003 share),
//!   optionally limited to `rel_path` under `<prefix>/` (a range on the
//!   C-collated path, FTS5's `GLOB '<prefix>/*'`), ranked by BM25 with
//!   k1 1.2 and b 0.75 over the build's own statistics — a document's
//!   length is the sum of its lexemes' position counts, a lexeme's document
//!   frequency is the number of staged nodes it occurs in — negated to
//!   FTS5's sign and broken by node id. `score_norm` is
//!   `1 / (1 + exp(rank))`, as `_attach_score_norm`;
//! * **phrase count** (`count_phrase_matches`): `phraseto_tsquery` over the
//!   same column;
//! * **dense** (`search_dense`): the exact pgvector L2 distance, ranked over
//!   ALL the build's vectors first and filtered by the prefix after the
//!   limit — sqlite-vec's order, by the owner's decision, so fewer than `k`
//!   hits can come back, as in Python. (Filtering first, pgvector's natural
//!   order, made 3–10× more semantic edges on the parity corpora.)
//!
//! Writes: `replace_edges` replaces the staged edges (collapsed onto the
//! primary key, as the publish would collapse them). The hub flags and the
//! meta entries have no column in the ADR-0022 schema (`publish.py` never
//! copied `is_hub`, `hub_assignment` or `wiki_meta`), so the store keeps
//! them for the caller ([`PgTopologyStore::into_parts`]).
//!
//! The trait is synchronous, so the store runs on a blocking thread
//! (`tokio::task::spawn_blocking`) and blocks on each query through the
//! runtime [`Handle`]. Each call first checks the invocation's
//! [`StopSignal`]: a stop fails the phase at its next index access, which
//! is its checkpoint inside the orphan loop.

use super::build::Build;
use super::rows::{IndexEdge, collapse_edges};
use crate::graph::EdgeRow;
use crate::graph::topology::{SearchHit, StoreError, StoredNode, TopologyStore};
use crate::runner::StopSignal;
use crate::storage::text;
use serde_json::Value;
use sqlx::Row;
use std::collections::HashMap;
use tokio::runtime::Handle;

/// Rows per `= ANY($ids)` lookup.
const LOOKUP_ROUND: usize = 5_000;

/// The message of a store call made after a stop request. The caller maps
/// the failed phase to the stop line.
pub const STOPPED: &str = "Invocation cancelled";

/// The folded text a lexical query is matched with: every run of
/// non-alphanumeric characters becomes one space, as the stored `fts`
/// expression folds the indexed text (migration 0001).
const FOLD: &str = "regexp_replace($2, '[^[:alnum:]]+', ' ', 'g')";

/// The BM25 statistics of the whole build: `N` and the mean document
/// length.
#[derive(Debug, Clone, Copy, PartialEq)]
struct Corpus {
    documents: f64,
    average_length: f64,
}

/// [`TopologyStore`] over one build's staged rows.
#[derive(Debug)]
pub struct PgTopologyStore {
    build: Build,
    handle: Handle,
    stop: StopSignal,
    corpus: Option<Corpus>,
    /// Document frequency per lexeme, read once per lexeme: the staged
    /// nodes do not change during the phase.
    document_frequency: HashMap<String, f64>,
    node_count: Option<u64>,
    hubs: Vec<String>,
    meta: Vec<(String, Value)>,
}

/// What the store hands back after the phase.
#[derive(Debug)]
pub struct StoreParts {
    pub build: Build,
    /// Every hub Phase 2 flagged (`set_hub`), in its order.
    pub hubs: Vec<String>,
    /// The meta entries Phase 2 wrote (`phase2_completed`, `phase2_stats`).
    pub meta: Vec<(String, Value)>,
}

fn database(error: impl std::fmt::Display) -> StoreError {
    StoreError::new(format!("the build space could not answer: {error}"))
}

/// `rel_path GLOB '<prefix>/*'` as a range on the C-collated path:
/// `[<prefix>/, <prefix>0)` (`0` follows `/`). `None` for no prefix (an
/// empty one included, as Python's `if path_prefix`).
fn prefix_range(path_prefix: Option<&str>) -> (Option<String>, Option<String>) {
    match path_prefix.filter(|p| !p.is_empty()) {
        None => (None, None),
        Some(prefix) => {
            let start = format!("{}/", prefix.trim_end_matches('/'));
            let end = format!("{}0", &start[..start.len() - 1]);
            (Some(start), Some(end))
        }
    }
}

impl PgTopologyStore {
    /// The store of `build`, whose nodes (and vectors) are staged. Its
    /// queries run on `handle`; `stop` fails the next call after a stop.
    #[must_use]
    pub fn new(build: Build, handle: Handle, stop: StopSignal) -> Self {
        Self {
            build,
            handle,
            stop,
            corpus: None,
            document_frequency: HashMap::new(),
            node_count: None,
            hubs: Vec::new(),
            meta: Vec::new(),
        }
    }

    /// The build back, with what Phase 2 wrote that has no column.
    #[must_use]
    pub fn into_parts(self) -> StoreParts {
        StoreParts {
            build: self.build,
            hubs: self.hubs,
            meta: self.meta,
        }
    }

    fn check_stop(&self) -> Result<(), StoreError> {
        if self.stop.is_requested() {
            Err(StoreError::new(STOPPED))
        } else {
            Ok(())
        }
    }

    fn corpus(&mut self) -> Result<Corpus, StoreError> {
        if let Some(corpus) = self.corpus {
            return Ok(corpus);
        }
        let pool = self.build.pool().clone();
        let build = self.build.build_id().to_owned();
        let row = self
            .handle
            .block_on(
                sqlx::query(
                    "SELECT count(*)::float8 AS documents, avg(coalesce(( \
                         SELECT sum(coalesce(array_length(u.positions, 1), 1)) \
                         FROM unnest(n.fts) AS u(lexeme, positions, weights)), 0)::float8 \
                     ) AS average_length \
                     FROM deepwiki_build.wiki_nodes n WHERE n.build_id = $1",
                )
                .bind(&build)
                .fetch_one(&pool),
            )
            .map_err(database)?;
        let documents: f64 = row.try_get("documents").map_err(database)?;
        let average: Option<f64> = row.try_get("average_length").map_err(database)?;
        // `self._avgdl if self._avgdl and self._avgdl > 0 else 1.0`.
        let average_length = average.filter(|a| *a > 0.0).unwrap_or(1.0);
        let corpus = Corpus {
            documents,
            average_length,
        };
        self.corpus = Some(corpus);
        Ok(corpus)
    }

    /// The document frequency of each of `terms` (lexemes), cached.
    fn frequencies(&mut self, terms: &[String]) -> Result<Vec<f64>, StoreError> {
        let missing: Vec<String> = terms
            .iter()
            .filter(|t| !self.document_frequency.contains_key(*t))
            .cloned()
            .collect();
        if !missing.is_empty() {
            let pool = self.build.pool().clone();
            let build = self.build.build_id().to_owned();
            // `'lexeme'::tsquery` names the lexeme itself, unnormalised; a
            // folded lexeme is alphanumeric, so `quote_literal` quotes it
            // plainly. One GIN lookup per lexeme.
            let rows = self
                .handle
                .block_on(
                    sqlx::query(
                        "SELECT t.term, ( \
                             SELECT count(*) FROM deepwiki_build.wiki_nodes n \
                             WHERE n.build_id = $1 AND n.fts @@ quote_literal(t.term)::tsquery \
                         )::float8 AS df \
                         FROM unnest($2::text[]) AS t(term)",
                    )
                    .bind(&build)
                    .bind(&missing)
                    .fetch_all(&pool),
                )
                .map_err(database)?;
            for row in rows {
                let term: String = row.try_get("term").map_err(database)?;
                let df: f64 = row.try_get("df").map_err(database)?;
                self.document_frequency.insert(term, df);
            }
        }
        Ok(terms
            .iter()
            .map(|t| self.document_frequency.get(t).copied().unwrap_or(0.0))
            .collect())
    }
}

impl TopologyStore for PgTopologyStore {
    fn get_nodes(&mut self, ids: &[&str]) -> Result<Vec<Option<StoredNode>>, StoreError> {
        self.check_stop()?;
        let pool = self.build.pool().clone();
        let build = self.build.build_id().to_owned();
        let mut found: HashMap<String, StoredNode> = HashMap::with_capacity(ids.len());
        for round in ids.chunks(LOOKUP_ROUND) {
            let rows = self
                .handle
                .block_on(
                    sqlx::query(
                        "SELECT node_id, symbol_name, rel_path, symbol_type, source_text, \
                             docstring, is_doc, language \
                         FROM deepwiki_build.wiki_nodes \
                         WHERE build_id = $1 AND node_id = ANY($2)",
                    )
                    .bind(&build)
                    .bind(round)
                    .fetch_all(&pool),
                )
                .map_err(database)?;
            for row in rows {
                let is_doc: bool = row.try_get("is_doc").map_err(database)?;
                found.insert(
                    row.try_get("node_id").map_err(database)?,
                    StoredNode {
                        symbol_name: row.try_get("symbol_name").map_err(database)?,
                        rel_path: row.try_get("rel_path").map_err(database)?,
                        symbol_type: row.try_get("symbol_type").map_err(database)?,
                        // NOT NULL in the build space: a NULL was staged as
                        // "", which every Phase 2 reader treats as absent.
                        source_text: Some(row.try_get("source_text").map_err(database)?),
                        docstring: Some(row.try_get("docstring").map_err(database)?),
                        is_doc: i64::from(is_doc),
                        language: row.try_get("language").map_err(database)?,
                    },
                );
            }
        }
        Ok(ids.iter().map(|id| found.remove(*id)).collect())
    }

    fn node_count(&mut self) -> Result<u64, StoreError> {
        self.check_stop()?;
        if let Some(count) = self.node_count {
            return Ok(count);
        }
        let pool = self.build.pool().clone();
        let build = self.build.build_id().to_owned();
        let count: i64 = self
            .handle
            .block_on(
                sqlx::query_scalar(
                    "SELECT count(*) FROM deepwiki_build.wiki_nodes WHERE build_id = $1",
                )
                .bind(&build)
                .fetch_one(&pool),
            )
            .map_err(database)?;
        let count = u64::try_from(count).unwrap_or(0);
        self.node_count = Some(count);
        Ok(count)
    }

    fn count_phrase_matches(&mut self, query: &str) -> Result<u64, StoreError> {
        self.check_stop()?;
        if crate::graph::pystr::strip(query).is_empty() {
            return Ok(0);
        }
        let pool = self.build.pool().clone();
        let build = self.build.build_id().to_owned();
        let count: i64 = self
            .handle
            .block_on(
                sqlx::query_scalar(&format!(
                    "SELECT count(*) FROM deepwiki_build.wiki_nodes n \
                     WHERE n.build_id = $1 \
                       AND n.fts @@ phraseto_tsquery('deepwiki_porter', {FOLD})"
                ))
                .bind(&build)
                .bind(query)
                .fetch_one(&pool),
            )
            .map_err(database)?;
        Ok(u64::try_from(count).unwrap_or(0))
    }

    fn search_lexical(
        &mut self,
        query: &str,
        path_prefix: Option<&str>,
        limit: usize,
    ) -> Result<Vec<SearchHit>, StoreError> {
        self.check_stop()?;
        if crate::graph::pystr::strip(query).is_empty() {
            return Ok(Vec::new());
        }
        let pool = self.build.pool().clone();
        let build = self.build.build_id().to_owned();
        // The query's lexemes: what `plainto_tsquery` ANDs, each once.
        let terms: Vec<String> = self
            .handle
            .block_on(
                sqlx::query_scalar(&format!(
                    "SELECT lexeme FROM unnest(to_tsvector('deepwiki_porter', {}))",
                    FOLD.replace("$2", "$1")
                ))
                .bind(query)
                .fetch_all(&pool),
            )
            .map_err(database)?;
        if terms.is_empty() {
            // An empty tsquery matches nothing.
            return Ok(Vec::new());
        }
        let frequencies = self.frequencies(&terms)?;
        let corpus = self.corpus()?;
        let (start, end) = prefix_range(path_prefix);
        let rows = self
            .handle
            .block_on(
                sqlx::query(&format!(
                    "WITH m AS ( \
                         SELECT n.node_id, n.rel_path, n.symbol_type, n.fts, \
                             coalesce((SELECT sum(coalesce(array_length(u.positions, 1), 1)) \
                                 FROM unnest(n.fts) AS u(lexeme, positions, weights)), 0)::float8 \
                                 AS dl \
                         FROM deepwiki_build.wiki_nodes n \
                         WHERE n.build_id = $1 \
                           AND n.fts @@ plainto_tsquery('deepwiki_porter', {FOLD}) \
                           AND ($3::text IS NULL \
                                OR (n.rel_path COLLATE \"C\" >= $3 AND n.rel_path COLLATE \"C\" < $4))), \
                     q AS (SELECT * FROM unnest($5::text[], $6::float8[]) AS q(term, df)), \
                     s AS ( \
                         SELECT m.node_id, sum( \
                             ln(1.0 + ($7::float8 - q.df + 0.5) / (q.df + 0.5)) \
                             * (coalesce(array_length(u.positions, 1), 1) * 2.2) \
                             / (coalesce(array_length(u.positions, 1), 1) \
                                + 1.2 * (0.25 + 0.75 * m.dl / $8::float8)) \
                         ) AS score \
                         FROM m CROSS JOIN LATERAL unnest(m.fts) AS u(lexeme, positions, weights) \
                         JOIN q ON q.term = u.lexeme \
                         GROUP BY m.node_id) \
                     SELECT m.node_id, m.rel_path, m.symbol_type, \
                         (-coalesce(s.score, 0.0))::float8 AS fts_rank \
                     FROM m LEFT JOIN s ON s.node_id = m.node_id \
                     ORDER BY fts_rank, m.node_id COLLATE \"C\" \
                     LIMIT $9"
                ))
                .bind(&build)
                .bind(query)
                .bind(start)
                .bind(end)
                .bind(&terms)
                .bind(&frequencies)
                .bind(corpus.documents)
                .bind(corpus.average_length)
                .bind(i64::try_from(limit).unwrap_or(i64::MAX))
                .fetch_all(&pool),
            )
            .map_err(database)?;
        rows.iter()
            .map(|row| {
                let rank: f64 = row.try_get("fts_rank").map_err(database)?;
                Ok(SearchHit {
                    node_id: row.try_get("node_id").map_err(database)?,
                    rel_path: row.try_get("rel_path").map_err(database)?,
                    symbol_type: row.try_get("symbol_type").map_err(database)?,
                    fts_rank: Some(rank),
                    score_norm: Some(text::score_norm(rank)),
                    vec_distance: None,
                })
            })
            .collect()
    }

    fn get_embeddings(&mut self, ids: &[&str]) -> Result<Vec<Option<Vec<f64>>>, StoreError> {
        self.check_stop()?;
        let pool = self.build.pool().clone();
        let build = self.build.build_id().to_owned();
        let mut found: HashMap<String, Vec<f64>> = HashMap::with_capacity(ids.len());
        for round in ids.chunks(LOOKUP_ROUND) {
            let rows = self
                .handle
                .block_on(
                    sqlx::query(
                        "SELECT node_id, embedding::real[] AS embedding \
                         FROM deepwiki_build.wiki_node_embeddings \
                         WHERE build_id = $1 AND node_id = ANY($2)",
                    )
                    .bind(&build)
                    .bind(round)
                    .fetch_all(&pool),
                )
                .map_err(database)?;
            for row in rows {
                let vector: Vec<f32> = row.try_get("embedding").map_err(database)?;
                found.insert(
                    row.try_get("node_id").map_err(database)?,
                    vector.into_iter().map(f64::from).collect(),
                );
            }
        }
        Ok(ids.iter().map(|id| found.remove(*id)).collect())
    }

    fn search_dense(
        &mut self,
        embedding: &[f64],
        k: usize,
        path_prefix: Option<&str>,
    ) -> Result<Vec<SearchHit>, StoreError> {
        self.check_stop()?;
        if embedding.is_empty() {
            return Ok(Vec::new());
        }
        let pool = self.build.pool().clone();
        let build = self.build.build_id().to_owned();
        let (start, end) = prefix_range(path_prefix);
        let rows = self
            .handle
            .block_on(
                sqlx::query(
                    "SELECT x.node_id, x.rel_path, x.symbol_type, x.distance FROM ( \
                         SELECT n.node_id, n.rel_path, n.symbol_type, \
                             e.embedding <-> $2::text::vector AS distance \
                         FROM deepwiki_build.wiki_node_embeddings e \
                         JOIN deepwiki_build.wiki_nodes n \
                           ON n.build_id = e.build_id AND n.node_id = e.node_id \
                         WHERE e.build_id = $1 \
                         ORDER BY distance, n.node_id COLLATE \"C\" \
                         LIMIT $3) AS x \
                     WHERE $4::text IS NULL \
                        OR (x.rel_path COLLATE \"C\" >= $4 AND x.rel_path COLLATE \"C\" < $5) \
                     ORDER BY x.distance, x.node_id COLLATE \"C\"",
                )
                .bind(&build)
                .bind(super::search::vector_literal(embedding))
                .bind(i64::try_from(k).unwrap_or(i64::MAX))
                .bind(start)
                .bind(end)
                .fetch_all(&pool),
            )
            .map_err(database)?;
        rows.iter()
            .map(|row| {
                Ok(SearchHit {
                    node_id: row.try_get("node_id").map_err(database)?,
                    rel_path: row.try_get("rel_path").map_err(database)?,
                    symbol_type: row.try_get("symbol_type").map_err(database)?,
                    fts_rank: None,
                    score_norm: None,
                    vec_distance: Some(row.try_get("distance").map_err(database)?),
                })
            })
            .collect()
    }

    fn set_hubs(&mut self, hubs: &[&str]) -> Result<(), StoreError> {
        self.check_stop()?;
        self.hubs.extend(hubs.iter().map(|hub| (*hub).to_owned()));
        Ok(())
    }

    /// Returns the number of edge ROWS Phase 2 persisted, which its stats
    /// report as `edges_persisted` (Python's `edge_count()` over a table
    /// without a uniqueness rule). The staged table holds them collapsed
    /// onto `(source, target, rel_type)`, as the publish would.
    fn replace_edges(
        &mut self,
        rows: &mut dyn Iterator<Item = EdgeRow>,
    ) -> Result<u64, StoreError> {
        self.check_stop()?;
        let mut offered = 0_u64;
        let edges = collapse_edges(rows.map(|row| {
            offered += 1;
            IndexEdge::from_row(row)
        }));
        self.handle
            .block_on(self.build.replace_edges(&edges))
            .map_err(database)?;
        Ok(offered)
    }

    fn set_meta(&mut self, key: &str, value: &Value) -> Result<(), StoreError> {
        self.check_stop()?;
        self.meta.push((key.to_owned(), value.clone()));
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_prefix_is_a_c_collated_range() {
        assert_eq!(prefix_range(None), (None, None));
        assert_eq!(prefix_range(Some("")), (None, None));
        assert_eq!(
            prefix_range(Some("src/app")),
            (Some("src/app/".to_owned()), Some("src/app0".to_owned()))
        );
        assert_eq!(
            prefix_range(Some("src/app/")),
            (Some("src/app/".to_owned()), Some("src/app0".to_owned()))
        );
    }
}
