//! JSON-RPC 2.0 over a language server's stdio, framed the LSP way: a
//! `Content-Length` header, a blank line, then that many bytes of JSON.
//!
//! The framing is this crate's own (`lsp-types` gives the protocol's
//! types): async-lsp 0.2.4 links lsp-types 0.95 and reads through
//! `futures`' traits, not tokio's pipes. Everything the server sends is
//! untrusted: a header line, the header count and a message have fixed
//! caps ([`MAX_MESSAGE`]), a server that breaks the framing is dropped,
//! and what is kept of its diagnostics is cut to [`MAX_FILES`] files of
//! [`MAX_PER_FILE`] entries with short messages (see [`Diag`]).

use std::collections::HashMap;
use std::fmt::Write as _;
use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use lsp_types::notification::{Notification as _, PublishDiagnostics};
use lsp_types::{DiagnosticSeverity, NumberOrString, PublishDiagnosticsParams};
use serde::Serialize;
use serde_json::{Value, json};
use tokio::io::{AsyncRead, AsyncReadExt as _, AsyncWrite, AsyncWriteExt as _};
use tokio::sync::{Notify, oneshot};

/// The largest message read from a server.
pub const MAX_MESSAGE: usize = 8 * 1024 * 1024;

/// The longest header line, and the most header lines, of one message.
const MAX_HEADER_LINE: usize = 1024;
const MAX_HEADERS: usize = 16;

/// The most files whose diagnostics are kept, and entries per file.
pub const MAX_FILES: usize = 4096;
pub const MAX_PER_FILE: usize = 1000;

/// A diagnostic message is cut to this many bytes, a code to
/// [`MAX_CODE`].
pub const MAX_MESSAGE_TEXT: usize = 512;
const MAX_CODE: usize = 64;

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

/// One message's body, or `None` at the end of the stream.
///
/// # Errors
///
/// A header line or a message over its cap, a header that does not parse,
/// or I/O.
pub async fn read_message<R: AsyncRead + Unpin>(
    reader: &mut R,
) -> std::io::Result<Option<Vec<u8>>> {
    let invalid = |why: &str| std::io::Error::new(std::io::ErrorKind::InvalidData, why.to_owned());
    let mut length = None;
    let mut lines = 0;
    loop {
        let mut line = Vec::new();
        loop {
            let byte = match reader.read_u8().await {
                Ok(byte) => byte,
                Err(error) if error.kind() == std::io::ErrorKind::UnexpectedEof => {
                    return if line.is_empty() && lines == 0 {
                        Ok(None)
                    } else {
                        Err(invalid("the stream ended inside a header"))
                    };
                }
                Err(error) => return Err(error),
            };
            if byte == b'\n' {
                break;
            }
            if line.len() >= MAX_HEADER_LINE {
                return Err(invalid("a header line is too long"));
            }
            line.push(byte);
        }
        if line.last() == Some(&b'\r') {
            line.pop();
        }
        if line.is_empty() {
            break;
        }
        lines += 1;
        if lines > MAX_HEADERS {
            return Err(invalid("too many header lines"));
        }
        let text = std::str::from_utf8(&line).map_err(|_| invalid("a header is not UTF-8"))?;
        if let Some((name, value)) = text.split_once(':')
            && name.trim().eq_ignore_ascii_case("content-length")
        {
            length = Some(
                value
                    .trim()
                    .parse::<usize>()
                    .map_err(|_| invalid("Content-Length is not a number"))?,
            );
        }
    }
    let length = length.ok_or_else(|| invalid("a message without Content-Length"))?;
    if length > MAX_MESSAGE {
        return Err(invalid("a message is too large"));
    }
    let mut body = vec![0u8; length];
    reader.read_exact(&mut body).await?;
    Ok(Some(body))
}

/// `body` framed for the server.
#[must_use]
pub fn frame(body: &[u8]) -> Vec<u8> {
    let mut out = format!("Content-Length: {}\r\n\r\n", body.len()).into_bytes();
    out.extend_from_slice(body);
    out
}

/// How bad a diagnostic is.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Severity {
    Error,
    Warning,
    Information,
    Hint,
}

impl Severity {
    fn of(severity: Option<DiagnosticSeverity>) -> Self {
        match severity {
            Some(DiagnosticSeverity::WARNING) => Self::Warning,
            Some(DiagnosticSeverity::INFORMATION) => Self::Information,
            Some(DiagnosticSeverity::HINT) => Self::Hint,
            // The protocol leaves an absent severity to the client: an
            // error, so it is not hidden.
            _ => Self::Error,
        }
    }
}

/// One diagnostic as kept: 1-based line and column (the column counts
/// UTF-16 units, as the protocol does), the code and a cut, single-line
/// message. The server's text is data for the model, never instructions.
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct Diag {
    pub line: u32,
    pub col: u32,
    pub severity: Severity,
    pub code: Option<String>,
    pub message: String,
}

