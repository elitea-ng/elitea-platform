//! The macOS driver against a real WKWebView: the conformance kit of
//! `elitea-browser-tools` on its fixture pages, a cross-origin frame, the
//! isolation of the driver world from the page, and the trusted-input
//! fallback's refusal without the Accessibility grant.
//!
//! WebKit needs AppKit's main thread and a window server, so this test owns
//! `main` (`harness = false`) and is skipped unless asked for:
//!
//! ```text
//! cd apps/elitea-desktop/src-tauri
//! cargo test -p elitea-browser-driver-macos --test webkit -- --ignored
//! ```
//!
//! Run it on a Mac with a logged-in session (a GitHub macOS runner has
//! one). It opens one small window for a few seconds. The fixtures are
//! served over loopback `http` by a server in this process; the cross-
//! origin frame is the same server under `localhost` (another origin than
//! `127.0.0.1`).

#[cfg(not(target_os = "macos"))]
fn main() {
    println!("the WebKit tests run on macOS only");
}

#[cfg(target_os = "macos")]
fn main() {
    if !std::env::args().any(|arg| arg == "--ignored" || arg == "--include-ignored") {
        println!("webkit: 4 tests ignored (they open a WKWebView window): run with `-- --ignored`");
        return;
    }
    mac::run();
}

#[cfg(target_os = "macos")]
mod mac {
    use std::io::{BufRead as _, BufReader, Write as _};
    use std::net::TcpListener;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::time::Duration;

    use elitea_browser_driver_macos::{NavigationCheck, NoHandOver, WebDriver, WebKitEngine};
    use elitea_browser_tools::conformance::{self, Config, Difference, Fixture};
    use elitea_browser_tools::driver::BrowserDriver;
    use elitea_browser_tools::{ErrorCode, dispatch};
    use objc2::rc::Retained;
    use objc2::{MainThreadMarker, MainThreadOnly as _};
    use objc2_app_kit::{
        NSApplication, NSApplicationActivationPolicy, NSBackingStoreType, NSWindow,
        NSWindowStyleMask,
    };
    use objc2_core_foundation::{CGPoint, CGRect, CGSize};
    use objc2_foundation::NSString;
    use objc2_web_kit::{WKWebView, WKWebViewConfiguration, WKWebsiteDataStore};
    use serde_json::{Value, json};

    const EXTRA: &[(&str, &str)] = &[
        ("isolation", include_str!("fixtures/isolation.html")),
        ("cross-origin", include_str!("fixtures/cross-origin.html")),
        (
            "cross-origin-inner",
            include_str!("fixtures/cross-origin-inner.html"),
        ),
        ("untrusted", include_str!("fixtures/untrusted.html")),
    ];

