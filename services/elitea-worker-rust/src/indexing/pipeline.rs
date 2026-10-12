//! One index run (ADR-0030 decisions 2 to 4).
//!
//! Order, and why: the inputs are read first (a malformed entry is Main's
//! fault, a terminal `INVALID_INPUT`); the embedding model is compared with
//! the command's expected space before anything is listed; the source is
//! listed and diffed against what the vector store holds; the documents to
//! write are fetched, extracted and chunked with bounded concurrency; the
//! first embeddings answer fixes the dimension and is compared with the
//! expected space BEFORE the first point is written (and before a
//! `clean_index` delete); then the points go in, changed documents' old
//! chunks are deleted first, and documents the source no longer has are
//! deleted last. Between documents the run checks the lease (Stop) and the
//! command deadline.
//!
//! What the run reports is a typed [`IndexIngestSummaryV1`]. A run that
//! cannot proceed for a reason the user can act on (an unserved toolkit, a
//! refused connector, a model that returns another dimension) is an
//! `ERROR` summary with that reason, not a runtime failure: elitea-main
//! records it on the index and the user reads it.

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;
use std::sync::Arc;
use std::time::Duration;

use elitea_content_source::{Acl, DocumentRef, SourceError};
use elitea_doc_chunk::{Chunk, ChunkError, ChunkOptions, ChunkingConfig};
use elitea_doc_extract::{ExtractOptions, Extracted};
use elitea_vector_index::client::toolkit_namespace;
use elitea_vector_index::{DeleteSelector, pb};
use prost::Message as _;
use serde_json::{Map, Value};
use tokio::task::JoinSet;

use super::IndexIngestRuntime;
use super::inputs::{self, IndexParameters};
use super::slug::vector_space_slug;
use super::source::{IndexSource, PathFilter, PlatformAccess};
use crate::execution::agent_lease::ClaimLeaseStateProbe;
use crate::execution::{ClaimLeaseError, ClaimLeaseErrorCode};
use crate::protocol::elitea::runtime::v1::{
    IndexIngestCommandV1, IndexIngestDocumentV1, IndexIngestStatusV1, IndexIngestSummaryV1,
    IndexIngestTerminalStateV1,
};
use crate::protocol::vector_token::{VectorClaimToken, VectorSource};
use crate::transport::embeddings_facade::{EmbeddingsClient, EmbeddingsError, MAX_BATCH_INPUTS};
use crate::transport::runtime_context::ClaimScopedEliteaContext;

/// The byte budget of the per-document record in the summary. The frame is
/// 64 KiB; the rest of the summary, the bindings and the frame envelope
/// take a few KiB.
const DOCUMENT_RECORD_BYTES: usize = 40 * 1024;
/// Document keys deleted per `Delete` call (the facade bounds a key list).
const DELETE_BATCH: usize = 256;
/// Embedding attempts per batch when the gateway's refusal is retryable.
const EMBEDDING_ATTEMPTS: u32 = 3;
/// Closed skip reasons (`IndexIngestSummaryV1.skipped`).
const SKIP_UNSUPPORTED: &str = "unsupported_type";
const SKIP_UNREADABLE: &str = "unreadable";
const SKIP_NEEDS_MODEL: &str = "needs_model";
const SKIP_EMPTY: &str = "empty";
const SKIP_FETCH_FAILED: &str = "fetch_failed";
const SKIP_EMBEDDING_FAILED: &str = "embedding_failed";
const SKIP_TOO_LARGE: &str = "too_large";

/// What one run is given.
pub(crate) struct IndexRun<'a> {
    pub(crate) command: &'a IndexIngestCommandV1,
    pub(crate) toolkit_configuration: &'a [u8],
    pub(crate) tool_parameters: &'a [u8],
    pub(crate) embedding_binding: Option<&'a [u8]>,
    pub(crate) vector_token: Option<&'a VectorClaimToken>,
    /// The redeemed claim: the PAT the gateway and the artifact API take,
    /// the project and the execution.
    pub(crate) context: &'a ClaimScopedEliteaContext,
    pub(crate) lease: &'a dyn RunLiveness,
    pub(crate) deadline_unix_millis: i64,
    pub(crate) now_unix_millis: &'a (dyn Fn() -> i64 + Send + Sync),
}

