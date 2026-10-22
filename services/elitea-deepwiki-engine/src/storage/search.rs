//! The read path: `storage/postgres.py`'s searches and `storage/base.py`'s
//! frozen weighted RRF.
//!
//! Each branch is held to the parity `postgres.py` holds, with the same SQL:
//!
//! | branch | score field | parity |
//! | --- | --- | --- |
//! | dense | `vec_distance` (cosine distance `1 - similarity`, lower is better) | rank only: elitea-vector's HNSW search (Qdrant), not pgvector's exact L2 scan |
//! | fts | `fts_rank` (negated, lower is better), `score_norm` | match set and order: `plainto_tsquery` over the folded text, ranked by 'fts' statistics |
//! | fused | `combined_score` (higher is better) | the frozen weighted RRF over fts + dense |
//!
//! The query text is folded with the expression migration 0001 folds the
//! stored text with (`regexp_replace(text, '[^[:alnum:]]+', ' ', 'g')`):
//! PostgreSQL's parser keeps `self.connection.execute` whole as a `host`
//! token, and FTS5's `unicode61` split it. Both sides must fold, or terms
//! are indexed that no query can name.
//!
//! `plainto_tsquery`, not `websearch_to_tsquery`: the latter reads `or`,
//! `-` and quotes as operators, so a question containing "or" would become
//! a disjunction.
//!
//! Every [`IndexReader`] method runs in one `REPEATABLE READ READ ONLY`
//! transaction, so a search that takes several statements (match, then
//! statistics, then metadata) reads one snapshot. Python ran them in
//! autocommit statements; a publish between two of them could mix two
//! indexes in one answer.
//!
//! The dense vectors are in elitea-vector ([`crate::storage::dense`]). The
//! dense branch reads the wiki's `active_generation`, embedding model and
//! dimension in the snapshot, searches only that generation, and keeps the
//! hits whose node the snapshot holds, so it ranks the vectors of the
//! nodes it answers with. The distance changed from L2 to cosine with the
//! move; for the unit-length vectors embedding models answer, both rank
//! alike (`|a-b|^2 = 2 - 2cos`). When elitea-vector cannot be reached the
//! branch is skipped with a warning and the search answers lexically.

use crate::storage::dense::{DenseIndex, Space};
use crate::storage::text::{self, BRANCH_FTS};
use crate::storage::{Result, StorageError, WikiKey};
use indexmap::IndexMap;
use sqlx::Row;
use sqlx::postgres::{PgConnection, PgPool};
use std::collections::HashMap;

/// FTS weight of the frozen RRF (`unified_db.search_hybrid`).
pub const DEFAULT_FTS_WEIGHT: f64 = 0.4;
/// Vector weight of the frozen RRF.
pub const DEFAULT_VEC_WEIGHT: f64 = 0.6;
/// The RRF constant `k`.
pub const RRF_CONSTANT: f64 = 60.0;
/// FTS candidate pool.
pub const DEFAULT_FTS_POOL: usize = 30;
/// Dense candidate pool.
pub const DEFAULT_VEC_POOL: usize = 30;

/// The opening statement of every read.
pub(crate) const READ_SNAPSHOT: &str = "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY";

/// A branch's score fields. Only the fields of the branch (and, for a
/// fused hit, `combined_score`) are set: Python's `Hit.scores` dict holds
/// exactly those keys.
#[derive(Debug, Clone, Copy, Default, PartialEq)]
pub struct Scores {
    pub fts_rank: Option<f64>,
    pub score_norm: Option<f64>,
    pub vec_distance: Option<f64>,
    pub combined_score: Option<f64>,
}

impl Scores {
    /// `(key, value)` of each set field, in the order Python's dict holds
    /// them (`{**source.scores, "combined_score": …}`).
    #[must_use]
    pub fn entries(&self) -> Vec<(&'static str, f64)> {
        [
            ("fts_rank", self.fts_rank),
            ("score_norm", self.score_norm),
            ("vec_distance", self.vec_distance),
            ("combined_score", self.combined_score),
        ]
        .into_iter()
        .filter_map(|(key, value)| value.map(|v| (key, v)))
        .collect()
    }
}

