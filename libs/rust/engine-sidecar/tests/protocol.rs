//! The sidecar protocol (elitea.engine.v1, gRPC) over a REAL Unix socket,
//! with a toy engine: what
//! every engine gets from this crate regardless of its tools. Each engine
//! keeps its own suite for what its tools answer (`DeepWiki`:
//! `services/elitea-deepwiki-engine/tests/sidecar.rs`).
#![cfg(feature = "server")]

use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::stream::Context;
use elitea_engine_sidecar::client::EngineClient;
use elitea_engine_sidecar::{Engine, healthcheck, server};
use serde_json::{Map, Value, json};
use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;
use tokio::sync::oneshot;
use tonic::Code;

static NEXT: AtomicUsize = AtomicUsize::new(0);

/// `echo` answers with its arguments after two progress lines and two
/// answer fragments; `wait` pauses until stopped; `fail` fails. Every
/// successful result is counted by `after_success`.
#[derive(Clone, Default)]
struct Toy {
    published: Arc<AtomicUsize>,
    /// Whether the toy names its tools in the health document.
    listed: bool,
}

impl Engine for Toy {
    fn runner_name(&self) -> &'static str {
        "toy"
    }

    fn serves(&self, tool: &str) -> bool {
        matches!(tool, "echo" | "wait" | "fail")
    }

    fn tools(&self) -> &'static [&'static str] {
        if self.listed {
            &["echo", "wait", "fail"]
        } else {
            &[]
        }
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
        Self::start_with_limit(engine, server::DEFAULT_INVOKE_BYTES)
    }

    fn start_with_limit(engine: Toy, max_invoke_bytes: usize) -> Self {
        let socket = PathBuf::from(format!(
            "/tmp/esc-{}-{}/e.sock",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::SeqCst)
        ));
        let listener = server::bind(&socket).expect("bind");
        let (stop, stopped) = oneshot::channel::<()>();
        tokio::spawn(server::serve_with_limit(
            listener,
            engine,
            max_invoke_bytes,
            async {
                let _ = stopped.await;
            },
        ));
        Self {
            socket,
            stop: Some(stop),
        }
    }
}

