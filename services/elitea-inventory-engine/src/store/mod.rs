//! The graph's PostgreSQL storage (`migrations/`), replacing the
//! `graph.json` object in the toolkit's bucket.
//!
//! A graph is addressed by [`GraphKey`] — the platform project and the
//! Inventory toolkit — and both lead every key and every statement, so a
//! read or a delete never crosses projects.
//!
//! This phase writes a graph whole ([`save`]: one transaction replaces its
//! rows, serialised per graph by an advisory lock) and reads it whole
//! ([`load`]); ingestion's row-level writes and SQL retrieval come in later
//! phases on the same tables.
//!
//! A DSN carries a password. Nothing here logs or formats one, and a DSN
//! that does not parse is reported without its text.

use crate::graph::Graph;
use elitea_pg_migrate::{Ledger, MigrateError, Migration};
use serde_json::{Map, Value};
use sqlx::postgres::{PgConnectOptions, PgPool, PgPoolOptions};
use sqlx::types::Json;
use sqlx::{QueryBuilder, Row};
use std::str::FromStr;
use std::time::Duration;

pub mod graph_store;
pub mod sources;
pub mod vectors;

pub use graph_store::PgGraphStore;

/// The DSN variable: the engine's database. Unset, the engine stores
/// nothing (the fixture runner needs no database).
pub const DSN_ENV: &str = "ELITEA_INVENTORY_DATABASE_URL";

/// This engine's migration ledger, in its own schema so it can share a
/// database with another engine's.
pub const LEDGER: Ledger = Ledger {
    table: "inventory_graph.schema_migrations",
    lock_name: "elitea_inventory.schema_migrations",
};

/// `(file name, text)` of every migration, in version order. A test fails
/// while this list and the `migrations/` directory disagree.
const EMBEDDED: &[(&str, &str)] = &[
    (
        "0001_graph_store.sql",
        include_str!("../../migrations/0001_graph_store.sql"),
    ),
    (
        "0002_sources.sql",
        include_str!("../../migrations/0002_sources.sql"),
    ),
    (
        "0003_documents.sql",
        include_str!("../../migrations/0003_documents.sql"),
    ),
    (
        "0004_entity_vectors.sql",
        include_str!("../../migrations/0004_entity_vectors.sql"),
    ),
];

/// Rows per multi-row INSERT: 7 bound columns a row stay far below the 65535
/// bind parameters a statement may carry.
const BATCH_ROWS: usize = 1000;

/// `_metadata` keys a document export stamps afresh; not stored.
const STAMPED_METADATA: [&str; 2] = ["last_saved", "version"];

/// A storage failure.
#[derive(Debug, thiserror::Error)]
pub enum StoreError {
    /// The DSN does not parse. Its text is never part of the message.
    #[error("{DSN_ENV} is not a valid postgresql:// URL")]
    InvalidDsn,
    /// A graph address that is not a pair of positive integers.
    #[error("{0}")]
    InvalidKey(String),
    /// A migration file problem or an edited applied migration.
    #[error("{0}")]
    Migration(String),
    /// A graph that cannot be stored as it is.
    #[error("{0}")]
    Unstorable(String),
    #[error(transparent)]
    Database(#[from] sqlx::Error),
}

impl From<MigrateError> for StoreError {
    fn from(error: MigrateError) -> Self {
        match error {
            MigrateError::Migration(message) => Self::Migration(message),
            MigrateError::Database(error) => Self::Database(error),
        }
    }
}

/// The result type of this module.
pub type Result<T> = std::result::Result<T, StoreError>;

/// Which graph (the shared core's type, ADR-0029 decision 7).
pub use elitea_inventory_core::store::{GraphKey, InvalidKey};

impl From<InvalidKey> for StoreError {
    fn from(error: InvalidKey) -> Self {
        Self::InvalidKey(error.0)
    }
}

/// Parse a DSN (URL form only).
///
/// # Errors
///
/// [`StoreError::InvalidDsn`], without the DSN text.
pub fn connect_options(dsn: &str) -> Result<PgConnectOptions> {
    // The parse error can quote the URL; it is dropped, not wrapped.
    PgConnectOptions::from_str(dsn.trim())
        .map_err(|_| StoreError::InvalidDsn)
        .map(|options| {
            options
                .application_name("elitea-inventory-engine")
                .options([("client_min_messages", "warning")])
        })
}

/// A pool that connects on first use, so a start while the database is
/// still coming up does not fail; the first statement reports it instead.
///
/// # Errors
///
/// [`StoreError::InvalidDsn`].
pub fn lazy_pool(dsn: &str, max_connections: u32) -> Result<PgPool> {
    Ok(PgPoolOptions::new()
        .max_connections(max_connections.max(1))
        .acquire_timeout(Duration::from_secs(30))
        .connect_lazy_with(connect_options(dsn)?))
}

/// The embedded migrations, in version order.
///
/// # Errors
///
/// [`StoreError::Migration`] for a misnamed file (a test proves there is
/// none).
pub fn embedded() -> Result<Vec<Migration>> {
    Ok(elitea_pg_migrate::discover_from(EMBEDDED.iter().copied())?)
}

/// The embedded file names, for the test that compares them with the
/// directory.
#[must_use]
pub fn embedded_file_names() -> Vec<&'static str> {
    EMBEDDED.iter().map(|(name, _)| *name).collect()
}

