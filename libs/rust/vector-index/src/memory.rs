//! The SDK `memory` toolkit on `elitea-vector` (worker retirement unit M9).
//!
//! The SDK's four tools (`manage_memory`, `search_memory`, `get_memory`,
//! `delete_memory`) keep a `LangGraph` `PostgresStore` in a user-configured
//! pgvector database, under the namespace `(namespace,)` of the toolkit's
//! settings. Here the same four operations run over `elitea-vector`:
//!
//! * **Who may see a memory is decided by the SERVICE.** A memory point is of
//!   the `memory` source, and the facade stamps its `owner` from the token
//!   (`user:<id>` for the personal scope, `project` for the project scope) and
//!   narrows every read, count, update and delete by it. This crate only says
//!   *which scope* a call works in ([`MemoryScope`]); it cannot name an owner,
//!   and a caller that computes another user's namespace id reads nothing of
//!   theirs. The namespace is the hash of the toolkit's `namespace` string
//!   ([`namespace_id`]); the project is the token's.
//! * **Points and time.** One point per VERSION of a memory: `document_key` is
//!   the memory key, `text` is the content, `chunk_id` is `v<id>` and
//!   `metadata_json` is `{"m": <the user's metadata>, "t": {"created_at",
//!   "valid_from", "valid_to", "id"}}` (milliseconds since the epoch;
//!   `valid_to` is null while the version is open). The version `id` is a
//!   random UUID, so two concurrent writes never share a point and none is
//!   lost; each version also has its own generation, `<hash of the key>-<id>`,
//!   which is what lets one old version be deleted alone. Writing an existing
//!   key SUPERSEDES: the new version is written, then every older open version
//!   is closed in place (`valid_to` := the new `valid_from`, vector kept,
//!   nothing re-embedded). "Current" is the open version with the latest
//!   `valid_from` (ties broken by id), so when two writers raced the later one
//!   is current and the other stays in the history, with its `valid_to`
//!   derived from the next version. Reads return the current version by default;
//!   `as_of` returns what was valid at that instant; `include_history` every
//!   version (with `as_of`: every version that existed by then).
//!   `delete_memory` removes every version at once: privacy beats history. At
//!   most [`MAX_VERSIONS_PER_KEY`] versions are kept per key (the oldest is
//!   deleted to make room).
//! * **Recency.** Search ranks by `(1 - w) * cosine + w * recency` with
//!   `w = ` [`RECENCY_WEIGHT`] and `recency = 2^(-age / half-life)`, half-life
//!   [`RECENCY_HALF_LIFE_DAYS`] days, age counted from the version's
//!   `valid_from` to now (or to `as_of`). A small, bounded nudge: cosine still
//!   decides. The store is asked for up to three times the limit and the result
//!   is re-ranked here.
//! * **Ranking.** The SDK builds its store without an index, so its
//!   `search_memory` ignores the query and returns the most recently updated
//!   items. Here the query is embedded and the closest memories come first;
//!   this is a deliberate behavioural difference.
//! * **Limits.** [`MAX_MEMORIES_PER_NAMESPACE`] points (memories with their
//!   versions) per namespace and scope, [`MAX_CONTENT_BYTES`] of content,
//!   [`MAX_METADATA_BYTES`] of metadata, [`MAX_KEY_BYTES`] of key. The SDK has
//!   none; a store that grows without bound is also an embedding bill that
//!   grows without bound. The count check is advisory (two concurrent writes
//!   can pass it together).
//! * **Output.** The same sentences and the same `json.dumps(indent=2)` text
//!   as the SDK, with the keys in the SDK's order (`key`, `content`,
//!   `metadata`) whatever `serde_json` is built with.
//!
//! Nothing is read from the SDK's store: memories written under the Python
//! worker are not carried over.

use std::collections::HashSet;
use std::fmt::Write as _;
use std::future::Future;
use std::sync::Arc;
use std::sync::atomic::{AtomicU32, Ordering};

use chrono::{DateTime, SecondsFormat, Utc};
use elitea_engine_core::pyjson::dumps_indent2;
use serde_json::{Map, Value};
use sha2::{Digest, Sha256};
use tonic::Code;

use crate::client::{
    ClaimToken, DeleteSelector, MAX_LIMIT, SearchParams, VectorClient, memory_namespace,
    memory_scope_of,
};
use crate::error::{Error, Result};
use crate::filter::{keyword, nest, with_must};
use crate::pb;
use crate::search::{VectorBackend, VectorSession};

