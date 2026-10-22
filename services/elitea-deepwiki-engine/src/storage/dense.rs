//! The dense vectors of a wiki: Qdrant behind elitea-vector (ADR-0031
//! `DeepWiki` D1-D3, #1359). PostgreSQL holds no vector any more and needs no
//! `vector` extension.
//!
//! # Where a wiki's vectors are
//!
//! In elitea-vector's shared family (`emb_<model_slug>_<dimension>`: float16,
//! cosine, HNSW), source `SOURCE_DEEPWIKI`:
//!
//! * **namespace** — [`namespace_id`]: `UUIDv5` under [`DEEPWIKI_NAMESPACE`] of
//!   the name `"{project_id}/{wiki_id}"` (the decimal project id, `/`, the
//!   wiki id exactly as the index stores it). The project id has no `/`, so
//!   the name is unambiguous. This is the ONE definition; anything else that
//!   must name a wiki's namespace (elitea-main, a tool) computes it with the
//!   same constant and the same name.
//! * **generation** — the build id ([`generation_of`]): a server-drawn UUID,
//!   which is inside elitea-vector's `[A-Za-z0-9._:-]{1,128}`. A build writes
//!   its vectors under its own generation BEFORE its publish transaction; the
//!   publish records it in `wikis.active_generation` (migration 0008) together
//!   with the nodes and edges, so a reader sees the old generation or the new
//!   one, never a mix. The generation it replaces is added to
//!   `wiki_stale_generations` in the same transaction, and after the commit
//!   exactly the generations listed there are deleted ([`DeleteSelector`]
//!   `Generations`), best effort. Never "every generation but the active
//!   one": builds of one wiki run concurrently (only their publishes are
//!   serialised), and an in-flight build's generation is not stale.
//! * **points** — one per embedded node. `document_key` and `chunk_id` are the
//!   node id when it is 1 to 1024 bytes without a control character, else
//!   `sha256:<hex of the node id>` ([`point_key`]); the node id itself is
//!   always in `metadata_json` (`{"node_id": …}`), which is what a search maps
//!   back to.
//! * **embedding space** — [`model_slug`] of `wikis.embedding_model` and
//!   `wikis.embedding_dim`. The slug is the platform's vector-space slug (the
//!   toolkit indexes' rule, `indexregistry.VectorSpaceSlug` in elitea-main and
//!   `vector_space_slug` in the worker): the model lower-cased, every run of
//!   characters outside `[a-z0-9]` replaced by one `-`, trimmed of `-`,
//!   truncated to 50 bytes (trailing `-` dropped again), `model` when nothing
//!   is left, then `-` and the first 12 hex digits of the SHA-256 of the EXACT
//!   model string. So it matches `^[a-z0-9][a-z0-9-]{0,62}$`, two models never
//!   share a slug, and a wiki and a toolkit index of the same model share one
//!   collection.
//!
//! # Who deletes what
//!
//! * a publish: the generations `wiki_stale_generations` lists for its
//!   project (the previous active one, and any recorded below), after the
//!   commit;
//! * an abandoned build ([`crate::storage::build::Build::abandon`], the
//!   worker parent's clean-up of a killed child): its own generation, then
//!   the project's listed ones;
//! * a wiki deletion (`delete_wiki_index`): the whole namespace, with the
//!   invocation's callback bearer;
//! * a project deletion: nothing here. elitea-main's project deprovisioning
//!   calls elitea-vector `DropProject` (its `vector_data` step), which removes
//!   every point of the project in every collection and for every source.
//!
//! A build removed WITHOUT a bearer (the startup reconciliation, the
//! heartbeat sweep), or whose own delete failed, has its generation added
//! to `wiki_stale_generations`, also when its wiki was never published (no
//! `wikis` row); its points are never searched (a reader names the active
//! generation only) and the project's next publish, abandon or worker
//! clean-up deletes them. What remains: a project in which nothing runs
//! with a bearer again; its points stay in Qdrant until the project is
//! deleted.
//!
//! # Two backends
//!
//! [`DenseIndex`] is elitea-vector over gRPC ([`DenseIndex::remote`]) or an
//! in-process store ([`MemoryDense`]) that scores with exact cosine
//! similarity, for the tests and the parity tool: the storage tests need no
//! Qdrant.

