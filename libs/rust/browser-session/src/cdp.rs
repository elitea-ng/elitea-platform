//! A thin Chrome `DevTools` Protocol client over the engine's debugging pipe.
//!
//! The engine runs with `--remote-debugging-pipe`: it reads commands from
//! its fd 3 and writes replies and events to its fd 4, one JSON message per
//! NUL-terminated record. There is no TCP or WebSocket listener anywhere, so
//! CDP cannot be reached from outside the agent process, not even from the
//! pod's loopback (ADR-0049 decision 4, "CDP never leaves the pod").
//!
//! The client is deliberately small: a command is `{id, method, params,
//! sessionId?}` and its reply carries the same `id`; everything else is an
//! event, fanned out on a broadcast channel. The driver names the methods it
//! uses; there is no generated protocol binding to keep in step with Chrome.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde_json::{Value, json};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::sync::{broadcast, oneshot};

/// The largest message read from the engine. A full accessibility tree of a
/// very large page and a full-viewport PNG are the big ones, both well under
/// this; a larger record means something is wrong, and the connection ends.
pub const MAX_MESSAGE_BYTES: usize = 128 << 20;

/// JSON-RPC's "Internal error": the engine failed in itself. Protocol
/// errors about the page (no such node, not attached) use other codes.
pub const INTERNAL_ERROR: i64 = -32603;

/// How long one command may take before it fails (`timeout`).
pub const COMMAND_TIMEOUT: Duration = Duration::from_secs(30);

const EVENT_CAPACITY: usize = 4096;

tokio::task_local! {
    /// The faults of the tool call running on this task
    /// ([`scoped_faults`]). Background traffic (screencast acks, the page
    /// tracker's replies, `Fetch.enable` on new targets) runs on tasks of
    /// its own, outside any scope, so it is never counted against a call.
    static CALL_FAULTS: Arc<Faults>;
}

#[derive(Default)]
struct Faults {
    unsupported: AtomicU64,
    timeouts: AtomicU64,
    closed: AtomicU64,
    engine_errors: AtomicU64,
}

/// What went wrong with the commands one tool call sent itself.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct CallFaults {
    /// Answered as unknown or unsupported ([`is_unsupported`]).
    pub unsupported: u64,
    /// Not answered within [`COMMAND_TIMEOUT`].
    pub timeouts: u64,
    /// Failed because the connection was gone.
    pub closed: u64,
    /// Answered with JSON-RPC's internal error ([`INTERNAL_ERROR`]): the
    /// engine's own fault, not the page's.
    pub engine_errors: u64,
}

/// Run `future` (one tool call) and count the faults of the commands IT
/// sent. Only commands awaited on this task count: a command another task
/// sends at the same time does not, even on the same connection.
pub async fn scoped_faults<F: std::future::Future>(future: F) -> (F::Output, CallFaults) {
    let faults = Arc::new(Faults::default());
    let output = CALL_FAULTS.scope(Arc::clone(&faults), future).await;
    let counted = CallFaults {
        unsupported: faults.unsupported.load(Ordering::Relaxed),
        timeouts: faults.timeouts.load(Ordering::Relaxed),
        closed: faults.closed.load(Ordering::Relaxed),
        engine_errors: faults.engine_errors.load(Ordering::Relaxed),
    };
    (output, counted)
}

/// Count a fault against the call running on this task, if any.
fn count_fault(pick: fn(&Faults) -> &AtomicU64) {
    let _ = CALL_FAULTS.try_with(|faults| pick(faults).fetch_add(1, Ordering::Relaxed));
}

/// One CDP event.
#[derive(Debug)]
pub struct Event {
    pub method: String,
    pub params: Value,
    /// The flattened target session it came from; `None` for the browser.
    pub session_id: Option<String>,
}

/// Why a command failed.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum CdpError {
    /// The engine answered with a protocol error.
    Protocol { code: i64, message: String },
    /// No answer within [`COMMAND_TIMEOUT`].
    Timeout,
    /// The pipe closed: the engine exited or the reader stopped.
    Closed,
}

impl std::fmt::Display for CdpError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Protocol { code, message } => write!(formatter, "CDP error {code}: {message}"),
            Self::Timeout => formatter.write_str("the browser did not answer in time"),
            Self::Closed => formatter.write_str("the browser connection closed"),
        }
    }
}

