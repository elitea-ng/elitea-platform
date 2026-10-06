//! What the query tools read: the [`IndexStore`] trait, its PostgreSQL
//! implementation ([`PgIndex`], the live path), and a recorded one
//! ([`ReplayIndex`], the parity gate).
//!
//! ADR-0026 decision 5: `ask`, `deep_research` and `resolve_wiki` read
//! ONLY PostgreSQL. The Python tools read the scratch `.wiki.db` in three
//! places the PostgreSQL adapter never reached: `StorageQueryService`'s
//! `storage.conn` SQL (`_name_rows`, `_scan_rows`, `_connection_counts`)
//! and `search_fts5`, and the research tools' `_search_unified_db_fts`,
//! `_get_unified_db_relationships` and `_get_code_from_unified_db`, which
//! open the file by path. On a query replica (no file) the first group
//! failed with `AttributeError` and the second answered "Code graph not
//! available". Each is a method here, with the same SQL over the ADR-0022
//! tables.
//!
//! The method docs name the Python statement each one replaces. Two
//! translations are not exact, and the parity gate reports them:
//!
//! * FTS5 `MATCH` becomes the folded `tsvector` match ranked by the
//!   `'fts'` BM25 statistics (`storage::search`). `search_fts` is the
//!   conjunctive form (`plainto_tsquery`, FTS5's implicit AND); a bareword
//!   FTS5 reads as a phrase (`handle_search`) is a conjunction here.
//!   `search_fts_any` is the disjunctive form `_search_unified_db_fts`
//!   built (`kw1 OR kw2`). Ties are broken by node id (FTS5: rowid).
//! * The published edges hold no annotations (`metadata` is `{}`, as
//!   `publish.py` wrote it) and parallel edges are collapsed, so the
//!   `via` anchors and the duplicate edges the `.wiki.db` showed are absent.
//!
//! Row order without an `ORDER BY` is the table's (`SQLite`: rowid;
//! PostgreSQL: the heap, which a publish fills in graph order).

use crate::errors::{EngineError, ErrorType};
use crate::storage::adapter::{self, Scope, UnifiedDb};
pub use crate::storage::adapter::{EdgeRecord, HybridRow, NodeRecord};
use crate::storage::search::{self, Hybrid, READ_SNAPSHOT, Scores};
use crate::storage::text::{self, BRANCH_FTS};
use serde_json::Value;
use sqlx::Row;
use std::collections::HashMap;
use std::future::Future;

/// One lexical hit: the node and its FTS5-signed rank.
#[derive(Debug, Clone, PartialEq)]
pub struct FtsRow {
    pub node: NodeRecord,
    pub fts_rank: f64,
    pub score_norm: f64,
}

/// One edge joined to the node at its other end
/// (`_get_unified_db_relationships`' `SELECT e.<other>, e.rel_type,
/// n.symbol_name, n.symbol_type`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct JoinedEdge {
    pub other_id: String,
    pub rel_type: String,
    pub symbol_name: String,
    pub symbol_type: String,
}

fn database(error: impl std::fmt::Display) -> EngineError {
    EngineError::new(ErrorType::Runtime, error.to_string())
}

/// The reads of the query tools. Every method is one Python statement (or
/// adapter call); see the module comment for the two inexact ones.
pub trait IndexStore: Send + Sync {
    /// Whether the wiki has a `wikis` row (a published index).
    fn wiki_exists(&self) -> impl Future<Output = Result<bool, EngineError>> + Send;

    /// `get_node(id)`.
    fn get_node(
        &self,
        node_id: &str,
    ) -> impl Future<Output = Result<Option<NodeRecord>, EngineError>> + Send;

    /// `get_nodes_by_ids(ids)`: the rows that exist, in any order.
    fn nodes_by_ids(
        &self,
        node_ids: &[String],
    ) -> impl Future<Output = Result<Vec<NodeRecord>, EngineError>> + Send;

    /// `get_edges_from(id)`.
    fn edges_from(
        &self,
        node_id: &str,
    ) -> impl Future<Output = Result<Vec<EdgeRecord>, EngineError>> + Send;

    /// `get_edges_to(id)`.
    fn edges_to(
        &self,
        node_id: &str,
    ) -> impl Future<Output = Result<Vec<EdgeRecord>, EngineError>> + Send;