/// What the run asks between documents: is the claim still ours and not
/// stopped? The lease monitor's state probe answers in production.
pub(crate) trait RunLiveness: Send + Sync {
    /// Ok while the lease is held and no Stop was observed.
    ///
    /// # Errors
    ///
    /// The lease cause (Stop, loss, a closed monitor).
    fn ensure_running(&self) -> Result<(), ClaimLeaseError>;
}

impl RunLiveness for ClaimLeaseStateProbe {
    fn ensure_running(&self) -> Result<(), ClaimLeaseError> {
        Self::ensure_running(self)
    }
}

/// Why a run ended without a summary: the terminal is a runtime failure.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum IndexRunFailure {
    /// An input entry Main wrote is malformed.
    InvalidInput(String),
    Cancelled,
    DeadlineExceeded,
    /// The lease was lost or the monitor closed.
    LeaseLost,
}

/// Why a run stopped early: with a summary the user reads, or as a runtime
/// failure.
enum Halt {
    Summary(Box<IndexIngestSummaryV1>),
    Failure(IndexRunFailure),
}

impl From<IndexRunFailure> for Halt {
    fn from(failure: IndexRunFailure) -> Self {
        Self::Failure(failure)
    }
}

/// What `plan` decided: the namespace, the documents to write and those to
/// delete.
struct RunPlan {
    namespace: pb::Namespace,
    to_write: Vec<(DocumentRef, Plan)>,
    vanished: Vec<String>,
    reindex: bool,
}

/// The run's working counts.
#[derive(Default)]
struct Tally {
    indexed_documents: u64,
    updated_documents: u64,
    indexed_chunks: u64,
    failed_chunks: u64,
    skipped: BTreeMap<&'static str, u64>,
    unchanged_documents: u64,
    unchanged_chunks: u64,
    removed_documents: u64,
    /// The documents written: key, version, chunk count.
    written: Vec<IndexIngestDocumentV1>,
    removed_keys: Vec<String>,
}

impl Tally {
    fn skip(&mut self, reason: &'static str) {
        *self.skipped.entry(reason).or_default() += 1;
    }
}

/// A document the run decided about.
#[derive(Clone, Copy)]
enum Plan {
    New,
    Changed,
}

/// A document fetched, extracted and chunked, ready to embed.
struct Prepared {
    reference: DocumentRef,
    plan: Plan,
    /// The source's metadata (`sha`, `branch`, `url`, …) with the SDK's
    /// `source` key, shared by every chunk.
    metadata: Map<String, Value>,
    acl: Vec<String>,
    chunks: Vec<Chunk>,
}

