//! `elitea.extract.v1.DocumentExtractService`.
//!
//! What a request goes through, in order:
//!
//! 0. the peer: the connecting process's credentials (`SO_PEERCRED`) must
//!    name one of the configured worker uids, else `PERMISSION_DENIED`
//!    before anything is read (the pod-local Unix-socket sidecar exception
//!    to the gRPC-over-mTLS rule, AGENTS.md);
//! 1. the header: a media type this build extracts, else the document is
//!    skipped `UNSUPPORTED`; its limits, each lowered to the process's
//!    [`Caps`]; a declared length over the input cap is `TOO_LARGE` before a
//!    byte is read;
//! 2. the chunks, counted as they arrive: the input cap is enforced on the
//!    running total, so an over-sized document is refused at the cap and
//!    never held whole; a stream that goes quiet for longer than the idle
//!    limit is `DEADLINE_EXCEEDED`;
//! 3. a slot: at most `concurrency` extractions run at once, and a request
//!    waits at most [`Capacity::slot_wait`] for one (`RESOURCE_EXHAUSTED`
//!    after). The slot wait is a separate, generous bound: a client that
//!    keeps no more calls in flight than the advertised concurrency never
//!    waits at all, and one that does is queued, not refused, for as long
//!    as its own deadline allows;
//! 4. the extraction, on a dedicated large-stack thread, under the timeout.
//!    A thread that overruns cannot be killed; the request is answered
//!    `TIMEOUT` and the thread keeps its slot until it ends. When every slot
//!    is held by an overrun the process is useless and EXITS (after a short
//!    grace so the last answers reach the worker): the container restarts
//!    it, and the worker records `extract_failed` for whatever was in
//!    flight and reconnects.
//! 5. the text, unless it is over the output cap (`OUTPUT_TOO_LARGE`) or
//!    empty (`EMPTY`).
//!
//! Every skip is a statement about the document and travels as a response.
//! A malformed stream, no capacity and an extractor failure are gRPC
//! statuses, which the worker records as `extract_failed`.

use std::collections::BTreeSet;
use std::sync::Arc;
use std::sync::atomic::{AtomicU8, AtomicUsize, Ordering};
use std::time::Duration;

use elitea_doc_extract::{
    DocumentError, DocumentLimits, document_types, extract_document, is_document_type,
};
use tokio::sync::{OwnedSemaphorePermit, Semaphore};
use tonic::transport::server::UdsConnectInfo;
use tonic::{Request, Response, Status, Streaming};

use crate::pb::document_extract_service_server::DocumentExtractService;
use crate::pb::{
    CapabilitiesRequest, CapabilitiesResponse, ExtractHeader, ExtractLimits, ExtractRequest,
    ExtractResponse, ExtractSkip, ExtractedText, SkipReason, extract_request, extract_response,
};

/// The grace between the decision to exit and the exit itself: long enough
/// for the answers already written to reach the worker.
const EXIT_GRACE: Duration = Duration::from_millis(250);

/// The exit code of a process that gave up its slots to overruns.
pub const EXIT_SLOTS_EXHAUSTED: i32 = 75;

/// The caps a request's limits are lowered to.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Caps {
    pub max_input_bytes: u64,
    pub max_pages: usize,
    pub max_output_bytes: u64,
    pub timeout: Duration,
}

impl Default for Caps {
    fn default() -> Self {
        let defaults = DocumentLimits::default();
        Self {
            max_input_bytes: 50 << 20,
            max_pages: defaults.max_pages,
            max_output_bytes: 20 << 20,
            timeout: defaults.timeout,
        }
    }
}

impl Caps {
    /// The limits of one request: each field of `asked` lowered to this
    /// cap, 0 meaning the cap itself.
    #[must_use]
    pub fn apply(&self, asked: Option<&ExtractLimits>) -> Caps {
        let asked = asked.copied().unwrap_or_default();
        let bound = |value: u64, cap: u64| if value == 0 { cap } else { value.min(cap) };
        Caps {
            max_input_bytes: bound(asked.max_input_bytes, self.max_input_bytes),
            max_pages: usize::try_from(bound(
                u64::from(asked.max_pages),
                u64::try_from(self.max_pages).unwrap_or(u64::MAX),
            ))
            .unwrap_or(self.max_pages),
            max_output_bytes: bound(asked.max_output_bytes, self.max_output_bytes),
            timeout: Duration::from_millis(bound(
                u64::from(asked.timeout_millis),
                u64::try_from(self.timeout.as_millis()).unwrap_or(u64::MAX),
            )),
        }
    }

