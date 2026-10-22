//! A graph's entity vectors in Qdrant, behind elitea-vector (#1359): the
//! upsert every graph write makes, the ranking, the backfill of a graph whose
//! vectors are not there yet, and the clean-ups.
//!
//! Only the ranking is here. Which ranked entities a caller sees (the ACL
//! view), the type, layer and file filters, the rounding and the answer's
//! wording stay with retrieval (`retrieval::semantic`).
//!
//! # Where a graph's vectors are
//!
//! * **Source** `inventory`: elitea-vector keeps it in its own collection
//!   family, `inv_<model_slug>_<dimension>`, float32, no quantization, no
//!   BM25, searched by an exact scan, so a score IS the exact cosine
//!   similarity (to float32 precision, within 1e-6).
//! * **Namespace**: [`namespace_id`] — the `UUIDv5`, under
//!   [`NAMESPACE_ROOT`], of the name `"<project_id>/<application_id>"`
//!   (ASCII decimal). One graph, one namespace. elitea-main computes the same
//!   id to delete a deleted toolkit's vectors as elitea-vector's
//!   administrator (`services/elitea-main/internal/infra/vectorclient/
//!   inventory.go`, `InventoryGraphNamespace`); the two are pinned to the same
//!   vectors by a test on each side.
//! * **Generation**: a fresh UUID drawn by PostgreSQL for every write. The
//!   graph row records the generation its rows belong to
//!   (`graphs.vector_generation`, migration 0008); a ranking searches that
//!   generation only, so a write is all or nothing for a reader too.
//! * **Space**: [`model_slug`] of the graph's `_metadata.embeddings_model`
//!   stamp, and the vectors' width.
//! * **Point**: one per entity whose vector is non-empty and of non-zero
//!   norm (a zero vector has no cosine; it never ranked). `document_key` and
//!   `chunk_id` are the entity id when elitea-vector can hold it as a key (1
//!   to 1024 bytes, no control character), else `sha256:<hex of the id>`;
//!   `metadata_json` is always `{"entity_id": <id>}`, which a ranking reads.
//!
//! # A write
//!
//! [`stage`] runs BEFORE the write's transaction opens, so no transaction is
//! open and no lock held across the call to elitea-vector: it upserts the
//! graph's points under a new generation. A failed upsert fails the write
//! before anything is written. Inside the transaction, under the graph's
//! write lock, [`record`] makes that generation the graph row's and appends
//! the one it replaces to `vector_stale_generations`. A committed generation
//! is therefore always complete in Qdrant, and a generation nobody committed
//! is never searched. After the transaction [`settle`] deletes the staged
//! generation when the write did not commit it, then sweeps: it deletes
//! exactly the stale generations the row lists ([`EntityVectors::sweep`]),
//! never "every generation but the current one", which would delete a
//! concurrent writer's staged points before that writer commits. A sweep
//! that fails is logged and repeated by the next write's. A staged
//! generation outlives its write only if the process dies between the
//! upsert and the clean-up; it is never searched, and the toolkit's
//! deletion removes it with the namespace. A write made WITHOUT vector
//! access (the `import-graph` command, which has no invocation bearer)
//! records no generation, and its graph is backfilled by the first ranking.
//!
//! # A ranking
//!
//! [`rank`] keeps the semantics of the PostgreSQL ranking it replaces: a zero
//! query vector ranks nothing; a query whose width differs from the graph's
//! (or a graph of mixed widths) is numpy's `shapes … not aligned` message,
//! decided from the graph row before Qdrant is called; the entities scoring at
//! least `min_score`, best first, ties by entity id. elitea-vector orders
//! equal scores by point id, so the ties are re-ordered here. The pages are
//! read until one comes back short; [`MAX_RANKED`] (elitea-vector's search
//! depth) bounds the ranking: past it the best [`MAX_RANKED`] are returned
//! and a warning is logged (`semantic_search` keeps at most 50 after its
//! filters; graphs today hold hundreds to low thousands of entities).
//!
//! # The backfill
//!
//! A graph without a recorded generation (a database upgraded from pgvector,
//! or a write without vector access) is published from `entities.embedding`
//! by the first ranking, which has the invocation's bearer. It reads the
//! vectors and the graph's revision in one snapshot, stages them like a
//! write, then, under the graph's write lock (`store::lock`, the lock every
//! writer and every deletion takes), records the generation only when the
//! row still has none at the same revision. Otherwise (another ranking
//! published first, or the graph was rewritten meanwhile) its staged points
//! are deleted and the row is read again. A deletion either precedes it (no
//! row: nothing to backfill) or follows it. The revision does not move: the
//! graph did not change.