/// The most versions one key keeps (the open one included).
pub const MAX_VERSIONS_PER_KEY: usize = 20;
/// The weight of recency in a search score.
pub const RECENCY_WEIGHT: f32 = 0.1;
/// The recency half-life, in days.
pub const RECENCY_HALF_LIFE_DAYS: f64 = 30.0;
/// The most points (memories, with their versions) one namespace holds, per scope.
pub const MAX_MEMORIES_PER_NAMESPACE: u64 = 10_000;
/// The most bytes of content one memory holds.
pub const MAX_CONTENT_BYTES: usize = 16 * 1024;
/// The most bytes of metadata (as JSON) one memory holds.
pub const MAX_METADATA_BYTES: usize = 8 * 1024;
/// The most bytes of key one memory has.
pub const MAX_KEY_BYTES: usize = 256;
/// The SDK's default `limit` of `search_memory`.
pub const DEFAULT_SEARCH_LIMIT: u32 = 5;
/// The most results one `search_memory` returns.
pub const MAX_SEARCH_LIMIT: u32 = 50;

const CHUNK_TYPE: &str = "memory";
const DOCUMENT_VERSION: &str = "1";
/// Mixed into the namespace hash, so a memory namespace is a different id
/// space from every other use of a vector namespace UUID.
const NAMESPACE_DOMAIN: &[u8] = b"elitea.memory.namespace.v3\0";

/// Whose memory a call works in. The service turns it into an owner from the
/// token; nothing here names a user.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum MemoryScope {
    /// The memories of the user the token was minted for.
    Personal,
    /// The memories shared by every user of the token's project.
    Project,
}

impl MemoryScope {
    const fn wire(self) -> pb::MemoryScope {
        match self {
            Self::Personal => pb::MemoryScope::Personal,
            Self::Project => pb::MemoryScope::Project,
        }
    }
}

/// The vector namespace UUID of a toolkit `namespace` string: the first 16
/// bytes of `SHA-256(domain || len(namespace) || namespace)` with the UUID
/// version (8, custom) and variant bits set. Deterministic, so every run and
/// worker agrees, and it holds no secret: the service, not this hash, keeps
/// one user's memories from another.
#[must_use]
pub fn namespace_id(namespace: &str) -> String {
    let mut hasher = Sha256::new();
    hasher.update(NAMESPACE_DOMAIN);
    hasher.update(
        u64::try_from(namespace.len())
            .unwrap_or(u64::MAX)
            .to_be_bytes(),
    );
    hasher.update(namespace.as_bytes());
    let digest = hasher.finalize();
    let mut bytes = [0_u8; 16];
    bytes.copy_from_slice(&digest[..16]);
    bytes[6] = (bytes[6] & 0x0f) | 0x80;
    bytes[8] = (bytes[8] & 0x3f) | 0x80;
    let mut hex = String::with_capacity(32);
    for byte in bytes {
        let _ = write!(hex, "{byte:02x}");
    }
    format!(
        "{}-{}-{}-{}-{}",
        &hex[0..8],
        &hex[8..12],
        &hex[12..16],
        &hex[16..20],
        &hex[20..32]
    )
}

/// Embeds memory text and search queries with the deployment's memory
/// embedding model. A vector's length is the embedding space's dimension.
pub trait MemoryEmbedder: Send + Sync {
    /// The embedding of `text`.
    fn embed(&self, text: &str) -> impl Future<Output = Result<Vec<f32>>> + Send;
}

/// What the memory tools need from `elitea-vector`. [`VectorSession`] is the
/// real one; tests use it against the in-process fake.
pub trait MemoryBackend: Send + Sync {
    /// Writes one point of the namespace, under `generation` (one per
    /// version, so one version can be deleted alone).
    fn put(
        &self,
        scope: MemoryScope,
        space: pb::EmbeddingSpace,
        namespace_id: &str,
        generation: &str,
        point: pb::Point,
    ) -> impl Future<Output = Result<()>> + Send;
    /// Dense cosine search.
    fn search(
        &self,
        scope: MemoryScope,
        params: SearchParams,
    ) -> impl Future<Output = Result<Vec<pb::ScoredPoint>>> + Send;
    /// Reads points by filter, with no query vector.
    fn read(
        &self,
        scope: MemoryScope,
        namespace_id: &str,
        space: Option<pb::EmbeddingSpace>,
        filter: Option<pb::Filter>,
        limit: u32,
    ) -> impl Future<Output = Result<Vec<pb::ScoredPoint>>> + Send;
    /// The points of the namespace and scope that match `filter`.
    fn count(
        &self,
        scope: MemoryScope,
        namespace_id: &str,
        space: Option<pb::EmbeddingSpace>,
        filter: Option<pb::Filter>,
    ) -> impl Future<Output = Result<u64>> + Send;
    /// Replaces the metadata of one version in place, keeping its vector.
    fn set_metadata(
        &self,
        scope: MemoryScope,
        namespace_id: &str,
        version: &VersionRef<'_>,
        metadata_json: &str,
    ) -> impl Future<Output = Result<()>> + Send;
    /// Removes every version of one memory key, in every embedding space.
    fn delete_key(
        &self,
        scope: MemoryScope,
        namespace_id: &str,
        key: &str,
    ) -> impl Future<Output = Result<()>> + Send;
    /// Removes the named versions' generations, in every embedding space.
    fn delete_generations(
        &self,
        scope: MemoryScope,
        namespace_id: &str,
        generations: Vec<String>,
    ) -> impl Future<Output = Result<()>> + Send;
}

