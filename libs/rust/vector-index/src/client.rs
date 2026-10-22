//! A typed client for `elitea.vector.v1` (ADR-0031 decision 1).
//!
//! The client speaks gRPC over mTLS to `elitea-vector`. Every call carries
//! `authorization: Bearer <token>` metadata, taken from the claim's
//! [`ClaimToken`]; the facade verifies it, injects the token's project into
//! the request and refuses anything else. The client never sets a
//! `project_id` itself (zero means "the token's project").

use std::fmt;
use std::ops::ControlFlow;
use std::time::Duration;

use tonic::Request;
use tonic::metadata::MetadataValue;
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Endpoint, Identity};

use crate::error::{Error, Result};
use crate::pb;
use crate::pb::vector_service_client::VectorServiceClient;

/// The most points `elitea-vector` accepts in one upsert batch.
pub const MAX_BATCH_POINTS: usize = 512;
/// The most results one search may ask for.
pub const MAX_LIMIT: u32 = 1000;
/// The deepest a search page may reach: `offset + limit` is at most this.
pub const MAX_SEARCH_DEPTH: u32 = 10_000;
/// The most results one search of an exactly searched source
/// ([`pb::Source::Inventory`]) may ask for: its whole depth, so a caller
/// that needs the whole ranking reads it with one exact scan instead of one
/// scan per page.
pub const MAX_EXACT_LIMIT: u32 = MAX_SEARCH_DEPTH;
/// The largest response the client decodes: `elitea-vector`'s own message
/// bound. A search of [`MAX_EXACT_LIMIT`] results is past tonic's 4 MiB
/// default.
const MAX_RESPONSE_BYTES: usize = 64 << 20;

/// The handle toolkit code holds for the claim it runs under. The worker's
/// `VectorClaimToken` implements it; nothing else may read the bearer, and
/// this crate reads it only to build the `authorization` header.
pub trait ClaimToken: Send + Sync {
    /// The bearer, without the `Bearer ` prefix.
    fn bearer(&self) -> &str;
}

impl ClaimToken for String {
    fn bearer(&self) -> &str {
        self
    }
}

/// How to reach `elitea-vector`.
#[derive(Clone)]
pub struct ClientConfig {
    /// `https://host:port`. A plaintext `http://` endpoint is refused unless
    /// [`Self::allow_insecure`] is set, so a bearer token never travels in
    /// cleartext by accident.
    pub endpoint: String,
    /// The PEM of the CA that signed the server certificate. Without it the
    /// platform roots would be needed, and the crate links none.
    pub ca_pem: Option<Vec<u8>>,
    /// The worker's client certificate and key, PEM, for mTLS.
    pub identity_pem: Option<(Vec<u8>, Vec<u8>)>,
    /// The name to verify the server certificate against, when it differs
    /// from the endpoint's host.
    pub server_name: Option<String>,
    /// How long to wait for a connection.
    pub connect_timeout: Duration,
    /// The deadline of one call. A search is well under a second; an upsert
    /// batch of 512 points is slower.
    pub request_timeout: Duration,
    /// Allows a plaintext `http://` endpoint, for tests against an
    /// in-process server. Off by default; never set it in a deployment.
    pub allow_insecure: bool,
}

impl ClientConfig {
    /// A config for `endpoint` with the platform's default timeouts (5 s to
    /// connect, 60 s per call).
    #[must_use]
    pub fn new(endpoint: impl Into<String>) -> Self {
        Self {
            endpoint: endpoint.into(),
            ca_pem: None,
            identity_pem: None,
            server_name: None,
            connect_timeout: Duration::from_secs(5),
            request_timeout: Duration::from_mins(1),
            allow_insecure: false,
        }
    }
}

impl fmt::Debug for ClientConfig {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ClientConfig")
            .field("endpoint", &self.endpoint)
            .field("ca_pem", &self.ca_pem.as_ref().map(|_| "<pem>"))
            .field(
                "identity_pem",
                &self.identity_pem.as_ref().map(|_| "<redacted>"),
            )
            .field("server_name", &self.server_name)
            .field("allow_insecure", &self.allow_insecure)
            .finish_non_exhaustive()
    }
}

/// Which points of a namespace a delete removes.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum DeleteSelector {
    /// Every point of the namespace.
    WholeNamespace,
    /// The points of these documents (by `document_key`).
    Documents(Vec<String>),
    /// The points of these generations.
    Generations(Vec<String>),
    /// The points of every generation but this one: the lazy clean-up after
    /// a publish.
    GenerationsExcept(String),
}

