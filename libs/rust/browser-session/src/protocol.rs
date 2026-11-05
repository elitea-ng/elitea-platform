//! The supervisor's line protocol, revision 1
//! (`libs/proto/elitea/runtime/v1/browser_session_agent_v1.md`, #1595).
//!
//! The supervisor execs `elitea-browser-session serve` in the session's
//! container and speaks to it over the exec's stdin and stdout: one JSON
//! object per line, at most [`MAX_LINE_BYTES`], binary data as standard
//! base64. There is no socket, port or TLS between them, as with the
//! workspace runner. CDP stays between this process and the engine.
//!
//! [`serve`] is the protocol; the browser behind it is a [`Backend`], so the
//! protocol is tested without an engine and a later transport (a Unix-socket
//! gRPC service, say) can reuse the backend unchanged.
//!
//! * Lines are read while calls run: a `view`, `ack` or `input` line is
//!   handled at once, so the person's input can pause a running call
//!   (`user_active`). Calls run one at a time, in order.
//! * Every line out goes through one writer, so lines never interleave.
//! * Stderr carries the agent's log, by stable code only: never page
//!   content, a URL or the token.

use std::sync::Arc;

use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64;
use elitea_browser_tools::ToolOutput;
use serde_json::{Value, json};
use tokio::io::{AsyncBufRead, AsyncBufReadExt, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::sync::{Mutex, mpsc, watch};

/// The protocol revision this agent speaks.
pub const REVISION: u64 = 1;
/// The longest line in either direction.
pub const MAX_LINE_BYTES: usize = 12 * 1024 * 1024;
const OUT_QUEUE: usize = 64;

/// The `start` line.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Start {
    /// `full` (chrome-headless-shell); `lite` is not admitted yet.
    pub engine: String,
    /// `false` is `--no-sandbox`, admitted under gVisor only.
    pub engine_sandbox: bool,
    pub max_pages: u32,
    pub width: u32,
    pub height: u32,
    /// The egress proxy (`http://<ip>:<port>`) and the run token; `None` is
    /// no network at all.
    pub proxy: Option<(String, String)>,
}

impl Start {
    /// Read a `start` line.
    ///
    /// # Errors
    ///
    /// A line that is not revision 1's `start`.
    pub fn parse(line: &Value) -> Result<Self, &'static str> {
        if line["op"] != "start" || line["revision"].as_u64() != Some(REVISION) {
            return Err("protocol");
        }
        let viewport = &line["viewport"];
        let size = |name: &str, default: u32| match viewport.get(name) {
            None | Some(Value::Null) => Some(default),
            Some(value) => value.as_u64().and_then(|value| u32::try_from(value).ok()),
        };
        let proxy = match &line["proxy"] {
            Value::Null => None,
            Value::Object(proxy) => Some((
                proxy
                    .get("url")
                    .and_then(Value::as_str)
                    .ok_or("protocol")?
                    .to_owned(),
                proxy
                    .get("token")
                    .and_then(Value::as_str)
                    .ok_or("protocol")?
                    .to_owned(),
            )),
            _ => return Err("protocol"),
        };
        Ok(Self {
            engine: line["engine"].as_str().unwrap_or("full").to_owned(),
            engine_sandbox: line["engine_sandbox"].as_bool().ok_or("protocol")?,
            max_pages: line["max_pages"]
                .as_u64()
                .and_then(|value| u32::try_from(value).ok())
                .unwrap_or(1),
            width: size("width", crate::engine::DEFAULT_WIDTH).ok_or("protocol")?,
            height: size("height", crate::engine::DEFAULT_HEIGHT).ok_or("protocol")?,
            proxy,
        })
    }
}

/// A `start` the agent refuses, by its protocol code.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum StartError {
    /// `engine_start_failed`
    EngineStartFailed,
    /// `isolation_refused`
    IsolationRefused,
}

impl StartError {
    #[must_use]
    pub const fn code(self) -> &'static str {
        match self {
            Self::EngineStartFailed => "engine_start_failed",
            Self::IsolationRefused => "isolation_refused",
        }
    }
}

/// One view frame for the supervisor.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ViewFrame {
    pub seq: u64,
    pub jpeg: Vec<u8>,
    pub width: u32,
    pub height: u32,
    pub url: String,
    pub title: String,
    /// The person drives.
    pub user: bool,
}

/// A stream of view frames: the next one is taken only after the previous
/// one was acknowledged.
#[async_trait]
pub trait FrameSource: Send {
    /// The newest frame not yet taken; `None` when the stream ended.
    async fn next(&mut self) -> Option<ViewFrame>;
}

