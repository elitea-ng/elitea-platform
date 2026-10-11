//! The engine sidecar protocol on a Unix socket (ADR-0023 H2, ADR-0026).
//!
//! ```text
//! POST /engine/invoke                  {invocation_id, tool, arguments}
//!   → application/x-ndjson: {"thinking": …} and {"token": …} interleaved,
//!     then {"result": …} | {"error": …}
//! POST /engine/invocations/{id}/stop   a cooperative stop → 202 {"stopped": bool}
//! GET  /engine/health                  {"status": "UP", "runner": …, "active": n[, "tools": […]]}
//! ```
//!
//! The wire is the Python sidecars' (`elitea_deepwiki/sidecar.py`, the
//! Inventory `sidecar.py`) byte for byte where the host can see it: routes,
//! status codes, keys, the empty token rule and the stop line. One client in
//! the host (`internal/engine`) talks to every engine through it. No SPI, no
//! descriptor and no identity headers live here — the socket is reachable
//! only from the host's pod.
//!
//! What an engine adds is an [`Engine`]: the tools it serves and how it runs
//! one. Everything about the socket is here.

use axum::Router;
use axum::body::{Body, Bytes};
use axum::extract::{DefaultBodyLimit, Path as UrlPath, State};
use axum::http::{StatusCode, header};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use elitea_engine_core::errors::EngineError;
use elitea_engine_core::pyjson::dumps;
use elitea_engine_core::pyvalue::py_str;
use elitea_engine_core::stream::{Context, Line, StopSignal};
use serde_json::{Map, Value, json};
use std::collections::HashMap;
use std::io;
use std::os::unix::fs::{FileTypeExt, PermissionsExt};
use std::path::Path;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, PoisonError};
use tokio::net::UnixListener;
use tokio::sync::mpsc;
use tokio_stream::StreamExt;
use tokio_stream::wrappers::UnboundedReceiverStream;

/// One engine behind the socket: the tools it serves and how it runs one.
///
/// The server owns the protocol — admission of the request, the invocation
/// registry, the stop route, the NDJSON stream and its stop-on-disconnect —
/// so an engine implements only what is its own.
pub trait Engine: Send + Sync + 'static {
    /// The name `GET /engine/health` reports as `runner` (`native`,
    /// `fixture`, `unavailable`, …).
    fn runner_name(&self) -> &'static str;

    /// Whether `tool` is one this socket serves. Anything else is refused
    /// with `400 Unknown tool` before a run starts; the host serves every
    /// other tool of the application itself.
    fn serves(&self, tool: &str) -> bool;

    /// The names of the tools this socket serves, listed in
    /// `GET /engine/health` as `tools` so the host can tell what an engine
    /// of any release can do before it asks (a rolling deploy runs a newer
    /// host against an older engine). Empty by default, and an empty list is
    /// left out of the document, so an engine that does not list its tools
    /// answers byte for byte what it always did.
    fn tools(&self) -> &'static [&'static str] {
        &[]
    }

    /// Run one tool with the arguments the host derived, reporting progress
    /// and answer fragments through `context`.
    ///
    /// # Errors
    ///
    /// The tool's failure (the error line), or the stop line after a stop.
    fn run(
        &self,
        tool: &str,
        arguments: Map<String, Value>,
        context: &Context,
    ) -> impl Future<Output = Result<Value, EngineError>> + Send;

    /// Called with a successful result (`"success": true`) before its line
    /// is sent, for work that must be done by then — publishing what the
    /// result points at. Nothing by default.
    fn after_success(
        &self,
        tool: &str,
        result: &Value,
        context: &Context,
    ) -> impl Future<Output = ()> + Send {
        let _ = (tool, result, context);
        async {}
    }
}

/// What every request handler shares.
struct Shared<E> {
    engine: E,
    running: Mutex<HashMap<String, StopSignal>>,
}

impl<E> Shared<E> {
    fn running(&self) -> std::sync::MutexGuard<'_, HashMap<String, StopSignal>> {
        // A poisoned map still holds valid stop signals; keep serving.
        self.running.lock().unwrap_or_else(PoisonError::into_inner)
    }
}