/// Names one stored version.
#[derive(Clone, Copy, Debug)]
pub struct VersionRef<'a> {
    /// The memory key.
    pub key: &'a str,
    /// The point's chunk id, `v<id>`.
    pub chunk_id: &'a str,
    /// The version's generation.
    pub generation: &'a str,
}

impl MemoryBackend for VectorSession<'_> {
    async fn put(
        &self,
        scope: MemoryScope,
        space: pb::EmbeddingSpace,
        namespace_id: &str,
        generation: &str,
        point: pb::Point,
    ) -> Result<()> {
        self.client()
            .upsert(
                self.token(),
                space,
                memory_namespace(namespace_id, scope.wire(), generation),
                vec![point],
            )
            .await
            .map(|_| ())
    }

    async fn search(
        &self,
        scope: MemoryScope,
        mut params: SearchParams,
    ) -> Result<Vec<pb::ScoredPoint>> {
        // The scope names the memory scope; the namespace ids stay as built.
        params.scope.source = pb::Source::Memory as i32;
        params.scope.memory_scope = scope.wire() as i32;
        VectorBackend::search(self, params).await
    }

    async fn read(
        &self,
        scope: MemoryScope,
        namespace_id: &str,
        space: Option<pb::EmbeddingSpace>,
        filter: Option<pb::Filter>,
        limit: u32,
    ) -> Result<Vec<pb::ScoredPoint>> {
        self.client()
            .read_memory(
                self.token(),
                namespace_id,
                scope.wire(),
                space,
                filter,
                limit,
            )
            .await
    }

    async fn count(
        &self,
        scope: MemoryScope,
        namespace_id: &str,
        space: Option<pb::EmbeddingSpace>,
        filter: Option<pb::Filter>,
    ) -> Result<u64> {
        self.client()
            .count(
                self.token(),
                memory_scope_of(namespace_id, scope.wire()),
                space,
                filter,
            )
            .await
    }

    async fn set_metadata(
        &self,
        scope: MemoryScope,
        namespace_id: &str,
        version: &VersionRef<'_>,
        metadata_json: &str,
    ) -> Result<()> {
        self.client()
            .set_memory_metadata(
                self.token(),
                namespace_id,
                scope.wire(),
                version.generation,
                None,
                version.key,
                version.chunk_id,
                metadata_json,
            )
            .await
    }

    async fn delete_key(&self, scope: MemoryScope, namespace_id: &str, key: &str) -> Result<()> {
        self.client()
            .delete(
                self.token(),
                None,
                memory_namespace(namespace_id, scope.wire(), ""),
                DeleteSelector::Documents(vec![key.to_owned()]),
            )
            .await
            .map(|_| ())
    }

    async fn delete_generations(
        &self,
        scope: MemoryScope,
        namespace_id: &str,
        generations: Vec<String>,
    ) -> Result<()> {
        self.client()
            .delete(
                self.token(),
                None,
                memory_namespace(namespace_id, scope.wire(), ""),
                DeleteSelector::Generations(generations),
            )
            .await
            .map(|_| ())
    }
}

/// `manage_memory`'s arguments.
#[derive(Clone, Debug, Default, PartialEq, serde::Deserialize)]
pub struct ManageArgs {
    /// The information to remember.
    pub content: String,
    /// Optional key; generated when absent or empty.
    #[serde(default)]
    pub key: Option<String>,
    /// Optional metadata object.
    #[serde(default)]
    pub metadata: Option<Map<String, Value>>,
}

/// `search_memory`'s arguments.
#[derive(Clone, Debug, PartialEq, serde::Deserialize)]
pub struct SearchMemoryArgs {
    /// Natural language query.
    pub query: String,
    /// The most results; the SDK default is 5.
    #[serde(default)]
    pub limit: Option<i64>,
    /// RFC 3339 instant: return what was valid then.
    #[serde(default)]
    pub as_of: Option<String>,
    /// Return every version, superseded ones included.
    #[serde(default)]
    pub include_history: Option<bool>,
}

/// `get_memory`'s arguments.
#[derive(Clone, Debug, PartialEq, serde::Deserialize)]
pub struct GetMemoryArgs {
    /// The key.
    pub key: String,
    /// RFC 3339 instant: return what was valid then.
    #[serde(default)]
    pub as_of: Option<String>,
    /// Return every version, superseded ones included.
    #[serde(default)]
    pub include_history: Option<bool>,
}

