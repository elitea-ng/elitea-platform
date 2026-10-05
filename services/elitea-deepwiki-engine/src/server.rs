//! The engine sidecar protocol on a Unix socket (ADR-0023 H2, ADR-0026).
//!
//! ```text
//! POST /engine/invoke                  {invocation_id, tool, arguments}
//!   → application/x-ndjson: {"thinking": …} and {"token": …} interleaved,
//!     then {"result": …} | {"error": …}
//! POST /engine/invocations/{id}/stop   a cooperative stop → 202 {"stopped": bool}
//! GET  /engine/health                  {"status": "UP", "runner": …, "active": n}
//! ```
//!
//! The wire is the Python sidecar's (`elitea_deepwiki/sidecar.py`) byte for
//! byte where the host can see it: routes, status codes, keys, the empty
//! token rule and the stop line. No SPI, no descriptor and no identity
//! headers live here — the socket is reachable only from the host's pod.

use crate::pyjson::dumps;
use crate::runner::{Context, ENGINE_TOOLS, Line, Runner, StopSignal};
use axum::Router;
use axum::body::{Body, Bytes};
use axum::extract::{Path as UrlPath, State};
use axum::http::{StatusCode, header};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use serde_json::{Value, json};
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

/// What every request handler shares.
#[derive(Debug)]
struct Shared {
    runner: Runner,
    running: Mutex<HashMap<String, StopSignal>>,
}

impl Shared {
    fn running(&self) -> std::sync::MutexGuard<'_, HashMap<String, StopSignal>> {
        // A poisoned map still holds valid stop signals; keep serving.
        self.running.lock().unwrap_or_else(PoisonError::into_inner)
    }
}

/// The sidecar's routes over `runner`.
pub fn router(runner: Runner) -> Router {
    let shared = Arc::new(Shared {
        runner,
        running: Mutex::new(HashMap::new()),
    });
    Router::new()
        .route("/engine/health", get(health))
        .route("/engine/invoke", post(invoke))
        .route("/engine/invocations/{invocation_id}/stop", post(stop))
        .fallback(|| async { detail(StatusCode::NOT_FOUND, "Not Found") })
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

async fn health(State(shared): State<Arc<Shared>>) -> Response {
    let active = shared.running().len();
    json_response(
        StatusCode::OK,
        &json!({"status": "UP", "runner": shared.runner.name(), "active": active}),
    )
}

async fn stop(
    State(shared): State<Arc<Shared>>,
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
struct StreamGuard {
    shared: Arc<Shared>,
    invocation_id: String,
    stop: StopSignal,
    finished: Arc<AtomicBool>,
}

impl Drop for StreamGuard {
    fn drop(&mut self) {
        if !self.finished.load(Ordering::SeqCst) {
            self.stop.request();
        }
        self.shared.running().remove(&self.invocation_id);
    }
}

/// Python's `f"{value}"` for the refusal message.
fn shown(value: Option<&Value>) -> String {
    value.map_or_else(|| "None".to_owned(), crate::source::py_str)
}

async fn invoke(State(shared): State<Arc<Shared>>, body: Bytes) -> Response {
    let Ok(Value::Object(request)) = serde_json::from_slice::<Value>(&body) else {
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
        Some(Value::String(tool)) if ENGINE_TOOLS.contains(&tool.as_str()) => tool.clone(),
        other => {
            return detail(
                StatusCode::BAD_REQUEST,
                &format!("Unknown tool: {}", shown(other)),
            );
        }
    };
    let Some(Value::Object(arguments)) = request.get("arguments").cloned() else {
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
        let outcome = task_shared.runner.run(&tool, arguments, &context).await;
        let line = match outcome {
            Ok(result) => {
                if tool == "generate_wiki" && result.get("success") == Some(&Value::Bool(true)) {
                    task_shared.runner.publish(&result, &context).await;
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

/// Serve `runner` on `listener` until `shutdown` resolves.
///
/// # Errors
///
/// The server's I/O failure.
pub async fn serve(
    listener: UnixListener,
    runner: Runner,
    shutdown: impl std::future::Future<Output = ()> + Send + 'static,
) -> io::Result<()> {
    axum::serve(listener, router(runner))
        .with_graceful_shutdown(shutdown)
        .await
}
