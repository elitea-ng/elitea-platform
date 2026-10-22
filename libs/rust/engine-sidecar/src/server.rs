//! The engine sidecar protocol on a Unix socket (ADR-0023 H2, ADR-0026):
//! `elitea.engine.v1.EngineService` (libs/proto/elitea/engine/v1), gRPC over
//! the socket the host dials (#1359, which replaced the NDJSON-over-HTTP
//! routes this module used to serve).
//!
//! ```text
//! Invoke(invocation_id, tool, arguments_json)
//!   → stream: thinking and token events interleaved, then one result | error
//! Stop(invocation_id)   a cooperative stop → stopped
//! Health()              status "UP", runner, active[, tools]
//! ```
//!
//! What the host reads is what it always read, one event per former NDJSON
//! line: the keys, the empty token rule, the error's Python class and legacy
//! category, the stop on a reader that goes away. One client in the host
//! (`internal/engine`) talks to every engine through it. No SPI, no
//! descriptor, no identity and no TLS live here — the socket is reachable only
//! from the host's pod (the pod-local Unix-socket exception, AGENTS.md).
//!
//! What an engine adds is an [`Engine`]: the tools it serves and how it runs
//! one. Everything about the socket is here.

use crate::proto::engine_service_server::{EngineService, EngineServiceServer};
use crate::proto::{
    EngineError as WireError, HealthRequest, HealthResponse, InvokeEvent, InvokeRequest,
    StopRequest, StopResponse, invoke_event,
};
use elitea_engine_core::errors::EngineError;
use elitea_engine_core::pyjson::dumps;
use elitea_engine_core::stream::{Context, Line, StopSignal};
use serde_json::{Map, Value};
use std::collections::HashMap;
use std::io;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, PoisonError};
use std::time::Duration;
use tokio::net::UnixListener;
use tokio::sync::{Notify, mpsc};
use tokio_stream::wrappers::{UnboundedReceiverStream, UnixListenerStream};
use tokio_stream::{Stream, StreamExt};
use tonic::{Request, Response, Status};

// The engines call `server::bind`; the binder lives in `socket` so a sidecar
// built without this module (feature `server` off) still has it.
pub use crate::socket::bind;

/// One engine behind the socket: the tools it serves and how it runs one.
///
/// The server owns the protocol — admission of the request, the invocation
/// registry, Stop, the event stream and its stop-on-disconnect — so an engine
/// implements only what is its own.
pub trait Engine: Send + Sync + 'static {
    /// The name `Health` reports as `runner` (`native`, `fixture`,
    /// `unavailable`, …).
    fn runner_name(&self) -> &'static str;

    /// Whether `tool` is one this socket serves. Anything else is refused
    /// (`INVALID_ARGUMENT`, "Unknown tool: …") before a run starts; the host
    /// serves every other tool of the application itself.
    fn serves(&self, tool: &str) -> bool;

    /// The names of the tools this socket serves, listed in `Health` as
    /// `tools` so the host can tell what an engine of any release can do
    /// before it asks (a rolling deploy runs a newer host against an older
    /// engine). Empty by default: the host then falls back to calling and
    /// recognising the refusal.
    fn tools(&self) -> &'static [&'static str] {
        &[]
    }

    /// Run one tool with the arguments the host derived, reporting progress
    /// and answer fragments through `context`.
    ///
    /// # Errors
    ///
    /// The tool's failure (the error event), or the stop error after a stop.
    fn run(
        &self,
        tool: &str,
        arguments: Map<String, Value>,
        context: &Context,
    ) -> impl Future<Output = Result<Value, EngineError>> + Send;

    /// Called with a successful result (`"success": true`) before its event
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

/// What every call shares.
struct Shared<E> {
    engine: E,
    running: Mutex<HashMap<String, StopSignal>>,
    /// Woken each time an invocation ends, for the drain.
    ended: Notify,
}

impl<E> Shared<E> {
    fn running(&self) -> std::sync::MutexGuard<'_, HashMap<String, StopSignal>> {
        // A poisoned map still holds valid stop signals; keep serving.
        self.running.lock().unwrap_or_else(PoisonError::into_inner)
    }
}

/// The default largest Invoke message the sidecar reads: 2 MiB, what the
/// HTTP sidecar admitted. An engine whose tools carry large documents
/// (Inventory's `import_graph` takes a graph the host read from a bucket: up
/// to 32 MiB, JSON-escaped once more) asks for more with
/// [`service_with_limit`] / [`serve_with_limit`].
pub const DEFAULT_INVOKE_BYTES: usize = 2 << 20;