/// The browser behind the protocol.
#[async_trait]
pub trait Backend: Send + Sync + 'static {
    /// Start the engine; its version (`chrome-headless-shell/<version>`).
    async fn start(&self, start: &Start) -> Result<String, StartError>;
    /// One browser tool call, through the contract's `dispatch`.
    async fn call(&self, name: &str, args: &Value) -> ToolOutput;
    /// A viewer attached: frames at most `max_fps` a second.
    async fn view(&self, max_fps: u32) -> Option<Box<dyn FrameSource>>;
    /// A driving viewer's input event.
    async fn input(&self, event: &Value) -> Result<(), &'static str>;
    /// The run ended: no more calls.
    async fn run_ended(&self);
    /// Stop the engine.
    async fn stop(&self);
}

/// Log one event to stderr by its code (never content).
pub fn log(code: &str) {
    eprintln!("elitea-browser-session: {code}");
}

fn line_of(value: &Value) -> Vec<u8> {
    let mut line = serde_json::to_vec(value)
        .unwrap_or_else(|_| b"{\"op\":\"error\",\"code\":\"internal\"}".to_vec());
    line.push(b'\n');
    line
}

/// One call's `result` line.
fn result_line(id: &Value, output: ToolOutput) -> Value {
    match output {
        ToolOutput::Json(result) => json!({ "op": "result", "id": id, "result": result }),
        ToolOutput::Image {
            media_type,
            data,
            result,
        } => {
            let encoded = BASE64.encode(&data);
            // A line must stay under the limit with room for the rest.
            if encoded.len() + 4096 > MAX_LINE_BYTES {
                return json!({ "op": "result", "id": id, "result": {
                    "status": "error", "code": "internal",
                    "message": "the screenshot is too large to return",
                }});
            }
            json!({
                "op": "result", "id": id, "result": result,
                "image": { "media_type": media_type, "data": encoded },
            })
        }
    }
}

fn frame_line(frame: &ViewFrame) -> Value {
    json!({
        "op": "frame",
        "seq": frame.seq,
        "jpeg": BASE64.encode(&frame.jpeg),
        "width": frame.width,
        "height": frame.height,
        "url": frame.url,
        "title": frame.title,
        "control": if frame.user { "user" } else { "agent" },
    })
}

/// Read one line of at most [`MAX_LINE_BYTES`]; `Ok(None)` at end of file.
async fn read_line<R: AsyncBufRead + Unpin>(input: &mut R) -> std::io::Result<Option<Vec<u8>>> {
    let mut line = Vec::new();
    let mut limited = (&mut *input).take(MAX_LINE_BYTES as u64 + 1);
    let read = limited.read_until(b'\n', &mut line).await?;
    if read == 0 {
        return Ok(None);
    }
    if line.len() > MAX_LINE_BYTES {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            "line too long",
        ));
    }
    Ok(Some(line))
}

/// The view loop's state.
struct View {
    /// Highest `seq` the supervisor acknowledged.
    acked: watch::Sender<u64>,
    task: Option<tokio::task::JoinHandle<()>>,
}