use super::{GraphKey, Result, StoreError, lock};
use elitea_vector_index::client::{
    ClaimToken, DeleteSelector, MAX_LIMIT, MAX_SEARCH_DEPTH, SearchParams, VectorClient,
    inventory_namespace,
};
use elitea_vector_index::pb;
use sha1::Digest as _;
use sqlx::postgres::PgPool;
use std::fmt::Write as _;
use std::sync::Arc;

/// A ranking (the shared core's type, ADR-0029 decision 7).
pub use elitea_inventory_core::store::Ranking;

/// The `UUIDv5` namespace of every graph's vector namespace:
/// `UUIDv5(NAMESPACE_URL, "https://elitea.ai/ns/inventory/graph-vectors")`
/// = `634fdd4c-7b6b-5648-80b0-46de40560443`. Never change it: every stored
/// vector is addressed under it, and elitea-main computes the same ids.
pub const NAMESPACE_ROOT: [u8; 16] = [
    0x63, 0x4f, 0xdd, 0x4c, 0x7b, 0x6b, 0x56, 0x48, 0x80, 0xb0, 0x46, 0xde, 0x40, 0x56, 0x04, 0x43,
];

/// The most entities one ranking returns: elitea-vector's search depth.
pub const MAX_RANKED: u32 = MAX_SEARCH_DEPTH;

/// elitea-vector's model-slug limit (`^[a-z0-9][a-z0-9-]{0,62}$`).
const MAX_SLUG_BYTES: usize = 63;
/// How much of the model name's SHA-256 a slug carries.
const SLUG_HASH_HEX: usize = 12;
/// The longest a point key may be (elitea-vector's `MAX_KEY_BYTES`).
const MAX_KEY_BYTES: usize = 1024;

/// The vector namespace of the graph `key`: the canonical lower-case text of
/// `UUIDv5(NAMESPACE_ROOT, "<project_id>/<application_id>")`.
#[must_use]
pub fn namespace_id(key: GraphKey) -> String {
    let mut hasher = sha1::Sha1::new();
    hasher.update(NAMESPACE_ROOT);
    hasher.update(format!("{}/{}", key.project_id, key.application_id).as_bytes());
    let digest = hasher.finalize();
    let mut bytes = [0u8; 16];
    bytes.copy_from_slice(&digest[..16]);
    bytes[6] = (bytes[6] & 0x0f) | 0x50;
    bytes[8] = (bytes[8] & 0x3f) | 0x80;
    let mut text = String::with_capacity(36);
    for (index, byte) in bytes.iter().enumerate() {
        if matches!(index, 4 | 6 | 8 | 10) {
            text.push('-');
        }
        let _ = write!(text, "{byte:02x}");
    }
    text
}

