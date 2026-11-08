//! The desktop browser pane's Linux driver (ADR-0048 decision 6).
//!
//! The pane's page is a WebKitGTK `WebView` (a Tauri child webview). The
//! agent drives it with the SAME page script and [`WebDriver`] as on macOS
//! (`elitea-browser-driver-dom`); this crate is the engine under them,
//! [`GtkEngine`], in safe Rust over the `webkit2gtk` bindings:
//!
//! * **The driver world.** The page script runs ONLY in a
//!   `WebKitScriptWorld` named [`WORLD_NAME`]: installed as a user script
//!   of that world (every frame, at document start) and called with
//!   `webkit_web_view_call_async_javascript_function` in that world, with
//!   the fixed body [`CALL_BODY`] and the operation and its JSON arguments
//!   as ARGUMENTS. The page's scripts live in the page world: they cannot
//!   read the driver's globals or post to its message handlers, which are
//!   registered in the driver world only.
//! * **The person's input.** The script reports each trusted input event's
//!   type to [`INPUT_HANDLER`]; the host's callback pauses the agent.
//! * **Screenshots** are `webkit_web_view_get_snapshot` of the visible
//!   area, PNG-encoded with cairo (cut to an element's rectangle).
//! * **Trusted input** (the fallback for a click a page ignores) is XTEST
//!   fake input over a pure-Rust X connection (`x11rb`), on **X11 only**,
//!   and only when the server has the XTEST extension. Not GDK's
//!   `gdk_test_simulate_button`: it sends core button events to the window
//!   (`XSendEvent`), which GTK3, reading the pointer through XInput2,
//!   ignores. XTEST's events are device events, as a real mouse's, so they
//!   hit whatever is on screen: the driver moves the pointer to the point,
//!   and clicks only when the X window under it is the pane's toplevel or
//!   one of its children; otherwise it answers `unsupported` and the model
//!   is told to hand the page to the person. On Wayland no client may post
//!   input to another surface, so trusted input is off and the model is told
//!   to hand the page to the person (`browser_request_user`), as on macOS
//!   without the Accessibility grant.
//!
//! GTK is main-thread only: every call goes to the GLib main context with
//! `invoke` and answers on a oneshot channel. Off Linux the crate is empty.
//!
//! The frames of a page: WebKitGTK 2.40's call API has no frame argument, so
//! a cross-origin frame is out of reach of the driver (the script inlines
//! same-origin frames itself). Such a frame shows as an `iframe` node with
//! no content, and acts in it fail (`stale_ref`); see the docs page.

#[cfg(target_os = "linux")]
mod gtk_engine;
// Pure (no display): tested on every system.
#[cfg(any(target_os = "linux", test))]
#[cfg_attr(not(target_os = "linux"), allow(dead_code))]
mod x11_click;

#[cfg(target_os = "linux")]
pub use gtk_engine::{GtkEngine, attach_to};

pub use elitea_browser_driver_dom::{
    CALL_BODY, Engine, EnginePage, FRAME_HANDLER, HandOver, INPUT_HANDLER, NavigationCheck,
    NoHandOver, SCRIPT, WORLD_NAME, WebDriver, driver, input, reply,
};

/// Whether trusted input can be posted on a display of this GDK backend
/// (the `GdkDisplay` type name): X11 only (and then only with the server's
/// XTEST extension, checked at attach).
#[must_use]
pub fn trusted_input_on(display_type: &str) -> bool {
    display_type == "GdkX11Display"
}

/// The driver of a Tauri child webview (the pane's page): installs the
/// driver world in it. Call once per webview, right after it is made.
/// `person_input` runs on each trusted input of the person in the page.
///
/// # Errors
///
/// `unsupported` when Tauri could not reach the webview.
#[cfg(all(target_os = "linux", feature = "tauri"))]
pub async fn attach(
    webview: &tauri::Webview,
    check: NavigationCheck,
    hand_over: std::sync::Arc<dyn HandOver>,
    person_input: std::sync::Arc<dyn Fn() + Send + Sync>,
) -> Result<WebDriver<GtkEngine>, elitea_browser_tools::BrowserError> {
    use elitea_browser_tools::{BrowserError, ErrorCode};
    let unavailable =
        || BrowserError::new(ErrorCode::Unsupported, "the browser page is not available");
    let (tx, rx) = tokio::sync::oneshot::channel();
    webview
        .with_webview(move |platform| {
            let _ = tx.send(attach_to(&platform.inner()));
        })
        .map_err(|_| unavailable())?;
    let engine = rx.await.map_err(|_| unavailable())?;
    engine.on_person_input(person_input);
    Ok(WebDriver::new(engine, check, hand_over))
}

/// The browser buttons WebKitGTK runs itself (the page cannot override
/// them).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Go {
    Back,
    Forward,
    Stop,
}

/// Back, Forward or Stop on the pane's WebKitGTK view itself.
///
/// # Errors
///
/// Tauri could not reach the webview.
#[cfg(all(target_os = "linux", feature = "tauri"))]
pub async fn go(webview: &tauri::Webview, action: Go) -> Result<(), String> {
    use webkit2gtk::WebViewExt as _;
    let (done, result) = tokio::sync::oneshot::channel();
    webview
        .with_webview(move |platform| {
            let view = platform.inner();
            match action {
                Go::Back => view.go_back(),
                Go::Forward => view.go_forward(),
                Go::Stop => view.stop_loading(),
            }
            let _ = done.send(());
        })
        .map_err(|error| error.to_string())?;
    result.await.map_err(|_| "the page view is gone".to_owned())
}

#[cfg(test)]
mod tests {
    use super::trusted_input_on;

    #[test]
    fn trusted_input_is_on_x11_only() {
        assert!(trusted_input_on("GdkX11Display"));
        for other in ["GdkWaylandDisplay", "GdkBroadwayDisplay", ""] {
            assert!(!trusted_input_on(other), "{other}");
        }
    }
}
