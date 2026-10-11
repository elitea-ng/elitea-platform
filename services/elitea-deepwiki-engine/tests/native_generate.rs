//! The `native` runner end to end (ADR-0026 phase 5c): the sidecar on a
//! real Unix socket, `generate_wiki` in the real worker CHILD process (this
//! crate's binary, `worker` subcommand), a real repository cloned from
//! `git http-backend` on loopback, a mock OpenAI-compatible gateway (the
//! e2e stub's answers, deterministic stand-in embeddings), and PostgreSQL
//! with pgvector.
//!
//! * a whole run: the result against the frozen generation contract, the
//!   published index rows, the artifacts, the progress, the clean-up;
//! * a stop while the worker embeds: the stop line, the child gone, no
//!   staging rows, no scratch directory.
//!
//! Needs `DEEPWIKI_TEST_DSN` (skipped without it, FAILS with
//! `DEEPWIKI_REQUIRE_POSTGRES=1`, as in CI) and the `loopback-git-http`
//! feature (`cargo test --all-features`): the worker child derives its clone
//! URL from `repo_config`, which is `https://` in every other build.
#![cfg(feature = "loopback-git-http")]

mod storage_common;

use axum::body::{Body, to_bytes};
use axum::extract::{Request, State};
use axum::http::{HeaderValue, StatusCode};
use axum::response::{IntoResponse, Response};
use elitea_deepwiki_engine::config::Settings;
use elitea_deepwiki_engine::graph::topology::replay::standin_embedding;
use elitea_deepwiki_engine::runner::Runner;
use elitea_deepwiki_engine::runner::native::{
    KILL_AFTER, NativeRunner, PUBLISH_KILL_MARGIN, WorkerCommand,
};
use elitea_deepwiki_engine::server;
use elitea_deepwiki_engine::storage::build::WikiRecord;
use elitea_deepwiki_engine::storage::search::IndexReader;
use elitea_deepwiki_engine::wiki::parity::stub_answer;
use http_body_util::{BodyExt, Full};
use hyper::body::Bytes;
use hyper_util::rt::TokioIo;
use serde_json::{Map, Value, json};
use sqlx::PgPool;
use std::collections::HashMap;
use std::fmt::Write as _;
use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::time::{Duration, Instant};
use tokio::net::UnixStream;
use tokio::sync::{mpsc, oneshot};

static NEXT: AtomicUsize = AtomicUsize::new(0);

/// A short scratch root (macOS caps a socket path at 104 bytes).
fn scratch(name: &str) -> PathBuf {
    let dir = PathBuf::from(format!(
        "/tmp/dwn-{name}-{}-{}",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::SeqCst)
    ));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

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
        .unwrap();
    assert!(
        out.status.success(),
        "git {args:?}: {}",
        String::from_utf8_lossy(&out.stderr)
    );
    String::from_utf8_lossy(&out.stdout).trim().to_owned()
}

/// The notes service the e2e stub describes.
const NOTES_FILES: [(&str, &str); 6] = [
    (
        "README.md",
        "# notes-service\n\nA small notes service with SQLite persistence, ranked search and bearer-token auth.\n\nSee [the design](docs/design.md).\n",
    ),
    (
        "docs/design.md",
        "# Design\n\nRequests reach `api.py`, which checks the token with `verify_token` and calls `NoteStore`.\n",
    ),
    (
        "api.py",
        "from notes.store import NoteStore\nfrom notes.search import rank_notes\nfrom auth.tokens import verify_token\n\n\ndef handle_create_note(store: NoteStore, token: str, body: str) -> int:\n    \"\"\"Create a note for the token's owner.\"\"\"\n    owner = verify_token(token)\n    return store.save_note(owner, body)\n\n\ndef handle_search(store: NoteStore, token: str, query: str) -> list:\n    \"\"\"Rank the owner's notes against the query.\"\"\"\n    owner = verify_token(token)\n    return rank_notes(store.load_notes(owner), query)\n",
    ),
    (
        "notes/store.py",
        "import sqlite3\n\n\nclass NoteStore:\n    \"\"\"Notes in one SQLite file.\"\"\"\n\n    def __init__(self, path: str) -> None:\n        self.connection = sqlite3.connect(path)\n\n    def save_note(self, owner: str, body: str) -> int:\n        cursor = self.connection.execute(\"INSERT INTO notes (owner, body) VALUES (?, ?)\", (owner, body))\n        return cursor.lastrowid\n\n    def load_notes(self, owner: str) -> list:\n        return self.connection.execute(\"SELECT body FROM notes WHERE owner = ?\", (owner,)).fetchall()\n\n    def delete_note(self, note_id: int) -> None:\n        self.connection.execute(\"DELETE FROM notes WHERE id = ?\", (note_id,))\n",
    ),
    (
        "notes/search.py",
        "def rank_notes(notes: list, query: str) -> list:\n    \"\"\"Order notes by how many query terms they share.\"\"\"\n    terms = set(query.lower().split())\n    return sorted(notes, key=lambda note: -len(terms & set(note[0].lower().split())))\n",
    ),
    (
        "auth/tokens.py",
        "import hashlib\nimport hmac\n\nSECRET = b\"change-me\"\n\n\ndef issue_token(owner: str) -> str:\n    signature = hmac.new(SECRET, owner.encode(), hashlib.sha256).hexdigest()\n    return f\"{owner}.{signature}\"\n\n\ndef verify_token(token: str) -> str:\n    owner, _, signature = token.partition(\".\")\n    if issue_token(owner) != token:\n        raise PermissionError(\"bad token\")\n    return owner\n",
    ),
];