impl std::error::Error for CdpError {}

type Pending = HashMap<u64, oneshot::Sender<Result<Value, CdpError>>>;

struct Shared {
    pending: Mutex<Option<Pending>>,
    events: broadcast::Sender<Arc<Event>>,
    /// Commands the engine answered as a method or feature it does not
    /// implement ([`is_unsupported`]), over the whole connection, for the
    /// record. `auto` switches on [`scoped_faults`], not this.
    unsupported: AtomicU64,
}

impl Shared {
    fn close(&self) {
        let pending = self
            .pending
            .lock()
            .ok()
            .and_then(|mut pending| pending.take());
        for (_, reply) in pending.into_iter().flatten() {
            let _ = reply.send(Err(CdpError::Closed));
        }
    }
}

/// A CDP connection. Cheap to clone.
#[derive(Clone)]
pub struct Connection {
    shared: Arc<Shared>,
    writer: Arc<tokio::sync::Mutex<Box<dyn AsyncWrite + Send + Unpin>>>,
    next_id: Arc<AtomicU64>,
}

impl Connection {
    /// A connection over `reader` (the engine's fd 4) and `writer` (its
    /// fd 3). Spawns the reader task on the current runtime.
    pub fn new<R, W>(reader: R, writer: W) -> Self
    where
        R: AsyncRead + Send + Unpin + 'static,
        W: AsyncWrite + Send + Unpin + 'static,
    {
        let (events, _) = broadcast::channel(EVENT_CAPACITY);
        let shared = Arc::new(Shared {
            pending: Mutex::new(Some(HashMap::new())),
            events,
            unsupported: AtomicU64::new(0),
        });
        tokio::spawn(read_loop(reader, Arc::clone(&shared)));
        Self {
            shared,
            writer: Arc::new(tokio::sync::Mutex::new(Box::new(writer))),
            next_id: Arc::new(AtomicU64::new(1)),
        }
    }

    /// Every event from now on. Subscribe before sending the command whose
    /// events you wait for.
    #[must_use]
    pub fn events(&self) -> broadcast::Receiver<Arc<Event>> {
        self.shared.events.subscribe()
    }

    /// Send `method` to the browser (`session` `None`) or to a flattened
    /// target session, and wait for its result.
    ///
    /// # Errors
    ///
    /// The engine's protocol error, [`CdpError::Timeout`], or
    /// [`CdpError::Closed`].
    pub async fn call(
        &self,
        session: Option<&str>,
        method: &str,
        params: Value,
    ) -> Result<Value, CdpError> {
        let traced = trace_on().then(|| params.to_string().chars().take(300).collect::<String>());
        let started = std::time::Instant::now();
        let result = self.exchange(session, method, params).await;
        if let Some(params) = traced {
            let answer = match &result {
                Ok(value) => {
                    let text = value.to_string();
                    format!(
                        "ok {}B {}",
                        text.len(),
                        text.chars().take(300).collect::<String>()
                    )
                }
                Err(error) => format!("ERR {error}"),
            };
            eprintln!(
                "cdp-trace cmd {method} {}ms {params} -> {answer}",
                started.elapsed().as_millis()
            );
        }
        // Outside a scope (background traffic) there is nothing to count.
        match &result {
            Err(CdpError::Protocol { code, message }) if is_unsupported(*code, message) => {
                count_fault(|faults| &faults.unsupported);
            }
            Err(CdpError::Protocol { code, .. }) if *code == INTERNAL_ERROR => {
                count_fault(|faults| &faults.engine_errors);
            }
            Err(CdpError::Timeout) => count_fault(|faults| &faults.timeouts),
            Err(CdpError::Closed) => count_fault(|faults| &faults.closed),
            _ => {}
        }
        result
    }

