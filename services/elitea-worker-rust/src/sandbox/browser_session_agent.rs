//! The supervisor's side of the in-pod session agent's line protocol
//! (ADR-0049 decision 4; `libs/proto/elitea/runtime/v1/browser_session_agent_v1.md`).
//!
//! The supervisor execs `elitea-browser-session serve` in the session's
//! container and speaks to it over the exec's stdin and stdout, one JSON
//! object per line, as it does with the workspace runner. CDP never leaves
//! the Pod: these lines are tool calls, frames and viewer input, nothing
//! else. [`SessionAgent`] is the trait the supervisor holds; [`LineAgent`]
//! implements it over any byte pipe, so tests run it against an in-process
//! fake agent.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, PoisonError};
use std::time::Duration;

use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use tokio::io::{AsyncBufReadExt as _, AsyncRead, AsyncWrite, AsyncWriteExt as _, BufReader};
use tokio::sync::{broadcast, mpsc, oneshot, watch};

/// The protocol revision the supervisor speaks.
pub const PROTOCOL_REVISION: u32 = 1;
/// The longest line either side may write: a screenshot, base64-encoded.
pub const MAX_LINE_BYTES: usize = 12 * 1024 * 1024;
/// How long the agent may take to start its engine.
pub const READY_TIMEOUT: Duration = Duration::from_mins(1);
/// The longest one call may take: `browser_wait`'s 60 s ceiling plus a
/// navigation's margin.
pub const CALL_TIMEOUT: Duration = Duration::from_secs(90);
const QUEUE: usize = 64;

/// The first line: what the agent starts.
#[derive(Clone, Debug, Serialize)]
pub struct StartLine {
    pub engine: &'static str,
    /// `false` is `--no-sandbox` (admitted under gVisor only).
    pub engine_sandbox: bool,
    pub max_pages: u32,
    pub viewport: Viewport,
    /// The egress proxy and the run token; `None` is no network.
    pub proxy: Option<ProxyLine>,
    /// Whether a person can be asked: `false` (a schedule or webhook run,
    /// ADR-0049 decision 7) makes `browser_request_user` answer
    /// `no_person` at once.
    pub attended: bool,
}

#[derive(Clone, Copy, Debug, Serialize)]
pub struct Viewport {
    pub width: u32,
    pub height: u32,
}

impl Default for Viewport {
    fn default() -> Self {
        Self {
            width: 1280,
            height: 800,
        }
    }
}

#[derive(Clone, Debug, Serialize)]
pub struct ProxyLine {
    pub url: String,
    pub token: String,
}

#[derive(Serialize)]
#[serde(tag = "op", rename_all = "snake_case")]
enum Command<'a> {
    Start {
        revision: u32,
        #[serde(flatten)]
        start: &'a StartLine,
    },
    Call {
        id: &'a str,
        name: &'a str,
        args: &'a Value,
    },
    View {
        on: bool,
        max_fps: u32,
    },
    Ack {
        seq: u64,
    },
    Input {
        event: &'a Value,
    },
    RunEnded,
    Destroy,
}

#[derive(Deserialize)]
#[serde(tag = "op", rename_all = "snake_case")]
enum Reply {
    Ready {
        #[serde(default)]
        engine_version: String,
    },
    Result {
        id: String,
        result: Value,
        #[serde(default)]
        image: Option<ImageLine>,
    },
    Frame {
        seq: u64,
        jpeg: String,
        width: u32,
        height: u32,
        #[serde(default)]
        url: String,
        #[serde(default)]
        title: String,
        #[serde(default)]
        control: String,
    },
    Notice {
        code: String,
    },
    Error {
        code: String,
        #[serde(default)]
        id: Option<String>,
    },
    Bye,
    #[serde(other)]
    Unknown,
}

#[derive(Deserialize)]
struct ImageLine {
    media_type: String,
    data: String,
}

/// One call's answer.
#[derive(Clone, Debug, PartialEq)]
pub struct CallResult {
    pub result: Value,
    pub image: Option<(String, Vec<u8>)>,
}

