//! The read surface `ask`, `deep_research` and `resolve_wiki` use: a port
//! of `storage/unified_db_adapter.py` (`PostgresUnifiedDB`).
//!
//! The Python engine's retriever asked its `db` object for `search_hybrid`,
//! `get_node`, `get_edges_from`, `get_edges_to` and `vec_available`, and
//! read specific keys off the rows. Those row shapes are the contract, so
//! the records here convert to the legacy `repo_nodes` dict
//! ([`NodeRecord::to_legacy`]): the legacy column names, the flags as 0/1
//! integers, and the legacy-only columns present with their legacy
//! defaults (`analysis_level`, `parameters`, `return_type`, `is_hub`,
//! `hub_assignment`), because an absent key and a false value differ to a
//! caller.
//!
//! The write methods of `UnifiedWikiDB` are absent: a build writes through
//! [`crate::storage::build`], never through a reader.
//!
//! Deliberate difference: the `path_prefix` filter escapes `%`, `_` and `\`
//! in the prefix. Python escaped only `%`, so `_` in a directory name
//! matched any character.

use crate::storage::search::{self, Hit, Hybrid, IndexReader, READ_SNAPSHOT, Scores};
use crate::storage::{Result, WikiKey};
use serde_json::{Map, Value, json};
use sqlx::Row;
use sqlx::postgres::{PgConnection, PgPool, PgRow};

/// One `wiki_nodes` row in the legacy column order (`_NODE_COLUMNS`).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct NodeRecord {
    pub node_id: String,
    pub rel_path: String,
    pub file_name: String,
    pub language: String,
    pub start_line: i32,
    pub end_line: i32,
    pub symbol_name: String,
    pub symbol_type: String,
    pub parent_symbol: Option<String>,
    pub source_text: String,
    pub docstring: String,
    pub signature: String,
    pub is_architectural: bool,
    pub is_doc: bool,
    pub is_test: bool,
    pub chunk_type: Option<String>,
    pub macro_cluster: Option<i32>,
    pub micro_cluster: Option<i32>,
}

impl NodeRecord {
    /// `_row_to_node`: the legacy-shaped dict.
    #[must_use]
    pub fn to_legacy(&self) -> Map<String, Value> {
        let mut node = Map::new();
        node.insert("node_id".into(), json!(self.node_id));
        node.insert("rel_path".into(), json!(self.rel_path));
        node.insert("file_name".into(), json!(self.file_name));
        node.insert("language".into(), json!(self.language));
        node.insert("start_line".into(), json!(self.start_line));
        node.insert("end_line".into(), json!(self.end_line));
        node.insert("symbol_name".into(), json!(self.symbol_name));
        node.insert("symbol_type".into(), json!(self.symbol_type));
        node.insert("parent_symbol".into(), json!(self.parent_symbol));
        node.insert("source_text".into(), json!(self.source_text));
        node.insert("docstring".into(), json!(self.docstring));
        node.insert("signature".into(), json!(self.signature));
        // The legacy schema stored the flags as integers and callers test
        // their truthiness: keep the type as well as the value.
        node.insert(
            "is_architectural".into(),
            json!(i32::from(self.is_architectural)),
        );
        node.insert("is_doc".into(), json!(i32::from(self.is_doc)));
        node.insert("is_test".into(), json!(i32::from(self.is_test)));
        node.insert("chunk_type".into(), json!(self.chunk_type));
        node.insert("macro_cluster".into(), json!(self.macro_cluster));
        node.insert("micro_cluster".into(), json!(self.micro_cluster));
        // Legacy columns this schema drops: present with their defaults.
        node.insert("analysis_level".into(), json!("comprehensive"));
        node.insert("parameters".into(), json!(""));
        node.insert("return_type".into(), json!(""));
        node.insert("is_hub".into(), json!(0));
        node.insert("hub_assignment".into(), Value::Null);
        node
    }