/// The time source of the tools (milliseconds since the epoch).
pub type Clock = Arc<dyn Fn() -> i64 + Send + Sync>;

fn system_clock() -> Clock {
    Arc::new(|| Utc::now().timestamp_millis())
}

/// The embedding dimension a run has seen, shared by its tool calls so a read
/// that embeds nothing (`get_memory`) can name the one collection it needs
/// instead of reading every collection. 0 is "not seen yet".
pub type SpaceCache = Arc<AtomicU32>;

/// The four memory tools of one namespace and scope over a backend and an
/// embedder.
pub struct MemoryTools<'a, B, E> {
    backend: &'a B,
    embedder: &'a E,
    model_slug: &'a str,
    scope: MemoryScope,
    namespace_id: String,
    clock: Clock,
    dimension: SpaceCache,
}

/// One version of a memory, as stored.
#[derive(Clone, Debug, PartialEq)]
struct Version {
    key: String,
    content: String,
    metadata: Option<Value>,
    /// The version's random id (the chunk id without its `v`).
    id: String,
    created_at: i64,
    valid_from: i64,
    valid_to: Option<i64>,
    /// Whether the store holds this version open (a race can leave an older
    /// version open; [`close_superseded`] then gives it a derived end that
    /// the store does not hold yet).
    stored_open: bool,
    cosine: f32,
}

impl Version {
    fn from_point(point: &pb::ScoredPoint) -> Self {
        let parsed: Value = serde_json::from_str(&point.metadata_json).unwrap_or(Value::Null);
        let time = parsed.get("t").unwrap_or(&Value::Null);
        let integer = |name: &str| time.get(name).and_then(Value::as_i64);
        let metadata = parsed
            .get("m")
            .filter(|value| value.as_object().is_some_and(|object| !object.is_empty()))
            .cloned();
        let valid_from = integer("valid_from").unwrap_or(0);
        Self {
            key: point.document_key.clone(),
            content: point.text.clone(),
            metadata,
            id: point
                .chunk_id
                .strip_prefix('v')
                .unwrap_or(&point.chunk_id)
                .to_owned(),
            created_at: integer("created_at").unwrap_or(valid_from),
            valid_from,
            valid_to: integer("valid_to"),
            stored_open: integer("valid_to").is_none(),
            cosine: point.dense_score.unwrap_or(point.score),
        }
    }

    fn payload(&self) -> String {
        let mut object = Map::new();
        if let Some(metadata) = &self.metadata {
            object.insert("m".to_owned(), metadata.clone());
        }
        object.insert(
            "t".to_owned(),
            serde_json::json!({
                "created_at": self.created_at,
                "valid_from": self.valid_from,
                "valid_to": self.valid_to,
                "id": self.id,
            }),
        );
        Value::Object(object).to_string()
    }

    fn chunk_id(&self) -> String {
        format!("v{}", self.id)
    }

    fn point(&self, vector: Vec<f32>) -> pb::Point {
        pb::Point {
            document_key: self.key.clone(),
            document_version: DOCUMENT_VERSION.to_owned(),
            chunk_id: self.chunk_id(),
            chunk_type: CHUNK_TYPE.to_owned(),
            text: self.content.clone(),
            metadata_json: self.payload(),
            vector,
            ..pb::Point::default()
        }
    }

    fn generation(&self) -> String {
        version_generation(&self.key, &self.id)
    }

    /// Newest first: the later `valid_from`, then the larger id.
    fn recency_order(left: &Self, right: &Self) -> std::cmp::Ordering {
        (right.valid_from, &right.id).cmp(&(left.valid_from, &left.id))
    }

    /// The SDK's fields, then the temporal ones when `temporal`.
    fn render(&self, depth: usize, temporal: bool) -> String {
        let pad = "  ".repeat(depth + 1);
        let text = |value: &str| dumps_indent2(&Value::String(value.to_owned()));
        let mut fields = vec![
            format!("{pad}\"key\": {}", text(&self.key)),
            format!("{pad}\"content\": {}", text(&self.content)),
        ];
        if let Some(metadata) = &self.metadata {
            let nested = dumps_indent2(metadata).replace('\n', &format!("\n{pad}"));
            fields.push(format!("{pad}\"metadata\": {nested}"));
        }
        if temporal {
            fields.push(format!(
                "{pad}\"valid_from\": {}",
                text(&rfc3339(self.valid_from))
            ));
            fields.push(format!(
                "{pad}\"valid_to\": {}",
                self.valid_to
                    .map_or_else(|| "null".to_owned(), |ms| text(&rfc3339(ms)))
            ));
        }
        format!("{{\n{}\n{}}}", fields.join(",\n"), "  ".repeat(depth))
    }
}

