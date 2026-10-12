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
//! command deadline: a Stop or a passed deadline ends the run at that
//! boundary, never inside an upsert or a delete. Nothing outside the run
//! drops it (the processor awaits it directly, not under the lease's
//! pre-invocation race), so every store call it starts completes.
//!
//! What the run reports is a typed [`IndexIngestSummaryV1`]. A run that
//! cannot proceed for a reason the user can act on (an unserved toolkit, a
//! refused connector, a model that returns another dimension, a gateway that
//! refuses the credential or the budget) is an `ERROR` summary with that
//! reason, not a runtime failure: elitea-main records it on the index and the
//! user reads it.
//!
//! The summary describes the STORE, not the run's wishes. Its totals are the
//! documents and chunks the namespace holds after the run: unchanged
//! documents, documents this run wrote, changed documents whose old points
//! stayed because they could not be re-indexed, and (when the run failed
//! before deleting them) vanished documents still there. They are present
//! whenever the run touched the store; a run that failed before any write or
//! delete carries none, so the registry keeps what it had. A run that failed
//! after touching the store carries them too, with the documents it wrote
//! and the keys whose points it deleted, so elitea-main's registry follows
//! the store. `whole_index_record` is the run's statement that the record
//! names every document the store holds (nothing truncated, no unchanged or
//! retained document outside it); it is never left for Main to infer.
//!
//! A run stopped (or out of time) AFTER it touched the store ends the same
//! way: an `ERROR` summary with the store's totals and record, its terminal
//! state `CANCELLED` for a Stop and `FAILED` for the deadline, so elitea-main
//! records the store and settles the execution cancelled or failed. Stopped
//! before the first write or delete it is a plain runtime failure, as is a
//! lost lease at any point: another worker may own the claim, and this one
//! publishes nothing.

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;
use std::ops::ControlFlow;
use std::sync::Arc;

use elitea_content_source::{Acl, DocumentRef, SourceError};
use elitea_doc_chunk::{Chunk, ChunkError, ChunkOptions, ChunkingConfig};
use elitea_doc_extract::{ExtractOptions, Extracted};
use elitea_vector_index::client::toolkit_namespace;
use elitea_vector_index::{DeleteSelector, pb};
use prost::Message as _;
use serde_json::{Map, Value};
use tokio::task::JoinSet;

use super::inputs::{self, IndexParameters};
use super::slug::vector_space_slug;
use super::source::{IndexSource, PathFilter, PlatformAccess};
use super::{IndexIngestRuntime, IndexRunLimits};
use crate::execution::agent_lease::ClaimLeaseStateProbe;
use crate::execution::{ClaimLeaseError, ClaimLeaseErrorCode};
use crate::protocol::elitea::runtime::v1::{
    IndexIngestCommandV1, IndexIngestDocumentV1, IndexIngestStatusV1, IndexIngestSummaryV1,
    IndexIngestTerminalStateV1,
};
use crate::protocol::output::MAX_INDEX_RESULT_DOCUMENTS;
use crate::protocol::vector_token::{VectorClaimToken, VectorSource};
use crate::transport::embeddings_facade::{EmbeddingsClient, EmbeddingsError, MAX_BATCH_INPUTS};
use crate::transport::extractor_client::{Extraction, ExtractorClient, SkipReason};
use crate::transport::runtime_context::ClaimScopedEliteaContext;

