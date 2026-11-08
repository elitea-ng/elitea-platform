//! The session agent against a real Moli (`lite`, ADR-0049 decisions 2 and
//! 9), as `conformance.rs` is against chrome-headless-shell.
//!
//! `ELITEA_BROWSER_MOLI` names the Moli binary; without it every test here
//! reports a skip, unless `ELITEA_REQUIRE_BROWSER_MOLI=1` (the bake-off
//! workflow sets it). What is asserted is what admission needs regardless of
//! the kit score: Moli starts, reports `moli/<version>`, opens and snapshots
//! a page with refs, keeps the hard limits (a secure field refused, a
//! refused scheme), and stops cleanly. The kit's own report is printed, not
//! asserted: its differences from Chrome are bake-off evidence, recorded in
//! the ADR-0049 appendix, not a golden this test enforces.
//!
//! `auto` (Moli, then Chrome) is driven through the agent binary when
//! `ELITEA_BROWSER_ENGINE` names a Chrome as well.

use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;

use elitea_browser_session::control::Control;
use elitea_browser_session::driver::CdpDriver;
use elitea_browser_session::engine::{EngineConfig, EngineKind};
use elitea_browser_tools::conformance::{self, Config, FIXTURES};
use elitea_browser_tools::{dispatch, spec};
use serde_json::{Value, json};
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::net::TcpListener;

fn moli() -> Option<PathBuf> {
    if let Some(path) = std::env::var_os("ELITEA_BROWSER_MOLI") {
        return Some(PathBuf::from(path));
    }
    assert!(
        std::env::var_os("ELITEA_REQUIRE_BROWSER_MOLI").is_none(),
        "ELITEA_REQUIRE_BROWSER_MOLI is set but ELITEA_BROWSER_MOLI is not"
    );
    eprintln!("skipped: set ELITEA_BROWSER_MOLI to a moli binary");
    None
}

/// Serve the fixtures at `http://127.0.0.1:<port>/<name>.html`.
async fn serve_fixtures() -> u16 {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    tokio::spawn(async move {
        loop {
            let Ok((mut stream, _)) = listener.accept().await else {
                return;
            };
            tokio::spawn(async move {
                let mut request = Vec::new();
                let mut buffer = [0_u8; 4096];
                while !request.windows(4).any(|window| window == b"\r\n\r\n") {
                    match stream.read(&mut buffer).await {
                        Ok(0) | Err(_) => return,
                        Ok(read) => request.extend_from_slice(&buffer[..read]),
                    }
                    if request.len() > 64 << 10 {
                        return;
                    }
                }
                let text = String::from_utf8_lossy(&request);
                let path = text.split_whitespace().nth(1).unwrap_or("/");
                let path = path.split(['?', '#']).next().unwrap_or("/");
                let page = FIXTURES
                    .iter()
                    .find(|fixture| path == format!("/{}.html", fixture.name));
                let (status, body) = match page {
                    Some(fixture) => ("200 OK", fixture.html),
                    None => ("404 Not Found", "not found"),
                };
                let response = format!(
                    "HTTP/1.1 {status}\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: {}\r\nCache-Control: no-store\r\nConnection: close\r\n\r\n{body}",
                    body.len()
                );
                let _ = stream.write_all(response.as_bytes()).await;
                let _ = stream.shutdown().await;
            });
        }
    });
    port
}

