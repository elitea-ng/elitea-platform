//! The Windows driver against a real `WebView2`: the conformance kit of
//! `elitea-browser-tools` on its fixture pages, driven by the cloud
//! browser's `CdpDriver` over this crate's bridge (the same path as the
//! desktop pane), plus the rules' reads and a page that takes real input
//! only (`WebView2`'s `Input.*` is trusted input).
//!
//! `WebView2` is UI-thread only and needs a window, so this test owns `main`
//! (`harness = false`) and is skipped unless asked for:
//!
//! ```text
//! cd apps/elitea-desktop/src-tauri
//! cargo test -p elitea-browser-driver-windows --test webview2 -- --ignored
//! ```
//!
//! The fixtures are served over loopback `http` by a server in this
//! process. The window is a plain tao window with a wry webview (Tauri
//! builds its panes the same way), with a user data folder of its own.

#[cfg(not(windows))]
fn main() {
    println!("the WebView2 tests run on Windows only");
}

#[cfg(windows)]
fn main() {
    if !std::env::args().any(|arg| arg == "--ignored" || arg == "--include-ignored") {
        println!("webview2: 3 tests ignored (they open a window): run with `-- --ignored`");
        return;
    }
    windows_run::run();
}

#[cfg(windows)]
mod windows_run {
    use std::io::{BufRead as _, BufReader, Write as _};
    use std::net::TcpListener;
    use std::sync::Arc;
    use std::time::Duration;

    use elitea_browser_driver_windows::{OnUiThread, PAGE_SESSION, open_stream};
    use elitea_browser_session::cdp::Connection;
    use elitea_browser_session::control::Control;
    use elitea_browser_session::driver::CdpDriver;
    use elitea_browser_session::engine::EngineConfig;
    use elitea_browser_tools::conformance::{self, Config, FIXTURES, Fixture};
    use elitea_browser_tools::driver::BrowserDriver as _;
    use elitea_browser_tools::{ElementRef, dispatch, spec};
    use serde_json::{Value, json};
    use tao::event::Event;
    use tao::event_loop::{ControlFlow, EventLoopBuilder};
    use tao::window::WindowBuilder;
    use wry::{WebContext, WebViewBuilder, WebViewExtWindows as _};

    const EXTRA: &[(&str, &str)] = &[
        (
            "untrusted",
            include_str!("../../browser-driver-macos/tests/fixtures/untrusted.html"),
        ),
        (
            "controls",
            include_str!("../../browser-driver-macos/tests/fixtures/controls.html"),
        ),
    ];

    /// Work for the UI thread, sent through the event loop.
    type Work = Box<dyn FnOnce() + Send>;

    enum UserEvent {
        Work(Work),
        Done(bool),
    }

    /// The fixtures over loopback http, one request per connection.
    fn serve() -> u16 {
        let listener = TcpListener::bind("127.0.0.1:0").expect("bind");
        let port = listener.local_addr().expect("addr").port();
        std::thread::spawn(move || {
            for stream in listener.incoming() {
                let Ok(stream) = stream else { continue };
                std::thread::spawn(move || answer(stream));
            }
        });
        port
    }

    fn answer(mut stream: std::net::TcpStream) {
        let mut reader = BufReader::new(&stream);
        let mut line = String::new();
        if reader.read_line(&mut line).is_err() {
            return;
        }
        // Read the rest of the head so the close is clean.
        let mut header = String::new();
        while reader.read_line(&mut header).is_ok_and(|read| read > 2) {
            header.clear();
        }
        let path = line.split_whitespace().nth(1).unwrap_or("/");
        let name = path
            .trim_start_matches('/')
            .split(['?', '#'])
            .next()
            .unwrap_or_default()
            .trim_end_matches(".html");
        let body = FIXTURES
            .iter()
            .find(|fixture| fixture.name == name)
            .map(|fixture| fixture.html)
            .or_else(|| {
                EXTRA
                    .iter()
                    .find(|(extra, _)| *extra == name)
                    .map(|(_, html)| *html)
            });
        let response = match body {
            Some(body) => format!(
                "HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: {}\r\nConnection: close\r\nCache-Control: no-store\r\n\r\n{body}",
                body.len()
            ),
            None => "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
                .to_owned(),
        };
        let _ = stream.write_all(response.as_bytes());
    }

    fn url(port: u16, name: &str) -> String {
        format!("http://127.0.0.1:{port}/{name}.html")
    }

