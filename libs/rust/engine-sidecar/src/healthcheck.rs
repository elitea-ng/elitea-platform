//! `elitea-deepwiki-engine healthcheck`: the container probe.
//!
//! The runtime image is distroless — no shell, no Python, no curl — so the
//! binary probes itself: it connects to the socket and reads
//! `GET /engine/health`. Success is HTTP 200 with `status: "UP"`, which
//! proves the server answers, not merely that a socket file exists.

use http_body_util::{BodyExt, Empty};
use hyper::body::Bytes;
use hyper_util::rt::TokioIo;
use std::path::Path;
use std::time::Duration;
use tokio::net::UnixStream;

/// Probe the sidecar at `socket`.
///
/// # Errors
///
/// A message naming what failed: the connection, the exchange, the status or
/// the body.
pub async fn probe(socket: &Path, timeout: Duration) -> Result<(), String> {
    tokio::time::timeout(timeout, exchange(socket))
        .await
        .map_err(|_| format!("no answer from {} within {timeout:?}", socket.display()))?
}

async fn exchange(socket: &Path) -> Result<(), String> {
    let stream = UnixStream::connect(socket)
        .await
        .map_err(|error| format!("cannot connect to {}: {error}", socket.display()))?;
    let (mut sender, connection) = hyper::client::conn::http1::handshake(TokioIo::new(stream))
        .await
        .map_err(|error| format!("handshake failed: {error}"))?;
    tokio::spawn(connection);
    let request = hyper::Request::get("/engine/health")
        .header(hyper::header::HOST, "engine")
        .body(Empty::<Bytes>::new())
        .map_err(|error| format!("cannot build the request: {error}"))?;
    let response = sender
        .send_request(request)
        .await
        .map_err(|error| format!("request failed: {error}"))?;
    let status = response.status();
    let body = response
        .into_body()
        .collect()
        .await
        .map_err(|error| format!("cannot read the body: {error}"))?
        .to_bytes();
    if !status.is_success() {
        return Err(format!("GET /engine/health answered {status}"));
    }
    let document: serde_json::Value = serde_json::from_slice(&body)
        .map_err(|error| format!("the health document is not JSON: {error}"))?;
    if document.get("status").and_then(serde_json::Value::as_str) == Some("UP") {
        Ok(())
    } else {
        Err(format!("the engine reports {document}"))
    }
}