/// What an upsert wrote.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct UpsertSummary {
    /// Points written.
    pub upserted: u64,
    /// The Qdrant collection they went into (empty when nothing was sent).
    pub collection: String,
}

/// One search, dense.
#[derive(Clone, Debug)]
pub struct SearchParams {
    /// Where to look.
    pub scope: pb::Scope,
    /// The embedding space (collection) of the vector.
    pub space: pb::EmbeddingSpace,
    /// The query vector, of the space's dimension.
    pub vector: Vec<f32>,
    /// Narrowing conditions, added to the injected scope.
    pub filter: Option<pb::Filter>,
    /// 1 to 1000.
    pub limit: u32,
    /// Drops results below this cosine similarity, server side.
    pub score_threshold: Option<f32>,
}

/// One search, dense fused with BM25 over the chunk text.
#[derive(Clone, Debug)]
pub struct HybridSearchParams {
    /// The dense side.
    pub search: SearchParams,
    /// The BM25 query.
    pub text: String,
    /// Weight of the dense ranks. Both weights zero means 0.7 / 0.3.
    pub dense_weight: f32,
    /// Weight of the text ranks.
    pub text_weight: f32,
}

/// A client of `elitea-vector`. Cloning is cheap: clones share one channel.
#[derive(Clone)]
pub struct VectorClient {
    inner: VectorServiceClient<Channel>,
}

impl fmt::Debug for VectorClient {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("VectorClient")
            .finish_non_exhaustive()
    }
}

impl VectorClient {
    /// A client over an existing channel (tests, or a host that builds its
    /// own transport).
    #[must_use]
    pub fn from_channel(channel: Channel) -> Self {
        Self {
            inner: VectorServiceClient::new(channel).max_decoding_message_size(MAX_RESPONSE_BYTES),
        }
    }

    /// A client that connects on first use and reconnects when the
    /// connection drops, so a worker can build it at startup before
    /// `elitea-vector` is up.
    ///
    /// The scheme decides the transport, compared case-insensitively:
    /// `https` is TLS (with the configured CA, identity and server name);
    /// `http` is plaintext and allowed only with
    /// [`ClientConfig::allow_insecure`] and no TLS material, since every
    /// call carries a bearer token.
    ///
    /// # Errors
    ///
    /// [`Error::Transport`] when the endpoint or the certificates are
    /// malformed, when TLS material is configured for a non-`https`
    /// endpoint, or when a plaintext endpoint is not explicitly allowed.
    pub fn connect_lazy(config: &ClientConfig) -> Result<Self> {
        let (scheme, rest) = config
            .endpoint
            .split_once("://")
            .ok_or_else(|| Error::Transport("invalid endpoint: it has no scheme".to_owned()))?;
        let scheme = scheme.to_ascii_lowercase();
        let has_tls_material = config.ca_pem.is_some() || config.identity_pem.is_some();
        let refusal = if scheme == "https" {
            None
        } else if scheme != "http" {
            Some("invalid endpoint: the scheme must be https")
        } else if has_tls_material {
            Some(
                "TLS certificates are configured but the endpoint is not https://; refusing to send the token in cleartext",
            )
        } else if config.allow_insecure {
            None
        } else {
            Some(
                "the endpoint is plaintext http://; elitea-vector is served over TLS, and a plaintext endpoint is allowed only for tests (allow_insecure)",
            )
        };
        if let Some(message) = refusal {
            return Err(Error::Transport(message.to_owned()));
        }
        let mut endpoint = Endpoint::from_shared(format!("{scheme}://{rest}"))
            .map_err(|error| Error::Transport(format!("invalid endpoint: {error}")))?
            .connect_timeout(config.connect_timeout)
            .timeout(config.request_timeout)
            .tcp_nodelay(true);
        if scheme == "https" {
            let mut tls = ClientTlsConfig::new();
            if let Some(ca) = &config.ca_pem {
                tls = tls.ca_certificate(Certificate::from_pem(ca));
            }
            if let Some((certificate, key)) = &config.identity_pem {
                tls = tls.identity(Identity::from_pem(certificate, key));
            }
            if let Some(name) = &config.server_name {
                tls = tls.domain_name(name.clone());
            }
            endpoint = endpoint
                .tls_config(tls)
                .map_err(|error| Error::Transport(format!("invalid TLS config: {error}")))?;
        }
        Ok(Self::from_channel(endpoint.connect_lazy()))
    }

