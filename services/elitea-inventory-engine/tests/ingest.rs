//! Ingestion against the Python engine's ids, over a checked-out tree, and
//! end to end: a source cloned from a real smart-HTTP git server on
//! loopback (`git http-backend`; `git` builds and serves the repository,
//! the engine never runs it) into PostgreSQL.
//!
//! The end-to-end tests need `INVENTORY_TEST_DSN` (see `graph_store.rs`) and
//! the `loopback-git-http` feature (`--all-features`).

use axum::body::{Body, to_bytes};
use axum::extract::{Request, State};
use axum::http::{HeaderValue, StatusCode};
use axum::response::Response;
use elitea_engine_core::stream::{Context, Line, StopSignal};
use elitea_inventory_engine::config::INGEST_NAMES;
use elitea_inventory_engine::graph::Graph;
use elitea_inventory_engine::ingest::ids::entity_id;
use elitea_inventory_engine::ingest::source::Source;
use elitea_inventory_engine::ingest::{self, ingest_tree};
use elitea_inventory_engine::store::{self, GraphKey, sources};
use elitea_repo_ingest::IngestSettings;
use elitea_repo_ingest::artifact::ArtifactCaps;
use elitea_repo_ingest::egress::EgressPolicy;
use elitea_repo_ingest::limits::IngestLimits;
use serde_json::{Value, json};
use sqlx::postgres::PgPool;
use sqlx::{Connection, PgConnection};
use std::collections::BTreeMap;
use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};

const IDS: &str = include_str!("fixtures/ingest/entity_ids.json");

#[test]
fn entity_ids_are_the_python_pipeline_s() {
    let cases: Vec<Value> = serde_json::from_str(IDS).expect("entity_ids.json");
    assert!(cases.len() > 10);
    for case in cases {
        let path = case["file_path"].as_str();
        assert_eq!(
            entity_id(
                case["type"].as_str().expect("type"),
                case["name"].as_str().expect("name"),
                path
            ),
            case["id"].as_str().expect("id"),
            "{case}"
        );
    }
}

fn context() -> (
    Context,
    tokio::sync::mpsc::UnboundedReceiver<Line>,
    StopSignal,
) {
    let (sender, receiver) = tokio::sync::mpsc::unbounded_channel();
    let stop = StopSignal::default();
    (Context::new(sender, stop.clone()), receiver, stop)
}

fn scratch(name: &str) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("inv-ingest-{name}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).expect("scratch");
    dir
}

fn write(root: &Path, path: &str, text: &[u8]) {
    let full = root.join(path);
    std::fs::create_dir_all(full.parent().expect("a parent")).expect("dirs");
    std::fs::write(full, text).expect("write");
}

fn source(extra: &Value) -> Source {
    let mut raw = json!({
        "toolkit_id": 5,
        "type": "github",
        "name": "repo",
        "github_configuration": {},
        "repository": "o/r",
    });
    if let (Value::Object(fields), Value::Object(more)) = (&mut raw, extra) {
        fields.extend(more.clone());
    }
    Source::parse(Some(&raw), &["github".to_owned()]).expect("a source")
}

/// The file nodes of a graph (`source_file`, `document_file`, …), by name.
fn file_names(graph: &Graph) -> Vec<String> {
    let mut names: Vec<String> = graph
        .nodes()
        .filter(|(_, n)| n["layer"] == json!("structure"))
        .filter_map(|(_, n)| n["name"].as_str().map(str::to_owned))
        .collect();
    names.sort_unstable();
    names
}