/// Gives every open version but the newest the end its successor implies, so
/// a race that left two open versions reads as a clean history.
fn close_superseded(versions: &mut [Version]) {
    versions.sort_by(Version::recency_order);
    let starts: Vec<i64> = versions.iter().map(|version| version.valid_from).collect();
    for (index, version) in versions.iter_mut().enumerate().skip(1) {
        if version.valid_to.is_none() {
            version.valid_to = Some(starts[index - 1]);
        }
    }
}

fn rfc3339(millis: i64) -> String {
    DateTime::<Utc>::from_timestamp_millis(millis)
        .unwrap_or_default()
        .to_rfc3339_opts(SecondsFormat::Millis, true)
}

/// `<first 8 bytes of SHA-256(key), hex>-<version id>`: the generation that
/// names one version of one key (at most 128 bytes of `[A-Za-z0-9._:-]`).
fn version_generation(key: &str, id: &str) -> String {
    let digest = Sha256::digest(key.as_bytes());
    let mut hex = String::with_capacity(16);
    for byte in &digest[..8] {
        let _ = write!(hex, "{byte:02x}");
    }
    format!("{hex}-{id}")
}

#[allow(
    clippy::cast_precision_loss,
    reason = "milliseconds since the epoch fit f64 exactly"
)]
fn range_condition(key: &str, gt: Option<i64>, lte: Option<i64>) -> pb::Condition {
    pb::Condition {
        key: key.to_owned(),
        r#match: Some(pb::condition::Match::Range(pb::Range {
            gt: gt.map(|ms| ms as f64),
            gte: None,
            lt: None,
            lte: lte.map(|ms| ms as f64),
        })),
    }
}

fn is_null(key: &str) -> pb::Condition {
    pb::Condition {
        key: key.to_owned(),
        r#match: Some(pb::condition::Match::IsNull(pb::IsNull {})),
    }
}

const VALID_FROM: &str = "metadata.t.valid_from";
const VALID_TO: &str = "metadata.t.valid_to";

/// The filter of a read: the key (when given) and the temporal view.
///
/// * default: the open versions;
/// * `as_of`: the versions valid at that instant;
/// * `history`: every version;
/// * `history` and `as_of`: every version that existed by that instant.
fn view_filter(key: Option<&str>, as_of: Option<i64>, history: bool) -> Option<pb::Filter> {
    let mut filter: Option<pb::Filter> = None;
    if let Some(key) = key {
        filter = Some(with_must(filter.as_ref(), keyword("document_key", key)));
    }
    match (as_of, history) {
        (Some(at), true) => {
            filter = Some(with_must(
                filter.as_ref(),
                range_condition(VALID_FROM, None, Some(at)),
            ));
        }
        (Some(at), false) => {
            // valid_from <= at AND (valid_to is null OR valid_to > at)
            filter = Some(with_must(
                filter.as_ref(),
                range_condition(VALID_FROM, None, Some(at)),
            ));
            let open = pb::Filter {
                should: vec![is_null(VALID_TO), range_condition(VALID_TO, Some(at), None)],
                ..pb::Filter::default()
            };
            filter = Some(with_must(filter.as_ref(), nest(open)));
        }
        (None, false) => {
            filter = Some(with_must(filter.as_ref(), is_null(VALID_TO)));
        }
        (None, true) => {}
    }
    filter
}

impl<'a, B: MemoryBackend, E: MemoryEmbedder> MemoryTools<'a, B, E> {
    /// The tools of the toolkit `namespace` in `scope`; `model_slug` names the
    /// deployment's memory embedding space.
    #[must_use]
    pub fn new(
        backend: &'a B,
        embedder: &'a E,
        model_slug: &'a str,
        scope: MemoryScope,
        namespace: &str,
    ) -> Self {
        Self {
            backend,
            embedder,
            model_slug,
            scope,
            namespace_id: namespace_id(namespace),
            clock: system_clock(),
            dimension: Arc::new(AtomicU32::new(0)),
        }
    }

    /// The same tools on another clock (tests).
    #[must_use]
    pub fn with_clock(mut self, clock: Clock) -> Self {
        self.clock = clock;
        self
    }

    /// The same tools sharing a run's dimension cache.
    #[must_use]
    pub fn with_space_cache(mut self, cache: SpaceCache) -> Self {
        self.dimension = cache;
        self
    }

    fn now(&self) -> i64 {
        (self.clock)()
    }

    fn space_of(&self, vector: &[f32]) -> Result<pb::EmbeddingSpace> {
        if vector.is_empty() {
            return Err(Error::Embedding(
                "the embedding model returned an empty vector".to_owned(),
            ));
        }
        let dimension = u32::try_from(vector.len())
            .map_err(|_| Error::Embedding("the embedding vector is too long".to_owned()))?;
        self.dimension.store(dimension, Ordering::Relaxed);
        Ok(pb::EmbeddingSpace {
            model_slug: self.model_slug.to_owned(),
            dimension,
        })
    }

