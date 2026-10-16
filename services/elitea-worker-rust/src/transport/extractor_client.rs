//! The client of `elitea-doc-extractor`, the document extraction sidecar
//! (ADR-0030 I1), over its Unix socket.
//!
//! The worker's own lock cannot hold the PDF backend (Cargo.toml), so a PDF
//! or Office document an index run fetched is streamed to the sidecar and
//! its text comes back, or a typed reason it did not. The channel is lazy
//! and reconnects by itself: a sidecar that crashed or was restarted costs
//! the documents in flight (each reported `extract_failed`) and nothing
//! after it.
//!
//! The client learns the sidecar's capacity and caps from `Capabilities`
//! on the first document (and again after any failure, since a restarted
//! sidecar may be configured differently) and shapes itself to them:
//!
//! * it keeps no more calls in flight than the sidecar's advertised
//!   `concurrency`, queueing further documents HERE, so the sidecar's slot
//!   wait is never what turns ordinary queueing into `RESOURCE_EXHAUSTED`.
//!   The permits are ONE pool for the life of the client, resized to each
//!   `Capabilities` answer (permits added, or forgotten as the calls that
//!   hold them end) and never replaced: a failure that makes the client ask
//!   again while other calls are in flight must not hand out a second pool
//!   next to the permits those calls still hold;
//! * it asks for an extraction timeout (the run's, lowered to the sidecar's
//!   cap) in the header, and sets its own call deadline from that timeout
//!   plus the advertised slot wait and stream idle limit plus a margin,
//!   instead of a fixed figure that would be wrong for any other
//!   configuration.
//!
//! The document is streamed as slices of one reference-counted buffer
//! (`bytes::Bytes`): nothing here copies it, and the buffer is released as
//! the call completes.
//!
//! Nothing here is a statement about the document except what the sidecar
//! said: a transport failure, a gRPC status and a response without an
//! outcome are all [`ExtractorError`], which the run records as
//! `extract_failed`.

use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use hyper_util::rt::TokioIo;
use tokio::net::UnixStream;
use tokio::sync::{Mutex, OwnedSemaphorePermit, Semaphore};
use tonic::transport::{Channel, Endpoint, Uri};
use tower::service_fn;

use crate::protocol::extract::document_extract_service_client::DocumentExtractServiceClient;
use crate::protocol::extract::{
    CapabilitiesRequest, CapabilitiesResponse, ExtractHeader, ExtractImages, ExtractLimits,
    ExtractRequest, SkipReason as WireSkipReason, extract_request, extract_response,
};

/// Bytes per streamed chunk; the sidecar's message limit is 4 MiB.
const CHUNK_BYTES: usize = 256 * 1024;
/// The `Capabilities` probe that precedes the first document.
const CAPABILITIES_TIMEOUT: Duration = Duration::from_secs(10);
/// Added to the derived call deadline: the header and the chunks over a
/// local socket, the answer, and scheduling slack on both sides.
const DEADLINE_MARGIN: Duration = Duration::from_secs(30);
/// The largest answer accepted: the sidecar's default output cap is 20 MiB.
const MAX_RESPONSE_BYTES: usize = 64 << 20;

/// What stands in for a `Capabilities` field the sidecar left at 0 (an
/// older build): the sidecar's own defaults.
const FALLBACK_TIMEOUT: Duration = Duration::from_mins(2);
const FALLBACK_SLOT_WAIT: Duration = Duration::from_mins(10);
const FALLBACK_STREAM_IDLE: Duration = Duration::from_secs(30);
const FALLBACK_CONCURRENCY: usize = 1;

/// Why the sidecar read a document and gave no text.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum SkipReason {
    TooLarge,
    TooManyPages,
    Encrypted,
    Unsupported,
    Corrupt,
    Timeout,
    OutputTooLarge,
    Empty,
}

/// What a run asks of the sidecar for the pictures embedded in a document
/// (ADR-0030 I4 part C). The sidecar lowers each figure to its own cap.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) struct PictureRequest {
    /// Most pictures returned for one document.
    pub(crate) pictures: u32,
    /// Most bytes of one picture.
    pub(crate) picture_bytes: u64,
    /// Most bytes of all the pictures together.
    pub(crate) total_bytes: u64,
}

