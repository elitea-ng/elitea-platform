//! `elitea-<engine>-engine healthcheck`: the container probe.
//!
//! The runtime image is distroless — no shell, no Python, no curl — so the
//! binary probes itself: it connects to the socket and calls
//! `EngineService.Health`. Success is a status of `"UP"`, which proves the
//! server answers, not merely that a socket file exists.

use crate::client::EngineClient;
use std::path::Path;
use std::time::Duration;

/// Probe the sidecar at `socket`.
///
/// # Errors
///
/// A message naming what failed: the connection, the call or the status.
pub async fn probe(socket: &Path, timeout: Duration) -> Result<(), String> {
    tokio::time::timeout(timeout, exchange(socket))
        .await
        .map_err(|_| format!("no answer from {} within {timeout:?}", socket.display()))?
}

async fn exchange(socket: &Path) -> Result<(), String> {
    let health = EngineClient::connect(socket)
        .await?
        .health()
        .await
        .map_err(|status| format!("Health failed: {status}"))?;
    if health.get("status").and_then(serde_json::Value::as_str) == Some("UP") {
        Ok(())
    } else {
        Err(format!("the engine reports {health}"))
    }
}
