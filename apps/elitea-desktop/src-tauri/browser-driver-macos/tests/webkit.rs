//! The macOS driver against a real WKWebView: the conformance kit of
//! `elitea-browser-tools` on its fixture pages, a cross-origin frame, the
//! isolation of the driver world from the page, the trusted-input
//! fallback's refusal without the Accessibility grant, the pane's content
//! rules (the host's own JSON, `fixtures/content-rules.json`) blocking
//! loopback and private loads, and native Back against a page that
//! overrides `history.back`.
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
//!
//! The content-rules test needs a page on a PUBLIC origin that loads from
//! this loopback server. It loads inline HTML (`loadHTMLString`) under the
//! base URL `http://public.example/`, no network involved, in a webview
//! of its own with the rules installed; the server records every request
//! that reaches it. The same page in a webview WITHOUT the rules must reach
//! the server, so the block is the rules' and not WebKit's own.

#[cfg(not(target_os = "macos"))]
fn main() {
    println!("the WebKit tests run on macOS only");
}

#[cfg(target_os = "macos")]
fn main() {
    if !std::env::args().any(|arg| arg == "--ignored" || arg == "--include-ignored") {
        println!("webkit: 6 tests ignored (they open WKWebView windows): run with `-- --ignored`");
        return;
    }
    mac::run();
}

#[cfg(target_os = "macos")]
mod mac {
    use std::io::{BufRead as _, BufReader, Write as _};
    use std::net::TcpListener;
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::sync::{Arc, Mutex};
    use std::time::Duration;