/// Serve the protocol on `input` and `output` until `destroy` or the end of
/// `input`. The backend's engine is stopped before this returns.
///
/// # Errors
///
/// Writing to `output` failed.
#[allow(clippy::too_many_lines)]
pub async fn serve<R, W>(backend: Arc<dyn Backend>, mut input: R, output: W) -> std::io::Result<()>
where
    R: AsyncBufRead + Unpin + Send,
    W: AsyncWrite + Unpin + Send + 'static,
{
    let (out, mut queue) = mpsc::channel::<Value>(OUT_QUEUE);
    let writer = tokio::spawn(async move {
        let mut output = output;
        while let Some(line) = queue.recv().await {
            let last = line["op"] == "bye";
            output.write_all(&line_of(&line)).await?;
            output.flush().await?;
            if last {
                break;
            }
        }
        output.shutdown().await
    });
    let send = |line: Value| {
        let out = out.clone();
        async move {
            let _ = out.send(line).await;
        }
    };

    // The first line must be `start`.
    let started = match read_line(&mut input).await {
        Ok(Some(line)) => match serde_json::from_slice::<Value>(&line)
            .ok()
            .and_then(|value| Start::parse(&value).ok())
        {
            Some(start) => backend.start(&start).await.map_err(StartError::code),
            None => Err("protocol"),
        },
        Ok(None) => {
            send(json!({ "op": "bye" })).await;
            drop(out);
            return writer.await.unwrap_or(Ok(()));
        }
        Err(_) => Err("protocol"),
    };
    match started {
        Ok(version) => send(json!({ "op": "ready", "engine_version": version })).await,
        Err(code) => {
            log(code);
            backend.stop().await;
            send(json!({ "op": "error", "code": code })).await;
            send(json!({ "op": "bye" })).await;
            drop(out);
            return writer.await.unwrap_or(Ok(()));
        }
    }

    // Calls run one at a time, in order, on their own task.
    let (calls, mut call_queue) = mpsc::unbounded_channel::<(Value, String, Value)>();
    let call_backend = Arc::clone(&backend);
    let call_out = out.clone();
    let caller = tokio::spawn(async move {
        while let Some((id, name, args)) = call_queue.recv().await {
            let output = call_backend.call(&name, &args).await;
            let _ = call_out.send(result_line(&id, output)).await;
        }
    });

    let view = Arc::new(Mutex::new(View {
        acked: watch::channel(0).0,
        task: None,
    }));
    let mut ended = false;
    loop {
        let line = match read_line(&mut input).await {
            Ok(Some(line)) => line,
            Ok(None) => break,
            Err(_) => {
                log("protocol");
                send(json!({ "op": "error", "code": "protocol" })).await;
                break;
            }
        };
        let Ok(message) = serde_json::from_slice::<Value>(&line) else {
            log("protocol");
            send(json!({ "op": "error", "code": "protocol" })).await;
            break;
        };
        match message["op"].as_str() {
            Some("call") => {
                let id = message["id"].clone();
                let name = message["name"].as_str().map(str::to_owned);
                match name {
                    Some(name) if !ended && (id.is_string() || id.is_number()) => {
                        let args = message.get("args").cloned().unwrap_or(Value::Null);
                        let _ = calls.send((id, name, args));
                    }
                    _ => {
                        let code = if ended { "internal" } else { "protocol" };
                        send(json!({ "op": "error", "code": code, "id": id })).await;
                    }
                }
            }
            Some("view") => {
                let on = message["on"].as_bool().unwrap_or(false);
                let max_fps = message["max_fps"]
                    .as_u64()
                    .and_then(|fps| u32::try_from(fps).ok())
                    .unwrap_or(crate::screencast::MAX_FPS);
                let mut state = view.lock().await;
                if let Some(task) = state.task.take() {
                    task.abort();
                }
                if on {
                    match backend.view(max_fps).await {
                        Some(source) => {
                            state.acked.send_replace(0);
                            let acked = state.acked.subscribe();
                            state.task = Some(tokio::spawn(stream(source, acked, out.clone())));
                        }
                        None => send(json!({ "op": "notice", "code": "view_unavailable" })).await,
                    }
                }
            }
            Some("ack") => {
                if let Some(seq) = message["seq"].as_u64() {
                    view.lock().await.acked.send_if_modified(|acked| {
                        let newer = seq > *acked;
                        if newer {
                            *acked = seq;
                        }
                        newer
                    });
                }
            }
            Some("input") => {
                if backend.input(&message["event"]).await.is_err() {
                    send(json!({ "op": "notice", "code": "input_refused" })).await;
                }
            }
            Some("run_ended") => {
                ended = true;
                backend.run_ended().await;
            }
            Some("destroy") => break,
            // An unknown op is ignored (the protocol's forward rule).
            _ => {}
        }
    }
    if let Some(task) = view.lock().await.task.take() {
        task.abort();
    }
    drop(calls);
    caller.abort();
    backend.stop().await;
    send(json!({ "op": "bye" })).await;
    drop(out);
    writer.await.unwrap_or(Ok(()))
}

/// Send frames: the next only after the previous one's `ack`.
async fn stream(
    mut source: Box<dyn FrameSource>,
    mut acked: watch::Receiver<u64>,
    out: mpsc::Sender<Value>,
) {
    while let Some(frame) = source.next().await {
        let seq = frame.seq;
        if out.send(frame_line(&frame)).await.is_err() {
            return;
        }
        // A viewer that never acks holds the stream; it does not pile up.
        if acked.wait_for(|acked| *acked >= seq).await.is_err() {
            return;
        }
    }
}

