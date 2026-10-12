//! `elitea.extract.v1.DocumentExtractService`.
//!
//! What a request goes through, in order:
//!
//! 1. the header: a media type this build extracts, else the document is
//!    skipped `UNSUPPORTED`; its limits, each lowered to the process's
//!    [`Caps`]; a declared length over the input cap is `TOO_LARGE` before a
//!    byte is read;
//! 2. the chunks, counted as they arrive: the input cap is enforced on the
//!    running total, so an over-sized document is refused at the cap and
//!    never held whole; a stream that goes quiet for longer than the idle
//!    limit is `DEADLINE_EXCEEDED`;
//! 3. a slot: at most `concurrency` extractions run at once, and a request
//!    waits at most its own timeout for one (`RESOURCE_EXHAUSTED` after);
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

use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;

use elitea_doc_extract::{
    DocumentError, DocumentLimits, document_types, extract_document, is_document_type,
};
use tokio::sync::Semaphore;
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
            timeout_millis: u32::try_from(self.timeout.as_millis()).unwrap_or(u32::MAX),
        }
    }
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

/// The service.
pub struct Extractor {
    caps: Caps,
    concurrency: usize,
    stream_idle: Duration,
    slots: Arc<Semaphore>,
    /// Extractions that outran their timeout and still run.
    overruns: Arc<AtomicUsize>,
    exhausted: Arc<dyn Exhausted>,
}

impl Extractor {
    /// A service with `caps`, `concurrency` slots and `stream_idle` as the
    /// longest wait for a chunk; `exhausted` is told when every slot is
    /// held by an overrun.
    #[must_use]
    pub fn new(
        caps: Caps,
        concurrency: usize,
        stream_idle: Duration,
        exhausted: Arc<dyn Exhausted>,
    ) -> Self {
        let concurrency = concurrency.max(1);
        Self {
            caps,
            concurrency,
            stream_idle,
            slots: Arc::new(Semaphore::new(concurrency)),
            overruns: Arc::new(AtomicUsize::new(0)),
            exhausted,
        }
    }

    /// Extractions that outran their timeout and still run.
    #[must_use]
    pub fn overruns(&self) -> usize {
        self.overruns.load(Ordering::SeqCst)
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
            let next = tokio::time::timeout(self.stream_idle, stream.message())
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
        let permit = tokio::time::timeout(limits.timeout, Arc::clone(&self.slots).acquire_owned())
            .await
            .map_err(|_| Status::resource_exhausted("no extraction slot came free in time"))?
            .map_err(|_| Status::internal("the extraction slots are closed"))?;
        let mime = header.content_type.clone();
        let document_limits = DocumentLimits {
            max_pages: limits.max_pages,
            max_content_bytes: usize::try_from(limits.max_input_bytes.max(limits.max_output_bytes))
                .unwrap_or(usize::MAX),
            timeout: limits.timeout,
        };
        let overruns = Arc::clone(&self.overruns);
        let timed_out = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let thread_flag = Arc::clone(&timed_out);
        let thread_overruns = Arc::clone(&overruns);
        let work = tokio::task::spawn_blocking(move || {
            let result = extract_document(&mime, &bytes, &document_limits);
            drop(bytes);
            if thread_flag.load(Ordering::SeqCst) {
                thread_overruns.fetch_sub(1, Ordering::SeqCst);
            }
            // The slot is held until the extraction really ended.
            drop(permit);
            result
        });
        let result = match tokio::time::timeout(limits.timeout, work).await {
            Ok(Ok(result)) => result,
            Ok(Err(join)) => {
                return Err(Status::internal(format!(
                    "the extraction task ended: {join}"
                )));
            }
            Err(_) => {
                timed_out.store(true, Ordering::SeqCst);
                let running = overruns.fetch_add(1, Ordering::SeqCst) + 1;
                tracing::warn!(
                    event = "extractor_timeout",
                    filename = %header.filename,
                    content_type = %header.content_type,
                    overruns = running,
                    "an extraction outran its timeout; its thread keeps its slot until it ends"
                );
                if running >= self.concurrency {
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
        let mut stream = request.into_inner();
        let first = tokio::time::timeout(self.stream_idle, stream.message())
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
        _request: Request<CapabilitiesRequest>,
    ) -> Result<Response<CapabilitiesResponse>, Status> {
        Ok(Response::new(CapabilitiesResponse {
            content_types: document_types()
                .iter()
                .map(|mime| (*mime).to_owned())
                .collect(),
            limits: Some(self.caps.wire()),
            version: env!("CARGO_PKG_VERSION").to_owned(),
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
}