    use block2::RcBlock;
    use dispatch2::{DispatchQueue, MainThreadBound};
    use elitea_browser_driver_macos::native::{self, Go};
    use elitea_browser_driver_macos::{NavigationCheck, NoHandOver, WebDriver, WebKitEngine};
    use elitea_browser_tools::conformance::{self, Config, Difference, Fixture};
    use elitea_browser_tools::driver::BrowserDriver;
    use elitea_browser_tools::{ErrorCode, dispatch};
    use objc2::rc::Retained;
    use objc2::runtime::AnyObject;
    use objc2::{MainThreadMarker, MainThreadOnly as _};
    use objc2_app_kit::{
        NSApplication, NSApplicationActivationPolicy, NSBackingStoreType, NSWindow,
        NSWindowStyleMask,
    };
    use objc2_core_foundation::{CGPoint, CGRect, CGSize};
    use objc2_foundation::{NSError, NSString, NSURL, NSURLRequest};
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
        ("history-a", include_str!("fixtures/history-a.html")),
        ("history-b", include_str!("fixtures/history-b.html")),
        ("controls", include_str!("fixtures/controls.html")),
    ];

    /// The host's rules (`content_rules` in `src/browser_pane.rs`; a host
    /// test keeps this copy equal to them).
    const RULES: &str = include_str!("fixtures/content-rules.json");

    /// The page on a public origin that loads from the loopback server.
    const PRIVATE_LOADS: &str = include_str!("fixtures/private-loads.html");

    /// Every `/probe?<token>` request that reached the server.
    static PROBES: Mutex<Vec<String>> = Mutex::new(Vec::new());

    fn probes(token: &str) -> usize {
        PROBES
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .iter()
            .filter(|seen| seen.starts_with(token))
            .count()
    }

    /// The fixtures over loopback http, one request per connection, each
    /// connection on its own thread (WebKit may open one it never uses).
    fn serve() -> u16 {
        let listener = TcpListener::bind("127.0.0.1:0").expect("bind");
        let port = listener.local_addr().expect("addr").port();
        std::thread::spawn(move || {
            for stream in listener.incoming() {
                let Ok(stream) = stream else { continue };
                std::thread::spawn(move || answer(stream, port));
            }
        });
        port
    }

    fn answer(mut stream: std::net::TcpStream, port: u16) {
        let mut line = String::new();
        if BufReader::new(&stream).read_line(&mut line).is_err() {
            return;
        }
        let path = line.split_whitespace().nth(1).unwrap_or("/");
        if let Some(query) = path.strip_prefix("/probe?") {
            PROBES
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .push(query.to_owned());
        }
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
            None => "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
                .to_owned(),
        };
        let _ = stream.write_all(response.as_bytes());
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

        let webview = window_with_webview(mtm, "elitea-browser-driver-macos test", 0.0);
        let page = Arc::new(MainThreadBound::new(webview.clone(), mtm));
        let ruled = Arc::new(MainThreadBound::new(
            window_with_webview(mtm, "content rules", 920.0),
            mtm,
        ));
        let unruled = Arc::new(MainThreadBound::new(
            window_with_webview(mtm, "no content rules", 1840.0),
            mtm,
        ));

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
                    runtime.block_on(tests(
                        &driver,
                        port,
                        &Views {
                            page,
                            ruled,
                            unruled,
                        },
                    ))
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
            objc2_core_foundation::CFRunLoop::run_in_mode(default_mode(), 0.05, true);
        }
        if failed.load(Ordering::SeqCst) {
            eprintln!("webkit: FAILED");
            std::process::exit(1);
        }
        println!("webkit: ok");
    }

    /// A WKWebView with a non-persistent store, alone in a small window at
    /// `x` (the windows sit side by side).
    #[allow(unsafe_code)]
    fn window_with_webview(mtm: MainThreadMarker, title: &str, x: f64) -> Retained<WKWebView> {
        // SAFETY: on the main thread (`mtm`). Every call is a WebKit or
        // AppKit initialiser or setter given live, retained objects of the
        // declared classes; the window is never released by AppKit
        // (`setReleasedWhenClosed(false)`) and is leaked on purpose, so the
        // webview's superview outlives the test.
        unsafe {
            let configuration = WKWebViewConfiguration::new(mtm);
            configuration.setWebsiteDataStore(&WKWebsiteDataStore::nonPersistentDataStore(mtm));
            let frame = CGRect::new(CGPoint::new(x, 0.0), CGSize::new(900.0, 700.0));
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
            window.setTitle(&NSString::from_str(title));
            window.setContentView(Some(&webview));
            window.orderFrontRegardless();
            std::mem::forget(window);
            webview
        }
    }

    #[allow(unsafe_code)]
    fn default_mode() -> Option<&'static objc2_core_foundation::CFRunLoopMode> {
        // SAFETY: `kCFRunLoopDefaultMode` is an immutable CFString static
        // that CoreFoundation initialises before `main`; reading it has no
        // other precondition.
        unsafe { objc2_core_foundation::kCFRunLoopDefaultMode }
    }

    /// The test's own webviews (the driver's page, and two for the rules).
    struct Views {
        page: Arc<MainThreadBound<Retained<WKWebView>>>,
        ruled: Arc<MainThreadBound<Retained<WKWebView>>>,
        unruled: Arc<MainThreadBound<Retained<WKWebView>>>,
    }

    /// Run `work` on the main thread with `view`; its answer.
    async fn on_main<T: Send + 'static>(
        view: &Arc<MainThreadBound<Retained<WKWebView>>>,
        work: impl FnOnce(&WKWebView) -> T + Send + 'static,
    ) -> T {
        let (tx, rx) = tokio::sync::oneshot::channel();
        let view = view.clone();
        DispatchQueue::main().exec_async(move || {
            let mtm = MainThreadMarker::new().expect("main queue");
            let _ = tx.send(work(view.get(mtm)));
        });
        rx.await.expect("the main thread answered")
    }

    #[allow(unsafe_code)]
    fn title_of(webview: &WKWebView) -> String {
        // SAFETY: on the main thread (a `&WKWebView` is main-thread only);
        // `title` is a getter returning a retained NSString or nil.
        unsafe { webview.title() }
            .map(|title| title.to_string())
            .unwrap_or_default()
    }

    #[allow(unsafe_code)]
    fn url_of(webview: &WKWebView) -> String {
        // SAFETY: on the main thread; `URL` and `absoluteString` are getters
        // returning retained objects or nil.
        unsafe { webview.URL() }
            .and_then(|url| url.absoluteString())
            .map(|url| url.to_string())
            .unwrap_or_default()
    }

    /// `url title` once the page finished loading, else empty: `URL`
    /// changes when a navigation STARTS, before its document commits.
    #[allow(unsafe_code)]
    fn settled(webview: &WKWebView) -> String {
        // SAFETY: on the main thread (a `&WKWebView` is main-thread only);
        // `isLoading` is a BOOL getter.
        if unsafe { webview.isLoading() } {
            return format!("loading {}", url_of(webview));
        }
        format!("{} {}", url_of(webview), title_of(webview))
    }

    #[allow(unsafe_code)]
    fn load_url(webview: &WKWebView, address: &str) {
        let url = NSURL::URLWithString(&NSString::from_str(address)).expect("a URL");
        let request = NSURLRequest::requestWithURL(&url);
        // SAFETY: on the main thread, with a retained NSURLRequest; the
        // returned navigation is dropped.
        drop(unsafe { webview.loadRequest(&request) });
    }

    #[allow(unsafe_code)]
    fn load_html(webview: &WKWebView, html: &str, base: &str) {
        let base = NSURL::URLWithString(&NSString::from_str(base)).expect("a URL");
        // SAFETY: on the main thread, with live NSString and NSURL
        // arguments (WebKit copies them); the navigation is dropped.
        drop(unsafe { webview.loadHTMLString_baseURL(&NSString::from_str(html), Some(&base)) });
    }

    /// Run `source` in the PAGE's world (as `webview.eval` does in the
    /// host), ignoring its result.
    #[allow(unsafe_code)]
    fn eval_in_page(webview: &WKWebView, source: &str) {
        let done = RcBlock::new(|_result: *mut AnyObject, _error: *mut NSError| {});
        // SAFETY: on the main thread; the source is a live NSString and the
        // block has the declared `(id, NSError *)` signature (WebKit copies
        // it).
        unsafe {
            webview.evaluateJavaScript_completionHandler(&NSString::from_str(source), Some(&done));
        }
    }

    /// Until `view`'s `read` satisfies `done`, at most `seconds`; the last read.
    async fn until(
        view: &Arc<MainThreadBound<Retained<WKWebView>>>,
        read: fn(&WKWebView) -> String,
        done: impl Fn(&str) -> bool,
        seconds: u64,
    ) -> String {
        let mut last = String::new();
        for _ in 0..seconds * 10 {
            last = on_main(view, read).await;
            if done(&last) {
                break;
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
        last
    }

    async fn tests(driver: &WebDriver<WebKitEngine>, port: u16, views: &Views) -> bool {
        let mut ok = true;
        ok &= conformance_kit(driver, port).await;
        ok &= cross_origin_frame(driver, port).await;
        ok &= isolation(driver, port).await;
        ok &= untrusted_input(driver, port).await;
        ok &= rule_reads(driver, port).await;
        ok &= content_rules(views, port).await;
        ok &= native_history(views, port).await;
        ok
    }

    /// The loads' outcomes the page put in its title, once all three ended.
    fn outcomes(title: &str) -> Option<Value> {
        let value: Value = serde_json::from_str(title).ok()?;
        ["fetch", "loopback_img", "private_img"]
            .iter()
            .all(|key| value.get(key).is_some())
            .then_some(value)
    }

    async fn content_rules(views: &Views, port: u16) -> bool {
        let outcome = async {
            let installed = on_main(&views.ruled, |webview| {
                let (tx, rx) = std::sync::mpsc::channel();
                native::install_content_rules_on(webview, RULES.trim_end(), move |outcome| {
                    let _ = tx.send(outcome);
                });
                rx
            })
            .await;
            // The completion runs on the main thread, which the test thread
            // does not block: poll the channel.
            let mut answer = None;
            for _ in 0..100 {
                if let Ok(outcome) = installed.try_recv() {
                    answer = Some(outcome);
                    break;
                }
                tokio::time::sleep(Duration::from_millis(50)).await;
            }
            match answer {
                Some(Ok(())) => {}
                Some(Err(error)) => {
                    return Err(format!("WebKit refused the host's rules: {error}"));
                }
                None => return Err("the rules never compiled".to_owned()),
            }
            let page = |token: &str| {
                PRIVATE_LOADS
                    .replace("{PORT}", &port.to_string())
                    .replace("?fetch", &format!("?{token}-fetch"))
                    .replace("?img", &format!("?{token}-img"))
            };
            // Without the rules the page reaches the loopback server: the
            // block below is the rules', not WebKit's own.
            let html = page("unruled");
            on_main(&views.unruled, move |webview| {
                load_html(webview, &html, "http://public.example/");
            })
            .await;
            for _ in 0..50 {
                if probes("unruled-fetch") > 0 {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(100)).await;
            }
            if probes("unruled-fetch") == 0 {
                return Err("the control page never reached the loopback server".to_owned());
            }
            // With the rules: a page on a public origin loads, its scripts
            // run, and its loopback fetch and both images are blocked
            // before any request leaves.
            let html = page("ruled");
            on_main(&views.ruled, move |webview| {
                load_html(webview, &html, "http://public.example/");
            })
            .await;
            let title = until(
                &views.ruled,
                title_of,
                |title| outcomes(title).is_some(),
                10,
            )
            .await;
            let seen = outcomes(&title).ok_or(format!("the loads never ended: {title:?}"))?;
            let expected =
                json!({ "fetch": "error", "loopback_img": "error", "private_img": "error" });
            if seen != expected {
                return Err(format!("a private load was not blocked: {seen}"));
            }
            let shown = on_main(&views.ruled, url_of).await;
            if shown != "http://public.example/" {
                return Err(format!("the public page did not load: {shown}"));
            }
            // A top-level navigation to loopback is blocked too.
            let address = format!("http://127.0.0.1:{port}/probe?ruled-nav");
            on_main(&views.ruled, move |webview| load_url(webview, &address)).await;
            tokio::time::sleep(Duration::from_millis(1000)).await;
            let reached = probes("ruled");
            if reached != 0 {
                return Err(format!(
                    "{reached} blocked loads reached the loopback server"
                ));
            }
            Ok(())
        }
        .await;
        report(
            "content rules block loopback and private loads from a public page",
            outcome,
        )
    }

    async fn native_history(views: &Views, port: u16) -> bool {
        let outcome = async {
            let a = "/history-a.html History A";
            let b = "/history-b.html History B";
            for (name, want) in [("history-a", a), ("history-b", b)] {
                let address = url(port, name);
                on_main(&views.page, move |webview| load_url(webview, &address)).await;
                let shown = until(&views.page, settled, |seen| seen.ends_with(want), 10).await;
                if !shown.ends_with(want) {
                    return Err(format!("{name} did not load: {shown:?}"));
                }
            }
            // The page's override holds against an in-page `history.back()`
            // (what the toolbar ran before)...
            on_main(&views.page, |webview| {
                eval_in_page(webview, "history.back()")
            })
            .await;
            tokio::time::sleep(Duration::from_millis(700)).await;
            let stayed = on_main(&views.page, settled).await;
            if !stayed.ends_with(b) {
                return Err(format!(
                    "the page's override did not hold (test is moot): {stayed:?}"
                ));
            }
            // ...and the toolbar's native Back goes back anyway.
            on_main(&views.page, |webview| native::go_on(webview, Go::Back)).await;
            let back = until(&views.page, settled, |seen| seen.ends_with(a), 10).await;
            if !back.ends_with(a) {
                return Err(format!("native Back did not go back: {back:?}"));
            }
            on_main(&views.page, |webview| native::go_on(webview, Go::Forward)).await;
            let forward = until(&views.page, settled, |seen| seen.ends_with(b), 10).await;
            if !forward.ends_with(b) {
                return Err(format!("native Forward did not go forward: {forward:?}"));
            }
            on_main(&views.page, |webview| native::go_on(webview, Go::Stop)).await;
            Ok(())
        }
        .await;
        report(
            "native Back and Forward work on a page that overrides history",
            outcome,
        )
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

    /// What the host's rules read before deciding a call (ADR-0048
    /// decision 8): the focused control, an element's input type and
    /// download attribute, and the live address.
    async fn rule_reads(driver: &WebDriver<WebKitEngine>, port: u16) -> bool {
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