    fn wire(&self) -> ExtractLimits {
        ExtractLimits {
            max_input_bytes: self.max_input_bytes,
            max_pages: u32::try_from(self.max_pages).unwrap_or(u32::MAX),
            max_output_bytes: self.max_output_bytes,
            timeout_millis: millis(self.timeout),
        }
    }
}

/// How much runs at once and how long a request waits.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Capacity {
    /// Extractions at once (at least 1).
    pub concurrency: usize,
    /// The longest wait for a slot before `RESOURCE_EXHAUSTED`. Separate
    /// from the extraction timeout on purpose: queueing is not a timeout.
    pub slot_wait: Duration,
    /// The longest wait for a document's next chunk.
    pub stream_idle: Duration,
}

impl Default for Capacity {
    fn default() -> Self {
        Self {
            concurrency: 4,
            slot_wait: Duration::from_mins(10),
            stream_idle: Duration::from_secs(30),
        }
    }
}

fn millis(duration: Duration) -> u32 {
    u32::try_from(duration.as_millis()).unwrap_or(u32::MAX)
}

/// What happens when every slot is held by an overrun.
pub trait Exhausted: Send + Sync + 'static {
    /// Called once, from the request that observed it.
    fn slots_exhausted(&self);
}

/// Production: log, wait a short grace (250 ms, for the answers already
/// written to reach the worker), exit [`EXIT_SLOTS_EXHAUSTED`].
pub struct ExitProcess;

impl Exhausted for ExitProcess {
    fn slots_exhausted(&self) {
        tracing::error!(
            event = "extractor_slots_exhausted",
            "every extraction slot is held by a timed-out extraction; exiting so the \
             container restarts this process"
        );
        tokio::spawn(async {
            tokio::time::sleep(EXIT_GRACE).await;
            std::process::exit(EXIT_SLOTS_EXHAUSTED);
        });
    }
}

/// One extraction thread's standing against the overrun counter.
///
/// Two parties touch it from different threads: the request's timeout
/// (which may fire while the thread still runs, or just as it ended) and
/// the thread itself (which ends before or after that). The state cell
/// orders them: the timeout counts the thread as an overrun only when it
/// moved the state from `RUNNING` to `OVERRUN`, and the thread takes the
/// overrun back only when it found `OVERRUN` there. The counter is raised
/// BEFORE the state is claimed and lowered after it is read, so every
/// decrement is preceded by its own increment and the count can neither
/// underflow nor keep a finished thread.
pub struct OverrunLedger {
    state: AtomicU8,
    overruns: Arc<AtomicUsize>,
}

const RUNNING: u8 = 0;
const FINISHED: u8 = 1;
const OVERRUN: u8 = 2;

/// The thread had already ended when its timeout fired: its result is
/// there to be used, and nothing was counted.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct AlreadyFinished;

impl OverrunLedger {
    /// A ledger for one thread, against the process's `overruns` counter.
    #[must_use]
    pub fn new(overruns: Arc<AtomicUsize>) -> Arc<Self> {
        Arc::new(Self {
            state: AtomicU8::new(RUNNING),
            overruns,
        })
    }

    /// The thread ended. When its timeout had counted it as an overrun,
    /// the overrun is over and the counter goes down by one.
    pub fn finished(&self) {
        if self.state.swap(FINISHED, Ordering::SeqCst) == OVERRUN {
            self.overruns.fetch_sub(1, Ordering::SeqCst);
        }
    }