/// One ranked result (`base.Hit`).
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Hit {
    pub node_id: String,
    pub rel_path: String,
    pub symbol_name: String,
    pub symbol_type: String,
    pub scores: Scores,
}

/// The fusion parameters of `rrf_fuse`.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Fusion {
    pub fts_weight: f64,
    pub vec_weight: f64,
    pub rrf_constant: f64,
    pub limit: usize,
}

impl Default for Fusion {
    fn default() -> Self {
        Self {
            fts_weight: DEFAULT_FTS_WEIGHT,
            vec_weight: DEFAULT_VEC_WEIGHT,
            rrf_constant: RRF_CONSTANT,
            limit: 20,
        }
    }
}

/// The parameters of `search_hybrid`.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Hybrid {
    pub limit: usize,
    pub fts_weight: f64,
    pub vec_weight: f64,
    pub fts_pool: usize,
    pub vec_pool: usize,
}

impl Default for Hybrid {
    fn default() -> Self {
        Self {
            limit: 20,
            fts_weight: DEFAULT_FTS_WEIGHT,
            vec_weight: DEFAULT_VEC_WEIGHT,
            fts_pool: DEFAULT_FTS_POOL,
            vec_pool: DEFAULT_VEC_POOL,
        }
    }
}

/// A 1-based rank as `f64`, exactly (ranks are far below 2^32).
fn rank(position: usize) -> f64 {
    f64::from(u32::try_from(position).unwrap_or(u32::MAX))
}

/// Weighted reciprocal-rank fusion (`base.rrf_fuse`), frozen:
/// `w_fts / (k + pos_fts) + w_vec / (k + pos_vec)`, positions from 1.
///
/// Only FTS and dense are fused; the standalone BM25 index never was. A
/// hit keeps the FTS hit's fields when it is in both lists. Equal scores
/// keep first-seen order (Python's stable `sort(reverse=True)`), so the
/// fused order of tied documents follows their component positions, as in
/// the legacy engine.
#[must_use]
pub fn rrf_fuse(fts: &[Hit], dense: &[Hit], fusion: &Fusion) -> Vec<Hit> {
    let mut merged: IndexMap<&str, (f64, &Hit)> = IndexMap::new();
    for (index, hit) in fts.iter().enumerate() {
        let add = fusion.fts_weight / (fusion.rrf_constant + rank(index + 1));
        let entry = merged.entry(hit.node_id.as_str()).or_insert((0.0, hit));
        entry.0 += add;
        // `merged[hit.node_id] = hit`: the last FTS occurrence wins.
        entry.1 = hit;
    }
    for (index, hit) in dense.iter().enumerate() {
        let add = fusion.vec_weight / (fusion.rrf_constant + rank(index + 1));
        // `merged.setdefault(...)`: an FTS hit keeps its fields.
        let entry = merged.entry(hit.node_id.as_str()).or_insert((0.0, hit));
        entry.0 += add;
    }
    let mut fused: Vec<Hit> = merged
        .into_iter()
        .map(|(node_id, (score, source))| Hit {
            node_id: node_id.to_owned(),
            rel_path: source.rel_path.clone(),
            symbol_name: source.symbol_name.clone(),
            symbol_type: source.symbol_type.clone(),
            scores: Scores {
                combined_score: Some(score),
                ..source.scores
            },
        })
        .collect();
    // Stable, descending; a NaN cannot occur (finite weights and ranks).
    fused.sort_by(|a, b| {
        let key = |hit: &Hit| hit.scores.combined_score.unwrap_or(f64::NEG_INFINITY);
        key(b).total_cmp(&key(a))
    });
    fused.truncate(fusion.limit);
    fused
}

/// Searches over one wiki's published index (`PostgresBackend`'s read
/// half), within one project: every statement filters by the
/// [`WikiKey`]'s `(project_id, wiki_id)`.
#[derive(Debug, Clone)]
pub struct IndexReader {
    pool: PgPool,
    key: WikiKey,
    dense: Option<DenseIndex>,
}

/// One branch's statistics (`wiki_bm25_meta`), for health and reports.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct BranchStats {
    pub doc_count: i64,
    pub avgdl: f64,
    pub k1: f64,
    pub b: f64,
}

