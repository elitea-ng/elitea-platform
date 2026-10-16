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

use adk_rust::futures::StreamExt as _;
use adk_rust::futures::stream::FuturesOrdered;
use bytes::Bytes;
use elitea_connectors::pictures::{PictureBatch, PictureStep};
use elitea_content_source::{Acl, DocumentRef, SourceError};
use elitea_doc_chunk::{
    Chunk, ChunkError, ChunkOptions, ChunkPlan, ChunkingConfig, Described, EmbeddedVisionPlan,
    ImagePlan, Inline,
};
use elitea_doc_extract::{ExtractOptions, Extracted, VisionImage};
use elitea_vector_index::client::toolkit_namespace;
use elitea_vector_index::{DeleteSelector, pb};
use prost::Message as _;
use serde_json::{Map, Value};
use tokio::task::JoinSet;

use super::image_fit::{self, ImageKind};
use super::inputs::{self, IndexParameters};
use super::model_steps::{
    ChatBinding, ChatLoader, ChatUnavailable, ModelSteps, PictureGateNow, RawImage, StepEnd,
    StepEnv,
};
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
use crate::transport::extractor_client::{
    ExtractedDocument, Extraction, ExtractorClient, Picture, PictureRequest, PicturesDropped,
    SkipReason,
};
use crate::transport::input_content::MaterializedInput;
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
pub(super) const SKIP_UNREADABLE: &str = "unreadable";
const SKIP_NEEDS_MODEL: &str = "needs_model";
/// A step that calls a chat model has no model bound (ADR-0030 I4).
pub(super) const SKIP_NO_MODEL: &str = "no_model";
/// The chat model failed for this document for a reason another run cures;
/// its stored version is untouched (ADR-0030 I4).
pub(super) const SKIP_MODEL_FAILED: &str = "model_failed";
/// One document alone needs more model calls than the whole per-run cap
/// (ADR-0030 I4); its stored version is untouched.
pub(super) const SKIP_MODEL_CAP_EXCEEDED: &str = "model_cap_exceeded";
const SKIP_EMPTY: &str = "empty";
/// The model's reply to one picture is not one another attempt would change
/// (it declined, or the reply is over the size bound): final for the picture,
/// never marks the document for another run.
pub(super) const SKIP_REPLY_UNUSABLE: &str = "reply_unusable";
/// The model spent the whole `max_tokens` on reasoning and described
/// nothing. The same request gets the same cut, so it is final for the
/// picture (no retry, no pending mark) and benign for the run; the summary
/// says to raise `max_tokens`.
pub(super) const SKIP_REASONING_EXHAUSTED_TOKENS: &str = "reasoning_exhausted_tokens";
const SKIP_FETCH_FAILED: &str = "fetch_failed";
/// The source stayed rate limited past its backoff bound: nothing is wrong
/// with the document, a later run reads it. The stored version is untouched.
pub(super) const SKIP_RATE_LIMITED: &str = "rate_limited";
const SKIP_EMBEDDING_FAILED: &str = "embedding_failed";
pub(super) const SKIP_TOO_LARGE: &str = "too_large";
/// The model refused this picture alone (a text-only model, a provider's 400
/// for one image); the run goes on and a later run asks again.
pub(super) const SKIP_VISION_REFUSED: &str = "vision_refused";
/// The extraction sidecar did not answer about the document (socket, stream
/// or sidecar failure); the document is retried by the next run.
const SKIP_EXTRACT_FAILED: &str = "extract_failed";
/// The sidecar's typed reasons that have no older label.
const SKIP_ENCRYPTED: &str = "encrypted";
const SKIP_TOO_MANY_PAGES: &str = "too_many_pages";
/// A picture embedded in a document, over the per-document count.
const SKIP_TOO_MANY_PICTURES: &str = "too_many_pictures";
pub(super) const SKIP_EXTRACT_TIMEOUT: &str = "extract_timeout";

/// Whether a skip of `reason` makes the run `PARTLY_INDEXED`. The SDK
/// degrades a run for lost work only (failed chunks, documents it could not
/// read or parse, `explicitly_failed_documents`), never for documents that
/// are not indexable by design: an unsupported type, an empty file, one
/// excluded by a filter or a limit (`NOT_INDEXED`/`SKIPPED` in its report).
/// So `unsupported_type`, `empty`, `too_large`, `too_many_pages` and
/// `encrypted` leave the status `OK`, but only for a NEW document: when the
/// skipped document is a CHANGED stored one its old points stay, stale, and
/// that is lost work. `no_model` and `needs_model` are benign unless this
/// document's own config asked for a model step (`explicit_model`: its
/// extension's `proposal` or `statistical` chunker, or `use_llm: true` for
/// its picture); the picture step that is on by default is optional. Every
/// other reason, an unknown one included, is a lost document.
pub(super) fn skip_loses_document(reason: &str, plan: Plan, explicit_model: bool) -> bool {
    if matches!(plan, Plan::Changed { .. }) {
        return true;
    }
    match reason {
        SKIP_UNSUPPORTED
        | SKIP_EMPTY
        | SKIP_TOO_LARGE
        | SKIP_TOO_MANY_PAGES
        | SKIP_TOO_MANY_PICTURES
        | SKIP_ENCRYPTED
        | SKIP_REASONING_EXHAUSTED_TOKENS => false,
        SKIP_NO_MODEL | SKIP_NEEDS_MODEL => explicit_model,
        _ => true,
    }
}

/// Appended (with a run count, `#pictures-pending-2`) to a document's stored
/// version when some of its embedded pictures were skipped for a reason
/// another run cures (`model_failed`, `model_cap_exceeded`,
/// `extract_timeout`): the text is indexed, and the version no source ever
/// lists, so the next run finds the document changed and prepares it again.
/// Once every picture is described, or skipped for a final reason, or the
/// mark has been carried for [`MAX_PENDING_RUNS`] runs in a row, the plain
/// source version is stored. A stored mark without a count is a first run.
pub(super) const PICTURES_PENDING: &str = "#pictures-pending";

/// The most consecutive runs a document carries the retry mark: a picture
/// that fails every time is given up on, not retried for ever.
pub(super) const MAX_PENDING_RUNS: u32 = 3;

/// The longest version the registry accepts.
const MAX_VERSION_BYTES: usize = 1024;

/// A picture skip that another run cures: only the classes that are
/// transient by nature (a retryable gateway error, a spent call cap, a
/// timeout). A reply the model will not change, an unreadable or oversized
/// picture and a refusing model are final.
fn retryable_picture_skip(reason: &str) -> bool {
    matches!(
        reason,
        SKIP_MODEL_FAILED
            | SKIP_MODEL_CAP_EXCEEDED
            | SKIP_EXTRACT_TIMEOUT
            | SKIP_FETCH_FAILED
            | SKIP_RATE_LIMITED
    )
}

/// `version` marked as pending for the `run`-th consecutive run; unmarked
/// (and warned) when the mark would not fit the registry's limit or the
/// document has carried it for [`MAX_PENDING_RUNS`] runs already.
fn pictures_pending(version: &str, run: u32) -> String {
    if run > MAX_PENDING_RUNS {
        tracing::warn!(
            event = "index_pictures_pending_given_up",
            runs = run - 1,
            "some of the document's pictures failed in every one of its runs; stored without the retry mark"
        );
        return version.to_owned();
    }
    let mark = format!("{PICTURES_PENDING}-{run}");
    if version.len() + mark.len() > MAX_VERSION_BYTES {
        tracing::warn!(
            event = "index_pictures_pending_unmarked",
            "the document's version is too long to carry the retry mark; its skipped pictures are not retried"
        );
        return version.to_owned();
    }
    format!("{version}{mark}")
}

/// The source's version behind a stored one, and how many consecutive runs
/// the retry mark has been carried (`0`: none).
fn split_version(stored: &str) -> (&str, u32) {
    let Some(at) = stored.rfind(PICTURES_PENDING) else {
        return (stored, 0);
    };
    let rest = &stored[at + PICTURES_PENDING.len()..];
    if rest.is_empty() {
        return (&stored[..at], 1);
    }
    match rest.strip_prefix('-').map(str::parse::<u32>) {
        Some(Ok(runs)) if runs > 0 => (&stored[..at], runs),
        _ => (stored, 0),
    }
}

/// How many consecutive runs the one stored version has carried the retry
/// mark while the source has not moved (`0`: none, or the source changed).
fn prior_pending(stored: &[(String, u64)], source: &str) -> u32 {
    match stored {
        [(version, _)] if source_version(version) == source => split_version(version).1,
        _ => 0,
    }
}

/// The source's version behind a stored one (the retry mark removed).
fn source_version(stored: &str) -> &str {
    split_version(stored).0
}

/// How many of one document's pictures are described at the same time.
const PICTURES_IN_FLIGHT: usize = 4;

/// The figure above, for the tests that pin the bound.
#[cfg(test)]
pub(super) const PICTURES_IN_FLIGHT_FOR_TESTS: usize = PICTURES_IN_FLIGHT;

/// [`split_version`], for the tests.
#[cfg(test)]
pub(super) fn split_version_for_tests(stored: &str) -> (&str, u32) {
    split_version(stored)
}

