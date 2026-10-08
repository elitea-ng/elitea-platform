//! `investigate` over a REAL Unix socket: a scripted model that calls a
//! graph tool, then a source toolkit's tool (through a mock of elitea-main's
//! `test_tool` route, which must receive the invocation's bearer), then
//! answers. Needs `INVENTORY_TEST_DSN`.

mod common;

use elitea_engine_sidecar::server;
use elitea_inventory_engine::config::Settings;
use elitea_inventory_engine::graph::{Citation, Graph};
use elitea_inventory_engine::native::NativeRunner;
use elitea_inventory_engine::runner::Runner;
use elitea_inventory_engine::store::{GraphKey, sources};
use serde_json::{Value, json};
use std::collections::{BTreeMap, HashMap};
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;

type Seen = Arc<Mutex<Vec<Value>>>;

/// The model: by how many tool results the conversation already holds.
async fn completions(
    axum::extract::State(seen): axum::extract::State<Seen>,
    axum::Json(body): axum::Json<Value>,
) -> axum::Json<Value> {
    seen.lock().map(|mut s| s.push(body.clone())).ok();
    let tool_results = body["messages"].as_array().map_or(0, |m| {
        m.iter().filter(|m| m["role"] == json!("tool")).count()
    });
    let call = |name: &str, arguments: Value| {
        json!({"role": "assistant", "content": null, "tool_calls": [{
            "id": format!("call-{tool_results}"), "type": "function",
            "function": {"name": name, "arguments": arguments.to_string()}
        }]})
    };
    let message = match tool_results {
        0 => call("search_knowledge_graph", json!({"query": "refund"})),
        1 => call("repo_read_file", json!({"file_path": "docs/refunds.md"})),
        _ => json!({"role": "assistant", "content":
            "Refunds go through `RefundService`.\nSource: repo - docs/refunds.md"}),
    };
    axum::Json(json!({
        "id": "x", "object": "chat.completion", "model": "m",
        "choices": [{"index": 0, "finish_reason": "stop", "message": message}],
        "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
    }))
}

/// elitea-main's `test_tool` route.
async fn test_tool(
    axum::extract::State(seen): axum::extract::State<Seen>,
    axum::extract::Path((project, toolkit)): axum::extract::Path<(String, String)>,
    headers: axum::http::HeaderMap,
    axum::Json(body): axum::Json<Value>,
) -> axum::Json<Value> {
    let bearer = headers
        .get("authorization")
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default()
        .to_owned();
    seen.lock()
        .map(|mut s| {
            s.push(json!({"project": project, "toolkit": toolkit, "bearer": bearer, "body": body}));
        })
        .ok();
    axum::Json(
        json!({"ok": true, "task_id": "t", "tool_name": body["tool_name"],
                      "result": "# Refund policy\nRefunds within 30 days."}),
    )
}

async fn invoke(socket: &PathBuf, arguments: &Value) -> Vec<Value> {
    let mut stream = UnixStream::connect(socket).await.expect("connect");
    let payload =
        json!({"invocation_id": "i", "tool": "investigate", "arguments": arguments}).to_string();
    let request = format!(
        "POST /engine/invoke HTTP/1.1\r\nhost: engine\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{payload}",
        payload.len()
    );
    stream.write_all(request.as_bytes()).await.expect("write");
    let mut raw = Vec::new();
    tokio::time::timeout(Duration::from_mins(1), stream.read_to_end(&mut raw))
        .await
        .expect("in time")
        .expect("read");
    String::from_utf8_lossy(&raw)
        .lines()
        .filter(|line| line.starts_with('{'))
        .map(|line| serde_json::from_str(line).expect("NDJSON"))
        .collect()
}

