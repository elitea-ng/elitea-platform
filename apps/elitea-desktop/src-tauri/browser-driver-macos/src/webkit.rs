//! The WebKit half of the driver (macOS): [`WebKitEngine`].
//!
//! **The driver world.** [`install`] adds the page script ([`SCRIPT`])
//! to the webview's user content controller as a user script in a PRIVATE
//! `WKContentWorld` ([`WORLD_NAME`]), at document start, in every frame,
//! and a script message handler ([`FRAME_HANDLER`]) in that same world only.
//! A content world has its own JavaScript globals over the shared DOM: the
//! page's scripts cannot read the driver's globals, call its functions,
//! replace the built-ins it captured, or post to its handler (the page
//! world's `window.webkit.messageHandlers` has no such name). Every call is
//! `callAsyncJavaScript(_:arguments:in:in:)` in that world, with a fixed
//! function body ([`CALL_BODY`]) and the operation and its JSON arguments
//! as ARGUMENTS: no source text is ever built from the model's input.
//!
//! **The person's input.** The script reports the type of each trusted
//! pointer, key, wheel, touch, text input, paste or drop event of the page
//! to [`INPUT_HANDLER`] (same world only), and the engine calls the host's
//! [`WebKitEngine::on_person_input`] callback: the control pill's take-over
//! (ADR-0048 decision 4). The downs of a click the driver itself posts as
//! trusted input are not the person's ([`elitea_browser_driver_dom::input::OwnInput`]).
//!
//! **Frames.** The script announces each subframe to [`FRAME_HANDLER`];
//! the host keeps the `WKFrameInfo` it came with, and calls into a cross-
//! origin frame with it. A same-origin frame is read by its parent's walk.
//!
//! **Screenshots** are `takeSnapshot(with:)`, PNG-encoded.
//!
//! **Trusted input** is a `CGEvent` mouse click at the element's point of
//! the page view, posted to this process only (`CGEventPostToPid`), and
//! only while `AXIsProcessTrusted()` (the ADR-0047 grant) holds.
//!
//! WebKit is main-thread only: every call hops to the main queue and
//! answers on a oneshot channel. Nothing here blocks the main thread.

use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use block2::RcBlock;
use dispatch2::{DispatchQueue, MainThreadBound};
use elitea_browser_tools::driver::{HistoryStep, Screenshot};
use elitea_browser_tools::{BrowserError, ErrorCode};
use objc2::rc::Retained;
use objc2::runtime::{AnyObject, ProtocolObject};
use objc2::{DefinedClass as _, MainThreadMarker, MainThreadOnly, define_class, msg_send};
use objc2_app_kit::{NSBitmapImageFileType, NSBitmapImageRep, NSImage, NSView};
use objc2_core_foundation::{CGPoint, CGRect, CGSize};
use objc2_core_graphics::{CGEvent, CGEventField, CGEventType, CGMouseButton};
use objc2_foundation::{
    NSDictionary, NSError, NSObject, NSObjectProtocol, NSString, NSURL, NSURLRequest,
};
use objc2_web_kit::{
    WKContentWorld, WKFrameInfo, WKScriptMessage, WKScriptMessageHandler, WKSnapshotConfiguration,
    WKUserContentController, WKUserScript, WKUserScriptInjectionTime, WKWebView,
};
use serde_json::Value;
use tokio::sync::oneshot;

use elitea_browser_driver_dom::driver::{Engine, EnginePage, FrameId, TOP};
use elitea_browser_driver_dom::reply::{self, RawRect};
pub use elitea_browser_driver_dom::{CALL_BODY, FRAME_HANDLER, INPUT_HANDLER, WORLD_NAME};
use elitea_browser_driver_dom::{MISSING, SCRIPT};

/// Frames remembered per page (a page that makes frames without end).
const MAX_FRAMES: usize = 64;

struct Frames {
    next: FrameId,
    known: Vec<(FrameId, String, MainThreadBound<Retained<WKFrameInfo>>)>,
}

