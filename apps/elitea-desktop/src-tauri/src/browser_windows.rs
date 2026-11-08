//! The browser pane's Windows driver (ADR-0048 decision 6, #1593): the
//! cloud browser's CDP driver over the page's own `WebView2` CDP.
//!
//! `browser-driver-windows` (COM FFI, its own crate: this one forbids
//! `unsafe`) turns `WebView2`'s `CallDevToolsProtocolMethod` and
//! `DevTools` events into a CDP stream; `elitea-browser-session`'s
//! `CdpDriver::attach_page` drives it with the #1594 mapping
//! (`Accessibility.getFullAXTree`, `DOM`, `Input.dispatch*Event`,
//! `Page.captureScreenshot`), so a Windows pane gives the agent the same
//! trees and results as a cloud thread.
//!
//! The host's side of the control pill (`src/browser_control.rs`) is kept:
//!
//! - **The person's input.** The dom crate's input reporter
//!   (`INPUT_SCRIPT`) runs in an isolated world of its own
//!   (`Page.addScriptToEvaluateOnNewDocument` with `worldName`), reporting
//!   on a binding exposed to that world only (`Runtime.addBinding` with
//!   `executionContextName`). Each `Runtime.bindingCalled` is the
//!   person's input unless the driver's own CDP input caused it
//!   ([`elitea_browser_driver_dom::input::cdp_reports`]: `WebView2` makes
//!   `Input.*` trusted, so the page sees the agent's clicks as real).
//! - **`browser_request_user`** is the pane's hand-over, not the cloud
//!   driver's own control (which stays at "agent": the host gates every
//!   call before it reaches the driver).

use std::sync::{Arc, Mutex, PoisonError};
use std::time::{Duration, Instant};

use async_trait::async_trait;
use elitea_browser_driver_dom::input::{OwnInput, REPORTED, cdp_reports};
use elitea_browser_driver_dom::{INPUT_BINDING, INPUT_SCRIPT, INPUT_WORLD, NavigationCheck};
use elitea_browser_session::cdp::Connection;
use elitea_browser_session::control::Control;
use elitea_browser_session::driver::CdpDriver;
use elitea_browser_session::engine::EngineConfig;
use elitea_browser_tools::call::{ElementRef, Key, ScrollTarget, WaitCondition};
use elitea_browser_tools::driver::{
    BrowserDriver, ControlInfo, Handback, HistoryStep, PageInfo, PageText, Screenshot,
};
use elitea_browser_tools::snapshot::Snapshot;
use elitea_browser_tools::{BrowserError, ErrorCode};
use serde_json::json;
use tauri::{AppHandle, Webview};

/// The person's answer to `browser_request_user`, the host's.
pub type HandOver = Arc<
    dyn Fn(String) -> std::pin::Pin<Box<dyn Future<Output = Result<Handback, BrowserError>> + Send>>
        + Send
        + Sync,
>;

/// The pane's driver: the CDP driver, the agent's navigation guard and the
/// host's hand-over.
pub struct WindowsDriver {
    cdp: CdpDriver,
    check: NavigationCheck,
    hand_over: HandOver,
}

/// The page size a page scroll moves by when nothing better is known.
const PAGE: (u32, u32) = (1280, 800);

/// Attach the driver to a pane just made.
///
/// # Errors
///
/// `WebView2` could not be reached, or the page did not answer.
pub async fn attach(
    app: &AppHandle,
    webview: &Webview,
    check: NavigationCheck,
    hand_over: HandOver,
    person_input: Arc<dyn Fn() + Send + Sync>,
) -> Result<WindowsDriver, BrowserError> {
    let unavailable = |why: String| {
        BrowserError::new(
            ErrorCode::Unsupported,
            format!("the browser page is not available: {why}"),
        )
    };
    let own = Arc::new(Mutex::new(OwnInput::default()));
    let observe: elitea_browser_driver_windows::Observe = {
        let own = Arc::clone(&own);
        Arc::new(move |command| {
            let params = serde_json::from_str(&command.params).unwrap_or_default();
            let reports = cdp_reports(&command.method, &params);
            if reports.is_empty() {
                return;
            }
            let now = Instant::now();
            let mut own = own.lock().unwrap_or_else(PoisonError::into_inner);
            for (kind, count) in reports {
                own.expect(kind, count, now);
            }
        })
    };
    let stream = elitea_browser_driver_windows::attach(app, webview, observe)
        .await
        .map_err(unavailable)?;
    let (read, write) = tokio::io::split(stream);
    let cdp = Connection::new(read, write);
    let page = Some(elitea_browser_driver_windows::PAGE_SESSION);
    let call = |method: &'static str, params: serde_json::Value| {
        let cdp = cdp.clone();
        async move {
            cdp.call(page, method, params)
                .await
                .map_err(|error| unavailable(error.to_string()))
        }
    };
    // The input reporter's world and binding, before any page loads.
    call("Runtime.enable", json!({})).await?;
    call(
        "Runtime.addBinding",
        json!({ "name": INPUT_BINDING, "executionContextName": INPUT_WORLD }),
    )
    .await?;
    call(
        "Page.addScriptToEvaluateOnNewDocument",
        json!({ "source": INPUT_SCRIPT, "worldName": INPUT_WORLD }),
    )
    .await?;
    spawn_input_watch(&cdp, own, person_input);
    let config = EngineConfig {
        width: PAGE.0,
        height: PAGE.1,
        ..EngineConfig::default()
    };
    // The cloud driver's own control never changes: the host gates calls
    // (`src/browser_tools.rs`) and answers `browser_request_user` itself.
    let control = Arc::new(Control::new(true, Duration::from_mins(10)));
    let cdp = CdpDriver::attach_page(
        cdp,
        &config,
        control,
        elitea_browser_driver_windows::PAGE_SESSION,
    )
    .await?;
    Ok(WindowsDriver {
        cdp,
        check,
        hand_over,
    })
}