/// The byte budget of the per-document record in the summary. The frame is
/// 64 KiB; the rest of the summary, the bindings and the frame envelope
/// take a few KiB.
const DOCUMENT_RECORD_BYTES: usize = 40 * 1024;
/// Document keys deleted per `Delete` call (the facade bounds a key list).
const DELETE_BATCH: usize = 256;
/// Embedding attempts per batch when the gateway's refusal is retryable.
const EMBEDDING_ATTEMPTS: u32 = 3;
/// Documents in a row whose embedding failed for a retryable reason before
/// the run stops: a gateway outage must not end as every document skipped.
pub(crate) const MAX_CONSECUTIVE_EMBEDDING_FAILURES: u32 = 5;
/// Closed skip reasons (`IndexIngestSummaryV1.skipped`).
const SKIP_UNSUPPORTED: &str = "unsupported_type";
const SKIP_UNREADABLE: &str = "unreadable";
const SKIP_NEEDS_MODEL: &str = "needs_model";
const SKIP_EMPTY: &str = "empty";
const SKIP_FETCH_FAILED: &str = "fetch_failed";
const SKIP_EMBEDDING_FAILED: &str = "embedding_failed";
const SKIP_TOO_LARGE: &str = "too_large";
/// The extraction sidecar did not answer about the document (socket, stream
/// or sidecar failure); the document is retried by the next run.
const SKIP_EXTRACT_FAILED: &str = "extract_failed";
/// The sidecar's typed reasons that have no older label.
const SKIP_ENCRYPTED: &str = "encrypted";
const SKIP_TOO_MANY_PAGES: &str = "too_many_pages";
const SKIP_EXTRACT_TIMEOUT: &str = "extract_timeout";

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
/// `Cancelled` and `DeadlineExceeded` are returned only while the store is
/// untouched; after the first write or delete they become an interrupted
/// summary instead (see [`interrupt_summary`]).
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum IndexRunFailure {
    /// An input entry Main wrote is malformed.
    InvalidInput(String),
    Cancelled,
    DeadlineExceeded,
    /// The lease was lost or the monitor closed.
    LeaseLost,
}

/// What ended a run early at a document boundary, after it touched the
/// store: the summary it reports is still the store's record.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Interruption {
    /// The server requested Stop: the summary's terminal state is
    /// `CANCELLED` and the execution settles cancelled.
    Stopped,
    /// The command deadline passed: `FAILED`, settled failed.
    DeadlineExceeded,
}

/// Why a run stopped early: for a reason the user reads (an `ERROR`
/// summary, with the store's record when the store was touched), or as a
/// runtime failure.
enum Halt {
    Reason(String),
    Failure(IndexRunFailure),
}

impl From<IndexRunFailure> for Halt {
    fn from(failure: IndexRunFailure) -> Self {
        Self::Failure(failure)
    }
}

/// What `plan` decided: the namespace, the documents to write and those to
/// delete (with the chunks the store holds for them).
struct RunPlan {
    namespace: pb::Namespace,
    to_write: Vec<(DocumentRef, Plan)>,
    /// Documents the source no longer lists, with their stored chunk count,
    /// deleted last and in order.
    vanished: Vec<(String, u64)>,
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
    /// The documents written: key, version, chunk count.
    written: Vec<IndexIngestDocumentV1>,
    /// Vanished documents deleted so far: a prefix of `RunPlan::vanished`.
    vanished_deleted: usize,
    /// Changed documents whose old points were deleted and whose new points
    /// were not written (the write failed): gone from the store.
    dropped: BTreeSet<String>,
    /// Whether any delete or upsert was issued. Once true, a failure
    /// summary carries the store's totals and record.
    store_touched: bool,
}

impl Tally {
    fn skip(&mut self, reason: &'static str) {
        *self.skipped.entry(reason).or_default() += 1;
    }

    fn written_keys(&self) -> BTreeSet<&str> {
        self.written
            .iter()
            .map(|document| document.document_key.as_str())
            .collect()
    }
}

