//! `elitea.extract.v1.DocumentExtractService`.
//!
//! What a request goes through, in order:
//!
//! 0. the peer: the connecting process's credentials (`SO_PEERCRED`) must
//!    name one of the configured worker uids, else `PERMISSION_DENIED`
//!    before anything is read (the pod-local Unix-socket sidecar exception
//!    to the gRPC-over-mTLS rule, AGENTS.md). `Capabilities` alone is also
//!    served to this process's own uid: it is the health probe, and the
//!    probe is this binary asking itself, whatever uids the worker runs as;
//! 1. the header: a media type this build extracts, else the document is
//!    skipped `UNSUPPORTED`; its limits, each lowered to the process's
//!    [`Caps`]; a declared length over the input cap is `TOO_LARGE` before a
//!    byte is read;
//! 2. a slot, BEFORE the body: at most `concurrency` extractions run at
//!    once, and a request waits at most [`Capacity::slot_wait`] for one
//!    (`RESOURCE_EXHAUSTED` after). A request that waits holds no document
//!    bytes while it waits, so the memory the process holds in bodies is
//!    bounded by `concurrency` documents at the cap, not by how many
//!    requests a client has in flight. The slot wait is a separate,
//!    generous bound: a client that keeps no more calls in flight than the
//!    advertised concurrency never waits at all, and one that does is
//!    queued, not refused, for as long as its own deadline allows;
//! 3. the chunks, counted as they arrive: the input cap is enforced on the
//!    running total, so an over-sized document is refused at the cap and
//!    never held whole; a stream that goes quiet for longer than the idle
//!    limit is `DEADLINE_EXCEEDED`;
//! 4. the extraction, on ONE dedicated large-stack thread of its own, which
//!    owns the slot, the document and its overrun ledger and signals its
//!    result back over a oneshot channel. The parser's own cooperative
//!    timeout runs at the request's timeout rounded up to whole seconds; the
//!    request waits that long plus a short grace, so the cooperative timeout
//!    fires first and an ordinary overrun ends by itself. A thread that
//!    outruns even that cannot be killed; the request is answered `TIMEOUT`
//!    and the thread keeps its slot until it ends. A request the client
//!    cancels (or whose deadline the transport enforces) while its thread
//!    runs is accounted exactly like a timeout: the thread is an overrun
//!    until it ends. When every slot is held by an overrun the process is
//!    useless and EXITS (after a short grace so the last answers reach the
//!    worker): the container restarts it, and the worker records
//!    `extract_failed` for whatever was in flight and reconnects.
//!    `Capabilities` reports the slots held and the overruns, and the health
//!    probe fails when the overruns are every slot;
//! 5. the text, unless it is over the output cap (`OUTPUT_TOO_LARGE`) or
//!    empty (`EMPTY`).
//!
//! Every skip is a statement about the document and travels as a response.
//! A malformed stream, no capacity and an extractor failure (its own, not
//! the document's: a temp file it could not write, a thread that did not
//! start) are gRPC statuses, which the worker records as `extract_failed`.

use std::collections::BTreeSet;
use std::sync::Arc;
use std::sync::atomic::{AtomicU8, AtomicU64, AtomicUsize, Ordering};
use std::time::Duration;

use elitea_doc_extract::{
    DocumentError, DocumentLimits, EXTRACTION_STACK_BYTES, cooperative_timeout, document_types,
    extract_document_on_this_thread, is_document_type,
};
use tokio::sync::{OwnedSemaphorePermit, Semaphore, oneshot};
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

/// Added to the parser's whole-second cooperative timeout to make the
/// request's own bound: the parser's timer, its unwinding and the thread's
/// hand-off. The cooperative timeout fires first; this fires only for a
/// thread that no longer answers its own timer.
pub const TIMEOUT_GRACE: Duration = Duration::from_millis(500);

/// The exit code of a process that gave up its slots to overruns.
pub const EXIT_SLOTS_EXHAUSTED: i32 = 75;