    /// Writes points into one namespace. `points` are sent in batches of at
    /// most [`MAX_BATCH_POINTS`] on one stream; the stream is one unit of
    /// authentication and scope, not one transaction (batches already
    /// written stay written when a later one fails).
    ///
    /// # Errors
    ///
    /// [`Error::InvalidArgument`] for a vector whose length differs from the
    /// space's dimension (nothing is sent), [`Error::InvalidToken`], or the
    /// facade's refusal.
    pub async fn upsert(
        &self,
        token: &dyn ClaimToken,
        space: pb::EmbeddingSpace,
        namespace: pb::Namespace,
        points: Vec<pb::Point>,
    ) -> Result<UpsertSummary> {
        if points.is_empty() {
            return Ok(UpsertSummary {
                upserted: 0,
                collection: String::new(),
            });
        }
        let dimension = space.dimension as usize;
        if let Some(bad) = points.iter().find(|point| point.vector.len() != dimension) {
            return Err(Error::InvalidArgument(format!(
                "the point {:?} has a vector of {} dimensions, the embedding space has {dimension}",
                bad.chunk_id,
                bad.vector.len()
            )));
        }
        let mut messages = Vec::with_capacity(1 + points.len().div_ceil(MAX_BATCH_POINTS));
        messages.push(pb::UpsertRequest {
            message: Some(pb::upsert_request::Message::Header(pb::UpsertHeader {
                project_id: 0,
                space: Some(space),
                namespace: Some(namespace),
            })),
        });
        let mut points = points.into_iter().peekable();
        while points.peek().is_some() {
            let batch: Vec<pb::Point> = points.by_ref().take(MAX_BATCH_POINTS).collect();
            messages.push(pb::UpsertRequest {
                message: Some(pb::upsert_request::Message::Batch(pb::UpsertBatch {
                    points: batch,
                })),
            });
        }
        let request = authorize(token, tokio_stream::iter(messages))?;
        let response = self.inner.clone().upsert(request).await?.into_inner();
        Ok(UpsertSummary {
            upserted: response.upserted,
            collection: response.collection,
        })
    }

    /// Deletes points of one namespace. `space` narrows the delete to one
    /// collection; without it every collection is tried. Returns the number
    /// of collections the delete was applied to.
    ///
    /// # Errors
    ///
    /// [`Error::InvalidToken`] or the facade's refusal.
    pub async fn delete(
        &self,
        token: &dyn ClaimToken,
        space: Option<pb::EmbeddingSpace>,
        namespace: pb::Namespace,
        selector: DeleteSelector,
    ) -> Result<u32> {
        let selector = match selector {
            DeleteSelector::WholeNamespace => {
                pb::delete_request::Selector::WholeNamespace(pb::WholeNamespace {})
            }
            DeleteSelector::Documents(document_keys) => {
                pb::delete_request::Selector::Documents(pb::DocumentKeys { document_keys })
            }
            DeleteSelector::Generations(generations) => {
                pb::delete_request::Selector::Generations(pb::Generations { generations })
            }
            DeleteSelector::GenerationsExcept(keep) => {
                pb::delete_request::Selector::GenerationsExcept(pb::GenerationsExcept { keep })
            }
        };
        let request = authorize(
            token,
            pb::DeleteRequest {
                project_id: 0,
                space,
                namespace: Some(namespace),
                selector: Some(selector),
            },
        )?;
        Ok(self
            .inner
            .clone()
            .delete(request)
            .await?
            .into_inner()
            .collections)
    }

    /// Dense cosine search: the first page of the ranking.
    ///
    /// # Errors
    ///
    /// [`Error::InvalidToken`] or the facade's refusal.
    pub async fn search(
        &self,
        token: &dyn ClaimToken,
        params: SearchParams,
    ) -> Result<Vec<pb::ScoredPoint>> {
        self.search_page(token, params, 0).await
    }

