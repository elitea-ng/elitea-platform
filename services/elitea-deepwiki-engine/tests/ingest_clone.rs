//! The clone against a real smart-HTTP git server on loopback.
//!
//! The server is `git http-backend` (the CGI program every git HTTP
//! server runs) behind a small axum handler that also records each
//! request's `Authorization` header and can demand one. `git` is used
//! ONLY to build and serve the test repositories; the engine under test
//! never runs it.
//!
//! Set `ELITEA_DEEPWIKI_LIVE_CLONE=1` to also clone
//! `https://github.com/elitea-ng/elitea-platform` (about 45 MB).

use axum::body::{Body, to_bytes};
use axum::extract::{Request, State};
use axum::http::{HeaderValue, StatusCode};
use axum::response::Response;
use base64::Engine as _;
use elitea_deepwiki_engine::errors::{ErrorType, classify};
use elitea_deepwiki_engine::ingest::clone::{clone_repository, ls_remote};
use elitea_deepwiki_engine::ingest::egress::{AdmittedTarget, EgressPolicy};
use elitea_deepwiki_engine::ingest::limits::IngestLimits;
use elitea_deepwiki_engine::ingest::providers::{CloneTarget, ProviderType};
use elitea_deepwiki_engine::ingest::secret::Secret;
use elitea_deepwiki_engine::ingest::{IngestSettings, ingest, ingest_admitted};
use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::atomic::AtomicBool;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

const TOKEN: &str = "tok-SUPER-secret-4242";

fn scratch(name: &str) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("dw-ingest-{name}-{}", std::process::id()));
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

/// `git hash-object -t <kind> --literally -w --stdin`: writes objects git
/// itself would refuse to build (that is the point of the malicious ones).
fn write_object(dir: &Path, kind: &str, bytes: &[u8]) -> String {
    let mut child = Command::new("git")
        .args(["hash-object", "-t", kind, "--literally", "-w", "--stdin"])
        .current_dir(dir)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .spawn()
        .unwrap();
    child.stdin.take().unwrap().write_all(bytes).unwrap();
    let out = child.wait_with_output().unwrap();
    assert!(out.status.success());
    String::from_utf8_lossy(&out.stdout).trim().to_owned()
}

fn hex_to_bytes(hex: &str) -> Vec<u8> {
    (0..hex.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&hex[i..i + 2], 16).unwrap())
        .collect()
}

/// A raw tree object from `(mode, name, id)` entries, in the given order.
fn raw_tree(entries: &[(&str, &str, &str)]) -> Vec<u8> {
    let mut out = Vec::new();
    for (mode, name, id) in entries {
        out.extend_from_slice(format!("{mode} {name}\0").as_bytes());
        out.extend_from_slice(&hex_to_bytes(id));
    }
    out
}

/// The served root, with `o/r.git`: `main` (two commits), `dev` (three),
/// a tag, a symlink and a 300 kB incompressible file.
fn served_repository(root: &Path) -> (PathBuf, String, String) {
    let work = root.join("work");
    std::fs::create_dir_all(&work).unwrap();
    git(&work, &["init", "-q", "-b", "main"]);
    std::fs::create_dir_all(work.join("src")).unwrap();
    std::fs::write(work.join("README.md"), "# demo\n").unwrap();
    std::fs::write(work.join("src/app.py"), "def hello():\n    return 1\n").unwrap();
    git(&work, &["add", "."]);
    git(&work, &["commit", "-q", "-m", "one"]);
    std::fs::write(work.join("src/app.py"), "def hello():\n    return 2\n").unwrap();
    std::os::unix::fs::symlink("src/app.py", work.join("link.py")).unwrap();
    let mut noise = Vec::with_capacity(300_000);
    let mut state = 0x2545_f491_u32;
    for _ in 0..300_000 {
        state ^= state << 13;
        state ^= state >> 17;
        state ^= state << 5;
        noise.push(state.to_le_bytes()[0]);
    }
    std::fs::write(work.join("blob.bin"), noise).unwrap();
    git(&work, &["add", "."]);
    git(&work, &["commit", "-q", "-m", "two"]);
    git(&work, &["tag", "-a", "v1", "-m", "v1"]);
    git(&work, &["checkout", "-q", "-b", "dev"]);
    std::fs::write(work.join("src/dev.py"), "X = 1\n").unwrap();
    git(&work, &["add", "."]);
    git(&work, &["commit", "-q", "-m", "three"]);
    let dev = git(&work, &["rev-parse", "dev"]);
    let main = git(&work, &["rev-parse", "main"]);
    let served = root.join("served");
    std::fs::create_dir_all(served.join("o")).unwrap();
    git(
        root,
        &[
            "clone",
            "-q",
            "--bare",
            work.to_str().unwrap(),
            served.join("o/r.git").to_str().unwrap(),
        ],
    );
    git(
        &served.join("o/r.git"),
        &["config", "uploadpack.allowFilter", "true"],
    );
    (served, main, dev)
}