/// `PostgresBackend.stats()`.
#[derive(Debug, Clone, PartialEq)]
pub struct IndexStats {
    pub node_count: i64,
    pub vector_count: i64,
    pub branches: Vec<(String, BranchStats)>,
}

impl IndexReader {
    /// The reader of one project's wiki.
    #[must_use]
    pub fn new(pool: PgPool, key: WikiKey) -> Self {
        Self {
            pool,
            key,
            dense: None,
        }
    }

    /// The dense store the dense branch searches. Without one the dense
    /// branch answers nothing (lexical only).
    #[must_use]
    pub fn with_dense(mut self, dense: Option<DenseIndex>) -> Self {
        self.dense = dense;
        self
    }

    /// The dense store, for the adapter's own reads.
    pub(crate) fn dense(&self) -> Option<&DenseIndex> {
        self.dense.as_ref()
    }

    /// The wiki this reader is scoped to.
    #[must_use]
    pub fn wiki_id(&self) -> &str {
        self.key.wiki_id()
    }

    /// The project and wiki this reader is scoped to.
    #[must_use]
    pub fn key(&self) -> &WikiKey {
        &self.key
    }

    /// The pool, for the adapter's own statements.
    pub(crate) fn pool(&self) -> &PgPool {
        &self.pool
    }

    /// Lexical search: hits carry `fts_rank` and `score_norm`.
    ///
    /// # Errors
    ///
    /// [`crate::storage::StorageError::Database`].
    pub async fn search_fts(&self, query: &str, limit: usize) -> Result<Vec<Hit>> {
        let mut tx = self.pool.begin_with(READ_SNAPSHOT).await?;
        let hits = search_fts(&mut tx, &self.key, query, limit).await?;
        tx.commit().await?;
        Ok(hits)
    }

    /// Dense KNN over the wiki's active generation: hits carry
    /// `vec_distance` (cosine distance).
    ///
    /// # Errors
    ///
    /// [`crate::storage::StorageError::Database`];
    /// [`crate::storage::StorageError::Vector`] for a vector whose dimension
    /// differs from the stored ones.
    pub async fn search_dense(&self, embedding: &[f64], k: usize) -> Result<Vec<Hit>> {
        let mut tx = self.pool.begin_with(READ_SNAPSHOT).await?;
        let hits =
            search_dense(&mut tx, &self.key, self.dense.as_ref(), embedding, k, None).await?;
        tx.commit().await?;
        Ok(hits)
    }

    /// FTS + dense, fused: hits carry `combined_score`. An absent or empty
    /// embedding skips the dense branch (`if embedding`).
    ///
    /// # Errors
    ///
    /// [`crate::storage::StorageError::Database`].
    pub async fn search_hybrid(
        &self,
        query: &str,
        embedding: Option<&[f64]>,
        params: &Hybrid,
    ) -> Result<Vec<Hit>> {
        let mut tx = self.pool.begin_with(READ_SNAPSHOT).await?;
        let hits = search_hybrid(
            &mut tx,
            &self.key,
            self.dense.as_ref(),
            query,
            embedding,
            params,
            None,
        )
        .await?;
        tx.commit().await?;
        Ok(hits)
    }

