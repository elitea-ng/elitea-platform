//! The SDK `memory` toolkit on `elitea-vector` (worker retirement unit M9).
//!
//! The SDK's four tools (`manage_memory`, `search_memory`, `get_memory`,
//! `delete_memory`) keep a `LangGraph` `PostgresStore` in a user-configured
//! pgvector database, under the namespace `(namespace,)` of the toolkit's
//! settings. Here the same four operations run over `elitea-vector`:
//!
//! * **Scope.** A memory belongs to the claim token's project (the facade
//!   injects it into every call and refuses anything wider) and to the
//!   toolkit's `namespace` string. The namespace becomes the vector namespace
//!   UUID through [`namespace_id`], a hash that no `index_registry` UUID can
//!   equal, so memories are never reachable through an index tool and the
//!   same string in another project is another namespace. The owner is part of
//!   the hash: a project namespace is shared by everyone in the project (the
//!   SDK's behaviour), a personal one belongs to one authenticated user and
//!   is invisible to every other, through every tool.
//! * **Points and time.** One point per VERSION of a memory: `document_key` is
//!   the memory key, `text` is the content, `chunk_id` is `v<version>` and
//!   `metadata_json` is `{"m": <the user's metadata>, "t": {"created_at",
//!   "valid_from", "valid_to", "version"}}` (milliseconds since the epoch;
//!   `valid_to` is null while the version is current). Each version also has
//!   its own generation, `<hash of the key>-v<version>`, which is what lets one
//!   old version be deleted alone. Writing an existing key SUPERSEDES: the new
//!   version is written, then the old one is stamped with `valid_to` (history is
//!   kept, nothing is overwritten). Reads return the current version by default;
//!   `as_of` returns what was valid at that instant, `include_history` every
//!   version. `delete_memory` removes every version at once: privacy beats
//!   history. At most [`MAX_VERSIONS_PER_KEY`] versions are kept per key (the
//!   oldest is deleted to make room). Stamping needs the old vector, which the
//!   store does not return, so the old text is embedded again with the new one
//!   in one batch.
//! * **Recency.** Search ranks by `(1 - w) * cosine + w * recency` with
//!   `w = ` [`RECENCY_WEIGHT`] and `recency = 2^(-age / half-life)`, half-life
//!   [`RECENCY_HALF_LIFE_DAYS`] days, age counted from the version's
//!   `valid_from` to now (or to `as_of`). A small, bounded nudge: cosine still
//!   decides. The store is asked for up to three times the limit and the result
//!   is re-ranked here.
//! * **Ranking.** The SDK builds its store without an index, so its
//!   `search_memory` ignores the query and returns the most recently updated
//!   items. Here the query is embedded and the closest memories come first;
//!   this is the one deliberate behavioural difference.
//! * **Limits.** [`MAX_MEMORIES_PER_NAMESPACE`] memories per namespace,
//!   [`MAX_CONTENT_BYTES`] of content, [`MAX_METADATA_BYTES`] of metadata,
//!   [`MAX_KEY_BYTES`] of key. The SDK has none; a store that grows without
//!   bound is also an embedding bill that grows without bound. The count
//!   check is advisory (two concurrent writes can pass it together).
//! * **Output.** The same sentences and the same `json.dumps(indent=2)` text
//!   as the SDK, with the keys in the SDK's order (`key`, `content`,
//!   `metadata`) whatever `serde_json` is built with.
//!
//! Nothing is read from the SDK's store: memories written under the Python
//! worker are not carried over.

use std::fmt::Write as _;
use std::future::Future;
use std::sync::Arc;

use elitea_engine_core::pyjson::dumps_indent2;
use serde_json::{Map, Value};
use sha2::{Digest, Sha256};
use tonic::Code;

use chrono::{DateTime, SecondsFormat, Utc};

use crate::client::{
    ClaimToken, DeleteSelector, MAX_LIMIT, SearchParams, VectorClient, toolkit_namespace,
    toolkit_scope,
};
use crate::error::{Error, Result};
use crate::filter::{keyword, nest, with_must};
use crate::pb;
use crate::search::{VectorBackend, VectorSession};