    async fn exchange(
        &self,
        session: Option<&str>,
        method: &str,
        params: Value,
    ) -> Result<Value, CdpError> {
        let id = self.next_id.fetch_add(1, Ordering::Relaxed);
        let (reply, answer) = oneshot::channel();
        {
            let mut pending = self.shared.pending.lock().map_err(|_| CdpError::Closed)?;
            pending.as_mut().ok_or(CdpError::Closed)?.insert(id, reply);
        }
        let mut message = json!({ "id": id, "method": method, "params": params });
        if let Some(session) = session {
            message["sessionId"] = Value::String(session.to_owned());
        }
        let mut bytes = serde_json::to_vec(&message).map_err(|_| CdpError::Closed)?;
        bytes.push(0);
        let sent = {
            let mut writer = self.writer.lock().await;
            match writer.write_all(&bytes).await {
                Ok(()) => writer.flush().await,
                Err(error) => Err(error),
            }
        };
        if sent.is_err() {
            self.forget(id);
            return Err(CdpError::Closed);
        }
        match tokio::time::timeout(COMMAND_TIMEOUT, answer).await {
            Ok(Ok(result)) => result,
            Ok(Err(_)) => Err(CdpError::Closed),
            Err(_) => {
                self.forget(id);
                Err(CdpError::Timeout)
            }
        }
    }

    fn forget(&self, id: u64) {
        if let Ok(mut pending) = self.shared.pending.lock()
            && let Some(pending) = pending.as_mut()
        {
            pending.remove(&id);
        }
    }

    /// How many commands the engine refused as unsupported so far.
    #[must_use]
    pub fn unsupported_errors(&self) -> u64 {
        self.shared.unsupported.load(Ordering::Relaxed)
    }

    /// Whether the reader is still running.
    #[must_use]
    pub fn is_open(&self) -> bool {
        self.shared
            .pending
            .lock()
            .is_ok_and(|pending| pending.is_some())
    }
}

async fn read_loop<R: AsyncRead + Unpin>(mut reader: R, shared: Arc<Shared>) {
    let mut buffer: Vec<u8> = Vec::with_capacity(64 << 10);
    let mut chunk = vec![0_u8; 64 << 10];
    'read: loop {
        let read = match reader.read(&mut chunk).await {
            Ok(0) | Err(_) => break,
            Ok(read) => read,
        };
        let mut start = buffer.len();
        buffer.extend_from_slice(&chunk[..read]);
        while let Some(end) = buffer[start..].iter().position(|byte| *byte == 0) {
            let end = start + end;
            dispatch(&shared, &buffer[..end]);
            buffer.drain(..=end);
            start = 0;
        }
        if buffer.len() > MAX_MESSAGE_BYTES {
            break 'read;
        }
    }
    shared.close();
}

/// Diagnostic CDP trace on stderr (`ELITEA_CDP_TRACE`), temporary (#1599).
pub fn trace_on() -> bool {
    static ON: std::sync::OnceLock<bool> = std::sync::OnceLock::new();
    *ON.get_or_init(|| std::env::var_os("ELITEA_CDP_TRACE").is_some())
}

fn dispatch(shared: &Shared, record: &[u8]) {
    let Ok(message) = serde_json::from_slice::<Value>(record) else {
        if trace_on() {
            eprintln!("cdp-trace unparsable record {}B", record.len());
        }
        return;
    };
    if trace_on() && message.get("id").is_none() {
        let method = message["method"].as_str().unwrap_or_default();
        if !matches!(method, "Page.screencastFrame" | "Network.dataReceived") {
            let text = message["params"].to_string();
            eprintln!(
                "cdp-trace evt {method} {}",
                text.chars().take(240).collect::<String>()
            );
        }
    }
    let session_id = message["sessionId"].as_str().map(str::to_owned);
    if let Some(id) = message["id"].as_u64() {
        let reply = shared
            .pending
            .lock()
            .ok()
            .and_then(|mut pending| pending.as_mut().and_then(|pending| pending.remove(&id)));
        if let Some(reply) = reply {
            let result = match message.get("error") {
                Some(error) => {
                    let code = error["code"].as_i64().unwrap_or_default();
                    let message = error["message"].as_str().unwrap_or_default().to_owned();
                    if is_unsupported(code, &message) {
                        shared.unsupported.fetch_add(1, Ordering::Relaxed);
                    }
                    Err(CdpError::Protocol { code, message })
                }
                None => Ok(message.get("result").cloned().unwrap_or(Value::Null)),
            };
            let _ = reply.send(result);
        }
        return;
    }
    if let Some(method) = message["method"].as_str().map(str::to_owned) {
        let mut message = message;
        let event = Event {
            method,
            params: message.get_mut("params").map_or(Value::Null, Value::take),
            session_id,
        };
        // No subscriber is not an error: events nobody waits for are dropped.
        let _ = shared.events.send(Arc::new(event));
    }
}

