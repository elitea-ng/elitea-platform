//! The sidecar protocol over a REAL Unix socket, with a toy engine: what
//! every engine gets from this crate regardless of its tools. Each engine
//! keeps its own suite for what its tools answer (`DeepWiki`:
//! `services/elitea-deepwiki-engine/tests/sidecar.rs`).

use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::stream::Context;
use elitea_engine_sidecar::{Engine, healthcheck, server};
use http_body_util::{BodyExt, Full};
use hyper::body::{Bytes, Incoming};
use hyper_util::rt::TokioIo;
use serde_json::{Map, Value, json};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;
use tokio::net::UnixStream;
use tokio::sync::oneshot;

static NEXT: AtomicUsize = AtomicUsize::new(0);

/// `echo` answers with its arguments after two progress lines and two
/// answer fragments; `wait` pauses until stopped; `fail` fails. Every
/// successful result is counted by `after_success`.
#[derive(Clone, Default)]
struct Toy {
    published: Arc<AtomicUsize>,
}

impl Engine for Toy {
    fn runner_name(&self) -> &'static str {
        "toy"
    }

    fn serves(&self, tool: &str) -> bool {
        matches!(tool, "echo" | "wait" | "fail")
    }

    async fn run(
        &self,
        tool: &str,
        arguments: Map<String, Value>,
        context: &Context,
    ) -> Result<Value, EngineError> {
        match tool {
            "echo" => {
                context.thinking("one");
                context.thinking("two");
                context.token("an ");
                context.token("");
                context.token("answer");
                Ok(json!({"success": true, "arguments": arguments}))
            }
            "wait" => {
                context.thinking("waiting");
                context.pause(Duration::from_secs(30)).await;
                context.checkpoint()?;
                Ok(json!({"success": true}))
            }
            _ => Err(EngineError::new(ErrorType::Value, "the toy refuses")),
        }
    }

    async fn after_success(&self, _tool: &str, _result: &Value, _context: &Context) {
        self.published.fetch_add(1, Ordering::SeqCst);
    }
}

/// A running sidecar on its own socket. macOS caps a socket path at 104
/// bytes, so it lives in a short directory under /tmp.
struct Sidecar {
    socket: PathBuf,
    stop: Option<oneshot::Sender<()>>,
}

impl Sidecar {
    fn start(engine: Toy) -> Self {
        let socket = PathBuf::from(format!(
            "/tmp/esc-{}-{}/e.sock",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::SeqCst)
        ));
        let listener = server::bind(&socket).expect("bind");
        let (stop, stopped) = oneshot::channel::<()>();
        tokio::spawn(server::serve(listener, engine, async {
            let _ = stopped.await;
        }));
        Self {
            socket,
            stop: Some(stop),
        }
    }
}

impl Drop for Sidecar {
    fn drop(&mut self) {
        if let Some(stop) = self.stop.take() {
            let _ = stop.send(());
        }
        if let Some(dir) = self.socket.parent() {
            let _ = std::fs::remove_dir_all(dir);
        }
    }
}

async fn send(
    socket: &Path,
    method: &str,
    path: &str,
    body: Option<&Value>,
) -> hyper::Response<Incoming> {
    let stream = UnixStream::connect(socket).await.expect("connect");
    let (mut sender, connection) = hyper::client::conn::http1::handshake(TokioIo::new(stream))
        .await
        .expect("handshake");
    tokio::spawn(connection);
    let bytes = body.map(|b| Bytes::from(b.to_string())).unwrap_or_default();
    let request = hyper::Request::builder()
        .method(method)
        .uri(path)
        .header("host", "engine")
        .header("content-type", "application/json")
        .body(Full::new(bytes))
        .expect("request");
    sender.send_request(request).await.expect("send")
}

async fn read_json(response: hyper::Response<Incoming>) -> (u16, Value) {
    let status = response.status().as_u16();
    let body = response
        .into_body()
        .collect()
        .await
        .expect("body")
        .to_bytes();
    (status, serde_json::from_slice(&body).unwrap_or(Value::Null))
}

async fn lines(response: hyper::Response<Incoming>) -> Vec<Value> {
    let body = response
        .into_body()
        .collect()
        .await
        .expect("body")
        .to_bytes();
    String::from_utf8_lossy(&body)
        .lines()
        .filter(|line| !line.trim().is_empty())
        .map(|line| serde_json::from_str(line).expect("line is JSON"))
        .collect()
}

fn request(id: &str, tool: &str, arguments: &Value) -> Value {
    json!({"invocation_id": id, "tool": tool, "arguments": arguments})
}

#[tokio::test]
async fn a_run_streams_progress_then_tokens_then_one_result() {
    let toy = Toy::default();
    let sidecar = Sidecar::start(toy.clone());
    let response = send(
        &sidecar.socket,
        "POST",
        "/engine/invoke",
        Some(&request("i-1", "echo", &json!({"q": 1}))),
    )
    .await;
    assert_eq!(response.status().as_u16(), 200);
    assert_eq!(
        response
            .headers()
            .get("content-type")
            .map(|v| v.to_str().unwrap_or_default()),
        Some("application/x-ndjson")
    );
    let lines = lines(response).await;
    assert_eq!(
        lines,
        [
            json!({"thinking": "one"}),
            json!({"thinking": "two"}),
            json!({"token": "an "}),
            json!({"token": "answer"}),
            json!({"result": {"success": true, "arguments": {"q": 1}}}),
        ],
        "an empty token is dropped, the result is last"
    );
    assert_eq!(
        toy.published.load(Ordering::SeqCst),
        1,
        "after_success ran once"
    );
}