/// A document the run decided about.
#[derive(Clone, Copy)]
enum Plan {
    New,
    /// The store holds another version (or several): its points are deleted
    /// before the new ones are written. `prior_chunks` is what it holds now,
    /// which stays the document's chunk count if it cannot be re-indexed.
    Changed {
        prior_chunks: u64,
    },
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

/// The store as the summary describes it.
struct StoreRecord {
    total_documents: u64,
    total_chunks: u64,
    /// No unchanged, retained or still-present vanished document stands
    /// outside `Tally::written`.
    complete: bool,
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
        // The command verifier refuses such a command as unsupported (it is
        // the Python runtime's); this is the defence behind it.
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
        space: expected_space(run.command),
        consecutive_embedding_failures: 0,
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
    /// The embedding space the run writes into: the command's expected space,
    /// or the one the first embeddings answer fixed.
    space: Option<pb::EmbeddingSpace>,
    /// Documents in a row whose embedding failed for a retryable reason.
    consecutive_embedding_failures: u32,
    tally: Tally,
}

/// The version the store holds for each key that holds exactly one.
fn single_versions(prior: &BTreeMap<String, Vec<(String, u64)>>) -> BTreeMap<String, String> {
    prior
        .iter()
        .filter(|(_, versions)| versions.len() == 1)
        .map(|(key, versions)| (key.clone(), versions[0].0.clone()))
        .collect()
}

impl Runner<'_> {
    async fn run(mut self) -> Result<IndexIngestSummaryV1, IndexRunFailure> {
        let plan = match self.plan().await {
            Ok(plan) => plan,
            // Nothing is written before the plan is made.
            Err(Halt::Reason(reason)) => return Ok(failure(self.parameters, &reason)),
            Err(Halt::Failure(failure)) => return Err(failure),
        };
        match self.write(&plan).await {
            Ok(()) => Ok(self.summary(&plan)),
            Err(Halt::Reason(reason)) => Ok(self.failed(&reason, &plan)),
            // Stopped or out of time after the store was touched: the
            // summary carries what the run did to it. A lost lease never
            // reports (another worker may hold the claim), and nothing was
            // touched before the plan was made, so the rest stays a failure.
            Err(Halt::Failure(IndexRunFailure::Cancelled)) if self.tally.store_touched => {
                Ok(self.interrupted(Interruption::Stopped, &plan))
            }
            Err(Halt::Failure(IndexRunFailure::DeadlineExceeded)) if self.tally.store_touched => {
                Ok(self.interrupted(Interruption::DeadlineExceeded, &plan))
            }
            Err(Halt::Failure(failure)) => Err(failure),
        }
    }

    /// Lists the source, reads what the store holds and diffs the two
    /// (ADR-0030 decision 3).
    async fn plan(&mut self) -> Result<RunPlan, Halt> {
        let parameters = self.parameters;
        let namespace = toolkit_namespace(&self.run.command.index_id);
        let mut prior: BTreeMap<String, Vec<(String, u64)>> = BTreeMap::new();
        let filter = PathFilter::new(&parameters.whitelist, &parameters.blacklist);
        if !parameters.clean_index {
            // The listing is read where the index lives: the expected space's
            // collection when the index is stamped, every collection before
            // the first run stamps it. The pages are folded as they arrive
            // and bounded, on both sides: the facade is asked for at most
            // twice the documents one run handles (every document may hold
            // two versions after a failed rewrite) and stops scrolling at
            // that bound, and the fold here breaks at the same count should
            // an older facade not. An index that outgrew what one run
            // handles is refused, not silently diffed against part of itself.
            let max_rows = self.runtime.limits.max_documents.saturating_mul(2);
            let too_many = || {
                halt(&format!(
                    "the vector store holds more than {max_rows} document versions for this index; one run handles at most {}; re-index into a new index",
                    self.runtime.limits.max_documents
                ))
            };
            let mut rows = 0_usize;
            let consumed = self
                .runtime
                .vector
                .list_documents_with(
                    self.token,
                    namespace.clone(),
                    expected_space(self.run.command),
                    Some(max_rows as u64),
                    |document| {
                        rows += 1;
                        if rows > max_rows {
                            return ControlFlow::Break(());
                        }
                        prior
                            .entry(document.document_key)
                            .or_default()
                            .push((document.document_version, document.chunk_count));
                        ControlFlow::Continue(())
                    },
                )
                .await
                .map_err(|error| match error {
                    elitea_vector_index::Error::Rpc {
                        code: tonic::Code::ResourceExhausted,
                        ..
                    } => too_many(),
                    other => halt(&vector_text(&other)),
                })?;
            if consumed.is_break() {
                return Err(too_many());
            }
        }
        self.check_liveness()?;
        // The source lists after the store is read, so a connector that can
        // skip re-reading what has not moved is told what the index holds.
        self.source.remember_versions(single_versions(&prior));
        let listing = self
            .source
            .list()
            .await
            .map_err(|error| halt(&source_text(&error)))?;
        self.check_liveness()?;
        let listing: Vec<DocumentRef> = listing
            .into_iter()
            .filter(|reference| filter.keeps(&reference.key))
            .collect();
        if listing.len() > self.runtime.limits.max_documents {
            return Err(halt(&format!(
                "the source holds {} documents after filtering; one run indexes at most {}",
                listing.len(),
                self.runtime.limits.max_documents
            )));
        }
        let mut to_write: Vec<(DocumentRef, Plan)> = Vec::new();
        let listed: BTreeSet<&str> = listing.iter().map(|r| r.key.as_str()).collect();
        for reference in &listing {
            match prior.get(&reference.key) {
                None => to_write.push((reference.clone(), Plan::New)),
                Some(versions) if versions.len() == 1 && versions[0].0 == reference.version => {
                    self.tally.unchanged_documents += 1;
                    self.tally.unchanged_chunks += versions[0].1;
                }
                Some(versions) => to_write.push((
                    reference.clone(),
                    Plan::Changed {
                        prior_chunks: versions.iter().map(|(_, chunks)| chunks).sum(),
                    },
                )),
            }
        }
        let vanished: Vec<(String, u64)> = prior
            .iter()
            .filter(|(key, _)| !listed.contains(key.as_str()))
            .map(|(key, versions)| {
                (
                    key.clone(),
                    versions.iter().map(|(_, chunks)| chunks).sum::<u64>(),
                )
            })
            .collect();
        Ok(RunPlan {
            namespace,
            to_write,
            vanished,
            reindex: !prior.is_empty(),
        })
    }