    /// Dense cosine search: the page of `params.limit` results that starts
    /// `offset` results down the ranking. `limit` is at most [`MAX_LIMIT`]
    /// ([`MAX_EXACT_LIMIT`] for [`pb::Source::Inventory`]), and
    /// `offset + limit` at most [`MAX_SEARCH_DEPTH`].
    ///
    /// The ranking is by score, highest first, then by point id, so
    /// consecutive pages over unchanged points are disjoint and in order. For
    /// [`pb::Source::Inventory`] the facade scans exactly, so the ranking is
    /// the exact cosine ranking; for the other sources it is an HNSW walk's.
    ///
    /// # Errors
    ///
    /// [`Error::InvalidToken`] or the facade's refusal (`INVALID_ARGUMENT`
    /// past the depth bound).
    pub async fn search_page(
        &self,
        token: &dyn ClaimToken,
        params: SearchParams,
        offset: u32,
    ) -> Result<Vec<pb::ScoredPoint>> {
        let request = authorize(
            token,
            pb::SearchRequest {
                scope: Some(params.scope),
                space: Some(params.space),
                vector: params.vector,
                filter: params.filter,
                limit: params.limit,
                score_threshold: params.score_threshold,
                offset,
            },
        )?;
        Ok(self
            .inner
            .clone()
            .search(request)
            .await?
            .into_inner()
            .points)
    }

    /// Dense search fused with BM25 over the chunk text by weighted
    /// reciprocal rank fusion. The returned `score`s are the fused scores;
    /// every point also carries its dense cosine `dense_score`.
    ///
    /// # Errors
    ///
    /// [`Error::InvalidToken`], the facade's refusal, or [`Error::Rpc`]
    /// (`UNIMPLEMENTED`) when the facade predates dense scores on hybrid
    /// results.
    pub async fn hybrid_search(
        &self,
        token: &dyn ClaimToken,
        params: HybridSearchParams,
    ) -> Result<Vec<pb::ScoredPoint>> {
        let HybridSearchParams {
            search,
            text,
            dense_weight,
            text_weight,
        } = params;
        let request = authorize(
            token,
            pb::HybridSearchRequest {
                scope: Some(search.scope),
                space: Some(search.space),
                vector: search.vector,
                text,
                filter: search.filter,
                limit: search.limit,
                dense_weight,
                text_weight,
                prefetch_limit: 0,
                score_threshold: search.score_threshold,
            },
        )?;
        let points = self
            .inner
            .clone()
            .hybrid_search(request)
            .await?
            .into_inner()
            .points;
        if points.iter().any(|point| point.dense_score.is_none()) {
            return Err(Error::Rpc {
                code: tonic::Code::Unimplemented,
                message: "elitea-vector returned hybrid results without dense scores; it is older than this client".to_owned(),
            });
        }
        Ok(points)
    }

    /// Counts the points matching a scope and a filter.
    ///
    /// # Errors
    ///
    /// [`Error::InvalidToken`] or the facade's refusal.
    pub async fn count(
        &self,
        token: &dyn ClaimToken,
        scope: pb::Scope,
        space: Option<pb::EmbeddingSpace>,
        filter: Option<pb::Filter>,
    ) -> Result<u64> {
        let request = authorize(
            token,
            pb::CountRequest {
                scope: Some(scope),
                space,
                filter,
            },
        )?;
        Ok(self.inner.clone().count(request).await?.into_inner().count)
    }

    /// Lists the token's project's namespaces, per embedding space. `source`
    /// narrows the list to one source.
    ///
    /// # Errors
    ///
    /// [`Error::InvalidToken`] or the facade's refusal.
    pub async fn list_indexes(
        &self,
        token: &dyn ClaimToken,
        source: Option<pb::Source>,
    ) -> Result<Vec<pb::IndexInfo>> {
        let request = authorize(
            token,
            pb::ListIndexesRequest {
                project_id: 0,
                source: source.map_or(0, |source| source as i32),
            },
        )?;
        Ok(self
            .inner
            .clone()
            .list_indexes(request)
            .await?
            .into_inner()
            .indexes)
    }
}

impl VectorClient {
    /// Reads the points of one memory namespace by filter, with no query
    /// vector. `space` narrows the read to one collection; without it every
    /// collection is read. The service narrows the read to the token's own
    /// memories for `scope`.
    ///
    /// # Errors
    ///
    /// [`Error::InvalidToken`] or the facade's refusal.
    pub async fn read_memory(
        &self,
        token: &dyn ClaimToken,
        target: &MemoryTarget,
        scope: pb::MemoryScope,
        space: Option<pb::EmbeddingSpace>,
        filter: Option<pb::Filter>,
        limit: u32,
    ) -> Result<Vec<pb::ScoredPoint>> {
        let request = authorize(
            token,
            pb::ReadMemoryRequest {
                scope: Some(memory_scope_of(target, scope)),
                space,
                filter,
                limit,
            },
        )?;
        Ok(self
            .inner
            .clone()
            .read_memory(request)
            .await?
            .into_inner()
            .points)
    }