/// Who drives the page.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum Control {
    #[default]
    Agent,
    User,
}

/// One screencast frame.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Frame {
    pub seq: u64,
    pub jpeg: Vec<u8>,
    pub width: u32,
    pub height: u32,
    pub url: String,
    pub title: String,
    pub control: Control,
}

/// Why the agent cannot answer. Data-free.
#[derive(Clone, Debug, Eq, PartialEq, thiserror::Error)]
pub enum AgentError {
    #[error("the session agent is gone")]
    Gone,
    #[error("the session agent did not answer in time")]
    Timeout,
    /// The agent answered with a stable error code.
    #[error("the session agent failed: {0}")]
    Failed(String),
}

/// The in-pod session agent, as the supervisor holds it.
#[async_trait]
pub trait SessionAgent: Send + Sync + 'static {
    /// Start the engine; the engine's version when it is ready.
    async fn start(&self, start: &StartLine) -> Result<String, AgentError>;
    /// One browser tool call.
    async fn call(&self, id: &str, name: &str, args: &Value) -> Result<CallResult, AgentError>;
    /// A viewer attached: the screencast runs while at least one is.
    async fn viewer_attached(&self, max_fps: u32);
    /// A viewer detached.
    async fn viewer_detached(&self);
    /// The latest frame, for viewers.
    fn frames(&self) -> watch::Receiver<Option<Arc<Frame>>>;
    /// The agent's notices a viewer is shown ([`VIEWER_NOTICES`]), from
    /// subscription on.
    fn notices(&self) -> broadcast::Receiver<&'static str>;
    /// A viewer showed frame `seq`.
    async fn ack(&self, seq: u64);
    /// A driving viewer's input event (the line's `event` object).
    async fn input(&self, event: &Value) -> Result<(), AgentError>;
    /// The run ended: the session lingers (calls are refused before here).
    async fn run_ended(&self);
    /// Ask the agent to stop its engine and exit.
    async fn destroy(&self);
    /// Resolves when the agent is gone (its stdout ended or it failed).
    fn gone(&self) -> watch::Receiver<bool>;
}

type Pending = Arc<Mutex<HashMap<String, oneshot::Sender<Result<CallResult, AgentError>>>>>;

/// The agent notices a viewer is shown, beside the log line every notice
/// gets: `engine_switched`, an `auto` session moved from Moli to Chrome
/// (ADR-0049 decision 2, #1599). Any other code is logged only.
pub const VIEWER_NOTICES: &[&str] = &["engine_switched"];

/// [`SessionAgent`] over a pipe pair: the exec's stdin and stdout.
pub struct LineAgent {
    lines: mpsc::Sender<String>,
    pending: Pending,
    frames: watch::Receiver<Option<Arc<Frame>>>,
    notices: broadcast::Sender<&'static str>,
    gone: watch::Receiver<bool>,
    viewers: AtomicUsize,
    sequence: AtomicU64,
}

fn encode(command: &Command<'_>) -> Option<String> {
    serde_json::to_string(command).ok()
}

