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
    assert_eq!(graph.node_count(), 3);
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
    assert_eq!(graph.node_count(), 2);
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

    let first = ingest::run(&pool, key, &loopback_source(port), &settings, &context)
        .await
        .expect("the first run");
    assert_eq!(first.documents_processed, 2);
    let (graph, revision) = store::load(&pool, key)
        .await
        .expect("load")
        .expect("a graph");
    assert_eq!((graph.node_count(), revision), (2, 1));
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
    let second = ingest::run(&pool, key, &loopback_source(port), &settings, &context)
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
    let mut names: Vec<&str> = graph
        .nodes()
        .filter_map(|(_, n)| n["name"].as_str())
        .collect();
    names.sort_unstable();
    assert_eq!(names, ["app.py", "guide.md"]);
    let hashes = sources::file_hashes(&pool, key, "repo")
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
