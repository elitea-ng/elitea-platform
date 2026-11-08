//! The `WebView2` COM calls (Windows only): the one place `unsafe` is used
//! in this crate. Every COM object stays on the UI thread; what crosses
//! threads is bytes.
//!
//! The unsafe blocks, all `WebView2` COM methods called on the UI thread on
//! a live interface the webview holds:
//!
//! 1. `ICoreWebView2Controller::CoreWebView2` — the page's `ICoreWebView2`.
//! 2. `ICoreWebView2::GetDevToolsProtocolEventReceiver` and
//!    `ICoreWebView2DevToolsProtocolEventReceiver::add_DevToolsProtocolEventReceived`
//!    — one subscription per event the driver reads.
//! 3. `ICoreWebView2DevToolsProtocolEventReceivedEventArgs::ParameterObjectAsJson`
//!    and `ICoreWebView2DevToolsProtocolEventReceivedEventArgs2::SessionId`
//!    — an event's parameters and its target session (out-parameter
//!    strings, freed by `take_pwstr`).
//! 4. `ICoreWebView2::CallDevToolsProtocolMethod` and
//!    `ICoreWebView2_11::CallDevToolsProtocolMethodForSession` — one
//!    command.
//! 5. `ICoreWebView2DevToolsProtocolEventReceiver::remove_DevToolsProtocolEventReceived`
//!    — the subscriptions go when the stream closes.
//! 6. `ICoreWebView2::GoBack`, `GoForward` and `Stop` — the pane's buttons.

use std::cell::RefCell;
use std::collections::HashMap;

use tauri::webview::PlatformWebview;
use tauri::{AppHandle, Runtime, Webview};
use tokio::io::{AsyncBufReadExt as _, AsyncWriteExt as _, BufReader, DuplexStream};
use tokio::sync::mpsc;
use webview2_com::Microsoft::Web::WebView2::Win32::{
    ICoreWebView2, ICoreWebView2_11, ICoreWebView2Controller,
    ICoreWebView2DevToolsProtocolEventReceivedEventArgs,
    ICoreWebView2DevToolsProtocolEventReceivedEventArgs2,
    ICoreWebView2DevToolsProtocolEventReceiver,
};
use webview2_com::{
    CallDevToolsProtocolMethodCompletedHandler, DevToolsProtocolEventReceivedEventHandler,
    take_pwstr,
};
use windows_core::{HSTRING, Interface as _, PWSTR};

use crate::{EVENTS, wire};

/// The buffer of each direction of the byte stream.
const STREAM_BYTES: usize = 4 << 20;

/// Bytes on their way to the driver (answers and events).
type Out = mpsc::UnboundedSender<Vec<u8>>;

/// The page and its event subscriptions, removed when its stream closes.
struct Pane {
    webview: ICoreWebView2,
    receivers: Vec<(ICoreWebView2DevToolsProtocolEventReceiver, i64)>,
}

thread_local! {
    /// The panes whose CDP stream is open, by Tauri webview label. UI
    /// thread only: the COM objects are not `Send`.
    static PANES: RefCell<HashMap<String, Pane>> = RefCell::new(HashMap::new());
}

/// The browser buttons `WebView2` runs itself (the page cannot override
/// them).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Go {
    Back,
    Forward,
    Stop,
}

/// Hands work to the UI thread; `false` when it is gone (the app quits).
pub type OnUiThread = std::sync::Arc<dyn Fn(Box<dyn FnOnce() + Send>) -> bool + Send + Sync>;

/// Sees each command on its way to the page (the host matches its own
/// trusted input to the reports of the person's).
pub type Observe = std::sync::Arc<dyn Fn(&wire::Command) + Send + Sync>;

/// Open the CDP stream of a Tauri webview's page, for
/// `elitea-browser-session`'s `Connection::new` (the halves of the one
/// stream); its page session is [`crate::PAGE_SESSION`]. Runs inside a
/// tokio runtime. The stream ends when the connection is dropped (or the
/// app quits); a pane that closes first answers every command with an
/// error.
///
/// # Errors
///
/// The webview is not a `WebView2`, or Tauri could not reach it.
pub async fn attach<R: Runtime>(
    app: &AppHandle<R>,
    webview: &Webview<R>,
    observe: Observe,
) -> Result<DuplexStream, String> {
    let label = webview.label().to_owned();
    let app = app.clone();
    let on_ui: OnUiThread = std::sync::Arc::new(move |work| app.run_on_main_thread(work).is_ok());
    let (ready, opened) = tokio::sync::oneshot::channel();
    {
        let on_ui = on_ui.clone();
        webview
            .with_webview(move |platform: PlatformWebview| {
                let _ = ready.send(open_stream(&label, &platform.controller(), on_ui, observe));
            })
            .map_err(|error| error.to_string())?;
    }
    opened
        .await
        .map_err(|_| "the page view is gone".to_owned())?
}