    /// Row counts and the statistics of each branch. `vector_count` is the
    /// active generation's point count in elitea-vector (0 without a dense
    /// store, without vectors, or when the store cannot be reached).
    ///
    /// # Errors
    ///
    /// [`crate::storage::StorageError::Database`].
    pub async fn stats(&self) -> Result<IndexStats> {
        let mut tx = self.pool.begin_with(READ_SNAPSHOT).await?;
        let node_count: i64 = sqlx::query_scalar(
            "SELECT count(*) FROM wiki_nodes WHERE wiki_id = $1 AND project_id = $2",
        )
        .bind(self.key.wiki_id())
        .bind(self.key.project_id())
        .fetch_one(&mut *tx)
        .await?;
        let active = active_vectors(&mut tx, &self.key).await?;
        let vector_count = match (&self.dense, &active) {
            (Some(store), Some(active)) => {
                match store
                    .count(&self.key, &active.generation, &active.space)
                    .await
                {
                    Ok(count) => i64::try_from(count).unwrap_or(i64::MAX),
                    Err(error) => {
                        tracing::warn!(wiki = self.key.wiki_id(), %error, "could not count the wiki's vectors");
                        0
                    }
                }
            }
            _ => 0,
        };
        let rows = sqlx::query(
            "SELECT branch, doc_count, avgdl, k1, b FROM wiki_bm25_meta \
             WHERE wiki_id = $1 AND project_id = $2 ORDER BY branch",
        )
        .bind(self.key.wiki_id())
        .bind(self.key.project_id())
        .fetch_all(&mut *tx)
        .await?;
        let mut branches = Vec::with_capacity(rows.len());
        for row in rows {
            branches.push((
                row.try_get("branch")?,
                BranchStats {
                    doc_count: i64::from(row.try_get::<i32, _>("doc_count")?),
                    avgdl: row.try_get("avgdl")?,
                    k1: row.try_get("k1")?,
                    b: row.try_get("b")?,
                },
            ));
        }
        tx.commit().await?;
        Ok(IndexStats {
            node_count,
            vector_count,
            branches,
        })
    }
}

/// `PostgresBackend._bm25_scores`: every document matching `terms`, scored
/// in one statement, the legacy formula term for term (including the
/// query-term multiplicity, `Counter(terms)`).
pub(crate) async fn bm25_scores(
    tx: &mut PgConnection,
    key: &WikiKey,
    branch: &str,
    terms: &[String],
) -> Result<HashMap<String, f64>> {
    if terms.is_empty() {
        return Ok(HashMap::new());
    }
    let meta = sqlx::query(
        "SELECT doc_count, avgdl, k1, b FROM wiki_bm25_meta \
         WHERE wiki_id = $1 AND branch = $2 AND project_id = $3",
    )
    .bind(key.wiki_id())
    .bind(branch)
    .bind(key.project_id())
    .fetch_optional(&mut *tx)
    .await?;
    let Some(meta) = meta else {
        return Ok(HashMap::new());
    };
    let doc_count: i32 = meta.try_get("doc_count")?;
    if doc_count == 0 {
        return Ok(HashMap::new());
    }
    let avgdl: f64 = meta.try_get("avgdl")?;
    let avgdl = if avgdl > 0.0 { avgdl } else { 1.0 };
    let k1: f64 = meta.try_get("k1")?;
    let b: f64 = meta.try_get("b")?;

    let mut query_tf: IndexMap<&str, f64> = IndexMap::new();
    for term in terms {
        *query_tf.entry(term.as_str()).or_insert(0.0) += 1.0;
    }
    let keys: Vec<&str> = query_tf.keys().copied().collect();
    let values: Vec<f64> = query_tf.values().copied().collect();

    let rows = sqlx::query(
        "SELECT d.node_id, \
                sum( \
                    ln(1.0 + ($1::float8 - t.df + 0.5) / (t.df + 0.5)) \
                    * ((p.tf * ($2::float8 + 1.0)) \
                       / (p.tf + $3::float8 \
                          * (1.0 - $4::float8 \
                             + $5::float8 * d.length / $6::float8))) \
                    * q.query_tf \
                ) AS score \
         FROM wiki_bm25_postings p \
         JOIN wiki_bm25_terms t \
           ON t.project_id = p.project_id \
          AND t.wiki_id = p.wiki_id \
          AND t.branch  = p.branch \
          AND t.term    = p.term \
         JOIN wiki_bm25_docs d \
           ON d.project_id = p.project_id \
          AND d.wiki_id = p.wiki_id \
          AND d.branch  = p.branch \
          AND d.doc_idx = p.doc_idx \
         JOIN unnest($7::text[], $8::float8[]) AS q(term, query_tf) \
           ON q.term = p.term \
         WHERE p.wiki_id = $9 AND p.branch = $10 AND p.project_id = $11 AND t.df > 0 \
         GROUP BY d.node_id",
    )
    .bind(f64::from(doc_count))
    .bind(k1)
    .bind(k1)
    .bind(b)
    .bind(b)
    .bind(avgdl)
    .bind(&keys)
    .bind(&values)
    .bind(key.wiki_id())
    .bind(branch)
    .bind(key.project_id())
    .fetch_all(&mut *tx)
    .await?;
    let mut scores = HashMap::with_capacity(rows.len());
    for row in rows {
        scores.insert(row.try_get("node_id")?, row.try_get("score")?);
    }
    Ok(scores)
}