impl Frames {
    fn add(&mut self, url: String, info: Retained<WKFrameInfo>, mtm: MainThreadMarker) {
        self.next += 1;
        self.known
            .push((self.next, url, MainThreadBound::new(info, mtm)));
        if self.known.len() > MAX_FRAMES {
            self.known.remove(0);
        }
    }
}

/// The host's callback for the person's input.
type InputCallback = Arc<dyn Fn() + Send + Sync>;

/// What the handler shares with the engine.
#[derive(Clone)]
struct Shared {
    frames: Arc<Mutex<Frames>>,
    input: Arc<Mutex<Option<InputCallback>>>,
    /// The downs of the driver's own trusted clicks, not yet reported.
    posting: Arc<Mutex<elitea_browser_driver_dom::input::OwnInput>>,
}

impl Shared {
    fn lock(&self) -> std::sync::MutexGuard<'_, Frames> {
        self.frames
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// The person used the page (an input of type `kind`), unless it is
    /// the report of the driver's own click.
    fn person_input(&self, kind: &str) {
        if !elitea_browser_driver_dom::input::REPORTED.contains(&kind) {
            return;
        }
        let own = self
            .posting
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .is_own(kind, std::time::Instant::now());
        if own {
            return;
        }
        let callback = self
            .input
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .clone();
        if let Some(callback) = callback {
            callback();
        }
    }
}

struct HandlerIvars {
    shared: Shared,
}

define_class!(
    // SAFETY: NSObject has no subclassing requirements; the class adds no
    // Drop-sensitive state beyond its ivars.
    #[unsafe(super(NSObject))]
    #[thread_kind = MainThreadOnly]
    #[ivars = HandlerIvars]
    struct FrameHandler;

    // SAFETY: NSObjectProtocol has no extra requirements.
    unsafe impl NSObjectProtocol for FrameHandler {}

    // SAFETY: the method signature is WKScriptMessageHandler's.
    unsafe impl WKScriptMessageHandler for FrameHandler {
        #[unsafe(method(userContentController:didReceiveScriptMessage:))]
        fn did_receive(&self, _controller: &WKUserContentController, message: &WKScriptMessage) {
            self.received(message);
        }
    }
);

impl FrameHandler {
    #[allow(unsafe_code)]
    fn new(shared: Shared, mtm: MainThreadMarker) -> Retained<Self> {
        let this = Self::alloc(mtm).set_ivars(HandlerIvars { shared });
        // SAFETY: `this` is a freshly allocated, ivar-initialised instance
        // of a direct NSObject subclass; NSObject's `init` takes no
        // arguments and returns the same, now initialised, object.
        unsafe { msg_send![super(this), init] }
    }

    #[allow(unsafe_code)]
    fn received(&self, message: &WKScriptMessage) {
        let Some(mtm) = MainThreadMarker::new() else {
            return;
        };
        // SAFETY: on the main thread (`mtm` above); `message` is the live
        // message WebKit passed in, and these getters take no arguments and
        // return retained, non-null objects.
        let (world, frame, body, name) = unsafe {
            (
                message.world(),
                message.frameInfo(),
                message.body(),
                message.name(),
            )
        };
        // SAFETY: on the main thread; `world` is a retained WKContentWorld
        // and `name` returns a retained NSString or nil.
        let from_driver =
            unsafe { world.name() }.is_some_and(|name| name.to_string() == WORLD_NAME);
        if !from_driver {
            return;
        }
        if name.to_string() == INPUT_HANDLER {
            if let Ok(kind) = body.downcast::<NSString>() {
                self.ivars().shared.person_input(&kind.to_string());
            }
            return;
        }
        // SAFETY: on the main thread; `frame` is a retained WKFrameInfo and
        // `isMainFrame` is a plain BOOL getter.
        if unsafe { frame.isMainFrame() } {
            return;
        }
        let Ok(text) = body.downcast::<NSString>() else {
            return;
        };
        let url = serde_json::from_str::<Value>(&text.to_string())
            .ok()
            .and_then(|value| value.get("url").and_then(Value::as_str).map(str::to_owned))
            .unwrap_or_default();
        self.ivars().shared.lock().add(url, frame, mtm);
    }
}