#[tokio::test]
#[allow(clippy::too_many_lines)]
async fn investigate_asks_the_model_with_graph_and_source_tools() {
    let Some(pool) = common::database("investigate").await else {
        return;
    };
    let key = GraphKey::new(7, 70).expect("key");
    let mut graph = Graph::new();
    let cite = Citation {
        file_path: "src/refund.py".to_owned(),
        source_toolkit: Some("repo".to_owned()),
        ..Citation::default()
    };
    graph.add_entity("r1", "RefundService", "class", Some(&cite), None);
    sources::start(
        &pool,
        key,
        &sources::SourceStatus {
            toolkit_id: "5".to_owned(),
            toolkit_name: "repo".to_owned(),
            toolkit_type: "github".to_owned(),
            branch: Some("main".to_owned()),
        },
    )
    .await
    .expect("status");
    sources::complete(
        &pool,
        key,
        &graph,
        &sources::Completion {
            toolkit_id: "5",
            source_name: "repo",
            hashes: &BTreeMap::new(),
            counts: sources::RunCounts::default(),
            commit_sha: None,
        },
    )
    .await
    .expect("graph");

    let model_seen: Seen = Arc::default();
    let platform_seen: Seen = Arc::default();
    let app = axum::Router::new()
        .route("/llm/v1/chat/completions", axum::routing::post(completions))
        .with_state(Arc::clone(&model_seen))
        .merge(
            axum::Router::new()
                .route(
                    "/api/v2/elitea_core/test_tool/prompt_lib/{project}/{toolkit}",
                    axum::routing::post(test_tool),
                )
                .with_state(Arc::clone(&platform_seen)),
        );
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    let port = listener.local_addr().expect("addr").port();
    tokio::spawn(async move { axum::serve(listener, app).await.expect("serve") });

    let env: HashMap<String, String> = [
        ("ELITEA_INVENTORY_RUNNER", "native".to_owned()),
        (
            "ELITEA_INVENTORY_DATABASE_URL",
            common::database_url("investigate"),
        ),
    ]
    .into_iter()
    .map(|(k, v)| (k.to_owned(), v))
    .collect();
    let settings = Settings::from_lookup(|name| env.get(name).cloned()).expect("settings");
    let runner = Runner::Native(NativeRunner::new(settings).expect("runner"));
    let socket = PathBuf::from(format!("/tmp/iinv-{}/e.sock", std::process::id()));
    let listener = server::bind(&socket).expect("bind");
    tokio::spawn(server::serve(
        listener,
        runner,
        std::future::pending::<()>(),
    ));

    let arguments = json!({
        "family": "inventory_search", "project_id": 7, "application_id": 70,
        "params": {
            "question": "How are refunds handled?",
            "llm_model": "chat-model",
            "entity_types": "class, service",
            "llm_settings": {"api_base": format!("http://127.0.0.1:{port}/llm/v1"), "api_key": "callback-bearer", "organization": "7"},
            "output_format": "json"
        }
    });
    let lines = invoke(&socket, &arguments).await;
    let last = lines.last().cloned().unwrap_or_default();
    let result: Value =
        serde_json::from_str(last["result"]["result"].as_str().unwrap_or("null")).expect("json");
    assert_eq!(
        result["answer"],
        json!("Refunds go through `RefundService`.\nSource: repo - docs/refunds.md"),
        "{last}"
    );
    assert_eq!(result["tokens_in"], json!(30));
    assert_eq!(result["total_tokens"], json!(45));
    let tools: Vec<&str> = result["tool_calls"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(|c| c["tool"].as_str())
        .collect();
    assert_eq!(tools, ["search_knowledge_graph", "repo_read_file"]);
    assert_eq!(
        result["citations"],
        json!([{"source_toolkit": "repo", "file_path": "docs/refunds.md"}, {"entity_name": "RefundService"}])
    );

    let platform = platform_seen.lock().map(|s| s.clone()).unwrap_or_default();
    assert_eq!(platform.len(), 1);
    assert_eq!(platform[0]["project"], json!("7"));
    assert_eq!(platform[0]["toolkit"], json!("5"));
    assert_eq!(platform[0]["bearer"], json!("Bearer callback-bearer"));
    assert_eq!(platform[0]["body"]["tool_name"], json!("read_file"));
    assert_eq!(
        platform[0]["body"]["tool_params"],
        json!({"file_path": "docs/refunds.md"})
    );

    let model = model_seen.lock().map(|s| s.clone()).unwrap_or_default();
    let first = &model[0];
    let system = first["messages"][0]["content"].as_str().unwrap_or_default();
    assert!(
        system.contains(
            "## Current Settings\nDepth: 2 (relationship hops to traverse)\nMax nodes: 500"
        ),
        "{system}"
    );
    assert!(system.contains("Entity types: class, service"));
    let offered: Vec<&str> = first["tools"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(|t| t["function"]["name"].as_str())
        .collect();
    assert!(
        offered.contains(&"repo_read_file") && offered.contains(&"repo_search_code"),
        "{offered:?}"
    );
    assert!(
        !offered
            .iter()
            .any(|t| t.contains("create") || t.contains("update")),
        "read-only source tools only"
    );
    assert_eq!(first["temperature"], json!(0.1));
    let source_answer = model[2]["messages"]
        .as_array()
        .and_then(|m| m.last())
        .cloned()
        .unwrap_or_default();
    assert_eq!(
        source_answer["content"],
        json!("# Refund policy\nRefunds within 30 days.")
    );
}

#[tokio::test]
async fn a_call_without_a_question_answers_the_python_refusal() {
    let question =
        elitea_inventory_engine::investigate::Question::from_params(&serde_json::Map::new());
    assert!(question.is_none());
    assert_eq!(
        elitea_inventory_engine::investigate::missing_question(),
        r#"{"error": "Missing required parameter: question", "usage": "Provide a 'question' parameter with your investigation query"}"#
    );
}