/// One picture embedded in a document, as the sidecar passed it through.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct Picture {
    pub(crate) bytes: Bytes,
    /// By the bytes: `image/png`, `image/jpeg`, `image/gif`, `image/webp`
    /// or `image/bmp`.
    pub(crate) media_type: &'static str,
    /// Position among the document's pictures, in document order.
    pub(crate) ordinal: u32,
    /// Where its description belongs in the text (a byte offset).
    pub(crate) text_offset: usize,
    /// The file name inside the package, when the format has one.
    pub(crate) name: Option<String>,
}

/// Pictures the sidecar found and did not return, by reason; plus those the
/// client refused to take from an answer that exceeded what was asked.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub(crate) struct PicturesDropped {
    pub(crate) too_many: u32,
    pub(crate) too_large: u32,
    pub(crate) unsupported: u32,
}

/// A document's text and, when the run asked, its embedded pictures.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct ExtractedDocument {
    pub(crate) text: String,
    pub(crate) pictures: Vec<Picture>,
    pub(crate) dropped: PicturesDropped,
}

/// What the sidecar answered.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum Extraction {
    Text(ExtractedDocument),
    Skipped(SkipReason),
}

/// The sidecar did not answer about the document: the socket, the stream
/// or the sidecar itself failed. Carries nothing from the document.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct ExtractorError(pub(crate) String);

impl std::fmt::Display for ExtractorError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for ExtractorError {}

/// What the sidecar advertised.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) struct Sidecar {
    /// The sidecar's extraction timeout cap.
    pub(crate) timeout: Duration,
    /// How long the sidecar lets a request wait for a slot.
    pub(crate) slot_wait: Duration,
    /// How long the sidecar waits for a document's next chunk.
    pub(crate) stream_idle: Duration,
    /// Documents the sidecar extracts at once; the client's in-flight calls
    /// are capped at it ([`Slots`]).
    pub(crate) concurrency: usize,
}

impl Sidecar {
    fn from_capabilities(answer: &CapabilitiesResponse) -> Self {
        let limits = answer.limits.unwrap_or_default();
        let or = |millis: u32, fallback: Duration| {
            if millis == 0 {
                fallback
            } else {
                Duration::from_millis(u64::from(millis))
            }
        };
        let concurrency = usize::try_from(answer.concurrency)
            .ok()
            .filter(|n| *n > 0)
            .unwrap_or(FALLBACK_CONCURRENCY);
        Self {
            timeout: or(limits.timeout_millis, FALLBACK_TIMEOUT),
            slot_wait: or(answer.slot_wait_millis, FALLBACK_SLOT_WAIT),
            stream_idle: or(answer.stream_idle_millis, FALLBACK_STREAM_IDLE),
            concurrency,
        }
    }

    /// The extraction timeout asked of the sidecar for a document the run
    /// gives `asked`: never more than the sidecar's cap, so the header
    /// states what will really apply.
    pub(crate) fn effective_timeout(&self, asked: Duration) -> Duration {
        asked.min(self.timeout)
    }

    /// The whole call's deadline for a document extracted under `timeout`:
    /// the extraction itself, the longest the sidecar may hold the request
    /// for a slot, its idle limit on the stream, and `margin`.
    pub(crate) fn call_deadline(&self, timeout: Duration, margin: Duration) -> Duration {
        timeout
            .saturating_add(self.slot_wait)
            .saturating_add(self.stream_idle)
            .saturating_add(margin)
    }
}

/// The client's one permit pool, for the life of the client: in-flight
/// calls hold its permits, and its size follows the sidecar's advertised
/// concurrency without ever being replaced.
///
/// Growing is immediate (`add_permits`). Shrinking forgets what is free at
/// once and remembers the rest as a DEBT, which each call that ends pays by
/// forgetting its own permit instead of returning it ([`Slots::release`]);
/// so the pool is exactly the advertised size as soon as the calls that
/// outnumber it have ended, and never more than the live permits plus the
/// free ones in between. A failure that makes the client ask the sidecar
/// again while calls are in flight therefore changes NOTHING about how many
/// may run: there is no second semaphore to acquire from next to the
/// permits the running calls hold.
#[derive(Debug)]
struct Slots {
    semaphore: Arc<Semaphore>,
    sizing: std::sync::Mutex<Sizing>,
}