/// `_node_metadata` + `_hits`: attach path, name and type to scored ids.
async fn hits(
    tx: &mut PgConnection,
    key: &WikiKey,
    scored: Vec<(String, Scores)>,
) -> Result<Vec<Hit>> {
    if scored.is_empty() {
        return Ok(Vec::new());
    }
    let ids: Vec<&str> = scored.iter().map(|(id, _)| id.as_str()).collect();
    let rows = sqlx::query(
        "SELECT node_id, rel_path, symbol_name, symbol_type \
         FROM wiki_nodes WHERE wiki_id = $1 AND node_id = ANY($2) AND project_id = $3",
    )
    .bind(key.wiki_id())
    .bind(&ids)
    .bind(key.project_id())
    .fetch_all(&mut *tx)
    .await?;
    let mut metadata: HashMap<String, (String, String, String)> =
        HashMap::with_capacity(rows.len());
    for row in rows {
        metadata.insert(
            row.try_get("node_id")?,
            (
                row.try_get("rel_path")?,
                row.try_get("symbol_name")?,
                row.try_get("symbol_type")?,
            ),
        );
    }
    Ok(scored
        .into_iter()
        .map(|(node_id, scores)| {
            let (rel_path, symbol_name, symbol_type) =
                metadata.remove(&node_id).unwrap_or_default();
            Hit {
                node_id,
                rel_path,
                symbol_name,
                symbol_type,
                scores,
            }
        })
        .collect())
}

/// `PostgresBackend.search_fts`: conjunctive match, ranked by the 'fts'
/// statistics, negated to FTS5's sign, ties broken by node id.
pub(crate) async fn search_fts(
    tx: &mut PgConnection,
    key: &WikiKey,
    query: &str,
    limit: usize,
) -> Result<Vec<Hit>> {
    // `not query.strip()`: Python's whitespace, not Rust's.
    if crate::graph::pystr::strip(query).is_empty() {
        return Ok(Vec::new());
    }
    let matched: Vec<String> = sqlx::query_scalar(
        "SELECT n.node_id \
         FROM wiki_nodes n \
         WHERE n.wiki_id = $1 AND n.project_id = $3 \
           AND n.fts @@ plainto_tsquery( \
                   'deepwiki_porter', regexp_replace($2, '[^[:alnum:]]+', ' ', 'g') \
               )",
    )
    .bind(key.wiki_id())
    .bind(query)
    .bind(key.project_id())
    .fetch_all(&mut *tx)
    .await?;
    if matched.is_empty() {
        return Ok(Vec::new());
    }
    // The query's lexemes under the same configuration: what
    // plainto_tsquery ANDs (a tsquery cannot be unnested).
    let terms: Option<Vec<String>> = sqlx::query_scalar(
        "SELECT array_agg(lexeme) FROM unnest(to_tsvector( \
           'deepwiki_porter', regexp_replace($1, '[^[:alnum:]]+', ' ', 'g') \
         ))",
    )
    .bind(query)
    .fetch_one(&mut *tx)
    .await?;
    let scores = bm25_scores(tx, key, BRANCH_FTS, &terms.unwrap_or_default()).await?;

    let mut ranked: Vec<(String, Scores)> = matched
        .into_iter()
        .map(|node_id| {
            // Negated: FTS5's rank, more negative is better.
            let rank = -scores.get(&node_id).copied().unwrap_or(0.0);
            let scores = Scores {
                fts_rank: Some(rank),
                score_norm: Some(text::score_norm(rank)),
                ..Scores::default()
            };
            (node_id, scores)
        })
        .collect();
    // Ascending rank, ties on node id (code-point order, as Python's str
    // comparison), so the order is total.
    ranked.sort_by(|a, b| {
        let rank = |s: &Scores| s.fts_rank.unwrap_or(0.0);
        rank(&a.1)
            .total_cmp(&rank(&b.1))
            .then_with(|| a.0.cmp(&b.0))
    });
    ranked.truncate(limit);
    hits(tx, key, ranked).await
}

