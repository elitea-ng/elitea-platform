//! The sidecar (elitea.engine.v1, gRPC) over a REAL Unix socket — not an
//! in-process service call — so streaming, a stop mid-stream and a reader
//! that goes away behave as they will in the pod. Ports of `services/elitea-deepwiki/tests/unit/
//! test_sidecar.py`, `test_answer_tokens.py` and
//! `test_fixture_runner_todos.py`.

use elitea_deepwiki_engine::healthcheck;
use elitea_deepwiki_engine::runner::Runner;
use elitea_deepwiki_engine::runner::fixture::FixtureRunner;
use elitea_deepwiki_engine::server;
use elitea_engine_sidecar::client::EngineClient;
use serde_json::{Value, json};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;
use tokio::sync::oneshot;
use tonic::Code;

static NEXT: AtomicUsize = AtomicUsize::new(0);

/// A running sidecar on its own socket. macOS caps a socket path at 104
/// bytes, so it lives in a short directory under /tmp, not a test tempdir.
struct Sidecar {
    socket: PathBuf,
    stop: Option<oneshot::Sender<()>>,
}

impl Sidecar {
    fn start(runner: Runner) -> Self {
        let socket = PathBuf::from(format!(
            "/tmp/dwe-{}-{}/e.sock",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::SeqCst)
        ));
        let listener = server::bind(&socket).expect("bind");
        let (stop, stopped) = oneshot::channel::<()>();
        tokio::spawn(server::serve(listener, runner, async {
            let _ = stopped.await;
        }));
        Self {
            socket,
            stop: Some(stop),
        }
    }

    fn fixture(step: Duration) -> Runner {
        Runner::Fixture(FixtureRunner { step })
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

async fn client(socket: &Path) -> EngineClient {
    EngineClient::connect(socket).await.expect("connect")
}

/// The fields of a request document: the id, the tool and the arguments.
fn fields(request: &Value) -> (String, String, Value) {
    (
        request["invocation_id"]
            .as_str()
            .unwrap_or_default()
            .to_owned(),
        request["tool"].as_str().unwrap_or_default().to_owned(),
        request["arguments"].clone(),
    )
}

/// Every event of one invoke, in order, as its JSON document.
async fn invoke(socket: &Path, request: &Value) -> Vec<Value> {
    let (id, tool, arguments) = fields(request);
    client(socket)
        .await
        .invoke(&id, &tool, &arguments)
        .await
        .expect("a stream")
}

async fn active(socket: &Path) -> Value {
    client(socket).await.health().await.expect("health")["active"].clone()
}

fn key(line: &Value) -> &str {
    line.as_object()
        .and_then(|o| o.keys().next())
        .map_or("", String::as_str)
}

fn request(id: &str, tool: &str, arguments: &Value) -> Value {
    json!({"invocation_id": id, "tool": tool, "arguments": arguments})
}

#[tokio::test]
async fn the_fixture_engine_streams_progress_then_the_result() {
    let sidecar = Sidecar::start(Sidecar::fixture(Duration::ZERO));
    let lines = invoke(
        &sidecar.socket,
        &request(
            "inv-1",
            "generate_wiki",
            &json!({"query": "x", "repo_config": {"repository": "acme/notes"}, "active_branch": "main"}),
        ),
    )
    .await;
    let thinking: Vec<&str> = lines
        .iter()
        .filter_map(|l| l["thinking"].as_str())
        .collect();
    assert_eq!(
        thinking,
        [
            "Cloning the repository",
            "Indexing 12 files",
            "Planning the wiki structure",
            "Writing 3 pages",
            "Assembling the manifest"
        ]
    );
    let last = lines.last().expect("a last line");
    assert_eq!(key(last), "result");
    assert_eq!(last["result"]["success"], true);
    assert_eq!(last["result"]["wiki_id"], "acme--notes--main");
    // A generation produces a wiki, not an answer: no token lines.
    assert!(lines.iter().all(|l| key(l) != "token"));
}

#[tokio::test]
async fn an_engine_failure_becomes_an_error_line_with_type_and_category() {
    let sidecar = Sidecar::start(Runner::Unavailable);
    let lines = invoke(
        &sidecar.socket,
        &request("inv-2", "ask", &json!({"question": "q"})),
    )
    .await;
    assert_eq!(lines.len(), 1);
    assert_eq!(lines[0]["error"]["error_type"], "FileNotFoundError");
    assert_eq!(lines[0]["error"]["error_category"], "resource_not_found");
    assert!(
        lines[0]["error"]["message"]
            .as_str()
            .is_some_and(|m| m.contains("'ask' cannot run"))
    );
}

#[tokio::test]
async fn a_missing_required_argument_is_a_type_error_line() {
    let sidecar = Sidecar::start(Sidecar::fixture(Duration::ZERO));
    let lines = invoke(&sidecar.socket, &request("inv-3", "ask", &json!({}))).await;
    let last = lines.last().expect("a last line");
    assert_eq!(last["error"]["error_type"], "TypeError");
    assert_eq!(last["error"]["error_category"], "unknown_error");
}

#[tokio::test]
async fn a_stop_reaches_the_running_tool() {
    let sidecar = Sidecar::start(Sidecar::fixture(Duration::from_secs(30)));
    let socket = sidecar.socket.clone();
    let reader = tokio::spawn(async move {
        invoke(
            &socket,
            &request("inv-stop", "generate_wiki", &json!({"query": "x"})),
        )
        .await
    });
    // Wait until the run is registered, then stop it mid-pause.
    for _ in 0..100 {
        if active(&sidecar.socket).await == 1 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    assert!(
        client(&sidecar.socket)
            .await
            .stop("inv-stop")
            .await
            .expect("stop")
    );
    let lines = tokio::time::timeout(Duration::from_secs(10), reader)
        .await
        .expect("the stop ends the run long before its 30 s step")
        .expect("reader");
    assert_eq!(
        lines.first().and_then(|l| l["thinking"].as_str()),
        Some("Cloning the repository")
    );
    assert_eq!(
        lines.last(),
        Some(
            &json!({"error": {"message": "Invocation cancelled", "error_type": "RuntimeError", "error_category": "runtime_error"}})
        )
    );
}

#[tokio::test]
async fn stopping_an_unknown_invocation_says_so() {
    let sidecar = Sidecar::start(Runner::Unavailable);
    assert!(
        !client(&sidecar.socket)
            .await
            .stop("nope")
            .await
            .expect("stop")
    );
}

#[tokio::test]
async fn a_reader_that_goes_away_stops_the_run() {
    let sidecar = Sidecar::start(Sidecar::fixture(Duration::from_secs(30)));
    let mut stream = client(&sidecar.socket)
        .await
        .start("inv-gone", "generate_wiki", &json!({"query": "x"}))
        .await
        .expect("started");
    assert!(stream.message().await.expect("an event").is_some());
    drop(stream);
    let mut now = Value::Null;
    for _ in 0..200 {
        now = active(&sidecar.socket).await;
        if now == 0 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    assert_eq!(now, 0, "an abandoned run must deregister");
    // The id is free again.
    let lines = invoke(
        &sidecar.socket,
        &request("inv-gone", "resolve_wiki", &json!({"question": "q"})),
    )
    .await;
    assert_eq!(key(lines.last().expect("line")), "result");
}

#[tokio::test]
async fn the_door_refuses_what_the_host_would_never_send() {
    let sidecar = Sidecar::start(Sidecar::fixture(Duration::ZERO));
    let mut client = client(&sidecar.socket).await;
    let cases = [
        ("", "ask", json!({}), "invocation_id is required"),
        ("a", "list_wikis", json!({}), "Unknown tool: list_wikis"),
        ("a", "", json!({}), "Unknown tool: "),
        ("a", "ask", json!([]), "arguments must be an object"),
        ("a", "ask", Value::Null, "arguments must be an object"),
    ];
    for (id, tool, arguments, message) in cases {
        let refused = client
            .invoke(id, tool, &arguments)
            .await
            .expect_err("refused");
        assert_eq!(
            (refused.code(), refused.message()),
            (Code::InvalidArgument, message),
            "{tool} {arguments}"
        );
    }
}

#[tokio::test]
async fn a_second_invoke_for_a_running_id_is_a_conflict() {
    let sidecar = Sidecar::start(Sidecar::fixture(Duration::from_secs(30)));
    let mut client = client(&sidecar.socket).await;
    let first = client
        .start("dup", "generate_wiki", &json!({"query": "x"}))
        .await
        .expect("started");
    let refused = client
        .invoke("dup", "ask", &json!({"question": "q"}))
        .await
        .expect_err("a conflict");
    assert_eq!(
        (refused.code(), refused.message()),
        (Code::AlreadyExists, "dup is already running")
    );
    drop(first);
}

#[tokio::test]
async fn the_answer_arrives_as_token_lines_after_the_progress_lines() {
    let sidecar = Sidecar::start(Sidecar::fixture(Duration::ZERO));
    let lines = invoke(
        &sidecar.socket,
        &request("inv-ask", "ask", &json!({"question": "where?"})),
    )
    .await;
    let keys: Vec<&str> = lines.iter().map(key).collect();
    assert_eq!(
        keys,
        ["thinking", "thinking", "token", "token", "token", "result"]
    );
    let streamed: String = lines.iter().filter_map(|l| l["token"].as_str()).collect();
    assert_eq!(streamed, "Fixture answer to: where?");
    assert_eq!(
        lines.last().map(|l| &l["result"]["answer"]),
        Some(&json!("Fixture answer to: where?"))
    );
}

#[tokio::test]
async fn deep_research_publishes_its_plan_and_streams_its_report() {
    let sidecar = Sidecar::start(Sidecar::fixture(Duration::ZERO));
    let lines = invoke(
        &sidecar.socket,
        &request(
            "inv-dr",
            "deep_research",
            &json!({"question": "how?", "research_type": "security"}),
        ),
    )
    .await;
    let plan: Value =
        serde_json::from_str(lines[1]["thinking"].as_str().unwrap_or_default()).unwrap_or_default();
    assert_eq!(plan["event"], "todo_update");
    let report = lines
        .last()
        .map(|l| l["result"]["report"].clone())
        .unwrap_or_default();
    let streamed: String = lines.iter().filter_map(|l| l["token"].as_str()).collect();
    assert_eq!(Value::String(streamed), report);
    assert!(
        report
            .as_str()
            .is_some_and(|r| r.starts_with("# Research report (security)"))
    );
}

#[tokio::test]
async fn unresolved_attachments_are_refused_before_any_progress() {
    let sidecar = Sidecar::start(Sidecar::fixture(Duration::ZERO));
    let lines = invoke(
        &sidecar.socket,
        &request(
            "inv-ctx",
            "generate_wiki",
            &json!({"query": "x", "context_paths": ["wiki_pages/a.md"]}),
        ),
    )
    .await;
    assert_eq!(lines.len(), 1);
    assert_eq!(lines[0]["error"]["error_type"], "ValueError");
    assert_eq!(lines[0]["error"]["error_category"], "invalid_input");
    // An empty selection and its version key are dropped, not refused.
    let lines = invoke(
        &sidecar.socket,
        &request(
            "inv-ctx2",
            "ask",
            &json!({"question": "q", "context_paths": [], "context_wiki_version_id": "v1"}),
        ),
    )
    .await;
    assert_eq!(key(lines.last().expect("line")), "result");
}

#[tokio::test]
async fn health_names_the_runner_and_the_probe_reads_it() {
    let sidecar = Sidecar::start(Sidecar::fixture(Duration::ZERO));
    assert_eq!(
        client(&sidecar.socket)
            .await
            .health()
            .await
            .expect("health"),
        json!({
            "status": "UP",
            "runner": "fixture",
            "active": 0,
            // The served tools, so the host can detect capabilities.
            "tools": elitea_deepwiki_engine::runner::ENGINE_TOOLS,
        })
    );
    assert_eq!(
        healthcheck::probe(&sidecar.socket, Duration::from_secs(2)).await,
        Ok(())
    );
    let missing = PathBuf::from("/tmp/dwe-missing/e.sock");
    assert!(
        healthcheck::probe(&missing, Duration::from_secs(2))
            .await
            .is_err()
    );
}

#[test]
fn bind_refuses_to_delete_a_file_that_is_not_a_socket() {
    let dir = PathBuf::from(format!("/tmp/dwe-file-{}", std::process::id()));
    std::fs::create_dir_all(&dir).expect("dir");
    let path = dir.join("e.sock");
    std::fs::write(&path, b"keep me").expect("file");
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("runtime");
    let outcome = runtime.block_on(async { server::bind(&path).map(|_| ()) });
    assert!(outcome.is_err());
    assert_eq!(std::fs::read(&path).ok().as_deref(), Some(&b"keep me"[..]));
    let _ = std::fs::remove_dir_all(&dir);
}