    /// `_name_rows`: `LOWER(symbol_name) = lower(name)` (`exact`) or
    /// `LIKE '%lower(name)%'` (with `LIKE`'s own `%` and `_`), `LIMIT`.
    ///
    /// [`PgIndex`] lowers both sides with PostgreSQL's `lower()` (Unicode
    /// under the database's `LC_CTYPE`), so "Ärger" finds "Ärger". Python
    /// lowered the name with `str.lower()` (Unicode) and the column with
    /// `SQLite`'s `LOWER()` (ASCII only), so it never found a name with a
    /// non-ASCII capital. [`ReplayIndex`] lowers ASCII only on both sides
    /// (`SQLite`'s `LOWER()` and `LIKE`): the recorded names are ASCII,
    /// where the three agree.
    fn name_rows(
        &self,
        name: &str,
        exact: bool,
        limit: usize,
    ) -> impl Future<Output = Result<Vec<NodeRecord>, EngineError>> + Send;

    /// `_get_code_from_unified_db`'s fallback: the `LIKE` match with the
    /// shortest name (`ORDER BY length(symbol_name) LIMIT 1`). `SQLite`
    /// broke ties by rowid (insertion order); the published table has no
    /// such column, so [`PgIndex`] breaks them by node id.
    fn shortest_like(
        &self,
        name: &str,
    ) -> impl Future<Output = Result<Option<NodeRecord>, EngineError>> + Send;

    /// `search_fts5(query, path_prefix, symbol_types, limit)`.
    /// `path_prefix` is the plain prefix (`rel_path` under `prefix/`).
    fn search_fts(
        &self,
        query: &str,
        path_prefix: Option<&str>,
        symbol_types: Option<&[String]>,
        limit: usize,
    ) -> impl Future<Output = Result<Vec<FtsRow>, EngineError>> + Send;

    /// `_search_unified_db_fts`: any of `keywords`, documentation chunk
    /// types (`module_doc`, `file_doc`, `readme`) excluded, best first.
    fn search_fts_any(
        &self,
        keywords: &[String],
        limit: usize,
    ) -> impl Future<Output = Result<Vec<NodeRecord>, EngineError>> + Send;

    /// `_scan_rows`.
    fn scan_rows(
        &self,
        limit: usize,
        symbol_types: Option<&[String]>,
        path_prefix: Option<&str>,
    ) -> impl Future<Output = Result<Vec<NodeRecord>, EngineError>> + Send;

    /// `_connection_counts`: edges out plus edges in, per id (0 included).
    fn connection_counts(
        &self,
        node_ids: &[String],
    ) -> impl Future<Output = Result<HashMap<String, i64>, EngineError>> + Send;

    /// Edges out of (`outgoing`) or into `node_id`, joined to the node at
    /// the other end, `LIMIT`.
    fn joined_edges(
        &self,
        node_id: &str,
        outgoing: bool,
        limit: usize,
    ) -> impl Future<Output = Result<Vec<JoinedEdge>, EngineError>> + Send;

    /// `search_hybrid(query, embedding, limit, fts_k, vec_k)`.
    fn search_hybrid(
        &self,
        query: &str,
        embedding: Option<&[f64]>,
        limit: usize,
        fts_pool: usize,
        vec_pool: usize,
    ) -> impl Future<Output = Result<Vec<HybridRow>, EngineError>> + Send;
}

/// The live store: one published wiki in PostgreSQL.
#[derive(Debug, Clone)]
pub struct PgIndex {
    db: UnifiedDb,
}

/// `LIKE` text matching `value` literally (`\` is the default escape).
fn like_literal(value: &str) -> String {
    let mut escaped = String::with_capacity(value.len() + 4);
    for c in value.chars() {
        if matches!(c, '%' | '_' | '\\') {
            escaped.push('\\');
        }
        escaped.push(c);
    }
    escaped
}

/// `SQLite`'s `LIKE` has no escape character: only `\` needs escaping for
/// PostgreSQL's, so `%` and `_` keep their wildcard meaning as in Python.
fn like_sqlite(value: &str) -> String {
    value.replace('\\', "\\\\")
}

fn prefix_pattern(prefix: Option<&str>) -> Option<String> {
    prefix
        .filter(|p| !p.is_empty())
        .map(|p| format!("{}/%", like_literal(p.trim_end_matches('/'))))
}