use crate::config::VectorSettings;
use crate::storage::WikiKey;
use elitea_vector_index::client::MAX_LIMIT;
use elitea_vector_index::{ClientConfig, DeleteSelector, SearchParams, VectorClient, pb};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use std::collections::HashMap;
use std::fmt::Write as _;
use std::sync::{Arc, Mutex};
use uuid::Uuid;

/// The `UUIDv5` namespace every `DeepWiki` namespace id is derived under
/// (`5669574f-401d-4878-a59e-0c34ecc43970`). Fixed for ever: changing it
/// orphans every stored vector.
pub const DEEPWIKI_NAMESPACE: Uuid = Uuid::from_u128(0x5669_574f_401d_4878_a59e_0c34_ecc4_3970);

/// The longest `document_key` / `chunk_id` elitea-vector accepts.
const MAX_KEY_BYTES: usize = 1024;

/// The longest generation elitea-vector accepts.
const MAX_GENERATION_BYTES: usize = 128;

/// elitea-vector's model-slug limit.
const MAX_SLUG_BYTES: usize = 63;
/// The hex digits of the model's SHA-256 a slug carries.
const SLUG_HASH_HEX: usize = 12;
/// Room for the readable part of a slug.
const MAX_SLUG_READABLE: usize = MAX_SLUG_BYTES - 1 - SLUG_HASH_HEX;

/// The name a wiki's namespace id is derived from: `"{project_id}/{wiki_id}"`.
#[must_use]
pub fn namespace_name(key: &WikiKey) -> String {
    format!("{}/{}", key.project_id(), key.wiki_id())
}

/// The wiki's namespace id: `UUIDv5` under [`DEEPWIKI_NAMESPACE`] of
/// [`namespace_name`], in the canonical lower-case form elitea-vector
/// requires.
#[must_use]
pub fn namespace_id(key: &WikiKey) -> String {
    Uuid::new_v5(&DEEPWIKI_NAMESPACE, namespace_name(key).as_bytes())
        .hyphenated()
        .to_string()
}

/// The platform's vector-space slug of `model` (see the module docs).
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
    if readable.len() > MAX_SLUG_READABLE {
        readable.truncate(MAX_SLUG_READABLE);
        while readable.ends_with('-') {
            readable.pop();
        }
    }
    if readable.is_empty() {
        readable.push_str("model");
    }
    let digest = Sha256::digest(model.as_bytes());
    let mut slug = readable;
    slug.push('-');
    for byte in digest.iter().take(SLUG_HASH_HEX / 2) {
        let _ = write!(slug, "{byte:02x}");
    }
    slug
}

/// The generation of a build: its id, which must already be inside
/// elitea-vector's `[A-Za-z0-9._:-]{1,128}` (a build id is a UUID).
///
/// # Errors
///
/// [`DenseError`] for an id outside the charset or too long.
pub fn generation_of(build_id: &str) -> Result<String, DenseError> {
    let valid = !build_id.is_empty()
        && build_id.len() <= MAX_GENERATION_BYTES
        && build_id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b':' | b'-'));
    if valid {
        Ok(build_id.to_owned())
    } else {
        Err(DenseError::invalid(format!(
            "the build id {build_id:?} cannot be a vector generation"
        )))
    }
}

/// The `document_key` / `chunk_id` of a node's point.
#[must_use]
pub fn point_key(node_id: &str) -> String {
    let plain = !node_id.is_empty()
        && node_id.len() <= MAX_KEY_BYTES
        && !node_id.chars().any(char::is_control);
    if plain {
        return node_id.to_owned();
    }
    let mut key = String::from("sha256:");
    for byte in Sha256::digest(node_id.as_bytes()) {
        let _ = write!(key, "{byte:02x}");
    }
    key
}

/// The embedding space of a wiki's vectors.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Space {
    /// [`model_slug`] of the model.
    pub slug: String,
    pub dimension: u32,
}

impl Space {
    /// The space of `model` at `dimension`.
    ///
    /// # Errors
    ///
    /// [`DenseError`] for a dimension of 0 or past `u32`.
    pub fn new(model: &str, dimension: usize) -> Result<Self, DenseError> {
        let dimension = u32::try_from(dimension)
            .ok()
            .filter(|d| *d > 0)
            .ok_or_else(|| DenseError::invalid(format!("{dimension} is not a vector dimension")))?;
        Ok(Self {
            slug: model_slug(model),
            dimension,
        })
    }

