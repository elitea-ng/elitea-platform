//! `import_graph` / `export_graph` as socket tools (descriptor revision
//! `legacy-v2`), over a REAL Unix socket and PostgreSQL (needs
//! `INVENTORY_TEST_DSN`). The document is larger than axum's default body
//! limit, as a real graph is: the sidecar must read it.

mod common;

use elitea_engine_sidecar::server;
use elitea_inventory_engine::config::Settings;
use elitea_inventory_engine::native::NativeRunner;
use elitea_inventory_engine::runner::Runner;
use serde_json::{Value, json};
use std::collections::HashMap;
use std::path::PathBuf;
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;

async fn invoke(socket: &PathBuf, tool: &str, arguments: &Value) -> Value {
    let mut stream = UnixStream::connect(socket).await.expect("connect");
    let payload =
        json!({"invocation_id": format!("i-{tool}"), "tool": tool, "arguments": arguments})
            .to_string();
    let request = format!(
        "POST /engine/invoke HTTP/1.1\r\nhost: engine\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n",
        payload.len()
    );
    stream.write_all(request.as_bytes()).await.expect("write");
    stream.write_all(payload.as_bytes()).await.expect("write");
    let mut raw = Vec::new();
    tokio::time::timeout(Duration::from_mins(1), stream.read_to_end(&mut raw))
        .await
        .expect("an answer in time")
        .expect("read");
    let text = String::from_utf8_lossy(&raw);
    assert!(text.starts_with("HTTP/1.1 200"), "{text:.300}");
    text.lines()
        .rev()
        .find(|line| line.starts_with('{'))
        .map(|line| serde_json::from_str::<Value>(line).expect("NDJSON"))
        .unwrap_or_default()
}

/// A Python-shaped node-link document of `n` nodes in a chain.
fn document(n: usize) -> String {
    let nodes: Vec<Value> = (0..n)
        .map(|i| json!({"id": format!("code:n{i}"), "name": format!("{i:0>240}"), "type": "function"}))
        .collect();
    let links: Vec<Value> = (1..n)
        .map(|i| json!({"source": format!("code:n{}", i - 1), "target": format!("code:n{i}"), "relation_type": "calls"}))
        .collect();
    json!({"directed": true, "multigraph": false, "graph": {}, "nodes": nodes, "links": links,
           "_metadata": {"embeddings_model": "embed-model"}})
    .to_string()
}

#[tokio::test]
async fn a_bucket_graph_is_imported_and_exported_through_the_socket() {
    let Some(_pool) = common::database("transfer_tools").await else {
        return;
    };
    let env: HashMap<String, String> = [
        ("ELITEA_INVENTORY_RUNNER", "native".to_owned()),
        (
            "ELITEA_INVENTORY_DATABASE_URL",
            common::database_url("transfer_tools"),
        ),
    ]
    .into_iter()
    .map(|(k, v)| (k.to_owned(), v))
    .collect();
    let settings = Settings::from_lookup(|name| env.get(name).cloned()).expect("settings");
    let runner = Runner::Native(NativeRunner::new(settings).expect("runner"));
    let socket = PathBuf::from(format!("/tmp/itt-{}/e.sock", std::process::id()));
    let listener = server::bind(&socket).expect("bind");
    tokio::spawn(server::serve_with_limit(
        listener,
        runner,
        elitea_inventory_engine::MAX_INVOKE_BYTES,
        std::future::pending::<()>(),
    ));

    let text = document(10_000);
    assert!(
        text.len() > 2 << 20,
        "the document must exceed axum's default limit"
    );
    let call = |tool: &str, params: Value| json!({"family": "inventory", "tool": tool, "project_id": 7, "application_id": 71, "params": params});

    // An export before anything is stored names the missing graph.
    let missing = invoke(&socket, "export_graph", &call("export_graph", json!({}))).await;
    assert_eq!(
        missing["error"]["error_type"],
        json!("FileNotFoundError"),
        "{missing}"
    );

    // No document: the host did not read one, and the engine says so.
    let refused = invoke(&socket, "import_graph", &call("import_graph", json!({}))).await;
    assert_eq!(
        refused["error"]["error_type"],
        json!("ValueError"),
        "{refused}"
    );

    let imported = invoke(
        &socket,
        "import_graph",
        &call(
            "import_graph",
            json!({"graph_document": text, "output_format": "json"}),
        ),
    )
    .await;
    let result = &imported["result"];
    assert_eq!(result["success"], json!(true), "{imported:.300}");
    let report: Value =
        serde_json::from_str(result["result"].as_str().unwrap_or_default()).expect("JSON");
    assert_eq!(report["stored"], json!(true));
    assert_eq!(report["entities"], json!(10_000));
    assert_eq!(report["relations"], json!(9_999));
    assert_eq!(report["embeddings_model"], json!("embed-model"));

    let exported = invoke(&socket, "export_graph", &call("export_graph", json!({}))).await;
    let result = &exported["result"];
    assert_eq!(
        result["result"],
        json!("Exported 10000 entities and 9999 relations to graph.json.")
    );
    let artifact = &result["artifacts"][0];
    assert_eq!(artifact["name"], json!("graph.json"));
    assert_eq!(artifact["type"], json!("application/json"));
    let graph: Value =
        serde_json::from_str(artifact["data"].as_str().unwrap_or_default()).expect("graph.json");
    assert_eq!(graph["nodes"].as_array().map(Vec::len), Some(10_000));

    // A document the store cannot hold is the caller's fault, named.
    let bad = invoke(
        &socket,
        "import_graph",
        &call(
            "import_graph",
            json!({"graph_document": "{\"directed\": false}"}),
        ),
    )
    .await;
    assert_eq!(bad["error"]["error_type"], json!("ValueError"), "{bad}");
    assert!(
        bad["error"]["message"]
            .as_str()
            .is_some_and(|m| m.contains("undirected")),
        "{bad}"
    );
}