impl LineAgent {
    /// Run the protocol over `stdin` (to the agent) and `stdout` (from it).
    /// The pipes' tasks end when either side closes.
    #[must_use]
    pub fn new(
        stdin: impl AsyncWrite + Unpin + Send + 'static,
        stdout: impl AsyncRead + Unpin + Send + 'static,
    ) -> Arc<Self> {
        let (lines, mut queue) = mpsc::channel::<String>(QUEUE);
        let pending: Pending = Arc::new(Mutex::new(HashMap::new()));
        let (frame_tx, frames) = watch::channel(None);
        let (notices, _) = broadcast::channel(8);
        let reader_notices = notices.clone();
        let (gone_tx, gone) = watch::channel(false);
        let gone_tx = Arc::new(gone_tx);
        let writer_gone = Arc::clone(&gone_tx);
        tokio::spawn(async move {
            let mut stdin = stdin;
            while let Some(mut line) = queue.recv().await {
                line.push('\n');
                if stdin.write_all(line.as_bytes()).await.is_err() || stdin.flush().await.is_err() {
                    break;
                }
            }
            let _ = stdin.shutdown().await;
            writer_gone.send_replace(true);
        });
        let reader_pending = Arc::clone(&pending);
        tokio::spawn(async move {
            read_replies(stdout, &reader_pending, &frame_tx, &reader_notices).await;
            gone_tx.send_replace(true);
            let waiting: Vec<_> = reader_pending
                .lock()
                .unwrap_or_else(PoisonError::into_inner)
                .drain()
                .collect();
            for (_, waiter) in waiting {
                let _ = waiter.send(Err(AgentError::Gone));
            }
        });
        Arc::new(Self {
            lines,
            pending,
            frames,
            notices,
            gone,
            viewers: AtomicUsize::new(0),
            sequence: AtomicU64::new(0),
        })
    }

    async fn send(&self, command: &Command<'_>) -> Result<(), AgentError> {
        let line = encode(command).ok_or(AgentError::Gone)?;
        if line.len() > MAX_LINE_BYTES {
            return Err(AgentError::Failed("line_too_long".into()));
        }
        self.lines.send(line).await.map_err(|_| AgentError::Gone)
    }

    async fn request(
        &self,
        id: String,
        command: &Command<'_>,
        timeout: Duration,
    ) -> Result<CallResult, AgentError> {
        let (tx, rx) = oneshot::channel();
        self.pending
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .insert(id.clone(), tx);
        let outcome = async {
            self.send(command).await?;
            match tokio::time::timeout(timeout, rx).await {
                Ok(Ok(answer)) => answer,
                Ok(Err(_)) => Err(AgentError::Gone),
                Err(_) => Err(AgentError::Timeout),
            }
        }
        .await;
        self.pending
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .remove(&id);
        outcome
    }
}

/// The reply id of the start line (no call id is empty).
const START_ID: &str = "";

async fn read_replies(
    stdout: impl AsyncRead + Unpin,
    pending: &Pending,
    frames: &watch::Sender<Option<Arc<Frame>>>,
    notices: &broadcast::Sender<&'static str>,
) {
    let mut reader = BufReader::new(stdout);
    let mut line = Vec::new();
    loop {
        line.clear();
        // Bounded: a line past the limit ends the agent.
        let mut limited = (&mut reader).take(MAX_LINE_BYTES as u64 + 1);
        match limited.read_until(b'\n', &mut line).await {
            Ok(0) | Err(_) => return,
            Ok(_) if line.len() > MAX_LINE_BYTES || line.last() != Some(&b'\n') => {
                tracing::warn!(event = "browser_agent_line_refused");
                return;
            }
            Ok(_) => {}
        }
        let Ok(reply) = serde_json::from_slice::<Reply>(&line) else {
            tracing::warn!(event = "browser_agent_line_invalid");
            return;
        };
        let answer = |id: &str, value: Result<CallResult, AgentError>| {
            if let Some(waiter) = pending
                .lock()
                .unwrap_or_else(PoisonError::into_inner)
                .remove(id)
            {
                let _ = waiter.send(value);
            }
        };
        match reply {
            Reply::Ready { engine_version } => answer(
                START_ID,
                Ok(CallResult {
                    result: Value::String(engine_version),
                    image: None,
                }),
            ),
            Reply::Result { id, result, image } => {
                let image = match image {
                    None => None,
                    Some(image) => match BASE64.decode(image.data.as_bytes()) {
                        Ok(data) if image.media_type.starts_with("image/") => {
                            Some((image.media_type, data))
                        }
                        _ => {
                            answer(&id, Err(AgentError::Failed("invalid_image".into())));
                            continue;
                        }
                    },
                };
                answer(&id, Ok(CallResult { result, image }));
            }
            Reply::Frame {
                seq,
                jpeg,
                width,
                height,
                url,
                title,
                control,
            } => {
                if let Ok(jpeg) = BASE64.decode(jpeg.as_bytes()) {
                    frames.send_replace(Some(Arc::new(Frame {
                        seq,
                        jpeg,
                        width,
                        height,
                        url,
                        title,
                        control: if control == "user" {
                            Control::User
                        } else {
                            Control::Agent
                        },
                    })));
                }
            }
            Reply::Error { code, id: Some(id) } => answer(&id, Err(AgentError::Failed(code))),
            // Without an id: the start failed, or the session is broken.
            // Either way the agent is done; the waiters fail `Gone`.
            Reply::Error { code, id: None } => {
                answer(START_ID, Err(AgentError::Failed(code)));
                return;
            }
            Reply::Bye => return,
            // The agent's own non-fatal notices are logged by stable code,
            // never by content.
            Reply::Notice { code } => {
                if code.len() <= 64
                    && code
                        .bytes()
                        .all(|byte| byte.is_ascii_lowercase() || byte == b'_')
                {
                    tracing::info!(event = "browser_agent_notice", code = code.as_str());
                }
                // No viewer subscribed is not an error.
                if let Some(shown) = VIEWER_NOTICES.iter().find(|shown| **shown == code) {
                    let _ = notices.send(shown);
                }
            }
            Reply::Unknown => {}
        }
    }
}