/// The service over `engine`, with the default Invoke limit.
pub fn service<E: Engine>(engine: E) -> EngineServiceServer<Sidecar<E>> {
    service_with_limit(engine, DEFAULT_INVOKE_BYTES)
}

/// The service over `engine`; an Invoke above `max_invoke_bytes` is refused
/// (`RESOURCE_EXHAUSTED`). What it sends back is not bounded here: a result
/// carries every artifact inline, and the host bounds what it reads.
pub fn service_with_limit<E: Engine>(
    engine: E,
    max_invoke_bytes: usize,
) -> EngineServiceServer<Sidecar<E>> {
    with_limit(Sidecar::new(engine), max_invoke_bytes)
}

fn with_limit<E: Engine>(
    sidecar: Sidecar<E>,
    max_invoke_bytes: usize,
) -> EngineServiceServer<Sidecar<E>> {
    EngineServiceServer::new(sidecar)
        .max_decoding_message_size(max_invoke_bytes)
        .max_encoding_message_size(usize::MAX)
}

/// The `EngineService` implementation over one [`Engine`].
pub struct Sidecar<E>(Arc<Shared<E>>);

impl<E> Sidecar<E> {
    fn new(engine: E) -> Self {
        Self(Arc::new(Shared {
            engine,
            running: Mutex::new(HashMap::new()),
            ended: Notify::new(),
        }))
    }
}

/// One protocol event from the line a tool produced.
fn event(line: &Line) -> InvokeEvent {
    let event = match line {
        Line::Thinking(text) => invoke_event::Event::Thinking(text.clone()),
        Line::Token(text) => invoke_event::Event::Token(text.clone()),
        Line::Result(result) => invoke_event::Event::ResultJson(dumps(result).into_bytes().into()),
        Line::Error(error) => invoke_event::Event::Error(WireError {
            message: error.wire_message().to_owned(),
            error_type: error.error_type.wire_name().to_owned(),
            error_category: error.category().to_owned(),
            code: error.code.unwrap_or_default().to_owned(),
        }),
    };
    InvokeEvent { event: Some(event) }
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
        self.shared.ended.notify_waiters();
    }
}

type EventStream = Pin<Box<dyn Stream<Item = Result<InvokeEvent, Status>> + Send>>;

#[tonic::async_trait]
impl<E: Engine> EngineService for Sidecar<E> {
    type InvokeStream = EventStream;

    async fn invoke(
        &self,
        request: Request<InvokeRequest>,
    ) -> Result<Response<Self::InvokeStream>, Status> {
        let request = request.into_inner();
        let shared = &self.0;
        if request.invocation_id.is_empty() {
            return Err(Status::invalid_argument("invocation_id is required"));
        }
        if !shared.engine.serves(&request.tool) {
            return Err(Status::invalid_argument(format!(
                "Unknown tool: {}",
                request.tool
            )));
        }
        let Ok(Value::Object(arguments)) = serde_json::from_slice::<Value>(&request.arguments_json)
        else {
            return Err(Status::invalid_argument("arguments must be an object"));
        };
        let (invocation_id, tool) = (request.invocation_id, request.tool);

        let stop = StopSignal::default();
        {
            let mut running = shared.running();
            if running.contains_key(&invocation_id) {
                return Err(Status::already_exists(format!(
                    "{invocation_id} is already running"
                )));
            }
            running.insert(invocation_id.clone(), stop.clone());
        }

        let (sender, receiver) = mpsc::unbounded_channel::<Line>();
        let finished = Arc::new(AtomicBool::new(false));
        let context = Context::new(sender.clone(), stop.clone());
        let task_shared = Arc::clone(shared);
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
            shared: Arc::clone(shared),
            invocation_id,
            stop,
            finished,
        };
        let events = UnboundedReceiverStream::new(receiver).map(move |line| {
            let _keep = &guard;
            Ok(event(&line))
        });
        Ok(Response::new(Box::pin(events)))
    }

    async fn stop(&self, request: Request<StopRequest>) -> Result<Response<StopResponse>, Status> {
        let invocation_id = request.into_inner().invocation_id;
        let signal = self.0.running().get(&invocation_id).cloned();
        let stopped = signal.is_some_and(|signal| {
            signal.request();
            true
        });
        Ok(Response::new(StopResponse { stopped }))
    }

    async fn health(
        &self,
        _request: Request<HealthRequest>,
    ) -> Result<Response<HealthResponse>, Status> {
        let active = i64::try_from(self.0.running().len()).unwrap_or(i64::MAX);
        Ok(Response::new(HealthResponse {
            status: "UP".to_owned(),
            runner: self.0.engine.runner_name().to_owned(),
            active,
            tools: self
                .0
                .engine
                .tools()
                .iter()
                .map(|tool| (*tool).to_owned())
                .collect(),
        }))
    }
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