#[derive(Clone)]
struct Server {
    root: PathBuf,
    seen: Arc<Mutex<Vec<Option<String>>>>,
    required: Option<String>,
    delay: Duration,
    /// Stream large bodies slowly (16 kB every 20 ms), so a fetch is still
    /// running when the size watchdog looks.
    throttle: bool,
}

/// What the CGI call needs from a request, read before any await (the
/// request body is not `Sync`, so a borrow of it cannot cross one).
fn request_parts(request: &Request) -> [String; 8] {
    let header = |name: &str| {
        request
            .headers()
            .get(name)
            .and_then(|v| v.to_str().ok())
            .map(str::to_owned)
            .unwrap_or_default()
    };
    [
        header("authorization"),
        request.method().to_string(),
        request.uri().path().to_owned(),
        request.uri().query().unwrap_or_default().to_owned(),
        header("content-type"),
        header("git-protocol"),
        header("content-encoding"),
        String::new(),
    ]
}

#[allow(clippy::too_many_lines)]
async fn handle(State(server): State<Server>, request: Request) -> Response {
    let [
        authorization,
        method,
        path,
        query,
        content_type,
        protocol,
        encoding,
        _,
    ] = request_parts(&request);
    let authorization = (!authorization.is_empty()).then_some(authorization);
    server.seen.lock().unwrap().push(authorization.clone());
    if !server.delay.is_zero() {
        tokio::time::sleep(server.delay).await;
    }
    if let Some(required) = &server.required
        && authorization.as_deref() != Some(required.as_str())
    {
        let mut response = Response::new(Body::empty());
        *response.status_mut() = StatusCode::UNAUTHORIZED;
        response.headers_mut().insert(
            "www-authenticate",
            HeaderValue::from_static("Basic realm=\"git\""),
        );
        return response;
    }
    let body = to_bytes(request.into_body(), usize::MAX).await.unwrap();
    let root = server.root.clone();
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
    let payload = output[split.0 + split.1..].to_vec();
    let mut response = if server.throttle && payload.len() > 65_536 {
        let (sender, receiver) = tokio::sync::mpsc::channel::<Result<Vec<u8>, std::io::Error>>(4);
        tokio::spawn(async move {
            for chunk in payload.chunks(16_384) {
                if sender.send(Ok(chunk.to_vec())).await.is_err() {
                    return;
                }
                tokio::time::sleep(Duration::from_millis(20)).await;
            }
        });
        Response::new(Body::from_stream(
            tokio_stream::wrappers::ReceiverStream::new(receiver),
        ))
    } else {
        Response::new(Body::from(payload))
    };
    for line in head.lines() {
        let Some((name, value)) = line.split_once(':') else {
            continue;
        };
        let value = value.trim();
        if name.eq_ignore_ascii_case("status") {
            let code = value
                .split_whitespace()
                .next()
                .unwrap()
                .parse::<u16>()
                .unwrap();
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

struct Running {
    port: u16,
    seen: Arc<Mutex<Vec<Option<String>>>>,
}

async fn serve(root: &Path, required: Option<String>, delay: Duration) -> Running {
    serve_with(root, required, delay, false).await
}

async fn serve_with(
    root: &Path,
    required: Option<String>,
    delay: Duration,
    throttle: bool,
) -> Running {
    let seen = Arc::new(Mutex::new(Vec::new()));
    let state = Server {
        root: root.to_path_buf(),
        seen: Arc::clone(&seen),
        required,
        delay,
        throttle,
    };
    let app = axum::Router::new().fallback(handle).with_state(state);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    Running { port, seen }
}

fn basic(user: &str, password: &str) -> String {
    format!(
        "Basic {}",
        base64::engine::general_purpose::STANDARD.encode(format!("{user}:{password}"))
    )
}

fn admitted(port: u16, repo: &str, branch: &str, authorization: Option<String>) -> AdmittedTarget {
    let target = CloneTarget::new(
        ProviderType::GitHub,
        format!("http://127.0.0.1:{port}/{repo}.git"),
        repo.to_owned(),
        branch.to_owned(),
        authorization.and_then(Secret::new),
        "token",
    )
    .unwrap();
    EgressPolicy::parse(Some("127.0.0.1"))
        .admit(target)
        .unwrap()
}

fn limits() -> IngestLimits {
    IngestLimits {
        clone_timeout: Duration::from_mins(1),
        ..IngestLimits::default()
    }
}

/// Every byte of every file under `dir`, links not followed.
fn all_bytes(dir: &Path) -> Vec<(PathBuf, Vec<u8>)> {
    let mut out = Vec::new();
    let mut pending = vec![dir.to_path_buf()];
    while let Some(current) = pending.pop() {
        for entry in std::fs::read_dir(&current).unwrap().flatten() {
            let meta = entry.path().symlink_metadata().unwrap();
            if meta.is_dir() {
                pending.push(entry.path());
            } else if meta.is_file() {
                out.push((entry.path(), std::fs::read(entry.path()).unwrap()));
            }
        }
    }
    out
}

fn contains(haystack: &[u8], needle: &[u8]) -> bool {
    haystack.windows(needle.len()).any(|w| w == needle)
}

#[tokio::test(flavor = "multi_thread")]
async fn a_shallow_clone_of_the_branch_head_with_its_identity() {
    let root = scratch("shallow");
    let (served, _main, dev) = served_repository(&root);
    let http = serve(&served, None, Duration::ZERO).await;
    let job = root.join("job");

    let head = ls_remote(&admitted(http.port, "o/r", "dev", None), &job).unwrap();
    assert_eq!(
        head.map(|h| (h.reference, h.commit)),
        Some(("refs/heads/dev".to_owned(), dev.clone()))
    );

    let cloned = ingest_admitted(
        admitted(http.port, "o/r", "dev", None),
        limits(),
        &job,
        Arc::new(AtomicBool::new(false)),
    )
    .await
    .unwrap();
    assert_eq!(cloned.identity.commit(), dev);
    assert_eq!(
        cloned.identity.to_string(),
        format!("o/r:dev:{}", &dev[..8])
    );
    assert_eq!(cloned.path, job.join(format!("o_r_dev_{}", &dev[..8])));
    // Depth 1: one commit, and git records the clone as shallow.
    assert_eq!(git(&cloned.path, &["rev-list", "--count", "HEAD"]), "1");
    assert_eq!(
        std::fs::read_to_string(cloned.path.join(".git/shallow"))
            .unwrap()
            .trim(),
        dev
    );
    assert_eq!(
        git(&cloned.path, &["rev-parse", "--abbrev-ref", "HEAD"]),
        "dev"
    );
    // Single branch, no tags.
    let refs = git(&cloned.path, &["for-each-ref", "--format=%(refname)"]);
    assert!(
        !refs.contains("refs/tags/") && !refs.contains("origin/main"),
        "{refs}"
    );
    assert_eq!(
        std::fs::read_to_string(cloned.path.join("src/dev.py")).unwrap(),
        "X = 1\n"
    );
    // The symlink is checked out AS a link (later stages skip it).
    assert!(
        cloned
            .path
            .join("link.py")
            .symlink_metadata()
            .unwrap()
            .file_type()
            .is_symlink()
    );
    assert_eq!(cloned.tree.files, 5);
    assert_eq!(cloned.tree.symlinks, 1);
    // The ls-remote probe repository is gone.
    assert!(!job.join(".ls-remote").exists());
    // A tag of that name is accepted, as `git clone --branch` does.
    let tag = ls_remote(&admitted(http.port, "o/r", "v1", None), &job).unwrap();
    assert_eq!(tag.map(|h| h.reference).as_deref(), Some("refs/tags/v1"));
    let _ = std::fs::remove_dir_all(&root);
}

#[tokio::test(flavor = "multi_thread")]
async fn the_credential_is_a_header_and_never_reaches_the_disk() {
    let root = scratch("credential");
    let (served, main, _dev) = served_repository(&root);
    let expected = basic(TOKEN, "");
    let http = serve(&served, Some(expected.clone()), Duration::ZERO).await;
    let job = root.join("job");

    let cloned = ingest_admitted(
        admitted(http.port, "o/r", "main", Some(expected.clone())),
        limits(),
        &job,
        Arc::new(AtomicBool::new(false)),
    )
    .await
    .unwrap();
    assert_eq!(cloned.identity.commit(), main);
    // Every request carried the header (it is sent up front, not after a
    // 401 challenge).
    let seen = http.seen.lock().unwrap().clone();
    assert!(seen.len() >= 3, "{seen:?}");
    assert!(
        seen.iter().all(|h| h.as_deref() == Some(expected.as_str())),
        "{seen:?}"
    );
    // Nothing on disk holds the token or the header value.
    let encoded = expected.trim_start_matches("Basic ");
    for (path, bytes) in all_bytes(&cloned.path) {
        assert!(
            !contains(&bytes, TOKEN.as_bytes()),
            "{} holds the token",
            path.display()
        );
        assert!(
            !contains(&bytes, encoded.as_bytes()),
            "{} holds the header",
            path.display()
        );
    }
    let config = std::fs::read_to_string(cloned.path.join(".git/config")).unwrap();
    assert!(
        config.contains(&format!("url = http://127.0.0.1:{}/o/r.git", http.port)),
        "{config}"
    );
    assert!(!config.contains('@'), "{config}");

    // A wrong credential: the classified error, without the credential.
    let wrong = basic("tok-WRONG-0000", "");
    let error = ingest_admitted(
        admitted(http.port, "o/r", "main", Some(wrong.clone())),
        limits(),
        &root.join("job2"),
        Arc::new(AtomicBool::new(false)),
    )
    .await
    .unwrap_err();
    assert!(
        error
            .message
            .starts_with("Authentication failed for repository 'o/r'"),
        "{error}"
    );
    assert!(
        !error.message.contains("tok-WRONG")
            && !error.message.contains(wrong.trim_start_matches("Basic "))
    );
    let _ = std::fs::remove_dir_all(&root);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_missing_branch_or_repository_is_resource_not_found() {
    let root = scratch("missing");
    let (served, _main, _dev) = served_repository(&root);
    let http = serve(&served, None, Duration::ZERO).await;
    let job = root.join("job");
    let flag = || Arc::new(AtomicBool::new(false));

    let error = ingest_admitted(
        admitted(http.port, "o/r", "nope", None),
        limits(),
        &job,
        flag(),
    )
    .await
    .unwrap_err();
    assert_eq!(
        error.message,
        "Branch 'nope' not found in repository 'o/r'. Please verify the branch name exists."
    );
    assert_eq!(error.error_type, ErrorType::Runtime);
    assert_eq!(
        classify(error.error_type, &error.message),
        "resource_not_found"
    );
    // Nothing was cloned.
    assert_eq!(std::fs::read_dir(&job).unwrap().count(), 0);

    let error = ingest_admitted(
        admitted(http.port, "o/gone", "main", None),
        limits(),
        &job,
        flag(),
    )
    .await
    .unwrap_err();
    assert!(
        error.message.starts_with("Repository not found: 'o/gone'"),
        "{error}"
    );
    assert_eq!(
        classify(error.error_type, &error.message),
        "resource_not_found"
    );
    let _ = std::fs::remove_dir_all(&root);
}

#[tokio::test(flavor = "multi_thread")]
async fn limits_stop_the_clone_before_the_working_tree() {
    let root = scratch("limits");
    let (served, _main, _dev) = served_repository(&root);
    let http = serve(&served, None, Duration::ZERO).await;
    let flag = || Arc::new(AtomicBool::new(false));
    let cases = [
        (
            IngestLimits {
                max_clone_bytes: 50_000,
                ..limits()
            },
            "MAX_CLONE_BYTES",
        ),
        (
            IngestLimits {
                max_file_count: 2,
                ..limits()
            },
            "MAX_FILE_COUNT",
        ),
        (
            IngestLimits {
                max_file_bytes: 100_000,
                ..limits()
            },
            "MAX_FILE_BYTES",
        ),
        (
            IngestLimits {
                max_parsed_bytes: 10,
                ..limits()
            },
            "MAX_PARSED_BYTES",
        ),
    ];
    for (index, (limits, setting)) in cases.into_iter().enumerate() {
        let job = root.join(format!("job{index}"));
        let error = ingest_admitted(
            admitted(http.port, "o/r", "main", None),
            limits,
            &job,
            flag(),
        )
        .await
        .unwrap_err();
        eprintln!("limit {setting}: {error}");
        assert!(error.message.contains(setting), "{setting}: {error}");
        assert_eq!(
            classify(error.error_type, &error.message),
            "invalid_input",
            "{error}"
        );
        // The partial clone is gone: no working tree was left behind.
        let left: Vec<_> = std::fs::read_dir(&job)
            .unwrap()
            .flatten()
            .map(|e| e.file_name())
            .collect();
        assert!(left.is_empty(), "{setting}: {left:?}");
    }
    let _ = std::fs::remove_dir_all(&root);
}

/// A repository with one 4 MB incompressible file.
fn big_repository(root: &Path) -> PathBuf {
    let work = root.join("work");
    std::fs::create_dir_all(&work).unwrap();
    git(&work, &["init", "-q", "-b", "main"]);
    let mut noise = Vec::with_capacity(4_000_000);
    let mut state = 0x9e37_79b9_u32;
    for _ in 0..4_000_000 {
        state ^= state << 13;
        state ^= state >> 17;
        state ^= state << 5;
        noise.push(state.to_le_bytes()[1]);
    }
    std::fs::write(work.join("big.bin"), noise).unwrap();
    git(&work, &["add", "."]);
    git(&work, &["commit", "-q", "-m", "big"]);
    let served = root.join("served");
    std::fs::create_dir_all(served.join("o")).unwrap();
    git(
        root,
        &[
            "clone",
            "-q",
            "--bare",
            work.to_str().unwrap(),
            served.join("o/big.git").to_str().unwrap(),
        ],
    );
    served
}

#[tokio::test(flavor = "multi_thread")]
async fn the_size_watchdog_interrupts_a_running_fetch() {
    let root = scratch("watchdog");
    let served = big_repository(&root);
    let http = serve_with(&served, None, Duration::ZERO, true).await;
    let job = root.join("job");
    let limits = IngestLimits {
        max_clone_bytes: 1_000_000,
        ..limits()
    };
    let started = Instant::now();
    let error = ingest_admitted(
        admitted(http.port, "o/big", "main", None),
        limits,
        &job,
        Arc::new(AtomicBool::new(false)),
    )
    .await
    .unwrap_err();
    let elapsed = started.elapsed();
    eprintln!("watchdog: {error} after {elapsed:?}");
    // The whole pack takes about 5 s to arrive; the fetch stopped early.
    assert!(elapsed < Duration::from_secs(4), "{elapsed:?}");
    assert!(
        error.message.contains("MAX_CLONE_BYTES") && error.message.ends_with("(while fetching)"),
        "{error}"
    );
    let left: Vec<_> = std::fs::read_dir(&job)
        .unwrap()
        .flatten()
        .map(|e| e.file_name())
        .collect();
    assert!(left.is_empty(), "{left:?}");
    let _ = std::fs::remove_dir_all(&root);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_slow_remote_times_out() {
    let root = scratch("timeout");
    let (served, _main, _dev) = served_repository(&root);
    let http = serve(&served, None, Duration::from_secs(4)).await;
    let limits = IngestLimits {
        clone_timeout: Duration::from_millis(800),
        ..limits()
    };
    let started = Instant::now();
    let error = ingest_admitted(
        admitted(http.port, "o/r", "main", None),
        limits,
        &root.join("job"),
        Arc::new(AtomicBool::new(false)),
    )
    .await
    .unwrap_err();
    assert!(
        started.elapsed() < Duration::from_secs(3),
        "{:?}",
        started.elapsed()
    );
    assert!(error.message.starts_with("Clone timeout: 'o/r'"), "{error}");
    assert_eq!(classify(error.error_type, &error.message), "timeout_error");
    let _ = std::fs::remove_dir_all(&root);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_cancelled_clone_reports_the_stop_line() {
    let root = scratch("cancel");
    let (served, _main, _dev) = served_repository(&root);
    let http = serve(&served, None, Duration::ZERO).await;
    let error = tokio::task::spawn_blocking(move || {
        clone_repository(
            &admitted(http.port, "o/r", "main", None),
            &root.join("job"),
            &limits(),
            &AtomicBool::new(true),
        )
    })
    .await
    .unwrap()
    .unwrap_err();
    assert_eq!(error.message, "Invocation cancelled");
}

/// A repository whose `main` commit has the tree `entries` (raw, so it
/// can hold what `git` refuses to create).
fn malicious_repository(root: &Path, build: impl FnOnce(&Path) -> Vec<u8>) -> PathBuf {
    let served = root.join("served");
    let bare = served.join("o/evil.git");
    std::fs::create_dir_all(&bare).unwrap();
    git(&bare, &["init", "-q", "--bare", "-b", "main"]);
    let tree = write_object(&bare, "tree", &build(&bare));
    let commit = write_object(
        &bare,
        "commit",
        format!("tree {tree}\nauthor t <t@e> 0 +0000\ncommitter t <t@e> 0 +0000\n\nevil\n")
            .as_bytes(),
    );
    git(&bare, &["update-ref", "refs/heads/main", &commit]);
    served
}

#[tokio::test(flavor = "multi_thread")]
async fn a_dotdot_path_never_leaves_the_clone() {
    let root = scratch("dotdot");
    let served = malicious_repository(&root, |bare| {
        let blob = write_object(bare, "blob", b"pwned\n");
        let inner = write_object(bare, "tree", &raw_tree(&[("100644", "escaped.txt", &blob)]));
        raw_tree(&[("40000", "..", &inner), ("100644", "ok.txt", &blob)])
    });
    let http = serve(&served, None, Duration::ZERO).await;
    let job = root.join("deep/job");
    let result = ingest_admitted(
        admitted(http.port, "o/evil", "main", None),
        limits(),
        &job,
        Arc::new(AtomicBool::new(false)),
    )
    .await;
    let error = result.unwrap_err();
    eprintln!("dotdot: {error}");
    assert_eq!(error.error_type, ErrorType::Runtime, "{error}");
    for place in [
        root.join("deep/escaped.txt"),
        root.join("escaped.txt"),
        job.join("escaped.txt"),
    ] {
        assert!(!place.exists(), "{} was written", place.display());
    }
    let _ = std::fs::remove_dir_all(&root);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_symlinked_directory_is_never_written_through() {
    let root = scratch("symlink");
    let outside = root.join("outside");
    std::fs::create_dir_all(&outside).unwrap();
    let target = outside.to_str().unwrap().to_owned();
    // `link` is a symbolic link to a directory outside the clone, and a
    // second `link` entry is a directory holding `pwned.txt`; on a
    // case-insensitive file system `Link` would do the same.
    let served = malicious_repository(&root, |bare| {
        let link = write_object(bare, "blob", target.as_bytes());
        let blob = write_object(bare, "blob", b"pwned\n");
        let inner = write_object(bare, "tree", &raw_tree(&[("100644", "pwned.txt", &blob)]));
        raw_tree(&[
            ("40000", "Link", &inner),
            ("120000", "link", &link),
            ("40000", "link", &inner),
        ])
    });
    let http = serve(&served, None, Duration::ZERO).await;
    let job = root.join("job");
    let result = ingest_admitted(
        admitted(http.port, "o/evil", "main", None),
        limits(),
        &job,
        Arc::new(AtomicBool::new(false)),
    )
    .await;
    eprintln!("symlink: {result:?}");
    assert_eq!(
        std::fs::read_dir(&outside).unwrap().count(),
        0,
        "wrote through the link: {result:?}"
    );
    if let Ok(cloned) = result {
        // If the checkout succeeded, the link is a link and nothing below
        // it was written.
        let link = cloned.path.join("link");
        assert!(
            link.symlink_metadata()
                .map_or(true, |m| m.file_type().is_symlink() || m.is_dir())
        );
    }
    let _ = std::fs::remove_dir_all(&root);
}

/// A tree bomb: every directory lists the same subtree 16 times, six
/// levels deep, so 7 small objects describe 16.7 million files. The tree
/// was walked into memory whole, outside the deadline, before the file
/// count was checked.
#[tokio::test(flavor = "multi_thread")]
async fn a_tree_bomb_stops_at_the_file_count() {
    let root = scratch("treebomb");
    let served = malicious_repository(&root, |bare| {
        let blob = write_object(bare, "blob", b"x\n");
        let names: Vec<String> = (0..16).map(|i| format!("e{i:02}")).collect();
        let level = |id: &str, mode: &str| {
            let entries: Vec<(&str, &str, &str)> =
                names.iter().map(|n| (mode, n.as_str(), id)).collect();
            raw_tree(&entries)
        };
        let mut id = write_object(bare, "tree", &level(&blob, "100644"));
        for _ in 0..4 {
            id = write_object(bare, "tree", &level(&id, "40000"));
        }
        level(&id, "40000")
    });
    let http = serve(&served, None, Duration::ZERO).await;
    let job = root.join("job");
    let started = Instant::now();
    let error = ingest_admitted(
        admitted(http.port, "o/evil", "main", None),
        limits(),
        &job,
        Arc::new(AtomicBool::new(false)),
    )
    .await
    .unwrap_err();
    let elapsed = started.elapsed();
    eprintln!("tree bomb: {error} after {elapsed:?}");
    assert!(error.message.contains("MAX_FILE_COUNT"), "{error}");
    assert!(elapsed < Duration::from_secs(5), "{elapsed:?}");
    let _ = std::fs::remove_dir_all(&root);
}

/// An anonymous clone whose remote redirects to a host off the allowlist
/// is refused: the redirect is not followed, so nothing reaches that host.
#[tokio::test(flavor = "multi_thread")]
async fn a_redirect_off_the_allowlist_is_not_followed() {
    let root = scratch("redirect");
    let (served, _main, _dev) = served_repository(&root);
    let http = serve(&served, None, Duration::ZERO).await;
    // The redirector: every request goes to `localhost`, which the
    // allowlist (`127.0.0.1`) does not name.
    let port = http.port;
    let redirector = axum::Router::new().fallback(move |request: Request| async move {
        let tail = request
            .uri()
            .path_and_query()
            .map(|p| p.as_str().to_owned())
            .unwrap_or_default();
        let mut response = Response::new(Body::empty());
        *response.status_mut() = StatusCode::FOUND;
        response.headers_mut().insert(
            "location",
            HeaderValue::from_str(&format!("http://localhost:{port}{tail}")).unwrap(),
        );
        response
    });
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let redirect_port = listener.local_addr().unwrap().port();
    tokio::spawn(async move { axum::serve(listener, redirector).await.unwrap() });

    let result = ingest_admitted(
        admitted(redirect_port, "o/r", "main", None),
        limits(),
        &root.join("job"),
        Arc::new(AtomicBool::new(false)),
    )
    .await;
    let reached = http.seen.lock().unwrap().len();
    eprintln!("redirect: {result:?}, {reached} request(s) reached the redirect target");
    assert!(result.is_err(), "the clone followed the redirect");
    assert_eq!(reached, 0);
    let _ = std::fs::remove_dir_all(&root);
}

/// One 512 MiB blob of zeros is half a megabyte in the pack. Resolving the
/// pack decoded it whole in memory before any limit looked at it; now an
/// object larger than `MAX_FILE_BYTES` is refused while fetching.
#[tokio::test(flavor = "multi_thread")]
async fn a_decompression_bomb_is_refused_while_fetching() {
    let root = scratch("zipbomb");
    let served = malicious_repository(&root, |bare| {
        let mut child = Command::new("git")
            .args(["hash-object", "-t", "blob", "-w", "--stdin"])
            .current_dir(bare)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .spawn()
            .unwrap();
        let mut stdin = child.stdin.take().unwrap();
        let zeros = vec![0u8; 1 << 20];
        for _ in 0..512 {
            stdin.write_all(&zeros).unwrap();
        }
        drop(stdin);
        let out = child.wait_with_output().unwrap();
        assert!(out.status.success());
        let blob = String::from_utf8_lossy(&out.stdout).trim().to_owned();
        raw_tree(&[("100644", "zeros.bin", &blob)])
    });
    let http = serve(&served, None, Duration::ZERO).await;
    let error = ingest_admitted(
        admitted(http.port, "o/evil", "main", None),
        limits(),
        &root.join("job"),
        Arc::new(AtomicBool::new(false)),
    )
    .await
    .unwrap_err();
    eprintln!("decompression bomb: {error}");
    assert!(
        error.message.contains("MAX_FILE_BYTES") && error.message.ends_with("(while fetching)"),
        "{error}"
    );
    assert_eq!(classify(error.error_type, &error.message), "invalid_input");
    let _ = std::fs::remove_dir_all(&root);
}

#[tokio::test(flavor = "multi_thread")]
async fn the_settings_path_refuses_a_host_off_the_allowlist() {
    let settings = IngestSettings {
        git_allowlist: EgressPolicy::parse(Some("gitlab.com")),
        limits: limits(),
        scratch_path: PathBuf::from("/nonexistent"),
    };
    let config = serde_json::json!({"provider_type": "github", "provider_config": {"access_token": TOKEN}, "repository": "o/r"});
    let error = ingest(
        &config,
        &settings,
        Path::new("/nonexistent/job"),
        Arc::new(AtomicBool::new(false)),
    )
    .await
    .unwrap_err();
    assert!(
        error
            .message
            .contains("'github.com' is not on the git-host allowlist"),
        "{error}"
    );
    assert!(!error.message.contains(TOKEN));
}

#[tokio::test(flavor = "multi_thread")]
async fn live_clone_of_the_platform_repository() {
    if std::env::var("ELITEA_DEEPWIKI_LIVE_CLONE").as_deref() != Ok("1") {
        return;
    }
    let job = std::env::var("ELITEA_DEEPWIKI_LIVE_CLONE_DIR")
        .map_or_else(|_| scratch("live"), PathBuf::from);
    let settings = IngestSettings {
        git_allowlist: EgressPolicy::parse(Some("github.com,*.github.com")),
        limits: IngestLimits {
            clone_timeout: Duration::from_mins(5),
            ..IngestLimits::default()
        },
        scratch_path: job.clone(),
    };
    let config = serde_json::json!({"provider_type": "github", "provider_config": {"base_url": "https://api.github.com"}, "repository": "elitea-ng/elitea-platform", "branch": "main"});
    let started = Instant::now();
    let cloned = ingest(&config, &settings, &job, Arc::new(AtomicBool::new(false)))
        .await
        .unwrap();
    eprintln!(
        "live clone: {} in {:?}, {:?}",
        cloned.identity,
        started.elapsed(),
        cloned.tree
    );
    assert_eq!(git(&cloned.path, &["rev-list", "--count", "HEAD"]), "1");
    assert!(cloned.path.join("go.work").is_file());
}
