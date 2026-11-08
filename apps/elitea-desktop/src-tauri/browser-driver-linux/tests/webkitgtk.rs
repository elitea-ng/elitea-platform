//! The Linux driver against a real WebKitGTK `WebView`: the conformance kit
//! of `elitea-browser-tools` on its fixture pages, the isolation of the
//! driver world from the page, the rules' reads (the focus, an element, the
//! live address), and the trusted-input fallback on X11.
//!
//! GTK needs its main thread and a display, so this test owns `main`
//! (`harness = false`) and is skipped unless asked for:
//!
//! ```text
//! cd apps/elitea-desktop/src-tauri
//! xvfb-run -a cargo test -p elitea-browser-driver-linux --test webkitgtk -- --ignored
//! ```
//!
//! The fixtures (shared with the macOS run, `browser-driver-macos/tests/fixtures`)
//! are served over loopback `http` by a server in this process.

#[cfg(not(target_os = "linux"))]
fn main() {
    println!("the WebKitGTK tests run on Linux only");
}

#[cfg(target_os = "linux")]
fn main() {
    if !std::env::args().any(|arg| arg == "--ignored" || arg == "--include-ignored") {
        println!("webkitgtk: 4 tests ignored (they open a GTK window): run with `-- --ignored`");
        return;
    }
    gtk_run::run();
}

#[cfg(target_os = "linux")]
mod gtk_run {
    use std::io::{BufRead as _, BufReader, Write as _};
    use std::net::TcpListener;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::time::Duration;

    use elitea_browser_driver_linux::{
        GtkEngine, NavigationCheck, NoHandOver, WebDriver, attach_to,
    };
    use elitea_browser_tools::conformance::{self, Config, Difference, Fixture};
    use elitea_browser_tools::driver::BrowserDriver as _;
    use elitea_browser_tools::{ErrorCode, dispatch};
    use gtk::prelude::*;
    use serde_json::{Value, json};
    use webkit2gtk::{WebContext, WebView};

    const EXTRA: &[(&str, &str)] = &[
        (
            "isolation",
            include_str!("../../browser-driver-macos/tests/fixtures/isolation.html"),
        ),
        (
            "untrusted",
            include_str!("../../browser-driver-macos/tests/fixtures/untrusted.html"),
        ),
        (
            "controls",
            include_str!("../../browser-driver-macos/tests/fixtures/controls.html"),
        ),
    ];

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
        let mut line = String::new();
        if BufReader::new(&stream).read_line(&mut line).is_err() {
            return;
        }
        let path = line.split_whitespace().nth(1).unwrap_or("/");
        let name = path
            .trim_start_matches('/')
            .split(['?', '#'])
            .next()
            .unwrap_or_default()
            .trim_end_matches(".html");
        let body = conformance::FIXTURES
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

    /// Where the WebKitGTK driver's tree differs from the Chromium goldens.
    /// Each is pinned: the conformance run fails if the tree moves.
    fn differences() -> Vec<Difference> {
        Vec::new()
    }

