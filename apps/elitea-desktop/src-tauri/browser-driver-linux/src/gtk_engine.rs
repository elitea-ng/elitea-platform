//! [`GtkEngine`]: the driver's engine over one WebKitGTK `WebView`.

use std::sync::{Arc, Mutex, MutexGuard, PoisonError};
use std::time::Instant;

use async_trait::async_trait;
use elitea_browser_driver_dom::driver::{Engine, EnginePage, FrameId, TOP};
use elitea_browser_driver_dom::input::{OwnInput, REPORTED};
use elitea_browser_driver_dom::reply::{self, RawRect};
use elitea_browser_driver_dom::{
    CALL_BODY, FRAME_HANDLER, INPUT_HANDLER, MISSING, SCRIPT, WORLD_NAME,
};
use elitea_browser_tools::driver::{HistoryStep, Screenshot};
use elitea_browser_tools::{BrowserError, ErrorCode};
use gtk::prelude::*;
use javascriptcore::ValueExt as _;
use serde_json::Value;
use tokio::sync::oneshot;
use webkit2gtk::glib::{self, SendWeakRef, ToVariant, VariantDict};
use webkit2gtk::{
    SnapshotOptions, SnapshotRegion, UserContentInjectedFrames, UserContentManagerExt as _,
    UserScript, UserScriptInjectionTime, WebView, WebViewExt as _,
};
use x11rb::connection::{Connection as _, RequestConnection as _};
use x11rb::errors::ReplyError;
use x11rb::protocol::xproto::{
    BUTTON_PRESS_EVENT, BUTTON_RELEASE_EVENT, ConnectionExt as _, MOTION_NOTIFY_EVENT,
};
use x11rb::protocol::xtest::{self, ConnectionExt as _};
use x11rb::rust_connection::RustConnection;
use x11rb::wrapper::ConnectionExt as _;

use crate::x11_click::{MAX_DEPTH, NO_WINDOW, ViewPlace, over_the_pane, root_point};

/// Trusted input cannot be posted here: the person does the step.
const NO_TRUSTED_INPUT: &str = "trusted input is not available on this display: ask the person to click it with browser_request_user";

/// The pointer is over another window than the pane's: never click it.
const NOT_THE_PANE: &str = "another window covers the page at that point: ask the person to click it with browser_request_user";

/// The host's callback for the person's input.
type InputCallback = Arc<dyn Fn() + Send + Sync>;

/// What the message handler shares with the engine.
#[derive(Clone, Default)]
struct Shared {
    input: Arc<Mutex<Option<InputCallback>>>,
    /// The downs of the driver's own trusted clicks, not yet reported.
    posting: Arc<Mutex<OwnInput>>,
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

impl Shared {
    /// The person used the page (an input of type `kind`), unless it is
    /// the report of the driver's own click.
    fn person_input(&self, kind: &str) {
        if !REPORTED.contains(&kind) || lock(&self.posting).is_own(kind, Instant::now()) {
            return;
        }
        let callback = lock(&self.input).clone();
        if let Some(callback) = callback {
            callback();
        }
    }
}

/// The driver's engine over one WebKitGTK `WebView`.
pub struct GtkEngine {
    /// Upgraded on the GTK main thread only (where it was made).
    webview: SendWeakRef<WebView>,
    shared: Shared,
    /// Whether trusted input may be posted (X11 with XTEST, not Wayland).
    trusted: Arc<dyn Fn() -> bool + Send + Sync>,
    /// The one X connection trusted clicks go through (made at attach,
    /// made again at the next click after it failed).
    x11: Arc<Mutex<Option<Xtest>>>,
}

/// An X connection to the pane's display with the XTEST extension.
struct Xtest {
    connection: RustConnection,
    /// The root window of the connection's screen.
    root: u32,
}

impl Xtest {
    /// Connect to `display` (a GDK display name, `:99`); `None` when there
    /// is no X server there or it has no XTEST.
    fn connect(display: &str) -> Option<Self> {
        let name = (!display.is_empty()).then_some(display);
        let (connection, screen) = RustConnection::connect(name).ok()?;
        let _xtest = connection
            .extension_information(xtest::X11_EXTENSION_NAME)
            .ok()??;
        let root = connection.setup().roots.get(screen)?.root;
        Some(Self { connection, root })
    }