#[test]
fn a_tree_is_selected_as_the_loader_did_and_diffed_by_hash() {
    let root = scratch("tree");
    write(&root, "src/app.py", b"def hello():\n    return 1\n");
    write(&root, "src/vendor/lib.py", b"x = 1\n");
    write(&root, "README.md", b"# demo\n");
    write(&root, "logo.png", b"\x89PNG");
    write(&root, "empty.py", b"");
    write(&root, "latin1.txt", b"caf\xe9\n");
    write(&root, ".git/config", b"[core]\n");
    let (context, _lines, _) = context();
    let source = source(&json!({"exclude_patterns": "*/vendor/*"}));

    let mut graph = Graph::new();
    let first =
        ingest_tree(&mut graph, &source, &root, &BTreeMap::new(), &context).expect("ingests");
    assert_eq!(first.documents_processed, 2, "{first:?}");
    assert_eq!(first.skipped_blacklist, 1);
    assert_eq!(first.skipped_unsupported, 1, "logo.png");
    assert_eq!(first.skipped_empty, 1);
    assert_eq!(first.skipped_unreadable, 1, "not UTF-8");
    assert_eq!(
        first.hashes.keys().collect::<Vec<_>>(),
        ["README.md", "src/app.py"]
    );
    let app = graph
        .node(&entity_id("file", "src/app.py", Some("src/app.py")))
        .expect("the file node");
    assert_eq!(app["type"], json!("source_file"));
    assert_eq!(app["layer"], json!("structure"));
    assert_eq!(app["citations"][0]["doc_id"], json!("repo://src/app.py"));
    assert_eq!(app["line_count"], json!(3));

    // A second run: README unchanged, app.py changed, a new file, nothing
    // else gone.
    write(&root, "src/app.py", b"def hello():\n    return 2\n");
    write(&root, "src/new.go", b"package main\n");
    let second = ingest_tree(&mut graph, &source, &root, &first.hashes, &context).expect("ingests");
    assert_eq!(second.unchanged, 1);
    assert_eq!(second.documents_processed, 2);
    assert_eq!(file_names(&graph), ["README.md", "app.py", "new.go"]);
    let app = graph
        .node(&entity_id("file", "src/app.py", Some("src/app.py")))
        .expect("re-read");
    assert_eq!(
        app["citations"].as_array().map(Vec::len),
        Some(1),
        "the old citation went with the old content"
    );

    // A deleted file loses its node.
    std::fs::remove_file(root.join("README.md")).expect("rm");
    let third = ingest_tree(&mut graph, &source, &root, &second.hashes, &context).expect("ingests");
    assert_eq!(third.removed_files, 1);
    assert_eq!(file_names(&graph), ["app.py", "new.go"]);
    let _ = std::fs::remove_dir_all(&root);
}

#[test]
fn a_stop_ends_the_walk() {
    let root = scratch("stop");
    write(&root, "a.py", b"x = 1\n");
    let (context, _lines, stop) = context();
    stop.request();
    let stopped = ingest_tree(
        &mut Graph::new(),
        &source(&json!({})),
        &root,
        &BTreeMap::new(),
        &context,
    );
    assert!(stopped.is_err());
    let _ = std::fs::remove_dir_all(&root);
}

// ---------------------------------------------------------------------------
// End to end
// ---------------------------------------------------------------------------

fn git(dir: &Path, args: &[&str]) -> String {
    let out = Command::new("git")
        .args(args)
        .current_dir(dir)
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .env("GIT_CONFIG_GLOBAL", "/dev/null")
        .env("GIT_AUTHOR_NAME", "t")
        .env("GIT_AUTHOR_EMAIL", "t@example.com")
        .env("GIT_COMMITTER_NAME", "t")
        .env("GIT_COMMITTER_EMAIL", "t@example.com")
        .output()
        .expect("git");
    assert!(
        out.status.success(),
        "git {args:?}: {}",
        String::from_utf8_lossy(&out.stderr)
    );
    String::from_utf8_lossy(&out.stdout).trim().to_owned()
}