    /// Replaces the metadata of one memory version in place, keeping its
    /// vector. `generation` names the version's generation.
    ///
    /// # Errors
    ///
    /// [`Error::InvalidToken`] or the facade's refusal.
    #[allow(
        clippy::too_many_arguments,
        reason = "each argument names one part of the point"
    )]
    pub async fn set_memory_metadata(
        &self,
        token: &dyn ClaimToken,
        target: &MemoryTarget,
        scope: pb::MemoryScope,
        generation: &str,
        space: Option<pb::EmbeddingSpace>,
        document_key: &str,
        chunk_id: &str,
        metadata_json: &str,
    ) -> Result<()> {
        let request = authorize(
            token,
            pb::SetMemoryMetadataRequest {
                namespace: Some(memory_namespace(target, scope, generation)),
                space,
                document_key: document_key.to_owned(),
                chunk_id: chunk_id.to_owned(),
                metadata_json: metadata_json.to_owned(),
            },
        )?;
        self.inner.clone().set_memory_metadata(request).await?;
        Ok(())
    }

    /// Lists the documents of one toolkit-index namespace as the store holds
    /// them: each (`document_key`, `document_version`) with its chunk count,
    /// in `document_key` order. `space` narrows the read to one collection;
    /// without it every collection is read. An incremental indexer compares
    /// this with the source's listing (ADR-0030 decision 3).
    ///
    /// The whole listing is collected, with no bound but the facade's own; a
    /// caller that wants to fold the pages as they arrive, bound the
    /// listing, or stop early, uses [`Self::list_documents_with`].
    ///
    /// # Errors
    ///
    /// [`Error::InvalidToken`] or the facade's refusal.
    pub async fn list_documents(
        &self,
        token: &dyn ClaimToken,
        namespace: pb::Namespace,
        space: Option<pb::EmbeddingSpace>,
    ) -> Result<Vec<pb::IndexedDocument>> {
        let mut documents = Vec::new();
        // The sink never breaks, so the listing is always consumed whole.
        let _consumed = self
            .list_documents_with(token, namespace, space, None, |document| {
                documents.push(document);
                ControlFlow::Continue(())
            })
            .await?;
        Ok(documents)
    }

    /// [`Self::list_documents`], handing each document to `sink` as its page
    /// arrives, so the caller folds the listing into what it needs without
    /// holding every page: the facade streams pages of at most 1024
    /// documents and this reads one at a time. `max_documents` bounds the
    /// facade's work: it stops reading and refuses the call
    /// (`RESOURCE_EXHAUSTED`, [`Error::Rpc`]) as soon as the distinct
    /// documents exceed it, before any page goes out; `None` leaves only its
    /// own point bound. `sink` returns [`ControlFlow::Break`] to stop the
    /// listing (the stream is dropped, which cancels the call); the result
    /// says which of the two happened.
    ///
    /// # Errors
    ///
    /// [`Error::InvalidToken`] or the facade's refusal.
    pub async fn list_documents_with(
        &self,
        token: &dyn ClaimToken,
        namespace: pb::Namespace,
        space: Option<pb::EmbeddingSpace>,
        max_documents: Option<u64>,
        mut sink: impl FnMut(pb::IndexedDocument) -> ControlFlow<()>,
    ) -> Result<ControlFlow<()>> {
        let request = authorize(
            token,
            pb::ListDocumentsRequest {
                project_id: 0,
                namespace: Some(namespace),
                space,
                max_documents: max_documents.unwrap_or(0),
            },
        )?;
        let mut pages = self
            .inner
            .clone()
            .list_documents(request)
            .await?
            .into_inner();
        while let Some(page) = pages.message().await? {
            for document in page.documents {
                if sink(document).is_break() {
                    return Ok(ControlFlow::Break(()));
                }
            }
        }
        Ok(ControlFlow::Continue(()))
    }
}