    fn proto(&self) -> pb::EmbeddingSpace {
        pb::EmbeddingSpace {
            model_slug: self.slug.clone(),
            dimension: self.dimension,
        }
    }
}

/// A dense-store failure. `unavailable` marks a failure of the store (the
/// network, elitea-vector, Qdrant, a refused bearer), after which a reader
/// answers lexically; anything else is the caller's mistake.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("{message}")]
pub struct DenseError {
    pub message: String,
    pub unavailable: bool,
}

impl DenseError {
    fn invalid(message: impl Into<String>) -> Self {
        Self {
            message: message.into(),
            unavailable: false,
        }
    }

    fn unavailable(message: impl Into<String>) -> Self {
        Self {
            message: message.into(),
            unavailable: true,
        }
    }
}

impl From<elitea_vector_index::Error> for DenseError {
    fn from(error: elitea_vector_index::Error) -> Self {
        match error {
            elitea_vector_index::Error::InvalidArgument(message) => Self::invalid(message),
            // A refusal of the request shape is ours; every other failure
            // (unreachable, busy, a refused or expired bearer, an internal
            // error) is the store's.
            elitea_vector_index::Error::Rpc {
                code: tonic::Code::InvalidArgument,
                message,
            } => Self::invalid(format!("elitea-vector refused the request: {message}")),
            other => Self::unavailable(other.to_string()),
        }
    }
}

/// One search result: the node and its cosine similarity.
#[derive(Debug, Clone, PartialEq)]
pub struct DenseHit {
    pub node_id: String,
    /// Cosine similarity, higher is closer.
    pub score: f64,
}

impl DenseHit {
    /// The distance the read path reports (`vec_distance`, lower is
    /// better): `1 - cosine similarity`. It is rank-only: nothing compares
    /// it with a threshold.
    #[must_use]
    pub fn distance(&self) -> f64 {
        1.0 - self.score
    }
}

/// A wiki's dense store, bound to one caller's bearer (or in memory).
/// Cloning is cheap.
#[derive(Clone)]
pub struct DenseIndex(Backend);

#[derive(Clone)]
enum Backend {
    Remote {
        client: VectorClient,
        bearer: Arc<String>,
    },
    Memory(MemoryDense),
}

impl std::fmt::Debug for DenseIndex {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match &self.0 {
            // Never the bearer.
            Backend::Remote { .. } => formatter.write_str("DenseIndex(elitea-vector)"),
            Backend::Memory(_) => formatter.write_str("DenseIndex(memory)"),
        }
    }
}

/// The client of the configured elitea-vector: TLS with the callback CA
/// and the callback identity, the PEM files read now (so a rotated
/// certificate is used by the next invocation). Connects lazily.
///
/// # Errors
///
/// [`DenseError`] for a file that cannot be read or material the client
/// refuses.
pub fn connect(settings: &VectorSettings) -> Result<VectorClient, DenseError> {
    let read = |path: &std::path::Path, setting: &str| {
        std::fs::read(path).map_err(|error| {
            DenseError::invalid(format!(
                "{setting} ({}) cannot be read: {error}",
                path.display()
            ))
        })
    };
    let mut config = ClientConfig::new(settings.url.clone());
    config.allow_insecure = settings.insecure_loopback;
    if !settings.insecure_loopback {
        if let Some(ca) = &settings.ca_file {
            config.ca_pem = Some(read(ca, settings.ca_setting)?);
        }
        if let Some(identity) = &settings.identity {
            config.identity_pem = Some((
                read(&identity.cert_file, identity.cert_file_setting)?,
                read(&identity.key_file, identity.key_file_setting)?,
            ));
        }
        config.server_name.clone_from(&settings.server_name);
    }
    Ok(VectorClient::connect_lazy(&config)?)
}

/// The bearer an invocation carries for its callbacks: `llm_settings.api_key`
/// (or `openai_api_key`), the per-invocation callback token elitea-main
/// minted and recorded a `deepwiki` grant for. elitea-vector admits it for
/// the invocation's project and source `deepwiki` only.
#[must_use]
pub fn callback_bearer(arguments: &serde_json::Map<String, Value>) -> Option<String> {
    let settings = arguments.get("llm_settings")?.as_object()?;
    ["api_key", "openai_api_key"]
        .iter()
        .filter_map(|key| settings.get(*key).and_then(Value::as_str))
        .map(str::trim)
        .find(|value| !value.is_empty())
        .map(str::to_owned)
}