/// Commit `files` to the work tree and refresh the served bare copy.
fn publish(root: &Path, files: &[(&str, Option<&str>)]) {
    let work = root.join("work");
    for (path, text) in files {
        match text {
            Some(text) => write(&work, path, text.as_bytes()),
            None => std::fs::remove_file(work.join(path)).expect("rm"),
        }
    }
    git(&work, &["add", "-A"]);
    git(&work, &["commit", "-q", "-m", "change"]);
    let served = root.join("served/o/r.git");
    git(
        &work,
        &[
            "push",
            "-q",
            "--force",
            served.to_str().expect("utf-8"),
            "main",
        ],
    );
}

fn repository(root: &Path) {
    let work = root.join("work");
    std::fs::create_dir_all(&work).expect("work");
    git(&work, &["init", "-q", "-b", "main"]);
    write(&work, "README.md", b"# demo\n");
    write(&work, "src/app.py", b"def hello():\n    return 1\n");
    git(&work, &["add", "."]);
    git(&work, &["commit", "-q", "-m", "one"]);
    let served = root.join("served");
    std::fs::create_dir_all(served.join("o")).expect("served");
    git(
        root,
        &[
            "clone",
            "-q",
            "--bare",
            work.to_str().expect("utf-8"),
            served.join("o/r.git").to_str().expect("utf-8"),
        ],
    );
}

/// What the CGI call needs from a request, read before any await (the body
/// is not `Sync`, so a borrow of the request cannot cross one).
fn request_parts(request: &Request) -> [String; 6] {
    let header = |name: &str| {
        request
            .headers()
            .get(name)
            .and_then(|v| v.to_str().ok())
            .map(str::to_owned)
            .unwrap_or_default()
    };
    [
        request.method().to_string(),
        request.uri().path().to_owned(),
        request.uri().query().unwrap_or_default().to_owned(),
        header("content-type"),
        header("git-protocol"),
        header("content-encoding"),
    ]
}

async fn handle(State(root): State<PathBuf>, request: Request) -> Response {
    let [method, path, query, content_type, protocol, encoding] = request_parts(&request);
    let body = to_bytes(request.into_body(), usize::MAX)
        .await
        .expect("body");
    let output = tokio::task::spawn_blocking(move || {
        let mut child = Command::new("git")
            .arg("http-backend")
            .env("GIT_PROJECT_ROOT", &root)
            .env("GIT_HTTP_EXPORT_ALL", "1")
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .env("REQUEST_METHOD", method)
            .env("PATH_INFO", path)
            .env("QUERY_STRING", query)
            .env("CONTENT_TYPE", content_type)
            .env("CONTENT_LENGTH", body.len().to_string())
            .env("GIT_PROTOCOL", protocol)
            .env("HTTP_CONTENT_ENCODING", encoding)
            .env("REMOTE_ADDR", "127.0.0.1")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn()
            .expect("http-backend");
        let mut stdin = child.stdin.take().expect("stdin");
        stdin.write_all(&body).expect("body");
        drop(stdin);
        child.wait_with_output().expect("output").stdout
    })
    .await
    .expect("cgi");
    let split = output
        .windows(4)
        .position(|w| w == b"\r\n\r\n")
        .map(|i| (i, 4))
        .or_else(|| output.windows(2).position(|w| w == b"\n\n").map(|i| (i, 2)))
        .expect("CGI headers");
    let head = String::from_utf8_lossy(&output[..split.0]).to_string();
    let mut response = Response::new(Body::from(output[split.0 + split.1..].to_vec()));
    for line in head.lines() {
        let Some((name, value)) = line.split_once(':') else {
            continue;
        };
        let value = value.trim();
        if name.eq_ignore_ascii_case("status") {
            let code: u16 = value
                .split_whitespace()
                .next()
                .and_then(|c| c.parse().ok())
                .expect("status");
            *response.status_mut() = StatusCode::from_u16(code).expect("a status");
        } else {
            response.headers_mut().insert(
                axum::http::HeaderName::from_bytes(name.as_bytes()).expect("a name"),
                HeaderValue::from_str(value).expect("a value"),
            );
        }
    }
    response
}