use tokio::io::AsyncReadExt as _;

#[async_trait]
impl SessionAgent for LineAgent {
    async fn start(&self, start: &StartLine) -> Result<String, AgentError> {
        let command = Command::Start {
            revision: PROTOCOL_REVISION,
            start,
        };
        let ready = self
            .request(START_ID.to_owned(), &command, READY_TIMEOUT)
            .await?;
        Ok(ready.result.as_str().unwrap_or_default().to_owned())
    }

    async fn call(&self, id: &str, name: &str, args: &Value) -> Result<CallResult, AgentError> {
        // The agent's ids are the supervisor's: a worker's call id may repeat.
        let wire = format!("c{}", self.sequence.fetch_add(1, Ordering::Relaxed) + 1);
        tracing::debug!(event = "browser_call", call = id, wire = wire.as_str());
        self.request(
            wire.clone(),
            &Command::Call {
                id: &wire,
                name,
                args,
            },
            CALL_TIMEOUT,
        )
        .await
    }

    async fn viewer_attached(&self, max_fps: u32) {
        if self.viewers.fetch_add(1, Ordering::SeqCst) == 0 {
            let _ = self.send(&Command::View { on: true, max_fps }).await;
        }
    }

    async fn viewer_detached(&self) {
        let previous = self
            .viewers
            .try_update(Ordering::SeqCst, Ordering::SeqCst, |count| {
                count.checked_sub(1)
            })
            .unwrap_or(0);
        if previous == 1 {
            let _ = self
                .send(&Command::View {
                    on: false,
                    max_fps: 0,
                })
                .await;
        }
    }

    fn frames(&self) -> watch::Receiver<Option<Arc<Frame>>> {
        self.frames.clone()
    }

    fn notices(&self) -> broadcast::Receiver<&'static str> {
        self.notices.subscribe()
    }

    async fn ack(&self, seq: u64) {
        let _ = self.send(&Command::Ack { seq }).await;
    }

    async fn input(&self, event: &Value) -> Result<(), AgentError> {
        self.send(&Command::Input { event }).await
    }

    async fn run_ended(&self) {
        let _ = self.send(&Command::RunEnded).await;
    }

    async fn destroy(&self) {
        let _ = self.send(&Command::Destroy).await;
    }

    fn gone(&self) -> watch::Receiver<bool> {
        self.gone.clone()
    }
}

/// An in-process session agent for tests: answers the start line with
/// `ready`, echoes every call, emits a frame per `input`, and can be told
/// to stall or die.
#[cfg(test)]
pub(crate) mod fake {
    use super::*;