/// Install the driver world in `webview`'s content controller. Idempotent
/// per controller: call it once, when the pane's webview is made, before
/// its first page loads.
///
/// # Safety
///
/// `webview` must be a live `WKWebView` and this must run on the main
/// thread.
#[allow(unsafe_code)]
unsafe fn install(webview: &WKWebView, mtm: MainThreadMarker) -> Shared {
    let shared = Shared {
        frames: Arc::new(Mutex::new(Frames {
            next: TOP,
            known: Vec::new(),
        })),
        input: Arc::default(),
        posting: Arc::default(),
    };
    // SAFETY: the caller guarantees a live WKWebView on the main thread
    // (`mtm`). Every argument is a live, retained object of the declared
    // class (the NSStrings, the world, the user script, the handler, which
    // implements WKScriptMessageHandler); the controller retains what it is
    // given, so nothing here must outlive this block.
    unsafe {
        let world = WKContentWorld::worldWithName(&NSString::from_str(WORLD_NAME), mtm);
        let controller = webview.configuration().userContentController();
        let script = WKUserScript::initWithSource_injectionTime_forMainFrameOnly_inContentWorld(
            WKUserScript::alloc(mtm),
            &NSString::from_str(SCRIPT),
            WKUserScriptInjectionTime::AtDocumentStart,
            false,
            &world,
        );
        controller.addUserScript(&script);
        let handler = FrameHandler::new(shared.clone(), mtm);
        controller.addScriptMessageHandler_contentWorld_name(
            ProtocolObject::from_ref(&*handler),
            &world,
            &NSString::from_str(FRAME_HANDLER),
        );
        controller.addScriptMessageHandler_contentWorld_name(
            ProtocolObject::from_ref(&*handler),
            &world,
            &NSString::from_str(INPUT_HANDLER),
        );
    }
    shared
}

/// The driver's engine over one `WKWebView`.
pub struct WebKitEngine {
    webview: Arc<MainThreadBound<Retained<WKWebView>>>,
    shared: Shared,
    /// Whether trusted input may be posted (the Accessibility grant).
    trusted: Arc<dyn Fn() -> bool + Send + Sync>,
}

impl WebKitEngine {
    /// Take over `webview`: install the driver world. On the main thread.
    #[must_use]
    #[allow(unsafe_code)]
    pub fn new(webview: Retained<WKWebView>, mtm: MainThreadMarker) -> Self {
        // SAFETY: `install`'s contract: `webview` is retained (so live) and
        // `mtm` proves the main thread.
        let shared = unsafe { install(&webview, mtm) };
        Self {
            webview: Arc::new(MainThreadBound::new(webview, mtm)),
            shared,
            trusted: Arc::new(accessibility_granted),
        }
    }

    /// The person used the page (a trusted click, key, scroll or touch):
    /// `callback` runs on the main thread, once per report.
    pub fn on_person_input(&self, callback: Arc<dyn Fn() + Send + Sync>) {
        *self
            .shared
            .input
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = Some(callback);
    }

    /// Replace the grant check (tests: a page that needs trusted input).
    #[must_use]
    pub fn with_trusted_input(mut self, trusted: Arc<dyn Fn() -> bool + Send + Sync>) -> Self {
        self.trusted = trusted;
        self
    }

    /// Run `work` with the webview on the main thread; its answer.
    async fn on_main<T, F>(&self, work: F) -> Result<T, BrowserError>
    where
        T: Send + 'static,
        F: FnOnce(&WKWebView, MainThreadMarker, oneshot::Sender<T>) + Send + 'static,
    {
        let (tx, rx) = oneshot::channel();
        let webview = self.webview.clone();
        DispatchQueue::main().exec_async(move || {
            let Some(mtm) = MainThreadMarker::new() else {
                return;
            };
            work(webview.get(mtm), mtm, tx);
        });
        rx.await
            .map_err(|_| BrowserError::new(ErrorCode::Internal, "the page view is gone"))
    }
}