/// Wait for SIGTERM or SIGINT (the container's `idle` entrypoint).
///
/// # Errors
///
/// The signal handlers could not be installed.
pub async fn idle() -> std::io::Result<()> {
    use tokio::signal::unix::{SignalKind, signal};
    let mut terminate = signal(SignalKind::terminate())?;
    let mut interrupt = signal(SignalKind::interrupt())?;
    tokio::select! {
        _ = terminate.recv() => {}
        _ = interrupt.recv() => {}
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex as StdMutex;
    use std::time::Duration;
    use tokio::io::BufReader;

    #[derive(Default)]
    struct Fake {
        seen: StdMutex<Vec<String>>,
    }

    struct Frames(u64);

    #[async_trait]
    impl FrameSource for Frames {
        async fn next(&mut self) -> Option<ViewFrame> {
            self.0 += 1;
            (self.0 <= 3).then(|| ViewFrame {
                seq: self.0,
                jpeg: vec![0xff, 0xd8],
                width: 1280,
                height: 800,
                url: "https://a.test/".to_owned(),
                title: "A".to_owned(),
                user: false,
            })
        }
    }

    #[async_trait]
    impl Backend for Fake {
        async fn start(&self, start: &Start) -> Result<String, StartError> {
            self.seen
                .lock()
                .unwrap()
                .push(format!("start {}", start.engine));
            if start.engine_sandbox {
                Ok("chrome-headless-shell/1.0".to_owned())
            } else {
                Err(StartError::IsolationRefused)
            }
        }
        async fn call(&self, name: &str, _: &Value) -> ToolOutput {
            self.seen.lock().unwrap().push(format!("call {name}"));
            if name == "browser_screenshot" {
                ToolOutput::Image {
                    media_type: "image/png",
                    data: b"png".to_vec(),
                    result: json!({"status": "ok"}),
                }
            } else {
                ToolOutput::Json(json!({"status": "ok"}))
            }
        }
        async fn view(&self, _: u32) -> Option<Box<dyn FrameSource>> {
            Some(Box::new(Frames(0)))
        }
        async fn input(&self, event: &Value) -> Result<(), &'static str> {
            self.seen
                .lock()
                .unwrap()
                .push(format!("input {}", event["type"].as_str().unwrap_or("?")));
            Ok(())
        }
        async fn run_ended(&self) {
            self.seen.lock().unwrap().push("run_ended".to_owned());
        }
        async fn stop(&self) {
            self.seen.lock().unwrap().push("stop".to_owned());
        }
    }

    async fn run(lines: &[Value]) -> (Vec<Value>, Vec<String>) {
        let fake = Arc::new(Fake::default());
        let mut text = String::new();
        for line in lines {
            text.push_str(&line.to_string());
            text.push('\n');
        }
        let (output, mut read_back) = tokio::io::duplex(1 << 20);
        let backend: Arc<dyn Backend> = Arc::clone(&fake) as Arc<dyn Backend>;
        let served = tokio::spawn(serve(
            backend,
            BufReader::new(std::io::Cursor::new(text.into_bytes())),
            output,
        ));
        let mut out = Vec::new();
        read_back.read_to_end(&mut out).await.unwrap();
        served.await.unwrap().unwrap();
        let replies = out
            .split(|byte| *byte == b'\n')
            .filter(|line| !line.is_empty())
            .map(|line| serde_json::from_slice(line).unwrap())
            .collect();
        let seen = fake.seen.lock().unwrap().clone();
        (replies, seen)
    }

    fn start(sandbox: bool) -> Value {
        json!({"op": "start", "revision": 1, "engine": "full", "engine_sandbox": sandbox,
               "max_pages": 3, "viewport": {"width": 1280, "height": 800}, "proxy": null})
    }

    #[tokio::test]
    async fn start_call_screenshot_and_destroy() {
        let (replies, seen) = run(&[
            start(true),
            json!({"op": "call", "id": "c1", "name": "browser_open", "args": {"url": "https://a.test/"}}),
            json!({"op": "call", "id": "c2", "name": "browser_screenshot", "args": {}}),
            json!({"op": "future_op"}),
            json!({"op": "destroy"}),
            json!({"op": "call", "id": "never", "name": "browser_open", "args": {}}),
        ])
        .await;
        assert_eq!(
            replies[0],
            json!({"op": "ready", "engine_version": "chrome-headless-shell/1.0"})
        );
        // The calls race the destroy; whichever answered is well formed.
        for reply in &replies[1..replies.len() - 1] {
            assert_eq!(reply["op"], "result");
            if reply["id"] == "c2" {
                assert_eq!(reply["image"]["media_type"], "image/png");
                assert_eq!(reply["image"]["data"], BASE64.encode(b"png"));
            }
        }
        assert_eq!(replies.last().unwrap(), &json!({"op": "bye"}));
        assert_eq!(seen.last().map(String::as_str), Some("stop"));
        assert!(!seen.iter().any(|line| line.contains("never")));
    }

    #[tokio::test]
    async fn a_refused_start_is_an_error_then_bye() {
        let (replies, seen) = run(&[start(false)]).await;
        assert_eq!(
            replies,
            vec![
                json!({"op": "error", "code": "isolation_refused"}),
                json!({"op": "bye"})
            ]
        );
        assert_eq!(seen, vec!["start full".to_owned(), "stop".to_owned()]);
        let (replies, _) = run(&[json!({"op": "call", "id": "1"})]).await;
        assert_eq!(replies[0], json!({"op": "error", "code": "protocol"}));
    }

    #[tokio::test]
    async fn frames_wait_for_their_ack() {
        let fake = Arc::new(Fake::default());
        let (mut to_agent, agent_in) = tokio::io::duplex(1 << 20);
        let (agent_out, from_agent) = tokio::io::duplex(1 << 20);
        let backend: Arc<dyn Backend> = Arc::clone(&fake) as Arc<dyn Backend>;
        let served = tokio::spawn(serve(backend, BufReader::new(agent_in), agent_out));
        let mut replies = BufReader::new(from_agent).lines();
        let send = |line: Value| {
            let mut text = line.to_string();
            text.push('\n');
            text
        };
        to_agent
            .write_all(send(start(true)).as_bytes())
            .await
            .unwrap();
        assert_eq!(
            serde_json::from_str::<Value>(&replies.next_line().await.unwrap().unwrap()).unwrap()["op"],
            "ready"
        );
        to_agent
            .write_all(send(json!({"op": "view", "on": true, "max_fps": 10})).as_bytes())
            .await
            .unwrap();
        let first: Value =
            serde_json::from_str(&replies.next_line().await.unwrap().unwrap()).unwrap();
        assert_eq!(first["op"], "frame");
        assert_eq!(first["seq"], 1);
        assert_eq!(first["control"], "agent");
        // No second frame before the ack.
        let early = tokio::time::timeout(Duration::from_millis(100), replies.next_line()).await;
        assert!(early.is_err());
        to_agent
            .write_all(send(json!({"op": "ack", "seq": 1})).as_bytes())
            .await
            .unwrap();
        let second: Value =
            serde_json::from_str(&replies.next_line().await.unwrap().unwrap()).unwrap();
        assert_eq!(second["seq"], 2);
        to_agent
            .write_all(send(json!({"op": "input", "event": {"type": "take_over"}})).as_bytes())
            .await
            .unwrap();
        to_agent
            .write_all(send(json!({"op": "run_ended"})).as_bytes())
            .await
            .unwrap();
        to_agent
            .write_all(
                send(json!({"op": "call", "id": 9, "name": "browser_open", "args": {}})).as_bytes(),
            )
            .await
            .unwrap();
        let refused: Value =
            serde_json::from_str(&replies.next_line().await.unwrap().unwrap()).unwrap();
        assert_eq!(refused, json!({"op": "error", "code": "internal", "id": 9}));
        drop(to_agent);
        let last: Value =
            serde_json::from_str(&replies.next_line().await.unwrap().unwrap()).unwrap();
        assert_eq!(last["op"], "bye");
        served.await.unwrap().unwrap();
        let seen = fake.seen.lock().unwrap().clone();
        assert!(seen.contains(&"input take_over".to_owned()));
        assert!(seen.contains(&"run_ended".to_owned()));
    }

    #[test]
    fn start_lines_parse() {
        let parsed = Start::parse(&json!({"op": "start", "revision": 1, "engine": "full",
            "engine_sandbox": false, "max_pages": 3, "viewport": {"width": 1024, "height": 768},
            "proxy": {"url": "http://10.0.0.2:3128", "token": "t"}}))
        .unwrap();
        assert_eq!((parsed.width, parsed.height), (1024, 768));
        assert_eq!(
            parsed.proxy,
            Some(("http://10.0.0.2:3128".to_owned(), "t".to_owned()))
        );
        assert!(!parsed.engine_sandbox);
        assert!(
            Start::parse(&json!({"op": "start", "revision": 2, "engine_sandbox": true})).is_err()
        );
        assert!(Start::parse(&json!({"op": "start", "revision": 1})).is_err());
        let defaults = Start::parse(
            &json!({"op": "start", "revision": 1, "engine_sandbox": true, "proxy": null}),
        )
        .unwrap();
        assert_eq!((defaults.width, defaults.height), (1280, 800));
    }
}