    /// Fetches, extracts and chunks with bounded concurrency; embeds and
    /// writes as documents complete; deletes the vanished ones last. The
    /// embedding space the run wrote into is left in `self.space`.
    #[allow(clippy::too_many_lines)] // One linear pass: prepare, embed, fence, write.
    async fn write(&mut self, plan: &RunPlan) -> Result<(), Halt> {
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
                    let limits = self.runtime.limits;
                    let extractor = self.runtime.extractor.clone();
                    tasks.spawn(prepare(
                        source, chunking, reference, plan, limits, extractor,
                    ));
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
                    Ok(Preparation::Configuration(reason)) => break Err(halt(&reason)),
                    Err(_) => {
                        break Err(halt(
                            "a document could not be prepared: the preparation task stopped unexpectedly",
                        ));
                    }
                };
                let vectors = match self.embed_all(&prepared.chunks).await {
                    Ok(vectors) => {
                        self.consecutive_embedding_failures = 0;
                        vectors
                    }
                    Err(EmbedOutcome::Mismatch(message) | EmbedOutcome::Fatal(message)) => {
                        break Err(halt(&message));
                    }
                    Err(EmbedOutcome::TooLarge) => {
                        self.tally.skip(SKIP_TOO_LARGE);
                        continue;
                    }
                    Err(EmbedOutcome::Failed(cause)) => {
                        self.tally.failed_chunks += prepared.chunks.len() as u64;
                        self.tally.skip(SKIP_EMBEDDING_FAILED);
                        self.consecutive_embedding_failures += 1;
                        if self.consecutive_embedding_failures >= MAX_CONSECUTIVE_EMBEDDING_FAILURES {
                            break Err(halt(&format!(
                                "the embedding gateway failed for {} documents in a row ({cause}); the run stopped rather than skip every document, retry once the gateway is healthy",
                                self.consecutive_embedding_failures
                            )));
                        }
                        continue;
                    }
                };
                let dimension = vectors.first().map_or(0, Vec::len);
                if let Some(expected) = expected_dimension(self.run.command)
                    && expected != dimension
                {
                    break Err(halt(&format!(
                        "embedding dimension changed from {expected} ({}) to {dimension} ({}): an index keeps one embedding space, re-index into a new index to change the model",
                        self.run.command.expected_embedding_model, self.model
                    )));
                }
                let written_into = self
                    .space
                    .get_or_insert_with(|| pb::EmbeddingSpace {
                        model_slug: vector_space_slug(self.model),
                        dimension: u32::try_from(dimension).unwrap_or(u32::MAX),
                    })
                    .clone();
                // Nothing was written before this point. A clean_index drops
                // the namespace once, now that the embedding space is proven.
                if !cleaned {
                    self.delete(&plan.namespace, DeleteSelector::WholeNamespace)
                        .await?;
                    cleaned = true;
                }
                let key = prepared.reference.key.clone();
                if matches!(prepared.plan, Plan::Changed { .. }) {
                    self.delete(
                        &plan.namespace,
                        DeleteSelector::Documents(vec![key.clone()]),
                    )
                    .await?;
                    // Its old points are gone; until the new ones are written
                    // the document is not in the store.
                    self.tally.dropped.insert(key.clone());
                }
                let points = points_of(&prepared, vectors);
                let count = points.len() as u64;
                self.tally.store_touched = true;
                self.runtime
                    .vector
                    .upsert(self.token, written_into, plan.namespace.clone(), points)
                    .await
                    .map_err(|error| halt(&vector_text(&error)))?;
                self.tally.dropped.remove(&key);
                self.tally.indexed_documents += 1;
                if matches!(prepared.plan, Plan::Changed { .. }) {
                    self.tally.updated_documents += 1;
                }
                self.tally.indexed_chunks += count;
                self.tally.written.push(IndexIngestDocumentV1 {
                    document_key: key,
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
            self.check_liveness()?;
            self.delete(&plan.namespace, DeleteSelector::WholeNamespace)
                .await?;
        }
        for batch in plan.vanished.chunks(DELETE_BATCH) {
            self.check_liveness()?;
            let keys: Vec<String> = batch.iter().map(|(key, _)| key.clone()).collect();
            self.delete(&plan.namespace, DeleteSelector::Documents(keys))
                .await?;
            self.tally.vanished_deleted += batch.len();
        }
        Ok(())
    }