/// The most versions one key keeps (the current one included).
pub const MAX_VERSIONS_PER_KEY: u64 = 20;
/// The weight of recency in a search score.
pub const RECENCY_WEIGHT: f32 = 0.1;
/// The recency half-life, in days.
pub const RECENCY_HALF_LIFE_DAYS: f64 = 30.0;
/// The most points (memories, with their versions) one namespace holds.
pub const MAX_MEMORIES_PER_NAMESPACE: u64 = 10_000;
/// The most bytes of content one memory holds.
pub const MAX_CONTENT_BYTES: usize = 16 * 1024;
/// The most bytes of metadata (as JSON) one memory holds.
pub const MAX_METADATA_BYTES: usize = 8 * 1024;
/// The most bytes of key one memory has.
pub const MAX_KEY_BYTES: usize = 256;
/// The most bytes of namespace string a toolkit may set.
pub const MAX_NAMESPACE_BYTES: usize = 256;
/// The SDK's default `limit` of `search_memory`.
pub const DEFAULT_SEARCH_LIMIT: u32 = 5;
/// The most results one `search_memory` returns.
pub const MAX_SEARCH_LIMIT: u32 = 50;

const CHUNK_TYPE: &str = "memory";
const DOCUMENT_VERSION: &str = "1";
/// Mixed into the namespace hash, so a memory namespace is a different id
/// space from every other use of a vector namespace UUID.
const NAMESPACE_DOMAIN: &[u8] = b"elitea.memory.namespace.v2\0";

/// Whose memories a namespace holds.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum MemoryOwner {
    /// Everyone in the project.
    Project,
    /// One user, by the authenticated user id Main stated in the claim.
    User(i64),
}

/// The vector namespace UUID of a toolkit `namespace` string for an owner:
/// the first 16 bytes of `SHA-256` over a domain tag, the owner (a tag byte,
/// and the user id for a user) and the length-prefixed namespace string, with
/// the UUID version (8, custom) and variant bits set. Deterministic, so every
/// run and worker agrees; the project is not part of it, because the claim
/// token supplies the project on the server side. The tag bytes and the length
/// prefix keep the inputs apart: a project namespace never equals a personal
/// one, two users never share one, and a namespace string cannot be crafted to
/// run into the owner bytes.
#[must_use]
pub fn namespace_id(owner: MemoryOwner, namespace: &str) -> String {
    let mut hasher = Sha256::new();
    hasher.update(NAMESPACE_DOMAIN);
    match owner {
        MemoryOwner::Project => hasher.update([0_u8]),
        MemoryOwner::User(user) => {
            hasher.update([1_u8]);
            hasher.update(user.to_be_bytes());
        }
    }
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

    /// The embeddings of `texts`, in order. One gateway call where the
    /// embedder can batch; the default asks one by one.
    fn embed_many(&self, texts: &[String]) -> impl Future<Output = Result<Vec<Vec<f32>>>> + Send {
        async move {
            let mut vectors = Vec::with_capacity(texts.len());
            for text in texts {
                vectors.push(self.embed(text).await?);
            }
            Ok(vectors)
        }
    }
}

/// What the memory tools need from `elitea-vector`. [`VectorSession`] is the
/// real one; tests use it against the in-process fake.
pub trait MemoryBackend: Send + Sync {
    /// Writes (or replaces) one point of the namespace, under `generation`
    /// (one per version, so one version can be deleted alone).
    fn put(
        &self,
        space: pb::EmbeddingSpace,
        namespace_id: &str,
        generation: &str,
        point: pb::Point,
    ) -> impl Future<Output = Result<()>> + Send;
    /// Dense cosine search.
    fn search(
        &self,
        params: SearchParams,
    ) -> impl Future<Output = Result<Vec<pb::ScoredPoint>>> + Send;
    /// The points of the namespace that match `filter`, over every embedding
    /// space.
    fn count(
        &self,
        namespace_id: &str,
        filter: Option<pb::Filter>,
    ) -> impl Future<Output = Result<u64>> + Send;
    /// The dimension of the namespace's points in the model's space, if it
    /// has any.
    fn dimension(
        &self,
        model_slug: &str,
        namespace_id: &str,
    ) -> impl Future<Output = Result<Option<u32>>> + Send;
    /// Removes every version of one memory key, in every embedding space.
    fn delete_key(&self, namespace_id: &str, key: &str) -> impl Future<Output = Result<()>> + Send;
    /// Removes the named versions' generations, in every embedding space.
    fn delete_generations(
        &self,
        namespace_id: &str,
        generations: Vec<String>,
    ) -> impl Future<Output = Result<()>> + Send;
}

