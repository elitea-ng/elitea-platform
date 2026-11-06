//! The pane's native WebKit controls (macOS, #1634): what the desktop host
//! cannot do through wry or Tauri, which expose neither.
//!
//! * **Content rules.** [`install_content_rules_on`] compiles a
//!   `WKContentRuleList` (WebKit's content-blocker JSON) in
//!   `WKContentRuleListStore.defaultStore` and adds it to the webview's
//!   `userContentController`. The rules then apply to EVERY load the page
//!   makes (top level, frames, images, scripts, `fetch`/XHR, WebSockets),
//!   before the request leaves the process. Compiling is asynchronous: the
//!   caller must not load a page until `done` reported `Ok` (the host
//!   makes the pane at `about:blank` and navigates only after that). A
//!   compiled list is kept for the next pane with the same JSON.
//! * **History.** [`go_on`]: `goBack`, `goForward` and `stopLoading` on the
//!   WKWebView itself, so a page that replaces `history.back` or
//!   `window.stop` cannot stop the toolbar's buttons.
//!
//! Everything here runs on the main thread (a `WKWebView` reference is
//! proof of it: the type is main-thread only). With the `tauri` feature,
//! [`install_content_rules`] and [`go`] take a Tauri webview and hop to the
//! main thread themselves.

use std::cell::{Cell, RefCell};
use std::fmt;

use block2::RcBlock;
use objc2::rc::Retained;
use objc2::{MainThreadMarker, Message as _};
use objc2_foundation::{NSError, NSString};
use objc2_web_kit::{WKContentRuleList, WKContentRuleListStore, WKWebView};

/// Why the native controls could not do what was asked.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum NativeError {
    /// The page view is gone, or is not a WKWebView.
    Unavailable,
    /// WebKit has no content rule store (it always has one on macOS 10.13+).
    NoStore,
    /// WebKit refused the rule list (its message, cut short).
    RulesRejected(String),
}

impl fmt::Display for NativeError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Unavailable => f.write_str("the browser page is not available"),
            Self::NoStore => f.write_str("WebKit has no content rule store"),
            Self::RulesRejected(why) => write!(f, "WebKit refused the content rules: {why}"),
        }
    }
}

impl std::error::Error for NativeError {}

/// What the toolbar's native history buttons ask for.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Go {
    Back,
    Forward,
    Stop,
}

/// The store identifier of a rule list: its JSON's FNV-1a hash, so two
/// panes with the same rules share one compiled list, and a changed rule
/// set never reuses a stale one.
#[must_use]
pub fn rules_identifier(json: &str) -> String {
    let mut hash: u64 = 0xcbf2_9ce4_8422_2325;
    for byte in json.bytes() {
        hash ^= u64::from(byte);
        hash = hash.wrapping_mul(0x0100_0000_01b3);
    }
    format!("elitea-browser-pane-{hash:016x}")
}

thread_local! {
    /// The last rule list compiled on this (the main) thread, by its JSON.
    static COMPILED: RefCell<Option<(String, Retained<WKContentRuleList>)>> =
        const { RefCell::new(None) };
}

/// Add `list` to `webview`'s content controller.
#[allow(unsafe_code)]
fn add_list(webview: &WKWebView, list: &WKContentRuleList) {
    // SAFETY: `webview` and `list` are live, retained Objective-C objects
    // of the classes these methods are declared on, and we are on the main
    // thread (a `&WKWebView` is main-thread only). `configuration` and
    // `userContentController` return retained, non-null objects; the
    // controller retains the list it is given.
    unsafe {
        webview
            .configuration()
            .userContentController()
            .addContentRuleList(list);
    }
}