    /// The space a read that embeds nothing can narrow to: this run's, once
    /// it has embedded; otherwise every collection.
    fn known_space(&self) -> Option<pb::EmbeddingSpace> {
        match self.dimension.load(Ordering::Relaxed) {
            0 => None,
            dimension => Some(pb::EmbeddingSpace {
                model_slug: self.model_slug.to_owned(),
                dimension,
            }),
        }
    }

    /// The stored versions of `key` that the view selects, with the end an
    /// unclosed one implies.
    async fn read_versions(
        &self,
        key: &str,
        as_of: Option<i64>,
        history: bool,
    ) -> Result<Vec<Version>> {
        let points = self
            .backend
            .read(
                self.scope,
                &self.namespace_id,
                self.known_space(),
                view_filter(Some(key), as_of, history),
                u32::try_from(MAX_VERSIONS_PER_KEY + 5).unwrap_or(25),
            )
            .await?;
        let mut versions: Vec<Version> = points.iter().map(Version::from_point).collect();
        close_superseded(&mut versions);
        Ok(versions)
    }

    /// `manage_memory`: the SDK's success sentence. An existing key is
    /// superseded: its open version is kept, closed at this instant.
    ///
    /// # Errors
    ///
    /// [`Error::Refused`] for a limit, [`Error::InvalidArgument`] for an
    /// unusable argument, [`Error::Embedding`] or the facade's refusal.
    pub async fn manage_memory(&self, args: &ManageArgs) -> Result<String> {
        let key = match args.key.as_deref() {
            None | Some("") => uuid::Uuid::new_v4().to_string(),
            Some(key) => {
                check_key(key)?;
                key.to_owned()
            }
        };
        let metadata = check_content(args)?;
        let all = self.read_versions(&key, None, true).await?;
        let open: Vec<&Version> = all.iter().filter(|version| version.stored_open).collect();
        // Saying what is already current changes nothing and costs no version.
        if let Some(latest) = all.first()
            && latest.stored_open
            && latest.content == args.content
            && latest.metadata == metadata
        {
            return Ok(format!("Successfully stored memory with key: {key}"));
        }
        let vector = self.embedder.embed(&args.content).await?;
        let space = self.space_of(&vector)?;
        // The oldest versions go to keep the key within its history cap (the
        // new version counts).
        let overflow = (all.len() + 1).saturating_sub(MAX_VERSIONS_PER_KEY);
        let held = self
            .backend
            .count(self.scope, &self.namespace_id, Some(space.clone()), None)
            .await?;
        if held >= MAX_MEMORIES_PER_NAMESPACE && (all.is_empty() || overflow == 0) {
            return Err(Error::Refused(format!(
                "this memory namespace is full: it holds {held} memories and versions, the limit is {MAX_MEMORIES_PER_NAMESPACE}; delete some with delete_memory first"
            )));
        }
        let now = self.now();
        let fresh = Version {
            key: key.clone(),
            content: args.content.clone(),
            metadata,
            id: uuid::Uuid::new_v4().simple().to_string(),
            created_at: all
                .iter()
                .map(|version| version.created_at)
                .min()
                .unwrap_or(now),
            valid_from: now,
            valid_to: None,
            stored_open: true,
            cosine: 0.0,
        };
        // New first, close second: a failure in between leaves two open
        // versions (reads take the newest, and the next write closes both),
        // never none.
        self.backend
            .put(
                self.scope,
                space,
                &self.namespace_id,
                &fresh.generation(),
                fresh.point(vector),
            )
            .await?;
        // Closing is in place: the version keeps its vector, nothing is
        // embedded again.
        for version in open {
            let mut closed = version.clone();
            closed.valid_to = Some(now.max(version.valid_from));
            self.backend
                .set_metadata(
                    self.scope,
                    &self.namespace_id,
                    &VersionRef {
                        key: &closed.key,
                        chunk_id: &closed.chunk_id(),
                        generation: &closed.generation(),
                    },
                    &closed.payload(),
                )
                .await?;
        }
        if overflow > 0 {
            let mut oldest = all;
            oldest.sort_by(|left, right| Version::recency_order(right, left));
            let dropped: Vec<String> = oldest
                .iter()
                .take(overflow)
                .map(Version::generation)
                .collect();
            self.backend
                .delete_generations(self.scope, &self.namespace_id, dropped)
                .await?;
        }
        Ok(format!("Successfully stored memory with key: {key}"))
    }