#[tokio::test]
async fn a_failure_is_an_error_line_with_its_python_class_and_category() {
    let toy = Toy::default();
    let sidecar = Sidecar::start(toy.clone());
    let response = send(
        &sidecar.socket,
        "POST",
        "/engine/invoke",
        Some(&request("i-2", "fail", &json!({}))),
    )
    .await;
    let lines = lines(response).await;
    let last = lines.last().expect("a line");
    assert_eq!(last["error"]["error_type"], "ValueError");
    assert_eq!(last["error"]["message"], "the toy refuses");
    assert!(last["error"]["error_category"].is_string());
    assert_eq!(
        toy.published.load(Ordering::SeqCst),
        0,
        "no after_success on failure"
    );
}

#[tokio::test]
async fn the_door_refuses_a_bad_request_before_any_run() {
    let sidecar = Sidecar::start(Toy::default());
    for (body, status, detail) in [
        (json!([1]), 422, "the request body must be a JSON object"),
        (
            json!({"tool": "echo", "arguments": {}}),
            400,
            "invocation_id is required",
        ),
        (
            request("i-3", "nope", &json!({})),
            400,
            "Unknown tool: nope",
        ),
        (
            json!({"invocation_id": "i-3", "tool": "echo"}),
            400,
            "arguments must be an object",
        ),
    ] {
        let (code, answer) =
            read_json(send(&sidecar.socket, "POST", "/engine/invoke", Some(&body)).await).await;
        assert_eq!(code, status, "{body}");
        assert_eq!(answer["detail"], detail, "{body}");
    }
    let (code, answer) = read_json(send(&sidecar.socket, "GET", "/nowhere", None).await).await;
    assert_eq!((code, answer), (404, json!({"detail": "Not Found"})));
}

#[tokio::test]
async fn a_stop_reaches_the_running_tool_and_a_repeat_id_conflicts() {
    let sidecar = Sidecar::start(Toy::default());
    let socket = sidecar.socket.clone();
    let running = tokio::spawn(async move {
        lines(
            send(
                &socket,
                "POST",
                "/engine/invoke",
                Some(&request("i-4", "wait", &json!({}))),
            )
            .await,
        )
        .await
    });
    // Wait until the run is registered.
    for _ in 0..200 {
        let (_, health) =
            read_json(send(&sidecar.socket, "GET", "/engine/health", None).await).await;
        if health["active"] == 1 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    let (code, answer) = read_json(
        send(
            &sidecar.socket,
            "POST",
            "/engine/invoke",
            Some(&request("i-4", "wait", &json!({}))),
        )
        .await,
    )
    .await;
    assert_eq!(
        (code, answer["detail"].clone()),
        (409, json!("i-4 is already running"))
    );

    let (code, answer) = read_json(
        send(
            &sidecar.socket,
            "POST",
            "/engine/invocations/i-4/stop",
            None,
        )
        .await,
    )
    .await;
    assert_eq!((code, answer), (202, json!({"stopped": true})));
    let lines = tokio::time::timeout(Duration::from_secs(10), running)
        .await
        .expect("the run ended")
        .expect("joined");
    assert_eq!(lines.first(), Some(&json!({"thinking": "waiting"})));
    assert!(
        lines.last().is_some_and(|l| l.get("error").is_some()),
        "{lines:?}"
    );

    let (code, answer) = read_json(
        send(
            &sidecar.socket,
            "POST",
            "/engine/invocations/i-4/stop",
            None,
        )
        .await,
    )
    .await;
    assert_eq!(
        (code, answer),
        (202, json!({"stopped": false})),
        "a finished run is unknown"
    );
}

#[tokio::test]
async fn a_reader_that_goes_away_stops_the_run_and_frees_the_id() {
    let sidecar = Sidecar::start(Toy::default());
    let response = send(
        &sidecar.socket,
        "POST",
        "/engine/invoke",
        Some(&request("i-5", "wait", &json!({}))),
    )
    .await;
    assert_eq!(response.status().as_u16(), 200);
    drop(response);
    let mut active = Value::Null;
    for _ in 0..500 {
        let (_, health) =
            read_json(send(&sidecar.socket, "GET", "/engine/health", None).await).await;
        active = health["active"].clone();
        if active == 0 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    assert_eq!(active, 0, "the abandoned run was stopped and deregistered");
}

#[tokio::test]
async fn health_names_the_runner_and_the_probe_reads_it() {
    let sidecar = Sidecar::start(Toy::default());
    let (code, health) =
        read_json(send(&sidecar.socket, "GET", "/engine/health", None).await).await;
    assert_eq!(
        (code, health),
        (200, json!({"status": "UP", "runner": "toy", "active": 0}))
    );
    assert_eq!(
        healthcheck::probe(&sidecar.socket, Duration::from_secs(5)).await,
        Ok(())
    );
    let missing = sidecar.socket.with_file_name("absent.sock");
    assert!(
        healthcheck::probe(&missing, Duration::from_secs(1))
            .await
            .is_err()
    );
}

#[test]
fn bind_refuses_to_delete_a_file_that_is_not_a_socket() {
    let dir = PathBuf::from(format!("/tmp/esc-{}-file", std::process::id()));
    let _ = std::fs::create_dir_all(&dir);
    let path = dir.join("not-a-socket");
    std::fs::write(&path, b"keep me").expect("write");
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("runtime");
    let refused = runtime.block_on(async { server::bind(&path).map(drop) });
    assert!(refused.is_err());
    assert_eq!(std::fs::read(&path).ok().as_deref(), Some(&b"keep me"[..]));
    let _ = std::fs::remove_dir_all(&dir);
}