enum Preparation {
    Ready(Box<Prepared>),
    Skipped(&'static str),
    /// A chunking configuration the run cannot honour (the whole run fails).
    Configuration(String),
}

/// Runs one index and reports its summary.
///
/// # Errors
///
/// A malformed input entry, Stop, the deadline, or a lost lease; every other
/// failure is an `ERROR` summary.
pub(crate) async fn run_index(
    runtime: &IndexIngestRuntime,
    run: IndexRun<'_>,
) -> Result<IndexIngestSummaryV1, IndexRunFailure> {
    let configuration = inputs::toolkit_configuration(run.toolkit_configuration)
        .map_err(|error| IndexRunFailure::InvalidInput(error.0))?;
    let parameters = inputs::index_parameters(run.tool_parameters)
        .map_err(|error| IndexRunFailure::InvalidInput(error.0))?;
    if run.command.index_id.is_empty() {
        return Err(IndexRunFailure::InvalidInput(
            "the index command names no registry index".to_owned(),
        ));
    }
    let model = match run.embedding_binding {
        Some(raw) => {
            inputs::embedding_model(raw).map_err(|error| IndexRunFailure::InvalidInput(error.0))?
        }
        None => {
            return Ok(failure(
                &parameters,
                "no embedding model is configured for this toolkit; set one in the toolkit's settings",
            ));
        }
    };
    let expected = run.command.expected_embedding_model.as_str();
    if !expected.is_empty() && expected != model {
        return Ok(failure(
            &parameters,
            &format!(
                "embedding model changed from {expected} to {model}: an index keeps one embedding space, re-index into a new index to change the model"
            ),
        ));
    }
    let Some(token) = run
        .vector_token
        .filter(|token| token.allows(VectorSource::ToolkitIndex))
    else {
        return Ok(failure(
            &parameters,
            "the vector store is not available to this run: the claim carries no project token for toolkit indexes",
        ));
    };
    let mut chunking = match ChunkingConfig::from_value(&parameters.chunking_config) {
        Ok(chunking) => chunking,
        Err(error) => return Ok(failure(&parameters, &error.to_string())),
    };
    if let Some(tool) = &parameters.chunking_tool {
        chunking.base.chunker = Some(tool.clone());
    }
    for warning in &chunking.warnings {
        tracing::warn!(event = "index_chunking_config_setting_dropped", warning);
    }
    let bearer = run.context.model_facade_token();
    let project_id = run.context.resource_project_id().to_string();
    let source = match IndexSource::build(
        &configuration,
        &parameters,
        &PlatformAccess {
            origin: &runtime.platform_origin,
            bearer: &bearer,
            project_id: &project_id,
        },
        &runtime.allowlist,
        Arc::clone(&runtime.transport),
        runtime.limits,
    ) {
        Ok(source) => Arc::new(source),
        Err(error) => return Ok(failure(&parameters, &error.0)),
    };
    drop(bearer);
    Runner {
        runtime,
        run: &run,
        parameters: &parameters,
        toolkit_name: &configuration.toolkit_name,
        model: &model,
        token,
        source,
        chunking: Arc::new(chunking),
        tally: Tally::default(),
    }
    .run()
    .await
}

struct Runner<'a> {
    runtime: &'a IndexIngestRuntime,
    run: &'a IndexRun<'a>,
    parameters: &'a IndexParameters,
    toolkit_name: &'a str,
    model: &'a str,
    token: &'a VectorClaimToken,
    source: Arc<IndexSource>,
    chunking: Arc<ChunkingConfig>,
    tally: Tally,
}