fn sql_limit(limit: usize) -> i64 {
    i64::try_from(limit).unwrap_or(i64::MAX)
}

impl PgIndex {
    #[must_use]
    pub fn new(db: UnifiedDb) -> Self {
        Self { db }
    }

    fn wiki(&self) -> &str {
        self.db.reader().wiki_id()
    }

    fn pool(&self) -> &sqlx::PgPool {
        self.db.reader().pool()
    }

    async fn nodes_where(
        &self,
        clause: &str,
        bind: Vec<Option<String>>,
        limit: usize,
    ) -> Result<Vec<NodeRecord>, EngineError> {
        let sql = format!(
            "{} WHERE wiki_id = $1 AND {clause} LIMIT ${}",
            adapter::NODE_SELECT,
            bind.len() + 2
        );
        let mut query = sqlx::query(&sql).bind(self.wiki());
        for value in bind {
            query = query.bind(value);
        }
        let rows = query
            .bind(sql_limit(limit))
            .fetch_all(self.pool())
            .await
            .map_err(database)?;
        rows.iter()
            .map(|row| NodeRecord::from_row(row).map_err(database))
            .collect()
    }

    /// Rank `matched` by the 'fts' BM25 statistics of `query_text`'s
    /// lexemes, FTS5 sign, ties on node id; keep `limit`.
    async fn ranked(
        &self,
        tx: &mut sqlx::PgConnection,
        matched: Vec<String>,
        terms_source: &[String],
        limit: usize,
    ) -> Result<Vec<(String, f64)>, EngineError> {
        if matched.is_empty() {
            return Ok(Vec::new());
        }
        let terms: Option<Vec<String>> = sqlx::query_scalar(
            "SELECT array_agg(lexeme) FROM unnest(to_tsvector( \
               'deepwiki_porter', regexp_replace(array_to_string($1::text[], ' '), \
               '[^[:alnum:]]+', ' ', 'g') \
             ))",
        )
        .bind(terms_source)
        .fetch_one(&mut *tx)
        .await
        .map_err(database)?;
        let scores = search::bm25_scores(tx, self.wiki(), BRANCH_FTS, &terms.unwrap_or_default())
            .await
            .map_err(database)?;
        let mut ranked: Vec<(String, f64)> = matched
            .into_iter()
            .map(|id| {
                let rank = -scores.get(&id).copied().unwrap_or(0.0);
                (id, rank)
            })
            .collect();
        ranked.sort_by(|a, b| a.1.total_cmp(&b.1).then_with(|| a.0.cmp(&b.0)));
        ranked.truncate(limit);
        Ok(ranked)
    }
}

