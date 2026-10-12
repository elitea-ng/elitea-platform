//! A client of the extractor over its Unix socket: `healthcheck` and the
//! tests. The worker carries its own (`transport/extractor_client.rs`); this
//! one is the reference the socket is proven against.

use std::path::{Path, PathBuf};
use std::time::Duration;

use hyper_util::rt::TokioIo;
use tokio::net::UnixStream;
use tonic::transport::{Channel, Endpoint, Uri};
use tower::service_fn;

use crate::pb::document_extract_service_client::DocumentExtractServiceClient;
use crate::pb::{CapabilitiesRequest, CapabilitiesResponse, ExtractRequest, ExtractResponse};

/// Bytes per streamed chunk.
pub const CHUNK_BYTES: usize = 256 * 1024;

/// A lazily connected channel to the socket at `path`. The authority is a
/// placeholder: HTTP/2 needs one, and nothing routes on it.
///
/// # Errors
///
/// The placeholder endpoint could not be built (it cannot happen for a
/// constant; the error type is tonic's).
pub fn channel(path: &Path) -> Result<Channel, tonic::transport::Error> {
    let path: PathBuf = path.to_path_buf();
    Ok(Endpoint::try_from("http://extractor.local")?
        .connect_timeout(Duration::from_secs(5))
        .connect_with_connector_lazy(service_fn(move |_: Uri| {
            let path = path.clone();
            async move { UnixStream::connect(path).await.map(TokioIo::new) }
        })))
}

/// `Capabilities` over `channel`.
///
/// # Errors
///
/// The gRPC status.
pub async fn capabilities(channel: Channel) -> Result<CapabilitiesResponse, tonic::Status> {
    let mut client = DocumentExtractServiceClient::new(channel);
    client
        .capabilities(CapabilitiesRequest {})
        .await
        .map(tonic::Response::into_inner)
}

/// `Extract` over `channel`: `header`, then `bytes` in [`CHUNK_BYTES`]
/// chunks.
///
/// # Errors
///
/// The gRPC status.
pub async fn extract(
    channel: Channel,
    header: crate::pb::ExtractHeader,
    bytes: Vec<u8>,
) -> Result<ExtractResponse, tonic::Status> {
    let mut client = DocumentExtractServiceClient::new(channel);
    let mut messages = vec![ExtractRequest {
        message: Some(crate::pb::extract_request::Message::Header(header)),
    }];
    messages.extend(bytes.chunks(CHUNK_BYTES).map(|chunk| ExtractRequest {
        message: Some(crate::pb::extract_request::Message::Chunk(chunk.to_vec())),
    }));
    client
        .extract(tokio_stream::iter(messages))
        .await
        .map(tonic::Response::into_inner)
}