    /// The timeout fired. `Ok(total)` when the thread still runs and is now
    /// counted, with the new number of overruns; `Err` when it had already
    /// ended, in which case nothing is counted.
    ///
    /// # Errors
    ///
    /// [`AlreadyFinished`].
    pub fn timed_out(&self) -> Result<usize, AlreadyFinished> {
        // Raised first, so a thread that ends between this and the claim
        // below and reads OVERRUN finds something to take back.
        let raised = self.overruns.fetch_add(1, Ordering::SeqCst) + 1;
        let claimed = self
            .state
            .compare_exchange(RUNNING, OVERRUN, Ordering::SeqCst, Ordering::SeqCst)
            .is_ok();
        if claimed {
            return Ok(raised);
        }
        self.overruns.fetch_sub(1, Ordering::SeqCst);
        Err(AlreadyFinished)
    }
}

/// Test seams on the two sides of the timeout race. Compiled only for the
/// crate's own tests.
#[cfg(test)]
#[derive(Default, Clone)]
struct Hooks {
    /// Runs on the extraction thread before the extraction starts.
    before_extract: Option<Arc<dyn Fn() + Send + Sync>>,
    /// Runs on the request's task after its timeout fired and before the
    /// overrun is accounted.
    before_timeout_accounting: Option<Arc<dyn Fn() + Send + Sync>>,
}

/// The service.
pub struct Extractor {
    caps: Caps,
    capacity: Capacity,
    /// The peer uids served; every other peer is `PERMISSION_DENIED`.
    peer_uids: BTreeSet<u32>,
    slots: Arc<Semaphore>,
    /// Extractions that outran their timeout and still run.
    overruns: Arc<AtomicUsize>,
    exhausted: Arc<dyn Exhausted>,
    #[cfg(test)]
    hooks: Hooks,
}

impl Extractor {
    /// A service with `caps`, `capacity` and `peer_uids` (the uids whose
    /// connections are served); `exhausted` is told when every slot is held
    /// by an overrun.
    #[must_use]
    pub fn new(
        caps: Caps,
        capacity: Capacity,
        peer_uids: BTreeSet<u32>,
        exhausted: Arc<dyn Exhausted>,
    ) -> Self {
        let capacity = Capacity {
            concurrency: capacity.concurrency.max(1),
            ..capacity
        };
        Self {
            caps,
            capacity,
            peer_uids,
            slots: Arc::new(Semaphore::new(capacity.concurrency)),
            overruns: Arc::new(AtomicUsize::new(0)),
            exhausted,
            #[cfg(test)]
            hooks: Hooks::default(),
        }
    }

    /// Extractions that outran their timeout and still run.
    #[must_use]
    pub fn overruns(&self) -> usize {
        self.overruns.load(Ordering::SeqCst)
    }

    /// Takes one extraction slot and holds it until the permit is dropped,
    /// exactly as an overrun thread would. A seam for the tests of the slot
    /// wait; nothing in the binary calls it.
    #[doc(hidden)]
    pub async fn hold_slot(&self) -> OwnedSemaphorePermit {
        Arc::clone(&self.slots)
            .acquire_owned()
            .await
            .unwrap_or_else(|_| unreachable!("the slots are never closed"))
    }

    /// The peer check. A request from a uid outside `peer_uids`, or one
    /// whose credentials the transport did not deliver, is refused.
    fn authorize<T>(&self, request: &Request<T>) -> Result<(), Status> {
        let uid = request
            .extensions()
            .get::<UdsConnectInfo>()
            .and_then(|info| info.peer_cred.as_ref())
            .map(tokio::net::unix::UCred::uid);
        match uid {
            Some(uid) if self.peer_uids.contains(&uid) => Ok(()),
            Some(uid) => {
                tracing::warn!(event = "extractor_peer_refused", peer_uid = uid);
                Err(Status::permission_denied(format!(
                    "peer uid {uid} is not a configured worker uid"
                )))
            }
            None => {
                tracing::warn!(event = "extractor_peer_refused", peer_uid = "unknown");
                Err(Status::permission_denied(
                    "the peer's credentials are unknown; only a Unix-socket peer is served",
                ))
            }
        }
    }