/// [`source_version`], for the tests.
#[cfg(test)]
pub(super) fn source_version_of(stored: &str) -> &str {
    source_version(stored)
}

/// What a run asks the extraction sidecar for, per document, when the
/// document's `use_llm` is on: the pictures embedded in it (ADR-0030 I4
/// part C). The sidecar lowers each figure to its own cap. Twenty pictures
/// of at most 5 MiB, 12 MiB together: a picture over 256 KiB is downscaled
/// to fit the gateway's inline budget before it is sent, and a document with
/// more than twenty keeps its first twenty (the rest are counted, not
/// described).
const PICTURE_REQUEST: PictureRequest = PictureRequest {
    pictures: 20,
    picture_bytes: 5 << 20,
    total_bytes: 12 << 20,
};

/// What one run is given.
pub(crate) struct IndexRun<'a> {
    pub(crate) command: &'a IndexIngestCommandV1,
    pub(crate) toolkit_configuration: &'a [u8],
    pub(crate) tool_parameters: &'a [u8],
    /// `tool_parameters` already parsed by the caller (once per claim). When
    /// absent the run parses them itself.
    pub(crate) parsed: Option<&'a inputs::ParsedArgs>,
    pub(crate) embedding_binding: Option<&'a [u8]>,
    /// `index.llm_model`: the chat model of the steps that call one, a JSON
    /// string. Absent, those steps skip their documents `no_model`.
    pub(crate) llm_model: Option<&'a [u8]>,
    /// `index.llm_configuration`: that model's settings, a JSON object.
    pub(crate) llm_configuration: Option<&'a [u8]>,
    /// Both chat entries, fetched together on the first step that needs the
    /// chat model (instead of `llm_model` / `llm_configuration`). A run that
    /// never needs the model fetches nothing.
    pub(crate) chat_entries: Option<&'a dyn ChatEntrySource>,
    pub(crate) vector_token: Option<&'a VectorClaimToken>,
    /// The redeemed claim: the PAT the gateway and the artifact API take,
    /// the project and the execution.
    pub(crate) context: &'a ClaimScopedEliteaContext,
    pub(crate) lease: &'a dyn RunLiveness,
    pub(crate) deadline_unix_millis: i64,
    pub(crate) now_unix_millis: &'a (dyn Fn() -> i64 + Send + Sync),
}

/// The chat model entries of a claim, fetched on demand.
#[async_trait::async_trait]
pub(crate) trait ChatEntrySource: Send + Sync {
    /// `index.llm_model` and `index.llm_configuration`, TOGETHER: either
    /// entry that is named and cannot be fetched fails the whole load, so a
    /// run never goes on with default settings for a configuration that
    /// failed. Implementations record the failure.
    ///
    /// # Errors
    ///
    /// An entry could not be fetched.
    async fn load(&self) -> Result<ChatEntries, ChatUnavailable>;
}

/// The fetched chat model entries; `None` is an entry the claim does not
/// carry.
pub(crate) struct ChatEntries {
    pub(crate) model: Option<MaterializedInput>,
    pub(crate) configuration: Option<MaterializedInput>,
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
    /// Stored documents (and their chunks) the current filters exclude: kept,
    /// not deleted, and counted in the store's totals.
    out_of_scope: (u64, u64),
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
    /// How many of the skips lost work (see [`skip_loses_document`]) and so
    /// make the run `PARTLY_INDEXED`.
    lost_skips: u64,
    /// Pictures EMBEDDED in an indexed document that were not described, by
    /// reason. The document itself was indexed (without that description),
    /// so these are not `skipped` documents and do not make the run
    /// `PARTLY_INDEXED`; the summary's message names them.
    pictures_skipped: BTreeMap<&'static str, u64>,
    /// DOCUMENTS whose embedded pictures were not even read, by reason (no
    /// chat model, a model that refuses images): nothing was asked of the
    /// extractor for them.
    pictures_not_read: BTreeMap<&'static str, u64>,
    /// The same two counts for the pictures a connector's documents LINK.
    linked_skipped: BTreeMap<&'static str, u64>,
    linked_not_read: BTreeMap<&'static str, u64>,
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
    /// A skip that loses work (an unknown reason, a failed embedding).
    fn skip(&mut self, reason: &'static str) {
        self.skip_document(reason, true);
    }