    pub fn run() {
        let event_loop = EventLoopBuilder::<UserEvent>::with_user_event().build();
        let window = WindowBuilder::new()
            .with_title("elitea WebView2 conformance")
            .with_inner_size(tao::dpi::LogicalSize::new(1280.0, 800.0))
            .build(&event_loop)
            .expect("window");
        let data =
            std::env::temp_dir().join(format!("elitea-webview2-test-{}", std::process::id()));
        let mut context = WebContext::new(Some(data.clone()));
        let webview = WebViewBuilder::new_with_web_context(&mut context)
            .with_url("about:blank")
            .build(&window)
            .expect("WebView2 (is the WebView2 runtime installed?)");

        let proxy = event_loop.create_proxy();
        let on_ui: OnUiThread = {
            let proxy = proxy.clone();
            Arc::new(move |work| proxy.send_event(UserEvent::Work(work)).is_ok())
        };
        let port = serve();
        let runtime = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()
            .expect("runtime");
        // The bridge's tasks run on the runtime; the COM calls on this thread.
        let stream = {
            let _entered = runtime.enter();
            open_stream(
                "conformance",
                &webview.controller(),
                on_ui,
                Arc::new(|_| {}),
            )
            .expect("the bridge")
        };
        // The tests on a thread of their own (the conformance kit's future
        // is not `Send`); the bridge's tasks stay on the runtime's workers.
        let handle = runtime.handle().clone();
        std::thread::spawn(move || {
            let outcome = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                handle.block_on(tests(stream, port))
            }));
            let _ = proxy.send_event(UserEvent::Done(matches!(outcome, Ok(true))));
        });

        event_loop.run(move |event, _, control_flow| {
            *control_flow = ControlFlow::Wait;
            // Keep the webview, its context and the runtime alive.
            let _ = (&webview, &context, &runtime, &window);
            match event {
                Event::UserEvent(UserEvent::Work(work)) => work(),
                Event::UserEvent(UserEvent::Done(ok)) => {
                    let _ = std::fs::remove_dir_all(&data);
                    if ok {
                        println!("webview2: ok");
                        std::process::exit(0);
                    }
                    eprintln!("webview2: FAILED");
                    std::process::exit(1);
                }
                _ => {}
            }
        });
    }

    async fn tests(stream: tokio::io::DuplexStream, port: u16) -> bool {
        let (read, write) = tokio::io::split(stream);
        let cdp = Connection::new(read, write);
        let config = EngineConfig {
            width: 1280,
            height: 800,
            ..EngineConfig::default()
        };
        let control = Arc::new(Control::new(false, Duration::from_secs(1)));
        let driver = match CdpDriver::attach_page(cdp, &config, control, PAGE_SESSION).await {
            Ok(driver) => driver,
            Err(error) => {
                println!("FAIL attach: {error}");
                return false;
            }
        };
        let mut ok = true;
        ok &= conformance_kit(&driver, port).await;
        ok &= rule_reads(&driver, port).await;
        ok &= trusted_input(&driver, port).await;
        ok
    }

    fn report(name: &str, outcome: Result<(), String>) -> bool {
        match outcome {
            Ok(()) => {
                println!("ok   {name}");
                true
            }
            Err(why) => {
                println!("FAIL {name}: {why}");
                false
            }
        }
    }

    async fn conformance_kit(driver: &CdpDriver, port: u16) -> bool {
        let url_for = move |fixture: &Fixture| url(port, fixture.name);
        let config = Config {
            url_for: &url_for,
            differences: Vec::new(),
        };
        let run = conformance::run(driver, &config).await;
        print!("{run}");
        let failures = run.failures();
        report(
            "conformance kit on the fixture pages",
            if failures.is_empty() {
                Ok(())
            } else {
                Err(format!("{} checks failed", failures.len()))
            },
        )
    }

    async fn call(driver: &CdpDriver, tool: &str, args: Value) -> Value {
        dispatch(driver, tool, &args).await.result().clone()
    }

    fn find_ref(
        snapshot: &elitea_browser_tools::Snapshot,
        role: &str,
        name: &str,
    ) -> Result<ElementRef, String> {
        let text = snapshot
            .find(role, name)
            .and_then(|node| node.ref_index)
            .map(|index| format!("@{}:e{index}", snapshot.id))
            .ok_or_else(|| format!("no {role} {name:?} in\n{}", snapshot.render_tree()))?;
        ElementRef::parse(&text).map_err(|error| error.to_string())
    }

    async fn rule_reads(driver: &CdpDriver, port: u16) -> bool {
        let outcome = async {
            let address = url(port, "controls");
            let opened = call(driver, spec::BROWSER_OPEN, json!({ "url": address })).await;
            if opened["status"] != "ok" {
                return Err(format!("open: {opened}"));
            }
            let current = driver.current_url().await.map_err(|e| e.to_string())?;
            if current != address {
                return Err(format!("current_url: {current}"));
            }
            let focused = driver.focused().await.map_err(|e| e.to_string())?;
            let focused = focused.ok_or("nothing focused")?;
            if focused.role != "button" || focused.name != "Place order" {
                return Err(format!("focused: {focused:?}"));
            }
            let snapshot = driver.snapshot(500).await.map_err(|e| e.to_string())?;
            let link = find_ref(&snapshot, "link", "Invoice 7")?;
            let info = driver.inspect(&link).await.map_err(|e| e.to_string())?;
            if !info.download {
                return Err(format!("the download attribute was not read: {info:?}"));
            }
            let field = find_ref(&snapshot, "textbox", "Resume")?;
            let info = driver.inspect(&field).await.map_err(|e| e.to_string())?;
            if info.input_type.as_deref() != Some("file") {
                return Err(format!("the file input was not read: {info:?}"));
            }
            Ok(())
        }
        .await;
        report(
            "the rules read the focus, an element and the live address",
            outcome,
        )
    }

    /// `Input.dispatchMouseEvent` is trusted input in `WebView2`: a page that
    /// acts on real clicks only sees the agent's click.
    async fn trusted_input(driver: &CdpDriver, port: u16) -> bool {
        let outcome = async {
            let opened = call(
                driver,
                spec::BROWSER_OPEN,
                json!({ "url": url(port, "untrusted") }),
            )
            .await;
            if opened["status"] != "ok" {
                return Err(format!("open: {opened}"));
            }
            let snapshot = driver.snapshot(500).await.map_err(|e| e.to_string())?;
            let go = find_ref(&snapshot, "button", "Continue")?;
            let clicked = driver.click(&go, false).await.map_err(|e| e.to_string())?;
            drop(clicked);
            tokio::time::sleep(Duration::from_millis(300)).await;
            let snapshot = driver.snapshot(500).await.map_err(|e| e.to_string())?;
            if snapshot.find("paragraph", "Clicked").is_none() {
                return Err(format!(
                    "the page did not see a real click:\n{}",
                    snapshot.render_tree()
                ));
            }
            Ok(())
        }
        .await;
        report("a page that takes real input only gets it", outcome)
    }
}