/// The pool's bookkeeping: what it is sized to, and how many permits are
/// still to be forgotten as calls end.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct Sizing {
    size: usize,
    debt: usize,
}

impl Slots {
    /// An empty pool: nothing runs until the sidecar has advertised a size.
    fn new() -> Self {
        Self {
            semaphore: Arc::new(Semaphore::new(0)),
            sizing: std::sync::Mutex::new(Sizing { size: 0, debt: 0 }),
        }
    }

    fn sizing(&self) -> std::sync::MutexGuard<'_, Sizing> {
        self.sizing
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// Size the pool to `concurrency` permits, live ones included.
    fn resize(&self, concurrency: usize) {
        let mut sizing = self.sizing();
        if concurrency > sizing.size {
            let grow = concurrency - sizing.size;
            // A pending debt is cancelled first: a permit still to be
            // forgotten and a permit to be added are the same permit.
            let cancelled = grow.min(sizing.debt);
            sizing.debt -= cancelled;
            self.semaphore.add_permits(grow - cancelled);
        } else if concurrency < sizing.size {
            let shrink = sizing.size - concurrency;
            let forgotten = self.semaphore.forget_permits(shrink);
            sizing.debt += shrink - forgotten;
        }
        sizing.size = concurrency;
    }

    /// One permit, when one is free.
    async fn acquire(&self) -> Result<OwnedSemaphorePermit, ExtractorError> {
        Arc::clone(&self.semaphore)
            .acquire_owned()
            .await
            .map_err(|_| ExtractorError("the extractor queue is closed".to_owned()))
    }

    /// A call ended: its permit goes back to the pool, or pays one of the
    /// debt a shrink left behind.
    fn release(&self, permit: OwnedSemaphorePermit) {
        let mut sizing = self.sizing();
        if sizing.debt > 0 {
            sizing.debt -= 1;
            permit.forget();
        } else {
            drop(permit);
        }
    }

    /// Free permits right now (tests).
    #[cfg(test)]
    fn available(&self) -> usize {
        self.semaphore.available_permits()
    }
}

/// The client. Clone-cheap: one channel, one learned sidecar, one permit
/// pool, shared.
#[derive(Clone)]
pub(crate) struct ExtractorClient {
    channel: Channel,
    socket: PathBuf,
    /// Learned on the first document; forgotten after a failure.
    sidecar: Arc<Mutex<Option<Sidecar>>>,
    /// In-flight calls, at most the advertised concurrency. Outlives every
    /// `sidecar` the client learns.
    slots: Arc<Slots>,
    deadline_margin: Duration,
}

impl ExtractorClient {
    /// A client of the sidecar at `socket`. Nothing is dialled until the
    /// first document; a failed dial is retried on the next.
    ///
    /// # Errors
    ///
    /// The placeholder endpoint could not be built (tonic's error).
    pub(crate) fn connect_lazy(socket: &Path) -> Result<Self, tonic::transport::Error> {
        let path = socket.to_path_buf();
        // HTTP/2 needs an authority; nothing routes on it.
        let channel = Endpoint::try_from("http://doc-extractor.local")?
            .connect_timeout(Duration::from_secs(5))
            .tcp_nodelay(true)
            .connect_with_connector_lazy(service_fn(move |_: Uri| {
                let path = path.clone();
                async move { UnixStream::connect(path).await.map(TokioIo::new) }
            }));
        Ok(Self {
            channel,
            socket: socket.to_path_buf(),
            sidecar: Arc::new(Mutex::new(None)),
            slots: Arc::new(Slots::new()),
            deadline_margin: DEADLINE_MARGIN,
        })
    }

    /// The same client with another deadline margin (tests).
    #[cfg(test)]
    pub(crate) fn with_deadline_margin(mut self, margin: Duration) -> Self {
        self.deadline_margin = margin;
        self
    }

    /// The socket this client dials.
    pub(crate) fn socket(&self) -> &Path {
        &self.socket
    }

    /// What the sidecar advertised, as learned so far (tests).
    #[cfg(test)]
    pub(crate) async fn sidecar_for_tests(&self) -> Option<Sidecar> {
        *self.sidecar.lock().await
    }