    /// A skip that is lost work when `lost` (see [`skip_loses_document`]).
    fn skip_document(&mut self, reason: &'static str, lost: bool) {
        *self.skipped.entry(reason).or_default() += 1;
        self.lost_skips += u64::from(lost);
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
pub(super) enum Plan {
    New,
    /// The store holds another version (or several): its points are deleted
    /// before the new ones are written. `prior_chunks` is what it holds now,
    /// which stays the document's chunk count if it cannot be re-indexed.
    Changed {
        prior_chunks: u64,
        /// The consecutive runs the stored version has carried the
        /// pictures-pending mark when the source has not moved (`0`: none,
        /// or the source changed since).
        prior_pending: u32,
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
    /// What became of the document's embedded pictures: tallied only when
    /// the document ends up indexed.
    /// Whether this document's own config asks for a model step (see
    /// [`skip_loses_document`]).
    explicit_model: bool,
    pictures: PictureTally,
}

/// What of a document is known before chunking, to finish it.
struct Shell {
    reference: DocumentRef,
    plan: Plan,
    metadata: Map<String, Value>,
    acl: Vec<String>,
    /// Whether the document's own config asks for a model step (see
    /// [`skip_loses_document`]).
    explicit_model: bool,
    pictures: PictureTally,
}

/// The picture counts of ONE document, held until its outcome is known: a
/// document that is then skipped, or fails to embed, has told the summary
/// nothing about its pictures, and a retried document reports only the run
/// that finally wrote it.
#[derive(Default)]
struct PictureTally {
    /// Pictures not described, by reason.
    skipped: BTreeMap<&'static str, u64>,
    /// The reason the document's pictures were not even asked for.
    not_read: Option<&'static str>,
    /// The pictures are the ones a connector's document LINKS, not ones
    /// embedded in a file: reported under their own label.
    linked: bool,
}

impl PictureTally {
    fn skip(&mut self, reason: &'static str, count: u64) {
        if count > 0 {
            *self.skipped.entry(reason).or_default() += count;
        }
    }

    /// Adds the document's counts to the run's, once, when it is indexed.
    fn settle(self, tally: &mut Tally) {
        let (skipped, not_read) = if self.linked {
            (&mut tally.linked_skipped, &mut tally.linked_not_read)
        } else {
            (&mut tally.pictures_skipped, &mut tally.pictures_not_read)
        };
        for (reason, count) in self.skipped {
            if count > 0 {
                *skipped.entry(reason).or_default() += count;
            }
        }
        if let Some(reason) = self.not_read {
            *not_read.entry(reason).or_default() += 1;
        }
    }
}

/// What became of one embedded picture.
enum PictureOutcome {
    Text(String),
    /// The same bytes were described earlier in this document (DOCX writes
    /// a back-reference).
    Repeat,
    Nothing,
}

/// A document extracted with pictures to describe.
struct EmbeddedJob {
    shell: Shell,
    key: String,
    chunking: Arc<ChunkingConfig>,
    plan: EmbeddedVisionPlan,
    /// The extractor's text: exactly what an extraction that did not ask
    /// for pictures gives.
    text: String,
    pictures: Vec<Picture>,
    /// Pictures found and not returned (over a bound, or not a format a
    /// model takes).
    dropped: PicturesDropped,
}

/// A connector's document with the pictures its text links read (ADR-0030
/// I4, PR-C2): the loop describes them and renders the descriptions into
/// the text, then the document is chunked like any other.
struct ConnectorJob {
    shell: Shell,
    key: String,
    mime: String,
    chunking: Arc<ChunkingConfig>,
    batch: PictureBatch,
    /// The toolkit's prompt (`image_description_prompt`), else the SDK's
    /// default.
    prompt: Option<String>,
}

/// What preparing one document gives the run's loop: a document that is
/// ready to go on, or one whose embedded pictures wait for their
/// descriptions before its text is chunked (ADR-0030 I4 part C), which the
/// loop finishes and turns into a [`Preparation`].
enum Prepare {
    Done(Preparation),
    Embedded(Box<EmbeddedJob>),
    Connector(Box<ConnectorJob>),
}

/// Whether a document's pictures are asked of the extractor, decided by the
/// run's loop.
#[derive(Clone)]
enum PictureAsk {
    /// Not a document with the step on.
    No,
    /// The run can describe pictures.
    Yes,
    /// The run cannot (this is the reason, counted for the document).
    NotRead(&'static str),
    /// The chat model is still loading: the document is fetched meanwhile,
    /// and its preparation waits for the answer only when it comes to ask.
    Later(tokio::sync::watch::Receiver<PictureGate>),
}

/// The run's chat model load, in flight.
type ChatLoad<'a> = std::pin::Pin<
    Box<dyn std::future::Future<Output = Result<Option<ChatBinding>, ChatUnavailable>> + Send + 'a>,
>;

/// What the loop knows about a document's pictures without waiting.
enum Asked {
    Decided(PictureAsk),
    NeedsModel,
}

/// What the loop waits for: the chat model, or a document's preparation.
enum PrepareEvent {
    Loaded(Result<Option<ChatBinding>, ChatUnavailable>),
    Joined(Option<Result<Prepare, tokio::task::JoinError>>),
}

/// What the chat model's load decided for the pictures of the run.
#[derive(Clone, Copy, PartialEq, Eq)]
enum PictureGate {
    Waiting,
    Open,
    Closed(&'static str),
}

impl PictureAsk {
    /// `Ok(wanted)`, or the reason the pictures are not asked for.
    async fn resolve(self) -> Result<bool, &'static str> {
        match self {
            Self::No => Ok(false),
            Self::Yes => Ok(true),
            Self::NotRead(reason) => Err(reason),
            Self::Later(mut gate) => {
                let decided = gate
                    .wait_for(|state| *state != PictureGate::Waiting)
                    .await
                    .map(|state| *state);
                match decided {
                    Ok(PictureGate::Open) => Ok(true),
                    Ok(PictureGate::Closed(reason)) => Err(reason),
                    // The run ended before the model loaded.
                    Ok(PictureGate::Waiting) | Err(_) => Ok(false),
                }
            }
        }
    }
}

impl From<Preparation> for Prepare {
    fn from(done: Preparation) -> Self {
        Self::Done(done)
    }
}

enum Preparation {
    Ready(Box<Prepared>),
    /// Chunking waits for a model (ADR-0030 I4): the plan to fulfil, and the
    /// document with no chunks yet.
    Model(Box<Prepared>, ChunkPlan),
    /// An image file waiting for its description (ADR-0030 I4 part B).
    Vision(Box<Prepared>, RawImage),
    /// Not indexed, for the reason, with the plan the document had.
    Skipped(&'static str, Plan),
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
#[allow(clippy::too_many_lines)] // One linear pass: validate the inputs, then build the run.
pub(crate) async fn run_index(
    runtime: &IndexIngestRuntime,
    run: IndexRun<'_>,
) -> Result<IndexIngestSummaryV1, IndexRunFailure> {
    let configuration = inputs::toolkit_configuration(run.toolkit_configuration)
        .map_err(|error| IndexRunFailure::InvalidInput(error.0))?;
    let own;
    let parsed = if let Some(parsed) = run.parsed {
        parsed
    } else {
        own = inputs::ParsedArgs::parse(run.tool_parameters)
            .map_err(|error| IndexRunFailure::InvalidInput(error.0))?;
        &own
    };
    let parameters = parsed.parameters.clone();
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
    let chunking = match &parsed.chunking {
        Ok(chunking) => chunking.clone(),
        Err(error) => return Ok(failure(&parameters, &error.to_string())),
    };
    for warning in &chunking.warnings {
        tracing::warn!(event = "index_chunking_config_setting_dropped", warning);
    }
    let unserved = chunking.unserved_use_llm_extensions();
    if !unserved.is_empty() {
        tracing::warn!(
            event = "index_use_llm_ignored",
            extensions = ?unserved,
            "`use_llm` describes standalone image files only; for these extensions (images inside documents) it has no effect"
        );
    }
    let filter = match PathFilter::for_run(&configuration.toolkit_type, &parameters) {
        Ok(filter) => filter,
        Err(reason) => return Ok(failure(&parameters, &reason)),
    };
    let ignored = inputs::ignored_loader_keys(&parameters.chunking_config);
    if !ignored.is_empty() {
        tracing::warn!(
            event = "index_loader_keys_ignored",
            keys = ?ignored,
            "per-page loader settings of spreadsheets, presentations and PDFs are not read by this worker"
        );
    }
    let bearer = run.context.model_facade_token();
    let project_id = run.context.resource_project_id().to_string();
    let source = match IndexSource::build(
        &configuration,
        &parameters,
        &filter,
        &PlatformAccess {
            origin: &runtime.platform_origin,
            bearer: &bearer,
            project_id: &project_id,
        },
        &runtime.allowlist,
        Arc::clone(&runtime.transport),
        runtime.limits,
    ) {
        Ok(source) => source,
        Err(error) => return Ok(failure(&parameters, &error.0)),
    };
    drop(bearer);
    let filter = source.scoped(filter);
    let source = Arc::new(source);
    Runner {
        runtime,
        run: &run,
        parameters: &parameters,
        toolkit_name: &configuration.toolkit_name,
        model: &model,
        token,
        source,
        filter,
        ignored_loader_keys: ignored,
        dependent_chunking: Arc::new(dependent_chunking(&chunking)),
        chunking: Arc::new(chunking),
        space: expected_space(run.command),
        consecutive_embedding_failures: 0,
        steps: ModelSteps::lazy(&runtime.limits),
        tally: Tally::default(),
    }
    .run()
    .await
}

/// Loads the run's chat model on first need: the claim's `index.llm_model`
/// entry bound through the runtime's binder, with `index.llm_configuration`'s
/// settings. Entries come from the claim's [`ChatEntrySource`] (fetched
/// together, once) or, without one, from the run's own bytes.
struct RunChatLoader<'a> {
    runtime: &'a IndexIngestRuntime,
    run: &'a IndexRun<'a>,
}

#[async_trait::async_trait]
impl ChatLoader for RunChatLoader<'_> {
    async fn load(&self) -> Result<Option<ChatBinding>, ChatUnavailable> {
        let fetched;
        let (model, configuration) = match self.run.chat_entries {
            Some(source) => {
                fetched = source.load().await?;
                (
                    fetched.model.as_ref().map(MaterializedInput::as_bytes),
                    fetched
                        .configuration
                        .as_ref()
                        .map(MaterializedInput::as_bytes),
                )
            }
            None => (self.run.llm_model, self.run.llm_configuration),
        };
        chat_binding(self.runtime, model, configuration)
    }
}

/// What every model step of a run is handed: the loader of the run's chat
/// model, the embeddings and limits, and the liveness probe. Built from the
/// run's shared parts, so no step assembles its own.
struct StepContext<'a> {
    loader: RunChatLoader<'a>,
    runtime: &'a IndexIngestRuntime,
    run: &'a IndexRun<'a>,
    model: &'a str,
}

impl<'a> StepContext<'a> {
    fn new(runtime: &'a IndexIngestRuntime, run: &'a IndexRun<'a>, model: &'a str) -> Self {
        Self {
            loader: RunChatLoader { runtime, run },
            runtime,
            run,
            model,
        }
    }

    fn env(&self) -> StepEnv<'_> {
        StepEnv {
            embeddings: &self.runtime.embeddings,
            context: self.run.context,
            embedding_model: self.model,
            limits: &self.runtime.limits,
            chat_loader: Some(&self.loader),
        }
    }

    /// Stop, the lease and the deadline, asked before every attempt.
    fn alive(&self) -> impl Fn() -> Result<(), IndexRunFailure> + Sync + 'a {
        let run = self.run;
        move || run_liveness(run)
    }
}

/// The chat model of the steps that call one. `Ok(None)` when the claim names
/// no model or the deployment has no binder (those steps skip `no_model`).
///
/// # Errors
///
/// An entry that is present and malformed: no model, and never default
/// settings in place of a configuration that cannot be read.
fn chat_binding(
    runtime: &IndexIngestRuntime,
    model: Option<&[u8]>,
    configuration: Option<&[u8]>,
) -> Result<Option<ChatBinding>, ChatUnavailable> {
    let unreadable = |error: inputs::InputError| {
        tracing::warn!(
            event = "index_chat_model_entries_unreadable",
            reason = %error.0,
            "the chat model entries are unreadable; images are skipped no_model and a proposal run fails"
        );
        ChatUnavailable
    };
    let Some(raw) = model else {
        return Ok(None);
    };
    let model_name = inputs::llm_model(raw).map_err(unreadable)?;
    let settings = match configuration {
        Some(raw) => inputs::llm_settings(raw).map_err(unreadable)?,
        None => inputs::LlmSettings::default(),
    };
    Ok(runtime.chat_models.clone().map(|binder| ChatBinding {
        binder,
        model_name,
        settings,
    }))
}

struct Runner<'a> {
    runtime: &'a IndexIngestRuntime,
    run: &'a IndexRun<'a>,
    parameters: &'a IndexParameters,
    toolkit_name: &'a str,
    model: &'a str,
    token: &'a VectorClaimToken,
    source: Arc<IndexSource>,
    /// What the run keeps of the listing (whitelist, blacklist, the
    /// family's own filters).
    filter: PathFilter,
    /// `.ext.key` settings of the per-page loaders that this worker does not
    /// read; named in the summary.
    ignored_loader_keys: Vec<String>,
    chunking: Arc<ChunkingConfig>,
    /// The config of dependent documents (attachments): `chunking`
    /// without the base chunker, built once.
    dependent_chunking: Arc<ChunkingConfig>,
    /// The embedding space the run writes into: the command's expected space,
    /// or the one the first embeddings answer fixed.
    space: Option<pb::EmbeddingSpace>,
    /// Documents in a row whose embedding failed for a retryable reason.
    consecutive_embedding_failures: u32,
    /// The model steps' state: the chat binding, the call count against the
    /// run's cap, the failure streak.
    steps: ModelSteps,
    tally: Tally,
}