impl IndexStore for PgIndex {
    async fn wiki_exists(&self) -> Result<bool, EngineError> {
        sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM wikis WHERE wiki_id = $1)")
            .bind(self.wiki())
            .fetch_one(self.pool())
            .await
            .map_err(database)
    }

    async fn get_node(&self, node_id: &str) -> Result<Option<NodeRecord>, EngineError> {
        self.db.get_node(node_id).await.map_err(database)
    }

    async fn nodes_by_ids(&self, node_ids: &[String]) -> Result<Vec<NodeRecord>, EngineError> {
        let ids: Vec<&str> = node_ids.iter().map(String::as_str).collect();
        self.db.get_nodes_by_ids(&ids).await.map_err(database)
    }

    async fn edges_from(&self, node_id: &str) -> Result<Vec<EdgeRecord>, EngineError> {
        self.db.get_edges_from(node_id, &[]).await.map_err(database)
    }

    async fn edges_to(&self, node_id: &str) -> Result<Vec<EdgeRecord>, EngineError> {
        self.db.get_edges_to(node_id).await.map_err(database)
    }

    async fn name_rows(
        &self,
        name: &str,
        exact: bool,
        limit: usize,
    ) -> Result<Vec<NodeRecord>, EngineError> {
        if name.is_empty() {
            return Ok(Vec::new());
        }
        // The name is bound as given and both sides go through the same
        // `lower()`, so a non-ASCII capital ("Ärger") matches itself.
        if exact {
            self.nodes_where(
                "lower(symbol_name) = lower($2)",
                vec![Some(name.to_owned())],
                limit,
            )
            .await
        } else {
            self.nodes_where(
                "lower(symbol_name) LIKE lower($2)",
                vec![Some(format!("%{}%", like_sqlite(name)))],
                limit,
            )
            .await
        }
    }

    async fn shortest_like(&self, name: &str) -> Result<Option<NodeRecord>, EngineError> {
        // Ties on length break on node id: the table has no insertion
        // order (SQLite's rowid) to break them by.
        let sql = format!(
            "{} WHERE wiki_id = $1 AND lower(symbol_name) LIKE lower($2) \
             ORDER BY length(symbol_name), node_id LIMIT 1",
            adapter::NODE_SELECT
        );
        let row = sqlx::query(&sql)
            .bind(self.wiki())
            .bind(format!("%{}%", like_sqlite(name)))
            .fetch_optional(self.pool())
            .await
            .map_err(database)?;
        row.as_ref()
            .map(NodeRecord::from_row)
            .transpose()
            .map_err(database)
    }

    async fn search_fts(
        &self,
        query: &str,
        path_prefix: Option<&str>,
        symbol_types: Option<&[String]>,
        limit: usize,
    ) -> Result<Vec<FtsRow>, EngineError> {
        if crate::graph::pystr::strip(query).is_empty() {
            return Ok(Vec::new());
        }
        let mut tx = self
            .pool()
            .begin_with(READ_SNAPSHOT)
            .await
            .map_err(database)?;
        let matched: Vec<String> = sqlx::query_scalar(
            "SELECT n.node_id FROM wiki_nodes n \
             WHERE n.wiki_id = $1 \
               AND n.fts @@ plainto_tsquery( \
                   'deepwiki_porter', regexp_replace($2, '[^[:alnum:]]+', ' ', 'g')) \
               AND ($3::text IS NULL OR n.rel_path LIKE $3) \
               AND ($4::text[] IS NULL OR n.symbol_type = ANY($4))",
        )
        .bind(self.wiki())
        .bind(query)
        .bind(prefix_pattern(path_prefix))
        .bind(symbol_types)
        .fetch_all(&mut *tx)
        .await
        .map_err(database)?;
        let ranked = self
            .ranked(&mut tx, matched, &[query.to_owned()], limit)
            .await?;
        let ids: Vec<&str> = ranked.iter().map(|(id, _)| id.as_str()).collect();
        let mut nodes = adapter::nodes_by_id(&mut tx, self.wiki(), &ids)
            .await
            .map_err(database)?;
        tx.commit().await.map_err(database)?;
        Ok(ranked
            .into_iter()
            .filter_map(|(id, rank)| {
                nodes.remove(&id).map(|node| FtsRow {
                    node,
                    fts_rank: rank,
                    score_norm: text::score_norm(rank),
                })
            })
            .collect())
    }

    async fn search_fts_any(
        &self,
        keywords: &[String],
        limit: usize,
    ) -> Result<Vec<NodeRecord>, EngineError> {
        if keywords.is_empty() {
            return Ok(Vec::new());
        }
        let mut tx = self
            .pool()
            .begin_with(READ_SNAPSHOT)
            .await
            .map_err(database)?;
        let matched: Vec<String> = sqlx::query_scalar(
            "SELECT n.node_id FROM wiki_nodes n \
             WHERE n.wiki_id = $1 \
               AND n.fts @@ ANY (ARRAY( \
                   SELECT plainto_tsquery('deepwiki_porter', \
                          regexp_replace(k, '[^[:alnum:]]+', ' ', 'g')) \
                   FROM unnest($2::text[]) AS k)) \
               AND n.symbol_type NOT IN ('module_doc', 'file_doc', 'readme')",
        )
        .bind(self.wiki())
        .bind(keywords)
        .fetch_all(&mut *tx)
        .await
        .map_err(database)?;
        let ranked = self.ranked(&mut tx, matched, keywords, limit).await?;
        let ids: Vec<&str> = ranked.iter().map(|(id, _)| id.as_str()).collect();
        let mut nodes = adapter::nodes_by_id(&mut tx, self.wiki(), &ids)
            .await
            .map_err(database)?;
        tx.commit().await.map_err(database)?;
        Ok(ranked
            .into_iter()
            .filter_map(|(id, _)| nodes.remove(&id))
            .collect())
    }

    async fn scan_rows(
        &self,
        limit: usize,
        symbol_types: Option<&[String]>,
        path_prefix: Option<&str>,
    ) -> Result<Vec<NodeRecord>, EngineError> {
        let sql = format!(
            "{} WHERE wiki_id = $1 \
               AND ($2::text[] IS NULL OR symbol_type = ANY($2)) \
               AND ($3::text IS NULL OR rel_path LIKE $3) LIMIT $4",
            adapter::NODE_SELECT
        );
        let rows = sqlx::query(&sql)
            .bind(self.wiki())
            .bind(symbol_types)
            .bind(prefix_pattern(path_prefix))
            .bind(sql_limit(limit))
            .fetch_all(self.pool())
            .await
            .map_err(database)?;
        rows.iter()
            .map(|row| NodeRecord::from_row(row).map_err(database))
            .collect()
    }

    async fn connection_counts(
        &self,
        node_ids: &[String],
    ) -> Result<HashMap<String, i64>, EngineError> {
        let mut counts: HashMap<String, i64> = node_ids.iter().map(|id| (id.clone(), 0)).collect();
        if node_ids.is_empty() {
            return Ok(counts);
        }
        let rows = sqlx::query(
            "SELECT node_id, sum(c)::bigint AS c FROM ( \
                 SELECT source_id AS node_id, count(*) AS c FROM wiki_edges \
                 WHERE wiki_id = $1 AND source_id = ANY($2) GROUP BY source_id \
                 UNION ALL \
                 SELECT target_id AS node_id, count(*) AS c FROM wiki_edges \
                 WHERE wiki_id = $1 AND target_id = ANY($2) GROUP BY target_id \
             ) AS per GROUP BY node_id",
        )
        .bind(self.wiki())
        .bind(node_ids)
        .fetch_all(self.pool())
        .await
        .map_err(database)?;
        for row in rows {
            let id: String = row.try_get("node_id").map_err(database)?;
            let count: i64 = row.try_get("c").map_err(database)?;
            counts.insert(id, count);
        }
        Ok(counts)
    }

    async fn joined_edges(
        &self,
        node_id: &str,
        outgoing: bool,
        limit: usize,
    ) -> Result<Vec<JoinedEdge>, EngineError> {
        let sql = if outgoing {
            "SELECT e.target_id AS other, e.rel_type, n.symbol_name, n.symbol_type \
             FROM wiki_edges e JOIN wiki_nodes n \
               ON n.wiki_id = e.wiki_id AND n.node_id = e.target_id \
             WHERE e.wiki_id = $1 AND e.source_id = $2 LIMIT $3"
        } else {
            "SELECT e.source_id AS other, e.rel_type, n.symbol_name, n.symbol_type \
             FROM wiki_edges e JOIN wiki_nodes n \
               ON n.wiki_id = e.wiki_id AND n.node_id = e.source_id \
             WHERE e.wiki_id = $1 AND e.target_id = $2 LIMIT $3"
        };
        let rows = sqlx::query(sql)
            .bind(self.wiki())
            .bind(node_id)
            .bind(sql_limit(limit))
            .fetch_all(self.pool())
            .await
            .map_err(database)?;
        rows.iter()
            .map(|row| {
                Ok(JoinedEdge {
                    other_id: row.try_get("other").map_err(database)?,
                    rel_type: row.try_get("rel_type").map_err(database)?,
                    symbol_name: row.try_get("symbol_name").map_err(database)?,
                    symbol_type: row.try_get("symbol_type").map_err(database)?,
                })
            })
            .collect()
    }

    async fn search_hybrid(
        &self,
        query: &str,
        embedding: Option<&[f64]>,
        limit: usize,
        fts_pool: usize,
        vec_pool: usize,
    ) -> Result<Vec<HybridRow>, EngineError> {
        if crate::graph::pystr::strip(query).is_empty() {
            return Ok(Vec::new());
        }
        let params = Hybrid {
            limit,
            fts_pool,
            vec_pool,
            ..Hybrid::default()
        };
        self.db
            .search_hybrid(query, embedding, &Scope::default(), &params)
            .await
            .map_err(database)
    }
}