/// How an invocation reaches the dense store.
#[derive(Debug, Clone)]
pub enum DenseAccess {
    /// No elitea-vector is configured (the fixture runner, tests).
    Off,
    /// One is configured but this invocation cannot use it: why (no
    /// callback bearer, unreadable TLS material).
    Missing(String),
    /// The store, with the invocation's bearer.
    Ready(DenseIndex),
}

impl DenseAccess {
    /// The access of an invocation with `arguments` to the elitea-vector
    /// of `settings`.
    #[must_use]
    pub fn resolve(
        settings: Option<&VectorSettings>,
        arguments: &serde_json::Map<String, Value>,
    ) -> Self {
        let Some(settings) = settings else {
            return Self::Off;
        };
        let Some(bearer) = callback_bearer(arguments) else {
            return Self::Missing(
                "the invocation carries no callback token (llm_settings.api_key) for elitea-vector"
                    .to_owned(),
            );
        };
        match connect(settings) {
            Ok(client) => Self::Ready(DenseIndex::remote(client, bearer)),
            Err(error) => Self::Missing(error.to_string()),
        }
    }

    /// The store when it is ready.
    #[must_use]
    pub fn index(&self) -> Option<&DenseIndex> {
        match self {
            Self::Ready(index) => Some(index),
            Self::Off | Self::Missing(_) => None,
        }
    }

    /// The store, `None` when none is configured.
    ///
    /// # Errors
    ///
    /// Why a configured store cannot be used.
    pub fn required(self) -> Result<Option<DenseIndex>, String> {
        match self {
            Self::Off => Ok(None),
            Self::Missing(why) => Err(why),
            Self::Ready(index) => Ok(Some(index)),
        }
    }
}

/// The points per upsert call (elitea-vector's batch limit).
const UPSERT_ROUND: usize = elitea_vector_index::client::MAX_BATCH_POINTS;

fn deepwiki_namespace(key: &WikiKey, generation: &str) -> pb::Namespace {
    pb::Namespace {
        source: pb::Source::Deepwiki as i32,
        namespace_id: namespace_id(key),
        generation: generation.to_owned(),
        memory_scope: 0,
        toolkit_id: 0,
    }
}

fn deepwiki_scope(key: &WikiKey, generation: &str) -> pb::Scope {
    let namespace = namespace_id(key);
    pb::Scope {
        project_id: 0,
        source: pb::Source::Deepwiki as i32,
        namespace_ids: vec![namespace.clone()],
        generation: String::new(),
        active_generations: vec![pb::ActiveGeneration {
            namespace_id: namespace,
            generation: generation.to_owned(),
        }],
        memory_scope: 0,
        toolkit_id: 0,
    }
}

fn check_finite(vector: &[f32], node_id: &str) -> Result<(), DenseError> {
    if vector.iter().all(|v| v.is_finite()) {
        Ok(())
    } else {
        Err(DenseError::invalid(format!(
            "the vector of {node_id} has a value that is not a finite number"
        )))
    }
}

impl DenseIndex {
    /// elitea-vector, called with `bearer`.
    #[must_use]
    pub fn remote(client: VectorClient, bearer: impl Into<String>) -> Self {
        Self(Backend::Remote {
            client,
            bearer: Arc::new(bearer.into()),
        })
    }

    /// An in-process store.
    #[must_use]
    pub fn memory(store: MemoryDense) -> Self {
        Self(Backend::Memory(store))
    }

    /// Write the vectors of `points` (`(node id, vector)`) into the wiki's
    /// namespace under `generation`, in rounds of at most 512. Returns the
    /// points written.
    ///
    /// # Errors
    ///
    /// [`DenseError`]: a vector of another dimension or with a non-finite
    /// value (nothing of that round is sent), or the store's failure.
    pub async fn upsert(
        &self,
        key: &WikiKey,
        generation: &str,
        space: &Space,
        points: &[(&str, &[f32])],
    ) -> Result<u64, DenseError> {
        let dimension = space.dimension as usize;
        let mut written = 0;
        for round in points.chunks(UPSERT_ROUND) {
            for (node_id, vector) in round {
                if vector.len() != dimension {
                    return Err(DenseError::invalid(format!(
                        "the vector of {node_id} has {} dimensions, the wiki's space has {dimension}",
                        vector.len()
                    )));
                }
                check_finite(vector, node_id)?;
            }
            written += match &self.0 {
                Backend::Remote { client, bearer } => {
                    let points = round
                        .iter()
                        .map(|(node_id, vector)| {
                            let key = point_key(node_id);
                            pb::Point {
                                document_key: key.clone(),
                                chunk_id: key,
                                metadata_json: json!({ "node_id": node_id }).to_string(),
                                vector: vector.to_vec(),
                                ..pb::Point::default()
                            }
                        })
                        .collect();
                    client
                        .upsert(
                            bearer.as_ref(),
                            space.proto(),
                            deepwiki_namespace(key, generation),
                            points,
                        )
                        .await?
                        .upserted
                }
                Backend::Memory(store) => store.upsert(key, generation, space, round)?,
            };
        }
        Ok(written)
    }

