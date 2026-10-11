//! A typed client for `elitea.vector.v1` (ADR-0031 decision 1).
//!
//! The client speaks gRPC over mTLS to `elitea-vector`. Every call carries
//! `authorization: Bearer <token>` metadata, taken from the claim's
//! [`ClaimToken`]; the facade verifies it, injects the token's project into
//! the request and refuses anything else. The client never sets a
//! `project_id` itself (zero means "the token's project").

use std::fmt;
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
    /// `https://host:port` (or `http://` for a plaintext test server).
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
            inner: VectorServiceClient::new(channel),
        }
    }

    /// A client that connects on first use and reconnects when the
    /// connection drops, so a worker can build it at startup before
    /// `elitea-vector` is up.
    ///
    /// # Errors
    ///
    /// [`Error::Transport`] when the endpoint or the certificates are
    /// malformed.
    pub fn connect_lazy(config: &ClientConfig) -> Result<Self> {
        let mut endpoint = Endpoint::from_shared(config.endpoint.clone())
            .map_err(|error| Error::Transport(format!("invalid endpoint: {error}")))?
            .connect_timeout(config.connect_timeout)
            .timeout(config.request_timeout)
            .tcp_nodelay(true);
        if config.endpoint.starts_with("https://") {
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

    /// Dense cosine search.
    ///
    /// # Errors
    ///
    /// [`Error::InvalidToken`] or the facade's refusal.
    pub async fn search(
        &self,
        token: &dyn ClaimToken,
        params: SearchParams,
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
    /// reciprocal rank fusion. The returned scores are the fused scores.
    ///
    /// # Errors
    ///
    /// [`Error::InvalidToken`] or the facade's refusal.
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
        Ok(self
            .inner
            .clone()
            .hybrid_search(request)
            .await?
            .into_inner()
            .points)
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
#[must_use]
pub fn toolkit_scope(namespace_ids: Vec<String>) -> pb::Scope {
    pb::Scope {
        project_id: 0,
        source: pb::Source::ToolkitIndex as i32,
        namespace_ids,
        generation: String::new(),
    }
}

/// The toolkit-index namespace `namespace_id`, without a generation.
#[must_use]
pub fn toolkit_namespace(namespace_id: &str) -> pb::Namespace {
    pb::Namespace {
        source: pb::Source::ToolkitIndex as i32,
        namespace_id: namespace_id.to_owned(),
        generation: String::new(),
    }
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

    #[test]
    fn the_config_debug_hides_the_private_key() {
        let mut config = ClientConfig::new("https://elitea-vector:9443");
        config.identity_pem = Some((b"CERT".to_vec(), b"PRIVATE-KEY".to_vec()));
        let printed = format!("{config:?}");
        assert!(!printed.contains("PRIVATE-KEY"), "{printed}");
    }
}