async fn serve(root: &Path) -> u16 {
    let app = axum::Router::new()
        .fallback(handle)
        .with_state(root.join("served"));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    let port = listener.local_addr().expect("addr").port();
    tokio::spawn(async move { axum::serve(listener, app).await.expect("serve") });
    port
}

fn settings(scratch: &Path, allowlist: &str) -> IngestSettings {
    IngestSettings {
        git_allowlist: EgressPolicy::parse(Some(allowlist)).named(&INGEST_NAMES),
        limits: IngestLimits {
            names: &INGEST_NAMES,
            ..IngestLimits::default()
        },
        scratch_path: scratch.to_path_buf(),
        artifact: ArtifactCaps::default(),
    }
}

fn dsn() -> Option<String> {
    if let Some(value) = std::env::var("INVENTORY_TEST_DSN")
        .ok()
        .filter(|v| !v.trim().is_empty())
    {
        return Some(value);
    }
    assert!(
        std::env::var("INVENTORY_REQUIRE_POSTGRES").is_err(),
        "INVENTORY_TEST_DSN is not set and INVENTORY_REQUIRE_POSTGRES is: the end-to-end ingestion tests must run"
    );
    eprintln!("INVENTORY_TEST_DSN is not set: the end-to-end ingestion tests did NOT run");
    None
}

async fn database(name: &str) -> Option<PgPool> {
    let dsn = dsn()?;
    let database = format!("invi_{name}");
    let mut admin = PgConnection::connect(&dsn).await.expect("connect");
    sqlx::query(&format!("DROP DATABASE IF EXISTS {database} WITH (FORCE)"))
        .execute(&mut admin)
        .await
        .expect("drop");
    sqlx::query(&format!("CREATE DATABASE {database}"))
        .execute(&mut admin)
        .await
        .expect("create");
    let options = store::connect_options(&dsn)
        .expect("dsn")
        .database(&database);
    let pool = PgPool::connect_with(options).await.expect("pool");
    store::migrate(&pool).await.expect("migrate");
    Some(pool)
}

fn loopback_source(port: u16) -> Source {
    source(&json!({"github_configuration": {"base_url": format!("http://127.0.0.1:{port}")}}))
}

#[cfg(feature = "loopback-git-http")]
#[tokio::test]
async fn a_source_is_cloned_ingested_and_re_ingested_incrementally() {
    let Some(pool) = database("e2e").await else {
        return;
    };
    let root = scratch("e2e");
    repository(&root);
    let port = serve(&root).await;
    let key = GraphKey::new(1, 10).expect("key");
    let settings = settings(&root.join("jobs"), "127.0.0.1");
    let (context, mut lines, _) = context();

    let first = ingest::run(
        &pool,
        key,
        &loopback_source(port),
        &settings,
        &ingest::RunOptions::default(),
        &context,
    )
    .await
    .expect("the first run");
    assert_eq!(first.documents_processed, 2);
    let (graph, revision) = store::load(&pool, key)
        .await
        .expect("load")
        .expect("a graph");
    assert_eq!(file_names(&graph), ["README.md", "app.py"]);
    assert_eq!(revision, 1);
    let status = sources::status_document(&pool, key).await.expect("status");
    assert_eq!(status["sources"]["5"]["status"], json!("completed"));
    assert_eq!(status["sources"]["5"]["documents_processed"], json!(2));
    assert_eq!(status["sources"]["5"]["branch"], json!("main"));
    let mut said = Vec::new();
    while let Ok(line) = lines.try_recv() {
        said.push(line.to_json().to_string());
    }
    assert!(
        said.iter()
            .any(|l| l.contains("Ingesting from github source repo")),
        "{said:?}"
    );

    publish(
        &root,
        &[
            ("src/app.py", Some("def hello():\n    return 2\n")),
            ("README.md", None),
            ("docs/guide.md", Some("# guide\n")),
        ],
    );
    let second = ingest::run(
        &pool,
        key,
        &loopback_source(port),
        &settings,
        &ingest::RunOptions::default(),
        &context,
    )
    .await
    .expect("the second run");
    assert_eq!(
        (
            second.documents_processed,
            second.unchanged,
            second.removed_files
        ),
        (2, 0, 1),
        "{second:?}"
    );
    let (graph, revision) = store::load(&pool, key)
        .await
        .expect("load")
        .expect("a graph");
    assert_eq!(revision, 2);
    assert_eq!(file_names(&graph), ["app.py", "guide.md"]);
    assert!(
        graph
            .nodes()
            .any(|(_, n)| n["name"] == json!("hello") && n["type"] == json!("function")),
        "the re-read file's function"
    );
    let hashes = sources::document_versions(&pool, key, "repo")
        .await
        .expect("hashes");
    assert_eq!(
        hashes.keys().collect::<Vec<_>>(),
        ["docs/guide.md", "src/app.py"]
    );
    assert_eq!(
        std::fs::read_dir(root.join("jobs"))
            .map(Iterator::count)
            .ok(),
        Some(0),
        "each run's clone is removed"
    );
    let _ = std::fs::remove_dir_all(&root);
}