/// A recorded index: the Python run's rows, and its answers to the
/// searches whose ranking is the backend's (FTS5, the fused search).
///
/// Everything a plain SQL statement answers is computed from the rows in
/// rowid order, as `SQLite` answered it; a search without a recording is
/// an error, so the gate cannot pass on a search it never compared.
#[derive(Debug, Clone, Default)]
pub struct ReplayIndex {
    pub nodes: Vec<NodeRecord>,
    pub edges: Vec<EdgeRecord>,
    /// `search_fts` answers by [`ReplayIndex::fts_key`].
    pub fts: HashMap<String, Vec<(String, f64, f64)>>,
    /// `search_fts_any` answers by [`ReplayIndex::fts_any_key`].
    pub fts_any: HashMap<String, Vec<String>>,
    /// `search_hybrid` answers by [`ReplayIndex::hybrid_key`].
    pub hybrid: HashMap<String, Vec<(String, Scores)>>,
}

impl ReplayIndex {
    /// The recording key of a `search_fts` call.
    #[must_use]
    pub fn fts_key(
        query: &str,
        path_prefix: Option<&str>,
        symbol_types: Option<&[String]>,
        limit: usize,
    ) -> String {
        serde_json::json!([query, path_prefix, symbol_types, limit]).to_string()
    }

