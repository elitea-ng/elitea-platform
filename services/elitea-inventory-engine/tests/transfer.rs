//! `import-graph` / `export-graph` (issue #1129, ADR-0027 §3) through the
//! real binary and PostgreSQL (needs `INVENTORY_TEST_DSN`): a graph.json the
//! Python engine wrote is imported, exported, and comes back as the same
//! document.

mod common;

use elitea_inventory_engine::store::{self, GraphKey};
use serde_json::Value;
use std::io::Write as _;
use std::process::{Command, Output, Stdio};

/// Written by the Python `KnowledgeGraph.dump_to_json` (generators in
/// `fixtures/PROVENANCE.md`): one from the graph-store replay, one with embeddings and
/// community data.
const PYTHON_DOCUMENTS: [(&str, &str); 2] = [
    (
        "graph_store",
        include_str!("fixtures/graph_store/graph.golden.json"),
    ),
    (
        "retrieval_more",
        include_str!("fixtures/retrieval_more/graph.json"),
    ),
];

fn engine(database: &str, arguments: &[&str], stdin: Option<&str>) -> Output {
    let mut child = Command::new(env!("CARGO_BIN_EXE_elitea-inventory-engine"))
        .args(arguments)
        .env(store::DSN_ENV, common::database_url(database))
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .expect("the engine binary");
    if let Some(text) = stdin {
        child
            .stdin
            .take()
            .expect("stdin")
            .write_all(text.as_bytes())
            .expect("write stdin");
    }
    drop(child.stdin.take());
    child.wait_with_output().expect("the engine exits")
}

fn stderr(output: &Output) -> String {
    String::from_utf8_lossy(&output.stderr).into_owned()
}

/// The document with the parts an export stamps afresh set aside: the save
/// time (Python stamped every save too), and the ORDER of the ids in each
/// `_indices` entry — Python wrote `list(set)`, in string-hash order, so
/// two Python saves of one graph differ there too; here they are node order.
fn comparable(text: &str) -> Value {
    let mut document: Value = serde_json::from_str(text).expect("json");
    document["_metadata"]["last_saved"] = Value::Null;
    if let Some(indices) = document["_indices"].as_object_mut() {
        for index in indices.values_mut().filter_map(Value::as_object_mut) {
            for ids in index.values_mut().filter_map(Value::as_array_mut) {
                ids.sort_by_key(ToString::to_string);
            }
        }
    }
    document
}

#[tokio::test]
async fn a_python_graph_json_imports_and_exports_unchanged() {
    let Some(pool) = common::database("transfer").await else {
        return;
    };
    for (index, (name, document)) in PYTHON_DOCUMENTS.iter().enumerate() {
        let toolkit = (index + 1).to_string();
        let file =
            std::env::temp_dir().join(format!("inv-transfer-{name}-{}.json", std::process::id()));
        std::fs::write(&file, document).expect("write");
        let imported = engine(
            "transfer",
            &[
                "import-graph",
                "--project-id",
                "3",
                "--application-id",
                &toolkit,
                "--file",
                file.to_str().expect("utf-8"),
            ],
            None,
        );
        assert!(imported.status.success(), "{name}: {}", stderr(&imported));
        let python: Value = serde_json::from_str(document).expect("json");
        let nodes = python["nodes"].as_array().expect("nodes").len();
        assert!(
            stderr(&imported).starts_with(&format!("imported {nodes} entities and ")),
            "{}",
            stderr(&imported)
        );

        let exported = engine(
            "transfer",
            &[
                "export-graph",
                "--project-id",
                "3",
                "--application-id",
                &toolkit,
            ],
            None,
        );
        assert!(exported.status.success(), "{name}: {}", stderr(&exported));
        let text = String::from_utf8(exported.stdout).expect("utf-8");
        // Every link's `source` is its source node id in both: networkx
        // wrote it there, overwriting the edge's own provenance, and the
        // export writes the endpoint there again.
        assert_eq!(comparable(&text), comparable(document), "{name}");
        assert!(
            text.contains("\n  \"directed\": true,"),
            "json.dump(indent=2) layout"
        );

        // To a file, and idempotent: a re-import replaces it, revision + 1.
        let key = GraphKey::new(3, i64::try_from(index + 1).expect("id")).expect("key");
        let before = store::revision(&pool, key).await.expect("revision");
        let again = engine(
            "transfer",
            &[
                "import-graph",
                "--project-id",
                "3",
                "--application-id",
                &toolkit,
                "-",
            ],
            Some(&text),
        );
        assert!(again.status.success(), "{}", stderr(&again));
        assert_eq!(
            store::revision(&pool, key).await.expect("revision"),
            before.map(|r| r + 1)
        );
        let out = file.with_extension("out.json");
        let to_file = engine(
            "transfer",
            &[
                "export-graph",
                "--project-id",
                "3",
                "--application-id",
                &toolkit,
                "--file",
                out.to_str().expect("utf-8"),
            ],
            None,
        );
        assert!(to_file.status.success(), "{}", stderr(&to_file));
        assert_eq!(
            comparable(&std::fs::read_to_string(&out).expect("exported file")),
            comparable(document)
        );
        let _ = std::fs::remove_file(&file);
        let _ = std::fs::remove_file(&out);
    }
}

