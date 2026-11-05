//! The `elitea-browser-tools` conformance kit against a real
//! chrome-headless-shell (ADR-0049 verification 2).
//!
//! The fixture pages are served by a small HTTP server on loopback inside
//! the test; the driver runs the engine with no proxy so it can reach them.
//!
//! `ELITEA_BROWSER_ENGINE` names the engine binary. Without it the test
//! reports a skip and passes, unless `ELITEA_REQUIRE_BROWSER_CONFORMANCE=1`
//! (the CI job sets it): a skipped proof must not read as a passing one.
//! `ELITEA_BROWSER_ENGINE_LOG=1` passes the engine's stderr through.

use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use elitea_browser_session::control::Control;
use elitea_browser_session::driver::CdpDriver;
use elitea_browser_session::engine::EngineConfig;
use elitea_browser_session::session::under_gvisor;
use elitea_browser_tools::conformance::{self, Config, FIXTURES};
use elitea_browser_tools::{ToolOutput, dispatch, spec};
use serde_json::{Value, json};
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::net::TcpListener;

fn engine() -> Option<PathBuf> {
    if let Some(path) = std::env::var_os("ELITEA_BROWSER_ENGINE") {
        return Some(PathBuf::from(path));
    }
    assert!(
        std::env::var_os("ELITEA_REQUIRE_BROWSER_CONFORMANCE").is_none(),
        "ELITEA_REQUIRE_BROWSER_CONFORMANCE is set but ELITEA_BROWSER_ENGINE is not"
    );
    eprintln!("skipped: set ELITEA_BROWSER_ENGINE to a chrome-headless-shell binary");
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

async fn start(engine: PathBuf) -> CdpDriver {
    let config = EngineConfig {
        engine,
        engine_log: std::env::var_os("ELITEA_BROWSER_ENGINE_LOG").is_some(),
        // Under gVisor (the CI container job) the container is the boundary.
        gvisor: under_gvisor(),
        // The fixtures are on loopback: no proxy, and not offline.
        ..EngineConfig::default()
    };
    let control = Arc::new(Control::new(false, Duration::from_secs(1)));
    CdpDriver::start(&config, control).await.unwrap()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn conformance_kit_passes_on_chrome_headless_shell() {
    let Some(engine) = engine() else { return };
    let port = serve_fixtures().await;
    let driver = start(engine).await;
    let version = driver.engine_version().await.unwrap();
    eprintln!("engine: {version}");
    let url_for = move |fixture: &conformance::Fixture| {
        format!("http://127.0.0.1:{port}/{}.html", fixture.name)
    };
    let config = Config {
        url_for: &url_for,
        differences: Vec::new(),
    };
    let report = conformance::run(&driver, &config).await;
    eprintln!("{report}");
    assert!(
        report.failures().is_empty(),
        "{} conformance check(s) failed:\n{report}",
        report.failures().len()
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn hard_limits_and_the_view_stream() {
    let Some(engine) = engine() else { return };
    let port = serve_fixtures().await;
    let driver = start(engine).await;
    let forms = format!("http://127.0.0.1:{port}/forms.html");
    let opened = dispatch(&driver, spec::BROWSER_OPEN, &json!({ "url": forms })).await;
    assert_eq!(opened.result()["status"], "ok", "{}", opened.result());

    // A navigation the page starts to a refused scheme is stopped.
    let snapshot = dispatch(&driver, spec::BROWSER_SNAPSHOT, &json!({})).await;
    assert_eq!(snapshot.result()["status"], "ok");

    // No request_user without a person (an unattended run).
    let asked = dispatch(
        &driver,
        spec::BROWSER_REQUEST_USER,
        &json!({ "reason": "sign in" }),
    )
    .await;
    assert_eq!(asked.result()["code"], "no_person");

    // More than ten acts in a second are rate limited.
    let mut limited = false;
    for _ in 0..12 {
        let pressed = dispatch(&driver, spec::BROWSER_PRESS, &json!({ "key": "Tab" })).await;
        if pressed.result()["code"] == "rate_limited" {
            limited = true;
            break;
        }
    }
    assert!(limited, "twelve presses in a row were never rate limited");

    // The screencast: frames come, paced, at most the default size.
    let mut viewer = driver.screencast().subscribe(10).await.unwrap();
    let first = tokio::time::timeout(Duration::from_secs(10), viewer.next())
        .await
        .unwrap()
        .unwrap();
    assert!(first.jpeg.starts_with(&[0xff, 0xd8]), "not a JPEG");
    assert!(first.width <= 1280 && first.height <= 800);
    // Move the page so another frame is painted.
    let _ = dispatch(&driver, spec::BROWSER_RELOAD, &json!({})).await;
    let second = tokio::time::timeout(Duration::from_secs(10), viewer.next())
        .await
        .unwrap()
        .unwrap();
    assert!(second.sequence > first.sequence);
    drop(viewer);

    let shot = dispatch(&driver, spec::BROWSER_SCREENSHOT, &json!({})).await;
    match shot {
        ToolOutput::Image { data, result, .. } => {
            assert!(data.starts_with(b"\x89PNG"));
            assert_eq!(result["width"], 1280);
            assert_eq!(result["height"], 800);
        }
        ToolOutput::Json(result) => panic!("no image: {result}"),
    }
}

/// The agent binary, driven over its stdin and stdout.
struct Agent {
    child: tokio::process::Child,
    stdin: tokio::process::ChildStdin,
    lines: tokio::io::Lines<BufReader<tokio::process::ChildStdout>>,
}

impl Agent {
    fn spawn(engine: &PathBuf) -> Self {
        let binary = std::env::var_os("ELITEA_BROWSER_SESSION_BIN").map_or_else(
            || PathBuf::from(env!("CARGO_BIN_EXE_elitea-browser-session")),
            PathBuf::from,
        );
        let mut child = tokio::process::Command::new(binary)
            .arg("serve")
            .arg("--engine")
            .arg(engine)
            .stdin(std::process::Stdio::piped())
            .stdout(std::process::Stdio::piped())
            .kill_on_drop(true)
            .spawn()
            .unwrap();
        let stdin = child.stdin.take().unwrap();
        let lines = BufReader::new(child.stdout.take().unwrap()).lines();
        Self {
            child,
            stdin,
            lines,
        }
    }

    async fn send(&mut self, line: &Value) {
        let mut text = line.to_string();
        text.push('\n');
        self.stdin.write_all(text.as_bytes()).await.unwrap();
        self.stdin.flush().await.unwrap();
    }

    async fn next(&mut self) -> Value {
        let line = tokio::time::timeout(Duration::from_secs(90), self.lines.next_line())
            .await
            .unwrap()
            .unwrap()
            .unwrap();
        serde_json::from_str(&line).unwrap()
    }

    /// The next line that is not a frame or a notice.
    async fn answer(&mut self) -> Value {
        loop {
            let line = self.next().await;
            if line["op"] != "frame" && line["op"] != "notice" {
                return line;
            }
        }
    }

    async fn call(&mut self, id: &str, name: &str, args: Value) -> Value {
        self.send(&json!({"op": "call", "id": id, "name": name, "args": args}))
            .await;
        let answer = self.answer().await;
        assert_eq!(answer["op"], "result", "{answer}");
        assert_eq!(answer["id"], id, "{answer}");
        answer
    }
}

/// The agent binary over its supervisor line protocol: `start` with no
/// network, a refused page, the view stream with its ack, take over and
/// hand back, `destroy` and `bye`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_agent_binary_speaks_the_line_protocol() {
    let Some(engine) = engine() else { return };
    let port = serve_fixtures().await;
    let mut agent = Agent::spawn(&engine);
    agent
        .send(&json!({
            "op": "start", "revision": 1, "engine": "full",
            "engine_sandbox": !under_gvisor(), "max_pages": 3,
            "viewport": {"width": 1280, "height": 800}, "proxy": null,
        }))
        .await;
    let ready = agent.next().await;
    assert_eq!(ready["op"], "ready", "{ready}");
    let version = ready["engine_version"].as_str().unwrap();
    assert!(version.starts_with("chrome-headless-shell/"), "{version}");

    // `proxy: null` is no network at all: not even the loopback fixtures.
    let forms = format!("http://127.0.0.1:{port}/forms.html");
    let opened = agent
        .call("c1", "browser_open", json!({ "url": forms }))
        .await;
    assert_eq!(opened["result"]["code"], "navigation_failed", "{opened}");
    let shot = agent.call("c2", "browser_screenshot", json!({})).await;
    assert_eq!(shot["result"]["status"], "ok", "{shot}");
    assert_eq!(shot["image"]["media_type"], "image/png");

    // The view: a frame, then the ack that lets the next one through.
    agent
        .send(&json!({"op": "view", "on": true, "max_fps": 10}))
        .await;
    let frame = agent.next().await;
    assert_eq!(frame["op"], "frame", "{frame}");
    assert_eq!(frame["control"], "agent");
    let seq = frame["seq"].as_u64().unwrap();
    agent.send(&json!({"op": "ack", "seq": seq})).await;
    agent
        .send(&json!({"op": "view", "on": false, "max_fps": 10}))
        .await;

    // The person takes over: an act is user_in_control, a snapshot is not.
    agent
        .send(&json!({"op": "input", "event": {"type": "take_over"}}))
        .await;
    let pressed = agent
        .call("c3", "browser_press", json!({ "key": "Tab" }))
        .await;
    assert_eq!(pressed["result"]["code"], "user_in_control", "{pressed}");
    let snapshot = agent.call("c4", "browser_snapshot", json!({})).await;
    assert_eq!(snapshot["result"]["status"], "ok", "{snapshot}");
    agent
        .send(&json!({"op": "input", "event": {"type": "hand_back", "note": "done"}}))
        .await;
    let pressed = agent
        .call("c5", "browser_press", json!({ "key": "Tab" }))
        .await;
    assert_eq!(pressed["result"]["status"], "ok", "{pressed}");

    agent.send(&json!({"op": "destroy"})).await;
    assert_eq!(agent.answer().await, json!({"op": "bye"}));
    let status = tokio::time::timeout(Duration::from_secs(10), agent.child.wait())
        .await
        .unwrap()
        .unwrap();
    assert!(status.success());
}
