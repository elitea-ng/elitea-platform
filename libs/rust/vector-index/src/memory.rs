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
//!   same string in another project is another namespace. As in the SDK,
//!   everyone who can run a toolkit with that namespace in that project sees
//!   the same memories: the toolkit's namespace is the unit, not the user.
//! * **Points.** One point per memory: `document_key` is the memory key, `text`
//!   is the content, `metadata_json` is the metadata object. Writing a key
//!   again replaces it (the point id is derived from the key).
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

use elitea_engine_core::pyjson::dumps_indent2;
use serde_json::{Map, Value};
use sha2::{Digest, Sha256};
use tonic::Code;

use crate::client::{
    ClaimToken, DeleteSelector, MAX_LIMIT, SearchParams, VectorClient, toolkit_namespace,
    toolkit_scope,
};
use crate::error::{Error, Result};
use crate::filter::{keyword, with_must};
use crate::pb;
use crate::search::{VectorBackend, VectorSession};

/// The most memories one namespace holds.
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
const CHUNK_ID: &str = "0";
const DOCUMENT_VERSION: &str = "1";
/// Mixed into the namespace hash, so a memory namespace is a different id
/// space from every other use of a vector namespace UUID.
const NAMESPACE_DOMAIN: &[u8] = b"elitea.memory.namespace.v1\0";

/// The vector namespace UUID of a toolkit `namespace` string: the first 16
/// bytes of `SHA-256(domain || namespace)` with the UUID version (8, custom)
/// and variant bits set. Deterministic, so every run and every worker agrees;
/// the project is not part of it, because the claim token supplies the
/// project on the server side.
#[must_use]
pub fn namespace_id(namespace: &str) -> String {
    let mut hasher = Sha256::new();
    hasher.update(NAMESPACE_DOMAIN);
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

/// Embeds a memory's content or a search query with the deployment's memory
/// embedding model. The vector's length is the embedding space's dimension.
pub trait MemoryEmbedder: Send + Sync {
    /// The embedding of `text`.
    fn embed(&self, text: &str) -> impl Future<Output = Result<Vec<f32>>> + Send;
}

/// What the memory tools need from `elitea-vector`. [`VectorSession`] is the
/// real one; tests use it against the in-process fake.
pub trait MemoryBackend: Send + Sync {
    /// Writes (or replaces) one point of the namespace.
    fn put(
        &self,
        space: pb::EmbeddingSpace,
        namespace_id: &str,
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
    /// Removes the points of one memory key, in every embedding space.
    fn delete_key(&self, namespace_id: &str, key: &str) -> impl Future<Output = Result<()>> + Send;
}

impl MemoryBackend for VectorSession<'_> {
    async fn put(
        &self,
        space: pb::EmbeddingSpace,
        namespace_id: &str,
        point: pb::Point,
    ) -> Result<()> {
        self.client()
            .upsert(
                self.token(),
                space,
                toolkit_namespace(namespace_id),
                vec![point],
            )
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
}

/// The four memory tools of one namespace over a backend and an embedder.
pub struct MemoryTools<'a, B, E> {
    backend: &'a B,
    embedder: &'a E,
    model_slug: &'a str,
    namespace_id: String,
}

impl<'a, B: MemoryBackend, E: MemoryEmbedder> MemoryTools<'a, B, E> {
    /// The tools of the toolkit `namespace`; `model_slug` names the
    /// deployment's memory embedding space.
    #[must_use]
    pub fn new(backend: &'a B, embedder: &'a E, model_slug: &'a str, namespace: &str) -> Self {
        Self {
            backend,
            embedder,
            model_slug,
            namespace_id: namespace_id(namespace),
        }
    }

    /// `manage_memory`: the SDK's success sentence.
    ///
    /// # Errors
    ///
    /// [`Error::Refused`] for a limit, [`Error::InvalidArgument`] for an
    /// unusable argument, [`Error::Embedding`] or the facade's refusal.
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
        let metadata_json = match args
            .metadata
            .as_ref()
            .filter(|metadata| !metadata.is_empty())
        {
            Some(metadata) => {
                let text = Value::Object(metadata.clone()).to_string();
                if text.len() > MAX_METADATA_BYTES {
                    return Err(Error::Refused(format!(
                        "the metadata is {} bytes as JSON; a memory holds at most {MAX_METADATA_BYTES} bytes",
                        text.len()
                    )));
                }
                text
            }
            None => "{}".to_owned(),
        };
        // A full namespace still lets a key it already holds be rewritten.
        let held = self.backend.count(&self.namespace_id, None).await?;
        if held >= MAX_MEMORIES_PER_NAMESPACE {
            let existing = self
                .backend
                .count(
                    &self.namespace_id,
                    Some(with_must(None, keyword("document_key", &key))),
                )
                .await?;
            if existing == 0 {
                return Err(Error::Refused(format!(
                    "this memory namespace is full: it holds {held} memories, the limit is {MAX_MEMORIES_PER_NAMESPACE}; delete some with delete_memory first"
                )));
            }
        }
        let vector = self.embedder.embed(&args.content).await?;
        if vector.is_empty() {
            return Err(Error::Embedding(
                "the embedding model returned an empty vector".to_owned(),
            ));
        }
        let space = pb::EmbeddingSpace {
            model_slug: self.model_slug.to_owned(),
            dimension: u32::try_from(vector.len())
                .map_err(|_| Error::Embedding("the embedding vector is too long".to_owned()))?,
        };
        let point = pb::Point {
            document_key: key.clone(),
            document_version: DOCUMENT_VERSION.to_owned(),
            chunk_id: CHUNK_ID.to_owned(),
            chunk_type: CHUNK_TYPE.to_owned(),
            text: args.content.clone(),
            metadata_json,
            vector,
            ..pb::Point::default()
        };
        self.backend.put(space, &self.namespace_id, point).await?;
        Ok(format!("Successfully stored memory with key: {key}"))
    }

    /// `search_memory`: the SDK's JSON text, or its "nothing found" sentence.
    ///
    /// # Errors
    ///
    /// [`Error::InvalidArgument`] for a `limit` below 1, [`Error::Embedding`]
    /// or the facade's refusal.
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
        let vector = self.embedder.embed(&args.query).await?;
        if vector.is_empty() {
            return Err(Error::Embedding(
                "the embedding model returned an empty vector".to_owned(),
            ));
        }
        let space = pb::EmbeddingSpace {
            model_slug: self.model_slug.to_owned(),
            dimension: u32::try_from(vector.len())
                .map_err(|_| Error::Embedding("the embedding vector is too long".to_owned()))?,
        };
        let points = self
            .backend
            .search(SearchParams {
                scope: toolkit_scope(vec![self.namespace_id.clone()], None),
                space,
                vector,
                filter: None,
                limit,
                score_threshold: None,
            })
            .await?;
        if points.is_empty() {
            return Ok("No memories found matching your query.".to_owned());
        }
        let entries: Vec<Entry> = points.iter().map(Entry::from_point).collect();
        Ok(render_list(&entries))
    }

    /// `get_memory`: the SDK's JSON text, or its "no memory" sentence.
    ///
    /// # Errors
    ///
    /// [`Error::InvalidArgument`] for an unusable key or the facade's
    /// refusal.
    pub async fn get_memory(&self, key: &str) -> Result<String> {
        check_key(key)?;
        let missing = || format!("No memory found with key: {key}");
        // The read is a filtered search; it needs the space's dimension, which
        // the namespace's own points tell.
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
                filter: Some(with_must(None, keyword("document_key", key))),
                limit: 1,
                score_threshold: None,
            })
            .await?;
        Ok(points
            .first()
            .map_or_else(missing, |point| render_one(&Entry::from_point(point))))
    }

    /// `delete_memory`: the SDK's success sentence (also for a key that was
    /// not there, as the SDK's store answers).
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

