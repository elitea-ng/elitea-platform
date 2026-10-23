//! The engine over a REAL Unix socket, through the shared sidecar server:
//! what the Go host's client sees.

use elitea_engine_sidecar::client::EngineClient;
use elitea_engine_sidecar::server;
use elitea_inventory_engine::fixture::{FixtureGraph, PACKAGED_GRAPH};
use elitea_inventory_engine::runner::{FixtureRunner, Runner};
use serde_json::{Value, json};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;
use tonic::Status;

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

/// One run over the socket: every event as its JSON document, or the
/// sidecar's refusal.
async fn invoke(
    path: &Path,
    id: &str,
    tool: &str,
    arguments: &Value,
) -> Result<Vec<Value>, Status> {
    let mut client = EngineClient::connect(path)
        .await
        .unwrap_or_else(|e| panic!("{e}"));
    tokio::time::timeout(Duration::from_secs(10), client.invoke(id, tool, arguments))
        .await
        .unwrap_or_else(|_| panic!("no answer"))
}

fn fixture() -> Runner {
    let graph = FixtureGraph::parse(PACKAGED_GRAPH).unwrap_or_else(|e| panic!("{e}"));
    Runner::Fixture(FixtureRunner::new(graph, Duration::ZERO))
}

#[tokio::test]
async fn an_ingestion_streams_progress_then_the_graph_artifacts() {
    let path = start(fixture());
    let lines = invoke(
        &path,
        "i-1",
        "run_ingestion",
        &json!({"family": "inventory", "params": {"source": {"type": "github", "id": "acme"}}}),
    )
    .await
    .unwrap_or_else(|e| panic!("{e}"));
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
    let last = invoke(
        &path,
        "i-2",
        "query_graph",
        &json!({"family": "inventory", "params": {}}),
    )
    .await
    .unwrap_or_else(|e| panic!("{e}"))
    .pop()
    .unwrap_or_default();
    assert_eq!(last["error"]["error_type"], "ValueError", "{last}");
    assert!(last["error"]["message"].as_str().is_some_and(|m| m.starts_with("Unknown tool: query_graph. Available: cleanup_cache, ")));
    let refused = invoke(&path, "i-3", "delta_update", &json!({}))
        .await
        .expect_err("a deferred tool never reaches a runner");
    assert_eq!(refused.code(), tonic::Code::InvalidArgument, "{refused:?}");
    assert_eq!(refused.message(), "Unknown tool: delta_update");
}

#[tokio::test]
async fn the_default_runner_refuses_loudly() {
    let path = start(Runner::Unavailable);
    let last = invoke(&path, "i-4", "get_stats", &json!({}))
        .await
        .unwrap_or_else(|e| panic!("{e}"))
        .pop()
        .unwrap_or_default();
    assert_eq!(last["error"]["error_type"], "FileNotFoundError", "{last}");
    assert_eq!(last["error"]["error_category"], "resource_not_found");
}
