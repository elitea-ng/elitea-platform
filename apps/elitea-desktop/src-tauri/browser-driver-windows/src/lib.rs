//! The desktop browser pane's Windows driver (ADR-0048 decision 6): the
//! page's own `WebView2` CDP as a CDP byte stream.
//!
//! `WebView2` has the Chrome `DevTools` Protocol built in, so the Windows
//! pane is driven by the cloud browser's CDP driver
//! (`elitea-browser-session`'s `CdpDriver::attach_page`, the #1594 mapping:
//! `Accessibility.getFullAXTree`, `DOM`, `Input.dispatch*Event`,
//! `Page.captureScreenshot`). That driver speaks CDP as NUL-terminated
//! JSON records over a byte stream. This crate makes that stream out of
//! `WebView2`'s COM API:
//!
//! * a command `{id, method, params, sessionId?}` becomes
//!   `ICoreWebView2::CallDevToolsProtocolMethod` (the page's own session,
//!   [`PAGE_SESSION`]) or `ICoreWebView2_11::CallDevToolsProtocolMethodForSession`
//!   (a child target the page auto-attached), and its completion becomes
//!   `{id, result}` or `{id, error}`;
//! * each event the driver listens for ([`EVENTS`]) has a
//!   `DevToolsProtocolEventReceived` handler, which writes `{method,
//!   params, sessionId}`.
//!
//! **Threads.** `WebView2` is single-threaded: every COM call runs on the
//! UI thread. `attach` takes the `ICoreWebView2` in Tauri's
//! `with_webview` (the UI thread) and starts a pump that hands each command
//! to the UI thread with `AppHandle::run_on_main_thread`; the COM objects
//! never leave it (they are kept in a thread-local, by pane).
//! `open_stream` is the same for any host that gives the controller on its
//! UI thread (the conformance run's plain `wry` window).
//!
//! **Unsafe.** This is one of the TWO first-party crates allowed `unsafe`
//! (AGENTS.md, owner decision on #1593): the COM calls only, each in its
//! own block with a `// SAFETY:` note. The API out is safe.
//!
//! Off Windows the crate is empty.

/// The session id the driver uses for the page itself: `WebView2`'s CDP is
/// the page's, with no browser target to attach to.
pub const PAGE_SESSION: &str = "elitea-webview2-page";

/// The CDP events the driver reads (`elitea-browser-session`'s page
/// tracker and screencast). `WebView2` delivers only events it has a
/// receiver for, so each is subscribed by name.
pub const EVENTS: &[&str] = &[
    "Page.frameStartedLoading",
    "Page.frameStoppedLoading",
    "Page.loadEventFired",
    "Page.frameNavigated",
    "Page.navigatedWithinDocument",
    "Page.javascriptDialogOpening",
    "Page.screencastFrame",
    "Network.requestWillBeSent",
    "Network.responseReceived",
    "Network.loadingFinished",
    "Network.loadingFailed",
    "Fetch.requestPaused",
    "Fetch.authRequired",
    "Target.attachedToTarget",
    "Target.detachedFromTarget",
    "Target.targetCreated",
    "Target.targetInfoChanged",
    "Target.targetDestroyed",
    // The person's input, reported by the host's input script.
    "Runtime.bindingCalled",
];

pub mod wire;

#[cfg(windows)]
mod com;

#[cfg(windows)]
pub use com::{Go, Observe, OnUiThread, attach, go, open_stream};
