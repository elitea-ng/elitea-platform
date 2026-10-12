//! `index.ingest.v1` in the Rust worker (ADR-0030 I1).
//!
//! One run of a toolkit index: the claim's input entries name the toolkit
//! (a `ContentSource` connector, `elitea-connectors`), the `index_data`
//! arguments and the embedding model; the run lists the source, compares it
//! with what `elitea-vector` already holds for the index's namespace
//! ([`VectorClient::list_documents`]), fetches, extracts (`elitea-doc-extract`)
//! and chunks (`elitea-doc-chunk`) the documents that are new or changed,
//! embeds the chunks through the platform's `/llm` gateway
//! ([`EmbeddingsClient`]) and writes the points; documents that disappeared
//! from the source are deleted. It reports a typed [`IndexIngestSummaryV1`]:
//! counts, skips by reason, the embedding stamp, the store's totals (present
//! whenever the store was touched, a failure included) and the per-document
//! record with the run's own statement of whether that record is the whole
//! index. elitea-main is the only writer of index metadata.
//!
//! * [`inputs`] — the three JSON entries, strictly read.
//! * [`source`] — the toolkit type to its connector, with the `index_data`
//!   whitelist/blacklist.
//! * [`pipeline`] — the run itself.
//! * [`slug`] — Main's vector-space slug, so the worker writes the
//!   collection Main's registry names.
//!
//! What a run never does: hold a Qdrant credential (only the claim's
//! `elitea-vector` token), write a vector of another embedding space than
//! the command expects, or write index metadata.

pub(crate) mod inputs;
pub(crate) mod pipeline;
pub(crate) mod slug;
pub(crate) mod source;
pub(crate) mod tools;

#[cfg(test)]
mod tests;
#[cfg(test)]
mod tools_tests;

use std::sync::Arc;
use std::time::Duration;

use elitea_connectors::egress::HostAllowlist;
use elitea_connectors::transport::Transport;
use elitea_vector_index::{ClientConfig, VectorClient};

use crate::protocol::control::IndexRemovalControl;
use crate::transport::embeddings_facade::EmbeddingsClient;
use crate::transport::extractor_client::ExtractorClient;

pub(crate) use pipeline::{IndexRun, IndexRunFailure, Interruption, interrupt_summary, run_index};

/// What every index run shares: the vector client, the gateway embeddings
/// route, the platform origin for the artifact object API, the egress
/// allowlist and the HTTP transport the connectors send through.
pub(crate) struct IndexIngestRuntime {
    pub(crate) vector: VectorClient,
    pub(crate) embeddings: EmbeddingsClient,
    /// The control plane the `remove_index` tool asks Main through
    /// (ADR-0030 I1); `None` leaves the tool refusing with a sentence.
    pub(crate) removal: Option<Arc<dyn IndexRemovalControl>>,
    /// `https://host[:port]`: where the artifact connector finds the
    /// platform's object API (the worker's `platform_origin`).
    pub(crate) platform_origin: String,
    pub(crate) allowlist: HostAllowlist,
    pub(crate) transport: Arc<dyn Transport>,
    pub(crate) limits: IndexRunLimits,
    /// The document extraction sidecar (`elitea-doc-extractor`), when the
    /// deployment names its socket; `None` reports a PDF or Office document
    /// skipped `unsupported_type`.
    pub(crate) extractor: Option<Arc<ExtractorClient>>,
}

/// Bounds of one run.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) struct IndexRunLimits {
    /// Documents fetched, extracted and chunked at once.
    pub(crate) document_concurrency: usize,
    /// Chunk texts per embeddings request (at most
    /// [`crate::transport::embeddings_facade::MAX_BATCH_INPUTS`]).
    pub(crate) embedding_batch: usize,
    /// Documents one listing may hold; a larger source fails the run.
    pub(crate) max_documents: usize,
    /// One document's bytes.
    pub(crate) max_document_bytes: u64,
    /// The extraction time asked of the document extraction sidecar for one
    /// PDF or Office document (sent in the request header; the sidecar
    /// lowers it to its own cap). The client's call deadline is derived
    /// from this and what the sidecar advertises, never fixed.
    pub(crate) extract_timeout: Duration,
    /// The wait before the second embeddings attempt of a batch after a
    /// retryable refusal; the third waits twice that.
    pub(crate) embedding_retry_backoff: Duration,
}

impl Default for IndexRunLimits {
    fn default() -> Self {
        Self {
            document_concurrency: 4,
            embedding_batch: 64,
            max_documents: 50_000,
            max_document_bytes: 50 * 1024 * 1024,
            extract_timeout: Duration::from_mins(2),
            embedding_retry_backoff: Duration::from_millis(500),
        }
    }
}

impl IndexIngestRuntime {
    /// A runtime over `vector_target` (`host:port`, mTLS with the worker's
    /// identity), the gateway `embeddings`, the connectors' hardened reqwest
    /// transport and the deployment's egress allowlist.
    ///
    /// # Errors
    ///
    /// The vector endpoint or TLS material is malformed, or the HTTP client
    /// cannot be built.
    pub(crate) fn new(
        vector_target: &str,
        ca_pem: Vec<u8>,
        identity_pem: (Vec<u8>, Vec<u8>),
        embeddings: EmbeddingsClient,
        platform_origin: String,
        allowlist: HostAllowlist,
        limits: IndexRunLimits,
    ) -> Result<Self, IndexRuntimeBuildError> {
        let mut config = ClientConfig::new(format!("https://{vector_target}"));
        config.ca_pem = Some(ca_pem);
        config.identity_pem = Some(identity_pem);
        config.server_name = vector_target
            .rsplit_once(':')
            .map(|(host, _)| host.to_owned());
        let vector = VectorClient::connect_lazy(&config).map_err(|_| IndexRuntimeBuildError)?;
        let transport = elitea_connectors::reqwest012::ReqwestTransport::hardened(
            &elitea_connectors::github::client::CLIENT_POLICY,
        )
        .map_err(|_| IndexRuntimeBuildError)?;
        Ok(Self {
            vector,
            embeddings,
            removal: None,
            platform_origin,
            allowlist,
            transport: Arc::new(transport),
            limits,
            extractor: None,
        })
    }

    /// Lend the control plane the `remove_index` tool runs Main's index
    /// delete flow through.
    #[must_use]
    pub(crate) fn with_removal_control(mut self, control: Arc<dyn IndexRemovalControl>) -> Self {
        self.removal = Some(control);
        self
    }

    /// Send PDF and Office documents to the extraction sidecar.
    #[must_use]
    pub(crate) fn with_extractor(mut self, extractor: Arc<ExtractorClient>) -> Self {
        self.extractor = Some(extractor);
        self
    }
}

/// The indexing runtime could not be composed. Carries nothing from the
/// configuration.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct IndexRuntimeBuildError;

impl std::fmt::Display for IndexRuntimeBuildError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str("the indexing runtime configuration is invalid")
    }
}

impl std::error::Error for IndexRuntimeBuildError {}