/// Wraps a message with the claim's `authorization: Bearer …` metadata.
/// The header is marked sensitive so transports and logs do not print it.
fn authorize<T>(token: &dyn ClaimToken, message: T) -> Result<Request<T>> {
    let mut request = Request::new(message);
    let mut value =
        MetadataValue::try_from(format!("Bearer {}", token.bearer())).map_err(|_| {
            Error::InvalidToken("it holds characters gRPC metadata cannot carry".to_owned())
        })?;
    value.set_sensitive(true);
    request.metadata_mut().insert("authorization", value);
    Ok(request)
}

/// A search scope over some toolkit-index namespaces. `namespace_ids` is 1
/// to 64 UUIDs; the project is the token's.
///
/// `active_generation`, when set, is the generation every listed namespace
/// is published at: a point matches when it carries that generation or no
/// generation at all (an index written incrementally), so the points of a
/// build still in progress are not searched. Main knows each index's active
/// generation and supplies it through the [`crate::IndexCatalog`]; use
/// [`toolkit_scope_with_generations`] when the namespaces differ.
#[must_use]
pub fn toolkit_scope(namespace_ids: Vec<String>, active_generation: Option<&str>) -> pb::Scope {
    let active_generations = active_generation
        .map(|generation| {
            namespace_ids
                .iter()
                .map(|namespace_id| pb::ActiveGeneration {
                    namespace_id: namespace_id.clone(),
                    generation: generation.to_owned(),
                })
                .collect()
        })
        .unwrap_or_default();
    pb::Scope {
        project_id: 0,
        source: pb::Source::ToolkitIndex as i32,
        namespace_ids,
        generation: String::new(),
        active_generations,
        memory_scope: 0,
        toolkit_id: 0,
    }
}

/// A search scope over toolkit-index namespaces, each with its own active
/// generation (or none). See [`toolkit_scope`].
#[must_use]
pub fn toolkit_scope_with_generations(namespaces: &[(String, Option<String>)]) -> pb::Scope {
    pb::Scope {
        project_id: 0,
        source: pb::Source::ToolkitIndex as i32,
        namespace_ids: namespaces.iter().map(|(id, _)| id.clone()).collect(),
        generation: String::new(),
        active_generations: namespaces
            .iter()
            .filter_map(|(namespace_id, generation)| {
                generation.as_ref().map(|generation| pb::ActiveGeneration {
                    namespace_id: namespace_id.clone(),
                    generation: generation.clone(),
                })
            })
            .collect(),
        memory_scope: 0,
        toolkit_id: 0,
    }
}

/// A search scope over Inventory namespaces (no generation).
#[must_use]
pub fn inventory_scope(namespace_ids: Vec<String>) -> pb::Scope {
    pb::Scope {
        project_id: 0,
        source: pb::Source::Inventory as i32,
        namespace_ids,
        generation: String::new(),
        active_generations: Vec::new(),
        memory_scope: 0,
        toolkit_id: 0,
    }
}

/// The Inventory namespace `namespace_id`, without a generation.
#[must_use]
pub fn inventory_namespace(namespace_id: &str) -> pb::Namespace {
    pb::Namespace {
        source: pb::Source::Inventory as i32,
        ..toolkit_namespace(namespace_id)
    }
}

/// The toolkit-index namespace `namespace_id`, without a generation.
#[must_use]
pub fn toolkit_namespace(namespace_id: &str) -> pb::Namespace {
    pb::Namespace {
        source: pb::Source::ToolkitIndex as i32,
        namespace_id: namespace_id.to_owned(),
        generation: String::new(),
        memory_scope: 0,
        toolkit_id: 0,
    }
}

/// The memory namespace `namespace_id` of `scope`, under `generation` (empty:
/// none). The OWNER is never named here: the service takes it from the token.
#[must_use]
pub fn memory_namespace(
    target: &MemoryTarget,
    scope: pb::MemoryScope,
    generation: &str,
) -> pb::Namespace {
    pb::Namespace {
        source: pb::Source::Memory as i32,
        namespace_id: target.namespace_id.clone(),
        generation: generation.to_owned(),
        memory_scope: scope as i32,
        toolkit_id: target.toolkit_id,
    }
}

/// A read scope over the one memory namespace of `target` in `scope`.
#[must_use]
pub fn memory_scope_of(target: &MemoryTarget, scope: pb::MemoryScope) -> pb::Scope {
    pb::Scope {
        project_id: 0,
        source: pb::Source::Memory as i32,
        namespace_ids: vec![target.namespace_id.clone()],
        generation: String::new(),
        active_generations: Vec::new(),
        memory_scope: scope as i32,
        toolkit_id: target.toolkit_id,
    }
}