/// Each `Runtime.bindingCalled` of the reporter is an input of the person
/// unless the driver's own input caused it.
fn spawn_input_watch(
    cdp: &Connection,
    own: Arc<Mutex<OwnInput>>,
    person_input: Arc<dyn Fn() + Send + Sync>,
) {
    let mut events = cdp.events();
    tokio::spawn(async move {
        loop {
            let event = match events.recv().await {
                Ok(event) => event,
                Err(tokio::sync::broadcast::error::RecvError::Lagged(_)) => {
                    // Reports were lost: some of them may have been the
                    // person's, so the agent pauses (fail closed).
                    person_input();
                    continue;
                }
                Err(tokio::sync::broadcast::error::RecvError::Closed) => break,
            };
            if event.method != "Runtime.bindingCalled" || event.params["name"] != INPUT_BINDING {
                continue;
            }
            let kind = event.params["payload"].as_str().unwrap_or_default();
            if !REPORTED.contains(&kind) {
                continue;
            }
            let mine = own
                .lock()
                .unwrap_or_else(PoisonError::into_inner)
                .is_own(kind, Instant::now());
            if !mine {
                person_input();
            }
        }
    });
}

#[async_trait]
impl BrowserDriver for WindowsDriver {
    async fn open(&self, url: &str) -> Result<PageInfo, BrowserError> {
        elitea_browser_tools::call::check_url(url)?;
        (self.check)(url)?;
        self.cdp.open(url).await
    }

    async fn snapshot(&self, max_nodes: usize) -> Result<Snapshot, BrowserError> {
        self.cdp.snapshot(max_nodes).await
    }

    async fn text(&self, max_chars: usize) -> Result<PageText, BrowserError> {
        self.cdp.text(max_chars).await
    }

    async fn screenshot(&self, target: Option<&ElementRef>) -> Result<Screenshot, BrowserError> {
        self.cdp.screenshot(target).await
    }

    async fn click(&self, target: &ElementRef, double: bool) -> Result<PageInfo, BrowserError> {
        self.cdp.click(target, double).await
    }

    async fn type_text(
        &self,
        target: &ElementRef,
        text: &str,
        clear: bool,
        submit: bool,
    ) -> Result<PageInfo, BrowserError> {
        self.cdp.type_text(target, text, clear, submit).await
    }

    async fn select(
        &self,
        target: &ElementRef,
        values: &[String],
    ) -> Result<PageInfo, BrowserError> {
        self.cdp.select(target, values).await
    }

    async fn scroll(&self, target: &ScrollTarget) -> Result<PageInfo, BrowserError> {
        self.cdp.scroll(target).await
    }

    async fn press(&self, key: &Key) -> Result<PageInfo, BrowserError> {
        self.cdp.press(key).await
    }

    async fn history(&self, step: HistoryStep) -> Result<PageInfo, BrowserError> {
        self.cdp.history(step).await
    }

    async fn wait(
        &self,
        condition: &WaitCondition,
        timeout: Duration,
    ) -> Result<PageInfo, BrowserError> {
        self.cdp.wait(condition, timeout).await
    }

    async fn request_user(&self, reason: &str) -> Result<Handback, BrowserError> {
        (self.hand_over)(reason.to_owned()).await
    }

    async fn focused(&self) -> Result<Option<ControlInfo>, BrowserError> {
        self.cdp.focused().await
    }

    async fn inspect(&self, target: &ElementRef) -> Result<ControlInfo, BrowserError> {
        self.cdp.inspect(target).await
    }

    async fn current_url(&self) -> Result<String, BrowserError> {
        self.cdp.current_url().await
    }
}