    /// Free permits in the pool right now (tests).
    #[cfg(test)]
    pub(crate) fn free_slots_for_tests(&self) -> usize {
        self.slots.available()
    }

    /// The sidecar's capabilities, asked once and kept until a failure. The
    /// permit pool is resized to the answer under the same lock, so two
    /// documents that ask at once size it once.
    async fn sidecar(&self) -> Result<Sidecar, ExtractorError> {
        let mut learned = self.sidecar.lock().await;
        if let Some(sidecar) = learned.as_ref() {
            return Ok(*sidecar);
        }
        let mut client = DocumentExtractServiceClient::new(self.channel.clone());
        let answer = tokio::time::timeout(
            CAPABILITIES_TIMEOUT,
            client.capabilities(CapabilitiesRequest {}),
        )
        .await
        .map_err(|_| {
            ExtractorError(format!(
                "the extractor did not state its capabilities within {} s",
                CAPABILITIES_TIMEOUT.as_secs()
            ))
        })?
        .map_err(|status| {
            ExtractorError(format!(
                "the extractor refused the capabilities probe: {status}"
            ))
        })?
        .into_inner();
        let sidecar = Sidecar::from_capabilities(&answer);
        tracing::info!(
            event = "index_extractor_capabilities",
            socket = %self.socket.display(),
            version = %answer.version,
            concurrency = sidecar.concurrency,
            busy = answer.busy,
            overruns = answer.overruns,
            timeout_ms = u64::try_from(sidecar.timeout.as_millis()).unwrap_or(u64::MAX),
            slot_wait_ms = u64::try_from(sidecar.slot_wait.as_millis()).unwrap_or(u64::MAX),
        );
        self.slots.resize(sidecar.concurrency);
        *learned = Some(sidecar);
        Ok(sidecar)
    }

    /// Forget what the sidecar advertised: the next document asks again (a
    /// restarted sidecar may answer differently). The permit pool is kept:
    /// the calls in flight hold its permits, and the next answer resizes it.
    async fn forget(&self) {
        *self.sidecar.lock().await = None;
    }

    /// `bytes` of media type `mime`, named `filename`, as the sidecar reads
    /// them; `max_input_bytes` is this run's document cap and `timeout` the
    /// extraction time it asks for (lowered to the sidecar's cap). With
    /// `pictures` the answer carries the document's embedded pictures too;
    /// without, none is read.
    ///
    /// # Errors
    ///
    /// [`ExtractorError`] when the sidecar did not answer about the document.
    pub(crate) async fn extract(
        &self,
        filename: &str,
        mime: &str,
        bytes: Vec<u8>,
        max_input_bytes: u64,
        timeout: Duration,
        pictures: Option<PictureRequest>,
    ) -> Result<Extraction, ExtractorError> {
        let sidecar = self.sidecar().await?;
        // The local queue: no more calls in flight than the sidecar has
        // slots. Waiting here costs nothing on the sidecar's side. The
        // permit is given back through the pool (`release`), never dropped
        // on its own, so a shrink in between is honoured.
        let slot = self.slots.acquire().await?;
        let timeout = sidecar.effective_timeout(timeout);
        let deadline = sidecar.call_deadline(timeout, self.deadline_margin);
        let header = ExtractHeader {
            filename: filename.to_owned(),
            content_type: mime.to_owned(),
            content_length: bytes.len() as u64,
            limits: Some(ExtractLimits {
                max_input_bytes,
                timeout_millis: u32::try_from(timeout.as_millis()).unwrap_or(u32::MAX),
                ..ExtractLimits::default()
            }),
            // Absent unless the run asked: the sidecar then reads no picture.
            images: pictures.map(|asked| ExtractImages {
                max_images: asked.pictures,
                max_image_bytes: asked.picture_bytes,
                max_total_bytes: asked.total_bytes,
            }),
        };
        let result = self
            .call(header, Bytes::from(bytes), deadline, pictures)
            .await;
        self.slots.release(slot);
        match result {
            Ok(Extraction::Skipped(reason)) => {
                tracing::info!(
                    event = "index_document_extractor_skip",
                    filename,
                    content_type = mime,
                    reason = ?reason,
                );
                Ok(Extraction::Skipped(reason))
            }
            Ok(text) => Ok(text),
            Err(error) => {
                self.forget().await;
                Err(error)
            }
        }
    }