    async fn read_document(
        &self,
        stream: &mut Streaming<ExtractRequest>,
        limits: &Caps,
        declared: u64,
    ) -> Result<Result<Vec<u8>, ExtractSkip>, Status> {
        if declared > limits.max_input_bytes {
            return Ok(Err(skip(
                SkipReason::TooLarge,
                format!(
                    "the document declares {declared} bytes, over the {}-byte limit",
                    limits.max_input_bytes
                ),
            )));
        }
        // Reserve what was declared, bounded by the cap: a lying header
        // cannot make the process allocate more than it would accept.
        let reserve = usize::try_from(declared.min(limits.max_input_bytes)).unwrap_or(0);
        let mut bytes: Vec<u8> = Vec::with_capacity(reserve);
        loop {
            let next = tokio::time::timeout(self.capacity.stream_idle, stream.message())
                .await
                .map_err(|_| Status::deadline_exceeded("no chunk arrived within the idle limit"))?
                .map_err(|status| {
                    Status::new(status.code(), format!("reading the stream: {status}"))
                })?;
            let Some(message) = next else {
                return Ok(Ok(bytes));
            };
            match message.message {
                Some(extract_request::Message::Chunk(chunk)) => {
                    let total = bytes.len() as u64 + chunk.len() as u64;
                    if total > limits.max_input_bytes {
                        return Ok(Err(skip(
                            SkipReason::TooLarge,
                            format!(
                                "the document is over the {}-byte limit",
                                limits.max_input_bytes
                            ),
                        )));
                    }
                    bytes.extend_from_slice(&chunk);
                }
                Some(extract_request::Message::Header(_)) => {
                    return Err(Status::invalid_argument(
                        "a second header arrived; one document per call",
                    ));
                }
                None => return Err(Status::invalid_argument("an empty message arrived")),
            }
        }
    }

    async fn extract_bytes(
        &self,
        header: &ExtractHeader,
        limits: Caps,
        bytes: Vec<u8>,
    ) -> Result<Result<ExtractedText, ExtractSkip>, Status> {
        let permit = tokio::time::timeout(
            self.capacity.slot_wait,
            Arc::clone(&self.slots).acquire_owned(),
        )
        .await
        .map_err(|_| {
            Status::resource_exhausted(format!(
                "no extraction slot came free within {} ms",
                self.capacity.slot_wait.as_millis()
            ))
        })?
        .map_err(|_| Status::internal("the extraction slots are closed"))?;
        let mime = header.content_type.clone();
        let document_limits = DocumentLimits {
            max_pages: limits.max_pages,
            max_content_bytes: usize::try_from(limits.max_input_bytes.max(limits.max_output_bytes))
                .unwrap_or(usize::MAX),
            timeout: limits.timeout,
        };
        let ledger = OverrunLedger::new(Arc::clone(&self.overruns));
        let thread_ledger = Arc::clone(&ledger);
        #[cfg(test)]
        let before_extract = self.hooks.before_extract.clone();
        let mut work = tokio::task::spawn_blocking(move || {
            #[cfg(test)]
            if let Some(hook) = before_extract {
                hook();
            }
            // The bytes move into the extractor: no copy is made here.
            let result = extract_document(&mime, bytes, &document_limits);
            thread_ledger.finished();
            // The slot is held until the extraction really ended.
            drop(permit);
            result
        });
        let result = match tokio::time::timeout(limits.timeout, &mut work).await {
            Ok(Ok(result)) => result,
            Ok(Err(join)) => {
                return Err(Status::internal(format!(
                    "the extraction task ended: {join}"
                )));
            }
            Err(_) => {
                #[cfg(test)]
                if let Some(hook) = &self.hooks.before_timeout_accounting {
                    hook();
                }
                match ledger.timed_out() {
                    Ok(running) => {
                        tracing::warn!(
                            event = "extractor_timeout",
                            filename = %header.filename,
                            content_type = %header.content_type,
                            overruns = running,
                            "an extraction outran its timeout; its thread keeps its slot until it ends"
                        );
                        if running >= self.capacity.concurrency {
                            self.exhausted.slots_exhausted();
                        }
                        return Ok(Err(skip(
                            SkipReason::Timeout,
                            format!(
                                "the extraction took longer than {} ms",
                                limits.timeout.as_millis()
                            ),
                        )));
                    }
                    // The thread ended just as the timeout fired: its answer
                    // is a moment away and is the truth about the document.
                    Err(AlreadyFinished) => work.await.map_err(|join| {
                        Status::internal(format!("the extraction task ended: {join}"))
                    })?,
                }
            }
        };
        Ok(match result {
            Ok(text) => {
                if text.text.len() as u64 > limits.max_output_bytes {
                    Err(skip(
                        SkipReason::OutputTooLarge,
                        format!(
                            "the text is {} bytes, over the {}-byte limit",
                            text.text.len(),
                            limits.max_output_bytes
                        ),
                    ))
                } else {
                    Ok(ExtractedText {
                        text: text.text,
                        extractor: text.extractor.to_owned(),
                    })
                }
            }
            Err(DocumentError::Extractor(detail)) => {
                return Err(Status::internal(format!(
                    "the document extractor failed: {detail}"
                )));
            }
            Err(error) => Err(skip_for(&error)),
        })
    }
}