#[cfg(feature = "loopback-git-http")]
#[tokio::test]
async fn a_refused_clone_is_recorded_and_commits_nothing() {
    let Some(pool) = database("refused").await else {
        return;
    };
    let root = scratch("refused");
    repository(&root);
    let port = serve(&root).await;
    let key = GraphKey::new(1, 10).expect("key");
    let (context, _lines, _) = context();
    // The allowlist names another host: the clone is refused before any
    // request leaves.
    let refused = ingest::run(
        &pool,
        key,
        &loopback_source(port),
        &settings(&root.join("jobs"), "github.com"),
        &ingest::RunOptions::default(),
        &context,
    )
    .await;
    let error = refused.expect_err("refused");
    assert!(
        error.message.contains("not on the git-host allowlist"),
        "{}",
        error.message
    );
    assert!(store::load(&pool, key).await.expect("load").is_none());
    let status = sources::status_document(&pool, key).await.expect("status");
    assert_eq!(status["sources"]["5"]["status"], json!("error"));
    assert!(
        status["sources"]["5"]["error_message"]
            .as_str()
            .is_some_and(|m| m.contains("allowlist"))
    );
    let _ = std::fs::remove_dir_all(&root);
}

#[tokio::test]
async fn one_ingestion_per_graph_at_a_time() {
    let Some(pool) = database("lease").await else {
        return;
    };
    let key = GraphKey::new(1, 10).expect("key");
    let held = sources::lease(&pool, key)
        .await
        .expect("lease")
        .expect("taken");
    assert!(
        sources::lease(&pool, key).await.expect("lease").is_none(),
        "held"
    );
    assert!(
        sources::lease(&pool, GraphKey::new(1, 11).expect("key"))
            .await
            .expect("lease")
            .is_some(),
        "another graph is free"
    );
    drop(held);
    // The dropped lease's connection closes asynchronously.
    let mut retaken = None;
    for _ in 0..50 {
        retaken = sources::lease(&pool, key).await.expect("lease");
        if retaken.is_some() {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(20)).await;
    }
    assert!(retaken.is_some(), "released when dropped");
}

fn by_name(graph: &Graph, name: &str, kind: &str) -> String {
    let found = graph
        .nodes()
        .find(|(_, n)| n["name"] == json!(name) && n["type"] == json!(kind));
    match found {
        Some((id, _)) => id.to_owned(),
        None => panic!(
            "no {kind} {name}: {:?}",
            graph
                .nodes()
                .map(|(_, n)| (n["name"].clone(), n["type"].clone()))
                .collect::<Vec<_>>()
        ),
    }
}