    /// The fixtures over loopback http, one request per connection.
    fn serve() -> u16 {
        let listener = TcpListener::bind("127.0.0.1:0").expect("bind");
        let port = listener.local_addr().expect("addr").port();
        std::thread::spawn(move || {
            for stream in listener.incoming() {
                let Ok(mut stream) = stream else { continue };
                let mut line = String::new();
                if BufReader::new(&stream).read_line(&mut line).is_err() {
                    continue;
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
                    .map(|fixture| fixture.html.to_owned())
                    .or_else(|| {
                        EXTRA
                            .iter()
                            .find(|(extra, _)| *extra == name)
                            .map(|(_, html)| html.replace("{PORT}", &port.to_string()))
                    });
                let response = match body {
                    Some(body) => format!(
                        "HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: {}\r\nConnection: close\r\nCache-Control: no-store\r\n\r\n{body}",
                        body.len()
                    ),
                    None => {
                        "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
                            .to_owned()
                    }
                };
                let _ = stream.write_all(response.as_bytes());
            }
        });
        port
    }

    fn url(port: u16, name: &str) -> String {
        format!("http://127.0.0.1:{port}/{name}.html")
    }

    /// Where the WebKit driver's tree differs from the Chromium goldens.
    /// Each is pinned: the conformance run fails if the tree moves.
    fn differences() -> Vec<Difference> {
        Vec::new()
    }

    pub fn run() {
        let mtm = MainThreadMarker::new().expect("main thread");
        let app = NSApplication::sharedApplication(mtm);
        app.setActivationPolicy(NSApplicationActivationPolicy::Accessory);
        app.finishLaunching();

        // SAFETY: WebKit and AppKit setup on the main thread.
        let webview: Retained<WKWebView> = unsafe {
            let configuration = WKWebViewConfiguration::new(mtm);
            configuration.setWebsiteDataStore(&WKWebsiteDataStore::nonPersistentDataStore(mtm));
            let frame = CGRect::new(CGPoint::new(0.0, 0.0), CGSize::new(900.0, 700.0));
            let webview = WKWebView::initWithFrame_configuration(
                WKWebView::alloc(mtm),
                frame,
                &configuration,
            );
            let window = NSWindow::initWithContentRect_styleMask_backing_defer(
                NSWindow::alloc(mtm),
                frame,
                NSWindowStyleMask::Titled,
                NSBackingStoreType::Buffered,
                false,
            );
            window.setReleasedWhenClosed(false);
            window.setTitle(&NSString::from_str("elitea-browser-driver-macos test"));
            window.setContentView(Some(&webview));
            window.orderFrontRegardless();
            std::mem::forget(window);
            webview
        };

        let port = serve();
        let allow: NavigationCheck = Arc::new(|_| Ok(()));
        let engine = WebKitEngine::new(webview, mtm).with_trusted_input(Arc::new(|| false));
        let driver = WebDriver::new(engine, allow, Arc::new(NoHandOver));

        let done = Arc::new(AtomicBool::new(false));
        let failed = Arc::new(AtomicBool::new(false));
        {
            let done = done.clone();
            let failed = failed.clone();
            std::thread::spawn(move || {
                let runtime = tokio::runtime::Builder::new_current_thread()
                    .enable_all()
                    .build()
                    .expect("runtime");
                let outcome = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    runtime.block_on(tests(&driver, port))
                }));
                match outcome {
                    Ok(true) => {}
                    Ok(false) | Err(_) => failed.store(true, Ordering::SeqCst),
                }
                done.store(true, Ordering::SeqCst);
            });
        }

        while !done.load(Ordering::SeqCst) {
            // Spin AppKit's run loop so WebKit can load and answer.
            objc2_core_foundation::CFRunLoop::run_in_mode(
                // SAFETY: the default mode constant is a static CFString.
                unsafe { objc2_core_foundation::kCFRunLoopDefaultMode },
                0.05,
                true,
            );
        }
        if failed.load(Ordering::SeqCst) {
            eprintln!("webkit: FAILED");
            std::process::exit(1);
        }
        println!("webkit: ok");
    }

    async fn tests(driver: &WebDriver<WebKitEngine>, port: u16) -> bool {
        let mut ok = true;
        ok &= conformance_kit(driver, port).await;
        ok &= cross_origin_frame(driver, port).await;
        ok &= isolation(driver, port).await;
        ok &= untrusted_input(driver, port).await;
        ok
    }

    fn report(name: &str, outcome: Result<(), String>) -> bool {
        match outcome {
            Ok(()) => {
                println!("test {name} ... ok");
                true
            }
            Err(why) => {
                println!("test {name} ... FAILED\n{why}");
                false
            }
        }
    }

    async fn conformance_kit(driver: &WebDriver<WebKitEngine>, port: u16) -> bool {
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

    async fn call(driver: &WebDriver<WebKitEngine>, tool: &str, args: Value) -> Value {
        dispatch(driver, tool, &args).await.result().clone()
    }

    async fn tree(
        driver: &WebDriver<WebKitEngine>,
    ) -> Result<elitea_browser_tools::Snapshot, String> {
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

    async fn cross_origin_frame(driver: &WebDriver<WebKitEngine>, port: u16) -> bool {
        let outcome = async {
            let opened = call(driver, "browser_open", json!({ "url": url(port, "cross-origin") })).await;
            if opened["status"] != "ok" {
                return Err(format!("open: {opened}"));
            }
            // The inner frame loads after the outer document.
            let mut snapshot = tree(driver).await?;
            for _ in 0..40 {
                if snapshot.find("button", "Apply").is_some() {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(100)).await;
                snapshot = tree(driver).await?;
            }
            let expected = "- heading \"Outer\" [level=1]\n- iframe \"Widget\":\n  - textbox \"Coupon\" [ref=e1]\n  - button \"Apply\" [ref=e2]\n  - paragraph \"Not applied\"\n- link \"After the widget\" [ref=e3]\n";
            if snapshot.render_tree() != expected {
                return Err(format!("tree:\n{}", snapshot.render_tree()));
            }
            let coupon = find_ref(&snapshot, "textbox", "Coupon")?;
            let typed = call(driver, "browser_type", json!({ "ref": coupon, "text": "SAVE10" })).await;
            if typed["status"] != "ok" {
                return Err(format!("type: {typed}"));
            }
            let snapshot = tree(driver).await?;
            let apply = find_ref(&snapshot, "button", "Apply")?;
            let clicked = call(driver, "browser_click", json!({ "ref": apply })).await;
            if clicked["status"] != "ok" {
                return Err(format!("click: {clicked}"));
            }
            let snapshot = tree(driver).await?;
            if snapshot.find("paragraph", "Applied SAVE10").is_none() {
                return Err(format!("not applied:\n{}", snapshot.render_tree()));
            }
            let shot = dispatch(driver, "browser_screenshot", &json!({ "ref": apply.rsplit(':').next().unwrap_or_default() })).await;
            if shot.result()["status"] != "ok" {
                return Err(format!("element screenshot: {}", shot.result()));
            }
            Ok(())
        }
        .await;
        report(
            "a cross-origin frame is read and driven in its own frame",
            outcome,
        )
    }

    async fn isolation(driver: &WebDriver<WebKitEngine>, port: u16) -> bool {
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
            // The driver works although the page replaced JSON, the input
            // value accessor, getAttribute, getComputedStyle, dispatchEvent,
            // Promise.then and planted a fake driver global, all before the
            // driver's first call.
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
            // What the page itself saw, before load and after the driver ran.
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
            // A forged frame announcement from the page world never arrives.
            if !driver.engine_frames().await.is_empty() {
                return Err("the page posted to the driver's frame handler".to_owned());
            }
            Ok(())
        }
        .await;
        report("page scripts cannot see or call the driver world", outcome)
    }

    async fn untrusted_input(driver: &WebDriver<WebKitEngine>, port: u16) -> bool {
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
            if clicked["code"] != ErrorCode::Unsupported.as_str()
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
            "a page that takes real input only leads to browser_request_user",
            outcome,
        )
    }

    trait Frames {
        async fn engine_frames(&self) -> Vec<(u32, String)>;
    }

    impl Frames for WebDriver<WebKitEngine> {
        async fn engine_frames(&self) -> Vec<(u32, String)> {
            use elitea_browser_driver_macos::Engine as _;
            self.engine().frames().await
        }
    }
}