fn skip(reason: SkipReason, detail: String) -> ExtractSkip {
    ExtractSkip {
        reason: reason as i32,
        detail,
    }
}

/// The wire reason of a typed extraction error.
fn skip_for(error: &DocumentError) -> ExtractSkip {
    let reason = match error {
        DocumentError::Unsupported(_) => SkipReason::Unsupported,
        DocumentError::Encrypted(_) => SkipReason::Encrypted,
        DocumentError::TooManyPages(_) => SkipReason::TooManyPages,
        DocumentError::TooLarge(_) => SkipReason::TooLarge,
        DocumentError::Timeout(_) => SkipReason::Timeout,
        DocumentError::Corrupt(_) => SkipReason::Corrupt,
        DocumentError::NoText => SkipReason::Empty,
        // Handled by the caller as a status; kept total for the compiler.
        DocumentError::Extractor(_) => SkipReason::Unspecified,
    };
    skip(reason, error.to_string())
}

#[tonic::async_trait]
impl DocumentExtractService for Extractor {
    async fn extract(
        &self,
        request: Request<Streaming<ExtractRequest>>,
    ) -> Result<Response<ExtractResponse>, Status> {
        self.authorize(&request)?;
        let mut stream = request.into_inner();
        let first = tokio::time::timeout(self.capacity.stream_idle, stream.message())
            .await
            .map_err(|_| Status::deadline_exceeded("no header arrived within the idle limit"))?
            .map_err(|status| Status::new(status.code(), format!("reading the stream: {status}")))?
            .ok_or_else(|| Status::invalid_argument("the stream ended before a header"))?;
        let Some(extract_request::Message::Header(header)) = first.message else {
            return Err(Status::invalid_argument(
                "the first message must be the header",
            ));
        };
        if header.content_type.is_empty() {
            return Err(Status::invalid_argument("content_type is required"));
        }
        let limits = self.caps.apply(header.limits.as_ref());
        if !is_document_type(&header.content_type) {
            // Not read at all: the stream is left to the client to finish.
            return Ok(Response::new(outcome(Err(skip(
                SkipReason::Unsupported,
                format!("no extractor for {}", header.content_type),
            )))));
        }
        let bytes = match self
            .read_document(&mut stream, &limits, header.content_length)
            .await?
        {
            Ok(bytes) => bytes,
            Err(skip) => return Ok(Response::new(outcome(Err(skip)))),
        };
        let started = std::time::Instant::now();
        let result = self.extract_bytes(&header, limits, bytes).await?;
        match &result {
            Ok(text) => tracing::info!(
                event = "extractor_document",
                filename = %header.filename,
                content_type = %header.content_type,
                text_bytes = text.text.len(),
                elapsed_ms = u64::try_from(started.elapsed().as_millis()).unwrap_or(u64::MAX),
                "extracted"
            ),
            Err(skip) => tracing::info!(
                event = "extractor_skip",
                filename = %header.filename,
                content_type = %header.content_type,
                reason = ?SkipReason::try_from(skip.reason).unwrap_or_default(),
                detail = %skip.detail,
                "skipped"
            ),
        }
        Ok(Response::new(outcome(result)))
    }