    /// The recording key of a `search_fts_any` call.
    #[must_use]
    pub fn fts_any_key(keywords: &[String], limit: usize) -> String {
        serde_json::json!([keywords, limit]).to_string()
    }

    /// The recording key of a `search_hybrid` call.
    #[must_use]
    pub fn hybrid_key(query: &str, limit: usize, fts_pool: usize, vec_pool: usize) -> String {
        serde_json::json!([query, limit, fts_pool, vec_pool]).to_string()
    }

    fn node(&self, node_id: &str) -> Option<&NodeRecord> {
        self.nodes.iter().find(|node| node.node_id == node_id)
    }

    fn unrecorded(what: &str, key: &str) -> EngineError {
        EngineError::new(
            ErrorType::Runtime,
            format!("the replay index has no recording of {what} {key}"),
        )
    }

    /// `LIKE` as `SQLite` evaluates it: ASCII case-insensitive, `%` any
    /// run, `_` one character.
    fn like(text: &str, pattern: &str) -> bool {
        let text: Vec<char> = super::pyfmt::sqlite_lower(text).chars().collect();
        let pattern: Vec<char> = super::pyfmt::sqlite_lower(pattern).chars().collect();
        like_at(&text, &pattern)
    }
}

fn like_at(text: &[char], pattern: &[char]) -> bool {
    let (mut t, mut p) = (0, 0);
    let mut star: Option<(usize, usize)> = None;
    while t < text.len() {
        if p < pattern.len() && (pattern[p] == '_' || pattern[p] == text[t]) {
            t += 1;
            p += 1;
        } else if p < pattern.len() && pattern[p] == '%' {
            star = Some((p, t));
            p += 1;
        } else if let Some((sp, st)) = star {
            p = sp + 1;
            t = st + 1;
            star = Some((sp, st + 1));
        } else {
            return false;
        }
    }
    pattern[p..].iter().all(|&c| c == '%')
}

fn under(rel_path: &str, prefix: Option<&str>) -> bool {
    match prefix.filter(|p| !p.is_empty()) {
        None => true,
        Some(prefix) => rel_path.starts_with(&format!("{}/", prefix.trim_end_matches('/'))),
    }
}

impl IndexStore for ReplayIndex {
    async fn wiki_exists(&self) -> Result<bool, EngineError> {
        Ok(true)
    }

    async fn get_node(&self, node_id: &str) -> Result<Option<NodeRecord>, EngineError> {
        Ok(self.node(node_id).cloned())
    }

    async fn nodes_by_ids(&self, node_ids: &[String]) -> Result<Vec<NodeRecord>, EngineError> {
        Ok(self
            .nodes
            .iter()
            .filter(|node| node_ids.contains(&node.node_id))
            .cloned()
            .collect())
    }

    async fn edges_from(&self, node_id: &str) -> Result<Vec<EdgeRecord>, EngineError> {
        Ok(self
            .edges
            .iter()
            .filter(|e| e.source_id == node_id)
            .cloned()
            .collect())
    }