impl Runner<'_> {
    async fn run(mut self) -> Result<IndexIngestSummaryV1, IndexRunFailure> {
        let plan = match self.plan().await {
            Ok(plan) => plan,
            Err(Halt::Summary(summary)) => return Ok(*summary),
            Err(Halt::Failure(failure)) => return Err(failure),
        };
        match self.write(&plan).await {
            Ok(space) => Ok(self.summary(space.as_ref(), plan.reindex)),
            Err(Halt::Summary(summary)) => Ok(*summary),
            Err(Halt::Failure(failure)) => Err(failure),
        }
    }

    fn halt(&self, reason: &str) -> Halt {
        Halt::Summary(Box::new(failure(self.parameters, reason)))
    }

    /// Lists the source, reads what the store holds and diffs the two
    /// (ADR-0030 decision 3).
    async fn plan(&mut self) -> Result<RunPlan, Halt> {
        let parameters = self.parameters;
        let listing = self
            .source
            .list()
            .await
            .map_err(|error| self.halt(&source_text(&error)))?;
        self.check_liveness()?;
        let filter = PathFilter::new(&parameters.whitelist, &parameters.blacklist);
        let listing: Vec<DocumentRef> = listing
            .into_iter()
            .filter(|reference| filter.keeps(&reference.key))
            .collect();
        if listing.len() > self.runtime.limits.max_documents {
            return Err(self.halt(&format!(
                "the source holds {} documents after filtering; one run indexes at most {}",
                listing.len(),
                self.runtime.limits.max_documents
            )));
        }
        let namespace = toolkit_namespace(&self.run.command.index_id);
        let mut prior: BTreeMap<String, Vec<(String, u64)>> = BTreeMap::new();
        if !parameters.clean_index {
            let documents = self
                .runtime
                .vector
                .list_documents(self.token, namespace.clone(), None)
                .await
                .map_err(|error| self.halt(&vector_text(&error)))?;
            for document in documents {
                prior
                    .entry(document.document_key)
                    .or_default()
                    .push((document.document_version, document.chunk_count));
            }
        }
        self.check_liveness()?;
        let mut to_write: Vec<(DocumentRef, Plan)> = Vec::new();
        let listed: BTreeSet<&str> = listing.iter().map(|r| r.key.as_str()).collect();
        for reference in &listing {
            match prior.get(&reference.key) {
                None => to_write.push((reference.clone(), Plan::New)),
                Some(versions) if versions.len() == 1 && versions[0].0 == reference.version => {
                    self.tally.unchanged_documents += 1;
                    self.tally.unchanged_chunks += versions[0].1;
                }
                Some(_) => to_write.push((reference.clone(), Plan::Changed)),
            }
        }
        let vanished: Vec<String> = prior
            .keys()
            .filter(|key| !listed.contains(key.as_str()))
            .cloned()
            .collect();
        Ok(RunPlan {
            namespace,
            to_write,
            vanished,
            reindex: !prior.is_empty(),
        })
    }

    /// Fetches, extracts and chunks with bounded concurrency; embeds and
    /// writes as documents complete; deletes the vanished ones last. Returns
    /// the embedding space the run wrote into (`None` when nothing was
    /// embedded).
    #[allow(clippy::too_many_lines)] // One linear pass: prepare, embed, fence, write.
    async fn write(&mut self, plan: &RunPlan) -> Result<Option<pb::EmbeddingSpace>, Halt> {
        let mut space: Option<pb::EmbeddingSpace> = expected_space(self.run.command);
        let mut cleaned = !self.parameters.clean_index;
        let mut pending = plan.to_write.iter().cloned();
        let mut tasks: JoinSet<Preparation> = JoinSet::new();
        let outcome: Result<(), Halt> = async {
            loop {
                while tasks.len() < self.runtime.limits.document_concurrency {
                    let Some((reference, plan)) = pending.next() else {
                        break;
                    };
                    let source = Arc::clone(&self.source);
                    let chunking = Arc::clone(&self.chunking);
                    let max_bytes = self.runtime.limits.max_document_bytes;
                    tasks.spawn(prepare(source, chunking, reference, plan, max_bytes));
                }
                let Some(joined) = tasks.join_next().await else {
                    break Ok(());
                };
                self.check_liveness()?;
                let prepared = match joined {
                    Ok(Preparation::Ready(prepared)) => prepared,
                    Ok(Preparation::Skipped(reason)) => {
                        self.tally.skip(reason);
                        continue;
                    }
                    Ok(Preparation::Configuration(reason)) => break Err(self.halt(&reason)),
                    Err(_) => {
                        break Err(self.halt(
                            "a document could not be prepared: the preparation task stopped unexpectedly",
                        ));
                    }
                };
                let vectors = match self.embed_all(&prepared.chunks).await {
                    Ok(vectors) => vectors,
                    Err(EmbedOutcome::Mismatch(message)) => break Err(self.halt(&message)),
                    Err(EmbedOutcome::Failed) => {
                        self.tally.failed_chunks += prepared.chunks.len() as u64;
                        self.tally.skip(SKIP_EMBEDDING_FAILED);
                        continue;
                    }
                };
                let dimension = vectors.first().map_or(0, Vec::len);
                if let Some(expected) = expected_dimension(self.run.command)
                    && expected != dimension
                {
                    break Err(self.halt(&format!(
                        "embedding dimension changed from {expected} ({}) to {dimension} ({}): an index keeps one embedding space, re-index into a new index to change the model",
                        self.run.command.expected_embedding_model, self.model
                    )));
                }
                let written_into = space.get_or_insert_with(|| pb::EmbeddingSpace {
                    model_slug: vector_space_slug(self.model),
                    dimension: u32::try_from(dimension).unwrap_or(u32::MAX),
                });
                // Nothing was written before this point. A clean_index drops
                // the namespace once, now that the embedding space is proven.
                if !cleaned {
                    self.delete(&plan.namespace, DeleteSelector::WholeNamespace)
                        .await?;
                    cleaned = true;
                }
                if matches!(prepared.plan, Plan::Changed) {
                    self.delete(
                        &plan.namespace,
                        DeleteSelector::Documents(vec![prepared.reference.key.clone()]),
                    )
                    .await?;
                }
                let points = points_of(&prepared, vectors);
                let count = points.len() as u64;
                self.runtime
                    .vector
                    .upsert(
                        self.token,
                        written_into.clone(),
                        plan.namespace.clone(),
                        points,
                    )
                    .await
                    .map_err(|error| self.halt(&vector_text(&error)))?;
                self.tally.indexed_documents += 1;
                if matches!(prepared.plan, Plan::Changed) {
                    self.tally.updated_documents += 1;
                }
                self.tally.indexed_chunks += count;
                self.tally.written.push(IndexIngestDocumentV1 {
                    document_key: prepared.reference.key.clone(),
                    version: prepared.reference.version.clone(),
                    chunk_count: u32::try_from(count).unwrap_or(u32::MAX),
                });
            }
        }
        .await;
        if outcome.is_err() {
            tasks.abort_all();
        }
        outcome?;
        if !cleaned && self.parameters.clean_index {
            // A clean run that had nothing to embed still starts empty.
            self.delete(&plan.namespace, DeleteSelector::WholeNamespace)
                .await?;
        }
        for batch in plan.vanished.chunks(DELETE_BATCH) {
            self.check_liveness()?;
            self.delete(&plan.namespace, DeleteSelector::Documents(batch.to_vec()))
                .await?;
            self.tally.removed_documents += batch.len() as u64;
            self.tally.removed_keys.extend(batch.iter().cloned());
        }
        Ok(space)
    }

    async fn delete(
        &self,
        namespace: &pb::Namespace,
        selector: DeleteSelector,
    ) -> Result<(), Halt> {
        self.runtime
            .vector
            .delete(self.token, None, namespace.clone(), selector)
            .await
            .map(|_| ())
            .map_err(|error| self.halt(&vector_text(&error)))
    }

    /// Stop and the deadline, checked between documents.
    fn check_liveness(&self) -> Result<(), IndexRunFailure> {
        if let Err(error) = self.run.lease.ensure_running() {
            return Err(match error.code() {
                ClaimLeaseErrorCode::Cancelled => IndexRunFailure::Cancelled,
                _ => IndexRunFailure::LeaseLost,
            });
        }
        let now = (self.run.now_unix_millis)();
        if now <= 0 || now >= self.run.deadline_unix_millis {
            return Err(IndexRunFailure::DeadlineExceeded);
        }
        Ok(())
    }

    /// Every chunk of one document, in batches, with bounded retries for a
    /// retryable gateway refusal.
    async fn embed_all(&self, chunks: &[Chunk]) -> Result<Vec<Vec<f32>>, EmbedOutcome> {
        let batch_size = self
            .runtime
            .limits
            .embedding_batch
            .clamp(1, MAX_BATCH_INPUTS);
        let mut vectors = Vec::with_capacity(chunks.len());
        for batch in chunks.chunks(batch_size) {
            let inputs: Vec<String> = batch.iter().map(|chunk| chunk.text.clone()).collect();
            let embedded = embed_with_retry(
                &self.runtime.embeddings,
                self.run.context,
                self.model,
                &inputs,
            )
            .await
            .map_err(|error| {
                tracing::warn!(
                    event = "index_embedding_failed",
                    error = %error,
                    retryable = error.retryable(),
                );
                EmbedOutcome::Failed
            })?;
            if let Some(first) = vectors.first().map(Vec::len)
                && embedded.vectors.iter().any(|vector| vector.len() != first)
            {
                return Err(EmbedOutcome::Mismatch(
                    "the embedding model returned vectors of two dimensions within one run"
                        .to_owned(),
                ));
            }
            vectors.extend(embedded.vectors);
        }
        Ok(vectors)
    }

    fn summary(self, space: Option<&pb::EmbeddingSpace>, reindex: bool) -> IndexIngestSummaryV1 {
        let tally = self.tally;
        let total_documents = tally.unchanged_documents + tally.indexed_documents;
        let total_chunks = tally.unchanged_chunks + tally.indexed_chunks;
        let partial = tally.failed_chunks > 0 || !tally.skipped.is_empty();
        let (status, state) = if partial {
            (
                IndexIngestStatusV1::PartlyIndexed,
                IndexIngestTerminalStateV1::PartlyIndexed,
            )
        } else {
            (
                IndexIngestStatusV1::Ok,
                IndexIngestTerminalStateV1::Completed,
            )
        };
        let mut message = format!(
            "Index '{}' of toolkit '{}': {} documents indexed ({} chunks), {} unchanged, {} removed.",
            self.parameters.index_name,
            self.toolkit_name,
            tally.indexed_documents,
            tally.indexed_chunks,
            tally.unchanged_documents,
            tally.removed_documents,
        );
        if !tally.skipped.is_empty() {
            let reasons: Vec<String> = tally
                .skipped
                .iter()
                .map(|(reason, count)| format!("{count} {reason}"))
                .collect();
            let _ = write!(message, " Skipped: {}.", reasons.join(", "));
        }
        if tally.failed_chunks > 0 {
            let _ = write!(message, " {} chunks failed to embed.", tally.failed_chunks);
        }
        let (documents, removed_document_keys, documents_truncated) =
            bounded_record(tally.written, tally.removed_keys);
        // The stamp is reported only when vectors were written: an empty
        // run has nothing to stamp (Main stamps on the first chunk).
        let (embedding_model, embedding_dimension) = match (space, tally.indexed_chunks) {
            (Some(space), chunks) if chunks > 0 => (self.model.to_owned(), space.dimension),
            _ => (String::new(), 0),
        };
        IndexIngestSummaryV1 {
            status: status as i32,
            message,
            terminal_state: state as i32,
            indexed: tally.indexed_documents,
            updated: tally.updated_documents,
            reindex: Some(reindex),
            indexed_documents: tally.indexed_documents,
            indexed_chunks: tally.indexed_chunks,
            failed_chunks: tally.failed_chunks,
            skipped: tally
                .skipped
                .into_iter()
                .map(|(reason, count)| (reason.to_owned(), count))
                .collect(),
            embedding_model,
            embedding_dimension,
            documents,
            removed_document_keys,
            documents_truncated,
            total_documents,
            total_chunks,
        }
    }
}