impl WebKitEngine {
    /// One call; `None` when the frame's document has no driver yet.
    #[allow(unsafe_code)]
    async fn call_once(
        &self,
        frame: FrameId,
        op: &str,
        args: &str,
    ) -> Result<Option<Value>, BrowserError> {
        let op = op.to_owned();
        let args = args.to_owned();
        let shared = self.shared.clone();
        self.on_main(move |webview, mtm, tx| {
            let info = match frame_info(&shared, frame, mtm) {
                Ok(info) => info,
                Err(error) => {
                    let _ = tx.send(Err(error));
                    return;
                }
            };
            let tx = Mutex::new(Some(tx));
            let done = RcBlock::new(move |result: *mut AnyObject, error: *mut NSError| {
                let Some(tx) = tx.lock().ok().and_then(|mut tx| tx.take()) else {
                    return;
                };
                // SAFETY: WebKit calls the handler with a valid object (alive
                // for the call) or null, which `as_ref` maps to `None`; the
                // reference does not escape the call.
                let text = unsafe { result.as_ref() }
                    .and_then(|object| object.downcast_ref::<NSString>())
                    .map(NSString::to_string);
                let _ = tx.send(match text {
                    Some(text) if text == MISSING => Ok(None),
                    Some(text) => reply::parse(&text).map(Some),
                    None => Err(ns_error(error, "the page script did not answer")),
                });
            });
            let keys = [
                &*NSString::from_str("op"),
                &*NSString::from_str("args"),
                &*NSString::from_str("MISSING"),
            ];
            let op = NSString::from_str(&op);
            let args = NSString::from_str(&args);
            let missing = NSString::from_str(MISSING);
            let values: [&AnyObject; 3] = [op.as_ref(), args.as_ref(), missing.as_ref()];
            let arguments = NSDictionary::<NSString, AnyObject>::from_slices(&keys, &values);
            // SAFETY: on the main thread (`mtm`), with the retained webview.
            // The arguments are an NSDictionary of NSString keys to NSString
            // values (JSON-serialisable, as `callAsyncJavaScript` requires);
            // `info` is a retained WKFrameInfo or None (the top frame); the
            // completion block has the declared `(id, NSError *)` signature
            // and WebKit copies it.
            unsafe {
                let world = WKContentWorld::worldWithName(&NSString::from_str(WORLD_NAME), mtm);
                webview.callAsyncJavaScript_arguments_inFrame_inContentWorld_completionHandler(
                    &NSString::from_str(CALL_BODY),
                    Some(&arguments),
                    info.as_deref(),
                    &world,
                    Some(&*done),
                );
            }
        })
        .await?
    }

    /// Evaluate the driver script (the constant [`SCRIPT`], never
    /// anything else) in `frame`, in the driver world.
    #[allow(unsafe_code)]
    async fn inject(&self, frame: FrameId) -> Result<(), BrowserError> {
        let shared = self.shared.clone();
        self.on_main(move |webview, mtm, tx| {
            let info = match frame_info(&shared, frame, mtm) {
                Ok(info) => info,
                Err(error) => {
                    let _ = tx.send(Err(error));
                    return;
                }
            };
            let tx = Mutex::new(Some(tx));
            let done = RcBlock::new(move |_result: *mut AnyObject, error: *mut NSError| {
                if let Some(tx) = tx.lock().ok().and_then(|mut tx| tx.take()) {
                    let _ = tx.send(if error.is_null() {
                        Ok(())
                    } else {
                        Err(ns_error(error, "the driver could not start in this page"))
                    });
                }
            });
            // SAFETY: on the main thread (`mtm`), with the retained webview;
            // the source is the constant driver script, `info` a retained
            // WKFrameInfo or None, and the block has the declared
            // `(id, NSError *)` signature (WebKit copies it).
            unsafe {
                let world = WKContentWorld::worldWithName(&NSString::from_str(WORLD_NAME), mtm);
                webview.evaluateJavaScript_inFrame_inContentWorld_completionHandler(
                    &NSString::from_str(SCRIPT),
                    info.as_deref(),
                    &world,
                    Some(&*done),
                );
            }
        })
        .await?
    }
}