    async fn edges_to(&self, node_id: &str) -> Result<Vec<EdgeRecord>, EngineError> {
        Ok(self
            .edges
            .iter()
            .filter(|e| e.target_id == node_id)
            .cloned()
            .collect())
    }

    async fn name_rows(
        &self,
        name: &str,
        exact: bool,
        limit: usize,
    ) -> Result<Vec<NodeRecord>, EngineError> {
        if name.is_empty() {
            return Ok(Vec::new());
        }
        let lowered = super::pyfmt::sqlite_lower(name);
        let pattern = format!("%{lowered}%");
        Ok(self
            .nodes
            .iter()
            .filter(|node| {
                if exact {
                    super::pyfmt::sqlite_lower(&node.symbol_name) == lowered
                } else {
                    Self::like(&node.symbol_name, &pattern)
                }
            })
            .take(limit)
            .cloned()
            .collect())
    }

    async fn shortest_like(&self, name: &str) -> Result<Option<NodeRecord>, EngineError> {
        let pattern = format!("%{}%", super::pyfmt::sqlite_lower(name));
        // `ORDER BY length(...)` is stable on rowid in SQLite's sorter for
        // this query shape: the first of the shortest.
        let mut best: Option<&NodeRecord> = None;
        for node in self
            .nodes
            .iter()
            .filter(|n| Self::like(&n.symbol_name, &pattern))
        {
            let length = node.symbol_name.chars().count();
            if best.is_none_or(|b| length < b.symbol_name.chars().count()) {
                best = Some(node);
            }
        }
        Ok(best.cloned())
    }

    async fn search_fts(
        &self,
        query: &str,
        path_prefix: Option<&str>,
        symbol_types: Option<&[String]>,
        limit: usize,
    ) -> Result<Vec<FtsRow>, EngineError> {
        if crate::graph::pystr::strip(query).is_empty() {
            return Ok(Vec::new());
        }
        let key = Self::fts_key(query, path_prefix, symbol_types, limit);
        let recorded = self
            .fts
            .get(&key)
            .ok_or_else(|| Self::unrecorded("search_fts", &key))?;
        Ok(recorded
            .iter()
            .filter_map(|(id, rank, norm)| {
                self.node(id).map(|node| FtsRow {
                    node: node.clone(),
                    fts_rank: *rank,
                    score_norm: *norm,
                })
            })
            .collect())
    }

    async fn search_fts_any(
        &self,
        keywords: &[String],
        limit: usize,
    ) -> Result<Vec<NodeRecord>, EngineError> {
        if keywords.is_empty() {
            return Ok(Vec::new());
        }
        let key = Self::fts_any_key(keywords, limit);
        let recorded = self
            .fts_any
            .get(&key)
            .ok_or_else(|| Self::unrecorded("search_fts_any", &key))?;
        Ok(recorded
            .iter()
            .filter_map(|id| self.node(id).cloned())
            .collect())
    }

    async fn scan_rows(
        &self,
        limit: usize,
        symbol_types: Option<&[String]>,
        path_prefix: Option<&str>,
    ) -> Result<Vec<NodeRecord>, EngineError> {
        Ok(self
            .nodes
            .iter()
            .filter(|n| symbol_types.is_none_or(|types| types.contains(&n.symbol_type)))
            .filter(|n| under(&n.rel_path, path_prefix))
            .take(limit)
            .cloned()
            .collect())
    }

    async fn connection_counts(
        &self,
        node_ids: &[String],
    ) -> Result<HashMap<String, i64>, EngineError> {
        let mut counts: HashMap<String, i64> = node_ids.iter().map(|id| (id.clone(), 0)).collect();
        for edge in &self.edges {
            for end in [&edge.source_id, &edge.target_id] {
                if let Some(count) = counts.get_mut(end.as_str()) {
                    *count += 1;
                }
            }
        }
        Ok(counts)
    }

    async fn joined_edges(
        &self,
        node_id: &str,
        outgoing: bool,
        limit: usize,
    ) -> Result<Vec<JoinedEdge>, EngineError> {
        Ok(self
            .edges
            .iter()
            .filter(|e| {
                if outgoing {
                    e.source_id == node_id
                } else {
                    e.target_id == node_id
                }
            })
            .filter_map(|e| {
                let other = if outgoing { &e.target_id } else { &e.source_id };
                self.node(other).map(|n| JoinedEdge {
                    other_id: other.clone(),
                    rel_type: e.rel_type.clone(),
                    symbol_name: n.symbol_name.clone(),
                    symbol_type: n.symbol_type.clone(),
                })
            })
            .take(limit)
            .collect())
    }