impl Sidecar {
    async fn client(&self) -> EngineClient {
        // The server task may not have reached accept yet.
        for _ in 0..200 {
            if let Ok(client) = EngineClient::connect(&self.socket).await {
                return client;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        panic!("cannot connect to {}", self.socket.display());
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

#[tokio::test]
async fn a_run_streams_progress_then_tokens_then_one_result() {
    let toy = Toy::default();
    let sidecar = Sidecar::start(toy.clone());
    let lines = sidecar
        .client()
        .await
        .invoke("i-1", "echo", &json!({"q": 1}))
        .await
        .expect("a run");
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
async fn a_failure_is_an_error_event_with_its_python_class_and_category() {
    let toy = Toy::default();
    let sidecar = Sidecar::start(toy.clone());
    let lines = sidecar
        .client()
        .await
        .invoke("i-2", "fail", &json!({}))
        .await
        .expect("a run");
    let last = lines.last().expect("an event");
    assert_eq!(last["error"]["error_type"], "ValueError");
    assert_eq!(last["error"]["message"], "the toy refuses");
    assert!(last["error"]["error_category"].is_string());
    assert!(last["error"].get("code").is_none(), "no code, no key");
    assert_eq!(
        toy.published.load(Ordering::SeqCst),
        0,
        "no after_success on failure"
    );
}

/// The Invoke limit is the service's parameter: 2 MiB by default, whatever
/// an engine asks for otherwise. A 3 MiB document is refused by the default
/// and served under a 4 MiB limit, and one above the raised limit is refused
/// again.
#[tokio::test]
async fn the_invoke_limit_is_a_parameter_with_a_two_megabyte_default() {
    let big = |bytes: usize| json!({"document": "x".repeat(bytes)});
    let too_large = |code: Code| matches!(code, Code::ResourceExhausted | Code::OutOfRange);

    let default = Sidecar::start(Toy::default());
    let refused = default
        .client()
        .await
        .invoke("i-big", "echo", &big(3 << 20))
        .await
        .expect_err("the default is 2 MiB");
    assert!(too_large(refused.code()), "{refused:?}");

    let raised = Sidecar::start_with_limit(Toy::default(), 4 << 20);
    let mut client = raised.client().await;
    let lines = client
        .invoke("i-big", "echo", &big(3 << 20))
        .await
        .expect("served under 4 MiB");
    let last = lines.last().expect("an event");
    assert_eq!(last["result"]["success"], json!(true));
    assert_eq!(
        last["result"]["arguments"]["document"]
            .as_str()
            .map(str::len),
        Some(3 << 20),
        "the arguments reach the engine whole"
    );
    let refused = client
        .invoke("i-big-2", "echo", &big(5 << 20))
        .await
        .expect_err("above the raised limit");
    assert!(too_large(refused.code()), "{refused:?}");
}

#[tokio::test]
async fn the_door_refuses_a_bad_request_before_any_run() {
    let sidecar = Sidecar::start(Toy::default());
    let mut client = sidecar.client().await;
    for (id, tool, arguments, code, message) in [
        (
            "",
            "echo",
            json!({}),
            Code::InvalidArgument,
            "invocation_id is required",
        ),
        (
            "i-3",
            "nope",
            json!({}),
            Code::InvalidArgument,
            "Unknown tool: nope",
        ),
        (
            "i-3",
            "echo",
            json!([1]),
            Code::InvalidArgument,
            "arguments must be an object",
        ),
    ] {
        let refused = client
            .invoke(id, tool, &arguments)
            .await
            .expect_err("refused");
        assert_eq!(
            (refused.code(), refused.message()),
            (code, message),
            "{tool} {arguments}"
        );
    }
    assert_eq!(
        client.health().await.expect("health")["active"],
        0,
        "no run was registered"
    );
}

#[tokio::test]
async fn a_stop_reaches_the_running_tool_and_a_repeat_id_conflicts() {
    let sidecar = Sidecar::start(Toy::default());
    let mut client = sidecar.client().await;
    let mut runner = client.clone();
    let running = tokio::spawn(async move { runner.invoke("i-4", "wait", &json!({})).await });
    // Wait until the run is registered.
    for _ in 0..200 {
        if client.health().await.expect("health")["active"] == 1 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    let conflict = client
        .invoke("i-4", "wait", &json!({}))
        .await
        .expect_err("a repeated id");
    assert_eq!(
        (conflict.code(), conflict.message()),
        (Code::AlreadyExists, "i-4 is already running")
    );

    assert!(client.stop("i-4").await.expect("stop"));
    let lines = tokio::time::timeout(Duration::from_secs(10), running)
        .await
        .expect("the run ended")
        .expect("joined")
        .expect("a stream");
    assert_eq!(lines.first(), Some(&json!({"thinking": "waiting"})));
    assert!(
        lines.last().is_some_and(|l| l.get("error").is_some()),
        "{lines:?}"
    );

    assert!(
        !client.stop("i-4").await.expect("stop"),
        "a finished run is unknown"
    );
}

#[tokio::test]
async fn a_reader_that_goes_away_stops_the_run_and_frees_the_id() {
    let sidecar = Sidecar::start(Toy::default());
    let mut client = sidecar.client().await;
    let mut stream = client
        .start("i-5", "wait", &json!({}))
        .await
        .expect("started");
    assert!(stream.message().await.expect("an event").is_some());
    drop(stream);
    let mut active = Value::Null;
    for _ in 0..500 {
        active = client.health().await.expect("health")["active"].clone();
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
    assert_eq!(
        sidecar.client().await.health().await.expect("health"),
        json!({"status": "UP", "runner": "toy", "active": 0})
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

/// An engine that lists its tools has them in the health document; one that
/// does not (the toy) keeps the document it always had.
#[tokio::test]
async fn health_lists_the_tools_an_engine_names() {
    let sidecar = Sidecar::start(Toy {
        listed: true,
        ..Toy::default()
    });
    assert_eq!(
        sidecar.client().await.health().await.expect("health"),
        json!({"status": "UP", "runner": "toy", "active": 0, "tools": ["echo", "wait", "fail"]})
    );
}