    async fn capabilities(
        &self,
        request: Request<CapabilitiesRequest>,
    ) -> Result<Response<CapabilitiesResponse>, Status> {
        self.authorize(&request)?;
        Ok(Response::new(CapabilitiesResponse {
            content_types: document_types()
                .iter()
                .map(|mime| (*mime).to_owned())
                .collect(),
            limits: Some(self.caps.wire()),
            version: env!("CARGO_PKG_VERSION").to_owned(),
            concurrency: u32::try_from(self.capacity.concurrency).unwrap_or(u32::MAX),
            slot_wait_millis: millis(self.capacity.slot_wait),
            stream_idle_millis: millis(self.capacity.stream_idle),
        }))
    }
}

fn outcome(result: Result<ExtractedText, ExtractSkip>) -> ExtractResponse {
    ExtractResponse {
        outcome: Some(match result {
            Ok(text) => extract_response::Outcome::Text(text),
            Err(skip) => extract_response::Outcome::Skip(skip),
        }),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_request_lowers_to_the_caps_and_zero_is_the_cap() {
        let caps = Caps {
            max_input_bytes: 100,
            max_pages: 10,
            max_output_bytes: 50,
            timeout: Duration::from_secs(1),
        };
        assert_eq!(caps.apply(None), caps);
        let asked = ExtractLimits {
            max_input_bytes: 1000,
            max_pages: 5,
            max_output_bytes: 0,
            timeout_millis: 10,
        };
        assert_eq!(
            caps.apply(Some(&asked)),
            Caps {
                max_input_bytes: 100,
                max_pages: 5,
                max_output_bytes: 50,
                timeout: Duration::from_millis(10),
            }
        );
        assert_eq!(caps.wire().max_pages, 10);
    }

    #[test]
    fn every_typed_error_has_a_wire_reason() {
        let cases = [
            (
                DocumentError::Unsupported("x".into()),
                SkipReason::Unsupported,
            ),
            (DocumentError::Encrypted("x".into()), SkipReason::Encrypted),
            (
                DocumentError::TooManyPages("x".into()),
                SkipReason::TooManyPages,
            ),
            (DocumentError::TooLarge("x".into()), SkipReason::TooLarge),
            (DocumentError::Timeout("x".into()), SkipReason::Timeout),
            (DocumentError::Corrupt("x".into()), SkipReason::Corrupt),
            (DocumentError::NoText, SkipReason::Empty),
        ];
        for (error, reason) in cases {
            assert_eq!(skip_for(&error).reason, reason as i32);
        }
    }

    // ── the overrun ledger, both orderings, exactly ─────────────────────────

    #[test]
    fn the_ledger_counts_a_thread_that_outran_its_timeout_until_it_ends() {
        let overruns = Arc::new(AtomicUsize::new(0));
        let ledger = OverrunLedger::new(Arc::clone(&overruns));
        // The timeout fires while the thread runs: counted.
        assert_eq!(ledger.timed_out(), Ok(1));
        assert_eq!(overruns.load(Ordering::SeqCst), 1);
        // The thread ends later: taken back, never below zero.
        ledger.finished();
        assert_eq!(overruns.load(Ordering::SeqCst), 0);
        // A second ending (impossible in practice) changes nothing.
        ledger.finished();
        assert_eq!(overruns.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn the_ledger_counts_nothing_for_a_thread_that_ended_first() {
        let overruns = Arc::new(AtomicUsize::new(0));
        let ledger = OverrunLedger::new(Arc::clone(&overruns));
        // The thread ends; then the timeout, which had fired at the same
        // moment, does its accounting: nothing counted, nothing to undo.
        ledger.finished();
        assert_eq!(ledger.timed_out(), Err(AlreadyFinished));
        assert_eq!(overruns.load(Ordering::SeqCst), 0);
        // And a late timeout on a long-finished thread stays at zero too.
        assert_eq!(ledger.timed_out(), Err(AlreadyFinished));
        assert_eq!(overruns.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn two_ledgers_against_one_counter_interleave_exactly() {
        let overruns = Arc::new(AtomicUsize::new(0));
        let a = OverrunLedger::new(Arc::clone(&overruns));
        let b = OverrunLedger::new(Arc::clone(&overruns));
        assert_eq!(a.timed_out(), Ok(1));
        b.finished();
        assert_eq!(b.timed_out(), Err(AlreadyFinished));
        assert_eq!(overruns.load(Ordering::SeqCst), 1);
        a.finished();
        assert_eq!(overruns.load(Ordering::SeqCst), 0);
    }

    // ── the race through the service, forced each way with the hooks ───────

    /// Counts exhaustion notices.
    #[derive(Default)]
    struct Counting(AtomicUsize);

    impl Exhausted for Counting {
        fn slots_exhausted(&self) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    fn pdf_header() -> ExtractHeader {
        ExtractHeader {
            filename: "race.pdf".to_owned(),
            content_type: "application/pdf".to_owned(),
            content_length: 0,
            limits: None,
        }
    }

    fn racing(timeout: Duration, hooks: Hooks) -> (Extractor, Arc<Counting>) {
        let exhausted = Arc::new(Counting::default());
        let mut extractor = Extractor::new(
            Caps {
                timeout,
                ..Caps::default()
            },
            Capacity {
                concurrency: 1,
                ..Capacity::default()
            },
            BTreeSet::new(),
            Arc::clone(&exhausted) as Arc<dyn Exhausted>,
        );
        extractor.hooks = hooks;
        (extractor, exhausted)
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_thread_that_ends_as_its_timeout_fires_is_not_an_overrun_and_its_answer_is_used() {
        // The timeout fires (1 ms) and the accounting is held back long
        // enough for the thread to finish first: the ledger reports
        // AlreadyFinished, the count stays at zero, the exhaustion hook is
        // not called, and the document's real answer (corrupt bytes) is
        // what the request gets, not TIMEOUT.
        let (extractor, exhausted) = racing(
            Duration::from_millis(1),
            Hooks {
                before_extract: None,
                before_timeout_accounting: Some(Arc::new(|| {
                    std::thread::sleep(Duration::from_millis(400));
                })),
            },
        );
        let limits = extractor.caps.apply(None);
        let result = extractor
            .extract_bytes(&pdf_header(), limits, b"%PDF-1.4 not really a pdf".to_vec())
            .await
            .expect("no status");
        let skip = result.expect_err("a skip");
        assert_eq!(skip.reason, SkipReason::Corrupt as i32, "{skip:?}");
        assert_eq!(extractor.overruns(), 0);
        assert_eq!(exhausted.0.load(Ordering::SeqCst), 0);
        // The slot came back with the thread.
        assert_eq!(extractor.slots.available_permits(), 1);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_thread_that_outruns_its_timeout_is_counted_until_it_ends() {
        // The thread is held before it extracts (300 ms) and the timeout is
        // 1 ms: TIMEOUT is answered, the overrun is counted and, with one
        // slot, exhaustion is reported once; when the thread ends the count
        // returns to zero and the slot is free again.
        let (extractor, exhausted) = racing(
            Duration::from_millis(1),
            Hooks {
                before_extract: Some(Arc::new(|| {
                    std::thread::sleep(Duration::from_millis(300));
                })),
                before_timeout_accounting: None,
            },
        );
        let limits = extractor.caps.apply(None);
        let result = extractor
            .extract_bytes(&pdf_header(), limits, b"%PDF-1.4 not really a pdf".to_vec())
            .await
            .expect("no status");
        let skip = result.expect_err("a skip");
        assert_eq!(skip.reason, SkipReason::Timeout as i32, "{skip:?}");
        assert_eq!(extractor.overruns(), 1);
        assert_eq!(exhausted.0.load(Ordering::SeqCst), 1);
        assert_eq!(extractor.slots.available_permits(), 0);
        // Wait for the thread; the count is exact afterwards.
        let waited = std::time::Instant::now();
        while extractor.overruns() != 0 && waited.elapsed() < Duration::from_secs(5) {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert_eq!(extractor.overruns(), 0);
        assert_eq!(extractor.slots.available_permits(), 1);
    }
}