    pub fn run() {
        gtk::init().expect("GTK (is a display set? run under xvfb-run)");
        let window = gtk::Window::new(gtk::WindowType::Toplevel);
        window.set_default_size(900, 700);
        // An ephemeral context, as the pane's private store would be.
        let context = WebContext::new_ephemeral();
        let webview = WebView::with_context(&context);
        window.add(&webview);
        window.show_all();

        let port = serve();
        let allow: NavigationCheck = Arc::new(|_| Ok(()));
        let engine = attach_to(&webview);
        let driver = WebDriver::new(engine, allow, Arc::new(NoHandOver));

        let done = Arc::new(AtomicBool::new(false));
        let failed = Arc::new(AtomicBool::new(false));
        {
            let (done, failed) = (done.clone(), failed.clone());
            std::thread::spawn(move || {
                let runtime = tokio::runtime::Builder::new_current_thread()
                    .enable_all()
                    .build()
                    .expect("runtime");
                let outcome = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    runtime.block_on(tests(&driver, port))
                }));
                if !matches!(outcome, Ok(true)) {
                    failed.store(true, Ordering::SeqCst);
                }
                done.store(true, Ordering::SeqCst);
            });
        }
        let main = gtk::glib::MainContext::default();
        while !done.load(Ordering::SeqCst) {
            // Spin GTK's main loop so WebKit can load and answer.
            main.iteration(false);
            std::thread::sleep(Duration::from_millis(2));
        }
        if failed.load(Ordering::SeqCst) {
            eprintln!("webkitgtk: FAILED");
            std::process::exit(1);
        }
        println!("webkitgtk: ok");
    }

    async fn tests(driver: &WebDriver<GtkEngine>, port: u16) -> bool {
        let mut ok = true;
        ok &= conformance_kit(driver, port).await;
        ok &= isolation(driver, port).await;
        ok &= rule_reads(driver, port).await;
        ok &= trusted_input(driver, port).await;
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

    async fn conformance_kit(driver: &WebDriver<GtkEngine>, port: u16) -> bool {
        let url_for = move |fixture: &Fixture| url(port, fixture.name);
        let config = Config {
            url_for: &url_for,
            differences: differences(),
        };
        let report_ = conformance::run(driver, &config).await;
        print!("{report_}");
        let failures = report_.failures();
        report(
            "conformance kit on the fixture pages",
            if failures.is_empty() {
                Ok(())
            } else {
                Err(format!("{} checks failed", failures.len()))
            },
        )
    }

    async fn call(driver: &WebDriver<GtkEngine>, tool: &str, args: Value) -> Value {
        dispatch(driver, tool, &args).await.result().clone()
    }

    async fn tree(driver: &WebDriver<GtkEngine>) -> Result<elitea_browser_tools::Snapshot, String> {
        driver
            .snapshot(500)
            .await
            .map_err(|error| error.to_string())
    }

    fn find_ref(
        snapshot: &elitea_browser_tools::Snapshot,
        role: &str,
        name: &str,
    ) -> Result<String, String> {
        snapshot
            .find(role, name)
            .and_then(|node| node.ref_index)
            .map(|index| format!("@{}:e{index}", snapshot.id))
            .ok_or_else(|| format!("no {role} {name:?} in\n{}", snapshot.render_tree()))
    }

    async fn isolation(driver: &WebDriver<GtkEngine>, port: u16) -> bool {
        let outcome = async {
            let opened = call(
                driver,
                "browser_open",
                json!({ "url": url(port, "isolation") }),
            )
            .await;
            if opened["status"] != "ok" {
                return Err(format!("open: {opened}"));
            }
            let snapshot = tree(driver).await?;
            if snapshot.title != "Isolation" || snapshot.find("heading", "Isolation").is_none() {
                return Err(format!(
                    "the page's tampering reached the driver:\n{}",
                    snapshot.render()
                ));
            }
            let name = find_ref(&snapshot, "textbox", "Name")?;
            let typed = call(
                driver,
                "browser_type",
                json!({ "ref": name, "text": "Ada" }),
            )
            .await;
            if typed["status"] != "ok" {
                return Err(format!("type: {typed}"));
            }
            tokio::time::sleep(Duration::from_millis(500)).await;
            let snapshot = tree(driver).await?;
            let field = snapshot.find("textbox", "Name").ok_or("no Name field")?;
            if field.value.as_deref() != Some("Ada") {
                return Err(format!(
                    "the page's value accessor was used: {:?}",
                    field.value
                ));
            }
            let probe = snapshot
                .nodes
                .iter()
                .find(|node| node.role == "paragraph")
                .map(|node| node.name.clone())
                .ok_or("no probe paragraph")?;
            if !probe.contains("before load: nothing") || !probe.contains("later: nothing") {
                return Err(format!(
                    "the page did not probe after the driver ran: {probe}"
                ));
            }
            for line in probe.split("; ") {
                if !line.ends_with("nothing") {
                    return Err(format!("the page reached the driver world: {line}"));
                }
            }
            Ok(())
        }
        .await;
        report("page scripts cannot see or call the driver world", outcome)
    }

    async fn rule_reads(driver: &WebDriver<GtkEngine>, port: u16) -> bool {
        let outcome = async {
            let opened = call(
                driver,
                "browser_open",
                json!({ "url": url(port, "controls") }),
            )
            .await;
            if opened["status"] != "ok" {
                return Err(format!("open: {opened}"));
            }
            let address = driver.current_url().await.map_err(|e| e.to_string())?;
            if !address.ends_with("/controls.html") {
                return Err(format!("current_url: {address}"));
            }
            let focused = driver.focused().await.map_err(|e| e.to_string())?;
            let focused = focused.ok_or("nothing focused")?;
            if focused.role != "button" || focused.name != "Place order" {
                return Err(format!("focused: {focused:?}"));
            }
            let snapshot = tree(driver).await?;
            let link = find_ref(&snapshot, "link", "Invoice 7")?;
            let link = elitea_browser_tools::ElementRef::parse(&link).map_err(|e| e.to_string())?;
            let info = driver.inspect(&link).await.map_err(|e| e.to_string())?;
            if !info.download {
                return Err(format!("the download attribute was not read: {info:?}"));
            }
            let field = find_ref(&snapshot, "textbox", "Resume")?;
            let field =
                elitea_browser_tools::ElementRef::parse(&field).map_err(|e| e.to_string())?;
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

    /// A page that takes real input only: on X11 (xvfb) XTEST fake input
    /// clicks it; elsewhere the model is told to hand the page over.
    async fn trusted_input(driver: &WebDriver<GtkEngine>, port: u16) -> bool {
        use elitea_browser_driver_linux::Engine as _;
        let outcome = async {
            let opened = call(
                driver,
                "browser_open",
                json!({ "url": url(port, "untrusted") }),
            )
            .await;
            if opened["status"] != "ok" {
                return Err(format!("open: {opened}"));
            }
            let snapshot = tree(driver).await?;
            let go = find_ref(&snapshot, "button", "Continue")?;
            let clicked = call(driver, "browser_click", json!({ "ref": go })).await;
            if driver.engine().trusted_input() {
                if clicked["status"] != "ok" {
                    return Err(format!("the trusted click failed: {clicked}"));
                }
                // XTEST's input goes through the X server and then to
                // the web process: the page sees it after the call returns,
                // so wait for it rather than snapshot at once.
                let seen = call(
                    driver,
                    "browser_wait",
                    json!({ "text": "Clicked", "timeout_ms": 5000 }),
                )
                .await;
                if seen["status"] != "ok" {
                    let snapshot = tree(driver).await?;
                    return Err(format!(
                        "the page did not see a real click ({seen}):\n{}",
                        snapshot.render_tree()
                    ));
                }
            } else if clicked["code"] != ErrorCode::Unsupported.as_str()
                || !clicked["message"]
                    .as_str()
                    .unwrap_or_default()
                    .contains("browser_request_user")
            {
                return Err(format!("expected the hand-over hint, got {clicked}"));
            }
            Ok(())
        }
        .await;
        report(
            "a page that takes real input only gets it (X11) or a hand-over",
            outcome,
        )
    }
}