/// The vectors a wiki's published index searches: its active generation
/// and the space (model and dimension) the generation was written in, read
/// in the caller's snapshot.
#[derive(Debug, Clone, PartialEq)]
pub(crate) struct ActiveVectors {
    pub generation: String,
    pub model: String,
    pub space: Space,
}

/// The wiki's [`ActiveVectors`], `None` when its last publish wrote no
/// vector (or it has no published index).
///
/// # Errors
///
/// [`StorageError::Database`]; [`StorageError::Vector`] for a recorded
/// dimension elitea-vector cannot hold.
pub(crate) async fn active_vectors(
    tx: &mut PgConnection,
    key: &WikiKey,
) -> Result<Option<ActiveVectors>> {
    let row: Option<(Option<String>, Option<String>, Option<i32>)> = sqlx::query_as(
        "SELECT active_generation, embedding_model, embedding_dim FROM wikis \
         WHERE project_id = $1 AND wiki_id = $2",
    )
    .bind(key.project_id())
    .bind(key.wiki_id())
    .fetch_optional(&mut *tx)
    .await?;
    let Some((Some(generation), Some(model), Some(dimension))) = row else {
        return Ok(None);
    };
    let dimension = usize::try_from(dimension).unwrap_or(0);
    let space = Space::new(&model, dimension).map_err(|e| StorageError::Vector(e.to_string()))?;
    Ok(Some(ActiveVectors {
        generation,
        model,
        space,
    }))
}

/// `PostgresBackend.search_dense`, over elitea-vector: the `k` nearest
/// points of the wiki's active generation, best first, as hits with
/// `vec_distance` = `1 - cosine similarity`.
///
/// With `expected_model`, the wiki's recorded embedding model is read in the
/// same snapshot as the active generation. A wiki republished with another
/// model between the caller's model choice and this search has vectors from
/// another space; the search then answers
/// [`crate::storage::StorageError::EmbeddingModelChanged`] instead of
/// ranking by a meaningless distance. A wiki without vectors has nothing to
/// compare, so there is nothing to refuse.
///
/// Without a dense store, or when the store cannot be reached, the branch
/// answers nothing (the latter with a warning): the caller's search goes on
/// lexically. Hits whose node the snapshot does not hold (a generation
/// being replaced) are dropped.
pub(crate) async fn search_dense(
    tx: &mut PgConnection,
    key: &WikiKey,
    dense: Option<&DenseIndex>,
    embedding: &[f64],
    k: usize,
    expected_model: Option<&str>,
) -> Result<Vec<Hit>> {
    let Some(active) = active_vectors(tx, key).await? else {
        return Ok(Vec::new());
    };
    if let Some(expected) = expected_model {
        let stored = active.model.trim();
        if !stored.is_empty() && stored != expected {
            return Err(StorageError::EmbeddingModelChanged {
                wiki_id: key.wiki_id().to_owned(),
                stored: stored.to_owned(),
                expected: expected.to_owned(),
            });
        }
    }
    let Some(store) = dense else {
        return Ok(Vec::new());
    };
    #[allow(clippy::cast_possible_truncation)] // f64 -> f32, as pgvector's float4
    let query: Vec<f32> = embedding.iter().map(|v| *v as f32).collect();
    let found = match store
        .search(key, &active.generation, &active.space, &query, k)
        .await
    {
        Ok(found) => found,
        Err(error) if error.unavailable => {
            tracing::warn!(
                wiki = key.wiki_id(),
                project = key.project_id(),
                %error,
                "elitea-vector is unavailable; answering with the lexical search only"
            );
            return Ok(Vec::new());
        }
        Err(error) => return Err(StorageError::Vector(error.to_string())),
    };
    let ids: Vec<&str> = found.iter().map(|hit| hit.node_id.as_str()).collect();
    let held: std::collections::HashSet<String> = sqlx::query_scalar(
        "SELECT node_id FROM wiki_nodes WHERE project_id = $1 AND wiki_id = $2 AND node_id = ANY($3)",
    )
    .bind(key.project_id())
    .bind(key.wiki_id())
    .bind(&ids)
    .fetch_all(&mut *tx)
    .await?
    .into_iter()
    .collect();
    let scored: Vec<(String, Scores)> = found
        .into_iter()
        .filter(|hit| held.contains(&hit.node_id))
        .map(|hit| {
            let distance = hit.distance();
            (
                hit.node_id,
                Scores {
                    vec_distance: Some(distance),
                    ..Scores::default()
                },
            )
        })
        .collect();
    hits(tx, key, scored).await
}

