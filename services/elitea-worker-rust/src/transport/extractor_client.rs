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
//! Nothing here is a statement about the document except what the sidecar
//! said: a transport failure, a gRPC status and a response without an
//! outcome are all [`ExtractorError`], which the run records as
//! `extract_failed`.

use std::path::{Path, PathBuf};
use std::time::Duration;

use hyper_util::rt::TokioIo;
use tokio::net::UnixStream;
use tonic::transport::{Channel, Endpoint, Uri};
use tower::service_fn;

use crate::protocol::extract::document_extract_service_client::DocumentExtractServiceClient;
use crate::protocol::extract::{
    ExtractHeader, ExtractLimits, ExtractRequest, SkipReason as WireSkipReason, extract_request,
    extract_response,
};

/// Bytes per streamed chunk; the sidecar's message limit is 4 MiB.
const CHUNK_BYTES: usize = 256 * 1024;
/// The whole call: the sidecar's default extraction timeout (120 s) plus
/// the stream and a slot wait.
const CALL_TIMEOUT: Duration = Duration::from_mins(5);
/// The largest answer accepted: the sidecar's default output cap is 20 MiB.
const MAX_RESPONSE_BYTES: usize = 64 << 20;

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

/// What the sidecar answered.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum Extraction {
    Text(String),
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

/// The client. Clone-cheap: one channel, shared.
#[derive(Clone)]
pub(crate) struct ExtractorClient {
    channel: Channel,
    socket: PathBuf,
    call_timeout: Duration,
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
            call_timeout: CALL_TIMEOUT,
        })
    }

    /// The same client with another call timeout (tests).
    #[cfg(test)]
    pub(crate) fn with_call_timeout(mut self, call_timeout: Duration) -> Self {
        self.call_timeout = call_timeout;
        self
    }

    /// The socket this client dials.
    pub(crate) fn socket(&self) -> &Path {
        &self.socket
    }

    /// `bytes` of media type `mime`, named `filename`, as the sidecar reads
    /// them; `max_input_bytes` is this run's document cap.
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
    ) -> Result<Extraction, ExtractorError> {
        let header = ExtractHeader {
            filename: filename.to_owned(),
            content_type: mime.to_owned(),
            content_length: bytes.len() as u64,
            limits: Some(ExtractLimits {
                max_input_bytes,
                ..ExtractLimits::default()
            }),
        };
        let mut messages = Vec::with_capacity(1 + bytes.len() / CHUNK_BYTES + 1);
        messages.push(ExtractRequest {
            message: Some(extract_request::Message::Header(header)),
        });
        messages.extend(bytes.chunks(CHUNK_BYTES).map(|chunk| ExtractRequest {
            message: Some(extract_request::Message::Chunk(chunk.to_vec())),
        }));
        drop(bytes);
        let mut client = DocumentExtractServiceClient::new(self.channel.clone())
            .max_decoding_message_size(MAX_RESPONSE_BYTES);
        let call = client.extract(tokio_stream::iter(messages));
        let response = tokio::time::timeout(self.call_timeout, call)
            .await
            .map_err(|_| {
                ExtractorError(format!(
                    "the extractor did not answer within {} s",
                    self.call_timeout.as_secs()
                ))
            })?
            .map_err(|status| ExtractorError(format!("the extractor refused: {status}")))?
            .into_inner();
        match response.outcome {
            Some(extract_response::Outcome::Text(text)) => Ok(Extraction::Text(text.text)),
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
                tracing::info!(
                    event = "index_document_extractor_skip",
                    filename,
                    content_type = mime,
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