    /// `search_memory`: the SDK's JSON text, or its "nothing found" sentence.
    /// Open versions by default; `as_of` and `include_history` widen it.
    ///
    /// # Errors
    ///
    /// [`Error::InvalidArgument`] for a `limit` below 1 or an unreadable
    /// `as_of`, [`Error::Embedding`] or the facade's refusal.
    pub async fn search_memory(&self, args: &SearchMemoryArgs) -> Result<String> {
        let limit = match args.limit {
            None => DEFAULT_SEARCH_LIMIT,
            Some(limit) if limit < 1 => {
                return Err(Error::InvalidArgument(
                    "limit must be at least 1".to_owned(),
                ));
            }
            Some(limit) => u32::try_from(limit)
                .unwrap_or(MAX_SEARCH_LIMIT)
                .min(MAX_SEARCH_LIMIT)
                .min(MAX_LIMIT),
        };
        if args.query.trim().is_empty() {
            return Err(Error::InvalidArgument("query must not be empty".to_owned()));
        }
        let as_of = parse_as_of(args.as_of.as_deref())?;
        let history = args.include_history.unwrap_or(false);
        let vector = self.embedder.embed(&args.query).await?;
        let space = self.space_of(&vector)?;
        let points = self
            .backend
            .search(
                self.scope,
                SearchParams {
                    scope: memory_scope_of(&self.namespace_id, self.scope.wire()),
                    space,
                    vector,
                    filter: view_filter(None, as_of, history),
                    limit: (limit * 3).min(150).min(MAX_LIMIT),
                    score_threshold: None,
                },
            )
            .await?;
        if points.is_empty() {
            return Ok("No memories found matching your query.".to_owned());
        }
        let reference = as_of.unwrap_or_else(|| self.now());
        let mut found: Vec<Version> = points.iter().map(Version::from_point).collect();
        if !history {
            // One answer per key: the newest version that matched.
            found.sort_by(Version::recency_order);
            let mut seen = HashSet::new();
            found.retain(|version| seen.insert(version.key.clone()));
        }
        let mut ranked: Vec<(f32, Version)> = found
            .into_iter()
            .map(|version| (ranked_score(&version, reference), version))
            .collect();
        ranked.sort_by(|left, right| {
            right
                .0
                .total_cmp(&left.0)
                .then_with(|| Version::recency_order(&left.1, &right.1))
        });
        ranked.truncate(limit as usize);
        let mut shown: Vec<Version> = ranked.into_iter().map(|(_, version)| version).collect();
        if history {
            // A superseded version's end may be implied by a version the
            // top results do not hold; the stored end is shown as stored.
            shown.sort_by(Version::recency_order);
        }
        Ok(render_list(&shown, history || as_of.is_some()))
    }

    /// `get_memory`: the SDK's JSON text, or its "no memory" sentence. The
    /// open version by default; `as_of` the one valid then; `include_history`
    /// a list of every version, newest first.
    ///
    /// # Errors
    ///
    /// [`Error::InvalidArgument`] for an unusable key or `as_of`, or the
    /// facade's refusal.
    pub async fn get_memory(&self, args: &GetMemoryArgs) -> Result<String> {
        let key = args.key.as_str();
        check_key(key)?;
        let as_of = parse_as_of(args.as_of.as_deref())?;
        let history = args.include_history.unwrap_or(false);
        let versions = self.read_versions(key, as_of, history).await?;
        if versions.is_empty() {
            return Ok(match args.as_of.as_deref() {
                Some(at) => format!("No memory found with key: {key} as of {at}"),
                None => format!("No memory found with key: {key}"),
            });
        }
        if history {
            return Ok(render_list(&versions, true));
        }
        // The newest version that matched; its end (when a race left it
        // open) is not shown, as the view is a single memory.
        Ok(versions[0].render(0, as_of.is_some()))
    }

    /// `delete_memory`: the SDK's success sentence (also for a key that was
    /// not there, as the SDK's store answers). Every version goes: history
    /// is not a reason to keep what a person asked to forget.
    ///
    /// # Errors
    ///
    /// [`Error::InvalidArgument`] for an unusable key or the facade's
    /// refusal.
    pub async fn delete_memory(&self, key: &str) -> Result<String> {
        check_key(key)?;
        self.backend
            .delete_key(self.scope, &self.namespace_id, key)
            .await?;
        Ok(format!("Successfully deleted memory with key: {key}"))
    }
}