    /// Move the pointer to `at` (root pixels) with XTEST, then whether it is
    /// there and over `toplevel` or one of its children: the windows under
    /// the pointer, from the root's child down (QueryPointer's `child`,
    /// followed), must hold `toplevel`.
    fn warp_over(&self, at: (i16, i16), toplevel: u32) -> Result<bool, ReplyError> {
        let connection = &self.connection;
        connection.xtest_fake_input(
            MOTION_NOTIFY_EVENT,
            0, // absolute
            x11rb::CURRENT_TIME,
            self.root,
            at.0,
            at.1,
            0,
        )?;
        connection.sync()?;
        let pointer = connection.query_pointer(self.root)?.reply()?;
        if !pointer.same_screen || (pointer.root_x, pointer.root_y) != at {
            return Ok(false);
        }
        let mut chain = Vec::new();
        let mut child = pointer.child;
        while child != NO_WINDOW && chain.len() < MAX_DEPTH {
            chain.push(child);
            child = connection.query_pointer(child)?.reply()?.child;
        }
        Ok(over_the_pane(&chain, toplevel))
    }

    /// Press and release button 1 `clicks` times where the pointer is.
    fn click(&self, clicks: usize) -> Result<(), ReplyError> {
        for _ in 0..clicks {
            for kind in [BUTTON_PRESS_EVENT, BUTTON_RELEASE_EVENT] {
                self.connection.xtest_fake_input(
                    kind,
                    1,
                    x11rb::CURRENT_TIME,
                    self.root,
                    0,
                    0,
                    0,
                )?;
            }
        }
        self.connection.sync()
    }
}

/// Take over `webview`: install the driver world. On the GTK main thread
/// (Tauri's `with_webview` runs there).
#[must_use]
pub fn attach_to(webview: &WebView) -> GtkEngine {
    let shared = Shared::default();
    install(webview, &shared);
    // Trusted input needs X11 (not Wayland) and the server's XTEST: one
    // local connection, kept for the clicks.
    let xtest = gtk::gdk::Display::default()
        .filter(|display| crate::trusted_input_on(display.type_().name()))
        .and_then(|display| Xtest::connect(&display.name()));
    let trusted = xtest.is_some();
    GtkEngine {
        webview: SendWeakRef::from(webview.downgrade()),
        shared,
        trusted: Arc::new(move || trusted),
        x11: Arc::new(Mutex::new(xtest)),
    }
}

/// The driver world in `webview`'s user content manager (wry gives each
/// webview its own): the script for every frame at document start, and the
/// two message handlers, in the driver world only.
fn install(webview: &WebView, shared: &Shared) {
    let Some(manager) = webview.user_content_manager() else {
        return;
    };
    let script = UserScript::for_world(
        SCRIPT,
        UserContentInjectedFrames::AllFrames,
        UserScriptInjectionTime::Start,
        WORLD_NAME,
        &[],
        &[],
    );
    manager.add_script(&script);
    manager.register_script_message_handler_in_world(INPUT_HANDLER, WORLD_NAME);
    // The frame announcements have no use here (the call API cannot reach a
    // frame), but the handler is there so the script's post succeeds.
    manager.register_script_message_handler_in_world(FRAME_HANDLER, WORLD_NAME);
    let shared = shared.clone();
    manager.connect_script_message_received(Some(INPUT_HANDLER), move |_, result| {
        if let Some(value) = result.js_value() {
            shared.person_input(&value.to_str());
        }
    });
}

impl GtkEngine {
    /// The person used the page (a trusted click, key, scroll or touch):
    /// `callback` runs on the GTK main thread, once per report.
    pub fn on_person_input(&self, callback: Arc<dyn Fn() + Send + Sync>) {
        *lock(&self.shared.input) = Some(callback);
    }