    pub(crate) fn from_row(row: &PgRow) -> Result<Self> {
        Ok(Self {
            node_id: row.try_get("node_id")?,
            rel_path: row.try_get("rel_path")?,
            file_name: row.try_get("file_name")?,
            language: row.try_get("language")?,
            start_line: row.try_get("start_line")?,
            end_line: row.try_get("end_line")?,
            symbol_name: row.try_get("symbol_name")?,
            symbol_type: row.try_get("symbol_type")?,
            parent_symbol: row.try_get("parent_symbol")?,
            source_text: row.try_get("source_text")?,
            docstring: row.try_get("docstring")?,
            signature: row.try_get("signature")?,
            is_architectural: row.try_get("is_architectural")?,
            is_doc: row.try_get("is_doc")?,
            is_test: row.try_get("is_test")?,
            chunk_type: row.try_get("chunk_type")?,
            macro_cluster: row.try_get("macro_cluster")?,
            micro_cluster: row.try_get("micro_cluster")?,
        })
    }
}

/// One fused result: the node and its scores (`node.update(hit.scores)`).
#[derive(Debug, Clone, PartialEq)]
pub struct HybridRow {
    pub node: NodeRecord,
    pub scores: Scores,
}

impl HybridRow {
    /// The legacy dict: the node's keys, then the score keys.
    #[must_use]
    pub fn to_legacy(&self) -> Map<String, Value> {
        let mut row = self.node.to_legacy();
        for (key, value) in self.scores.entries() {
            row.insert(key.to_owned(), json!(value));
        }
        row
    }
}

/// One `wiki_edges` row.
#[derive(Debug, Clone, PartialEq)]
pub struct EdgeRecord {
    pub source_id: String,
    pub target_id: String,
    pub rel_type: String,
    pub edge_class: Option<String>,
    /// `REAL`, widened (Python `float(row[4])`).
    pub weight: f64,
    pub metadata: Value,
}

impl EdgeRecord {
    /// The legacy dict.
    #[must_use]
    pub fn to_legacy(&self) -> Map<String, Value> {
        let mut edge = Map::new();
        edge.insert("source_id".into(), json!(self.source_id));
        edge.insert("target_id".into(), json!(self.target_id));
        edge.insert("rel_type".into(), json!(self.rel_type));
        edge.insert("edge_class".into(), json!(self.edge_class));
        edge.insert("weight".into(), json!(self.weight));
        edge.insert("metadata".into(), self.metadata.clone());
        edge
    }

    pub(crate) fn from_row(row: &PgRow) -> Result<Self> {
        Ok(Self {
            source_id: row.try_get("source_id")?,
            target_id: row.try_get("target_id")?,
            rel_type: row.try_get("rel_type")?,
            edge_class: row.try_get("edge_class")?,
            weight: f64::from(row.try_get::<f32, _>("weight")?),
            metadata: row.try_get("metadata")?,
        })
    }
}

/// The scoped filters of `search_hybrid`.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Scope {
    pub path_prefix: Option<String>,
    pub cluster_id: Option<i32>,
}

/// `_NODE_SELECT`.
pub(crate) const NODE_SELECT: &str = "SELECT node_id, rel_path, file_name, language, start_line, end_line, \
     symbol_name, symbol_type, parent_symbol, source_text, docstring, signature, \
     is_architectural, is_doc, is_test, chunk_type, macro_cluster, micro_cluster \
     FROM wiki_nodes";

/// `PostgresUnifiedDB`: one wiki's read surface, within one project. Every
/// statement filters by the [`WikiKey`]'s `(project_id, wiki_id)`.
#[derive(Debug, Clone)]
pub struct UnifiedDb {
    reader: IndexReader,
}

impl UnifiedDb {
    /// The reader of one project's wiki.
    #[must_use]
    pub fn new(pool: PgPool, key: WikiKey) -> Self {
        Self {
            reader: IndexReader::new(pool, key),
        }
    }

    /// The branch searches.
    #[must_use]
    pub fn reader(&self) -> &IndexReader {
        &self.reader
    }