/// The largest gRPC message accepted: one streamed chunk plus its framing.
/// The document itself is bounded by the input cap, not by this; and a
/// body's buffer is first sized to at most this, however much the header
/// declares, growing with the chunks that really arrive.
pub const MAX_MESSAGE_BYTES: usize = 4 << 20;

/// This process's real uid: the default peer (the worker and the extractor
/// run as the same non-root user in every shipped deployment), and the one
/// uid `Capabilities` is always served to (the health probe).
#[must_use]
pub fn own_uid() -> u32 {
    rustix::process::getuid().as_raw()
}

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

/// How long a request waits for its extraction thread before it stops
/// waiting: the parser's own cooperative timeout for `timeout` (whole
/// seconds, rounded up) plus [`TIMEOUT_GRACE`]. Always after the
/// cooperative timeout, so that fires first.
#[must_use]
pub fn outer_timeout(timeout: Duration) -> Duration {
    cooperative_timeout(timeout).saturating_add(TIMEOUT_GRACE)
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
    /// Called once per request that observed it. May be called from a
    /// `Drop` (a cancelled request), so it must not need an async runtime.
    fn slots_exhausted(&self);
}

/// Production: log, wait a short grace (250 ms, for the answers already
/// written to reach the worker), exit [`EXIT_SLOTS_EXHAUSTED`]. On a plain
/// thread: it is called from a dropped request's guard as well as from a
/// running one, and must work from either.
pub struct ExitProcess;

impl Exhausted for ExitProcess {
    fn slots_exhausted(&self) {
        tracing::error!(
            event = "extractor_slots_exhausted",
            "every extraction slot is held by a timed-out or abandoned extraction; exiting so \
             the container restarts this process"
        );
        let spawned = std::thread::Builder::new()
            .name("exit-on-exhaustion".to_owned())
            .spawn(|| {
                std::thread::sleep(EXIT_GRACE);
                std::process::exit(EXIT_SLOTS_EXHAUSTED);
            });
        if spawned.is_err() {
            std::process::exit(EXIT_SLOTS_EXHAUSTED);
        }
    }
}

/// One extraction thread's standing against the overrun counter.
///
/// Two parties touch it from different threads: the request (whose timeout
/// may fire, or which may be dropped, while the thread still runs or just
/// as it ended) and the thread itself (which ends before or after that).
/// The state cell orders them: the request counts the thread as an overrun
/// only when it moved the state from `RUNNING` to `OVERRUN`, and the thread
/// takes the overrun back only when it found `OVERRUN` there. The counter
/// is raised BEFORE the state is claimed and lowered after it is read, so
/// every decrement is preceded by its own increment and the count can
/// neither underflow nor keep a finished thread.
pub struct OverrunLedger {
    state: AtomicU8,
    overruns: Arc<AtomicUsize>,
}

const RUNNING: u8 = 0;
const FINISHED: u8 = 1;
const OVERRUN: u8 = 2;

/// The thread had already ended when its request gave up on it: its result
/// is there to be used, and nothing was counted.
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

    /// The thread ended. When its request had counted it as an overrun,
    /// the overrun is over and the counter goes down by one.
    pub fn finished(&self) {
        if self.state.swap(FINISHED, Ordering::SeqCst) == OVERRUN {
            self.overruns.fetch_sub(1, Ordering::SeqCst);
        }
    }

    /// The request gave up on the thread (its timeout fired, or it was
    /// cancelled). `Ok(total)` when the thread still runs and is now
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

/// A document's bytes, counted in the process's `held` total from the first
/// chunk until the extraction thread that owns them ends (the bytes move
/// into the parser; the guard outlives them on the thread). What the tests
/// read to prove a queued request holds no body.
struct Body {
    bytes: Vec<u8>,
    counted: u64,
    held: Arc<AtomicU64>,
}

impl Body {
    fn new(reserve: usize, held: Arc<AtomicU64>) -> Self {
        Self {
            bytes: Vec::with_capacity(reserve),
            counted: 0,
            held,
        }
    }

    fn len(&self) -> u64 {
        self.bytes.len() as u64
    }