enum EmbedOutcome {
    /// The gateway failed for this document; the run goes on.
    Failed,
    /// The vectors disagree with the index's space; the run stops.
    Mismatch(String),
}

async fn embed_with_retry(
    embeddings: &EmbeddingsClient,
    context: &ClaimScopedEliteaContext,
    model: &str,
    inputs: &[String],
) -> Result<crate::transport::embeddings_facade::Embedded, EmbeddingsError> {
    let mut attempt = 0;
    loop {
        match embeddings.embed(context, model, inputs).await {
            Ok(embedded) => return Ok(embedded),
            Err(error) if error.retryable() && attempt + 1 < EMBEDDING_ATTEMPTS => {
                attempt += 1;
                tokio::time::sleep(Duration::from_millis(500 * u64::from(attempt))).await;
            }
            Err(error) => return Err(error),
        }
    }
}

/// The expected space from the command, when the index is stamped.
fn expected_space(command: &IndexIngestCommandV1) -> Option<pb::EmbeddingSpace> {
    (!command.expected_embedding_model.is_empty()).then(|| pb::EmbeddingSpace {
        model_slug: vector_space_slug(&command.expected_embedding_model),
        dimension: command.expected_embedding_dimension,
    })
}

fn expected_dimension(command: &IndexIngestCommandV1) -> Option<usize> {
    (command.expected_embedding_dimension != 0)
        .then_some(command.expected_embedding_dimension as usize)
}

