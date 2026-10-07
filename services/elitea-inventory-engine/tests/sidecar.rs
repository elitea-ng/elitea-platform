//! The engine over a REAL Unix socket, through the shared sidecar server:
//! what the Go host's client sees.

use elitea_engine_sidecar::server;
use elitea_inventory_engine::fixture::{FixtureGraph, PACKAGED_GRAPH};
use elitea_inventory_engine::runner::{FixtureRunner, Runner};
use serde_json::{Value, json};
use std::path::PathBuf;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;

static NEXT: AtomicUsize = AtomicUsize::new(0);

fn socket() -> PathBuf {
    PathBuf::from(format!(
        "/tmp/ieng-{}-{}/e.sock",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::SeqCst)
    ))
}

fn start(runner: Runner) -> PathBuf {
    let path = socket();
    let listener = server::bind(&path).unwrap_or_else(|e| panic!("{e}"));
    tokio::spawn(server::serve(
        listener,
        runner,
        std::future::pending::<()>(),
    ));
    path
}

/// One HTTP/1.1 request, `Connection: close`, the body read to the end.
async fn exchange(path: &PathBuf, request_path: &str, body: &Value) -> (u16, String) {
    let mut stream = UnixStream::connect(path)
        .await
        .unwrap_or_else(|e| panic!("{e}"));
    let payload = body.to_string();
    let request = format!(
        "POST {request_path} HTTP/1.1\r\nhost: engine\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{payload}",
        payload.len()
    );
    stream
        .write_all(request.as_bytes())
        .await
        .unwrap_or_else(|e| panic!("{e}"));
    let mut raw = Vec::new();
    tokio::time::timeout(Duration::from_secs(10), stream.read_to_end(&mut raw))
        .await
        .unwrap_or_else(|_| panic!("no answer"))
        .unwrap_or_else(|e| panic!("{e}"));
    let text = String::from_utf8_lossy(&raw).into_owned();
    let status = text[9..12].parse().unwrap_or(0);
    let body = text
        .split_once("\r\n\r\n")
        .map(|(_, b)| b.to_owned())
        .unwrap_or_default();
    (status, body)
}

/// The NDJSON lines of a chunked body (`{...}` lines only).
fn lines(body: &str) -> Vec<Value> {
    body.lines()
        .filter(|line| line.starts_with('{'))
        .map(|line| serde_json::from_str(line).unwrap_or_else(|e| panic!("{line}: {e}")))
        .collect()
}

fn fixture() -> Runner {
    let graph = FixtureGraph::parse(PACKAGED_GRAPH).unwrap_or_else(|e| panic!("{e}"));
    Runner::Fixture(FixtureRunner::new(graph, Duration::ZERO))
}

#[tokio::test]
async fn an_ingestion_streams_progress_then_the_graph_artifacts() {
    let path = start(fixture());
    let (status, body) = exchange(
        &path,
        "/engine/invoke",
        &json!({"invocation_id": "i-1", "tool": "run_ingestion",
                "arguments": {"family": "inventory", "params": {"source": {"type": "github", "id": "acme"}}}}),
    )
    .await;
    assert_eq!(status, 200);
    let lines = lines(&body);
    let thinking: Vec<&str> = lines
        .iter()
        .filter_map(|l| l["thinking"].as_str())
        .collect();
    assert_eq!(
        thinking,
        ["Received run_ingestion", "Reading the graph", "Done"]
    );
    let result = &lines.last().unwrap_or_else(|| panic!("no lines"))["result"];
    assert_eq!(result["success"], true);
    let names: Vec<&str> = result["artifacts"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(|a| a["name"].as_str())
        .collect();
    assert_eq!(
        names,
        [
            "graph.json",
            "sources_status.json",
            ".ingestion-checkpoint-github:acme.json"
        ]
    );
}

#[tokio::test]
async fn a_tool_of_another_family_and_an_unknown_tool_are_refused() {
    let path = start(fixture());
    let (status, body) = exchange(
        &path,
        "/engine/invoke",
        &json!({"invocation_id": "i-2", "tool": "query_graph",
                "arguments": {"family": "inventory", "params": {}}}),
    )
    .await;
    assert_eq!(status, 200);
    let last = lines(&body).pop().unwrap_or_default();
    assert_eq!(last["error"]["error_type"], "ValueError", "{last}");
    assert!(last["error"]["message"].as_str().is_some_and(|m| m.starts_with("Unknown tool: query_graph. Available: cleanup_cache, ")));
    let (status, _) = exchange(
        &path,
        "/engine/invoke",
        &json!({"invocation_id": "i-3", "tool": "delta_update", "arguments": {}}),
    )
    .await;
    assert_eq!(status, 400, "a deferred tool never reaches a runner");
}

#[tokio::test]
async fn the_default_runner_refuses_loudly() {
    let path = start(Runner::Unavailable);
    let (_, body) = exchange(
        &path,
        "/engine/invoke",
        &json!({"invocation_id": "i-4", "tool": "get_stats", "arguments": {}}),
    )
    .await;
    let last = lines(&body).pop().unwrap_or_default();
    assert_eq!(last["error"]["error_type"], "FileNotFoundError", "{last}");
    assert_eq!(last["error"]["error_category"], "resource_not_found");
}