/// The frame's `WKFrameInfo` (`None`: the top document).
fn frame_info(
    shared: &Shared,
    frame: FrameId,
    mtm: MainThreadMarker,
) -> Result<Option<Retained<WKFrameInfo>>, BrowserError> {
    if frame == TOP {
        return Ok(None);
    }
    let frames = shared.lock();
    frames
        .known
        .iter()
        .find(|(id, ..)| *id == frame)
        .map(|(_, _, info)| Some(info.get(mtm).clone()))
        .ok_or_else(|| BrowserError::stale_ref("that frame is gone: snapshot again"))
}

/// Whether the person gave Elitea the macOS Accessibility grant (the
/// ADR-0047 permission that trusted input needs).
#[must_use]
#[allow(unsafe_code)]
pub fn accessibility_granted() -> bool {
    #[link(name = "ApplicationServices", kind = "framework")]
    unsafe extern "C" {
        fn AXIsProcessTrusted() -> bool;
    }
    // SAFETY: `AXIsProcessTrusted` (ApplicationServices) takes no
    // arguments, has no preconditions and returns a C `Boolean`, which is
    // ABI-compatible with `bool` (0 or 1).
    unsafe { AXIsProcessTrusted() }
}

#[allow(unsafe_code)]
fn ns_error(error: *mut NSError, fallback: &str) -> BrowserError {
    // SAFETY: every caller passes the `NSError *` WebKit gave its completion
    // handler, valid for the handler's call or null (`as_ref`: `None`); the
    // reference does not escape this function.
    let text = unsafe { error.as_ref() }.map_or_else(
        || fallback.to_owned(),
        |error| error.localizedDescription().to_string(),
    );
    // A WebKit error message can name page content (an exception text):
    // it goes to the logs' level of detail, cut short.
    BrowserError::new(
        ErrorCode::Internal,
        text.chars().take(200).collect::<String>(),
    )
}

#[async_trait]
impl Engine for WebKitEngine {
    async fn call(&self, frame: FrameId, op: &str, args: Value) -> Result<Value, BrowserError> {
        let args = args.to_string();
        if let Some(answer) = self.call_once(frame, op, &args).await? {
            return Ok(answer);
        }
        // A document from before the user script was installed: put the
        // driver in it (in the driver world), then call again.
        self.inject(frame).await?;
        self.call_once(frame, op, &args).await?.ok_or_else(|| {
            BrowserError::new(
                ErrorCode::Internal,
                "the driver could not start in this page",
            )
        })
    }

    async fn frames(&self) -> Vec<(FrameId, String)> {
        self.shared
            .lock()
            .known
            .iter()
            .map(|(id, url, _)| (*id, url.clone()))
            .collect()
    }

    #[allow(unsafe_code)]
    async fn load(&self, url: &str) -> Result<(), BrowserError> {
        let url = url.to_owned();
        let shared = self.shared.clone();
        self.on_main(move |webview, _mtm, tx| {
            shared.lock().known.clear();
            let Some(nsurl) = NSURL::URLWithString(&NSString::from_str(&url)) else {
                let _ = tx.send(Err(BrowserError::invalid("not a URL")));
                return;
            };
            let request = NSURLRequest::requestWithURL(&nsurl);
            // SAFETY: on the main thread (`on_main`), with the retained
            // webview and a retained NSURLRequest; the returned navigation
            // is retained or nil.
            let started = unsafe { webview.loadRequest(&request) };
            let _ = tx.send(started.map(|_| ()).ok_or_else(|| {
                BrowserError::new(
                    ErrorCode::NavigationFailed,
                    "the page did not start loading",
                )
            }));
        })
        .await?
    }