/// Fetch, extract and chunk one document. Runs as its own task; the CPU
/// work (extraction, tokenising) is on a blocking thread.
async fn prepare(
    source: Arc<IndexSource>,
    chunking: Arc<ChunkingConfig>,
    reference: DocumentRef,
    plan: Plan,
    max_bytes: u64,
) -> Preparation {
    if reference.size > max_bytes {
        return Preparation::Skipped(SKIP_TOO_LARGE);
    }
    let document = match source.fetch(&reference.key).await {
        Ok(document) => document,
        Err(SourceError::NotFound(_)) => {
            // Listed, then gone: the next run deletes it.
            return Preparation::Skipped(SKIP_FETCH_FAILED);
        }
        Err(error) => {
            tracing::warn!(event = "index_document_fetch_failed", error = %error);
            return Preparation::Skipped(SKIP_FETCH_FAILED);
        }
    };
    if document.bytes.len() as u64 > max_bytes {
        return Preparation::Skipped(SKIP_TOO_LARGE);
    }
    let key = reference.key.clone();
    let mime = reference.mime.clone();
    let mut metadata = document.metadata;
    metadata.insert("source".to_owned(), Value::String(key.clone()));
    if !document.title.is_empty() {
        metadata
            .entry("title".to_owned())
            .or_insert_with(|| Value::String(document.title.clone()));
    }
    if let Some(uri) = &document.uri {
        metadata
            .entry("url".to_owned())
            .or_insert_with(|| Value::String(uri.clone()));
    }
    let acl = acl_entries(&reference.acl);
    // The version the listing gave is what the points carry; a fetch that
    // came back with another version is the connector's `Changed`.
    let reference = DocumentRef {
        version: document.reference.version.clone(),
        ..reference
    };
    let bytes = document.bytes;
    let chunked = tokio::task::spawn_blocking(move || {
        let is_html = matches!(mime.as_str(), "text/html" | "application/xhtml+xml");
        let options = ExtractOptions {
            html_to_markdown: true,
        };
        let text = match elitea_doc_extract::extract_with(&mime, &bytes, &options) {
            Extracted::Text { text, .. } => text,
            Extracted::Unsupported => return Preparation::Skipped(SKIP_UNSUPPORTED),
            Extracted::Unreadable(_) => return Preparation::Skipped(SKIP_UNREADABLE),
        };
        if text.trim().is_empty() {
            return Preparation::Skipped(SKIP_EMPTY);
        }
        let options = ChunkOptions {
            html_converted: is_html,
        };
        match elitea_doc_chunk::chunk_with(&text, &key, &chunking, options) {
            Ok(chunks) if chunks.is_empty() => Preparation::Skipped(SKIP_EMPTY),
            Ok(chunks) => Preparation::Ready(Box::new(Prepared {
                reference,
                plan,
                metadata,
                acl,
                chunks,
            })),
            Err(ChunkError::RequiresModel { .. }) => Preparation::Skipped(SKIP_NEEDS_MODEL),
            Err(error @ (ChunkError::UnknownChunker(_) | ChunkError::Tokenizer(_))) => {
                Preparation::Configuration(format!(
                    "the chunking configuration cannot be used: {error}"
                ))
            }
        }
    })
    .await;
    chunked.unwrap_or(Preparation::Configuration(
        "the document preparation task stopped unexpectedly".to_owned(),
    ))
}

