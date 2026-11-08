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
    /// Whether trusted input may be posted (X11, not Wayland).
    trusted: Arc<dyn Fn() -> bool + Send + Sync>,
}

/// Take over `webview`: install the driver world. On the GTK main thread
/// (Tauri's `with_webview` runs there).
#[must_use]
pub fn attach_to(webview: &WebView) -> GtkEngine {
    let shared = Shared::default();
    install(webview, &shared);
    let x11 = gtk::gdk::Display::default()
        .is_some_and(|display| crate::trusted_input_on(display.type_().name()));
    GtkEngine {
        webview: SendWeakRef::from(webview.downgrade()),
        shared,
        trusted: Arc::new(move || x11),
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
            return Err(BrowserError::new(
                ErrorCode::Unsupported,
                "trusted input is not available on this display (Wayland)",
            ));
        }
        // The page reports this click's downs as trusted input: they are
        // the driver's.
        lock(&self.shared.posting).posting(if double { 2 } else { 1 }, Instant::now());
        self.on_main(move |webview, tx| {
            let _ = tx.send(post_click(webview, at, double));
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
/// origin), through GDK's test input on the page view's own window.
fn post_click(webview: &WebView, at: (f64, f64), double: bool) -> Result<(), BrowserError> {
    let unavailable = || BrowserError::new(ErrorCode::Internal, "the page view is not on screen");
    if !webview.is_mapped() || !webview.is_visible() {
        // The pane is hidden (another tab, a modal sheet): never click
        // whatever is under that point instead.
        return Err(unavailable());
    }
    let window = webview.window().ok_or_else(unavailable)?;
    let zoom = webview.zoom_level();
    let allocation = webview.allocation();
    let (local_x, local_y) = (at.0 * zoom, at.1 * zoom);
    if local_x < 0.0
        || local_y < 0.0
        || local_x > f64::from(allocation.width())
        || local_y > f64::from(allocation.height())
    {
        return Err(BrowserError::invalid(
            "that element is outside the visible page",
        ));
    }
    // A widget without a window of its own draws on its parent's, at its
    // allocation.
    let (origin_x, origin_y) = if webview.has_window() {
        (0, 0)
    } else {
        (allocation.x(), allocation.y())
    };
    let x = origin_x + to_pixels(local_x.max(0.0)).min(allocation.width());
    let y = origin_y + to_pixels(local_y.max(0.0)).min(allocation.height());
    let clicks = if double { 2 } else { 1 };
    for _ in 0..clicks {
        for kind in [
            gtk::gdk::EventType::ButtonPress,
            gtk::gdk::EventType::ButtonRelease,
        ] {
            let posted = gtk::gdk::test_simulate_button(
                &window,
                x,
                y,
                1,
                gtk::gdk::ModifierType::empty(),
                kind,
            );
            if !posted {
                return Err(BrowserError::new(
                    ErrorCode::Unsupported,
                    "trusted input is not available on this display",
                ));
            }
        }
    }
    Ok(())
}