    #[allow(unsafe_code)]
    async fn history(&self, step: HistoryStep) -> Result<(), BrowserError> {
        let shared = self.shared.clone();
        self.on_main(move |webview, _mtm, tx| {
            shared.lock().known.clear();
            // SAFETY: on the main thread (`on_main`), with the retained
            // webview; these methods take no arguments and return a
            // retained navigation or nil.
            let started = unsafe {
                match step {
                    HistoryStep::Back => webview.goBack(),
                    HistoryStep::Forward => webview.goForward(),
                    HistoryStep::Reload => webview.reload(),
                }
            };
            let _ = tx.send(started.map(|_| ()).ok_or_else(|| {
                BrowserError::invalid(match step {
                    HistoryStep::Back => "there is no page to go back to",
                    HistoryStep::Forward => "there is no page to go forward to",
                    HistoryStep::Reload => "there is no page to reload",
                })
            }));
        })
        .await?
    }

    #[allow(unsafe_code)]
    async fn page(&self) -> Result<EnginePage, BrowserError> {
        self.on_main(|webview, _mtm, tx| {
            // SAFETY: on the main thread (`on_main`), with the retained
            // webview; `URL`, `title` and `isLoading` are getters that take
            // no arguments and return retained objects, nil or a BOOL.
            let page = unsafe {
                EnginePage {
                    url: webview
                        .URL()
                        .and_then(|url| url.absoluteString())
                        .map(|url| url.to_string())
                        .unwrap_or_else(|| "about:blank".to_owned()),
                    title: webview
                        .title()
                        .map(|title| title.to_string())
                        .unwrap_or_default(),
                    loading: webview.isLoading(),
                }
            };
            let _ = tx.send(page);
        })
        .await
    }

    #[allow(unsafe_code)]
    async fn screenshot(&self, rect: Option<RawRect>) -> Result<Screenshot, BrowserError> {
        self.on_main(move |webview, mtm, tx| {
            let tx = Mutex::new(Some(tx));
            let done = RcBlock::new(move |image: *mut NSImage, error: *mut NSError| {
                let Some(tx) = tx.lock().ok().and_then(|mut tx| tx.take()) else {
                    return;
                };
                // SAFETY: WebKit calls the handler with a valid NSImage (alive
                // for the call) or null (`as_ref`: `None`); `png` copies the
                // bytes out, so the reference does not escape.
                let shot = match unsafe { image.as_ref() } {
                    Some(image) => png(image),
                    None => Err(ns_error(error, "the page could not be captured")),
                };
                let _ = tx.send(shot);
            });
            // SAFETY: on the main thread (`mtm`), with the retained webview;
            // the configuration is a fresh retained object and the block has
            // the declared `(NSImage *, NSError *)` signature (WebKit copies
            // it and calls it once).
            unsafe {
                let configuration = WKSnapshotConfiguration::new(mtm);
                if let Some(rect) = rect {
                    configuration.setRect(CGRect::new(
                        CGPoint::new(rect.x, rect.y),
                        CGSize::new(rect.width.max(1.0), rect.height.max(1.0)),
                    ));
                }
                webview
                    .takeSnapshotWithConfiguration_completionHandler(Some(&configuration), &done);
            }
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
                "trusted input needs the Accessibility permission",
            ));
        }
        // The page reports this click's downs as trusted input: they are
        // the driver's.
        self.shared
            .posting
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .posting(if double { 2 } else { 1 }, std::time::Instant::now());
        self.on_main(move |webview, _mtm, tx| {
            let _ = tx.send(post_click(webview, at, double));
        })
        .await?
    }
}