    /// One `Extract` call: the header, then `bytes` as slices of itself.
    async fn call(
        &self,
        header: ExtractHeader,
        bytes: Bytes,
        deadline: Duration,
        asked: Option<PictureRequest>,
    ) -> Result<Extraction, ExtractorError> {
        let total = bytes.len();
        let chunks = (0..total).step_by(CHUNK_BYTES).map(move |start| {
            let end = (start + CHUNK_BYTES).min(total);
            ExtractRequest {
                message: Some(extract_request::Message::Chunk(bytes.slice(start..end))),
            }
        });
        let messages = std::iter::once(ExtractRequest {
            message: Some(extract_request::Message::Header(header)),
        })
        .chain(chunks);
        let mut client = DocumentExtractServiceClient::new(self.channel.clone())
            .max_decoding_message_size(MAX_RESPONSE_BYTES);
        let call = client.extract(tokio_stream::iter(messages));
        let response = tokio::time::timeout(deadline, call)
            .await
            .map_err(|_| {
                ExtractorError(format!(
                    "the extractor did not answer within {} s",
                    deadline.as_secs()
                ))
            })?
            .map_err(|status| ExtractorError(format!("the extractor refused: {status}")))?
            .into_inner();
        match response.outcome {
            Some(extract_response::Outcome::Text(text)) => {
                let (pictures, dropped) = pictures_of(&text, asked);
                Ok(Extraction::Text(ExtractedDocument {
                    text: text.text,
                    pictures,
                    dropped,
                }))
            }
            Some(extract_response::Outcome::Skip(skip)) => {
                let reason = match WireSkipReason::try_from(skip.reason) {
                    Ok(WireSkipReason::TooLarge) => SkipReason::TooLarge,
                    Ok(WireSkipReason::TooManyPages) => SkipReason::TooManyPages,
                    Ok(WireSkipReason::Encrypted) => SkipReason::Encrypted,
                    Ok(WireSkipReason::Unsupported) => SkipReason::Unsupported,
                    Ok(WireSkipReason::Corrupt) => SkipReason::Corrupt,
                    Ok(WireSkipReason::Timeout) => SkipReason::Timeout,
                    Ok(WireSkipReason::OutputTooLarge) => SkipReason::OutputTooLarge,
                    Ok(WireSkipReason::Empty) => SkipReason::Empty,
                    Ok(WireSkipReason::Unspecified) | Err(_) => {
                        return Err(ExtractorError(format!(
                            "the extractor skipped with an unknown reason {}: {}",
                            skip.reason, skip.detail
                        )));
                    }
                };
                tracing::debug!(
                    event = "index_document_extractor_skip_detail",
                    reason = ?reason,
                    detail = %skip.detail
                );
                Ok(Extraction::Skipped(reason))
            }
            None => Err(ExtractorError(
                "the extractor answered without an outcome".to_owned(),
            )),
        }
    }
}