impl MemoryBackend for VectorSession<'_> {
    async fn put(
        &self,
        space: pb::EmbeddingSpace,
        namespace_id: &str,
        generation: &str,
        point: pb::Point,
    ) -> Result<()> {
        let mut namespace = toolkit_namespace(namespace_id);
        namespace.generation = generation.to_owned();
        self.client()
            .upsert(self.token(), space, namespace, vec![point])
            .await
            .map(|_| ())
    }

    async fn search(&self, params: SearchParams) -> Result<Vec<pb::ScoredPoint>> {
        VectorBackend::search(self, params).await
    }

    async fn count(&self, namespace_id: &str, filter: Option<pb::Filter>) -> Result<u64> {
        self.client()
            .count(
                self.token(),
                toolkit_scope(vec![namespace_id.to_owned()], None),
                None,
                filter,
            )
            .await
    }

    async fn dimension(&self, model_slug: &str, namespace_id: &str) -> Result<Option<u32>> {
        let indexes = self
            .client()
            .list_indexes(self.token(), Some(pb::Source::ToolkitIndex))
            .await?;
        Ok(indexes
            .into_iter()
            .filter(|index| index.namespace_id == namespace_id && index.point_count > 0)
            .filter_map(|index| index.space)
            .find(|space| space.model_slug == model_slug)
            .map(|space| space.dimension))
    }

    async fn delete_key(&self, namespace_id: &str, key: &str) -> Result<()> {
        self.client()
            .delete(
                self.token(),
                None,
                toolkit_namespace(namespace_id),
                DeleteSelector::Documents(vec![key.to_owned()]),
            )
            .await
            .map(|_| ())
    }

    async fn delete_generations(&self, namespace_id: &str, generations: Vec<String>) -> Result<()> {
        self.client()
            .delete(
                self.token(),
                None,
                toolkit_namespace(namespace_id),
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

/// The four memory tools of one namespace over a backend and an embedder.
pub struct MemoryTools<'a, B, E> {
    backend: &'a B,
    embedder: &'a E,
    model_slug: &'a str,
    namespace_id: String,
    clock: Clock,
}

/// One version of a memory, as stored.
#[derive(Clone, Debug, PartialEq)]
struct Version {
    key: String,
    content: String,
    metadata: Option<Value>,
    number: u64,
    created_at: i64,
    valid_from: i64,
    valid_to: Option<i64>,
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
            number: integer("version")
                .and_then(|v| u64::try_from(v).ok())
                .unwrap_or(1),
            created_at: integer("created_at").unwrap_or(valid_from),
            valid_from,
            valid_to: integer("valid_to"),
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
                "version": self.number,
            }),
        );
        Value::Object(object).to_string()
    }

    fn point(&self, vector: Vec<f32>) -> pb::Point {
        pb::Point {
            document_key: self.key.clone(),
            document_version: DOCUMENT_VERSION.to_owned(),
            chunk_id: format!("v{}", self.number),
            chunk_type: CHUNK_TYPE.to_owned(),
            text: self.content.clone(),
            metadata_json: self.payload(),
            vector,
            ..pb::Point::default()
        }
    }

    fn generation(&self) -> String {
        version_generation(&self.key, self.number)
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
            fields.push(format!("{pad}\"version\": {}", self.number));
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

fn rfc3339(millis: i64) -> String {
    DateTime::<Utc>::from_timestamp_millis(millis)
        .unwrap_or_default()
        .to_rfc3339_opts(SecondsFormat::Millis, true)
}

/// `<first 8 bytes of SHA-256(key), hex>-v<version>`: the generation that
/// names one version of one key (at most 128 bytes of `[A-Za-z0-9._:-]`).
fn version_generation(key: &str, version: u64) -> String {
    let digest = Sha256::digest(key.as_bytes());
    let mut hex = String::with_capacity(16);
    for byte in &digest[..8] {
        let _ = write!(hex, "{byte:02x}");
    }
    format!("{hex}-v{version}")
}

fn number(key: &str, ms: i64) -> pb::Range {
    let _ = key;
    #[allow(
        clippy::cast_precision_loss,
        reason = "milliseconds since the epoch fit f64 exactly"
    )]
    let value = ms as f64;
    pb::Range {
        gt: None,
        gte: None,
        lt: None,
        lte: Some(value),
    }
}