/// The default largest invoke body the sidecar reads: axum's own 2 MB. An
/// engine whose tools carry large documents (Inventory's `import_graph`
/// takes a graph the host read from a bucket: up to 32 MiB, JSON-escaped once
/// more) asks for more with [`router_with_limit`] / [`serve_with_limit`].
pub const DEFAULT_INVOKE_BYTES: usize = 2 << 20;

/// The sidecar's routes over `engine`, with the default body limit.
pub fn router<E: Engine>(engine: E) -> Router {
    router_with_limit(engine, DEFAULT_INVOKE_BYTES)
}

/// The sidecar's routes over `engine`; an invoke body above `max_invoke_bytes`
/// is refused (413). The socket is the host's alone.
pub fn router_with_limit<E: Engine>(engine: E, max_invoke_bytes: usize) -> Router {
    let shared = Arc::new(Shared {
        engine,
        running: Mutex::new(HashMap::new()),
    });
    Router::new()
        .route("/engine/health", get(health::<E>))
        .route("/engine/invoke", post(invoke::<E>))
        .route("/engine/invocations/{invocation_id}/stop", post(stop::<E>))
        .fallback(|| async { detail(StatusCode::NOT_FOUND, "Not Found") })
        .layer(DefaultBodyLimit::max(max_invoke_bytes))
        .with_state(shared)
}

fn json_response(status: StatusCode, body: &Value) -> Response {
    (
        status,
        [(header::CONTENT_TYPE, "application/json")],
        dumps(body),
    )
        .into_response()
}

/// The error body `FastAPI` sent, which the Python sidecar answered with.
fn detail(status: StatusCode, message: &str) -> Response {
    json_response(status, &json!({ "detail": message }))
}

async fn health<E: Engine>(State(shared): State<Arc<Shared<E>>>) -> Response {
    let active = shared.running().len();
    let mut body =
        json!({"status": "UP", "runner": shared.engine.runner_name(), "active": active});
    let tools = shared.engine.tools();
    if !tools.is_empty() {
        body["tools"] = json!(tools);
    }
    json_response(StatusCode::OK, &body)
}

async fn stop<E: Engine>(
    State(shared): State<Arc<Shared<E>>>,
    UrlPath(invocation_id): UrlPath<String>,
) -> Response {
    let signal = shared.running().get(&invocation_id).cloned();
    let stopped = signal.is_some_and(|signal| {
        signal.request();
        true
    });
    json_response(StatusCode::ACCEPTED, &json!({ "stopped": stopped }))
}

/// Ends an invocation's registration when its stream is dropped — finished
/// or abandoned. A reader that went away mid-run is a stop: nothing will
/// read the result.
struct StreamGuard<E> {
    shared: Arc<Shared<E>>,
    invocation_id: String,
    stop: StopSignal,
    finished: Arc<AtomicBool>,
}

impl<E> Drop for StreamGuard<E> {
    fn drop(&mut self) {
        if !self.finished.load(Ordering::SeqCst) {
            self.stop.request();
        }
        self.shared.running().remove(&self.invocation_id);
    }
}

/// Python's `f"{value}"` for the refusal message.
fn shown(value: Option<&Value>) -> String {
    value.map_or_else(|| "None".to_owned(), py_str)
}