/// [`serve`] with an Invoke limit of `max_invoke_bytes`.
///
/// [`serve`] and this function do not drain: the server stops when
/// `shutdown` resolves. For a drain, use [`serve_with_options`].
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
    let options = ServeOptions {
        max_invoke_bytes,
        drain_timeout: Duration::ZERO,
    };
    serve_with_options(listener, engine, options, shutdown).await
}

/// The default drain deadline: 30 seconds, the default of the host
/// (`<PREFIX>DRAIN_TIMEOUT_SECONDS` not set).
pub const DEFAULT_DRAIN_TIMEOUT: Duration = Duration::from_secs(30);

/// How [`serve_with_options`] serves.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ServeOptions {
    /// The largest Invoke message the server reads.
    pub max_invoke_bytes: usize,
    /// How long the server continues to serve after the shutdown signal
    /// while an invocation runs. Zero: stop at once.
    pub drain_timeout: Duration,
}

impl Default for ServeOptions {
    fn default() -> Self {
        Self {
            max_invoke_bytes: DEFAULT_INVOKE_BYTES,
            drain_timeout: DEFAULT_DRAIN_TIMEOUT,
        }
    }
}

/// The drain deadline from the value of `<PREFIX>DRAIN_TIMEOUT_SECONDS`,
/// the setting that the host in the same pod reads. Each engine reads the
/// variable with its own prefix and gives the value here, so that the two
/// containers use the same value. `None` or empty: [`DEFAULT_DRAIN_TIMEOUT`].
///
/// # Errors
///
/// A value that is not a whole number of seconds.
pub fn parse_drain_timeout(value: Option<&str>) -> Result<Duration, String> {
    match value.map(str::trim) {
        None | Some("") => Ok(DEFAULT_DRAIN_TIMEOUT),
        Some(text) => text
            .parse::<u64>()
            .map(Duration::from_secs)
            .map_err(|_| format!("must be a whole number of seconds, got '{text}'")),
    }
}

/// Serve `engine` on `listener`, with a drain after `shutdown` resolves.
///
/// The kubelet sends SIGTERM to the host and to this sidecar at the same
/// time. The host continues to serve its running invocations for its drain
/// time, and it calls this socket for them (Stop, Health, new streams). The
/// container probe also connects to this socket. Thus, after `shutdown`, the
/// server continues to serve normally and accepts new connections until no
/// invocation runs (the `active` count of Health) or until
/// `options.drain_timeout` ends. Only then does the graceful stop of tonic
/// start: the server accepts no new connection, and the calls in progress
/// complete.
///
/// # Errors
///
/// The server's I/O failure.
pub async fn serve_with_options<E: Engine>(
    listener: UnixListener,
    engine: E,
    options: ServeOptions,
    shutdown: impl std::future::Future<Output = ()> + Send + 'static,
) -> io::Result<()> {
    let sidecar = Sidecar::new(engine);
    let shared = Arc::clone(&sidecar.0);
    let drained = async move {
        shutdown.await;
        drain(&shared, options.drain_timeout).await;
    };
    tonic::transport::Server::builder()
        .add_service(with_limit(sidecar, options.max_invoke_bytes))
        .serve_with_incoming_shutdown(UnixListenerStream::new(listener), drained)
        .await
        .map_err(io::Error::other)
}

/// Wait until no invocation runs, or until `timeout` ends.
async fn drain<E>(shared: &Shared<E>, timeout: Duration) {
    if timeout.is_zero() {
        return;
    }
    let running = shared.running().len();
    if running == 0 {
        return;
    }
    tracing::info!(
        running,
        timeout_seconds = timeout.as_secs(),
        "shutdown signal: the engine continues to serve until its invocations end"
    );
    let deadline = tokio::time::Instant::now() + timeout;
    loop {
        // Register for the wake-up before the count is read, so that an
        // invocation that ends between the two is not missed.
        let ended = shared.ended.notified();
        tokio::pin!(ended);
        ended.as_mut().enable();
        if shared.running().is_empty() {
            tracing::info!("drain complete: no invocation runs; the engine stops");
            return;
        }
        tokio::select! {
            () = &mut ended => {}
            () = tokio::time::sleep_until(deadline) => {
                tracing::warn!(
                    running = shared.running().len(),
                    "drain deadline: the engine stops while invocations run"
                );
                return;
            }
        }
    }
}