/// [`attach`] for any `WebView2` host (the conformance run's plain window):
/// call it ON the UI thread with the webview's controller. `key` names the
/// page (one stream per key); `on_ui` hands work to this same thread.
///
/// # Errors
///
/// The controller has no page.
pub fn open_stream(
    key: &str,
    controller: &ICoreWebView2Controller,
    on_ui: OnUiThread,
    observe: Observe,
) -> Result<DuplexStream, String> {
    let (driver_side, bridge_side) = tokio::io::duplex(STREAM_BYTES);
    let (bridge_read, mut bridge_write) = tokio::io::split(bridge_side);
    let (out, mut outgoing) = mpsc::unbounded_channel::<Vec<u8>>();
    open(key, controller, &out)?;
    let key = key.to_owned();
    // Driver to the UI thread: one command per record.
    tokio::spawn(async move {
        let mut reader = BufReader::new(bridge_read);
        let mut record = Vec::new();
        loop {
            record.clear();
            match reader.read_until(0, &mut record).await {
                Ok(0) | Err(_) => break,
                Ok(_) => {}
            }
            if record.last() == Some(&0) {
                record.pop();
            }
            let Some(command) = wire::command(&record) else {
                continue;
            };
            observe(&command);
            let id = command.id;
            let (on_main, key) = (out.clone(), key.clone());
            if !on_ui(Box::new(move || call(&key, command, &on_main))) {
                let _ = out.send(wire::refused(id, "the app is closing"));
            }
        }
        let _ = on_ui(Box::new(move || close(&key)));
    });
    // The UI thread to the driver.
    tokio::spawn(async move {
        while let Some(bytes) = outgoing.recv().await {
            if bridge_write.write_all(&bytes).await.is_err() {
                break;
            }
        }
    });
    Ok(driver_side)
}

/// Back, Forward or Stop on the `WebView2` itself.
///
/// # Errors
///
/// The webview is not a `WebView2`, Tauri could not reach it, or the call
/// failed.
pub async fn go<R: Runtime>(webview: &Webview<R>, action: Go) -> Result<(), String> {
    let (done, result) = tokio::sync::oneshot::channel();
    webview
        .with_webview(move |platform: PlatformWebview| {
            let _ = done.send(go_now(&platform.controller(), action));
        })
        .map_err(|error| error.to_string())?;
    result
        .await
        .map_err(|_| "the page view is gone".to_owned())?
}

#[allow(unsafe_code)]
fn core_of(controller: &ICoreWebView2Controller) -> Result<ICoreWebView2, String> {
    // SAFETY: (1) on the UI thread (Tauri's `with_webview` runs us there),
    // on the live controller the webview holds; `CoreWebView2` takes no
    // arguments and returns an owned interface or an error.
    unsafe { controller.CoreWebView2() }.map_err(|error| error.message())
}

#[allow(unsafe_code)]
fn go_now(controller: &ICoreWebView2Controller, action: Go) -> Result<(), String> {
    let webview = core_of(controller)?;
    // SAFETY: (6) on the UI thread, on the live `ICoreWebView2`; these take
    // no arguments. Going back with no history is a no-op, not an error.
    let done = unsafe {
        match action {
            Go::Back => webview.GoBack(),
            Go::Forward => webview.GoForward(),
            Go::Stop => webview.Stop(),
        }
    };
    done.map_err(|error| error.message())
}