    async fn search_hybrid(
        &self,
        query: &str,
        _embedding: Option<&[f64]>,
        limit: usize,
        fts_pool: usize,
        vec_pool: usize,
    ) -> Result<Vec<HybridRow>, EngineError> {
        if crate::graph::pystr::strip(query).is_empty() {
            return Ok(Vec::new());
        }
        let key = Self::hybrid_key(query, limit, fts_pool, vec_pool);
        let recorded = self
            .hybrid
            .get(&key)
            .ok_or_else(|| Self::unrecorded("search_hybrid", &key))?;
        Ok(recorded
            .iter()
            .filter_map(|(id, scores)| {
                self.node(id).map(|node| HybridRow {
                    node: node.clone(),
                    scores: *scores,
                })
            })
            .collect())
    }
}

/// A node row of a `.wiki.db` dump (`repo_nodes` as JSON).
///
/// # Errors
///
/// A `ValueError` for a row without an id.
pub fn node_from_dump(row: &Value) -> Result<NodeRecord, EngineError> {
    let text = |key: &str| row[key].as_str().unwrap_or_default().to_owned();
    let int = |key: &str| {
        row[key]
            .as_i64()
            .and_then(|v| i32::try_from(v).ok())
            .unwrap_or(0)
    };
    let flag = |key: &str| row[key].as_i64().unwrap_or(0) != 0 || row[key] == Value::Bool(true);
    let node_id = row["node_id"]
        .as_str()
        .ok_or_else(|| EngineError::new(ErrorType::Value, "a dumped node has no node_id"))?
        .to_owned();
    Ok(NodeRecord {
        node_id,
        rel_path: text("rel_path"),
        file_name: text("file_name"),
        language: text("language"),
        start_line: int("start_line"),
        end_line: int("end_line"),
        symbol_name: text("symbol_name"),
        symbol_type: text("symbol_type"),
        parent_symbol: row["parent_symbol"].as_str().map(str::to_owned),
        source_text: text("source_text"),
        docstring: text("docstring"),
        signature: text("signature"),
        is_architectural: flag("is_architectural"),
        is_doc: flag("is_doc"),
        is_test: flag("is_test"),
        chunk_type: row["chunk_type"].as_str().map(str::to_owned),
        macro_cluster: row["macro_cluster"]
            .as_i64()
            .and_then(|v| i32::try_from(v).ok()),
        micro_cluster: row["micro_cluster"]
            .as_i64()
            .and_then(|v| i32::try_from(v).ok()),
    })
}

/// An edge row of a `.wiki.db` dump: its `annotations` JSON text becomes
/// the record's `metadata` (where the PostgreSQL row keeps `{}`).
#[must_use]
pub fn edge_from_dump(row: &Value) -> EdgeRecord {
    let text = |key: &str| row[key].as_str().unwrap_or_default().to_owned();
    let metadata = row["annotations"]
        .as_str()
        .and_then(|raw| serde_json::from_str(raw).ok())
        .unwrap_or_else(|| Value::Object(serde_json::Map::new()));
    EdgeRecord {
        source_id: text("source_id"),
        target_id: text("target_id"),
        rel_type: text("rel_type"),
        edge_class: row["edge_class"].as_str().map(str::to_owned),
        weight: row["weight"].as_f64().unwrap_or(1.0),
        metadata,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sqlite_like_has_two_wildcards() {
        assert!(ReplayIndex::like("NoteStore", "%store%"));
        assert!(ReplayIndex::like("note_x", "%e_x%"));
        assert!(ReplayIndex::like("noteAx", "%e_x%"));
        assert!(!ReplayIndex::like("note", "%store%"));
        assert_eq!(like_sqlite("a\\b%"), "a\\\\b%");
        assert_eq!(
            prefix_pattern(Some("src/a_b/")).as_deref(),
            Some("src/a\\_b/%")
        );
        assert!(under("src/a/b.py", Some("src/a")));
        assert!(!under("src/ab/b.py", Some("src/a")));
    }
}