    fn key(&self) -> &WikiKey {
        self.reader.key()
    }

    async fn snapshot(&self) -> Result<sqlx::Transaction<'static, sqlx::Postgres>> {
        Ok(self.reader.pool().begin_with(READ_SNAPSHOT).await?)
    }

    /// Whether any dense vector exists for this wiki: "can a dense search
    /// return anything", which is what the caller decides with it.
    ///
    /// # Errors
    ///
    /// [`crate::storage::StorageError::Database`].
    pub async fn vec_available(&self) -> Result<bool> {
        Ok(sqlx::query_scalar(
            "SELECT EXISTS (SELECT 1 FROM wiki_node_embeddings \
             WHERE wiki_id = $1 AND project_id = $2)",
        )
        .bind(self.key().wiki_id())
        .bind(self.key().project_id())
        .fetch_one(self.reader.pool())
        .await?)
    }

    /// Fused search with legacy-shaped rows. `scope` is applied after the
    /// fusion and the result cut to `params.limit` again, as Python does.
    ///
    /// # Errors
    ///
    /// [`crate::storage::StorageError::Database`].
    pub async fn search_hybrid(
        &self,
        query: &str,
        embedding: Option<&[f64]>,
        scope: &Scope,
        params: &Hybrid,
    ) -> Result<Vec<HybridRow>> {
        let mut tx = self.snapshot().await?;
        let key = self.key();
        let mut hits = search::search_hybrid(&mut tx, key, query, embedding, params).await?;
        let filtered = scope.path_prefix.as_deref().is_some_and(|p| !p.is_empty())
            || scope.cluster_id.is_some();
        if filtered {
            hits = filter(&mut tx, key, hits, scope).await?;
            hits.truncate(params.limit);
        }
        let ids: Vec<&str> = hits.iter().map(|hit| hit.node_id.as_str()).collect();
        let mut nodes = nodes_by_id(&mut tx, key, &ids).await?;
        tx.commit().await?;
        Ok(hits
            .into_iter()
            .filter_map(|hit| {
                nodes.remove(&hit.node_id).map(|node| HybridRow {
                    node,
                    scores: hit.scores,
                })
            })
            .collect())
    }

    /// One node, or `None`.
    ///
    /// # Errors
    ///
    /// [`crate::storage::StorageError::Database`].
    pub async fn get_node(&self, node_id: &str) -> Result<Option<NodeRecord>> {
        let row = sqlx::query(&format!(
            "{NODE_SELECT} WHERE wiki_id = $1 AND node_id = $2 AND project_id = $3"
        ))
        .bind(self.key().wiki_id())
        .bind(node_id)
        .bind(self.key().project_id())
        .fetch_optional(self.reader.pool())
        .await?;
        row.as_ref().map(NodeRecord::from_row).transpose()
    }

    /// The nodes of `node_ids` that exist, in no particular order.
    ///
    /// # Errors
    ///
    /// [`crate::storage::StorageError::Database`].
    pub async fn get_nodes_by_ids(&self, node_ids: &[&str]) -> Result<Vec<NodeRecord>> {
        if node_ids.is_empty() {
            return Ok(Vec::new());
        }
        let mut connection = self.reader.pool().acquire().await?;
        Ok(nodes_by_id(&mut connection, self.key(), node_ids)
            .await?
            .into_values()
            .collect())
    }

    /// Edges out of `node_id`, optionally of the given relation types (an
    /// empty list means all, as Python's falsy check).
    ///
    /// # Errors
    ///
    /// [`crate::storage::StorageError::Database`].
    pub async fn get_edges_from(
        &self,
        node_id: &str,
        rel_types: &[&str],
    ) -> Result<Vec<EdgeRecord>> {
        let rows = if rel_types.is_empty() {
            sqlx::query(
                "SELECT source_id, target_id, rel_type, edge_class, weight, metadata \
                 FROM wiki_edges WHERE wiki_id = $1 AND source_id = $2 AND project_id = $3",
            )
            .bind(self.key().wiki_id())
            .bind(node_id)
            .bind(self.key().project_id())
            .fetch_all(self.reader.pool())
            .await?
        } else {
            sqlx::query(
                "SELECT source_id, target_id, rel_type, edge_class, weight, metadata \
                 FROM wiki_edges WHERE wiki_id = $1 AND source_id = $2 AND rel_type = ANY($3) \
                 AND project_id = $4",
            )
            .bind(self.key().wiki_id())
            .bind(node_id)
            .bind(rel_types)
            .bind(self.key().project_id())
            .fetch_all(self.reader.pool())
            .await?
        };
        rows.iter().map(EdgeRecord::from_row).collect()
    }

    /// Edges into `node_id`.
    ///
    /// # Errors
    ///
    /// [`crate::storage::StorageError::Database`].
    pub async fn get_edges_to(&self, node_id: &str) -> Result<Vec<EdgeRecord>> {
        let rows = sqlx::query(
            "SELECT source_id, target_id, rel_type, edge_class, weight, metadata \
             FROM wiki_edges WHERE wiki_id = $1 AND target_id = $2 AND project_id = $3",
        )
        .bind(self.key().wiki_id())
        .bind(node_id)
        .bind(self.key().project_id())
        .fetch_all(self.reader.pool())
        .await?;
        rows.iter().map(EdgeRecord::from_row).collect()
    }

    /// The stored dense vector of a node (`float4` components), or `None`.
    ///
    /// # Errors
    ///
    /// [`crate::storage::StorageError::Database`].
    pub async fn get_embedding(&self, node_id: &str) -> Result<Option<Vec<f32>>> {
        let vector: Option<pgvector::Vector> = sqlx::query_scalar(
            "SELECT embedding FROM wiki_node_embeddings \
             WHERE wiki_id = $1 AND node_id = $2 AND project_id = $3",
        )
        .bind(self.key().wiki_id())
        .bind(node_id)
        .bind(self.key().project_id())
        .fetch_optional(self.reader.pool())
        .await?;
        Ok(vector.map(Vec::from))
    }

    /// Node count.
    ///
    /// # Errors
    ///
    /// [`crate::storage::StorageError::Database`].
    pub async fn node_count(&self) -> Result<i64> {
        Ok(sqlx::query_scalar(
            "SELECT count(*) FROM wiki_nodes WHERE wiki_id = $1 AND project_id = $2",
        )
        .bind(self.key().wiki_id())
        .bind(self.key().project_id())
        .fetch_one(self.reader.pool())
        .await?)
    }

    /// Edge count.
    ///
    /// # Errors
    ///
    /// [`crate::storage::StorageError::Database`].
    pub async fn edge_count(&self) -> Result<i64> {
        Ok(sqlx::query_scalar(
            "SELECT count(*) FROM wiki_edges WHERE wiki_id = $1 AND project_id = $2",
        )
        .bind(self.key().wiki_id())
        .bind(self.key().project_id())
        .fetch_one(self.reader.pool())
        .await?)
    }

    /// Wiki-level metadata off the `wikis` row (`get_meta`): one of
    /// `wiki_id`, `repo`, `branch`, `commit_hash`, `wiki_version_id`,
    /// `analysis_key`, `canonical_repo_identifier`; `None` for any other
    /// key, a missing row or a NULL value (the caller's default applies).
    ///
    /// # Errors
    ///
    /// [`crate::storage::StorageError::Database`].
    pub async fn get_meta(&self, key: &str) -> Result<Option<String>> {
        let known = [
            "wiki_id",
            "repo",
            "branch",
            "commit_hash",
            "wiki_version_id",
            "analysis_key",
            "canonical_repo_identifier",
        ];
        if !known.contains(&key) {
            return Ok(None);
        }
        let row = sqlx::query(
            "SELECT wiki_id, repo, branch, commit_hash, wiki_version_id, analysis_key, \
                 canonical_repo_identifier FROM wikis WHERE wiki_id = $1 AND project_id = $2",
        )
        .bind(self.key().wiki_id())
        .bind(self.key().project_id())
        .fetch_optional(self.reader.pool())
        .await?;
        match row {
            Some(row) => Ok(row.try_get::<Option<String>, _>(key)?),
            None => Ok(None),
        }
    }
}