/// JSON-RPC's "method not found", which Chromium and Moli both answer for
/// a method they do not implement.
pub const METHOD_NOT_FOUND: i64 = -32601;

/// Whether a protocol error says the engine does not implement what was
/// asked: the method itself (`-32601`), or a feature of it (Moli answers an
/// explicit "unsupported" error rather than pretending, ADR-0049 decision 2).
#[must_use]
pub fn is_unsupported(code: i64, message: &str) -> bool {
    if code == METHOD_NOT_FOUND {
        return true;
    }
    let message = message.to_ascii_lowercase();
    [
        "unsupported",
        "not supported",
        "not implemented",
        "unknownmethod",
    ]
    .iter()
    .any(|needle| message.contains(needle))
}

/// Carry CDP between a WebSocket (Moli's endpoint on the session's
/// loopback) and the NUL-delimited byte stream [`Connection`] reads and
/// writes, so one client serves both engines. Returns the two ends for
/// [`Connection::new`] and spawns the two copy tasks; both end when either
/// side closes.
pub fn websocket_bridge<S>(
    socket: tokio_tungstenite::WebSocketStream<S>,
) -> (tokio::io::DuplexStream, tokio::io::DuplexStream)
where
    S: AsyncRead + AsyncWrite + Send + Unpin + 'static,
{
    use futures_util::{SinkExt as _, StreamExt as _};
    use tokio_tungstenite::tungstenite::Message;

    let (mut sink, mut stream) = socket.split();
    // Engine to client: every text message becomes one NUL-terminated record.
    let (mut to_client, client_reads) = tokio::io::duplex(1 << 20);
    // Client to engine: every record becomes one text message.
    let (client_writes, from_client) = tokio::io::duplex(1 << 20);
    tokio::spawn(async move {
        loop {
            let message = match stream.next().await {
                Some(Ok(message)) => message,
                Some(Err(error)) => {
                    if trace_on() {
                        eprintln!("cdp-trace bridge read error: {error}");
                    }
                    break;
                }
                None => {
                    if trace_on() {
                        eprintln!("cdp-trace bridge stream ended");
                    }
                    break;
                }
            };
            let bytes = match message {
                Message::Text(text) => text.as_bytes().to_vec(),
                Message::Binary(bytes) => bytes.to_vec(),
                Message::Close(frame) => {
                    if trace_on() {
                        eprintln!("cdp-trace bridge close frame {frame:?}");
                    }
                    break;
                }
                _ => continue,
            };
            if bytes.contains(&0) || bytes.len() > MAX_MESSAGE_BYTES {
                if trace_on() {
                    eprintln!(
                        "cdp-trace bridge refused message {}B nul={}",
                        bytes.len(),
                        bytes.contains(&0)
                    );
                }
                break;
            }
            if to_client.write_all(&bytes).await.is_err()
                || to_client.write_all(&[0]).await.is_err()
            {
                break;
            }
        }
        let _ = to_client.shutdown().await;
    });
    tokio::spawn(async move {
        let mut reader = tokio::io::BufReader::new(from_client);
        let mut record = Vec::new();
        loop {
            record.clear();
            match tokio::io::AsyncBufReadExt::read_until(&mut reader, 0, &mut record).await {
                Ok(0) | Err(_) => break,
                Ok(_) => {}
            }
            if record.last() == Some(&0) {
                record.pop();
            }
            let Ok(text) = String::from_utf8(std::mem::take(&mut record)) else {
                break;
            };
            if sink.send(Message::text(text)).await.is_err() {
                break;
            }
        }
        let _ = sink.close().await;
    });
    (client_reads, client_writes)
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{AsyncBufReadExt, BufReader};

    /// A fake engine on the other end of two in-memory pipes: it answers
    /// every command with its own method and params, and sends one event
    /// first.
    #[tokio::test]
    async fn replies_and_events_round_trip() {
        let (client_write, engine_read) = tokio::io::duplex(1 << 16);
        let (engine_write, client_read) = tokio::io::duplex(1 << 16);
        tokio::spawn(async move {
            let mut lines = BufReader::new(engine_read);
            let mut engine_write = engine_write;
            let mut record = Vec::new();
            while lines.read_until(0, &mut record).await.unwrap() > 0 {
                record.pop();
                let command: Value = serde_json::from_slice(&record).unwrap();
                record.clear();
                let event =
                    json!({"method": "Page.loadEventFired", "params": {"t": 1}, "sessionId": "S"});
                let reply = json!({"id": command["id"], "result": {"echo": command["method"], "session": command["sessionId"]}});
                for message in [event, reply] {
                    let mut bytes = serde_json::to_vec(&message).unwrap();
                    bytes.push(0);
                    engine_write.write_all(&bytes).await.unwrap();
                }
            }
        });
        let connection = Connection::new(client_read, client_write);
        let mut events = connection.events();
        let result = connection
            .call(Some("S"), "Page.enable", json!({}))
            .await
            .unwrap();
        assert_eq!(result["echo"], "Page.enable");
        assert_eq!(result["session"], "S");
        let event = events.recv().await.unwrap();
        assert_eq!(event.method, "Page.loadEventFired");
        assert_eq!(event.session_id.as_deref(), Some("S"));
        assert_eq!(event.params["t"], 1);
    }

    #[tokio::test]
    async fn a_protocol_error_is_typed_and_a_closed_pipe_fails_pending_calls() {
        let (client_write, engine_read) = tokio::io::duplex(1 << 16);
        let (engine_write, client_read) = tokio::io::duplex(1 << 16);
        tokio::spawn(async move {
            let mut reader = BufReader::new(engine_read);
            let mut engine_write = engine_write;
            let mut record = Vec::new();
            reader.read_until(0, &mut record).await.unwrap();
            record.pop();
            let command: Value = serde_json::from_slice(&record).unwrap();
            let reply =
                json!({"id": command["id"], "error": {"code": -32000, "message": "No node"}});
            let mut bytes = serde_json::to_vec(&reply).unwrap();
            bytes.push(0);
            engine_write.write_all(&bytes).await.unwrap();
            // Read the second command, then close both ends.
            record.clear();
            reader.read_until(0, &mut record).await.unwrap();
        });
        let connection = Connection::new(client_read, client_write);
        let error = connection
            .call(None, "DOM.describeNode", json!({}))
            .await
            .unwrap_err();
        assert_eq!(
            error,
            CdpError::Protocol {
                code: -32000,
                message: "No node".to_owned()
            }
        );
        let closed = connection.call(None, "Page.enable", json!({})).await;
        assert_eq!(closed.unwrap_err(), CdpError::Closed);
        assert!(!connection.is_open());
    }

    #[test]
    fn unsupported_is_method_not_found_or_an_explicit_refusal() {
        assert!(is_unsupported(-32601, "'Foo.bar' wasn't found"));
        assert!(is_unsupported(
            -32000,
            "Unsupported protocol path: Page.printToPDF"
        ));
        assert!(is_unsupported(-32000, "UnknownMethod"));
        assert!(!is_unsupported(-32000, "No node with given id found"));
    }

    #[tokio::test]
    async fn an_unsupported_reply_is_counted() {
        let (client_write, engine_read) = tokio::io::duplex(1 << 16);
        let (engine_write, client_read) = tokio::io::duplex(1 << 16);
        tokio::spawn(async move {
            let mut reader = BufReader::new(engine_read);
            let mut engine_write = engine_write;
            let mut record = Vec::new();
            while reader.read_until(0, &mut record).await.unwrap() > 0 {
                record.pop();
                let command: Value = serde_json::from_slice(&record).unwrap();
                record.clear();
                let reply = json!({"id": command["id"], "error": {"code": -32601, "message": "UnknownMethod"}});
                let mut bytes = serde_json::to_vec(&reply).unwrap();
                bytes.push(0);
                engine_write.write_all(&bytes).await.unwrap();
            }
        });
        let connection = Connection::new(client_read, client_write);
        assert_eq!(connection.unsupported_errors(), 0);
        let _ = connection.call(None, "Page.printToPDF", json!({})).await;
        assert_eq!(connection.unsupported_errors(), 1);
        // A tool call's scope counts its own commands only: background
        // traffic on another task, at the same time, is not the call's.
        let background = connection.clone();
        let ((), during) = scoped_faults(async {
            let noise = tokio::spawn(async move {
                let _ = background
                    .call(None, "Page.screencastFrameAck", json!({}))
                    .await;
            });
            noise.await.expect("the background call");
            let _ = connection.call(None, "Runtime.evaluate", json!({})).await;
        })
        .await;
        assert_eq!(
            during.unsupported, 1,
            "only the call's own unsupported answer counts"
        );
        let ((), quiet) = scoped_faults(async {
            let background = connection.clone();
            tokio::spawn(async move {
                let _ = background
                    .call(None, "Page.screencastFrameAck", json!({}))
                    .await;
            })
            .await
            .unwrap();
        })
        .await;
        assert_eq!(
            quiet,
            CallFaults::default(),
            "background traffic counted against the call"
        );
        assert_eq!(connection.unsupported_errors(), 4);
    }

    /// A call's internal errors, unanswered commands and a closed pipe are
    /// counted against it; page-side protocol errors are not.
    #[tokio::test(start_paused = true)]
    async fn a_calls_engine_faults_are_counted() {
        let (client_write, engine_read) = tokio::io::duplex(1 << 16);
        let (engine_write, client_read) = tokio::io::duplex(1 << 16);
        tokio::spawn(async move {
            let mut reader = BufReader::new(engine_read);
            let mut engine_write = engine_write;
            let mut record = Vec::new();
            while reader.read_until(0, &mut record).await.unwrap_or(0) > 0 {
                record.pop();
                let command: Value = serde_json::from_slice(&record).unwrap();
                record.clear();
                let error = match command["method"].as_str() {
                    Some("Hang.up") => break,
                    Some("Never.answer") => continue,
                    Some("Engine.fail") => json!({"code": -32603, "message": "Internal error"}),
                    _ => json!({"code": -32000, "message": "No node with given id found"}),
                };
                let mut bytes =
                    serde_json::to_vec(&json!({"id": command["id"], "error": error})).unwrap();
                bytes.push(0);
                engine_write.write_all(&bytes).await.unwrap();
            }
        });
        let connection = Connection::new(client_read, client_write);
        let ((), page_side) = scoped_faults(async {
            let _ = connection.call(None, "DOM.resolveNode", json!({})).await;
        })
        .await;
        assert_eq!(page_side, CallFaults::default());
        let ((), faults) = scoped_faults(async {
            let _ = connection.call(None, "Engine.fail", json!({})).await;
            assert_eq!(
                connection.call(None, "Never.answer", json!({})).await,
                Err(CdpError::Timeout)
            );
            assert_eq!(
                connection.call(None, "Hang.up", json!({})).await,
                Err(CdpError::Closed)
            );
            // After the end, a command fails at once, and still counts.
            let _ = connection.call(None, "Page.enable", json!({})).await;
        })
        .await;
        assert_eq!(
            faults,
            CallFaults {
                unsupported: 0,
                timeouts: 1,
                closed: 2,
                engine_errors: 1,
            }
        );
    }

    #[tokio::test]
    async fn the_websocket_bridge_carries_records_both_ways() {
        use futures_util::{SinkExt as _, StreamExt as _};
        use tokio_tungstenite::tungstenite::Message;
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        // A fake engine: answers each command with its method.
        tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut socket = tokio_tungstenite::accept_async(stream).await.unwrap();
            while let Some(Ok(Message::Text(text))) = socket.next().await {
                let command: Value = serde_json::from_str(text.as_str()).unwrap();
                let reply = json!({"id": command["id"], "result": {"echo": command["method"]}});
                socket.send(Message::text(reply.to_string())).await.unwrap();
            }
        });
        let (socket, _) = tokio_tungstenite::client_async(
            format!("ws://{address}/devtools/browser/x"),
            tokio::net::TcpStream::connect(address).await.unwrap(),
        )
        .await
        .unwrap();
        let (reads, writes) = websocket_bridge(socket);
        let connection = Connection::new(reads, writes);
        let result = connection
            .call(None, "Browser.getVersion", json!({}))
            .await
            .unwrap();
        assert_eq!(result["echo"], "Browser.getVersion");
    }
}