    /// Replace the trusted-input check (tests).
    #[must_use]
    pub fn with_trusted_input(mut self, trusted: Arc<dyn Fn() -> bool + Send + Sync>) -> Self {
        self.trusted = trusted;
        self
    }

    /// Run `work` with the webview on the GTK main thread; its answer.
    async fn on_main<T, F>(&self, work: F) -> Result<T, BrowserError>
    where
        T: Send + 'static,
        F: FnOnce(&WebView, oneshot::Sender<T>) + Send + 'static,
    {
        let (tx, rx) = oneshot::channel();
        let webview = self.webview.clone();
        glib::MainContext::default().invoke(move || {
            if let Some(webview) = webview.upgrade() {
                work(&webview, tx);
            }
        });
        rx.await
            .map_err(|_| BrowserError::new(ErrorCode::Internal, "the page view is gone"))
    }

    /// One call; `None` when the document has no driver yet.
    async fn call_once(&self, op: &str, args: &str) -> Result<Option<Value>, BrowserError> {
        let (op, args) = (op.to_owned(), args.to_owned());
        self.on_main(move |webview, tx| {
            let arguments = VariantDict::new(None);
            arguments.insert_value("op", &op.to_variant());
            arguments.insert_value("args", &args.to_variant());
            arguments.insert_value("MISSING", &MISSING.to_variant());
            webview.call_async_javascript_function(
                CALL_BODY,
                Some(&arguments.end()),
                Some(WORLD_NAME),
                None,
                None::<&webkit2gtk::gio::Cancellable>,
                move |result| {
                    let _ = tx.send(match result {
                        Ok(value) => {
                            let text = value.to_str();
                            if text == MISSING {
                                Ok(None)
                            } else {
                                reply::parse(&text).map(Some)
                            }
                        }
                        Err(error) => Err(gerror(&error, "the page script did not answer")),
                    });
                },
            );
        })
        .await?
    }

    /// Evaluate the driver script (the constant [`SCRIPT`], never anything
    /// else) in the top document, in the driver world.
    async fn inject(&self) -> Result<(), BrowserError> {
        self.on_main(|webview, tx| {
            webview.evaluate_javascript(
                SCRIPT,
                Some(WORLD_NAME),
                None,
                None::<&webkit2gtk::gio::Cancellable>,
                move |result| {
                    let _ = tx.send(
                        result
                            .map(|_| ())
                            .map_err(|error| gerror(&error, "the driver could not start")),
                    );
                },
            );
        })
        .await?
    }
}

/// A WebKit error as `internal`, cut short: its message can name page
/// content (an exception text).
fn gerror(error: &glib::Error, fallback: &str) -> BrowserError {
    let text = error.message();
    let text = if text.is_empty() { fallback } else { text };
    BrowserError::new(
        ErrorCode::Internal,
        text.chars().take(200).collect::<String>(),
    )
}

fn only_top(frame: FrameId) -> Result<(), BrowserError> {
    if frame == TOP {
        Ok(())
    } else {
        Err(BrowserError::stale_ref(
            "that frame is out of reach on this system: snapshot again",
        ))
    }
}

#[async_trait]
impl Engine for GtkEngine {
    async fn call(&self, frame: FrameId, op: &str, args: Value) -> Result<Value, BrowserError> {
        only_top(frame)?;
        let args = args.to_string();
        if let Some(answer) = self.call_once(op, &args).await? {
            return Ok(answer);
        }
        // A document from before the user script was installed: put the
        // driver in it (in the driver world), then call again.
        self.inject().await?;
        self.call_once(op, &args).await?.ok_or_else(|| {
            BrowserError::new(
                ErrorCode::Internal,
                "the driver could not start in this page",
            )
        })
    }