    /// The `k` nearest points of the wiki's `generation` to `vector`, by
    /// cosine similarity, best first.
    ///
    /// # Errors
    ///
    /// [`DenseError`]: a query of another dimension or with a non-finite
    /// value, or the store's failure.
    pub async fn search(
        &self,
        key: &WikiKey,
        generation: &str,
        space: &Space,
        vector: &[f32],
        k: usize,
    ) -> Result<Vec<DenseHit>, DenseError> {
        if vector.len() != space.dimension as usize {
            return Err(DenseError::invalid(format!(
                "the question's vector has {} dimensions and the wiki's vectors have {}",
                vector.len(),
                space.dimension
            )));
        }
        check_finite(vector, "the question")?;
        let limit = u32::try_from(k).unwrap_or(MAX_LIMIT).clamp(1, MAX_LIMIT);
        match &self.0 {
            Backend::Remote { client, bearer } => {
                let points = client
                    .search(
                        bearer.as_ref(),
                        SearchParams {
                            scope: deepwiki_scope(key, generation),
                            space: space.proto(),
                            vector: vector.to_vec(),
                            filter: None,
                            limit,
                            score_threshold: None,
                        },
                    )
                    .await?;
                Ok(points
                    .into_iter()
                    .map(|point| DenseHit {
                        node_id: node_of(&point.metadata_json).unwrap_or(point.chunk_id),
                        score: f64::from(point.score),
                    })
                    .collect())
            }
            Backend::Memory(store) => {
                store.pass_search_gate().await;
                store.search(key, generation, space, vector, limit as usize)
            }
        }
    }

    /// The points of the wiki's `generation`.
    ///
    /// # Errors
    ///
    /// The store's failure.
    pub async fn count(
        &self,
        key: &WikiKey,
        generation: &str,
        space: &Space,
    ) -> Result<u64, DenseError> {
        match &self.0 {
            Backend::Remote { client, bearer } => Ok(client
                .count(
                    bearer.as_ref(),
                    deepwiki_scope(key, generation),
                    Some(space.proto()),
                    None,
                )
                .await?),
            Backend::Memory(store) => store.count(key, Some(generation)),
        }
    }

    /// Delete the wiki's points of `generations`.
    ///
    /// # Errors
    ///
    /// The store's failure.
    pub async fn delete_generations(
        &self,
        key: &WikiKey,
        generations: Vec<String>,
    ) -> Result<(), DenseError> {
        self.delete(key, DeleteSelector::Generations(generations))
            .await
    }

    /// Delete every point of the wiki.
    ///
    /// # Errors
    ///
    /// The store's failure.
    pub async fn delete_namespace(&self, key: &WikiKey) -> Result<(), DenseError> {
        self.delete(key, DeleteSelector::WholeNamespace).await
    }

    async fn delete(&self, key: &WikiKey, selector: DeleteSelector) -> Result<(), DenseError> {
        match &self.0 {
            Backend::Remote { client, bearer } => {
                client
                    .delete(bearer.as_ref(), None, deepwiki_namespace(key, ""), selector)
                    .await?;
                Ok(())
            }
            Backend::Memory(store) => store.delete(key, &selector),
        }
    }
}

/// The node id a point's metadata names.
fn node_of(metadata: &str) -> Option<String> {
    serde_json::from_str::<Value>(metadata)
        .ok()?
        .get("node_id")?
        .as_str()
        .map(str::to_owned)
}

/// One point of the in-process store.
#[derive(Debug, Clone)]
struct MemoryPoint {
    project: i32,
    namespace: String,
    generation: String,
    space: Space,
    node_id: String,
    vector: Vec<f32>,
}