fn range_condition(key: &str, gt: Option<i64>, lte: Option<i64>) -> pb::Condition {
    #[allow(
        clippy::cast_precision_loss,
        reason = "milliseconds since the epoch fit f64 exactly"
    )]
    let as_f64 = |ms: i64| ms as f64;
    let mut range = number(key, 0);
    range.lte = lte.map(as_f64);
    range.gt = gt.map(as_f64);
    pb::Condition {
        key: key.to_owned(),
        r#match: Some(pb::condition::Match::Range(range)),
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
fn view_filter(key: Option<&str>, as_of: Option<i64>, history: bool) -> Option<pb::Filter> {
    let mut filter: Option<pb::Filter> = None;
    if let Some(key) = key {
        filter = Some(with_must(filter.as_ref(), keyword("document_key", key)));
    }
    if let Some(at) = as_of {
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
    } else if !history {
        filter = Some(with_must(filter.as_ref(), is_null(VALID_TO)));
    }
    filter
}

impl<'a, B: MemoryBackend, E: MemoryEmbedder> MemoryTools<'a, B, E> {
    /// The tools of the toolkit `namespace` for `owner`; `model_slug` names
    /// the deployment's memory embedding space.
    #[must_use]
    pub fn new(
        backend: &'a B,
        embedder: &'a E,
        model_slug: &'a str,
        owner: MemoryOwner,
        namespace: &str,
    ) -> Self {
        Self {
            backend,
            embedder,
            model_slug,
            namespace_id: namespace_id(owner, namespace),
            clock: system_clock(),
        }
    }

    /// The same tools on another clock (tests).
    #[must_use]
    pub fn with_clock(mut self, clock: Clock) -> Self {
        self.clock = clock;
        self
    }

    fn now(&self) -> i64 {
        (self.clock)()
    }

    fn space(&self, vector: &[f32]) -> Result<pb::EmbeddingSpace> {
        if vector.is_empty() {
            return Err(Error::Embedding(
                "the embedding model returned an empty vector".to_owned(),
            ));
        }
        Ok(pb::EmbeddingSpace {
            model_slug: self.model_slug.to_owned(),
            dimension: u32::try_from(vector.len())
                .map_err(|_| Error::Embedding("the embedding vector is too long".to_owned()))?,
        })
    }

    /// Every stored version of `key` (at most [`MAX_VERSIONS_PER_KEY`] + a
    /// few), newest first. A filtered search over a unit vector: the filter
    /// decides what matches, the vector only has to be non-zero.
    async fn versions_of(&self, key: &str, dimension: u32) -> Result<Vec<Version>> {
        let mut vector = vec![0.0_f32; dimension as usize];
        vector[0] = 1.0;
        let points = self
            .backend
            .search(SearchParams {
                scope: toolkit_scope(vec![self.namespace_id.clone()], None),
                space: pb::EmbeddingSpace {
                    model_slug: self.model_slug.to_owned(),
                    dimension,
                },
                vector,
                filter: view_filter(Some(key), None, true),
                limit: u32::try_from(MAX_VERSIONS_PER_KEY).unwrap_or(20) + 5,
                score_threshold: None,
            })
            .await?;
        let mut versions: Vec<Version> = points.iter().map(Version::from_point).collect();
        versions.sort_by_key(|version| std::cmp::Reverse(version.number));
        Ok(versions)
    }

    /// `manage_memory`: the SDK's success sentence. An existing key is
    /// superseded: its current version is kept, closed at this instant.
    ///
    /// # Errors
    ///
    /// [`Error::Refused`] for a limit, [`Error::InvalidArgument`] for an
    /// unusable argument, [`Error::Embedding`] or the facade's refusal.
    #[allow(
        clippy::too_many_lines,
        reason = "one write path: validate, find versions, embed once, write new, close old"
    )]
    pub async fn manage_memory(&self, args: &ManageArgs) -> Result<String> {
        let key = match args.key.as_deref() {
            None | Some("") => generated_key(),
            Some(key) => {
                check_key(key)?;
                key.to_owned()
            }
        };
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
        let metadata = match args
            .metadata
            .as_ref()
            .filter(|metadata| !metadata.is_empty())
        {
            Some(metadata) => {
                let value = Value::Object(metadata.clone());
                let text = value.to_string();
                if text.len() > MAX_METADATA_BYTES {
                    return Err(Error::Refused(format!(
                        "the metadata is {} bytes as JSON; a memory holds at most {MAX_METADATA_BYTES} bytes",
                        text.len()
                    )));
                }
                Some(value)
            }
            None => None,
        };
        let dimension = self
            .backend
            .dimension(self.model_slug, &self.namespace_id)
            .await?;
        let mut history = match dimension {
            Some(dimension) => self.versions_of(&key, dimension).await?,
            None => Vec::new(),
        };
        let current: Vec<Version> = history
            .iter()
            .filter(|version| version.valid_to.is_none())
            .cloned()
            .collect();
        // Saying what is already current changes nothing and costs no version.
        if let Some(latest) = current.first()
            && latest.content == args.content
            && latest.metadata == metadata
        {
            return Ok(format!("Successfully stored memory with key: {key}"));
        }
        let held = self.backend.count(&self.namespace_id, None).await?;
        let new_key = history.is_empty();
        if new_key && held >= MAX_MEMORIES_PER_NAMESPACE {
            return Err(Error::Refused(format!(
                "this memory namespace is full: it holds {held} memories and versions, the limit is {MAX_MEMORIES_PER_NAMESPACE}; delete some with delete_memory first"
            )));
        }
        // The oldest versions go to keep the key within its history cap
        // (the new version counts).
        let keep = usize::try_from(MAX_VERSIONS_PER_KEY)
            .unwrap_or(20)
            .saturating_sub(1);
        let mut dropped = Vec::new();
        while history.len() > keep {
            if let Some(oldest) = history.pop() {
                dropped.push(oldest);
            }
        }
        if !new_key && held >= MAX_MEMORIES_PER_NAMESPACE && dropped.is_empty() {
            return Err(Error::Refused(format!(
                "this memory namespace is full: it holds {held} memories and versions, the limit is {MAX_MEMORIES_PER_NAMESPACE}; delete some with delete_memory first"
            )));
        }
        // One embedding call: the new content, then the text of each
        // version about to be closed (their vectors are not returned by the
        // store, and a closed version is written again).
        let mut texts = vec![args.content.clone()];
        texts.extend(current.iter().map(|version| version.content.clone()));
        let mut vectors = self.embedder.embed_many(&texts).await?;
        if vectors.len() != texts.len() {
            return Err(Error::Embedding(
                "the embedding model returned the wrong number of vectors".to_owned(),
            ));
        }
        let new_vector = vectors.remove(0);
        let space = self.space(&new_vector)?;
        let closing: Vec<(Version, Vec<f32>)> = current.into_iter().zip(vectors).collect();
        if closing
            .iter()
            .any(|(_, vector)| vector.len() != new_vector.len())
        {
            return Err(Error::Embedding(
                "the embedding model returned vectors of different lengths".to_owned(),
            ));
        }
        let now = self.now();
        let next = history
            .iter()
            .chain(dropped.iter())
            .map(|version| version.number)
            .max()
            .unwrap_or(0)
            + 1;
        let created_at = history
            .iter()
            .chain(dropped.iter())
            .map(|version| version.created_at)
            .min()
            .unwrap_or(now);
        if !dropped.is_empty() {
            self.backend
                .delete_generations(
                    &self.namespace_id,
                    dropped.iter().map(Version::generation).collect(),
                )
                .await?;
        }
        let fresh = Version {
            key: key.clone(),
            content: args.content.clone(),
            metadata,
            number: next,
            created_at,
            valid_from: now,
            valid_to: None,
            cosine: 0.0,
        };
        // New first, close second: a failure in between leaves two current
        // versions (reads take the newest), never none.
        self.backend
            .put(
                space.clone(),
                &self.namespace_id,
                &fresh.generation(),
                fresh.point(new_vector),
            )
            .await?;
        for (mut old, vector) in closing {
            old.valid_to = Some(now);
            self.backend
                .put(
                    space.clone(),
                    &self.namespace_id,
                    &old.generation(),
                    old.point(vector),
                )
                .await?;
        }
        Ok(format!("Successfully stored memory with key: {key}"))
    }

    /// `search_memory`: the SDK's JSON text, or its "nothing found" sentence.
    /// Current versions by default; `as_of` and `include_history` widen it.
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
        let space = self.space(&vector)?;
        let points = self
            .backend
            .search(SearchParams {
                scope: toolkit_scope(vec![self.namespace_id.clone()], None),
                space,
                vector,
                filter: view_filter(None, as_of, history),
                limit: (limit * 3).min(150).min(MAX_LIMIT),
                score_threshold: None,
            })
            .await?;
        if points.is_empty() {
            return Ok("No memories found matching your query.".to_owned());
        }
        let reference = as_of.unwrap_or_else(|| self.now());
        let mut found: Vec<Version> = points.iter().map(Version::from_point).collect();
        if !history {
            // One answer per key: the newest version that matched.
            found.sort_by_key(|version| std::cmp::Reverse(version.number));
            let mut seen = std::collections::HashSet::new();
            found.retain(|version| seen.insert(version.key.clone()));
        }
        let mut ranked: Vec<(f32, Version)> = found
            .into_iter()
            .map(|version| (ranked_score(&version, reference), version))
            .collect();
        ranked.sort_by(|a, b| {
            b.0.total_cmp(&a.0)
                .then_with(|| b.1.number.cmp(&a.1.number))
                .then_with(|| a.1.key.cmp(&b.1.key))
        });
        ranked.truncate(limit as usize);
        let temporal = history || as_of.is_some();
        Ok(render_list(
            &ranked
                .into_iter()
                .map(|(_, version)| version)
                .collect::<Vec<_>>(),
            temporal,
        ))
    }

    /// `get_memory`: the SDK's JSON text, or its "no memory" sentence. The
    /// current version by default; `as_of` the one valid then;
    /// `include_history` a list of every version, newest first.
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
        let missing = || match args.as_of.as_deref() {
            Some(at) => format!("No memory found with key: {key} as of {at}"),
            None => format!("No memory found with key: {key}"),
        };
        let Some(dimension) = self
            .backend
            .dimension(self.model_slug, &self.namespace_id)
            .await?
        else {
            return Ok(missing());
        };
        let mut vector = vec![0.0_f32; dimension as usize];
        vector[0] = 1.0;
        let points = self
            .backend
            .search(SearchParams {
                scope: toolkit_scope(vec![self.namespace_id.clone()], None),
                space: pb::EmbeddingSpace {
                    model_slug: self.model_slug.to_owned(),
                    dimension,
                },
                vector,
                filter: view_filter(Some(key), as_of, history),
                limit: u32::try_from(MAX_VERSIONS_PER_KEY).unwrap_or(20) + 5,
                score_threshold: None,
            })
            .await?;
        let mut versions: Vec<Version> = points.iter().map(Version::from_point).collect();
        versions.sort_by_key(|version| std::cmp::Reverse(version.number));
        if versions.is_empty() {
            return Ok(missing());
        }
        if history {
            return Ok(render_list(&versions, true));
        }
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
        self.backend.delete_key(&self.namespace_id, key).await?;
        Ok(format!("Successfully deleted memory with key: {key}"))
    }
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

fn generated_key() -> String {
    uuid::Uuid::new_v4().to_string()
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
        Error::Rpc { code, .. } => match code {
            Code::Unauthenticated => {
                "the vector store refused this run's token; it expired or was revoked".to_owned()
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