fn edge(graph: &Graph, source: &str, target: &str) -> Option<serde_json::Map<String, Value>> {
    graph
        .edges()
        .find(|(s, t, _)| *s == source && *t == target)
        .map(|(_, _, e)| e.clone())
}

#[test]
fn code_files_add_their_symbols_and_relations() {
    let root = scratch("parse");
    write(
        &root,
        "src/users.py",
        b"from src.util import helper\n\nclass Users:\n    \"\"\"The user registry.\"\"\"\n    def create(self, name):\n        local = name\n        return helper(local)\n",
    );
    write(&root, "src/util.py", b"def helper(x):\n    return x\n");
    write(
        &root,
        "cmd/main.go",
        b"package main\n\nfunc Run() int {\n\treturn 1\n}\n",
    );
    write(
        &root,
        "app/Order.kt",
        b"package app\n\ndata class Order(val id: String)\n",
    );
    write(
        &root,
        "ios/Cart.swift",
        b"import Foundation\n\nstruct Cart {\n    var items: [String]\n}\n",
    );
    write(&root, "README.md", b"# demo\n");
    let (context, _lines, _) = context();
    let mut graph = Graph::new();
    let outcome = ingest_tree(
        &mut graph,
        &source(&json!({})),
        &root,
        &BTreeMap::new(),
        &context,
    )
    .expect("ingests");
    assert_eq!(outcome.documents_processed, 6);
    // The Kotlin and Swift ports of the Python regex parsers.
    for (name, file) in [("Order", "app/Order.kt"), ("Cart", "ios/Cart.swift")] {
        assert!(
            graph
                .nodes()
                .any(|(_, n)| n["name"] == json!(name)
                    && n["citations"][0]["file_path"] == json!(file)),
            "{name} from {file}"
        );
    }
    assert!(
        outcome.parse_errors.is_empty(),
        "{:?}",
        outcome.parse_errors
    );

    let users = by_name(&graph, "Users", "class");
    let create = by_name(&graph, "create", "method");
    let helper = by_name(&graph, "helper", "function");
    let run = by_name(&graph, "Run", "function");
    let users_file = by_name(&graph, "users.py", "source_file");
    assert!(
        graph
            .nodes()
            .all(|(_, n)| n["name"] != json!("name") && n["name"] != json!("local")),
        "no parameter or local entities"
    );
    let users_node = graph.node(&users).expect("Users");
    assert_eq!(users_node["layer"], json!("code"));
    assert_eq!(users_node["description"], json!("The user registry."));
    assert_eq!(users_node["citations"][0]["line_start"], json!(3));
    assert!(graph.node(&run).is_some());

    let contains = edge(&graph, &users, &create).expect("class contains its method");
    assert_eq!(contains["relation_type"], json!("contains"));
    let calls = edge(&graph, &create, &helper).expect("the cross-file call resolved by name");
    assert_eq!(
        calls["relation_type"],
        json!("calls"),
        "not replaced by `references`"
    );
    assert_eq!(calls["discovered_in_file"], json!("src/users.py"));
    assert_eq!(calls["source"], json!("parser"));
    let file_edge = edge(&graph, &users_file, &users).expect("the file contains its class");
    assert_eq!(file_edge["relation_type"], json!("contains"));
    let file_node = graph.node(&users_file).expect("the file node");
    assert!(file_node["entity_count"].as_u64().is_some_and(|n| n >= 2));
    assert!(
        file_node["code_entity_count"]
            .as_u64()
            .is_some_and(|n| n >= 2)
    );

    // The file changes: its symbols and edges go and come back; the helper
    // another file declares stays.
    write(&root, "src/users.py", b"class Accounts:\n    pass\n");
    let hashes = outcome.hashes.clone();
    ingest_tree(&mut graph, &source(&json!({})), &root, &hashes, &context).expect("re-ingests");
    assert!(
        graph.nodes().all(|(_, n)| n["name"] != json!("Users")),
        "the old class went"
    );
    by_name(&graph, "Accounts", "class");
    by_name(&graph, "helper", "function");
    assert!(edge(&graph, &create, &helper).is_none());
    let _ = std::fs::remove_dir_all(&root);
}