    async fn frames(&self) -> Vec<(FrameId, String)> {
        Vec::new()
    }

    async fn load(&self, url: &str) -> Result<(), BrowserError> {
        let url = url.to_owned();
        self.on_main(move |webview, tx| {
            webview.load_uri(&url);
            let _ = tx.send(());
        })
        .await
    }

    async fn history(&self, step: HistoryStep) -> Result<(), BrowserError> {
        self.on_main(move |webview, tx| {
            let _ = tx.send(match step {
                HistoryStep::Back if webview.can_go_back() => {
                    webview.go_back();
                    Ok(())
                }
                HistoryStep::Back => Err(BrowserError::invalid("there is no page to go back to")),
                HistoryStep::Forward if webview.can_go_forward() => {
                    webview.go_forward();
                    Ok(())
                }
                HistoryStep::Forward => {
                    Err(BrowserError::invalid("there is no page to go forward to"))
                }
                HistoryStep::Reload => {
                    webview.reload();
                    Ok(())
                }
            });
        })
        .await?
    }

    async fn page(&self) -> Result<EnginePage, BrowserError> {
        self.on_main(|webview, tx| {
            let _ = tx.send(EnginePage {
                url: webview
                    .uri()
                    .map_or_else(|| "about:blank".to_owned(), |uri| uri.to_string()),
                title: webview.title().map(|t| t.to_string()).unwrap_or_default(),
                loading: webview.is_loading(),
            });
        })
        .await
    }

    async fn screenshot(&self, rect: Option<RawRect>) -> Result<Screenshot, BrowserError> {
        self.on_main(move |webview, tx| {
            let zoom = webview.zoom_level();
            webview.snapshot(
                SnapshotRegion::Visible,
                SnapshotOptions::NONE,
                None::<&webkit2gtk::gio::Cancellable>,
                move |result| {
                    let _ = tx.send(match result {
                        Ok(surface) => png(&surface, rect, zoom),
                        Err(error) => Err(gerror(&error, "the page could not be captured")),
                    });
                },
            );
        })
        .await?
    }

    fn trusted_input(&self) -> bool {
        (self.trusted)()
    }