/// The destructive flag is read strictly: the string "false" must not
/// delete the toolkit's ingestion state (Python truthiness would).
#[tokio::test]
async fn replace_ingestion_state_false_string_keeps_the_state() {
    let Some(pool) = common::database("transfer_tools_flag").await else {
        return;
    };
    let env: HashMap<String, String> = [
        ("ELITEA_INVENTORY_RUNNER", "native".to_owned()),
        (
            "ELITEA_INVENTORY_DATABASE_URL",
            common::database_url("transfer_tools_flag"),
        ),
    ]
    .into_iter()
    .map(|(k, v)| (k.to_owned(), v))
    .collect();
    let settings = Settings::from_lookup(|name| env.get(name).cloned()).expect("settings");
    let runner = Runner::Native(NativeRunner::new(settings).expect("runner"));
    let socket = PathBuf::from(format!("/tmp/ittf-{}/e.sock", std::process::id()));
    let listener = server::bind(&socket).expect("bind");
    tokio::spawn(server::serve_with_limit(
        listener,
        runner,
        elitea_inventory_engine::MAX_INVOKE_BYTES,
        std::future::pending::<()>(),
    ));
    let call = |params: Value| json!({"family": "inventory", "tool": "import_graph", "project_id": 8, "application_id": 81, "params": params});
    let text = document(3);
    let first = invoke(
        &socket,
        "import_graph",
        &call(json!({"graph_document": text})),
    )
    .await;
    assert_eq!(first["result"]["success"], json!(true), "{first:.300}");
    sqlx::query(
        "INSERT INTO inventory_graph.documents (project_id, application_id, source_name, document_key, version)
         VALUES (8, 81, 'repo', 'a.py', 'abc')",
    )
    .execute(&pool)
    .await
    .expect("a document version");
    let count = || async {
        sqlx::query_scalar::<_, i64>(
            "SELECT count(*) FROM inventory_graph.documents WHERE project_id = 8 AND application_id = 81",
        )
        .fetch_one(&pool)
        .await
        .expect("count")
    };
    for off in [json!("false"), json!("0"), json!("no"), json!(0)] {
        let refused = invoke(
            &socket,
            "import_graph",
            &call(json!({"graph_document": text, "replace_ingestion_state": off})),
        )
        .await;
        assert!(refused["error"].is_object(), "{off}: {refused:.300}");
        assert_eq!(count().await, 1, "{off} must not delete the state");
    }
    let replaced = invoke(
        &socket,
        "import_graph",
        &call(json!({"graph_document": text, "replace_ingestion_state": "True"})),
    )
    .await;
    assert_eq!(
        replaced["result"]["success"],
        json!(true),
        "{replaced:.300}"
    );
    assert_eq!(count().await, 0);
}