pub(crate) async fn nodes_by_id(
    connection: &mut PgConnection,
    key: &WikiKey,
    node_ids: &[&str],
) -> Result<std::collections::HashMap<String, NodeRecord>> {
    if node_ids.is_empty() {
        return Ok(std::collections::HashMap::new());
    }
    let rows = sqlx::query(&format!(
        "{NODE_SELECT} WHERE wiki_id = $1 AND node_id = ANY($2) AND project_id = $3"
    ))
    .bind(key.wiki_id())
    .bind(node_ids)
    .bind(key.project_id())
    .fetch_all(&mut *connection)
    .await?;
    let mut nodes = std::collections::HashMap::with_capacity(rows.len());
    for row in &rows {
        let node = NodeRecord::from_row(row)?;
        nodes.insert(node.node_id.clone(), node);
    }
    Ok(nodes)
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

/// `_filter`: keep the hits under `path_prefix` and in `cluster_id`.
async fn filter(
    connection: &mut PgConnection,
    key: &WikiKey,
    hits: Vec<Hit>,
    scope: &Scope,
) -> Result<Vec<Hit>> {
    if hits.is_empty() {
        return Ok(hits);
    }
    let ids: Vec<&str> = hits.iter().map(|hit| hit.node_id.as_str()).collect();
    let pattern = scope
        .path_prefix
        .as_deref()
        .filter(|prefix| !prefix.is_empty())
        .map(|prefix| format!("{}/%", like_literal(prefix.trim_end_matches('/'))));
    // Each filter is optional: a NULL parameter switches it off, so the
    // statement stays static.
    let allowed: Vec<String> = sqlx::query_scalar(
        "SELECT node_id FROM wiki_nodes \
         WHERE wiki_id = $1 AND node_id = ANY($2) \
           AND ($3::text IS NULL OR rel_path LIKE $3) \
           AND ($4::integer IS NULL OR macro_cluster = $4) \
           AND project_id = $5",
    )
    .bind(key.wiki_id())
    .bind(&ids)
    .bind(pattern)
    .bind(scope.cluster_id)
    .bind(key.project_id())
    .fetch_all(&mut *connection)
    .await?;
    let allowed: std::collections::HashSet<String> = allowed.into_iter().collect();
    Ok(hits
        .into_iter()
        .filter(|hit| allowed.contains(&hit.node_id))
        .collect())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_legacy_node_shape_has_every_key() {
        let node = NodeRecord {
            node_id: "a".into(),
            is_doc: true,
            ..NodeRecord::default()
        };
        let legacy = node.to_legacy();
        let keys: Vec<&str> = legacy.keys().map(String::as_str).collect();
        assert_eq!(
            keys,
            [
                "node_id",
                "rel_path",
                "file_name",
                "language",
                "start_line",
                "end_line",
                "symbol_name",
                "symbol_type",
                "parent_symbol",
                "source_text",
                "docstring",
                "signature",
                "is_architectural",
                "is_doc",
                "is_test",
                "chunk_type",
                "macro_cluster",
                "micro_cluster",
                "analysis_level",
                "parameters",
                "return_type",
                "is_hub",
                "hub_assignment",
            ]
        );
        assert_eq!(legacy["is_doc"], json!(1));
        assert_eq!(legacy["is_test"], json!(0));
        assert_eq!(legacy["parent_symbol"], Value::Null);
    }

    #[test]
    fn like_patterns_match_literally() {
        assert_eq!(like_literal("a_b%c\\d"), "a\\_b\\%c\\\\d");
    }
}