#[derive(Debug, Default)]
struct MemoryState {
    points: Vec<MemoryPoint>,
    unavailable: bool,
    fail_deletes: bool,
    calls: Vec<&'static str>,
    /// [`MemoryDense::pause_next_search`]'s pair: `(arrived, release)`.
    search_gate: Option<(Arc<tokio::sync::Notify>, Arc<tokio::sync::Notify>)>,
}

/// An in-process dense store with exact cosine scoring, for the tests and
/// the parity tool. Clones share one store.
#[derive(Debug, Clone, Default)]
pub struct MemoryDense {
    state: Arc<Mutex<MemoryState>>,
}

impl MemoryDense {
    /// An empty store.
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, MemoryState> {
        // A test that panicked while holding the lock leaves the data valid.
        self.state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// Every call from now fails as an unreachable elitea-vector does
    /// (`true`), or works again (`false`).
    pub fn set_unavailable(&self, unavailable: bool) {
        self.lock().unavailable = unavailable;
    }

    /// Every delete from now fails as elitea-vector's refusal does
    /// (`true`), or works again (`false`); the other calls are served.
    pub fn fail_deletes(&self, fail: bool) {
        self.lock().fail_deletes = fail;
    }

    /// Hold the next search before it reads the points: it signals the
    /// first returned `Notify` when it got there and waits for the second.
    /// A test makes a publish (and its sweep) happen in the middle of a
    /// reader's query with it.
    #[must_use]
    pub fn pause_next_search(&self) -> (Arc<tokio::sync::Notify>, Arc<tokio::sync::Notify>) {
        let arrived = Arc::new(tokio::sync::Notify::new());
        let release = Arc::new(tokio::sync::Notify::new());
        self.lock().search_gate = Some((arrived.clone(), release.clone()));
        (arrived, release)
    }

    async fn pass_search_gate(&self) {
        let gate = self.lock().search_gate.take();
        if let Some((arrived, release)) = gate {
            arrived.notify_one();
            release.notified().await;
        }
    }

    /// The calls made, by name, in order.
    #[must_use]
    pub fn calls(&self) -> Vec<&'static str> {
        self.lock().calls.clone()
    }

    /// Every point stored, of every wiki.
    #[must_use]
    pub fn point_count(&self) -> usize {
        self.lock().points.len()
    }

    /// The generations stored for one wiki, sorted, each with its points.
    #[must_use]
    pub fn generations(&self, key: &WikiKey) -> Vec<(String, usize)> {
        let namespace = namespace_id(key);
        let mut counts: HashMap<String, usize> = HashMap::new();
        for point in &self.lock().points {
            if point.project == key.project_id() && point.namespace == namespace {
                *counts.entry(point.generation.clone()).or_default() += 1;
            }
        }
        let mut generations: Vec<(String, usize)> = counts.into_iter().collect();
        generations.sort();
        generations
    }

    /// The stored vector of one node of one wiki's generation.
    #[must_use]
    pub fn vector(&self, key: &WikiKey, generation: &str, node_id: &str) -> Option<Vec<f32>> {
        let namespace = namespace_id(key);
        self.lock()
            .points
            .iter()
            .find(|p| {
                p.project == key.project_id()
                    && p.namespace == namespace
                    && p.generation == generation
                    && p.node_id == node_id
            })
            .map(|p| p.vector.clone())
    }