/// Subscribe to [`EVENTS`] of the page, on the UI thread.
#[allow(unsafe_code)]
fn open(label: &str, controller: &ICoreWebView2Controller, out: &Out) -> Result<(), String> {
    let webview = core_of(controller)?;
    close(label);
    let mut receivers = Vec::with_capacity(EVENTS.len());
    for &name in EVENTS {
        // SAFETY: (2) on the UI thread, on the live `ICoreWebView2`; the
        // name is a valid, NUL-terminated wide string (`HSTRING`) that
        // outlives the call. The result is an owned receiver or an error.
        let receiver = unsafe { webview.GetDevToolsProtocolEventReceiver(&HSTRING::from(name)) }
            .map_err(|error| error.message())?;
        let out = out.clone();
        let handler = DevToolsProtocolEventReceivedEventHandler::create(Box::new(
            move |_sender, args: Option<ICoreWebView2DevToolsProtocolEventReceivedEventArgs>| {
                if let Some(args) = args {
                    let (params, session) = event_of(&args);
                    let _ = out.send(wire::event(name, &params, &session));
                }
                Ok(())
            },
        ));
        let mut token = 0_i64;
        // SAFETY: (2) on the UI thread, on the live receiver; `handler` is a
        // COM object that `WebView2` AddRefs and keeps, and `token` is a
        // valid `i64` out-parameter for the call's duration.
        unsafe { receiver.add_DevToolsProtocolEventReceived(&handler, &raw mut token) }
            .map_err(|error| error.message())?;
        receivers.push((receiver, token));
    }
    PANES.with(|panes| {
        panes
            .borrow_mut()
            .insert(label.to_owned(), Pane { webview, receivers });
    });
    Ok(())
}

/// An event's parameters and its session (empty: the page's own).
#[allow(unsafe_code)]
fn event_of(args: &ICoreWebView2DevToolsProtocolEventReceivedEventArgs) -> (String, String) {
    let mut params = PWSTR::null();
    // SAFETY: (3) on the UI thread (`WebView2` calls the handler there) on
    // the live args; `params` is a valid out-parameter, which `WebView2`
    // fills with a CoTaskMem string (or leaves null) that `take_pwstr`
    // copies and frees.
    let params = match unsafe { args.ParameterObjectAsJson(&raw mut params) } {
        Ok(()) => take_pwstr(params),
        Err(_) => String::new(),
    };
    let session = args
        .cast::<ICoreWebView2DevToolsProtocolEventReceivedEventArgs2>()
        .ok()
        .map(|args| {
            let mut session = PWSTR::null();
            // SAFETY: (3) as above, on the args' second interface.
            match unsafe { args.SessionId(&raw mut session) } {
                Ok(()) => take_pwstr(session),
                Err(_) => String::new(),
            }
        })
        .unwrap_or_default();
    (params, session)
}

/// Send one command, on the UI thread; its answer goes to `out`.
#[allow(unsafe_code)]
fn call(label: &str, command: wire::Command, out: &Out) {
    let webview = PANES.with(|panes| panes.borrow().get(label).map(|pane| pane.webview.clone()));
    let Some(webview) = webview else {
        let _ = out.send(wire::refused(command.id, "the page is closed"));
        return;
    };
    let id = command.id;
    let done = {
        let out = out.clone();
        CallDevToolsProtocolMethodCompletedHandler::create(Box::new(
            move |result: windows_core::Result<()>, answer: String| {
                let _ = out.send(wire::reply(id, result.is_err(), &answer));
                Ok(())
            },
        ))
    };
    let method = HSTRING::from(command.method.as_str());
    let params = HSTRING::from(command.params.as_str());
    let sent = match &command.session {
        // SAFETY: (4) on the UI thread, on the live `ICoreWebView2`; the
        // method and parameters are valid wide strings that outlive the
        // call, and `done` is a COM handler `WebView2` AddRefs and calls
        // once.
        None => unsafe { webview.CallDevToolsProtocolMethod(&method, &params, &done) },
        Some(session) => match webview.cast::<ICoreWebView2_11>() {
            Ok(webview) => {
                let session = HSTRING::from(session.as_str());
                // SAFETY: (4) as above, with the session id a valid wide
                // string too.
                unsafe {
                    webview.CallDevToolsProtocolMethodForSession(&session, &method, &params, &done)
                }
            }
            Err(error) => Err(error),
        },
    };
    if let Err(error) = sent {
        let _ = out.send(wire::refused(id, &error.message()));
    }
}

/// Drop the page's subscriptions, on the UI thread.
#[allow(unsafe_code)]
fn close(label: &str) {
    let Some(pane) = PANES.with(|panes| panes.borrow_mut().remove(label)) else {
        return;
    };
    for (receiver, token) in pane.receivers {
        // SAFETY: (5) on the UI thread, on the live receiver, with the token
        // its own `add_DevToolsProtocolEventReceived` gave.
        let _ = unsafe { receiver.remove_DevToolsProtocolEventReceived(token) };
    }
}