async fn invoke<E: Engine>(State(shared): State<Arc<Shared<E>>>, body: Bytes) -> Response {
    let Ok(Value::Object(mut request)) = serde_json::from_slice::<Value>(&body) else {
        return detail(
            StatusCode::UNPROCESSABLE_ENTITY,
            "the request body must be a JSON object",
        );
    };
    let invocation_id = match request.get("invocation_id") {
        Some(Value::String(id)) if !id.is_empty() => id.clone(),
        _ => return detail(StatusCode::BAD_REQUEST, "invocation_id is required"),
    };
    let tool = match request.get("tool") {
        Some(Value::String(tool)) if shared.engine.serves(tool) => tool.clone(),
        other => {
            return detail(
                StatusCode::BAD_REQUEST,
                &format!("Unknown tool: {}", shown(other)),
            );
        }
    };
    let Some(Value::Object(arguments)) = request.remove("arguments") else {
        return detail(StatusCode::BAD_REQUEST, "arguments must be an object");
    };

    let stop = StopSignal::default();
    {
        let mut running = shared.running();
        if running.contains_key(&invocation_id) {
            return detail(
                StatusCode::CONFLICT,
                &format!("{invocation_id} is already running"),
            );
        }
        running.insert(invocation_id.clone(), stop.clone());
    }

    let (sender, receiver) = mpsc::unbounded_channel::<Line>();
    let finished = Arc::new(AtomicBool::new(false));
    let context = Context::new(sender.clone(), stop.clone());
    let task_shared = Arc::clone(&shared);
    let task_finished = Arc::clone(&finished);
    tokio::spawn(async move {
        let outcome = task_shared.engine.run(&tool, arguments, &context).await;
        let line = match outcome {
            Ok(result) => {
                if result.get("success") == Some(&Value::Bool(true)) {
                    task_shared
                        .engine
                        .after_success(&tool, &result, &context)
                        .await;
                }
                Line::Result(result)
            }
            Err(error) => {
                tracing::warn!(tool = %tool, error_type = error.error_type.wire_name(), "engine tool failed");
                Line::Error(error)
            }
        };
        task_finished.store(true, Ordering::SeqCst);
        let _ = sender.send(line);
    });

    let guard = StreamGuard {
        shared,
        invocation_id,
        stop,
        finished,
    };
    let lines = UnboundedReceiverStream::new(receiver).map(move |line| {
        let _keep = &guard;
        Ok::<_, io::Error>(Bytes::from(dumps(&line.to_json()) + "\n"))
    });
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "application/x-ndjson")
        .body(Body::from_stream(lines))
        .unwrap_or_else(|_| detail(StatusCode::INTERNAL_SERVER_ERROR, "Internal Server Error"))
}

/// Bind the socket the host dials.
///
/// The parent directory is created; a stale SOCKET at the path is removed,
/// anything else at the path is refused rather than deleted. The socket is
/// made world-writable: the host's container runs as another non-root user
/// and connecting needs write permission. The shared directory (an emptyDir
/// or compose volume) is the only thing that scopes who reaches it.
///
/// # Errors
///
/// Any filesystem or bind failure, or a non-socket file at the path.
pub fn bind(path: &Path) -> io::Result<UnixListener> {
    if let Some(parent) = path.parent().filter(|p| !p.as_os_str().is_empty()) {
        std::fs::create_dir_all(parent)?;
    }
    match std::fs::symlink_metadata(path) {
        Ok(meta) if meta.file_type().is_socket() => std::fs::remove_file(path)?,
        Ok(_) => {
            return Err(io::Error::new(
                io::ErrorKind::AlreadyExists,
                format!("{} exists and is not a socket", path.display()),
            ));
        }
        Err(error) if error.kind() == io::ErrorKind::NotFound => {}
        Err(error) => return Err(error),
    }
    let listener = UnixListener::bind(path)?;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o777))?;
    Ok(listener)
}

/// Serve `engine` on `listener` until `shutdown` resolves.
///
/// # Errors
///
/// The server's I/O failure.
pub async fn serve<E: Engine>(
    listener: UnixListener,
    engine: E,
    shutdown: impl std::future::Future<Output = ()> + Send + 'static,
) -> io::Result<()> {
    serve_with_limit(listener, engine, DEFAULT_INVOKE_BYTES, shutdown).await
}

/// [`serve`] with an invoke body limit of `max_invoke_bytes`.
///
/// # Errors
///
/// The server's I/O failure.
pub async fn serve_with_limit<E: Engine>(
    listener: UnixListener,
    engine: E,
    max_invoke_bytes: usize,
    shutdown: impl std::future::Future<Output = ()> + Send + 'static,
) -> io::Result<()> {
    axum::serve(listener, router_with_limit(engine, max_invoke_bytes))
        .with_graceful_shutdown(shutdown)
        .await
}