/// `PostgresBackend.search_hybrid`.
pub(crate) async fn search_hybrid(
    tx: &mut PgConnection,
    key: &WikiKey,
    dense: Option<&DenseIndex>,
    query: &str,
    embedding: Option<&[f64]>,
    params: &Hybrid,
    expected_model: Option<&str>,
) -> Result<Vec<Hit>> {
    let fts = search_fts(tx, key, query, params.fts_pool).await?;
    let dense = match embedding {
        Some(vector) if !vector.is_empty() => {
            search_dense(tx, key, dense, vector, params.vec_pool, expected_model).await?
        }
        _ => Vec::new(),
    };
    Ok(rrf_fuse(
        &fts,
        &dense,
        &Fusion {
            fts_weight: params.fts_weight,
            vec_weight: params.vec_weight,
            rrf_constant: RRF_CONSTANT,
            limit: params.limit,
        },
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn hit(id: &str, scores: Scores) -> Hit {
        Hit {
            node_id: id.into(),
            rel_path: format!("{id}.py"),
            scores,
            ..Hit::default()
        }
    }

    #[test]
    fn rrf_is_the_frozen_weighted_formula() {
        let fts_scores = Scores {
            fts_rank: Some(-2.0),
            score_norm: Some(0.88),
            ..Scores::default()
        };
        let dense_scores = Scores {
            vec_distance: Some(0.5),
            ..Scores::default()
        };
        let fts = [hit("a", fts_scores), hit("b", fts_scores)];
        let dense = [hit("b", dense_scores), hit("c", dense_scores)];
        let fused = rrf_fuse(&fts, &dense, &Fusion::default());
        let ids: Vec<&str> = fused.iter().map(|h| h.node_id.as_str()).collect();
        assert_eq!(ids, ["b", "c", "a"]);
        let b = 0.4 / 62.0 + 0.6 / 61.0;
        assert_eq!(fused[0].scores.combined_score, Some(b));
        // An FTS hit keeps the FTS fields.
        assert_eq!(fused[0].scores.fts_rank, Some(-2.0));
        assert_eq!(fused[0].scores.vec_distance, None);
        assert_eq!(fused[1].scores.vec_distance, Some(0.5));
        assert_eq!(fused[2].scores.combined_score, Some(0.4 / 61.0));
    }

    #[test]
    fn rrf_ties_keep_first_seen_order_and_limit_applies() {
        let none = Scores::default();
        let dense = [hit("x", none), hit("y", none)];
        let fts = [hit("y", none), hit("x", none)];
        // x: .4/62 + .6/61, y: .4/61 + .6/62 — distinct; check the limit.
        let fused = rrf_fuse(
            &fts,
            &dense,
            &Fusion {
                limit: 1,
                ..Fusion::default()
            },
        );
        assert_eq!(fused.len(), 1);
        assert_eq!(fused[0].node_id, "x");
        // Equal weights make both equal: first-seen (FTS) order is kept.
        let equal = rrf_fuse(
            &fts,
            &dense,
            &Fusion {
                fts_weight: 0.5,
                vec_weight: 0.5,
                ..Fusion::default()
            },
        );
        let ids: Vec<&str> = equal.iter().map(|h| h.node_id.as_str()).collect();
        assert_eq!(ids, ["y", "x"]);
    }

    #[test]
    fn score_entries_follow_python_dict_order() {
        let scores = Scores {
            fts_rank: Some(-1.0),
            score_norm: Some(0.7),
            combined_score: Some(0.01),
            ..Scores::default()
        };
        let keys: Vec<&str> = scores.entries().into_iter().map(|(k, _)| k).collect();
        assert_eq!(keys, ["fts_rank", "score_norm", "combined_score"]);
    }
}