    /// What the fake saw, for assertions.
    #[derive(Default)]
    pub(crate) struct Seen {
        pub(crate) lines: Mutex<Vec<Value>>,
    }

    /// Serve the agent's side of the protocol on `stdin`/`stdout`.
    pub(crate) async fn serve(
        stdin: impl AsyncRead + Unpin,
        mut stdout: impl AsyncWrite + Unpin,
        seen: Arc<Seen>,
    ) {
        let mut lines = BufReader::new(stdin).lines();
        let mut seq = 0_u64;
        while let Ok(Some(line)) = lines.next_line().await {
            let value: Value = serde_json::from_str(&line).unwrap_or_default();
            seen.lines.lock().unwrap().push(value.clone());
            let reply = match value["op"].as_str() {
                Some("start") => {
                    serde_json::json!({"op":"ready","engine_version":"chrome-headless-shell/test"})
                }
                Some("call") if value["name"] == "browser_hang" => continue,
                Some("call") if value["name"] == "browser_die" => return,
                // An `auto` session moving to Chrome: a logged-only notice,
                // the viewer notice, then the call's result.
                Some("call") if value["args"]["switch"] == true => {
                    let _ = stdout
                        .write_all(b"{\"op\":\"notice\",\"code\":\"cdp_reconnected\"}\n{\"op\":\"notice\",\"code\":\"engine_switched\"}\n")
                        .await;
                    serde_json::json!({"op":"result","id":value["id"],
                        "result":{"status":"ok","engine_switched":{"from":"lite","to":"full","reason":"empty_snapshot"}}})
                }
                Some("call") if value["name"] == "browser_screenshot" => {
                    serde_json::json!({"op":"result","id":value["id"],
                        "result":{"status":"ok","media_type":"image/png"},
                        "image":{"media_type":"image/png","data":BASE64.encode(b"png")}})
                }
                Some("call") => {
                    serde_json::json!({"op":"result","id":value["id"],
                        "result":{"status":"ok","name":value["name"],"args":value["args"]}})
                }
                Some("view" | "input") => {
                    seq += 1;
                    serde_json::json!({"op":"frame","seq":seq,"jpeg":BASE64.encode(b"jpeg"),
                        "width":1280,"height":800,"url":"https://example.org/","title":"t",
                        "control":"agent"})
                }
                Some("destroy") => {
                    let _ = stdout.write_all(b"{\"op\":\"bye\"}\n").await;
                    return;
                }
                _ => continue,
            };
            let mut encoded = reply.to_string().into_bytes();
            encoded.push(b'\n');
            if stdout.write_all(&encoded).await.is_err() {
                return;
            }
            let _ = stdout.flush().await;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn pair() -> (Arc<LineAgent>, Arc<fake::Seen>) {
        let (agent_in, fake_in) = tokio::io::duplex(1 << 20);
        let (fake_out, agent_out) = tokio::io::duplex(1 << 20);
        let seen = Arc::new(fake::Seen::default());
        tokio::spawn(fake::serve(fake_in, fake_out, Arc::clone(&seen)));
        (LineAgent::new(agent_in, agent_out), seen)
    }

    fn start_line() -> StartLine {
        StartLine {
            engine: "full",
            engine_sandbox: false,
            max_pages: 3,
            viewport: Viewport::default(),
            proxy: Some(ProxyLine {
                url: "http://10.0.0.7:3128".into(),
                token: "t0k".into(),
            }),
            attended: true,
        }
    }

    #[tokio::test]
    async fn start_call_screenshot_and_destroy_round_trip() {
        let (agent, seen) = pair();
        assert_eq!(
            agent.start(&start_line()).await.expect("ready"),
            "chrome-headless-shell/test"
        );
        let args = serde_json::json!({"url":"https://example.org/"});
        let result = agent
            .call("tool-1", "browser_open", &args)
            .await
            .expect("result");
        assert_eq!(result.result["name"], "browser_open");
        assert_eq!(result.result["args"], args);
        let shot = agent
            .call("tool-2", "browser_screenshot", &serde_json::json!({}))
            .await
            .expect("image");
        assert_eq!(shot.image, Some(("image/png".to_owned(), b"png".to_vec())));
        agent.destroy().await;
        let mut gone = agent.gone();
        gone.wait_for(|gone| *gone).await.expect("gone");
        let lines = seen.lines.lock().unwrap().clone();
        assert_eq!(lines[0]["op"], "start");
        assert_eq!(lines[0]["revision"], 1);
        assert_eq!(lines[0]["engine"], "full");
        assert_eq!(lines[0]["engine_sandbox"], false);
        assert_eq!(lines[0]["max_pages"], 3);
        assert_eq!(lines[0]["viewport"]["width"], 1280);
        assert_eq!(lines[0]["proxy"]["url"], "http://10.0.0.7:3128");
        assert_eq!(lines[0]["attended"], true);
        assert_eq!(lines[1]["op"], "call");
        assert_eq!(lines[1]["id"], "c1");
        assert_eq!(lines.last().unwrap()["op"], "destroy");
    }

    #[tokio::test]
    async fn the_screencast_runs_only_while_a_viewer_is_attached() {
        let (agent, seen) = pair();
        agent.start(&start_line()).await.expect("ready");
        let mut frames = agent.frames();
        agent.viewer_attached(10).await;
        agent.viewer_attached(10).await;
        frames.changed().await.expect("a frame");
        assert_eq!(frames.borrow().as_ref().expect("frame").jpeg, b"jpeg");
        agent.viewer_detached().await;
        agent.viewer_detached().await;
        agent.viewer_detached().await;
        // Wait for the fake to see the last line.
        agent
            .call("x", "browser_snapshot", &Value::Null)
            .await
            .expect("sync");
        let views: Vec<_> = seen
            .lines
            .lock()
            .unwrap()
            .iter()
            .filter(|line| line["op"] == "view")
            .map(|line| line["on"].as_bool().unwrap())
            .collect();
        assert_eq!(views, [true, false]);
    }

    /// Only the viewer notices reach a subscriber; every notice is logged.
    #[tokio::test]
    async fn only_viewer_notices_reach_a_viewer() {
        let (agent, _) = pair();
        agent.start(&start_line()).await.expect("ready");
        let mut notices = agent.notices();
        let result = agent
            .call(
                "s",
                "browser_snapshot",
                &serde_json::json!({"switch": true}),
            )
            .await
            .expect("result");
        assert_eq!(result.result["engine_switched"]["to"], "full");
        assert_eq!(notices.recv().await.expect("notice"), "engine_switched");
        assert!(
            notices.try_recv().is_err(),
            "a logged-only notice reached a viewer"
        );
    }

    #[tokio::test(start_paused = true)]
    async fn a_dead_or_silent_agent_fails_the_call() {
        let (agent, _) = pair();
        agent.start(&start_line()).await.expect("ready");
        assert_eq!(
            agent.call("x", "browser_hang", &Value::Null).await,
            Err(AgentError::Timeout)
        );
        assert_eq!(
            agent.call("x", "browser_die", &Value::Null).await,
            Err(AgentError::Gone)
        );
        assert!(*agent.gone().borrow());
    }

    #[tokio::test]
    async fn an_overlong_or_invalid_line_ends_the_agent() {
        for garbage in [b"not json\n".to_vec(), vec![b'x'; MAX_LINE_BYTES + 8]] {
            let (agent_in, _keep) = tokio::io::duplex(64);
            let (mut fake_out, agent_out) = tokio::io::duplex(1 << 16);
            let agent = LineAgent::new(agent_in, agent_out);
            tokio::spawn(async move {
                let _ = fake_out.write_all(&garbage).await;
                let _ = fake_out.flush().await;
                tokio::time::sleep(Duration::from_secs(3600)).await;
            });
            let mut gone = agent.gone();
            gone.wait_for(|gone| *gone).await.expect("gone");
        }
    }
}