/// Compile `json` as a `WKContentRuleList` and install it in `webview`;
/// `done` gets the outcome, on the main thread, once. Do not load a page in
/// `webview` before `done` reported `Ok`: loads that start earlier are not
/// covered.
#[allow(unsafe_code)]
pub fn install_content_rules_on(
    webview: &WKWebView,
    json: &str,
    done: impl FnOnce(Result<(), NativeError>) + 'static,
) {
    let cached = COMPILED.with_borrow(|compiled| {
        compiled
            .as_ref()
            .filter(|(known, _)| known == json)
            .map(|(_, list)| list.clone())
    });
    if let Some(list) = cached {
        add_list(webview, &list);
        done(Ok(()));
        return;
    }
    let mtm = MainThreadMarker::from(webview);
    // SAFETY: a class method that takes no arguments, called on the main
    // thread (`mtm`); it returns a retained store or nil, which `Option`
    // carries.
    let Some(store) = (unsafe { WKContentRuleListStore::defaultStore(mtm) }) else {
        done(Err(NativeError::NoStore));
        return;
    };
    let webview = webview.retain();
    let source = json.to_owned();
    let done = Cell::new(Some(done));
    let handler = RcBlock::new(move |list: *mut WKContentRuleList, error: *mut NSError| {
        let Some(done) = done.take() else {
            return;
        };
        // SAFETY: WebKit calls this handler on the main thread with either
        // a valid, autoreleased-but-alive `WKContentRuleList` or null, and
        // a valid `NSError` or null; `as_ref` maps null to `None`, and the
        // references do not outlive this call (`retain` keeps the list).
        let (list, error) = unsafe { (list.as_ref(), error.as_ref()) };
        match list {
            Some(list) => {
                let list = list.retain();
                add_list(&webview, &list);
                COMPILED.with_borrow_mut(|compiled| *compiled = Some((source.clone(), list)));
                done(Ok(()));
            }
            None => {
                let why = error.map_or_else(
                    || "no reason given".to_owned(),
                    |error| error.localizedDescription().to_string(),
                );
                done(Err(NativeError::RulesRejected(
                    why.chars().take(200).collect(),
                )));
            }
        }
    });
    // SAFETY: on the main thread; the identifier and the JSON are live
    // NSStrings for the call (WebKit copies them), and the handler is a
    // heap block of the declared signature
    // `(WKContentRuleList *, NSError *) -> void`, which WebKit copies and
    // calls exactly once.
    unsafe {
        store.compileContentRuleListForIdentifier_encodedContentRuleList_completionHandler(
            Some(&NSString::from_str(&rules_identifier(json))),
            Some(&NSString::from_str(json)),
            Some(&handler),
        );
    }
}

/// Go back, forward, or stop loading, natively. Back and Forward with no
/// page there do nothing.
#[allow(unsafe_code)]
pub fn go_on(webview: &WKWebView, action: Go) {
    // SAFETY: `webview` is a live WKWebView and we are on the main thread
    // (a `&WKWebView` is main-thread only). The methods take no arguments;
    // the returned navigation (or nil) is dropped.
    unsafe {
        match action {
            Go::Back => drop(webview.goBack()),
            Go::Forward => drop(webview.goForward()),
            Go::Stop => webview.stopLoading(),
        }
    }
}

/// The WKWebView of a Tauri webview, retained. On the main thread
/// (`with_webview` runs there).
#[cfg(feature = "tauri")]
#[allow(unsafe_code)]
pub(crate) fn webview_of(
    platform: &tauri::webview::PlatformWebview,
) -> Option<Retained<WKWebView>> {
    MainThreadMarker::new()?;
    let pointer = platform.inner().cast::<WKWebView>();
    // SAFETY: Tauri's `PlatformWebview::inner` on macOS is the `WKWebView *`
    // of a webview it owns and keeps alive for the duration of the
    // `with_webview` callback; `retain` takes our own strong reference
    // (null gives `None`), so the object outlives the callback.
    unsafe { Retained::retain(pointer) }
}

/// Run `work` with the Tauri webview's WKWebView on the main thread.
#[cfg(feature = "tauri")]
async fn with_wk<T: Send + 'static>(
    webview: &tauri::Webview,
    work: impl FnOnce(&WKWebView, tokio::sync::oneshot::Sender<T>) + Send + 'static,
) -> Result<T, NativeError> {
    let (tx, rx) = tokio::sync::oneshot::channel();
    webview
        .with_webview(move |platform| {
            if let Some(wk) = webview_of(&platform) {
                work(&wk, tx);
            }
        })
        .map_err(|_| NativeError::Unavailable)?;
    rx.await.map_err(|_| NativeError::Unavailable)
}

/// [`install_content_rules_on`] for a Tauri webview: answers once the rules
/// are installed (or refused).
///
/// # Errors
///
/// [`NativeError`]: the page view is gone, or WebKit refused the rules.
#[cfg(feature = "tauri")]
pub async fn install_content_rules(
    webview: &tauri::Webview,
    json: &str,
) -> Result<(), NativeError> {
    let json = json.to_owned();
    with_wk(webview, move |wk, tx| {
        install_content_rules_on(wk, &json, move |outcome| {
            let _ = tx.send(outcome);
        });
    })
    .await?
}

/// [`go_on`] for a Tauri webview.
///
/// # Errors
///
/// [`NativeError::Unavailable`]: the page view is gone.
#[cfg(feature = "tauri")]
pub async fn go(webview: &tauri::Webview, action: Go) -> Result<(), NativeError> {
    with_wk(webview, move |wk, tx| {
        go_on(wk, action);
        let _ = tx.send(());
    })
    .await
}

#[cfg(test)]
mod tests {
    use super::rules_identifier;

    #[test]
    fn a_rule_set_has_one_stable_identifier() {
        let a = rules_identifier("[{\"a\":1}]");
        assert_eq!(a, rules_identifier("[{\"a\":1}]"));
        assert_ne!(a, rules_identifier("[{\"a\":2}]"));
        assert!(a.starts_with("elitea-browser-pane-"));
        assert_eq!(a.len(), "elitea-browser-pane-".len() + 16);
    }
}