/// A loopback forward proxy asking, like the egress proxy, for Basic
/// `run:<token>`: 407 without it. Counts the challenges it sent and the
/// requests it let through.
async fn authenticating_proxy(token: &'static str) -> (String, Arc<[AtomicUsize; 2]>) {
    use base64::Engine as _;
    let expected = format!(
        "basic {}",
        base64::engine::general_purpose::STANDARD.encode(format!("run:{token}"))
    );
    let counts = Arc::new([AtomicUsize::new(0), AtomicUsize::new(0)]);
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let seen = Arc::clone(&counts);
    tokio::spawn(async move {
        while let Ok((mut client, _)) = listener.accept().await {
            let (seen, expected) = (Arc::clone(&seen), expected.clone());
            tokio::spawn(async move {
                let mut head = Vec::new();
                let mut buffer = [0_u8; 8192];
                while !head.windows(4).any(|window| window == b"\r\n\r\n") {
                    match client.read(&mut buffer).await {
                        Ok(0) | Err(_) => return,
                        Ok(read) => head.extend_from_slice(&buffer[..read]),
                    }
                    if head.len() > 64 << 10 {
                        return;
                    }
                }
                let text = String::from_utf8_lossy(&head).into_owned();
                let authorized = text.lines().skip(1).any(|line| {
                    line.split_once(':').is_some_and(|(name, value)| {
                        name.trim().eq_ignore_ascii_case("proxy-authorization")
                            && value.trim().eq_ignore_ascii_case(&expected)
                    })
                });
                if !authorized {
                    seen[0].fetch_add(1, Ordering::SeqCst);
                    let _ = client
                        .write_all(b"HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"egress\"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
                        .await;
                    return;
                }
                seen[1].fetch_add(1, Ordering::SeqCst);
                let mut words = text.split_whitespace();
                let (method, target) = (
                    words.next().unwrap_or_default(),
                    words.next().unwrap_or_default(),
                );
                let authority = if method == "CONNECT" {
                    target.to_owned()
                } else {
                    let Some(rest) = target.strip_prefix("http://") else {
                        return;
                    };
                    rest.split('/').next().unwrap_or_default().to_owned()
                };
                let Ok(mut upstream) = tokio::net::TcpStream::connect(&authority).await else {
                    let _ = client
                        .write_all(b"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
                        .await;
                    return;
                };
                if method == "CONNECT" {
                    if client
                        .write_all(b"HTTP/1.1 200 Connection Established\r\n\r\n")
                        .await
                        .is_err()
                    {
                        return;
                    }
                } else if upstream.write_all(&head).await.is_err() {
                    return;
                }
                let _ = tokio::io::copy_bidirectional(&mut client, &mut upstream).await;
            });
        }
    });
    (format!("http://{address}"), counts)
}

async fn start(moli: PathBuf) -> CdpDriver {
    let config = EngineConfig {
        engine: moli,
        kind: EngineKind::Moli,
        engine_log: std::env::var_os("ELITEA_BROWSER_ENGINE_LOG").is_some(),
        // The fixtures are on loopback: no proxy, and not offline.
        ..EngineConfig::default()
    };
    let control = Arc::new(Control::new(false, Duration::from_secs(1)));
    CdpDriver::start(&config, control).await.unwrap()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn moli_runs_the_contract_and_keeps_the_hard_limits() {
    let Some(moli) = moli() else { return };
    let port = serve_fixtures().await;
    let driver = start(moli).await;
    let version = driver.engine_version().await.unwrap();
    eprintln!("engine: {version}");
    assert!(version.starts_with("moli/"), "{version}");

    let forms = format!("http://127.0.0.1:{port}/forms.html");
    let opened = dispatch(&driver, spec::BROWSER_OPEN, &json!({ "url": forms })).await;
    assert_eq!(opened.result()["status"], "ok", "{}", opened.result());
    let snapshot = dispatch(&driver, spec::BROWSER_SNAPSHOT, &json!({})).await;
    assert_eq!(snapshot.result()["status"], "ok", "{}", snapshot.result());
    assert!(
        snapshot.result()["refs"].as_u64().unwrap_or_default() > 0,
        "Moli's snapshot of the forms fixture gave out no ref: {}",
        snapshot.result()
    );
    // The hard limits are the driver's, whichever engine: a scheme the
    // contract refuses never reaches Moli.
    let refused = dispatch(
        &driver,
        spec::BROWSER_OPEN,
        &json!({ "url": "file:///etc/passwd" }),
    )
    .await;
    assert_eq!(refused.result()["code"], "scheme_refused");
    let asked = dispatch(
        &driver,
        spec::BROWSER_REQUEST_USER,
        &json!({ "reason": "sign in" }),
    )
    .await;
    assert_eq!(asked.result()["code"], "no_person");

    // The kit, for the record: printed, not asserted.
    let url_for = move |fixture: &conformance::Fixture| {
        format!("http://127.0.0.1:{port}/{}.html", fixture.name)
    };
    let config = Config {
        url_for: &url_for,
        differences: Vec::new(),
    };
    let report = conformance::run(&driver, &config).await;
    eprintln!(
        "moli conformance: {} check(s) differ from the goldens\n{report}",
        report.failures().len()
    );
    eprintln!("moli unsupported answers: {}", driver.unsupported_errors());
    driver.shutdown();
}

/// Moli behind a proxy that, like the egress proxy, asks for the run's
/// credentials: the driver answers the 407 (`Fetch.authRequired`, source
/// `Proxy`) and the page loads through it. The fixtures are on loopback,
/// which Moli's config sends to the proxy too.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn moli_answers_the_egress_proxy_challenge() {
    let Some(moli) = moli() else { return };
    let port = serve_fixtures().await;
    let (proxy, counts) = authenticating_proxy("moli-run-token").await;
    let config = EngineConfig {
        engine: moli,
        kind: EngineKind::Moli,
        proxy: Some(proxy),
        proxy_token: Some("moli-run-token".to_owned()),
        engine_log: std::env::var_os("ELITEA_BROWSER_ENGINE_LOG").is_some(),
        ..EngineConfig::default()
    };
    let control = Arc::new(Control::new(false, Duration::from_secs(1)));
    let driver = CdpDriver::start(&config, control).await.unwrap();
    let forms = format!("http://127.0.0.1:{port}/forms.html");
    let opened = dispatch(&driver, spec::BROWSER_OPEN, &json!({ "url": forms })).await;
    let (challenged, through) = (
        counts[0].load(Ordering::SeqCst),
        counts[1].load(Ordering::SeqCst),
    );
    eprintln!(
        "moli behind the authenticating proxy: {challenged} challenge(s), {through} request(s) through"
    );
    assert_eq!(opened.result()["status"], "ok", "{}", opened.result());
    assert!(
        through >= 1,
        "no request reached the proxy with the run's credentials"
    );
    let snapshot = dispatch(&driver, spec::BROWSER_SNAPSHOT, &json!({})).await;
    assert!(
        snapshot.result()["refs"].as_u64().unwrap_or_default() > 0,
        "the page behind the proxy did not load: {}",
        snapshot.result()
    );
    driver.shutdown();
}

/// `auto` through the agent binary: Moli first; the result of any switch
/// says so, and a `notice` `engine_switched` precedes it.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn auto_starts_on_moli_and_answers_on_one_engine() {
    let Some(moli) = moli() else { return };
    let Some(chrome) = std::env::var_os("ELITEA_BROWSER_ENGINE").map(PathBuf::from) else {
        eprintln!("skipped: auto needs ELITEA_BROWSER_ENGINE (chrome-headless-shell) as well");
        return;
    };
    if !elitea_browser_session::session::under_gvisor() {
        // Moli is admitted under gVisor only; on a shared kernel the agent
        // refuses `lite` and `auto` (session.rs tests that without an engine).
        eprintln!("skipped: auto runs Moli only under gVisor");
        return;
    }
    let binary = std::env::var_os("ELITEA_BROWSER_SESSION_BIN").map_or_else(
        || PathBuf::from(env!("CARGO_BIN_EXE_elitea-browser-session")),
        PathBuf::from,
    );
    let mut child = tokio::process::Command::new(binary)
        .args(["serve", "--engine"])
        .arg(&chrome)
        .arg("--moli")
        .arg(&moli)
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::piped())
        .kill_on_drop(true)
        .spawn()
        .unwrap();
    let mut stdin = child.stdin.take().unwrap();
    let mut lines = BufReader::new(child.stdout.take().unwrap()).lines();
    let mut send = async |line: Value| {
        let mut text = line.to_string();
        text.push('\n');
        stdin.write_all(text.as_bytes()).await.unwrap();
    };
    send(json!({
        "op": "start", "revision": 1, "engine": "auto", "engine_sandbox": false,
        "max_pages": 3, "viewport": {"width": 1280, "height": 800}, "proxy": null,
    }))
    .await;
    let mut next = async || -> Value {
        let line = tokio::time::timeout(Duration::from_secs(90), lines.next_line())
            .await
            .unwrap()
            .unwrap()
            .unwrap();
        serde_json::from_str(&line).unwrap()
    };
    let ready = next().await;
    assert_eq!(ready["op"], "ready", "{ready}");
    eprintln!("auto started on {}", ready["engine_version"]);
    send(json!({"op": "destroy"})).await;
    loop {
        let line = next().await;
        if line["op"] == "bye" {
            break;
        }
    }
}