struct Entry {
    key: String,
    content: String,
    metadata: Option<Value>,
}

impl Entry {
    fn from_point(point: &pb::ScoredPoint) -> Self {
        let metadata = serde_json::from_str::<Value>(&point.metadata_json)
            .ok()
            .filter(|value| value.as_object().is_some_and(|object| !object.is_empty()));
        Self {
            key: point.document_key.clone(),
            content: point.text.clone(),
            metadata,
        }
    }

    /// The object's fields in the SDK's order, each rendered at `depth`
    /// (the nesting level of the object's braces).
    fn render(&self, depth: usize) -> String {
        let pad = "  ".repeat(depth + 1);
        let mut fields = vec![
            format!(
                "{pad}\"key\": {}",
                dumps_indent2(&Value::String(self.key.clone()))
            ),
            format!(
                "{pad}\"content\": {}",
                dumps_indent2(&Value::String(self.content.clone()))
            ),
        ];
        if let Some(metadata) = &self.metadata {
            let nested = dumps_indent2(metadata).replace('\n', &format!("\n{pad}"));
            fields.push(format!("{pad}\"metadata\": {nested}"));
        }
        format!("{{\n{}\n{}}}", fields.join(",\n"), "  ".repeat(depth))
    }
}

/// `json.dumps([...], indent=2)`.
fn render_list(entries: &[Entry]) -> String {
    let items: Vec<String> = entries
        .iter()
        .map(|entry| format!("  {}", entry.render(1)))
        .collect();
    format!("[\n{}\n]", items.join(",\n"))
}

/// `json.dumps({...}, indent=2)`.
fn render_one(entry: &Entry) -> String {
    entry.render(0)
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