/// The NSImage as PNG bytes.
#[allow(unsafe_code)]
fn png(image: &NSImage) -> Result<Screenshot, BrowserError> {
    let fail = || BrowserError::new(ErrorCode::Internal, "the capture could not be encoded");
    let tiff = image.TIFFRepresentation().ok_or_else(fail)?;
    let bitmap = NSBitmapImageRep::imageRepWithData(&tiff).ok_or_else(fail)?;
    // SAFETY: `bitmap` is a retained NSBitmapImageRep; the properties are
    // an empty (so correctly typed) dictionary; the result is retained
    // NSData or nil.
    let data = unsafe {
        bitmap.representationUsingType_properties(NSBitmapImageFileType::PNG, &NSDictionary::new())
    }
    .ok_or_else(fail)?;
    Ok(Screenshot {
        media_type: "image/png",
        data: data.to_vec(),
        width: u32::try_from(bitmap.pixelsWide()).unwrap_or(0),
        height: u32::try_from(bitmap.pixelsHigh()).unwrap_or(0),
    })
}

/// A real left click at `at` (CSS pixels of the page view, top-left
/// origin), posted to this process only, aimed at the page view's window.
#[allow(unsafe_code)]
fn post_click(webview: &WKWebView, at: (f64, f64), double: bool) -> Result<(), BrowserError> {
    let unavailable = || BrowserError::new(ErrorCode::Internal, "the page view is not on screen");
    let view: &NSView = webview;
    let window = view.window().ok_or_else(unavailable)?;
    if view.isHiddenOrHasHiddenAncestor() || !window.isVisible() {
        // The pane is hidden (another tab, a modal sheet): never click
        // whatever is under that point instead.
        return Err(unavailable());
    }
    // SAFETY: on the main thread (`on_main` calls this), with the retained
    // webview; `pageZoom` is a CGFloat getter.
    let zoom = unsafe { webview.pageZoom() };
    let bounds = view.bounds();
    let local_y = if view.isFlipped() {
        at.1 * zoom
    } else {
        bounds.size.height - at.1 * zoom
    };
    let local = CGPoint::new(at.0 * zoom, local_y);
    if local.x < 0.0 || local.y < 0.0 || local.x > bounds.size.width || local.y > bounds.size.height
    {
        return Err(BrowserError::invalid(
            "that element is outside the visible page",
        ));
    }
    let in_window = view.convertPoint_toView(local, None);
    let on_screen = window.convertPointToScreen(in_window);
    // CGEvent coordinates have their origin at the top-left of the main
    // display; AppKit's screen coordinates at its bottom-left.
    let main_height = objc2_app_kit::NSScreen::screens(MainThreadMarker::from(webview))
        .firstObject()
        .map_or(0.0, |screen| screen.frame().size.height);
    let point = CGPoint::new(on_screen.x, main_height - on_screen.y);
    let window_number = window.windowNumber();
    let pid = std::process::id();
    let clicks = if double { 2 } else { 1 };
    for click in 1..=clicks {
        for kind in [CGEventType::LeftMouseDown, CGEventType::LeftMouseUp] {
            let event = CGEvent::new_mouse_event(None, kind, point, CGMouseButton::Left)
                .ok_or_else(|| BrowserError::new(ErrorCode::Internal, "no input event"))?;
            CGEvent::set_integer_value_field(
                Some(&event),
                CGEventField::MouseEventClickState,
                click,
            );
            CGEvent::set_integer_value_field(
                Some(&event),
                CGEventField::MouseEventWindowUnderMousePointer,
                i64::try_from(window_number).unwrap_or(0),
            );
            CGEvent::set_integer_value_field(
                Some(&event),
                CGEventField::MouseEventWindowUnderMousePointerThatCanHandleThisEvent,
                i64::try_from(window_number).unwrap_or(0),
            );
            CGEvent::post_to_pid(libc::pid_t::try_from(pid).unwrap_or(0), Some(&event));
        }
    }
    Ok(())
}

/// Built by [`crate::attach`]: the engine of a Tauri webview.
#[cfg(feature = "tauri")]
pub(crate) fn engine_of(platform: &tauri::webview::PlatformWebview) -> Option<WebKitEngine> {
    let mtm = MainThreadMarker::new()?;
    crate::native::webview_of(platform).map(|webview| WebKitEngine::new(webview, mtm))
}