    fn enter(
        &self,
        call: &'static str,
    ) -> Result<std::sync::MutexGuard<'_, MemoryState>, DenseError> {
        let mut state = self.lock();
        state.calls.push(call);
        if state.unavailable {
            return Err(DenseError::unavailable(
                "cannot reach elitea-vector: the in-memory store is switched off",
            ));
        }
        Ok(state)
    }

    fn upsert(
        &self,
        key: &WikiKey,
        generation: &str,
        space: &Space,
        points: &[(&str, &[f32])],
    ) -> Result<u64, DenseError> {
        let mut state = self.enter("upsert")?;
        let namespace = namespace_id(key);
        for (node_id, vector) in points {
            // Idempotent, as elitea-vector's point ids are.
            state.points.retain(|p| {
                !(p.project == key.project_id()
                    && p.namespace == namespace
                    && p.generation == generation
                    && p.space == *space
                    && p.node_id == *node_id)
            });
            state.points.push(MemoryPoint {
                project: key.project_id(),
                namespace: namespace.clone(),
                generation: generation.to_owned(),
                space: space.clone(),
                node_id: (*node_id).to_owned(),
                vector: vector.to_vec(),
            });
        }
        Ok(points.len() as u64)
    }

    fn search(
        &self,
        key: &WikiKey,
        generation: &str,
        space: &Space,
        vector: &[f32],
        limit: usize,
    ) -> Result<Vec<DenseHit>, DenseError> {
        let state = self.enter("search")?;
        let namespace = namespace_id(key);
        let mut hits: Vec<DenseHit> = state
            .points
            .iter()
            .filter(|p| {
                p.project == key.project_id()
                    && p.namespace == namespace
                    && p.generation == generation
                    && p.space == *space
            })
            .map(|p| DenseHit {
                node_id: p.node_id.clone(),
                score: cosine(&p.vector, vector),
            })
            .collect();
        hits.sort_by(|a, b| {
            b.score
                .total_cmp(&a.score)
                .then_with(|| a.node_id.cmp(&b.node_id))
        });
        hits.truncate(limit);
        Ok(hits)
    }

    fn count(&self, key: &WikiKey, generation: Option<&str>) -> Result<u64, DenseError> {
        let state = self.enter("count")?;
        let namespace = namespace_id(key);
        Ok(state
            .points
            .iter()
            .filter(|p| {
                p.project == key.project_id()
                    && p.namespace == namespace
                    && generation.is_none_or(|g| p.generation == g)
            })
            .count() as u64)
    }

    fn delete(&self, key: &WikiKey, selector: &DeleteSelector) -> Result<(), DenseError> {
        let mut state = self.enter("delete")?;
        if state.fail_deletes {
            return Err(DenseError::unavailable(
                "elitea-vector refused the delete: the in-memory store fails deletes",
            ));
        }
        let namespace = namespace_id(key);
        state.points.retain(|p| {
            let ours = p.project == key.project_id() && p.namespace == namespace;
            let selected = match selector {
                DeleteSelector::WholeNamespace => true,
                DeleteSelector::Generations(generations) => generations.contains(&p.generation),
                DeleteSelector::GenerationsExcept(keep) => p.generation != *keep,
                DeleteSelector::Documents(keys) => keys.contains(&point_key(&p.node_id)),
            };
            !(ours && selected)
        });
        Ok(())
    }
}