    fn extend(&mut self, chunk: &[u8]) {
        self.bytes.extend_from_slice(chunk);
        self.counted += chunk.len() as u64;
        self.held.fetch_add(chunk.len() as u64, Ordering::SeqCst);
    }

    /// The bytes, for the parser; the guard keeps counting them until it is
    /// dropped.
    fn take(&mut self) -> Vec<u8> {
        std::mem::take(&mut self.bytes)
    }
}

impl Drop for Body {
    fn drop(&mut self) {
        self.held.fetch_sub(self.counted, Ordering::SeqCst);
    }
}

/// Which RPC a peer asks for; `Capabilities` is also the health probe.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Rpc {
    Extract,
    Capabilities,
}

/// Test seam on the request's side of the timeout race. Compiled only for
/// the crate's own tests.
#[cfg(test)]
#[derive(Default, Clone)]
struct Hooks {
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
    /// This process's uid: served `Capabilities` (the health probe) always.
    own_uid: u32,
    slots: Arc<Semaphore>,
    /// Extractions that outran their timeout, or lost their request, and
    /// still run.
    overruns: Arc<AtomicUsize>,
    /// Bytes of document bodies this process holds right now: being read
    /// (a slot already held) or under extraction. Never a queued request's.
    held_body_bytes: Arc<AtomicU64>,
    exhausted: Arc<dyn Exhausted>,
    /// A pause on the extraction thread before it extracts: a seam for the
    /// tests of the timeout and of a cancelled request (nothing in the
    /// binary sets it).
    extraction_delay: Duration,
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
            own_uid: own_uid(),
            slots: Arc::new(Semaphore::new(capacity.concurrency)),
            overruns: Arc::new(AtomicUsize::new(0)),
            held_body_bytes: Arc::new(AtomicU64::new(0)),
            exhausted,
            extraction_delay: Duration::ZERO,
            #[cfg(test)]
            hooks: Hooks::default(),
        }
    }

    /// Extractions that outran their timeout, or lost their request, and
    /// still run.
    #[must_use]
    pub fn overruns(&self) -> usize {
        self.overruns.load(Ordering::SeqCst)
    }

    /// Slots held at this moment.
    #[must_use]
    pub fn busy(&self) -> usize {
        self.capacity
            .concurrency
            .saturating_sub(self.slots.available_permits())
    }

    /// Bytes of document bodies held right now (being read or extracted).
    /// A seam for the tests: a queued request contributes nothing.
    #[doc(hidden)]
    #[must_use]
    pub fn held_body_bytes(&self) -> u64 {
        self.held_body_bytes.load(Ordering::SeqCst)
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

    /// Pause every extraction thread for `delay` before it extracts: a seam
    /// for the tests of a timeout and of a cancelled request, standing in
    /// for a document that takes that long. Nothing in the binary calls it.
    #[doc(hidden)]
    #[must_use]
    pub fn with_extraction_delay(mut self, delay: Duration) -> Self {
        self.extraction_delay = delay;
        self
    }

    /// The peer check. A request from a uid outside `peer_uids`, or one
    /// whose credentials the transport did not deliver, is refused — except
    /// `Capabilities` from this process's own uid, which is the health
    /// probe (the binary asking itself) and is served whatever uids the
    /// worker is configured as. `Extract` is never served on that ground.
    fn authorize<T>(&self, request: &Request<T>, rpc: Rpc) -> Result<(), Status> {
        let uid = request
            .extensions()
            .get::<UdsConnectInfo>()
            .and_then(|info| info.peer_cred.as_ref())
            .map(tokio::net::unix::UCred::uid);
        match uid {
            Some(uid) if self.peer_uids.contains(&uid) => Ok(()),
            Some(uid) if rpc == Rpc::Capabilities && uid == self.own_uid => Ok(()),
            Some(uid) => {
                tracing::warn!(event = "extractor_peer_refused", peer_uid = uid, rpc = ?rpc);
                Err(Status::permission_denied(format!(
                    "peer uid {uid} is not a configured worker uid"
                )))
            }
            None => {
                tracing::warn!(event = "extractor_peer_refused", peer_uid = "unknown", rpc = ?rpc);
                Err(Status::permission_denied(
                    "the peer's credentials are unknown; only a Unix-socket peer is served",
                ))
            }
        }
    }

    /// One slot, within the slot wait.
    async fn slot(&self) -> Result<OwnedSemaphorePermit, Status> {
        tokio::time::timeout(
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
        .map_err(|_| Status::internal("the extraction slots are closed"))
    }

    /// The document's chunks, after the header and with a slot held, into
    /// one buffer counted in `held_body_bytes`. The buffer is first sized to
    /// the declared length bounded by the cap AND by one message, so a
    /// header cannot make the process allocate what no chunk delivers.
    async fn read_document(
        &self,
        stream: &mut Streaming<ExtractRequest>,
        limits: &Caps,
        declared: u64,
    ) -> Result<Result<Body, ExtractSkip>, Status> {
        let reserve = usize::try_from(declared.min(limits.max_input_bytes))
            .unwrap_or(0)
            .min(MAX_MESSAGE_BYTES);
        let mut body = Body::new(reserve, Arc::clone(&self.held_body_bytes));
        loop {
            let next = tokio::time::timeout(self.capacity.stream_idle, stream.message())
                .await
                .map_err(|_| Status::deadline_exceeded("no chunk arrived within the idle limit"))?
                .map_err(|status| {
                    Status::new(status.code(), format!("reading the stream: {status}"))
                })?;
            let Some(message) = next else {
                return Ok(Ok(body));
            };
            match message.message {
                Some(extract_request::Message::Chunk(chunk)) => {
                    let total = body.len() + chunk.len() as u64;
                    if total > limits.max_input_bytes {
                        return Ok(Err(skip(
                            SkipReason::TooLarge,
                            format!(
                                "the document is over the {}-byte limit",
                                limits.max_input_bytes
                            ),
                        )));
                    }
                    body.extend(&chunk);
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

    /// The extraction of `body` under `limits`, on a thread of its own that
    /// owns `permit` (the slot), the body and the ledger. The request waits
    /// for the thread's answer for [`outer_timeout`] of the limits; a thread
    /// still running after that, or when the request is dropped, is an
    /// overrun until it ends.
    async fn extract_body(
        &self,
        header: &ExtractHeader,
        limits: Caps,
        mut body: Body,
        permit: OwnedSemaphorePermit,
    ) -> Result<Result<ExtractedText, ExtractSkip>, Status> {
        let mime = header.content_type.clone();
        let document_limits = DocumentLimits {
            max_pages: limits.max_pages,
            max_content_bytes: usize::try_from(limits.max_input_bytes.max(limits.max_output_bytes))
                .unwrap_or(usize::MAX),
            timeout: limits.timeout,
        };
        let ledger = OverrunLedger::new(Arc::clone(&self.overruns));
        let thread_ledger = Arc::clone(&ledger);
        let delay = self.extraction_delay;
        let (answer, mut answered) = oneshot::channel();
        let spawned = std::thread::Builder::new()
            .name("doc-extract".to_owned())
            .stack_size(EXTRACTION_STACK_BYTES)
            .spawn(move || {
                // Owned by the thread until it ends, however the request
                // fares: the slot, and the body's place in the held total.
                let _slot = permit;
                let bytes = body.take();
                let _held = body;
                if !delay.is_zero() {
                    std::thread::sleep(delay);
                }
                // The bytes move into the extractor: no copy is made here.
                let result = extract_document_on_this_thread(&mime, bytes, &document_limits);
                // Finished BEFORE the answer is sent: a request that reads
                // AlreadyFinished knows the answer is a moment away at most.
                thread_ledger.finished();
                let _ = answer.send(result);
            });
        if let Err(error) = spawned {
            // Nothing ran: the closure (and the permit with it) is dropped,
            // the ledger was never claimed, nothing is counted.
            return Err(Status::internal(format!(
                "the extraction thread did not start: {error}"
            )));
        }
        // From here the thread runs; the watch accounts for it when this
        // request stops waiting, by timeout below or by being dropped.
        let mut watch = Watch {
            extractor: self,
            ledger,
            header,
            settled: false,
        };
        let result = match tokio::time::timeout(outer_timeout(limits.timeout), &mut answered).await
        {
            Ok(Ok(result)) => {
                watch.settle();
                result
            }
            Ok(Err(_)) => {
                // The thread ended without answering: it panicked (in the
                // release profile a panic aborts the process instead).
                watch.settle();
                return Err(Status::internal(
                    "the extraction thread ended without an answer",
                ));
            }
            Err(_) => {
                #[cfg(test)]
                if let Some(hook) = &self.hooks.before_timeout_accounting {
                    hook();
                }
                match watch.abandon("outran its timeout") {
                    Some(_) => {
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
                    None => answered.await.map_err(|_| {
                        Status::internal("the extraction thread ended without an answer")
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

/// A request's standing towards its running extraction thread. Settled when
/// the thread answered (nothing to account); otherwise, when the request
/// stops waiting — its timeout fired, or the future was dropped because the
/// client cancelled or the transport enforced its deadline — the thread is
/// accounted as an overrun through its ledger, once, and exhaustion is
/// reported when the overruns are every slot. The `Drop` is what makes a
/// cancelled request's thread count: without it a hung extraction whose
/// client went away would hold its slot uncounted for ever.
struct Watch<'a> {
    extractor: &'a Extractor,
    ledger: Arc<OverrunLedger>,
    header: &'a ExtractHeader,
    settled: bool,
}

impl Watch<'_> {
    /// The thread answered: nothing to account.
    fn settle(&mut self) {
        self.settled = true;
    }

    /// The request stops waiting (`why`). `Some(overruns)` when the thread
    /// still ran and is now counted; `None` when it had already ended.
    fn abandon(&mut self, why: &str) -> Option<usize> {
        if self.settled {
            return None;
        }
        self.settled = true;
        let running = self.ledger.timed_out().ok()?;
        tracing::warn!(
            event = "extractor_timeout",
            filename = %self.header.filename,
            content_type = %self.header.content_type,
            overruns = running,
            "an extraction {why}; its thread keeps its slot until it ends"
        );
        if running >= self.extractor.capacity.concurrency {
            self.extractor.exhausted.slots_exhausted();
        }
        Some(running)
    }
}

impl Drop for Watch<'_> {
    fn drop(&mut self) {
        self.abandon("lost its request (cancelled, or past the transport's deadline)");
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
        self.authorize(&request, Rpc::Extract)?;
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
        if header.content_length > limits.max_input_bytes {
            return Ok(Response::new(outcome(Err(skip(
                SkipReason::TooLarge,
                format!(
                    "the document declares {} bytes, over the {}-byte limit",
                    header.content_length, limits.max_input_bytes
                ),
            )))));
        }
        // The slot first, the body after: a request that waits holds no
        // bytes, so the bodies in memory are bounded by the concurrency.
        let permit = self.slot().await?;
        let body = match self
            .read_document(&mut stream, &limits, header.content_length)
            .await?
        {
            Ok(body) => body,
            Err(skip) => return Ok(Response::new(outcome(Err(skip)))),
        };
        let started = std::time::Instant::now();
        let result = self.extract_body(&header, limits, body, permit).await?;
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
        self.authorize(&request, Rpc::Capabilities)?;
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
            busy: u32::try_from(self.busy()).unwrap_or(u32::MAX),
            overruns: u32::try_from(self.overruns()).unwrap_or(u32::MAX),
        }))
    }
}

/// Whether a `Capabilities` answer describes a process that can still
/// extract anything: not when every slot is held by an overrun. What the
/// health probe decides on. A `concurrency` of 0 (an older build that did
/// not report it) is read as healthy: the answer itself proves the process
/// serves, and nothing more can be told.
#[must_use]
pub fn is_healthy(answer: &CapabilitiesResponse) -> bool {
    answer.concurrency == 0 || answer.overruns < answer.concurrency
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

    #[test]
    fn the_outer_timeout_is_after_the_parsers_whole_second_one() {
        // 1 ms asked: the parser gets a whole second, the request waits
        // that plus the grace, never less.
        assert_eq!(
            outer_timeout(Duration::from_millis(1)),
            Duration::from_secs(1) + TIMEOUT_GRACE
        );
        // 1500 ms asked: rounded UP to two seconds, not down to one.
        assert_eq!(
            cooperative_timeout(Duration::from_millis(1500)),
            Duration::from_secs(2)
        );
        assert_eq!(
            outer_timeout(Duration::from_millis(1500)),
            Duration::from_secs(2) + TIMEOUT_GRACE
        );
        for millis in [1_u64, 999, 1000, 1001, 1500, 120_000] {
            let asked = Duration::from_millis(millis);
            let cooperative = cooperative_timeout(asked);
            assert!(cooperative >= asked, "{millis} ms: {cooperative:?}");
            assert_eq!(cooperative.subsec_nanos(), 0, "whole seconds");
            assert!(outer_timeout(asked) > cooperative, "{millis} ms");
        }
    }

    #[test]
    fn health_is_overruns_under_concurrency() {
        let answer = |concurrency, overruns| CapabilitiesResponse {
            concurrency,
            overruns,
            ..CapabilitiesResponse::default()
        };
        assert!(is_healthy(&answer(4, 0)));
        assert!(is_healthy(&answer(4, 3)));
        assert!(!is_healthy(&answer(4, 4)));
        assert!(!is_healthy(&answer(1, 1)));
        // An older build that reports no concurrency: the answer is the proof.
        assert!(is_healthy(&answer(0, 0)));
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

    // ── the race through the service, forced each way ───────────────────────

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

    fn racing(timeout: Duration, delay: Duration, hooks: Hooks) -> (Extractor, Arc<Counting>) {
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
        )
        .with_extraction_delay(delay);
        extractor.hooks = hooks;
        (extractor, exhausted)
    }

    /// A body of `bytes`, counted in the extractor's held total.
    fn body_of(extractor: &Extractor, bytes: &[u8]) -> Body {
        let mut body = Body::new(0, Arc::clone(&extractor.held_body_bytes));
        body.extend(bytes);
        body
    }

    async fn wait_for_zero_overruns(extractor: &Extractor) {
        let waited = std::time::Instant::now();
        while extractor.overruns() != 0 && waited.elapsed() < Duration::from_secs(10) {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_thread_that_ends_as_its_timeout_fires_is_not_an_overrun_and_its_answer_is_used() {
        // The timeout fires (1 ms asked: 1 s cooperative + the grace) and
        // the accounting is held back long enough for the thread to finish
        // first: the ledger reports AlreadyFinished, the count stays at
        // zero, the exhaustion hook is not called, and the document's real
        // answer (corrupt bytes) is what the request gets, not TIMEOUT.
        let (extractor, exhausted) = racing(
            Duration::from_millis(1),
            // Past the outer timeout, but done before the accounting runs.
            outer_timeout(Duration::from_millis(1)) + Duration::from_millis(100),
            Hooks {
                before_timeout_accounting: Some(Arc::new(|| {
                    std::thread::sleep(Duration::from_millis(600));
                })),
            },
        );
        let limits = extractor.caps.apply(None);
        let permit = extractor.hold_slot().await;
        let body = body_of(&extractor, b"%PDF-1.4 not really a pdf");
        let result = extractor
            .extract_body(&pdf_header(), limits, body, permit)
            .await
            .expect("no status");
        let skip = result.expect_err("a skip");
        assert_eq!(skip.reason, SkipReason::Corrupt as i32, "{skip:?}");
        assert_eq!(extractor.overruns(), 0);
        assert_eq!(exhausted.0.load(Ordering::SeqCst), 0);
        // The slot came back with the thread, and the body is released.
        assert_eq!(extractor.slots.available_permits(), 1);
        assert_eq!(extractor.held_body_bytes(), 0);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_thread_that_outruns_its_timeout_is_counted_until_it_ends() {
        // The thread is held before it extracts for longer than the outer
        // timeout: TIMEOUT is answered, the overrun is counted and, with
        // one slot, exhaustion is reported once; the body stays counted
        // while the thread holds it; when the thread ends the count returns
        // to zero and the slot is free again.
        let delay = outer_timeout(Duration::from_millis(1)) + Duration::from_millis(500);
        let (extractor, exhausted) = racing(Duration::from_millis(1), delay, Hooks::default());
        let limits = extractor.caps.apply(None);
        let permit = extractor.hold_slot().await;
        let bytes = b"%PDF-1.4 not really a pdf";
        let body = body_of(&extractor, bytes);
        let result = extractor
            .extract_body(&pdf_header(), limits, body, permit)
            .await
            .expect("no status");
        let skip = result.expect_err("a skip");
        assert_eq!(skip.reason, SkipReason::Timeout as i32, "{skip:?}");
        assert_eq!(extractor.overruns(), 1);
        assert_eq!(exhausted.0.load(Ordering::SeqCst), 1);
        assert_eq!(extractor.slots.available_permits(), 0);
        assert_eq!(extractor.busy(), 1);
        assert_eq!(extractor.held_body_bytes(), bytes.len() as u64);
        wait_for_zero_overruns(&extractor).await;
        assert_eq!(extractor.overruns(), 0);
        assert_eq!(extractor.slots.available_permits(), 1);
        assert_eq!(extractor.held_body_bytes(), 0);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_cancelled_request_counts_its_running_thread_like_a_timeout() {
        // The request's future is dropped (the client went away, or the
        // transport enforced its deadline) 100 ms in, long before its own
        // timeout (10 s) and while the thread is still in its delay. The
        // thread is counted as an overrun at once, exhaustion is reported
        // (one slot), the slot and the body stay held; when the thread
        // ends everything is released and the count is exact.
        let (extractor, exhausted) = racing(
            Duration::from_secs(10),
            Duration::from_secs(1),
            Hooks::default(),
        );
        let limits = extractor.caps.apply(None);
        let permit = extractor.hold_slot().await;
        let body = body_of(&extractor, b"%PDF-1.4 not really a pdf");
        let header = pdf_header();
        let cancelled = tokio::time::timeout(
            Duration::from_millis(100),
            extractor.extract_body(&header, limits, body, permit),
        )
        .await;
        assert!(cancelled.is_err(), "the request was dropped, not answered");
        assert_eq!(extractor.overruns(), 1, "counted at the drop");
        assert_eq!(exhausted.0.load(Ordering::SeqCst), 1);
        assert_eq!(extractor.slots.available_permits(), 0);
        assert!(extractor.held_body_bytes() > 0);
        wait_for_zero_overruns(&extractor).await;
        assert_eq!(extractor.overruns(), 0);
        assert_eq!(extractor.slots.available_permits(), 1);
        assert_eq!(extractor.held_body_bytes(), 0);
        // Exhaustion was reported exactly once, by the drop.
        assert_eq!(exhausted.0.load(Ordering::SeqCst), 1);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_request_dropped_after_its_answer_counts_nothing() {
        // The thread answers at once; the request is settled; dropping it
        // afterwards (every request is dropped eventually) accounts nothing.
        let (extractor, exhausted) =
            racing(Duration::from_secs(10), Duration::ZERO, Hooks::default());
        let limits = extractor.caps.apply(None);
        let permit = extractor.hold_slot().await;
        let body = body_of(&extractor, b"%PDF-1.4 not really a pdf");
        let result = extractor
            .extract_body(&pdf_header(), limits, body, permit)
            .await
            .expect("no status");
        assert!(result.is_err(), "corrupt: a skip");
        assert_eq!(extractor.overruns(), 0);
        assert_eq!(exhausted.0.load(Ordering::SeqCst), 0);
        assert_eq!(extractor.held_body_bytes(), 0);
    }
}