/// ADR-0028 ACL entries as the point payload carries them: nothing for a
/// project-readable document, `kind:id` per principal otherwise.
fn acl_entries(acl: &Acl) -> Vec<String> {
    match acl {
        Acl::Project => Vec::new(),
        Acl::Restricted { principals } => principals
            .iter()
            .map(|principal| {
                let kind = match principal.kind {
                    elitea_content_source::PrincipalKind::User => "user",
                    elitea_content_source::PrincipalKind::Group => "group",
                    elitea_content_source::PrincipalKind::Email => "email",
                };
                format!("{kind}:{}", principal.id)
            })
            .collect(),
    }
}

/// The points of one prepared document: the chunk's metadata over the
/// document's, the chunk id as text, the chunk type, the text and the ACL.
fn points_of(prepared: &Prepared, vectors: Vec<Vec<f32>>) -> Vec<pb::Point> {
    prepared
        .chunks
        .iter()
        .zip(vectors)
        .map(|(chunk, vector)| {
            let mut metadata = prepared.metadata.clone();
            for (key, value) in &chunk.metadata {
                metadata.insert(key.clone(), value.clone());
            }
            pb::Point {
                document_key: prepared.reference.key.clone(),
                document_version: prepared.reference.version.clone(),
                parent_id: String::new(),
                chunk_id: chunk.chunk_id.to_string(),
                chunk_type: chunk.chunk_type.to_owned(),
                acl: prepared.acl.clone(),
                text: chunk.text.clone(),
                metadata_json: Value::Object(metadata).to_string(),
                vector,
            }
        })
        .collect()
}