    async fn trusted_click(&self, at: (f64, f64), double: bool) -> Result<(), BrowserError> {
        if !self.trusted_input() {
            return Err(BrowserError::new(ErrorCode::Unsupported, NO_TRUSTED_INPUT));
        }
        // The page reports this click's downs as trusted input: they are
        // the driver's.
        lock(&self.shared.posting).posting(if double { 2 } else { 1 }, Instant::now());
        let x11 = self.x11.clone();
        self.on_main(move |webview, tx| {
            let _ = tx.send(post_click(webview, at, double, &x11));
        })
        .await?
    }
}

/// The capture (device pixels) as PNG, cut to `rect` (CSS pixels of the top
/// viewport) when given.
fn png(
    surface: &cairo::Surface,
    rect: Option<RawRect>,
    zoom: f64,
) -> Result<Screenshot, BrowserError> {
    let fail = || BrowserError::new(ErrorCode::Internal, "the capture could not be encoded");
    let (scale, _) = surface.device_scale();
    let scale = if scale > 0.0 { scale } else { 1.0 };
    // From here on every coordinate is a device pixel.
    surface.set_device_scale(1.0, 1.0);
    let image = cairo::ImageSurface::try_from(surface.clone()).map_err(|_| fail())?;
    let (full_width, full_height) = (image.width(), image.height());
    let pixels = |css: f64| css * zoom * scale;
    let (x, y, width, height) = match rect {
        None => (0.0, 0.0, f64::from(full_width), f64::from(full_height)),
        Some(rect) => {
            let x = pixels(rect.x).clamp(0.0, f64::from(full_width));
            let y = pixels(rect.y).clamp(0.0, f64::from(full_height));
            let width = pixels(rect.width.max(1.0)).min(f64::from(full_width) - x);
            let height = pixels(rect.height.max(1.0)).min(f64::from(full_height) - y);
            (x, y, width.max(1.0), height.max(1.0))
        }
    };
    let (width, height) = (to_pixels(width), to_pixels(height));
    let cut =
        cairo::ImageSurface::create(cairo::Format::ARgb32, width, height).map_err(|_| fail())?;
    {
        let context = cairo::Context::new(&cut).map_err(|_| fail())?;
        context
            .set_source_surface(&image, -x.floor(), -y.floor())
            .map_err(|_| fail())?;
        context.paint().map_err(|_| fail())?;
    }
    let mut data = Vec::new();
    cut.write_to_png(&mut data).map_err(|_| fail())?;
    Ok(Screenshot {
        media_type: "image/png",
        data,
        width: u32::try_from(width).unwrap_or(0),
        height: u32::try_from(height).unwrap_or(0),
    })
}

/// A size in device pixels, at least one.
fn to_pixels(value: f64) -> i32 {
    let rounded = value.round().clamp(1.0, f64::from(i32::MAX));
    // In range by the clamp above.
    #[allow(clippy::cast_possible_truncation)]
    let pixels = rounded as i32;
    pixels
}

/// A real left click at `at` (CSS pixels of the page view, top-left
/// origin), with XTEST fake input on the pane's X display.
///
/// Not GDK's `gdk_test_simulate_button`: on X11 it sends core button events
/// to the window (`XSendEvent`), and GTK3 reads the pointer through
/// XInput2, so WebKit never sees them. XTEST's events are the server's own
/// device events, as a real mouse's. They go to whatever is on screen at
/// that point, so the pointer is moved there first and the click is made
/// only when the window under it is the pane's toplevel or one of its
/// children (never another application's window on top). The pointer is
/// left where it is.
fn post_click(
    webview: &WebView,
    at: (f64, f64),
    double: bool,
    x11: &Mutex<Option<Xtest>>,
) -> Result<(), BrowserError> {
    let unavailable = || BrowserError::new(ErrorCode::Internal, "the page view is not on screen");
    let no_trusted_input = || BrowserError::new(ErrorCode::Unsupported, NO_TRUSTED_INPUT);
    if !webview.is_mapped() || !webview.is_visible() {
        // The pane is hidden (another tab, a modal sheet): never click
        // whatever is under that point instead.
        return Err(unavailable());
    }
    let window = webview.window().ok_or_else(unavailable)?;
    let zoom = webview.zoom_level();
    let allocation = webview.allocation();
    let (_, origin_x, origin_y) = window.origin();
    let place = ViewPlace {
        origin: (origin_x, origin_y),
        // A widget without a window of its own draws on its parent's, at
        // its allocation.
        offset: if webview.has_window() {
            (0, 0)
        } else {
            (allocation.x(), allocation.y())
        },
        size: (allocation.width(), allocation.height()),
        scale: window.scale_factor(),
    };
    let point = root_point((at.0 * zoom, at.1 * zoom), place)
        .ok_or_else(|| BrowserError::invalid("that element is outside the visible page"))?;
    let toplevel = window
        .toplevel()
        .downcast::<gdkx11::X11Window>()
        .ok()
        .and_then(|toplevel| u32::try_from(toplevel.xid()).ok())
        .ok_or_else(no_trusted_input)?;
    let mut x11 = lock(x11);
    if x11.is_none() {
        *x11 = Xtest::connect(&webview.display().name());
    }
    let xtest = x11.as_ref().ok_or_else(no_trusted_input)?;
    let over = match xtest.warp_over(point, toplevel) {
        Ok(over) => over,
        Err(_) => {
            // Made again at the next click.
            *x11 = None;
            return Err(no_trusted_input());
        }
    };
    if !over {
        return Err(BrowserError::new(ErrorCode::Unsupported, NOT_THE_PANE));
    }
    if xtest.click(if double { 2 } else { 1 }).is_err() {
        *x11 = None;
        return Err(no_trusted_input());
    }
    Ok(())
}