/// Exact cosine similarity, in `f64`; 0 when either vector is zero.
fn cosine(a: &[f32], b: &[f32]) -> f64 {
    let (mut dot, mut left, mut right) = (0.0_f64, 0.0_f64, 0.0_f64);
    for (x, y) in a.iter().zip(b) {
        let (x, y) = (f64::from(*x), f64::from(*y));
        dot += x * y;
        left += x * x;
        right += y * y;
    }
    if left == 0.0 || right == 0.0 {
        0.0
    } else {
        dot / (left.sqrt() * right.sqrt())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::storage::ProjectScope;

    fn key(project: i32, wiki: &str) -> WikiKey {
        WikiKey::new(ProjectScope::new(project).unwrap(), wiki)
    }

    fn sha256_hex(text: &str) -> String {
        let mut hex = String::new();
        for byte in Sha256::digest(text.as_bytes()) {
            let _ = write!(hex, "{byte:02x}");
        }
        hex
    }

    #[test]
    fn the_namespace_is_uuidv5_of_project_and_wiki() {
        let id = namespace_id(&key(7, "acme--notes--main"));
        assert_eq!(
            id,
            Uuid::new_v5(&DEEPWIKI_NAMESPACE, b"7/acme--notes--main")
                .hyphenated()
                .to_string()
        );
        // Pinned: a change orphans every stored vector.
        // (Python: uuid.uuid5(UUID("5669574f-…"), "7/acme--notes--main")).
        assert_eq!(id, "c903dad9-fa43-5b23-93df-c52ad96e4d9c");
        assert_eq!(id.len(), 36);
        assert_eq!(id, id.to_lowercase());
        assert_ne!(id, namespace_id(&key(8, "acme--notes--main")));
        assert_ne!(id, namespace_id(&key(7, "acme--notes--dev")));
        assert_eq!(
            DEEPWIKI_NAMESPACE.hyphenated().to_string(),
            "5669574f-401d-4878-a59e-0c34ecc43970"
        );
    }

    #[test]
    fn the_slug_is_the_platforms_vector_space_slug() {
        let slug = model_slug("text-embedding-3-small");
        assert_eq!(
            slug,
            format!(
                "text-embedding-3-small-{}",
                &sha256_hex("text-embedding-3-small")[..12]
            )
        );
        assert_eq!(
            model_slug("Org/Model.A"),
            format!("org-model-a-{}", &sha256_hex("Org/Model.A")[..12])
        );
        assert_eq!(
            model_slug("///"),
            format!("model-{}", &sha256_hex("///")[..12])
        );
        let long = model_slug(&"a".repeat(100));
        assert_eq!(long.len(), MAX_SLUG_BYTES);
        assert_ne!(model_slug("Model"), model_slug("model"));
        for model in ["Model", "Ünïcode-Ëmbed", "-x-", "a_b c"] {
            let slug = model_slug(model);
            let bytes = slug.as_bytes();
            assert!(
                bytes[0].is_ascii_lowercase() || bytes[0].is_ascii_digit(),
                "{slug}"
            );
            assert!(slug.len() <= MAX_SLUG_BYTES, "{slug}");
            assert!(
                bytes
                    .iter()
                    .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || *b == b'-'),
                "{slug}"
            );
        }
    }

    #[test]
    fn generations_and_point_keys_fit_the_service_limits() {
        assert!(generation_of("0f6e3a2c-1d2b-4c5d-8e9f-0a1b2c3d4e5f").is_ok());
        assert!(generation_of("").is_err());
        assert!(generation_of("a b").is_err());
        assert!(generation_of(&"a".repeat(129)).is_err());
        assert_eq!(point_key("py::mod::f"), "py::mod::f");
        let long = "x".repeat(2000);
        let hashed = point_key(&long);
        assert_eq!(hashed, format!("sha256:{}", sha256_hex(&long)));
        assert!(point_key("a\nb").starts_with("sha256:"));
        assert!(point_key("").starts_with("sha256:"));
    }

    #[test]
    fn the_bearer_is_the_callback_api_key() {
        let arguments = |value: Value| value.as_object().cloned().unwrap_or_default();
        assert_eq!(
            callback_bearer(&arguments(json!({"llm_settings": {"api_key": " t "}}))),
            Some("t".to_owned())
        );
        assert_eq!(
            callback_bearer(&arguments(json!({"llm_settings": {"openai_api_key": "o"}}))),
            Some("o".to_owned())
        );
        assert_eq!(
            callback_bearer(&arguments(json!({"llm_settings": {}}))),
            None
        );
        assert_eq!(callback_bearer(&arguments(json!({}))), None);
    }

    #[tokio::test]
    async fn the_memory_store_scopes_by_wiki_generation_and_space() {
        let store = MemoryDense::new();
        let index = DenseIndex::memory(store.clone());
        let a = key(1, "w");
        let b = key(2, "w");
        let space = Space::new("m", 2).unwrap();
        let points: [(&str, &[f32]); 2] = [("n1", &[1.0, 0.0]), ("n2", &[0.0, 1.0])];
        assert_eq!(index.upsert(&a, "g1", &space, &points).await, Ok(2));
        assert_eq!(index.upsert(&a, "g2", &space, &points[..1]).await, Ok(1));
        assert_eq!(index.upsert(&b, "g1", &space, &points).await, Ok(2));
        let hits = index
            .search(&a, "g1", &space, &[1.0, 0.1], 10)
            .await
            .unwrap();
        assert_eq!(
            hits.iter().map(|h| h.node_id.as_str()).collect::<Vec<_>>(),
            ["n1", "n2"]
        );
        assert!(hits[0].distance() < hits[1].distance());
        assert!(index.search(&a, "g1", &space, &[1.0], 10).await.is_err());
        index
            .delete_generations(&a, vec!["g1".to_owned()])
            .await
            .unwrap();
        assert_eq!(store.generations(&a), [("g2".to_owned(), 1)]);
        assert_eq!(store.generations(&b), [("g1".to_owned(), 2)]);
        index.delete_namespace(&a).await.unwrap();
        assert_eq!(
            store.generations(&a),
            [] as [(std::string::String, usize); 0]
        );
        store.set_unavailable(true);
        let failed = index.search(&b, "g1", &space, &[1.0, 0.0], 1).await;
        assert!(failed.is_err_and(|e| e.unavailable));
    }
}