/// The embedding space's model slug of the model name `model` (the graph's
/// `_metadata.embeddings_model` stamp; `""` for an unstamped graph): the
/// platform's rule, byte for byte the indexing worker's
/// (`elitea-worker-rust/src/indexing/slug.rs`) — the lower-cased name's runs
/// of `[a-z0-9]` joined by single dashes, cut to 50 bytes (`model` when
/// nothing is left), then `-` and the first 12 hex digits of the SHA-256 of
/// the name as given. The hash keeps two names that read alike apart.
#[must_use]
pub fn model_slug(model: &str) -> String {
    let mut readable = String::with_capacity(model.len());
    let mut pending_dash = false;
    for character in model.to_lowercase().chars() {
        if character.is_ascii_lowercase() || character.is_ascii_digit() {
            if pending_dash && !readable.is_empty() {
                readable.push('-');
            }
            pending_dash = false;
            readable.push(character);
        } else {
            pending_dash = true;
        }
    }
    let room = MAX_SLUG_BYTES - 1 - SLUG_HASH_HEX;
    if readable.len() > room {
        readable.truncate(room);
        while readable.ends_with('-') {
            readable.pop();
        }
    }
    if readable.is_empty() {
        readable.push_str("model");
    }
    let digest = sha2::Sha256::digest(model.as_bytes());
    let mut slug = readable;
    slug.push('-');
    for byte in digest.iter().take(SLUG_HASH_HEX / 2) {
        let _ = write!(slug, "{byte:02x}");
    }
    slug
}

/// The point key of the entity `id`: the id itself when elitea-vector can
/// hold it as a key, else `sha256:<hex>` of it.
#[must_use]
pub fn point_key(id: &str) -> String {
    if !id.is_empty() && id.len() <= MAX_KEY_BYTES && !id.chars().any(char::is_control) {
        return id.to_owned();
    }
    let mut key = String::from("sha256:");
    for byte in sha2::Sha256::digest(id.as_bytes()) {
        let _ = write!(key, "{byte:02x}");
    }
    key
}

/// Access to elitea-vector for one invocation: the process's client and the
/// invocation's callback bearer (elitea-main admits it for the invocation's
/// project, source `inventory`). Cloning is cheap. Its `Debug` never shows
/// the bearer.
#[derive(Clone)]
pub struct EntityVectors {
    client: VectorClient,
    bearer: Arc<Bearer>,
}

struct Bearer(String);

impl ClaimToken for Bearer {
    fn bearer(&self) -> &str {
        &self.0
    }
}

impl std::fmt::Debug for EntityVectors {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("EntityVectors")
            .finish_non_exhaustive()
    }
}

/// What a graph's vectors are, as the graph row records them.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VectorSpace {
    pub model_slug: String,
    /// The width of the first non-empty vector (by ordinal); `None`: none.
    pub dimension: Option<i32>,
    /// Another width some entity has, when the widths differ.
    pub other_dimension: Option<i32>,
}

impl VectorSpace {
    /// The space of a graph stamped with `model` whose non-empty vectors, in
    /// ordinal order, have these widths.
    #[must_use]
    pub fn of(model: Option<&str>, widths: impl IntoIterator<Item = usize>) -> Self {
        let mut dimension = None;
        let mut other_dimension = None;
        for width in widths.into_iter().filter(|width| *width > 0) {
            let width = i32::try_from(width).unwrap_or(i32::MAX);
            match dimension {
                None => dimension = Some(width),
                Some(first) if first != width && other_dimension.is_none() => {
                    other_dimension = Some(width);
                }
                Some(_) => {}
            }
        }
        Self {
            model_slug: model_slug(model.unwrap_or_default()),
            dimension,
            other_dimension,
        }
    }

    fn embedding_space(&self) -> Option<pb::EmbeddingSpace> {
        let dimension = u32::try_from(self.dimension?).ok()?;
        Some(pb::EmbeddingSpace {
            model_slug: self.model_slug.clone(),
            dimension,
        })
    }
}

/// One entity's vector, as written.
pub(super) struct EntityVector<'a> {
    pub id: &'a str,
    pub vector: &'a [f64],
}

fn vectors_failed(what: &str, error: &elitea_vector_index::Error) -> StoreError {
    StoreError::Vectors(format!("elitea-vector {what} failed: {error}"))
}

impl EntityVectors {
    /// Access over `client` with the invocation's `bearer`.
    #[must_use]
    pub fn new(client: VectorClient, bearer: impl Into<String>) -> Self {
        Self {
            client,
            bearer: Arc::new(Bearer(bearer.into())),
        }
    }