    /// One delete. A document delete counts the store as touched before the
    /// call: one that failed may or may not have happened, and the summary
    /// then describes the store (the document retained, conservatively)
    /// rather than keep silent about it. The whole-namespace delete of a
    /// clean run counts only once it SUCCEEDED: before it the summary would
    /// state an empty store (`whole_index_record`, zero totals) and Main
    /// would forget every document, while the delete may never have
    /// happened; a failed one leaves the summary saying nothing about the
    /// store, and the registry keeps what it had.
    async fn delete(
        &mut self,
        namespace: &pb::Namespace,
        selector: DeleteSelector,
    ) -> Result<(), Halt> {
        let whole_namespace = matches!(selector, DeleteSelector::WholeNamespace);
        if !whole_namespace {
            self.tally.store_touched = true;
        }
        self.runtime
            .vector
            .delete(self.token, None, namespace.clone(), selector)
            .await
            .map_err(|error| halt(&vector_text(&error)))?;
        self.tally.store_touched = true;
        Ok(())
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
    /// retryable gateway refusal. The gateway's answer is classified: a
    /// refusal that another attempt cannot cure (the credential, the
    /// project, the budget, an unknown model, any other 4xx but 429) is
    /// fatal to the run; a retryable one (429, 5xx, a timeout, the
    /// transport) fails this document.
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
                self.runtime.limits.embedding_retry_backoff,
            )
            .await
            .map_err(|error| {
                tracing::warn!(
                    event = "index_embedding_failed",
                    error = %error,
                    retryable = error.retryable(),
                );
                classify_embedding_error(error)
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

    /// The store after the run, from the plan and what the run did to it:
    /// unchanged documents, documents written, changed documents whose old
    /// points stayed (not written, not dropped), and vanished documents not
    /// deleted yet.
    fn store_record(&self, plan: &RunPlan) -> StoreRecord {
        let written = self.tally.written_keys();
        let mut retained_documents = 0_u64;
        let mut retained_chunks = 0_u64;
        for (reference, decided) in &plan.to_write {
            if let Plan::Changed { prior_chunks } = decided
                && !written.contains(reference.key.as_str())
                && !self.tally.dropped.contains(&reference.key)
            {
                retained_documents += 1;
                retained_chunks += prior_chunks;
            }
        }
        for (_, chunks) in &plan.vanished[self.tally.vanished_deleted..] {
            retained_documents += 1;
            retained_chunks += chunks;
        }
        StoreRecord {
            total_documents: self.tally.unchanged_documents
                + self.tally.written.len() as u64
                + retained_documents,
            total_chunks: self.tally.unchanged_chunks + self.tally.indexed_chunks + retained_chunks,
            complete: self.tally.unchanged_documents == 0 && retained_documents == 0,
        }
    }

    /// The keys whose points this run deleted: vanished documents deleted,
    /// and changed documents dropped by a failed write.
    fn removed_keys(&self, plan: &RunPlan) -> Vec<String> {
        plan.vanished[..self.tally.vanished_deleted]
            .iter()
            .map(|(key, _)| key.clone())
            .chain(self.tally.dropped.iter().cloned())
            .collect()
    }

    /// The summary of a run that went through: the counts, the store's
    /// totals and record, the stamp.
    fn summary(self, plan: &RunPlan) -> IndexIngestSummaryV1 {
        let partial = self.tally.failed_chunks > 0 || !self.tally.skipped.is_empty();
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
        let tally = &self.tally;
        let mut message = format!(
            "Index '{}' of toolkit '{}': {} documents indexed ({} chunks), {} unchanged, {} removed.",
            self.parameters.index_name,
            self.toolkit_name,
            tally.indexed_documents,
            tally.indexed_chunks,
            tally.unchanged_documents,
            tally.vanished_deleted,
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
        self.with_record(status, state, message, plan)
    }

    /// The `ERROR` summary of a run that stopped for `reason`. Once the store
    /// was touched it carries the store's totals and record, so the registry
    /// follows what was written and deleted before the failure; before that,
    /// it says nothing about the store.
    fn failed(self, reason: &str, plan: &RunPlan) -> IndexIngestSummaryV1 {
        let bare = failure(self.parameters, reason);
        if !self.tally.store_touched {
            return bare;
        }
        self.with_record(
            IndexIngestStatusV1::Error,
            IndexIngestTerminalStateV1::Failed,
            bare.message,
            plan,
        )
    }

    /// The summary of a run stopped, or out of time, at a document boundary
    /// after it touched the store: an `ERROR` summary with the store's
    /// totals and record, so the registry follows the store.
    fn interrupted(self, cause: Interruption, plan: &RunPlan) -> IndexIngestSummaryV1 {
        let tally = &self.tally;
        let message = format!(
            "Index '{}' of toolkit '{}' {}: {} documents indexed ({} chunks), {} unchanged, {} removed before it ended.",
            self.parameters.index_name,
            self.toolkit_name,
            cause.phrase(),
            tally.indexed_documents,
            tally.indexed_chunks,
            tally.unchanged_documents,
            tally.vanished_deleted,
        );
        let (status, state) = cause.terminal();
        self.with_record(status, state, message, plan)
    }

    fn with_record(
        self,
        status: IndexIngestStatusV1,
        state: IndexIngestTerminalStateV1,
        message: String,
        plan: &RunPlan,
    ) -> IndexIngestSummaryV1 {
        let store = self.store_record(plan);
        let removed = self.removed_keys(plan);
        let tally = self.tally;
        let (documents, removed_document_keys, documents_truncated) =
            bounded_record(tally.written, removed);
        // The stamp is reported only when vectors were written: an empty
        // run has nothing to stamp (Main stamps on the first chunk).
        let (embedding_model, embedding_dimension) = match (&self.space, tally.indexed_chunks) {
            (Some(space), chunks) if chunks > 0 => (self.model.to_owned(), space.dimension),
            _ => (String::new(), 0),
        };
        IndexIngestSummaryV1 {
            status: status as i32,
            message,
            terminal_state: state as i32,
            indexed: tally.indexed_documents,
            updated: tally.updated_documents,
            reindex: Some(plan.reindex),
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
            whole_index_record: store.complete && !documents_truncated,
            documents,
            removed_document_keys,
            documents_truncated,
            total_documents: Some(store.total_documents),
            total_chunks: Some(store.total_chunks),
        }
    }
}

impl Interruption {
    const fn phrase(self) -> &'static str {
        match self {
            Self::Stopped => "was stopped",
            Self::DeadlineExceeded => "ran out of time",
        }
    }

    const fn terminal(self) -> (IndexIngestStatusV1, IndexIngestTerminalStateV1) {
        match self {
            Self::Stopped => (
                IndexIngestStatusV1::Error,
                IndexIngestTerminalStateV1::Cancelled,
            ),
            Self::DeadlineExceeded => (
                IndexIngestStatusV1::Error,
                IndexIngestTerminalStateV1::Failed,
            ),
        }
    }
}

/// Turns a summary whose run touched the store (it carries the totals) into
/// the interrupted form when a Stop or the deadline arrives after the run
/// finished and before its terminal is published: the store's totals and
/// record stay as they are, the status becomes `ERROR` with the cause's
/// terminal state, and the message says so. Returns `false`, touching
/// nothing, for a summary without totals: the run never touched the store,
/// and the terminal is the plain runtime failure. Idempotent for a summary
/// already in that form.
pub(crate) fn interrupt_summary(summary: &mut IndexIngestSummaryV1, cause: Interruption) -> bool {
    if summary.total_documents.is_none() {
        return false;
    }
    let (status, state) = cause.terminal();
    if summary.status == status as i32 && summary.terminal_state == state as i32 {
        return true;
    }
    summary.status = status as i32;
    summary.terminal_state = state as i32;
    let _ = write!(
        summary.message,
        " The run {} as it finished; the index records what was written.",
        cause.phrase()
    );
    true
}

enum EmbedOutcome {
    /// The gateway failed for this document for a reason another attempt
    /// could cure; the run goes on (up to
    /// [`MAX_CONSECUTIVE_EMBEDDING_FAILURES`] documents in a row).
    Failed(String),
    /// The gateway refused for a reason no attempt cures: the credential,
    /// the project, the budget, an unknown model, another 4xx. The run
    /// stops with the reason.
    Fatal(String),
    /// One request of this document exceeds the request bound; the document
    /// is skipped `too_large`.
    TooLarge,
    /// The vectors disagree with the index's space; the run stops.
    Mismatch(String),
}

fn classify_embedding_error(error: EmbeddingsError) -> EmbedOutcome {
    match error {
        EmbeddingsError::Refused {
            code,
            retryable: false,
        } => EmbedOutcome::Fatal(format!(
            "the embedding gateway refused the run ({code}); nothing further was embedded"
        )),
        EmbeddingsError::TooLarge => EmbedOutcome::TooLarge,
        retryable @ (EmbeddingsError::Refused { .. }
        | EmbeddingsError::Transport
        | EmbeddingsError::InvalidResponse(_)) => EmbedOutcome::Failed(retryable.to_string()),
    }
}

async fn embed_with_retry(
    embeddings: &EmbeddingsClient,
    context: &ClaimScopedEliteaContext,
    model: &str,
    inputs: &[String],
    backoff: std::time::Duration,
) -> Result<crate::transport::embeddings_facade::Embedded, EmbeddingsError> {
    let mut attempt = 0;
    loop {
        match embeddings.embed(context, model, inputs).await {
            Ok(embedded) => return Ok(embedded),
            Err(error) if error.retryable() && attempt + 1 < EMBEDDING_ATTEMPTS => {
                attempt += 1;
                tokio::time::sleep(backoff * attempt).await;
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
/// work (extraction, tokenising) is on a blocking thread. A PDF or Office
/// document is read by the extraction sidecar when the run has one
/// (`extractor`), and skipped `unsupported_type` otherwise.
async fn prepare(
    source: Arc<IndexSource>,
    chunking: Arc<ChunkingConfig>,
    reference: DocumentRef,
    plan: Plan,
    limits: IndexRunLimits,
    extractor: Option<Arc<ExtractorClient>>,
) -> Preparation {
    let max_bytes = limits.max_document_bytes;
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
    let mut bytes = document.bytes;
    // A document format (PDF, Office, e-mail) is the sidecar's to read; this
    // process extracts text and HTML itself, on the blocking thread below.
    let sidecar_text = if elitea_doc_extract::is_document_type(&mime) {
        let sent = std::mem::take(&mut bytes);
        match sidecar_text(extractor, &key, &mime, sent, limits).await {
            Ok(text) => Some(text),
            Err(label) => return Preparation::Skipped(label),
        }
    } else {
        None
    };
    let chunked = tokio::task::spawn_blocking(move || {
        let is_html = matches!(mime.as_str(), "text/html" | "application/xhtml+xml");
        let options = ExtractOptions {
            html_to_markdown: true,
        };
        let text = match sidecar_text {
            Some(text) => text,
            None => match elitea_doc_extract::extract_with(&mime, &bytes, &options) {
                Extracted::Text { text, .. } => text,
                Extracted::Unsupported => return Preparation::Skipped(SKIP_UNSUPPORTED),
                Extracted::Unreadable(_) => return Preparation::Skipped(SKIP_UNREADABLE),
            },
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

/// One document's text from the extraction sidecar, or the summary label
/// it is skipped under: `unsupported_type` without a sidecar, the sidecar's
/// own reason, or `extract_failed` when it did not answer about the
/// document. The bytes move into the client; nothing copies them here.
async fn sidecar_text(
    extractor: Option<Arc<ExtractorClient>>,
    key: &str,
    mime: &str,
    bytes: Vec<u8>,
    limits: IndexRunLimits,
) -> Result<String, &'static str> {
    let Some(extractor) = extractor else {
        return Err(SKIP_UNSUPPORTED);
    };
    let extracted = extractor
        .extract(
            key,
            mime,
            bytes,
            limits.max_document_bytes,
            limits.extract_timeout,
        )
        .await;
    match extracted {
        Ok(Extraction::Text(text)) => Ok(text),
        Ok(Extraction::Skipped(reason)) => Err(skip_label(reason)),
        Err(error) => {
            tracing::warn!(
                event = "index_document_extract_failed",
                socket = %extractor.socket().display(),
                error = %error
            );
            Err(SKIP_EXTRACT_FAILED)
        }
    }
}

/// The summary's skip label of a sidecar skip reason. The older labels are
/// kept where the meaning is the same; the sidecar's own reasons get their
/// own.
fn skip_label(reason: SkipReason) -> &'static str {
    match reason {
        SkipReason::TooLarge | SkipReason::OutputTooLarge => SKIP_TOO_LARGE,
        SkipReason::TooManyPages => SKIP_TOO_MANY_PAGES,
        SkipReason::Encrypted => SKIP_ENCRYPTED,
        SkipReason::Unsupported => SKIP_UNSUPPORTED,
        SkipReason::Corrupt => SKIP_UNREADABLE,
        SkipReason::Timeout => SKIP_EXTRACT_TIMEOUT,
        SkipReason::Empty => SKIP_EMPTY,
    }
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
/// A dependent document (an attachment, a discussion; ADR-0030 "Dependent
/// documents") names its parent in `parent_id`, from the connector's
/// `parent_key` metadata.
fn points_of(prepared: &Prepared, vectors: Vec<Vec<f32>>) -> Vec<pb::Point> {
    let parent_id = prepared
        .metadata
        .get("parent_key")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_owned();
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
                parent_id: parent_id.clone(),
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

/// The per-document record, bounded to the frame's budget in bytes and to
/// [`MAX_INDEX_RESULT_DOCUMENTS`] entries per list (the protocol's bound,
/// which Main enforces too): written documents first, then removed keys;
/// `truncated` when some did not fit. The totals are not derived from the
/// lists, so a capped record still carries the store's counts.
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
        if documents.len() >= MAX_INDEX_RESULT_DOCUMENTS || cost > budget {
            truncated = true;
            break;
        }
        budget -= cost;
        documents.push(document);
    }
    for key in removed {
        let cost = key.len() + 3;
        if truncated || removed_keys.len() >= MAX_INDEX_RESULT_DOCUMENTS || cost > budget {
            truncated = true;
            break;
        }
        budget -= cost;
        removed_keys.push(key);
    }
    (documents, removed_keys, truncated)
}

/// An `ERROR` summary with `reason` and nothing about the store: the run
/// stopped before touching it.
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

/// The run stops for `reason`; `Runner::run` decides whether the summary
/// carries the store's record (see `Runner::failed`).
fn halt(reason: &str) -> Halt {
    Halt::Reason(reason.to_owned())
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
            arguments: Map::new(),
        },
        reason,
    )
}
