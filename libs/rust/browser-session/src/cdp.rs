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

/// How long one command may take before it fails (`timeout`).
pub const COMMAND_TIMEOUT: Duration = Duration::from_secs(30);

const EVENT_CAPACITY: usize = 4096;

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

fn dispatch(shared: &Shared, record: &[u8]) {
    let Ok(message) = serde_json::from_slice::<Value>(record) else {
        return;
    };
    let session_id = message["sessionId"].as_str().map(str::to_owned);
    if let Some(id) = message["id"].as_u64() {
        let reply = shared
            .pending
            .lock()
            .ok()
            .and_then(|mut pending| pending.as_mut().and_then(|pending| pending.remove(&id)));
        if let Some(reply) = reply {
            let result = match message.get("error") {
                Some(error) => Err(CdpError::Protocol {
                    code: error["code"].as_i64().unwrap_or_default(),
                    message: error["message"].as_str().unwrap_or_default().to_owned(),
                }),
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
}
