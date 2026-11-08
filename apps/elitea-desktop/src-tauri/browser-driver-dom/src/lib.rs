//! The desktop browser pane's webview driver core (ADR-0048 decisions 5
//! and 6), shared by the macOS and Linux drivers.
//!
//! The pane's page is a system webview (a Tauri child webview, #1588).
//! On macOS (WKWebView) and Linux (WebKitGTK) the agent drives it with a
//! page script in an isolated world, behind the shared browser tool
//! contract (`elitea-browser-tools`), so the agent sees the same tools,
//! results and errors as on a cloud thread:
//!
//! * [`SCRIPT`] — the driver's page script, run ONLY in the driver's
//!   isolated world (a private `WKContentWorld` on macOS, a
//!   `WebKitScriptWorld` on Linux): the snapshot is computed there from
//!   the DOM and ARIA (roles, accessible names, states, visibility from
//!   layout; same-origin frames and shadow roots inlined), and actions are
//!   DOM events on the target element. The page's own scripts cannot see
//!   or call it.
//! * [`reply`] — the script's answers read on the host, and its raw nodes
//!   turned into contract nodes (secure fields decided again here).
//! * [`driver`] — [`WebDriver`], the contract's `BrowserDriver`: refs, the
//!   hard limits, the act rate, cross-origin frames read in their own frame,
//!   and the trusted-input fallback. Plain Rust over an [`Engine`].
//! * [`input`] — telling the person's input from the driver's own.
//!
//! The engines are elsewhere: `elitea-browser-driver-macos` (WebKit FFI,
//! one of the two crates allowed `unsafe`) and `elitea-browser-driver-linux`
//! (the safe `webkit2gtk` API). Windows uses only
//! [`INPUT_SCRIPT`] and [`input`] from this crate: `WebView2` has CDP, so
//! its driver is the cloud's CDP driver (`elitea-browser-session`) over
//! `CallDevToolsProtocolMethod`.
//!
//! No JavaScript ever comes from the model: the host sends an operation
//! name and JSON arguments to a fixed function body.

pub mod driver;
pub mod input;
pub mod reply;

pub use driver::{Engine, EnginePage, HandOver, NavigationCheck, NoHandOver, WebDriver};

/// The driver's page script: `js/driver.ts` compiled by `tsc`
/// (`js/tsconfig.json`) into `$OUT_DIR/driver.js` by `build.rs`. No
/// JavaScript is committed; a type error in `driver.ts` fails the build.
pub const SCRIPT: &str = include_str!(concat!(env!("OUT_DIR"), "/driver.js"));

/// The one function body an engine passes to its async-call API
/// (`callAsyncJavaScript` on macOS, `webkit_web_view_call_async_javascript_function`
/// on Linux). `op`, `args` and `MISSING` are its ARGUMENTS (strings, the
/// second a JSON text), never source.
pub const CALL_BODY: &str = "const driver = globalThis.__eliteaBrowserDriver; \
if (driver === undefined) return MISSING; \
return await driver.run(op, args);";

/// [`CALL_BODY`]'s answer in a document loaded before the user script was
/// installed (the pane's first page): the engine then evaluates
/// [`SCRIPT`] itself in that frame, in the driver world, and calls again.
pub const MISSING: &str = "elitea-driver-missing";

/// The driver's isolated world. Named, so the Web Inspector shows it; the
/// name is not a secret (a world is reached through the API, not by name).
pub const WORLD_NAME: &str = "elitea-browser-driver";

/// The Windows pane's input reporter: `js/input.ts` compiled by `build.rs`
/// into `$OUT_DIR/input.js`. Installed over CDP in [`INPUT_WORLD`], it
/// calls the [`INPUT_BINDING`] binding with the type of each trusted input
/// event of the person (the list of [`input::REPORTED`]).
pub const INPUT_SCRIPT: &str = include_str!(concat!(env!("OUT_DIR"), "/input.js"));

/// The isolated world [`INPUT_SCRIPT`] runs in (Windows).
pub const INPUT_WORLD: &str = "elitea_browser_input";

/// The binding [`INPUT_SCRIPT`] reports on, in [`INPUT_WORLD`] only.
pub const INPUT_BINDING: &str = "eliteaBrowserInput";

/// The message handler a subframe announces itself on, in the driver world.
pub const FRAME_HANDLER: &str = "eliteaBrowserFrame";

/// The message handler the person's input is reported on, in the driver
/// world.
pub const INPUT_HANDLER: &str = "eliteaBrowserInput";

#[cfg(test)]
mod tests {
    use super::{INPUT_BINDING, INPUT_SCRIPT, SCRIPT};

    #[test]
    fn the_input_reporter_reports_the_same_kinds_through_its_binding() {
        assert!(INPUT_SCRIPT.contains(INPUT_BINDING));
        assert!(INPUT_SCRIPT.contains("isTrusted"));
        for kind in super::input::REPORTED {
            assert!(INPUT_SCRIPT.contains(&format!("'{kind}'")), "{kind}");
        }
        for banned in ["eval(", "new Function", "Function("] {
            assert!(!INPUT_SCRIPT.contains(banned), "{banned}");
        }
    }

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
