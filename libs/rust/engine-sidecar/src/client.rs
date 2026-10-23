//! A client of the engine sidecar over its socket: the container probe
//! ([`crate::healthcheck`]) and the engines' tests. The host's own client is
//! Go (`services/elitea-subapp-host/internal/engine`).
//!
//! [`EngineClient::invoke`] collects a run as the JSON documents the NDJSON
//! protocol carried — `{"thinking": …}`, `{"token": …}`, `{"result": …}`,
//! `{"error": {message, error_type, error_category[, code]}}` — so what a
//! test asserts about a run reads the same as the lines it used to read.

use crate::proto::engine_service_client::EngineServiceClient;
use crate::proto::{HealthRequest, InvokeEvent, InvokeRequest, StopRequest, invoke_event};
use hyper_util::rt::TokioIo;
use serde_json::{Value, json};
use std::path::Path;
use tokio::net::UnixStream;
use tonic::transport::{Channel, Endpoint, Uri};
use tonic::{Status, Streaming};
use tower::service_fn;

/// What the client reads in one message: a result carries every artifact
/// inline (the host reads up to 64 MiB; so does this client).
pub const MAX_RESULT_BYTES: usize = 64 << 20;

/// A connected client of one sidecar.
#[derive(Clone)]
pub struct EngineClient {
    inner: EngineServiceClient<Channel>,
}

impl EngineClient {
    /// Connect to the sidecar at `socket`.
    ///
    /// # Errors
    ///
    /// A message naming the socket and the failure.
    pub async fn connect(socket: &Path) -> Result<Self, String> {
        let path = socket.to_path_buf();
        // HTTP/2 needs an authority; nothing routes on it.
        let channel = Endpoint::try_from("http://engine.local")
            .map_err(|error| error.to_string())?
            .connect_with_connector(service_fn(move |_: Uri| {
                let path = path.clone();
                async move { UnixStream::connect(path).await.map(TokioIo::new) }
            }))
            .await
            .map_err(|error| format!("cannot connect to {}: {error}", socket.display()))?;
        Ok(Self {
            inner: EngineServiceClient::new(channel)
                .max_decoding_message_size(MAX_RESULT_BYTES)
                .max_encoding_message_size(usize::MAX),
        })
    }

    /// Start one run and return its event stream.
    ///
    /// # Errors
    ///
    /// The sidecar's refusal (an unknown tool, a repeated id, …).
    pub async fn start(
        &mut self,
        invocation_id: &str,
        tool: &str,
        arguments: &Value,
    ) -> Result<Streaming<InvokeEvent>, Status> {
        let request = InvokeRequest {
            invocation_id: invocation_id.to_owned(),
            tool: tool.to_owned(),
            arguments_json: serde_json::to_vec(arguments)
                .map_err(|error| Status::invalid_argument(error.to_string()))?
                .into(),
        };
        Ok(self.inner.invoke(request).await?.into_inner())
    }

    /// Run one tool to its end and return every event as its JSON document.
    ///
    /// # Errors
    ///
    /// The sidecar's refusal, or a stream that failed.
    pub async fn invoke(
        &mut self,
        invocation_id: &str,
        tool: &str,
        arguments: &Value,
    ) -> Result<Vec<Value>, Status> {
        let mut stream = self.start(invocation_id, tool, arguments).await?;
        let mut lines = Vec::new();
        while let Some(event) = stream.message().await? {
            lines.push(event_json(&event));
        }
        Ok(lines)
    }

    /// Ask a running invocation to stop; false when none with that id runs.
    ///
    /// # Errors
    ///
    /// The call's failure.
    pub async fn stop(&mut self, invocation_id: &str) -> Result<bool, Status> {
        let request = StopRequest {
            invocation_id: invocation_id.to_owned(),
        };
        Ok(self.inner.stop(request).await?.into_inner().stopped)
    }

    /// The health document as JSON: `{"status", "runner", "active"[, "tools"]}`,
    /// `tools` left out when the engine lists none.
    ///
    /// # Errors
    ///
    /// The call's failure.
    pub async fn health(&mut self) -> Result<Value, Status> {
        let health = self.inner.health(HealthRequest {}).await?.into_inner();
        let mut document =
            json!({"status": health.status, "runner": health.runner, "active": health.active});
        if !health.tools.is_empty() {
            document["tools"] = json!(health.tools);
        }
        Ok(document)
    }
}

/// One event as the JSON document the NDJSON protocol carried.
#[must_use]
pub fn event_json(event: &InvokeEvent) -> Value {
    match &event.event {
        Some(invoke_event::Event::Thinking(text)) => json!({ "thinking": text }),
        Some(invoke_event::Event::Token(text)) => json!({ "token": text }),
        Some(invoke_event::Event::ResultJson(raw)) => json!({
            "result": serde_json::from_slice::<Value>(raw).unwrap_or(Value::Null)
        }),
        Some(invoke_event::Event::Error(error)) => {
            let mut fields = json!({
                "message": error.message,
                "error_type": error.error_type,
                "error_category": error.error_category,
            });
            if !error.code.is_empty() {
                fields["code"] = json!(error.code);
            }
            json!({ "error": fields })
        }
        None => Value::Null,
    }
}