/// Validates content and metadata; the metadata to store.
fn check_content(args: &ManageArgs) -> Result<Option<Value>> {
    if args.content.trim().is_empty() {
        return Err(Error::InvalidArgument(
            "content must not be empty".to_owned(),
        ));
    }
    if args.content.len() > MAX_CONTENT_BYTES {
        return Err(Error::Refused(format!(
            "the content is {} bytes; a memory holds at most {MAX_CONTENT_BYTES} bytes",
            args.content.len()
        )));
    }
    let Some(metadata) = args
        .metadata
        .as_ref()
        .filter(|metadata| !metadata.is_empty())
    else {
        return Ok(None);
    };
    let value = Value::Object(metadata.clone());
    let size = value.to_string().len();
    if size > MAX_METADATA_BYTES {
        return Err(Error::Refused(format!(
            "the metadata is {size} bytes as JSON; a memory holds at most {MAX_METADATA_BYTES} bytes"
        )));
    }
    Ok(Some(value))
}

fn parse_as_of(value: Option<&str>) -> Result<Option<i64>> {
    let Some(text) = value.filter(|text| !text.trim().is_empty()) else {
        return Ok(None);
    };
    DateTime::parse_from_rfc3339(text.trim())
        .map(|at| Some(at.timestamp_millis()))
        .map_err(|_| {
            Error::InvalidArgument(
                "as_of must be an RFC 3339 timestamp such as 2026-01-31T12:00:00Z".to_owned(),
            )
        })
}

/// `(1 - w) * cosine + w * recency`; recency halves every
/// [`RECENCY_HALF_LIFE_DAYS`] days of age at `reference`.
fn ranked_score(version: &Version, reference: i64) -> f32 {
    #[allow(clippy::cast_precision_loss, reason = "an age in milliseconds")]
    let age_days = (reference - version.valid_from).max(0) as f64 / 86_400_000.0;
    #[allow(clippy::cast_possible_truncation, reason = "a score in 0..=1")]
    let recency = (-(age_days / RECENCY_HALF_LIFE_DAYS) * std::f64::consts::LN_2).exp() as f32;
    (1.0 - RECENCY_WEIGHT) * version.cosine + RECENCY_WEIGHT * recency
}

fn check_key(key: &str) -> Result<()> {
    if key.is_empty() {
        return Err(Error::InvalidArgument("key must not be empty".to_owned()));
    }
    if key.len() > MAX_KEY_BYTES {
        return Err(Error::InvalidArgument(format!(
            "the key is {} bytes; a key holds at most {MAX_KEY_BYTES} bytes",
            key.len()
        )));
    }
    if key.chars().any(char::is_control) {
        return Err(Error::InvalidArgument(
            "the key must not contain control characters".to_owned(),
        ));
    }
    Ok(())
}

/// `json.dumps([...], indent=2)`.
fn render_list(versions: &[Version], temporal: bool) -> String {
    let items: Vec<String> = versions
        .iter()
        .map(|version| format!("  {}", version.render(1, temporal)))
        .collect();
    format!("[\n{}\n]", items.join(",\n"))
}

/// The sentence a model reads for a failed memory operation, by cause. It
/// holds no token and no provider body.
#[must_use]
pub fn describe(error: &Error) -> String {
    match error {
        Error::InvalidArgument(reason) | Error::Refused(reason) | Error::NotFound(reason) => {
            reason.clone()
        }
        Error::Embedding(reason) => {
            format!("the memory embedding model could not embed the text ({reason})")
        }
        Error::Rpc { code, message } => match code {
            Code::Unauthenticated => {
                "the vector store refused this run's token; it expired or was revoked".to_owned()
            }
            Code::PermissionDenied if message.starts_with("memory.personal_unavailable") => {
                "personal memory is not available to this run: its token names no user, so there is nobody whose memory it could be (a scheduled run acts as the person who owns the schedule and has their memory). Project memory is unaffected".to_owned()
            }
            Code::PermissionDenied if message.starts_with("memory.personal_shared_conversation") => {
                "personal memory is not available in a conversation with more than one person, because what is remembered or recalled would be shown to the others. Use the project scope for shared memory, and only when asked to".to_owned()
            }
            Code::PermissionDenied | Code::FailedPrecondition => {
                "the vector store does not allow memory for this run's project".to_owned()
            }
            Code::InvalidArgument => {
                "the vector store rejected the request as malformed".to_owned()
            }
            Code::Unavailable | Code::DeadlineExceeded | Code::Aborted => {
                "the vector store is unavailable right now; try again in a moment".to_owned()
            }
            Code::ResourceExhausted => {
                "the vector store is busy or the memory is over a store limit; try again later"
                    .to_owned()
            }
            other => format!("the vector store failed the request ({other:?})"),
        },
        Error::Transport(_) => {
            "the vector store is unreachable right now; try again in a moment".to_owned()
        }
        Error::InvalidToken(_) | Error::Filter(_) | Error::Model(_) => {
            "the memory request could not be sent".to_owned()
        }
    }
}

impl<'a> VectorSession<'a> {
    pub(crate) fn client(&self) -> &'a VectorClient {
        self.client
    }

    pub(crate) fn token(&self) -> &'a dyn ClaimToken {
        self.token
    }
}
