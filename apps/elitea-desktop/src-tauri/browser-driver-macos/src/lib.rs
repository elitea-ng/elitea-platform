//! The desktop browser pane's macOS driver (ADR-0048 decisions 5 and 6):
//! the WebKit engine under the shared webview driver.
//!
//! The pane's page is a WKWebView (a Tauri child webview, #1588). The
//! contract logic ([`WebDriver`]: refs, hard limits, the act rate, frames,
//! the trusted-input fallback) and the page script are the shared crate
//! `elitea-browser-driver-dom`, which the Linux driver uses too. This crate
//! is only what macOS needs native calls for:
//!
//! * `webkit` (macOS) — [`WebKitEngine`], the WebKit, AppKit and
//!   CoreGraphics calls: the page script in a private `WKContentWorld`,
//!   `callAsyncJavaScript` in that world, `takeSnapshot(with:)`, and CGEvent
//!   clicks posted to this process only when the Accessibility grant
//!   (ADR-0047) is there.
//! * `native` (macOS) — the pane's own controls that wry and Tauri do not
//!   expose (#1634): the private-network `WKContentRuleList`
//!   ([`native::install_content_rules`]) and native Back, Forward and Stop
//!   ([`native::go`]).
//!
//! One of the two first-party crates allowed `unsafe` (`unsafe_code =
//! "deny"`, each FFI item opted in with `#[allow(unsafe_code)]` and a
//! `// SAFETY:` comment; repo-root `AGENTS.md`). Everything it exports is
//! safe to call.

#[cfg(target_os = "macos")]
pub mod native;
#[cfg(target_os = "macos")]
pub mod webkit;

pub use elitea_browser_driver_dom::{
    Engine, EnginePage, HandOver, NavigationCheck, NoHandOver, SCRIPT, WebDriver, driver, input,
    reply,
};
#[cfg(target_os = "macos")]
pub use webkit::{WebKitEngine, accessibility_granted};

/// The driver of a Tauri child webview (the pane's page): installs the
/// driver world in it. Call once per webview, right after it is made.
/// `person_input` runs on each trusted input of the person in the page
/// (the control pill's take-over, ADR-0048 decision 4).
///
/// # Errors
///
/// `unsupported` when the webview is not a WKWebView or Tauri could not
/// reach it.
#[cfg(all(target_os = "macos", feature = "tauri"))]
pub async fn attach(
    webview: &tauri::Webview,
    check: NavigationCheck,
    hand_over: std::sync::Arc<dyn HandOver>,
    person_input: std::sync::Arc<dyn Fn() + Send + Sync>,
) -> Result<WebDriver<WebKitEngine>, elitea_browser_tools::BrowserError> {
    use elitea_browser_tools::{BrowserError, ErrorCode};
    let unavailable =
        || BrowserError::new(ErrorCode::Unsupported, "the browser page is not available");
    let (tx, rx) = tokio::sync::oneshot::channel();
    webview
        .with_webview(move |platform| {
            let _ = tx.send(webkit::engine_of(&platform));
        })
        .map_err(|_| unavailable())?;
    let engine = rx.await.ok().flatten().ok_or_else(unavailable)?;
    engine.on_person_input(person_input);
    Ok(WebDriver::new(engine, check, hand_over))
}