/// The memory store one toolkit's tools address: the toolkit (which the
/// service stamps on every point and narrows every operation by) and the
/// namespace id hashed from the toolkit and its `namespace` label.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct MemoryTarget {
    /// The memory toolkit's id.
    pub toolkit_id: i64,
    /// The vector namespace UUID.
    pub namespace_id: String,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_bearer_header_is_sensitive_and_prefixed() {
        let token = "elvc_secret".to_owned();
        let request = authorize(&token, ()).expect("valid");
        let value = request.metadata().get("authorization").expect("set");
        assert!(value.is_sensitive());
        assert_eq!(value.to_str().expect("ascii"), "Bearer elvc_secret");
    }

    #[test]
    fn a_bearer_with_control_characters_is_refused_without_echoing_it() {
        let token = "bad\ntoken".to_owned();
        let error = authorize(&token, ()).expect_err("refused");
        assert!(matches!(error, Error::InvalidToken(_)));
        assert!(!error.to_string().contains("bad"));
    }

    fn refusal(config: &ClientConfig) -> String {
        match VectorClient::connect_lazy(config) {
            Err(Error::Transport(message)) => message,
            Err(other) => panic!("{other}"),
            Ok(_) => panic!("{config:?} was accepted"),
        }
    }

    #[tokio::test]
    async fn the_scheme_decides_tls_case_insensitively() {
        // https in any case builds a TLS channel.
        for endpoint in [
            "https://vector:9443",
            "HTTPS://vector:9443",
            "Https://vector:9443",
        ] {
            let mut config = ClientConfig::new(endpoint);
            config.server_name = Some("vector".to_owned());
            assert!(VectorClient::connect_lazy(&config).is_ok(), "{endpoint}");
        }
        // TLS material with a non-https endpoint is refused, even when
        // plaintext is allowed.
        for endpoint in ["http://vector:9443", "HTTP://vector:9443"] {
            let mut config = ClientConfig::new(endpoint);
            config.allow_insecure = true;
            config.ca_pem = Some(b"CA".to_vec());
            assert!(refusal(&config).contains("not https"), "{endpoint}");
            config.ca_pem = None;
            config.identity_pem = Some((b"CERT".to_vec(), b"KEY".to_vec()));
            assert!(refusal(&config).contains("not https"), "{endpoint}");
        }
        // Plaintext without the explicit flag is refused.
        let plain = ClientConfig::new("http://vector:9443");
        assert!(refusal(&plain).contains("allow_insecure"));
        let mut allowed = plain.clone();
        allowed.allow_insecure = true;
        assert!(VectorClient::connect_lazy(&allowed).is_ok());
        // Anything else is not an endpoint.
        for endpoint in ["vector:9443", "grpc://vector:9443", "ftp://vector"] {
            let mut config = ClientConfig::new(endpoint);
            config.allow_insecure = true;
            let _ = refusal(&config);
        }
    }

    #[test]
    fn scopes_carry_the_active_generation_per_namespace() {
        let none = toolkit_scope(vec!["a".to_owned()], None);
        assert!(none.active_generations.is_empty() && none.generation.is_empty());
        let one = toolkit_scope(vec!["a".to_owned(), "b".to_owned()], Some("g1"));
        assert_eq!(one.active_generations.len(), 2);
        assert!(one.generation.is_empty(), "never the strict generation");
        let mixed = toolkit_scope_with_generations(&[
            ("a".to_owned(), Some("g1".to_owned())),
            ("b".to_owned(), None),
        ]);
        assert_eq!(mixed.namespace_ids, ["a", "b"]);
        assert_eq!(
            mixed.active_generations,
            vec![pb::ActiveGeneration {
                namespace_id: "a".to_owned(),
                generation: "g1".to_owned(),
            }]
        );
    }

    #[test]
    fn the_config_debug_hides_the_private_key() {
        let mut config = ClientConfig::new("https://elitea-vector:9443");
        config.identity_pem = Some((b"CERT".to_vec(), b"PRIVATE-KEY".to_vec()));
        let printed = format!("{config:?}");
        assert!(!printed.contains("PRIVATE-KEY"), "{printed}");
    }
}
