//! Shared by the engine's integration tests: a local smart-HTTP git server
//! (`git http-backend`), a mock OpenAI-compatible gateway, and a fresh
//! PostgreSQL database per test.

#![allow(dead_code)]

use axum::body::{Body, to_bytes};
use axum::extract::{Request, State};
use axum::http::{HeaderValue, StatusCode};
use axum::response::Response;
use elitea_inventory_engine::store;
use serde_json::{Value, json};
use sqlx::postgres::PgPool;
use sqlx::{Connection, PgConnection};
use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};

pub fn scratch(name: &str) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("inv-common-{name}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).expect("scratch");
    dir
}

pub fn write(root: &Path, path: &str, text: &[u8]) {
    let full = root.join(path);
    std::fs::create_dir_all(full.parent().expect("a parent")).expect("dirs");
    std::fs::write(full, text).expect("write");
}

pub fn git(dir: &Path, args: &[&str]) -> String {
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
pub fn publish(root: &Path, files: &[(&str, Option<&str>)]) {
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

pub fn repository(root: &Path) {
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

pub async fn serve(root: &Path) -> u16 {
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

/// A fresh, migrated database `invn_<name>`, or `None` without a DSN (a
/// failure when `INVENTORY_REQUIRE_POSTGRES` is set).
pub async fn database(name: &str) -> Option<PgPool> {
    let Some(dsn) = std::env::var("INVENTORY_TEST_DSN")
        .ok()
        .filter(|v| !v.trim().is_empty())
    else {
        assert!(
            std::env::var("INVENTORY_REQUIRE_POSTGRES").is_err(),
            "INVENTORY_TEST_DSN is not set and INVENTORY_REQUIRE_POSTGRES is"
        );
        eprintln!("INVENTORY_TEST_DSN is not set: these tests did NOT run");
        return None;
    };
    let database = format!("invn_{name}");
    let mut admin = PgConnection::connect(&dsn).await.expect("connect");
    sqlx::query(&format!("DROP DATABASE IF EXISTS {database} WITH (FORCE)"))
        .execute(&mut admin)
        .await
        .expect("drop");
    sqlx::query(&format!("CREATE DATABASE {database}"))
        .execute(&mut admin)
        .await
        .expect("create");
    let pool = PgPool::connect_with(
        store::connect_options(&dsn)
            .expect("dsn")
            .database(&database),
    )
    .await
    .expect("pool");
    store::migrate(&pool).await.expect("migrate");
    Some(pool)
}

/// The DSN of [`database`]'s database `invn_<name>`.
pub fn database_url(name: &str) -> String {
    let dsn = std::env::var("INVENTORY_TEST_DSN").unwrap_or_default();
    let (base, _) = dsn.rsplit_once('/').unwrap_or((&dsn, ""));
    format!("{base}/invn_{name}")
}

/// A mock gateway: `/v1/chat/completions` answers `chat(prompt)`,
/// `/v1/embeddings` a 4-wide vector per input. Returns its base URL.
pub async fn gateway(chat: fn(&str) -> String) -> String {
    async fn completions(
        State(chat): State<fn(&str) -> String>,
        axum::Json(body): axum::Json<Value>,
    ) -> axum::Json<Value> {
        let prompt = body["messages"][0]["content"].as_str().unwrap_or_default();
        axum::Json(json!({
            "id": "x", "object": "chat.completion", "model": body["model"],
            "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": chat(prompt)}}],
            "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
        }))
    }
    async fn embeddings(axum::Json(body): axum::Json<Value>) -> axum::Json<Value> {
        let inputs = body["input"].as_array().cloned().unwrap_or_default();
        let data: Vec<Value> = inputs
            .iter()
            .enumerate()
            .map(|(index, _)| json!({"object": "embedding", "index": index, "embedding": [0.1, 0.2, 0.3, 0.4]}))
            .collect();
        axum::Json(
            json!({"object": "list", "data": data, "model": body["model"], "usage": {"prompt_tokens": 1, "total_tokens": 1}}),
        )
    }
    let app = axum::Router::new()
        .route("/v1/chat/completions", axum::routing::post(completions))
        .route("/v1/embeddings", axum::routing::post(embeddings))
        .with_state(chat);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    let port = listener.local_addr().expect("addr").port();
    tokio::spawn(async move { axum::serve(listener, app).await.expect("serve") });
    format!("http://127.0.0.1:{port}/v1")
}