/// Apply the embedded migrations; returns the versions this call applied.
///
/// # Errors
///
/// [`StoreError::Migration`] for an edited applied migration,
/// [`StoreError::Database`] for a failed statement.
pub async fn migrate(pool: &PgPool) -> Result<Vec<String>> {
    Ok(elitea_pg_migrate::apply(pool, &LEDGER, &embedded()?).await?)
}

/// Why the store's schema cannot serve a read-only command, or `None`
/// when every embedded migration is applied. Reads the ledger only: a
/// read-only command (`export-graph`) must not migrate the database it
/// reads, so it checks instead.
///
/// # Errors
///
/// [`StoreError::Database`] when the ledger cannot be read.
pub async fn schema_gap(pool: &PgPool) -> Result<Option<String>> {
    let present: bool = sqlx::query_scalar("SELECT to_regclass($1) IS NOT NULL")
        .bind(LEDGER.table)
        .fetch_one(pool)
        .await?;
    if !present {
        return Ok(Some(format!(
            "the Inventory graph store is not set up in this database ({} is missing)",
            LEDGER.table
        )));
    }
    let applied: Vec<String> = sqlx::query_scalar(&format!("SELECT version FROM {}", LEDGER.table))
        .fetch_all(pool)
        .await?;
    let missing: Vec<String> = embedded()?
        .into_iter()
        .filter(|migration| !applied.contains(&migration.version))
        .map(|migration| format!("{}_{}", migration.version, migration.name))
        .collect();
    Ok((!missing.is_empty()).then(|| {
        format!(
            "the Inventory graph store's schema is behind this engine (not applied: {})",
            missing.join(", ")
        )
    }))
}

fn ordinal(index: usize) -> Result<i32> {
    i32::try_from(index)
        .map_err(|_| StoreError::Unstorable("a graph holds at most 2^31 rows".to_owned()))
}

/// An entity row: id, ordinal, attributes, citations, embedding.
type EntityRow = (
    String,
    i32,
    Json<Value>,
    Option<Json<Value>>,
    Option<Vec<f64>>,
);
/// A relation row: source, target, ordinal within the source, attributes.
type RelationRow = (String, String, i32, Json<Value>);

/// The rows of `graph`: citations and embedding split off each node,
/// edges numbered within their source.
fn rows(graph: &Graph) -> Result<(Vec<EntityRow>, Vec<RelationRow>)> {
    let mut entities = Vec::with_capacity(graph.node_count());
    for (index, (id, node)) in graph.nodes().enumerate() {
        let mut attributes = node.clone();
        let citations = attributes.shift_remove("citations");
        let embedding = match attributes.shift_remove("embedding") {
            None | Some(Value::Null) => None,
            Some(Value::Array(values)) => Some(
                values
                    .iter()
                    .map(Value::as_f64)
                    .collect::<Option<Vec<f64>>>()
                    .ok_or_else(|| {
                        StoreError::Unstorable(format!(
                            "entity {id}: the embedding is not a list of numbers"
                        ))
                    })?,
            ),
            Some(_) => {
                return Err(StoreError::Unstorable(format!(
                    "entity {id}: the embedding is not a list"
                )));
            }
        };
        entities.push((
            id.to_owned(),
            ordinal(index)?,
            Json(Value::Object(attributes)),
            citations.map(Json),
            embedding,
        ));
    }
    let mut position_in_source: std::collections::HashMap<&str, usize> =
        std::collections::HashMap::new();
    let mut relations = Vec::with_capacity(graph.edge_count());
    for (source, target, attributes) in graph.edges() {
        let position = position_in_source.entry(source).or_insert(0);
        relations.push((
            source.to_owned(),
            target.to_owned(),
            ordinal(*position)?,
            Json(Value::Object(attributes.clone())),
        ));
        *position += 1;
    }
    Ok((entities, relations))
}