impl Diag {
    /// What compares equal across an edit that moved the lines.
    pub fn key(&self) -> (Severity, Option<&str>, &str) {
        (self.severity, self.code.as_deref(), self.message.as_str())
    }
}

/// `text` without control characters (a newline becomes a space), cut to
/// `max` bytes on a character boundary.
#[must_use]
pub fn clean(text: &str, max: usize) -> String {
    let mut out = String::with_capacity(text.len().min(max));
    for c in text.chars() {
        let c = if c.is_control() { ' ' } else { c };
        if out.len() + c.len_utf8() > max {
            break;
        }
        out.push(c);
    }
    out.trim_end().to_owned()
}

fn convert(diagnostic: lsp_types::Diagnostic) -> Diag {
    let code = diagnostic.code.map(|code| match code {
        NumberOrString::Number(number) => number.to_string(),
        NumberOrString::String(text) => clean(&text, MAX_CODE),
    });
    Diag {
        line: diagnostic.range.start.line.saturating_add(1),
        col: diagnostic.range.start.character.saturating_add(1),
        severity: Severity::of(diagnostic.severity),
        code,
        message: clean(&diagnostic.message, MAX_MESSAGE_TEXT),
    }
}

/// A `file://` URI's path, percent-decoded; `None` for another scheme or
/// a path that is not UTF-8. How diagnostics are matched to documents:
/// servers spell URIs their own way.
#[must_use]
pub fn uri_path(uri: &str) -> Option<String> {
    let rest = uri.strip_prefix("file://")?;
    let rest = rest.strip_prefix("localhost").unwrap_or(rest);
    if !rest.starts_with('/') {
        return None;
    }
    let bytes = rest.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut index = 0;
    while index < bytes.len() {
        if bytes[index] == b'%' {
            let hex = std::str::from_utf8(bytes.get(index + 1..index + 3)?).ok()?;
            out.push(u8::from_str_radix(hex, 16).ok()?);
            index += 3;
        } else {
            out.push(bytes[index]);
            index += 1;
        }
    }
    String::from_utf8(out).ok()
}

/// `path` (absolute) as a `file://` URI: every byte but the unreserved
/// ones and `/` percent-encoded.
#[must_use]
pub fn path_uri(path: &str) -> String {
    let mut out = String::from("file://");
    for byte in path.bytes() {
        if byte.is_ascii_alphanumeric() || matches!(byte, b'/' | b'-' | b'.' | b'_' | b'~') {
            out.push(char::from(byte));
        } else {
            let _ = write!(out, "%{byte:02X}");
        }
    }
    out
}

/// One file's last published diagnostics.
#[derive(Clone, Debug, Default)]
pub struct Published {
    /// The connection's publish counter when they came.
    pub seq: u64,
    pub version: Option<i32>,
    pub diags: Vec<Diag>,
}

#[derive(Default)]
struct Store {
    seq: u64,
    /// By decoded path.
    files: HashMap<String, Published>,
}

type Pending = HashMap<i64, oneshot::Sender<Result<Value, String>>>;

/// A started server's connection.
pub struct Connection {
    stdin: tokio::sync::Mutex<Box<dyn AsyncWrite + Send + Unpin>>,
    next_id: AtomicI64,
    pending: Mutex<Pending>,
    store: Mutex<Store>,
    /// A publish came, or the connection closed.
    changed: Notify,
    closed: AtomicBool,
    /// What `workspace/configuration` answers, by section.
    settings: Value,
    /// The workspace folder, for `workspace/workspaceFolders`.
    folder: Value,
}

impl Connection {
    /// A connection writing to `stdin`; [`Self::spawn_reader`] reads.
    pub fn new(
        stdin: Box<dyn AsyncWrite + Send + Unpin>,
        settings: Value,
        folder: Value,
    ) -> Arc<Self> {
        Arc::new(Self {
            stdin: tokio::sync::Mutex::new(stdin),
            next_id: AtomicI64::new(1),
            pending: Mutex::new(HashMap::new()),
            store: Mutex::new(Store::default()),
            changed: Notify::new(),
            closed: AtomicBool::new(false),
            settings,
            folder,
        })
    }

