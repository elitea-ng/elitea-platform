//! The native runner over a REAL Unix socket, as the Go host calls it:
//! `run_ingestion` against a local git server, a mock gateway and
//! PostgreSQL, then the status tools. Needs `INVENTORY_TEST_DSN` and the
//! `loopback-git-http` feature.

#![cfg(feature = "loopback-git-http")]

mod common;

use elitea_engine_sidecar::server;
use elitea_inventory_engine::config::Settings;
use elitea_inventory_engine::native::NativeRunner;
use elitea_inventory_engine::runner::Runner;
use elitea_inventory_engine::store::{self, GraphKey};
use serde_json::{Value, json};
use std::collections::HashMap;
use std::path::PathBuf;
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;

async fn invoke(socket: &PathBuf, tool: &str, arguments: &Value) -> Vec<Value> {
    let mut stream = UnixStream::connect(socket).await.expect("connect");
    let payload =
        json!({"invocation_id": format!("i-{tool}"), "tool": tool, "arguments": arguments})
            .to_string();
    let request = format!(
        "POST /engine/invoke HTTP/1.1\r\nhost: engine\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{payload}",
        payload.len()
    );
    stream.write_all(request.as_bytes()).await.expect("write");
    let mut raw = Vec::new();
    tokio::time::timeout(Duration::from_mins(1), stream.read_to_end(&mut raw))
        .await
        .expect("an answer in time")
        .expect("read");
    String::from_utf8_lossy(&raw)
        .lines()
        .filter(|line| line.starts_with('{'))
        .map(|line| serde_json::from_str(line).expect("NDJSON"))
        .collect()
}

fn answer(prompt: &str) -> String {
    if prompt.starts_with("Extract semantic entities") && prompt.contains("Refund policy") {
        r#"[{"type": "feature", "name": "Refund requests", "line_start": 1, "line_end": 5,
             "properties": {"description": "Customers request refunds"}}]"#
            .to_owned()
    } else {
        "[]".to_owned()
    }
}

#[tokio::test]
#[allow(clippy::too_many_lines)]
async fn run_ingestion_over_the_socket_builds_the_graph_and_reports_status() {
    let Some(pool) = common::database("socket").await else {
        return;
    };
    let root = common::scratch("socket");
    common::repository(&root);
    let policy = format!(
        "# Refund policy\n\n{}",
        "Customers request a refund through support within thirty days.\n".repeat(25)
    );
    common::publish(&root, &[("docs/refunds.md", Some(policy.as_str()))]);
    let git_port = common::serve(&root).await;
    let gateway = common::gateway(answer).await;

    let env: HashMap<String, String> = [
        ("ELITEA_INVENTORY_RUNNER", "native".to_owned()),
        (
            "ELITEA_INVENTORY_DATABASE_URL",
            common::database_url("socket"),
        ),
        ("ELITEA_INVENTORY_GIT_ALLOWLIST", "127.0.0.1".to_owned()),
        (
            "ELITEA_INVENTORY_SCRATCH_PATH",
            root.join("jobs").display().to_string(),
        ),
    ]
    .into_iter()
    .map(|(k, v)| (k.to_owned(), v))
    .collect();
    let settings = Settings::from_lookup(|name| env.get(name).cloned()).expect("settings");
    let runner = Runner::Native(NativeRunner::new(settings).expect("runner"));
    let socket = PathBuf::from(format!("/tmp/inat-{}/e.sock", std::process::id()));
    let listener = server::bind(&socket).expect("bind");
    tokio::spawn(server::serve(
        listener,
        runner,
        std::future::pending::<()>(),
    ));

    let arguments = json!({
        "family": "inventory",
        "project_id": 7,
        "application_id": "70",
        "params": {
            "source": {
                "toolkit_id": 5, "type": "github", "name": "repo",
                "github_configuration": {"base_url": format!("http://127.0.0.1:{git_port}")},
                "repository": "o/r"
            },
            "llm_settings": {"api_base": gateway, "api_key": "k", "organization": "7"},
            "llm_model": "chat-model",
            "embedding_model": "embed-model"
        }
    });
    let lines = invoke(&socket, "run_ingestion", &arguments).await;
    let last = lines.last().cloned().unwrap_or_default();
    let result = &last["result"];
    assert_eq!(result["success"], json!(true), "{last}");
    let text = result["result"].as_str().unwrap_or_default();
    assert!(
        text.starts_with("# Ingestion Complete: repo\n\n**Source:** repo\n**Documents:** 3\n"),
        "{text}"
    );
    assert_eq!(
        result["artifacts"],
        json!([]),
        "the graph is in PostgreSQL, not a bucket object"
    );
    let thinking: Vec<&str> = lines
        .iter()
        .filter_map(|l| l["thinking"].as_str())
        .collect();
    assert!(thinking.contains(&"Running run_ingestion"), "{thinking:?}");
    assert!(
        thinking.iter().any(|t| t.starts_with("[embeddings]")),
        "{thinking:?}"
    );

    let key = GraphKey::new(7, 70).expect("key");
    let (graph, _) = store::load(&pool, key)
        .await
        .expect("load")
        .expect("a graph");
    let feature = graph
        .nodes()
        .find(|(_, n)| n["name"] == json!("Refund requests"))
        .map(|(_, n)| n.clone())
        .expect("the model's feature");
    assert_eq!(feature["type"], json!("feature"));
    assert_eq!(feature["embedding"], json!([0.1, 0.2, 0.3, 0.4]));
    assert_eq!(graph.metadata["embeddings_model"], json!("embed-model"));
    assert_eq!(graph.metadata["embeddings_dimension"], json!(4));

    let mut status_arguments = arguments.clone();
    status_arguments["params"] = json!({"output_format": "json"});
    let status = invoke(&socket, "get_sources_status", &status_arguments).await;
    let summary: Value = serde_json::from_str(
        status
            .last()
            .and_then(|l| l["result"]["result"].as_str())
            .unwrap_or_default(),
    )
    .expect("json");
    assert_eq!(summary["total_sources"], json!(1));
    assert_eq!(summary["status_counts"]["completed"], json!(1));
    let running = invoke(&socket, "get_ingestion_status", &arguments).await;
    assert!(
        running
            .last()
            .and_then(|l| l["result"]["result"].as_str())
            .is_some_and(|t| t.starts_with("No active ingestion for this toolkit."))
    );

    // Not native yet: refused by name, not answered empty.
    let refused = invoke(&socket, "search_graph", &arguments).await;
    assert_eq!(
        refused.last().map(|l| l["error"]["error_type"].clone()),
        Some(json!("FileNotFoundError"))
    );

    // No LLM model: refused before anything runs.
    let mut no_model = arguments.clone();
    no_model["params"]
        .as_object_mut()
        .map(|p| p.remove("llm_model"));
    let refused = invoke(&socket, "run_ingestion", &no_model).await;
    assert!(
        refused
            .last()
            .and_then(|l| l["error"]["message"].as_str())
            .is_some_and(|m| m.starts_with("no LLM model is configured"))
    );
    let _ = std::fs::remove_dir_all(&root);
}