/// The per-document record, bounded to the frame's budget: written
/// documents first, then removed keys; `truncated` when some did not fit.
fn bounded_record(
    written: Vec<IndexIngestDocumentV1>,
    removed: Vec<String>,
) -> (Vec<IndexIngestDocumentV1>, Vec<String>, bool) {
    let mut budget = DOCUMENT_RECORD_BYTES;
    let mut documents = Vec::new();
    let mut removed_keys = Vec::new();
    let mut truncated = false;
    for document in written {
        let cost = document.encoded_len() + 3;
        if cost > budget {
            truncated = true;
            break;
        }
        budget -= cost;
        documents.push(document);
    }
    for key in removed {
        let cost = key.len() + 3;
        if truncated || cost > budget {
            truncated = true;
            break;
        }
        budget -= cost;
        removed_keys.push(key);
    }
    (documents, removed_keys, truncated)
}

/// An `ERROR` summary with `reason`, before or instead of any write.
fn failure(parameters: &IndexParameters, reason: &str) -> IndexIngestSummaryV1 {
    const MAX_MESSAGE: usize = 8 * 1024;
    let mut message = format!("Index '{}' failed: {reason}", parameters.index_name);
    if message.len() > MAX_MESSAGE {
        let mut cut = MAX_MESSAGE;
        while !message.is_char_boundary(cut) {
            cut -= 1;
        }
        message.truncate(cut);
    }
    IndexIngestSummaryV1 {
        status: IndexIngestStatusV1::Error as i32,
        message,
        terminal_state: IndexIngestTerminalStateV1::Failed as i32,
        ..IndexIngestSummaryV1::default()
    }
}

fn source_text(error: &SourceError) -> String {
    format!("the source could not be read: {error}")
}

fn vector_text(error: &elitea_vector_index::Error) -> String {
    format!("the vector store refused the run: {error}")
}

#[cfg(test)]
pub(super) fn record_for_test(
    written: Vec<IndexIngestDocumentV1>,
    removed: Vec<String>,
) -> (Vec<IndexIngestDocumentV1>, Vec<String>, bool) {
    bounded_record(written, removed)
}

#[cfg(test)]
pub(super) fn failure_for_test(index_name: &str, reason: &str) -> IndexIngestSummaryV1 {
    failure(
        &IndexParameters {
            index_name: index_name.to_owned(),
            clean_index: false,
            chunking_tool: None,
            chunking_config: Value::Null,
            branch: None,
            whitelist: Vec::new(),
            blacklist: Vec::new(),
        },
        reason,
    )
}