/// The notes service the e2e stub describes, as `acme/notes-service`.
/// Returns the served root and the commit of `main`.
fn served_repository(root: &Path) -> (PathBuf, String) {
    let work = root.join("work");
    for dir in ["notes", "auth", "docs"] {
        std::fs::create_dir_all(work.join(dir)).unwrap();
    }
    git(&work, &["init", "-q", "-b", "main"]);
    for (path, text) in NOTES_FILES {
        std::fs::write(work.join(path), text).unwrap();
    }
    git(&work, &["add", "."]);
    git(&work, &["commit", "-q", "-m", "notes service"]);
    let commit = git(&work, &["rev-parse", "main"]);
    let served = root.join("served");
    std::fs::create_dir_all(served.join("acme")).unwrap();
    git(
        root,
        &[
            "clone",
            "-q",
            "--bare",
            work.to_str().unwrap(),
            served.join("acme/notes-service.git").to_str().unwrap(),
        ],
    );
    (served, commit)
}

/// `git http-backend` behind axum (see `tests/ingest_clone.rs`).
/// What the CGI call needs, read before any await (the request body is not
/// `Sync`, so a borrow of the request cannot cross one).
fn request_parts(request: &Request) -> [String; 6] {
    let header = |name: &str| {
        request
            .headers()
            .get(name)
            .and_then(|v| v.to_str().ok())
            .unwrap_or_default()
            .to_owned()
    };
    [
        header("content-type"),
        header("git-protocol"),
        header("content-encoding"),
        request.method().to_string(),
        request.uri().path().to_owned(),
        request.uri().query().unwrap_or_default().to_owned(),
    ]
}

async fn git_backend(State(root): State<PathBuf>, request: Request) -> Response {
    let [content_type, protocol, encoding, method, path, query] = request_parts(&request);
    let body = to_bytes(request.into_body(), usize::MAX).await.unwrap();
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
            .unwrap();
        let mut stdin = child.stdin.take().unwrap();
        stdin.write_all(&body).unwrap();
        drop(stdin);
        child.wait_with_output().unwrap().stdout
    })
    .await
    .unwrap();
    let split = output
        .windows(4)
        .position(|w| w == b"\r\n\r\n")
        .map(|i| (i, 4))
        .or_else(|| output.windows(2).position(|w| w == b"\n\n").map(|i| (i, 2)))
        .unwrap();
    let head = String::from_utf8_lossy(&output[..split.0]).to_string();
    let mut response = Response::new(Body::from(output[split.0 + split.1..].to_vec()));
    for line in head.lines() {
        let Some((name, value)) = line.split_once(':') else {
            continue;
        };
        let value = value.trim();
        if name.eq_ignore_ascii_case("status") {
            let code = value.split_whitespace().next().unwrap().parse().unwrap();
            *response.status_mut() = StatusCode::from_u16(code).unwrap();
        } else {
            response.headers_mut().insert(
                axum::http::HeaderName::from_bytes(name.as_bytes()).unwrap(),
                HeaderValue::from_str(value).unwrap(),
            );
        }
    }
    response
}

async fn serve_git(root: PathBuf) -> u16 {
    let app = axum::Router::new().fallback(git_backend).with_state(root);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    port
}

/// The mock gateway: chat answers are `llm_stub.answer` (streamed as SSE
/// when asked), embeddings the 16-dimension stand-in. With `hang`, an
/// embeddings request never answers.
#[derive(Clone)]
struct Gateway {
    hang: bool,
    chats: Arc<AtomicUsize>,
    embedded: Arc<AtomicUsize>,
    embedding_requested: Arc<AtomicBool>,
    /// Every request to the object API (`path?query`).
    objects_seen: Arc<std::sync::Mutex<Vec<String>>>,
}

/// The platform's object API for the bucket `docs` of project 42: the
/// notes service under `handbook/`, a folder placeholder, and one object
/// outside the folder that must not be read.
const OBJECTS_ROUTE: &str = "/api/v2/artifacts/objects/42/docs";