#[tokio::test]
async fn what_cannot_be_imported_is_refused_and_nothing_is_written() {
    let Some(pool) = common::database("transfer_refused").await else {
        return;
    };
    let key = GraphKey::new(4, 40).expect("key");
    let import = |stdin: &str, extra: &[&str]| {
        let mut arguments = vec![
            "import-graph",
            "--project-id",
            "4",
            "--application-id",
            "40",
        ];
        arguments.extend_from_slice(extra);
        engine("transfer_refused", &arguments, Some(stdin))
    };
    for (document, needle) in [
        (
            r#"{"directed": false, "nodes": []}"#,
            "the graph is undirected",
        ),
        (
            r#"{"multigraph": true, "nodes": []}"#,
            "the graph is a multigraph",
        ),
        (
            r#"{"nodes": [{"id": "a", "name": "x\u0000"}]}"#,
            "NUL character at /nodes/0/name",
        ),
        ("{", "it is not JSON"),
    ] {
        let refused = import(document, &[]);
        assert_eq!(refused.status.code(), Some(1), "{document}");
        assert!(
            stderr(&refused).contains(needle),
            "{document}: {}",
            stderr(&refused)
        );
        assert_eq!(store::revision(&pool, key).await.expect("revision"), None);
    }
    let usage = engine(
        "transfer_refused",
        &["import-graph", "--project-id", "4"],
        None,
    );
    assert_eq!(usage.status.code(), Some(2));
    assert!(stderr(&usage).contains("needs --application-id"));
    let missing = engine(
        "transfer_refused",
        &[
            "export-graph",
            "--project-id",
            "4",
            "--application-id",
            "41",
        ],
        None,
    );
    assert_eq!(missing.status.code(), Some(1));
    assert!(stderr(&missing).contains("no graph is stored for project 4, toolkit 41"));

    // Native ingestion state: refused unless replacing it is asked for.
    let document = PYTHON_DOCUMENTS[0].1;
    assert!(import(document, &[]).status.success());
    sqlx::query(
        "INSERT INTO inventory_graph.documents (project_id, application_id, source_name, document_key, version)
         VALUES (4, 40, 'repo', 'a.py', 'abc')",
    )
    .execute(&pool)
    .await
    .expect("a document version");
    let refused = import(document, &[]);
    assert_eq!(refused.status.code(), Some(1));
    assert!(
        stderr(&refused)
            .contains("already has native ingestion state (0 source(s), 1 document version(s))")
            && stderr(&refused).contains("--replace-ingestion-state"),
        "{}",
        stderr(&refused)
    );
    let replaced = import(document, &["--replace-ingestion-state"]);
    assert!(replaced.status.success(), "{}", stderr(&replaced));
    let left: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM inventory_graph.documents WHERE project_id = 4 AND application_id = 40",
    )
    .fetch_one(&pool)
    .await
    .expect("count");
    assert_eq!(left, 0);
}
