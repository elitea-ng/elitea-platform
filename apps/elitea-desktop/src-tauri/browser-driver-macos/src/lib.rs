//! The desktop browser pane's macOS driver (ADR-0048 decisions 5 and 6).
//!
//! The pane's page is a WKWebView (a Tauri child webview, #1588). This
//! crate drives it for the agent behind the shared browser tool contract
//! (`elitea-browser-tools`), so the agent sees the same tools, results and
//! errors as on a cloud thread:
//!
//! * [`SCRIPT`] — the driver's page script, run ONLY in a private
//!   `WKContentWorld`: the snapshot is computed there from the DOM and ARIA
//!   (roles, accessible names, states, visibility from layout; same-origin
//!   frames and shadow roots inlined), and actions are DOM events on the
//!   target element. The page's own scripts cannot see or call it.
//! * [`reply`] — the script's answers read on the host, and its raw nodes
//!   turned into contract nodes (secure fields decided again here).
//! * [`driver`] — [`WebDriver`], the contract's `BrowserDriver`: refs, the
//!   hard limits, the act rate, cross-origin frames read in their own frame,
//!   and the trusted-input fallback. Plain Rust over an [`Engine`].
//! * `webkit` (macOS) — [`WebKitEngine`], the WebKit, AppKit and
//!   CoreGraphics calls: `callAsyncJavaScript` in the driver world,
//!   `takeSnapshot(with:)`, and CGEvent clicks posted to this process only
//!   when the Accessibility grant (ADR-0047) is there.
//! * `native` (macOS) — the pane's own controls that wry and Tauri do not
//!   expose (#1634): the private-network `WKContentRuleList`
//!   ([`native::install_content_rules`]) and native Back, Forward and Stop
//!   ([`native::go`]).
//!
//! This is the one first-party crate allowed `unsafe` (`unsafe_code =
//! "deny"`, each FFI item opted in with `#[allow(unsafe_code)]` and a
//! `// SAFETY:` comment; repo-root `AGENTS.md`). Everything it exports is
//! safe to call.
//!
//! No JavaScript ever comes from the model: the host sends an operation
//! name and JSON arguments to a fixed function body.

pub mod driver;
pub mod input;
#[cfg(target_os = "macos")]
pub mod native;
pub mod reply;
#[cfg(target_os = "macos")]
pub mod webkit;

pub use driver::{Engine, EnginePage, HandOver, NavigationCheck, NoHandOver, WebDriver};
#[cfg(target_os = "macos")]
pub use webkit::{WebKitEngine, accessibility_granted};

/// The driver's page script: `js/driver.ts` compiled by `tsc`
/// (`js/tsconfig.json`) into `$OUT_DIR/driver.js` by `build.rs`. No
/// JavaScript is committed; a type error in `driver.ts` fails the build.
pub const SCRIPT: &str = include_str!(concat!(env!("OUT_DIR"), "/driver.js"));

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

#[cfg(test)]
mod tests {
    use super::SCRIPT;

    #[test]
    fn the_script_is_one_self_installing_function_with_no_eval() {
        assert!(SCRIPT.contains("(function install()"));
        assert!(SCRIPT.contains("__eliteaBrowserDriver"));
        // The driver world never evaluates strings itself: nothing the page
        // or the model wrote can become code there.
        for banned in [
            "eval(",
            "new Function",
            "Function(",
            "setTimeout('",
            "setTimeout(\"",
        ] {
            assert!(!SCRIPT.contains(banned), "{banned}");
        }
    }

    #[test]
    fn the_script_reports_every_kind_of_the_persons_input_the_host_reads() {
        let start = SCRIPT
            .find("['pointerdown'")
            .expect("the reported input list");
        let list = &SCRIPT[start..start + SCRIPT[start..].find(']').unwrap() + 1];
        let reported: Vec<&str> = list
            .trim_matches(|c| c == '[' || c == ']')
            .split(',')
            .map(|item| item.trim().trim_matches('\''))
            .collect();
        assert_eq!(reported, super::input::REPORTED);
        // IME, dictation, paste and drop have no keydown or pointerdown.
        for kind in ["beforeinput", "paste", "drop"] {
            assert!(reported.contains(&kind), "{kind}");
        }
    }

    #[test]
    fn the_script_decides_secure_fields_like_the_contract() {
        for token in ["current-password", "new-password", "cc-number", "cc-csc"] {
            assert!(SCRIPT.contains(&format!("'{token}'")), "{token}");
        }
        assert!(SCRIPT.contains("type === 'password'"));
        assert!(SCRIPT.contains("secure_field_refused"));
    }

    #[test]
    fn the_script_answers_only_the_codes_the_host_accepts() {
        let codes: Vec<&str> = SCRIPT
            .match_indices("failure('")
            .map(|(at, _)| {
                let rest = &SCRIPT[at + "failure('".len()..];
                &rest[..rest.find('\'').unwrap_or(0)]
            })
            .collect();
        assert!(!codes.is_empty());
        for code in codes {
            let parsed = elitea_browser_tools::ErrorCode::parse(code);
            assert!(
                parsed.is_some_and(|code| super::reply::SCRIPT_CODES.contains(&code))
                    || code == "internal",
                "{code}"
            );
        }
    }
}