// ---------------------------------------------------------------------------
// With a model: a mock OpenAI-compatible gateway behind the real client
// ---------------------------------------------------------------------------

/// `/chat/completions` answering by prompt kind, as a model would.
async fn gateway(
    axum::extract::State(calls): axum::extract::State<
        std::sync::Arc<std::sync::atomic::AtomicUsize>,
    >,
    axum::Json(body): axum::Json<Value>,
) -> axum::Json<Value> {
    calls.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
    let prompt = body["messages"][0]["content"]
        .as_str()
        .unwrap_or_default()
        .to_owned();
    assert_eq!(
        body["temperature"],
        json!(0.0),
        "the extractors sample deterministically"
    );
    let answer = if prompt.starts_with("Extract semantic entities") {
        if prompt.contains("Refund policy") {
            r#"[{"id": "r", "type": "Business Rules", "name": "Refund window", "line_start": 3, "line_end": 9,
                 "properties": {"description": "Refunds within 30 days"}},
                {"id": "s", "type": "service", "name": "Billing Service", "line_start": 10, "line_end": 14,
                 "properties": {"description": "Issues refunds"}}]"#
        } else {
            "[]"
        }
    } else if prompt.starts_with("Extract SEMANTIC relationships") {
        r#"[{"source_id": "Billing Service", "relation_type": "implements", "target_id": "Refund window", "confidence": 0.9},
            {"source_id": "Billing Service", "relation_type": "related_to", "target_id": "Billing Service", "confidence": 0.9}]"#
    } else if prompt.starts_with("Extract factual information") {
        r#"[{"fact_type": "decision", "title": "Refunds go through Billing", "line_start": 2, "line_end": 4, "confidence": 0.8}]"#
    } else {
        "[]"
    };
    axum::Json(json!({
        "id": "x", "object": "chat.completion", "model": "m",
        "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": answer}}],
        "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
    }))
}