/// Replace the stored graph `key` with `graph`, in one transaction.
/// Returns the graph's new revision.
///
/// # Errors
///
/// [`StoreError::Unstorable`] for a graph PostgreSQL cannot hold (an
/// embedding that is not a list of numbers; text with a NUL character,
/// which `jsonb` refuses), [`StoreError::Database`] otherwise. Nothing is
/// written on an error.
pub async fn save(pool: &PgPool, key: GraphKey, graph: &Graph) -> Result<i64> {
    let mut transaction = pool.begin().await?;
    let revision = write_graph(&mut transaction, key, graph).await?;
    transaction.commit().await?;
    Ok(revision)
}

/// [`save`] inside a caller's transaction (which commits it): takes the
/// graph's write lock, replaces its rows, returns the new revision.
///
/// # Errors
///
/// See [`save`].
pub async fn write_graph(
    transaction: &mut sqlx::PgTransaction<'_>,
    key: GraphKey,
    graph: &Graph,
) -> Result<i64> {
    let (entities, relations) = rows(graph)?;
    let mut metadata = graph.metadata.clone();
    for stamped in STAMPED_METADATA {
        metadata.shift_remove(stamped);
    }
    lock(transaction, key).await?;
    let revision: i64 = sqlx::query_scalar(
        "INSERT INTO inventory_graph.graphs
             (project_id, application_id, attributes, metadata, schema_document)
         VALUES ($1, $2, $3, $4, $5)
         ON CONFLICT (project_id, application_id) DO UPDATE SET
             attributes = EXCLUDED.attributes,
             metadata = EXCLUDED.metadata,
             schema_document = EXCLUDED.schema_document,
             revision = inventory_graph.graphs.revision + 1,
             updated_at = clock_timestamp()
         RETURNING revision",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .bind(Json(Value::Object(graph.attributes.clone())))
    .bind(Json(Value::Object(metadata)))
    .bind(graph.schema.clone().map(Json))
    .fetch_one(&mut **transaction)
    .await
    .map_err(unstorable)?;
    // Relations go with their entities (ON DELETE CASCADE).
    sqlx::query(
        "DELETE FROM inventory_graph.entities WHERE project_id = $1 AND application_id = $2",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .execute(&mut **transaction)
    .await?;
    insert_entities(transaction, key, &entities).await?;
    insert_relations(transaction, key, &relations).await?;
    Ok(revision)
}

async fn insert_entities(
    transaction: &mut sqlx::PgTransaction<'_>,
    key: GraphKey,
    entities: &[EntityRow],
) -> Result<()> {
    for batch in entities.chunks(BATCH_ROWS) {
        let mut insert = QueryBuilder::new(
            "INSERT INTO inventory_graph.entities
                 (project_id, application_id, entity_id, ordinal, attributes, citations, embedding) ",
        );
        insert.push_values(batch, |mut row, entity| {
            row.push_bind(key.project_id)
                .push_bind(key.application_id)
                .push_bind(&entity.0)
                .push_bind(entity.1)
                .push_bind(&entity.2)
                .push_bind(&entity.3)
                .push_bind(&entity.4);
        });
        insert
            .build()
            .execute(&mut **transaction)
            .await
            .map_err(unstorable)?;
    }
    Ok(())
}

async fn insert_relations(
    transaction: &mut sqlx::PgTransaction<'_>,
    key: GraphKey,
    relations: &[RelationRow],
) -> Result<()> {
    for batch in relations.chunks(BATCH_ROWS) {
        let mut insert = QueryBuilder::new(
            "INSERT INTO inventory_graph.relations
                 (project_id, application_id, source_id, target_id, ordinal, attributes) ",
        );
        insert.push_values(batch, |mut row, relation| {
            row.push_bind(key.project_id)
                .push_bind(key.application_id)
                .push_bind(&relation.0)
                .push_bind(&relation.1)
                .push_bind(relation.2)
                .push_bind(&relation.3);
        });
        insert
            .build()
            .execute(&mut **transaction)
            .await
            .map_err(unstorable)?;
    }
    Ok(())
}

/// Serialise writers of one graph (released at commit or rollback).
async fn lock(transaction: &mut sqlx::PgTransaction<'_>, key: GraphKey) -> Result<()> {
    sqlx::query(
        "SELECT pg_advisory_xact_lock(hashtext('elitea_inventory.graph'), hashtext($1 || '/' || $2))",
    )
    .bind(key.project_id.to_string())
    .bind(key.application_id.to_string())
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

/// `jsonb` refuses `\u0000` (SQLSTATE 22P05); that is the graph's content,
/// not the database failing, so it is said as such.
fn unstorable(error: sqlx::Error) -> StoreError {
    let code = error
        .as_database_error()
        .and_then(sqlx::error::DatabaseError::code)
        .map(std::borrow::Cow::into_owned);
    if code.as_deref() == Some("22P05") {
        StoreError::Unstorable(
            "the graph holds text with a NUL character, which PostgreSQL cannot store".to_owned(),
        )
    } else {
        StoreError::Database(error)
    }
}

/// The stored graph `key` and its revision, or `None` when there is none.
///
/// # Errors
///
/// [`StoreError::Database`].
pub async fn load(pool: &PgPool, key: GraphKey) -> Result<Option<(Graph, i64)>> {
    // One snapshot for the three reads: a concurrent save is all or nothing.
    let mut transaction = pool.begin().await?;
    sqlx::query("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY")
        .execute(&mut *transaction)
        .await?;
    let Some(header) = sqlx::query(
        "SELECT attributes, metadata, schema_document, revision
           FROM inventory_graph.graphs
          WHERE project_id = $1 AND application_id = $2",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .fetch_optional(&mut *transaction)
    .await?
    else {
        return Ok(None);
    };
    let mut graph = Graph::new();
    graph.attributes = object(header.try_get::<Json<Value>, _>("attributes")?.0);
    graph.metadata = object(header.try_get::<Json<Value>, _>("metadata")?.0);
    graph.schema = header
        .try_get::<Option<Json<Value>>, _>("schema_document")?
        .map(|schema| schema.0);
    let revision: i64 = header.try_get("revision")?;

    let entities = sqlx::query(
        "SELECT entity_id, attributes, citations, embedding
           FROM inventory_graph.entities
          WHERE project_id = $1 AND application_id = $2
          ORDER BY ordinal",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .fetch_all(&mut *transaction)
    .await?;
    for row in entities {
        let mut attributes = object(row.try_get::<Json<Value>, _>("attributes")?.0);
        if let Some(citations) = row.try_get::<Option<Json<Value>>, _>("citations")? {
            attributes.insert("citations".to_owned(), citations.0);
        }
        if let Some(embedding) = row.try_get::<Option<Vec<f64>>, _>("embedding")? {
            attributes.insert("embedding".to_owned(), Value::from(embedding));
        }
        graph.insert_node(row.try_get("entity_id")?, attributes);
    }

    let relations = sqlx::query(
        "SELECT r.source_id, r.target_id, r.attributes
           FROM inventory_graph.relations r
           JOIN inventory_graph.entities s
             ON s.project_id = r.project_id
            AND s.application_id = r.application_id
            AND s.entity_id = r.source_id
          WHERE r.project_id = $1 AND r.application_id = $2
          ORDER BY s.ordinal, r.ordinal",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .fetch_all(&mut *transaction)
    .await?;
    for row in relations {
        let source: String = row.try_get("source_id")?;
        let target: String = row.try_get("target_id")?;
        graph.insert_edge(
            &source,
            &target,
            object(row.try_get::<Json<Value>, _>("attributes")?.0),
        );
    }
    transaction.commit().await?;
    Ok(Some((graph, revision)))
}

/// The stored graph's revision, or `None` when there is no graph: a
/// cheap check of whether a cached copy is still current.
///
/// # Errors
///
/// [`StoreError::Database`].
pub async fn revision(pool: &PgPool, key: GraphKey) -> Result<Option<i64>> {
    Ok(sqlx::query_scalar(
        "SELECT revision FROM inventory_graph.graphs WHERE project_id = $1 AND application_id = $2",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .fetch_optional(pool)
    .await?)
}

/// Delete the stored graph `key` and its sources' state; `true` when there
/// was a graph.
///
/// # Errors
///
/// [`StoreError::Database`].
pub async fn delete(pool: &PgPool, key: GraphKey) -> Result<bool> {
    let mut transaction = pool.begin().await?;
    lock(&mut transaction, key).await?;
    let deleted = sqlx::query(
        "DELETE FROM inventory_graph.graphs WHERE project_id = $1 AND application_id = $2",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .execute(&mut *transaction)
    .await?
    .rows_affected();
    for table in ["inventory_graph.sources", "inventory_graph.documents"] {
        sqlx::query(&format!(
            "DELETE FROM {table} WHERE project_id = $1 AND application_id = $2"
        ))
        .bind(key.project_id)
        .bind(key.application_id)
        .execute(&mut *transaction)
        .await?;
    }
    transaction.commit().await?;
    Ok(deleted > 0)
}

/// What [`import`] did.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Imported {
    /// The graph was saved at this revision.
    Saved { revision: i64 },
    /// Nothing was written: native ingestion state exists for the graph
    /// (source status rows, document versions) and replacing it was not
    /// asked for.
    HasIngestionState { sources: i64, documents: i64 },
}

/// Store an imported graph (`import-graph`) in one transaction: refused
/// while the graph has native ingestion state, unless `replace_state`, which
/// deletes that state with the old graph. Without the refusal a re-import
/// over a natively ingested graph would keep document versions that claim
/// files the new graph never read (the next run would skip them) and ACL
/// rows for documents it does not hold.
///
/// # Errors
///
/// See [`save`].
pub async fn import(
    pool: &PgPool,
    key: GraphKey,
    graph: &Graph,
    replace_state: bool,
) -> Result<Imported> {
    let mut transaction = pool.begin().await?;
    lock(&mut transaction, key).await?;
    let mut counts = [0i64; 2];
    for (count, table) in counts
        .iter_mut()
        .zip(["inventory_graph.sources", "inventory_graph.documents"])
    {
        *count = sqlx::query_scalar(&format!(
            "SELECT count(*) FROM {table} WHERE project_id = $1 AND application_id = $2"
        ))
        .bind(key.project_id)
        .bind(key.application_id)
        .fetch_one(&mut *transaction)
        .await?;
        if replace_state {
            sqlx::query(&format!(
                "DELETE FROM {table} WHERE project_id = $1 AND application_id = $2"
            ))
            .bind(key.project_id)
            .bind(key.application_id)
            .execute(&mut *transaction)
            .await?;
        }
    }
    if !replace_state && counts.iter().any(|count| *count > 0) {
        return Ok(Imported::HasIngestionState {
            sources: counts[0],
            documents: counts[1],
        });
    }
    let revision = write_graph(&mut transaction, key, graph).await?;
    transaction.commit().await?;
    Ok(Imported::Saved { revision })
}

fn object(value: Value) -> Map<String, Value> {
    match value {
        Value::Object(fields) => fields,
        _ => Map::new(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn the_embedded_set_is_the_directory_and_valid() {
        let mut on_disk: Vec<String> =
            std::fs::read_dir(concat!(env!("CARGO_MANIFEST_DIR"), "/migrations"))
                .map(|entries| {
                    entries
                        .filter_map(|entry| entry.ok()?.file_name().into_string().ok())
                        .filter(|name| {
                            std::path::Path::new(name)
                                .extension()
                                .is_some_and(|ext| ext.eq_ignore_ascii_case("sql"))
                        })
                        .collect()
                })
                .unwrap_or_default();
        on_disk.sort();
        assert_eq!(on_disk, embedded_file_names());
        assert!(embedded().is_ok_and(|migrations| !migrations.is_empty()));
    }

    #[test]
    fn keys_are_positive_integers_from_numbers_or_digit_strings() {
        let key = |arguments: Value| {
            GraphKey::from_arguments(arguments.as_object().unwrap_or(&Map::new()))
                .map_err(StoreError::from)
        };
        assert_eq!(
            key(json!({"project_id": 3, "application_id": "17"})).ok(),
            GraphKey::new(3, 17).ok()
        );
        for refused in [
            json!({"project_id": 3}),
            json!({"project_id": "x", "application_id": 1}),
            json!({"project_id": 0, "application_id": 1}),
            json!({"project_id": 1.5, "application_id": 1}),
        ] {
            assert!(
                matches!(key(refused.clone()), Err(StoreError::InvalidKey(_))),
                "{refused}"
            );
        }
    }

    #[test]
    fn a_bad_dsn_is_refused_without_its_text() {
        let Err(error) = connect_options("postgresql://user:hunter2@[bad") else {
            panic!("refused");
        };
        assert!(!error.to_string().contains("hunter2"));
    }
}