/// The version the store holds for each key that holds exactly one.
fn single_versions(prior: &BTreeMap<String, Vec<(String, u64)>>) -> BTreeMap<String, String> {
    prior
        .iter()
        .filter(|(_, versions)| versions.len() == 1)
        // The source's own version: a connector compares it with what the
        // source says, and the retry mark is the pipeline's, not the source's.
        .map(|(key, versions)| (key.clone(), source_version(&versions[0].0).to_owned()))
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
    #[allow(clippy::too_many_lines)] // One linear pass: read the store, list, diff.
    async fn plan(&mut self) -> Result<RunPlan, Halt> {
        let parameters = self.parameters;
        let namespace = toolkit_namespace(&self.run.command.index_id);
        let mut prior: BTreeMap<String, Vec<(String, u64)>> = BTreeMap::new();
        // A clean run replaces the index whatever it holds, so it reads
        // nothing of it: it may rebuild the index or empty it (an explicit
        // user action; the summary says so). An incremental run reads the
        // store to diff against it.
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
        let listed_by_source = listing.len();
        let listing: Vec<DocumentRef> = listing
            .into_iter()
            .filter(|reference| self.filter.keeps(&reference.key))
            .collect();
        // A stored document the file-type allowlist now excludes (the new
        // default `skip_unsupported_extensions`: pictures, PDFs) is out of
        // scope, not vanished: a deploy must not delete what an earlier run
        // indexed. It stays, and the summary says so, until a clean run
        // rebuilds the index. (A
        // whitelist, blacklist or artifact pattern still removes what it no
        // longer lists, as before.)
        let out_of_scope_stored = |key: &str| self.filter.excluded_by_file_type(key);
        let out_of_scope = prior.keys().filter(|key| out_of_scope_stored(key)).count();
        let in_scope_stored = prior.len() - out_of_scope;
        let out_of_scope_chunks: u64 = prior
            .iter()
            .filter(|(key, _)| out_of_scope_stored(key))
            .flat_map(|(_, versions)| versions.iter().map(|(_, chunks)| *chunks))
            .sum();
        if out_of_scope > 0 {
            tracing::info!(
                event = "index_out_of_scope_documents_kept",
                documents = out_of_scope,
                "stored documents of a file type the run no longer indexes were kept; a clean index run applies the current scope"
            );
        }
        self.guard_empty_listing(listed_by_source, listing.len(), in_scope_stored)?;
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
                        prior_pending: prior_pending(versions, &reference.version),
                    },
                )),
            }
        }
        let vanished: Vec<(String, u64)> = prior
            .iter()
            .filter(|(key, _)| !listed.contains(key.as_str()) && !out_of_scope_stored(key))
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
            out_of_scope: (out_of_scope as u64, out_of_scope_chunks),
            reindex: !prior.is_empty(),
        })
    }

    /// An empty result must not read as "everything was deleted" (the SDK's
    /// EL-6232 guard). `listed` documents came from the connector and `kept`
    /// passed the filters. An incremental run whose listing is empty, or whose
    /// filters keep none of it, must not delete the `in_scope_stored`
    /// documents of a populated index: unless the connector attests that the
    /// source is empty it fails with the reason, before the store is
    /// touched. A clean run is the user's explicit instruction to replace
    /// the index, so it may rebuild it or empty it (the summary says so).
    fn guard_empty_listing(
        &self,
        listed: usize,
        kept: usize,
        in_scope_stored: usize,
    ) -> Result<(), Halt> {
        if self.parameters.clean_index
            || kept > 0
            || in_scope_stored == 0
            || (listed == 0 && self.filter.attests_empty())
        {
            return Ok(());
        }
        Err(halt(&if listed == 0 {
            "the source listing returned no documents while the index holds data from earlier runs; nothing was deleted. Check the source (branch, query, permissions) before running again, or run with clean_index to empty the index".to_owned()
        } else {
            format!(
                "the source lists {listed} documents but the whitelist, blacklist and file-type filters kept none of them; the index was left untouched. Check the filters, or run with clean_index to empty the index"
            )
        }))
    }

    /// Fetches, extracts and chunks with bounded concurrency; embeds and
    /// writes as documents complete; deletes the vanished ones last. The
    /// embedding space the run wrote into is left in `self.space`.
    #[allow(clippy::too_many_lines)] // One linear pass: prepare, embed, fence, write.
    async fn write(&mut self, plan: &RunPlan) -> Result<(), Halt> {
        let mut cleaned = !self.parameters.clean_index;
        let mut pending = plan.to_write.iter().cloned();
        let mut tasks: JoinSet<Prepare> = JoinSet::new();
        // The run's chat model, loaded while documents are fetched: started
        // by the first document whose pictures could be described, polled
        // beside the preparations, never awaited by the loop that feeds them.
        let mut loading: Option<ChatLoad<'_>> = None;
        let (gate_tx, _gate_rx) = tokio::sync::watch::channel(PictureGate::Waiting);
        let outcome: Result<(), Halt> = async {
            loop {
                while tasks.len() < self.runtime.limits.document_concurrency {
                    let Some((reference, plan)) = pending.next() else {
                        break;
                    };
                    let source = Arc::clone(&self.source);
                    let chunking = Chunkings {
                        base: Arc::clone(&self.chunking),
                        dependent: Arc::clone(&self.dependent_chunking),
                    };
                    let limits = self.runtime.limits;
                    let extractor = self.runtime.extractor.clone();
                    // Whether the document's pictures are worth reading is
                    // known BEFORE the extractor is asked for them, and not
                    // by waiting here for the model to load.
                    let pictures = match self.picture_ask(&reference) {
                        Asked::Decided(ask) => ask,
                        Asked::NeedsModel => {
                            if loading.is_none() {
                                let (runtime, run) = (self.runtime, self.run);
                                loading = Some(Box::pin(async move {
                                    RunChatLoader { runtime, run }.load().await
                                }));
                            }
                            PictureAsk::Later(gate_tx.subscribe())
                        }
                    };
                    tasks.spawn(prepare(
                        source, chunking, reference, plan, limits, extractor, pictures,
                    ));
                }
                let event = tokio::select! {
                    loaded = async {
                        match loading.as_mut() {
                            Some(load) => load.await,
                            None => std::future::pending().await,
                        }
                    } => PrepareEvent::Loaded(loaded),
                    joined = tasks.join_next() => PrepareEvent::Joined(joined),
                };
                let joined = match event {
                    PrepareEvent::Loaded(loaded) => {
                        loading = None;
                        self.steps.install_chat(loaded);
                        gate_tx.send_replace(match self.steps.picture_gate_now() {
                            PictureGateNow::Open => PictureGate::Open,
                            PictureGateNow::Closed(reason) => PictureGate::Closed(reason),
                            PictureGateNow::Unknown => PictureGate::Waiting,
                        });
                        continue;
                    }
                    PrepareEvent::Joined(None) => break Ok(()),
                    PrepareEvent::Joined(Some(joined)) => joined,
                };
                self.check_liveness()?;
                // A document with pictures to describe: describe them, put the
                // descriptions into its text, chunk it; then it is any other.
                let preparation = match joined {
                    Ok(Prepare::Done(done)) => done,
                    Ok(Prepare::Embedded(job)) => match self.finish_embedded(*job).await {
                        Ok(finished) => finished,
                        Err(halted) => break Err(halted),
                    },
                    Ok(Prepare::Connector(job)) => match self.finish_connector(*job).await {
                        Ok(finished) => finished,
                        Err(halted) => break Err(halted),
                    },
                    Err(_) => {
                        break Err(halt(
                            "a document could not be prepared: the preparation task stopped unexpectedly",
                        ));
                    }
                };
                let prepared = match preparation {
                    Preparation::Ready(prepared) => prepared,
                    Preparation::Model(mut prepared, chunk_plan) => {
                        match self
                            .fulfil_model_step(chunk_plan, prepared.plan, prepared.explicit_model)
                            .await
                        {
                            Ok(Some(chunks)) => {
                                prepared.chunks = chunks;
                                prepared
                            }
                            Ok(None) => continue,
                            Err(halt) => break Err(halt),
                        }
                    }
                    Preparation::Vision(mut prepared, raw) => {
                        match self
                            .fulfil_vision(&raw, prepared.plan, prepared.explicit_model)
                            .await
                        {
                            Ok(Some(chunks)) => {
                                prepared.chunks = chunks;
                                prepared
                            }
                            Ok(None) => continue,
                            Err(halt) => break Err(halt),
                        }
                    }
                    Preparation::Skipped(reason, plan) => {
                        // `prepare` skips only name a model step as
                        // `needs_model`: the chunker was asked for.
                        self.tally
                            .skip_document(reason, skip_loses_document(reason, plan, true));
                        continue;
                    }
                    Preparation::Configuration(reason) => break Err(halt(&reason)),
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
                        if let Err(halted) = self
                            .note_embedding_failure(&cause, prepared.chunks.len() as u64)
                        {
                            break Err(halted);
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
                // The pictures of a document are reported once, by the run
                // that wrote it.
                prepared.pictures.settle(&mut self.tally);
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
        run_liveness(self.run)
    }

    /// One document whose embedding failed for a reason another run cures:
    /// counted, skipped, and the run stops at the Nth in a row.
    fn note_embedding_failure(&mut self, cause: &str, failed_chunks: u64) -> Result<(), Halt> {
        self.tally.failed_chunks += failed_chunks;
        self.tally.skip(SKIP_EMBEDDING_FAILED);
        self.consecutive_embedding_failures += 1;
        if self.consecutive_embedding_failures >= MAX_CONSECUTIVE_EMBEDDING_FAILURES {
            return Err(halt(&format!(
                "the embedding gateway failed for {} documents in a row ({cause}); the run stopped rather than skip every document, retry once the gateway is healthy",
                self.consecutive_embedding_failures
            )));
        }
        Ok(())
    }

    /// Fulfils the model step of one document's chunking (ADR-0030 I4): the
    /// chunks, or `None` when the document was skipped under a reason the
    /// tally now holds.
    async fn fulfil_model_step(
        &mut self,
        plan: ChunkPlan,
        decided: Plan,
        explicit_model: bool,
    ) -> Result<Option<Vec<Chunk>>, Halt> {
        let context = StepContext::new(self.runtime, self.run, self.model);
        let (env, alive) = (context.env(), context.alive());
        let end = self.steps.fulfil(plan, &env, &alive).await;
        self.settle(end, decided, explicit_model)
    }

    /// Whether `reference`'s embedded pictures are to be asked of the
    /// extractor: only a PDF, DOCX or PPTX whose `use_llm` is on, and only
    /// while a picture of this run can still be described. With no chat
    /// model, or a model that refuses images, the pictures are not read at
    /// all (the document is counted in the summary's message, if it is
    /// indexed).
    ///
    /// Never waits: while the chat model has not loaded the answer is
    /// [`Asked::NeedsModel`], and the loop starts the load (once) and hands
    /// the document a receiver for the result, which its preparation waits
    /// on only when it comes to ask the extractor, after it was fetched.
    fn picture_ask(&self, reference: &DocumentRef) -> Asked {
        // A connector's own picture setting (ADR-0030 I4, PR-C2).
        let step = self.source.picture_step(reference);
        if matches!(step, Some(PictureStep::Inline { .. })) {
            return self.gate_ask();
        }
        let overlaid;
        let chunking: &ChunkingConfig = match step
            .as_ref()
            .and_then(|step| binary_overlay(&self.dependent_chunking, step, &reference.key))
        {
            Some(config) => {
                overlaid = config;
                &overlaid
            }
            None => &self.chunking,
        };
        if !elitea_doc_extract::is_document_type(&reference.mime)
            || elitea_doc_chunk::plan_embedded_for(&reference.key, &reference.mime, chunking)
                .is_none()
        {
            return Asked::Decided(PictureAsk::No);
        }
        self.gate_ask()
    }

    /// The answer for a document whose pictures are wanted, by what the
    /// run's chat model is known to do.
    fn gate_ask(&self) -> Asked {
        match self.steps.picture_gate_now() {
            PictureGateNow::Open => Asked::Decided(PictureAsk::Yes),
            PictureGateNow::Closed(reason) => {
                tracing::info!(
                    event = "index_pictures_not_requested",
                    reason,
                    "pictures not requested: the run cannot describe them"
                );
                Asked::Decided(PictureAsk::NotRead(reason))
            }
            PictureGateNow::Unknown => Asked::NeedsModel,
        }
    }

    /// Describes the pictures a connector's document links, renders the
    /// descriptions into its text and chunks it. A picture never fails the
    /// document or stops the run for a reason of its own; the document's
    /// text keeps the link's original form where nothing was written.
    async fn finish_connector(&mut self, job: ConnectorJob) -> Result<Preparation, Halt> {
        let ConnectorJob {
            mut shell,
            key,
            mime,
            chunking,
            batch,
            prompt,
        } = job;
        shell.pictures.linked = true;
        for (reason, count) in &batch.skipped {
            shell.pictures.skip(reason, u64::from(*count));
        }
        // Only what an image model takes goes to it: the bytes decide (the
        // declared type only has to be an image one).
        let mut pictures: Vec<Picture> = Vec::new();
        let mut prompts: Vec<String> = Vec::new();
        let mut owner: Vec<usize> = Vec::new();
        for (index, source) in batch.pictures.iter().enumerate() {
            let media_type = match elitea_doc_extract::image_for_vision("image/png", &source.bytes)
            {
                VisionImage::Supported(media_type) => media_type,
                VisionImage::Bmp => "image/bmp",
                // SVG, TIFF, ICO, bytes that are no picture.
                VisionImage::Corrupt | VisionImage::Unsupported | VisionImage::NotAnImage => {
                    shell.pictures.skip(SKIP_UNSUPPORTED, 1);
                    continue;
                }
            };
            pictures.push(Picture {
                bytes: source.bytes.clone(),
                media_type,
                ordinal: u32::try_from(index).unwrap_or(u32::MAX),
                text_offset: index,
                name: Some(source.name.clone()),
            });
            prompts.push(
                source
                    .prompt
                    .clone()
                    .or_else(|| prompt.clone())
                    .unwrap_or_else(|| elitea_doc_chunk::image_processing_prompt().to_owned()),
            );
            owner.push(index);
        }
        let prompt_refs: Vec<&str> = prompts.iter().map(String::as_str).collect();
        let (outcomes, described_pending) = self
            .describe_pictures(&prompt_refs, false, "", &pictures, &mut shell.pictures)
            .await?;
        // A picture the connector could not read for a transient reason
        // marks the document as one that has not been described.
        let pending = described_pending
            || batch
                .skipped
                .keys()
                .any(|reason| retryable_picture_skip(reason));
        let mut described: Vec<Option<String>> = vec![None; batch.pictures.len()];
        for (slot, outcome) in owner.iter().zip(outcomes) {
            if let PictureOutcome::Text(text) = outcome {
                described[*slot] = Some(if text.trim().is_empty() {
                    elitea_doc_chunk::EMPTY_DESCRIPTION.to_owned()
                } else {
                    text
                });
            }
        }
        tracing::info!(
            event = "index_connector_pictures",
            pictures = batch.pictures.len(),
            described = described.iter().flatten().count(),
            pending,
        );
        if pending {
            let prior = match shell.plan {
                Plan::Changed { prior_pending, .. } => prior_pending,
                Plan::New => 0,
            };
            shell.reference.version = pictures_pending(&shell.reference.version, prior + 1);
        }
        let refs: Vec<Option<&str>> = described.iter().map(Option::as_deref).collect();
        let text = batch.render(&refs);
        let chunked = tokio::task::spawn_blocking(move || {
            let options = ExtractOptions {
                html_to_markdown: true,
            };
            let plan = shell.plan;
            match elitea_doc_extract::extract_with(&mime, text.as_bytes(), &options) {
                Extracted::Text { text, .. } => {
                    let is_html = matches!(mime.as_str(), "text/html" | "application/xhtml+xml");
                    chunk_text(&text, &key, is_html, &chunking, shell)
                }
                Extracted::Unsupported => Preparation::Skipped(SKIP_UNSUPPORTED, plan),
                Extracted::Unreadable(_) => Preparation::Skipped(SKIP_UNREADABLE, plan),
            }
        })
        .await;
        Ok(chunked.unwrap_or(Preparation::Configuration(
            "the document preparation task stopped unexpectedly".to_owned(),
        )))
    }

    /// Describes the pictures embedded in one document, writes the
    /// descriptions into its text and chunks it (ADR-0030 I4 part C).
    ///
    /// A picture never stops the run for a reason of its own and never fails
    /// the document: one that cannot be described is counted in the
    /// summary's message and the text simply has no description at that
    /// place. Only the refusals of the credential, the project or the budget
    /// stop the run (the same as an image file). Each picture goes through
    /// the one description path an image file does, so they share the run's
    /// cache (a picture repeated in a document, or across documents, costs
    /// one call), its image budget, its refused-model breaker and its
    /// downscale.
    ///
    /// A picture skipped for a reason another run cures marks the document
    /// (see [`PICTURES_PENDING`]): it is written, and prepared again by the
    /// next run.
    async fn finish_embedded(&mut self, job: EmbeddedJob) -> Result<Preparation, Halt> {
        let EmbeddedJob {
            mut shell,
            key,
            chunking,
            plan,
            text,
            pictures,
            dropped,
        } = job;
        // What the extractor or the client dropped is final: no later run
        // would get those pictures either. Held with the document until it
        // is indexed.
        shell
            .pictures
            .skip(SKIP_TOO_MANY_PICTURES, u64::from(dropped.too_many));
        shell
            .pictures
            .skip(SKIP_TOO_LARGE, u64::from(dropped.too_large));
        shell
            .pictures
            .skip(SKIP_UNSUPPORTED, u64::from(dropped.unsupported));
        let (outcomes, pending) = self
            .describe_pictures(
                &vec![plan.prompt(); pictures.len()],
                plan.repeats_are_back_references(),
                &text,
                &pictures,
                &mut shell.pictures,
            )
            .await?;
        let inline: Vec<Inline<'_>> = pictures
            .iter()
            .zip(&outcomes)
            .filter_map(|(picture, outcome)| {
                let described = match outcome {
                    PictureOutcome::Text(description) => Described::Description(description),
                    PictureOutcome::Repeat => Described::Repeat,
                    PictureOutcome::Nothing => return None,
                };
                Some(Inline {
                    offset: picture.text_offset,
                    name: picture.name.as_deref(),
                    number: usize::try_from(picture.ordinal).map_or(usize::MAX, |n| n + 1),
                    described,
                })
            })
            .collect();
        tracing::info!(
            event = "index_embedded_pictures",
            pictures = pictures.len(),
            described = inline.len(),
            dropped = dropped.too_many + dropped.too_large + dropped.unsupported,
            pending,
        );
        if pending {
            // The consecutive run this is: one more than the stored version
            // had, given up on after `MAX_PENDING_RUNS`.
            let prior = match shell.plan {
                Plan::Changed { prior_pending, .. } => prior_pending,
                Plan::New => 0,
            };
            shell.reference.version = pictures_pending(&shell.reference.version, prior + 1);
        }
        let text = plan.inline(&text, &inline);
        let chunked =
            tokio::task::spawn_blocking(move || chunk_text(&text, &key, false, &chunking, shell))
                .await;
        Ok(chunked.unwrap_or(Preparation::Configuration(
            "the document preparation task stopped unexpectedly".to_owned(),
        )))
    }

    /// The model's description of each of `pictures`, in document order, and
    /// whether any was skipped for a reason another run cures. A picture
    /// that could not be described is counted in `tally` (the document's, not
    /// the run's) and is [`PictureOutcome::Nothing`].
    ///
    /// Distinct pictures (by the sha256 of their bytes and their prompt, all
    /// hashed in one blocking task) are described [`PICTURES_IN_FLIGHT`] at a time and
    /// their results applied in document order; the call cap and the image
    /// budget are charged atomically by the one description path. A picture
    /// whose bytes repeat an earlier one's takes that one's outcome, with
    /// no call of its own: a back-reference for DOCX, the same description
    /// otherwise. "Earlier" is the order the descriptions are WRITTEN in
    /// (by the offset each is placed at), not the order the parser numbered
    /// the pictures in, so a back-reference never points down the page.
    async fn describe_pictures(
        &mut self,
        prompts: &[&str],
        back_references: bool,
        text: &str,
        pictures: &[Picture],
        tally: &mut PictureTally,
    ) -> Result<(Vec<PictureOutcome>, bool), Halt> {
        let context = StepContext::new(self.runtime, self.run, self.model);
        let (env, alive) = (context.env(), context.alive());
        self.steps.load_chat(&env).await;
        let digests = image_fit::digests(
            pictures
                .iter()
                .map(|picture| picture.bytes.clone())
                .collect(),
        )
        .await;
        let offsets: Vec<usize> = pictures.iter().map(|picture| picture.text_offset).collect();
        let order = elitea_doc_chunk::written_order(text, &offsets);
        // The first picture of each distinct content, in written order, is
        // the one described.
        let mut first_of: BTreeMap<([u8; 32], &str), usize> = BTreeMap::new();
        let mut distinct: Vec<usize> = Vec::new();
        for index in order {
            if let Some(digest) = digests[index]
                && let std::collections::btree_map::Entry::Vacant(slot) =
                    first_of.entry((digest, prompts[index]))
            {
                slot.insert(index);
                distinct.push(index);
            }
        }
        let mut answers: BTreeMap<usize, Result<String, &'static str>> = BTreeMap::new();
        {
            let steps = &self.steps;
            let mut pending = FuturesOrdered::new();
            let mut queue = distinct.iter().copied();
            loop {
                while pending.len() < PICTURES_IN_FLIGHT {
                    let Some(index) = queue.next() else {
                        break;
                    };
                    pending.push_back(describe_picture(
                        index,
                        steps,
                        &pictures[index],
                        digests[index],
                        prompts[index],
                        &env,
                        &alive,
                    ));
                }
                let Some((index, answer)) = pending.next().await else {
                    break;
                };
                let answer = match answer {
                    Ok(description) => Ok(description),
                    Err(StepEnd::Skip(reason)) => Err(reason),
                    Err(StepEnd::Stop(reason)) => return Err(halt(&reason)),
                    Err(StepEnd::Liveness(failure)) => return Err(Halt::Failure(failure)),
                    // A picture's description is never chunks nor an embedding.
                    Err(StepEnd::Chunks(_) | StepEnd::EmbeddingFailed(_)) => Err(SKIP_MODEL_FAILED),
                };
                answers.insert(index, answer);
            }
        }
        let mut pending = false;
        let mut outcomes = Vec::with_capacity(pictures.len());
        for (index, digest) in digests.iter().enumerate() {
            let Some(first) = digest.and_then(|digest| first_of.get(&(digest, prompts[index])))
            else {
                tally.skip(SKIP_UNREADABLE, 1);
                outcomes.push(PictureOutcome::Nothing);
                continue;
            };
            match answers.get(first) {
                Some(Ok(_)) if *first != index && back_references => {
                    outcomes.push(PictureOutcome::Repeat);
                }
                Some(Ok(description)) => outcomes.push(PictureOutcome::Text(description.clone())),
                Some(Err(reason)) => {
                    tally.skip(reason, 1);
                    pending |= retryable_picture_skip(reason);
                    outcomes.push(PictureOutcome::Nothing);
                }
                None => {
                    tally.skip(SKIP_MODEL_FAILED, 1);
                    pending = true;
                    outcomes.push(PictureOutcome::Nothing);
                }
            }
        }
        Ok((outcomes, pending))
    }

    /// [`Self::fulfil_model_step`] for an image file's description.
    async fn fulfil_vision(
        &mut self,
        raw: &RawImage,
        decided: Plan,
        explicit_model: bool,
    ) -> Result<Option<Vec<Chunk>>, Halt> {
        let context = StepContext::new(self.runtime, self.run, self.model);
        let (env, alive) = (context.env(), context.alive());
        let end = self.steps.describe(raw, &env, &alive).await;
        self.settle(end, decided, explicit_model)
    }

    /// What a model step's end means for the run. `decided` is the
    /// document's plan and `explicit_model` whether its own config asked for
    /// the step (see [`skip_loses_document`]). A `too_large` that ends a
    /// model step is the embedding model's or the chat model's rejection,
    /// never a limit of this worker: lost work whatever the plan.
    fn settle(
        &mut self,
        end: StepEnd,
        decided: Plan,
        explicit_model: bool,
    ) -> Result<Option<Vec<Chunk>>, Halt> {
        match end {
            StepEnd::Chunks(chunks) if chunks.is_empty() => {
                self.tally.skip_document(
                    SKIP_EMPTY,
                    skip_loses_document(SKIP_EMPTY, decided, explicit_model),
                );
                Ok(None)
            }
            StepEnd::Chunks(chunks) => Ok(Some(chunks)),
            StepEnd::Skip(reason) => {
                self.tally.skip_document(
                    reason,
                    reason == SKIP_TOO_LARGE
                        || skip_loses_document(reason, decided, explicit_model),
                );
                Ok(None)
            }
            StepEnd::EmbeddingFailed(cause) => {
                self.note_embedding_failure(&cause, 0)?;
                Ok(None)
            }
            StepEnd::Stop(reason) => Err(halt(&reason)),
            StepEnd::Liveness(failure) => Err(Halt::Failure(failure)),
        }
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
            if let Plan::Changed { prior_chunks, .. } = decided
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
        retained_documents += plan.out_of_scope.0;
        retained_chunks += plan.out_of_scope.1;
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
        let partial = self.tally.failed_chunks > 0 || self.tally.lost_skips > 0;
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
        for (counts, label) in [
            (&tally.pictures_skipped, "Embedded pictures not described"),
            (
                &tally.pictures_not_read,
                "Embedded pictures not requested for documents",
            ),
            (&tally.linked_skipped, "Linked pictures not described"),
            (
                &tally.linked_not_read,
                "Linked pictures not requested for documents",
            ),
        ] {
            if counts.is_empty() {
                continue;
            }
            let reasons: Vec<String> = counts
                .iter()
                .map(|(reason, count)| format!("{count} {reason}"))
                .collect();
            let _ = write!(message, " {label}: {}.", reasons.join(", "));
        }
        if [
            &tally.skipped,
            &tally.pictures_skipped,
            &tally.linked_skipped,
        ]
        .iter()
        .any(|counts| counts.contains_key(SKIP_REASONING_EXHAUSTED_TOKENS))
        {
            message.push_str(
                " The chat model spent its whole max_tokens on reasoning for some pictures (reasoning_exhausted_tokens): raise max_tokens in the toolkit's LLM settings and re-run.",
            );
        }
        if tally.failed_chunks > 0 {
            let _ = write!(message, " {} chunks failed to embed.", tally.failed_chunks);
        }
        if self.parameters.clean_index && tally.indexed_documents == 0 {
            message.push_str(
                " clean_index replaced the index and nothing was indexed (the source lists no document the filters keep, or every one was skipped), so the index is now empty.",
            );
        }
        if plan.out_of_scope.0 > 0 {
            let _ = write!(
                message,
                " {} stored documents are outside the current file-type filter and were left unchanged; run with clean_index to drop them.",
                plan.out_of_scope.0
            );
        }
        if !self.ignored_loader_keys.is_empty() {
            let _ = write!(
                message,
                " Ignored chunking settings this worker does not read: {}.",
                self.ignored_loader_keys.join(", ")
            );
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
            unchanged_documents: Some(tally.unchanged_documents),
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

/// Stop and the deadline, as the run sees them between documents and calls.
fn run_liveness(run: &IndexRun<'_>) -> Result<(), IndexRunFailure> {
    if let Err(error) = run.lease.ensure_running() {
        return Err(match error.code() {
            ClaimLeaseErrorCode::Cancelled => IndexRunFailure::Cancelled,
            _ => IndexRunFailure::LeaseLost,
        });
    }
    let now = (run.now_unix_millis)();
    if now <= 0 || now >= run.deadline_unix_millis {
        return Err(IndexRunFailure::DeadlineExceeded);
    }
    Ok(())
}

pub(super) enum EmbedOutcome {
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

pub(super) fn classify_embedding_error(error: EmbeddingsError) -> EmbedOutcome {
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

pub(super) async fn embed_with_retry(
    embeddings: &EmbeddingsClient,
    context: &ClaimScopedEliteaContext,
    model: &str,
    inputs: &[String],
    backoff: std::time::Duration,
) -> Result<crate::transport::embeddings_facade::Embedded, EmbeddingsError> {
    match embed_gated(embeddings, context, model, inputs, backoff, &|_| {
        Ok::<(), std::convert::Infallible>(())
    })
    .await
    {
        Ok(embedded) => Ok(embedded),
        Err(Gated::Embeddings(error)) => Err(error),
        Err(Gated::Gate(never)) => match never {},
    }
}

/// Why a gated embeddings call ended.
pub(super) enum Gated<E> {
    /// The gate refused an attempt (or a wait).
    Gate(E),
    Embeddings(EmbeddingsError),
}

/// [`embed_with_retry`] with a gate asked before every attempt (`true`: the
/// attempt is about to be made, so charge it) and before every retry wait
/// (`false`: only liveness).
pub(super) async fn embed_gated<E>(
    embeddings: &EmbeddingsClient,
    context: &ClaimScopedEliteaContext,
    model: &str,
    inputs: &[String],
    backoff: std::time::Duration,
    gate: &(dyn Fn(bool) -> Result<(), E> + Sync),
) -> Result<crate::transport::embeddings_facade::Embedded, Gated<E>> {
    let mut attempt = 0;
    loop {
        gate(true).map_err(Gated::Gate)?;
        match embeddings.embed(context, model, inputs).await {
            Ok(embedded) => return Ok(embedded),
            Err(error) if error.retryable() && attempt + 1 < EMBEDDING_ATTEMPTS => {
                attempt += 1;
                gate(false).map_err(Gated::Gate)?;
                tokio::time::sleep(backoff * attempt).await;
            }
            Err(error) => return Err(Gated::Embeddings(error)),
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

/// The extension whose settings block governs an image whose key has no image
/// extension (a download named by its id): the bytes' own type when they are
/// one the step reads, else the declared media type's.
fn image_extension(class: VisionImage, declared: &str) -> String {
    let sniffed = match class {
        VisionImage::Supported("image/png") => ".png",
        VisionImage::Supported("image/jpeg") => ".jpg",
        VisionImage::Supported("image/gif") => ".gif",
        VisionImage::Supported("image/webp") => ".webp",
        VisionImage::Bmp => ".bmp",
        _ => "",
    };
    if !sniffed.is_empty() {
        return sniffed.to_owned();
    }
    let declared = declared
        .split(';')
        .next()
        .unwrap_or("")
        .trim()
        .to_ascii_lowercase();
    match declared.as_str() {
        "image/png" | "image/x-png" => ".png",
        "image/jpeg" | "image/jpg" | "image/pjpeg" => ".jpg",
        "image/gif" => ".gif",
        "image/webp" => ".webp",
        "image/bmp" | "image/x-bmp" | "image/x-ms-bmp" => ".bmp",
        "image/svg+xml" | "image/svg" => ".svg",
        _ => "",
    }
    .to_owned()
}

/// `chunking` with the vision step forced on, with the connector's prompt,
/// for the extension of `key` when the connector's step names it (the SDK's
/// `bins_with_llm`: the toolkit's own switch, over the extension's
/// `use_llm`).
pub(super) fn binary_overlay(
    chunking: &ChunkingConfig,
    step: &PictureStep,
    key: &str,
) -> Option<ChunkingConfig> {
    let PictureStep::Binary { extensions, prompt } = step else {
        return None;
    };
    let extension = elitea_doc_chunk::extension_of(key);
    if !extensions.contains(&extension.as_str()) {
        return None;
    }
    let mut config = chunking.clone();
    let own = config.by_extension.entry(extension).or_default();
    own.use_llm = Some(true);
    own.use_default_prompt = Some(false);
    own.prompt = Some(prompt.clone());
    Some(config)
}

/// The chunking configs one document can be parsed with.
struct Chunkings {
    base: Arc<ChunkingConfig>,
    dependent: Arc<ChunkingConfig>,
}

/// The config of a dependent document: the SDK applies `chunking_tool` to
/// the base document only; an attachment is parsed by its own extension.
pub(super) fn dependent_chunking(chunking: &ChunkingConfig) -> ChunkingConfig {
    let mut dependent = chunking.clone();
    dependent.base.chunker = None;
    dependent
}

/// Fetch, extract and chunk one document. Runs as its own task; the CPU
/// work (extraction, tokenising) is on a blocking thread. A PDF or Office
/// document is read by the extraction sidecar when the run has one
/// (`extractor`), and skipped `unsupported_type` otherwise.
#[allow(clippy::too_many_lines)] // One linear pass: fetch, extract, chunk.
async fn prepare(
    source: Arc<IndexSource>,
    chunkings: Chunkings,
    reference: DocumentRef,
    plan: Plan,
    limits: IndexRunLimits,
    extractor: Option<Arc<ExtractorClient>>,
    pictures: PictureAsk,
) -> Prepare {
    let max_bytes = limits.max_document_bytes;
    if reference.size > max_bytes {
        return Preparation::Skipped(SKIP_TOO_LARGE, plan).into();
    }
    let document = match source.fetch(&reference.key).await {
        Ok(document) => document,
        Err(SourceError::NotFound(_)) => {
            // Listed, then gone: the next run deletes it.
            return Preparation::Skipped(SKIP_FETCH_FAILED, plan).into();
        }
        Err(error @ SourceError::RateLimited(_)) => {
            tracing::warn!(event = "index_document_rate_limited", error = %error);
            return Preparation::Skipped(SKIP_RATE_LIMITED, plan).into();
        }
        Err(error) => {
            tracing::warn!(event = "index_document_fetch_failed", error = %error);
            return Preparation::Skipped(SKIP_FETCH_FAILED, plan).into();
        }
    };
    if document.bytes.len() as u64 > max_bytes {
        return Preparation::Skipped(SKIP_TOO_LARGE, plan).into();
    }
    let key = reference.key.clone();
    let mime = reference.mime.clone();
    let mut metadata = document.metadata;
    // What a connector's picture pass needs from the fetch travels in the
    // document and goes no further: off before anything is stored.
    let picture_context = metadata.remove(elitea_connectors::pictures::CONTEXT_KEY);
    // The SDK applies `chunking_tool` to the base document only; a dependent
    // document (an attachment) is parsed by its own extension with the
    // per-extension settings.
    let chunking = if metadata.contains_key("parent_key") {
        chunkings.dependent
    } else {
        chunkings.base
    };
    // A connector's own picture setting (ADR-0030 I4, PR-C2): `bins_with_llm`
    // turns the vision step on for the attachment, with the toolkit's prompt.
    let step = source.picture_step(&reference);
    let chunking = match step
        .as_ref()
        .and_then(|step| binary_overlay(&chunking, step, &key))
    {
        Some(overlaid) => Arc::new(overlaid),
        None => chunking,
    };
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
    let class = elitea_doc_extract::image_for_vision(&mime, &bytes);
    // A repository file's `commit_hash` hashes its whole text: on a blocking
    // thread, never on this task (the text routes below do it in their own
    // blocking step).
    let mut hash_commit = source.hashes_commit() && !metadata.contains_key("commit_hash");
    if hash_commit
        && (!matches!(class, VisionImage::NotAnImage)
            || elitea_doc_extract::is_document_type(&mime))
    {
        hash_commit = false;
        match hashed_off_executor(bytes).await {
            Ok((returned, hash)) => {
                bytes = returned;
                metadata.insert("commit_hash".to_owned(), Value::String(hash));
            }
            Err(_) => {
                return Preparation::Configuration(
                    "the document preparation task stopped unexpectedly".to_owned(),
                )
                .into();
            }
        }
    }
    // A standalone image is described by the chat model (ADR-0030 I4 part B),
    // never extracted as text.
    if !matches!(class, VisionImage::NotAnImage) {
        // Whether the step is on comes FIRST: a vision-off image is
        // `unsupported_type` whatever its bytes are.
        let by_name = elitea_doc_chunk::extension_of(&key);
        let settings_extension = if elitea_doc_chunk::is_image_extension(&by_name) {
            by_name
        } else {
            image_extension(class, &mime)
        };
        let ImagePlan::Vision(vision) =
            elitea_doc_chunk::plan_image(&settings_extension, &chunking)
        else {
            return Preparation::Skipped(SKIP_UNSUPPORTED, plan).into();
        };
        let kind = match class {
            VisionImage::Supported(media_type) => ImageKind::Native(media_type),
            VisionImage::Bmp => ImageKind::Bmp,
            VisionImage::Corrupt => return Preparation::Skipped(SKIP_UNREADABLE, plan).into(),
            // SVG, TIFF, ...: not converted by this build.
            VisionImage::Unsupported | VisionImage::NotAnImage => {
                return Preparation::Skipped(SKIP_UNSUPPORTED, plan).into();
            }
        };
        // Fitting (decode, resize) waits for the run's loop: the chat model,
        // the cache and the image budget are checked first, and only a call
        // that will really be made pays for the decode.
        return Preparation::Vision(
            Box::new(Prepared {
                reference,
                plan,
                metadata,
                acl,
                chunks: Vec::new(),
                explicit_model: chunking.explicitly_needs_model_for(&settings_extension),
                pictures: PictureTally::default(),
            }),
            RawImage {
                bytes: Bytes::from(bytes),
                kind,
                plan: vision,
            },
        )
        .into();
    }
    let mut picture_tally = PictureTally::default();
    // The pictures the text of a connector's document links: read through
    // the connector, described by the run's loop.
    if let Some(PictureStep::Inline { prompt }) = step
        && source.may_have_pictures(&key, &bytes, picture_context.as_ref())
    {
        // These pictures are reported as the document's LINKED ones.
        picture_tally.linked = true;
        match pictures.clone().resolve().await {
            Ok(true) => {
                if let Some(batch) = source
                    .pictures(&key, &bytes, picture_context.as_ref())
                    .await
                {
                    let explicit_model =
                        chunking.explicitly_needs_model_for(&elitea_doc_chunk::extension_of(&key));
                    return Prepare::Connector(Box::new(ConnectorJob {
                        shell: Shell {
                            reference,
                            plan,
                            metadata,
                            acl,
                            explicit_model,
                            pictures: picture_tally,
                        },
                        key,
                        mime,
                        chunking,
                        batch,
                        prompt,
                    }));
                }
            }
            Ok(false) => {}
            Err(reason) => picture_tally.not_read = Some(reason),
        }
    }
    let plan_of_run = plan;
    // A document format (PDF, Office, e-mail) is the sidecar's to read; this
    // process extracts text and HTML itself, on the blocking thread below.
    // A PDF, DOCX or PPTX whose `use_llm` is on also asks the sidecar for its
    // embedded pictures (ADR-0030 I4 part C); every other document, and these
    // with `use_llm` off (the SDK's default for documents), ask for none and
    // are read exactly as before.
    let is_document = elitea_doc_extract::is_document_type(&mime);
    // Planned by the key's extension, else by the media type the source
    // delivered it as (a key without an extension).
    let embedded_plan = if is_document {
        elitea_doc_chunk::plan_embedded_for(&key, &mime, &chunking)
    } else {
        None
    };
    // Only when the run can still describe a picture. The decision may
    // wait for the run's chat model to load; the document was fetched
    // meanwhile, so no slot sat idle for the load.
    let wanted = if embedded_plan.is_some() {
        match pictures.resolve().await {
            Ok(wanted) => wanted,
            Err(reason) => {
                picture_tally.not_read = Some(reason);
                false
            }
        }
    } else {
        false
    };
    let sidecar_text = if is_document {
        let sent = std::mem::take(&mut bytes);
        let asked = embedded_plan
            .as_ref()
            .filter(|_| wanted)
            .map(|_| PICTURE_REQUEST);
        match sidecar_text(extractor, &key, &mime, sent, limits, asked).await {
            Ok(extracted) => Some(extracted),
            Err(label) => return Preparation::Skipped(label, plan).into(),
        }
    } else {
        None
    };
    let explicit_model = chunking.explicitly_needs_model_for(&elitea_doc_chunk::extension_of(&key));
    let sidecar_text = match (sidecar_text, embedded_plan) {
        // Pictures to describe, or pictures found and dropped to count: the
        // run's loop finishes the document.
        (Some(extracted), Some(plan))
            if !extracted.pictures.is_empty()
                || extracted.dropped != PicturesDropped::default() =>
        {
            return Prepare::Embedded(Box::new(EmbeddedJob {
                shell: Shell {
                    reference,
                    plan: plan_of_run,
                    metadata,
                    acl,
                    explicit_model,
                    pictures: picture_tally,
                },
                key,
                chunking,
                plan,
                text: extracted.text,
                pictures: extracted.pictures,
                dropped: extracted.dropped,
            }));
        }
        (Some(extracted), _) => Some(extracted.text),
        (None, _) => None,
    };
    let chunked = tokio::task::spawn_blocking(move || {
        if hash_commit {
            metadata.insert(
                "commit_hash".to_owned(),
                Value::String(super::source::commit_hash(&bytes)),
            );
        }
        let is_html = matches!(mime.as_str(), "text/html" | "application/xhtml+xml");
        let options = ExtractOptions {
            html_to_markdown: true,
        };
        let text = match sidecar_text {
            Some(text) => text,
            None => match elitea_doc_extract::extract_with(&mime, &bytes, &options) {
                Extracted::Text { text, .. } => text,
                Extracted::Unsupported => return Preparation::Skipped(SKIP_UNSUPPORTED, plan),
                Extracted::Unreadable(_) => return Preparation::Skipped(SKIP_UNREADABLE, plan),
            },
        };
        chunk_text(
            &text,
            &key,
            is_html,
            &chunking,
            Shell {
                reference,
                plan: plan_of_run,
                metadata,
                acl,
                explicit_model,
                pictures: picture_tally,
            },
        )
    })
    .await;
    chunked
        .unwrap_or(Preparation::Configuration(
            "the document preparation task stopped unexpectedly".to_owned(),
        ))
        .into()
}

/// One embedded picture's description through the one path an image file
/// takes, tagged with the picture's index.
async fn describe_picture(
    index: usize,
    steps: &ModelSteps,
    picture: &Picture,
    digest: Option<[u8; 32]>,
    prompt: &str,
    env: &StepEnv<'_>,
    alive: &(dyn Fn() -> Result<(), IndexRunFailure> + Sync),
) -> (usize, Result<String, StepEnd>) {
    let kind = if picture.media_type == "image/bmp" {
        ImageKind::Bmp
    } else {
        ImageKind::Native(picture.media_type)
    };
    (
        index,
        steps
            .describe_one(&picture.bytes, kind, prompt, digest, env, alive)
            .await,
    )
}

/// A document's text chunked, or what stands in the way: the chunks are
/// ready, a model step has to fulfil them, or the document is skipped.
fn chunk_text(
    text: &str,
    key: &str,
    is_html: bool,
    chunking: &ChunkingConfig,
    shell: Shell,
) -> Preparation {
    if text.trim().is_empty() {
        return Preparation::Skipped(SKIP_EMPTY, shell.plan);
    }
    let options = ChunkOptions {
        html_converted: is_html,
    };
    let Shell {
        reference,
        plan,
        metadata,
        acl,
        explicit_model,
        pictures,
    } = shell;
    let prepared = |chunks: Vec<Chunk>| {
        Box::new(Prepared {
            reference,
            plan,
            metadata,
            acl,
            chunks,
            explicit_model,
            pictures,
        })
    };
    match elitea_doc_chunk::plan_with(text, key, chunking, options) {
        Ok(ChunkPlan::Chunks(chunks)) if chunks.is_empty() => {
            Preparation::Skipped(SKIP_EMPTY, plan)
        }
        Ok(ChunkPlan::Chunks(chunks)) => Preparation::Ready(prepared(chunks)),
        Ok(ChunkPlan::Proposal(proposal)) if proposal.windows().is_empty() => {
            Preparation::Skipped(SKIP_EMPTY, plan)
        }
        Ok(ChunkPlan::Statistical(statistical)) if statistical.splits().is_empty() => {
            Preparation::Skipped(SKIP_EMPTY, plan)
        }
        Ok(model_step @ (ChunkPlan::Proposal(_) | ChunkPlan::Statistical(_))) => {
            Preparation::Model(prepared(Vec::new()), model_step)
        }
        // `plan_with` returns the model steps as plans, so this is a
        // defensive arm: a chunker that still needs a model skips.
        Err(ChunkError::RequiresModel { .. }) => Preparation::Skipped(SKIP_NEEDS_MODEL, plan),
        Err(
            error @ (ChunkError::UnknownChunker(_)
            | ChunkError::Tokenizer(_)
            | ChunkError::InvalidModelStep(_)),
        ) => Preparation::Configuration(format!(
            "the chunking configuration cannot be used: {error}"
        )),
    }
}

/// The bytes of a repository file with the SDK's `commit_hash` of their
/// text, computed on a blocking thread.
///
/// # Errors
///
/// The blocking thread stopped.
pub(super) async fn hashed_off_executor(
    bytes: Vec<u8>,
) -> Result<(Vec<u8>, String), tokio::task::JoinError> {
    tokio::task::spawn_blocking(move || {
        let hash = super::source::commit_hash(&bytes);
        (bytes, hash)
    })
    .await
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
    pictures: Option<PictureRequest>,
) -> Result<ExtractedDocument, &'static str> {
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
            pictures,
        )
        .await;
    match extracted {
        Ok(Extraction::Text(document)) => Ok(document),
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