    /// Read the server's messages until its output ends or breaks the
    /// framing; then the connection is closed.
    pub fn spawn_reader<R: AsyncRead + Send + Unpin + 'static>(self: &Arc<Self>, stdout: R) {
        let this = Arc::clone(self);
        tokio::spawn(async move {
            let mut reader = tokio::io::BufReader::new(stdout);
            while let Ok(Some(body)) = read_message(&mut reader).await {
                match serde_json::from_slice::<Value>(&body) {
                    Ok(message) => this.handle(&message),
                    Err(_) => break,
                }
            }
            this.close();
        });
    }

    /// Whether the server's output ended.
    pub fn closed(&self) -> bool {
        self.closed.load(Ordering::SeqCst)
    }

    fn close(&self) {
        self.closed.store(true, Ordering::SeqCst);
        for (_, waiter) in lock(&self.pending).drain() {
            let _ = waiter.send(Err("the language server stopped".to_owned()));
        }
        self.changed.notify_waiters();
    }

    fn handle(self: &Arc<Self>, message: &Value) {
        let method = message.get("method").and_then(Value::as_str);
        let id = message.get("id").cloned();
        match (method, id) {
            (Some(method), Some(id)) => {
                let reply = self.answer(method, message.get("params"));
                let body = match reply {
                    Ok(result) => json!({ "jsonrpc": "2.0", "id": id, "result": result }),
                    Err(()) => json!({
                        "jsonrpc": "2.0",
                        "id": id,
                        "error": { "code": -32601, "message": "not supported by this client" },
                    }),
                };
                // Off the reader: a write waiting on a full pipe must not
                // stop the reads that would drain the server.
                let this = Arc::clone(self);
                tokio::spawn(async move {
                    let _ = this.send(&body).await;
                });
            }
            (Some(method), None) if method == PublishDiagnostics::METHOD => {
                if let Some(params) = message.get("params").cloned()
                    && let Ok(params) = serde_json::from_value::<PublishDiagnosticsParams>(params)
                {
                    self.publish(params);
                }
            }
            (None, Some(id)) => {
                let Some(id) = id.as_i64() else { return };
                let Some(waiter) = lock(&self.pending).remove(&id) else {
                    return;
                };
                let outcome = match message.get("error") {
                    Some(error) => Err(clean(
                        error
                            .get("message")
                            .and_then(Value::as_str)
                            .unwrap_or("the request failed"),
                        MAX_MESSAGE_TEXT,
                    )),
                    None => Ok(message.get("result").cloned().unwrap_or(Value::Null)),
                };
                let _ = waiter.send(outcome);
            }
            _ => {}
        }
    }

    /// A server's request to this client: its result, or `Err` for one
    /// this client does not take.
    fn answer(&self, method: &str, params: Option<&Value>) -> Result<Value, ()> {
        match method {
            "workspace/configuration" => {
                let items = params
                    .and_then(|params| params.get("items"))
                    .and_then(Value::as_array)
                    .map_or(&[][..], Vec::as_slice);
                Ok(Value::Array(
                    items
                        .iter()
                        .take(64)
                        .map(|item| {
                            item.get("section")
                                .and_then(Value::as_str)
                                .and_then(|section| self.settings.get(section))
                                .cloned()
                                .unwrap_or(Value::Null)
                        })
                        .collect(),
                ))
            }
            "workspace/workspaceFolders" => Ok(json!([self.folder])),
            "client/registerCapability"
            | "client/unregisterCapability"
            | "window/workDoneProgress/create"
            | "window/showMessageRequest"
            | "workspace/diagnostic/refresh"
            | "workspace/semanticTokens/refresh"
            | "workspace/inlayHint/refresh"
            | "workspace/codeLens/refresh" => Ok(Value::Null),
            _ => Err(()),
        }
    }

    fn publish(&self, params: PublishDiagnosticsParams) {
        let Some(path) = uri_path(params.uri.as_str()) else {
            return;
        };
        let mut store = lock(&self.store);
        if !store.files.contains_key(&path) && store.files.len() >= MAX_FILES {
            return;
        }
        store.seq += 1;
        let seq = store.seq;
        let diags = params
            .diagnostics
            .into_iter()
            .take(MAX_PER_FILE)
            .map(convert)
            .collect();
        store.files.insert(
            path,
            Published {
                seq,
                version: params.version,
                diags,
            },
        );
        drop(store);
        self.changed.notify_waiters();
    }

    /// The publish counter now: a later publish has a larger `seq`.
    pub fn seq(&self) -> u64 {
        lock(&self.store).seq
    }

    /// What the server last published for `path` (absolute), if anything.
    pub fn published(&self, path: &str) -> Option<Published> {
        lock(&self.store).files.get(path).cloned()
    }

    /// Wait until `path` gets a publish after `after` (for `version` or a
    /// later one, when the server names versions), until `timeout` ends or
    /// the connection closes; what it got.
    pub async fn wait_publish(
        &self,
        path: &str,
        after: u64,
        version: Option<i32>,
        timeout: Duration,
    ) -> Option<Published> {
        let deadline = tokio::time::Instant::now() + timeout;
        loop {
            let notified = self.changed.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            if let Some(published) = self.published(path)
                && published.seq > after
                && match (published.version, version) {
                    (Some(got), Some(wanted)) => got >= wanted,
                    _ => true,
                }
            {
                return Some(published);
            }
            if self.closed() {
                return None;
            }
            if tokio::time::timeout_at(deadline, notified).await.is_err() {
                return None;
            }
        }
    }

    /// Wait up to `quiet` for another publish of `path` after `after`: a
    /// server that publishes twice (rust-analyzer's own check, then
    /// cargo's) has its later answer taken when it comes quickly.
    pub async fn settle(&self, path: &str, after: u64, quiet: Duration) -> Option<Published> {
        let deadline = tokio::time::Instant::now() + quiet;
        loop {
            let notified = self.changed.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            if let Some(published) = self.published(path)
                && published.seq > after
            {
                return Some(published);
            }
            if self.closed() || tokio::time::timeout_at(deadline, notified).await.is_err() {
                return None;
            }
        }
    }

    async fn send(&self, message: &Value) -> std::io::Result<()> {
        let body = serde_json::to_vec(message).map_err(std::io::Error::other)?;
        let mut stdin = self.stdin.lock().await;
        stdin.write_all(&frame(&body)).await?;
        stdin.flush().await
    }

    /// Send a notification.
    ///
    /// # Errors
    ///
    /// The server's input is closed.
    pub async fn notify(&self, method: &str, params: Value) -> Result<(), String> {
        if self.closed() {
            return Err("the language server stopped".to_owned());
        }
        self.send(&json!({ "jsonrpc": "2.0", "method": method, "params": params }))
            .await
            .map_err(|_| "the language server stopped reading".to_owned())
    }

    /// Send a request and wait at most `timeout` for its result.
    ///
    /// # Errors
    ///
    /// The server stopped, timed out or answered with an error.
    pub async fn request(
        &self,
        method: &str,
        params: Value,
        timeout: Duration,
    ) -> Result<Value, String> {
        if self.closed() {
            return Err("the language server stopped".to_owned());
        }
        let id = self.next_id.fetch_add(1, Ordering::SeqCst);
        let (sender, receiver) = oneshot::channel();
        lock(&self.pending).insert(id, sender);
        let message = json!({ "jsonrpc": "2.0", "id": id, "method": method, "params": params });
        if self.send(&message).await.is_err() {
            lock(&self.pending).remove(&id);
            return Err("the language server stopped reading".to_owned());
        }
        match tokio::time::timeout(timeout, receiver).await {
            Ok(Ok(outcome)) => outcome,
            Ok(Err(_)) => Err("the language server stopped".to_owned()),
            Err(_) => {
                lock(&self.pending).remove(&id);
                Err(format!("no answer to {method} in time"))
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn messages_are_read_back_as_framed() {
        let mut input = frame(br#"{"a":1}"#);
        input.extend(b"content-length: 2\r\nContent-Type: x\r\n\r\n[]");
        let mut reader = &input[..];
        assert_eq!(
            read_message(&mut reader).await.unwrap().unwrap(),
            br#"{"a":1}"#
        );
        assert_eq!(read_message(&mut reader).await.unwrap().unwrap(), b"[]");
        assert!(read_message(&mut reader).await.unwrap().is_none());
    }

    #[tokio::test]
    async fn oversized_or_broken_frames_are_refused() {
        let big = format!("Content-Length: {}\r\n\r\n", MAX_MESSAGE + 1);
        assert!(read_message(&mut big.as_bytes()).await.is_err());
        let long = format!("X-{}: y\r\n\r\n", "a".repeat(MAX_HEADER_LINE));
        assert!(read_message(&mut long.as_bytes()).await.is_err());
        let many = "X: y\r\n".repeat(MAX_HEADERS + 1);
        assert!(read_message(&mut many.as_bytes()).await.is_err());
        assert!(read_message(&mut &b"X: y\r\n\r\n{}"[..]).await.is_err());
        assert!(
            read_message(&mut &b"Content-Length: 9\r\n\r\n{}"[..])
                .await
                .is_err()
        );
    }

    #[test]
    fn uris_round_trip_and_decode() {
        let uri = path_uri("/tmp/a b/ü.rs");
        assert_eq!(uri, "file:///tmp/a%20b/%C3%BC.rs");
        assert_eq!(uri_path(&uri).as_deref(), Some("/tmp/a b/ü.rs"));
        assert_eq!(
            uri_path("file:///tmp/a%20b/%c3%bc.rs").as_deref(),
            Some("/tmp/a b/ü.rs")
        );
        assert_eq!(uri_path("file://localhost/x").as_deref(), Some("/x"));
        assert_eq!(uri_path("untitled:x"), None);
        assert_eq!(uri_path("file:///bad%2"), None);
    }

    #[test]
    fn messages_lose_control_characters_and_are_cut() {
        assert_eq!(clean("a\nb\u{1b}[31mc", 100), "a b [31mc");
        assert_eq!(clean("ééé", 5), "éé");
    }
}