    /// Upsert `entities` (those of non-zero norm, in the space's width) of
    /// the graph `key` under `generation`.
    async fn upsert(
        &self,
        key: GraphKey,
        space: &VectorSpace,
        generation: &str,
        entities: &[EntityVector<'_>],
    ) -> Result<()> {
        let Some(embedding_space) = space.embedding_space() else {
            return Ok(());
        };
        let width = embedding_space.dimension as usize;
        let points: Vec<pb::Point> = entities
            .iter()
            .filter(|entity| entity.vector.len() == width)
            .filter(|entity| entity.vector.iter().any(|value| *value != 0.0))
            .map(|entity| {
                let key = point_key(entity.id);
                #[allow(clippy::cast_possible_truncation)] // float32 is the family's type
                let vector = entity.vector.iter().map(|value| *value as f32).collect();
                pb::Point {
                    document_key: key.clone(),
                    chunk_id: key,
                    metadata_json: serde_json::json!({"entity_id": entity.id}).to_string(),
                    vector,
                    ..pb::Point::default()
                }
            })
            .collect();
        if points.is_empty() {
            return Ok(());
        }
        let mut namespace = inventory_namespace(&namespace_id(key));
        namespace.generation = generation.to_owned();
        self.client
            .upsert(&*self.bearer, embedding_space, namespace, points)
            .await
            .map(|_| ())
            .map_err(|error| vectors_failed("upsert", &error))
    }

    /// Delete the points of `generations` of `key`'s namespace.
    async fn delete_generations(&self, key: GraphKey, generations: Vec<String>) -> Result<()> {
        self.client
            .delete(
                &*self.bearer,
                None,
                inventory_namespace(&namespace_id(key)),
                DeleteSelector::Generations(generations),
            )
            .await
            .map(|_| ())
            .map_err(|error| vectors_failed("delete", &error))
    }

    /// Delete the stale generations the graph row of `key` lists (those a
    /// commit superseded), and remove them from the list. A failure is
    /// logged, not returned: the write is committed, and the next write's
    /// sweep removes what this one left (a ranking never searches a stale
    /// generation).
    pub async fn sweep(&self, pool: &PgPool, key: GraphKey) {
        if let Err(error) = self.sweep_stale(pool, key).await {
            tracing::warn!(
                project_id = key.project_id,
                application_id = key.application_id,
                %error,
                "the graph's superseded entity vectors were not removed; the next write removes them"
            );
        }
    }

    async fn sweep_stale(&self, pool: &PgPool, key: GraphKey) -> Result<()> {
        let stale: Option<Vec<String>> = sqlx::query_scalar(
            "SELECT vector_stale_generations FROM inventory_graph.graphs
              WHERE project_id = $1 AND application_id = $2",
        )
        .bind(key.project_id)
        .bind(key.application_id)
        .fetch_optional(pool)
        .await?;
        for batch in stale.unwrap_or_default().chunks(SWEEP_BATCH) {
            self.delete_generations(key, batch.to_vec()).await?;
            sqlx::query(
                "UPDATE inventory_graph.graphs
                    SET vector_stale_generations = ARRAY(
                            SELECT generation FROM unnest(vector_stale_generations) AS generation
                             WHERE NOT generation = ANY($3))
                  WHERE project_id = $1 AND application_id = $2",
            )
            .bind(key.project_id)
            .bind(key.application_id)
            .bind(batch)
            .execute(pool)
            .await?;
        }
        Ok(())
    }

    /// Remove every vector of `key`'s namespace (a deleted graph).
    ///
    /// # Errors
    ///
    /// [`StoreError::Vectors`].
    pub async fn remove_all(&self, key: GraphKey) -> Result<()> {
        self.client
            .delete(
                &*self.bearer,
                None,
                inventory_namespace(&namespace_id(key)),
                DeleteSelector::WholeNamespace,
            )
            .await
            .map(|_| ())
            .map_err(|error| vectors_failed("delete", &error))
    }

    /// The ranking of `vector` over the generation `generation` of `key` in
    /// `space`: every point scoring at least `min_score`, best first, ties by
    /// entity id.
    async fn search(
        &self,
        key: GraphKey,
        space: pb::EmbeddingSpace,
        generation: &str,
        vector: &[f64],
        min_score: f64,
    ) -> Result<Vec<(String, f64)>> {
        let namespace = namespace_id(key);
        let mut scope = elitea_vector_index::client::inventory_scope(vec![namespace.clone()]);
        scope.active_generations = vec![pb::ActiveGeneration {
            namespace_id: namespace,
            generation: generation.to_owned(),
        }];
        #[allow(clippy::cast_possible_truncation)] // float32 is the family's type
        let query: Vec<f32> = vector.iter().map(|value| *value as f32).collect();
        #[allow(clippy::cast_possible_truncation)]
        let threshold = min_score as f32;
        let mut ranked: Vec<(String, f64)> = Vec::new();
        let mut offset = 0u32;
        loop {
            let page = self
                .client
                .search_page(
                    &*self.bearer,
                    SearchParams {
                        scope: scope.clone(),
                        space: space.clone(),
                        vector: query.clone(),
                        filter: None,
                        limit: MAX_LIMIT,
                        score_threshold: Some(threshold),
                    },
                    offset,
                )
                .await
                .map_err(|error| vectors_failed("search", &error))?;
            let full = page.len() >= MAX_LIMIT as usize;
            ranked.extend(page.into_iter().map(|point| {
                let id = serde_json::from_str::<serde_json::Value>(&point.metadata_json)
                    .ok()
                    .and_then(|metadata| {
                        metadata
                            .get("entity_id")
                            .and_then(serde_json::Value::as_str)
                            .map(str::to_owned)
                    })
                    .unwrap_or(point.document_key);
                (id, f64::from(point.score))
            }));
            offset += MAX_LIMIT;
            if !full {
                break;
            }
            if offset >= MAX_RANKED {
                tracing::warn!(
                    project_id = key.project_id,
                    application_id = key.application_id,
                    limit = MAX_RANKED,
                    "more entities of the graph reach min_score than one ranking returns; the best are kept"
                );
                break;
            }
        }
        // float32 rounding can put a score a hair under min_score past the
        // float32 threshold; the contract is the float64 comparison.
        ranked.retain(|(_, score)| *score >= min_score);
        // elitea-vector orders a tie by point id; the contract is entity id.
        ranked.sort_by(|left, right| {
            right
                .1
                .total_cmp(&left.1)
                .then_with(|| left.0.cmp(&right.0))
        });
        Ok(ranked)
    }
}

/// How many stale generations one delete names (elitea-vector takes up to
/// 1024).
const SWEEP_BATCH: usize = 256;

/// A graph's vectors upserted ahead of its write ([`stage`]).
#[derive(Debug)]
pub struct Staged {
    /// The new generation; `None` for a write without vector access.
    generation: Option<String>,
    space: VectorSpace,
}

/// Upsert the graph `key`'s vectors under a new generation, before the
/// write's transaction opens (see the module documentation). Without
/// `vectors` nothing is upserted and the write records no generation (the
/// first ranking backfills).
///
/// # Errors
///
/// [`StoreError::Vectors`] for a failed upsert (the write must not go on),
/// [`StoreError::Database`].
pub(super) async fn stage(
    pool: &PgPool,
    key: GraphKey,
    space: VectorSpace,
    entities: &[EntityVector<'_>],
    vectors: Option<&EntityVectors>,
) -> Result<Staged> {
    let generation = match vectors {
        Some(vectors) => {
            let generation: String = sqlx::query_scalar("SELECT gen_random_uuid()::text")
                .fetch_one(pool)
                .await?;
            vectors.upsert(key, &space, &generation, entities).await?;
            Some(generation)
        }
        None => None,
    };
    Ok(Staged { generation, space })
}

/// Make `staged` the graph row's generation, inside the write's transaction
/// (the graph's write lock is held), and list the one it replaces as stale.
/// The space is recorded either way.
///
/// # Errors
///
/// [`StoreError::Database`].
pub(super) async fn record(
    transaction: &mut sqlx::PgTransaction<'_>,
    key: GraphKey,
    staged: &Staged,
) -> Result<()> {
    // Every right-hand side reads the row as it was.
    sqlx::query(
        "UPDATE inventory_graph.graphs
            SET vector_stale_generations = CASE
                    WHEN vector_generation IS NULL OR vector_generation = $3
                        THEN vector_stale_generations
                    ELSE array_append(vector_stale_generations, vector_generation)
                END,
                vector_generation = $3, vector_model_slug = $4,
                vector_dimension = $5, vector_other_dimension = $6
          WHERE project_id = $1 AND application_id = $2",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .bind(&staged.generation)
    .bind(&staged.space.model_slug)
    .bind(staged.space.dimension)
    .bind(staged.space.other_dimension)
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

/// After a write's transaction ended: delete the staged generation unless
/// the graph row holds it, then sweep the stale ones. `committed` is the
/// transaction's outcome as the caller saw it; a commit reported as failed
/// may still have committed, so the row decides before anything is
/// deleted. Never fails: what it cannot delete is logged, never searched,
/// and removed with the toolkit's namespace.
pub(super) async fn settle(
    pool: &PgPool,
    key: GraphKey,
    staged: &Staged,
    vectors: Option<&EntityVectors>,
    committed: bool,
) {
    let Some(vectors) = vectors else {
        return;
    };
    if let Some(generation) = &staged.generation
        && !committed
    {
        let held = match vector_row(pool, key).await {
            Ok(row) => row.and_then(|row| row.0).as_deref() == Some(generation.as_str()),
            // Unknown: keep the points rather than delete a live generation.
            Err(_) => true,
        };
        if !held
            && let Err(error) = vectors
                .delete_generations(key, vec![generation.clone()])
                .await
        {
            tracing::warn!(
                project_id = key.project_id,
                application_id = key.application_id,
                %error,
                "the entity vectors of a write that did not commit were not removed; they are never searched, and the toolkit's deletion removes them"
            );
        }
    }
    vectors.sweep(pool, key).await;
}

/// The graph row's vector columns.
type VectorRow = (Option<String>, Option<String>, Option<i32>, Option<i32>);

/// The entities of `key` whose vector is at least `min_score` similar to
/// `vector`, best first (ties by entity id, so the order is stable). See the
/// module documentation.
///
/// A query vector of zero length or zero norm ranks nothing, as numpy's
/// division guard did; so does an entity vector of zero norm.
///
/// # Errors
///
/// [`StoreError::Database`] for a failed query; [`StoreError::Vectors`] when
/// elitea-vector fails, or when the graph has vectors to search and no
/// `vectors` access was given.
pub async fn rank(
    pool: &PgPool,
    key: GraphKey,
    vector: &[f64],
    min_score: f64,
    vectors: Option<&EntityVectors>,
) -> Result<Ranking> {
    if vector.iter().all(|value| *value == 0.0) {
        return Ok(Ok(Vec::new()));
    }
    let Some(mut row) = vector_row(pool, key).await? else {
        return Ok(Ok(Vec::new()));
    };
    // A write that commits while the search runs sweeps the generation being
    // searched: a ranking that finds the graph row moved on searches again,
    // so it never answers from a half-deleted generation (a bounded number
    // of times; a graph rewritten that often answers from the last one).
    for _ in 0..RANK_ATTEMPTS {
        if row.0.is_none() {
            let vectors = vectors.ok_or_else(no_access)?;
            match backfill(pool, key, vectors).await? {
                Some(published) => row = published,
                None => return Ok(Ok(Vec::new())),
            }
        }
        let (Some(generation), Some(slug), dimension, other) = row.clone() else {
            return Ok(Ok(Vec::new()));
        };
        let Some(dimension) = dimension else {
            // No entity has a vector.
            return Ok(Ok(Vec::new()));
        };
        let width = i32::try_from(vector.len()).unwrap_or(i32::MAX);
        if let Some(other) = Some(dimension)
            .filter(|dimension| *dimension != width)
            .or(other)
        {
            return Ok(Err(format!(
                "shapes ({width},) and ({other},) not aligned: {width} (dim 0) != {other} (dim 0)"
            )));
        }
        let vectors = vectors.ok_or_else(no_access)?;
        let space = pb::EmbeddingSpace {
            model_slug: slug,
            dimension: u32::try_from(dimension).unwrap_or_default(),
        };
        let ranked = vectors
            .search(key, space, &generation, vector, min_score)
            .await?;
        match vector_row(pool, key).await? {
            Some(now) if now.0.as_deref() != Some(generation.as_str()) => row = now,
            // Unchanged, or the graph was deleted meanwhile (its ranking as
            // it was is still the answer to a call made before).
            _ => return Ok(Ok(ranked)),
        }
    }
    Err(StoreError::Vectors(
        "the graph was rewritten during every attempt to rank it; try again".to_owned(),
    ))
}

/// How many times [`rank`] searches a graph that keeps being rewritten.
const RANK_ATTEMPTS: usize = 3;

fn no_access() -> StoreError {
    StoreError::Vectors(
        "the graph's entity vectors are ranked by elitea-vector, and this call carries no bearer for it".to_owned(),
    )
}

async fn vector_row(pool: &PgPool, key: GraphKey) -> Result<Option<VectorRow>> {
    Ok(sqlx::query_as(
        "SELECT vector_generation, vector_model_slug, vector_dimension, vector_other_dimension
           FROM inventory_graph.graphs
          WHERE project_id = $1 AND application_id = $2",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .fetch_optional(pool)
    .await?)
}

/// Publish the stored vectors of a graph that has no generation (see the
/// module documentation), and return its vector row; `None` when the graph
/// is gone. A graph rewritten while its backfill ran is backfilled again,
/// a bounded number of times.
///
/// # Errors
///
/// [`StoreError::Vectors`], [`StoreError::Database`].
pub async fn backfill(
    pool: &PgPool,
    key: GraphKey,
    vectors: &EntityVectors,
) -> Result<Option<VectorRow>> {
    for _ in 0..RANK_ATTEMPTS {
        match backfill_once(pool, key, vectors).await? {
            Backfill::Done(row) => return Ok(row),
            Backfill::Rewritten => {}
        }
    }
    vector_row(pool, key).await
}

enum Backfill {
    Done(Option<VectorRow>),
    /// The graph was rewritten without vector access while the backfill ran.
    Rewritten,
}

async fn backfill_once(pool: &PgPool, key: GraphKey, vectors: &EntityVectors) -> Result<Backfill> {
    // The vectors and the revision they belong to, in one snapshot.
    let mut read = pool.begin().await?;
    sqlx::query("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY")
        .execute(&mut *read)
        .await?;
    let row: Option<(Option<String>, Option<String>, i64)> = sqlx::query_as(
        "SELECT vector_generation, metadata ->> 'embeddings_model', revision
           FROM inventory_graph.graphs
          WHERE project_id = $1 AND application_id = $2",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .fetch_optional(&mut *read)
    .await?;
    let Some((generation, model, revision)) = row else {
        return Ok(Backfill::Done(None));
    };
    if generation.is_some() {
        // Published meanwhile (another ranking, or a write).
        read.commit().await?;
        return Ok(Backfill::Done(vector_row(pool, key).await?));
    }
    let stored: Vec<(String, Vec<f64>)> = sqlx::query_as(
        "SELECT entity_id, embedding FROM inventory_graph.entities
          WHERE project_id = $1 AND application_id = $2
            AND embedding IS NOT NULL AND cardinality(embedding) > 0
          ORDER BY ordinal",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .fetch_all(&mut *read)
    .await?;
    read.commit().await?;
    let space = VectorSpace::of(model.as_deref(), stored.iter().map(|(_, v)| v.len()));
    let entities: Vec<EntityVector<'_>> = stored
        .iter()
        .map(|(id, vector)| EntityVector { id, vector })
        .collect();
    let staged = stage(pool, key, space, &entities, Some(vectors)).await?;
    let recorded = async {
        let mut transaction = pool.begin().await?;
        lock(&mut transaction, key).await?;
        let now: Option<(Option<String>, i64)> = sqlx::query_as(
            "SELECT vector_generation, revision FROM inventory_graph.graphs
              WHERE project_id = $1 AND application_id = $2",
        )
        .bind(key.project_id)
        .bind(key.application_id)
        .fetch_optional(&mut *transaction)
        .await?;
        let unchanged = matches!(now, Some((None, now)) if now == revision);
        if unchanged {
            record(&mut transaction, key, &staged).await?;
        }
        transaction.commit().await?;
        Ok::<_, StoreError>((unchanged, now))
    }
    .await;
    settle(
        pool,
        key,
        &staged,
        Some(vectors),
        matches!(recorded, Ok((true, _))),
    )
    .await;
    match recorded? {
        (true, _) => {
            tracing::info!(
                project_id = key.project_id,
                application_id = key.application_id,
                entities = entities.len(),
                "the graph's entity vectors were published to elitea-vector (backfill)"
            );
            Ok(Backfill::Done(Some((
                staged.generation,
                Some(staged.space.model_slug),
                staged.space.dimension,
                staged.space.other_dimension,
            ))))
        }
        // Deleted, or published by someone else: their row is the answer.
        (false, None | Some((Some(_), _))) => Ok(Backfill::Done(vector_row(pool, key).await?)),
        (false, Some((None, _))) => Ok(Backfill::Rewritten),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_namespace_is_the_documented_uuid_v5() {
        // Pinned with elitea-main's InventoryGraphNamespace (Go) and Python's
        // uuid.uuid5(uuid.uuid5(uuid.NAMESPACE_URL,
        // "https://elitea.ai/ns/inventory/graph-vectors"), "7/42").
        let key = |project_id, application_id| GraphKey {
            project_id,
            application_id,
        };
        assert_eq!(
            namespace_id(key(1, 1)),
            "09265e87-a97c-53e6-8062-d685fb6cf4ad"
        );
        assert_eq!(
            namespace_id(key(7, 42)),
            "454412b3-aea7-5ecf-aefe-49264f4557d0"
        );
        assert_eq!(
            namespace_id(key(2_147_483_647, i64::MAX)),
            "3111a581-a4af-549c-af63-b217cec52eca"
        );
    }

    #[test]
    fn slugs_follow_the_platform_rule() {
        let slug = model_slug("text-embedding-3-small");
        assert!(slug.starts_with("text-embedding-3-small-"), "{slug}");
        assert_eq!(slug.len(), "text-embedding-3-small-".len() + 12);
        assert!(model_slug("").starts_with("model-"));
        assert_ne!(
            model_slug("A_b"),
            model_slug("a-b"),
            "the hash keeps them apart"
        );
        for model in [
            "",
            "x",
            "Qwen/Qwen3-Embedding-4B",
            &"é".repeat(100),
            &"ab-".repeat(60),
        ] {
            let slug = model_slug(model);
            assert!(slug.len() <= 63, "{slug}");
            assert!(slug.starts_with(|c: char| c.is_ascii_lowercase() || c.is_ascii_digit()));
            assert!(
                slug.chars()
                    .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-')
            );
        }
    }

    #[test]
    fn keys_are_ids_unless_the_store_cannot_hold_them() {
        assert_eq!(point_key("a1b2c3"), "a1b2c3");
        assert!(point_key("").starts_with("sha256:"));
        assert!(point_key("a\nb").starts_with("sha256:"));
        assert!(point_key(&"x".repeat(1025)).starts_with("sha256:"));
        assert_eq!(point_key(&"x".repeat(1024)), "x".repeat(1024));
    }

    #[test]
    fn a_space_records_the_first_width_and_another_one() {
        let space = VectorSpace::of(Some("m"), [0, 4, 4, 0, 5, 6]);
        assert_eq!((space.dimension, space.other_dimension), (Some(4), Some(5)));
        let none = VectorSpace::of(None, [0, 0]);
        assert_eq!((none.dimension, none.other_dimension), (None, None));
    }
}