/// The pictures of an answer, held to what was asked: none when the run
/// asked for none, never more than the asked count or bytes (a sidecar
/// that returns more is not believed), a media type outside the five a
/// model takes is dropped, and every dropped one is counted.
fn pictures_of(
    text: &crate::protocol::extract::ExtractedText,
    asked: Option<PictureRequest>,
) -> (Vec<Picture>, PicturesDropped) {
    let Some(asked) = asked else {
        return (Vec::new(), PicturesDropped::default());
    };
    let said = text.images_skipped.unwrap_or_default();
    let mut dropped = PicturesDropped {
        too_many: said.too_many,
        too_large: said.too_large,
        unsupported: said.unsupported,
    };
    let mut pictures: Vec<Picture> = Vec::with_capacity(text.images.len());
    let mut total = 0_u64;
    for image in &text.images {
        let media_type = match image.media_type.as_str() {
            "image/png" => "image/png",
            "image/jpeg" => "image/jpeg",
            "image/gif" => "image/gif",
            "image/webp" => "image/webp",
            "image/bmp" => "image/bmp",
            _ => {
                dropped.unsupported += 1;
                continue;
            }
        };
        let len = image.data.len() as u64;
        if pictures.len() >= usize::try_from(asked.pictures).unwrap_or(usize::MAX) {
            dropped.too_many += 1;
            continue;
        }
        if len > asked.picture_bytes || total.saturating_add(len) > asked.total_bytes {
            dropped.too_large += 1;
            continue;
        }
        total += len;
        pictures.push(Picture {
            bytes: image.data.clone(),
            media_type,
            ordinal: image.ordinal,
            text_offset: usize::try_from(image.text_offset).unwrap_or(usize::MAX),
            name: Some(image.name.clone()).filter(|name| !name.is_empty()),
        });
    }
    (pictures, dropped)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::protocol::extract::{EmbeddedImage, ExtractedText, ImagesSkipped};

    fn advertised(timeout: u32, slot_wait: u32, idle: u32, concurrency: u32) -> Sidecar {
        Sidecar::from_capabilities(&CapabilitiesResponse {
            content_types: vec![],
            limits: Some(ExtractLimits {
                timeout_millis: timeout,
                ..ExtractLimits::default()
            }),
            version: "t".to_owned(),
            concurrency,
            slot_wait_millis: slot_wait,
            stream_idle_millis: idle,
            busy: 0,
            overruns: 0,
            image_limits: None,
        })
    }

    fn answer(images: Vec<(&str, usize)>, skipped: Option<ImagesSkipped>) -> ExtractedText {
        ExtractedText {
            text: "t".to_owned(),
            extractor: "xberg".to_owned(),
            images: images
                .into_iter()
                .enumerate()
                .map(|(n, (media_type, len))| EmbeddedImage {
                    data: Bytes::from(vec![7_u8; len]),
                    media_type: media_type.to_owned(),
                    ordinal: u32::try_from(n).expect("small"),
                    text_offset: 3,
                    page: 0,
                    name: if n == 0 {
                        "image1.png".to_owned()
                    } else {
                        String::new()
                    },
                })
                .collect(),
            images_skipped: skipped,
        }
    }

    const ASKED: PictureRequest = PictureRequest {
        pictures: 2,
        picture_bytes: 10,
        total_bytes: 15,
    };

    #[test]
    fn pictures_are_taken_only_when_asked_and_only_within_what_was_asked() {
        let said = answer(vec![("image/png", 4), ("image/jpeg", 4)], None);
        // Not asked: whatever the answer carries is ignored.
        assert_eq!(
            pictures_of(&said, None),
            (Vec::new(), PicturesDropped::default())
        );
        let (pictures, dropped) = pictures_of(&said, Some(ASKED));
        assert_eq!(pictures.len(), 2);
        assert_eq!(pictures[0].name.as_deref(), Some("image1.png"));
        assert_eq!(pictures[1].name, None);
        assert_eq!(
            (pictures[0].media_type, pictures[1].media_type),
            ("image/png", "image/jpeg")
        );
        assert_eq!(pictures[0].text_offset, 3);
        assert_eq!(dropped, PicturesDropped::default());

        // A sidecar that returns more than was asked is not believed: the
        // count, the per-picture bytes and the total each hold here too.
        let greedy = answer(
            vec![
                ("image/png", 4),
                ("image/png", 11),
                ("image/png", 4),
                ("image/png", 9),
            ],
            Some(ImagesSkipped {
                too_many: 1,
                too_large: 0,
                unsupported: 2,
            }),
        );
        let (pictures, dropped) = pictures_of(&greedy, Some(ASKED));
        // #1 fits, #2 is over 10 bytes, #3 fits (8 of 15), #4 would be
        // the third picture.
        assert_eq!(pictures.len(), 2);
        assert_eq!(
            dropped,
            PicturesDropped {
                too_many: 2,
                too_large: 1,
                unsupported: 2
            }
        );
        // The total bound: 9 + 9 > 15.
        let heavy = answer(vec![("image/png", 9), ("image/png", 9)], None);
        let (pictures, dropped) = pictures_of(&heavy, Some(ASKED));
        assert_eq!((pictures.len(), dropped.too_large), (1, 1));
        // A media type outside the five a model takes is dropped.
        let odd = answer(vec![("image/svg+xml", 4), ("image/bmp", 4)], None);
        let (pictures, dropped) = pictures_of(&odd, Some(ASKED));
        assert_eq!(pictures.len(), 1);
        assert_eq!(pictures[0].media_type, "image/bmp");
        assert_eq!(dropped.unsupported, 1);
    }

    #[test]
    fn the_deadline_is_the_sidecars_figures_plus_the_margin() {
        let sidecar = advertised(120_000, 600_000, 30_000, 4);
        assert_eq!(sidecar.concurrency, 4);
        // The run asks for more than the cap: the cap applies.
        let timeout = sidecar.effective_timeout(Duration::from_mins(5));
        assert_eq!(timeout, Duration::from_mins(2));
        assert_eq!(
            sidecar.call_deadline(timeout, Duration::from_secs(30)),
            // 120 s + 600 s + 30 s + 30 s.
            Duration::from_mins(13)
        );
        // The run asks for less: its own figure travels and bounds the call.
        let timeout = sidecar.effective_timeout(Duration::from_secs(10));
        assert_eq!(timeout, Duration::from_secs(10));
        assert_eq!(
            sidecar.call_deadline(timeout, Duration::ZERO),
            Duration::from_secs(10 + 600 + 30)
        );
    }

    #[test]
    fn zero_fields_fall_back_to_the_sidecars_defaults() {
        let sidecar = Sidecar::from_capabilities(&CapabilitiesResponse::default());
        assert_eq!(sidecar.timeout, FALLBACK_TIMEOUT);
        assert_eq!(sidecar.slot_wait, FALLBACK_SLOT_WAIT);
        assert_eq!(sidecar.stream_idle, FALLBACK_STREAM_IDLE);
        assert_eq!(sidecar.concurrency, FALLBACK_CONCURRENCY);
        let slots = Slots::new();
        slots.resize(sidecar.concurrency);
        assert_eq!(slots.available(), 1);
    }

    #[tokio::test]
    async fn the_pool_is_one_for_the_clients_life_and_resizes_around_live_permits() {
        let slots = Slots::new();
        assert_eq!(slots.available(), 0, "nothing runs before the first answer");
        // Four advertised: four permits, all taken by four calls in flight.
        slots.resize(4);
        assert_eq!(slots.available(), 4);
        let mut live: Vec<OwnedSemaphorePermit> = Vec::new();
        for _ in 0..4 {
            live.push(slots.acquire().await.expect("permit"));
        }
        assert_eq!(slots.available(), 0);
        // One call fails; the client forgets the sidecar and, on the next
        // document, learns four again while the other calls still hold
        // their permits. The OLD code made a second Semaphore(4) here, so
        // seven calls could run at once. Now: nothing is added, the pool is
        // still the four live permits.
        slots.resize(4);
        assert_eq!(
            slots.available(),
            0,
            "re-learning the same size adds no permits next to the live ones"
        );
        slots.release(live.pop().expect("one"));
        assert_eq!(slots.available(), 1, "a finished call's permit is back");
        // The restarted sidecar advertises two while three are live: the one
        // free permit is forgotten at once, the other one as a call ends.
        slots.resize(2);
        assert_eq!(slots.available(), 0);
        assert_eq!(*slots.sizing(), Sizing { size: 2, debt: 1 });
        slots.release(live.pop().expect("two"));
        assert_eq!(slots.available(), 0, "the debt is paid, not returned");
        assert_eq!(*slots.sizing(), Sizing { size: 2, debt: 0 });
        slots.release(live.pop().expect("three"));
        assert_eq!(slots.available(), 1, "live 1 + free 1 = the advertised 2");
        // Growing with a debt pending cancels the debt first, then adds.
        slots.resize(1);
        assert_eq!(*slots.sizing(), Sizing { size: 1, debt: 0 });
        assert_eq!(slots.available(), 0, "the free one was forgotten at once");
        slots.resize(0);
        assert_eq!(*slots.sizing(), Sizing { size: 0, debt: 1 });
        slots.resize(3);
        assert_eq!(*slots.sizing(), Sizing { size: 3, debt: 0 });
        assert_eq!(slots.available(), 2, "1 live + 2 free = 3");
        slots.release(live.pop().expect("four"));
        assert_eq!(slots.available(), 3);
        assert!(live.is_empty());
    }
}