#[cfg(feature = "loopback-git-http")]
#[tokio::test]
async fn a_source_with_a_model_gets_entities_facts_and_relations() {
    use elitea_inventory_engine::extract::gateway_model;
    use elitea_inventory_engine::ingest::ModelOptions;
    use elitea_model_client::chat::ChatClient;
    use elitea_model_client::settings::ModelSettings;
    use elitea_model_client::transport::{Transport, TransportSettings};

    let Some(pool) = database("model").await else {
        return;
    };
    let root = scratch("model");
    repository(&root);
    let policy = format!(
        "# Refund policy\n\nRefunds are accepted within 30 days.\n{}\n## Billing Service\n\nThe Billing Service issues every refund.\n",
        "Customers ask support for a refund and support files it.\n".repeat(20)
    );
    publish(&root, &[("docs/refunds.md", Some(policy.as_str()))]);
    let port = serve(&root).await;

    let calls = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let app = axum::Router::new()
        .route("/v1/chat/completions", axum::routing::post(gateway))
        .with_state(std::sync::Arc::clone(&calls));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    let model_port = listener.local_addr().expect("addr").port();
    tokio::spawn(async move { axum::serve(listener, app).await.expect("serve") });
    let settings_value = json!({
        "api_base": format!("http://127.0.0.1:{model_port}/v1"),
        "api_key": "k",
        "model_name": "m",
        "streaming": false,
    });
    let model_settings = ModelSettings::from_llm_settings(&settings_value).expect("llm settings");
    let transport = Transport::new(&TransportSettings::default()).expect("transport");
    let (context, _lines, stop) = context();
    let model = gateway_model(ChatClient::new(transport, model_settings), stop);
    let options = ingest::RunOptions {
        model: Some(ModelOptions::new(model)),
        ..ingest::RunOptions::default()
    };

    let key = GraphKey::new(1, 10).expect("key");
    let outcome = ingest::run(
        &pool,
        key,
        &loopback_source(port),
        &settings(&root.join("jobs"), "127.0.0.1"),
        &options,
        &context,
    )
    .await
    .expect("the run");
    assert!(calls.load(std::sync::atomic::Ordering::SeqCst) >= 3);
    assert_eq!(
        outcome.model_skipped, 2,
        "README and app.py are small: {outcome:?}"
    );
    let (graph, _) = store::load(&pool, key)
        .await
        .expect("load")
        .expect("a graph");
    let rule = by_name(&graph, "Refund window", "rule");
    let service = by_name(&graph, "Billing Service", "service");
    let fact = by_name(&graph, "Refunds go through Billing", "fact");
    let rule_node = graph.node(&rule).expect("rule");
    assert!(
        rule_node.get("layer").is_none(),
        "`rule` is in no layer of LAYER_TYPE_MAPPING"
    );
    assert_eq!(
        rule_node["citations"][0]["file_path"],
        json!("docs/refunds.md")
    );
    assert_eq!(
        rule_node["properties"]["description"],
        json!("Refunds within 30 days")
    );
    assert_eq!(
        graph.node(&fact).expect("fact")["fact_type"],
        json!("decision")
    );
    let implements = edge(&graph, &service, &rule).expect("the model's relation");
    assert_eq!(implements["relation_type"], json!("implements"));
    assert_eq!(implements["source"], json!("llm"));
    assert_eq!(implements["discovered_in_file"], json!("docs/refunds.md"));
    assert!(
        edge(&graph, &service, &service).is_none(),
        "the quality pass removes self-loops"
    );
    let file = by_name(&graph, "refunds.md", "document_file");
    assert!(
        edge(&graph, &file, &rule).is_some(),
        "the file contains what the model found"
    );
    assert_eq!(graph.node(&file).expect("file")["fact_count"], json!(1));
    let _ = std::fs::remove_dir_all(&root);
}

/// Documents that are not text (ADR-0028 D4): an RTF and an e-mail in the
/// tree are extracted and cited like any file; an image is still skipped.
#[cfg(feature = "documents")]
#[test]
fn documents_in_a_tree_are_extracted() {
    let root = scratch("documents");
    write(
        &root,
        "policies/refunds.rtf",
        br"{\rtf1\ansi{\fonttbl\f0\fswiss Helvetica;}\f0\pard Refunds are accepted within thirty days.\par}",
    );
    write(
        &root,
        "mail/decision.eml",
        b"From: cfo@example.com\r\nSubject: Refund approvals\r\nContent-Type: text/plain\r\n\r\nBilling approves every refund.\r\n",
    );
    write(&root, "logo.png", b"\x89PNG\r\n\x1a\n");
    let (context, _lines, _) = context();
    let mut graph = Graph::new();
    let outcome = ingest_tree(
        &mut graph,
        &source(&json!({})),
        &root,
        &BTreeMap::new(),
        &context,
    )
    .expect("ingests");
    assert_eq!(outcome.documents_processed, 2, "{outcome:?}");
    assert_eq!(outcome.skipped_unsupported, 1, "the image");
    assert_eq!(
        outcome.documents["policies/refunds.rtf"].mime,
        "application/rtf"
    );
    let rtf = graph
        .node(&entity_id(
            "file",
            "policies/refunds.rtf",
            Some("policies/refunds.rtf"),
        ))
        .expect("the document's node");
    assert_eq!(
        rtf["citations"][0]["doc_id"],
        json!("repo://policies/refunds.rtf")
    );
    assert!(rtf["line_count"].as_u64().is_some_and(|n| n >= 1));
    assert!(
        graph
            .node(&entity_id(
                "file",
                "mail/decision.eml",
                Some("mail/decision.eml")
            ))
            .is_some()
    );
    let _ = std::fs::remove_dir_all(&root);
}