fn artifact_objects() -> Vec<(String, &'static str)> {
    let mut objects: Vec<(String, &'static str)> = NOTES_FILES
        .iter()
        .map(|(path, text)| (format!("handbook/{path}"), *text))
        .collect();
    objects.push(("handbook-archive/old.md".to_owned(), "# Old\n"));
    objects
}

fn objects_api(state: &Gateway, request: &Request) -> Response {
    let authorized = request
        .headers()
        .get("authorization")
        .is_some_and(|v| v.as_bytes() == b"Bearer mock-callback-bearer");
    let path = request.uri().path().to_owned();
    state.objects_seen.lock().unwrap().push(format!(
        "{path}?{}",
        request.uri().query().unwrap_or_default()
    ));
    if !authorized {
        return (StatusCode::UNAUTHORIZED, "").into_response();
    }
    if path == OBJECTS_ROUTE {
        let query = request.uri().query().unwrap_or_default();
        let prefix = if query.contains("prefix=handbook%2F") {
            "handbook/"
        } else {
            ""
        };
        let mut listed: Vec<Value> = artifact_objects()
            .iter()
            .filter(|(key, _)| key.starts_with(prefix))
            .map(|(key, text)| json!({"key": key, "size_bytes": text.len(), "modified_at": "2026-10-01T10:00:00Z"}))
            .collect();
        listed.push(
            json!({"key": "handbook/", "size_bytes": 0, "modified_at": "2026-10-01T10:00:00Z"}),
        );
        return axum::Json(json!({"objects": listed, "common_prefixes": []})).into_response();
    }
    let key = path
        .strip_prefix(&format!("{OBJECTS_ROUTE}/"))
        .unwrap_or_default();
    match artifact_objects().into_iter().find(|(k, _)| k == key) {
        Some((_, text)) => text.into_response(),
        None => (StatusCode::NOT_FOUND, "").into_response(),
    }
}

async fn gateway(State(state): State<Gateway>, request: Request) -> Response {
    let path = request.uri().path().to_owned();
    if path.starts_with("/api/v2/artifacts/") {
        return objects_api(&state, &request);
    }
    let bytes = to_bytes(request.into_body(), 64 * 1024 * 1024)
        .await
        .unwrap();
    let body: Value = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    if path.ends_with("/embeddings") {
        state.embedding_requested.store(true, Ordering::SeqCst);
        if state.hang {
            std::future::pending::<()>().await;
        }
        let inputs: Vec<String> = match &body["input"] {
            Value::String(text) => vec![text.clone()],
            Value::Array(items) => items
                .iter()
                .map(|v| v.as_str().unwrap_or_default().to_owned())
                .collect(),
            _ => Vec::new(),
        };
        state.embedded.fetch_add(inputs.len(), Ordering::SeqCst);
        let data: Vec<Value> = inputs
            .iter()
            .enumerate()
            .map(|(index, text)| json!({"object": "embedding", "index": index, "embedding": standin_embedding(text)}))
            .collect();
        return axum::Json(json!({"object": "list", "data": data, "model": body["model"], "usage": {"prompt_tokens": 1, "total_tokens": 1}})).into_response();
    }
    if !path.ends_with("/chat/completions") {
        return (StatusCode::NOT_FOUND, "").into_response();
    }
    state.chats.fetch_add(1, Ordering::SeqCst);
    let prompt = body["messages"]
        .as_array()
        .map(|messages| {
            messages
                .iter()
                .map(|m| m["content"].as_str().unwrap_or_default().to_owned())
                .collect::<Vec<_>>()
                .join("\n")
        })
        .unwrap_or_default();
    let content = stub_answer(&prompt);
    let model = body["model"].as_str().unwrap_or("stub").to_owned();
    if body["stream"].as_bool() == Some(true) {
        let mut sse = String::new();
        let chars: Vec<char> = content.chars().collect();
        for chunk in chars.chunks(400) {
            let text: String = chunk.iter().collect();
            let event = json!({"id": "chatcmpl-stub", "object": "chat.completion.chunk", "created": 0, "model": model,
                "choices": [{"index": 0, "finish_reason": null, "delta": {"content": text}}]});
            let _ = write!(sse, "data: {event}\n\n");
        }
        let last = json!({"id": "chatcmpl-stub", "object": "chat.completion.chunk", "created": 0, "model": model,
            "choices": [{"index": 0, "finish_reason": "stop", "delta": {}}]});
        let _ = write!(sse, "data: {last}\n\ndata: [DONE]\n\n");
        return Response::builder()
            .header("content-type", "text/event-stream")
            .body(Body::from(sse))
            .unwrap();
    }
    axum::Json(json!({"id": "chatcmpl-stub", "object": "chat.completion", "created": 0, "model": model,
        "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": content}}],
        "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}))
    .into_response()
}

async fn serve_gateway(hang: bool) -> (u16, Gateway) {
    let state = Gateway {
        hang,
        chats: Arc::new(AtomicUsize::new(0)),
        embedded: Arc::new(AtomicUsize::new(0)),
        embedding_requested: Arc::new(AtomicBool::new(false)),
        objects_seen: Arc::new(std::sync::Mutex::new(Vec::new())),
    };
    let app = axum::Router::new()
        .fallback(gateway)
        .with_state(state.clone());
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    (port, state)
}

/// The test database's URL: the DSN's server, the per-test database.
fn database_url(name: &str) -> String {
    let dsn = std::env::var(storage_common::DSN_ENV).unwrap();
    let mut url = reqwest::Url::parse(dsn.trim()).unwrap();
    url.set_path(&format!("/dwt_{name}"));
    url.to_string()
}

/// The sidecar with the native runner; the worker is this crate's binary
/// with `environment` (the same settings the parent reads).
struct Engine {
    socket: PathBuf,
    scratch: PathBuf,
    stop: Option<oneshot::Sender<()>>,
}

impl Engine {
    fn start(root: &Path, database: &str) -> Self {
        Self::start_with(root, database, &[])
    }

    fn start_with(root: &Path, database: &str, extra: &[(&str, &str)]) -> Self {
        let scratch = root.join("scratch");
        let mut environment: Vec<(String, String)> = [
            ("ELITEA_DEEPWIKI_RUNNER", "native".to_owned()),
            ("ELITEA_DEEPWIKI_DATABASE_URL", database_url(database)),
            (
                "ELITEA_DEEPWIKI_BUILD_OWNER",
                format!("native-e2e-{}", std::process::id()),
            ),
            ("ELITEA_DEEPWIKI_GIT_ALLOWLIST", "127.0.0.1".to_owned()),
            (
                "ELITEA_DEEPWIKI_SCRATCH_PATH",
                scratch.to_string_lossy().into_owned(),
            ),
            ("ELITEA_DEEPWIKI_WORKER_THREADS", "2".to_owned()),
        ]
        .into_iter()
        .map(|(k, v)| (k.to_owned(), v))
        .collect();
        environment.extend(
            extra
                .iter()
                .map(|(k, v)| ((*k).to_owned(), (*v).to_owned())),
        );
        let map: HashMap<String, String> = environment.iter().cloned().collect();
        let settings = Settings::from_lookup(|name| map.get(name).cloned()).unwrap();
        let runner = NativeRunner::with_worker(
            settings,
            WorkerCommand {
                program: PathBuf::from(env!("CARGO_BIN_EXE_elitea-deepwiki-engine")),
                env: environment,
            },
        )
        .unwrap();
        let socket = root.join("e.sock");
        let listener = server::bind(&socket).unwrap();
        let (stop, stopped) = oneshot::channel::<()>();
        tokio::spawn(server::serve(listener, Runner::Native(runner), async {
            let _ = stopped.await;
        }));
        Self {
            socket,
            scratch,
            stop: Some(stop),
        }
    }

    async fn post(&self, path: &str, body: &Value) -> hyper::Response<hyper::body::Incoming> {
        self.send("POST", path, body).await
    }

    async fn send(
        &self,
        method: &str,
        path: &str,
        body: &Value,
    ) -> hyper::Response<hyper::body::Incoming> {
        let stream = UnixStream::connect(&self.socket).await.unwrap();
        let (mut sender, connection) = hyper::client::conn::http1::handshake(TokioIo::new(stream))
            .await
            .unwrap();
        tokio::spawn(connection);
        let request = hyper::Request::builder()
            .method(method)
            .uri(path)
            .header("host", "engine")
            .header("content-type", "application/json")
            .body(Full::new(Bytes::from(body.to_string())))
            .unwrap();
        sender.send_request(request).await.unwrap()
    }

    /// Invoke, streaming each NDJSON line into the returned channel.
    async fn invoke(&self, id: &str, arguments: &Value) -> mpsc::UnboundedReceiver<Value> {
        let response = self
            .post(
                "/engine/invoke",
                &json!({"invocation_id": id, "tool": "generate_wiki", "arguments": arguments}),
            )
            .await;
        assert_eq!(response.status().as_u16(), 200);
        let (sender, receiver) = mpsc::unbounded_channel();
        let mut body = response.into_body();
        tokio::spawn(async move {
            let mut pending = Vec::new();
            while let Some(frame) = body.frame().await {
                let Ok(frame) = frame else { return };
                let Ok(data) = frame.into_data() else {
                    continue;
                };
                pending.extend_from_slice(&data);
                while let Some(end) = pending.iter().position(|b| *b == b'\n') {
                    let line: Vec<u8> = pending.drain(..=end).collect();
                    let value: Value = serde_json::from_slice(&line).unwrap();
                    if sender.send(value).is_err() {
                        return;
                    }
                }
            }
        });
        receiver
    }
}

impl Drop for Engine {
    fn drop(&mut self) {
        if let Some(stop) = self.stop.take() {
            let _ = stop.send(());
        }
    }
}

/// The host's argument set. `_elitea_project_id` (the host's
/// authenticated project) is deliberately NOT the `llm_settings.organization`
/// value: the index must be published into the host's project, and the
/// assertions read it back under [`storage_common::PROJECT`].
fn arguments(git_port: u16, gateway_port: u16) -> Value {
    json!({
        "_elitea_project_id": storage_common::PROJECT.to_string(),
        "query": "Document the notes service",
        "llm_settings": {
            "api_base": format!("http://127.0.0.1:{gateway_port}/llm/v1"),
            "api_key": "mock-callback-bearer",
            "organization": "42",
            "model_name": "gpt-4o",
        },
        "embedding_model": "text-embedding-3-small",
        "repo_config": {
            "provider_type": "github",
            "provider_config": {"base_url": format!("http://127.0.0.1:{git_port}")},
            "repository": "acme/notes-service",
            "branch": "main",
            "project": null,
            "is_cloud": null,
        },
        "active_branch": "main",
        "force_rebuild_index": true,
        "indexing_method": "filesystem",
        "planner_mode": "cluster",
        "exclude_tests": null,
        "run_in_subprocess": true,
    })
}

async fn count(pool: &PgPool, sql: &str) -> i64 {
    sqlx::query_scalar(sql).fetch_one(pool).await.unwrap()
}

fn fixture(name: &str) -> Value {
    let path = storage_common::repo_root()
        .join("conformance/provider/fixtures/deepwiki/generation")
        .join(name);
    serde_json::from_str(&std::fs::read_to_string(path).unwrap()).unwrap()
}

/// The manifest keys ADR-0026 removes (no per-wiki index files).
const OMITTED: [&str; 10] = [
    "faiss_cache_key",
    "graph_cache_key",
    "docstore_cache_key",
    "docstore_files",
    "bm25_cache_key",
    "bm25_files",
    "unified_db_key",
    "unified_db_files",
    "graph_files",
    "faiss_files",
];

fn jobs_left(scratch: &Path) -> usize {
    std::fs::read_dir(scratch.join("jobs")).map_or(0, Iterator::count)
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[allow(clippy::too_many_lines)]
async fn a_native_generation_publishes_the_wiki_it_returns() {
    let Some(pool) = storage_common::fresh_database("native_generate").await else {
        return;
    };
    let root = scratch("gen");
    let (served, commit) = served_repository(&root);
    let git_port = serve_git(served).await;
    let (gateway_port, gateway) = serve_gateway(false).await;
    let engine = Engine::start(&root, "native_generate");
    let health = engine.send("GET", "/engine/health", &Value::Null).await;
    let health = health.into_body().collect().await.unwrap().to_bytes();
    assert_eq!(
        serde_json::from_slice::<Value>(&health).unwrap(),
        json!({
            "status": "UP",
            "runner": "native",
            "active": 0,
            // The tools the engine serves, so the host can tell what it can do.
            "tools": elitea_deepwiki_engine::runner::ENGINE_TOOLS,
        })
    );

    let mut lines = engine
        .invoke("inv-native-1", &arguments(git_port, gateway_port))
        .await;
    let mut all = Vec::new();
    let deadline = Instant::now() + Duration::from_mins(5);
    while let Some(line) = tokio::time::timeout_at(deadline.into(), lines.recv())
        .await
        .expect("the run finished within 5 minutes")
    {
        all.push(line);
    }
    let thinking: Vec<&str> = all.iter().filter_map(|l| l["thinking"].as_str()).collect();
    let last = all.last().expect("a last line");
    let Some(result) = last.get("result").and_then(Value::as_object) else {
        panic!("the run failed: {last}\nprogress: {thinking:#?}");
    };
    // Exactly one last line, and no answer tokens.
    assert_eq!(
        all.iter()
            .filter(|l| l.get("result").is_some() || l.get("error").is_some())
            .count(),
        1
    );
    assert!(all.iter().all(|l| l.get("token").is_none()));
    for expected in [
        "DeepWiki worker started (pid ",
        "[worker] Clone config built: github - acme/notes-service @ main",
        "Repository cloned (branch: main, commit: ",
        "Writing unified DB: ",
        "Unified DB: ",
        "Phase 2 complete: ",
        "Phase 3 complete: ",
        "Wiki structure planned: ",
        "Publishing the index for query replicas",
        "Published ",
        "[worker] Done",
    ] {
        assert!(
            thinking.iter().any(|t| t.contains(expected)),
            "no progress line with {expected:?}: {thinking:#?}"
        );
    }

    // The result: the worker's composition, key for key.
    let keys: Vec<&str> = result.keys().map(String::as_str).collect();
    assert_eq!(
        keys,
        [
            "success",
            "result",
            "artifacts",
            "execution_time",
            "errors",
            "failed_pages",
            "repository_context",
            "commit_hash",
            "branch",
            "provider_type",
            "wiki_version_id",
            "wiki_id",
            "analysis_key",
            "canonical_repo_identifier",
            "wiki_title",
            "wiki_description",
        ]
    );
    let wiki_id = fixture("page_names.json")["wiki_id"]
        .as_str()
        .unwrap()
        .to_owned();
    assert_eq!(result["success"], true);
    assert_eq!(result["wiki_id"], wiki_id.as_str());
    assert_eq!(result["errors"], json!([]), "{:?}", result["errors"]);
    assert_eq!(result["failed_pages"], json!([]));
    assert_eq!(result["branch"], "main");
    assert_eq!(result["provider_type"], "github");
    assert_eq!(result["commit_hash"], commit.as_str());
    let identifier = format!("acme/notes-service:main:{}", &commit[..8]);
    assert_eq!(result["canonical_repo_identifier"], identifier.as_str());
    let version = result["wiki_version_id"].as_str().unwrap();
    assert_eq!(
        result["analysis_key"],
        format!("{identifier}@{version}").as_str()
    );
    // The stub answers the (unstructured) analysis prompt in prose, so the
    // description, made from a JSON analysis only, is empty, as in Python.
    assert!(result["wiki_description"].is_string());

    // The artifacts: every name under the wiki id; the manifest keeps the
    // frozen contract's keys minus the index files.
    let artifacts = result["artifacts"].as_array().unwrap();
    assert!(artifacts.iter().all(|a| {
        a["name"]
            .as_str()
            .unwrap()
            .starts_with(&format!("{wiki_id}/"))
    }));
    let pages: Vec<&Value> = artifacts
        .iter()
        .filter(|a| a["type"] == "text/markdown")
        .collect();
    assert!(pages.len() >= 2, "{} pages", pages.len());
    assert!(
        pages
            .iter()
            .any(|a| a["name"] == format!("{wiki_id}/wiki_pages/README.md").as_str())
    );
    assert!(pages.iter().all(|a| {
        a["name"]
            .as_str()
            .unwrap()
            .starts_with(&format!("{wiki_id}/wiki_pages/"))
    }));
    assert!(artifacts.iter().any(|a| {
        a["name"]
            .as_str()
            .unwrap()
            .starts_with(&format!("{wiki_id}/analysis/wiki_structure_"))
    }));
    let manifest = artifacts
        .iter()
        .find(|a| a["object_type"] == "wiki_manifest")
        .expect("a manifest");
    assert_eq!(
        manifest["name"],
        format!("{wiki_id}/wiki_manifest_{version}.json").as_str()
    );
    let manifest: Map<String, Value> =
        serde_json::from_str(manifest["data"].as_str().unwrap()).unwrap();
    let contract = fixture("wiki_manifest.json");
    let expected: Vec<&String> = contract["body"]
        .as_object()
        .unwrap()
        .keys()
        .filter(|k| !OMITTED.contains(&k.as_str()))
        .collect();
    assert_eq!(manifest.keys().collect::<Vec<_>>(), expected);
    assert_eq!(manifest["wiki_id"], wiki_id.as_str());
    assert_eq!(manifest["repository"], "acme/notes-service");
    let manifest_pages: Vec<&str> = manifest["pages"]
        .as_array()
        .unwrap()
        .iter()
        .map(|p| p.as_str().unwrap())
        .collect();
    let page_names: Vec<&str> = pages.iter().map(|a| a["name"].as_str().unwrap()).collect();
    assert_eq!(manifest_pages, page_names);

    // The published index: the registry row the result names, the rows,
    // both BM25 branches, the vectors; no build left behind.
    let record = WikiRecord::from_result(&Value::Object(result.clone()));
    let row = sqlx::query_as::<_, (String, String, String, String, String, Option<String>, Option<String>, Option<String>, Option<String>)>(
        "SELECT repo, branch, provider, host, folder_path, commit_hash, canonical_repo_identifier, analysis_key, wiki_version_id FROM wikis WHERE wiki_id = $1 AND project_id = 1",
    )
    .bind(&wiki_id)
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(
        row,
        (
            record.repo.unwrap(),
            record.branch.unwrap(),
            record.provider.unwrap(),
            record.host.unwrap(),
            record.folder_path.unwrap(),
            record.commit_hash,
            record.canonical_repo_identifier,
            record.analysis_key,
            record.wiki_version_id,
        )
    );
    let nodes = count(
        &pool,
        "SELECT count(*) FROM wiki_nodes WHERE wiki_id = 'acme--notes-service--main'",
    )
    .await;
    assert!(nodes > 10, "{nodes} nodes");
    let clustered = count(&pool, "SELECT count(*) FROM wiki_nodes WHERE wiki_id = 'acme--notes-service--main' AND macro_cluster IS NOT NULL").await;
    assert!(clustered > 0, "no node carries its Phase 3 section");
    let vectors = count(
        &pool,
        "SELECT count(*) FROM wiki_node_embeddings WHERE wiki_id = 'acme--notes-service--main'",
    )
    .await;
    assert!(vectors > 0 && vectors <= nodes, "{vectors} vectors");
    assert_eq!(
        usize::try_from(vectors).unwrap(),
        gateway.embedded.load(Ordering::SeqCst),
        "one stored vector per embedded text (no fallback query ran on a stored node)"
    );
    let edges = count(
        &pool,
        "SELECT count(*) FROM wiki_edges WHERE wiki_id = 'acme--notes-service--main'",
    )
    .await;
    assert!(edges > 0);
    // One statistics branch ('fts'); the dead 'bm25' branch is not written.
    assert_eq!(
        count(
            &pool,
            "SELECT count(*) FROM wiki_bm25_meta WHERE wiki_id = 'acme--notes-service--main'"
        )
        .await,
        1
    );
    assert_eq!(
        count(&pool, "SELECT count(*) FROM deepwiki_build.builds").await,
        0
    );
    assert_eq!(
        count(&pool, "SELECT count(*) FROM deepwiki_build.wiki_nodes").await,
        0
    );
    let reader = IndexReader::new(pool.clone(), storage_common::key(wiki_id.as_str()));
    let hits = reader.search_fts("NoteStore", 5).await.unwrap();
    assert!(
        hits.iter().any(|h| h.rel_path == "notes/store.py"),
        "{hits:?}"
    );
    let dense = reader
        .search_dense(&standin_embedding("def rank_notes"), 3)
        .await
        .unwrap();
    assert_eq!(dense.len(), 3);
    assert!(gateway.chats.load(Ordering::SeqCst) >= 3);
    assert_eq!(
        jobs_left(&engine.scratch),
        0,
        "the job's scratch is removed"
    );
    drop(engine);
    let _ = std::fs::remove_dir_all(&root);
}

/// An ARTIFACT-FOLDER source end to end: the worker child lists and
/// downloads `artifact://docs/handbook` from the platform's object API with
/// the invocation's callback bearer (the git allowlist does not apply; this
/// engine's names only 127.0.0.1 for git), and the wiki is named and keyed
/// as the Python engine names it.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[allow(clippy::too_many_lines)]
async fn a_native_generation_reads_an_artifact_folder() {
    use sha2::{Digest, Sha256};

    let Some(pool) = storage_common::fresh_database("native_artifact").await else {
        return;
    };
    let root = scratch("art");
    let (gateway_port, gateway) = serve_gateway(false).await;
    let engine = Engine::start_with(
        &root,
        "native_artifact",
        &[("ELITEA_DEEPWIKI_GIT_ALLOWLIST", "git.example.com")],
    );
    let mut arguments = arguments(1, gateway_port);
    arguments["repo_config"] = json!({
        "provider_type": "artifact",
        "provider_config": {"bucket": "docs", "prefix": "handbook"},
        "repository": "artifact://docs/handbook",
        "branch": "main",
        "project": null,
        "is_cloud": null,
    });
    let mut lines = engine.invoke("inv-native-artifact", &arguments).await;
    let mut all = Vec::new();
    let deadline = Instant::now() + Duration::from_mins(5);
    while let Some(line) = tokio::time::timeout_at(deadline.into(), lines.recv())
        .await
        .expect("the run finished within 5 minutes")
    {
        all.push(line);
    }
    let thinking: Vec<&str> = all.iter().filter_map(|l| l["thinking"].as_str()).collect();
    let last = all.last().expect("a last line");
    let Some(result) = last.get("result").and_then(Value::as_object) else {
        panic!("the run failed: {last}\nprogress: {thinking:#?}");
    };

    // The identity Python's listing_digest gives this listing.
    let mut listed: Vec<(String, usize)> = artifact_objects()
        .iter()
        .filter(|(key, _)| key.starts_with("handbook/"))
        .map(|(key, text)| (key.clone(), text.len()))
        .collect();
    listed.sort();
    let mut digest = Sha256::new();
    for (key, size) in &listed {
        digest.update(format!("{key}\0{size}\02026-10-01T10:00:00Z\n").as_bytes());
    }
    let digest: String = digest
        .finalize()
        .iter()
        .fold(String::new(), |mut hex, byte| {
            let _ = write!(hex, "{byte:02x}");
            hex
        });
    let wiki_id = "artifact--docs--handbook--main";
    let identifier = format!("artifact://docs/handbook:main:{}", &digest[..8]);
    assert_eq!(result["success"], true);
    assert_eq!(result["errors"], json!([]), "{:?}", result["errors"]);
    assert_eq!(result["wiki_id"], wiki_id);
    assert_eq!(result["provider_type"], "artifact");
    assert_eq!(result["branch"], "main");
    assert_eq!(result["commit_hash"], digest.as_str());
    assert_eq!(result["canonical_repo_identifier"], identifier.as_str());
    for expected in [
        "[worker] Clone config built: artifact - artifact://docs/handbook @ main",
        "Materialised artifact://docs/handbook: 6 objects, ",
        "Repository cloned (branch: main, commit: ",
        "[worker] Done",
    ] {
        assert!(
            thinking.iter().any(|t| t.contains(expected)),
            "no progress line with {expected:?}: {thinking:#?}"
        );
    }
    let artifacts = result["artifacts"].as_array().unwrap();
    assert!(artifacts.iter().all(|a| {
        a["name"]
            .as_str()
            .unwrap()
            .starts_with(&format!("{wiki_id}/"))
    }));
    let manifest = artifacts
        .iter()
        .find(|a| a["object_type"] == "wiki_manifest")
        .expect("a manifest");
    let manifest: Map<String, Value> =
        serde_json::from_str(manifest["data"].as_str().unwrap()).unwrap();
    assert_eq!(manifest["wiki_id"], wiki_id);
    assert_eq!(manifest["repository"], "artifact://docs/handbook");

    // The object API: one listing of the folder, one download per object,
    // nothing outside the folder, the bearer on every request.
    let seen = gateway.objects_seen.lock().unwrap().clone();
    assert_eq!(seen[0], format!("{OBJECTS_ROUTE}?prefix=handbook%2F"));
    assert_eq!(seen.len(), 1 + 6, "{seen:#?}");
    assert!(seen.iter().all(|s| !s.contains("archive")), "{seen:#?}");

    // The published index, under the wiki id Python would file it under.
    let row: (String, Option<String>) =
        sqlx::query_as("SELECT provider, canonical_repo_identifier FROM wikis WHERE wiki_id = $1")
            .bind(wiki_id)
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(row, ("artifact".to_owned(), Some(identifier.clone())));
    let reader = IndexReader::new(pool.clone(), storage_common::key(wiki_id));
    let hits = reader.search_fts("NoteStore", 5).await.unwrap();
    assert!(
        hits.iter().any(|h| h.rel_path == "notes/store.py"),
        "{hits:?}"
    );
    assert_eq!(
        jobs_left(&engine.scratch),
        0,
        "the job's scratch is removed"
    );
    drop(engine);
    let _ = std::fs::remove_dir_all(&root);
}

/// `kill -0 <pid>`: whether the process still exists.
fn alive(pid: &str) -> bool {
    Command::new("kill")
        .args(["-0", pid])
        .stderr(Stdio::null())
        .status()
        .is_ok_and(|s| s.success())
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_stop_kills_the_worker_and_leaves_no_staging_rows() {
    let Some(pool) = storage_common::fresh_database("native_stop").await else {
        return;
    };
    let root = scratch("stop");
    let (served, _commit) = served_repository(&root);
    let git_port = serve_git(served).await;
    // Embeddings never answer: the worker waits in the embedding phase,
    // after the graph was staged.
    let (gateway_port, gateway) = serve_gateway(true).await;
    let engine = Engine::start(&root, "native_stop");
    let mut lines = engine
        .invoke("inv-native-stop", &arguments(git_port, gateway_port))
        .await;

    let deadline = Instant::now() + Duration::from_mins(3);
    let mut pid = None;
    loop {
        let line = tokio::time::timeout_at(deadline.into(), lines.recv())
            .await
            .expect("the worker reached the embedding phase")
            .expect("the stream stayed open");
        if let Some(text) = line["thinking"].as_str()
            && let Some(rest) = text.strip_prefix("DeepWiki worker started (pid ")
        {
            pid = Some(rest.trim_end_matches(')').to_owned());
        }
        assert!(line.get("error").is_none(), "the run failed: {line}");
        if line["thinking"]
            .as_str()
            .is_some_and(|t| t.starts_with("Embedding the nodes with"))
        {
            break;
        }
    }
    while !gateway.embedding_requested.load(Ordering::SeqCst) {
        assert!(Instant::now() < deadline, "no embedding request came");
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    let pid = pid.expect("the worker's pid");
    assert!(alive(&pid));
    // The graph is staged while the worker waits for its vectors.
    let staged = count(&pool, "SELECT count(*) FROM deepwiki_build.wiki_nodes").await;
    assert!(staged > 0, "nothing staged before the stop");
    assert_eq!(
        count(&pool, "SELECT count(*) FROM deepwiki_build.builds").await,
        1
    );

    let started = Instant::now();
    let stopped = engine
        .post("/engine/invocations/inv-native-stop/stop", &json!({}))
        .await;
    assert_eq!(stopped.status().as_u16(), 202);
    let body = stopped.into_body().collect().await.unwrap().to_bytes();
    assert_eq!(
        serde_json::from_slice::<Value>(&body).unwrap(),
        json!({"stopped": true})
    );
    let mut last = None;
    while let Some(line) = tokio::time::timeout(Duration::from_secs(30), lines.recv())
        .await
        .expect("the stopped run ended")
    {
        last = Some(line);
    }
    assert!(
        started.elapsed() < Duration::from_secs(20),
        "{:?}",
        started.elapsed()
    );
    assert_eq!(
        last,
        Some(
            json!({"error": {"message": "Invocation cancelled", "error_type": "RuntimeError", "error_category": "runtime_error"}})
        )
    );
    assert!(!alive(&pid), "the worker {pid} outlived the stop");
    assert_eq!(
        count(&pool, "SELECT count(*) FROM deepwiki_build.builds").await,
        0
    );
    assert_eq!(
        count(&pool, "SELECT count(*) FROM deepwiki_build.wiki_nodes").await,
        0
    );
    assert_eq!(
        count(
            &pool,
            "SELECT count(*) FROM deepwiki_build.wiki_node_embeddings"
        )
        .await,
        0
    );
    assert_eq!(count(&pool, "SELECT count(*) FROM wikis").await, 0);
    assert_eq!(
        jobs_left(&engine.scratch),
        0,
        "the job's scratch is removed"
    );
    drop(engine);
    let _ = std::fs::remove_dir_all(&root);
}

/// Start a run whose publish waits behind `holder`'s lock on the wiki
/// (the advisory lock a publish of the same wiki takes), and stop it once
/// it publishes. Returns the engine, the stream and the worker's pid.
async fn stop_while_publishing(
    root: &Path,
    database: &str,
    extra: &[(&str, &str)],
    holder: &mut sqlx::PgConnection,
) -> (Engine, mpsc::UnboundedReceiver<Value>, String) {
    let wiki_id = fixture("page_names.json")["wiki_id"]
        .as_str()
        .unwrap()
        .to_owned();
    sqlx::query("SELECT pg_advisory_lock(hashtext($1), hashtext($2))")
        .bind(elitea_deepwiki_engine::storage::build::PUBLISH_WIKI_LOCK)
        .bind(
            elitea_deepwiki_engine::storage::build::publish_wiki_lock_object(&storage_common::key(
                &wiki_id,
            )),
        )
        .execute(&mut *holder)
        .await
        .unwrap();
    let (served, _commit) = served_repository(root);
    let git_port = serve_git(served).await;
    let (gateway_port, _gateway) = serve_gateway(false).await;
    let engine = Engine::start_with(root, database, extra);
    let id = format!("inv-{database}");
    let mut lines = engine.invoke(&id, &arguments(git_port, gateway_port)).await;
    let deadline = Instant::now() + Duration::from_mins(5);
    let mut pid = None;
    loop {
        let line = tokio::time::timeout_at(deadline.into(), lines.recv())
            .await
            .expect("the worker reached the publish")
            .expect("the stream stayed open");
        assert!(line.get("error").is_none(), "the run failed: {line}");
        assert!(
            line.get("publishing").is_none(),
            "a control line was relayed"
        );
        let text = line["thinking"].as_str().unwrap_or_default();
        if let Some(rest) = text.strip_prefix("DeepWiki worker started (pid ") {
            pid = Some(rest.trim_end_matches(')').to_owned());
        }
        if text == "Publishing the index for query replicas" {
            break;
        }
    }
    // The publish is now queued behind the held lock.
    tokio::time::sleep(Duration::from_millis(500)).await;
    let stopped = engine
        .post(&format!("/engine/invocations/{id}/stop"), &json!({}))
        .await;
    assert_eq!(stopped.status().as_u16(), 202);
    (engine, lines, pid.expect("the worker's pid"))
}

async fn last_line(lines: &mut mpsc::UnboundedReceiver<Value>, within: Duration) -> Option<Value> {
    let mut last = None;
    while let Some(line) = tokio::time::timeout(within, lines.recv())
        .await
        .expect("the stopped run ended")
    {
        last = Some(line);
    }
    last
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_stop_during_the_publish_waits_and_keeps_a_committed_result() {
    let Some(pool) = storage_common::fresh_database("native_stop_commit").await else {
        return;
    };
    let root = scratch("stopc");
    let mut holder = pool.acquire().await.unwrap();
    let (engine, mut lines, pid) =
        stop_while_publishing(&root, "native_stop_commit", &[], &mut holder).await;
    // Past the 3 s a stop otherwise gets: the worker is still publishing.
    tokio::time::sleep(KILL_AFTER + Duration::from_secs(2)).await;
    assert!(alive(&pid), "the worker was killed inside its publish");
    sqlx::query("SELECT pg_advisory_unlock_all()")
        .execute(&mut *holder)
        .await
        .unwrap();
    let last = last_line(&mut lines, Duration::from_mins(1)).await;
    let result = last
        .as_ref()
        .and_then(|l| l.get("result"))
        .unwrap_or_else(|| panic!("a committed publish reported {last:?}"));
    assert_eq!(result["success"], true);
    assert_eq!(result["errors"], json!([]));
    assert_eq!(count(&pool, "SELECT count(*) FROM wikis").await, 1);
    // Published into the host's project, not llm_settings.organization's.
    assert_eq!(
        count(&pool, "SELECT count(*) FROM wikis WHERE project_id = 1").await,
        1
    );
    assert!(count(&pool, "SELECT count(*) FROM wiki_nodes").await > 10);
    assert_eq!(
        count(&pool, "SELECT count(*) FROM deepwiki_build.builds").await,
        0
    );
    assert_eq!(jobs_left(&engine.scratch), 0);
    drop(engine);
    let _ = std::fs::remove_dir_all(&root);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_publish_that_never_commits_is_killed_and_cancelled() {
    let Some(pool) = storage_common::fresh_database("native_stop_hang").await else {
        return;
    };
    let root = scratch("stoph");
    let mut holder = pool.acquire().await.unwrap();
    // A 1 s statement timeout: the kill waits 1 s + the margin.
    let (engine, mut lines, pid) = stop_while_publishing(
        &root,
        "native_stop_hang",
        &[("ELITEA_DEEPWIKI_PUBLISH_STATEMENT_TIMEOUT_SECONDS", "1")],
        &mut holder,
    )
    .await;
    let started = Instant::now();
    let last = last_line(&mut lines, Duration::from_mins(2)).await;
    let waited = started.elapsed();
    assert!(waited >= PUBLISH_KILL_MARGIN, "killed after {waited:?}");
    assert!(
        waited < PUBLISH_KILL_MARGIN + Duration::from_secs(40),
        "{waited:?}"
    );
    assert_eq!(
        last,
        Some(
            json!({"error": {"message": "Invocation cancelled", "error_type": "RuntimeError", "error_category": "runtime_error"}})
        )
    );
    assert!(!alive(&pid), "the worker {pid} outlived the stop");
    // Nothing published; the killed worker's backend let go of the build,
    // so the parent deleted it while this lock is still held.
    assert_eq!(count(&pool, "SELECT count(*) FROM wikis").await, 0);
    assert_eq!(count(&pool, "SELECT count(*) FROM wiki_nodes").await, 0);
    assert_eq!(
        count(&pool, "SELECT count(*) FROM deepwiki_build.builds").await,
        0
    );
    assert_eq!(jobs_left(&engine.scratch), 0);
    sqlx::query("SELECT pg_advisory_unlock_all()")
        .execute(&mut *holder)
        .await
        .unwrap();
    drop(engine);
    let _ = std::fs::remove_dir_all(&root);
}
